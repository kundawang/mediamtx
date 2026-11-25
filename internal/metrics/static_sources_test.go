package metrics

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/test"
)

type staticSrcVerifyPathManager struct {
	dummyPathManager
	items []defs.APIPath
	rtsp  *defs.APIStaticSourceTypeSpecificRTSP
	srt   *defs.APIStaticSourceTypeSpecificSRT
}

func (m *staticSrcVerifyPathManager) APIPathsList() (*defs.APIPathList, error) {
	return &defs.APIPathList{Items: m.items}, nil
}

func (m *staticSrcVerifyPathManager) APIStaticSourcesGet(name string) (*defs.APIStaticSource, error) {
	switch name {
	case "rtspsrc":
		return &defs.APIStaticSource{
			State: defs.APIStaticSourceStateRunning,
			Type:  defs.APIPathSourceTypeRTSPSource,
			TypeSpecific: &defs.APIStaticSourceTypeSpecificRTSP{
				InboundRTPPackets:       111,
				InboundRTPPacketsLost:   4,
				InboundRTPPacketsJitter: 12.5,
			},
		}, nil

	case "srtsrc":
		return &defs.APIStaticSource{
			State: defs.APIStaticSourceStateRunning,
			Type:  defs.APIPathSourceTypeSRTSource,
			TypeSpecific: &defs.APIStaticSourceTypeSpecificSRT{
				PacketsReceived:     222,
				PacketsReceivedLoss: 6,
				PacketsRetrans:      7,
			},
		}, nil

	case "offlinesrc":
		return &defs.APIStaticSource{
			State:        defs.APIStaticSourceStateError,
			Type:         defs.APIPathSourceTypeRTSPSource,
			TypeSpecific: nil,
		}, nil
	}

	return nil, conf.ErrPathNotFound
}

func newStaticSrcVerifyMetrics(t *testing.T) *Metrics {
	t.Helper()

	m := &Metrics{
		Address:      "localhost:19991",
		AllowOrigins: []string{"*"},
		ReadTimeout:  conf.Duration(10 * time.Second),
		WriteTimeout: conf.Duration(10 * time.Second),
		AuthManager:  test.NilAuthManager,
		Parent:       test.NilLogger,
	}
	require.NoError(t, m.Initialize())
	t.Cleanup(m.Close)

	pm := &staticSrcVerifyPathManager{
		items: []defs.APIPath{
			{
				Name:   "rtspsrc",
				Ready:  true,
				Source: &defs.APIPathSource{Type: defs.APIPathSourceTypeRTSPSource},
			},
			{
				Name:   "srtsrc",
				Ready:  true,
				Source: &defs.APIPathSource{Type: defs.APIPathSourceTypeSRTSource},
			},
			{
				Name:   "offlinesrc",
				Ready:  false,
				Source: &defs.APIPathSource{Type: defs.APIPathSourceTypeRTSPSource},
			},
		},
	}
	m.SetPathManager(pm)

	return m
}

func staticSrcVerifyGet(t *testing.T, url string) string {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{}}
	res, err := hc.Get(url)
	require.NoError(t, err)
	defer res.Body.Close()
	byts, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return string(byts)
}

func TestStaticSourcesMetricsRender(t *testing.T) {
	newStaticSrcVerifyMetrics(t)

	bo := staticSrcVerifyGet(t, "http://localhost:19991/metrics?type=static_sources")

	require.Contains(t, bo, "# RTSP sources\n")
	require.Contains(t, bo, `rtsp_sources{name="rtspsrc"} 1`)
	require.Contains(t, bo, `rtsp_sources_inbound_rtp_packets{name="rtspsrc"} 111`)
	require.Contains(t, bo, `rtsp_sources_inbound_rtp_packets_lost{name="rtspsrc"} 4`)
	require.Contains(t, bo, `rtsp_sources_inbound_rtp_packets_jitter{name="rtspsrc"} 12.5`)

	require.Contains(t, bo, "# SRT sources\n")
	require.Contains(t, bo, `srt_sources{name="srtsrc"} 1`)
	require.Contains(t, bo, `srt_sources_packets_received{name="srtsrc"} 222`)
	require.Contains(t, bo, `srt_sources_packets_received_loss{name="srtsrc"} 6`)
	require.Contains(t, bo, `srt_sources_packets_retrans{name="srtsrc"} 7`)

	// offline source leaves no series
	require.NotContains(t, bo, `name="offlinesrc"`)
}

func TestStaticSourcesMetricsTypeFilter(t *testing.T) {
	newStaticSrcVerifyMetrics(t)

	rtspOnly := staticSrcVerifyGet(t, "http://localhost:19991/metrics?type=rtsp_sources")
	require.Contains(t, rtspOnly, `rtsp_sources{name="rtspsrc"} 1`)
	require.NotContains(t, rtspOnly, "srt_sources{")
	require.NotContains(t, rtspOnly, `name="srtsrc"`)

	srtOnly := staticSrcVerifyGet(t, "http://localhost:19991/metrics?type=srt_sources")
	require.Contains(t, srtOnly, `srt_sources{name="srtsrc"} 1`)
	require.NotContains(t, srtOnly, "rtsp_sources{")
}

func TestStaticSourcesMetricsPathFilter(t *testing.T) {
	newStaticSrcVerifyMetrics(t)

	one := staticSrcVerifyGet(t, "http://localhost:19991/metrics?path=srtsrc")
	require.Contains(t, one, `srt_sources_packets_received{name="srtsrc"} 222`)
	require.NotContains(t, one, `name="rtspsrc"`)

	missing := staticSrcVerifyGet(t, "http://localhost:19991/metrics?path=doesnotexist")
	require.NotContains(t, missing, "rtsp_sources{")
	require.NotContains(t, missing, "srt_sources{")
}

func TestStaticSourcesMetricsUnrelatedFilter(t *testing.T) {
	newStaticSrcVerifyMetrics(t)

	// Requesting an unrelated ID filter must hide path-scoped source families.
	bo := staticSrcVerifyGet(t, "http://localhost:19991/metrics?rtsp_conn=some-id")
	require.NotContains(t, bo, "rtsp_sources{")
	require.NotContains(t, bo, "srt_sources{")
}
