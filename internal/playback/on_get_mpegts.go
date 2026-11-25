package playback

import (
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"

	"github.com/bluenviron/mediamtx/internal/recordstore"
)

// mpegtsTracksAreEqual checks whether two segments contain the same tracks,
// in the same order and with the same codec types and time scales.
func mpegtsTracksAreEqual(init1 *fmp4.Init, init2 *fmp4.Init) bool {
	if len(init1.Tracks) != len(init2.Tracks) {
		return false
	}

	for i, track1 := range init1.Tracks {
		track2 := init2.Tracks[i]

		if track1.TimeScale != track2.TimeScale ||
			reflect.TypeOf(track1.Codec) != reflect.TypeOf(track2.Codec) {
			return false
		}
	}

	return true
}

// mpegtsCanBeConcatenated checks whether two consecutive segments can be
// concatenated into a single playback. The tracks must be identical and the
// beginning of the new segment must be close enough to the end of the previous
// one.
func mpegtsCanBeConcatenated(
	prevInit *fmp4.Init,
	prevEnd time.Time,
	curInit *fmp4.Init,
	curStart time.Time,
) bool {
	return mpegtsTracksAreEqual(prevInit, curInit) &&
		!curStart.Before(prevEnd.Add(-concatenationTolerance)) &&
		!curStart.After(prevEnd.Add(concatenationTolerance))
}

// seekAndMuxMPEGTS reads MPEG-TS recording segments and muxes the requested
// time range into the output muxer.
//
// Segments are concatenated when their tracks match and their time ranges are
// contiguous. A segment that cannot be opened or parsed terminates the
// playback at that point, instead of aborting the whole request.
func seekAndMuxMPEGTS(
	segments []*recordstore.Segment,
	start time.Time,
	duration time.Duration,
	m muxer,
) error {
	f, demuxer, err := readMPEGTSSegment(segments[0].Fpath)
	if err != nil {
		// the only available segment is missing or corrupted: there is no
		// playable data for the requested range.
		return fmt.Errorf("unable to read segment %s: %w", segments[0].Fpath, err)
	}
	defer f.Close()

	firstInit := demuxer.init()
	m.writeInit(firstInit)

	// the first segment starts at segments[0].Start, which can be before the
	// requested start: the offset is negative.
	startOffset := segments[0].Start.Sub(start)

	// DTS of the last written sample of each track, shared across segments
	// in order to drop overlapping frames at segment boundaries.
	lastDTS := make(map[int]time.Duration)

	segmentDuration, err := segmentMPEGTSMux(
		segments[0].Fpath, startOffset, duration, firstInit, demuxer, lastDTS, m)
	if err != nil {
		return err
	}

	segmentEnd := segments[0].Start.Add(segmentDuration)
	prevInit := firstInit

	for _, seg := range segments[1:] {
		// stop as soon as the requested range has been covered
		if !segmentEnd.Before(start.Add(duration)) {
			break
		}

		var nextFile *os.File
		var nextDemuxer *mpegtsDemuxer
		nextFile, nextDemuxer, readErr := readMPEGTSSegment(seg.Fpath)
		if readErr != nil {
			// a missing or corrupted segment ends the playback but does
			// not invalidate the data already muxed.
			break
		}

		nextInit := nextDemuxer.init()

		if !mpegtsCanBeConcatenated(prevInit, segmentEnd, nextInit, seg.Start) {
			nextFile.Close()
			break
		}

		// absolute time of the segment beginning mapped to playback-relative DTS
		segmentDTS := seg.Start.Sub(start)

		nextDuration, muxErr := segmentMPEGTSMux(
			seg.Fpath, segmentDTS, duration, firstInit, nextDemuxer, lastDTS, m)
		nextFile.Close()
		if muxErr != nil {
			break
		}

		segmentEnd = seg.Start.Add(nextDuration)
		prevInit = nextInit
	}

	return m.flush()
}
