package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/armanlife/arman-vpn-client/core/internal/config"
	"github.com/armanlife/arman-vpn-client/core/internal/resolver"
)

const relayTestTimeout = 30 * time.Second

type relayTestStep struct {
	Stage     string `json:"stage"`
	Status    string `json:"status"`
	Code      string `json:"code,omitempty"`
	Detail    string `json:"detail"`
	ElapsedMS int64  `json:"elapsed_ms"`
}

type relayTestResult struct {
	OK      bool            `json:"ok"`
	Host    string          `json:"host"`
	TotalMS int64           `json:"total_ms"`
	Steps   []relayTestStep `json:"steps"`
}

// A report belongs to one explicit test. Late transport callbacks cannot
// write to an HTTP response after the final result or leak into other tests.
type relayTestReport struct {
	mu      sync.Mutex
	steps   []relayTestStep
	closed  bool
	started time.Time
	emit    func(relayTestStep)
}

func (r *relayTestReport) add(stage, status, code, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	step := relayTestStep{stage, status, code, detail, time.Since(r.started).Milliseconds()}
	r.steps = append(r.steps, step)
	if r.emit != nil {
		r.emit(step)
	}
}

func (r *relayTestReport) finish(ok bool, host string) relayTestResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return relayTestResult{ok, host, time.Since(r.started).Milliseconds(), append([]relayTestStep{}, r.steps...)}
}

// The dependencies are per test, so local fixtures can verify TLS and failure
// stages without changing the running resolver, transport, or account state.
type relayHealthTest struct {
	network config.Network
	lookup  func(context.Context, string) ([]netip.Addr, error)
	dial    func(context.Context, string, string) (net.Conn, error)
	tls     *tls.Config
}

func newRelayHealthTest(n config.Network) *relayHealthTest {
	return &relayHealthTest{
		network: n,
		lookup:  resolver.LookupNetIP,
		dial:    resolverDialer{timeout: apiRelayDialTimeout}.DialContext,
		tls:     &tls.Config{MinVersion: tls.VersionTLS12},
	}
}

func (t *relayHealthTest) run(parent context.Context, raw string, emit func(relayTestStep)) relayTestResult {
	ctx, cancel := context.WithTimeout(parent, relayTestTimeout)
	defer cancel()
	report := &relayTestReport{started: time.Now(), emit: emit}
	relay, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || relay.Scheme != "https" || relay.Hostname() == "" || relay.User != nil || relay.Fragment != "" || relay.Query().Has("url") {
		report.add("address", "error", "invalid_address", "Enter an HTTPS relay address without a target URL, username, or fragment.")
		return report.finish(false, "")
	}
	host := relay.Hostname()
	preferEdge := false
	report.add("address", "ok", "", "Relay endpoint: "+relay.Host+". Query values are hidden.")
	if t.network.UpstreamProxyInUse() != "" {
		report.add("dns", "info", "upstream_proxy", "The configured upstream proxy resolves the relay hostname and carries this test.")
	} else {
		report.add("dns", "info", "", "Resolving the relay hostname using the core's DNS settings.")
		lookupCtx, stop := context.WithTimeout(ctx, 6*time.Second)
		ips, lookupErr := t.lookup(lookupCtx, host)
		stop()
		if lookupErr != nil || len(ips) == 0 {
			code, detail := relayTestError(lookupErr)
			if lookupErr == nil {
				code, detail = "dns", "No IP address was returned for the relay hostname."
			} else if code == "transport" {
				code, detail = "dns", "DNS could not resolve the relay hostname using the configured servers."
			}
			if !relayEdgeEligible(t.network, host) {
				report.add("dns", "error", code, detail)
				return report.finish(false, host)
			}
			report.add("dns", "warning", code, detail)
			preferEdge = true
		}
		addresses := make([]string, 0, min(len(ips), 8))
		for _, ip := range ips {
			switch ip.Unmap().String() {
			case "10.10.34.34", "10.10.34.35", "10.10.34.36":
				if !relayEdgeEligible(t.network, host) {
					report.add("dns", "error", "blocked_dns", "DNS returned a known Iranian filtering address instead of the relay. Try another DNS service or a relay on a custom domain/PHP host.")
					return report.finish(false, host)
				}
				report.add("dns", "warning", "blocked_dns", "DNS returned a known Iranian filtering address; testing the alternative connection through chatgpt.com.")
				preferEdge = true
			}
		}
		for _, ip := range ips[:min(len(ips), 8)] {
			addresses = append(addresses, ip.String())
		}
		if !preferEdge {
			report.add("dns", "ok", "", "Resolved addresses: "+strings.Join(addresses, ", "))
		}
	}

	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		report.add("tcp", "info", "", "Opening a connection to "+address+".")
		conn, err := t.dial(ctx, network, address)
		if err != nil {
			code, detail := relayTestError(err)
			report.add("tcp", "warning", code, detail)
		} else {
			report.add("tcp", "ok", "", "Connected to "+conn.RemoteAddr().String())
		}
		return conn, err
	}
	transport := &http.Transport{
		Proxy: nil, DialContext: dial, DisableKeepAlives: true,
		TLSClientConfig: t.tls.Clone(), TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	}
	defer transport.CloseIdleConnections()
	observe := func(stage string, started time.Time, err error) {
		status, code, detail := "ok", "", ""
		switch stage {
		case "ech_key":
			detail = "ECH configuration is available."
		case "tls_ech":
			detail = "TLS connected with ECH; the certificate was verified."
		case "tls":
			detail = "TLS connected without ECH; the certificate was verified."
		case "tls12_retry":
			status, code, detail = "info", "retry", "The first relay attempt did not complete; retrying once with TLS 1.2 without ECH."
		case "tls12":
			detail = "TLS 1.2 connected; the certificate was verified."
		case "edge_retry":
			status, code, detail = "info", "retry", "The relay could not be reached normally; retrying through chatgpt.com with the relay's SNI and HTTP Host."
		case "edge_selected":
			status, detail = "info", "Using the relay's previously successful connection through chatgpt.com."
		case "tls_edge":
			detail = "TLS connected through chatgpt.com with the relay's server name; the certificate was verified."
		}
		if err != nil {
			status = "warning"
			code, detail = relayTestError(err)
		}
		if stage == "ech_required" {
			status, code, detail = "error", "ech_required", "ECH is required by the settings; ordinary TLS fallback was not attempted."
		}
		report.add(stage, status, code, fmt.Sprintf("%s (%d ms)", detail, time.Since(started).Milliseconds()))
		if err == nil && (stage == "tls" || stage == "tls_ech" || stage == "tls12" || stage == "tls_edge") {
			report.add("http", "info", "waiting", "TLS is ready; sending the health request and waiting for its HTTP response.")
		}
	}
	if t.network.APIRelayECH != config.RelayECHOff {
		report.add("ech_key", "info", "", "Looking up ECH configuration; mode="+t.network.APIRelayECH+".")
		d, err := newECHDialer(host, t.network, dial)
		if err != nil {
			report.add("ech_key", "error", "ech_config", "The configured ECH source is invalid.")
			return report.finish(false, host)
		}
		d.base, d.observe = t.tls.Clone(), observe
		wait := relayTLSAttemptWait / 2
		if d.require {
			wait = 8 * time.Second
		}
		transport.DialTLSContext = boundedRelayTLS(d, wait)
	} else {
		report.add("tls", "info", "", "ECH is disabled; using certificate-verified TLS.")
		transport.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			raw, err := dial(ctx, network, address)
			if err != nil {
				return nil, err
			}
			cfg := t.tls.Clone()
			cfg.ServerName = host
			conn := tls.Client(raw, cfg)
			started := time.Now()
			err = conn.HandshakeContext(ctx)
			observe("tls", started, err)
			if err != nil {
				raw.Close()
				return nil, err
			}
			return conn, nil
		}
	}
	probe := *relay
	query := probe.Query()
	query.Set("health", "1")
	probe.RawQuery = query.Encode()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, probe.String(), nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cache-Control", "no-cache")
	route := newRelayTLSFallback(transport, t.network, host, dial, observe)
	if fallback, ok := route.(*relayTLSFallback); ok && preferEdge {
		fallback.remember("edge")
	}
	if closer, ok := route.(interface{ CloseIdleConnections() }); ok {
		defer closer.CloseIdleConnections()
	}
	client := &http.Client{Transport: route, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	report.add("http", "info", "", "Sending a local relay health request; no account authorization or Guardian request is sent.")
	resp, err := client.Do(req)
	if err != nil {
		code, detail := relayTestError(err)
		if strings.HasSuffix(strings.ToLower(host), ".workers.dev") && (code == "timeout" || code == "reset" || code == "eof") {
			report.add("network", "warning", "filtering_possible", "This workers.dev relay did not answer. Network filtering, edge connectivity, or a relay outage are possible; this result alone does not prove domain blocking. Try another network, a custom domain, or a PHP relay.")
		}
		report.add("http", "error", code, detail)
		return report.finish(false, host)
	}
	defer resp.Body.Close()
	report.add("http", "info", "", fmt.Sprintf("Relay answered HTTP %d (%s).", resp.StatusCode, http.StatusText(resp.StatusCode)))
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8193))
	if err != nil {
		code, detail := relayTestError(err)
		report.add("health", "error", code, detail)
		return report.finish(false, host)
	}
	if resp.StatusCode != http.StatusOK {
		code, detail := relayHealthHTTPError(resp, body)
		report.add("health", "error", code, detail)
		return report.finish(false, host)
	}
	var health struct {
		Relay   string `json:"relay"`
		Version int    `json:"version"`
		Ready   bool   `json:"ready"`
	}
	if len(body) > 8192 || json.Unmarshal(body, &health) != nil || health.Relay != "arman-vpn" || health.Version != 1 {
		report.add("health", "warning", "unsupported_health", "HTTP 200 was received, but the relay health response was not recognized. An older relay script or another page may be installed; update the relay script.")
		return report.finish(false, host)
	}
	if !health.Ready {
		report.add("health", "error", "not_ready", "The relay answered but reported that it is not ready.")
		return report.finish(false, host)
	}
	report.add("health", "ok", "ready", "The relay is reachable and its local health check is ready.")
	return report.finish(true, host)
}

// Return only known error categories. Raw URL errors, response bodies, and
// server-supplied text can contain the relay secret and must not be reported.
func relayTestError(err error) (string, string) {
	var u *url.Error
	if errors.As(err, &u) {
		err = u.Err
	}
	var timeout net.Error
	var dns *net.DNSError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var ech *tls.ECHRejectionError
	var record tls.RecordHeaderError
	var operatingSystem *os.SyscallError
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled", "The relay test was cancelled."
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		return "timeout", "The current step timed out before a response was received."
	case errors.As(err, &dns):
		return "dns", "DNS could not resolve the relay hostname."
	case errors.As(err, &authority):
		return "certificate", "The TLS certificate issuer is not trusted."
	case errors.As(err, &hostname):
		return "certificate", "The TLS certificate does not match the relay hostname."
	case errors.As(err, &invalid):
		return "certificate", "The TLS certificate is invalid or outside its validity period."
	case errors.As(err, &ech):
		return "ech_rejected", "The server did not accept the ECH configuration."
	case errors.As(err, &record):
		return "tls_record", "The endpoint did not answer with a valid TLS record."
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused", "The connection was refused."
	case errors.Is(err, syscall.ECONNRESET):
		return "reset", "The connection was reset before the current step completed."
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "eof", "The connection was closed before the current step completed (EOF)."
	case errors.Is(err, resolver.ErrNoECH):
		return "no_ech", "No ECH configuration was found for the relay hostname."
	default:
		if errors.As(err, &operatingSystem) {
			if code, ok := operatingSystem.Err.(syscall.Errno); ok {
				return "os_error", fmt.Sprintf("The operating system reported: %s (code %d).", code.Error(), uint64(code))
			}
		}
		return "transport", "The connection failed during this step."
	}
}

var relayCloudflareCode = regexp.MustCompile(`(?i)(?:error\s*(?:code)?\s*:?\s*)(1[0-9]{3})\b`)

func relayHealthHTTPError(resp *http.Response, body []byte) (string, string) {
	switch resp.Header.Get("X-Relay-Error") {
	case "wrong secret":
		return "wrong_secret", "The relay rejected its secret. Check the address and the secret configured on the relay."
	case "no secret is set in the relay":
		return "not_configured", "No secret is configured on the relay. Complete its deployment first."
	case "PHP curl extension is missing":
		return "php_curl_missing", "The PHP curl extension is missing on the relay host. Enable it before using this relay."
	}
	detail := "The relay health request was not accepted."
	switch resp.StatusCode {
	case 403:
		detail = "Access was denied. Check the relay secret, deployment, and host access rules."
	case 429:
		detail = "The relay or its hosting service is rate limiting requests."
	case 301, 302, 303, 307, 308:
		detail = "The address redirects to another page. Redirects are not followed by the relay test."
	case 404:
		detail = "The relay address or health endpoint was not found."
	case 500, 502, 503, 504:
		detail = "The relay or its hosting service returned a server error. Check its deployment and request limits."
	}
	if match := relayCloudflareCode.FindSubmatch(body); len(match) > 1 {
		detail += " Hosting error code: " + string(match[1]) + "."
	}
	return fmt.Sprintf("http_%d", resp.StatusCode), detail
}

func (s *dashboardServer) handleRelayTest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL    *string `json:"url"`
		Stream bool    `json:"stream"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	network := s.d.config().Network
	raw := network.APIRelay
	if in.URL != nil {
		raw = *in.URL
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var emit func(relayTestStep)
	encoder := json.NewEncoder(w)
	if in.Stream {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-store")
		emit = func(step relayTestStep) {
			if encoder.Encode(map[string]any{"type": "step", "step": step}) != nil {
				cancel()
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}
	result := newRelayHealthTest(network).run(ctx, raw, emit)
	if in.Stream {
		encoder.Encode(map[string]any{"type": "result", "result": result})
		return
	}
	writeJSON(w, http.StatusOK, result)
}
