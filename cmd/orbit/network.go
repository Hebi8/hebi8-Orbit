package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/discovery"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

// parseFlagsPermuted reorders flag tokens ahead of positional arguments so CLI
// invocations like `orbit join <url> <code> --service` and
// `orbit agents test agy --machine mini` work regardless of flag placement.
func parseFlagsPermuted(fs *flag.FlagSet, args []string) error {
	var flagArgs []string
	var posArgs []string
	hasDoubleDash := false
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if tok == "--" {
			hasDoubleDash = true
			posArgs = append(posArgs, args[i+1:]...)
			break
		}
		if len(tok) > 1 && tok[0] == '-' {
			if strings.Contains(tok, "=") {
				flagArgs = append(flagArgs, tok)
				continue
			}
			name := strings.TrimLeft(tok, "-")
			f := fs.Lookup(name)
			if f != nil {
				if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
					flagArgs = append(flagArgs, tok)
					continue
				}
				flagArgs = append(flagArgs, tok)
				if i+1 < len(args) {
					i++
					flagArgs = append(flagArgs, args[i])
				}
				continue
			}
			// Preserve -h/--help or unknown flags so fs.Parse reports them accurately.
			flagArgs = append(flagArgs, tok)
			continue
		}
		posArgs = append(posArgs, tok)
	}
	reordered := make([]string, 0, len(flagArgs)+len(posArgs)+1)
	reordered = append(reordered, flagArgs...)
	if hasDoubleDash {
		reordered = append(reordered, "--")
	}
	reordered = append(reordered, posArgs...)
	return fs.Parse(reordered)
}

// parseJoinInput parses either:
// - "<hubURL> <code|XXXX-XXXX>"
// - "orbit join <hubURL> <code> [--no-runner|--runner-only]" (full pasted command)
// - "<hubURL>/#/join/<code>[?scope=client|runner]" (pasted invite link)
func parseJoinInput(args []string, explicitCode string) (hubURL, code string, noRunnerHint bool, err error) {
	hubURL, code, noRunnerHint, _, err = parseJoinInputWithScope(args, explicitCode)
	return hubURL, code, noRunnerHint, err
}

func parseJoinInputWithScope(args []string, explicitCode string) (hubURL, code string, noRunnerHint bool, runnerOnlyHint bool, err error) {
	joined := strings.TrimSpace(strings.Join(args, " "))
	joined = strings.Trim(joined, `"'`)
	tokens := strings.Fields(joined)

	// Strip optional leading "orbit join"
	if len(tokens) >= 2 && strings.EqualFold(filepath.Base(tokens[0]), "orbit") && strings.EqualFold(tokens[1], "join") {
		tokens = tokens[2:]
	}

	var positional []string
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		switch {
		case tok == "--no-runner":
			noRunnerHint = true
		case tok == "--runner-only":
			runnerOnlyHint = true
		case tok == "--code" && i+1 < len(tokens):
			if explicitCode == "" {
				explicitCode = tokens[i+1]
			}
			i++
		case strings.HasPrefix(tok, "--code="):
			if explicitCode == "" {
				explicitCode = strings.TrimPrefix(tok, "--code=")
			}
		case strings.HasPrefix(tok, "-"):
			// ignore extra pasted flags
		default:
			positional = append(positional, tok)
		}
	}

	if len(positional) == 0 {
		return "", "", false, false, fmt.Errorf("usage: orbit join <hub-url> <pair-code>")
	}

	rawURL := positional[0]
	code = strings.TrimSpace(explicitCode)
	if code == "" && len(positional) >= 2 {
		code = strings.TrimSpace(positional[1])
	}

	// Check for invite link form: https://hub/#/join/XXXX-XXXX[?scope=client|runner]
	if idx := strings.Index(rawURL, "#/join/"); idx >= 0 {
		rest := rawURL[idx+len("#/join/"):]
		codePart := rest
		if qIdx := strings.Index(rest, "?"); qIdx >= 0 {
			codePart = rest[:qIdx]
			if qVals, qErr := url.ParseQuery(rest[qIdx+1:]); qErr == nil {
				switch strings.ToLower(strings.TrimSpace(qVals.Get("scope"))) {
				case "client":
					noRunnerHint = true
				case "runner":
					runnerOnlyHint = true
				}
			}
		}
		if code == "" {
			code = strings.Trim(codePart, "/ ")
		}
		rawURL = rawURL[:idx]
	}

	hubURL, err = normalizeHubBaseURL(rawURL)
	if err != nil {
		return "", "", false, false, err
	}
	return hubURL, strings.TrimSpace(code), noRunnerHint, runnerOnlyHint, nil
}

func runUp(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		configDirFlag = fs.String("config-dir", "", "Config directory (default ~/.config/hebi8-orbit)")
		dataDirFlag   = fs.String("data-dir", "", "Data directory (default ~/.local/share/hebi8-orbit)")
		listenFlag    = fs.String("listen", "127.0.0.1:4310", "Hub loopback listen address")
		workspaceFlag = fs.String("workspace", "Home LAN", "Workspace display name")
		machineFlag   = fs.String("machine", "", "Local machine ID override")
		noRunner      = fs.Bool("no-runner", false, "Start Hub only without local Runner")
		useService    = fs.Bool("service", false, "Install/start user background service (systemd / LaunchAgent)")
		managed       = fs.Bool("managed", false, "Emit machine-readable startup details for desktop shell")
		jsonOut       = fs.Bool("json", false, "Output JSON")
	)
	if err := parseFlagsPermuted(fs, args); err != nil {
		return err
	}
	if *managed {
		*jsonOut = true
	}

	cfgDir := resolveConfigDir(*configDirFlag)
	dataDir := resolveDataDir(*dataDirFlag, cfgDir)
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "logs"), 0o755); err != nil {
		return err
	}

	hubPath := filepath.Join(cfgDir, "hub.yaml")
	listenSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "listen" {
			listenSet = true
		}
	})
	listenAddr := strings.TrimSpace(*listenFlag)
	if !listenSet {
		if hubCfg, err := protocol.LoadHubConfig(hubPath); err == nil && strings.TrimSpace(hubCfg.ListenAddr) != "" {
			listenAddr = strings.TrimSpace(hubCfg.ListenAddr)
		}
	}
	if listenAddr == "" {
		listenAddr = "127.0.0.1:4310"
	}

	hubURL := "http://" + strings.TrimPrefix(strings.TrimPrefix(listenAddr, "http://"), "https://")
	machineID := defaultMachineID(*machineFlag)
	if mCfg, err := protocol.LoadMachinesConfig(filepath.Join(cfgDir, "machines.yaml")); err == nil {
		for id, m := range mCfg.Machines {
			if m.Transport == protocol.TransportLocalWS {
				machineID = id
				break
			}
		}
	}

	// 1. Attach first if an Orbit Hub is already healthy on listenAddr
	attached := isOrbitHubHealthy(ctx, hubURL)
	if !attached {
		// Initialize idempotently if hub.yaml does not exist yet
		if _, err := os.Stat(hubPath); os.IsNotExist(err) {
			listenAddr, err = pickFreeLoopbackAddr(listenAddr)
			if err != nil {
				return err
			}
			hubURL = "http://" + listenAddr
			if err := initLocalHubFiles(cfgDir, dataDir, machineID, *workspaceFlag, listenAddr); err != nil {
				return err
			}
		} else {
			// Ensure local machine & runner token exist if hub.yaml existed
			if err := ensureLocalMachineFiles(cfgDir, machineID); err != nil {
				return err
			}
		}
	}

	hubPID := 0
	runnerPID := 0
	if *useService {
		if !attached {
			if err := installAndStartService(ctx, "hub", cfgDir, dataDir, machineID, stdout); err != nil {
				return err
			}
		}
		if !*noRunner {
			if err := installAndStartService(ctx, "runner", cfgDir, dataDir, machineID, stdout); err != nil {
				return err
			}
		}
	} else {
		if !attached {
			pid, err := spawnDetachedBinary("orbit-hub", []string{"--config-dir", cfgDir}, filepath.Join(dataDir, "logs", "hub.log"))
			if err != nil {
				return fmt.Errorf("start orbit-hub: %w", err)
			}
			hubPID = pid
		}
		if !waitForOrbitHub(ctx, hubURL, 10*time.Second) {
			return fmt.Errorf("orbit-hub did not become ready at %s (check %s)", hubURL, filepath.Join(dataDir, "logs", "hub.log"))
		}
		if !*noRunner && !isLocalRunnerRunning(dataDir, machineID) {
			pid, err := spawnDetachedBinary("orbit-runner", []string{"--machine-id", machineID, "--config-dir", cfgDir}, filepath.Join(dataDir, "logs", "runner.log"))
			if err != nil {
				return fmt.Errorf("start orbit-runner: %w", err)
			}
			runnerPID = pid
		}
	}

	// Ensure client.yaml points to local hub if absent
	clientPath := filepath.Join(cfgDir, "client.yaml")
	if _, err := os.Stat(clientPath); os.IsNotExist(err) {
		hc, _ := protocol.LoadHubConfig(hubPath)
		_ = writeClientConfigFull(clientPath, hubURL, hc.DevToken, "", machineID, "local")
	}

	if *jsonOut {
		enc, _ := json.MarshalIndent(map[string]any{
			"hubUrl":    hubURL,
			"machineId": machineID,
			"attached":  attached,
			"hubPid":    hubPID,
			"runnerPid": runnerPID,
		}, "", "  ")
		return writeRawJSON(stdout, enc)
	}

	if attached {
		fmt.Fprintf(stdout, "已连接到本机 Orbit (%s · 机器 %s)\n", hubURL, machineID)
	} else {
		fmt.Fprintf(stdout, "本机 Orbit 已启动 (%s · 机器 %s)\n", hubURL, machineID)
	}
	fmt.Fprintln(stdout, "打开工作台：orbit open")
	return nil
}

func runPair(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		runnerOnly = fs.Bool("runner", false, "Only issue runner credentials")
		clientOnly = fs.Bool("client", false, "Only issue client credentials")
		machineID  = fs.String("machine", "", "Bind credentials to an existing machine ID (e.g. mini)")
		ttlSec     = fs.Int("ttl", 600, "Pairing code expiry in seconds")
		noWait     = fs.Bool("no-wait", false, "Return immediately without waiting for machine to connect")
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag  = fs.String("token", "", "optional auth token")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		jsonOut    = fs.Bool("json", false, "output JSON")
	)
	if err := parseFlagsPermuted(fs, args); err != nil {
		return err
	}

	scope := protocol.PairingScopeBoth
	if *runnerOnly && !*clientOnly {
		scope = protocol.PairingScopeRunner
	} else if *clientOnly && !*runnerOnly {
		scope = protocol.PairingScopeClient
	}

	c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)
	raw, err := c.doJSON(ctx, http.MethodPost, "/api/pairing/codes", protocol.CreatePairingCodeRequest{
		Scope:         scope,
		BindMachineID: strings.TrimSpace(*machineID),
		TTLSeconds:    *ttlSec,
	})
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeRawJSON(stdout, raw)
	}
	var resp protocol.CreatePairingCodeResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}

	fmt.Fprintln(stdout, "在要加入的机器上运行：")
	fmt.Fprintf(stdout, "  %s\n\n", resp.JoinCommand)
	if resp.LoopbackOnly {
		fmt.Fprintln(stdout, "提示：当前仅本机可访问（127.0.0.1），请先在设置中开启外部接入。")
		fmt.Fprintln(stdout)
	}
	if scope == protocol.PairingScopeRunner {
		fmt.Fprintf(stdout, "（仅限执行节点，请在目标机器终端运行；配对码 %s，%d 分钟内有效。）\n", resp.Code, *ttlSec/60)
	} else {
		fmt.Fprintf(stdout, "或在那台机器的 Orbit 桌面端选「连到已有的」并粘贴上面这一行（配对码 %s，%d 分钟内有效）。\n", resp.Code, *ttlSec/60)
	}

	if !*noWait {
		fmt.Fprintln(stdout, "\n等待机器连接…（按 Ctrl+C 退出等待，配对码仍然有效）")
	}
	if *noWait {
		return nil
	}
	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()
	lastState := ""
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			stRaw, err := c.doJSON(ctx, http.MethodGet, "/api/pairing/codes/"+url.PathEscape(resp.ID), nil)
			if err != nil {
				continue
			}
			var st protocol.PairingCodeStatusResponse
			if json.Unmarshal(stRaw, &st) != nil {
				continue
			}
			if st.State == protocol.PairingCodeExpired || st.State == protocol.PairingCodeRevoked {
				fmt.Fprintf(stdout, "配对码已%s。\n", map[protocol.PairingCodeState]string{
					protocol.PairingCodeExpired: "过期",
					protocol.PairingCodeRevoked: "撤销",
				}[st.State])
				return nil
			}
			if st.State == protocol.PairingCodeClaimed {
				if lastState != "claimed" {
					lastState = "claimed"
					// If code was client-only OR the joiner opted out of Runner (machineId empty), finish immediately (#16).
					if scope == protocol.PairingScopeClient || strings.TrimSpace(st.MachineID) == "" {
						fmt.Fprintln(stdout, "✓ 客户端已连接。")
						return nil
					}
					fmt.Fprintf(stdout, "✓ %s 已认领配对码，正在等待节点上线…\n", st.MachineID)
				}
				if st.Online {
					fmt.Fprintf(stdout, "✓ %s 已上线！\n", st.MachineID)
					_ = printMachineAgentsSummary(ctx, c, st.MachineID, stdout)
					return nil
				}
			}
		}
	}
}

func runDiscover(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		jsonOut = fs.Bool("json", false, "output JSON")
	)
	if err := parseFlagsPermuted(fs, args); err != nil {
		return err
	}

	res := discovery.Discover(ctx, discovery.Options{
		IncludeLocal: true,
	})
	if *jsonOut {
		enc, _ := json.MarshalIndent(res, "", "  ")
		return writeRawJSON(stdout, enc)
	}
	if len(res.Hubs) == 0 {
		if res.TailscaleAvailable && !res.TailscaleOnline {
			fmt.Fprintln(stdout, "Tailscale 未连接，未发现可用网络。")
		} else {
			fmt.Fprintln(stdout, "未发现可用网络。")
		}
		return nil
	}
	for _, h := range res.Hubs {
		idPart := ""
		if h.DiscoveryID != "" {
			idPart = "  [" + h.DiscoveryID + "]"
		}
		selfPart := ""
		if h.IsSelf {
			selfPart = "  (当前)"
		}
		fmt.Fprintf(stdout, "  • %s%s%s\n", h.Origin, idPart, selfPart)
	}
	return nil
}

func runJoin(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		codeFlag   = fs.String("code", "", "Pairing code (XXXX-XXXX)")
		nameFlag   = fs.String("name", "", "Machine name override")
		noRunner   = fs.Bool("no-runner", false, "Connect as client only without starting local Runner")
		runnerOnly = fs.Bool("runner-only", false, "Join as Runner execution node only without client credentials")
		noStart    = fs.Bool("no-start", false, "Save credentials without launching orbit-runner")
		useService = fs.Bool("service", false, "Install and start background Runner service")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		jsonOut    = fs.Bool("json", false, "output JSON")
	)
	if err := parseFlagsPermuted(fs, args); err != nil {
		return err
	}

	hubURL, code, pastedNoRunner, pastedRunnerOnly, err := parseJoinInputWithScope(fs.Args(), *codeFlag)
	if err != nil {
		return err
	}
	if pastedNoRunner {
		*noRunner = true
	}
	if pastedRunnerOnly {
		*runnerOnly = true
	}
	if *noRunner && *runnerOnly {
		return fmt.Errorf("--no-runner and --runner-only cannot both be set")
	}
	if code == "" {
		return fmt.Errorf("missing pairing code (usage: orbit join <hub-url> <pair-code>)")
	}

	cfgDir := resolveConfigDir(*configDir)
	dataDir := resolveDataDir(*dataDirFlg, cfgDir)
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		return err
	}

	hostname, _ := os.Hostname()
	proposedID := defaultMachineID(*nameFlag)
	displayName := strings.TrimSpace(*nameFlag)
	if displayName == "" {
		displayName = proposedID
	}

	claimReq := protocol.ClaimPairingRequest{
		Code:       code,
		WantRunner: !*noRunner,
		WantClient: !*runnerOnly,
		Machine: &protocol.ClaimMachineInfo{
			ProposedID:    proposedID,
			DisplayName:   displayName,
			OS:            runtime.GOOS,
			Arch:          runtime.GOARCH,
			Hostname:      hostname,
			RunnerVersion: Version,
			Capabilities:  defaultMachineCapabilities(),
		},
	}

	bodyBytes, _ := json.Marshal(claimReq)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, hubURL+"/api/pairing/claim", bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Orbit-Request", "1")

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(httpReq)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", hubURL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr protocol.APIError
		if json.Unmarshal(raw, &apiErr) == nil && apiErr.Message != "" {
			return fmt.Errorf("配对失败：%s", apiErr.Message)
		}
		return fmt.Errorf("配对失败 (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var claimed protocol.ClaimPairingResponse
	if err := json.Unmarshal(raw, &claimed); err != nil {
		return err
	}

	if !*runnerOnly && claimed.ClientToken != "" {
		clientPath := filepath.Join(cfgDir, "client.yaml")
		if err := writeClientConfigFull(clientPath, hubURL, "", claimed.ClientToken, claimed.MachineID, "remote"); err != nil {
			return err
		}
	}

	if !*noRunner && claimed.RunnerToken != "" {
		tokenPath := filepath.Join(cfgDir, "runner.token")
		if err := os.WriteFile(tokenPath, []byte(claimed.RunnerToken+"\n"), 0o600); err != nil {
			return err
		}
		wsURL := httpToWSURL(hubURL) + "/ws/runner"
		if err := protocol.SaveRunnerLocalConfig(filepath.Join(cfgDir, "runner.yaml"), protocol.RunnerLocalConfig{
			SchemaVersion: 1,
			MachineID:     claimed.MachineID,
			HubURL:        hubURL,
			HubWSURL:      wsURL,
			DataDir:       dataDir,
		}); err != nil {
			return err
		}

		if !*noStart {
			if *useService {
				if err := installAndStartService(ctx, "runner", cfgDir, dataDir, claimed.MachineID, io.Discard); err != nil {
					fmt.Fprintf(stderr, "警告：安装后台服务失败（%v），改为直接启动 Runner\n", err)
					_, _ = spawnDetachedBinary("orbit-runner", []string{"--config-dir", cfgDir}, filepath.Join(dataDir, "logs", "runner.log"))
				}
			} else {
				if err := os.MkdirAll(filepath.Join(dataDir, "logs"), 0o755); err == nil {
					if _, err := spawnDetachedBinary("orbit-runner", []string{"--config-dir", cfgDir}, filepath.Join(dataDir, "logs", "runner.log")); err != nil {
						fmt.Fprintf(stderr, "警告：未能自动启动 orbit-runner（%v），可手动运行：orbit-runner --config-dir %s\n", err, cfgDir)
					}
				}
			}
		}
	}

	if *jsonOut {
		enc, _ := json.MarshalIndent(map[string]any{
			"hubUrl":        hubURL,
			"machineId":     claimed.MachineID,
			"workspaceName": claimed.WorkspaceName,
			"runnerEnabled": !*noRunner && claimed.RunnerToken != "",
		}, "", "  ")
		return writeRawJSON(stdout, enc)
	}

	fmt.Fprintf(stdout, "✓ 已加入「%s」（%s）\n", claimed.WorkspaceName, hubURL)
	if claimed.MachineID != "" {
		fmt.Fprintf(stdout, "  节点名称：%s\n", claimed.MachineID)
		// Briefly wait for runner to report agents if started
		if !*noRunner && !*noStart {
			c := &hubClient{baseURL: hubURL, clientToken: claimed.ClientToken, http: &http.Client{Timeout: 10 * time.Second}}
			deadline := time.Now().Add(12 * time.Second)
			for time.Now().Before(deadline) {
				time.Sleep(1200 * time.Millisecond)
				if ok := tryPrintOnlineMachineAgents(ctx, c, claimed.MachineID, stdout); ok {
					break
				}
			}
		}
	}
	fmt.Fprintln(stdout, "\n打开工作台：orbit open")
	return nil
}

func runAgents(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	sub := "list"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "list", "ls", "check", "probe", "test", "install", "enable", "disable", "help", "-h", "--help":
			sub = args[0]
			rest = args[1:]
		}
	}
	if sub == "help" || sub == "-h" || sub == "--help" {
		fmt.Fprintln(stdout, `Usage:
  orbit agents [list] [--machine <id>] [--json]
  orbit agents check [--machine <id>]
  orbit agents test <agy|claude|codex> [--machine <id>]
  orbit agents install <claude|codex> [--machine <id>]
  orbit agents enable|disable <agy|claude|codex> [--machine <id>]`)
		return nil
	}

	fs := flag.NewFlagSet("agents", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		machineFlag = fs.String("machine", "", "Target machine ID (defaults to local/sole machine)")
		hubURLFlag  = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag   = fs.String("token", "", "optional auth token")
		configDir   = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg  = fs.String("data-dir", "", "orbit data directory")
		jsonOut     = fs.Bool("json", false, "output JSON")
	)
	if err := parseFlagsPermuted(fs, rest); err != nil {
		return err
	}

	c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)
	machines, err := fetchMachinesList(ctx, c)
	if err != nil {
		return err
	}

	if sub == "list" || sub == "ls" {
		target := strings.TrimSpace(*machineFlag)
		if target == "" && fs.NArg() > 0 {
			target = strings.TrimSpace(fs.Arg(0))
		}
		if *jsonOut {
			enc, _ := json.MarshalIndent(machines, "", "  ")
			return writeRawJSON(stdout, enc)
		}
		for _, m := range machines {
			if target != "" && m.MachineID != target {
				continue
			}
			fmt.Fprintf(stdout, "%s (%s)\n", displayMachineLabel(m), m.Status)
			for _, ag := range m.Agents {
				fmt.Fprintf(stdout, "  %-12s  %-15s  %s\n", ag.Adapter, ag.State, agentHumanHint(ag))
			}
		}
		return nil
	}

	targetMachine := resolveTargetMachineID(*machineFlag, *configDir, machines)
	if targetMachine == "" {
		return fmt.Errorf("cannot determine target machine; specify --machine <id>")
	}

	switch sub {
	case "check", "probe":
		raw, err := c.doJSON(ctx, http.MethodPost, "/api/machines/"+url.PathEscape(targetMachine)+"/agents/probe", map[string]any{})
		if err != nil {
			return err
		}
		if *jsonOut {
			return writeRawJSON(stdout, raw)
		}
		return printMachineAgentsSummary(ctx, c, targetMachine, stdout)

	case "test":
		if fs.NArg() < 1 {
			return fmt.Errorf("usage: orbit agents test <agy|claude|codex> [--machine <id>]")
		}
		adp := strings.TrimSpace(fs.Arg(0))
		raw, err := c.doJSON(ctx, http.MethodPost, "/api/machines/"+url.PathEscape(targetMachine)+"/agents/"+url.PathEscape(adp)+"/test", map[string]any{})
		if err != nil {
			return err
		}
		if *jsonOut {
			return writeRawJSON(stdout, raw)
		}
		var out protocol.MachineAgentActionResponse
		_ = json.Unmarshal(raw, &out)
		if out.Probe != nil && out.Probe.LastTest != nil && out.Probe.LastTest.OK {
			fmt.Fprintf(stdout, "✓ %s/%s 连接正常 (%dms)\n", targetMachine, adp, out.Probe.LastTest.LatencyMs)
		} else if out.Probe != nil {
			fmt.Fprintf(stdout, "✗ %s/%s: %s\n", targetMachine, adp, agentHumanHint(*out.Probe))
		}
		return nil

	case "install":
		if fs.NArg() < 1 {
			return fmt.Errorf("usage: orbit agents install <claude|codex> [--machine <id>]")
		}
		adp := strings.TrimSpace(fs.Arg(0))
		fmt.Fprintf(stdout, "正在 %s 上安装 %s 连接组件…\n", targetMachine, adp)
		c.http.Timeout = 190 * time.Second
		raw, err := c.doJSON(ctx, http.MethodPost, "/api/machines/"+url.PathEscape(targetMachine)+"/agents/"+url.PathEscape(adp)+"/install", map[string]any{})
		if err != nil {
			return err
		}
		if *jsonOut {
			return writeRawJSON(stdout, raw)
		}
		fmt.Fprintf(stdout, "✓ %s/%s 安装完成并已就绪。\n", targetMachine, adp)
		return nil

	case "enable", "disable":
		if fs.NArg() < 1 {
			return fmt.Errorf("usage: orbit agents %s <agy|claude|codex> [--machine <id>]", sub)
		}
		adp := strings.TrimSpace(fs.Arg(0))
		en := sub == "enable"
		raw, err := c.doJSON(ctx, http.MethodPatch, "/api/machines/"+url.PathEscape(targetMachine)+"/agents/"+url.PathEscape(adp), protocol.UpdateMachineAgentRequest{
			Enabled: &en,
		})
		if err != nil {
			return err
		}
		if *jsonOut {
			return writeRawJSON(stdout, raw)
		}
		fmt.Fprintf(stdout, "✓ 已更新 %s/%s enabled=%v\n", targetMachine, adp, en)
		return nil
	}
	return fmt.Errorf("unknown agents subcommand %q", sub)
}

func runMachines(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	sub := "ls"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		rest = args[1:]
	}
	fs := flag.NewFlagSet("machines", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		hubURLFlag = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag  = fs.String("token", "", "optional auth token")
		configDir  = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg = fs.String("data-dir", "", "orbit data directory")
		jsonOut    = fs.Bool("json", false, "output JSON")
	)
	if err := parseFlagsPermuted(fs, rest); err != nil {
		return err
	}
	c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)

	switch sub {
	case "ls", "list":
		machines, err := fetchMachinesList(ctx, c)
		if err != nil {
			return err
		}
		if *jsonOut {
			enc, _ := json.MarshalIndent(machines, "", "  ")
			return writeRawJSON(stdout, enc)
		}
		for _, m := range machines {
			ready := []string{}
			for _, ag := range m.Agents {
				if ag.State == protocol.AgentStateReady && ag.Enabled {
					ready = append(ready, string(ag.Adapter))
				}
			}
			fmt.Fprintf(stdout, "%-16s  %-8s  %-6s  agents=[%s]\n", displayMachineLabel(m), m.Status, m.Source, strings.Join(ready, ","))
		}
		return nil

	case "rename":
		if fs.NArg() < 2 {
			return fmt.Errorf("usage: orbit machines rename <machineId> <alias>")
		}
		mid := strings.TrimSpace(fs.Arg(0))
		alias := strings.TrimSpace(fs.Arg(1))
		raw, err := c.doJSON(ctx, http.MethodPatch, "/api/machines/"+url.PathEscape(mid), protocol.UpdateMachineRequest{
			DisplayAlias: &alias,
		})
		if err != nil {
			return err
		}
		if *jsonOut {
			return writeRawJSON(stdout, raw)
		}
		fmt.Fprintf(stdout, "✓ 已将 %s 重命名为 %s\n", mid, alias)
		return nil

	case "rm", "remove", "delete":
		if fs.NArg() < 1 {
			return fmt.Errorf("usage: orbit machines rm <machineId>")
		}
		mid := strings.TrimSpace(fs.Arg(0))
		raw, err := c.doJSON(ctx, http.MethodDelete, "/api/machines/"+url.PathEscape(mid), nil)
		if err != nil {
			return err
		}
		if *jsonOut {
			return writeRawJSON(stdout, raw)
		}
		fmt.Fprintf(stdout, "✓ 已移除节点 %s\n", mid)
		return nil

	default:
		return fmt.Errorf("unknown machines subcommand %q (try: ls, rename, rm)", sub)
	}
}

func runProjects(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	sub := "ls"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		rest = args[1:]
	}
	fs := flag.NewFlagSet("projects", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		machineFlag = fs.String("machine", "", "Target machine ID")
		nameFlag    = fs.String("name", "", "Display name override")
		idFlag      = fs.String("id", "", "Project ID override")
		adapterFlag = fs.String("adapter", "", "Default adapter override")
		yamlFlag    = fs.Bool("yaml", false, "Write legacy projects.yaml instead of Hub API")
		hubURLFlag  = fs.String("hub", "", "orbit-hub base URL")
		tokenFlag   = fs.String("token", "", "optional auth token")
		configDir   = fs.String("config-dir", "", "orbit config directory")
		dataDirFlg  = fs.String("data-dir", "", "orbit data directory")
		jsonOut     = fs.Bool("json", false, "output JSON")
	)
	if err := parseFlagsPermuted(fs, rest); err != nil {
		return err
	}

	c := newHubClient(*configDir, *dataDirFlg, *hubURLFlag, *tokenFlag)
	switch sub {
	case "ls", "list":
		raw, err := c.doJSON(ctx, http.MethodGet, "/api/bootstrap", nil)
		if err != nil {
			return err
		}
		var boot protocol.BootstrapResponse
		if err := json.Unmarshal(raw, &boot); err != nil {
			return err
		}
		if *jsonOut {
			enc, _ := json.MarshalIndent(boot.Projects, "", "  ")
			return writeRawJSON(stdout, enc)
		}
		for _, p := range boot.Projects {
			if p.ProjectID == protocol.ScratchProjectID {
				continue
			}
			fmt.Fprintf(stdout, "%-20s  %-6s  %-10s  %s\n", p.ProjectID, p.Source, p.DefaultBranch, p.DisplayName)
		}
		return nil

	case "add":
		dir := "."
		if fs.NArg() > 0 {
			dir = fs.Arg(0)
		}
		if *yamlFlag {
			cfgDir := resolveConfigDir(*configDir)
			dataDir := resolveDataDir(*dataDirFlg, cfgDir)
			return initProjectMode(cfgDir, dataDir, dir, *machineFlag, *idFlag, *nameFlag, *adapterFlag, stdout)
		}
		absDir, err := filepath.Abs(protocol.ExpandPath(dir))
		if err != nil {
			return err
		}
		machines, err := fetchMachinesList(ctx, c)
		if err != nil {
			return err
		}
		targetMachine := resolveTargetMachineID(*machineFlag, *configDir, machines)
		if targetMachine == "" {
			return fmt.Errorf("cannot determine target machine; specify --machine <id>")
		}
		raw, err := c.doJSON(ctx, http.MethodPost, "/api/projects", protocol.CreateProjectRequest{
			MachineID:      targetMachine,
			RepoPath:       absDir,
			DisplayName:    strings.TrimSpace(*nameFlag),
			ProjectID:      strings.TrimSpace(*idFlag),
			DefaultAdapter: protocol.Adapter(strings.TrimSpace(*adapterFlag)),
		})
		if err != nil {
			return err
		}
		if *jsonOut {
			return writeRawJSON(stdout, raw)
		}
		var res protocol.CreateProjectResponse
		_ = json.Unmarshal(raw, &res)
		fmt.Fprintf(stdout, "✓ 已添加项目 %s（%s · 机器 %s）\n", res.Project.DisplayName, res.Project.ProjectID, targetMachine)
		return nil

	case "rm", "remove", "delete":
		if fs.NArg() < 1 {
			return fmt.Errorf("usage: orbit projects rm <projectId>")
		}
		pid := strings.TrimSpace(fs.Arg(0))
		raw, err := c.doJSON(ctx, http.MethodDelete, "/api/projects/"+url.PathEscape(pid), nil)
		if err != nil {
			return err
		}
		if *jsonOut {
			return writeRawJSON(stdout, raw)
		}
		fmt.Fprintf(stdout, "✓ 已移除项目 %s\n", pid)
		return nil

	default:
		return fmt.Errorf("unknown projects subcommand %q (try: ls, add, rm)", sub)
	}
}

func resolveConfigDir(flagVal string) string {
	d := strings.TrimSpace(flagVal)
	if d == "" {
		d = strings.TrimSpace(os.Getenv("ORBIT_CONFIG_DIR"))
	}
	if d == "" {
		d = "~/.config/hebi8-orbit"
	}
	return protocol.ExpandPath(d)
}

func resolveDataDir(flagVal, cfgDir string) string {
	d := strings.TrimSpace(flagVal)
	if d == "" {
		d = strings.TrimSpace(os.Getenv("ORBIT_DATA_DIR"))
	}
	if d == "" {
		if hc, err := protocol.LoadHubConfig(filepath.Join(cfgDir, "hub.yaml")); err == nil && hc.DataDir != "" {
			d = hc.DataDir
		}
	}
	if d == "" {
		d = "~/.local/share/hebi8-orbit"
	}
	return protocol.ExpandPath(d)
}

func isOrbitHubHealthy(ctx context.Context, hubURL string) bool {
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	base := strings.TrimRight(hubURL, "/")
	client := &http.Client{
		Timeout: 2 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if !sameOriginURL(req.URL.Scheme+"://"+req.URL.Host, base) {
				return fmt.Errorf("cross-origin redirect refused")
			}
			return nil
		},
	}
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
	// Compatibility probe for pre-v0.3 Orbit Hubs lacking the "service" field (#14)
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

func waitForOrbitHub(ctx context.Context, hubURL string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if isOrbitHubHealthy(ctx, hubURL) {
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

func pickFreeLoopbackAddr(preferred string) (string, error) {
	host, portStr, err := net.SplitHostPort(preferred)
	if err != nil {
		host = "127.0.0.1"
		portStr = "4310"
	}
	startPort := 4310
	_, _ = fmt.Sscanf(portStr, "%d", &startPort)
	for p := startPort; p < startPort+25; p++ {
		addr := fmt.Sprintf("%s:%d", host, p)
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			_ = ln.Close()
			return addr, nil
		}
	}
	return "", fmt.Errorf("no free loopback port found near %s", preferred)
}

func initLocalHubFiles(cfgDir, dataDir, machineID, workspaceName, listenAddr string) error {
	devToken, err := randomHexToken(24)
	if err != nil {
		return err
	}
	origin := "http://" + listenAddr
	hubCfg := protocol.DefaultHubConfig()
	hubCfg.WorkspaceName = strings.TrimSpace(workspaceName)
	if hubCfg.WorkspaceName == "" {
		hubCfg.WorkspaceName = "Home LAN"
	}
	hubCfg.ListenAddr = listenAddr
	hubCfg.DataDir = dataDir
	hubCfg.ConfigDir = cfgDir
	hubCfg.HubOrigin = origin
	hubCfg.DevToken = devToken
	if ryugiPath, err := exec.LookPath("ryugi"); err == nil {
		hubCfg.RyugiBin = ryugiPath
	}
	if err := writeYAMLFile(filepath.Join(cfgDir, "hub.yaml"), hubCfg, 0o600); err != nil {
		return err
	}
	if err := ensureLocalMachineFiles(cfgDir, machineID); err != nil {
		return err
	}
	projectsPath := filepath.Join(cfgDir, "projects.yaml")
	if _, err := os.Stat(projectsPath); os.IsNotExist(err) {
		if err := writeYAMLFile(projectsPath, protocol.ProjectsConfig{
			SchemaVersion: 1,
			Projects:      map[string]protocol.ProjectConfig{},
		}, 0o644); err != nil {
			return err
		}
	}
	return writeClientConfigFull(filepath.Join(cfgDir, "client.yaml"), origin, devToken, "", machineID, "local")
}

func ensureLocalMachineFiles(cfgDir, machineID string) error {
	machinesPath := filepath.Join(cfgDir, "machines.yaml")
	tokenPath := filepath.Join(cfgDir, "runner.token")

	rawTokBytes, tokReadErr := os.ReadFile(tokenPath)
	if tokReadErr != nil && !os.IsNotExist(tokReadErr) {
		return fmt.Errorf("read runner.token: %w", tokReadErr)
	}
	existingTok := strings.TrimSpace(string(rawTokBytes))
	tokExists := tokReadErr == nil && existingTok != ""

	_, machStatErr := os.Stat(machinesPath)
	if machStatErr != nil && !os.IsNotExist(machStatErr) {
		return fmt.Errorf("stat machines.yaml: %w", machStatErr)
	}
	machExists := machStatErr == nil

	runnerToken := existingTok
	if runnerToken == "" {
		var err error
		runnerToken, err = randomHexToken(24)
		if err != nil {
			return err
		}
	}
	sum := sha256.Sum256([]byte(runnerToken))
	tokenHash := "sha256:" + hex.EncodeToString(sum[:])

	mCfg := protocol.MachinesConfig{
		SchemaVersion: 1,
		Machines:      map[string]protocol.MachineConfig{},
	}
	if machExists {
		loaded, err := protocol.LoadMachinesConfig(machinesPath)
		if err != nil {
			return fmt.Errorf("parse existing machines.yaml: %w", err)
		}
		mCfg = loaded
	}
	if mCfg.Machines == nil {
		mCfg.Machines = map[string]protocol.MachineConfig{}
	}
	yamlDirty := !machExists
	if cur, exists := mCfg.Machines[machineID]; !exists {
		mCfg.Machines[machineID] = protocol.MachineConfig{
			DisplayName:  machineID,
			Transport:    protocol.TransportLocalWS,
			TokenHash:    tokenHash,
			Capabilities: defaultMachineCapabilities(),
		}
		yamlDirty = true
	} else if strings.TrimSpace(cur.TokenHash) != tokenHash {
		// Preserve all other machines/fields; keep local machine's tokenHash synchronized with runner.token (#15).
		cur.TokenHash = tokenHash
		mCfg.Machines[machineID] = cur
		yamlDirty = true
	}
	if yamlDirty {
		if err := writeYAMLFile(machinesPath, mCfg, 0o644); err != nil {
			return err
		}
	}
	if !tokExists {
		if err := os.WriteFile(tokenPath, []byte(runnerToken+"\n"), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func writeClientConfigFull(path, hubURL, devToken, clientToken, machineID, mode string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	doc := struct {
		HubURL      string `yaml:"hubUrl"`
		DevToken    string `yaml:"devToken,omitempty"`
		ClientToken string `yaml:"clientToken,omitempty"`
		MachineID   string `yaml:"machineId,omitempty"`
		Mode        string `yaml:"mode,omitempty"`
		PAShortcut  string `yaml:"paShortcut"`
	}{
		HubURL:      strings.TrimRight(hubURL, "/"),
		DevToken:    strings.TrimSpace(devToken),
		ClientToken: strings.TrimSpace(clientToken),
		MachineID:   strings.TrimSpace(machineID),
		Mode:        strings.TrimSpace(mode),
		PAShortcut:  "CmdOrCtrl+Shift+O",
	}
	return writeYAMLFile(path, doc, 0o600)
}

func isLocalRunnerRunning(dataDir, machineID string) bool {
	lockPath := filepath.Join(dataDir, "runner-"+machineID+".lock")
	l, err := protocol.AcquireFileLock(lockPath)
	if err != nil {
		return true
	}
	l.Release()
	return false
}

func locateSiblingOrPathBin(name string) (string, error) {
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), name)
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand, nil
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		cand := filepath.Join(home, ".local", "bin", name)
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand, nil
		}
	}
	return exec.LookPath(name)
}

func spawnDetachedBinary(binName string, args []string, logPath string) (int, error) {
	binPath, err := locateSiblingOrPathBin(binName)
	if err != nil {
		return 0, fmt.Errorf("%s not found: %w", binName, err)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return 0, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(binPath, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	setDetachedProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return 0, err
	}
	_ = logFile.Close()
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

func fetchMachinesList(ctx context.Context, c *hubClient) ([]protocol.MachineView, error) {
	raw, err := c.doJSON(ctx, http.MethodGet, "/api/machines", nil)
	if err != nil {
		return nil, err
	}
	var res protocol.ListMachinesResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	return res.Machines, nil
}

func resolveTargetMachineID(flagVal, cfgDirFlag string, machines []protocol.MachineView) string {
	if s := strings.TrimSpace(flagVal); s != "" {
		return s
	}
	cfgDir := resolveConfigDir(cfgDirFlag)
	if rc, err := protocol.LoadRunnerLocalConfig(filepath.Join(cfgDir, "runner.yaml")); err == nil && rc.MachineID != "" {
		return rc.MachineID
	}
	localID := defaultMachineID("")
	for _, m := range machines {
		if m.MachineID == localID {
			return m.MachineID
		}
	}
	for _, m := range machines {
		if m.TransportMode == protocol.TransportLocalWS {
			return m.MachineID
		}
	}
	if len(machines) > 0 {
		return machines[0].MachineID
	}
	return ""
}

func displayMachineLabel(m protocol.MachineView) string {
	if strings.TrimSpace(m.DisplayAlias) != "" {
		return m.DisplayAlias
	}
	return m.MachineID
}

func agentHumanHint(ag protocol.AgentProbe) string {
	switch ag.State {
	case protocol.AgentStateReady:
		if ag.BridgeVersion != "" {
			return "已就绪 (" + ag.BridgeVersion + ")"
		}
		return "已就绪"
	case protocol.AgentStateMissingBridge:
		if ag.HintCode == "missing_npm" {
			return "需要 Node.js (npm) 才能安装连接组件"
		}
		return "已装主程序，还差连接组件（orbit agents install " + string(ag.Adapter) + "）"
	case protocol.AgentStateNeedsLogin:
		return "需要在这台机器上登录一次"
	case protocol.AgentStateDisabled:
		return "已停用"
	case protocol.AgentStateNotInstalled:
		return "未安装"
	case protocol.AgentStateError:
		if ag.Detail != "" {
			return ag.Detail
		}
		return "检查失败"
	default:
		return string(ag.State)
	}
}

func printMachineAgentsSummary(ctx context.Context, c *hubClient, machineID string, w io.Writer) error {
	machines, err := fetchMachinesList(ctx, c)
	if err != nil {
		return err
	}
	for _, m := range machines {
		if m.MachineID == machineID {
			for _, ag := range m.Agents {
				fmt.Fprintf(w, "  %-8s  %-14s  %s\n", ag.Adapter, ag.State, agentHumanHint(ag))
			}
			return nil
		}
	}
	return nil
}

func tryPrintOnlineMachineAgents(ctx context.Context, c *hubClient, machineID string, w io.Writer) bool {
	machines, err := fetchMachinesList(ctx, c)
	if err != nil {
		return false
	}
	for _, m := range machines {
		if m.MachineID == machineID && m.Status == protocol.MachineOnline && len(m.Agents) > 0 {
			fmt.Fprintln(w, "  已探测 Agent：")
			for _, ag := range m.Agents {
				fmt.Fprintf(w, "    - %-8s %s\n", ag.Adapter, agentHumanHint(ag))
			}
			return true
		}
	}
	return false
}
