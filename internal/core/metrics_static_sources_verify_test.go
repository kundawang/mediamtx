package core

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	tscodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
	srtlib "github.com/datarhei/gosrt"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/test"
)

const (
	vrfAPIAddr     = "127.0.0.1:39997"
	vrfMetricsAddr = "127.0.0.1:39998"
	vrfRTSPFeed    = "127.0.0.1:38554"
	vrfSRTFeed     = "127.0.0.1:39001"
	vrfRTSPPath    = "rtsp_count"
	vrfSRTPath     = "srt_count"
)

// ---------- Prometheus scraping ----------

var vrfMetricLineRe = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)\{([^}]*)\}\s+(\S+)\s*$`)

func vrfFetchBody(t *testing.T, hc *http.Client, u string) string {
	t.Helper()
	res, err := hc.Get(u)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)
	byts, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return string(byts)
}

// vrfScrapeNameMetrics returns metric name -> value for the single series
// carrying name="<pathName>".
func vrfScrapeNameMetrics(t *testing.T, body string, pathName string) map[string]float64 {
	t.Helper()
	out := make(map[string]float64)
	for _, line := range strings.Split(body, "\n") {
		m := vrfMetricLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if !strings.Contains(m[2], `name="`+pathName+`"`) {
			continue
		}
		v, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			continue
		}
		out[m[1]] = v
	}
	return out
}

// ---------- API ----------

func vrfAPIStaticSource(t *testing.T, hc *http.Client, pathName string) (string, map[string]any) {
	t.Helper()

	res, err := hc.Get("http://" + vrfAPIAddr + "/v3/paths/static-sources/get/" + pathName)
	require.NoError(t, err)
	defer res.Body.Close()

	var resp struct {
		State        string         `json:"state"`
		Type         string         `json:"type"`
		TypeSpecific map[string]any `json:"typeSpecific"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&resp))
	return resp.State, resp.TypeSpecific
}

func vrfAPINum(ts map[string]any, key string) float64 {
	v, _ := ts[key].(float64)
	return v
}

// ---------- controllable RTSP counting source ----------

type vrfRTSPFeedHandler struct {
	onDescribe func(*gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error)
	onSetup    func(*gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error)
	onPlay     func(*gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error)
}

func (h *vrfRTSPFeedHandler) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx,
) (*base.Response, *gortsplib.ServerStream, error) {
	return h.onDescribe(ctx)
}

func (h *vrfRTSPFeedHandler) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx,
) (*base.Response, *gortsplib.ServerStream, error) {
	return h.onSetup(ctx)
}

func (h *vrfRTSPFeedHandler) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	return h.onPlay(ctx)
}

// vrfStartRTSPFeed starts an RTSP server that sends exactly packetCount RTP
// packets. If gap is true, one sequence number in the middle is skipped.
func vrfStartRTSPFeed(t *testing.T, packetCount int, gap bool) func() {
	t.Helper()

	media := test.UniqueMediaH264()
	var strm *gortsplib.ServerStream

	s := &gortsplib.Server{
		RTSPAddress: vrfRTSPFeed,
		Handler: &vrfRTSPFeedHandler{
			onDescribe: func(_ *gortsplib.ServerHandlerOnDescribeCtx,
			) (*base.Response, *gortsplib.ServerStream, error) {
				return &base.Response{StatusCode: base.StatusOK}, strm, nil
			},
			onSetup: func(_ *gortsplib.ServerHandlerOnSetupCtx,
			) (*base.Response, *gortsplib.ServerStream, error) {
				return &base.Response{StatusCode: base.StatusOK}, strm, nil
			},
			onPlay: func(_ *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
				go func() {
					seq := uint16(1000)
					ts0 := uint32(720000)
					gapAt := packetCount / 2
					for i := 0; i < packetCount; i++ {
						if gap && i == gapAt {
							seq++ // omit one packet -> one lost
						}
						pkt := &rtp.Packet{
							Version:        2,
							PayloadType:    96,
							SequenceNumber: seq,
							Timestamp:      ts0,
							SSRC:           0x12345678,
							Marker:         i%10 == 9,
							Payload:        []byte{5, byte(i + 1), 2, 3, 4},
						}
						if err := strm.WritePacketRTP(media, pkt); err != nil {
							return
						}
						seq++
						ts0 += 3600
						time.Sleep(2 * time.Millisecond)
					}
				}()
				return &base.Response{StatusCode: base.StatusOK}, nil
			},
		},
	}

	require.NoError(t, s.Start())

	strm = &gortsplib.ServerStream{
		Server: s,
		Desc:   &description.Session{Medias: []*description.Media{media}},
	}
	require.NoError(t, strm.Initialize())

	var once sync.Once
	return func() {
		once.Do(func() {
			strm.Close()
			s.Close()
		})
	}
}

// ---------- controllable SRT counting source (listener/sender) ----------

func vrfStartSRTFeed(t *testing.T) func() {
	t.Helper()

	cfg := srtlib.DefaultConfig()
	cfg.PeerIdleTimeout = 30 * time.Second

	ln, err := srtlib.Listen("srt", vrfSRTFeed, cfg)
	require.NoError(t, err)

	stop := make(chan struct{})
	acceptDone := make(chan struct{})

	go func() {
		defer close(acceptDone)
		for {
			req, err2 := ln.Accept2()
			if err2 != nil {
				return
			}
			go func(req srtlib.ConnRequest) {
				conn, err3 := req.Accept()
				if err3 != nil {
					return
				}
				defer conn.Close()

				track := &mpegts.Track{Codec: &tscodecs.H264{}}
				bw := bufio.NewWriter(conn)
				w := &mpegts.Writer{W: bw, Tracks: []*mpegts.Track{track}}
				if err4 := w.Initialize(); err4 != nil {
					return
				}

				ticker := time.NewTicker(50 * time.Millisecond)
				defer ticker.Stop()
				i := 0
				for {
					select {
					case <-stop:
						return
					case <-ticker.C:
					}

					if err5 := w.WriteH264(track, int64(i)*3600, int64(i)*3600, [][]byte{
						test.FormatH264.SPS,
						test.FormatH264.PPS,
						{5, byte((i % 200) + 1)},
					}); err5 != nil {
						return
					}
					if err5 := bw.Flush(); err5 != nil {
						return
					}
					i++
				}
			}(req)
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			ln.Close()
			<-acceptDone
		})
	}
}

// ---------- the test ----------

func TestMetricsStaticSourcesProtocolStats(t *testing.T) {
	closeRTSP1 := vrfStartRTSPFeed(t, 25, true)
	defer closeRTSP1() // replaced/closed explicitly in reconnect phase

	closeSRT := vrfStartSRTFeed(t)
	defer closeSRT()

	p, ok := newInstance(t, "logLevel: warn\n"+
		"readTimeout: 60s\n"+
		"writeTimeout: 60s\n"+
		"api: yes\n"+
		"apiAddress: "+vrfAPIAddr+"\n"+
		"metrics: yes\n"+
		"metricsAddress: "+vrfMetricsAddr+"\n"+
		"pprof: no\n"+
		"rtsp: no\n"+
		"rtmp: no\n"+
		"hls: no\n"+
		"webrtc: no\n"+
		"srt: no\n"+
		"moq: no\n"+
		"paths:\n"+
		"  "+vrfRTSPPath+":\n"+
		"    source: rtsp://tester:supersecret@"+vrfRTSPFeed+"/count\n"+
		"    rtspTransport: tcp\n"+
		"  "+vrfSRTPath+":\n"+
		"    source: srt://"+vrfSRTFeed+"?streamid=count\n")
	require.True(t, ok)
	defer p.Close()

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr}

	metricsURL := "http://" + vrfMetricsAddr + "/metrics"

	// 1. RTSP: API and /metrics agree on received / lost / jitter.
	var rtspAPI map[string]any
	require.Eventually(t, func() bool {
		state, ts := vrfAPIStaticSource(t, hc, vrfRTSPPath)
		rtspAPI = ts
		return state == "running" && vrfAPINum(ts, "inboundRTPPackets") == 25
	}, 30*time.Second, 200*time.Millisecond, "RTSP source did not count 25 packets")

	// allow jitter EWMA / stats to settle
	time.Sleep(300 * time.Millisecond)

	require.Eventually(t, func() bool {
		_, ts := vrfAPIStaticSource(t, hc, vrfRTSPPath)
		rtspAPI = ts
		body := vrfFetchBody(t, hc, metricsURL+"?path="+vrfRTSPPath)
		mx := vrfScrapeNameMetrics(t, body, vrfRTSPPath)

		return mx["rtsp_sources"] == 1 &&
			mx["rtsp_sources_inbound_rtp_packets"] == vrfAPINum(ts, "inboundRTPPackets") &&
			mx["rtsp_sources_inbound_rtp_packets_lost"] == vrfAPINum(ts, "inboundRTPPacketsLost") &&
			mathNear(mx["rtsp_sources_inbound_rtp_packets_jitter"], vrfAPINum(ts, "inboundRTPPacketsJitter"))
	}, 5*time.Second, 100*time.Millisecond, "RTSP API and metrics disagree")

	// loss was induced by a sequence gap and must be visible and consistent
	require.GreaterOrEqual(t, vrfAPINum(rtspAPI, "inboundRTPPacketsLost"), float64(1))
	t.Logf("RTSP api=%v", rtspAPI)

	bodyRTSPPath := vrfFetchBody(t, hc, metricsURL+"?path="+vrfRTSPPath)
	require.Contains(t, bodyRTSPPath, `rtsp_sources_inbound_rtp_packets{name="`+vrfRTSPPath+`"} 25`)
	require.NotContains(t, bodyRTSPPath, "supersecret")
	t.Logf("RTSP /metrics?path:\n%s", bodyRTSPPath)

	// 2. SRT: API and /metrics agree on receive/loss/retrans counters.
	require.Eventually(t, func() bool {
		state, ts := vrfAPIStaticSource(t, hc, vrfSRTPath)
		return state == "running" && vrfAPINum(ts, "packetsReceived") > 5
	}, 30*time.Second, 200*time.Millisecond, "SRT source did not receive packets")

	srtFields := map[string]string{
		"packetsReceived":        "srt_sources_packets_received",
		"packetsReceivedLoss":    "srt_sources_packets_received_loss",
		"packetsRetrans":         "srt_sources_packets_retrans",
		"packetsReceivedRetrans": "srt_sources_packets_received_retrans",
		"packetsReceivedDrop":    "srt_sources_packets_received_drop",
	}
	require.Eventually(t, func() bool {
		_, ts := vrfAPIStaticSource(t, hc, vrfSRTPath)
		body := vrfFetchBody(t, hc, metricsURL+"?path="+vrfSRTPath)
		mx := vrfScrapeNameMetrics(t, body, vrfSRTPath)
		if mx["srt_sources"] != 1 {
			return false
		}
		for apiKey, metricName := range srtFields {
			if mx[metricName] != vrfAPINum(ts, apiKey) {
				return false
			}
		}
		return true
	}, 10*time.Second, 200*time.Millisecond, "SRT API and metrics disagree")

	srtSample := vrfFetchBody(t, hc, metricsURL+"?type=srt_sources&path="+vrfSRTPath)
	for _, l := range strings.Split(srtSample, "\n") {
		if strings.HasPrefix(l, "srt_sources_packets_received{") ||
			strings.HasPrefix(l, "srt_sources_packets_received_loss{") ||
			strings.HasPrefix(l, "srt_sources_packets_retrans{") {
			t.Logf("SRT sample: %s", l)
		}
	}

	// 3. filters and coexistence.
	allBody := vrfFetchBody(t, hc, metricsURL)
	require.Contains(t, allBody, `name="`+vrfRTSPPath+`"`)
	require.Contains(t, allBody, `name="`+vrfSRTPath+`"`)

	rtspTypeBody := vrfFetchBody(t, hc, metricsURL+"?type=rtsp_sources")
	require.Contains(t, rtspTypeBody, "rtsp_sources{")
	require.NotContains(t, rtspTypeBody, "srt_sources{")

	srtTypeBody := vrfFetchBody(t, hc, metricsURL+"?type=srt_sources")
	require.Contains(t, srtTypeBody, "srt_sources{")
	require.NotContains(t, srtTypeBody, "rtsp_sources{")

	rtspPathBody := vrfFetchBody(t, hc, metricsURL+"?path="+vrfRTSPPath)
	require.Contains(t, rtspPathBody, `name="`+vrfRTSPPath+`"`)
	require.NotContains(t, rtspPathBody, `name="`+vrfSRTPath+`"`)

	noneBody := vrfFetchBody(t, hc, metricsURL+"?path=does_not_exist")
	require.NotContains(t, noneBody, "rtsp_sources{")
	require.NotContains(t, noneBody, "srt_sources{")

	// 4. reconnect: bounce the RTSP feeder with a different count; metrics
	// must follow the new source instead of accumulating / staying stale.
	closeRTSP1()
	closeRTSP2 := vrfStartRTSPFeed(t, 7, false)
	defer closeRTSP2()

	require.Eventually(t, func() bool {
		state, ts := vrfAPIStaticSource(t, hc, vrfRTSPPath)
		if state != "running" || vrfAPINum(ts, "inboundRTPPackets") != 7 {
			return false
		}
		body := vrfFetchBody(t, hc, metricsURL+"?type=rtsp_sources&path="+vrfRTSPPath)
		mx := vrfScrapeNameMetrics(t, body, vrfRTSPPath)
		return mx["rtsp_sources_inbound_rtp_packets"] == 7 &&
			mx["rtsp_sources_inbound_rtp_packets_lost"] == vrfAPINum(ts, "inboundRTPPacketsLost")
	}, 40*time.Second, 300*time.Millisecond, "metrics did not reset onto reconnected source")

	// 5. offline: stop the RTSP feeder for good; its static-source series must
	// vanish (the generic paths{name=...} notReady series is allowed to remain)
	// while the unrelated SRT source stays.
	closeRTSP2()

	require.Eventually(t, func() bool {
		body := vrfFetchBody(t, hc, metricsURL)
		return !vrfBodyHasFamilyForPath(body, "rtsp_sources", vrfRTSPPath)
	}, 30*time.Second, 300*time.Millisecond, "offline RTSP source left stale labels")

	afterBody := vrfFetchBody(t, hc, metricsURL)
	require.NotContains(t, afterBody, "rtsp_sources{name=\""+vrfRTSPPath+"\"}")
	require.False(t, vrfBodyHasFamilyForPath(afterBody, "rtsp_sources", vrfRTSPPath))
	// The path itself is still listed as not ready.
	require.Contains(t, afterBody, `paths{name="`+vrfRTSPPath+`",state="notReady"} 1`)

	// SRT source is still online and must not have been removed or disturbed.
	require.Eventually(t, func() bool {
		b := vrfFetchBody(t, hc, metricsURL+"?path="+vrfSRTPath)
		return strings.Contains(b, `srt_sources{name="`+vrfSRTPath+`"} 1`)
	}, 10*time.Second, 200*time.Millisecond, "SRT source disappeared after RTSP offline")

	// credentials must never appear anywhere in the metrics output.
	require.NotContains(t, vrfFetchBody(t, hc, metricsURL), "supersecret")
}

func vrfBodyHasFamilyForPath(body, prefix, pathName string) bool {
	needle := `name="` + pathName + `"`
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, prefix) && strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

func mathNear(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
