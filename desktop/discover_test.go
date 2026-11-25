package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProbeDesktopCandidates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":          true,
				"service":     "hebi8-orbit-hub",
				"version":     "0.3.2",
				"discoveryId": "orb-test12",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	got := probeDesktopCandidates(context.Background(), []desktopProbeCandidate{
		{hostName: "hub-a", dnsName: "hub-a.example.ts.net", via: "tailscale", baseURL: srv.URL},
	}, 2*time.Second)
	if len(got) != 1 || got[0].DiscoveryID != "orb-test12" {
		t.Fatalf("unexpected desktop discovery result: %+v", got)
	}
}
