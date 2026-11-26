package main

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
	sshMaxIncludeDepth    = 4
	sshMaxIncludeFiles    = 64
	sshMaxConcreteAliases = 256
	sshMaxAllHosts        = 64
	sshGExecTimeout       = 1500 * time.Millisecond
	sshGResultCacheTTL    = 30 * time.Second
	sshDefaultProbeTO     = 800 * time.Millisecond
)

// SSHTarget describes the remote machine to match against the desktop user's local ~/.ssh/config.
type SSHTarget struct {
	MachineID     string
	DisplayName   string
	TailscaleHost string
	SSHAlias      string
}

// SSHCandidate describes one matched entry from the local ~/.ssh/config.
type SSHCandidate struct {
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

// SSHMatchResult summarizes local ~/.ssh/config resolution for the desktop helper endpoint.
type SSHMatchResult struct {
	LocalConfigAvailable bool           `json:"localConfigAvailable"`
	Recommended          string         `json:"recommended,omitempty"`
	Candidates           []SSHCandidate `json:"candidates"`
	AllHosts             []string       `json:"allHosts"`
}

// SSHMatchOptions overrides defaults for deterministic unit tests.
type SSHMatchOptions struct {
	ConfigPath     string
	SSHDir         string
	DisableSSHExec bool
	DisableProbe   bool
	DisableDNS     bool
	ProbeTimeout   time.Duration
}

type sshHostBlock struct {
	patterns  []string
	hostName  string
	user      string
	port      int
	proxyJump string
}

type sshResolvedEntry struct {
	alias     string
	hostName  string
	user      string
	port      int
	proxyJump string
}

type desktopSSHGCacheItem struct {
	entry     sshResolvedEntry
	expiresAt time.Time
}

var (
	desktopSSHGCacheMu sync.Mutex
	desktopSSHGCache   = map[string]desktopSSHGCacheItem{}
)

// MatchLocalSSHConfig parses the local ~/.ssh/config (including recursive Include directives)
// and scores matching Host aliases against target.
func MatchLocalSSHConfig(ctx context.Context, target SSHTarget, opts SSHMatchOptions) SSHMatchResult {
	res := SSHMatchResult{
		Candidates: []SSHCandidate{},
		AllHosts:   []string{},
	}

	cfgPath, sshDir := resolveDesktopSSHPaths(opts)
	if cfgPath == "" {
		return res
	}
	fi, err := os.Stat(cfgPath)
	if err != nil || fi.IsDir() {
		return res
	}
	res.LocalConfigAvailable = true

	aliases, blocks := parseDesktopSSHConfigTree(cfgPath, sshDir)
	if len(aliases) == 0 {
		return res
	}

	if len(aliases) <= sshMaxAllHosts {
		res.AllHosts = append(res.AllHosts, aliases...)
	} else {
		res.AllHosts = append(res.AllHosts, aliases[:sshMaxAllHosts]...)
	}

	entries := make([]sshResolvedEntry, 0, len(aliases))
	for _, alias := range aliases {
		entries = append(entries, resolveDesktopStaticEntry(alias, blocks))
	}

	targetIPs := map[string]bool{}
	if !opts.DisableDNS {
		for _, raw := range []string{target.TailscaleHost, target.SSHAlias, target.MachineID} {
			addDesktopHostIPs(ctx, raw, targetIPs)
		}
	}

	type scoredEntry struct {
		entry  sshResolvedEntry
		score  int
		reason string
	}
	preScored := make([]scoredEntry, len(entries))
	for i, e := range entries {
		sc, reason := scoreDesktopSSHEntry(ctx, e, target, targetIPs, opts.DisableDNS)
		preScored[i] = scoredEntry{entry: e, score: sc, reason: reason}
	}

	if !opts.DisableSSHExec {
		type pair struct {
			idx   int
			score int
		}
		pairs := make([]pair, 0, len(preScored))
		for i, s := range preScored {
			if s.score > 0 || len(preScored) <= 12 {
				pairs = append(pairs, pair{idx: i, score: s.score})
			}
		}
		sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].score > pairs[j].score })
		if len(pairs) > 12 {
			pairs = pairs[:12]
		}
		var wg sync.WaitGroup
		sem := make(chan struct{}, 4)
		for _, p := range pairs {
			idx := p.idx
			alias := preScored[idx].entry.alias
			if strings.HasPrefix(alias, "-") {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				if refined, ok := resolveDesktopViaSSHG(ctx, cfgPath, alias); ok {
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
					sc, reason := scoreDesktopSSHEntry(ctx, e, target, targetIPs, opts.DisableDNS)
					preScored[idx] = scoredEntry{entry: e, score: sc, reason: reason}
				}
			}()
		}
		wg.Wait()
	}

	matched := make([]SSHCandidate, 0, len(preScored))
	for _, s := range preScored {
		if s.score <= 0 {
			continue
		}
		port := s.entry.port
		if port == 0 {
			port = 22
		}
		matched = append(matched, SSHCandidate{
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

	if !opts.DisableProbe && len(matched) > 1 {
		probeTO := opts.ProbeTimeout
		if probeTO <= 0 {
			probeTO = sshDefaultProbeTO
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
					ok := desktopTCPProbe(ctx, host, port, probeTO)
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

			sort.SliceStable(matched, func(i, j int) bool {
				ri := matched[i].Reachable
				rj := matched[j].Reachable
				if ri != nil && rj != nil && *ri != *rj {
					return *ri
				}
				if ri != nil && rj != nil && *ri && *rj {
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

func resolveDesktopSSHPaths(opts SSHMatchOptions) (string, string) {
	if strings.TrimSpace(opts.ConfigPath) != "" {
		p := expandHome(opts.ConfigPath)
		dir := strings.TrimSpace(opts.SSHDir)
		if dir == "" {
			dir = filepath.Dir(p)
		} else {
			dir = expandHome(dir)
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

func parseDesktopSSHConfigTree(rootPath, sshDir string) ([]string, []sshHostBlock) {
	visited := map[string]bool{}
	inStack := map[string]bool{}
	seenAlias := map[string]bool{}
	var aliases []string
	var blocks []sshHostBlock
	filesParsed := 0

	var walk func(filePath string, depth int, inheritedPatterns []string, inheritedInMatch bool)
	walk = func(filePath string, depth int, inheritedPatterns []string, inheritedInMatch bool) {
		if depth > sshMaxIncludeDepth || filesParsed >= sshMaxIncludeFiles {
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

		var current *sshHostBlock
		if !inheritedInMatch && len(inheritedPatterns) > 0 {
			current = &sshHostBlock{patterns: append([]string(nil), inheritedPatterns...)}
		}
		inMatch := inheritedInMatch
		flushBlock := func() {
			if current != nil {
				blocks = append(blocks, *current)
				current = nil
			}
		}

		for _, rawLine := range lines {
			line := desktopStripSSHComment(rawLine)
			if line == "" {
				continue
			}
			key, val := desktopSplitSSHDirective(line)
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
				for _, pat := range desktopSplitSSHTokens(val) {
					expanded := desktopExpandIncludePattern(pat, sshDir)
					if expanded == "" {
						continue
					}
					matches, gErr := filepath.Glob(expanded)
					if gErr != nil || len(matches) == 0 {
						if !strings.ContainsAny(expanded, "*?[") {
							walk(expanded, depth+1, parentPatterns, parentInMatch)
						}
						continue
					}
					sort.Strings(matches)
					for _, m := range matches {
						if filesParsed >= sshMaxIncludeFiles {
							break
						}
						if info, sErr := os.Stat(m); sErr == nil && !info.IsDir() {
							walk(m, depth+1, parentPatterns, parentInMatch)
						}
					}
				}
				inMatch = parentInMatch
				if !parentInMatch && len(parentPatterns) > 0 {
					current = &sshHostBlock{patterns: parentPatterns}
				}
			case "match":
				flushBlock()
				inMatch = true
			case "host":
				flushBlock()
				inMatch = false
				pats := desktopSplitSSHTokens(val)
				if len(pats) == 0 {
					continue
				}
				current = &sshHostBlock{patterns: pats}
				for _, pat := range pats {
					if desktopIsConcreteAlias(pat) && !seenAlias[pat] && len(aliases) < sshMaxConcreteAliases {
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
						current.hostName = desktopUnquoteSSH(desktopFirstToken(val))
					}
				case "user":
					if current.user == "" {
						current.user = desktopUnquoteSSH(desktopFirstToken(val))
					}
				case "port":
					if current.port == 0 {
						if p, err := strconv.Atoi(desktopUnquoteSSH(desktopFirstToken(val))); err == nil && p > 0 && p <= 65535 {
							current.port = p
						}
					}
				case "proxyjump":
					if current.proxyJump == "" {
						v := strings.TrimSpace(val)
						if !strings.EqualFold(v, "none") {
							current.proxyJump = desktopUnquoteSSH(v)
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

func resolveDesktopStaticEntry(alias string, blocks []sshHostBlock) sshResolvedEntry {
	e := sshResolvedEntry{alias: alias}
	for _, blk := range blocks {
		if !desktopMatchHostPatterns(alias, blk.patterns) {
			continue
		}
		if e.hostName == "" && blk.hostName != "" {
			e.hostName = strings.NewReplacer("%%", "%", "%h", alias, "%n", alias).Replace(blk.hostName)
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

func desktopMatchHostPatterns(alias string, patterns []string) bool {
	matched := false
	for _, pat := range patterns {
		if pat == "" {
			continue
		}
		if strings.HasPrefix(pat, "!") {
			if desktopWildcardMatch(pat[1:], alias) {
				return false
			}
			continue
		}
		if desktopWildcardMatch(pat, alias) {
			matched = true
		}
	}
	return matched
}

func desktopWildcardMatch(pattern, s string) bool {
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

func desktopIsConcreteAlias(pat string) bool {
	pat = strings.TrimSpace(pat)
	if pat == "" || strings.HasPrefix(pat, "!") || strings.ContainsAny(pat, "*?[]") {
		return false
	}
	return true
}

func desktopExpandIncludePattern(pat, sshDir string) string {
	pat = desktopUnquoteSSH(strings.TrimSpace(pat))
	if pat == "" {
		return ""
	}
	if pat == "~" || strings.HasPrefix(pat, "~/") {
		return expandHome(pat)
	}
	if filepath.IsAbs(pat) {
		return filepath.Clean(pat)
	}
	return filepath.Join(sshDir, pat)
}

func desktopStripSSHComment(line string) string {
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

func desktopSplitSSHDirective(line string) (string, string) {
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

func desktopSplitSSHTokens(s string) []string {
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

func desktopFirstToken(s string) string {
	toks := desktopSplitSSHTokens(s)
	if len(toks) == 0 {
		return ""
	}
	return toks[0]
}

func desktopUnquoteSSH(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func resolveDesktopViaSSHG(ctx context.Context, cfgPath, alias string) (sshResolvedEntry, bool) {
	cacheKey := cfgPath + "\x00" + alias
	now := time.Now()
	desktopSSHGCacheMu.Lock()
	if item, ok := desktopSSHGCache[cacheKey]; ok && now.Before(item.expiresAt) {
		desktopSSHGCacheMu.Unlock()
		return item.entry, true
	}
	desktopSSHGCacheMu.Unlock()

	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		return sshResolvedEntry{}, false
	}
	gCtx, cancel := context.WithTimeout(ctx, sshGExecTimeout)
	defer cancel()

	cmd := exec.CommandContext(gCtx, sshBin, "-F", cfgPath, "-G", "--", alias)
	out, err := cmd.Output()
	if err != nil {
		return sshResolvedEntry{}, false
	}
	entry := sshResolvedEntry{alias: alias}
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

	desktopSSHGCacheMu.Lock()
	if len(desktopSSHGCache) > 512 {
		for k, v := range desktopSSHGCache {
			if now.After(v.expiresAt) {
				delete(desktopSSHGCache, k)
			}
		}
	}
	desktopSSHGCache[cacheKey] = desktopSSHGCacheItem{entry: entry, expiresAt: now.Add(sshGResultCacheTTL)}
	desktopSSHGCacheMu.Unlock()
	return entry, true
}

func desktopNormHost(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}

func desktopShortHostLabel(s string) string {
	s = desktopNormHost(s)
	if s == "" || net.ParseIP(s) != nil {
		return ""
	}
	if idx := strings.IndexByte(s, '.'); idx > 0 {
		return s[:idx]
	}
	return s
}

func addDesktopHostIPs(ctx context.Context, rawHost string, dst map[string]bool) {
	h := desktopNormHost(rawHost)
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

func scoreDesktopSSHEntry(ctx context.Context, e sshResolvedEntry, target SSHTarget, targetIPs map[string]bool, disableDNS bool) (int, string) {
	aliasNorm := desktopNormHost(e.alias)
	hostNorm := desktopNormHost(e.hostName)
	aliasLabel := desktopShortHostLabel(e.alias)
	hostLabel := desktopShortHostLabel(e.hostName)

	tAlias := desktopNormHost(target.SSHAlias)
	tTailscale := desktopNormHost(target.TailscaleHost)
	tMachine := desktopNormHost(target.MachineID)
	tDisplay := desktopNormHost(target.DisplayName)

	tLabels := map[string]bool{}
	for _, raw := range []string{target.SSHAlias, target.TailscaleHost, target.MachineID, target.DisplayName} {
		if l := desktopShortHostLabel(raw); l != "" {
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
	if hostNorm != "" && len(targetIPs) > 0 {
		if ip := net.ParseIP(hostNorm); ip != nil {
			if targetIPs[ip.String()] {
				bump(88, "hostname IP matches target")
			}
		} else if !disableDNS {
			candIPs := map[string]bool{}
			addDesktopHostIPs(ctx, hostNorm, candIPs)
			for ip := range candIPs {
				if targetIPs[ip] {
					bump(82, "DNS IP intersection")
					break
				}
			}
		}
	}
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

func desktopTCPProbe(ctx context.Context, host string, port int, timeout time.Duration) bool {
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
