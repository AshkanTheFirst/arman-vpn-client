package service

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/armanlife/arman-vpn-client/core/internal/config"
)

const edgeFixtureHost = "relay.workers.dev"

func relayEdgeFixture(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(3), DNSNames: []string{edgeFixtureHost}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func TestRelayDomainFallbackPreservesRelayRequestAndCachesRoute(t *testing.T) {
	var normalDials, edgeDials, posts atomic.Int32
	server := relayEdgeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.TLS.ServerName != edgeFixtureHost || r.Host != edgeFixtureHost || r.URL.Path != "/relay.php" || r.URL.Query().Get("secret") != "fixture" || r.URL.Query().Get("url") != "https://vpn.mozilla.org/api/v1/fpn/token" || r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("alternate connection changed relay SNI, Host, path, query, or headers")
		}
		if r.Method == http.MethodPost {
			posts.Add(1)
			body, _ := io.ReadAll(r.Body)
			if string(body) != "fixture-body" {
				t.Error("cached route changed the POST body")
			}
		}
		io.WriteString(w, "ready")
	})
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "chatgpt.com:443" {
			edgeDials.Add(1)
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		}
		normalDials.Add(1)
		if address != edgeFixtureHost+":443" {
			t.Error("unexpected connection address")
		}
		return nil, syscall.ECONNREFUSED
	}
	base := &http.Transport{DialContext: dial, TLSClientConfig: server.Client().Transport.(*http.Transport).TLSClientConfig.Clone(), DisableKeepAlives: true}
	route := newRelayTLSFallback(base, config.Network{APIRelayECH: config.RelayECHOff}, edgeFixtureHost, dial, nil).(*relayTLSFallback)
	defer route.CloseIdleConnections()
	network := "first"
	route.network = func() string { return network }
	call := func(method string) {
		t.Helper()
		var body io.Reader
		if method == http.MethodPost {
			body = strings.NewReader("fixture-body")
		}
		req, _ := http.NewRequestWithContext(t.Context(), method, "https://"+edgeFixtureHost+"/relay.php?secret=fixture&url=https%3A%2F%2Fvpn.mozilla.org%2Fapi%2Fv1%2Ffpn%2Ftoken", body)
		req.Header.Set("Authorization", "Bearer fixture")
		originalURL := req.URL.String()
		resp, err := route.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil || string(data) != "ready" || resp.Request != req || req.URL.String() != originalURL {
			t.Fatal("response or original relay URL changed")
		}
	}
	call(http.MethodGet)
	call(http.MethodPost)
	if normalDials.Load() != 2 || edgeDials.Load() != 2 || posts.Load() != 1 {
		t.Fatal("alternate route was not cached or a write was replayed")
	}
	network = "second"
	call(http.MethodGet)
	if normalDials.Load() != 4 || edgeDials.Load() != 3 {
		t.Fatal("network change did not reset the selected connection route")
	}
}

func TestRelayDomainRetriesAreBoundedAndSafe(t *testing.T) {
	for _, tc := range []struct {
		name, method string
		failure      error
		want         int
	}{
		{"GET retries", "GET", io.EOF, 3},
		{"POST is not replayed", "POST", io.EOF, 1},
		{"certificate remains fatal", "GET", x509.UnknownAuthorityError{}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var order []string
			transport := func(name string) http.RoundTripper {
				return relayTLSRoundTripFunc(func(*http.Request) (*http.Response, error) { order = append(order, name); return nil, tc.failure })
			}
			route := &relayTLSFallback{primary: transport("normal"), tls12: transport("tls12"), edge: transport("edge"), wait: time.Second, network: func() string { return "test" }}
			req, _ := http.NewRequestWithContext(t.Context(), tc.method, "https://"+edgeFixtureHost+"/", nil)
			_, err := route.RoundTrip(req)
			if err == nil || len(order) != tc.want || strings.Join(order, ",") != strings.Join([]string{"normal", "tls12", "edge"}[:tc.want], ",") {
				t.Fatal("unexpected retry order or count")
			}
		})
	}
}

func TestRelayDomainDoesNotApplyToUnrelatedHostsOrRequiredECH(t *testing.T) {
	for _, host := range []string{"relay.example.com", "workers.dev.example.com", "notworkers.dev"} {
		route := newRelayTLSFallback(&http.Transport{}, config.Network{APIRelayECH: config.RelayECHOff}, host, nil, nil).(*relayTLSFallback)
		if route.edge != nil {
			t.Fatal("unrelated relay received a Cloudflare-specific fallback")
		}
		route.CloseIdleConnections()
	}
	base := &http.Transport{}
	if newRelayTLSFallback(base, config.Network{APIRelayECH: config.RelayECHRequire}, edgeFixtureHost, nil, nil) != base {
		t.Fatal("required ECH gained an unencrypted-name fallback")
	}
}

func TestRelayDomainCachedRouteDoesNotExtendItsLifetime(t *testing.T) {
	expires := time.Now().Add(time.Minute)
	route := &relayTLSFallback{preferred: "edge", until: expires, networkKey: "test", wait: time.Second, network: func() string { return "test" },
		primary: relayTLSRoundTripFunc(func(*http.Request) (*http.Response, error) { t.Error("cached route was not used"); return nil, io.EOF }),
		tls12:   relayTLSRoundTripFunc(func(*http.Request) (*http.Response, error) { t.Error("cached route was not used"); return nil, io.EOF }),
		edge: relayTLSRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ready"))}, nil
		}),
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+edgeFixtureHost+"/", nil)
	resp, err := route.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !route.until.Equal(expires) {
		t.Fatal("successful traffic postponed route expiration")
	}
}

func TestRelayHealthCanRecoverWorkerDNSFailureThroughDomain(t *testing.T) {
	server := relayEdgeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host != edgeFixtureHost || r.URL.Query().Get("health") != "1" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("unexpected health request")
		}
		io.WriteString(w, `{"relay":"arman-vpn","version":1,"ready":true}`)
	})
	for _, poisoned := range []bool{false, true} {
		probe := newRelayHealthTest(config.Network{APIRelayECH: config.RelayECHOff})
		probe.tls = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
		probe.lookup = func(context.Context, string) ([]netip.Addr, error) {
			if poisoned {
				return []netip.Addr{netip.MustParseAddr("10.10.34.34")}, nil
			}
			return nil, &net.DNSError{Name: edgeFixtureHost, Err: "no address", IsNotFound: true}
		}
		probe.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "chatgpt.com:443" {
				t.Error("unusable DNS result was dialed")
				return nil, errors.New("unexpected dial")
			}
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		}
		result := probe.run(t.Context(), "https://"+edgeFixtureHost+"/?secret=fixture", nil)
		if !result.OK {
			t.Fatalf("alternate health route failed: %+v", result)
		}
		var selected bool
		for _, step := range result.Steps {
			if step.Stage == "edge_selected" {
				selected = true
			}
		}
		encoded, _ := json.Marshal(result)
		if !selected || strings.Contains(string(encoded), "secret=fixture") {
			t.Fatal("alternate route was not explained safely")
		}
	}
}
