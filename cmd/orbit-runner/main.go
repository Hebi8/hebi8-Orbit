package main

import (
	"context"
	"flag"
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
		orbitBinDir = flag.String("orbit-bin-dir", "", "directory containing `orbit` CLI to prepend to child PATH")
		idleClose   = flag.Int("idle-close-seconds", -1, "idle ACP session close timeout in seconds (0 disables)")
	)
	flag.Parse()

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

	// Resolve agy binary, args, idleCloseSeconds, and maxLiveWorkers from machines.yaml when not overridden
	bin := strings.TrimSpace(*agyBin)
	var args []string
	idleSec := 1200
	maxLiveWorkers := 0
	if mCfg, ok := machinesCfg.Machines[mid]; ok {
		if agyCfg, ok := mCfg.Adapters["agy"]; ok {
			if bin == "" && agyCfg.Bin != "" {
				bin = agyCfg.Bin
			}
			if len(agyCfg.DefaultArgs) > 0 {
				args = agyCfg.DefaultArgs
			}
			if agyCfg.IdleCloseSeconds > 0 {
				idleSec = agyCfg.IdleCloseSeconds
			}
			if agyCfg.MaxLiveWorkers > 0 {
				maxLiveWorkers = agyCfg.MaxLiveWorkers
			}
		}
	}
	if bin == "" {
		bin = "agy"
	}
	if *idleClose >= 0 {
		idleSec = *idleClose
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

	drv := runner.NewAgyAcpDriver(runner.AgyDriverConfig{
		Bin:         bin,
		Args:        args,
		RunnerSock:  sock,
		OrbitBinDir: binDir,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	r, err := runner.New(ctx, runner.Config{
		MachineID:         mid,
		HubWSURL:          wsURL,
		HubHTTPURL:        httpURL,
		BearerToken:       bearer,
		DataDir:           expDataDir,
		SocketPath:        sock,
		Driver:            drv,
		SupportedAdapters: append([]protocol.Adapter(nil), protocol.Phase1ImplementedSessionAdapters...),
		MaxLiveWorkers:    maxLiveWorkers,
		IdleCloseSeconds:  idleSec,
	})
	if err != nil {
		log.Fatalf("init orbit-runner: %v", err)
	}

	if err := r.Start(ctx); err != nil {
		log.Fatalf("start orbit-runner: %v", err)
	}
	log.Printf("orbit-runner started (machine=%s instance=%s hub=%s sock=%s agy=%s)", mid, r.InstanceID(), wsURL, r.SocketPath(), bin)

	<-ctx.Done()
	log.Printf("orbit-runner shutting down...")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Close(shutCtx); err != nil {
		log.Printf("orbit-runner shutdown error: %v", err)
	}
}
