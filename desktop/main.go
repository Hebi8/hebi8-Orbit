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

	var mu sync.RWMutex
	currentHubURL := cfg.HubURL

	getHubURL := func() string {
		mu.RLock()
		defer mu.RUnlock()
		return currentHubURL
	}
	setHubURL := func(u string) {
		mu.Lock()
		currentHubURL = u
		mu.Unlock()
	}

	// Lightweight loopback server solely for the embedded "Connect to Hub" setup page.
	setupOrigin, closeSetupSrv, err := startSetupServer(*configDir, getHubURL, func(u string) {
		setHubURL(u)
		cfg.HubURL = u
	})
	if err != nil {
		log.Fatalf("orbit-client: start setup server: %v", err)
	}
	defer closeSetupSrv()

	initialURL := setupOrigin + "/"
	if !*forceSetup && cfg.HubURL != "" {
		initialURL = cfg.HubURL + "/"
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
		mygo.App.SetMenu(buildAppMenu(getHubURL, setupOrigin, func() *mygo.Window { return mainWin }, openMainWindow))
		openMainWindow(initialURL)

		// Best-effort global shortcut to summon Orbit straight to PA
		if sc := strings.TrimSpace(cfg.PAShortcut); sc != "" {
			if err := mygo.GlobalShortcut.Register(sc, func() {
				hub := getHubURL()
				target := setupOrigin + "/"
				if hub != "" {
					target = hub + "/#/pa"
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
				openMainWindow(hub + "/")
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
					Label: "切换 Hub 地址…",
					Click: func(_ *mygo.MenuItem, _ *mygo.Window) {
						openWin(setupOrigin + "/")
					},
				},
				{
					Label: "在系统浏览器中打开",
					Click: func(_ *mygo.MenuItem, _ *mygo.Window) {
						if hub := getHubURL(); hub != "" {
							go func() { _ = mygo.Shell.OpenExternal(hub + "/") }()
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
					Label:       "PA 助手",
					Accelerator: "CmdOrCtrl+2",
					Click:       func(_ *mygo.MenuItem, _ *mygo.Window) { navigateHash("#/pa") },
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

func startSetupServer(configDir string, getHubURL func() string, onSaved func(string)) (string, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(setupHTML)
	})
	mux.HandleFunc("GET /api/current", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"hubUrl": getHubURL()})
	})
	mux.HandleFunc("POST /api/connect", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body struct {
			HubURL string `json:"hubUrl"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "无效的请求格式"})
			return
		}
		norm, err := NormalizeHubURL(body.HubURL)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		hc := &http.Client{Timeout: 6 * time.Second}
		resp, err := hc.Get(norm + "/api/health")
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("无法连接到 %s: %v", norm, err)})
			return
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("Hub 健康检查返回 HTTP %d", resp.StatusCode)})
			return
		}
		if err := SaveClientConfig(configDir, ClientConfig{HubURL: norm, PAShortcut: "CmdOrCtrl+Shift+O"}); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("保存配置失败: %v", err)})
			return
		}
		onSaved(norm)
		_ = json.NewEncoder(w).Encode(map[string]string{"hubUrl": norm})
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
