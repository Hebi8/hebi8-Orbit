package discovery

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProbeCandidatesDeduplicatesAndPrioritizesRemote(t *testing.T) {
	remoteHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":          true,
				"service":     "hebi8-orbit-hub",
				"version":     "0.3.2",
				"discoveryId": "orb-remote1",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer remoteHub.Close()

	selfHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":          true,
				"service":     "hebi8-orbit-hub",
				"version":     "0.3.2",
				"discoveryId": "orb-self99",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer selfHub.Close()

	nonOrbit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "service": "other-service"})
	}))
	defer nonOrbit.Close()

	cands := []probeCandidate{
		{hostName: "local", via: "local", baseURL: selfHub.URL},
		{hostName: "self-ts", dnsName: "self.example.ts.net", via: "tailscale", baseURL: selfHub.URL},
		{hostName: "devbox", dnsName: "devbox.example.ts.net", via: "tailscale", baseURL: remoteHub.URL},
		{hostName: "other", dnsName: "other.example.ts.net", via: "tailscale", baseURL: nonOrbit.URL},
	}

	got := probeCandidates(context.Background(), cands, 2*time.Second, "orb-self99", "")
	if len(got) != 2 {
		t.Fatalf("expected 2 discovered hubs (remote + deduped self), got %d: %+v", len(got), got)
	}
	if got[0].DiscoveryID != "orb-remote1" || got[0].IsSelf {
		t.Fatalf("expected remote hub first, got %+v", got[0])
	}
	if got[1].DiscoveryID != "orb-self99" || !got[1].IsSelf || got[1].Via != "tailscale" {
		t.Fatalf("expected deduped self hub via tailscale second, got %+v", got[1])
	}
}
