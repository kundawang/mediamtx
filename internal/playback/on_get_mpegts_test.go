package playback

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	tscodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/test"
)

// test H264 parameter sets and a couple of distinct IDR/non-IDR NALUs.
// The SPS/PPS come from the shared test format and are valid enough for the
// fMP4 muxer to serialize an avcC box.
var (
	testTSSPS = append([]byte(nil), test.FormatH264.SPS...)
	testTSPPS = append([]byte(nil), test.FormatH264.PPS...)
	testTSIDR = []byte{0x65, 0x01, 0x02}
	testTSP   = []byte{0x41, 0x03, 0x04}
)

type tsVideoFrame struct {
	idr     bool
	payload byte
}

func tsFrames(payloads ...byte) []tsVideoFrame {
	out := make([]tsVideoFrame, len(payloads))
	for i, p := range payloads {
		out[i] = tsVideoFrame{idr: i == 0, payload: p} // first frame is an IDR
	}
	return out
}

// writeMPEGTSSegmentFile writes a short H264 + AAC MPEG-TS segment with one
// video frame and one AAC frame every 100ms, starting at basePTS90k.
func writeMPEGTSSegmentFile(
	t *testing.T,
	fpath string,
	basePTS90k int64,
	videoFrames []tsVideoFrame,
	aacConfig mpeg4audio.AudioSpecificConfig,
) {
	t.Helper()

	videoTrack := &mpegts.Track{Codec: &tscodecs.H264{}}
	audioTrack := &mpegts.Track{Codec: &tscodecs.MPEG4Audio{Config: aacConfig}}

	var buf bytes.Buffer
	w := &mpegts.Writer{W: &buf, Tracks: []*mpegts.Track{videoTrack, audioTrack}}
	require.NoError(t, w.Initialize())

	for i, frame := range videoFrames {
		pts := basePTS90k + int64(i)*9000 // 100ms at 90kHz

		var au [][]byte
		if frame.idr {
			// parameter sets are repeated on every IDR, as a real encoder does
			au = append(au, testTSSPS, testTSPPS,
				append(append([]byte(nil), testTSIDR...), frame.payload))
		} else {
			au = append(au, append(append([]byte(nil), testTSP...), frame.payload))
		}

		require.NoError(t, w.WriteH264(videoTrack, pts, pts, au))
		require.NoError(t, w.WriteMPEG4Audio(audioTrack, pts,
			[][]byte{{byte(basePTS90k), byte(i)}}))
	}

	require.NoError(t, os.WriteFile(fpath, buf.Bytes(), 0o644))
}

func mpegtsTestServer(t *testing.T, dir string) *Server {
	t.Helper()
	s := &Server{
		Address:      "127.0.0.1:9998",
		ReadTimeout:  conf.Duration(10 * time.Second),
		WriteTimeout: conf.Duration(10 * time.Second),
		PathConfs: map[string]*conf.Path{
			"mypath": {
				Name:         "mypath",
				RecordPath:   filepath.Join(dir, "%path/%Y-%m-%d_%H-%M-%S-%f"),
				RecordFormat: conf.RecordFormatMPEGTS,
			},
		},
		AuthManager: test.NilAuthManager,
		Parent:      test.NilLogger,
	}
	require.NoError(t, s.Initialize())
	return s
}

func mpegtsRequest(t *testing.T, query url.Values) (int, []byte) {
	t.Helper()
	u, _ := url.Parse("http://localhost:9998/get")
	u.RawQuery = query.Encode()
	res, err := http.Get(u.String())
	require.NoError(t, err)
	defer res.Body.Close()
	buf, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return res.StatusCode, buf
}

func mpegtsGet(t *testing.T, start time.Time, duration, format string) []byte {
	t.Helper()
	v := url.Values{}
	v.Set("path", "mypath")
	v.Set("start", start.Format(time.RFC3339Nano))
	v.Set("duration", duration)
	v.Set("format", format)
	status, buf := mpegtsRequest(t, v)
	require.Equal(t, http.StatusOK, status, "body: %s", string(buf))
	return buf
}

// parseFMP4 returns the init and the per-track sample payloads, in order.
func parseFMP4(t *testing.T, buf []byte) (*fmp4.Init, map[int][][]byte) {
	t.Helper()

	var init fmp4.Init
	require.NoError(t, init.Unmarshal(bytes.NewReader(buf)))

	var parts fmp4.Parts
	require.NoError(t, parts.Unmarshal(buf))

	payloads := map[int][][]byte{}
	for _, part := range parts {
		for _, pt := range part.Tracks {
			for _, sample := range pt.Samples {
				payloads[pt.ID] = append(payloads[pt.ID], sample.Payload)
			}
		}
	}
	return &init, payloads
}

func TestPlaybackMPEGTS(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "mypath"), 0o755))

	aacConfig := mpeg4audio.AudioSpecificConfig{
		Type:          mpeg4audio.ObjectTypeAACLC,
		SampleRate:    48000,
		ChannelCount:  2, //nolint:staticcheck
		ChannelConfig: 2,
	}

	frames := tsFrames

	// two contiguous 500ms segments, five 100ms frames each.
	writeMPEGTSSegmentFile(t,
		filepath.Join(dir, "mypath", "2008-11-07_11-22-00-000000.ts"),
		0, tsFrames(0xA0, 0xA1, 0xA2, 0xA3, 0xA4), aacConfig)
	writeMPEGTSSegmentFile(t,
		filepath.Join(dir, "mypath", "2008-11-07_11-22-00-500000.ts"),
		45000, frames(0xB0, 0xB1, 0xB2, 0xB3, 0xB4), aacConfig)

	s := mpegtsTestServer(t, dir)
	defer s.Close()

	start := time.Date(2008, 11, 7, 11, 22, 0, 0, time.Local)

	t.Run("full range fMP4", func(t *testing.T) {
		buf := mpegtsGet(t, start, "2", "fmp4")
		init, payloads := parseFMP4(t, buf)

		videoID, audioID := 0, 0
		for _, tr := range init.Tracks {
			if tr.Codec.IsVideo() {
				videoID = tr.ID
			} else {
				audioID = tr.ID
			}
		}
		require.NotZero(t, videoID)
		require.NotZero(t, audioID)

		// 10 video frames and 10 AAC frames across the two segments
		require.Len(t, payloads[videoID], 10)
		require.Len(t, payloads[audioID], 10)

		// video frame order is preserved across the segment boundary:
		// each AVCC sample ends with its distinct marker byte
		var markers []byte
		for _, p := range payloads[videoID] {
			markers = append(markers, p[len(p)-1])
		}
		require.Equal(t, []byte{0xA0, 0xA1, 0xA2, 0xA3, 0xA4,
			0xB0, 0xB1, 0xB2, 0xB3, 0xB4}, markers)
	})

	t.Run("first key frame", func(t *testing.T) {
		// start 300ms in: the output must begin at an IDR, not a P frame
		buf := mpegtsGet(t, start.Add(300*time.Millisecond), "1", "fmp4")

		var parts fmp4.Parts
		require.NoError(t, parts.Unmarshal(buf))

		var firstVideo *fmp4.Sample
		for _, part := range parts {
			for _, pt := range part.Tracks {
				if pt.ID != 1 {
					continue
				}
				for _, sample := range pt.Samples {
					firstVideo = sample
					break
				}
				break
			}
			if firstVideo != nil {
				break
			}
		}
		require.NotNil(t, firstVideo)
		require.False(t, firstVideo.IsNonSyncSample, "output must start with a sync sample")
	})

	t.Run("mp4 output", func(t *testing.T) {
		buf := mpegtsGet(t, start, "2", "mp4")
		require.NotEmpty(t, buf)
		require.Equal(t, []byte{'f', 't', 'y', 'p'}, buf[4:8])
	})

	t.Run("list round trip", func(t *testing.T) {
		u, _ := url.Parse("http://localhost:9998/list")
		v := url.Values{}
		v.Set("path", "mypath")
		u.RawQuery = v.Encode()

		res, err := http.Get(u.String())
		require.NoError(t, err)
		defer res.Body.Close()
		require.Equal(t, http.StatusOK, res.StatusCode)

		var entries []map[string]any
		require.NoError(t, json.NewDecoder(res.Body).Decode(&entries))
		require.Len(t, entries, 1, "contiguous segments are merged into one entry")

		// the URL returned by /list must be playable and yield the frames of
		// the listed range (the exact count follows the listed duration, which
		// ends on the last sample boundary and may trim the final frame)
		getURL, _ := url.Parse(entries[0]["url"].(string))
		res2, err := http.Get(getURL.String())
		require.NoError(t, err)
		gbuf, err := io.ReadAll(res2.Body)
		require.NoError(t, err)
		res2.Body.Close()
		require.Equal(t, http.StatusOK, res2.StatusCode)

		_, payloads := parseFMP4(t, gbuf)
		require.NotEmpty(t, payloads[1])
		require.LessOrEqual(t, len(payloads[1]), 10)

		// every returned frame keeps its original order and marker
		var markers []byte
		for _, p := range payloads[1] {
			markers = append(markers, p[len(p)-1])
		}
		require.Equal(t, []byte{0xA0, 0xA1, 0xA2, 0xA3, 0xA4,
			0xB0, 0xB1, 0xB2, 0xB3}[:len(markers)], markers)
	})

	t.Run("empty range", func(t *testing.T) {
		v := url.Values{}
		v.Set("path", "mypath")
		v.Set("start", time.Date(2030, 1, 1, 0, 0, 0, 0, time.Local).Format(time.RFC3339Nano))
		v.Set("duration", "2")
		v.Set("format", "fmp4")
		status, body := mpegtsRequest(t, v)
		require.Equal(t, http.StatusNotFound, status, "body: %s", string(body))
	})

	t.Run("corrupted segment", func(t *testing.T) {
		// a third, corrupted segment after the two valid ones
		corrupt := filepath.Join(dir, "mypath", "2008-11-07_11-22-02-000000.ts")
		require.NoError(t, os.WriteFile(corrupt, []byte("not a valid MPEG-TS file"), 0o644))

		// requesting across the corruption still returns the valid prefix
		buf := mpegtsGet(t, start, "5", "fmp4")
		_, payloads := parseFMP4(t, buf)
		require.Len(t, payloads[1], 10, "playback stops at the last valid segment")

		// a range served only by the corrupted segment -> 400 with a clear message
		v := url.Values{}
		v.Set("path", "mypath")
		v.Set("start", time.Date(2030, 1, 1, 0, 0, 0, 0, time.Local).Format(time.RFC3339Nano))
		v.Set("duration", "2")
		v.Set("format", "fmp4")
		status, body := mpegtsRequest(t, v)
		require.Equal(t, http.StatusBadRequest, status)
		require.Contains(t, string(body), "unable to read segment")
	})
}

// TestPlaybackMPEGTSOverlap makes two consecutive segments share a frame at the
// boundary (the same absolute PTS is written by both). The playback output must
// contain it exactly once: no duplicate timestamps or samples.
func TestPlaybackMPEGTSOverlap(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "mypath"), 0o755))

	aacConfig := mpeg4audio.AudioSpecificConfig{
		Type:          mpeg4audio.ObjectTypeAACLC,
		SampleRate:    48000,
		ChannelCount:  2, //nolint:staticcheck
		ChannelConfig: 2,
	}

	// segment 1: frames at 0,100,200,300,400ms
	writeMPEGTSSegmentFile(t,
		filepath.Join(dir, "mypath", "2008-11-07_11-22-00-000000.ts"),
		0, tsFrames(0xA0, 0xA1, 0xA2, 0xA3, 0xA4), aacConfig)

	// segment 2 restarts its own timestamps at 0 (basePTS=0) but is stored
	// 400ms later, so its first frame collides with segment 1's 400ms frame.
	writeMPEGTSSegmentFile(t,
		filepath.Join(dir, "mypath", "2008-11-07_11-22-00-400000.ts"),
		0, tsFrames(0xB0, 0xB1, 0xB2, 0xB3), aacConfig)

	s := mpegtsTestServer(t, dir)
	defer s.Close()

	start := time.Date(2008, 11, 7, 11, 22, 0, 0, time.Local)
	buf := mpegtsGet(t, start, "2", "fmp4")
	_, payloads := parseFMP4(t, buf)

	// 5 frames from segment 1 + 4 from segment 2, with no duplicated frame:
	// the boundary frame is emitted once, markers stay strictly ordered.
	var markers []byte
	for _, p := range payloads[1] {
		markers = append(markers, p[len(p)-1])
	}
	require.Equal(t, []byte{0xA0, 0xA1, 0xA2, 0xA3, 0xA4,
		0xB1, 0xB2, 0xB3}, markers)

	// no duplicate DTS in the resulting parts: reconstruct each sample DTS from
	// its part BaseTime and the cumulative sample durations.
	var parts fmp4.Parts
	require.NoError(t, parts.Unmarshal(buf))
	seen := map[uint64]struct{}{}
	for _, part := range parts {
		for _, pt := range part.Tracks {
			if pt.ID != 1 {
				continue
			}
			dts := pt.BaseTime
			for _, sample := range pt.Samples {
				_, dup := seen[dts]
				require.False(t, dup, "duplicate DTS %d", dts)
				seen[dts] = struct{}{}
				dts += uint64(sample.Duration)
			}
		}
	}
}

// TestPlaybackMPEGTSBFrames writes video samples whose PTS and DTS differ
// (the B-frame signature) and checks that the fMP4 output keeps the correct
// PTS composition offsets while ordering samples by DTS.
func TestPlaybackMPEGTSBFrames(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "mypath"), 0o755))

	videoTrack := &mpegts.Track{Codec: &tscodecs.H264{}}

	var buf bytes.Buffer
	w := &mpegts.Writer{W: &buf, Tracks: []*mpegts.Track{videoTrack}}
	require.NoError(t, w.Initialize())

	// decode order (DTS): 0,1,2 ; display order (PTS): 0,2,1
	// the second frame is a B-frame presented after the third.
	type frame struct {
		pts, dts int64
		idr      bool
		marker   byte
	}
	frames := []frame{
		{0, 0, true, 0xC0},
		{2 * 9000, 1 * 9000, false, 0xC1}, // B-frame: PTS one step ahead of DTS
		{1 * 9000, 2 * 9000, false, 0xC2},
	}

	for _, fr := range frames {
		var au [][]byte
		if fr.idr {
			au = append(au, testTSSPS, testTSPPS,
				append(append([]byte(nil), testTSIDR...), fr.marker))
		} else {
			au = append(au, append(append([]byte(nil), testTSP...), fr.marker))
		}
		require.NoError(t, w.WriteH264(videoTrack, fr.pts, fr.dts, au))
	}

	fpath := filepath.Join(dir, "mypath", "2008-11-07_11-22-00-000000.ts")
	require.NoError(t, os.WriteFile(fpath, buf.Bytes(), 0o644))

	s := mpegtsTestServer(t, dir)
	defer s.Close()

	start := time.Date(2008, 11, 7, 11, 22, 0, 0, time.Local)
	out := mpegtsGet(t, start, "2", "fmp4")

	var parts fmp4.Parts
	require.NoError(t, parts.Unmarshal(out))

	// collect samples in DTS order with their PTS offsets
	type got struct {
		marker    byte
		ptsOffset int32
		nonSync   bool
	}
	var gotSamples []got
	for _, part := range parts {
		for _, pt := range part.Tracks {
			if pt.ID != 1 {
				continue
			}
			for _, sample := range pt.Samples {
				gotSamples = append(gotSamples, got{
					marker:    sample.Payload[len(sample.Payload)-1],
					ptsOffset: sample.PTSOffset,
					nonSync:   sample.IsNonSyncSample,
				})
			}
		}
	}

	require.Len(t, gotSamples, 3)

	// samples follow decode (DTS) order: C0, C1, C2
	require.Equal(t, byte(0xC0), gotSamples[0].marker)
	require.Equal(t, byte(0xC1), gotSamples[1].marker)
	require.Equal(t, byte(0xC2), gotSamples[2].marker)

	// the first sample is a sync point, the others are not
	require.False(t, gotSamples[0].nonSync)
	require.True(t, gotSamples[1].nonSync)
	require.True(t, gotSamples[2].nonSync)

	// B-frame (C1) has a positive PTS offset of one 100ms step (9000 ticks),
	// and the following P-frame (C2) a negative offset of the same size.
	require.Equal(t, int32(0), gotSamples[0].ptsOffset)
	require.Equal(t, int32(9000), gotSamples[1].ptsOffset)
	require.Equal(t, int32(-9000), gotSamples[2].ptsOffset)
}
