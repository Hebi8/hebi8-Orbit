package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

// AgentManagerConfig configures runtime ACP adapter discovery, probing, testing, and bridge installation.
type AgentManagerConfig struct {
	DataDir       string
	OrbitBinDir   string
	SocketPath    string
	RunnerVersion string
	FlagBins      map[protocol.Adapter]string
	FileConfigs   map[protocol.Adapter]protocol.AdapterConfig
}

type cachedAgentCaps struct {
	models        []string
	defaultModel  string
	modes         []protocol.AgentModeOption
	defaultMode   string
	modeViaLegacy bool
	efforts       []string
	defaultEffort string
	authChecked   bool
	needsLogin    bool
	detail        string
	cachedAt      time.Time
}

// AgentManager discovers, probes, tests, installs pinned bridges for, and constructs ACP drivers.
type AgentManager struct {
	cfg        AgentManagerConfig
	mergedPATH string

	mu         sync.RWMutex
	configRev  int64
	hubConfigs map[string]protocol.RunnerAgentConfig
	probes     map[protocol.Adapter]protocol.AgentProbe
	capCache   map[string]cachedAgentCaps
	installMu  sync.Mutex
}

// NewAgentManager initializes an AgentManager and captures the user's login-shell PATH once.
func NewAgentManager(cfg AgentManagerConfig) *AgentManager {
	cfg.DataDir = protocol.ExpandPath(cfg.DataDir)
	if cfg.DataDir == "" {
		cfg.DataDir = protocol.ExpandPath("~/.local/share/hebi8-orbit")
	}
	if cfg.RunnerVersion == "" {
		cfg.RunnerVersion = "0.2.0"
	}
	if cfg.FlagBins == nil {
		cfg.FlagBins = map[protocol.Adapter]string{}
	}
	if cfg.FileConfigs == nil {
		cfg.FileConfigs = map[protocol.Adapter]protocol.AdapterConfig{}
	}
	return &AgentManager{
		cfg:        cfg,
		mergedPATH: resolveLoginShellPATH(cfg.DataDir, cfg.OrbitBinDir),
		hubConfigs: map[string]protocol.RunnerAgentConfig{},
		probes:     map[protocol.Adapter]protocol.AgentProbe{},
		capCache:   map[string]cachedAgentCaps{},
	}
}

// MergedPATH returns the resolved search PATH including login-shell and managed adapter bins.
func (m *AgentManager) MergedPATH() string { return m.mergedPATH }

// ConfigRev returns the last applied Hub configuration revision.
func (m *AgentManager) ConfigRev() int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.configRev
}

// ApplyHubConfig stores Hub-pushed sparse agent overrides monotonically (ignoring stale revisions)
// and returns (applied, effectiveRev).
func (m *AgentManager) ApplyHubConfig(cfg protocol.RunnerConfigPayload) (bool, int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cfg.ConfigRev > 0 && cfg.ConfigRev < m.configRev {
		return false, m.configRev
	}
	if cfg.ConfigRev > m.configRev {
		m.configRev = cfg.ConfigRev
	}
	next := make(map[string]protocol.RunnerAgentConfig, len(cfg.Agents))
	for k, v := range cfg.Agents {
		next[k] = v
	}
	m.hubConfigs = next
	return true, m.configRev
}

// EffectiveMaxWorkers returns the configured maxLiveWorkers cap for adp (0 means unconfigured).
func (m *AgentManager) EffectiveMaxWorkers(adp protocol.Adapter) int {
	m.mu.RLock()
	hubCfg := m.hubConfigs[string(adp)]
	m.mu.RUnlock()
	return m.effectiveMaxWorkers(adp, hubCfg)
}

// SnapshotProbes returns the latest probes ordered canonically (agy, claude, codex).
func (m *AgentManager) SnapshotProbes() []protocol.AgentProbe {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]protocol.AgentProbe, 0, len(protocol.Phase1ImplementedSessionAdapters))
	for _, adp := range protocol.Phase1ImplementedSessionAdapters {
		if p, ok := m.probes[adp]; ok {
			out = append(out, p)
		}
	}
	return out
}

// ProbeAll probes all supported session adapters concurrently and stores results.
func (m *AgentManager) ProbeAll(ctx context.Context) []protocol.AgentProbe {
	probes, _, _ := m.ProbeAllWithRev(ctx)
	return probes
}

// ProbeAllWithRev probes all adapters bound to the current configRev; if a newer configRev
// supersedes this run before completion, stale results are discarded and fresh=false is returned.
func (m *AgentManager) ProbeAllWithRev(ctx context.Context) (probes []protocol.AgentProbe, probedRev int64, fresh bool) {
	probedRev = m.ConfigRev()
	adapters := protocol.Phase1ImplementedSessionAdapters
	results := make([]protocol.AgentProbe, len(adapters))
	var wg sync.WaitGroup
	for i, adp := range adapters {
		wg.Add(1)
		go func(idx int, a protocol.Adapter) {
			defer wg.Done()
			results[idx] = m.probeOneForRev(ctx, a, probedRev)
		}(i, adp)
	}
	wg.Wait()
	if ctx.Err() != nil || m.ConfigRev() != probedRev {
		return nil, probedRev, false
	}
	return results, probedRev, true
}

// ProbeOne inspects one adapter: enabled state, binary resolution, main CLI presence, and ACP initialize.
func (m *AgentManager) ProbeOne(ctx context.Context, adp protocol.Adapter) protocol.AgentProbe {
	return m.probeOneForRev(ctx, adp, m.ConfigRev())
}

func (m *AgentManager) probeOneForRev(ctx context.Context, adp protocol.Adapter, expectedRev int64) protocol.AgentProbe {
	now := time.Now().UnixMilli()
	m.mu.RLock()
	hubCfg := m.hubConfigs[string(adp)]
	prev := m.probes[adp]
	m.mu.RUnlock()

	enabled := true
	if hubCfg.Enabled != nil {
		enabled = *hubCfg.Enabled
	}

	probe := protocol.AgentProbe{
		Adapter:        adp,
		Enabled:        enabled,
		DefaultModel:   hubCfg.DefaultModel,
		MaxLiveWorkers: m.effectiveMaxWorkers(adp, hubCfg),
		LastTest:       prev.LastTest,
		ProbedAt:       now,
	}

	// Detect main CLI regardless of enabled state for helpful hints
	probe.MainCLI = m.findInPATH(defaultMainCLIBin(adp))

	if !enabled {
		probe.State = protocol.AgentStateDisabled
		probe.Source = "settings"
		m.storeProbeIfRev(adp, probe, expectedRev)
		return probe
	}

	binPath, args, source, explicitMissing := m.resolveBridgeBinary(adp, hubCfg)
	probe.Source = source
	if binPath == "" {
		if explicitMissing != "" {
			probe.State = protocol.AgentStateError
			probe.HintCode = "bin_not_found"
			probe.Detail = sanitizeOneLine("executable not found: "+explicitMissing, 200)
		} else if (adp == protocol.AdapterClaude || adp == protocol.AdapterCodex) && probe.MainCLI != "" {
			probe.State = protocol.AgentStateMissingBridge
			if m.findInPATH("npm") == "" {
				probe.HintCode = "missing_npm"
			} else {
				probe.HintCode = "missing_bridge"
			}
		} else {
			probe.State = protocol.AgentStateNotInstalled
			probe.HintCode = "not_installed"
		}
		m.storeProbeIfRev(adp, probe, expectedRev)
		return probe
	}

	probe.BridgeBin = binPath
	capKey := string(adp) + "\x00" + binPath + "\x00" + strings.Join(args, "\x00")
	m.mu.RLock()
	cached, hasCached := m.capCache[capKey]
	m.mu.RUnlock()
	useCache := hasCached && !cached.needsLogin && time.Since(cached.cachedAt) < 10*time.Minute

	ver, freshCaps, initErr := m.probeACPCapabilities(ctx, adp, binPath, args, !useCache)
	if initErr != nil {
		probe.State = protocol.AgentStateError
		probe.HintCode = "init_error"
		probe.Detail = sanitizeOneLine(initErr.Error(), 200)
	} else {
		probe.BridgeVersion = ver
		if probe.BridgeVersion == "" {
			probe.BridgeVersion = m.readManagedPackageVersion(adp)
		}
		var effCaps *cachedAgentCaps
		if freshCaps != nil {
			effCaps = freshCaps
			m.mu.Lock()
			m.capCache[capKey] = *freshCaps
			m.mu.Unlock()
		} else if useCache {
			c := cached
			effCaps = &c
		}
		if effCaps != nil {
			probe.Models = append([]string(nil), effCaps.models...)
			probe.Modes = append([]protocol.AgentModeOption(nil), effCaps.modes...)
			probe.DefaultMode = effCaps.defaultMode
			probe.ModeViaLegacy = effCaps.modeViaLegacy
			probe.Efforts = append([]string(nil), effCaps.efforts...)
			probe.DefaultEffort = effCaps.defaultEffort
			probe.AuthChecked = effCaps.authChecked
			if probe.DefaultModel == "" && effCaps.defaultModel != "" {
				probe.DefaultModel = effCaps.defaultModel
			}
			if effCaps.needsLogin {
				probe.State = protocol.AgentStateNeedsLogin
				probe.HintCode = "needs_login"
				probe.Detail = effCaps.detail
			} else {
				probe.State = protocol.AgentStateReady
			}
		} else {
			probe.State = protocol.AgentStateReady
		}
	}

	m.storeProbeIfRev(adp, probe, expectedRev)
	return probe
}

// TestAgent runs an active ACP initialize + session/new connectivity & auth check in a temp directory,
// bound to the configRev observed at start so concurrent disable/config changes are never overwritten.
func (m *AgentManager) TestAgent(ctx context.Context, adp protocol.Adapter) (protocol.AgentProbe, int64, error) {
	startRev := m.ConfigRev()
	m.mu.RLock()
	hubCfg := m.hubConfigs[string(adp)]
	m.mu.RUnlock()
	binPath, args, _, _ := m.resolveBridgeBinary(adp, hubCfg)
	if binPath != "" {
		capKey := string(adp) + "\x00" + binPath + "\x00" + strings.Join(args, "\x00")
		m.mu.Lock()
		delete(m.capCache, capKey)
		m.mu.Unlock()
	}

	probe := m.probeOneForRev(ctx, adp, startRev)
	if probe.State != protocol.AgentStateReady && probe.State != protocol.AgentStateNeedsLogin {
		return probe, startRev, fmt.Errorf("agent %s is not ready (%s)", adp, probe.State)
	}

	tmpDir, err := os.MkdirTemp("", "orbit-agent-test-*")
	if err != nil {
		return probe, startRev, err
	}
	defer os.RemoveAll(tmpDir)

	start := time.Now()
	testCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	drv := m.buildDriver(adp, binPath, args, nil)
	err = drv.Start(testCtx, func(context.Context, acp.SessionNotification) error { return nil },
		func(context.Context, acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
			return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
		})
	var caps cachedAgentCaps
	if err == nil {
		var sessResp acp.NewSessionResponse
		sessResp, err = drv.NewSession(testCtx, tmpDir, "")
		if err == nil {
			caps = extractSessionCapabilities(sessResp)
			caps.authChecked = true
			caps.cachedAt = time.Now()
			_ = drv.CloseSession(testCtx, sessResp.SessionId)
		}
		_ = drv.Close(context.Background())
	}
	latencyMs := time.Since(start).Milliseconds()

	now := time.Now().UnixMilli()
	capKey := string(adp) + "\x00" + binPath + "\x00" + strings.Join(args, "\x00")
	if err != nil {
		detail := sanitizeOneLine(err.Error(), 200)
		isAuth := looksLikeAuthError(err.Error())
		if isAuth {
			detail = "尚未登录，请先在该机器终端完成登录认证"
		}
		probe.LastTest = &protocol.AgentTestResult{
			OK:        false,
			At:        now,
			LatencyMs: latencyMs,
			Error:     detail,
		}
		probe.AuthChecked = true
		if isAuth {
			probe.State = protocol.AgentStateNeedsLogin
			probe.HintCode = "needs_login"
			probe.Detail = detail
			m.mu.Lock()
			m.capCache[capKey] = cachedAgentCaps{authChecked: true, needsLogin: true, detail: detail, cachedAt: time.Now()}
			m.mu.Unlock()
		} else {
			probe.State = protocol.AgentStateError
			probe.HintCode = "test_error"
			probe.Detail = detail
		}
		probe.ProbedAt = now
		m.storeProbeIfRev(adp, probe, startRev)
		return probe, startRev, errors.New(detail)
	}

	probe.State = protocol.AgentStateReady
	probe.HintCode = ""
	probe.Detail = ""
	probe.Models = append([]string(nil), caps.models...)
	probe.Modes = append([]protocol.AgentModeOption(nil), caps.modes...)
	probe.DefaultMode = caps.defaultMode
	probe.ModeViaLegacy = caps.modeViaLegacy
	probe.Efforts = append([]string(nil), caps.efforts...)
	probe.DefaultEffort = caps.defaultEffort
	probe.AuthChecked = true
	if hubCfg.DefaultModel != "" {
		probe.DefaultModel = hubCfg.DefaultModel
	} else if caps.defaultModel != "" {
		probe.DefaultModel = caps.defaultModel
	}
	probe.LastTest = &protocol.AgentTestResult{
		OK:        true,
		At:        now,
		LatencyMs: latencyMs,
	}
	probe.ProbedAt = now
	m.mu.Lock()
	m.capCache[capKey] = caps
	m.mu.Unlock()
	m.storeProbeIfRev(adp, probe, startRev)
	return probe, startRev, nil
}

// InstallBridge installs the exact pinned ACP bridge package into <dataDir>/acp-adapters via npm (no shell).
func (m *AgentManager) InstallBridge(ctx context.Context, adp protocol.Adapter) (protocol.AgentProbe, int64, error) {
	pkg, ok := protocol.PinnedBridgePackages[adp]
	if !ok {
		return protocol.AgentProbe{}, m.ConfigRev(), fmt.Errorf("no managed bridge package for %s", adp)
	}

	m.installMu.Lock()
	defer m.installMu.Unlock()

	npmBin := m.findInPATH("npm")
	if npmBin == "" {
		rev := m.ConfigRev()
		probe := m.probeOneForRev(ctx, adp, rev)
		probe.HintCode = "missing_npm"
		probe.Detail = "未找到 npm，请先安装 Node.js"
		m.storeProbeIfRev(adp, probe, rev)
		return probe, rev, errors.New("npm not found in PATH")
	}

	adaptersDir := filepath.Join(m.cfg.DataDir, "acp-adapters")
	if err := os.MkdirAll(adaptersDir, 0o755); err != nil {
		return protocol.AgentProbe{}, m.ConfigRev(), err
	}

	// Mark installing state while npm runs
	m.mu.Lock()
	cur := m.probes[adp]
	cur.Adapter = adp
	cur.State = protocol.AgentStateInstalling
	m.probes[adp] = cur
	m.mu.Unlock()

	instCtx, cancel := context.WithTimeout(ctx, 175*time.Second)
	defer cancel()

	cmd := exec.CommandContext(instCtx, npmBin, "install", "--prefix", adaptersDir, "--no-audit", "--no-fund", pkg)
	cmd.Env = setEnvVar(os.Environ(), "PATH", m.mergedPATH)
	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf

	if err := cmd.Run(); err != nil {
		rev := m.ConfigRev()
		probe := m.probeOneForRev(ctx, adp, rev)
		probe.State = protocol.AgentStateError
		probe.HintCode = "install_error"
		probe.Detail = sanitizeOneLine(fmt.Sprintf("npm install failed: %v (%s)", err, lastNonEmptyLine(outBuf.String())), 200)
		m.storeProbeIfRev(adp, probe, rev)
		return probe, rev, errors.New(probe.Detail)
	}

	// Re-probe bound to current configRev (respecting any disable received during install)
	rev := m.ConfigRev()
	probe := m.probeOneForRev(ctx, adp, rev)
	if probe.State != protocol.AgentStateReady && probe.State != protocol.AgentStateDisabled {
		return probe, rev, fmt.Errorf("installed %s, but probe reported state=%s (%s)", pkg, probe.State, probe.Detail)
	}
	return probe, rev, nil
}

// BuildDriverFor constructs an AgyAcpDriver for adp using the currently resolved executable and settings,
// returning the driver along with a full launch signature (bin + args) for change detection.
func (m *AgentManager) BuildDriverFor(adp protocol.Adapter) (*AgyAcpDriver, string, bool) {
	return m.BuildDriverWithEnvFor(adp, nil)
}

// BuildDriverWithEnvFor constructs an AgyAcpDriver with optional per-project environment overrides.
func (m *AgentManager) BuildDriverWithEnvFor(adp protocol.Adapter, projectEnv map[string]string) (*AgyAcpDriver, string, bool) {
	m.mu.RLock()
	hubCfg := m.hubConfigs[string(adp)]
	m.mu.RUnlock()
	if hubCfg.Enabled != nil && !*hubCfg.Enabled {
		return nil, "", false
	}
	binPath, args, _, _ := m.resolveBridgeBinary(adp, hubCfg)
	if binPath == "" {
		return nil, "", false
	}
	sig := binPath + "\x00" + strings.Join(args, "\x00")
	return m.buildDriver(adp, binPath, args, projectEnv), sig, true
}

func (m *AgentManager) buildDriver(adp protocol.Adapter, binPath string, args []string, projectEnv map[string]string) *AgyAcpDriver {
	extraEnv := []string{"PATH=" + m.mergedPATH}
	for k, v := range projectEnv {
		if key := strings.TrimSpace(k); key != "" {
			extraEnv = append(extraEnv, key+"="+v)
		}
	}
	return NewAdapterDriver(adp, AgyDriverConfig{
		Adapter:       adp,
		Bin:           binPath,
		Args:          args,
		RunnerSock:    m.cfg.SocketPath,
		OrbitBinDir:   m.cfg.OrbitBinDir,
		RunnerVersion: m.cfg.RunnerVersion,
		ExtraEnv:      extraEnv,
	})
}

func (m *AgentManager) effectiveMaxWorkers(adp protocol.Adapter, hubCfg protocol.RunnerAgentConfig) int {
	if fc, ok := m.cfg.FileConfigs[adp]; ok && fc.MaxLiveWorkers > 0 {
		return fc.MaxLiveWorkers
	}
	if hubCfg.MaxLiveWorkers > 0 {
		return hubCfg.MaxLiveWorkers
	}
	if adp == protocol.AdapterAgy {
		return 6
	}
	return 4
}

func (m *AgentManager) resolveBridgeBinary(adp protocol.Adapter, hubCfg protocol.RunnerAgentConfig) (resolved string, args []string, source string, explicitMissing string) {
	fileCfg, hasFile := m.cfg.FileConfigs[adp]
	if hasFile && len(fileCfg.DefaultArgs) > 0 {
		args = append([]string(nil), fileCfg.DefaultArgs...)
	} else if len(hubCfg.Args) > 0 {
		args = append([]string(nil), hubCfg.Args...)
	}

	// 1. CLI flag override
	if fb := strings.TrimSpace(m.cfg.FlagBins[adp]); fb != "" {
		if p := m.resolveExecutable(fb); p != "" {
			return p, args, "flag", ""
		}
		return "", args, "flag", fb
	}

	// 2. Local machines.yaml explicit bin
	if hasFile && strings.TrimSpace(fileCfg.Bin) != "" {
		if p := m.resolveExecutable(fileCfg.Bin); p != "" {
			return p, args, "file", ""
		}
		return "", args, "file", fileCfg.Bin
	}

	// 3. Hub dynamic settings override
	if sb := strings.TrimSpace(hubCfg.Bin); sb != "" {
		if p := m.resolveExecutable(sb); p != "" {
			return p, args, "settings", ""
		}
		return "", args, "settings", sb
	}

	// 4. Managed bridge dir (<dataDir>/acp-adapters/node_modules/.bin/<bridge>)
	defBin := defaultBridgeBin(adp)
	managedCand := filepath.Join(m.cfg.DataDir, "acp-adapters", "node_modules", ".bin", defBin)
	if isExecutableFile(managedCand) {
		return managedCand, args, "managed", ""
	}

	// 5. Merged login-shell PATH
	if p := m.findInPATH(defBin); p != "" {
		return p, args, "path", ""
	}

	return "", args, "none", ""
}

func (m *AgentManager) resolveExecutable(raw string) string {
	raw = protocol.ExpandPath(strings.TrimSpace(raw))
	if raw == "" {
		return ""
	}
	if strings.ContainsRune(raw, os.PathSeparator) {
		if isExecutableFile(raw) {
			return raw
		}
		return ""
	}
	return m.findInPATH(raw)
}

func (m *AgentManager) findInPATH(name string) string {
	if name == "" {
		return ""
	}
	for _, dir := range filepath.SplitList(m.mergedPATH) {
		if dir == "" {
			continue
		}
		cand := filepath.Join(dir, name)
		if isExecutableFile(cand) {
			return cand
		}
	}
	return ""
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (m *AgentManager) probeACPCapabilities(ctx context.Context, adp protocol.Adapter, binPath string, args []string, probeSession bool) (string, *cachedAgentCaps, error) {
	timeout := 12 * time.Second
	if probeSession {
		timeout = 22 * time.Second
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	drvArgs := args
	if adp == protocol.AdapterAgy && len(drvArgs) == 0 {
		drvArgs = []string{"acp", "--stdio", "--process-per-session"}
	}
	cmd := exec.Command(binPath, drvArgs...)
	setProcessGroup(cmd)
	cmd.Env = setEnvVar(os.Environ(), "PATH", m.mergedPATH)
	var stderrBuf lockedBuffer
	cmd.Stderr = &stderrBuf

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return "", nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return "", nil, err
	}
	defer func() {
		_ = stdin.Close()
		done := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(1 * time.Second):
			killProcessGroup(cmd)
			<-done
		}
	}()

	conn := acp.NewClientSideConnection(&noopProbeClient{}, stdin, stdout)
	initResp, err := conn.Initialize(pctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{
			Fs:       acp.FileSystemCapabilities{},
			Terminal: false,
		},
		ClientInfo: &acp.Implementation{
			Name:    "hebi8-orbit-probe",
			Version: m.cfg.RunnerVersion,
		},
	})
	if err != nil {
		stderrLine := lastNonEmptyLine(stderrBuf.String())
		if stderrLine != "" {
			return "", nil, fmt.Errorf("%v (%s)", err, stderrLine)
		}
		return "", nil, err
	}
	ver := ""
	if initResp.AgentInfo != nil && strings.TrimSpace(initResp.AgentInfo.Version) != "" {
		ver = strings.TrimSpace(initResp.AgentInfo.Version)
	}

	if !probeSession {
		return ver, nil, nil
	}

	tmpDir, err := os.MkdirTemp("", "orbit-acp-probe-*")
	if err != nil {
		return ver, nil, fmt.Errorf("create probe temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	sessResp, sErr := conn.NewSession(pctx, acp.NewSessionRequest{
		Cwd:        tmpDir,
		McpServers: []acp.McpServer{},
	})
	if sErr != nil {
		if looksLikeAuthError(sErr.Error()) {
			return ver, &cachedAgentCaps{
				authChecked: true,
				needsLogin:  true,
				detail:      "尚未登录，请先在该机器终端完成登录认证",
				cachedAt:    time.Now(),
			}, nil
		}
		stderrLine := lastNonEmptyLine(stderrBuf.String())
		if stderrLine != "" {
			return ver, nil, fmt.Errorf("session/new failed: %v (%s)", sErr, stderrLine)
		}
		return ver, nil, fmt.Errorf("session/new failed: %w", sErr)
	}

	caps := extractSessionCapabilities(sessResp)
	caps.authChecked = true
	caps.cachedAt = time.Now()
	if initResp.AgentCapabilities.SessionCapabilities.Close != nil {
		cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = conn.CloseSession(cctx, acp.CloseSessionRequest{SessionId: sessResp.SessionId})
		ccancel()
	}
	return ver, &caps, nil
}

func (m *AgentManager) readManagedPackageVersion(adp protocol.Adapter) string {
	var pkgDir string
	switch adp {
	case protocol.AdapterClaude:
		pkgDir = filepath.Join(m.cfg.DataDir, "acp-adapters", "node_modules", "@agentclientprotocol", "claude-agent-acp", "package.json")
	case protocol.AdapterCodex:
		pkgDir = filepath.Join(m.cfg.DataDir, "acp-adapters", "node_modules", "@agentclientprotocol", "codex-acp", "package.json")
	default:
		return ""
	}
	raw, err := os.ReadFile(pkgDir)
	if err != nil {
		return ""
	}
	var doc struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(raw, &doc) == nil {
		return strings.TrimSpace(doc.Version)
	}
	return ""
}

func (m *AgentManager) storeProbe(adp protocol.Adapter, p protocol.AgentProbe) {
	m.mu.Lock()
	m.probes[adp] = p
	m.mu.Unlock()
}

func (m *AgentManager) storeProbeIfRev(adp protocol.Adapter, p protocol.AgentProbe, expectedRev int64) {
	m.mu.Lock()
	if m.configRev == expectedRev {
		m.probes[adp] = p
	}
	m.mu.Unlock()
}

func defaultBridgeBin(adp protocol.Adapter) string {
	switch adp {
	case protocol.AdapterClaude:
		return "claude-agent-acp"
	case protocol.AdapterCodex:
		return "codex-acp"
	default:
		return "agy"
	}
}

func defaultMainCLIBin(adp protocol.Adapter) string {
	switch adp {
	case protocol.AdapterClaude:
		return "claude"
	case protocol.AdapterCodex:
		return "codex"
	default:
		return "agy"
	}
}

func isExecutableFile(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return fi.Mode()&0o111 != 0
}

func resolveLoginShellPATH(dataDir, orbitBinDir string) string {
	parts := []string{}
	add := func(dir string) {
		dir = strings.TrimSpace(dir)
		if dir == "" || slices.Contains(parts, dir) {
			return
		}
		parts = append(parts, dir)
	}

	if orbitBinDir != "" {
		add(orbitBinDir)
	}
	if dataDir != "" {
		add(filepath.Join(dataDir, "acp-adapters", "node_modules", ".bin"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		add(filepath.Join(home, ".local", "bin"))
		add(filepath.Join(home, ".local", "share", "fnm", "aliases", "default", "bin"))
	}
	if runtime.GOOS == "darwin" {
		add("/opt/homebrew/bin")
		add("/opt/homebrew/sbin")
	}
	add("/usr/local/bin")

	// Capture user's interactive login shell PATH once (3s timeout) so LaunchAgent/systemd sees nvm/fnm/homebrew.
	if runtime.GOOS != "windows" {
		shell := strings.TrimSpace(os.Getenv("SHELL"))
		if shell == "" {
			shell = "/bin/zsh"
			if !isExecutableFile(shell) {
				shell = "/bin/bash"
			}
		}
		if isExecutableFile(shell) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			out, err := exec.CommandContext(ctx, shell, "-lic", `printf '%s' "$PATH"`).Output()
			cancel()
			if err == nil {
				for _, seg := range filepath.SplitList(strings.TrimSpace(string(out))) {
					add(seg)
				}
			}
		}
	}

	for _, seg := range filepath.SplitList(os.Getenv("PATH")) {
		add(seg)
	}
	return strings.Join(parts, string(os.PathListSeparator))
}

func sanitizeOneLine(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if idx := strings.IndexAny(s, "\r\n"); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	// Redact any embedded URLs (which may contain OAuth state/codes/tokens in query or userinfo)
	fields := strings.Fields(s)
	for i, tok := range fields {
		lower := strings.ToLower(tok)
		if strings.Contains(lower, "http://") || strings.Contains(lower, "https://") {
			fields[i] = "[URL]"
		}
	}
	s = strings.Join(fields, " ")
	// Redact obvious secret-looking tokens defensively
	for _, prefix := range []string{"Bearer ", "sk-", "ort_", "ock_", "ghp_", "gho_", "token=", "code=", "access_token="} {
		if i := strings.Index(s, prefix); i >= 0 {
			s = s[:i] + "[REDACTED]"
		}
	}
	if len(s) > maxLen && maxLen > 3 {
		return s[:maxLen-3] + "..."
	}
	return s
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}

func looksLikeAuthError(msg string) bool {
	low := strings.ToLower(msg)
	for _, kw := range []string{
		"unauthorized", "not logged in", "authentication", "auth_required",
		"please log in", "login required", "api key", "invalid_api_key", "401", "credentials",
	} {
		if strings.Contains(low, kw) {
			return true
		}
	}
	return false
}

func extractSessionCapabilities(resp acp.NewSessionResponse) cachedAgentCaps {
	var out cachedAgentCaps
	appendSelectValues := func(opts acp.SessionConfigSelectOptions) []acp.SessionConfigSelectOption {
		var list []acp.SessionConfigSelectOption
		if opts.Ungrouped != nil {
			list = append(list, (*opts.Ungrouped)...)
		}
		if opts.Grouped != nil {
			for _, g := range *opts.Grouped {
				list = append(list, g.Options...)
			}
		}
		return list
	}

	for _, opt := range resp.ConfigOptions {
		if opt.Select == nil {
			continue
		}
		sel := opt.Select
		id := string(sel.Id)
		cat := ""
		if sel.Category != nil {
			cat = string(*sel.Category)
		}
		items := appendSelectValues(sel.Options)
		switch {
		case id == "model" || cat == "model":
			for _, item := range items {
				out.models = append(out.models, string(item.Value))
			}
			if cur := string(sel.CurrentValue); cur != "" {
				out.defaultModel = cur
			}
		case id == "mode" || cat == "mode":
			for _, item := range items {
				desc := ""
				if item.Description != nil {
					desc = strings.TrimSpace(*item.Description)
				}
				name := strings.TrimSpace(item.Name)
				if name == "" {
					name = string(item.Value)
				}
				out.modes = append(out.modes, protocol.AgentModeOption{
					ID:          string(item.Value),
					Name:        name,
					Description: desc,
				})
			}
			if cur := string(sel.CurrentValue); cur != "" {
				out.defaultMode = cur
			}
		case id == "thought_level" || id == "effort" || id == "reasoning_effort" || cat == "thought_level":
			for _, item := range items {
				out.efforts = append(out.efforts, string(item.Value))
			}
			if cur := string(sel.CurrentValue); cur != "" {
				out.defaultEffort = cur
			}
		}
	}

	if len(out.modes) == 0 && resp.Modes != nil {
		for _, m := range resp.Modes.AvailableModes {
			desc := ""
			if m.Description != nil {
				desc = strings.TrimSpace(*m.Description)
			}
			name := strings.TrimSpace(m.Name)
			if name == "" {
				name = string(m.Id)
			}
			out.modes = append(out.modes, protocol.AgentModeOption{
				ID:          string(m.Id),
				Name:        name,
				Description: desc,
			})
		}
		if len(out.modes) > 0 {
			out.modeViaLegacy = true
		}
		if cur := string(resp.Modes.CurrentModeId); cur != "" {
			out.defaultMode = cur
		}
	}
	return out
}

// noopProbeClient implements minimal acp.Client for lightweight initialize probes.
type noopProbeClient struct{}

func (*noopProbeClient) SessionUpdate(context.Context, acp.SessionNotification) error { return nil }
func (*noopProbeClient) RequestPermission(context.Context, acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
}
func (*noopProbeClient) CompleteElicitation(context.Context, acp.CompleteElicitationNotification) error {
	return nil
}
func (*noopProbeClient) CreateElicitation(context.Context, acp.CreateElicitationRequest) (acp.CreateElicitationResponse, error) {
	return acp.CreateElicitationResponse{}, errors.New("not supported")
}
func (*noopProbeClient) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, errors.New("not supported")
}
func (*noopProbeClient) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, errors.New("not supported")
}
func (*noopProbeClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, errors.New("not supported")
}
func (*noopProbeClient) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, errors.New("not supported")
}
func (*noopProbeClient) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, errors.New("not supported")
}
func (*noopProbeClient) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, errors.New("not supported")
}
func (*noopProbeClient) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, errors.New("not supported")
}
