package main

import (
	"bytes"
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

func TestInitConnectAndInteractiveWizard(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" {
			_, _ = w.Write([]byte(`{"ok":true,"version":"0.2.0"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	cfgDir := t.TempDir()
	var out, errBuf bytes.Buffer
	if err := runInit(context.Background(), []string{"--config-dir", cfgDir, "--connect", ts.URL}, nil, &out, &errBuf); err != nil {
		t.Fatalf("init --connect failed: %v (stderr=%s)", err, errBuf.String())
	}
	if !strings.Contains(out.String(), "[OK  ] Connected to Hub") {
		t.Fatalf("expected connected health output, got: %s", out.String())
	}

	t.Setenv("ORBIT_HUB_URL", "")
	t.Setenv("ORBIT_DEV_TOKEN", "")
	hubURL, _, _ := resolveHubDefaults(cfgDir, "", "", "")
	if hubURL != ts.URL {
		t.Fatalf("expected resolved hubURL %q, got %q", ts.URL, hubURL)
	}

	// Interactive mode selecting [2] join / connect
	cfgDir2 := t.TempDir()
	out.Reset()
	errBuf.Reset()
	in := strings.NewReader("2\n" + ts.URL + "\n")
	if err := runInit(context.Background(), []string{"--config-dir", cfgDir2}, in, &out, &errBuf); err != nil {
		t.Fatalf("interactive init connect failed: %v", err)
	}
	hubURL2, _, _ := resolveHubDefaults(cfgDir2, "", "", "")
	if hubURL2 != ts.URL {
		t.Fatalf("expected interactive hubURL %q, got %q", ts.URL, hubURL2)
	}
}

func TestParseJoinInput(t *testing.T) {
	cases := []struct {
		args     []string
		codeFlag string
		wantURL  string
		wantCode string
		wantNoR  bool
	}{
		{
			args:     []string{"https://orbit.example.ts.net", "K7QM-2XPA"},
			wantURL:  "https://orbit.example.ts.net",
			wantCode: "K7QM-2XPA",
		},
		{
			args:     []string{"orbit join https://orbit.example.ts.net K7QM-2XPA --no-runner"},
			wantURL:  "https://orbit.example.ts.net",
			wantCode: "K7QM-2XPA",
			wantNoR:  true,
		},
		{
			args:     []string{"https://orbit.example.ts.net/#/join/K7QM-2XPA"},
			wantURL:  "https://orbit.example.ts.net",
			wantCode: "K7QM-2XPA",
		},
		{
			args:     []string{"https://orbit.example.ts.net"},
			codeFlag: "ABCD-1234",
			wantURL:  "https://orbit.example.ts.net",
			wantCode: "ABCD-1234",
		},
	}
	for _, tc := range cases {
		u, c, noR, err := parseJoinInput(tc.args, tc.codeFlag)
		if err != nil {
			t.Fatalf("parseJoinInput(%v) err=%v", tc.args, err)
		}
		if u != tc.wantURL || c != tc.wantCode || noR != tc.wantNoR {
			t.Fatalf("parseJoinInput(%v) = (%q, %q, %v), want (%q, %q, %v)",
				tc.args, u, c, noR, tc.wantURL, tc.wantCode, tc.wantNoR)
		}
	}
}

func TestInitHubAndProject(t *testing.T) {
	cfgDir := t.TempDir()
	dataDir := t.TempDir()

	var out, errBuf bytes.Buffer
	if err := runInit(context.Background(), []string{
		"--hub",
		"--config-dir", cfgDir,
		"--data-dir", dataDir,
		"--machine", "dev-box",
		"--workspace", "Test LAN",
		"--listen", "127.0.0.1:4399",
	}, nil, &out, &errBuf); err != nil {
		t.Fatalf("init --hub failed: %v (stderr=%s)", err, errBuf.String())
	}

	hubCfg, err := protocol.LoadHubConfig(filepath.Join(cfgDir, "hub.yaml"))
	if err != nil {
		t.Fatalf("load generated hub.yaml: %v", err)
	}
	if hubCfg.WorkspaceName != "Test LAN" || hubCfg.ListenAddr != "127.0.0.1:4399" || hubCfg.DevToken == "" {
		t.Fatalf("unexpected generated hub.yaml: %+v", hubCfg)
	}

	machinesCfg, err := protocol.LoadMachinesConfig(filepath.Join(cfgDir, "machines.yaml"))
	if err != nil {
		t.Fatalf("load generated machines.yaml: %v", err)
	}
	m, ok := machinesCfg.Machines["dev-box"]
	if !ok || !strings.HasPrefix(m.TokenHash, "sha256:") {
		t.Fatalf("unexpected generated machine config: %+v", machinesCfg.Machines)
	}

	// Create a temp git repo and register it via `orbit init --project`
	repoDir := filepath.Join(t.TempDir(), "sample-app")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	if outGit, err := exec.Command("git", "init", "-b", "main", repoDir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, string(outGit))
	}

	out.Reset()
	errBuf.Reset()
	if err := runInit(context.Background(), []string{
		"--project",
		"--config-dir", cfgDir,
		"--data-dir", dataDir,
		"--machine", "dev-box",
		"--adapter", "claude",
		repoDir,
	}, nil, &out, &errBuf); err != nil {
		t.Fatalf("init --project failed: %v (stderr=%s)", err, errBuf.String())
	}

	projCfg, err := protocol.LoadProjectsConfig(filepath.Join(cfgDir, "projects.yaml"))
	if err != nil {
		t.Fatalf("load projects.yaml: %v", err)
	}
	p, ok := projCfg.Projects["sample-app"]
	if !ok || p.DefaultAdapter != protocol.AdapterClaude {
		t.Fatalf("expected sample-app with claude adapter, got: %+v", projCfg.Projects)
	}
}

func TestInitRunnerGeneratesTokenAndSnippet(t *testing.T) {
	cfgDir := t.TempDir()
	var out, errBuf bytes.Buffer
	if err := runInit(context.Background(), []string{
		"--runner",
		"--config-dir", cfgDir,
		"--hub-url", "https://orbit.example.ts.net",
		"--machine", "mac-node",
	}, nil, &out, &errBuf); err != nil {
		t.Fatalf("init --runner failed: %v", err)
	}
	if !strings.Contains(out.String(), "tokenHash: \"sha256:") {
		t.Fatalf("expected tokenHash instructions in output, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "wss://orbit.example.ts.net/ws/runner") {
		t.Fatalf("expected wss runner URL in output, got:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(cfgDir, "runner.token")); err != nil {
		t.Fatalf("expected runner.token to be created: %v", err)
	}
}

func TestParseFlagsPermuted(t *testing.T) {
	// Case 1: orbit join <url> <code> --service
	fsJoin := flag.NewFlagSet("join", flag.ContinueOnError)
	useService := fsJoin.Bool("service", false, "")
	noRunner := fsJoin.Bool("no-runner", false, "")
	if err := parseFlagsPermuted(fsJoin, []string{"https://orbit.example.ts.net", "ABCD-EFGH", "--service"}); err != nil {
		t.Fatalf("parseFlagsPermuted join failed: %v", err)
	}
	if !*useService || *noRunner {
		t.Fatalf("expected service=true noRunner=false, got service=%v noRunner=%v", *useService, *noRunner)
	}
	if got := strings.Join(fsJoin.Args(), ","); got != "https://orbit.example.ts.net,ABCD-EFGH" {
		t.Fatalf("unexpected join positional args: %q", got)
	}

	// Case 2: orbit agents test agy --machine mini --json
	fsAgents := flag.NewFlagSet("agents", flag.ContinueOnError)
	machine := fsAgents.String("machine", "", "")
	jsonOut := fsAgents.Bool("json", false, "")
	if err := parseFlagsPermuted(fsAgents, []string{"agy", "--machine", "mini", "--json"}); err != nil {
		t.Fatalf("parseFlagsPermuted agents failed: %v", err)
	}
	if *machine != "mini" || !*jsonOut {
		t.Fatalf("expected machine=mini json=true, got machine=%q json=%v", *machine, *jsonOut)
	}
	if got := strings.Join(fsAgents.Args(), ","); got != "agy" {
		t.Fatalf("unexpected agents positional args: %q", got)
	}

	// Case 3: double-dash stops flag scanning
	fsDash := flag.NewFlagSet("dash", flag.ContinueOnError)
	flagVal := fsDash.String("machine", "", "")
	if err := parseFlagsPermuted(fsDash, []string{"pos1", "--", "--machine", "literal"}); err != nil {
		t.Fatalf("parseFlagsPermuted -- failed: %v", err)
	}
	if *flagVal != "" || strings.Join(fsDash.Args(), ",") != "pos1,--machine,literal" {
		t.Fatalf("unexpected -- parse: machine=%q args=%v", *flagVal, fsDash.Args())
	}
}
