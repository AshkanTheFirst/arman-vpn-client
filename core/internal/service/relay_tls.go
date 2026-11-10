package service

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/armanlife/arman-vpn-client/core/internal/config"
	"github.com/armanlife/arman-vpn-client/core/internal/logx"
)

const relayTLSAttemptWait = 3 * time.Second

const relayEdgeAddress = "chatgpt.com"

// Read-only relay requests recover through TLS 1.2, then (for workers.dev)
// another DNS-selected Cloudflare connection. ECH require remains fail-closed.
type relayTLSFallback struct {
	primary, tls12 http.RoundTripper
	edge           http.RoundTripper
	host           string
	wait           time.Duration
	observe        func(string, time.Time, error)
	network        func() string
	mu             sync.Mutex
	until          time.Time
	networkKey     string
	preferred      string
}

func newRelayTLSFallback(primary *http.Transport, n config.Network, host string, dial func(context.Context, string, string) (net.Conn, error), observe func(string, time.Time, error)) http.RoundTripper {
	if n.APIRelayECH == config.RelayECHRequire {
		return primary
	}
	fallback := primary.Clone()
	fallback.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		raw, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		cfg := primary.TLSClientConfig.Clone()
		if cfg == nil {
			cfg = &tls.Config{}
		}
		cfg.ServerName = host
		cfg.MinVersion, cfg.MaxVersion = tls.VersionTLS12, tls.VersionTLS12
		cfg.EncryptedClientHelloConfigList = nil
		cfg.NextProtos = []string{"http/1.1"}
		conn := tls.Client(raw, cfg)
		started := time.Now()
		err = conn.HandshakeContext(ctx)
		if observe != nil {
			observe("tls12", started, err)
		}
		if err != nil {
			raw.Close()
			return nil, err
		}
		return conn, nil
	}
	route := &relayTLSFallback{primary: primary, tls12: fallback, host: host, wait: relayTLSAttemptWait, observe: observe,
		network: func() string { key, _ := networkNow(); return key },
	}
	if relayEdgeEligible(n, host) {
		base := primary.Clone()
		base.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			raw, err := dial(ctx, network, address)
			if err != nil {
				return nil, err
			}
			cfg := primary.TLSClientConfig.Clone()
			if cfg == nil {
				cfg = &tls.Config{}
			}
			cfg.ServerName = host
			cfg.EncryptedClientHelloConfigList = nil
			cfg.NextProtos = []string{"http/1.1"}
			conn := tls.Client(raw, cfg)
			started := time.Now()
			err = conn.HandshakeContext(ctx)
			if observe != nil {
				observe("tls_edge", started, err)
			}
			if err != nil {
				raw.Close()
				return nil, err
			}
			return conn, nil
		}
		route.edge = &relayEdgeTransport{base: base}
	}
	return route
}

func relayEdgeEligible(n config.Network, host string) bool {
	return n.APIRelayECH != config.RelayECHRequire && strings.HasSuffix(strings.ToLower(host), ".workers.dev")
}

// Change the connection URL while retaining the relay's HTTP host, TLS name,
// path and query. Responses remain associated with the original relay URL.
type relayEdgeTransport struct{ base *http.Transport }

func (t *relayEdgeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	u := *req.URL
	u.Host = relayEdgeAddress
	out.URL, out.Host = &u, req.URL.Host
	resp, err := t.base.RoundTrip(out)
	if resp != nil {
		resp.Request = req
	}
	return resp, err
}

func (t *relayEdgeTransport) CloseIdleConnections() { t.base.CloseIdleConnections() }

func (t *relayTLSFallback) preferredMode() string {
	key := t.network()
	t.mu.Lock()
	defer t.mu.Unlock()
	if key == t.networkKey && time.Now().Before(t.until) {
		return t.preferred
	}
	return ""
}

func (t *relayTLSFallback) remember(mode string) {
	key := t.network()
	t.mu.Lock()
	t.preferred, t.networkKey, t.until = mode, key, time.Now().Add(relayFirstFor)
	t.mu.Unlock()
}

func (t *relayTLSFallback) RoundTrip(req *http.Request) (*http.Response, error) {
	type attempt struct {
		mode      string
		transport http.RoundTripper
	}
	attempts := []attempt{{"", t.primary}, {"tls12", t.tls12}}
	if t.edge != nil {
		attempts = append(attempts, attempt{"edge", t.edge})
	}
	preferred := t.preferredMode()
	for i := range attempts {
		if attempts[i].mode == preferred && i > 0 {
			selected := attempts[i]
			copy(attempts[1:i+1], attempts[:i])
			attempts[0] = selected
			break
		}
	}
	// A timeout after writing a POST does not say whether the origin handled it.
	// Only bodyless GET/HEAD requests can be retried here.
	if (req.Method != http.MethodGet && req.Method != http.MethodHead) || (req.Body != nil && req.Body != http.NoBody) {
		return attempts[0].transport.RoundTrip(req)
	}
	var lastErr error
	for i, attempt := range attempts {
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		if t.observe != nil && attempt.mode != "" {
			stage := attempt.mode + "_retry"
			if i == 0 {
				stage = attempt.mode + "_selected"
			}
			t.observe(stage, time.Now(), nil)
		}
		resp, err, expired := t.try(req, attempt.transport, len(attempts)-i)
		if err == nil {
			if preferred == "" || preferred != attempt.mode {
				t.remember(attempt.mode)
			}
			if i > 0 || preferred != "" && preferred != attempt.mode {
				logx.Info("API relay %s reached connection_route=%s", t.host, map[string]string{"": "normal", "tls12": "tls12", "edge": "chatgpt.com"}[attempt.mode])
			}
			return resp, nil
		}
		lastErr = err
		if req.Context().Err() != nil || (!expired && !relayTLSRetryable(err)) {
			break
		}
	}
	return nil, lastErr
}

func (t *relayTLSFallback) try(req *http.Request, transport http.RoundTripper, remaining int) (*http.Response, error, bool) {
	if remaining == 1 {
		resp, err := transport.RoundTrip(req)
		return resp, err, false
	}
	wait := t.wait
	if deadline, ok := req.Context().Deadline(); ok {
		wait = min(wait, time.Until(deadline)/time.Duration(remaining))
	}
	ctx, cancel := context.WithCancel(req.Context())
	var expired atomic.Bool
	timer := time.AfterFunc(wait, func() { expired.Store(true); cancel() })
	resp, err := transport.RoundTrip(req.Clone(ctx))
	timer.Stop()
	if err == nil {
		// Stop the header budget without cutting off a successful response body.
		resp.Body = &automaticRelayBody{ReadCloser: resp.Body, cancel: cancel}
		return resp, nil, false
	}
	cancel()
	if resp != nil {
		resp.Body.Close()
	}
	return nil, err, expired.Load()
}

func relayTLSRetryable(err error) bool {
	var timeout net.Error
	var dns *net.DNSError
	return errors.As(err, &dns) || errors.As(err, &timeout) && timeout.Timeout() || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH)
}

func (t *relayTLSFallback) CloseIdleConnections() {
	for _, transport := range []http.RoundTripper{t.primary, t.tls12, t.edge} {
		if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
	}
}
