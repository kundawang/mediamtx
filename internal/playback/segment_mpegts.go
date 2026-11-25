package playback

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/ac3"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg1audio"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/opus"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mcodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	tscodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
)

func multiplyAndDivide(v, m, d int64) int64 {
	secs := v / d
	dec := v % d
	return secs*m + dec*m/d
}

func timestampToDuration90k(t int64) time.Duration {
	return time.Duration(multiplyAndDivide(t, int64(time.Second), 90000))
}

// mpegtsSample is a media sample extracted from a MPEG-TS segment.
type mpegtsSample struct {
	// DTS and PTS in 90kHz units, relative to the beginning of the segment.
	dts      int64
	pts      int64
	payload  []byte
	nonSync  bool
	trackIdx int
}

// mpegtsTrackState holds the state of a track while reading a segment.
type mpegtsTrackState struct {
	initTrack *fmp4.InitTrack
	// last DTS received, in 90kHz units relative to the segment beginning.
	lastDTS int64
	hasData bool
}

// mpegtsDemuxer extracts tracks and samples from a single MPEG-TS segment.
type mpegtsDemuxer struct {
	r *mpegts.Reader

	tracks  []*mpegtsTrackState
	samples []*mpegtsSample

	// DTS and PTS are decoded independently: a single TimeDecoder fed with
	// two interleaved time bases (B-frames make PTS differ from DTS) would
	// corrupt its internal state.
	dtsDecoder *mpegts.TimeDecoder
	ptsDecoder *mpegts.TimeDecoder
	firstDTS   int64
	hasFirst   bool
}

// mpegtsInitFromTSCodec maps a MPEG-TS codec to a fMP4 init track codec.
// nil is returned when the codec cannot be mapped.
func mpegtsInitFromTSCodec(codec tscodecs.Codec) (mcodecs.Codec, uint32, bool) {
	switch codec := codec.(type) {
	case *tscodecs.H264:
		return &mcodecs.H264{}, 90000, true

	case *tscodecs.H265:
		return &mcodecs.H265{}, 90000, true

	case *tscodecs.MPEG4Audio:
		return &mcodecs.MPEG4Audio{
			Config: codec.Config,
		}, uint32(codec.SampleRate), true

	case *tscodecs.Opus:
		channelCount := codec.ChannelCount
		if codec.Desc != nil {
			channelCount = codec.Desc.ChannelCount()
		}
		return &mcodecs.Opus{
			ChannelCount: channelCount,
		}, 48000, true

	case *tscodecs.AC3:
		return &mcodecs.AC3{
			SampleRate:   codec.SampleRate,
			ChannelCount: codec.ChannelCount,
		}, uint32(codec.SampleRate), true

	case *tscodecs.MPEG1Audio:
		// sample rate is not known until the first frame is parsed
		return &mcodecs.MPEG1Audio{}, 0, true

	case *tscodecs.MPEG4Video:
		return &mcodecs.MPEG4Video{}, 90000, true

	case *tscodecs.MPEG1Video:
		return &mcodecs.MPEG1Video{}, 90000, true

	default:
		return nil, 0, false
	}
}

// newMPEGTSDemuxer opens and initializes a MPEG-TS segment demuxer.
func newMPEGTSDemuxer(f *os.File) (*mpegtsDemuxer, error) {
	r := &mpegts.Reader{R: f}

	err := r.Initialize()
	if err != nil {
		return nil, err
	}

	d := &mpegtsDemuxer{
		r:          r,
		dtsDecoder: &mpegts.TimeDecoder{},
		ptsDecoder: &mpegts.TimeDecoder{},
	}
	d.dtsDecoder.Initialize()
	d.ptsDecoder.Initialize()

	nextID := 1

	for _, track := range r.Tracks() {
		codec, timeScale, ok := mpegtsInitFromTSCodec(track.Codec)
		if !ok {
			continue
		}

		ts := &mpegtsTrackState{
			initTrack: &fmp4.InitTrack{
				ID:        nextID,
				TimeScale: timeScale,
				Codec:     codec,
			},
		}
		nextID++

		idx := len(d.tracks)
		d.tracks = append(d.tracks, ts)

		switch track.Codec.(type) {
		case *tscodecs.H264:
			r.OnDataH264(track, func(pts int64, dts int64, au [][]byte) error {
				return d.onVideo(idx, pts, dts, au, true)
			})

		case *tscodecs.H265:
			r.OnDataH265(track, func(pts int64, dts int64, au [][]byte) error {
				return d.onVideo(idx, pts, dts, au, false)
			})

		case *tscodecs.MPEG4Audio:
			codec := track.Codec.(*tscodecs.MPEG4Audio)

			r.OnDataMPEG4Audio(track, func(pts int64, aus [][]byte) error {
				for i, au := range aus {
					framePTS := pts + multiplyAndDivide(
						int64(i)*mpeg4audio.SamplesPerAccessUnit, 90000, int64(codec.SampleRate))
					err := d.onAudio(idx, framePTS, au)
					if err != nil {
						return err
					}
				}
				return nil
			})

		case *tscodecs.Opus:
			r.OnDataOpus(track, func(pts int64, packets [][]byte) error {
				for _, packet := range packets {
					err := d.onAudio(idx, pts, packet)
					if err != nil {
						return err
					}
					pts += multiplyAndDivide(opus.PacketDuration2(packet), 90000, 48000)
				}
				return nil
			})

		case *tscodecs.AC3:
			codec := track.Codec.(*tscodecs.AC3)

			r.OnDataAC3(track, func(pts int64, frame []byte) error {
				err := d.onAudio(idx, pts, frame)
				if err != nil {
					return err
				}
				pts += multiplyAndDivide(ac3.SamplesPerFrame, 90000, int64(codec.SampleRate))
				return nil
			})

		case *tscodecs.MPEG1Audio:
			r.OnDataMPEG1Audio(track, func(pts int64, frames [][]byte) error {
				for _, frame := range frames {
					var h mpeg1audio.FrameHeader
					err := h.Unmarshal(frame)
					if err != nil {
						return err
					}

					if ts.initTrack.TimeScale == 0 {
						ts.initTrack.TimeScale = uint32(h.SampleRate)
						ts.initTrack.Codec = &mcodecs.MPEG1Audio{
							SampleRate:   h.SampleRate,
							ChannelCount: mpeg1audioChannelCount2(h.ChannelMode),
						}
					}

					err = d.onAudio(idx, pts, frame)
					if err != nil {
						return err
					}
					pts += multiplyAndDivide(int64(h.SampleCount()), 90000, int64(h.SampleRate))
				}
				return nil
			})

		case *tscodecs.MPEG4Video:
			r.OnDataMPEG4Video(track, func(pts int64, frame []byte) error {
				return d.onLegacyVideo(idx, pts, frame, true)
			})

		case *tscodecs.MPEG1Video:
			r.OnDataMPEG1Video(track, func(pts int64, frame []byte) error {
				return d.onLegacyVideo(idx, pts, frame, false)
			})
		}
	}

	if len(d.tracks) == 0 {
		return nil, fmt.Errorf("no supported tracks found in MPEG-TS segment")
	}

	return d, nil
}

// normalizeDTS converts an absolute (wrapped) MPEG-TS DTS into a DTS relative
// to the beginning of the segment.
func (d *mpegtsDemuxer) normalizeDTS(ts int64) int64 {
	v := d.dtsDecoder.Decode(ts)
	if !d.hasFirst {
		d.hasFirst = true
		d.firstDTS = v
	}
	return v - d.firstDTS
}

// normalizePTS converts an absolute (wrapped) MPEG-TS PTS into a timestamp
// relative to the beginning of the segment.
func (d *mpegtsDemuxer) normalizePTS(ts int64) int64 {
	return d.ptsDecoder.Decode(ts) - d.firstDTS
}

func (d *mpegtsDemuxer) onVideo(
	trackIdx int,
	pts int64,
	dts int64,
	au [][]byte,
	isH264 bool,
) error {
	ts := d.tracks[trackIdx]

	relDTS := d.normalizeDTS(dts)
	relPTS := d.normalizePTS(pts)

	var payload []byte
	var err error

	if isH264 {
		// extract SPS/PPS for the fMP4 init and remove parameter sets
		// and AUDs from the sample, since fMP4 carries them in the init.
		filtered := make(h264.AnnexB, 0, len(au))

		for _, nalu := range au {
			if len(nalu) == 0 {
				continue
			}

			switch h264.NALUType(nalu[0] & 0x1F) {
			case h264.NALUTypeSPS:
				codec := ts.initTrack.Codec.(*mcodecs.H264)
				if !bytes.Equal(codec.SPS, nalu) {
					codec.SPS = append([]byte(nil), nalu...)
				}

			case h264.NALUTypePPS:
				codec := ts.initTrack.Codec.(*mcodecs.H264)
				if !bytes.Equal(codec.PPS, nalu) {
					codec.PPS = append([]byte(nil), nalu...)
				}

			case h264.NALUTypeAccessUnitDelimiter:
				// not carried in fMP4

			default:
				filtered = append(filtered, nalu)
			}
		}

		if len(filtered) == 0 {
			return nil
		}

		payload, err = h264.AVCC(filtered).Marshal()
		if err != nil {
			return err
		}
	} else {
		codec := ts.initTrack.Codec.(*mcodecs.H265)

		filtered := make(h264.AnnexB, 0, len(au))

		for _, nalu := range au {
			if len(nalu) == 0 {
				continue
			}

			switch h265.NALUType((nalu[0] >> 1) & 0b111111) {
			case h265.NALUType_VPS_NUT:
				if !bytes.Equal(codec.VPS, nalu) {
					codec.VPS = append([]byte(nil), nalu...)
				}

			case h265.NALUType_SPS_NUT:
				if !bytes.Equal(codec.SPS, nalu) {
					codec.SPS = append([]byte(nil), nalu...)
				}

			case h265.NALUType_PPS_NUT:
				if !bytes.Equal(codec.PPS, nalu) {
					codec.PPS = append([]byte(nil), nalu...)
				}

			default:
				filtered = append(filtered, nalu)
			}
		}

		if len(filtered) == 0 {
			return nil
		}

		payload, err = h264.AVCC(filtered).Marshal()
		if err != nil {
			return err
		}
	}

	nonSync := true
	if isH264 {
		nonSync = !h264.IsRandomAccess(au)
	} else {
		nonSync = !h265.IsRandomAccess(au)
	}

	d.appendSample(trackIdx, relDTS, relPTS, payload, nonSync)
	return nil
}

func (d *mpegtsDemuxer) onLegacyVideo(
	trackIdx int,
	pts int64,
	frame []byte,
	isMPEG4 bool,
) error {
	ts := d.tracks[trackIdx]
	relPTS := d.normalizeDTS(pts)

	nonSync := true
	if isMPEG4 {
		codec := ts.initTrack.Codec.(*mcodecs.MPEG4Video)
		if bytes.HasPrefix(frame, []byte{0, 0, 1, 0xB0}) {
			if end := bytes.Index(frame[4:], []byte{0, 0, 1, 0xB3}); end >= 0 {
				config := frame[:end+8]
				if !bytes.Equal(codec.Config, config) {
					codec.Config = append([]byte(nil), config...)
				}
			}
		}
		nonSync = !bytes.Contains(frame, []byte{0, 0, 1, 0xB6})
	} else {
		codec := ts.initTrack.Codec.(*mcodecs.MPEG1Video)
		if bytes.HasPrefix(frame, []byte{0, 0, 1, 0xB3}) {
			if end := bytes.Index(frame[4:], []byte{0, 0, 1, 0xB8}); end >= 0 {
				config := frame[:end+8]
				if !bytes.Equal(codec.Config, config) {
					codec.Config = append([]byte(nil), config...)
				}
			}
		}
		nonSync = !bytes.Contains(frame, []byte{0, 0, 1, 0xB8})
	}

	d.appendSample(trackIdx, relPTS, relPTS, append([]byte(nil), frame...), nonSync)
	return nil
}

func (d *mpegtsDemuxer) onAudio(trackIdx int, pts int64, payload []byte) error {
	relPTS := d.normalizeDTS(pts)
	d.appendSample(trackIdx, relPTS, relPTS, append([]byte(nil), payload...), false)
	return nil
}

func (d *mpegtsDemuxer) appendSample(
	trackIdx int,
	dts int64,
	pts int64,
	payload []byte,
	nonSync bool,
) {
	ts := d.tracks[trackIdx]

	// drop samples that go backwards in time: they would produce negative
	// basetimes in fMP4 and break playback ordering.
	if ts.hasData && dts < ts.lastDTS {
		return
	}

	ts.hasData = true
	ts.lastDTS = dts

	d.samples = append(d.samples, &mpegtsSample{
		dts:      dts,
		pts:      pts,
		payload:  payload,
		nonSync:  nonSync,
		trackIdx: trackIdx,
	})
}

// read reads the whole segment. Non-fatal decode errors are tolerated:
// corrupted packets are skipped instead of aborting the playback.
func (d *mpegtsDemuxer) read() error {
	// non-fatal decode errors (e.g. a corrupted packet) must not stop
	// the playback of the rest of the segment.
	d.r.OnDecodeError(func(error) {})

	for {
		err := d.r.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// init returns the fMP4 init of the segment.
func (d *mpegtsDemuxer) init() *fmp4.Init {
	init := &fmp4.Init{
		Tracks: make([]*fmp4.InitTrack, len(d.tracks)),
	}
	for i, ts := range d.tracks {
		init.Tracks[i] = ts.initTrack
	}
	return init
}

// duration returns the duration of the segment, computed from the largest
// timestamp of any track.
func (d *mpegtsDemuxer) duration() time.Duration {
	var maxDTS int64
	for _, ts := range d.tracks {
		if ts.hasData && ts.lastDTS > maxDTS {
			maxDTS = ts.lastDTS
		}
	}
	return timestampToDuration90k(maxDTS)
}

func mpeg1audioChannelCount2(cm mpeg1audio.ChannelMode) int {
	switch cm {
	case mpeg1audio.ChannelModeStereo,
		mpeg1audio.ChannelModeJointStereo,
		mpeg1audio.ChannelModeDualChannel:
		return 2

	default:
		return 1
	}
}
