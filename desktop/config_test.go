package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeHubURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"orbit.example.ts.net/", "https://orbit.example.ts.net"},
		{"https://orbit.example.ts.net/app/", "https://orbit.example.ts.net/app"},
		{"127.0.0.1:4310", "http://127.0.0.1:4310"},
		{"localhost:4310/", "http://localhost:4310"},
	}
	for _, tc := range cases {
		got, err := NormalizeHubURL(tc.in)
		if err != nil {
			t.Fatalf("NormalizeHubURL(%q) error: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("NormalizeHubURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestShouldOpenExternally(t *testing.T) {
	hub := "https://orbit.example.ts.net"
	setup := "http://127.0.0.1:51234"

	cases := []struct {
		url  string
		want bool
	}{
		{"https://orbit.example.ts.net/#/t/tsk_1", false},
		{"http://127.0.0.1:51234/", false},
		{"cursor://vscode-remote/ssh-remote+hub-node/tmp/repo", true},
		{"vscode://file/tmp/repo/main.go:10", true},
		{"zed://ssh/hub-node/tmp/repo", true},
		{"antigravity://file/tmp/repo", true},
		{"mailto:user@example.com", true},
		{"mailto:user@example.com?subject=Orbit", true},
		{"http://example.com/docs", true},
		{"https://orbit.example.ts.net:8443/art/demo", true},
		{"https://github.com/Hebi8/hebi8-Orbit", true},
		{"javascript:alert(1)", false},
		{"data:text/html,hi", false},
	}
	for _, tc := range cases {
		if got := ShouldOpenExternally(tc.url, hub, setup); got != tc.want {
			t.Errorf("ShouldOpenExternally(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

func TestLoadAndSaveClientConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORBIT_HUB_URL", "")

	// Fallback to hub.yaml when client.yaml does not exist
	if err := os.WriteFile(filepath.Join(dir, "hub.yaml"), []byte("hubOrigin: https://orbit.example.ts.net\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadClientConfig(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HubURL != "https://orbit.example.ts.net" {
		t.Fatalf("expected hub.yaml fallback, got %q", cfg.HubURL)
	}

	// Save client.yaml overrides hub.yaml
	if err := SaveClientConfig(dir, ClientConfig{HubURL: "http://127.0.0.1:4310", PAShortcut: "Alt+Shift+O"}); err != nil {
		t.Fatal(err)
	}
	cfg2, err := LoadClientConfig(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.HubURL != "http://127.0.0.1:4310" || cfg2.PAShortcut != "Alt+Shift+O" {
		t.Fatalf("unexpected saved config: %+v", cfg2)
	}
}

func TestParseJoinInput(t *testing.T) {
	cases := []struct {
		in       string
		wantURL  string
		wantCode string
		wantNoR  bool
	}{
		{"orbit join https://orbit.example.ts.net K7QM-2XPA", "https://orbit.example.ts.net", "K7QM-2XPA", false},
		{"https://orbit.example.ts.net/#/join/K7QM-2XPA", "https://orbit.example.ts.net", "K7QM-2XPA", false},
		{"orbit join https://orbit.example.ts.net K7QM-2XPA --no-runner", "https://orbit.example.ts.net", "K7QM-2XPA", true},
		{"https://orbit.example.ts.net", "https://orbit.example.ts.net", "", false},
	}
	for _, tc := range cases {
		u, c, noR, err := ParseJoinInput(tc.in)
		if err != nil {
			t.Fatalf("ParseJoinInput(%q) err=%v", tc.in, err)
		}
		if u != tc.wantURL || c != tc.wantCode || noR != tc.wantNoR {
			t.Fatalf("ParseJoinInput(%q) = (%q,%q,%v) want (%q,%q,%v)", tc.in, u, c, noR, tc.wantURL, tc.wantCode, tc.wantNoR)
		}
	}

	// Verify --runner-only and ?scope=runner / ?scope=client hints
	u, c, noR, rOnly, err := ParseJoinInputWithScope("orbit join https://orbit.example.ts.net K7QM-2XPA --runner-only")
	if err != nil || u != "https://orbit.example.ts.net" || c != "K7QM-2XPA" || noR || !rOnly {
		t.Fatalf("unexpected runner-only parse: %q %q noR=%v rOnly=%v err=%v", u, c, noR, rOnly, err)
	}
	_, _, noR2, rOnly2, err := ParseJoinInputWithScope("https://orbit.example.ts.net/#/join/K7QM-2XPA?scope=client")
	if err != nil || !noR2 || rOnly2 {
		t.Fatalf("unexpected scope=client parse: noR=%v rOnly=%v err=%v", noR2, rOnly2, err)
	}
}

func TestOriginScopedCredentialsAndNonDestructiveMachinesYAML(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORBIT_HUB_URL", "")
	t.Setenv("ORBIT_CLIENT_TOKEN", "")
	t.Setenv("ORBIT_DEV_TOKEN", "")

	// Save credentials for Hub A
	if err := SaveClientConfig(dir, ClientConfig{
		HubURL:         "https://hub-a.example.ts.net",
		ClientToken:    "ock_secret_for_a",
		DevToken:       "dev_secret_for_a",
		MachineID:      "mac-a",
		Mode:           "remote",
		RunLocalRunner: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Overriding to Hub B MUST strip Hub A's credentials (#6)
	cfgB, err := LoadClientConfig(dir, "https://hub-b.example.ts.net")
	if err != nil {
		t.Fatal(err)
	}
	if cfgB.ClientToken != "" || cfgB.DevToken != "" || cfgB.RunLocalRunner {
		t.Fatalf("expected credentials stripped for different origin, got %+v", cfgB)
	}

	// Same origin keeps credentials
	cfgA, err := LoadClientConfig(dir, "https://hub-a.example.ts.net")
	if err != nil {
		t.Fatal(err)
	}
	if cfgA.ClientToken != "ock_secret_for_a" || !cfgA.RunLocalRunner {
		t.Fatalf("expected same-origin credentials preserved, got %+v", cfgA)
	}

	// Existing multi-machine machines.yaml with missing runner.token MUST preserve other nodes (#1)
	multiYAML := `schemaVersion: 1
machines:
  local-box:
    displayName: "Local Box"
    transport: local_ws
    capabilities: [primary-dev]
  remote-gpu:
    displayName: "GPU Box"
    transport: remote_ws
    tokenHash: "sha256:keep-me"
    capabilities: [gpu]
`
	if err := os.WriteFile(filepath.Join(dir, "machines.yaml"), []byte(multiYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureLocalRunnerTokenAndMachine(dir, "local-box"); err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(filepath.Join(dir, "machines.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(updated)
	if !strings.Contains(s, "remote-gpu") || !strings.Contains(s, "sha256:keep-me") {
		t.Fatalf("existing remote machine was clobbered:\n%s", s)
	}
	if _, err := os.Stat(filepath.Join(dir, "runner.token")); err != nil {
		t.Fatalf("expected runner.token to be created: %v", err)
	}
}

func TestJoinRemoteRequiresPairingCode(t *testing.T) {
	mgr := NewRuntimeManager()
	_, err := mgr.JoinRemote(t.Context(), t.TempDir(), "https://orbit.example.ts.net", true)
	if err == nil {
		t.Fatalf("expected JoinRemote without pairing code to return error")
	}
}

func TestMultiNetworkProfilesPreserveLocalAndSwitch(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORBIT_HUB_URL", "")
	t.Setenv("ORBIT_CLIENT_TOKEN", "")
	t.Setenv("ORBIT_DEV_TOKEN", "")

	var cfg ClientConfig
	cfg.PAShortcut = "CmdOrCtrl+Shift+O"
	cfg.UpsertNetwork(NetworkProfile{
		ID:       "local",
		Name:     "本机工作区",
		Mode:     "local",
		HubURL:   "http://127.0.0.1:4310",
		DevToken: "local_dev_tok",
	})
	remote := cfg.UpsertNetwork(NetworkProfile{
		Mode:        "remote",
		Name:        "lab-hub",
		HubURL:      "https://lab.example.ts.net",
		ClientToken: "ock_remote_tok",
		MachineID:   "laptop-1",
	})
	cfg.ActivateNetwork(remote.ID)
	if err := SaveClientConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadClientConfig(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Networks) != 2 {
		t.Fatalf("expected 2 saved networks, got %d", len(loaded.Networks))
	}
	if loaded.ActiveNetworkID != remote.ID || loaded.ClientToken != "ock_remote_tok" {
		t.Fatalf("expected active remote profile, got %+v", loaded)
	}

	// Switching to local preserves remote and restores local dev token
	if _, ok := loaded.ActivateNetwork("local"); !ok {
		t.Fatalf("expected local profile to activate")
	}
	if loaded.HubURL != "http://127.0.0.1:4310" || loaded.DevToken != "local_dev_tok" || loaded.ClientToken != "" {
		t.Fatalf("unexpected local profile state: %+v", loaded)
	}

	// Origin override to saved remote resolves its token, while unknown Hub C strips tokens
	cfgRemote, err := LoadClientConfig(dir, "https://lab.example.ts.net")
	if err != nil || cfgRemote.ClientToken != "ock_remote_tok" {
		t.Fatalf("expected saved remote token on origin match, got %+v err=%v", cfgRemote, err)
	}
	cfgUnknown, err := LoadClientConfig(dir, "https://other.example.ts.net")
	if err != nil || cfgUnknown.ClientToken != "" || cfgUnknown.DevToken != "" {
		t.Fatalf("expected stripped credentials on unknown origin, got %+v err=%v", cfgUnknown, err)
	}

	// Removing remote network keeps local intact
	if !loaded.RemoveNetwork(remote.ID) {
		t.Fatalf("expected RemoveNetwork to succeed")
	}
	if loaded.RemoveNetwork("local") {
		t.Fatalf("expected local network to be protected from removal")
	}
	if len(loaded.Networks) != 1 || loaded.Networks[0].ID != "local" {
		t.Fatalf("unexpected networks after removal: %+v", loaded.Networks)
	}
}

func TestRuntimeManagerSwitchNetworkSavesConfigFirstAndSerializes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORBIT_HUB_URL", "")
	t.Setenv("ORBIT_CLIENT_TOKEN", "")
	t.Setenv("ORBIT_DEV_TOKEN", "")

	fakeHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true,"service":"hebi8-orbit-hub"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer fakeHub.Close()

	var cfg ClientConfig
	cfg.PAShortcut = "CmdOrCtrl+Shift+O"
	cfg.UpsertNetwork(NetworkProfile{
		ID:       "local",
		Name:     "本机工作区",
		Mode:     "local",
		HubURL:   "http://127.0.0.1:4310",
		DevToken: "local_dev",
	})
	remote := cfg.UpsertNetwork(NetworkProfile{
		Mode:           "remote",
		Name:           "remote-hub",
		HubURL:         fakeHub.URL,
		ClientToken:    "ock_test_switch",
		RunLocalRunner: false,
	})
	cfg.ActivateNetwork("local")
	if err := SaveClientConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}

	mgr := NewRuntimeManager()
	prof, err := mgr.SwitchNetwork(t.Context(), dir, remote.ID)
	if err != nil {
		t.Fatalf("SwitchNetwork failed: %v", err)
	}
	if prof.ID != remote.ID || prof.HubURL != fakeHub.URL {
		t.Fatalf("unexpected switched profile: %+v", prof)
	}

	afterSwitch, err := LoadClientConfig(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if afterSwitch.ActiveNetworkID != remote.ID {
		t.Fatalf("expected activeNetworkId=%q, got %q", remote.ID, afterSwitch.ActiveNetworkID)
	}

	// RemoveNetwork serialized via switchMu cleanly removes active remote profile
	afterRemove, err := mgr.RemoveNetwork(dir, remote.ID)
	if err != nil {
		t.Fatalf("RemoveNetwork failed: %v", err)
	}
	if len(afterRemove.Networks) != 1 || afterRemove.Networks[0].ID != "local" {
		t.Fatalf("unexpected networks after RemoveNetwork: %+v", afterRemove.Networks)
	}
}

func TestStartOrAttachLocalFailsWithoutStoppingRemoteRunnerWhenSaveFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORBIT_HUB_URL", "")
	t.Setenv("ORBIT_CLIENT_TOKEN", "")
	t.Setenv("ORBIT_DEV_TOKEN", "")

	// Fake healthy local Hub so StartOrAttachLocal takes the attach path
	fakeLocalHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"service":"hebi8-orbit-hub"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer fakeLocalHub.Close()

	listenAddr := strings.TrimPrefix(fakeLocalHub.URL, "http://")
	if err := os.WriteFile(filepath.Join(dir, "hub.yaml"), []byte("listenAddr: "+listenAddr+"\ndataDir: "+filepath.Join(dir, "data")+"\ndevToken: localdev\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Make client.yaml.tmp a directory so SaveClientConfig deterministically fails (even under root)
	if err := os.MkdirAll(filepath.Join(dir, "client.yaml.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}

	// Register a synthetic managed remote runner entry to assert it is NOT stopped when save fails
	mgr := NewRuntimeManager()
	fakeDone := make(chan struct{})
	mgr.mu.Lock()
	mgr.procs = append(mgr.procs, &managedProc{name: "orbit-runner-remote", done: fakeDone})
	mgr.mu.Unlock()

	_, err := mgr.SwitchNetwork(t.Context(), dir, "local")
	if err == nil {
		t.Fatalf("expected SwitchNetwork('local') to return error when SaveClientConfig fails")
	}
	if !mgr.hasManaged("orbit-runner-remote") {
		t.Fatalf("expected existing orbit-runner-remote to remain untouched when SaveClientConfig fails")
	}
}
