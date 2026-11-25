package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/test"
)

// This verification suite drives a real MediaMTX Core over RTSP/TCP with two
// short, frame-marked H264 streams. Helper names carry the bkv prefix to avoid
// collisions with any other verification file.

func bkvTwoFreePorts(t *testing.T) (int, int) {
	t.Helper()
	l1, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	l2, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p1 := l1.Addr().(*net.TCPAddr).Port
	p2 := l2.Addr().(*net.TCPAddr).Port
	require.NoError(t, l1.Close())
	require.NoError(t, l2.Close())
	require.NotEqual(t, p1, p2)
	return p1, p2
}

func bkvItoa(i int) string { return strconv.Itoa(i) }

func bkvStart(t *testing.T, rtspPort, apiPort int, paths string) *Core {
	t.Helper()
	cnf := "api: yes\n" +
		"apiAddress: 127.0.0.1:" + bkvItoa(apiPort) + "\n" +
		"rtmp: no\n" +
		"hls: no\n" +
		"webrtc: no\n" +
		"srt: no\n" +
		"moq: no\n" +
		"rtspAddress: 127.0.0.1:" + bkvItoa(rtspPort) + "\n" +
		"rtspTransports: [tcp]\n" +
		"paths:\n" + paths

	p, ok := newInstance(t, cnf)
	require.True(t, ok)
	return p
}

func bkvStartBackup(t *testing.T) (*Core, int, int) {
	rtspPort, apiPort := bkvTwoFreePorts(t)
	paths := "  backupfeed:\n" +
		"  mystream:\n" +
		"    backupSource: rtsp://127.0.0.1:" + bkvItoa(rtspPort) + "/backupfeed\n"
	return bkvStart(t, rtspPort, apiPort, paths), rtspPort, apiPort
}

// ---------- marked H264 publisher ----------

type bkvPub struct {
	c    *gortsplib.Client
	medi *description.Media
	seq  uint16
	ts   uint32
	once sync.Once
}

func bkvNewPub(t *testing.T, port int, path string) *bkvPub {
	t.Helper()
	medi := test.UniqueMediaH264()
	tcp := gortsplib.ProtocolTCP
	c := &gortsplib.Client{Protocol: &tcp}
	require.NoError(t, c.StartRecording(
		"rtsp://127.0.0.1:"+bkvItoa(port)+"/"+path,
		&description.Session{Medias: []*description.Media{medi}}))
	p := &bkvPub{c: c, medi: medi, seq: 4000, ts: 180000}
	t.Cleanup(p.close)
	return p
}

// frame writes one marked NAL unit. Payload layout:
// [nalType, 0xAA, 0x55, marker, (optional tag...), seqLo, seqHi].
func (p *bkvPub) frame(idr bool, marker byte, tag []byte) error {
	nt := byte(1)
	if idr {
		nt = 5
	}
	pl := []byte{nt, 0xAA, 0x55, marker}
	if len(tag) > 0 {
		pl = append(pl, tag...)
	}
	pl = append(pl, byte(p.seq), byte(p.seq>>8))

	pkt := &rtp.Packet{Header: rtp.Header{
		Version:        2,
		Marker:         true,
		PayloadType:    96,
		SequenceNumber: p.seq,
		Timestamp:      p.ts,
		SSRC:           0x71717171,
	}, Payload: pl}

	p.seq++
	p.ts += 3000 // 90 kHz, ~33 ms
	return p.c.WritePacketRTP(p.medi, pkt)
}

// pump emits periodic IDR frames so late-joining RTSP clients initialize
// their timestamp decoder and key-frame gates open.
func (p *bkvPub) pump(marker byte, interval time.Duration) chan struct{} {
	stop := make(chan struct{})
	go func() {
		n := 0
		tk := time.NewTicker(interval)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				if err := p.frame(n%4 == 0, marker, nil); err != nil {
					return
				}
				n++
			}
		}
	}()
	return stop
}

func (p *bkvPub) close() {
	p.once.Do(func() { p.c.Close() })
}

// ---------- marked H264 reader ----------

type bkvEvent struct {
	marker byte
	ts     uint32
}

type bkvReader struct {
	c *gortsplib.Client

	mu      sync.Mutex
	events  []bkvEvent
	tagHits int
}

func bkvNewReader(t *testing.T, port int, path string) *bkvReader {
	t.Helper()
	u, err := base.ParseURL("rtsp://127.0.0.1:" + bkvItoa(port) + "/" + path)
	require.NoError(t, err)

	tcp := gortsplib.ProtocolTCP
	c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host, Protocol: &tcp}
	require.NoError(t, c.Start())
	t.Cleanup(func() { c.Close() })

	desc, _, err := c.Describe(u)
	require.NoError(t, err)
	require.NoError(t, c.SetupAll(desc.BaseURL, desc.Medias))

	r := &bkvReader{c: c}
	medi := desc.Medias[0]
	forma := medi.Formats[0]

	c.OnPacketRTP(medi, forma, func(pkt *rtp.Packet) {
		r.mu.Lock()
		defer r.mu.Unlock()

		pl := pkt.Payload
		if bytes.Contains(pl, []byte("PREK")) {
			r.tagHits++
		}

		for i := 0; i+3 < len(pl); i++ {
			if pl[i] == 0xAA && pl[i+1] == 0x55 {
				r.events = append(r.events, bkvEvent{marker: pl[i+2], ts: pkt.Timestamp})
				break
			}
		}
	})

	_, err = c.Play(nil)
	require.NoError(t, err)
	return r
}

func (r *bkvReader) snapshot() ([]bkvEvent, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bkvEvent(nil), r.events...), r.tagHits
}

func (r *bkvReader) count(marker byte) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e.marker == marker {
			n++
		}
	}
	return n
}

func (r *bkvReader) waitCount(t *testing.T, marker byte, atLeast int) {
	t.Helper()
	require.Eventually(t, func() bool { return r.count(marker) >= atLeast },
		10*time.Second, 20*time.Millisecond, "marker %#x count never reached %d (got %d)",
		marker, atLeast, r.count(marker))
}

func (r *bkvReader) conn() net.Conn { return r.c.NetConn() }

// waitDisconnected returns once the reader client session has ended.
func (r *bkvReader) waitDisconnected(t *testing.T) {
	t.Helper()
	errCh := make(chan error, 1)
	go func() { errCh <- r.c.Wait() }()
	select {
	case err := <-errCh:
		require.Error(t, err)
	case <-time.After(8 * time.Second):
		t.Fatal("reader RTSP session was not disconnected")
	}
}

// ---------- helpers ----------

func bkvPath(t *testing.T, p *Core, name string) *path {
	t.Helper()
	pa, ok := p.pathManager.paths[name]
	require.True(t, ok, "path %s missing", name)
	return pa
}

func bkvWaitState(t *testing.T, p *Core, name string, want BackupSourceState) {
	t.Helper()
	require.Eventually(t, func() bool {
		pa, ok := p.pathManager.paths[name]
		if !ok {
			return false
		}
		st, configured := pa.BackupSourceState()
		return configured && st == want
	}, 10*time.Second, 25*time.Millisecond, "backup state never became %s", want)
}

func bkvSessionID(t *testing.T, apiPort int, path string) string {
	t.Helper()
	var id string
	require.Eventually(t, func() bool {
		res, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v3/paths/get/%s", apiPort, path))
		if err != nil {
			return false
		}
		defer res.Body.Close()
		byts, err := io.ReadAll(res.Body)
		if err != nil {
			return false
		}
		var out struct {
			Readers []struct {
				ID string `json:"id"`
			} `json:"readers"`
		}
		if json.Unmarshal(byts, &out) != nil || len(out.Readers) == 0 {
			return false
		}
		id = out.Readers[0].ID
		return id != ""
	}, 5*time.Second, 25*time.Millisecond)
	return id
}

func bkvBackupAPIState(t *testing.T, apiPort int, path string) string {
	t.Helper()
	var state string
	require.Eventually(t, func() bool {
		res, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v3/paths/get/%s", apiPort, path))
		if err != nil {
			return false
		}
		defer res.Body.Close()
		byts, err := io.ReadAll(res.Body)
		if err != nil {
			return false
		}
		var out struct {
			BackupSource *struct {
				State string `json:"state"`
			} `json:"backupSource"`
		}
		if json.Unmarshal(byts, &out) != nil || out.BackupSource == nil {
			return false
		}
		state = out.BackupSource.State
		return state != ""
	}, 5*time.Second, 25*time.Millisecond)
	return state
}

func bkvPhaseSequence(evs []bkvEvent) []byte {
	var seq []byte
	var prev byte
	for _, e := range evs {
		if e.marker != prev {
			seq = append(seq, e.marker)
			prev = e.marker
		}
	}
	return seq
}

func bkvTimestampBackwards(evs []bkvEvent) int {
	bad := 0
	for i := 1; i < len(evs); i++ {
		// wrap-aware signed 32-bit delta: negative means backwards
		if int32(evs[i].ts-evs[i-1].ts) < 0 {
			bad++
		}
	}
	return bad
}

// TestBackupSourceVerifySwitch covers requirement 1 and 3: a pre-connected
// RTSP backup takes over when the main publisher disappears, and the main
// publisher is taken back at its first key frame, all on the same reader RTSP
// session with monotonic timestamps and main -> backup -> main frame order.
func TestBackupSourceVerifySwitch(t *testing.T) {
	const (
		mainMarker   byte = 0x11
		backupMarker byte = 0x22
	)

	p, rtspPort, apiPort := bkvStartBackup(t)
	defer p.Close()

	main := bkvNewPub(t, rtspPort, "mystream")
	mainStop := main.pump(mainMarker, 20*time.Millisecond)

	backup := bkvNewPub(t, rtspPort, "backupfeed")
	backupStop := backup.pump(backupMarker, 20*time.Millisecond)
	defer close(backupStop)

	bkvWaitState(t, p, "mystream", BackupSourceStateReady)
	require.Equal(t, "ready", bkvBackupAPIState(t, apiPort, "mystream"))

	reader := bkvNewReader(t, rtspPort, "mystream")
	reader.waitCount(t, mainMarker, 6)

	connBefore := reader.conn()
	sidBefore := bkvSessionID(t, apiPort, "mystream")

	// main disappears: backup must take over without dropping the session
	close(mainStop)
	main.close()

	reader.waitCount(t, backupMarker, 8)
	bkvWaitState(t, p, "mystream", BackupSourceStateActive)
	require.Equal(t, "active", bkvBackupAPIState(t, apiPort, "mystream"))

	require.Equal(t, sidBefore, bkvSessionID(t, apiPort, "mystream"),
		"reader session ID changed during main -> backup switch")
	require.Equal(t, connBefore, reader.conn(),
		"reader RTSP connection changed during failover")

	// main returns: pre-key-frame frames must be withheld
	main2 := bkvNewPub(t, rtspPort, "mystream")
	for i := 0; i < 6; i++ {
		require.NoError(t, main2.frame(false, mainMarker, []byte("PREK")))
		time.Sleep(20 * time.Millisecond)
	}
	_, preKeyHits := reader.snapshot()
	require.Equal(t, 0, preKeyHits, "pre-key-frame main frames reached the reader")

	mainBeforeIDR := reader.count(mainMarker)
	require.NoError(t, main2.frame(true, mainMarker, nil))
	main2Stop := main2.pump(mainMarker, 20*time.Millisecond)
	defer close(main2Stop)

	require.Eventually(t, func() bool {
		return reader.count(mainMarker) >= mainBeforeIDR+6
	}, 10*time.Second, 20*time.Millisecond)

	// once main frames are ahead again, no backup frame may follow
	require.Eventually(t, func() bool {
		evs, _ := reader.snapshot()
		lastMain, lastBackup := -1, -1
		for i, e := range evs {
			switch e.marker {
			case mainMarker:
				lastMain = i
			case backupMarker:
				lastBackup = i
			}
		}
		return lastMain > lastBackup
	}, 6*time.Second, 25*time.Millisecond)

	time.Sleep(250 * time.Millisecond) // settle to detect late backup leakage

	evs, preHits := reader.snapshot()
	require.Equal(t, 0, preHits, "pre-key-frame main frames reached the reader")
	require.Equal(t, 0, bkvTimestampBackwards(evs), "RTP timestamps went backwards across switches")
	require.Equal(t, []byte{mainMarker, backupMarker, mainMarker}, bkvPhaseSequence(evs),
		"unexpected frame phase order: %x", bkvPhaseSequence(evs))

	require.Equal(t, sidBefore, bkvSessionID(t, apiPort, "mystream"),
		"reader session ID changed during backup -> main switch-back")
	require.Equal(t, connBefore, reader.conn(),
		"reader RTSP connection changed during switch-back")

	bkvWaitState(t, p, "mystream", BackupSourceStateReady)
	require.Equal(t, "ready", bkvBackupAPIState(t, apiPort, "mystream"))
}

// TestBackupSourceVerifyUnavailable checks that when the main publisher dies
// and the backup is not connected, the standard behavior applies: the reader
// session is torn down.
func TestBackupSourceVerifyUnavailable(t *testing.T) {
	p, rtspPort, _ := bkvStartBackup(t)
	defer p.Close()

	bkvWaitState(t, p, "mystream", BackupSourceStateIdle)

	main := bkvNewPub(t, rtspPort, "mystream")
	mainStop := main.pump(0x11, 20*time.Millisecond)

	reader := bkvNewReader(t, rtspPort, "mystream")
	reader.waitCount(t, 0x11, 3)

	close(mainStop)
	main.close()

	reader.waitDisconnected(t)
}

// TestBackupSourceVerifyNoBackupKeepsBehavior ensures paths without a
// backupSource keep the original disconnect behavior and carry no manager.
func TestBackupSourceVerifyNoBackupKeepsBehavior(t *testing.T) {
	rtspPort, apiPort := bkvTwoFreePorts(t)
	p := bkvStart(t, rtspPort, apiPort, "  plain:\n")
	defer p.Close()

	_, configured := bkvPath(t, p, "plain").BackupSourceState()
	require.False(t, configured)

	main := bkvNewPub(t, rtspPort, "plain")
	mainStop := main.pump(0x33, 20*time.Millisecond)

	reader := bkvNewReader(t, rtspPort, "plain")
	reader.waitCount(t, 0x33, 3)

	close(mainStop)
	main.close()

	reader.waitDisconnected(t)
}

// TestBackupSourceVerifyFlapping cycles the main publisher multiple times and
// verifies the reader session survives and timestamps stay monotonic.
func TestBackupSourceVerifyFlapping(t *testing.T) {
	const (
		mainMarker   byte = 0x41
		backupMarker byte = 0x42
	)

	p, rtspPort, apiPort := bkvStartBackup(t)
	defer p.Close()

	backup := bkvNewPub(t, rtspPort, "backupfeed")
	backupStop := backup.pump(backupMarker, 15*time.Millisecond)
	defer close(backupStop)

	main := bkvNewPub(t, rtspPort, "mystream")
	mainStop := main.pump(mainMarker, 15*time.Millisecond)

	bkvWaitState(t, p, "mystream", BackupSourceStateReady)
	reader := bkvNewReader(t, rtspPort, "mystream")
	reader.waitCount(t, mainMarker, 5)
	sid := bkvSessionID(t, apiPort, "mystream")

	for cycle := 0; cycle < 2; cycle++ {
		close(mainStop)
		main.close()
		reader.waitCount(t, backupMarker, 6*(cycle+1))
		bkvWaitState(t, p, "mystream", BackupSourceStateActive)
		require.Equal(t, sid, bkvSessionID(t, apiPort, "mystream"))

		main = bkvNewPub(t, rtspPort, "mystream")
		require.NoError(t, main.frame(true, mainMarker, nil))
		mainStop = main.pump(mainMarker, 15*time.Millisecond)
		reader.waitCount(t, mainMarker, 5*(cycle+2))
		bkvWaitState(t, p, "mystream", BackupSourceStateReady)
		require.Equal(t, sid, bkvSessionID(t, apiPort, "mystream"))
	}
	defer close(mainStop)

	time.Sleep(200 * time.Millisecond)
	evs, _ := reader.snapshot()
	require.Equal(t, 0, bkvTimestampBackwards(evs), "timestamps went backwards during flapping")
	seq := bkvPhaseSequence(evs)
	t.Logf("flap phase sequence: %x (%d events)", seq, len(evs))
	require.GreaterOrEqual(t, len(seq), 5, "expected main/backup alternation, got %x", seq)
	require.Equal(t, mainMarker, seq[len(seq)-1])
}

// TestBackupSourceVerifyBackupDies checks that when the backup source is
// serving (main already gone) and then disappears too, readers are disconnected
// and the path can be closed without deadlocks or leaked resources.
func TestBackupSourceVerifyBackupDies(t *testing.T) {
	p, rtspPort, _ := bkvStartBackup(t)
	defer p.Close()

	main := bkvNewPub(t, rtspPort, "mystream")
	mainStop := main.pump(0x11, 20*time.Millisecond)

	backup := bkvNewPub(t, rtspPort, "backupfeed")
	backupStop := backup.pump(0x22, 20*time.Millisecond)

	bkvWaitState(t, p, "mystream", BackupSourceStateReady)

	reader := bkvNewReader(t, rtspPort, "mystream")
	reader.waitCount(t, 0x11, 4)

	close(mainStop)
	main.close()
	reader.waitCount(t, 0x22, 5)
	bkvWaitState(t, p, "mystream", BackupSourceStateActive)

	// backup goes away while it is the only source
	close(backupStop)
	backup.close()

	reader.waitDisconnected(t)

	require.Eventually(t, func() bool {
		st, configured := bkvPath(t, p, "mystream").BackupSourceState()
		return configured && st == BackupSourceStateIdle
	}, 8*time.Second, 25*time.Millisecond)
}

// TestBackupSourceVerifyHotReloadAdd ensures a backup source can be added to an
// existing publisher path through a configuration reload and then connects.
func TestBackupSourceVerifyHotReloadAdd(t *testing.T) {
	rtspPort, apiPort := bkvTwoFreePorts(t)
	p := bkvStart(t, rtspPort, apiPort, "  backupfeed:\n  dynpath:\n")
	defer p.Close()

	pa := bkvPath(t, p, "dynpath")
	_, configured := pa.BackupSourceState()
	require.False(t, configured)

	main := bkvNewPub(t, rtspPort, "dynpath")
	mainStop := main.pump(0x11, 20*time.Millisecond)
	defer close(mainStop)

	backup := bkvNewPub(t, rtspPort, "backupfeed")
	backupStop := backup.pump(0x22, 20*time.Millisecond)
	defer close(backupStop)

	newConf := pa.SafeConf().Clone()
	newConf.BackupSource = "rtsp://127.0.0.1:" + bkvItoa(rtspPort) + "/backupfeed"
	pa.reloadConf(newConf)

	require.Eventually(t, func() bool {
		st, ok := bkvPath(t, p, "dynpath").BackupSourceState()
		return ok && st == BackupSourceStateReady
	}, 10*time.Second, 25*time.Millisecond)
}

// TestBackupSourceVerifyHotReloadAndClose checks that removing backupSource at
// runtime stops and reclaims the static source, and that closing the Core
// reclaims the backup goroutine/connection.
func TestBackupSourceVerifyHotReloadAndClose(t *testing.T) {
	p, _, _ := bkvStartBackup(t)

	bkvWaitState(t, p, "mystream", BackupSourceStateIdle)
	pa := bkvPath(t, p, "mystream")
	_, configured := pa.BackupSourceState()
	require.True(t, configured)

	// remove backupSource through a configuration reload. Observing the
	// manager gone implies its Close() (which reaps the static-source
	// goroutine) has already returned.
	newConf := pa.SafeConf().Clone()
	newConf.BackupSource = ""
	pa.reloadConf(newConf)

	require.Eventually(t, func() bool {
		_, stillConfigured := bkvPath(t, p, "mystream").BackupSourceState()
		return !stillConfigured
	}, 5*time.Second, 20*time.Millisecond)

	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(8 * time.Second):
		t.Fatal("Core.Close hung after backup source reload")
	}

	// closing a Core that still owns a backup manager must reap it promptly
	p2, _, _ := bkvStartBackup(t)
	bkvWaitState(t, p2, "mystream", BackupSourceStateIdle)
	_, configured2 := bkvPath(t, p2, "mystream").BackupSourceState()
	require.True(t, configured2)

	closed2 := make(chan struct{})
	go func() { p2.Close(); close(closed2) }()
	select {
	case <-closed2:
	case <-time.After(8 * time.Second):
		t.Fatal("Core.Close did not reclaim the backup source in time")
	}
}
