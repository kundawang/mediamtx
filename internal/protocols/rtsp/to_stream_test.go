package rtsp

import (
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/stream"
	"github.com/bluenviron/mediamtx/internal/unit"
)

// fakeSource is a deterministic rtspSource used to feed ToStream without a
// real network connection. PTS and publisher NTP are fully controlled.
type fakeSource struct {
	handlers map[*description.Media]map[format.Format]gortsplib.OnPacketRTPFunc

	pts map[*rtp.Packet]int64
	ntp map[*rtp.Packet]time.Time
}

func newFakeSource() *fakeSource {
	return &fakeSource{
		handlers: make(map[*description.Media]map[format.Format]gortsplib.OnPacketRTPFunc),
		pts:      make(map[*rtp.Packet]int64),
		ntp:      make(map[*rtp.Packet]time.Time),
	}
}

func (s *fakeSource) OnPacketRTP(medi *description.Media, forma format.Format, cb gortsplib.OnPacketRTPFunc) {
	if s.handlers[medi] == nil {
		s.handlers[medi] = make(map[format.Format]gortsplib.OnPacketRTPFunc)
	}
	s.handlers[medi][forma] = cb
}

func (s *fakeSource) PacketPTS(_ *description.Media, pkt *rtp.Packet) (int64, bool) {
	pts, ok := s.pts[pkt]
	return pts, ok
}

func (s *fakeSource) PacketNTP(_ *description.Media, pkt *rtp.Packet) (time.Time, bool) {
	ntp, ok := s.ntp[pkt]
	return ntp, ok
}

func (s *fakeSource) emit(medi *description.Media, forma format.Format,
	pkt *rtp.Packet, pts int64, ntp time.Time,
) {
	s.pts[pkt] = pts
	if !ntp.IsZero() {
		s.ntp[pkt] = ntp
	}
	s.handlers[medi][forma](pkt)
}

type crossTrackHarness struct {
	source    *fakeSource
	subStream *stream.SubStream
	units     map[format.Format][]*unit.Unit
	unitsMu   sync.Mutex

	// alignMu serializes accesses to the aligner between the packet callback
	// (which runs in the emit goroutine) and the test goroutine
	alignMu sync.Mutex

	videoMedia *description.Media
	audioMedia *description.Media
	video      format.Format
	audio      format.Format
}

// addUnit appends a received unit in a race-safe way.
func (h *crossTrackHarness) addUnit(forma format.Format, u *unit.Unit) {
	h.unitsMu.Lock()
	defer h.unitsMu.Unlock()
	h.units[forma] = append(h.units[forma], u)
}

// unitCount returns the number of units received for a format.
func (h *crossTrackHarness) unitCount(forma format.Format) int {
	h.unitsMu.Lock()
	defer h.unitsMu.Unlock()
	return len(h.units[forma])
}

// unit returns a copy of the units received for a format.
func (h *crossTrackHarness) unitList(forma format.Format) []*unit.Unit {
	h.unitsMu.Lock()
	defer h.unitsMu.Unlock()
	out := make([]*unit.Unit, len(h.units[forma]))
	copy(out, h.units[forma])
	return out
}

// alignerOffset reads an aligner offset in a race-safe way.
func (h *crossTrackHarness) alignerOffset(medi *description.Media, forma format.Format) time.Duration {
	h.alignMu.Lock()
	defer h.alignMu.Unlock()
	if h.subStream.NTPAligner == nil {
		return 0
	}
	return h.subStream.NTPAligner.Offset(medi, forma)
}

// alignerAttached reports whether the aligner has been attached.
func (h *crossTrackHarness) alignerAttached() bool {
	h.alignMu.Lock()
	defer h.alignMu.Unlock()
	return h.subStream.NTPAligner != nil
}

// emit feeds a packet while holding the same lock used to read the aligner,
// so the test goroutine and the packet callback never race.
func (h *crossTrackHarness) emit(
	medi *description.Media, forma format.Format, pkt *rtp.Packet, pts int64, ntp time.Time,
) {
	h.alignMu.Lock()
	defer h.alignMu.Unlock()
	h.source.emit(medi, forma, pkt, pts, ntp)
}

func newCrossTrackHarness(t *testing.T, useAbsolute bool) *crossTrackHarness {
	t.Helper()

	video := &format.H264{PayloadTyp: 96, PacketizationMode: 1}
	audio := &format.G711{
		PayloadTyp:   97,
		MULaw:        true,
		SampleRate:   8000,
		ChannelCount: 1,
	}
	videoMedia := &description.Media{
		Type:    description.MediaTypeVideo,
		Formats: []format.Format{video},
	}
	audioMedia := &description.Media{
		Type:    description.MediaTypeAudio,
		Formats: []format.Format{audio},
	}
	desc := &description.Session{Medias: []*description.Media{videoMedia, audioMedia}}

	strm := &stream.Stream{
		OrigDesc:          desc,
		WriteQueueSize:    512,
		RTPMaxPayloadSize: 1450,
		ReplaceNTP:        !useAbsolute,
		Parent:            nilLogger{},
	}
	require.NoError(t, strm.Initialize())
	t.Cleanup(strm.Close)

	subStream := &stream.SubStream{
		Stream:        strm,
		UseRTPPackets: true,
	}
	require.NoError(t, subStream.Initialize())

	h := &crossTrackHarness{
		source:     newFakeSource(),
		subStream:  subStream,
		units:      make(map[format.Format][]*unit.Unit),
		videoMedia: videoMedia,
		audioMedia: audioMedia,
		video:      video,
		audio:      audio,
	}

	pathConf := &conf.Path{UseAbsoluteTimestamp: useAbsolute}
	ToStream(h.source, desc.Medias, pathConf, &subStream, nilLogger{})

	r := &stream.Reader{Parent: nilLogger{}}
	r.OnData(videoMedia, video, func(u *unit.Unit) error {
		h.addUnit(video, u)
		return nil
	})
	r.OnData(audioMedia, audio, func(u *unit.Unit) error {
		h.addUnit(audio, u)
		return nil
	})
	strm.AddReader(r)
	t.Cleanup(func() { strm.RemoveReader(r) })

	return h
}

type nilLogger struct{}

func (nilLogger) Log(logger.Level, string, ...any) {}

// TestToStreamCrossTrackAlignment verifies that two tracks sharing the same
// capture clock get aligned NTP timestamps, even when one track starts being
// delivered after the other.
func TestToStreamCrossTrackAlignment(t *testing.T) {
	// real local delivery delay between the first packet of the two tracks
	const deliveryDelay = 80 * time.Millisecond

	// packets are spaced by this amount and the PTS step matches it, so the
	// NTP estimator never has to re-anchor because of a discontinuity
	const step = 20 * time.Millisecond

	t.Run("without sender reports the start delay is preserved", func(t *testing.T) {
		h := newCrossTrackHarness(t, false)

		h.emit(h.videoMedia, h.video,
			&rtp.Packet{Header: rtp.Header{PayloadType: 96}, Payload: []byte{0x65}}, 0, time.Time{})
		time.Sleep(deliveryDelay)
		h.emit(h.audioMedia, h.audio,
			&rtp.Packet{Header: rtp.Header{PayloadType: 97}, Payload: []byte{0xff}}, 0, time.Time{})

		require.Eventually(t, func() bool {
			return h.unitCount(h.video) == 1 && h.unitCount(h.audio) == 1
		}, time.Second, 5*time.Millisecond)

		// without reports the two tracks keep their own local anchors, so the
		// delivery delay is reflected in the NTP difference
		videoUnits := h.unitList(h.video)
		audioUnits := h.unitList(h.audio)
		diff := audioUnits[0].NTP.Sub(videoUnits[0].NTP)
		require.InDelta(t, deliveryDelay, diff, float64(30*time.Millisecond))
	})

	t.Run("with consistent sender reports both tracks share one timeline", func(t *testing.T) {
		h := newCrossTrackHarness(t, false)

		// capture origin, close to the local clock so the reports are trusted
		base := time.Now()

		videoStep := int64(step * 90000 / time.Second) // 90kHz, one step
		audioStep := int64(step * 8000 / time.Second)  // 8kHz, one step

		newVideoPkt := func() *rtp.Packet {
			return &rtp.Packet{Header: rtp.Header{PayloadType: 96}, Payload: []byte{0x65}}
		}
		newAudioPkt := func() *rtp.Packet {
			return &rtp.Packet{Header: rtp.Header{PayloadType: 97}, Payload: []byte{0xff}}
		}

		// video starts immediately. Its first packet corresponds to capture base.
		h.emit(h.videoMedia, h.video, newVideoPkt(), 0, time.Time{})
		require.True(t, h.alignerAttached())

		// the audio track starts later, although it shares the same capture clock
		time.Sleep(deliveryDelay)
		h.emit(h.audioMedia, h.audio, newAudioPkt(), 0, time.Time{})
		time.Sleep(step)

		// before any report, no offset is applied and no packet is dropped
		require.Equal(t, time.Duration(0), h.alignerOffset(h.audioMedia, h.audio))

		// sender reports describing the same capture instant on both tracks
		h.emit(h.videoMedia, h.video, newVideoPkt(),
			videoStep, base.Add(step))
		h.emit(h.audioMedia, h.audio, newAudioPkt(),
			audioStep, base.Add(step))

		require.Eventually(t, func() bool {
			return h.unitCount(h.video) >= 2 && h.unitCount(h.audio) >= 2
		}, time.Second, 5*time.Millisecond)

		// both tracks describe the same capture clock, so the aligner must
		// cancel the measured delivery delay rather than keep the audio track
		// shifted
		audioOffset := h.alignerOffset(h.audioMedia, h.audio)
		require.InDelta(t, -deliveryDelay, audioOffset, float64(30*time.Millisecond))
		require.Equal(t, time.Duration(0), h.alignerOffset(h.videoMedia, h.video))

		// keep both tracks advancing: estimated NTP must never move backwards,
		// even though the audio offset is applied after its first packet
		videoPTS := videoStep
		audioPTS := audioStep
		for range 6 {
			time.Sleep(step)
			videoPTS += videoStep
			audioPTS += audioStep
			h.emit(h.videoMedia, h.video, newVideoPkt(), videoPTS, time.Time{})
			h.emit(h.audioMedia, h.audio, newAudioPkt(), audioPTS, time.Time{})
		}

		require.Eventually(t, func() bool {
			return h.unitCount(h.video) >= 8 && h.unitCount(h.audio) >= 8
		}, time.Second, 5*time.Millisecond)

		for _, forma := range []format.Format{h.video, h.audio} {
			units := h.unitList(forma)
			for i := 1; i < len(units); i++ {
				require.False(t, units[i].NTP.Before(units[i-1].NTP))
			}
		}
	})
}
