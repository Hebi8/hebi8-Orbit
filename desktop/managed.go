package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type managedProc struct {
	name string
	cmd  *exec.Cmd
	done chan struct{}
}

// RuntimeManager manages optional local orbit-hub / orbit-runner child processes
// for the desktop shell, always preferring to attach to an already-running service.
type RuntimeManager struct {
	mu    sync.Mutex
	procs []*managedProc
}

func NewRuntimeManager() *RuntimeManager {
	return &RuntimeManager{}
}

// Shutdown terminates all desktop-managed child processes cleanly (never touching external services).
func (m *RuntimeManager) Shutdown() {
	m.mu.Lock()
	procs := m.procs
	m.procs = nil
	m.mu.Unlock()
	for i := len(procs) - 1; i >= 0; i-- {
		p := procs[i]
		log.Printf("orbit-client: stopping managed %s", p.name)
		stopChildProcess(p.cmd, p.done)
	}
}

func (m *RuntimeManager) hasManaged(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.procs {
		if p.name != name {
			continue
		}
		select {
		case <-p.done:
			continue
		default:
			return true
		}
	}
	return false
}

func (m *RuntimeManager) startManaged(name, binPath string, args []string, logPath string) error {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}

	fullArgs := append([]string(nil), args...)
	fullArgs = append(fullArgs, "--parent-pid", strconv.Itoa(os.Getpid()))

	cmd := exec.Command(binPath, fullArgs...)
	setChildProcGroup(cmd)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	// Ensure common user bin directories are on PATH for child discovery
	cmd.Env = buildDesktopChildEnv(filepath.Dir(binPath))

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return err
	}
	_ = logFile.Close()

	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	m.mu.Lock()
	m.procs = append(m.procs, &managedProc{name: name, cmd: cmd, done: done})
	m.mu.Unlock()
	return nil
}

// LocateBinary searches bundled Orbit.app/Contents/Resources/bin, sibling dir, ~/.local/bin, Homebrew, and PATH.
func LocateBinary(name string) (string, error) {
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(name), ".exe") {
		name += ".exe"
	}
	cands := []string{}
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		cands = append(cands,
			filepath.Join(exeDir, "..", "Resources", "bin", name),
			filepath.Join(exeDir, name),
		)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		cands = append(cands, filepath.Join(home, ".local", "bin", name))
	}
	if runtime.GOOS == "darwin" {
		cands = append(cands, "/opt/homebrew/bin/"+name)
	}
	cands = append(cands, "/usr/local/bin/"+name)

	for _, c := range cands {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c, nil
		}
	}
	if found, err := exec.LookPath(name); err == nil {
		return found, nil
	}
	return "", fmt.Errorf("未找到 %s 可执行文件，请重新安装 Orbit", name)
}

func buildDesktopChildEnv(binDir string) []string {
	env := os.Environ()
	parts := []string{}
	add := func(d string) {
		d = strings.TrimSpace(d)
		if d == "" {
			return
		}
		for _, p := range parts {
			if p == d {
				return
			}
		}
		parts = append(parts, d)
	}
	add(binDir)
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		add(filepath.Join(home, ".local", "bin"))
		add(filepath.Join(home, ".local", "share", "fnm", "aliases", "default", "bin"))
	}
	if runtime.GOOS == "darwin" {
		add("/opt/homebrew/bin")
		add("/opt/homebrew/sbin")
	}
	add("/usr/local/bin")
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		add(p)
	}
	newPath := strings.Join(parts, string(os.PathListSeparator))
	replaced := false
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			env[i] = "PATH=" + newPath
			replaced = true
			break
		}
	}
	if !replaced {
		env = append(env, "PATH="+newPath)
	}
	return env
}

// StartOrAttachLocal initializes local Orbit files if needed, attaches to an existing Hub if healthy,
// or starts managed orbit-hub + orbit-runner child processes.
func (m *RuntimeManager) StartOrAttachLocal(ctx context.Context, configDir string) (hubURL, machineID string, attached bool, err error) {
	if configDir == "" {
		configDir = defaultConfigDir()
	}
	expCfgDir := expandHome(configDir)
	if err := os.MkdirAll(expCfgDir, 0o700); err != nil {
		return "", "", false, err
	}

	hubPath := filepath.Join(expCfgDir, "hub.yaml")
	listenAddr := "127.0.0.1:4310"
	dataDir := expandHome("~/.local/share/hebi8-orbit")
	devToken := ""

	if raw, rErr := os.ReadFile(hubPath); rErr == nil {
		var hc hubConfigFallback
		if yaml.Unmarshal(raw, &hc) == nil {
			if strings.TrimSpace(hc.ListenAddr) != "" {
				listenAddr = strings.TrimSpace(hc.ListenAddr)
			}
			if strings.TrimSpace(hc.DataDir) != "" {
				dataDir = expandHome(hc.DataDir)
			}
			devToken = strings.TrimSpace(hc.DevToken)
		}
	}

	hubURL = "http://" + strings.TrimPrefix(strings.TrimPrefix(listenAddr, "http://"), "https://")
	machineID = defaultLocalMachineID(expCfgDir)

	if isOrbitHealthOK(ctx, hubURL) {
		attached = true
	} else {
		if _, statErr := os.Stat(hubPath); os.IsNotExist(statErr) {
			listenAddr, err = findFreeLoopbackAddr(listenAddr)
			if err != nil {
				return "", "", false, err
			}
			hubURL = "http://" + listenAddr
			devToken, err = bootstrapLocalConfig(expCfgDir, dataDir, machineID, listenAddr)
			if err != nil {
				return "", "", false, err
			}
		} else {
			if err := ensureLocalRunnerTokenAndMachine(expCfgDir, machineID); err != nil {
				return "", "", false, err
			}
		}

		if !m.hasManaged("orbit-hub") {
			hubBin, bErr := LocateBinary("orbit-hub")
			if bErr != nil {
				return "", "", false, bErr
			}
			if err := m.startManaged("orbit-hub", hubBin, []string{"--config-dir", expCfgDir}, filepath.Join(dataDir, "logs", "hub.log")); err != nil {
				return "", "", false, fmt.Errorf("启动本机服务失败: %w", err)
			}
		}
		if !waitForHealth(ctx, hubURL, 10*time.Second) {
			return "", "", false, fmt.Errorf("本机服务启动超时，请检查日志 %s", filepath.Join(dataDir, "logs", "hub.log"))
		}
	}

	// Start local runner if not already managed and no local socket is active
	if err := m.ensureLocalRunner(expCfgDir, dataDir, machineID); err != nil {
		log.Printf("orbit-client: warning starting local runner: %v", err)
	}

	// Persist client.yaml in local mode
	_ = SaveClientConfig(expCfgDir, ClientConfig{
		HubURL:         hubURL,
		DevToken:       devToken,
		MachineID:      machineID,
		Mode:           "local",
		RunLocalRunner: true,
		PAShortcut:     "CmdOrCtrl+Shift+O",
	})
	return hubURL, machineID, attached, nil
}

func (m *RuntimeManager) ensureLocalRunner(expCfgDir, dataDir, machineID string) error {
	if m.hasManaged("orbit-runner") || isSocketAlive(filepath.Join(dataDir, "runner-"+machineID+".sock")) {
		return nil
	}
	runnerBin, bErr := LocateBinary("orbit-runner")
	if bErr != nil {
		return bErr
	}
	return m.startManaged("orbit-runner", runnerBin, []string{"--machine-id", machineID, "--config-dir", expCfgDir}, filepath.Join(dataDir, "logs", "runner.log"))
}

// EnsureRemoteRunner starts a managed orbit-runner for a previously paired remote desktop
// if runLocalRunner is enabled and runner.yaml + runner.token are present.
func (m *RuntimeManager) EnsureRemoteRunner(configDir string) error {
	if configDir == "" {
		configDir = defaultConfigDir()
	}
	expCfgDir := expandHome(configDir)
	raw, err := os.ReadFile(filepath.Join(expCfgDir, "runner.yaml"))
	if err != nil {
		return err
	}
	var rc struct {
		MachineID string `yaml:"machineId"`
		DataDir   string `yaml:"dataDir"`
	}
	if err := yaml.Unmarshal(raw, &rc); err != nil {
		return err
	}
	dataDir := expandHome(rc.DataDir)
	if dataDir == "" {
		dataDir = expandHome("~/.local/share/hebi8-orbit")
	}
	if m.hasManaged("orbit-runner") || (rc.MachineID != "" && isSocketAlive(filepath.Join(dataDir, "runner-"+rc.MachineID+".sock"))) {
		return nil
	}
	runnerBin, bErr := LocateBinary("orbit-runner")
	if bErr != nil {
		return bErr
	}
	return m.startManaged("orbit-runner", runnerBin, []string{"--config-dir", expCfgDir}, filepath.Join(dataDir, "logs", "runner.log"))
}

// JoinResult separates client connection success from optional local runner startup status.
type JoinResult struct {
	HubURL          string
	MachineID       string
	ClientConnected bool
	RunnerStarted   bool
	RunnerError     string
}

// JoinRemote connects this desktop app (and optionally registers this machine as an execution node)
// to an existing Orbit Hub via pairing code (or plain URL only when allowCodeFree is true).
func (m *RuntimeManager) JoinRemote(ctx context.Context, configDir, rawInput string, asRunner bool) (JoinResult, error) {
	return m.joinRemote(ctx, configDir, rawInput, asRunner, false)
}

func (m *RuntimeManager) ConnectDirect(ctx context.Context, configDir, rawInput string) (JoinResult, error) {
	return m.joinRemote(ctx, configDir, rawInput, false, true)
}

func (m *RuntimeManager) joinRemote(ctx context.Context, configDir, rawInput string, asRunner, allowCodeFree bool) (JoinResult, error) {
	if configDir == "" {
		configDir = defaultConfigDir()
	}
	expCfgDir := expandHome(configDir)
	if err := os.MkdirAll(expCfgDir, 0o700); err != nil {
		return JoinResult{}, err
	}

	hubURL, code, noRunnerHint, runnerOnlyHint, err := ParseJoinInputWithScope(rawInput)
	if err != nil {
		return JoinResult{}, err
	}
	if noRunnerHint {
		asRunner = false
	}
	if runnerOnlyHint {
		return JoinResult{}, fmt.Errorf("该邀请命令仅限终端执行节点（--runner-only）")
	}

	if code == "" {
		if !allowCodeFree {
			return JoinResult{}, fmt.Errorf("请输入配对码或完整的邀请命令")
		}
		if !isOrbitHealthOK(ctx, hubURL) {
			return JoinResult{}, fmt.Errorf("无法连接到 %s", hubURL)
		}
		if err := SaveClientConfig(expCfgDir, ClientConfig{
			HubURL:         hubURL,
			Mode:           "remote",
			RunLocalRunner: false,
			PAShortcut:     "CmdOrCtrl+Shift+O",
		}); err != nil {
			return JoinResult{}, err
		}
		return JoinResult{HubURL: hubURL, ClientConnected: true}, nil
	}

	// Pairing code claim flow (server-to-Hub, no Origin header)
	hostname, _ := os.Hostname()
	proposed := sanitizeLocalID(strings.Split(hostname, ".")[0])
	if proposed == "" {
		proposed = "desktop-node"
	}
	payload := map[string]any{
		"code":       code,
		"wantRunner": asRunner,
		"wantClient": true,
		"machine": map[string]any{
			"proposedId":    proposed,
			"displayName":   proposed,
			"os":            runtime.GOOS,
			"arch":          runtime.GOARCH,
			"hostname":      hostname,
			"runnerVersion": Version,
		},
	}
	rawBody, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hubURL+"/api/pairing/claim", bytes.NewReader(rawBody))
	if err != nil {
		return JoinResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Orbit-Request", "1")

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return JoinResult{}, fmt.Errorf("无法连接到 %s: %w", hubURL, err)
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(respBytes, &apiErr) == nil && apiErr.Message != "" {
			return JoinResult{}, fmt.Errorf("%s", apiErr.Message)
		}
		return JoinResult{}, fmt.Errorf("配对失败 (HTTP %d)", resp.StatusCode)
	}

	var claimed struct {
		MachineID   string `json:"machineId"`
		HubOrigin   string `json:"hubOrigin"`
		WSURL       string `json:"wsUrl"`
		RunnerToken string `json:"runnerToken"`
		ClientToken string `json:"clientToken"`
	}
	if err := json.Unmarshal(respBytes, &claimed); err != nil {
		return JoinResult{}, err
	}

	runRunner := asRunner && claimed.RunnerToken != "" && claimed.MachineID != ""
	if err := SaveClientConfig(expCfgDir, ClientConfig{
		HubURL:         hubURL,
		ClientToken:    claimed.ClientToken,
		MachineID:      claimed.MachineID,
		Mode:           "remote",
		RunLocalRunner: runRunner,
		PAShortcut:     "CmdOrCtrl+Shift+O",
	}); err != nil {
		return JoinResult{}, err
	}

	res := JoinResult{
		HubURL:          hubURL,
		MachineID:       claimed.MachineID,
		ClientConnected: true,
	}

	if runRunner {
		dataDir := expandHome("~/.local/share/hebi8-orbit")
		if err := os.WriteFile(filepath.Join(expCfgDir, "runner.token"), []byte(claimed.RunnerToken+"\n"), 0o600); err != nil {
			res.RunnerError = fmt.Sprintf("写入执行节点凭据失败: %v", err)
			return res, nil
		}
		wsURL := httpToWS(hubURL) + "/ws/runner"
		runnerDoc := map[string]any{
			"schemaVersion": 1,
			"machineId":     claimed.MachineID,
			"hubUrl":        hubURL,
			"hubWsUrl":      wsURL,
			"dataDir":       dataDir,
		}
		rawYAML, yErr := yaml.Marshal(runnerDoc)
		if yErr != nil {
			res.RunnerError = fmt.Sprintf("生成执行节点配置失败: %v", yErr)
			return res, nil
		}
		if wErr := os.WriteFile(filepath.Join(expCfgDir, "runner.yaml"), rawYAML, 0o600); wErr != nil {
			res.RunnerError = fmt.Sprintf("写入 runner.yaml 失败: %v", wErr)
			return res, nil
		}
		runnerBin, bErr := LocateBinary("orbit-runner")
		if bErr != nil {
			res.RunnerError = bErr.Error()
			return res, nil
		}
		if sErr := m.startManaged("orbit-runner", runnerBin, []string{"--config-dir", expCfgDir}, filepath.Join(dataDir, "logs", "runner.log")); sErr != nil {
			res.RunnerError = fmt.Sprintf("启动本机执行节点失败: %v", sErr)
			return res, nil
		}
		// Wait briefly for Hub to actually observe this runner online (or detect immediate child exit) (#16).
		if m.waitForRunnerOnline(ctx, hubURL, claimed.ClientToken, claimed.MachineID, 5*time.Second) {
			res.RunnerStarted = true
		} else if !m.hasManaged("orbit-runner") {
			res.RunnerError = "执行节点启动后已退出，请检查日志 " + filepath.Join(dataDir, "logs", "runner.log")
		} else {
			// Process is still alive and connecting; avoid claiming online prematurely
			res.RunnerStarted = false
		}
	}

	return res, nil
}

func (m *RuntimeManager) waitForRunnerOnline(ctx context.Context, hubURL, clientToken, machineID string, timeout time.Duration) bool {
	if hubURL == "" || clientToken == "" || machineID == "" {
		return false
	}
	client := originBoundHTTPClient(hubURL, 2*time.Second)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !m.hasManaged("orbit-runner") {
			return false
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(hubURL, "/")+"/api/machines", nil)
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+clientToken)
			if resp, rErr := client.Do(req); rErr == nil {
				var body struct {
					Machines []struct {
						MachineID string `json:"machineId"`
						Status    string `json:"status"`
					} `json:"machines"`
				}
				_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<18)).Decode(&body)
				_ = resp.Body.Close()
				for _, mach := range body.Machines {
					if mach.MachineID == machineID && mach.Status == "online" {
						return true
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(350 * time.Millisecond):
		}
	}
	return false
}

func originBoundHTTPClient(expectedOrigin string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			if !sameOrigin(req.URL, expectedOrigin) {
				return fmt.Errorf("refusing cross-origin redirect to %s", req.URL.Host)
			}
			return nil
		},
	}
}

// BuildOpenURL exchanges stored clientToken / devToken for a 30s single-use fragment handoff URL
// so the WebView or browser signs in automatically without ever asking for a token.
// Credentials are strictly origin-bound and never sent to a different Hub origin.
func (m *RuntimeManager) BuildOpenURL(ctx context.Context, configDir, hubURL, fallbackHash string) string {
	hubURL = strings.TrimRight(strings.TrimSpace(hubURL), "/")
	if hubURL == "" {
		return ""
	}
	cfg, _ := LoadClientConfig(configDir, hubURL)
	if cfg.ClientToken == "" && cfg.DevToken == "" {
		return hubURL + "/" + fallbackHash
	}
	if cfg.HubURL != "" && !sameOriginStrings(hubURL, cfg.HubURL) {
		return hubURL + "/" + fallbackHash
	}

	hctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(hctx, http.MethodPost, hubURL+"/api/auth/handoff", bytes.NewReader([]byte("{}")))
	if err != nil {
		return hubURL + "/" + fallbackHash
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Orbit-Request", "1")
	if cfg.ClientToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.ClientToken)
	} else if cfg.DevToken != "" {
		req.Header.Set("X-Orbit-Dev-Token", cfg.DevToken)
	}
	resp, err := originBoundHTTPClient(hubURL, 4*time.Second).Do(req)
	if err != nil {
		return hubURL + "/" + fallbackHash
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return hubURL + "/" + fallbackHash
	}
	var out struct {
		Code string `json:"code"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil || out.Code == "" {
		return hubURL + "/" + fallbackHash
	}
	return hubURL + "/#/auth/" + url.PathEscape(out.Code)
}

func isOrbitHealthOK(ctx context.Context, hubURL string) bool {
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	base := strings.TrimRight(hubURL, "/")
	client := originBoundHTTPClient(base, 2*time.Second)
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, base+"/api/health", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var doc map[string]any
	if json.NewDecoder(resp.Body).Decode(&doc) != nil {
		return false
	}
	ok, _ := doc["ok"].(bool)
	if !ok {
		return false
	}
	if svc, _ := doc["service"].(string); svc == "hebi8-orbit-hub" {
		return true
	}
	// Compatibility probe for pre-v0.3 Orbit Hubs (which returned {ok:true, version:...} without service):
	// verify /api/bootstrap responds with Orbit's JSON contract (401 ERR_UNAUTHORIZED or 200 with machines).
	if ver, _ := doc["version"].(string); strings.TrimSpace(ver) == "" {
		return false
	}
	bReq, err := http.NewRequestWithContext(pctx, http.MethodGet, base+"/api/bootstrap", nil)
	if err != nil {
		return false
	}
	bResp, err := client.Do(bReq)
	if err != nil {
		return false
	}
	defer bResp.Body.Close()
	var bDoc map[string]any
	if json.NewDecoder(io.LimitReader(bResp.Body, 1<<16)).Decode(&bDoc) != nil {
		return false
	}
	if bResp.StatusCode == http.StatusUnauthorized {
		code, _ := bDoc["code"].(string)
		return code == "ERR_UNAUTHORIZED"
	}
	if bResp.StatusCode == http.StatusOK {
		_, hasMachines := bDoc["machines"]
		return hasMachines
	}
	return false
}

func waitForHealth(ctx context.Context, hubURL string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if isOrbitHealthOK(ctx, hubURL) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(200 * time.Millisecond):
		}
	}
	return false
}

func findFreeLoopbackAddr(preferred string) (string, error) {
	host, portStr, err := net.SplitHostPort(preferred)
	if err != nil {
		host = "127.0.0.1"
		portStr = "4310"
	}
	basePort := 4310
	_, _ = fmt.Sscanf(portStr, "%d", &basePort)
	for p := basePort; p < basePort+25; p++ {
		addr := fmt.Sprintf("%s:%d", host, p)
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			_ = ln.Close()
			return addr, nil
		}
	}
	return "", fmt.Errorf("本机端口 %d 附近均被占用，请释放端口后重试", basePort)
}

func isSocketAlive(sockPath string) bool {
	conn, err := net.DialTimeout("unix", sockPath, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func httpToWS(u string) string {
	if strings.HasPrefix(u, "https://") {
		return "wss://" + strings.TrimPrefix(u, "https://")
	}
	return "ws://" + strings.TrimPrefix(u, "http://")
}

func defaultLocalMachineID(cfgDir string) string {
	if raw, err := os.ReadFile(filepath.Join(cfgDir, "machines.yaml")); err == nil {
		var m struct {
			Machines map[string]struct {
				Transport string `yaml:"transport"`
			} `yaml:"machines"`
		}
		if yaml.Unmarshal(raw, &m) == nil {
			for id, info := range m.Machines {
				if info.Transport == "local_ws" {
					return id
				}
			}
		}
	}
	h, _ := os.Hostname()
	id := sanitizeLocalID(strings.Split(h, ".")[0])
	if id == "" {
		return "local-node"
	}
	return id
}

func sanitizeLocalID(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '.':
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func bootstrapLocalConfig(cfgDir, dataDir, machineID, listenAddr string) (string, error) {
	devToken, err := randomHex(24)
	if err != nil {
		return "", err
	}
	origin := "http://" + listenAddr
	hubDoc := map[string]any{
		"schemaVersion":            1,
		"workspaceName":            "Home LAN",
		"listenAddr":               listenAddr,
		"dataDir":                  dataDir,
		"configDir":                cfgDir,
		"hubOrigin":                origin,
		"devToken":                 devToken,
		"ryugiBin":                 "ryugi",
		"ryugiPort":                27319,
		"ryugiPollIntervalSeconds": 6,
	}
	rawHub, _ := yaml.Marshal(hubDoc)
	if err := os.WriteFile(filepath.Join(cfgDir, "hub.yaml"), rawHub, 0o600); err != nil {
		return "", err
	}
	if err := ensureLocalRunnerTokenAndMachine(cfgDir, machineID); err != nil {
		return "", err
	}
	projPath := filepath.Join(cfgDir, "projects.yaml")
	if _, err := os.Stat(projPath); os.IsNotExist(err) {
		rawProj := []byte("schemaVersion: 1\nprojects: {}\n")
		if err := os.WriteFile(projPath, rawProj, 0o644); err != nil {
			return "", err
		}
	}
	return devToken, nil
}

// ensureLocalRunnerTokenAndMachine ensures local runner.token and machines.yaml are mutually consistent
// without ever overwriting existing multi-machine definitions or agent configs in machines.yaml (#1, #15).
func ensureLocalRunnerTokenAndMachine(cfgDir, machineID string) error {
	machPath := filepath.Join(cfgDir, "machines.yaml")
	tokPath := filepath.Join(cfgDir, "runner.token")

	rawMachBytes, machReadErr := os.ReadFile(machPath)
	if machReadErr != nil && !os.IsNotExist(machReadErr) {
		return fmt.Errorf("read machines.yaml: %w", machReadErr)
	}
	machExists := machReadErr == nil

	rawTokBytes, tokReadErr := os.ReadFile(tokPath)
	if tokReadErr != nil && !os.IsNotExist(tokReadErr) {
		return fmt.Errorf("read runner.token: %w", tokReadErr)
	}
	existingTok := strings.TrimSpace(string(rawTokBytes))
	tokExists := tokReadErr == nil && existingTok != ""

	// Preserve existing runner.token if present; otherwise mint a fresh one.
	runnerToken := existingTok
	if runnerToken == "" {
		var err error
		runnerToken, err = randomHex(24)
		if err != nil {
			return err
		}
	}
	sum := sha256.Sum256([]byte(runnerToken))
	tokenHash := "sha256:" + hex.EncodeToString(sum[:])

	caps := []string{"primary-dev", "linux-" + runtime.GOARCH}
	if runtime.GOOS == "darwin" {
		caps = []string{"macos-env", "primary-dev"}
	}

	var doc map[string]any
	if machExists {
		if err := yaml.Unmarshal(rawMachBytes, &doc); err != nil {
			return fmt.Errorf("parse existing machines.yaml: %w", err)
		}
	}
	if doc == nil {
		doc = map[string]any{"schemaVersion": 1}
	}
	machinesMap, ok := doc["machines"].(map[string]any)
	if !ok || machinesMap == nil {
		machinesMap = map[string]any{}
		doc["machines"] = machinesMap
	}

	yamlDirty := !machExists
	if existingNode, ok := machinesMap[machineID].(map[string]any); ok && existingNode != nil {
		curHash, _ := existingNode["tokenHash"].(string)
		if strings.TrimSpace(curHash) != tokenHash {
			existingNode["tokenHash"] = tokenHash
			machinesMap[machineID] = existingNode
			yamlDirty = true
		}
	} else {
		machinesMap[machineID] = map[string]any{
			"displayName":  machineID,
			"transport":    "local_ws",
			"tokenHash":    tokenHash,
			"capabilities": caps,
		}
		yamlDirty = true
	}

	if yamlDirty {
		rawMach, err := yaml.Marshal(doc)
		if err != nil {
			return err
		}
		if err := os.WriteFile(machPath, rawMach, 0o644); err != nil {
			return err
		}
	}
	if !tokExists {
		if err := os.WriteFile(tokPath, []byte(runnerToken+"\n"), 0o600); err != nil {
			return err
		}
	}
	return nil
}
