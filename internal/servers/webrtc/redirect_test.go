package webrtc

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/pion/rtp"
	pwebrtc "github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/auth"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/protocols/whip"
	"github.com/bluenviron/mediamtx/internal/stream"
	"github.com/bluenviron/mediamtx/internal/test"
	"github.com/bluenviron/mediamtx/internal/unit"
)

const (
	redirectTestAddress = "127.0.0.1:8886"
	redirectTestUser    = "myuser"
	redirectTestPass    = "mypass"
	// mediamtx accepts "Bearer user:pass" as an alternative to HTTP Basic auth
	redirectBearer = redirectTestUser + ":" + redirectTestPass
)

func newRedirectServer(t *testing.T, pm *test.PathManager) *Server {
	t.Helper()

	s := &Server{
		Address:               redirectTestAddress,
		AllowOrigins:          []string{"*"},
		TrustedProxies:        conf.IPNetworks{},
		ReadTimeout:           conf.Duration(10 * time.Second),
		WriteTimeout:          conf.Duration(10 * time.Second),
		LocalUDPAddress:       "127.0.0.1:8887",
		LocalTCPAddress:       "127.0.0.1:8887",
		IPsFromInterfaces:     true,
		IPsFromInterfacesList: []string{},
		AdditionalHosts:       []string{},
		ICEServers:            []conf.WebRTCICEServer{},
		STUNGatherTimeout:     conf.Duration(5 * time.Second),
		HandshakeTimeout:      conf.Duration(10 * time.Second),
		TrackGatherTimeout:    conf.Duration(2 * time.Second),
		PathManager:           pm,
		Parent:                test.NilLogger,
	}
	require.NoError(t, s.Initialize())
	return s
}

func newRedirectOffer(t *testing.T) string {
	t.Helper()

	pc, err := pwebrtc.NewPeerConnection(pwebrtc.Configuration{})
	require.NoError(t, err)
	defer pc.GracefulClose() //nolint:errcheck

	_, err = pc.AddTransceiverFromKind(pwebrtc.RTPCodecTypeVideo)
	require.NoError(t, err)

	offer, err := pc.CreateOffer(nil)
	require.NoError(t, err)

	return offer.SDP
}

func postWHEP(t *testing.T, hc *http.Client, rawURL string, offer string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader([]byte(offer)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/sdp")

	res, err := hc.Do(req)
	require.NoError(t, err)

	return res
}

func postWHEPWithBearer(t *testing.T, hc *http.Client, rawURL string, offer string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader([]byte(offer)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/sdp")
	req.Header.Set("Authorization", "Bearer "+redirectBearer)

	res, err := hc.Do(req)
	require.NoError(t, err)

	return res
}

// noRedirectClient never follows redirects.
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// newRedirectPathManager builds a path manager with one source stream and
// redirect aliases pointing at it. The redirect target depends on the mode.
func newRedirectPathManager(t *testing.T, strm *stream.Stream, absolute bool) *test.PathManager {
	t.Helper()

	target := "/path2"
	if absolute {
		target = "http://" + redirectTestAddress + "/path2"
	}

	return &test.PathManager{
		FindPathConfImpl: func(req defs.PathFindPathConfReq) (*defs.PathFindPathConfRes, error) {
			return &defs.PathFindPathConfRes{
				Conf:    &conf.Path{},
				AuthRes: &auth.Result{User: req.AccessRequest.Credentials.User},
			}, nil
		},
		DescribeImpl: func(req defs.PathDescribeReq) (*defs.PathDescribeRes, error) {
			require.Equal(t, redirectTestUser, req.AccessRequest.Credentials.User)
			require.Equal(t, redirectTestPass, req.AccessRequest.Credentials.Pass)

			switch req.AccessRequest.Name {
			case "alias1", "alias2":
				require.Equal(t, "param=value", req.AccessRequest.Query)
				return &defs.PathDescribeRes{Redirect: target}, nil

			case "path2":
				// the target is re-authenticated before serving the stream
				require.Equal(t, "param=value", req.AccessRequest.Query)
				return &defs.PathDescribeRes{Path: &dummyPath{}, Stream: strm}, nil

			default:
				return nil, &defs.PathNoStreamAvailableError{PathName: req.AccessRequest.Name}
			}
		},
		AddReaderImpl: func(req defs.PathAddReaderReq) (*defs.PathAddReaderRes, error) {
			// the reader is attached to the final target, never to an alias
			require.Equal(t, "path2", req.AccessRequest.Name)
			require.Equal(t, "param=value", req.AccessRequest.Query)
			require.Equal(t, redirectTestUser, req.AccessRequest.Credentials.User)
			return &defs.PathAddReaderRes{
				Path:    &dummyPath{},
				AuthRes: &auth.Result{User: req.AccessRequest.Credentials.User},
				Stream:  strm,
			}, nil
		},
	}
}

func newRedirectStream(t *testing.T) (*description.Session, *stream.SubStream) {
	t.Helper()

	desc := &description.Session{Medias: []*description.Media{test.MediaH264}}

	strm := &stream.Stream{
		OrigDesc:          desc,
		WriteQueueSize:    512,
		RTPMaxPayloadSize: 1450,
		Parent:            test.NilLogger,
	}
	require.NoError(t, strm.Initialize())

	subStream := &stream.SubStream{
		Stream:        strm,
		UseRTPPackets: false,
	}
	require.NoError(t, subStream.Initialize())

	return desc, subStream
}

func TestWHEPRedirectResponse(t *testing.T) {
	for _, ca := range []string{"relative", "absolute"} {
		t.Run(ca, func(t *testing.T) {
			_, subStream := newRedirectStream(t)

			s := newRedirectServer(t, newRedirectPathManager(t, subStream.Stream, ca == "absolute"))
			defer s.Close()

			hc := noRedirectClient()

			res := postWHEPWithBearer(t, hc,
				"http://"+redirectTestAddress+"/alias1/whep?param=value",
				newRedirectOffer(t))
			defer res.Body.Close()

			// 307 keeps the POST method (and its SDP body) on the same host
			require.Equal(t, http.StatusTemporaryRedirect, res.StatusCode)

			loc, err := url.Parse(res.Header.Get("Location"))
			require.NoError(t, err)
			require.Equal(t, "/path2/whep", loc.Path)
			require.Equal(t, "param=value", loc.RawQuery)
			require.Equal(t, redirectTestAddress, loc.Host)
		})
	}
}

func TestWHEPRedirectFrameRead(t *testing.T) {
	desc, subStream := newRedirectStream(t)

	s := newRedirectServer(t, newRedirectPathManager(t, subStream.Stream, false))
	defer s.Close()

	u, err := url.Parse("http://" + redirectTestAddress + "/alias2/whep?param=value")
	require.NoError(t, err)

	wc := &whip.Client{
		HTTPClient:  &http.Client{},
		URL:         u,
		BearerToken: redirectBearer,
		Log:         test.NilLogger,
	}

	caUnit := &unit.Unit{Payload: unit.PayloadH264{{5, 1}}}
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		// give the redirected peer connection time to establish, then emit a frame
		time.Sleep(500 * time.Millisecond)

		r := reflect.New(reflect.TypeOf(caUnit).Elem())
		r.Elem().Set(reflect.ValueOf(caUnit).Elem())
		subStream.WriteUnit(desc.Medias[0], desc.Medias[0].Formats[0], r.Interface().(*unit.Unit))
	}()

	require.NoError(t, wc.Initialize(context.Background()))
	defer checkClose(t, wc.Close)

	frameReceived := make(chan struct{}, 1)
	wc.InboundTracks()[0].OnPacketRTP = func(_ *rtp.Packet) {
		select {
		case frameReceived <- struct{}{}:
		default:
		}
	}

	wc.StartReading()

	<-writerDone
	select {
	case <-frameReceived:
	case <-time.After(5 * time.Second):
		t.Fatal("no frame received after redirect")
	}
}

func TestWHEPRedirectErrors(t *testing.T) {
	allowAll := func(req defs.PathFindPathConfReq) (*defs.PathFindPathConfRes, error) {
		return &defs.PathFindPathConfRes{
			Conf:    &conf.Path{},
			AuthRes: &auth.Result{User: req.AccessRequest.Credentials.User},
		}, nil
	}

	t.Run("target does not exist", func(t *testing.T) {
		pm := &test.PathManager{
			FindPathConfImpl: allowAll,
			DescribeImpl: func(req defs.PathDescribeReq) (*defs.PathDescribeRes, error) {
				if req.AccessRequest.Name == "alias" {
					return &defs.PathDescribeRes{Redirect: "/missing"}, nil
				}
				return nil, &defs.PathNoStreamAvailableError{PathName: req.AccessRequest.Name}
			},
		}

		s := newRedirectServer(t, pm)
		defer s.Close()

		hc := noRedirectClient()

		// first request: the alias redirects to the missing target
		res := postWHEP(t, hc, "http://"+redirectTestAddress+"/alias/whep", newRedirectOffer(t))
		require.Equal(t, http.StatusTemporaryRedirect, res.StatusCode)
		loc := res.Header.Get("Location")
		res.Body.Close()

		// following the redirect, the target reports a stable 404
		res2 := postWHEP(t, hc, loc, newRedirectOffer(t))
		defer res2.Body.Close()
		require.Equal(t, http.StatusNotFound, res2.StatusCode)
	})

	t.Run("self reference and loop", func(t *testing.T) {
		pm := &test.PathManager{
			FindPathConfImpl: allowAll,
			DescribeImpl: func(_ defs.PathDescribeReq) (*defs.PathDescribeRes, error) {
				// the path redirects to itself; a two-alias chain loops the same way
				return &defs.PathDescribeRes{Redirect: "/loop"}, nil
			},
		}

		s := newRedirectServer(t, pm)
		defer s.Close()

		// a client following redirects hits the standard 10-hop limit and
		// surfaces a stable error instead of looping forever
		req, err := http.NewRequest(http.MethodPost,
			"http://"+redirectTestAddress+"/loop/whep",
			bytes.NewReader([]byte(newRedirectOffer(t))))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/sdp")

		_, err = http.DefaultClient.Do(req)
		require.Error(t, err)
		require.Contains(t, err.Error(), "stopped after 10 redirects")

		// a client that does not follow redirects always gets the same response
		hc := noRedirectClient()
		res := postWHEP(t, hc, "http://"+redirectTestAddress+"/loop/whep", newRedirectOffer(t))
		defer res.Body.Close()
		require.Equal(t, http.StatusTemporaryRedirect, res.StatusCode)
		require.Contains(t, res.Header.Get("Location"), "/loop/whep")
	})

	t.Run("cross-host target is rejected", func(t *testing.T) {
		pm := &test.PathManager{
			FindPathConfImpl: allowAll,
			DescribeImpl: func(_ defs.PathDescribeReq) (*defs.PathDescribeRes, error) {
				return &defs.PathDescribeRes{
					Redirect: "http://other-host.example.com:8886/path2",
				}, nil
			},
		}

		s := newRedirectServer(t, pm)
		defer s.Close()

		hc := noRedirectClient()
		res := postWHEP(t, hc, "http://"+redirectTestAddress+"/alias/whep", newRedirectOffer(t))
		defer res.Body.Close()

		// the server refuses to point a credential-bearing request at another host
		require.Equal(t, http.StatusBadRequest, res.StatusCode)
		require.Equal(t, "", res.Header.Get("Location"))
	})
}

func TestWHEPRedirectAuthBoundary(t *testing.T) {
	pm := &test.PathManager{
		FindPathConfImpl: func(_ defs.PathFindPathConfReq) (*defs.PathFindPathConfRes, error) {
			return &defs.PathFindPathConfRes{
				Conf:    &conf.Path{},
				AuthRes: &auth.Result{},
			}, nil
		},
		DescribeImpl: func(req defs.PathDescribeReq) (*defs.PathDescribeRes, error) {
			if req.AccessRequest.Credentials.User == "" &&
				req.AccessRequest.Credentials.Token == "" {
				return nil, &auth.Error{
					AskCredentials: true,
					Wrapped:        fmt.Errorf("authentication error"),
				}
			}
			if req.AccessRequest.Name == "alias" {
				return &defs.PathDescribeRes{Redirect: "/path2"}, nil
			}
			return &defs.PathDescribeRes{Path: &dummyPath{}}, nil
		},
	}

	s := newRedirectServer(t, pm)
	defer s.Close()

	hc := noRedirectClient()

	// without credentials, no redirect target is revealed
	res := postWHEP(t, hc, "http://"+redirectTestAddress+"/alias/whep", newRedirectOffer(t))
	defer res.Body.Close()
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)
	require.Equal(t, `Basic realm="mediamtx"`, res.Header.Get("WWW-Authenticate"))
	require.Equal(t, "", res.Header.Get("Location"))

	// with credentials, the redirect is returned
	req, err := http.NewRequest(http.MethodPost,
		"http://"+redirectTestAddress+"/alias/whep",
		bytes.NewReader([]byte(newRedirectOffer(t))))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/sdp")
	req.Header.Set("Authorization", "Bearer "+redirectBearer)

	res2, err := hc.Do(req)
	require.NoError(t, err)
	defer res2.Body.Close()
	require.Equal(t, http.StatusTemporaryRedirect, res2.StatusCode)
	require.Contains(t, res2.Header.Get("Location"), "/path2/whep")
}

func TestWHEPRedirectReadPage(t *testing.T) {
	pm := &test.PathManager{
		FindPathConfImpl: func(req defs.PathFindPathConfReq) (*defs.PathFindPathConfRes, error) {
			return &defs.PathFindPathConfRes{
				Conf:    &conf.Path{},
				AuthRes: &auth.Result{User: req.AccessRequest.Credentials.User},
			}, nil
		},
		DescribeImpl: func(req defs.PathDescribeReq) (*defs.PathDescribeRes, error) {
			if req.AccessRequest.Name == "alias" {
				return &defs.PathDescribeRes{Redirect: "/path2?jwt=token"}, nil
			}
			return &defs.PathDescribeRes{Path: &dummyPath{}}, nil
		},
	}

	s := newRedirectServer(t, pm)
	defer s.Close()

	hc := noRedirectClient()

	t.Run("read page redirects to the target directory", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet,
			"http://"+redirectTestAddress+"/alias/?incoming=1", nil)
		require.NoError(t, err)

		res, err := hc.Do(req)
		require.NoError(t, err)
		defer res.Body.Close()

		require.Equal(t, http.StatusFound, res.StatusCode)
		require.Equal(t, "http://"+redirectTestAddress+"/path2/?incoming=1",
			res.Header.Get("Location"))
	})

	t.Run("publish page is not redirected", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet,
			"http://"+redirectTestAddress+"/alias/publish", nil)
		require.NoError(t, err)

		res, err := hc.Do(req)
		require.NoError(t, err)
		defer res.Body.Close()

		require.Equal(t, http.StatusOK, res.StatusCode)
	})
}

func TestWHEPRedirectURLLocation(t *testing.T) {
	req := &http.Request{
		Host: "example.com:8888",
		URL:  mustParseURL(t, "http://example.com:8888/alias/whep?jwt=abc"),
	}

	for _, ca := range []struct {
		name   string
		target string
		query  string
		want   string
	}{
		{
			name:   "relative path keeps host and query",
			target: "/path2",
			query:  "jwt=abc",
			want:   "http://example.com:8888/path2/whep?jwt=abc",
		},
		{
			name:   "relative path with embedded query is overridden",
			target: "/path2?embedded=1",
			query:  "jwt=abc",
			want:   "http://example.com:8888/path2/whep?jwt=abc",
		},
		{
			name:   "absolute same-host target",
			target: "http://example.com:8888/path2",
			query:  "jwt=abc",
			want:   "http://example.com:8888/path2/whep?jwt=abc",
		},
		{
			name:   "trailing slash is normalized",
			target: "/path2/",
			query:  "",
			want:   "http://example.com:8888/path2/whep",
		},
	} {
		t.Run(ca.name, func(t *testing.T) {
			got, err := whepRedirectLocation(req, ca.target, ca.query)
			require.NoError(t, err)
			require.Equal(t, ca.want, got)
		})
	}

	t.Run("cross-host absolute target is rejected", func(t *testing.T) {
		_, err := whepRedirectLocation(req, "http://evil.example.com:8888/path2", "")
		require.Error(t, err)
	})

	t.Run("case-insensitive host comparison", func(t *testing.T) {
		got, err := whepRedirectLocation(req, "http://EXAMPLE.COM:8888/path2", "")
		require.NoError(t, err)
		require.Equal(t, "http://EXAMPLE.COM:8888/path2/whep", got)
	})
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}
