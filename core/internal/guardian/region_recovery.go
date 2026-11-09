package guardian

import (
	"net/http"
	"sync"

	"github.com/armanlife/arman-vpn-client/core/internal/controlplane"
)

var regionRecovery struct {
	sync.RWMutex
	retry func(*http.Request) (*http.Response, error)
}

// SetRegionRecovery supplies optional recovery only after Guardian HTTP 451.
// It does not replace the shared control-plane transport or its TLS profile.
// A nil response or a failed retry leaves the original 451 response readable.
func SetRegionRecovery(retry func(*http.Request) (*http.Response, error)) {
	regionRecovery.Lock()
	regionRecovery.retry = retry
	regionRecovery.Unlock()
}

func do(req *http.Request) (*http.Response, error) {
	resp, err := controlplane.Do(req)
	if err != nil || resp.StatusCode != http.StatusUnavailableForLegalReasons {
		return resp, err
	}
	regionRecovery.RLock()
	retry := regionRecovery.retry
	regionRecovery.RUnlock()
	if retry != nil && req.Context().Err() == nil {
		if recovered, retryErr := retry(req); retryErr == nil && recovered != nil {
			resp.Body.Close()
			return recovered, nil
		}
	}
	return resp, nil
}
