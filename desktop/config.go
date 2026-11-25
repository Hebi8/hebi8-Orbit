package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ClientConfig persists user preferences for the desktop shell in ~/.config/hebi8-orbit/client.yaml.
type ClientConfig struct {
	HubURL         string `yaml:"hubUrl"`
	DevToken       string `yaml:"devToken,omitempty"`
	ClientToken    string `yaml:"clientToken,omitempty"`
	MachineID      string `yaml:"machineId,omitempty"`
	Mode           string `yaml:"mode,omitempty"` // local | remote
	RunLocalRunner bool   `yaml:"runLocalRunner,omitempty"`
	PAShortcut     string `yaml:"paShortcut,omitempty"`
}

// hubConfigFallback reads hub.yaml when client.yaml is absent.
type hubConfigFallback struct {
	HubOrigin  string `yaml:"hubOrigin"`
	ListenAddr string `yaml:"listenAddr"`
	DevToken   string `yaml:"devToken"`
	DataDir    string `yaml:"dataDir"`
}

func defaultConfigDir() string {
	if v := strings.TrimSpace(os.Getenv("ORBIT_CONFIG_DIR")); v != "" {
		return expandHome(v)
	}
	return expandHome("~/.config/hebi8-orbit")
}

func expandHome(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func clientConfigPath(configDir string) string {
	if configDir == "" {
		configDir = defaultConfigDir()
	}
	return filepath.Join(expandHome(configDir), "client.yaml")
}

// LoadClientConfig resolves the desktop client configuration using precedence:
// explicitURLFlag > ORBIT_HUB_URL > client.yaml > hub.yaml (hubOrigin / listenAddr).
func LoadClientConfig(configDir, explicitURLFlag string) (ClientConfig, error) {
	cfg := ClientConfig{
		PAShortcut: "CmdOrCtrl+Shift+O",
	}
	if configDir == "" {
		configDir = defaultConfigDir()
	}
	expDir := expandHome(configDir)

	if raw, err := os.ReadFile(filepath.Join(expDir, "client.yaml")); err == nil {
		var saved ClientConfig
		if err := yaml.Unmarshal(raw, &saved); err != nil {
			return cfg, fmt.Errorf("parse client.yaml: %w", err)
		}
		if strings.TrimSpace(saved.HubURL) != "" {
			cfg.HubURL = saved.HubURL
		}
		if strings.TrimSpace(saved.DevToken) != "" {
			cfg.DevToken = strings.TrimSpace(saved.DevToken)
		}
		if strings.TrimSpace(saved.ClientToken) != "" {
			cfg.ClientToken = strings.TrimSpace(saved.ClientToken)
		}
		if strings.TrimSpace(saved.MachineID) != "" {
			cfg.MachineID = strings.TrimSpace(saved.MachineID)
		}
		if strings.TrimSpace(saved.Mode) != "" {
			cfg.Mode = strings.TrimSpace(saved.Mode)
		}
		cfg.RunLocalRunner = saved.RunLocalRunner
		if strings.TrimSpace(saved.PAShortcut) != "" {
			cfg.PAShortcut = strings.TrimSpace(saved.PAShortcut)
		}
	}

	if cfg.HubURL == "" {
		if raw, err := os.ReadFile(filepath.Join(expDir, "hub.yaml")); err == nil {
			var hc hubConfigFallback
			if yaml.Unmarshal(raw, &hc) == nil {
				if strings.TrimSpace(hc.HubOrigin) != "" {
					cfg.HubURL = hc.HubOrigin
				} else if strings.TrimSpace(hc.ListenAddr) != "" {
					cfg.HubURL = hc.ListenAddr
				}
				if cfg.DevToken == "" && strings.TrimSpace(hc.DevToken) != "" {
					cfg.DevToken = strings.TrimSpace(hc.DevToken)
				}
				if cfg.Mode == "" && cfg.HubURL != "" {
					cfg.Mode = "local"
				}
			}
		}
	}

	savedNormOrigin := ""
	if cfg.HubURL != "" {
		if norm, err := NormalizeHubURL(cfg.HubURL); err == nil {
			savedNormOrigin = norm
		}
	}

	overrideURL := ""
	if envURL := strings.TrimSpace(os.Getenv("ORBIT_HUB_URL")); envURL != "" {
		overrideURL = envURL
	}
	if flagURL := strings.TrimSpace(explicitURLFlag); flagURL != "" {
		overrideURL = flagURL
	}
	if overrideURL != "" {
		cfg.HubURL = overrideURL
	}

	if cfg.HubURL != "" {
		norm, err := NormalizeHubURL(cfg.HubURL)
		if err != nil {
			return cfg, err
		}
		cfg.HubURL = norm
		// Origin-scope saved credentials: never leak Hub A's token when overriding to Hub B!
		if overrideURL != "" && savedNormOrigin != "" && !sameOriginStrings(norm, savedNormOrigin) {
			cfg.ClientToken = ""
			cfg.DevToken = ""
			cfg.MachineID = ""
			cfg.Mode = "remote"
			cfg.RunLocalRunner = false
		}
	}

	if envClientTok := strings.TrimSpace(os.Getenv("ORBIT_CLIENT_TOKEN")); envClientTok != "" {
		cfg.ClientToken = envClientTok
	}
	if envDevTok := strings.TrimSpace(os.Getenv("ORBIT_DEV_TOKEN")); envDevTok != "" {
		cfg.DevToken = envDevTok
	}
	return cfg, nil
}

// SaveClientConfig atomically writes client.yaml with 0600 permissions.
func SaveClientConfig(configDir string, cfg ClientConfig) error {
	if cfg.HubURL != "" {
		norm, err := NormalizeHubURL(cfg.HubURL)
		if err != nil {
			return err
		}
		cfg.HubURL = norm
	}
	path := clientConfigPath(configDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// NormalizeHubURL normalizes user-provided Hub addresses (adding scheme if omitted and stripping trailing slash).
func NormalizeHubURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("hub URL is empty")
	}
	if !strings.Contains(s, "://") {
		if strings.HasPrefix(s, "127.0.0.1") || strings.HasPrefix(s, "localhost") || strings.HasPrefix(s, "[::1]") {
			s = "http://" + s
		} else {
			s = "https://" + s
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid hub URL %q", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("hub URL must use http or https scheme, got %q", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// ShouldOpenExternally returns true when a navigation URL should be handed off to the OS
// (IDE deep-links like cursor://, vscode://, zed://, antigravity://, or any cross-origin external HTTP(S) URL
// such as Ryugi :8443 canvases or GitHub links).
func ShouldOpenExternally(targetURL, hubURL, setupOrigin string) bool {
	u, err := url.Parse(strings.TrimSpace(targetURL))
	if err != nil || u.Scheme == "" {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "about", "data", "blob":
		return false
	case "cursor", "vscode", "zed", "antigravity", "mailto":
		return true
	}
	if scheme != "http" && scheme != "https" {
		return true
	}
	if setupOrigin != "" && sameOrigin(u, setupOrigin) {
		return false
	}
	if hubURL != "" && sameOrigin(u, hubURL) {
		return false
	}
	return true
}

func sameOrigin(u *url.URL, baseRaw string) bool {
	b, err := url.Parse(baseRaw)
	if err != nil || b.Host == "" {
		return false
	}
	return strings.EqualFold(u.Scheme, b.Scheme) && strings.EqualFold(u.Host, b.Host)
}

func sameOriginStrings(a, b string) bool {
	ua, err := url.Parse(strings.TrimSpace(a))
	if err != nil || ua.Host == "" {
		return false
	}
	return sameOrigin(ua, b)
}

// ParseJoinInput parses either a pasted `orbit join <url> <code>`, an invite link
// `<url>/#/join/<code>`, `<url> <code>`, or a plain Hub address `<url>`.
func ParseJoinInput(raw string) (hubURL, code string, noRunnerHint bool, err error) {
	hubURL, code, noRunnerHint, _, err = ParseJoinInputWithScope(raw)
	return hubURL, code, noRunnerHint, err
}

// ParseJoinInputWithScope parses join input including --no-runner / --runner-only / ?scope=... hints.
func ParseJoinInputWithScope(raw string) (hubURL, code string, noRunnerHint bool, runnerOnlyHint bool, err error) {
	s := strings.TrimSpace(raw)
	s = strings.Trim(s, `"'`)
	if s == "" {
		return "", "", false, false, fmt.Errorf("请输入地址或粘贴邀请命令")
	}
	tokens := strings.Fields(s)
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
			code = strings.TrimSpace(tokens[i+1])
			i++
		case strings.HasPrefix(tok, "--code="):
			code = strings.TrimSpace(strings.TrimPrefix(tok, "--code="))
		case strings.HasPrefix(tok, "-"):
		default:
			positional = append(positional, tok)
		}
	}
	if len(positional) == 0 {
		return "", "", false, false, fmt.Errorf("请输入有效的地址或邀请命令")
	}

	rawURL := positional[0]
	if code == "" && len(positional) >= 2 {
		code = strings.TrimSpace(positional[1])
	}
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

	norm, err := NormalizeHubURL(rawURL)
	if err != nil {
		return "", "", false, false, err
	}
	return norm, code, noRunnerHint, runnerOnlyHint, nil
}
