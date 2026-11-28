package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/runner"
)

// Version metadata injected via -ldflags at release time.
var (
	Version   = "0.2.0-dev"
	Commit    = "none"
	BuildDate = "unknown"
)

func main() {
	var (
		machineID   = flag.String("machine-id", "", "machineId registered in machines.yaml (or ORBIT_MACHINE_ID)")
		hubWS       = flag.String("hub-ws", "", "Hub WebSocket URL (e.g. ws://127.0.0.1:4310/ws/runner)")
		hubHTTP     = flag.String("hub-http", "", "Hub HTTP base URL (e.g. http://127.0.0.1:4310)")
		configDir   = flag.String("config-dir", "~/.config/hebi8-orbit", "orbit config directory")
		dataDir     = flag.String("data-dir", "", "orbit data directory (default ~/.local/share/hebi8-orbit)")
		tokenFlag   = flag.String("token", "", "optional runner bearer token (overrides --token-file)")
		tokenFile   = flag.String("token-file", "", "path to runner bearer token file (default <config-dir>/runner.token)")
		sockPath    = flag.String("sock", "", "local Unix domain socket path for h8o deliver")
		agyBin      = flag.String("agy-bin", "", "override agy executable path")
		claudeBin   = flag.String("claude-bin", "", "override claude-agent-acp executable path")
		codexBin    = flag.String("codex-bin", "", "override codex-acp executable path")
		h8oBinDir   = flag.String("h8o-bin-dir", "", "directory containing h8o CLI to prepend to child PATH")
		idleClose   = flag.Int("idle-close-seconds", -1, "idle ACP session close timeout in seconds (0 disables)")
		parentPID   = flag.Int("parent-pid", 0, "optional parent PID to watch; exits cleanly if parent disappears")
		showVersion = flag.Bool("version", false, "print h8o-runner version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("h8o-runner %s (commit=%s built=%s)\n", Version, Commit, BuildDate)
		return
	}

	expCfgDir := protocol.ExpandPath(*configDir)
	hubCfg, _ := protocol.LoadHubConfig(filepath.Join(expCfgDir, "hub.yaml"))
	runnerLocalCfg, hasRunnerYAML := protocol.LoadRunnerLocalConfig(filepath.Join(expCfgDir, "runner.yaml"))
	machinesCfg, _ := protocol.LoadMachinesConfig(filepath.Join(expCfgDir, "machines.yaml"))

	mid := strings.TrimSpace(*machineID)
	if mid == "" {
		mid = strings.TrimSpace(os.Getenv("ORBIT_MACHINE_ID"))
	}
	if mid == "" && hasRunnerYAML == nil && strings.TrimSpace(runnerLocalCfg.MachineID) != "" {
		mid = strings.TrimSpace(runnerLocalCfg.MachineID)
	}
	if mid == "" {
		for id, m := range machinesCfg.Machines {
			if m.Transport == protocol.TransportLocalWS {
				mid = id
				break
			}
		}
	}
	if mid == "" {
		mid = "hub-node"
	}

	expDataDir := strings.TrimSpace(*dataDir)
	if expDataDir == "" && hasRunnerYAML == nil && strings.TrimSpace(runnerLocalCfg.DataDir) != "" {
		expDataDir = runnerLocalCfg.DataDir
	}
	if expDataDir == "" {
		expDataDir = hubCfg.DataDir
	}
	if expDataDir == "" {
		expDataDir = "~/.local/share/hebi8-orbit"
	}
	expDataDir = protocol.ExpandPath(expDataDir)

	wsURL := strings.TrimSpace(*hubWS)
	httpURL := strings.TrimSpace(*hubHTTP)
	if wsURL == "" && hasRunnerYAML == nil {
		if strings.TrimSpace(runnerLocalCfg.HubWSURL) != "" {
			wsURL = strings.TrimSpace(runnerLocalCfg.HubWSURL)
		}
		if strings.TrimSpace(runnerLocalCfg.HubURL) != "" {
			if httpURL == "" {
				httpURL = strings.TrimRight(strings.TrimSpace(runnerLocalCfg.HubURL), "/")
			}
			if wsURL == "" {
				u := strings.TrimRight(strings.TrimSpace(runnerLocalCfg.HubURL), "/")
				if strings.HasPrefix(u, "https://") {
					wsURL = "wss://" + strings.TrimPrefix(u, "https://") + "/ws/runner"
				} else if strings.HasPrefix(u, "http://") {
					wsURL = "ws://" + strings.TrimPrefix(u, "http://") + "/ws/runner"
				}
			}
		}
	}
	if wsURL == "" {
		addr := hubCfg.ListenAddr
		if addr == "" {
			addr = "127.0.0.1:4310"
		}
		if strings.HasPrefix(addr, "https://") {
			wsURL = "wss://" + strings.TrimPrefix(addr, "https://") + "/ws/runner"
			httpURL = strings.TrimRight(addr, "/")
		} else if strings.HasPrefix(addr, "http://") {
			wsURL = "ws://" + strings.TrimPrefix(addr, "http://") + "/ws/runner"
			httpURL = strings.TrimRight(addr, "/")
		} else {
			wsURL = "ws://" + addr + "/ws/runner"
			httpURL = "http://" + addr
		}
	}

	bearer := strings.TrimSpace(*tokenFlag)
	if bearer == "" {
		bearer = strings.TrimSpace(os.Getenv("ORBIT_RUNNER_TOKEN"))
	}
	if bearer == "" {
		tf := strings.TrimSpace(*tokenFile)
		if tf == "" {
			tf = filepath.Join(expCfgDir, "runner.token")
		}
		if raw, err := os.ReadFile(protocol.ExpandPath(tf)); err == nil {
			bearer = strings.TrimSpace(string(raw))
		}
	}

	sock := strings.TrimSpace(*sockPath)
	if sock == "" {
		sock = filepath.Join(expDataDir, "runner-"+mid+".sock")
	}
	sock = protocol.ExpandPath(sock)

	binDir := strings.TrimSpace(*h8oBinDir)
	if binDir == "" {
		if exe, err := os.Executable(); err == nil {
			binDir = filepath.Dir(exe)
		}
	}

	binOverrides := map[protocol.Adapter]string{
		protocol.AdapterAgy:    strings.TrimSpace(*agyBin),
		protocol.AdapterClaude: strings.TrimSpace(*claudeBin),
		protocol.AdapterCodex:  strings.TrimSpace(*codexBin),
	}

	idleSec := 1200
	maxLiveWorkers := 0
	fileConfigs := map[protocol.Adapter]protocol.AdapterConfig{}
	if mCfg, ok := machinesCfg.Machines[mid]; ok {
		for k, aCfg := range mCfg.Adapters {
			adp := protocol.Adapter(k)
			if protocol.IsSessionAdapterImplemented(adp) {
				fileConfigs[adp] = aCfg
				if aCfg.IdleCloseSeconds > 0 && (adp == protocol.AdapterAgy || idleSec == 1200) {
					idleSec = aCfg.IdleCloseSeconds
				}
				if aCfg.MaxLiveWorkers > maxLiveWorkers {
					maxLiveWorkers = aCfg.MaxLiveWorkers
				}
			}
		}
	}

	if *idleClose >= 0 {
		idleSec = *idleClose
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *parentPID > 0 {
		protocol.StartParentWatchdog(ctx, *parentPID, func() {
			log.Printf("h8o-runner: parent pid %d exited; shutting down", *parentPID)
			stop()
		})
	}

	runnerLock, err := protocol.AcquireFileLock(filepath.Join(expDataDir, "runner-"+mid+".lock"))
	if err != nil {
		log.Fatalf("h8o-runner single-instance lock: %v", err)
	}
	defer runnerLock.Release()

	agentMgr := runner.NewAgentManager(runner.AgentManagerConfig{
		DataDir:       expDataDir,
		OrbitBinDir:   binDir,
		SocketPath:    sock,
		RunnerVersion: Version,
		FlagBins:      binOverrides,
		FileConfigs:   fileConfigs,
	})

	r, err := runner.New(ctx, runner.Config{
		MachineID:        mid,
		HubWSURL:         wsURL,
		HubHTTPURL:       httpURL,
		BearerToken:      bearer,
		DataDir:          expDataDir,
		SocketPath:       sock,
		RunnerVersion:    Version,
		AgentManager:     agentMgr,
		MaxLiveWorkers:   maxLiveWorkers,
		IdleCloseSeconds: idleSec,
	})
	if err != nil {
		log.Fatalf("init h8o-runner: %v", err)
	}

	if err := r.Start(ctx); err != nil {
		log.Fatalf("start h8o-runner: %v", err)
	}
	probes := agentMgr.SnapshotProbes()
	var probeDesc []string
	for _, p := range probes {
		probeDesc = append(probeDesc, fmt.Sprintf("%s=%s", p.Adapter, p.State))
	}
	log.Printf("h8o-runner started (version=%s machine=%s instance=%s hub=%s sock=%s agents=[%s])",
		Version, mid, r.InstanceID(), wsURL, r.SocketPath(), strings.Join(probeDesc, ", "))

	<-ctx.Done()
	log.Printf("h8o-runner shutting down...")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Close(shutCtx); err != nil {
		log.Printf("h8o-runner shutdown error: %v", err)
	}
}
