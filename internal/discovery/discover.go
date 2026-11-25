package discovery

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

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

// Options controls a manual Tailscale + local loopback Orbit network scan.
type Options struct {
	SelfDiscoveryID string
	SelfOrigin      string
	IncludeLocal    bool
	PerProbeTimeout time.Duration
	TotalTimeout    time.Duration
}

type probeCandidate struct {
	hostName string
	dnsName  string
	via      string
	isSelf   bool
	ports    []int // HTTPS ports for tailscale peers; 0 = direct base URL
	baseURL  string
}

type tailscalePeer struct {
	HostName string
	DNSName  string
	Online   bool
	IsSelf   bool
}

// FindTailscaleBin locates the Tailscale CLI even under minimal macOS GUI / launchd PATHs.
func FindTailscaleBin() string {
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

// Discover scans online Tailscale tailnet peers (and optionally local loopback ports)
// for reachable Orbit Hub instances, returning only privacy-safe metadata (origin, host, discoveryId).
func Discover(ctx context.Context, opts Options) protocol.NetworkDiscoverResponse {
	start := time.Now()
	perProbe := opts.PerProbeTimeout
	if perProbe <= 0 {
		perProbe = 1500 * time.Millisecond
	}
	totalTimeout := opts.TotalTimeout
	if totalTimeout <= 0 {
		totalTimeout = 4500 * time.Millisecond
	}
	scanCtx, cancel := context.WithTimeout(ctx, totalTimeout)
	defer cancel()

	tsBin := FindTailscaleBin()
	resp := protocol.NetworkDiscoverResponse{
		TailscaleAvailable: tsBin != "",
		Hubs:               []protocol.DiscoveredHubView{},
	}

	var candidates []probeCandidate
	if tsBin != "" {
		online, peers := queryTailscalePeers(scanCtx, tsBin)
		resp.TailscaleOnline = online
		for _, p := range peers {
			if !p.Online || p.DNSName == "" {
				continue
			}
			if !p.IsSelf {
				resp.ScannedPeers++
			}
			candidates = append(candidates, probeCandidate{
				hostName: p.HostName,
				dnsName:  p.DNSName,
				via:      "tailscale",
				isSelf:   p.IsSelf,
				ports:    []int{443, 4443, 8443, 10000},
			})
		}
	}

	if opts.IncludeLocal {
		for port := 4310; port <= 4315; port++ {
			candidates = append(candidates, probeCandidate{
				hostName: "127.0.0.1",
				via:      "local",
				baseURL:  fmt.Sprintf("http://127.0.0.1:%d", port),
			})
		}
	}

	resp.Hubs = probeCandidates(scanCtx, candidates, perProbe, opts.SelfDiscoveryID, opts.SelfOrigin)
	resp.DurationMs = time.Since(start).Milliseconds()
	return resp
}

func queryTailscalePeers(ctx context.Context, tsBin string) (bool, []tailscalePeer) {
	pctx, cancel := context.WithTimeout(ctx, 3500*time.Millisecond)
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
			Online   bool   `json:"Online"`
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

	var peers []tailscalePeer
	if selfDNS := strings.TrimRight(strings.TrimSpace(doc.Self.DNSName), "."); selfDNS != "" {
		host := strings.TrimSpace(doc.Self.HostName)
		if host == "" {
			host = strings.Split(selfDNS, ".")[0]
		}
		peers = append(peers, tailscalePeer{
			HostName: host,
			DNSName:  selfDNS,
			Online:   true,
			IsSelf:   true,
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
		peers = append(peers, tailscalePeer{
			HostName: host,
			DNSName:  dns,
			Online:   true,
		})
	}
	return true, peers
}

func probeCandidates(
	ctx context.Context,
	candidates []probeCandidate,
	perProbe time.Duration,
	selfDiscoveryID string,
	selfOrigin string,
) []protocol.DiscoveredHubView {
	if len(candidates) == 0 {
		return []protocol.DiscoveredHubView{}
	}

	selfOriginNorm := strings.TrimRight(strings.TrimSpace(selfOrigin), "/")
	selfDiscNorm := strings.TrimSpace(selfDiscoveryID)

	client := &http.Client{
		Timeout: perProbe,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	const maxWorkers = 12
	sem := make(chan struct{}, maxWorkers)
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		results []protocol.DiscoveredHubView
	)

	for _, cand := range candidates {
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

			var found []protocol.DiscoveredHubView
			if cand.baseURL != "" {
				pTimeout := perProbe
				if cand.via == "local" && pTimeout > 350*time.Millisecond {
					pTimeout = 350 * time.Millisecond
				}
				if h, ok := probeOrigin(ctx, client, cand.baseURL, cand.hostName, cand.dnsName, cand.via, cand.isSelf, pTimeout); ok {
					found = append(found, h)
				}
			} else if cand.dnsName != "" {
				// Probe primary :443 first; if not Orbit, probe standard alternate HTTPS ports in parallel.
				primary := "https://" + cand.dnsName
				if h, ok := probeOrigin(ctx, client, primary, cand.hostName, cand.dnsName, cand.via, cand.isSelf, perProbe); ok {
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
							if h, ok := probeOrigin(ctx, client, u, cand.hostName, cand.dnsName, cand.via, cand.isSelf, perProbe); ok {
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

	// Mark self & deduplicate by discoveryId (preferring Tailscale HTTPS over local loopback) or origin.
	for i := range results {
		if selfOriginNorm != "" && strings.EqualFold(strings.TrimRight(results[i].Origin, "/"), selfOriginNorm) {
			results[i].IsSelf = true
		}
		if selfDiscNorm != "" && results[i].DiscoveryID != "" && strings.EqualFold(results[i].DiscoveryID, selfDiscNorm) {
			results[i].IsSelf = true
		}
	}

	byKey := map[string]protocol.DiscoveredHubView{}
	for _, h := range results {
		key := strings.ToLower(strings.TrimRight(h.Origin, "/"))
		if h.DiscoveryID != "" {
			key = "id:" + strings.ToLower(h.DiscoveryID)
		}
		prev, exists := byKey[key]
		if !exists {
			byKey[key] = h
			continue
		}
		// Prefer Tailscale HTTPS over local loopback when the same Hub appears on both.
		if prev.Via == "local" && h.Via == "tailscale" {
			h.IsSelf = h.IsSelf || prev.IsSelf
			byKey[key] = h
		} else {
			prev.IsSelf = prev.IsSelf || h.IsSelf
			byKey[key] = prev
		}
	}

	out := make([]protocol.DiscoveredHubView, 0, len(byKey))
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

func probeOrigin(
	ctx context.Context,
	client *http.Client,
	origin, hostName, dnsName, via string,
	isSelf bool,
	timeout time.Duration,
) (protocol.DiscoveredHubView, bool) {
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	base := strings.TrimRight(origin, "/")
	start := time.Now()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, base+"/api/health", nil)
	if err != nil {
		return protocol.DiscoveredHubView{}, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return protocol.DiscoveredHubView{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return protocol.DiscoveredHubView{}, false
	}
	var doc struct {
		OK          bool   `json:"ok"`
		Service     string `json:"service"`
		Version     string `json:"version"`
		DiscoveryID string `json:"discoveryId"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<14)).Decode(&doc) != nil || !doc.OK {
		return protocol.DiscoveredHubView{}, false
	}
	if doc.Service != "hebi8-orbit-hub" {
		if strings.TrimSpace(doc.Version) == "" || !probeLegacyBootstrap(pctx, client, base) {
			return protocol.DiscoveredHubView{}, false
		}
	}
	return protocol.DiscoveredHubView{
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

func probeLegacyBootstrap(ctx context.Context, client *http.Client, base string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/bootstrap", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var doc map[string]any
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&doc) != nil {
		return false
	}
	if resp.StatusCode == http.StatusUnauthorized {
		code, _ := doc["code"].(string)
		return code == "ERR_UNAUTHORIZED"
	}
	if resp.StatusCode == http.StatusOK {
		_, ok := doc["machines"]
		return ok
	}
	return false
}
