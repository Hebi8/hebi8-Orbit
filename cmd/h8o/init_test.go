package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"gopkg.in/yaml.v3"
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
			args:     []string{"h8o join https://orbit.example.ts.net K7QM-2XPA --no-runner"},
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
	// Case 1: h8o join <url> <code> --service
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

	// Case 2: h8o agents test agy --machine mini --json
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

func TestCLIJoin_PreservesMultiNetworkClientYAMLAndLocalRunnerToken(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "cfg")
	dataDir := filepath.Join(tmp, "data")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// 1. Start with existing local runner.token + local client.yaml profile
	localRunnerToken := "local-runner-secret-must-not-be-overwritten"
	if err := os.WriteFile(filepath.Join(cfgDir, "runner.token"), []byte(localRunnerToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeClientConfigFull(filepath.Join(cfgDir, "client.yaml"), "http://127.0.0.1:4310", "devtok_local", "", "local-box", "local"); err != nil {
		t.Fatalf("write initial local client.yaml: %v", err)
	}

	makeFakeHub := func(wsName, machID, cTok, rTok string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/pairing/claim" && r.Method == http.MethodPost {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(protocol.ClaimPairingResponse{
					MachineID:     machID,
					DisplayName:   machID,
					WorkspaceName: wsName,
					ClientToken:   cTok,
					RunnerToken:   rTok,
				})
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
	}

	hubA := makeFakeHub("Hub A", "node-a", "ock_token_a_1234567890", "ort_runner_a_1234567890")
	defer hubA.Close()
	hubB := makeFakeHub("Hub B", "node-b", "ock_token_b_1234567890", "ort_runner_b_1234567890")
	defer hubB.Close()

	var out, errBuf bytes.Buffer
	if err := runJoin(context.Background(), []string{
		hubA.URL, "AAAA-BBBB", "--no-start", "--config-dir", cfgDir, "--data-dir", dataDir,
	}, &out, &errBuf); err != nil {
		t.Fatalf("join hubA failed: %v (%s)", err, errBuf.String())
	}
	if err := runJoin(context.Background(), []string{
		hubB.URL, "CCCC-DDDD", "--no-start", "--config-dir", cfgDir, "--data-dir", dataDir,
	}, &out, &errBuf); err != nil {
		t.Fatalf("join hubB failed: %v (%s)", err, errBuf.String())
	}

	// Top-level local runner.token MUST remain untouched!
	gotLocalTok, err := os.ReadFile(filepath.Join(cfgDir, "runner.token"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(gotLocalTok)) != localRunnerToken {
		t.Fatalf("local runner.token was overwritten! got %q want %q", strings.TrimSpace(string(gotLocalTok)), localRunnerToken)
	}

	// client.yaml MUST contain all 3 network profiles (local + hubA + hubB) and activeNetworkId == hubB
	rawClient, err := os.ReadFile(filepath.Join(cfgDir, "client.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg cliClientConfig
	if err := yaml.Unmarshal(rawClient, &cfg); err != nil {
		t.Fatalf("parse client.yaml: %v", err)
	}
	if len(cfg.Networks) != 3 {
		t.Fatalf("expected 3 saved networks (local + hubA + hubB), got %d: %+v", len(cfg.Networks), cfg.Networks)
	}
	wantNetA := cliNetworkIDFor("remote", hubA.URL)
	wantNetB := cliNetworkIDFor("remote", hubB.URL)
	if cfg.ActiveNetworkID != wantNetB {
		t.Fatalf("expected activeNetworkId=%q, got %q", wantNetB, cfg.ActiveNetworkID)
	}
	for _, tc := range []struct {
		netID, wantTok string
	}{
		{wantNetA, "ort_runner_a_1234567890"},
		{wantNetB, "ort_runner_b_1234567890"},
	} {
		tokBytes, err := os.ReadFile(filepath.Join(cfgDir, "remotes", tc.netID, "runner.token"))
		if err != nil {
			t.Fatalf("missing isolated remote runner.token for %s: %v", tc.netID, err)
		}
		if strings.TrimSpace(string(tokBytes)) != tc.wantTok {
			t.Fatalf("isolated token mismatch for %s: got %q want %q", tc.netID, strings.TrimSpace(string(tokBytes)), tc.wantTok)
		}
		if _, err := os.Stat(filepath.Join(cfgDir, "remotes", tc.netID, "runner.yaml")); err != nil {
			t.Fatalf("missing isolated remote runner.yaml for %s: %v", tc.netID, err)
		}
	}

	// Also verify resolveHubAuth picks up hubA's token when --hub=hubA.URL is passed!
	_, _, _, tokA := resolveHubAuth(cfgDir, dataDir, hubA.URL, "")
	if tokA != "ock_token_a_1234567890" {
		t.Fatalf("resolveHubAuth for hubA returned %q, want ock_token_a_1234567890", tokA)
	}

	// Running `orbit init --hub --force` afterwards MUST preserve saved remote network profiles!
	var initOut bytes.Buffer
	if err := initHubMode(cfgDir, dataDir, "local-box", "Local WS", "127.0.0.1:4310", "", true, &initOut); err != nil {
		t.Fatalf("initHubMode failed: %v", err)
	}
	rawAfterHub, err := os.ReadFile(filepath.Join(cfgDir, "client.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cfgAfter cliClientConfig
	if err := yaml.Unmarshal(rawAfterHub, &cfgAfter); err != nil {
		t.Fatalf("parse client.yaml after initHubMode: %v", err)
	}
	if len(cfgAfter.Networks) != 3 {
		t.Fatalf("initHubMode wiped remote profiles! expected 3 networks, got %d: %+v", len(cfgAfter.Networks), cfgAfter.Networks)
	}
	if cfgAfter.ActiveNetworkID != "local" {
		t.Fatalf("expected activeNetworkId=local after initHubMode, got %q", cfgAfter.ActiveNetworkID)
	}
}

func TestRunPA_QueuedFollowupTracksExactDispatchedAttemptAndHandlesDelete(t *testing.T) {
	// Case 1: Queued PA follow-up polls GET /api/tasks/{taskId}/queue/{inputId} directly, so even if
	// timeline has >500 events and omits queue.updated completely, it still resolves exact dispatchedAttemptId!
	var pollCount int
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/tasks/tsk_pa_q/followup":
			_ = json.NewEncoder(w).Encode(protocol.FollowupTaskResponse{
				Mode:         protocol.FollowupModeQueue,
				PendingInput: &protocol.PendingInput{InputID: "inp_mine", State: protocol.PendingInputQueued},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/tasks/tsk_pa_q/queue/inp_mine":
			pollCount++
			if pollCount == 1 {
				_ = json.NewEncoder(w).Encode(protocol.PendingInput{
					InputID: "inp_mine",
					TaskID:  "tsk_pa_q",
					State:   protocol.PendingInputQueued,
				})
				return
			}
			_ = json.NewEncoder(w).Encode(protocol.PendingInput{
				InputID:             "inp_mine",
				TaskID:              "tsk_pa_q",
				State:               protocol.PendingInputDispatched,
				DispatchedAttemptID: "att_mine",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/tasks/tsk_pa_q/timeline":
			// Deliberately return empty Events (simulating >500 events pushing queue.updated off page)
			_ = json.NewEncoder(w).Encode(protocol.TaskTimelineResponse{
				Attempts: []protocol.AttemptView{
					{AttemptID: "att_prev", State: protocol.AttemptSucceeded},
					{AttemptID: "att_mine", State: protocol.AttemptSucceeded},
					{AttemptID: "att_unrelated_later", State: protocol.AttemptSucceeded},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/attempts/att_mine/raw":
			_ = json.NewEncoder(w).Encode(protocol.AttemptRawEventsResponse{
				AttemptID: "att_mine",
				Events: []protocol.EventRecord{
					{Seq: 1, Kind: protocol.EventACPMessageBatch, Payload: map[string]any{"text": "EXACT_MY_REPLY"}},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/attempts/att_unrelated_later/raw":
			_ = json.NewEncoder(w).Encode(protocol.AttemptRawEventsResponse{
				AttemptID: "att_unrelated_later",
				Events: []protocol.EventRecord{
					{Seq: 1, Kind: protocol.EventACPMessageBatch, Payload: map[string]any{"text": "WRONG_UNRELATED_REPLY"}},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv1.Close()

	var stdout, stderr bytes.Buffer
	if err := runPA(context.Background(), []string{
		"--task", "tsk_pa_q", "--hub", srv1.URL, "--timeout", "5", "my queued question",
	}, &stdout, &stderr); err != nil {
		t.Fatalf("runPA queued failed: %v", err)
	}
	if !strings.Contains(stdout.String(), "EXACT_MY_REPLY") || strings.Contains(stdout.String(), "WRONG_UNRELATED_REPLY") {
		t.Fatalf("expected EXACT_MY_REPLY, got %q", stdout.String())
	}

	// Case 2: Queued PA input deleted before dispatch (404 on /queue/{inputId}) MUST fail cleanly with error
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/tasks/tsk_pa_del/followup":
			_ = json.NewEncoder(w).Encode(protocol.FollowupTaskResponse{
				Mode:         protocol.FollowupModeQueue,
				PendingInput: &protocol.PendingInput{InputID: "inp_deleted", State: protocol.PendingInputQueued},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/tasks/tsk_pa_del/queue/inp_deleted":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"ERR_NOT_FOUND","message":"pending input deleted"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv2.Close()

	stdout.Reset()
	stderr.Reset()
	err := runPA(context.Background(), []string{
		"--task", "tsk_pa_del", "--hub", srv2.URL, "--timeout", "5", "will be deleted",
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "inp_deleted") {
		t.Fatalf("expected explicit error for deleted queued PA input, got err=%v", err)
	}
}
