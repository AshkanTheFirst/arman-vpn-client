package service

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/armanlife/arman-vpn-client/core/internal/config"
)

func TestAutomaticRelayReservesTLSFallbackTimeForStalledECH(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	for _, required := range []bool{false, true} {
		t.Run(map[bool]string{false: "optional", true: "required"}[required], func(t *testing.T) {
			var calls atomic.Int32
			d := &echDialer{
				host: u.Hostname(), keyName: u.Hostname(), require: required,
				base: server.Client().Transport.(*http.Transport).TLSClientConfig.Clone(),
				lookup: func(ctx context.Context, _ string) ([]byte, time.Duration, error) {
					<-ctx.Done()
					return nil, 0, ctx.Err()
				},
				dial: func(ctx context.Context, network, address string) (net.Conn, error) {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
					calls.Add(1)
					return (&net.Dialer{}).DialContext(ctx, network, address)
				},
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			conn, err := boundedRelayTLS(d, 20*time.Millisecond)(ctx, "tcp", u.Host)
			if required {
				if err == nil || conn != nil || calls.Load() != 0 {
					t.Fatal("required ECH must not fall back to ordinary TLS")
				}
				return
			}
			if err != nil {
				t.Fatalf("optional ECH exhausted the TLS fallback: %v", err)
			}
			defer conn.Close()
			if calls.Load() != 1 || ctx.Err() != nil || !conn.(*tls.Conn).ConnectionState().HandshakeComplete {
				t.Fatal("fallback did not complete with the original live context")
			}
		})
	}
}

type relayPoolFixture struct {
	t              *testing.T
	server         *httptest.Server
	pool           *automaticRelays
	urls           []string
	enabled        bool
	mu             sync.Mutex
	requests       []string
	health         map[string]int
	status         map[string]int
	bareResponse   map[string]bool
	manifestStatus int
}

func newRelayPoolFixture(t *testing.T) *relayPoolFixture {
	t.Helper()
	f := &relayPoolFixture{t: t, enabled: true, health: map[string]int{}, status: map[string]int{}, bareResponse: map[string]bool{}, manifestStatus: 200}
	f.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.URL.Path+"?health="+r.URL.Query().Get("health"))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/invalid" {
			w.WriteHeader(503)
			return
		}
		if r.URL.Path == "/manifest" {
			w.WriteHeader(f.manifestStatus)
			json.NewEncoder(w).Encode(map[string]any{"relays": map[string]any{"enabled": f.enabled, "urls": f.urls}})
			return
		}
		if r.URL.Query().Get("health") == "1" {
			if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
				t.Error("health must not carry account credentials or cookies")
			}
			if r.URL.Query().Get("secret") != "fixture" {
				t.Error("worker secret was not retained")
			}
			status := f.health[r.URL.Path]
			if status == 0 {
				status = 200
			}
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]any{"relay": "arman-vpn", "version": 1, "ready": status == 200})
			return
		}
		if r.URL.Query().Get("url") != "https://vpn.mozilla.org/api/v1/fpn/token" {
			t.Error("wrong target")
		}
		if r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("missing authenticated request")
		}
		status := f.status[r.URL.Path]
		if status == 0 {
			status = 200
		}
		w.Header().Set("X-Quota-Limit", "5000")
		if !f.bareResponse[r.URL.Path] {
			w.Header().Set("X-Arman-Relay", "1")
		}
		w.Header().Set("X-Quota-Remaining", "4000")
		w.Header().Set("X-Quota-Reset", "2026-12-01T00:00:00Z")
		w.WriteHeader(status)
		io.WriteString(w, `{"token":"fixture-response"}`)
	}))
	t.Cleanup(f.server.Close)
	f.pool = newAutomaticRelays(config.Network{APIRelayECH: config.RelayECHOff})
	f.pool.manifestURLs = []string{f.server.URL + "/manifest"}
	f.pool.client = f.server.Client()
	f.pool.transport = func(*url.URL) (http.RoundTripper, error) { return f.server.Client().Transport, nil }
	f.pool.shuffle = func(int, func(int, int)) {}
	return f
}

func (f *relayPoolFixture) relay(path string) string { return f.server.URL + path + "?secret=fixture" }

func (f *relayPoolFixture) recover() *http.Response {
	f.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "https://vpn.mozilla.org/api/v1/fpn/token", nil)
	req.Header.Set("Authorization", "Bearer fixture")
	resp, err := f.pool.recover(req)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp
}

func TestAutomaticRelayChecksHealthBeforeAuthenticationAndSkipsDeadWorker(t *testing.T) {
	f := newRelayPoolFixture(t)
	f.urls = []string{f.relay("/dead"), f.relay("/good")}
	f.health["/dead"] = 503
	resp := f.recover()
	defer resp.Body.Close()
	// Reading after recover returns catches premature context cancellation.
	body, err := io.ReadAll(resp.Body)
	if err != nil || !strings.Contains(string(body), "fixture-response") {
		t.Fatalf("body unreadable: %v", err)
	}
	want := []string{"/manifest?health=", "/dead?health=1", "/good?health=1", "/good?health="}
	if strings.Join(f.requests, ",") != strings.Join(want, ",") {
		t.Fatalf("requests: %v", f.requests)
	}
	resp = f.recover()
	resp.Body.Close()
	if len(f.requests) != 6 || f.requests[4] != "/good?health=1" {
		t.Fatalf("healthy relay was not reused: %v", f.requests)
	}
}

func TestAutomaticRelayShufflesAndRotatesAfterGuardian451(t *testing.T) {
	f := newRelayPoolFixture(t)
	f.urls = []string{f.relay("/good"), f.relay("/blocked")}
	f.status["/blocked"] = 451
	var shuffled bool
	f.pool.shuffle = func(n int, swap func(int, int)) { shuffled = true; swap(0, n-1) }
	resp := f.recover()
	resp.Body.Close()
	if !shuffled || f.requests[1] != "/blocked?health=1" || f.pool.active != f.relay("/good") {
		t.Fatalf("selection: %v", f.requests)
	}
}

func TestAutomaticRelaySupportsVariableListsAndIgnoresUnsafeAddresses(t *testing.T) {
	f := newRelayPoolFixture(t)
	f.urls = []string{"http://plain.invalid/", "https://u:p@bad.invalid/", "https://bad.invalid/#fragment", "https://bad.invalid/?url=x"}
	for i := 0; i < 12; i++ {
		path := "/dead" + strings.Repeat("x", i)
		f.urls = append(f.urls, f.relay(path))
		f.health[path] = 503
	}
	f.urls = append(f.urls, f.relay("/good"), f.relay("/good"))
	resp := f.recover()
	resp.Body.Close()
	if len(f.pool.urls) != 13 || len(f.requests) != 15 {
		t.Fatalf("list/probes: %d/%d", len(f.pool.urls), len(f.requests))
	}
}

func TestAutomaticRelayDisabledMetadataIsAuthoritative(t *testing.T) {
	f := newRelayPoolFixture(t)
	f.enabled = false
	f.urls = []string{f.relay("/good")}
	f.pool.manifestURLs = append(f.pool.manifestURLs, f.server.URL+"/mirror")
	req, _ := http.NewRequest(http.MethodGet, "https://vpn.mozilla.org/api/v1/fpn/token", nil)
	resp, err := f.pool.recover(req)
	if resp != nil || err == nil || len(f.requests) != 1 {
		t.Fatalf("disabled pool used: %v %v %v", resp, err, f.requests)
	}
}

func TestAutomaticRelayManifestMirrorAndShortFailureCache(t *testing.T) {
	f := newRelayPoolFixture(t)
	f.urls = []string{f.relay("/good")}
	f.pool.manifestURLs = []string{f.server.URL + "/invalid", f.server.URL + "/manifest"}
	resp := f.recover()
	resp.Body.Close()
	if f.requests[0] != "/invalid?health=" || f.requests[1] != "/manifest?health=" {
		t.Fatal(f.requests)
	}
	f.pool.active = ""
	f.pool.urls = nil
	f.pool.refreshAt = time.Time{}
	f.manifestStatus = 503
	req, _ := http.NewRequest(http.MethodGet, "https://vpn.mozilla.org/api/v1/fpn/token", nil)
	f.pool.recover(req)
	count := len(f.requests)
	f.pool.recover(req)
	if len(f.requests) != count {
		t.Fatal("failed metadata was hammered")
	}
}

func TestAutomaticRelayIgnoresOtherHostsAndCancellation(t *testing.T) {
	f := newRelayPoolFixture(t)
	f.urls = []string{f.relay("/good")}
	for _, target := range []string{"https://api.accounts.firefox.com/v1/oauth/token", "https://vpn.mozilla.org.evil.invalid/api/v1/fpn/token", "https://vpn.mozilla.org/unrelated"} {
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		resp, err := f.pool.recover(req)
		if resp != nil || err != nil {
			t.Fatalf("non-Guardian request used fallback")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://vpn.mozilla.org/api/v1/fpn/token", nil)
	if _, err := f.pool.recover(req); err == nil || len(f.requests) != 0 {
		t.Fatal("cancelled request contacted a relay")
	}
}

func TestAutomaticRelayRejectsHTMLHealthWithoutSendingCredential(t *testing.T) {
	f := newRelayPoolFixture(t)
	var authenticated atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			authenticated.Add(1)
		}
		io.WriteString(w, "<html>Working</html>")
	}))
	defer server.Close()
	f.urls = []string{server.URL + "/?secret=fixture"}
	f.pool.transport = func(*url.URL) (http.RoundTripper, error) { return server.Client().Transport, nil }
	req, _ := http.NewRequest(http.MethodGet, "https://vpn.mozilla.org/api/v1/fpn/token", nil)
	req.Header.Set("Authorization", "Bearer fixture")
	resp, err := f.pool.recover(req)
	if resp != nil || err == nil || authenticated.Load() != 0 {
		t.Fatal("HTML page was trusted as a healthy worker")
	}
}

func TestAutomaticRelayRejectsQuotaAndAvailabilityErrors(t *testing.T) {
	for _, status := range []int{403, 429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newRelayPoolFixture(t)
			f.urls = []string{f.relay("/quota-exhausted"), f.relay("/good")}
			f.health["/quota-exhausted"] = status
			resp := f.recover()
			resp.Body.Close()
			for _, path := range f.requests {
				if path == "/quota-exhausted?health=" {
					t.Fatal("account credential sent to unavailable worker")
				}
			}
			if f.pool.active != f.relay("/good") {
				t.Fatal("healthy worker was not selected")
			}
		})
	}
}

func TestAutomaticRelayHEADKeepsQuotaHeaders(t *testing.T) {
	f := newRelayPoolFixture(t)
	f.urls = []string{f.relay("/good")}
	req, _ := http.NewRequest(http.MethodHead, "https://vpn.mozilla.org/api/v1/fpn/token", nil)
	req.Header.Set("Authorization", "Bearer fixture")
	resp, err := f.pool.recover(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Quota-Remaining") != "4000" {
		t.Fatal("quota headers lost")
	}
}

func TestAutomaticRelayQuotaFailureAfterSuccessfulHealthTriesNextWorker(t *testing.T) {
	f := newRelayPoolFixture(t)
	f.urls = []string{f.relay("/quota-race"), f.relay("/good")}
	f.status["/quota-race"] = 429
	f.bareResponse["/quota-race"] = true
	resp := f.recover()
	resp.Body.Close()
	if f.pool.active != f.relay("/good") {
		t.Fatal("worker quota failure was mistaken for account quota")
	}
}

func TestAutomaticRelayUsesTheLocalUpdateServerManifest(t *testing.T) {
	t.Setenv("ARMAN_VPN_UPDATE_SERVER", "http://127.0.0.1:18199/")
	pool := newAutomaticRelays(config.Network{})
	if len(pool.manifestURLs) != 1 || pool.manifestURLs[0] != "http://127.0.0.1:18199/updates.json" {
		t.Fatal("local release server did not supply the relay manifest")
	}
}
