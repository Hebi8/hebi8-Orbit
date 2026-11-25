package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// DiscoveredHub describes one reachable Orbit Hub detected on Tailscale or loopback.
type DiscoveredHub struct {
	Origin      string `json:"origin"`
	HostName    string `json:"hostName"`
	DNSName     string `json:"dnsName,omitempty"`
	DiscoveryID string `json:"discoveryId,omitempty"`
	Version     string `json:"version,omitempty"`
	Via         string `json:"via"`
	IsSelf      bool   `json:"isSelf,omitempty"`
	LatencyMs   int64  `json:"latencyMs"`
}

// DiscoverResult is returned by the desktop setup server's /api/discover endpoint.
type DiscoverResult struct {
	TailscaleAvailable bool            `json:"tailscaleAvailable"`
	TailscaleOnline    bool            `json:"tailscaleOnline"`
	ScannedPeers       int             `json:"scannedPeers"`
	DurationMs         int64           `json:"durationMs"`
	Hubs               []DiscoveredHub `json:"hubs"`
}

type desktopProbeCandidate struct {
	hostName string
	dnsName  string
	via      string
	isSelf   bool
	ports    []int
	baseURL  string
}

func findDesktopTailscaleBin() string {
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p
	}
	cands := []string{"/usr/local/bin/tailscale"}
	if runtime.GOOS == "darwin" {
		cands = []string{
			"/opt/homebrew/bin/tailscale",
			"/usr/local/bin/tailscale",
			"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
		}
	}
	for _, c := range cands {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return c
		}
	}
	return ""
}

// DiscoverNetworks scans online Tailscale peers (and optionally local loopback ports)
// for reachable Orbit Hub instances within totalTimeout.
func DiscoverNetworks(ctx context.Context, includeLocal bool, totalTimeout time.Duration) DiscoverResult {
	start := time.Now()
	if totalTimeout <= 0 {
		totalTimeout = 4000 * time.Millisecond
	}
	perProbe := 1400 * time.Millisecond
	if perProbe > totalTimeout {
		perProbe = totalTimeout
	}
	scanCtx, cancel := context.WithTimeout(ctx, totalTimeout)
	defer cancel()

	tsBin := findDesktopTailscaleBin()
	res := DiscoverResult{
		TailscaleAvailable: tsBin != "",
		Hubs:               []DiscoveredHub{},
	}

	var cands []desktopProbeCandidate
	if tsBin != "" {
		online, peers := queryDesktopTailscalePeers(scanCtx, tsBin)
		res.TailscaleOnline = online
		for _, p := range peers {
			if !p.isSelf {
				res.ScannedPeers++
			}
			cands = append(cands, p)
		}
	}

	if includeLocal {
		for port := 4310; port <= 4315; port++ {
			cands = append(cands, desktopProbeCandidate{
				hostName: "127.0.0.1",
				via:      "local",
				isSelf:   true,
				baseURL:  fmt.Sprintf("http://127.0.0.1:%d", port),
			})
		}
	}

	res.Hubs = probeDesktopCandidates(scanCtx, cands, perProbe)
	res.DurationMs = time.Since(start).Milliseconds()
	return res
}

func queryDesktopTailscalePeers(ctx context.Context, tsBin string) (bool, []desktopProbeCandidate) {
	pctx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	raw, err := exec.CommandContext(pctx, tsBin, "status", "--json").Output()
	if err != nil {
		return false, nil
	}
	var doc struct {
		BackendState string `json:"BackendState"`
		Self         struct {
			HostName string `json:"HostName"`
			DNSName  string `json:"DNSName"`
		} `json:"Self"`
		Peer map[string]struct {
			HostName string `json:"HostName"`
			DNSName  string `json:"DNSName"`
			Online   bool   `json:"Online"`
		} `json:"Peer"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.BackendState != "Running" {
		return false, nil
	}

	var out []desktopProbeCandidate
	if selfDNS := strings.TrimRight(strings.TrimSpace(doc.Self.DNSName), "."); selfDNS != "" {
		host := strings.TrimSpace(doc.Self.HostName)
		if host == "" {
			host = strings.Split(selfDNS, ".")[0]
		}
		out = append(out, desktopProbeCandidate{
			hostName: host,
			dnsName:  selfDNS,
			via:      "tailscale",
			isSelf:   true,
			ports:    []int{443, 4443, 8443, 10000},
		})
	}
	for _, p := range doc.Peer {
		dns := strings.TrimRight(strings.TrimSpace(p.DNSName), ".")
		if dns == "" || !p.Online {
			continue
		}
		host := strings.TrimSpace(p.HostName)
		if host == "" || strings.EqualFold(host, "localhost") {
			host = strings.Split(dns, ".")[0]
		}
		out = append(out, desktopProbeCandidate{
			hostName: host,
			dnsName:  dns,
			via:      "tailscale",
			ports:    []int{443, 4443, 8443, 10000},
		})
	}
	return true, out
}

func probeDesktopCandidates(ctx context.Context, cands []desktopProbeCandidate, perProbe time.Duration) []DiscoveredHub {
	if len(cands) == 0 {
		return []DiscoveredHub{}
	}
	client := &http.Client{
		Timeout: perProbe,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	sem := make(chan struct{}, 12)
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		results []DiscoveredHub
	)
	for _, cand := range cands {
		cand := cand
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			var found []DiscoveredHub
			if cand.baseURL != "" {
				t := perProbe
				if cand.via == "local" && t > 300*time.Millisecond {
					t = 300 * time.Millisecond
				}
				if h, ok := probeDesktopOrigin(ctx, client, cand.baseURL, cand.hostName, cand.dnsName, cand.via, cand.isSelf, t); ok {
					found = append(found, h)
				}
			} else if cand.dnsName != "" {
				primary := "https://" + cand.dnsName
				if h, ok := probeDesktopOrigin(ctx, client, primary, cand.hostName, cand.dnsName, cand.via, cand.isSelf, perProbe); ok {
					found = append(found, h)
				} else if len(cand.ports) > 1 {
					var altWG sync.WaitGroup
					var altMu sync.Mutex
					for _, port := range cand.ports[1:] {
						port := port
						altWG.Add(1)
						go func() {
							defer altWG.Done()
							u := fmt.Sprintf("https://%s:%d", cand.dnsName, port)
							if h, ok := probeDesktopOrigin(ctx, client, u, cand.hostName, cand.dnsName, cand.via, cand.isSelf, perProbe); ok {
								altMu.Lock()
								found = append(found, h)
								altMu.Unlock()
							}
						}()
					}
					altWG.Wait()
				}
			}
			if len(found) > 0 {
				mu.Lock()
				results = append(results, found...)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	byKey := map[string]DiscoveredHub{}
	for _, h := range results {
		key := strings.ToLower(strings.TrimRight(h.Origin, "/"))
		if h.DiscoveryID != "" {
			key = "id:" + strings.ToLower(h.DiscoveryID)
		}
		prev, ok := byKey[key]
		if !ok {
			byKey[key] = h
			continue
		}
		if prev.Via == "local" && h.Via == "tailscale" {
			h.IsSelf = h.IsSelf || prev.IsSelf
			byKey[key] = h
		} else {
			prev.IsSelf = prev.IsSelf || h.IsSelf
			byKey[key] = prev
		}
	}
	out := make([]DiscoveredHub, 0, len(byKey))
	for _, h := range byKey {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsSelf != out[j].IsSelf {
			return !out[i].IsSelf
		}
		if out[i].Via != out[j].Via {
			return out[i].Via == "tailscale"
		}
		if out[i].LatencyMs != out[j].LatencyMs {
			return out[i].LatencyMs < out[j].LatencyMs
		}
		return out[i].Origin < out[j].Origin
	})
	return out
}

func probeDesktopOrigin(
	ctx context.Context,
	client *http.Client,
	origin, hostName, dnsName, via string,
	isSelf bool,
	timeout time.Duration,
) (DiscoveredHub, bool) {
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	base := strings.TrimRight(origin, "/")
	start := time.Now()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, base+"/api/health", nil)
	if err != nil {
		return DiscoveredHub{}, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return DiscoveredHub{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return DiscoveredHub{}, false
	}
	var doc struct {
		OK          bool   `json:"ok"`
		Service     string `json:"service"`
		Version     string `json:"version"`
		DiscoveryID string `json:"discoveryId"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<14)).Decode(&doc) != nil || !doc.OK {
		return DiscoveredHub{}, false
	}
	if doc.Service != "hebi8-orbit-hub" {
		if strings.TrimSpace(doc.Version) == "" {
			return DiscoveredHub{}, false
		}
		bReq, err := http.NewRequestWithContext(pctx, http.MethodGet, base+"/api/bootstrap", nil)
		if err != nil {
			return DiscoveredHub{}, false
		}
		bResp, err := client.Do(bReq)
		if err != nil {
			return DiscoveredHub{}, false
		}
		defer bResp.Body.Close()
		var bDoc map[string]any
		if json.NewDecoder(io.LimitReader(bResp.Body, 1<<16)).Decode(&bDoc) != nil {
			return DiscoveredHub{}, false
		}
		if bResp.StatusCode == http.StatusUnauthorized {
			if code, _ := bDoc["code"].(string); code != "ERR_UNAUTHORIZED" {
				return DiscoveredHub{}, false
			}
		} else if bResp.StatusCode == http.StatusOK {
			if _, ok := bDoc["machines"]; !ok {
				return DiscoveredHub{}, false
			}
		} else {
			return DiscoveredHub{}, false
		}
	}
	return DiscoveredHub{
		Origin:      base,
		HostName:    hostName,
		DNSName:     dnsName,
		DiscoveryID: strings.TrimSpace(doc.DiscoveryID),
		Version:     strings.TrimSpace(doc.Version),
		Via:         via,
		IsSelf:      isSelf,
		LatencyMs:   time.Since(start).Milliseconds(),
	}, true
}
