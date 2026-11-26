package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDesktopLocalSSHCandidatesOriginProtection(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config")
	content := `
Host build-box
    HostName build-box.example.ts.net
    User dev
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	hubOrigin := "https://hub.example.ts.net"
	setupOrigin := "http://127.0.0.1:45555"
	handler := newLocalSSHCandidatesHandler(
		func() string { return hubOrigin },
		setupOrigin,
		SSHMatchOptions{
			ConfigPath:     cfgPath,
			SSHDir:         dir,
			DisableSSHExec: true,
			DisableProbe:   true,
			DisableDNS:     true,
		},
	)

	// 1. Foreign origin must be rejected with 403 and NO Access-Control-Allow-Origin header.
	reqBad := httptest.NewRequest(http.MethodGet, "/api/local-ssh-candidates?host=build-box.example.ts.net", nil)
	reqBad.Header.Set("Origin", "https://evil.example.com")
	recBad := httptest.NewRecorder()
	handler.ServeHTTP(recBad, reqBad)
	if recBad.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for foreign origin, got %d", recBad.Code)
	}
	if recBad.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("must not emit Access-Control-Allow-Origin for forbidden origin")
	}

	// 2. Missing origin must be rejected.
	reqNoOrigin := httptest.NewRequest(http.MethodGet, "/api/local-ssh-candidates?host=build-box.example.ts.net", nil)
	recNoOrigin := httptest.NewRecorder()
	handler.ServeHTTP(recNoOrigin, reqNoOrigin)
	if recNoOrigin.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for missing origin, got %d", recNoOrigin.Code)
	}

	// 3. Connected Hub origin must succeed and return matching alias + exact CORS header.
	reqGood := httptest.NewRequest(http.MethodGet, "/api/local-ssh-candidates?host=build-box.example.ts.net&machineId=build-box", nil)
	reqGood.Header.Set("Origin", hubOrigin)
	recGood := httptest.NewRecorder()
	handler.ServeHTTP(recGood, reqGood)
	if recGood.Code != http.StatusOK {
		t.Fatalf("expected 200 for connected hub origin, got %d: %s", recGood.Code, recGood.Body.String())
	}
	if got := recGood.Header().Get("Access-Control-Allow-Origin"); got != hubOrigin {
		t.Fatalf("expected Access-Control-Allow-Origin=%q, got %q", hubOrigin, got)
	}
	var out SSHMatchResult
	if err := json.Unmarshal(recGood.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.LocalConfigAvailable || out.Recommended != "build-box" {
		t.Fatalf("expected recommended build-box, got %+v", out)
	}
}

func TestBuildDesktopOpenURLIncludesDesktopHelperPort(t *testing.T) {
	mgr := NewRuntimeManager()
	mgr.SetDesktopHelperPort(43219)
	got := mgr.BuildDesktopOpenURL(context.Background(), t.TempDir(), "https://hub.example.ts.net", "#/pa")
	if !strings.Contains(got, "orbitDesktop=1&desktopHelper=43219") {
		t.Fatalf("expected desktopHelper query param in URL, got %q", got)
	}
}

func TestDesktopSSHConfigIncludeInsideHostBlockPreservesContext(t *testing.T) {
	dir := t.TempDir()
	confD := filepath.Join(dir, "conf.d")
	if err := os.MkdirAll(confD, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confD, "opts.conf"), []byte("HostName 198.51.100.77\nPort 2200\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mainCfg := `
Host studio-box
    Include conf.d/opts.conf
    User bob
`
	cfgPath := filepath.Join(dir, "config")
	if err := os.WriteFile(cfgPath, []byte(mainCfg), 0o600); err != nil {
		t.Fatal(err)
	}

	res := MatchLocalSSHConfig(context.Background(), SSHTarget{
		MachineID:     "unrelated",
		TailscaleHost: "198.51.100.77",
	}, SSHMatchOptions{
		ConfigPath:     cfgPath,
		SSHDir:         dir,
		DisableSSHExec: true,
		DisableProbe:   true,
		DisableDNS:     true,
	})
	if res.Recommended != "studio-box" {
		t.Fatalf("P2-15 REGRESSION (desktop): expected studio-box, got %q (cands=%+v)", res.Recommended, res.Candidates)
	}
	if len(res.Candidates) == 0 || res.Candidates[0].HostName != "198.51.100.77" || res.Candidates[0].Port != 2200 || res.Candidates[0].User != "bob" {
		t.Fatalf("expected HostName/Port/User preserved across Include, got %+v", res.Candidates)
	}
}
