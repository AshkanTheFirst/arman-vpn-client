//go:build live

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/armanlife/arman-vpn-client/core/internal/config"
)

// This opt-in smoke test sends only local worker health requests. It never
// reads account files or sends a Guardian request, even to a healthy relay.
func TestLiveRelayWorkerHealth(t *testing.T) {
	path := os.Getenv("ARMAN_RELAY_HEALTH_MANIFEST")
	if path == "" {
		t.Skip("set ARMAN_RELAY_HEALTH_MANIFEST to an owner-supplied test manifest")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("test manifest cannot be read")
	}
	var manifest struct {
		Relays struct {
			URLs []string `json:"urls"`
		} `json:"relays"`
	}
	if json.Unmarshal(data, &manifest) != nil || len(manifest.Relays.URLs) == 0 {
		t.Fatal("test manifest has no relays")
	}
	pool := newAutomaticRelays(config.Network{APIRelayECH: config.RelayECHOn})
	for i, raw := range manifest.Relays.URLs {
		t.Run(fmt.Sprintf("relay-%d", i+1), func(t *testing.T) {
			u, err := url.Parse(raw)
			if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
				t.Fatal("invalid test relay")
			}
			transport, err := pool.workerTransport(u)
			if err != nil {
				t.Fatal("worker transport cannot be created")
			}
			if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
				t.Cleanup(closer.CloseIdleConnections)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if !pool.healthy(ctx, u, transport) {
				t.Errorf("worker health failed host=%s", u.Hostname())
			}
		})
	}
}
