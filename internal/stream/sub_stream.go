package stream

import (
	"fmt"
	"reflect"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"

	"github.com/bluenviron/mediamtx/internal/formatlabel"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/unit"
)

func formatMPEG4AudioConfig(asc *mpeg4audio.AudioSpecificConfig) string {
	return fmt.Sprintf("type=%d, sampleRate=%d, channelCount=%d",
		asc.Type, asc.SampleRate, asc.ChannelConfig)
}

func formatG711Config(f *format.G711) string {
	return fmt.Sprintf("MULaw=%v, sampleRate=%d, channelCount=%d",
		f.MULaw, f.SampleRate, f.ChannelCount)
}

func formatLPCMConfig(f *format.LPCM) string {
	return fmt.Sprintf("bitDepth=%d, sampleRate=%d, channelCount=%d",
		f.BitDepth, f.SampleRate, f.ChannelCount)
}

func mediasAreCompatible(medias1 []*description.Media, medias2 []*description.Media) error {
	if len(medias1) != len(medias2) {
		return fmt.Errorf("wants to publish %v, but stream expects %v",
			formatlabel.MediasToLabels(medias2), formatlabel.MediasToLabels(medias1))
	}

	for i := range medias1 {
		if len(medias1[i].Formats) != len(medias2[i].Formats) {
			return fmt.Errorf("wants to publish %v, but stream expects %v",
				formatlabel.MediasToLabels(medias2), formatlabel.MediasToLabels(medias1))
		}

		for j := range medias1[i].Formats {
			if reflect.TypeOf(medias1[i].Formats[j]) != reflect.TypeOf(medias2[i].Formats[j]) {
				return fmt.Errorf("wants to publish %v, but stream expects %v",
					formatlabel.MediasToLabels(medias2), formatlabel.MediasToLabels(medias1))
			}
		}
	}

	for i := range medias1 {
		for j := range medias1[i].Formats {
			switch format1 := medias1[i].Formats[j].(type) {
			case *format.MPEG4Audio:
				format2 := medias2[i].Formats[j].(*format.MPEG4Audio)

				if !reflect.DeepEqual(format1.Config, format2.Config) {
					return fmt.Errorf("MPEG-4 audio configuration does not match, is %s, but stream expects %s",
						formatMPEG4AudioConfig(format2.Config), formatMPEG4AudioConfig(format1.Config))
				}

			case *format.G711:
				format2 := medias2[i].Formats[j].(*format.G711)

				if format1.MULaw != format2.MULaw ||
					format1.SampleRate != format2.SampleRate ||
					format1.ChannelCount != format2.ChannelCount {
					return fmt.Errorf("G711 configuration does not match, is %s, but stream expects %s",
						formatG711Config(format2), formatG711Config(format1))
				}

			case *format.LPCM:
				format2 := medias2[i].Formats[j].(*format.LPCM)

				if format1.BitDepth != format2.BitDepth ||
					format1.SampleRate != format2.SampleRate ||
					format1.ChannelCount != format2.ChannelCount {
					return fmt.Errorf("LPCM configuration does not match, is %s, but stream expects %s",
						formatLPCMConfig(format2), formatLPCMConfig(format1))
				}
			}
		}
	}

	return nil
}

// SubStream is a Stream without interruptions.
type SubStream struct {
	Stream        *Stream
	InDesc        *description.Session
	UseRTPPackets bool

	// WaitForKeyFrame keeps a parked SubStream pending and promotes it
	// automatically when the first video key frame is received.
	WaitForKeyFrame bool

	// OnActivated is called after a key-frame promotion, outside the Stream
	// lock. It must not block for long.
	OnActivated func()

	medias map[*description.Media]*subStreamMedia
}

// Initialize initializes the SubStream.
func (ss *SubStream) Initialize() error {
	if !ss.Stream.AlwaysAvailable {
		if ss.Stream.subStream != nil {
			panic("should not happen")
		}

		if ss.InDesc != nil {
			panic("should not happen")
		}
	} else {
		if ss.InDesc == nil {
			panic("should not happen")
		}

		err := mediasAreCompatible(ss.Stream.OrigDesc.Medias, ss.InDesc.Medias)
		if err != nil {
			return err
		}
	}

	if !ss.Stream.AlwaysAvailable {
		ss.InDesc = ss.Stream.OrigDesc
	}

	err := ss.initializeMedias()
	if err != nil {
		return err
	}

	if ss.Stream.AlwaysAvailable {
		if ss.Stream.offlineSubStream != nil {
			ss.Stream.Parent.Log(logger.Info, "stream is online")

			// wait for the entire duration of the last sample of the offline sub stream
			// to minimize errors in clients.
			// TODO: it would be better in the future to wait for the last sample
			// of normal sub streams as well (this is currently impossible because
			// we don't know the duration of their samples).
			ss.Stream.offlineSubStream.close(true)
			ss.Stream.offlineSubStream = nil
		}
	}

	// keep mutex open to use writeUnit() inside initialize2()
	ss.Stream.mutex.Lock()
	defer ss.Stream.mutex.Unlock()

	ss.Stream.subStream = ss

	for _, ssm := range ss.medias {
		for _, ssf := range ssm.formats {
			ssf.initialize2(ss.Stream.firstTimeReceived, ss.Stream.lastPTS, ss.Stream.lastSystemTime)
		}
	}

	return nil
}

// InitializeParked initializes the SubStream without attaching it to the
// Stream. If WaitForKeyFrame is set, it is registered as pending and promoted
// on the first key frame.
func (ss *SubStream) InitializeParked() error {
	if ss.InDesc == nil {
		ss.InDesc = ss.Stream.OrigDesc
	} else if !ss.Stream.AlwaysAvailable {
		err := mediasAreCompatible(ss.Stream.OrigDesc.Medias, ss.InDesc.Medias)
		if err != nil {
			return err
		}
	}

	err := ss.initializeMedias()
	if err != nil {
		return err
	}

	ss.Stream.mutex.Lock()
	if ss.WaitForKeyFrame {
		ss.Stream.pendingSubStream = ss
	}
	ss.Stream.mutex.Unlock()

	return nil
}

func (ss *SubStream) initializeMedias() error {
	ss.medias = make(map[*description.Media]*subStreamMedia)

	for i, inMedia := range ss.InDesc.Medias {
		origMedia := ss.Stream.OrigDesc.Medias[i]

		ssm := &subStreamMedia{
			inMedia:       inMedia,
			streamMedia:   ss.Stream.medias[origMedia],
			useRTPPackets: ss.UseRTPPackets,
		}
		err := ssm.initialize()
		if err != nil {
			return err
		}
		ss.medias[inMedia] = ssm
	}

	return nil
}

// Activate attaches the SubStream to the Stream and rebases timestamps so that
// the output timeline continues from the last received frame. It is used to
// promote a parked SubStream.
func (ss *SubStream) Activate() {
	st := ss.Stream

	st.mutex.Lock()
	st.pendingSubStream = nil
	st.subStream = ss
	ss.markRebase()
	st.mutex.Unlock()
}

// IsActive reports whether the SubStream is the one currently writing to the
// Stream.
func (ss *SubStream) IsActive() bool {
	st := ss.Stream
	st.mutex.RLock()
	defer st.mutex.RUnlock()
	return st.subStream == ss
}

// Close detaches the SubStream from the Stream if it is active or pending.
func (ss *SubStream) Close() {
	st := ss.Stream
	st.mutex.Lock()
	if st.subStream == ss {
		st.subStream = nil
	}
	if st.pendingSubStream == ss {
		st.pendingSubStream = nil
	}
	st.mutex.Unlock()
}

func (ss *SubStream) markRebase() {
	if !ss.Stream.Continuous {
		return
	}

	for _, ssm := range ss.medias {
		for _, ssf := range ssm.formats {
			if ss.Stream.firstTimeReceived {
				ptsOffsetGo := ss.Stream.lastPTS + time.Since(ss.Stream.lastSystemTime)
				clockRate := int64(ssf.streamFormat.outFormat.ClockRate())
				ssf.streamFormat.rebaseTarget = multiplyAndDivide(
					int64(ptsOffsetGo), clockRate, int64(time.Second))
				ssf.streamFormat.needRebase = true
			} else {
				ssf.streamFormat.ptsOffset = 0
				ssf.streamFormat.needRebase = false
			}
		}
	}
}

// promotePending is called by WriteUnit when the pending SubStream receives
// its first key frame.
func (st *Stream) promotePending(ss *SubStream) bool {
	st.mutex.Lock()
	defer st.mutex.Unlock()

	if st.pendingSubStream != ss {
		return false
	}

	st.pendingSubStream = nil
	st.subStream = ss
	ss.markRebase()
	return true
}

// WriteUnit writes a Unit.
func (ss *SubStream) WriteUnit(inMedia *description.Media, inFormat format.Format, u *unit.Unit) {
	st := ss.Stream

	st.mutex.RLock()
	active := st.subStream == ss
	pending := st.pendingSubStream == ss && ss.WaitForKeyFrame
	st.mutex.RUnlock()

	if !active && !pending {
		return
	}

	ssm := ss.medias[inMedia]
	if ssm == nil {
		return
	}
	ssf := ssm.formats[inFormat]
	if ssf == nil {
		return
	}

	if pending {
		if !subStreamUnitIsKeyFrame(u) {
			return
		}

		if !st.promotePending(ss) {
			return
		}

		if ss.OnActivated != nil {
			ss.OnActivated()
		}
	}

	st.mutex.RLock()
	defer st.mutex.RUnlock()

	if st.subStream != ss {
		return
	}

	ssf.writeUnit(u)
}
