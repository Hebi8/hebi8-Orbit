package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

func runInit(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)

	connectURL := fs.String("connect", "", "Connect this machine as a client to an existing Hub URL (e.g. https://orbit.example.ts.net)")
	initHub := fs.Bool("hub", false, "Initialize a new Orbit Hub + local Runner configuration on this machine")
	initRunner := fs.Bool("runner", false, "Initialize this machine as a remote Runner node for an existing Hub")
	initProject := fs.Bool("project", false, "Register a local Git repository as an Orbit project")

	configDirFlag := fs.String("config-dir", "", "Config directory (default ~/.config/hebi8-orbit)")
	dataDirFlag := fs.String("data-dir", "", "Data directory (default ~/.local/share/hebi8-orbit)")
	hubFlag := fs.String("hub-url", "", "Hub URL (used with --runner or --connect)")
	tokenFlag := fs.String("token", "", "Optional token (devToken for --connect, runner token for --runner)")
	machineFlag := fs.String("machine", "", "Machine ID (default: sanitized local hostname)")
	workspaceFlag := fs.String("workspace", "Home LAN", "Workspace display name (for --hub)")
	listenFlag := fs.String("listen", "127.0.0.1:4310", "Hub listen address (for --hub)")
	originFlag := fs.String("origin", "", "Public Hub origin URL (e.g. https://orbit.example.ts.net)")
	projectIDFlag := fs.String("id", "", "Project ID override (for --project)")
	projectNameFlag := fs.String("name", "", "Project display name override (for --project)")
	adapterFlag := fs.String("adapter", "", "Default adapter override: agy|claude|codex")
	force := fs.Bool("force", false, "Overwrite existing config files if present")

	if err := fs.Parse(args); err != nil {
		return err
	}

	cfgDir := strings.TrimSpace(*configDirFlag)
	if cfgDir == "" {
		cfgDir = strings.TrimSpace(os.Getenv("ORBIT_CONFIG_DIR"))
	}
	if cfgDir == "" {
		cfgDir = "~/.config/hebi8-orbit"
	}
	cfgDirAbs := protocol.ExpandPath(cfgDir)

	dataDir := strings.TrimSpace(*dataDirFlag)
	if dataDir == "" {
		dataDir = strings.TrimSpace(os.Getenv("ORBIT_DATA_DIR"))
	}
	if dataDir == "" {
		dataDir = "~/.local/share/hebi8-orbit"
	}

	// Explicit non-interactive modes
	if strings.TrimSpace(*connectURL) != "" {
		return initConnectMode(ctx, cfgDirAbs, *connectURL, *tokenFlag, *force, stdout)
	}
	if *initHub {
		return initHubMode(cfgDirAbs, dataDir, *machineFlag, *workspaceFlag, *listenFlag, *originFlag, *force, stdout)
	}
	if *initRunner {
		u := strings.TrimSpace(*hubFlag)
		if u == "" {
			return fmt.Errorf("--runner requires --hub-url <https://...>")
		}
		return initRunnerMode(ctx, cfgDirAbs, u, *machineFlag, *tokenFlag, *force, stdout)
	}
	if *initProject {
		dir := "."
		if fs.NArg() > 0 {
			dir = fs.Arg(0)
		}
		return initProjectMode(cfgDirAbs, dataDir, dir, *machineFlag, *projectIDFlag, *projectNameFlag, *adapterFlag, stdout)
	}

	// Interactive guided wizard
	if stdin == nil {
		stdin = os.Stdin
	}
	reader := bufio.NewReader(stdin)

	fmt.Fprintf(stdout, "hebi8-Orbit Init (%s)\n", Version)
	fmt.Fprintf(stdout, "Config directory: %s\n\n", cfgDirAbs)
	fmt.Fprintln(stdout, "Choose how to set up this machine:")
	fmt.Fprintln(stdout, "  [1] Connect to an existing Orbit Hub (CLI + Desktop App)")
	fmt.Fprintln(stdout, "  [2] Initialize a new Orbit Hub + local Runner on this machine")
	fmt.Fprintln(stdout, "  [3] Join an existing Orbit Hub as a Runner node (remote worker)")
	fmt.Fprintln(stdout, "  [4] Register current Git repository as an Orbit project")

	choice, err := promptLine(reader, stdout, "\nSelect [1-4]", "1")
	if err != nil {
		return err
	}

	switch strings.TrimSpace(choice) {
	case "1", "connect":
		defaultURL := existingClientHubURL(cfgDirAbs)
		if defaultURL == "" {
			defaultURL = "http://127.0.0.1:4310"
		}
		hubURL, err := promptLine(reader, stdout, "Hub URL (e.g. https://orbit.example.ts.net)", defaultURL)
		if err != nil {
			return err
		}
		return initConnectMode(ctx, cfgDirAbs, hubURL, *tokenFlag, true, stdout)

	case "2", "hub":
		defMachine := defaultMachineID(*machineFlag)
		machineID, err := promptLine(reader, stdout, "Local machine ID", defMachine)
		if err != nil {
			return err
		}
		wsName, err := promptLine(reader, stdout, "Workspace display name", *workspaceFlag)
		if err != nil {
			return err
		}
		listenAddr, err := promptLine(reader, stdout, "Hub listen address", *listenFlag)
		if err != nil {
			return err
		}
		origin, err := promptLine(reader, stdout, "Public Hub origin (blank = http://"+listenAddr+")", *originFlag)
		if err != nil {
			return err
		}
		return initHubMode(cfgDirAbs, dataDir, machineID, wsName, listenAddr, origin, *force, stdout)

	case "3", "runner":
		defaultURL := existingClientHubURL(cfgDirAbs)
		hubURL, err := promptLine(reader, stdout, "Hub URL (e.g. https://orbit.example.ts.net)", defaultURL)
		if err != nil {
			return err
		}
		if strings.TrimSpace(hubURL) == "" {
			return fmt.Errorf("hub URL is required")
		}
		machineID, err := promptLine(reader, stdout, "Machine ID for this node", defaultMachineID(*machineFlag))
		if err != nil {
			return err
		}
		tok, err := promptLine(reader, stdout, "Runner token (leave blank to generate a new one)", *tokenFlag)
		if err != nil {
			return err
		}
		return initRunnerMode(ctx, cfgDirAbs, hubURL, machineID, tok, *force, stdout)

	case "4", "project":
		cwd, _ := os.Getwd()
		dir, err := promptLine(reader, stdout, "Repository path", cwd)
		if err != nil {
			return err
		}
		return initProjectMode(cfgDirAbs, dataDir, dir, *machineFlag, *projectIDFlag, *projectNameFlag, *adapterFlag, stdout)

	default:
		return fmt.Errorf("invalid choice %q (expected 1-4)", choice)
	}
}

func initConnectMode(ctx context.Context, cfgDirAbs, rawURL, devToken string, force bool, stdout io.Writer) error {
	normURL, err := normalizeHubBaseURL(rawURL)
	if err != nil {
		return err
	}

	healthStatus, healthErr := probeHubHealth(ctx, normURL, devToken)
	if healthErr != nil {
		fmt.Fprintf(stdout, "  [WARN] Could not reach %s/api/health (%v)\n", normURL, healthErr)
		fmt.Fprintln(stdout, "         Saving client.yaml anyway so you can retry once the Hub is reachable.")
	} else {
		fmt.Fprintf(stdout, "  [OK  ] Connected to Hub %s (%s)\n", normURL, healthStatus)
	}

	clientPath := filepath.Join(cfgDirAbs, "client.yaml")
	if !force {
		if _, err := os.Stat(clientPath); err == nil {
			return fmt.Errorf("%s already exists (re-run with --force to overwrite)", clientPath)
		}
	}

	if err := writeClientYAML(clientPath, normURL, devToken); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "  [OK  ] Saved %s\n", clientPath)

	adapters := detectLocalAdapters()
	if len(adapters) > 0 {
		names := sortedAdapterNames(adapters)
		fmt.Fprintf(stdout, "  [INFO] Local ACP adapters detected: %s\n", strings.Join(names, ", "))
	}

	fmt.Fprintln(stdout, "\nNext steps:")
	fmt.Fprintln(stdout, "  orbit status                  # Inspect inbox, projects, and online machines")
	fmt.Fprintln(stdout, "  orbit doctor                  # Run full local environment check")
	fmt.Fprintln(stdout, "  orbit open                    # Open the web workbench")
	fmt.Fprintln(stdout, "  orbit tasks list              # View active tasks")
	return nil
}

func initHubMode(cfgDirAbs, dataDir, machineFlag, workspaceName, listenAddr, originFlag string, force bool, stdout io.Writer) error {
	if err := os.MkdirAll(cfgDirAbs, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	dataDirAbs := protocol.ExpandPath(dataDir)
	if err := os.MkdirAll(dataDirAbs, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	hubPath := filepath.Join(cfgDirAbs, "hub.yaml")
	machinesPath := filepath.Join(cfgDirAbs, "machines.yaml")
	projectsPath := filepath.Join(cfgDirAbs, "projects.yaml")
	tokenPath := filepath.Join(cfgDirAbs, "runner.token")
	clientPath := filepath.Join(cfgDirAbs, "client.yaml")

	if !force {
		for _, p := range []string{hubPath, machinesPath, projectsPath} {
			if _, err := os.Stat(p); err == nil {
				return fmt.Errorf("%s already exists (re-run with --force to overwrite)", p)
			}
		}
	}

	machineID := defaultMachineID(machineFlag)
	devToken, err := randomHexToken(24)
	if err != nil {
		return err
	}
	runnerToken, err := randomHexToken(24)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(runnerToken))
	tokenHash := "sha256:" + hex.EncodeToString(sum[:])

	origin := strings.TrimSpace(originFlag)
	if origin == "" {
		if strings.HasPrefix(listenAddr, "http://") || strings.HasPrefix(listenAddr, "https://") {
			origin = strings.TrimRight(listenAddr, "/")
		} else {
			origin = "http://" + listenAddr
		}
	} else {
		norm, err := normalizeHubBaseURL(origin)
		if err != nil {
			return err
		}
		origin = norm
	}

	hubCfg := protocol.DefaultHubConfig()
	hubCfg.WorkspaceName = strings.TrimSpace(workspaceName)
	if hubCfg.WorkspaceName == "" {
		hubCfg.WorkspaceName = "Home LAN"
	}
	hubCfg.ListenAddr = listenAddr
	hubCfg.DataDir = dataDir
	hubCfg.ConfigDir = cfgDirAbs
	hubCfg.HubOrigin = origin
	hubCfg.DevToken = devToken
	if ryugiPath, err := exec.LookPath("ryugi"); err == nil {
		hubCfg.RyugiBin = ryugiPath
	}

	adapters := detectLocalAdapters()
	caps := defaultMachineCapabilities()
	machinesCfg := protocol.MachinesConfig{
		SchemaVersion: 1,
		Machines: map[string]protocol.MachineConfig{
			machineID: {
				DisplayName:  machineID,
				Transport:    protocol.TransportLocalWS,
				TokenHash:    tokenHash,
				Capabilities: caps,
				Adapters:     adapters,
			},
		},
	}

	projectsCfg := protocol.ProjectsConfig{
		SchemaVersion: 1,
		Projects:      map[string]protocol.ProjectConfig{},
	}
	if cwd, err := os.Getwd(); err == nil {
		if projID, projCfg, ok := detectGitProject(cwd, dataDirAbs, machineID, pickPreferredAdapter(adapters), ""); ok {
			projectsCfg.Projects[projID] = projCfg
		}
	}

	if err := writeYAMLFile(hubPath, hubCfg, 0o600); err != nil {
		return err
	}
	if err := writeYAMLFile(machinesPath, machinesCfg, 0o644); err != nil {
		return err
	}
	if err := writeYAMLFile(projectsPath, projectsCfg, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(tokenPath, []byte(runnerToken+"\n"), 0o600); err != nil {
		return fmt.Errorf("write runner.token: %w", err)
	}
	if err := writeClientYAML(clientPath, "http://"+strings.TrimPrefix(strings.TrimPrefix(listenAddr, "http://"), "https://"), devToken); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "Initialized Orbit Hub in %s\n\n", cfgDirAbs)
	fmt.Fprintf(stdout, "  [OK  ] hub.yaml       listen=%s workspace=%q\n", listenAddr, hubCfg.WorkspaceName)
	fmt.Fprintf(stdout, "  [OK  ] machines.yaml  machine=%s adapters=[%s]\n", machineID, strings.Join(sortedAdapterNames(adapters), ", "))
	fmt.Fprintf(stdout, "  [OK  ] projects.yaml  projects=%d\n", len(projectsCfg.Projects))
	fmt.Fprintf(stdout, "  [OK  ] runner.token   (0600)\n")
	fmt.Fprintf(stdout, "  [OK  ] client.yaml    -> http://%s\n", listenAddr)
	if len(adapters) == 0 {
		fmt.Fprintln(stdout, "\n  [WARN] No ACP adapters (agy / claude-agent-acp / codex-acp) were found in PATH.")
		fmt.Fprintln(stdout, "         Install at least one adapter and re-run `orbit init --hub --force` or edit machines.yaml.")
	}

	fmt.Fprintln(stdout, "\nStart Hub and Runner:")
	fmt.Fprintf(stdout, "  orbit-hub --config-dir %s\n", cfgDirAbs)
	fmt.Fprintf(stdout, "  orbit-runner --machine-id %s --config-dir %s\n", machineID, cfgDirAbs)
	fmt.Fprintln(stdout, "\nThen verify:")
	fmt.Fprintln(stdout, "  orbit doctor && orbit status")
	return nil
}

func initRunnerMode(ctx context.Context, cfgDirAbs, rawHubURL, machineFlag, tokenFlag string, force bool, stdout io.Writer) error {
	hubURL, err := normalizeHubBaseURL(rawHubURL)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfgDirAbs, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	machineID := defaultMachineID(machineFlag)
	token := strings.TrimSpace(tokenFlag)
	generatedToken := false
	if token == "" {
		token, err = randomHexToken(24)
		if err != nil {
			return err
		}
		generatedToken = true
	}
	sum := sha256.Sum256([]byte(token))
	tokenHash := "sha256:" + hex.EncodeToString(sum[:])

	tokenPath := filepath.Join(cfgDirAbs, "runner.token")
	machinesPath := filepath.Join(cfgDirAbs, "machines.yaml")
	clientPath := filepath.Join(cfgDirAbs, "client.yaml")

	if !force {
		if _, err := os.Stat(tokenPath); err == nil {
			return fmt.Errorf("%s already exists (re-run with --force to overwrite)", tokenPath)
		}
	}

	if status, err := probeHubHealth(ctx, hubURL, ""); err != nil {
		fmt.Fprintf(stdout, "  [WARN] Could not reach %s/api/health (%v)\n", hubURL, err)
	} else {
		fmt.Fprintf(stdout, "  [OK  ] Reachable Hub %s (%s)\n", hubURL, status)
	}

	adapters := detectLocalAdapters()
	machinesCfg := protocol.MachinesConfig{
		SchemaVersion: 1,
		Machines: map[string]protocol.MachineConfig{
			machineID: {
				DisplayName:  machineID,
				Transport:    protocol.TransportRemoteWS,
				TokenHash:    tokenHash,
				Capabilities: defaultMachineCapabilities(),
				Adapters:     adapters,
			},
		},
	}

	if err := os.WriteFile(tokenPath, []byte(token+"\n"), 0o600); err != nil {
		return fmt.Errorf("write runner.token: %w", err)
	}
	if err := writeYAMLFile(machinesPath, machinesCfg, 0o644); err != nil {
		return err
	}
	if err := writeClientYAML(clientPath, hubURL, ""); err != nil {
		return err
	}

	wsURL := httpToWSURL(hubURL) + "/ws/runner"
	fmt.Fprintf(stdout, "\nInitialized remote Runner node %q in %s\n", machineID, cfgDirAbs)
	fmt.Fprintf(stdout, "  [OK  ] client.yaml    -> %s\n", hubURL)
	fmt.Fprintf(stdout, "  [OK  ] runner.token   (0600)\n")
	fmt.Fprintf(stdout, "  [OK  ] machines.yaml  adapters=[%s]\n", strings.Join(sortedAdapterNames(adapters), ", "))

	if generatedToken {
		fmt.Fprintf(stdout, "\n1. On the Hub host, add this tokenHash under machines.%s in ~/.config/hebi8-orbit/machines.yaml (then restart orbit-hub):\n", machineID)
		fmt.Fprintf(stdout, "   tokenHash: %q\n", tokenHash)
	}
	fmt.Fprintln(stdout, "\n2. Start the runner on this machine:")
	fmt.Fprintf(stdout, "   orbit-runner --machine-id %s --hub %s --config-dir %s\n", machineID, wsURL, cfgDirAbs)
	return nil
}

func initProjectMode(cfgDirAbs, dataDir, dir, machineFlag, idOverride, nameOverride, adapterOverride string, stdout io.Writer) error {
	absDir, err := filepath.Abs(protocol.ExpandPath(dir))
	if err != nil {
		return err
	}
	dataDirAbs := protocol.ExpandPath(dataDir)
	machineID := defaultMachineID(machineFlag)
	adapters := detectLocalAdapters()
	adapter := protocol.Adapter(strings.TrimSpace(adapterOverride))
	if adapter == "" {
		adapter = pickPreferredAdapter(adapters)
	}

	projID, projCfg, ok := detectGitProject(absDir, dataDirAbs, machineID, adapter, idOverride)
	if !ok {
		return fmt.Errorf("%s is not inside a Git repository", absDir)
	}
	if strings.TrimSpace(nameOverride) != "" {
		projCfg.DisplayName = strings.TrimSpace(nameOverride)
	}

	projectsPath := filepath.Join(cfgDirAbs, "projects.yaml")
	if _, err := os.Stat(projectsPath); err == nil {
		cfg, err := protocol.LoadProjectsConfig(projectsPath)
		if err != nil {
			return err
		}
		if cfg.Projects == nil {
			cfg.Projects = map[string]protocol.ProjectConfig{}
		}
		cfg.Projects[projID] = projCfg
		if err := writeYAMLFile(projectsPath, cfg, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "  [OK  ] Registered project %q (%s) in %s\n", projID, projCfg.Anchors[projCfg.DefaultCapability].RepoPath, projectsPath)
		fmt.Fprintln(stdout, "         Restart orbit-hub to pick up the updated projects.yaml.")
		return nil
	}

	// Client machine without local projects.yaml: print a ready-to-paste YAML snippet for the Hub host.
	snippet := map[string]protocol.ProjectConfig{projID: projCfg}
	raw, err := yaml.Marshal(snippet)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "No local %s found (this machine looks like a client node).\n", projectsPath)
	fmt.Fprintln(stdout, "Add the following block under `projects:` in your Hub host's ~/.config/hebi8-orbit/projects.yaml:")
	fmt.Fprintln(stdout, "")
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		fmt.Fprintf(stdout, "  %s\n", line)
	}
	return nil
}

func promptLine(r *bufio.Reader, w io.Writer, label, defVal string) (string, error) {
	if defVal != "" {
		fmt.Fprintf(w, "%s [%s]: ", label, defVal)
	} else {
		fmt.Fprintf(w, "%s: ", label)
	}
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return defVal, nil
	}
	return line, nil
}

func normalizeHubBaseURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("hub URL cannot be empty")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("invalid hub URL %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("hub URL must use http or https (got %q)", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("hub URL missing host: %q", raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/"), nil
}

func httpToWSURL(httpURL string) string {
	if strings.HasPrefix(httpURL, "https://") {
		return "wss://" + strings.TrimPrefix(httpURL, "https://")
	}
	return "ws://" + strings.TrimPrefix(httpURL, "http://")
}

func probeHubHealth(ctx context.Context, hubURL, devToken string) (string, error) {
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, strings.TrimRight(hubURL, "/")+"/api/health", nil)
	if err != nil {
		return "", err
	}
	if devToken != "" {
		req.Header.Set("X-Orbit-Dev-Token", devToken)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return strings.TrimSpace(string(body)), nil
}

func existingClientHubURL(cfgDirAbs string) string {
	raw, err := os.ReadFile(filepath.Join(cfgDirAbs, "client.yaml"))
	if err != nil {
		return ""
	}
	var c struct {
		HubURL string `yaml:"hubUrl"`
	}
	if yaml.Unmarshal(raw, &c) == nil {
		return strings.TrimSpace(c.HubURL)
	}
	return ""
}

func writeClientYAML(path, hubURL, devToken string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	doc := struct {
		HubURL     string `yaml:"hubUrl"`
		DevToken   string `yaml:"devToken,omitempty"`
		PAShortcut string `yaml:"paShortcut"`
	}{
		HubURL:     strings.TrimRight(hubURL, "/"),
		DevToken:   strings.TrimSpace(devToken),
		PAShortcut: "CmdOrCtrl+Shift+O",
	}
	return writeYAMLFile(path, doc, 0o600)
}

func writeYAMLFile(path string, v any, perm os.FileMode) error {
	raw, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func randomHexToken(numBytes int) (string, error) {
	b := make([]byte, numBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func defaultMachineID(override string) string {
	if s := sanitizeID(override); s != "" {
		return s
	}
	h, err := os.Hostname()
	if err != nil || strings.TrimSpace(h) == "" {
		return "local-node"
	}
	h = strings.Split(h, ".")[0]
	if s := sanitizeID(h); s != "" {
		return s
	}
	return "local-node"
}

func sanitizeID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
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

func defaultMachineCapabilities() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{"macos-env", "primary-dev"}
	default:
		return []string{"primary-dev", "linux-" + runtime.GOARCH}
	}
}

func detectLocalAdapters() map[string]protocol.AdapterConfig {
	out := map[string]protocol.AdapterConfig{}
	if p, err := exec.LookPath("agy"); err == nil {
		out["agy"] = protocol.AdapterConfig{
			Bin:              p,
			DefaultArgs:      []string{"acp", "--stdio", "--process-per-session"},
			MaxLiveWorkers:   6,
			IdleCloseSeconds: 900,
		}
	}
	if p, err := exec.LookPath("claude-agent-acp"); err == nil {
		out["claude"] = protocol.AdapterConfig{
			Bin:              p,
			MaxLiveWorkers:   4,
			IdleCloseSeconds: 900,
		}
	}
	if p, err := exec.LookPath("codex-acp"); err == nil {
		out["codex"] = protocol.AdapterConfig{
			Bin:              p,
			MaxLiveWorkers:   4,
			IdleCloseSeconds: 900,
		}
	}
	return out
}

func sortedAdapterNames(m map[string]protocol.AdapterConfig) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func pickPreferredAdapter(m map[string]protocol.AdapterConfig) protocol.Adapter {
	for _, cand := range []protocol.Adapter{protocol.AdapterAgy, protocol.AdapterClaude, protocol.AdapterCodex} {
		if _, ok := m[string(cand)]; ok {
			return cand
		}
	}
	return protocol.AdapterAgy
}

func detectGitProject(dir, dataDirAbs, machineID string, adapter protocol.Adapter, idOverride string) (string, protocol.ProjectConfig, bool) {
	topRaw, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", protocol.ProjectConfig{}, false
	}
	repoRoot := strings.TrimSpace(string(topRaw))
	if repoRoot == "" {
		return "", protocol.ProjectConfig{}, false
	}

	branch := "main"
	if brRaw, err := exec.Command("git", "-C", repoRoot, "symbolic-ref", "--short", "HEAD").Output(); err == nil {
		if b := strings.TrimSpace(string(brRaw)); b != "" {
			branch = b
		}
	}

	baseName := filepath.Base(repoRoot)
	projID := sanitizeID(idOverride)
	if projID == "" {
		projID = strings.ToLower(sanitizeID(baseName))
	}
	if projID == "" {
		projID = "project"
	}

	repoIdentity := "local:" + projID
	if remRaw, err := exec.Command("git", "-C", repoRoot, "remote", "get-url", "origin").Output(); err == nil {
		if rem := strings.TrimSpace(string(remRaw)); rem != "" {
			rem = strings.TrimSuffix(rem, ".git")
			rem = strings.TrimPrefix(rem, "https://")
			rem = strings.TrimPrefix(rem, "http://")
			if strings.HasPrefix(rem, "git@") {
				rem = strings.TrimPrefix(rem, "git@")
				rem = strings.Replace(rem, ":", "/", 1)
			}
			if rem != "" {
				repoIdentity = rem
			}
		}
	}

	capName := "primary-dev"
	if runtime.GOOS == "darwin" {
		capName = "macos-env"
	}

	cfg := protocol.ProjectConfig{
		DisplayName:       baseName,
		RepoIdentity:      repoIdentity,
		DefaultBranch:     branch,
		DefaultCapability: capName,
		DefaultAdapter:    adapter,
		IDEScheme:         protocol.IDECursor,
		Anchors: map[string]protocol.ProjectAnchorConfig{
			capName: {
				Machine:      machineID,
				RepoPath:     repoRoot,
				WorktreeRoot: filepath.Join(dataDirAbs, "worktrees", projID),
			},
		},
	}
	return projID, cfg, true
}
