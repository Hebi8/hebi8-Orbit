package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
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
		sockPath    = flag.String("sock", "", "local Unix domain socket path for `orbit deliver`")
		agyBin      = flag.String("agy-bin", "", "override agy executable path")
		claudeBin   = flag.String("claude-bin", "", "override claude-agent-acp executable path")
		codexBin    = flag.String("codex-bin", "", "override codex-acp executable path")
		orbitBinDir = flag.String("orbit-bin-dir", "", "directory containing `orbit` CLI to prepend to child PATH")
		idleClose   = flag.Int("idle-close-seconds", -1, "idle ACP session close timeout in seconds (0 disables)")
		showVersion = flag.Bool("version", false, "print orbit-runner version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("orbit-runner %s (commit=%s built=%s)\n", Version, Commit, BuildDate)
		return
	}

	expCfgDir := protocol.ExpandPath(*configDir)
	hubCfg, _ := protocol.LoadHubConfig(filepath.Join(expCfgDir, "hub.yaml"))
	machinesCfg, _ := protocol.LoadMachinesConfig(filepath.Join(expCfgDir, "machines.yaml"))

	mid := strings.TrimSpace(*machineID)
	if mid == "" {
		mid = strings.TrimSpace(os.Getenv("ORBIT_MACHINE_ID"))
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
	if expDataDir == "" {
		expDataDir = hubCfg.DataDir
	}
	if expDataDir == "" {
		expDataDir = "~/.local/share/hebi8-orbit"
	}
	expDataDir = protocol.ExpandPath(expDataDir)

	wsURL := strings.TrimSpace(*hubWS)
	httpURL := strings.TrimSpace(*hubHTTP)
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

	binDir := strings.TrimSpace(*orbitBinDir)
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

	drivers := map[protocol.Adapter]runner.AcpAdapterDriver{}
	var supported []protocol.Adapter
	var adapterDesc []string
	idleSec := 1200
	maxLiveWorkers := 0

	mCfg, hasMachineCfg := machinesCfg.Machines[mid]
	for _, adp := range protocol.Phase1ImplementedSessionAdapters {
		var aCfg protocol.AdapterConfig
		configured := false
		if hasMachineCfg {
			aCfg, configured = mCfg.Adapters[string(adp)]
		}
		bin := binOverrides[adp]
		if bin == "" && configured {
			bin = strings.TrimSpace(aCfg.Bin)
		}
		bin = protocol.ExpandPath(bin)

		// If not explicitly configured in machines.yaml, auto-detect optional adapters only when executable exists.
		if !configured && bin == "" {
			defaultBin := defaultAdapterBin(adp)
			if resolved, ok := resolveExecutable(defaultBin, binDir); ok {
				bin = resolved
			} else if adp == protocol.AdapterAgy && !hasMachineCfg {
				bin = defaultBin
			} else {
				continue
			}
		} else if bin == "" {
			bin = defaultAdapterBin(adp)
		}

		if resolved, ok := resolveExecutable(bin, binDir); ok {
			bin = resolved
		} else if configured {
			log.Printf("orbit-runner: configured adapter %s binary %q not found on PATH; skipping", adp, bin)
			continue
		}

		if aCfg.IdleCloseSeconds > 0 && (adp == protocol.AdapterAgy || idleSec == 1200) {
			idleSec = aCfg.IdleCloseSeconds
		}
		if aCfg.MaxLiveWorkers > maxLiveWorkers {
			maxLiveWorkers = aCfg.MaxLiveWorkers
		}

		drivers[adp] = runner.NewAdapterDriver(adp, runner.AgyDriverConfig{
			Adapter:       adp,
			Bin:           bin,
			Args:          aCfg.DefaultArgs,
			RunnerSock:    sock,
			OrbitBinDir:   binDir,
			RunnerVersion: Version,
		})
		supported = append(supported, adp)
		adapterDesc = append(adapterDesc, fmt.Sprintf("%s=%s", adp, bin))
	}

	if len(drivers) == 0 {
		fallbackBin := binOverrides[protocol.AdapterAgy]
		if fallbackBin == "" {
			fallbackBin = "agy"
		}
		drivers[protocol.AdapterAgy] = runner.NewAdapterDriver(protocol.AdapterAgy, runner.AgyDriverConfig{
			Adapter:       protocol.AdapterAgy,
			Bin:           fallbackBin,
			RunnerSock:    sock,
			OrbitBinDir:   binDir,
			RunnerVersion: Version,
		})
		supported = []protocol.Adapter{protocol.AdapterAgy}
		adapterDesc = []string{"agy=" + fallbackBin}
	}

	if *idleClose >= 0 {
		idleSec = *idleClose
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	r, err := runner.New(ctx, runner.Config{
		MachineID:         mid,
		HubWSURL:          wsURL,
		HubHTTPURL:        httpURL,
		BearerToken:       bearer,
		DataDir:           expDataDir,
		SocketPath:        sock,
		RunnerVersion:     Version,
		Drivers:           drivers,
		SupportedAdapters: supported,
		MaxLiveWorkers:    maxLiveWorkers,
		IdleCloseSeconds:  idleSec,
	})
	if err != nil {
		log.Fatalf("init orbit-runner: %v", err)
	}

	if err := r.Start(ctx); err != nil {
		log.Fatalf("start orbit-runner: %v", err)
	}
	log.Printf("orbit-runner started (version=%s machine=%s instance=%s hub=%s sock=%s adapters=[%s])",
		Version, mid, r.InstanceID(), wsURL, r.SocketPath(), strings.Join(adapterDesc, ", "))

	<-ctx.Done()
	log.Printf("orbit-runner shutting down...")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Close(shutCtx); err != nil {
		log.Printf("orbit-runner shutdown error: %v", err)
	}
}

func defaultAdapterBin(adp protocol.Adapter) string {
	switch adp {
	case protocol.AdapterClaude:
		return "claude-agent-acp"
	case protocol.AdapterCodex:
		return "codex-acp"
	default:
		return "agy"
	}
}

func resolveExecutable(bin, extraDir string) (string, bool) {
	if bin == "" {
		return "", false
	}
	if strings.Contains(bin, string(os.PathSeparator)) {
		if fi, err := os.Stat(bin); err == nil && !fi.IsDir() {
			return bin, true
		}
		return "", false
	}
	if extraDir != "" {
		cand := filepath.Join(extraDir, bin)
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand, true
		}
	}
	if found, err := exec.LookPath(bin); err == nil {
		return found, true
	}
	return "", false
}
