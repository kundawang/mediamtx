package metrics

import (
	"strings"

	"github.com/bluenviron/mediamtx/internal/defs"
)

type staticSourceRow struct {
	path string
	data *defs.APIStaticSource
}

// writeStaticSourcesMetrics writes protocol statistics of static sources.
// Statistics are sourced from the same APIPathManager.APIStaticSourcesGet data
// that backs the REST API, so values have the same meaning.
//
// Series are only emitted for sources that are currently online (running and
// exposing protocol statistics). Offline sources therefore leave no stale
// series behind, and reconnecting sources are reflected automatically since
// data is read live on every scrape.
//
// Labels only contain the path name, never the configured source URL, so
// credentials embedded in source URLs never reach Prometheus.
func writeStaticSourcesMetrics(
	out *strings.Builder,
	typ metricsType,
	pathFilter string,
	anyFilterActive bool,
	pathManager defs.APIPathManager,
) {
	if pathManager == nil {
		return
	}

	wantRTSP := typ == "" || typ == metricsTypeStaticSources || typ == metricsTypeRTSPSources
	wantSRT := typ == "" || typ == metricsTypeStaticSources || typ == metricsTypeSRTSources
	if (!wantRTSP && !wantSRT) || (anyFilterActive && pathFilter == "") {
		return
	}

	paths, err := pathManager.APIPathsList()
	if err != nil {
		return
	}

	var rtspRows []staticSourceRow
	var srtRows []staticSourceRow

	for _, pa := range paths.Items {
		if pathFilter != "" && pathFilter != pa.Name {
			continue
		}
		if pa.Source == nil {
			continue
		}

		typeIsRTSP := pa.Source.Type == defs.APIPathSourceTypeRTSPSource
		typeIsSRT := pa.Source.Type == defs.APIPathSourceTypeSRTSource
		if !((wantRTSP && typeIsRTSP) || (wantSRT && typeIsSRT)) {
			continue
		}

		data, err2 := pathManager.APIStaticSourcesGet(pa.Name)
		// A source is "online" only while its handler is running and the path
		// is ready. Requiring readiness also suppresses all-zero series during
		// the brief redial window, so a reconnecting/offline source leaves no
		// stale or flickering label set.
		if err2 != nil || data == nil ||
			data.State != defs.APIStaticSourceStateRunning || !pa.Ready {
			continue
		}

		switch data.TypeSpecific.(type) {
		case *defs.APIStaticSourceTypeSpecificRTSP:
			if wantRTSP && pa.Source.Type == defs.APIPathSourceTypeRTSPSource {
				rtspRows = append(rtspRows, staticSourceRow{path: pa.Name, data: data})
			}

		case *defs.APIStaticSourceTypeSpecificSRT:
			if wantSRT && pa.Source.Type == defs.APIPathSourceTypeSRTSource {
				srtRows = append(srtRows, staticSourceRow{path: pa.Name, data: data})
			}
		}
	}

	if wantRTSP {
		writeRTSPSourceMetrics(out, typ, pathFilter, rtspRows)
	}
	if wantSRT {
		writeSRTSourceMetrics(out, typ, pathFilter, srtRows)
	}
}

func writeRTSPSourceMetrics(
	out *strings.Builder,
	typ metricsType,
	pathFilter string,
	rows []staticSourceRow,
) {
	if len(rows) == 0 {
		// Emit the zero fallback only for an explicit type query, so the
		// default /metrics output stays unchanged when no source is present.
		if (typ == metricsTypeRTSPSources || typ == metricsTypeStaticSources) && pathFilter == "" {
			out.WriteString("# RTSP sources\n")
			metric(out, "rtsp_sources", "", 0)
			writeRTSPSourceStats(out, "", &defs.APIStaticSourceTypeSpecificRTSP{})
			out.WriteString("\n")
		}
		return
	}

	out.WriteString("# RTSP sources\n")
	for _, row := range rows {
		i := row.data.TypeSpecific.(*defs.APIStaticSourceTypeSpecificRTSP)
		ta := tags(map[string]string{"name": row.path})

		metric(out, "rtsp_sources", ta, 1)
		writeRTSPSourceStats(out, ta, i)
	}
	out.WriteString("\n")
}

func writeRTSPSourceStats(out *strings.Builder, ta string, i *defs.APIStaticSourceTypeSpecificRTSP) {
	metric(out, "rtsp_sources_inbound_bytes", ta, int64(i.InboundBytes))
	metric(out, "rtsp_sources_inbound_rtp_packets", ta, int64(i.InboundRTPPackets))
	metric(out, "rtsp_sources_inbound_rtp_packets_lost", ta, int64(i.InboundRTPPacketsLost))
	metric(out, "rtsp_sources_inbound_rtp_packets_in_error", ta, int64(i.InboundRTPPacketsInError))
	metricFloat(out, "rtsp_sources_inbound_rtp_packets_jitter", ta, i.InboundRTPPacketsJitter)
	metric(out, "rtsp_sources_inbound_rtcp_packets", ta, int64(i.InboundRTCPPackets))
	metric(out, "rtsp_sources_inbound_rtcp_packets_in_error", ta, int64(i.InboundRTCPPacketsInError))
	metric(out, "rtsp_sources_outbound_bytes", ta, int64(i.OutboundBytes))
	metric(out, "rtsp_sources_outbound_rtp_packets", ta, int64(i.OutboundRTPPackets))
	metric(out, "rtsp_sources_outbound_rtcp_packets", ta, int64(i.OutboundRTCPPackets))
}

func writeSRTSourceMetrics(
	out *strings.Builder,
	typ metricsType,
	pathFilter string,
	rows []staticSourceRow,
) {
	if len(rows) == 0 {
		// Emit the zero fallback only for an explicit type query, so the
		// default /metrics output stays unchanged when no source is present.
		if (typ == metricsTypeSRTSources || typ == metricsTypeStaticSources) && pathFilter == "" {
			out.WriteString("# SRT sources\n")
			metric(out, "srt_sources", "", 0)
			writeSRTSourceStats(out, "", &defs.APIStaticSourceTypeSpecificSRT{})
			out.WriteString("\n")
		}
		return
	}

	out.WriteString("# SRT sources\n")
	for _, row := range rows {
		i := row.data.TypeSpecific.(*defs.APIStaticSourceTypeSpecificSRT)
		ta := tags(map[string]string{"name": row.path})

		metric(out, "srt_sources", ta, 1)
		writeSRTSourceStats(out, ta, i)
	}
	out.WriteString("\n")
}

// writeSRTSourceStats emits the same statistics, with the same suffixes, as
// the srt_conns family.
func writeSRTSourceStats(out *strings.Builder, ta string, i *defs.APIStaticSourceTypeSpecificSRT) {
	metric(out, "srt_sources_packets_sent", ta, int64(i.PacketsSent))
	metric(out, "srt_sources_packets_received", ta, int64(i.PacketsReceived))
	metric(out, "srt_sources_packets_sent_unique", ta, int64(i.PacketsSentUnique))
	metric(out, "srt_sources_packets_received_unique", ta, int64(i.PacketsReceivedUnique))
	metric(out, "srt_sources_packets_send_loss", ta, int64(i.PacketsSendLoss))
	metric(out, "srt_sources_packets_received_loss", ta, int64(i.PacketsReceivedLoss))
	metric(out, "srt_sources_packets_retrans", ta, int64(i.PacketsRetrans))
	metric(out, "srt_sources_packets_received_retrans", ta, int64(i.PacketsReceivedRetrans))
	metric(out, "srt_sources_packets_sent_ack", ta, int64(i.PacketsSentACK))
	metric(out, "srt_sources_packets_received_ack", ta, int64(i.PacketsReceivedACK))
	metric(out, "srt_sources_packets_sent_nak", ta, int64(i.PacketsSentNAK))
	metric(out, "srt_sources_packets_received_nak", ta, int64(i.PacketsReceivedNAK))
	metric(out, "srt_sources_packets_sent_km", ta, int64(i.PacketsSentKM))
	metric(out, "srt_sources_packets_received_km", ta, int64(i.PacketsReceivedKM))
	metric(out, "srt_sources_us_snd_duration", ta, int64(i.UsSndDuration))
	metric(out, "srt_sources_packets_received_belated", ta, int64(i.PacketsReceivedBelated))
	metric(out, "srt_sources_packets_send_drop", ta, int64(i.PacketsSendDrop))
	metric(out, "srt_sources_packets_received_drop", ta, int64(i.PacketsReceivedDrop))
	metric(out, "srt_sources_packets_received_undecrypt", ta, int64(i.PacketsReceivedUndecrypt))
	metric(out, "srt_sources_bytes_sent", ta, int64(i.BytesSent))
	metric(out, "srt_sources_bytes_received", ta, int64(i.BytesReceived))
	metric(out, "srt_sources_bytes_sent_unique", ta, int64(i.BytesSentUnique))
	metric(out, "srt_sources_bytes_received_unique", ta, int64(i.BytesReceivedUnique))
	metric(out, "srt_sources_bytes_received_loss", ta, int64(i.BytesReceivedLoss))
	metric(out, "srt_sources_bytes_retrans", ta, int64(i.BytesRetrans))
	metric(out, "srt_sources_bytes_received_retrans", ta, int64(i.BytesReceivedRetrans))
	metric(out, "srt_sources_bytes_received_belated", ta, int64(i.BytesReceivedBelated))
	metric(out, "srt_sources_bytes_send_drop", ta, int64(i.BytesSendDrop))
	metric(out, "srt_sources_bytes_received_drop", ta, int64(i.BytesReceivedDrop))
	metric(out, "srt_sources_bytes_received_undecrypt", ta, int64(i.BytesReceivedUndecrypt))
	metricFloat(out, "srt_sources_us_packets_send_period", ta, i.UsPacketsSendPeriod)
	metric(out, "srt_sources_packets_flow_window", ta, int64(i.PacketsFlowWindow))
	metric(out, "srt_sources_packets_flight_size", ta, int64(i.PacketsFlightSize))
	metricFloat(out, "srt_sources_ms_rtt", ta, i.MsRTT)
	metricFloat(out, "srt_sources_mbps_send_rate", ta, i.MbpsSendRate)
	metricFloat(out, "srt_sources_mbps_receive_rate", ta, i.MbpsReceiveRate)
	metricFloat(out, "srt_sources_mbps_link_capacity", ta, i.MbpsLinkCapacity)
	metric(out, "srt_sources_bytes_avail_send_buf", ta, int64(i.BytesAvailSendBuf))
	metric(out, "srt_sources_bytes_avail_receive_buf", ta, int64(i.BytesAvailReceiveBuf))
	metricFloat(out, "srt_sources_mbps_max_bw", ta, i.MbpsMaxBW)
	metric(out, "srt_sources_bytes_mss", ta, int64(i.ByteMSS))
	metric(out, "srt_sources_packets_send_buf", ta, int64(i.PacketsSendBuf))
	metric(out, "srt_sources_bytes_send_buf", ta, int64(i.BytesSendBuf))
	metric(out, "srt_sources_ms_send_buf", ta, int64(i.MsSendBuf))
	metric(out, "srt_sources_ms_send_tsb_pd_delay", ta, int64(i.MsSendTsbPdDelay))
	metric(out, "srt_sources_packets_receive_buf", ta, int64(i.PacketsReceiveBuf))
	metric(out, "srt_sources_bytes_receive_buf", ta, int64(i.BytesReceiveBuf))
	metric(out, "srt_sources_ms_receive_buf", ta, int64(i.MsReceiveBuf))
	metric(out, "srt_sources_ms_receive_tsb_pd_delay", ta, int64(i.MsReceiveTsbPdDelay))
	metric(out, "srt_sources_packets_reorder_tolerance", ta, int64(i.PacketsReorderTolerance))
	metric(out, "srt_sources_packets_received_avg_belated_time", ta, int64(i.PacketsReceivedAvgBelatedTime))
	metricFloat(out, "srt_sources_packets_send_loss_rate", ta, i.PacketsSendLossRate)
	metricFloat(out, "srt_sources_packets_received_loss_rate", ta, i.PacketsReceivedLossRate)
}
