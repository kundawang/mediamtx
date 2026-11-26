package stream

import (
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
)

// sanity bounds used to discard untrusted or inconsistent sender reports.
const (
	// a sender report mapping a packet in the future, relative to the local
	// clock, is inconsistent. Reports in the past are tolerated since they
	// reflect capture and network delay
	ntpAlignerMaxFutureError = 5 * time.Second

	// reports older than this, relative to the local clock, are not credible
	ntpAlignerMaxPastError = 60 * time.Second

	// offsets larger than this are not believable for tracks sharing the
	// same capture clock
	ntpAlignerMaxOffset = 2 * time.Second
)

type ntpAlignerTrack struct {
	clockRate int64

	// local arrival data of the first packet of the track
	firstPTS    int64
	firstTime   time.Time
	initialized bool

	// mapping extracted from the latest trusted RTCP sender report
	srAvailable bool
	srPTS       int64
	srNTP       time.Time
}

// NTPAligner aligns the NTP timestamps (absolute time) of multiple tracks
// that share the same capture clock, while keeping them anchored to the
// local clock.
//
// The inter-track difference is computed from the publisher RTCP sender
// reports, while the local delivery delay of every track is measured from the
// arrival time of its first packet. This allows cancelling a pure delivery
// delay (for instance a track that starts sending later) while preserving a
// genuine difference between capture clocks. Without it, every track would
// estimate NTP from its own first packet, and a track that starts sending
// later would be shifted in time even when the publisher provides consistent
// sender reports.
type NTPAligner struct {
	mutex  sync.Mutex
	tracks map[*description.Media]map[format.Format]*ntpAlignerTrack

	leadingMedia  *description.Media
	leadingFormat format.Format

	// local time at which the leading track's first packet was received
	anchorTime time.Time
}

// Initialize initializes an NTPAligner.
func (a *NTPAligner) Initialize() {
	a.tracks = make(map[*description.Media]map[format.Format]*ntpAlignerTrack)
}

func (a *NTPAligner) getTrack(medi *description.Media, forma format.Format) *ntpAlignerTrack {
	formats := a.tracks[medi]
	if formats == nil {
		formats = make(map[format.Format]*ntpAlignerTrack)
		a.tracks[medi] = formats
	}

	tr := formats[forma]
	if tr == nil {
		tr = &ntpAlignerTrack{
			clockRate: int64(forma.ClockRate()),
		}
		formats[forma] = tr

		if a.leadingMedia == nil {
			a.leadingMedia = medi
			a.leadingFormat = forma
		}
	}

	return tr
}

// ProcessPacket stores the PTS of the first packet of a track, as produced by
// the RTP time decoder, and the local time at which it was received.
func (a *NTPAligner) ProcessPacket(
	medi *description.Media,
	forma format.Format,
	pts int64,
	now time.Time,
) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	tr := a.getTrack(medi, forma)
	if !tr.initialized {
		tr.initialized = true
		tr.firstPTS = pts
		tr.firstTime = now

		if a.anchorTime.IsZero() {
			a.anchorTime = now
		}
	}
}

// ProcessSenderReport stores the mapping between a PTS and the NTP (absolute
// time) computed from a publisher RTCP sender report. Reports that are missing,
// too far from the local clock or internally inconsistent are rejected.
func (a *NTPAligner) ProcessSenderReport(
	medi *description.Media,
	forma format.Format,
	pts int64,
	ntp time.Time,
	now time.Time,
) {
	if ntp.IsZero() {
		return
	}

	a.mutex.Lock()
	defer a.mutex.Unlock()

	tr := a.getTrack(medi, forma)

	// reject reports that do not map to a plausible instant on the local clock
	delta := ntp.Sub(now)
	if delta > ntpAlignerMaxFutureError || delta < -ntpAlignerMaxPastError {
		return
	}

	// reject reports that move backwards on the same track
	if tr.srAvailable && !ntp.After(tr.srNTP) {
		return
	}

	tr.srAvailable = true
	tr.srPTS = pts
	tr.srNTP = ntp
}

// Offset returns the NTP offset that must be added to a track's local
// estimates in order to align it with the other tracks and anchor it to the
// local clock.
// It returns 0 until the leading track and the requested track both have a
// trusted sender report, so playback is preserved and no leading packet is
// dropped while reports are missing or inconsistent.
func (a *NTPAligner) Offset(
	medi *description.Media,
	forma format.Format,
) time.Duration {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	tr := a.getTrack(medi, forma)
	leading := a.tracks[a.leadingMedia][a.leadingFormat]

	if !tr.initialized || !tr.srAvailable ||
		!leading.initialized || !leading.srAvailable || a.anchorTime.IsZero() {
		return 0
	}

	// capture NTP of the first packet of every track, extrapolated from its
	// own sender report
	firstNTP := tr.srNTP.Add(ptsToDuration(tr.firstPTS-tr.srPTS, tr.clockRate))
	leadingFirstNTP := leading.srNTP.Add(
		ptsToDuration(leading.firstPTS-leading.srPTS, leading.clockRate))

	// position of the track's first packet on the leading track's local-clock
	// timeline: the local anchor, shifted by the genuine capture-clock offset
	captureOffset := firstNTP.Sub(leadingFirstNTP)
	localTarget := a.anchorTime.Add(captureOffset)

	// correct the track's own local anchor, which only reflects its delivery
	// delay, to the target position
	offset := localTarget.Sub(tr.firstTime)
	if offset > ntpAlignerMaxOffset || offset < -ntpAlignerMaxOffset {
		return 0
	}

	return offset
}

func ptsToDuration(pts int64, clockRate int64) time.Duration {
	secs := pts / clockRate
	dec := pts % clockRate
	return time.Duration(secs)*time.Second +
		time.Duration(dec)*time.Second/time.Duration(clockRate)
}
