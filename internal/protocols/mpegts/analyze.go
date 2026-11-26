package mpegts

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"time"

	mcmpegts "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	tscodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"

	"github.com/bluenviron/mediamtx/internal/logger"
)

const (
	// defaultAnalyzeMaxBytes bounds the amount of input buffered while the
	// startup analysis window is active. It is kept below the rewind buffer
	// size used internally by mediacommon's MPEG-TS Reader.
	defaultAnalyzeMaxBytes = 5 * 1024 * 1024

	// analyzeReadChunkSize is the size of every read performed by the pump.
	// It is a multiple of the MPEG-TS packet size.
	analyzeReadChunkSize = 32 * 188

	// maxPMTCandidates bounds the number of Reader initializations attempted
	// while looking for the most complete PMT inside the analysis buffer.
	maxPMTCandidates = 16
)

// ErrAnalysisNoTracks is returned when the startup analysis window ends
// without a single usable MPEG-TS track.
var ErrAnalysisNoTracks = errors.New("no MPEG-TS tracks found during startup analysis")

// ErrAnalysisTimeout is returned when the analysis window expires before any
// byte has been received from the source.
var ErrAnalysisTimeout = errors.New("startup analysis timed out before receiving data")

type analyzeChunk struct {
	b   []byte
	err error
}

type deadlineSetter interface {
	SetReadDeadline(t time.Time) error
}

// analyzePump continuously reads from src and makes chunks available to both
// the analysis phase and the final Reader. A single goroutine owns reads for
// the whole lifetime, so no bytes are lost and there are no competing readers.
type analyzePump struct {
	src         io.Reader
	readTimeout time.Duration
	ch          chan analyzeChunk
	done        chan struct{}
	once        sync.Once
}

func newAnalyzePump(src io.Reader, readTimeout time.Duration) *analyzePump {
	p := &analyzePump{
		src:         src,
		readTimeout: readTimeout,
		ch:          make(chan analyzeChunk),
		done:        make(chan struct{}),
	}
	go p.run()
	return p
}

func (p *analyzePump) stop() {
	p.once.Do(func() {
		close(p.done)
	})
}

func (p *analyzePump) run() {
	ds, canSetDeadline := p.src.(deadlineSetter)

	for {
		// Refresh the idle read deadline before every blocking read, so a
		// long analysis window never exceeds the configured source timeout on
		// connections that honor deadlines.
		if canSetDeadline && p.readTimeout > 0 {
			_ = ds.SetReadDeadline(time.Now().Add(p.readTimeout))
		}

		buf := make([]byte, analyzeReadChunkSize)
		n, err := p.src.Read(buf)
		if n > 0 {
			select {
			case p.ch <- analyzeChunk{b: buf[:n]}:
			case <-p.done:
				return
			}
		}
		if err != nil {
			select {
			case p.ch <- analyzeChunk{err: err}:
			case <-p.done:
			}
			return
		}
	}
}

// replayReader serves the selected buffered prefix first, then live chunks
// from the pump. A source error recorded during analysis is returned once
// the prefix has been consumed.
type replayReader struct {
	prefix    []byte
	head      []byte
	ch        <-chan analyzeChunk
	sourceErr error
}

func (r *replayReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	if len(r.prefix) > 0 {
		n := copy(p, r.prefix)
		r.prefix = r.prefix[n:]
		return n, nil
	}

	if len(r.head) > 0 {
		n := copy(p, r.head)
		r.head = r.head[n:]
		return n, nil
	}

	if r.sourceErr != nil {
		return 0, r.sourceErr
	}

	c, ok := <-r.ch
	if !ok {
		return 0, io.EOF
	}
	if c.err != nil {
		r.sourceErr = c.err
		if len(c.b) > 0 {
			r.head = c.b
			n := copy(p, r.head)
			r.head = r.head[n:]
			return n, nil
		}
		return 0, c.err
	}

	r.head = c.b
	n := copy(p, r.head)
	r.head = r.head[n:]
	return n, nil
}

// pmtCandidate is a PMT section start found in the buffered prefix.
type pmtCandidate struct {
	offset  int
	esCount int
}

// findPMTCandidates performs a light, allocation-light scan of raw MPEG-TS
// packets and returns offsets of PMT sections together with the number of
// elementary streams declared by each section. Corrupted packets are skipped:
// only structurally valid PMT sections are returned.
func findPMTCandidates(buf []byte) []pmtCandidate {
	var ret []pmtCandidate

	for off := 0; off+188 <= len(buf); {
		if buf[off] != 0x47 {
			off++
			continue
		}

		// Require the next packet to start with a sync byte when available,
		// this greatly reduces false positives on random corrupted data.
		if off+376 <= len(buf) && buf[off+188] != 0x47 {
			off++
			continue
		}

		pusi := buf[off+1]&0x40 != 0
		pid := uint16(buf[off+1]&0x1f)<<8 | uint16(buf[off+2])
		fc := (buf[off+3] >> 4) & 0x03

		payloadOff := off + 4
		switch fc {
		case 0: // reserved
			off += 188
			continue
		case 2: // adaptation field only, no payload
			off += 188
			continue
		case 3: // adaptation field followed by payload
			if off+5 >= len(buf) {
				off++
				continue
			}
			al := int(buf[off+4])
			payloadOff = off + 5 + al
			if payloadOff > off+188 {
				off += 188
				continue
			}
		}

		if !pusi || pid == 0x1fff || payloadOff >= off+188 || payloadOff >= len(buf) {
			off += 188
			continue
		}

		payload := buf[payloadOff : off+188]
		if len(payload) < 2 {
			off += 188
			continue
		}

		pointer := int(payload[0])
		if 1+pointer >= len(payload) {
			off += 188
			continue
		}

		section := payload[1+pointer:]
		if len(section) < 12 || section[0] != 0x02 { // table_id: TS_program_map_section
			off += 188
			continue
		}

		sectionLen := int(uint16(section[1]&0x0f)<<8 | uint16(section[2]))
		if sectionLen < 9 || 3+sectionLen > len(section) {
			off += 188
			continue
		}

		// program_info_length
		pil := int(uint16(section[10]&0x0f)<<8 | uint16(section[11]))
		pos := 12 + pil
		end := 3 + sectionLen - 4 // exclude CRC32

		esCount := 0
		valid := true
		for pos+5 <= end && pos+5 <= len(section) {
			esInfoLen := int(uint16(section[pos+3]&0x0f)<<8 | uint16(section[pos+4]))
			pos += 5 + esInfoLen
			if pos > end {
				valid = false
				break
			}
			esCount++
		}
		if !valid || esCount == 0 {
			off += 188
			continue
		}

		ret = append(ret, pmtCandidate{offset: off, esCount: esCount})
		off += 188
	}

	return ret
}

// supportedTrackCount returns the number of tracks that expose a known codec.
// Unsupported tracks do not count.
func supportedTrackCount(tracks []*mcmpegts.Track) int {
	n := 0
	for _, tr := range tracks {
		if _, ok := tr.Codec.(*tscodecs.Unsupported); !ok {
			n++
		}
	}
	return n
}

// tryInitializeReader attempts to initialize a Reader over r.
func tryInitializeReader(r io.Reader) (*mcmpegts.Reader, error) {
	mr := &mcmpegts.Reader{R: r}
	if err := mr.Initialize(); err != nil {
		return nil, err
	}
	return mr, nil
}

// selectAnalysisCut picks the offset of the most complete usable PMT inside
// prefix. Every probe is performed on a bounded bytes.Reader over the prefix,
// so this function never reads from the live source. ok is false when no cut
// allows a Reader to be initialized from buffered data alone.
func selectAnalysisCut(prefix []byte, logfn func(error)) (cut int, ok bool) {
	candidates := findPMTCandidates(prefix)

	maxES := 0
	for _, c := range candidates {
		if c.esCount > maxES {
			maxES = c.esCount
		}
	}

	// Probe offsets in this order:
	//  1. stream start, which lets mediacommon scan for the PMT itself and
	//     covers scanner false negatives;
	//  2. PMTs declaring the highest number of elementary streams first;
	//  3. a few remaining PMTs regardless of their declared count.
	offsets := make([]int, 0, len(candidates)+1)
	offsets = append(offsets, 0)

	attempted := 0
	for _, c := range candidates {
		if c.esCount != maxES {
			continue
		}
		if attempted >= maxPMTCandidates {
			break
		}
		attempted++
		offsets = append(offsets, c.offset)
	}

	start := max(0, len(candidates)-maxPMTCandidates)
	for i := len(candidates) - 1; i >= start; i-- {
		if candidates[i].esCount == maxES {
			continue // already considered above
		}
		offsets = append(offsets, candidates[i].offset)
	}

	bestOffset := -1
	bestTracks := 0

	for _, off := range offsets {
		mr, err := tryInitializeReader(bytes.NewReader(prefix[off:]))
		if err != nil {
			continue
		}

		n := supportedTrackCount(mr.Tracks())
		if n == 0 {
			continue
		}

		// Maximize track count, then prefer the earliest offset so the replayed
		// prefix contains as much codec-parameter data as possible.
		if n > bestTracks || (n == bestTracks && (bestOffset == -1 || off < bestOffset)) {
			bestTracks = n
			bestOffset = off
		}
	}

	if bestOffset < 0 {
		if logfn != nil {
			logfn(errors.New("startup analysis: no valid PMT candidate found in buffered data"))
		}
		return 0, false
	}

	return bestOffset, true
}

// StartupAnalysis configures AnalyzeReader.
type StartupAnalysis struct {
	// Duration is the maximum time during which the start of the stream is
	// observed before publishing it. Zero disables the analysis and preserves
	// the legacy startup behavior.
	Duration time.Duration

	// MaxBufferSize is the upper bound of the buffered input. Zero selects
	// defaultAnalyzeMaxBytes.
	MaxBufferSize int

	// ReadTimeout, when positive, is used to refresh the read deadline of
	// sources implementing deadlineSetter while the analysis pump is running.
	ReadTimeout time.Duration

	// Log is used for optional diagnostics.
	Log logger.Writer
}

// AnalyzeReader optionally buffers and analyzes the beginning of a MPEG-TS
// stream before returning an initialized Reader.
//
// When Duration is zero, it behaves exactly like allocating a Reader and
// calling Initialize on it.
//
// When Duration is positive, corrupted leading packets and incomplete early
// PMTs are skipped: the whole window is observed, the most complete PMT is
// selected, and buffered bytes are replayed before live data. The returned
// stop function must be called when the Reader is no longer used.
//
// Deterministic outcomes:
//   - window expires: the best validated track set is returned, or
//     ErrAnalysisNoTracks if no track can be validated inside the window;
//   - only video is present: a video-only Reader is returned;
//   - persistent corruption / no tracks: ErrAnalysisNoTracks or an init error;
//   - source disconnects: buffered data is replayed, then the source error is
//     returned by subsequent Read calls.
func AnalyzeReader(src io.Reader, a StartupAnalysis) (*mcmpegts.Reader, func(), error) {
	nopStop := func() {}

	if a.Duration <= 0 {
		mr := &mcmpegts.Reader{R: src}
		if err := mr.Initialize(); err != nil {
			return nil, nopStop, err
		}
		return mr, nopStop, nil
	}

	maxBuf := a.MaxBufferSize
	if maxBuf <= 0 {
		maxBuf = defaultAnalyzeMaxBytes
	}

	pump := newAnalyzePump(src, a.ReadTimeout)
	stop := pump.stop

	timer := time.NewTimer(a.Duration)
	defer timer.Stop()
	deadline := time.Now().Add(a.Duration)

	var prefix []byte
	var sourceErr error

loop:
	for {
		// Guarantee the bounded window even when data is always ready and the
		// timer channel would repeatedly lose the select race.
		if !time.Now().Before(deadline) {
			break loop
		}

		select {
		case c, ok := <-pump.ch:
			if !ok {
				sourceErr = io.EOF
				break loop
			}
			if c.err != nil {
				sourceErr = c.err
				break loop
			}
			prefix = append(prefix, c.b...)
			// Keep the buffer bounded: the overshoot is at most one read chunk.
			if len(prefix) >= maxBuf {
				break loop
			}

		case <-timer.C:
			break loop
		}
	}

	if a.Log != nil {
		a.Log.Log(logger.Debug, "startup analysis: observed %d bytes (limit %d, source error=%v)",
			len(prefix), maxBuf, sourceErr)
	}

	// No data at all: never block Initialize behind a source Read. Return a
	// deterministic result and let the owner close the source.
	if len(prefix) == 0 {
		stop()
		if sourceErr != nil {
			return nil, nopStop, sourceErr
		}
		return nil, nopStop, ErrAnalysisTimeout
	}

	// Select a cut that is guaranteed to initialize from buffered bytes alone.
	// This keeps persistent corruption from blocking on live data forever.
	cut, ok := selectAnalysisCut(prefix, func(err error) {
		if a.Log != nil {
			a.Log.Log(logger.Debug, "%v", err)
		}
	})
	if !ok {
		stop()
		if sourceErr != nil {
			return nil, nopStop, sourceErr
		}
		return nil, nopStop, ErrAnalysisNoTracks
	}

	rr := &replayReader{
		prefix:    prefix[cut:],
		ch:        pump.ch,
		sourceErr: sourceErr,
	}

	mr, err := tryInitializeReader(rr)
	if err != nil {
		stop()
		return nil, nopStop, err
	}

	if len(mr.Tracks()) == 0 || supportedTrackCount(mr.Tracks()) == 0 {
		stop()
		return nil, nopStop, ErrAnalysisNoTracks
	}

	return mr, stop, nil
}
