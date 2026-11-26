package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"
)

// Version metadata injected via -ldflags at release time.
var (
	Version   = "0.2.0-dev"
	Commit    = "none"
	BuildDate = "unknown"
)

//go:embed setup.html
var setupHTML []byte

func main() {
	var (
		urlFlag     = flag.String("url", "", "Hub URL override (e.g. https://orbit.example.ts.net)")
		configDir   = flag.String("config-dir", "", "Orbit config directory (default ~/.config/hebi8-orbit)")
		forceSetup  = flag.Bool("setup", false, "open the Hub connection setup screen")
		showVersion = flag.Bool("version", false, "print orbit-client version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("orbit-client %s (commit=%s built=%s)\n", Version, Commit, BuildDate)
		return
	}

	cfg, err := LoadClientConfig(*configDir, *urlFlag)
	if err != nil {
		log.Printf("orbit-client: warning loading config: %v", err)
	}

	runtimeMgr := NewRuntimeManager()
	defer runtimeMgr.Shutdown()

	var mu sync.RWMutex
	currentHubURL := cfg.HubURL
	currentMode := cfg.Mode

	getHubURL := func() string {
		mu.RLock()
		defer mu.RUnlock()
		return currentHubURL
	}
	setHubState := func(u, mode string) {
		mu.Lock()
		currentHubURL = u
		currentMode = mode
		mu.Unlock()
	}

	// Lightweight loopback server solely for the embedded setup screen.
	setupOrigin, closeSetupSrv, err := startSetupServer(*configDir, runtimeMgr, getHubURL, func() string {
		mu.RLock()
		defer mu.RUnlock()
		return currentMode
	}, func(u, mode string) {
		setHubState(u, mode)
		cfg.HubURL = u
		cfg.Mode = mode
	})
	if err != nil {
		log.Fatalf("orbit-client: start setup server: %v", err)
	}
	defer closeSetupSrv()

	initialURL := setupOrigin + "/"
	if !*forceSetup {
		if cfg.HubURL == "" {
			// Fresh machine: quickly check if an Orbit network already exists on Tailscale.
			// If a remote Orbit Hub is found, open the setup screen so the user can join it directly
			// (avoiding accidental split-brain standalone Hubs); otherwise launch zero-fork locally.
			disc := DiscoverNetworks(context.Background(), false, 1800*time.Millisecond)
			hasRemote := false
			for _, h := range disc.Hubs {
				if !h.IsSelf && h.Via == "tailscale" {
					hasRemote = true
					break
				}
			}
			if !hasRemote {
				if hubURL, _, _, sErr := runtimeMgr.StartOrAttachLocal(context.Background(), *configDir); sErr == nil && hubURL != "" {
					setHubState(hubURL, "local")
					cfg.HubURL = hubURL
					cfg.Mode = "local"
					initialURL = runtimeMgr.BuildDesktopOpenURL(context.Background(), *configDir, getHubURL(), "")
				} else if sErr != nil {
					log.Printf("orbit-client: auto-start local hub unavailable (%v); showing setup screen", sErr)
				}
			}
		} else {
			switch cfg.Mode {
			case "local":
				// Always check local Hub & local Runner independently (even if Hub is already attached).
				if hubURL, _, _, sErr := runtimeMgr.StartOrAttachLocal(context.Background(), *configDir); sErr == nil {
					setHubState(hubURL, "local")
					cfg.HubURL = hubURL
				}
			case "remote":
				// Restore managed remote Runner if user opted in ("让这台机器执行任务") during join.
				if cfg.RunLocalRunner {
					if rErr := runtimeMgr.EnsureRemoteRunner(*configDir); rErr != nil {
						log.Printf("orbit-client: warning restoring remote runner: %v", rErr)
					}
				}
			}
			initialURL = runtimeMgr.BuildDesktopOpenURL(context.Background(), *configDir, getHubURL(), "")
		}
	}

	mygo.App.SetName("hebi8-Orbit")
	mygo.App.SetVersion(Version)

	var mainWin *mygo.Window

	openMainWindow := func(targetURL string) {
		if mainWin != nil {
			mainWin.Show()
			mainWin.Focus()
			if targetURL != "" {
				_ = mainWin.Page().LoadURL(targetURL)
			}
			return
		}
		w := mygo.NewWindow(mygo.WindowOptions{
			Title:           "hebi8-Orbit",
			URL:             targetURL,
			Width:           1360,
			Height:          860,
			MinWidth:        960,
			MinHeight:       620,
			BackgroundColor: "light-dark(#fcfcfb, #0f0f11)",
		})
		mainWin = w

		page := w.Page()
		// Hand off IDE deep-links (cursor://, vscode://, zed://, antigravity://) and cross-origin URLs
		// (e.g. Ryugi :8443 canvases) natively via OS shell with zero browser prompt friction.
		page.OnWillNavigate(func(e *mygo.NavigateEvent) {
			if strings.HasPrefix(e.URL, "orbit-desktop://setup") {
				e.PreventDefault()
				target := setupOrigin + "/"
				if u, pErr := url.Parse(e.URL); pErr == nil && u.RawQuery != "" {
					target = setupOrigin + "/?" + u.RawQuery
				}
				_ = page.LoadURL(target)
				return
			}
			if ShouldOpenExternally(e.URL, getHubURL(), setupOrigin) {
				e.PreventDefault()
				go func(target string) {
					if err := mygo.Shell.OpenExternal(target); err != nil {
						log.Printf("orbit-client: open external %s: %v", target, err)
					}
				}(e.URL)
			}
		})
		page.SetWindowOpenHandler(func(req mygo.WindowOpenRequest) *mygo.WindowOptions {
			if strings.TrimSpace(req.URL) != "" {
				go func(target string) {
					if err := mygo.Shell.OpenExternal(target); err != nil {
						log.Printf("orbit-client: open external popup %s: %v", target, err)
					}
				}(req.URL)
			}
			return nil
		})
		page.OnDidFailLoad(func(loadErr *mygo.LoadError) {
			// If the configured Hub is unreachable on startup, gracefully offer the setup screen.
			if loadErr != nil && !strings.HasPrefix(page.URL(), setupOrigin) {
				log.Printf("orbit-client: load failed (%v); falling back to setup screen", loadErr)
				_ = page.LoadURL(setupOrigin + "/")
			}
		})
		w.OnClose(func(_ *mygo.CloseEvent) {
			mainWin = nil
		})
	}

	mygo.App.WhenReady(func() {
		mygo.App.SetMenu(buildAppMenu(*configDir, runtimeMgr, getHubURL, setupOrigin, func() *mygo.Window { return mainWin }, openMainWindow))
		openMainWindow(initialURL)

		// Best-effort global shortcut to summon Orbit straight to PA
		if sc := strings.TrimSpace(cfg.PAShortcut); sc != "" {
			if err := mygo.GlobalShortcut.Register(sc, func() {
				hub := getHubURL()
				target := setupOrigin + "/"
				if hub != "" {
					target = hub + "/#/hebi8"
				}
				mygo.App.Focus()
				openMainWindow(target)
			}); err != nil {
				log.Printf("orbit-client: global shortcut %q unavailable: %v", sc, err)
			}
		}
	})

	mygo.App.OnActivate(func(hasVisibleWindows bool) {
		if !hasVisibleWindows {
			hub := getHubURL()
			if hub != "" {
				openMainWindow(runtimeMgr.BuildDesktopOpenURL(context.Background(), *configDir, hub, ""))
			} else {
				openMainWindow(setupOrigin + "/")
			}
		}
	})

	if err := mygo.App.Run(); err != nil {
		log.Fatalf("orbit-client: %v", err)
	}
}

func buildAppMenu(
	configDir string,
	runtimeMgr *RuntimeManager,
	getHubURL func() string,
	setupOrigin string,
	getWin func() *mygo.Window,
	openWin func(string),
) *mygo.Menu {
	navigateHash := func(hash string) {
		hub := getHubURL()
		if hub == "" {
			openWin(setupOrigin + "/")
			return
		}
		openWin(hub + "/" + hash)
	}

	return mygo.NewMenu([]*mygo.MenuItem{
		{
			Label: "hebi8-Orbit",
			Submenu: []*mygo.MenuItem{
				{
					Label: "切换网络或重新连接…",
					Click: func(_ *mygo.MenuItem, _ *mygo.Window) {
						openWin(setupOrigin + "/")
					},
				},
				{
					Label: "在系统浏览器中打开",
					Click: func(_ *mygo.MenuItem, _ *mygo.Window) {
						if hub := getHubURL(); hub != "" {
							go func() {
								u := runtimeMgr.BuildOpenURL(context.Background(), configDir, hub, "")
								_ = mygo.Shell.OpenExternal(u)
							}()
						}
					},
				},
				mygo.Separator(),
				{Role: mygo.RoleQuit},
			},
		},
		{
			Label: "编辑",
			Submenu: []*mygo.MenuItem{
				{Role: mygo.RoleUndo},
				{Role: mygo.RoleRedo},
				mygo.Separator(),
				{Role: mygo.RoleCut},
				{Role: mygo.RoleCopy},
				{Role: mygo.RolePaste},
				{Role: mygo.RoleSelectAll},
			},
		},
		{
			Label: "视图",
			Submenu: []*mygo.MenuItem{
				{
					Label:       "总览",
					Accelerator: "CmdOrCtrl+1",
					Click:       func(_ *mygo.MenuItem, _ *mygo.Window) { navigateHash("#/") },
				},
				{
					Label:       "Hebi8",
					Accelerator: "CmdOrCtrl+2",
					Click:       func(_ *mygo.MenuItem, _ *mygo.Window) { navigateHash("#/hebi8") },
				},
				{
					Label:       "Artifacts",
					Accelerator: "CmdOrCtrl+3",
					Click:       func(_ *mygo.MenuItem, _ *mygo.Window) { navigateHash("#/artifacts") },
				},
				mygo.Separator(),
				{
					Label:       "刷新",
					Accelerator: "CmdOrCtrl+R",
					Click: func(_ *mygo.MenuItem, w *mygo.Window) {
						if w == nil {
							w = getWin()
						}
						if w != nil {
							w.Page().Reload()
						}
					},
				},
				{
					Label:       "开发者工具",
					Accelerator: "Alt+CmdOrCtrl+I",
					Click: func(_ *mygo.MenuItem, w *mygo.Window) {
						if w == nil {
							w = getWin()
						}
						if w != nil {
							w.Page().ToggleDevTools()
						}
					},
				},
			},
		},
	})
}

func startSetupServer(
	configDir string,
	runtimeMgr *RuntimeManager,
	getHubURL func() string,
	getMode func() string,
	onSaved func(url, mode string),
) (string, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	setupOrigin := "http://" + ln.Addr().String()
	if tcpAddr, ok := ln.Addr().(*net.TCPAddr); ok && tcpAddr.Port > 0 && runtimeMgr != nil {
		runtimeMgr.SetDesktopHelperPort(tcpAddr.Port)
	}
	checkSetupOrigin := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("X-Orbit-Setup") != "1" {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "missing X-Orbit-Setup header"})
			return false
		}
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		if !strings.EqualFold(origin, setupOrigin) {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "forbidden origin"})
			return false
		}
		return true
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(setupHTML)
	})
	mux.HandleFunc("GET /api/current", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"hubUrl": getHubURL(),
			"mode":   getMode(),
		})
	})
	mux.HandleFunc("GET /api/networks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		cfg, _ := LoadClientConfig(configDir, "")
		expDir := configDir
		if expDir == "" {
			expDir = defaultConfigDir()
		}
		expDir = expandHome(expDir)
		// Surface existing local workspace in the saved networks list even if user joined a remote Hub first.
		hasLocal := false
		for _, n := range cfg.Networks {
			if n.ID == "local" || n.Mode == "local" {
				hasLocal = true
				break
			}
		}
		if !hasLocal {
			if _, statErr := os.Stat(filepath.Join(expDir, "hub.yaml")); statErr == nil {
				cfg.Networks = append([]NetworkProfile{{
					ID:     "local",
					Name:   "本机工作区",
					Mode:   "local",
					HubURL: "http://127.0.0.1:4310",
				}}, cfg.Networks...)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"activeNetworkId": cfg.ActiveNetworkID,
			"hubUrl":          getHubURL(),
			"mode":            getMode(),
			"networks":        cfg.Networks,
		})
	})
	mux.HandleFunc("GET /api/local-agents", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"agents": ProbeLocalAgentsSummary(),
		})
	})
	localSSHHandler := newLocalSSHCandidatesHandler(getHubURL, setupOrigin, SSHMatchOptions{})
	mux.HandleFunc("GET /api/local-ssh-candidates", localSSHHandler)
	mux.HandleFunc("OPTIONS /api/local-ssh-candidates", localSSHHandler)
	mux.HandleFunc("POST /api/switch", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !checkSetupOrigin(w, r) {
			return
		}
		var body struct {
			NetworkID string `json:"networkId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.NetworkID) == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "请指定要切换的网络"})
			return
		}
		prof, err := runtimeMgr.SwitchNetwork(r.Context(), configDir, strings.TrimSpace(body.NetworkID))
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		onSaved(prof.HubURL, prof.Mode)
		openURL := runtimeMgr.BuildDesktopOpenURL(r.Context(), configDir, prof.HubURL, "")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"networkId": prof.ID,
			"hubUrl":    prof.HubURL,
			"mode":      prof.Mode,
			"openUrl":   openURL,
		})
	})
	mux.HandleFunc("POST /api/remove-network", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !checkSetupOrigin(w, r) {
			return
		}
		var body struct {
			NetworkID string `json:"networkId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.NetworkID) == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "无效的网络 ID"})
			return
		}
		cfg, err := runtimeMgr.RemoveNetwork(configDir, strings.TrimSpace(body.NetworkID))
		if err != nil {
			status := http.StatusInternalServerError
			if strings.Contains(err.Error(), "无法移除") {
				status = http.StatusBadRequest
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		onSaved(cfg.HubURL, cfg.Mode)
		_ = json.NewEncoder(w).Encode(map[string]any{"removed": true, "networks": cfg.Networks})
	})
	mux.HandleFunc("GET /api/discover", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		res := DiscoverNetworks(r.Context(), true, 4000*time.Millisecond)
		_ = json.NewEncoder(w).Encode(res)
	})
	mux.HandleFunc("POST /api/discover", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !checkSetupOrigin(w, r) {
			return
		}
		res := DiscoverNetworks(r.Context(), true, 4000*time.Millisecond)
		_ = json.NewEncoder(w).Encode(res)
	})
	mux.HandleFunc("POST /api/local-start", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !checkSetupOrigin(w, r) {
			return
		}
		hubURL, machineID, attached, err := runtimeMgr.StartOrAttachLocal(r.Context(), configDir)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		onSaved(hubURL, "local")
		openURL := runtimeMgr.BuildDesktopOpenURL(r.Context(), configDir, hubURL, "")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"hubUrl":    hubURL,
			"openUrl":   openURL,
			"machineId": machineID,
			"attached":  attached,
		})
	})
	mux.HandleFunc("POST /api/join", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !checkSetupOrigin(w, r) {
			return
		}
		var body struct {
			Input    string `json:"input"`
			AsRunner bool   `json:"asRunner"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "无效的请求格式"})
			return
		}
		res, err := runtimeMgr.JoinRemote(r.Context(), configDir, body.Input, body.AsRunner)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		onSaved(res.HubURL, "remote")
		openURL := runtimeMgr.BuildDesktopOpenURL(r.Context(), configDir, res.HubURL, "")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"hubUrl":          res.HubURL,
			"openUrl":         openURL,
			"machineId":       res.MachineID,
			"clientConnected": res.ClientConnected,
			"runnerStarted":   res.RunnerStarted,
			"runnerError":     res.RunnerError,
		})
	})
	mux.HandleFunc("POST /api/connect", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !checkSetupOrigin(w, r) {
			return
		}
		var body struct {
			HubURL string `json:"hubUrl"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "无效的请求格式"})
			return
		}
		res, err := runtimeMgr.ConnectDirect(r.Context(), configDir, body.HubURL)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		onSaved(res.HubURL, "remote")
		openURL := runtimeMgr.BuildDesktopOpenURL(r.Context(), configDir, res.HubURL, "")
		_ = json.NewEncoder(w).Encode(map[string]string{"hubUrl": res.HubURL, "openUrl": openURL})
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
	return "http://" + ln.Addr().String(), cleanup, nil
}

// newLocalSSHCandidatesHandler serves local ~/.ssh/config matching ONLY to the currently
// connected Orbit Hub origin (or the embedded setupOrigin) via strict Origin + CORS checks,
// preventing arbitrary websites from probing the user's local SSH config.
func newLocalSSHCandidatesHandler(getHubURL func() string, setupOrigin string, opts SSHMatchOptions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		hubURL := ""
		if getHubURL != nil {
			hubURL = strings.TrimSpace(getHubURL())
		}
		allowed := origin != "" && ((hubURL != "" && sameOriginStrings(origin, hubURL)) || (setupOrigin != "" && sameOriginStrings(origin, setupOrigin)))
		if !allowed {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "forbidden origin"})
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		q := r.URL.Query()
		target := SSHTarget{
			MachineID:     strings.TrimSpace(q.Get("machineId")),
			DisplayName:   strings.TrimSpace(q.Get("name")),
			TailscaleHost: strings.TrimSpace(q.Get("host")),
			SSHAlias:      strings.TrimSpace(q.Get("sshAlias")),
		}
		res := MatchLocalSSHConfig(r.Context(), target, opts)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	}
}
