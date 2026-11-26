package mpegts_test

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	mcmpegts "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	tscodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
	"github.com/stretchr/testify/require"

	protmpegts "github.com/bluenviron/mediamtx/internal/protocols/mpegts"
)

func uid66H265AU() [][]byte {
	return [][]byte{
		{0x40, 0x01, 0x0c, 0x01},
		{0x42, 0x01, 0x01, 0x01, 0x60, 0x00, 0x00, 0x03, 0x00, 0x90, 0x00, 0x00, 0x03, 0x00, 0x00},
		{0x44, 0x01, 0xc0, 0xf7, 0xc0, 0xcc, 0x90},
		{0x26, 0x01, 0xaf, 0x08},
	}
}

// uid66BuildSegment builds a self-contained MPEG-TS segment with one H265
// track and, optionally, one AAC track.
func uid66BuildSegment(t *testing.T, withAAC bool) []byte {
	t.Helper()

	video := &mcmpegts.Track{PID: 0x100, Codec: &tscodecs.H265{}}
	tracks := []*mcmpegts.Track{video}

	var audio *mcmpegts.Track
	if withAAC {
		audio = &mcmpegts.Track{
			PID: 0x101,
			Codec: &tscodecs.MPEG4Audio{
				Config: mpeg4audio.Config{
					Type:          mpeg4audio.ObjectTypeAACLC,
					SampleRate:    48000,
					ChannelCount:  2,
					ChannelConfig: 2,
				},
			},
		}
		tracks = append(tracks, audio)
	}

	var buf bytes.Buffer
	w := &mcmpegts.Writer{W: &buf, Tracks: tracks}
	require.NoError(t, w.Initialize())

	aacAU := []byte{0x21, 0x10, 0x05, 0x00, 0x20, 0x00, 0x1b, 0x80}

	for i := 0; i < 8; i++ {
		_, err := w.WriteTables()
		require.NoError(t, err)
		require.NoError(t, w.WriteH265(video, int64(i*3600), int64(i*3600), uid66H265AU()))
		if withAAC {
			require.NoError(t, w.WriteMPEG4Audio(audio, int64(i*3600), [][]byte{aacAU, aacAU}))
		}
	}

	return buf.Bytes()
}

func uid66CorruptPrefix(in []byte, n int) []byte {
	out := append([]byte(nil), in...)
	if n > len(out) {
		n = len(out)
	}
	for i := 0; i < n; i++ {
		out[i] = 0
	}
	return out
}

// With analysis disabled, the first (video-only) PMT wins and the later AAC
// track is not declared: this reproduces the premature determination.
func TestUID66DisabledKeepsLegacyBehavior(t *testing.T) {
	videoOnly := uid66BuildSegment(t, false)
	both := uid66BuildSegment(t, true)
	combined := append(append([]byte{}, videoOnly...), both...)

	mr, stop, err := protmpegts.AnalyzeReader(bytes.NewReader(combined), protmpegts.StartupAnalysis{})
	require.NoError(t, err)
	defer stop()
	require.Len(t, mr.Tracks(), 1)
	require.IsType(t, &tscodecs.H265{}, mr.Tracks()[0].Codec)
}

// With analysis enabled, the complete later PMT is selected and both tracks
// are detected.
func TestUID66EnabledDetectsH265AndAAC(t *testing.T) {
	videoOnly := uid66BuildSegment(t, false)
	both := uid66BuildSegment(t, true)
	combined := append(append([]byte{}, videoOnly...), both...)

	mr, stop, err := protmpegts.AnalyzeReader(bytes.NewReader(combined), protmpegts.StartupAnalysis{
		Duration: 200 * time.Millisecond,
	})
	require.NoError(t, err)
	defer stop()

	var hasH265, hasAAC bool
	var audioTrack *mcmpegts.Track
	for _, tr := range mr.Tracks() {
		switch tr.Codec.(type) {
		case *tscodecs.H265:
			hasH265 = true
		case *tscodecs.MPEG4Audio:
			hasAAC = true
			audioTrack = tr
		}
	}
	require.True(t, hasH265)
	require.True(t, hasAAC)
	require.Len(t, mr.Tracks(), 2)

	// Audio packets must be delivered through the declared track and must not
	// be permanently rejected as an undeclared PID.
	audioUnits := 0
	mr.OnDataMPEG4Audio(audioTrack, func(_ int64, _ [][]byte) error {
		audioUnits++
		return nil
	})

	decodeErr := 0
	mr.OnDecodeError(func(error) { decodeErr++ })

	for i := 0; i < 100000; i++ {
		err := mr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
	}
	require.Greater(t, audioUnits, 0, "later AAC must be decoded after analysis")
}

// Garbage bytes and zeroed leading packets do not prevent detection of a
// later complete PMT.
func TestUID66SkipsCorruptedLeadingPackets(t *testing.T) {
	both := uid66BuildSegment(t, true)

	garbage := []byte{0x99, 0x12, 0xf0, 0x47, 0x00, 0x55, 0xaa, 0xff}
	damaged := append(append([]byte{}, garbage...), uid66CorruptPrefix(both, 3*188)...)

	mr, stop, err := protmpegts.AnalyzeReader(bytes.NewReader(damaged), protmpegts.StartupAnalysis{
		Duration: 100 * time.Millisecond,
	})
	require.NoError(t, err)
	defer stop()

	var hasAAC bool
	for _, tr := range mr.Tracks() {
		if _, ok := tr.Codec.(*tscodecs.MPEG4Audio); ok {
			hasAAC = true
		}
	}
	require.True(t, hasAAC)
}

// A genuinely video-only stream yields one track and no error.
func TestUID66VideoOnly(t *testing.T) {
	videoOnly := uid66BuildSegment(t, false)

	mr, stop, err := protmpegts.AnalyzeReader(bytes.NewReader(videoOnly), protmpegts.StartupAnalysis{
		Duration: 50 * time.Millisecond,
	})
	require.NoError(t, err)
	defer stop()
	require.Len(t, mr.Tracks(), 1)
}

// Persistent garbage leads to a deterministic error.
func TestUID66PersistentGarbage(t *testing.T) {
	garbage := bytes.Repeat([]byte{0x00, 0x01, 0x02, 0x03}, 10000)

	_, stop, err := protmpegts.AnalyzeReader(bytes.NewReader(garbage), protmpegts.StartupAnalysis{
		Duration: 50 * time.Millisecond,
	})
	require.Error(t, err)
	stop()
}

type uid66ErrAfterReader struct {
	data []byte
	pos  int
	err  error
}

func (r *uid66ErrAfterReader) Read(p []byte) (int, error) {
	if r.pos < len(r.data) {
		n := copy(p, r.data[r.pos:])
		r.pos += n
		return n, nil
	}
	return 0, r.err
}

// A source that disconnects after its data: buffered data is replayed and the
// non-EOF disconnect error is surfaced by the read loop.
func TestUID66SourceDisconnect(t *testing.T) {
	disconnectErr := errors.New("connection reset by peer")
	both := uid66BuildSegment(t, true)
	src := &uid66ErrAfterReader{data: both, err: disconnectErr}

	mr, stop, err := protmpegts.AnalyzeReader(src, protmpegts.StartupAnalysis{
		Duration: time.Second,
	})
	require.NoError(t, err)
	defer stop()

	var gotErr error
	for i := 0; i < 100000; i++ {
		gotErr = mr.Read()
		if gotErr != nil {
			break
		}
	}
	require.ErrorIs(t, gotErr, disconnectErr)
}

type uid66BlockingReader struct {
	ch chan struct{}
}

func (r *uid66BlockingReader) Read(_ []byte) (int, error) {
	<-r.ch
	return 0, io.EOF
}

// The window expires even when the source never delivers any data.
func TestUID66WindowExpiryOnSilentSource(t *testing.T) {
	src := &uid66BlockingReader{ch: make(chan struct{})}
	t.Cleanup(func() { close(src.ch) })

	start := time.Now()
	_, stop, err := protmpegts.AnalyzeReader(src, protmpegts.StartupAnalysis{
		Duration: 80 * time.Millisecond,
	})
	elapsed := time.Since(start)
	stop()
	require.Error(t, err)
	require.GreaterOrEqual(t, elapsed, 60*time.Millisecond)
	require.Less(t, elapsed, 2*time.Second)
}

// The buffer is bounded even if the source provides unlimited data instantly.
func TestUID66BoundedBuffer(t *testing.T) {
	maxBuf := 64 * 1024
	src := bytes.NewReader(bytes.Repeat(uid66BuildSegment(t, true), 100))

	start := time.Now()
	mr, stop, err := protmpegts.AnalyzeReader(src, protmpegts.StartupAnalysis{
		Duration:      time.Second,
		MaxBufferSize: maxBuf,
	})
	require.NoError(t, err)
	defer stop()
	require.Less(t, time.Since(start), 500*time.Millisecond)
	require.Len(t, mr.Tracks(), 2)
}
