package recorder

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	rtspformat "github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/playback"
	"github.com/bluenviron/mediamtx/internal/recordwindow"
	"github.com/bluenviron/mediamtx/internal/stream"
	"github.com/bluenviron/mediamtx/internal/test"
	"github.com/bluenviron/mediamtx/internal/unit"
)

// newWindowTestStream creates a stream carrying a single H264 track.
func newWindowTestStream(t *testing.T) (*stream.Stream, *stream.SubStream, *description.Media) {
	t.Helper()

	forma := test.FormatH264
	desc := &description.Session{Medias: []*description.Media{{
		Type:    description.MediaTypeVideo,
		Formats: []rtspformat.Format{forma},
	}}}

	strm := &stream.Stream{
		OrigDesc:          desc,
		WriteQueueSize:    512,
		RTPMaxPayloadSize: 1450,
		Parent:            test.NilLogger,
	}
	require.NoError(t, strm.Initialize())
	t.Cleanup(strm.Close)

	subStream := &stream.SubStream{
		Stream:        strm,
		UseRTPPackets: false,
	}
	require.NoError(t, subStream.Initialize())

	return strm, subStream, desc.Medias[0]
}

// mediaWriter writes IDR samples whose DTS and NTP both advance by one second
// per tick, so the drift detector never trips unless NTP is jumped on purpose.
type mediaWriter struct {
	sub   *stream.SubStream
	media *description.Media
	dts   time.Duration
	ntp   time.Time
}

func (mw *mediaWriter) tickN(n int) {
	for range n {
		mw.sub.WriteUnit(mw.media, mw.media.Formats[0], &unit.Unit{
			PTS: int64(mw.dts/time.Second) * 90000,
			NTP: mw.ntp,
			Payload: unit.PayloadH264{
				test.FormatH264.SPS,
				test.FormatH264.PPS,
				{5}, // IDR
			},
		})
		mw.dts += time.Second
		mw.ntp = mw.ntp.Add(time.Second)
	}
}

// countFMP4Samples parses an fMP4 segment and returns the total number of
// samples across all its parts and tracks.
func countFMP4Samples(t *testing.T, path string) int {
	t.Helper()

	byts, err := os.ReadFile(path)
	require.NoError(t, err)

	var parts fmp4.Parts
	require.NoError(t, parts.Unmarshal(byts))

	n := 0
	for _, part := range parts {
		for _, track := range part.Tracks {
			n += len(track.Samples)
		}
	}
	return n
}

func segmentFiles(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		require.NoError(t, err)
	}

	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

func waitSegmentCount(t *testing.T, dir string, want int) []string {
	t.Helper()

	require.Eventually(t, func() bool {
		return len(segmentFiles(t, dir)) == want
	}, 3*time.Second, 10*time.Millisecond, "expected %d segments in %s, got %v", want, dir, segmentFiles(t, dir))

	return segmentFiles(t, dir)
}

func waitSegmentEvent(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func drainEvents(ch <-chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func requireFileSizeStable(t *testing.T, path string) {
	t.Helper()

	st1, err := os.Stat(path)
	require.NoError(t, err)
	time.Sleep(150 * time.Millisecond)
	st2, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, st1.Size(), st2.Size(), "segment %s kept growing", path)
}

type windowRecorderOpts struct {
	format    conf.RecordFormat
	localTime bool
}

func newWindowRecorder(
	t *testing.T,
	strm *stream.Stream,
	baseDir string,
	window *recordwindow.Window,
	clk Clock,
	opts *windowRecorderOpts,
) (*Recorder, chan struct{}, chan struct{}, chan struct{}) {
	t.Helper()

	format := conf.RecordFormatFMP4
	loc := time.UTC
	if opts != nil {
		format = opts.format
		if opts.localTime {
			loc = time.Local
		}
	}

	segCreated := make(chan struct{}, 64)
	segDone := make(chan struct{}, 64)
	instanceReady := make(chan struct{}, 64)

	w := &Recorder{
		location:        loc,
		PathFormat:      filepath.Join(baseDir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
		Format:          format,
		PartDuration:    100 * time.Millisecond,
		MaxPartSize:     50 * 1024 * 1024,
		SegmentDuration: time.Hour,
		PathName:        "mypath",
		Stream:          strm,
		Window:          window,
		Clock:           clk,
		OnSegmentCreate: func(string) {
			select {
			case segCreated <- struct{}{}:
			default:
			}
		},
		OnSegmentComplete: func(string, time.Duration) {
			select {
			case segDone <- struct{}{}:
			default:
			}
		},
		onInstanceReady: func() {
			select {
			case instanceReady <- struct{}{}:
			default:
			}
		},
		Parent:       test.NilLogger,
		restartPause: 5 * time.Millisecond,
	}
	w.Initialize()
	t.Cleanup(w.Close)

	return w, segCreated, segDone, instanceReady
}

func waitReady(t *testing.T, ready <-chan struct{}) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for recorder instance")
	}
}

// TestRecorderWindowSameDay verifies that a continuously online stream records
// only inside a same-day window, creates a segment when the window opens and
// finalizes it when the window closes.
func TestRecorderWindowSameDay(t *testing.T) {
	for _, format := range []conf.RecordFormat{conf.RecordFormatFMP4, conf.RecordFormatMPEGTS} {
		t.Run(string(format), func(t *testing.T) {
			strm, sub, media := newWindowTestStream(t)
			baseDir := t.TempDir()
			dir := filepath.Join(baseDir, "mypath")

			window, err := recordwindow.Parse("10:00-11:00")
			require.NoError(t, err)

			start := time.Date(2026, time.November, 25, 9, 30, 0, 0, time.UTC)
			clk := newFakeClock(start)
			_, segCreated, segDone, ready := newWindowRecorder(t, strm, baseDir, window, clk,
				&windowRecorderOpts{format: format})
			waitTimers(t, clk, 1)

			mw := &mediaWriter{sub: sub, media: media, ntp: start}

			// samples before the window open: no file may appear
			mw.tickN(5)
			time.Sleep(150 * time.Millisecond)
			require.Empty(t, segmentFiles(t, dir), "a segment was created outside the window")

			// window opens: wait for the reader to be registered before writing
			clk.Advance(30 * time.Minute) // 10:00
			waitReady(t, ready)
			mw.tickN(3)
			waitSegmentEvent(t, segCreated, "segment create at window start")

			// window closes: segment is finalized
			clk.Advance(time.Hour) // 11:00
			waitSegmentEvent(t, segDone, "segment complete at window end")

			// samples after close are discarded and the file must not grow
			mw.tickN(3)
			clk.Advance(30 * time.Minute)
			mw.tickN(3)

			files := waitSegmentCount(t, dir, 1)
			requireFileSizeStable(t, files[0])
			require.Greater(t, fileSize(t, files[0]), int64(0))

			if format == conf.RecordFormatFMP4 {
				// Three samples were written inside the window; the recorder
				// delays each sample by one, so the last in-flight sample is
				// not part of the finalized segment: 2 are stored. This proves
				// that outside-window samples never reach the file.
				require.Equal(t, 2, countFMP4Samples(t, files[0]))
			}
		})
	}
}

// TestRecorderWindowCrossMidnight verifies a window spanning midnight: one
// continuous stream keeps a single segment across the day boundary and it is
// finalized at the end of the window on the next day.
func TestRecorderWindowCrossMidnight(t *testing.T) {
	strm, sub, media := newWindowTestStream(t)
	baseDir := t.TempDir()
	dir := filepath.Join(baseDir, "mypath")

	window, err := recordwindow.Parse("23:30-00:30")
	require.NoError(t, err)

	start := time.Date(2026, time.November, 25, 23, 20, 0, 0, time.UTC)
	clk := newFakeClock(start)
	_, segCreated, segDone, ready := newWindowRecorder(t, strm, baseDir, window, clk, nil)
	waitTimers(t, clk, 1)

	mw := &mediaWriter{sub: sub, media: media, ntp: start}

	mw.tickN(3)
	time.Sleep(100 * time.Millisecond)
	require.Empty(t, segmentFiles(t, dir))

	// 23:30 opens
	clk.Advance(10 * time.Minute)
	waitReady(t, ready)
	mw.tickN(3)
	waitSegmentEvent(t, segCreated, "segment create at window start")

	// cross midnight while recording
	clk.Advance(25 * time.Minute) // 23:55
	mw.tickN(2)
	clk.Advance(10 * time.Minute) // 00:05, Nov 26
	mw.tickN(2)

	time.Sleep(100 * time.Millisecond)
	files := waitSegmentCount(t, dir, 1)
	require.Contains(t, filepath.Base(files[0]), "2026-11-25")

	// 00:30 closes on the next day
	clk.Advance(25 * time.Minute)
	waitSegmentEvent(t, segDone, "segment complete at cross-midnight window end")

	mw.tickN(2)
	clk.Advance(10 * time.Minute)
	mw.tickN(2)

	files = waitSegmentCount(t, dir, 1)
	requireFileSizeStable(t, files[0])
	require.Equal(t, 6, countFMP4Samples(t, files[0]))
}

// TestRecorderWindowReopens verifies that a still-online publisher gets a new,
// independent segment every time the window opens again, without reconnecting.
func TestRecorderWindowReopens(t *testing.T) {
	strm, sub, media := newWindowTestStream(t)
	baseDir := t.TempDir()
	dir := filepath.Join(baseDir, "mypath")

	window, err := recordwindow.Parse("10:00-11:00")
	require.NoError(t, err)

	// start inside the first window: instance is created synchronously
	start := time.Date(2026, time.November, 25, 10, 30, 0, 0, time.UTC)
	clk := newFakeClock(start)
	_, segCreated, segDone, ready := newWindowRecorder(t, strm, baseDir, window, clk, nil)
	waitReady(t, ready) // initial instance
	waitTimers(t, clk, 1)

	mw := &mediaWriter{sub: sub, media: media, ntp: start}

	mw.tickN(3)
	waitSegmentEvent(t, segCreated, "first segment create")

	clk.Advance(30 * time.Minute) // 11:00
	waitSegmentEvent(t, segDone, "first segment complete")
	first := waitSegmentCount(t, dir, 1)
	require.Equal(t, 2, countFMP4Samples(t, first[0]))

	// publisher keeps streaming through the closed window
	clk.Advance(30 * time.Minute) // 11:30
	mw.tickN(3)
	clk.Advance(22 * time.Hour) // next day 09:30
	mw.tickN(3)
	time.Sleep(100 * time.Millisecond)
	require.Len(t, segmentFiles(t, dir), 1)

	// next day 10:00 reopens
	clk.Advance(30 * time.Minute)
	waitReady(t, ready)
	mw.tickN(3)
	waitSegmentEvent(t, segCreated, "second segment create")

	clk.Advance(time.Hour) // 11:00
	waitSegmentEvent(t, segDone, "second segment complete")

	second := waitSegmentCount(t, dir, 2)
	for _, p := range second {
		require.Equal(t, 2, countFMP4Samples(t, p), "segment %s has unexpected content", p)
	}
}

// TestRecorderWithoutWindow verifies the legacy behavior: no window means the
// recorder starts immediately regardless of the clock position.
func TestRecorderWithoutWindow(t *testing.T) {
	strm, sub, media := newWindowTestStream(t)
	baseDir := t.TempDir()
	dir := filepath.Join(baseDir, "mypath")

	start := time.Date(2026, time.November, 25, 3, 0, 0, 0, time.UTC)
	clk := newFakeClock(start)
	_, _, _, ready := newWindowRecorder(t, strm, baseDir, nil, clk, nil)
	waitReady(t, ready)

	mw := &mediaWriter{sub: sub, media: media, ntp: start}
	mw.tickN(4)

	files := waitSegmentCount(t, dir, 1)

	// The segment file is created before its first part is flushed, so poll
	// until it contains at least one valid sample.
	require.Eventually(t, func() bool {
		n, ok := tryCountFMP4Samples(files[0])
		return ok && n > 0
	}, 3*time.Second, 10*time.Millisecond, "segment never received samples")
}

// tryCountFMP4Samples is a non-fatal variant of countFMP4Samples: it returns
// false while the segment is still being written and cannot be parsed yet.
func tryCountFMP4Samples(path string) (int, bool) {
	byts, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}

	var parts fmp4.Parts
	if err := parts.Unmarshal(byts); err != nil {
		return 0, false
	}

	n := 0
	for _, part := range parts {
		for _, track := range part.Tracks {
			n += len(track.Samples)
		}
	}
	return n, true
}

// TestRecorderWindowRestartKeepsBoundaryTimer verifies that when an instance
// terminates on its own (time-drift reset) and is restarted automatically, the
// window boundary timer is still armed and closes the new instance.
func TestRecorderWindowRestartKeepsBoundaryTimer(t *testing.T) {
	strm, sub, media := newWindowTestStream(t)
	baseDir := t.TempDir()
	dir := filepath.Join(baseDir, "mypath")

	window, err := recordwindow.Parse("10:00-11:00")
	require.NoError(t, err)

	start := time.Date(2026, time.November, 25, 10, 0, 0, 0, time.UTC)
	clk := newFakeClock(start)
	_, segCreated, segDone, ready := newWindowRecorder(t, strm, baseDir, window, clk, nil)
	waitReady(t, ready) // initial instance
	waitTimers(t, clk, 1)

	mw := &mediaWriter{sub: sub, media: media, ntp: start}

	mw.tickN(3)
	waitSegmentEvent(t, segCreated, "first segment create")
	drainEvents(segDone)

	// Trip the drift detector with a forward NTP jump: the instance exits and
	// finalizes its segment.
	mw.ntp = mw.ntp.Add(60 * time.Second)
	mw.tickN(3)
	waitSegmentEvent(t, segDone, "drift-reset segment complete")

	// The recorder restarts the instance after restartPause (real 5ms).
	waitReady(t, ready)
	drainEvents(segCreated)
	drainEvents(segDone)

	// Give the restarted reader a moment to settle, then open a segment.
	mw.tickN(3)
	waitSegmentEvent(t, segCreated, "restarted segment create")

	// Reach the window end: the boundary timer must still be armed.
	clk.Advance(time.Hour) // 11:00
	waitSegmentEvent(t, segDone, "window end after instance restart")

	closedCount := len(waitSegmentCount(t, dir, 2))

	// No additional segment may appear after the window closed.
	mw.tickN(4)
	time.Sleep(200 * time.Millisecond)
	require.Len(t, segmentFiles(t, dir), closedCount)
}

// TestRecorderWindowPlayback records a bounded window and verifies the
// finalized segment is listed and served by the playback server.
func TestRecorderWindowPlayback(t *testing.T) {
	strm, sub, media := newWindowTestStream(t)
	baseDir := t.TempDir()

	window, err := recordwindow.Parse("10:00-11:00")
	require.NoError(t, err)

	// File names use local time, like the production recorder, so
	// recordstore indexes them consistently.
	start := time.Date(2026, time.January, 15, 10, 0, 0, 0, time.UTC)
	clk := newFakeClock(start)
	_, segCreated, segDone, ready := newWindowRecorder(t, strm, baseDir, window, clk,
		&windowRecorderOpts{localTime: true})
	waitReady(t, ready)
	waitTimers(t, clk, 1)

	mw := &mediaWriter{sub: sub, media: media, ntp: start}
	mw.tickN(4)
	waitSegmentEvent(t, segCreated, "segment create")

	clk.Advance(time.Hour) // 11:00
	waitSegmentEvent(t, segDone, "segment complete")

	files := waitSegmentCount(t, filepath.Join(baseDir, "mypath"), 1)
	require.Greater(t, countFMP4Samples(t, files[0]), 0)

	// start the playback server on a free port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	srv := &playback.Server{
		Address:      addr,
		ReadTimeout:  conf.Duration(10 * time.Second),
		WriteTimeout: conf.Duration(10 * time.Second),
		PathConfs: map[string]*conf.Path{
			"mypath": {
				Name:         "mypath",
				RecordPath:   filepath.Join(baseDir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
				RecordFormat: conf.RecordFormatFMP4,
			},
		},
		AuthManager: test.NilAuthManager,
		Parent:      test.NilLogger,
	}
	require.NoError(t, srv.Initialize())
	defer srv.Close()

	hc := &http.Client{Timeout: 5 * time.Second}

	listURL := "http://" + addr + "/list?path=mypath"
	res, err := hc.Get(listURL)
	require.NoError(t, err)
	defer res.Body.Close()
	listBody, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.Equalf(t, http.StatusOK, res.StatusCode, "list failed: %s", listBody)

	var entries []struct {
		Start    string  `json:"start"`
		Duration float64 `json:"duration"`
	}
	require.NoError(t, json.Unmarshal(listBody, &entries), string(listBody))
	require.Len(t, entries, 1)
	require.Greater(t, entries[0].Duration, float64(0))

	getURL := "http://" + addr + "/get?path=mypath&format=fmp4&start=" +
		url.QueryEscape(entries[0].Start) + "&duration=3600"
	res2, err := hc.Get(getURL)
	require.NoError(t, err)
	defer res2.Body.Close()
	getBody, err := io.ReadAll(res2.Body)
	require.NoError(t, err)
	require.Equalf(t, http.StatusOK, res2.StatusCode, "get failed: %s", truncate(getBody))
	require.Contains(t, string(getBody), "ftyp")
	require.Contains(t, string(getBody), "moof")
}

func fileSize(t *testing.T, p string) int64 {
	t.Helper()
	st, err := os.Stat(p)
	require.NoError(t, err)
	return st.Size()
}

func truncate(b []byte) string {
	if len(b) > 300 {
		return string(b[:300])
	}
	return string(b)
}
