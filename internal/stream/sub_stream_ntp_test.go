package stream

import (
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/unit"
)

func TestSubStreamSetNTPOffset(t *testing.T) {
	t.Run("offset is applied to a regular stream", func(t *testing.T) {
		desc := &description.Session{Medias: []*description.Media{{
			Type:    description.MediaTypeAudio,
			Formats: []format.Format{&format.G711{PayloadTyp: 0, MULaw: true, SampleRate: 8000, ChannelCount: 1}},
		}}}

		strm := &Stream{
			OrigDesc:          desc,
			WriteQueueSize:    512,
			RTPMaxPayloadSize: 1450,
			ReplaceNTP:        true,
			Parent:            &nilLogger{},
		}
		require.NoError(t, strm.Initialize())
		defer strm.Close()

		subStream := &SubStream{Stream: strm, UseRTPPackets: true}
		require.NoError(t, subStream.Initialize())

		media := desc.Medias[0]
		forma := media.Formats[0]
		sf := strm.medias[media].formats[forma]
		require.NotNil(t, sf.ntpEstimator)

		subStream.SetNTPOffset(media, forma, 100*time.Millisecond)
		require.Equal(t, 100*time.Millisecond, sf.ntpEstimator.Offset())
	})

	t.Run("offset is ignored by an always available stream", func(t *testing.T) {
		tracks := []conf.AlwaysAvailableTrack{{Codec: conf.CodecH264}}

		strm := &Stream{
			AlwaysAvailable:       true,
			AlwaysAvailableTracks: tracks,
			WriteQueueSize:        512,
			RTPMaxPayloadSize:     1450,
			ReplaceNTP:            true,
			Parent:                &nilLogger{},
		}
		require.NoError(t, strm.Initialize())
		defer strm.Close()

		inDesc := &description.Session{Medias: []*description.Media{{
			Type:    description.MediaTypeVideo,
			Formats: []format.Format{&format.H264{PayloadTyp: 96, PacketizationMode: 1}},
		}}}

		subStream := &SubStream{Stream: strm, InDesc: inDesc, UseRTPPackets: false}
		require.NoError(t, subStream.Initialize())

		inMedia := inDesc.Medias[0]
		inForma := inMedia.Formats[0]
		ssf := subStream.medias[inMedia].formats[inForma]
		require.True(t, ssf.streamFormat.alwaysAvailable)
		require.NotNil(t, ssf.streamFormat.ntpEstimator)

		// applying an offset must be a no-op for always available streams
		subStream.SetNTPOffset(inMedia, inForma, 500*time.Millisecond)
		require.Equal(t, time.Duration(0), ssf.streamFormat.ntpEstimator.Offset())

		// writing a unit still works and the estimator reference is unchanged
		require.NotPanics(t, func() {
			subStream.WriteUnit(inMedia, inForma, &unit.Unit{PTS: 0})
		})
	})
}
