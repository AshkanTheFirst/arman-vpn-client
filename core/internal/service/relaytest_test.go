package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/armanlife/arman-vpn-client/core/internal/config"
	"github.com/armanlife/arman-vpn-client/core/internal/resolver"
)

func relayHealthFixture(t *testing.T, handler http.HandlerFunc) (*relayHealthTest, string) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	probe := newRelayHealthTest(config.Network{APIRelayECH: config.RelayECHOff})
	probe.tls = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	probe.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	probe.dial = (&net.Dialer{}).DialContext
	return probe, server.URL + "/?secret=fixture-secret"
}

func TestRelayHealthOnlyChecksTheWorkerAndStreamsProgress(t *testing.T) {
	calls := 0
	probe, address := relayHealthFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.Query().Get("health") != "1" || r.URL.Query().Get("secret") != "fixture-secret" || r.URL.Query().Has("url") {
			t.Error("unexpected health request")
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("account authorization must not reach the health endpoint")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"relay":"arman-vpn","version":1,"ready":true}`)
	})
	var progress []relayTestStep
	result := probe.run(t.Context(), address, func(step relayTestStep) { progress = append(progress, step) })
	if !result.OK || calls != 1 || len(progress) != len(result.Steps) || len(progress) < 5 {
		t.Fatalf("health test or progress failed: ok=%t calls=%d steps=%d", result.OK, calls, len(progress))
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "fixture-secret") || strings.Contains(string(encoded), "?secret=") {
		t.Fatal("the report leaked the relay address's secret")
	}
}

func TestRelayHealthRejectsUntrustedCertificates(t *testing.T) {
	probe, address := relayHealthFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted TLS reached HTTP") })
	probe.tls.RootCAs = nil
	result := probe.run(t.Context(), address, nil)
	if result.OK || result.Steps[len(result.Steps)-1].Code != "certificate" {
		t.Fatal("an untrusted relay certificate was accepted")
	}
}

func TestRelayHealthClassifiesResponsesWithoutEchoingBodies(t *testing.T) {
	for _, tc := range []struct {
		name               string
		status             int
		header, body, code string
	}{
		{"wrong secret", 403, "wrong secret", "fixture-secret account-response", "wrong_secret"},
		{"unconfigured", 503, "no secret is set in the relay", "fixture-secret", "not_configured"},
		{"quota", 429, "", "Error code: 1027 fixture-secret", "http_429"},
		{"redirect", 302, "", "fixture-secret", "http_302"},
		{"old script", 200, "", "<html>fixture-secret</html>", "unsupported_health"},
		{"not ready", 200, "", `{"relay":"arman-vpn","version":1,"ready":false}`, "not_ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			probe, address := relayHealthFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("X-Relay-Error", tc.header)
				w.Header().Set("Location", "/?secret=fixture-secret&redirected=1")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			result := probe.run(t.Context(), address, nil)
			if result.OK || calls != 1 || result.Steps[len(result.Steps)-1].Code != tc.code {
				t.Fatalf("unexpected response classification: %+v", result)
			}
			encoded, _ := json.Marshal(result)
			if strings.Contains(string(encoded), "fixture-secret") || strings.Contains(string(encoded), "account-response") {
				t.Fatal("a server response leaked into the report")
			}
		})
	}
}

func TestRelayHealthStopsAtDNSAndPreservesCancellation(t *testing.T) {
	probe := newRelayHealthTest(config.Network{})
	probe.lookup = func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("secret=fixture-secret") }
	probe.dial = func(context.Context, string, string) (net.Conn, error) {
		t.Error("DNS failure must stop before dialing")
		return nil, syscall.ECONNREFUSED
	}
	result := probe.run(t.Context(), "https://relay.example/?secret=fixture-secret", nil)
	if result.OK || result.Steps[len(result.Steps)-1].Code != "dns" {
		t.Fatal("DNS failure was not diagnosed")
	}
	probe.lookup = func(ctx context.Context, _ string) ([]netip.Addr, error) { <-ctx.Done(); return nil, ctx.Err() }
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result = probe.run(ctx, "https://relay.example/", nil)
	if result.OK || result.Steps[len(result.Steps)-1].Code != "cancelled" {
		t.Fatal("cancellation was lost")
	}
}

func TestRelayHealthTimeoutAndConnectionRefusal(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprint(timeout), func(t *testing.T) {
			probe := newRelayHealthTest(config.Network{APIRelayECH: config.RelayECHOff})
			probe.lookup = func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			probe.dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
				if timeout {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return nil, syscall.ECONNREFUSED
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			result := probe.run(ctx, "https://relay.example/?secret=fixture-secret", nil)
			want := "refused"
			if timeout {
				want = "timeout"
			}
			if result.OK || result.Steps[len(result.Steps)-1].Code != want {
				t.Fatalf("failure was not diagnosed as %s", want)
			}
		})
	}
}

func TestRelayHealthEndpointDoesNotSaveUnsavedAddress(t *testing.T) {
	d := newTestDaemon(t)
	c := newDashboardClient(t, d)
	before := d.config()
	for _, stream := range []bool{false, true} {
		response := c.do(http.MethodPost, "/api/relay/test", map[string]any{"url": "http://invalid.example/?secret=fixture-secret", "stream": stream})
		if response.Code != http.StatusOK {
			t.Fatal("diagnostics endpoint did not return a report")
		}
		if strings.Contains(response.Body.String(), "fixture-secret") {
			t.Fatal("unsaved relay secret was echoed")
		}
		if stream && !strings.Contains(response.Body.String(), `"type":"result"`) {
			t.Fatal("stream was not finalized")
		}
		if !stream {
			result := decodeBody[relayTestResult](t, response)
			if result.OK || result.Steps[0].Code != "invalid_address" {
				t.Fatal("invalid relay address was accepted")
			}
		}
	}
	if d.config().Network != before.Network {
		t.Fatal("the health test changed relay settings")
	}
}

func TestRelayHealthDashboardServesTheDiagnosticControls(t *testing.T) {
	d := newTestDaemon(t)
	c := newDashboardClient(t, d)
	for path, expected := range map[string]string{
		"/app/":       `id="relay-test-dialog"`,
		"/app/app.js": `async function runRelayTest()`,
	} {
		response := c.do(http.MethodGet, path, nil)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), expected) {
			t.Fatalf("diagnostic page asset was not served: %s", path)
		}
	}
}

func TestRelayHealthECHObservationsKeepFallbackAndStrictModes(t *testing.T) {
	for _, required := range []bool{false, true} {
		t.Run(fmt.Sprint(required), func(t *testing.T) {
			probe, address := relayHealthFixture(t, func(http.ResponseWriter, *http.Request) {})
			u, _ := url.Parse(address)
			mode := config.RelayECHOn
			if required {
				mode = config.RelayECHRequire
			}
			d, err := newECHDialer(u.Hostname(), config.Network{APIRelayECH: mode}, probe.dial)
			if err != nil {
				t.Fatal(err)
			}
			d.base = probe.tls
			d.lookup = func(context.Context, string) ([]byte, time.Duration, error) { return nil, 0, resolver.ErrNoECH }
			var stages []string
			d.observe = func(stage string, _ time.Time, _ error) { stages = append(stages, stage) }
			conn, err := d.DialTLSContext(t.Context(), "tcp", u.Host)
			if required {
				if conn != nil || err == nil || strings.Join(stages, ",") != "ech_key,ech_required" {
					t.Fatal("required ECH was not preserved by diagnostics")
				}
			} else {
				if err != nil {
					t.Fatal("ordinary TLS fallback failed")
				}
				conn.Close()
				if strings.Join(stages, ",") != "ech_key,tls" {
					t.Fatal("ECH fallback observations were incomplete")
				}
			}
		})
	}
}
