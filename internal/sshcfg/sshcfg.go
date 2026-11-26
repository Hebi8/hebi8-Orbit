package sshcfg

import (
	"bufio"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxIncludeDepth    = 4
	maxIncludeFiles    = 64
	maxConcreteAliases = 256
	maxAllHosts        = 64
	sshGTimeout        = 1500 * time.Millisecond
	sshGCacheTTL       = 30 * time.Second
	defaultProbeTO     = 800 * time.Millisecond
)

// Target describes the remote Orbit machine we want to match against ~/.ssh/config.
type Target struct {
	MachineID     string
	DisplayName   string
	TailscaleHost string
	SSHAlias      string
}

// Candidate represents one matched Host alias from ~/.ssh/config.
type Candidate struct {
	Alias       string `json:"alias"`
	HostName    string `json:"hostName,omitempty"`
	User        string `json:"user,omitempty"`
	Port        int    `json:"port,omitempty"`
	ProxyJump   string `json:"proxyJump,omitempty"`
	Score       int    `json:"score"`
	MatchReason string `json:"matchReason,omitempty"`
	Reachable   *bool  `json:"reachable,omitempty"`
	LatencyMs   int64  `json:"latencyMs,omitempty"`
}

// MatchResult summarizes local ~/.ssh/config discovery and candidate ranking.
type MatchResult struct {
	LocalConfigAvailable bool        `json:"localConfigAvailable"`
	Recommended          string      `json:"recommended,omitempty"`
	Candidates           []Candidate `json:"candidates"`
	AllHosts             []string    `json:"allHosts"`
}

// MatchOptions allows tests and callers to override default config path or probes.
type MatchOptions struct {
	ConfigPath     string
	SSHDir         string
	DisableSSHExec bool
	DisableProbe   bool
	DisableDNS     bool
	ProbeTimeout   time.Duration
}

type hostBlock struct {
	patterns  []string
	hostName  string
	user      string
	port      int
	proxyJump string
}

type resolvedEntry struct {
	alias     string
	hostName  string
	user      string
	port      int
	proxyJump string
}

type scoredEntry struct {
	entry  resolvedEntry
	score  int
	reason string
}

type sshGCacheItem struct {
	entry     resolvedEntry
	expiresAt time.Time
}

var (
	sshGCacheMu sync.Mutex
	sshGCache   = map[string]sshGCacheItem{}
)

// MatchMachine parses the local ~/.ssh/config (including recursive Includes), resolves candidate
// parameters, scores them against target, optionally probes multiple direct candidates concurrently,
// and returns the recommended alias along with ranked candidates and all concrete hosts.
func MatchMachine(ctx context.Context, target Target, opts MatchOptions) MatchResult {
	res := MatchResult{
		Candidates: []Candidate{},
		AllHosts:   []string{},
	}

	cfgPath, sshDir := resolveConfigPaths(opts)
	if cfgPath == "" {
		return res
	}
	fi, err := os.Stat(cfgPath)
	if err != nil || fi.IsDir() {
		return res
	}
	res.LocalConfigAvailable = true

	aliases, blocks := parseSSHConfigTree(cfgPath, sshDir)
	if len(aliases) == 0 {
		return res
	}

	if len(aliases) <= maxAllHosts {
		res.AllHosts = append(res.AllHosts, aliases...)
	} else {
		res.AllHosts = append(res.AllHosts, aliases[:maxAllHosts]...)
	}

	// Build static resolved entries for every concrete alias.
	entries := make([]resolvedEntry, 0, len(aliases))
	for _, alias := range aliases {
		entries = append(entries, resolveStaticEntry(alias, blocks))
	}

	// Resolve target DNS IPs once if DNS lookup is enabled and target has a hostname.
	targetIPs := map[string]bool{}
	if !opts.DisableDNS {
		for _, raw := range []string{target.TailscaleHost, target.SSHAlias, target.MachineID} {
			addHostIPs(ctx, raw, targetIPs)
		}
	}

	// Pre-score all entries statically so we only invoke `ssh -G` on promising candidates.
	preScored := make([]scoredEntry, len(entries))
	for i, e := range entries {
		sc, reason := scoreEntry(ctx, e, target, targetIPs, opts.DisableDNS)
		preScored[i] = scoredEntry{entry: e, score: sc, reason: reason}
	}

	// Optionally refine top candidates (or all if small config) with `ssh -G <alias>`.
	if !opts.DisableSSHExec {
		toResolve := selectForSSHG(preScored, 12)
		if len(toResolve) > 0 {
			var wg sync.WaitGroup
			sem := make(chan struct{}, 4)
			for _, idx := range toResolve {
				idx := idx
				alias := preScored[idx].entry.alias
				if strings.HasPrefix(alias, "-") {
					continue
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					if refined, ok := resolveViaSSHG(ctx, cfgPath, alias); ok {
						e := preScored[idx].entry
						if refined.hostName != "" {
							e.hostName = refined.hostName
						}
						if refined.user != "" {
							e.user = refined.user
						}
						if refined.port > 0 {
							e.port = refined.port
						}
						if refined.proxyJump != "" {
							e.proxyJump = refined.proxyJump
						}
						sc, reason := scoreEntry(ctx, e, target, targetIPs, opts.DisableDNS)
						preScored[idx] = scoredEntry{entry: e, score: sc, reason: reason}
					}
				}()
			}
			wg.Wait()
		}
	}

	// Collect matched candidates (score > 0).
	matched := make([]Candidate, 0, len(preScored))
	for _, s := range preScored {
		if s.score <= 0 {
			continue
		}
		port := s.entry.port
		if port == 0 {
			port = 22
		}
		matched = append(matched, Candidate{
			Alias:       s.entry.alias,
			HostName:    s.entry.hostName,
			User:        s.entry.user,
			Port:        port,
			ProxyJump:   s.entry.proxyJump,
			Score:       s.score,
			MatchReason: s.reason,
		})
	}

	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].Score != matched[j].Score {
			return matched[i].Score > matched[j].Score
		}
		return matched[i].Alias < matched[j].Alias
	})

	// If multiple direct candidates exist (no ProxyJump), run concurrent TCP probes (800ms timeout)
	// on top candidates and prefer the fastest reachable alias.
	if !opts.DisableProbe && len(matched) > 1 {
		probeTO := opts.ProbeTimeout
		if probeTO <= 0 {
			probeTO = defaultProbeTO
		}
		topScore := matched[0].Score
		probeIndices := []int{}
		for i := range matched {
			if len(probeIndices) >= 6 {
				break
			}
			if matched[i].ProxyJump != "" {
				continue
			}
			// Only probe candidates that are reasonably close in score or direct matches
			if matched[i].Score >= topScore-50 {
				probeIndices = append(probeIndices, i)
			}
		}
		if len(probeIndices) > 1 {
			var wg sync.WaitGroup
			for _, idx := range probeIndices {
				idx := idx
				wg.Add(1)
				go func() {
					defer wg.Done()
					host := matched[idx].HostName
					if host == "" {
						host = matched[idx].Alias
					}
					port := matched[idx].Port
					if port <= 0 {
						port = 22
					}
					start := time.Now()
					ok := tcpProbe(ctx, host, port, probeTO)
					elapsed := time.Since(start).Milliseconds()
					if elapsed < 1 && ok {
						elapsed = 1
					}
					b := ok
					matched[idx].Reachable = &b
					if ok {
						matched[idx].LatencyMs = elapsed
					}
				}()
			}
			wg.Wait()

			// Re-rank: among candidates with comparable scores, reachable beats unreachable,
			// and faster latency breaks ties cleanly.
			sort.SliceStable(matched, func(i, j int) bool {
				ri := matched[i].Reachable
				rj := matched[j].Reachable
				if ri != nil && rj != nil && *ri != *rj {
					return *ri
				}
				if ri != nil && rj != nil && *ri && *rj {
					// If scores are very close (within 25 pts), prefer faster reachable host.
					diff := matched[i].Score - matched[j].Score
					if diff >= -25 && diff <= 25 && matched[i].LatencyMs != matched[j].LatencyMs {
						return matched[i].LatencyMs < matched[j].LatencyMs
					}
				}
				if matched[i].Score != matched[j].Score {
					return matched[i].Score > matched[j].Score
				}
				return matched[i].Alias < matched[j].Alias
			})
		}
	}

	if len(matched) > 16 {
		matched = matched[:16]
	}
	res.Candidates = matched
	if len(matched) > 0 {
		res.Recommended = matched[0].Alias
	}
	return res
}

func resolveConfigPaths(opts MatchOptions) (string, string) {
	if strings.TrimSpace(opts.ConfigPath) != "" {
		p := expandTilde(opts.ConfigPath)
		dir := strings.TrimSpace(opts.SSHDir)
		if dir == "" {
			dir = filepath.Dir(p)
		} else {
			dir = expandTilde(dir)
		}
		return p, dir
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", ""
	}
	sshDir := filepath.Join(home, ".ssh")
	return filepath.Join(sshDir, "config"), sshDir
}

func expandTilde(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func parseSSHConfigTree(rootPath, sshDir string) ([]string, []hostBlock) {
	visited := map[string]bool{}
	inStack := map[string]bool{}
	seenAlias := map[string]bool{}
	var aliases []string
	var blocks []hostBlock
	filesParsed := 0

	var walk func(filePath string, depth int, inheritedPatterns []string, inheritedInMatch bool)
	walk = func(filePath string, depth int, inheritedPatterns []string, inheritedInMatch bool) {
		if depth > maxIncludeDepth || filesParsed >= maxIncludeFiles {
			return
		}
		absPath, err := filepath.Abs(filePath)
		if err != nil {
			return
		}
		if canon, err := filepath.EvalSymlinks(absPath); err == nil {
			absPath = canon
		}
		if inStack[absPath] {
			return
		}
		visitKey := absPath + "\x00" + strings.Join(inheritedPatterns, "\x1f") + "\x00" + strconv.FormatBool(inheritedInMatch)
		if visited[visitKey] {
			return
		}
		f, err := os.Open(absPath)
		if err != nil {
			return
		}
		visited[visitKey] = true
		inStack[absPath] = true
		defer delete(inStack, absPath)
		filesParsed++

		var lines []string
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 512*1024)
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		_ = f.Close()

		var current *hostBlock
		if !inheritedInMatch && len(inheritedPatterns) > 0 {
			current = &hostBlock{patterns: append([]string(nil), inheritedPatterns...)}
		}
		inMatch := inheritedInMatch

		flushBlock := func() {
			if current != nil {
				blocks = append(blocks, *current)
				current = nil
			}
		}

		for _, rawLine := range lines {
			line := stripComment(rawLine)
			if line == "" {
				continue
			}
			key, val := splitDirective(line)
			if key == "" {
				continue
			}
			switch strings.ToLower(key) {
			case "include":
				var parentPatterns []string
				if current != nil {
					parentPatterns = append([]string(nil), current.patterns...)
				}
				parentInMatch := inMatch
				flushBlock()
				for _, pat := range splitTokens(val) {
					expanded := expandIncludePattern(pat, sshDir)
					if expanded == "" {
						continue
					}
					matches, gErr := filepath.Glob(expanded)
					if gErr != nil || len(matches) == 0 {
						if !hasGlobMeta(expanded) {
							walk(expanded, depth+1, parentPatterns, parentInMatch)
						}
						continue
					}
					sort.Strings(matches)
					for _, m := range matches {
						if filesParsed >= maxIncludeFiles {
							break
						}
						if info, sErr := os.Stat(m); sErr == nil && !info.IsDir() {
							walk(m, depth+1, parentPatterns, parentInMatch)
						}
					}
				}
				inMatch = parentInMatch
				if !parentInMatch && len(parentPatterns) > 0 {
					current = &hostBlock{patterns: parentPatterns}
				}
			case "match":
				flushBlock()
				inMatch = true
			case "host":
				flushBlock()
				inMatch = false
				pats := splitTokens(val)
				if len(pats) == 0 {
					continue
				}
				current = &hostBlock{patterns: pats}
				for _, pat := range pats {
					if isConcreteAlias(pat) && !seenAlias[pat] && len(aliases) < maxConcreteAliases {
						seenAlias[pat] = true
						aliases = append(aliases, pat)
					}
				}
			default:
				if inMatch || current == nil {
					continue
				}
				switch strings.ToLower(key) {
				case "hostname":
					if current.hostName == "" {
						current.hostName = unquoteToken(firstToken(val))
					}
				case "user":
					if current.user == "" {
						current.user = unquoteToken(firstToken(val))
					}
				case "port":
					if current.port == 0 {
						if p, err := strconv.Atoi(unquoteToken(firstToken(val))); err == nil && p > 0 && p <= 65535 {
							current.port = p
						}
					}
				case "proxyjump":
					if current.proxyJump == "" {
						v := strings.TrimSpace(val)
						if !strings.EqualFold(v, "none") {
							current.proxyJump = unquoteToken(v)
						}
					}
				}
			}
		}
		flushBlock()
	}

	walk(rootPath, 0, nil, false)
	return aliases, blocks
}

func resolveStaticEntry(alias string, blocks []hostBlock) resolvedEntry {
	e := resolvedEntry{alias: alias}
	for _, blk := range blocks {
		if !matchHostPatterns(alias, blk.patterns) {
			continue
		}
		if e.hostName == "" && blk.hostName != "" {
			e.hostName = expandSSHPercentTokens(blk.hostName, alias)
		}
		if e.user == "" && blk.user != "" {
			e.user = blk.user
		}
		if e.port == 0 && blk.port > 0 {
			e.port = blk.port
		}
		if e.proxyJump == "" && blk.proxyJump != "" {
			e.proxyJump = blk.proxyJump
		}
	}
	return e
}

func expandSSHPercentTokens(val, alias string) string {
	if !strings.Contains(val, "%") {
		return val
	}
	repl := strings.NewReplacer("%%", "%", "%h", alias, "%n", alias)
	return repl.Replace(val)
}

func matchHostPatterns(alias string, patterns []string) bool {
	matched := false
	for _, pat := range patterns {
		if pat == "" {
			continue
		}
		if strings.HasPrefix(pat, "!") {
			if wildcardMatch(pat[1:], alias) {
				return false
			}
			continue
		}
		if wildcardMatch(pat, alias) {
			matched = true
		}
	}
	return matched
}

func wildcardMatch(pattern, s string) bool {
	pattern = strings.ToLower(pattern)
	s = strings.ToLower(s)
	px, sx := 0, 0
	starIdx, matchIdx := -1, 0
	for sx < len(s) {
		if px < len(pattern) && (pattern[px] == '?' || pattern[px] == s[sx]) {
			px++
			sx++
		} else if px < len(pattern) && pattern[px] == '*' {
			starIdx = px
			matchIdx = sx
			px++
		} else if starIdx != -1 {
			px = starIdx + 1
			matchIdx++
			sx = matchIdx
		} else {
			return false
		}
	}
	for px < len(pattern) && pattern[px] == '*' {
		px++
	}
	return px == len(pattern)
}

func isConcreteAlias(pat string) bool {
	pat = strings.TrimSpace(pat)
	if pat == "" || strings.HasPrefix(pat, "!") {
		return false
	}
	if strings.ContainsAny(pat, "*?[]") {
		return false
	}
	return true
}

func expandIncludePattern(pat, sshDir string) string {
	pat = unquoteToken(strings.TrimSpace(pat))
	if pat == "" {
		return ""
	}
	if pat == "~" || strings.HasPrefix(pat, "~/") {
		return expandTilde(pat)
	}
	if filepath.IsAbs(pat) {
		return filepath.Clean(pat)
	}
	return filepath.Join(sshDir, pat)
}

func hasGlobMeta(p string) bool {
	return strings.ContainsAny(p, "*?[")
}

func stripComment(line string) string {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return ""
	}
	inQuote := false
	for i := 0; i < len(line); i++ {
		if line[i] == '"' {
			inQuote = !inQuote
		} else if line[i] == '#' && !inQuote {
			return strings.TrimSpace(line[:i])
		}
	}
	return line
}

func splitDirective(line string) (string, string) {
	// OpenSSH accepts either `Key Value` or `Key=Value` (with optional surrounding spaces).
	inQuote := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if ch == '"' {
			inQuote = !inQuote
			continue
		}
		if !inQuote && (ch == ' ' || ch == '\t' || ch == '=') {
			key := strings.TrimSpace(line[:i])
			rest := strings.TrimSpace(line[i+1:])
			if ch != '=' && strings.HasPrefix(rest, "=") {
				rest = strings.TrimSpace(rest[1:])
			}
			return key, rest
		}
	}
	return strings.TrimSpace(line), ""
}

func splitTokens(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '"' {
			inQuote = !inQuote
			continue
		}
		if !inQuote && (ch == ' ' || ch == '\t') {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
			continue
		}
		cur.WriteByte(ch)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func firstToken(s string) string {
	toks := splitTokens(s)
	if len(toks) == 0 {
		return ""
	}
	return toks[0]
}

func unquoteToken(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func selectForSSHG(scored []scoredEntry, limit int) []int {
	type pair struct {
		idx   int
		score int
	}
	pairs := make([]pair, 0, len(scored))
	for i, s := range scored {
		if s.score > 0 || len(scored) <= limit {
			pairs = append(pairs, pair{idx: i, score: s.score})
		}
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		return pairs[i].score > pairs[j].score
	})
	if len(pairs) > limit {
		pairs = pairs[:limit]
	}
	out := make([]int, len(pairs))
	for i, p := range pairs {
		out[i] = p.idx
	}
	return out
}

func resolveViaSSHG(ctx context.Context, cfgPath, alias string) (resolvedEntry, bool) {
	cacheKey := cfgPath + "\x00" + alias
	now := time.Now()
	sshGCacheMu.Lock()
	if item, ok := sshGCache[cacheKey]; ok && now.Before(item.expiresAt) {
		sshGCacheMu.Unlock()
		return item.entry, true
	}
	sshGCacheMu.Unlock()

	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		return resolvedEntry{}, false
	}

	gCtx, cancel := context.WithTimeout(ctx, sshGTimeout)
	defer cancel()

	cmd := exec.CommandContext(gCtx, sshBin, "-F", cfgPath, "-G", "--", alias)
	out, err := cmd.Output()
	if err != nil {
		return resolvedEntry{}, false
	}

	entry := resolvedEntry{alias: alias}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		k, v, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.ToLower(k) {
		case "hostname":
			entry.hostName = v
		case "user":
			entry.user = v
		case "port":
			if p, pErr := strconv.Atoi(v); pErr == nil && p > 0 {
				entry.port = p
			}
		case "proxyjump":
			if !strings.EqualFold(v, "none") {
				entry.proxyJump = v
			}
		}
	}

	sshGCacheMu.Lock()
	// Bound cache size simply
	if len(sshGCache) > 512 {
		for k, v := range sshGCache {
			if now.After(v.expiresAt) {
				delete(sshGCache, k)
			}
		}
	}
	sshGCache[cacheKey] = sshGCacheItem{entry: entry, expiresAt: now.Add(sshGCacheTTL)}
	sshGCacheMu.Unlock()
	return entry, true
}

func normalizeHost(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, ".")
	return s
}

func shortHostLabel(s string) string {
	s = normalizeHost(s)
	if s == "" {
		return ""
	}
	if net.ParseIP(s) != nil {
		return ""
	}
	if idx := strings.IndexByte(s, '.'); idx > 0 {
		return s[:idx]
	}
	return s
}

func addHostIPs(ctx context.Context, rawHost string, dst map[string]bool) {
	h := normalizeHost(rawHost)
	if h == "" {
		return
	}
	if ip := net.ParseIP(h); ip != nil {
		dst[ip.String()] = true
		return
	}
	dCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(dCtx, h)
	if err != nil {
		return
	}
	for _, ip := range ips {
		dst[ip.IP.String()] = true
	}
}

func scoreEntry(ctx context.Context, e resolvedEntry, target Target, targetIPs map[string]bool, disableDNS bool) (int, string) {
	aliasNorm := normalizeHost(e.alias)
	hostNorm := normalizeHost(e.hostName)
	aliasLabel := shortHostLabel(e.alias)
	hostLabel := shortHostLabel(e.hostName)

	tAlias := normalizeHost(target.SSHAlias)
	tTailscale := normalizeHost(target.TailscaleHost)
	tMachine := normalizeHost(target.MachineID)
	tDisplay := normalizeHost(target.DisplayName)

	tLabels := map[string]bool{}
	for _, raw := range []string{target.SSHAlias, target.TailscaleHost, target.MachineID, target.DisplayName} {
		if l := shortHostLabel(raw); l != "" {
			tLabels[l] = true
		}
	}

	score := 0
	reason := ""
	bump := func(pts int, r string) {
		if pts > score {
			score = pts
			reason = r
		}
	}

	// Exact alias & HostName matches
	if tAlias != "" && aliasNorm == tAlias {
		bump(120, "alias exact match")
	}
	if tTailscale != "" && hostNorm != "" && hostNorm == tTailscale {
		bump(110, "hostname matches tailscaleHost")
	}
	if tTailscale != "" && aliasNorm == tTailscale {
		bump(100, "alias matches tailscaleHost")
	}
	if tMachine != "" && aliasNorm == tMachine {
		bump(95, "alias matches machineId")
	}
	if tDisplay != "" && aliasNorm == tDisplay {
		bump(90, "alias matches displayName")
	}
	if tMachine != "" && hostNorm != "" && hostNorm == tMachine {
		bump(85, "hostname matches machineId")
	}

	// IP intersection match (exact IP or DNS-resolved IP overlap)
	if hostNorm != "" && len(targetIPs) > 0 {
		if ip := net.ParseIP(hostNorm); ip != nil {
			if targetIPs[ip.String()] {
				bump(88, "hostname IP matches target")
			}
		} else if !disableDNS {
			candIPs := map[string]bool{}
			addHostIPs(ctx, hostNorm, candIPs)
			for ip := range candIPs {
				if targetIPs[ip] {
					bump(82, "DNS IP intersection")
					break
				}
			}
		}
	}

	// Short hostname label matches (strip domain suffix)
	if aliasLabel != "" && tLabels[aliasLabel] {
		bump(72, "short alias label match")
	}
	if hostLabel != "" && tLabels[hostLabel] {
		bump(68, "short hostname label match")
	}

	if score > 0 && strings.TrimSpace(e.user) != "" {
		score += 8
	}
	return score, reason
}

func tcpProbe(ctx context.Context, host string, port int, timeout time.Duration) bool {
	if host == "" || port <= 0 {
		return false
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	d := net.Dialer{Timeout: timeout}
	pCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := d.DialContext(pCtx, "tcp", addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
