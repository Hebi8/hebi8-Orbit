package main

import (
	"os"
	"path/filepath"
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
		{"https://orbit.example.ts.net:8443/art/demo", true},
		{"https://github.com/Hebi8/hebi8-Orbit", true},
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
