// Package rtsp provides RTSP utilities.
package rtsp

import (
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/stream"
	"github.com/bluenviron/mediamtx/internal/unit"
)

type ntpState int

const (
	ntpStateInitial ntpState = iota
	ntpStateReplace
	ntpStateAvailable
)

type rtspSource interface {
	PacketPTS(*description.Media, *rtp.Packet) (int64, bool)
	PacketNTP(*description.Media, *rtp.Packet) (time.Time, bool)
	OnPacketRTP(*description.Media, format.Format, gortsplib.OnPacketRTPFunc)
}

// ToStream maps a RTSP stream to a MediaMTX stream.
func ToStream(
	source rtspSource,
	medias []*description.Media,
	pathConf *conf.Path,
	subStream **stream.SubStream,
	log logger.Writer,
) {
	// In replace mode the publisher NTP is discarded and rebuilt from the
	// local clock. The aligner still reads the publisher RTCP sender reports
	// in order to align tracks sharing the same capture clock, while keeping
	// the resulting NTP anchored to the local clock.
	//
	// The aligner is attached to the sub stream inside the packet callback
	// because some callers pass a sub stream that is still nil at setup time
	// and gets populated only once the stream is ready.
	var aligner *stream.NTPAligner
	if !pathConf.UseAbsoluteTimestamp {
		aligner = &stream.NTPAligner{}
		aligner.Initialize()
	}

	for _, medi := range medias {
		for _, forma := range medi.Formats {
			cmedi := medi
			cforma := forma

			var ntpStat ntpState

			if !pathConf.UseAbsoluteTimestamp {
				ntpStat = ntpStateReplace
			}

			handleNTP := func(pkt *rtp.Packet) (time.Time, bool) {
				switch ntpStat {
				case ntpStateReplace:
					return time.Time{}, true

				case ntpStateInitial:
					ntp, avail := source.PacketNTP(cmedi, pkt)
					if !avail {
						log.Log(logger.Warn, "received RTP packet without absolute time, skipping it")
						return time.Time{}, false
					}

					ntpStat = ntpStateAvailable
					return ntp, true

				default: // ntpStateAvailable
					ntp, avail := source.PacketNTP(cmedi, pkt)
					if !avail {
						panic("should not happen")
					}

					return ntp, true
				}
			}

			source.OnPacketRTP(cmedi, cforma, func(pkt *rtp.Packet) {
				pts, ok := source.PacketPTS(cmedi, pkt)
				if !ok {
					return
				}

				ntp, ok := handleNTP(pkt)
				if !ok {
					return
				}

				if aligner != nil {
					now := time.Now()

					if (*subStream).NTPAligner == nil {
						(*subStream).NTPAligner = aligner
					}

					aligner.ProcessPacket(cmedi, cforma, pts, now)

					// publisher NTP is available only once a trusted RTCP
					// sender report has been received for this track
					if publisherNTP, avail := source.PacketNTP(cmedi, pkt); avail {
						aligner.ProcessSenderReport(cmedi, cforma, pts, publisherNTP, now)
					}

					(*subStream).SetNTPOffset(cmedi, cforma, aligner.Offset(cmedi, cforma))
				}

				(*subStream).WriteUnit(cmedi, cforma, &unit.Unit{
					PTS:        pts,
					NTP:        ntp,
					RTPPackets: []*rtp.Packet{pkt},
				})
			})
		}
	}
}
