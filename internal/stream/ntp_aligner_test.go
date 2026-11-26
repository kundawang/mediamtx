package stream

import (
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/stretchr/testify/require"
)

func TestNTPAligner(t *testing.T) {
	video := &format.H264{PayloadTyp: 96}
	audio := &format.MPEG4Audio{
		PayloadTyp: 97,
		Config: &mpeg4audio.AudioSpecificConfig{
			Type:          mpeg4audio.ObjectTypeAACLC,
			SampleRate:    48000,
			ChannelCount:  2, //nolint:staticcheck
			ChannelConfig: 2,
		},
	}
	videoMedia := &description.Media{Formats: []format.Format{video}}
	audioMedia := &description.Media{Formats: []format.Format{audio}}

	t0 := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	const deliveryDelay = 300 * time.Millisecond

	t.Run("no report means no offset and no leading packet loss", func(t *testing.T) {
		a := &NTPAligner{}
		a.Initialize()

		a.ProcessPacket(videoMedia, video, 0, t0)
		a.ProcessPacket(audioMedia, audio, 0, t0.Add(deliveryDelay))

		require.Equal(t, time.Duration(0), a.Offset(videoMedia, video))
		require.Equal(t, time.Duration(0), a.Offset(audioMedia, audio))
	})

	t.Run("leading report only means no offset yet", func(t *testing.T) {
		a := &NTPAligner{}
		a.Initialize()

		a.ProcessPacket(videoMedia, video, 0, t0)
		a.ProcessSenderReport(videoMedia, video, 90000, t0.Add(time.Second), t0.Add(time.Second))

		a.ProcessPacket(audioMedia, audio, 0, t0.Add(deliveryDelay))

		// audio has no report yet: keep the legacy behaviour, no packet dropped
		require.Equal(t, time.Duration(0), a.Offset(audioMedia, audio))
	})

	t.Run("pure delivery delay on a shared clock is cancelled", func(t *testing.T) {
		a := &NTPAligner{}
		a.Initialize()

		// video first packet: local t0, capture NTP t0
		a.ProcessPacket(videoMedia, video, 0, t0)
		a.ProcessSenderReport(videoMedia, video, 90000, t0.Add(time.Second), t0.Add(time.Second))

		// audio first packet: local t0+300ms, but the same capture NTP t0
		a.ProcessPacket(audioMedia, audio, 0, t0.Add(deliveryDelay))
		a.ProcessSenderReport(audioMedia, audio, 48000, t0.Add(time.Second), t0.Add(1300*time.Millisecond))

		require.Equal(t, time.Duration(0), a.Offset(videoMedia, video))
		require.Equal(t, -deliveryDelay, a.Offset(audioMedia, audio))
	})

	t.Run("genuine capture offset is preserved", func(t *testing.T) {
		a := &NTPAligner{}
		a.Initialize()

		a.ProcessPacket(videoMedia, video, 0, t0)
		a.ProcessSenderReport(videoMedia, video, 90000, t0.Add(time.Second), t0.Add(time.Second))

		// audio capture clock is genuinely 100ms ahead of the video clock,
		// while the track is delivered 300ms later
		a.ProcessPacket(audioMedia, audio, 0, t0.Add(deliveryDelay))
		a.ProcessSenderReport(audioMedia, audio, 48000,
			t0.Add(1100*time.Millisecond), t0.Add(1300*time.Millisecond))

		// localTarget = t0+100ms, local anchor = t0+300ms: offset -200ms
		require.Equal(t, -200*time.Millisecond, a.Offset(audioMedia, audio))
	})

	t.Run("different clock rates are converted correctly", func(t *testing.T) {
		a := &NTPAligner{}
		a.Initialize()

		// video (90kHz): first PTS 100ms, a report at 1s maps to t0+900ms,
		// so the first packet capture NTP is t0
		a.ProcessPacket(videoMedia, video, 9000, t0)
		a.ProcessSenderReport(videoMedia, video, 90000,
			t0.Add(900*time.Millisecond), t0.Add(time.Second))

		// audio (8kHz-like 48kHz here): first PTS 500ms, a report at 1s maps
		// to t0+500ms, so the first packet capture NTP is also t0
		a.ProcessPacket(audioMedia, audio, 24000, t0.Add(deliveryDelay))
		a.ProcessSenderReport(audioMedia, audio, 48000,
			t0.Add(500*time.Millisecond), t0.Add(1300*time.Millisecond))

		// same capture origin despite different clock rates: only the 300ms
		// delivery delay must be corrected
		require.Equal(t, -deliveryDelay, a.Offset(audioMedia, audio))
	})

	t.Run("report in the future is rejected", func(t *testing.T) {
		a := &NTPAligner{}
		a.Initialize()

		a.ProcessPacket(videoMedia, video, 0, t0)
		a.ProcessSenderReport(videoMedia, video, 90000, t0.Add(time.Second), t0.Add(time.Second))

		a.ProcessPacket(audioMedia, audio, 0, t0.Add(deliveryDelay))
		// report claims a capture time one minute in the future: not credible
		a.ProcessSenderReport(audioMedia, audio, 48000,
			t0.Add(60*time.Second), t0.Add(1300*time.Millisecond))

		require.Equal(t, time.Duration(0), a.Offset(audioMedia, audio))
	})

	t.Run("backwards report on the same track is rejected", func(t *testing.T) {
		a := &NTPAligner{}
		a.Initialize()

		a.ProcessPacket(videoMedia, video, 0, t0)
		a.ProcessSenderReport(videoMedia, video, 90000, t0.Add(time.Second), t0.Add(time.Second))

		a.ProcessPacket(audioMedia, audio, 0, t0.Add(deliveryDelay))
		a.ProcessSenderReport(audioMedia, audio, 48000,
			t0.Add(time.Second), t0.Add(1300*time.Millisecond))
		require.Equal(t, -deliveryDelay, a.Offset(audioMedia, audio))

		// an older report must not replace the trusted one
		a.ProcessSenderReport(audioMedia, audio, 48000,
			t0.Add(500*time.Millisecond), t0.Add(1300*time.Millisecond))
		require.Equal(t, -deliveryDelay, a.Offset(audioMedia, audio))
	})

	t.Run("implausible offset is rejected", func(t *testing.T) {
		a := &NTPAligner{}
		a.Initialize()

		a.ProcessPacket(videoMedia, video, 0, t0)
		a.ProcessSenderReport(videoMedia, video, 90000, t0.Add(time.Second), t0.Add(time.Second))

		a.ProcessPacket(audioMedia, audio, 0, t0.Add(deliveryDelay))
		// the capture clocks disagree by several seconds: not a shared clock
		a.ProcessSenderReport(audioMedia, audio, 48000,
			t0.Add(11*time.Second), t0.Add(1300*time.Millisecond))

		require.Equal(t, time.Duration(0), a.Offset(audioMedia, audio))
	})

	t.Run("offset stays constant as the tracks advance", func(t *testing.T) {
		a := &NTPAligner{}
		a.Initialize()

		a.ProcessPacket(videoMedia, video, 0, t0)
		a.ProcessSenderReport(videoMedia, video, 90000, t0.Add(time.Second), t0.Add(time.Second))

		a.ProcessPacket(audioMedia, audio, 0, t0.Add(deliveryDelay))
		a.ProcessSenderReport(audioMedia, audio, 48000,
			t0.Add(time.Second), t0.Add(1300*time.Millisecond))

		// later reports with larger PTS must keep the same constant offset
		a.ProcessSenderReport(videoMedia, video, 100*90000,
			t0.Add(100*time.Second), t0.Add(100*time.Second))
		a.ProcessSenderReport(audioMedia, audio, 100*48000,
			t0.Add(100*time.Second), t0.Add(100300*time.Millisecond))

		require.Equal(t, -deliveryDelay, a.Offset(audioMedia, audio))
	})
}
