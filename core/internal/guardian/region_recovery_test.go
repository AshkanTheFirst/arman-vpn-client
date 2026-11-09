package guardian

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/armanlife/arman-vpn-client/core/internal/controlplane"
)

type regionRoundTrip func(*http.Request) (*http.Response, error)

func (f regionRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRegionRecoveryOnlyAfter451AndKeepsOriginalFailure(t *testing.T) {
	previous := controlplane.Transport()
	t.Cleanup(func() { controlplane.SetTransport(previous); SetRegionRecovery(nil) })
	var status, called int
	controlplane.SetTransport(regionRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"unavailable_in_region"}`)), Request: r}, nil
	}))
	SetRegionRecovery(func(*http.Request) (*http.Response, error) { called++; return nil, nil })
	for _, code := range []int{200, 401, 403, 406, 500, 451} {
		status = code
		req, _ := http.NewRequest(http.MethodGet, "https://vpn.mozilla.org/api/v1/fpn/token", nil)
		resp, err := do(req)
		if err != nil || resp.StatusCode != code {
			t.Fatalf("response %d: %v", code, err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || len(body) == 0 {
			t.Fatal("original response lost")
		}
	}
	if called != 1 {
		t.Fatalf("recovery called %d times", called)
	}
}

func TestRegionRecoveryReplaces451WithReadableSuccessfulResponse(t *testing.T) {
	previous := controlplane.Transport()
	t.Cleanup(func() { controlplane.SetTransport(previous); SetRegionRecovery(nil) })
	controlplane.SetTransport(regionRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 451, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"unavailable_in_region"}`)), Request: r}, nil
	}))
	SetRegionRecovery(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"subscribed":true}`)), Request: r}, nil
	})
	info, err := FetchUserInfo(t.Context(), DefaultEndpoint, "fixture")
	if err != nil || !info.Subscribed {
		t.Fatalf("successful recovery was lost: %v", err)
	}
}
