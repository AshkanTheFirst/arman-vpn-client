package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/armanlife/arman-vpn-client/core/internal/config"
)

type relayTLSRoundTripFunc func(*http.Request) (*http.Response, error)

func (f relayTLSRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRelayTLS12RecoversStalledHTTPAndFollowsNetworkChanges(t *testing.T) {
	var modern, legacy atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS.Version == tls.VersionTLS13 {
			modern.Add(1)
			<-r.Context().Done()
			return
		}
		legacy.Add(1)
		if r.URL.RawQuery != "health=1&secret=fixture" || r.Header.Get("X-Fixture") != "preserved" {
			t.Error("retry changed the request")
		}
		io.WriteString(w, "ready")
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	base := server.Client().Transport.(*http.Transport).Clone()
	base.DisableKeepAlives = true
	route := newRelayTLSFallback(base, config.Network{APIRelayECH: config.RelayECHOff}, u.Hostname(), (&net.Dialer{}).DialContext, nil).(*relayTLSFallback)
	defer route.CloseIdleConnections()
	route.wait = 30 * time.Millisecond
	network := "network-one"
	route.network = func() string { return network }
	request := func(method string) {
		t.Helper()
		r, _ := http.NewRequestWithContext(t.Context(), method, server.URL+"/?health=1&secret=fixture", nil)
		r.Header.Set("X-Fixture", "preserved")
		response, err := route.RoundTrip(r)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || string(body) != "ready" {
			t.Fatal("TLS 1.2 retry did not return the relay response")
		}
	}
	request(http.MethodGet)
	request(http.MethodPost) // The successful read-only probe selected TLS 1.2.
	if modern.Load() != 1 || legacy.Load() != 2 {
		t.Fatal("the successful TLS mode was not reused")
	}
	network = "network-two"
	request(http.MethodGet)
	if modern.Load() != 2 || legacy.Load() != 3 {
		t.Fatal("a new network did not re-evaluate the primary TLS mode")
	}
}

func TestRelayTLSRetryDoesNotReplayWritesOrOverrideCertificatesAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, method string
		err          error
		cancelled    bool
		status       int
	}{
		{"post", "POST", os.ErrDeadlineExceeded, false, 0},
		{"certificate", "GET", x509.UnknownAuthorityError{}, false, 0},
		{"cancelled", "GET", context.Canceled, true, 0},
		{"http rejection", "GET", nil, false, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			route := &relayTLSFallback{wait: time.Second, network: func() string { return "test" }, primary: relayTLSRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if tc.err != nil {
					return nil, tc.err
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader("denied"))}, nil
			}), tls12: relayTLSRoundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Error("unexpected TLS fallback")
				return nil, errors.New("unexpected retry")
			})}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancelled {
				cancel()
			}
			req, _ := http.NewRequestWithContext(ctx, tc.method, "https://relay.example/", nil)
			resp, err := route.RoundTrip(req)
			if resp != nil {
				resp.Body.Close()
			}
			wantCalls := 1
			if tc.cancelled {
				wantCalls = 0
			}
			if calls != wantCalls || (tc.err != nil && err == nil) {
				t.Fatal("first result was not preserved")
			}
		})
	}
}

func TestRelayTLSHeaderBudgetDoesNotCutOffResponseBody(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-time.After(80 * time.Millisecond):
			io.WriteString(w, "complete")
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	base := server.Client().Transport.(*http.Transport).Clone()
	route := newRelayTLSFallback(base, config.Network{APIRelayECH: config.RelayECHOff}, u.Hostname(), (&net.Dialer{}).DialContext, nil).(*relayTLSFallback)
	route.wait = 40 * time.Millisecond
	defer route.CloseIdleConnections()
	req, _ := http.NewRequestWithContext(t.Context(), "GET", server.URL, nil)
	resp, err := route.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "complete" {
		t.Fatal("header budget cancelled the successful response body")
	}
}

func TestRelayTLSRequireNeverCreatesPlainTLSFallback(t *testing.T) {
	base := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13}}
	route := newRelayTLSFallback(base, config.Network{APIRelayECH: config.RelayECHRequire}, "relay.example", nil, nil)
	if route != base {
		t.Fatal("required ECH gained an ordinary TLS fallback")
	}
}

func TestRelayTLSRetryIsLimitedToOneAttemptAndPreservesFinalError(t *testing.T) {
	var first, second int
	route := &relayTLSFallback{wait: time.Second, network: func() string { return "test" },
		primary: relayTLSRoundTripFunc(func(*http.Request) (*http.Response, error) { first++; return nil, io.EOF }),
		tls12: relayTLSRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			second++
			if r.URL.RawQuery != "secret=fixture" || r.Header.Get("Authorization") != "Bearer fixture" {
				t.Error("retry lost request fields")
			}
			return nil, os.ErrDeadlineExceeded
		}),
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://relay.example/?secret=fixture", nil)
	req.Header.Set("Authorization", "Bearer fixture")
	resp, err := route.RoundTrip(req)
	if resp != nil || !errors.Is(err, os.ErrDeadlineExceeded) || first != 1 || second != 1 {
		t.Fatal("TLS retry loop or final error changed")
	}
}

func TestRelayHealthDetectsKnownDNSFilteringAddresses(t *testing.T) {
	for _, ip := range []string{"10.10.34.34", "10.10.34.35", "10.10.34.36", "::ffff:10.10.34.34"} {
		t.Run(ip, func(t *testing.T) {
			probe := newRelayHealthTest(config.Network{})
			probe.lookup = func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr(ip)}, nil }
			probe.dial = func(context.Context, string, string) (net.Conn, error) {
				t.Error("filtering address must not be dialed")
				return nil, errors.New("unexpected dial")
			}
			result := probe.run(t.Context(), "https://relay.example/?secret=fixture", nil)
			if result.OK || result.Steps[len(result.Steps)-1].Code != "blocked_dns" {
				t.Fatal("DNS filtering was not reported")
			}
		})
	}
}
