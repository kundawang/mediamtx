package stream

import (
	"encoding/binary"

	"github.com/pion/rtp"

	"github.com/bluenviron/mediamtx/internal/unit"
)

// h264RTPPacketIsKeyFrame reports whether a single H264 RTP packet carries the
// start of an IDR access unit. It supports single NALU, STAP-A and FU-A
// packetization.
func h264RTPPacketIsKeyFrame(pkt *rtp.Packet) bool {
	if pkt == nil || len(pkt.Payload) < 1 {
		return false
	}

	b0 := pkt.Payload[0]
	naluType := b0 & 0x1F

	switch naluType {
	case 5: // IDR slice, single NAL unit packet
		return true

	case 24: // STAP-A
		payload := pkt.Payload[1:]
		for len(payload) >= 2 {
			size := int(binary.BigEndian.Uint16(payload[:2]))
			payload = payload[2:]
			if size <= 0 || size > len(payload) {
				return false
			}
			if (payload[0] & 0x1F) == 5 {
				return true
			}
			payload = payload[size:]
		}
		return false

	case 28: // FU-A
		if len(pkt.Payload) < 2 {
			return false
		}
		fuHeader := pkt.Payload[1]
		if fuHeader&0x80 == 0 { // only the first fragment starts the NALU
			return false
		}
		reconstructedType := (b0 & 0xE0) | (fuHeader & 0x1F)
		return reconstructedType == 5

	default:
		return false
	}
}

// subStreamUnitIsKeyFrame reports whether the first media packet/unit is a
// video key frame. It is used to gate the switch-back to a returning publisher.
func subStreamUnitIsKeyFrame(u *unit.Unit) bool {
	for _, pkt := range u.RTPPackets {
		if h264RTPPacketIsKeyFrame(pkt) {
			return true
		}
	}

	if h264au, ok := u.Payload.(unit.PayloadH264); ok {
		for _, nalu := range h264au {
			if len(nalu) > 0 && (nalu[0]&0x1F) == 5 {
				return true
			}
		}
	}

	return false
}
