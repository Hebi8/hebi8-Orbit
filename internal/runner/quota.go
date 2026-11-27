package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

const (
	defaultQuotaCacheTTL = 2 * time.Minute
	defaultClaudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
	claudeOAuthBetaHeader = "oauth-2025-04-20"
)

// QuotaCollectorConfig configures real local agent quota collection on a Runner.
type QuotaCollectorConfig struct {
	TTL            time.Duration
	ResolveMainCLI func(protocol.Adapter) string
	MergedPATH     string
	HTTPClient     *http.Client
	NowFn          func() time.Time
	ClaudeTokenFn  func(ctx context.Context) string
	ClaudeUsageURL string
}

type quotaInFlight struct {
	done chan struct{}
	snap protocol.AgentQuotaSnapshot
}

// QuotaCollector collects real usage quotas for agy, claude, and codex with in-memory TTL caching
// and per-adapter single-flight deduplication so probes never spawn redundant subprocesses.
type QuotaCollector struct {
	cfg QuotaCollectorConfig

	mu       sync.Mutex
	cache    map[protocol.Adapter]protocol.AgentQuotaSnapshot
	inFlight map[protocol.Adapter]*quotaInFlight
}

// NewQuotaCollector creates a QuotaCollector with sane defaults.
func NewQuotaCollector(cfg QuotaCollectorConfig) *QuotaCollector {
	if cfg.TTL <= 0 {
		cfg.TTL = defaultQuotaCacheTTL
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	if cfg.NowFn == nil {
		cfg.NowFn = time.Now
	}
	if strings.TrimSpace(cfg.ClaudeUsageURL) == "" {
		cfg.ClaudeUsageURL = defaultClaudeUsageURL
	}
	if cfg.MergedPATH == "" {
		cfg.MergedPATH = os.Getenv("PATH")
	}
	return &QuotaCollector{
		cfg:      cfg,
		cache:    map[protocol.Adapter]protocol.AgentQuotaSnapshot{},
		inFlight: map[protocol.Adapter]*quotaInFlight{},
	}
}

// Snapshot returns cached quota snapshots in canonical adapter order without blocking or spawning processes.
func (c *QuotaCollector) Snapshot() []protocol.AgentQuotaSnapshot {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]protocol.AgentQuotaSnapshot, 0, len(c.cache))
	for _, adp := range protocol.Phase1ImplementedSessionAdapters {
		if snap, ok := c.cache[adp]; ok {
			out = append(out, cloneQuotaSnapshot(snap))
		}
	}
	return out
}

// CollectAll collects usage quotas concurrently for the requested adapters (defaulting to agy/claude/codex).
// When force is false, fresh cached entries within TTL are returned immediately.
func (c *QuotaCollector) CollectAll(ctx context.Context, adapters []protocol.Adapter, force bool) []protocol.AgentQuotaSnapshot {
	if c == nil {
		return nil
	}
	if len(adapters) == 0 {
		adapters = protocol.Phase1ImplementedSessionAdapters
	}
	results := make([]protocol.AgentQuotaSnapshot, len(adapters))
	var wg sync.WaitGroup
	for i, adp := range adapters {
		wg.Add(1)
		go func(idx int, a protocol.Adapter) {
			defer wg.Done()
			results[idx] = c.CollectOne(ctx, a, force)
		}(i, adp)
	}
	wg.Wait()
	return results
}

// CollectOne collects or returns a cached quota snapshot for a single adapter with single-flight deduplication.
func (c *QuotaCollector) CollectOne(ctx context.Context, adp protocol.Adapter, force bool) protocol.AgentQuotaSnapshot {
	now := c.cfg.NowFn()
	c.mu.Lock()
	if !force {
		if cached, ok := c.cache[adp]; ok && cached.FetchedAt > 0 {
			if now.Sub(time.UnixMilli(cached.FetchedAt)) < c.cfg.TTL {
				c.mu.Unlock()
				return cloneQuotaSnapshot(cached)
			}
		}
	}
	if call, busy := c.inFlight[adp]; busy {
		c.mu.Unlock()
		select {
		case <-call.done:
			return cloneQuotaSnapshot(call.snap)
		case <-ctx.Done():
			return protocol.AgentQuotaSnapshot{
				Adapter:   adp,
				Available: false,
				FetchedAt: c.cfg.NowFn().UnixMilli(),
			}
		}
	}
	call := &quotaInFlight{done: make(chan struct{})}
	c.inFlight[adp] = call
	c.mu.Unlock()

	var snap protocol.AgentQuotaSnapshot
	switch adp {
	case protocol.AdapterAgy:
		snap = c.collectAgy(ctx)
	case protocol.AdapterClaude:
		snap = c.collectClaude(ctx)
	case protocol.AdapterCodex:
		snap = c.collectCodex(ctx)
	default:
		snap = protocol.AgentQuotaSnapshot{
			Adapter:   adp,
			Available: false,
			FetchedAt: c.cfg.NowFn().UnixMilli(),
		}
	}

	c.mu.Lock()
	c.cache[adp] = cloneQuotaSnapshot(snap)
	call.snap = snap
	delete(c.inFlight, adp)
	close(call.done)
	c.mu.Unlock()
	return snap
}

func cloneQuotaSnapshot(in protocol.AgentQuotaSnapshot) protocol.AgentQuotaSnapshot {
	out := in
	if len(in.Windows) > 0 {
		out.Windows = make([]protocol.QuotaWindow, len(in.Windows))
		for i, w := range in.Windows {
			cp := w
			if len(w.ModelHints) > 0 {
				cp.ModelHints = append([]string(nil), w.ModelHints...)
			}
			out.Windows[i] = cp
		}
	}
	return out
}

func (c *QuotaCollector) resolveCLI(adp protocol.Adapter) string {
	if c.cfg.ResolveMainCLI != nil {
		if p := strings.TrimSpace(c.cfg.ResolveMainCLI(adp)); p != "" {
			return p
		}
	}
	name := defaultMainCLIBin(adp)
	for _, dir := range filepath.SplitList(c.cfg.MergedPATH) {
		if dir == "" {
			continue
		}
		cand := filepath.Join(dir, name)
		if isExecutableFile(cand) {
			return cand
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

func (c *QuotaCollector) collectAgy(ctx context.Context) protocol.AgentQuotaSnapshot {
	now := c.cfg.NowFn()
	empty := protocol.AgentQuotaSnapshot{
		Adapter:   protocol.AdapterAgy,
		Available: false,
		FetchedAt: now.UnixMilli(),
	}
	binPath := c.resolveCLI(protocol.AdapterAgy)
	if binPath == "" {
		return empty
	}

	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	outR, outW, err := os.Pipe()
	if err != nil {
		return empty
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		return empty
	}

	cmd := exec.Command(binPath, "-p", "/usage", "--output-format", "json")
	setProcessGroup(cmd)
	cmd.Env = setEnvVar(os.Environ(), "PATH", c.cfg.MergedPATH)
	cmd.Stdout = outW
	cmd.Stderr = errW
	if err := cmd.Start(); err != nil {
		_ = outR.Close()
		_ = outW.Close()
		_ = errR.Close()
		_ = errW.Close()
		return empty
	}
	pgid := processGroupID(cmd)
	// Close parent's write ends immediately so EOF reflects child/process-group exit.
	_ = outW.Close()
	_ = errW.Close()

	var stdout, stderr bytes.Buffer
	outDone := make(chan struct{})
	errDone := make(chan struct{})
	go func() {
		defer close(outDone)
		_, _ = io.Copy(&stdout, io.LimitReader(outR, 2<<20))
	}()
	go func() {
		defer close(errDone)
		_, _ = io.Copy(&stderr, io.LimitReader(errR, 512<<10))
	}()

	drainPipesBounded := func(timeout time.Duration) {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		for outCh, errCh := outDone, errDone; outCh != nil || errCh != nil; {
			select {
			case <-outCh:
				outCh = nil
			case <-errCh:
				errCh = nil
			case <-timer.C:
				_ = outR.Close()
				_ = errR.Close()
				<-outDone
				<-errDone
				return
			}
		}
		_ = outR.Close()
		_ = errR.Close()
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	var runErr error
	select {
	case runErr = <-waitCh:
		drainPipesBounded(150 * time.Millisecond)
	case <-pctx.Done():
		killProcessGroupByID(cmd, pgid)
		select {
		case <-waitCh:
		case <-time.After(150 * time.Millisecond):
		}
		drainPipesBounded(150 * time.Millisecond)
		empty.Error = "agy usage probe timed out"
		return empty
	}
	if runErr != nil {
		if msg := sanitizeOneLine(lastNonEmptyLine(stderr.String()), 160); msg != "" {
			empty.Error = msg
		} else {
			empty.Error = sanitizeOneLine(runErr.Error(), 160)
		}
		return empty
	}

	snap, err := parseAgyUsageJSON(stdout.Bytes(), c.cfg.NowFn())
	if err != nil {
		empty.Error = sanitizeOneLine(err.Error(), 160)
		return empty
	}
	return snap
}

func (c *QuotaCollector) collectClaude(ctx context.Context) protocol.AgentQuotaSnapshot {
	now := c.cfg.NowFn()
	empty := protocol.AgentQuotaSnapshot{
		Adapter:   protocol.AdapterClaude,
		Available: false,
		FetchedAt: now.UnixMilli(),
	}
	var token string
	if c.cfg.ClaudeTokenFn != nil {
		token = strings.TrimSpace(c.cfg.ClaudeTokenFn(ctx))
	} else {
		token = readClaudeOAuthAccessToken(ctx)
	}
	if token == "" {
		// Not logged in — skip silently without error noise.
		return empty
	}

	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, c.cfg.ClaudeUsageURL, nil)
	if err != nil {
		return empty
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", claudeOAuthBetaHeader)
	req.Header.Set("Accept", "application/json")

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		empty.Error = "claude usage request failed"
		return empty
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return empty
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// Expired/revoked OAuth token: report unavailable cleanly without leaking secrets.
		return empty
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		empty.Error = fmt.Sprintf("claude usage HTTP %d", resp.StatusCode)
		return empty
	}

	snap, err := parseClaudeUsageJSON(body, c.cfg.NowFn())
	if err != nil {
		empty.Error = sanitizeOneLine(err.Error(), 160)
		return empty
	}
	return snap
}

func (c *QuotaCollector) collectCodex(ctx context.Context) protocol.AgentQuotaSnapshot {
	now := c.cfg.NowFn()
	empty := protocol.AgentQuotaSnapshot{
		Adapter:   protocol.AdapterCodex,
		Available: false,
		FetchedAt: now.UnixMilli(),
	}
	binPath := c.resolveCLI(protocol.AdapterCodex)
	if binPath == "" {
		return empty
	}

	pctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	cmd := exec.Command(binPath, "app-server")
	setProcessGroup(cmd)
	cmd.Env = setEnvVar(os.Environ(), "PATH", c.cfg.MergedPATH)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return empty
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return empty
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return empty
	}
	pgid := processGroupID(cmd)

	type rpcReadResult struct {
		raw json.RawMessage
		err error
	}
	resCh := make(chan rpcReadResult, 1)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		br := bufio.NewReaderSize(stdout, 256*1024)
		for {
			line, rErr := br.ReadBytes('\n')
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				var env struct {
					ID     json.RawMessage `json:"id"`
					Result json.RawMessage `json:"result"`
					Error  *struct {
						Message string `json:"message"`
					} `json:"error"`
				}
				if json.Unmarshal(trimmed, &env) == nil && string(bytes.TrimSpace(env.ID)) == "2" {
					if env.Error != nil {
						resCh <- rpcReadResult{err: errors.New(env.Error.Message)}
						return
					}
					resCh <- rpcReadResult{raw: env.Result}
					return
				}
			}
			if rErr != nil {
				resCh <- rpcReadResult{err: rErr}
				return
			}
		}
	}()

	waitProcess := func(killImmediate bool) {
		_ = stdin.Close()
		if killImmediate {
			killProcessGroupByID(cmd, pgid)
			_ = stdout.Close()
		}
		select {
		case <-readDone:
		case <-time.After(150 * time.Millisecond):
			_ = stdout.Close()
			<-readDone
		}
		waitDone := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(waitDone)
		}()
		select {
		case <-waitDone:
		case <-time.After(250 * time.Millisecond):
			killProcessGroupByID(cmd, pgid)
			select {
			case <-waitDone:
			case <-time.After(150 * time.Millisecond):
			}
		}
	}

	initPayload := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"hebi8-orbit","version":"0.3.2"}}}`,
		`{"jsonrpc":"2.0","method":"initialized","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"account/rateLimits/read","params":{}}`,
	}, "\n") + "\n"
	if _, err := io.WriteString(stdin, initPayload); err != nil {
		waitProcess(true)
		return empty
	}

	select {
	case got := <-resCh:
		waitProcess(false)
		if got.err != nil || len(got.raw) == 0 {
			return empty
		}
		snap, err := parseCodexRateLimitsJSON(got.raw, c.cfg.NowFn())
		if err != nil {
			return empty
		}
		return snap
	case <-pctx.Done():
		waitProcess(true)
		return empty
	}
}

func readClaudeOAuthAccessToken(ctx context.Context) string {
	if runtime.GOOS == "darwin" {
		if tok := readClaudeKeychainToken(ctx); tok != "" {
			return tok
		}
	}
	configDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		configDir = filepath.Join(home, ".claude")
	} else {
		configDir = protocol.ExpandPath(configDir)
	}
	raw, err := os.ReadFile(filepath.Join(configDir, ".credentials.json"))
	if err != nil {
		return ""
	}
	return extractClaudeAccessTokenJSON(raw)
}

func readClaudeKeychainToken(ctx context.Context) string {
	kctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(kctx, "security", "find-generic-password", "-s", "Claude Code-credentials", "-w").Output()
	if err != nil {
		return ""
	}
	return extractClaudeAccessTokenJSON(out)
}

func extractClaudeAccessTokenJSON(raw []byte) string {
	var doc struct {
		ClaudeAiOauth *struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(bytes.TrimSpace(raw), &doc) != nil || doc.ClaudeAiOauth == nil {
		return ""
	}
	return strings.TrimSpace(doc.ClaudeAiOauth.AccessToken)
}

// parseAgyUsageJSON parses the JSON output of `agy -p /usage --output-format json`.
func parseAgyUsageJSON(raw []byte, now time.Time) (protocol.AgentQuotaSnapshot, error) {
	snap := protocol.AgentQuotaSnapshot{
		Adapter:   protocol.AdapterAgy,
		FetchedAt: now.UnixMilli(),
	}
	trimmed := bytes.TrimSpace(raw)
	if idx := bytes.IndexByte(trimmed, '{'); idx > 0 {
		trimmed = trimmed[idx:]
	}
	var doc struct {
		Status  string `json:"status"`
		Command *struct {
			Name string `json:"name"`
			Data *struct {
				Groups []struct {
					Name        string `json:"name"`
					Description string `json:"description"`
					Buckets     []struct {
						ID                string   `json:"id"`
						Name              string   `json:"name"`
						Window            string   `json:"window"`
						RemainingFraction *float64 `json:"remaining_fraction"`
						ResetTime         string   `json:"reset_time"`
					} `json:"buckets"`
				} `json:"groups"`
			} `json:"data"`
		} `json:"command"`
	}
	if err := json.Unmarshal(trimmed, &doc); err != nil {
		return snap, fmt.Errorf("parse agy usage json: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(doc.Status), "SUCCESS") {
		return snap, fmt.Errorf("agy usage status=%q", doc.Status)
	}
	if doc.Command == nil || doc.Command.Data == nil {
		return snap, nil
	}

	windows := make([]protocol.QuotaWindow, 0)
	for _, grp := range doc.Command.Data.Groups {
		groupName := strings.TrimSpace(grp.Name)
		hints := parseAgyModelHints(grp.Description)
		for _, b := range grp.Buckets {
			if b.RemainingFraction == nil {
				continue
			}
			win := normalizeQuotaWindowName(b.Window)
			if win == "" {
				continue
			}
			remFrac := *b.RemainingFraction
			if remFrac < 0 {
				remFrac = 0
			} else if remFrac > 1 {
				remFrac = 1
			}
			label := strings.TrimSpace(b.Name)
			if label == "" {
				label = win
			}
			windows = append(windows, protocol.QuotaWindow{
				Window:           win,
				Label:            label,
				Group:            groupName,
				ModelHints:       append([]string(nil), hints...),
				UsedPercent:      clampQuotaPercent((1.0 - remFrac) * 100.0),
				RemainingPercent: clampQuotaPercent(remFrac * 100.0),
				ResetsAt:         parseQuotaResetRFC3339(b.ResetTime, now),
			})
		}
	}
	snap.Windows = windows
	snap.Available = len(windows) > 0
	return snap, nil
}

func parseAgyModelHints(desc string) []string {
	s := strings.TrimSpace(desc)
	if s == "" {
		return nil
	}
	if idx := strings.Index(s, ":"); idx >= 0 {
		s = strings.TrimSpace(s[idx+1:])
	}
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func normalizeQuotaWindowName(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "5h", "five_hour", "five-hour", "5hr":
		return "5h"
	case "7d", "weekly", "seven_day", "seven-day", "1w":
		return "7d"
	default:
		return strings.TrimSpace(raw)
	}
}

// parseClaudeUsageJSON parses the response body from Anthropic's OAuth usage endpoint.
func parseClaudeUsageJSON(raw []byte, now time.Time) (protocol.AgentQuotaSnapshot, error) {
	snap := protocol.AgentQuotaSnapshot{
		Adapter:   protocol.AdapterClaude,
		FetchedAt: now.UnixMilli(),
	}
	type claudeBucket struct {
		Utilization *float64 `json:"utilization"`
		ResetsAt    *string  `json:"resets_at"`
	}
	var doc struct {
		FiveHour       *claudeBucket `json:"five_hour"`
		SevenDay       *claudeBucket `json:"seven_day"`
		SevenDayOpus   *claudeBucket `json:"seven_day_opus"`
		SevenDaySonnet *claudeBucket `json:"seven_day_sonnet"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &doc); err != nil {
		return snap, fmt.Errorf("parse claude usage json: %w", err)
	}

	addBucket := func(b *claudeBucket, win, label, group string, hints []string) {
		if b == nil || b.Utilization == nil {
			return
		}
		used := clampQuotaPercent(*b.Utilization)
		rem := clampQuotaPercent(100.0 - *b.Utilization)
		resetRaw := ""
		if b.ResetsAt != nil {
			resetRaw = *b.ResetsAt
		}
		snap.Windows = append(snap.Windows, protocol.QuotaWindow{
			Window:           win,
			Label:            label,
			Group:            group,
			ModelHints:       hints,
			UsedPercent:      used,
			RemainingPercent: rem,
			ResetsAt:         parseQuotaResetRFC3339(resetRaw, now),
		})
	}
	addBucket(doc.FiveHour, "5h", "5h", "", nil)
	addBucket(doc.SevenDay, "7d", "7d", "", nil)
	addBucket(doc.SevenDayOpus, "7d", "7d Opus", "Opus", []string{"opus"})
	addBucket(doc.SevenDaySonnet, "7d", "7d Sonnet", "Sonnet", []string{"sonnet"})

	snap.Available = len(snap.Windows) > 0
	return snap, nil
}

// parseCodexRateLimitsJSON parses the JSON-RPC result of `account/rateLimits/read`.
func parseCodexRateLimitsJSON(raw []byte, now time.Time) (protocol.AgentQuotaSnapshot, error) {
	snap := protocol.AgentQuotaSnapshot{
		Adapter:   protocol.AdapterCodex,
		FetchedAt: now.UnixMilli(),
	}
	type codexWindow struct {
		UsedPercent        *float64 `json:"usedPercent"`
		WindowDurationMins int      `json:"windowDurationMins"`
		ResetsAt           int64    `json:"resetsAt"`
	}
	type codexEntry struct {
		LimitID         string       `json:"limitId"`
		LimitName       string       `json:"limitName"`
		NormalModelSlug string       `json:"normalModelSlug"`
		Primary         *codexWindow `json:"primary"`
		Secondary       *codexWindow `json:"secondary"`
	}
	var doc struct {
		RateLimits          *codexEntry            `json:"rateLimits"`
		RateLimitsByLimitID map[string]*codexEntry `json:"rateLimitsByLimitId"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &doc); err != nil {
		return snap, fmt.Errorf("parse codex rateLimits json: %w", err)
	}

	seen := map[string]bool{}
	entries := make([]*codexEntry, 0, 1+len(doc.RateLimitsByLimitID))
	if doc.RateLimits != nil {
		entries = append(entries, doc.RateLimits)
		if id := strings.TrimSpace(doc.RateLimits.LimitID); id != "" {
			seen[id] = true
		}
	}
	if len(doc.RateLimitsByLimitID) > 0 {
		keys := make([]string, 0, len(doc.RateLimitsByLimitID))
		for k := range doc.RateLimitsByLimitID {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			e := doc.RateLimitsByLimitID[k]
			if e == nil {
				continue
			}
			id := strings.TrimSpace(e.LimitID)
			if id == "" {
				id = strings.TrimSpace(k)
			}
			if id != "" && seen[id] {
				continue
			}
			if id != "" {
				seen[id] = true
			}
			entries = append(entries, e)
		}
	}

	appendWin := func(entry *codexEntry, w *codexWindow, fallbackWin string) {
		if w == nil || w.UsedPercent == nil {
			return
		}
		win := fallbackWin
		if w.WindowDurationMins > 0 {
			if w.WindowDurationMins <= 360 {
				win = "5h"
			} else if w.WindowDurationMins >= 1440 {
				win = "7d"
			} else {
				win = fmt.Sprintf("%dm", w.WindowDurationMins)
			}
		}
		group := strings.TrimSpace(entry.LimitName)
		if group == "" && entry.LimitID != "" && !strings.EqualFold(entry.LimitID, "codex") {
			group = strings.TrimSpace(entry.LimitID)
		}
		var hints []string
		if slug := strings.TrimSpace(entry.NormalModelSlug); slug != "" {
			hints = []string{slug}
		}
		label := win
		if group != "" {
			label = group + " " + win
		}
		snap.Windows = append(snap.Windows, protocol.QuotaWindow{
			Window:           win,
			Label:            label,
			Group:            group,
			ModelHints:       hints,
			UsedPercent:      clampQuotaPercent(*w.UsedPercent),
			RemainingPercent: clampQuotaPercent(100.0 - *w.UsedPercent),
			ResetsAt:         parseQuotaResetUnix(w.ResetsAt, now),
		})
	}

	for _, e := range entries {
		appendWin(e, e.Primary, "5h")
		appendWin(e, e.Secondary, "7d")
	}

	snap.Available = len(snap.Windows) > 0
	return snap, nil
}

func clampQuotaPercent(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return math.Round(v*10) / 10
}

func parseQuotaResetRFC3339(raw string, now time.Time) int64 {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return 0
		}
	}
	if t.Unix() <= 0 || !t.After(now) {
		return 0
	}
	return t.UnixMilli()
}

func parseQuotaResetUnix(raw int64, now time.Time) int64 {
	if raw <= 0 {
		return 0
	}
	ms := raw
	if ms < 1_000_000_000_000 {
		ms = raw * 1000
	}
	if ms <= now.UnixMilli() {
		return 0
	}
	return ms
}
