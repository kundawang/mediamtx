package playback

import (
	"os"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"

	"github.com/bluenviron/mediamtx/internal/recordstore"
)

// segmentMPEGTSMux reads a MPEG-TS segment and writes its samples to a muxer.
//
// startDTS is the DTS (relative to the playback start) of the beginning of the
// segment. Samples whose DTS is negative are still passed to the muxer in order
// to keep GOPs complete; the muxer discards everything before the first sync
// sample of the requested range.
//
// lastDTS holds, for each track, the DTS of the last sample already written by
// previous segments. Samples whose DTS is not greater than it are dropped: this
// removes the frames that two consecutive segments both contain around their
// boundary, preventing duplicate timestamps in the output. It may be nil for
// the first segment. The map is updated in place.
//
// Muxing stops as soon as every track has crossed the requested duration.
// It returns the duration of the segment, measured from its beginning to the
// last muxed sample.
func segmentMPEGTSMux(
	fpath string,
	startDTS time.Duration,
	duration time.Duration,
	init *fmp4.Init,
	demuxer *mpegtsDemuxer,
	lastDTS map[int]time.Duration,
	m muxer,
) (time.Duration, error) {
	timeScales := make(map[int]uint32)
	for _, track := range init.Tracks {
		timeScales[track.ID] = track.TimeScale
	}

	durationReached := make(map[int]bool)
	endDTSMP4 := make(map[int]int64)
	for _, track := range init.Tracks {
		endDTSMP4[track.ID] = durationGoToMp4(duration, track.TimeScale)
	}

	var segmentDuration time.Duration
	curTrackID := -1

	for _, sample := range demuxer.samples {
		track := demuxer.tracks[sample.trackIdx]
		trackID := track.initTrack.ID
		timeScale := timeScales[trackID]

		if trackID != curTrackID {
			curTrackID = trackID
			m.setTrack(trackID)
		}

		sampleDTS := startDTS + timestampToDuration90k(sample.dts)

		// drop samples overlapping with the previous segment
		if prev, ok := lastDTS[trackID]; ok && sampleDTS <= prev {
			continue
		}

		dtsMP4 := durationGoToMp4(sampleDTS, timeScale)
		ptsOffset := int32(durationGoToMp4(
			timestampToDuration90k(sample.pts-sample.dts), timeScale))

		// samples before the beginning of the requested range have a negative
		// DTS: they are still passed to the muxer, which keeps the GOP complete
		// and discards everything before the first sync sample. Only samples at
		// or after the beginning can terminate the track.
		if dtsMP4 >= 0 && dtsMP4 >= endDTSMP4[trackID] {
			durationReached[trackID] = true
		}

		// once the track has crossed the end of the requested range,
		// skip its remaining samples but keep processing the other tracks.
		if durationReached[trackID] {
			continue
		}

		payload := sample.payload

		err := m.writeSample(
			dtsMP4,
			ptsOffset,
			sample.nonSync,
			uint32(len(payload)),
			func() ([]byte, error) {
				return payload, nil
			},
		)
		if err != nil {
			return 0, err
		}

		m.writeFinalDTS(dtsMP4)

		if lastDTS != nil {
			lastDTS[trackID] = sampleDTS
		}

		if elapsed := sampleDTS - startDTS; elapsed > segmentDuration {
			segmentDuration = elapsed
		}
	}

	return segmentDuration, nil
}

// readMPEGTSSegment opens a segment file and returns its demuxer.
func readMPEGTSSegment(fpath string) (*os.File, *mpegtsDemuxer, error) {
	f, err := os.Open(fpath)
	if err != nil {
		return nil, nil, err
	}

	demuxer, err := newMPEGTSDemuxer(f)
	if err != nil {
		f.Close()
		return nil, nil, err
	}

	err = demuxer.read()
	if err != nil {
		f.Close()
		return nil, nil, err
	}

	return f, demuxer, nil
}

// parseMPEGTSSegment reads a MPEG-TS segment and returns its fMP4 init and
// duration, for use by the /list endpoint.
func parseMPEGTSSegment(seg *recordstore.Segment) (*parsedSegment, error) {
	f, demuxer, err := readMPEGTSSegment(seg.Fpath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	return &parsedSegment{
		start:    seg.Start,
		init:     demuxer.init(),
		duration: demuxer.duration(),
	}, nil
}
