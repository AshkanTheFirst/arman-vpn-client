package service

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/armanlife/arman-vpn-client/core/internal/config"
	"github.com/armanlife/arman-vpn-client/core/internal/controlplane"
	"github.com/armanlife/arman-vpn-client/core/internal/guardian"
	"github.com/armanlife/arman-vpn-client/core/internal/logx"
	"github.com/armanlife/arman-vpn-client/core/internal/update"
)

const (
	relayManifestURL    = "https://raw.githubusercontent.com/arman-org/update-manifests/main/arman-vpn-client/updates.json"
	relayManifestMirror = "https://cdn.jsdelivr.net/gh/arman-org/update-manifests@main/arman-vpn-client/updates.json"
	autoRelayBudget     = 20 * time.Second
	relayProbeTimeout   = 4 * time.Second
	maxRelayManifest    = 512 * 1024
)

// automaticRelays is lazy: no metadata, worker probe or account request is
// sent until a production Guardian request has already returned HTTP 451.
// Candidate URLs and health state are in memory, never in user settings.
type automaticRelays struct {
	mu           sync.Mutex
	network      config.Network
	manifestURLs []string
	client       *http.Client
	transport    func(*url.URL) (http.RoundTripper, error)
	shuffle      func(int, func(int, int))
	urls         []string
	refreshAt    time.Time
	active       string
	activeUntil  time.Time
	networkKey   string
	badUntil     map[string]time.Time
}

func newAutomaticRelays(n config.Network) *automaticRelays {
	a := &automaticRelays{
		network:      n,
		manifestURLs: []string{relayManifestURL, relayManifestMirror},
		client: &http.Client{
			Transport:     controlplane.QuickTransport(directWait),
			Timeout:       relayProbeTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		shuffle:  rand.Shuffle,
		badUntil: make(map[string]time.Time),
	}
	// The app and desktop updater already use this local release server in
	// live tests. Read its manifest too, without publishing test relay URLs.
	if server := strings.TrimRight(os.Getenv(update.ServerEnv), "/"); server != "" {
		a.manifestURLs = []string{server + "/updates.json"}
	}
	a.transport = a.workerTransport
	return a
}

func (a *automaticRelays) workerTransport(relay *url.URL) (http.RoundTripper, error) {
	dialer := resolverDialer{timeout: relayProbeTimeout}
	base := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   relayProbeTimeout,
		ResponseHeaderTimeout: 8 * time.Second,
	}
	if a.network.APIRelayECH != config.RelayECHOff {
		d, err := newECHDialer(relay.Hostname(), a.network, dialer.DialContext)
		if err != nil {
			return nil, err
		}
		base.DialTLSContext = boundedRelayTLS(d, relayProbeTimeout/2)
	}
	return newRelayTLSFallback(base, a.network, relay.Hostname(), dialer.DialContext, nil), nil
}

// Keep time for the normal TLS fallback when an optional ECH key lookup or
// handshake stalls. A required-ECH relay must still fail closed.
func boundedRelayTLS(d *echDialer, wait time.Duration) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		attempt, cancel := context.WithTimeout(ctx, wait)
		conn, err := d.DialTLSContext(attempt, network, address)
		cancel()
		if err == nil || d.require || ctx.Err() != nil {
			return conn, err
		}
		return d.withoutECH(ctx, network, address, err)
	}
}

// load accepts a valid empty/disabled primary as authoritative. It tries
// the mirror only for an invalid/unreachable file and bounds both input
// size and candidate count. No URL with a relay secret is logged.
func (a *automaticRelays) load(ctx context.Context) {
	if time.Now().Before(a.refreshAt) {
		return
	}
	a.refreshAt = time.Now().Add(time.Minute)
	for _, source := range a.manifestURLs {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Accept", "application/json")
		resp, err := a.client.Do(req)
		if err != nil {
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxRelayManifest+1))
		resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusOK || len(body) > maxRelayManifest {
			continue
		}
		var manifest struct {
			Relays *struct {
				Enabled bool     `json:"enabled"`
				URLs    []string `json:"urls"`
			} `json:"relays"`
		}
		var root map[string]json.RawMessage
		if json.Unmarshal(body, &root) != nil || root == nil || json.Unmarshal(body, &manifest) != nil {
			continue
		}
		var candidates []string
		seen := map[string]bool{}
		if manifest.Relays != nil && manifest.Relays.Enabled {
			for _, raw := range manifest.Relays.URLs {
				u, err := url.Parse(strings.TrimSpace(raw))
				if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
					continue
				}
				q := u.Query()
				if q.Has("url") || q.Has("health") {
					continue
				}
				raw = u.String()
				if !seen[raw] {
					candidates = append(candidates, raw)
					seen[raw] = true
				}
				if len(candidates) >= 100 {
					break
				}
			}
		}
		a.urls = candidates
		for raw := range a.badUntil {
			if !seen[raw] {
				delete(a.badUntil, raw)
			}
		}
		a.refreshAt = time.Now().Add(10 * time.Minute)
		if !seen[a.active] {
			a.active = ""
			a.activeUntil = time.Time{}
		}
		logx.Info("automatic Guardian relay list loaded candidates=%d", len(candidates))
		return
	}
	// A failed refresh does not invent another metadata source or erase a
	// previously fetched list. The next 451 retries the refresh after a minute.
}

func (a *automaticRelays) healthy(ctx context.Context, relay *url.URL, base http.RoundTripper) bool {
	u := *relay
	query := u.Query()
	query.Set("health", "1")
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cache-Control", "no-cache")
	client := &http.Client{
		Transport: base, Timeout: 2 * relayProbeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		why := "transport"
		if errors.Is(err, context.DeadlineExceeded) {
			why = "timeout"
		}
		if errors.Is(err, context.Canceled) {
			why = "cancelled"
		}
		logx.Info("automatic relay health failed host=%s reason=%s", relay.Hostname(), why)
		return false
	}
	defer resp.Body.Close()
	var report struct {
		Relay   string `json:"relay"`
		Version int    `json:"version"`
		Ready   bool   `json:"ready"`
	}
	valid := resp.StatusCode == http.StatusOK &&
		json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&report) == nil &&
		report.Relay == "arman-vpn" && report.Version == 1 && report.Ready
	logx.Info("automatic relay health host=%s http=%d ready=%t", relay.Hostname(), resp.StatusCode, valid)
	return valid
}

func (a *automaticRelays) recover(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme+"://"+req.URL.Host != guardian.DefaultEndpoint ||
		!strings.HasPrefix(req.URL.Path, "/api/v1/fpn/") ||
		(req.Body != nil && req.GetBody == nil) {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(req.Context(), autoRelayBudget)
	handedOff := false
	defer func() {
		if !handedOff {
			cancel()
		}
	}()
	// Serializes selection/probes so concurrent quota and connection requests
	// reuse the successful relay instead of each scanning the full pool.
	a.mu.Lock()
	defer a.mu.Unlock()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	a.load(ctx)
	network, _ := networkNow()
	if network != a.networkKey {
		a.active = ""
		clear(a.badUntil)
		a.networkKey = network
	}
	order := append([]string(nil), a.urls...)
	a.shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	if a.active != "" && time.Now().Before(a.activeUntil) {
		order = append([]string{a.active}, order...)
	}
	seen := map[string]bool{}
	for _, raw := range order {
		if ctx.Err() != nil {
			break
		}
		if seen[raw] || time.Now().Before(a.badUntil[raw]) {
			continue
		}
		seen[raw] = true
		u, _ := url.Parse(raw)
		base, err := a.transport(u)
		if err != nil {
			continue
		}
		closeIdle, _ := base.(interface{ CloseIdleConnections() })
		if closeIdle != nil {
			defer closeIdle.CloseIdleConnections()
		}
		if !a.healthy(ctx, u, base) {
			a.badUntil[raw] = time.Now().Add(time.Minute)
			continue
		}
		out := req.Clone(ctx)
		if req.Body != nil {
			out.Body, err = req.GetBody()
			if err != nil {
				return nil, err
			}
		}
		resp, err := (&relayTransport{relay: u, base: base}).RoundTrip(out)
		if resp != nil {
			logx.Info("automatic relay Guardian response host=%s http=%d forwarded=%t", u.Hostname(), resp.StatusCode, resp.Header.Get("X-Arman-Relay") == "1")
		} else if err != nil {
			logx.Info("automatic relay Guardian request failed host=%s", u.Hostname())
		}
		if err == nil && usableGuardianRelayResponse(req, resp) {
			// RoundTrip's body still uses ctx. Keep its deadline alive until
			// the caller closes it rather than cancelling on this return.
			resp.Body = &automaticRelayBody{ReadCloser: resp.Body, cancel: cancel}
			handedOff = true
			a.active, a.activeUntil = raw, time.Now().Add(relayFirstFor)
			resp.Request = req
			logx.Info("Guardian HTTP 451 recovered through a healthy automatic relay")
			return resp, nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		a.badUntil[raw] = time.Now().Add(time.Minute)
		if req.Method != http.MethodGet && req.Method != http.MethodHead &&
			(err != nil || resp == nil || resp.StatusCode != http.StatusUnavailableForLegalReasons) {
			break
		}
	}
	return nil, errors.New("no healthy Guardian relay is available")
}

func usableGuardianRelayResponse(req *http.Request, resp *http.Response) bool {
	if resp == nil || resp.Header.Get("X-Arman-Relay") != "1" {
		return false
	}
	if req.Method == http.MethodHead && resp.StatusCode == http.StatusOK {
		_, err := guardian.ParseProxyUsage(resp.Header)
		return err == nil
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		return strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "application/json")
	}
	return false
}

type automaticRelayBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *automaticRelayBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}
