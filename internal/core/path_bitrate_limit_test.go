package core

import (
	"bufio"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	tscodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
	srt "github.com/datarhei/gosrt"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/test"
)

// testBitrateLimit is the per-path threshold shared by the integration tests,
// in bytes per second. Rates below and above it are chosen with wide margins.
const testBitrateLimit = 120_000

// Dedicated ports keep these tests independent from default-port processes
// and from other test suites running on the same machine.
const (
	testRTSPPort = "18554"
	testRTSPBase = "rtsp://127.0.0.1:" + testRTSPPort + "/"
	testSRTAddr  = "127.0.0.1:18890"
	testAPIURL   = "http://127.0.0.1:19997"
)

// bitrateTestConf prepends isolated server settings to a paths block.
func bitrateTestConf(pathsBlock string, srtEnabled bool, apiEnabled bool) string {
	conf := "rtspTransports: [tcp]\n" +
		"rtspAddress: :18554\n" +
		"rtmp: no\n" +
		"hls: no\n" +
		"webrtc: no\n" +
		"moq: no\n"

	if srtEnabled {
		conf += "srt: yes\n" +
			"srtAddress: :18890\n"
	} else {
		conf += "srt: no\n"
	}

	if apiEnabled {
		conf += "api: yes\n" +
			"apiAddress: :19997\n"
	} else {
		conf += "api: no\n"
	}

	return conf + "paths:\n" + pathsBlock
}

func newTestTCPClient() *gortsplib.Client {
	proto := gortsplib.ProtocolTCP
	return &gortsplib.Client{Protocol: &proto}
}

// ---------------------------------------------------------------------------
// RTSP fixed byte-rate publisher
// ---------------------------------------------------------------------------

type rtspRatePub struct {
	cli    *gortsplib.Client
	fatal  chan error
	stopW  chan struct{}
	once   sync.Once
	failed atomic.Bool
}

func newRTSPRatePub(
	t *testing.T,
	pathName string,
	interval time.Duration,
	packetsPerTick int,
	payloadSize int,
) *rtspRatePub {
	t.Helper()

	medi := test.UniqueMediaH264()
	cli := newTestTCPClient()

	require.NoError(t, cli.StartRecording(
		testRTSPBase+pathName,
		&description.Session{Medias: []*description.Media{medi}}))

	p := &rtspRatePub{
		cli:   cli,
		fatal: make(chan error, 1),
		stopW: make(chan struct{}),
	}

	go func() {
		p.fatal <- cli.Wait()
	}()

	payload := make([]byte, payloadSize)
	payload[0] = 0x05 // H264 IDR NAL unit

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		var seq uint16
		for {
			select {
			case <-p.stopW:
				return

			case <-ticker.C:
				for range packetsPerTick {
					seq++
					err := cli.WritePacketRTP(medi, &rtp.Packet{
						Version:        2,
						Marker:         true,
						PayloadType:    96,
						SequenceNumber: seq,
						Timestamp:      uint32(seq) * 3000,
						SSRC:           563423,
						Payload:        payload,
					})
					if err != nil {
						p.failed.Store(true)
						return
					}
				}
			}
		}
	}()

	t.Cleanup(p.close)
	return p
}

func (p *rtspRatePub) close() {
	p.once.Do(func() {
		close(p.stopW)
		p.cli.Close()
	})
}

// assertAlive makes sure the publisher is not terminated for d.
func (p *rtspRatePub) assertAlive(t *testing.T, d time.Duration) {
	t.Helper()

	select {
	case err := <-p.fatal:
		t.Fatalf("publisher was terminated early: %v", err)
	case <-time.After(d):
	}

	require.False(t, p.failed.Load(), "publisher writes started failing")
}

// expectKicked waits for the server to explicitly terminate the publisher.
func (p *rtspRatePub) expectKicked(t *testing.T, within time.Duration) error {
	t.Helper()

	select {
	case err := <-p.fatal:
		require.Error(t, err, "publisher closed without an error")
		return err

	case <-time.After(within):
		t.Fatalf("publisher was not terminated within %s", within)
		return nil
	}
}

// ---------------------------------------------------------------------------
// RTSP packet reader
// ---------------------------------------------------------------------------

type rtspPacketReader struct {
	cli *gortsplib.Client
	n   atomic.Int64
}

func newRTSPPacketReader(t *testing.T, pathName string) *rtspPacketReader {
	t.Helper()

	u, err := base.ParseURL(testRTSPBase + pathName)
	require.NoError(t, err)

	r := &rtspPacketReader{
		cli: newTestTCPClient(),
	}
	r.cli.Scheme = u.Scheme
	r.cli.Host = u.Host

	require.NoError(t, r.cli.Start())

	desc, _, err := r.cli.Describe(u)
	require.NoError(t, err)

	require.NoError(t, r.cli.SetupAll(desc.BaseURL, desc.Medias))

	r.cli.OnPacketRTP(desc.Medias[0], desc.Medias[0].Formats[0], func(*rtp.Packet) {
		r.n.Add(1)
	})

	_, err = r.cli.Play(nil)
	require.NoError(t, err)

	t.Cleanup(r.cli.Close)
	return r
}

func (r *rtspPacketReader) count() int64 {
	return r.n.Load()
}

func (r *rtspPacketReader) waitPackets(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool { return r.count() > 0 },
		5*time.Second, 25*time.Millisecond, "reader did not receive any packet")
}

// ---------------------------------------------------------------------------
// SRT fixed byte-rate publisher
// ---------------------------------------------------------------------------

type srtRatePub struct {
	conn   srt.Conn
	kicked chan error
	stopW  chan struct{}
	once   sync.Once
	failed atomic.Bool
}

func newSRTRatePub(
	t *testing.T,
	pathName string,
	interval time.Duration,
	nalsPerTick int,
	nalSize int,
) *srtRatePub {
	t.Helper()

	conf := srt.DefaultConfig()
	conf.StreamId = "publish:" + pathName + ":::"

	conn, err := srt.Dial("srt", testSRTAddr, conf)
	require.NoError(t, err)

	track := &mpegts.Track{Codec: &tscodecs.H264{}}
	bw := bufio.NewWriter(conn)
	w := &mpegts.Writer{W: bw, Tracks: []*mpegts.Track{track}}
	require.NoError(t, w.Initialize())

	// the MPEG-TS muxer needs two PES packets in order to write the first one
	require.NoError(t, w.WriteH264(track, 0, 0, [][]byte{
		test.FormatH264.SPS,
		test.FormatH264.PPS,
		{5, 1},
	}))
	require.NoError(t, bw.Flush())
	require.NoError(t, w.WriteH264(track, 0, 0, [][]byte{{5, 2}}))
	require.NoError(t, bw.Flush())

	nal := make([]byte, nalSize)
	nal[0] = 0x05

	p := &srtRatePub{
		conn:   conn,
		kicked: make(chan error, 1),
		stopW:  make(chan struct{}),
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-p.stopW:
				return

			case <-ticker.C:
				_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
				var werr error
				for range nalsPerTick {
					werr = w.WriteH264(track, 0, 0, [][]byte{nal})
					if werr != nil {
						break
					}
				}
				if werr == nil {
					werr = bw.Flush()
				}
				if werr != nil {
					p.failed.Store(true)
					select {
					case p.kicked <- werr:
					default:
					}
					return
				}
			}
		}
	}()

	t.Cleanup(p.close)
	return p
}

func (p *srtRatePub) close() {
	p.once.Do(func() {
		close(p.stopW)
		p.conn.Close()
	})
}

func (p *srtRatePub) assertAlive(t *testing.T, d time.Duration) {
	t.Helper()

	select {
	case err := <-p.kicked:
		t.Fatalf("srt publisher was terminated early: %v", err)
	case <-time.After(d):
	}

	require.False(t, p.failed.Load(), "srt publisher writes started failing")
}

func (p *srtRatePub) expectKicked(t *testing.T, within time.Duration) error {
	t.Helper()

	select {
	case err := <-p.kicked:
		require.Error(t, err)
		return err

	case <-time.After(within):
		t.Fatalf("srt publisher was not terminated within %s", within)
		return nil
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// A low-rate RTSP publisher must not be limited, and its readers must keep
// receiving media. Outbound reader bytes are not counted.
func TestPathPublishBitrateLimitRTSPLowRateAndReader(t *testing.T) {
	p, ok := newInstance(t, bitrateTestConf(
		"  live:\n"+
			"    maxInboundBytesPerSecond: 120000\n", false, false))
	require.True(t, ok)
	defer p.Close()

	pub := newRTSPRatePub(t, "live", 40*time.Millisecond, 1, 1200) // ~30 KB/s
	reader := newRTSPPacketReader(t, "live")
	reader.waitPackets(t)

	before := reader.count()
	pub.assertAlive(t, 2100*time.Millisecond)
	require.Greater(t, reader.count(), before, "reader stopped receiving packets")
}

// A sustained high-rate RTSP publisher is explicitly terminated. After the
// kill, a new publisher on the same path starts with a fresh sliding window
// and can publish at a normal rate (recovery + publisher switch reset).
func TestPathPublishBitrateLimitRTSPKickAndRecover(t *testing.T) {
	// maxInboundBitrate is expressed in bits per second: 960000/8 = 120000 B/s.
	p, ok := newInstance(t, bitrateTestConf(
		"  live:\n"+
			"    maxInboundBitrate: 960000\n", false, false))
	require.True(t, ok)
	defer p.Close()

	high := newRTSPRatePub(t, "live", 10*time.Millisecond, 8, 1200) // ~950 KB/s
	high.expectKicked(t, 15*time.Second)
	high.close()

	// let the path process the publisher removal
	time.Sleep(500 * time.Millisecond)

	low := newRTSPRatePub(t, "live", 40*time.Millisecond, 1, 1200)
	low.assertAlive(t, 2500*time.Millisecond)
}

// Switching publisher gracefully resets the window: bytes of a first, fast
// publisher are never charged to the replacement.
func TestPathPublishBitrateLimitRTSPPublisherSwitch(t *testing.T) {
	p, ok := newInstance(t, bitrateTestConf(
		"  live:\n"+
			"    maxInboundBytesPerSecond: 120000\n", false, false))
	require.True(t, ok)
	defer p.Close()

	first := newRTSPRatePub(t, "live", 10*time.Millisecond, 8, 1200)
	time.Sleep(250 * time.Millisecond) // send a fast burst
	first.close()

	time.Sleep(200 * time.Millisecond)

	second := newRTSPRatePub(t, "live", 40*time.Millisecond, 1, 1200)
	second.assertAlive(t, 2200*time.Millisecond)
}

// The limit works for SRT publishers as well, with two fixed byte rates.
func TestPathPublishBitrateLimitSRT(t *testing.T) {
	p, ok := newInstance(t, bitrateTestConf(
		"  low:\n"+
			"    maxInboundBytesPerSecond: 120000\n"+
			"  high:\n"+
			"    maxInboundBytesPerSecond: 120000\n", true, false))
	require.True(t, ok)
	defer p.Close()

	low := newSRTRatePub(t, "low", 250*time.Millisecond, 1, 7000) // ~28 KB/s
	low.assertAlive(t, 2500*time.Millisecond)

	high := newSRTRatePub(t, "high", 25*time.Millisecond, 8, 7000) // ~2.2 MB/s
	high.expectKicked(t, 15*time.Second)
}

// Terminating an abusive publisher on one path must not affect an unlimited
// path, a differently limited path, or readers on the unaffected path.
func TestPathPublishBitrateLimitPathIsolation(t *testing.T) {
	p, ok := newInstance(t, bitrateTestConf(
		"  capped:\n"+
			"    maxInboundBytesPerSecond: 120000\n"+
			"  capped2:\n"+
			"    maxInboundBytesPerSecond: 120000\n"+
			"  open: {}\n", false, false))
	require.True(t, ok)
	defer p.Close()

	// unlimited path carrying a high rate and a reader
	openPub := newRTSPRatePub(t, "open", 10*time.Millisecond, 8, 1200)
	openReader := newRTSPPacketReader(t, "open")
	openReader.waitPackets(t)

	// normal-rate publisher on another capped path
	otherLow := newRTSPRatePub(t, "capped2", 40*time.Millisecond, 1, 1200)

	// abusive publisher on the capped path gets terminated
	bad := newRTSPRatePub(t, "capped", 10*time.Millisecond, 8, 1200)
	bad.expectKicked(t, 15*time.Second)

	// give side effects time to propagate, then assert no collateral damage
	time.Sleep(500 * time.Millisecond)

	openPub.assertAlive(t, 800*time.Millisecond)
	otherLow.assertAlive(t, 800*time.Millisecond)

	before := openReader.count()
	require.Eventually(t, func() bool { return openReader.count() > before },
		3*time.Second, 50*time.Millisecond, "reader on other path was harmed")
}

// Enabling the limit through the configuration API applies to the running
// publisher without restarting it; the next sliding-window check trips it.
func TestPathPublishBitrateLimitHotReload(t *testing.T) {
	p, ok := newInstance(t, bitrateTestConf("  live: {}\n", false, true))
	require.True(t, ok)
	defer p.Close()

	high := newRTSPRatePub(t, "live", 10*time.Millisecond, 8, 1200) // ~950 KB/s

	// unconfigured: no restriction
	high.assertAlive(t, 1200*time.Millisecond)

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr}

	httpRequest(t, hc, http.MethodPatch,
		testAPIURL+"/v3/config/paths/patch/live",
		map[string]any{"maxInboundBytesPerSecond": testBitrateLimit}, nil)

	high.expectKicked(t, 10*time.Second)
}
