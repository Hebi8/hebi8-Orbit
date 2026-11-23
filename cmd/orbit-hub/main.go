package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/hub"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
	"github.com/Hebi8/hebi8-Orbit/web"
)

func main() {
	var (
		configDir         = flag.String("config-dir", "~/.config/hebi8-orbit", "directory containing hub.yaml, machines.yaml, projects.yaml")
		hubConfigPath     = flag.String("hub-config", "", "explicit path to hub.yaml (overrides --config-dir/hub.yaml)")
		dataDir           = flag.String("data-dir", "", "override data directory (default from hub.yaml or ~/.local/share/hebi8-orbit)")
		listenAddr        = flag.String("listen", "", "override listen address (default 127.0.0.1:4310)")
		hubOrigin         = flag.String("hub-origin", "", "override allowed browser Origin")
		devOpen           = flag.Bool("dev-open", false, "allow unauthenticated loopback access for local development")
		devToken          = flag.String("dev-token", "", "loopback X-Orbit-Dev-Token for local CLI/testing")
		ryugiBin          = flag.String("ryugi-bin", "", "override ryugi CLI executable path")
		ryugiServerHome   = flag.String("ryugi-server-home", "", "override RYUGI_SERVER_HOME for isolated sidecar state")
		ryugiPort         = flag.Int("ryugi-port", -1, "override ryugi server port (0 = default)")
		ryugiPublicOrigin = flag.String("ryugi-public-origin", "", "override public origin for ryugi Open URLs")
		ryugiPollSec      = flag.Int("ryugi-poll-seconds", -1, "override ryugi polling interval seconds (0 disables background poller)")
	)
	flag.Parse()

	expConfigDir := protocol.ExpandPath(*configDir)
	cfgPath := *hubConfigPath
	if cfgPath == "" {
		cfgPath = filepath.Join(expConfigDir, "hub.yaml")
	}

	cfg, err := protocol.LoadHubConfig(cfgPath)
	if err != nil {
		log.Fatalf("load hub config: %v", err)
	}
	cfg.ConfigDir = expConfigDir
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	if *listenAddr != "" {
		cfg.ListenAddr = *listenAddr
	}
	if *hubOrigin != "" {
		cfg.HubOrigin = *hubOrigin
	}
	if *devOpen {
		cfg.DevOpen = true
	}
	if *devToken != "" {
		cfg.DevToken = *devToken
	}
	if *ryugiBin != "" {
		cfg.RyugiBin = *ryugiBin
	}
	if *ryugiServerHome != "" {
		cfg.RyugiServerHome = *ryugiServerHome
	}
	if *ryugiPort >= 0 {
		cfg.RyugiPort = *ryugiPort
	}
	if *ryugiPublicOrigin != "" {
		cfg.RyugiPublicOrigin = *ryugiPublicOrigin
	}
	if *ryugiPollSec >= 0 {
		cfg.RyugiPollIntervalSeconds = *ryugiPollSec
	}

	cfg.DataDir = protocol.ExpandPath(cfg.DataDir)
	cfg.RyugiServerHome = protocol.ExpandPath(cfg.RyugiServerHome)

	// Unconditionally enforce loopback-only listenAddr (even when --dev-open is set)
	if err := hub.ValidateListenAddr(cfg); err != nil {
		log.Fatalf("invalid hub listen configuration: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbPath := filepath.Join(cfg.DataDir, "orbit.sqlite")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		log.Fatalf("open sqlite store %s: %v", dbPath, err)
	}
	defer st.Close()

	webFS, err := web.DistFS()
	if err != nil {
		log.Fatalf("load embedded web fs: %v", err)
	}

	srv, err := hub.NewServer(ctx, hub.ServerOptions{
		HubConfig: cfg,
		Store:     st,
		WebFS:     webFS,
	})
	if err != nil {
		log.Fatalf("init hub server: %v", err)
	}

	var pollerWG sync.WaitGroup
	pollerCtx, cancelPoller := context.WithCancel(ctx)
	if cfg.RyugiPollIntervalSeconds > 0 {
		srv.RyugiBridge().StartPoller(pollerCtx, &pollerWG)
	}

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("orbit-hub listening on http://%s (data=%s, devOpen=%v)", cfg.ListenAddr, cfg.DataDir, cfg.DevOpen)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		log.Printf("orbit-hub shutting down...")
	case err := <-errCh:
		if err != nil {
			cancelPoller()
			pollerWG.Wait()
			log.Fatalf("http server failed: %v", err)
		}
	}

	cancelPoller()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		fmt.Fprintf(os.Stderr, "http shutdown error: %v\n", err)
	}
	pollerWG.Wait()
}
