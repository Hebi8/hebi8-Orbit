package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/Hebi8/hebi8-Orbit/internal/gitops"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

func initTestGitRepoAndMirror(t *testing.T) (repoDir, mirrorDir, baseSHA string) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	repoDir = filepath.Join(tmp, "repo")
	mirrorDir = filepath.Join(tmp, "mirror.git")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	runGit(repoDir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(repoDir, "add", ".")
	runGit(repoDir, "commit", "-m", "initial")
	baseSHA = runGit(repoDir, "rev-parse", "HEAD")

	if err := gitops.EnsureBareMirror(ctx, mirrorDir); err != nil {
		t.Fatalf("ensure bare mirror: %v", err)
	}
	return repoDir, mirrorDir, baseSHA
}

func TestWorktreeAutoDeliver_AfterMidTurnDeliverAndErrorNotClean(t *testing.T) {
	ctx := context.Background()
	repoDir, mirrorDir, baseSHA := initTestGitRepoAndMirror(t)
	dataDir := t.TempDir()

	drv := &stubAdapterDriver{}
	r, err := New(ctx, Config{
		MachineID:  "test-node",
		DataDir:    dataDir,
		SocketPath: "none",
		Driver:     drv,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatalf("runner.Start: %v", err)
	}
	defer func() { _ = r.Close(context.Background()) }()

	// Scenario A: Agent runs mid-turn `orbit deliver` for file A, then modifies file B before end_turn.
	// Auto-wrapup MUST detect new changes relative to the mid-turn delivery and publish a second delivery!
	drv.onPrompt = func(pctx context.Context, _ acp.SessionId, wt, _ string) (acp.PromptResponse, error) {
		if err := os.WriteFile(filepath.Join(wt, "a.txt"), []byte("part 1\n"), 0o644); err != nil {
			t.Errorf("write a.txt: %v", err)
		}
		if _, err := r.HandleLocalDeliver(pctx, LocalDeliverRequest{
			AttemptID: "att_mid_1",
			Summary:   "mid-turn checkpoint",
			Final:     false,
		}); err != nil {
			t.Errorf("mid-turn deliver failed: %v", err)
		}
		// Make additional edits AFTER mid-turn deliver!
		if err := os.WriteFile(filepath.Join(wt, "b.txt"), []byte("part 2 after deliver\n"), 0o644); err != nil {
			t.Errorf("write b.txt: %v", err)
		}
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:      "att_mid_1",
		TaskID:         "tsk_mid_1",
		ProjectID:      "proj_mid",
		WBSessionID:    "ses_mid_1",
		LeaseToken:     "tok_mid_1",
		Adapter:        protocol.AdapterAgy,
		Isolation:      protocol.IsolationWorktree,
		RepoPath:       repoDir,
		MirrorBarePath: mirrorDir,
		BaseSHA:        baseSHA,
		Prompt:         "edit a then deliver then edit b",
	})
	if err := r.WaitAttemptDone(ctx, "att_mid_1"); err != nil {
		t.Fatalf("wait att_mid_1: %v", err)
	}

	_, items, err := r.outbox.ReadUnacked(ctx, "att_mid_1", 0, 100)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	deliveryCount := 0
	var lastDeliveryID string
	var finalState string
	for _, it := range items {
		if it.FrameType == protocol.OutboxFrameDeliveryPublished {
			deliveryCount++
			if id, ok := it.Payload["deliveryId"].(string); ok {
				lastDeliveryID = id
			}
		}
		if it.FrameType == protocol.OutboxFrameState {
			if st, ok := it.Payload["state"].(string); ok {
				finalState = st
			}
		}
	}
	if deliveryCount != 2 {
		t.Fatalf("expected 2 deliveries (1 mid-turn + 1 auto-wrapup for post-deliver edits), got %d", deliveryCount)
	}
	if finalState != string(protocol.AttemptSucceeded) {
		t.Fatalf("expected final state succeeded, got %q", finalState)
	}
	// Verify the second delivery ref actually contains b.txt in the bare mirror
	lsTree, err := exec.CommandContext(ctx, "git", "--git-dir", mirrorDir, "ls-tree", "-r", "--name-only", "refs/orbit/deliveries/"+lastDeliveryID).CombinedOutput()
	if err != nil {
		t.Fatalf("ls-tree latest delivery: %v (%s)", err, string(lsTree))
	}
	if !strings.Contains(string(lsTree), "b.txt") {
		t.Fatalf("expected auto-wrapup delivery %s to contain b.txt, got:\n%s", lastDeliveryID, string(lsTree))
	}

	// Scenario B: Corrupting the worktree (.git deleted) MUST cause worktreeHasChanges error and FAIL the attempt (never report clean success!).
	drv.onPrompt = func(_ context.Context, _ acp.SessionId, wt, _ string) (acp.PromptResponse, error) {
		_ = os.Remove(filepath.Join(wt, ".git"))
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:      "att_broken_wt",
		TaskID:         "tsk_broken_wt",
		ProjectID:      "proj_mid",
		WBSessionID:    "ses_broken_wt",
		LeaseToken:     "tok_broken_wt",
		Adapter:        protocol.AdapterAgy,
		Isolation:      protocol.IsolationWorktree,
		RepoPath:       repoDir,
		MirrorBarePath: mirrorDir,
		BaseSHA:        baseSHA,
		Prompt:         "corrupt git metadata",
	})
	if err := r.WaitAttemptDone(ctx, "att_broken_wt"); err != nil {
		t.Fatalf("wait att_broken_wt: %v", err)
	}
	_, brokenItems, err := r.outbox.ReadUnacked(ctx, "att_broken_wt", 0, 100)
	if err != nil {
		t.Fatalf("read broken outbox: %v", err)
	}
	var brokenState, brokenErrCode string
	for _, it := range brokenItems {
		if it.FrameType == protocol.OutboxFrameState {
			if st, ok := it.Payload["state"].(string); ok {
				brokenState = st
			}
			if ec, ok := it.Payload["errorCode"].(string); ok {
				brokenErrCode = ec
			}
		}
	}
	if brokenState != string(protocol.AttemptFailed) || brokenErrCode != "WORKTREE_CHECK_FAILED" {
		t.Fatalf("expected failed/WORKTREE_CHECK_FAILED when git status fails, got state=%q code=%q", brokenState, brokenErrCode)
	}
}

func TestAutoDeliverWorktree_TransientNetworkFailureRetainedForRetry(t *testing.T) {
	ctx := context.Background()
	repoDir, _, baseSHA := initTestGitRepoAndMirror(t)
	dataDir := t.TempDir()

	var putAttempts atomic.Int32
	fakeHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/mirror-heads") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"heads":[]}`))
			return
		}
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/bundle") {
			n := putAttempts.Add(1)
			if n == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`temporary outage`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer fakeHub.Close()

	drv := &stubAdapterDriver{}
	drv.onPrompt = func(_ context.Context, _ acp.SessionId, wt, _ string) (acp.PromptResponse, error) {
		if err := os.WriteFile(filepath.Join(wt, "transient.txt"), []byte("retry me\n"), 0o644); err != nil {
			t.Errorf("write transient.txt: %v", err)
		}
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	r, err := New(ctx, Config{
		MachineID:             "remote-node",
		HubHTTPURL:            fakeHub.URL,
		DataDir:               dataDir,
		SocketPath:            "none",
		Driver:                drv,
		DeliveryRetryInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatalf("runner.Start: %v", err)
	}
	defer func() { _ = r.Close(context.Background()) }()

	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:      "att_transient_1",
		TaskID:         "tsk_transient_1",
		ProjectID:      "proj_remote",
		WBSessionID:    "ses_transient_1",
		LeaseToken:     "tok_transient_1",
		Adapter:        protocol.AdapterAgy,
		Isolation:      protocol.IsolationWorktree,
		RepoPath:       repoDir,
		MirrorBarePath: "orbit-hub://proj_remote",
		BaseSHA:        baseSHA,
		Prompt:         "edit file on remote",
	})

	waitCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := r.WaitAttemptDone(waitCtx, "att_transient_1"); err != nil {
		t.Fatalf("wait att_transient_1: %v", err)
	}

	if got := putAttempts.Load(); got < 2 {
		t.Fatalf("expected at least 2 bundle upload attempts (503 then 200), got %d", got)
	}

	_, items, err := r.outbox.ReadUnacked(ctx, "att_transient_1", 0, 100)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	sawDelivery := false
	finalState := ""
	for _, it := range items {
		if it.FrameType == protocol.OutboxFrameDeliveryPublished {
			sawDelivery = true
		}
		if it.FrameType == protocol.OutboxFrameState {
			if st, ok := it.Payload["state"].(string); ok {
				finalState = st
			}
		}
	}
	if !sawDelivery || finalState != string(protocol.AttemptSucceeded) {
		t.Fatalf("expected delivery_published + succeeded after transient retry, got sawDelivery=%v finalState=%q", sawDelivery, finalState)
	}
}

func TestApplyAndVerifyConfig_LegacyModesAliasEffortAndExplicitErrors(t *testing.T) {
	ctx := context.Background()

	// 1. Uncategorized effort/reasoning_effort IDs MUST match resolveConfigOptionTarget
	uncatEffortOpts := []acp.SessionConfigOption{
		{
			Select: &acp.SessionConfigOptionSelect{
				Id:           acp.SessionConfigId("effort"),
				Category:     nil,
				CurrentValue: acp.SessionConfigValueId("medium"),
				Options: acp.SessionConfigSelectOptions{
					Ungrouped: &acp.SessionConfigSelectOptionsUngrouped{
						{Value: "low", Name: "Low"},
						{Value: "medium", Name: "Medium"},
						{Value: "high", Name: "High"},
					},
				},
			},
		},
	}
	matched, id, val := resolveConfigOptionTarget(uncatEffortOpts, "thought_level", "high")
	if matched == nil || id != "effort" || val != "high" {
		t.Fatalf("expected uncategorized effort to match thought_level, got matched=%v id=%q val=%q", matched != nil, id, val)
	}

	// 2. Legacy Modes (SetSessionMode) when ConfigOptions does not advertise `mode`
	legacyDrv := &stubAdapterDriver{
		sessionOpts: uncatEffortOpts,
		sessionModes: &acp.SessionModeState{
			CurrentModeId: "default",
			AvailableModes: []acp.SessionMode{
				{Id: "default", Name: "Default"},
				{Id: "agent", Name: "Agent"},
			},
		},
	}
	r, err := New(ctx, Config{
		MachineID:  "test-node",
		DataDir:    t.TempDir(),
		SocketPath: "none",
		Drivers:    map[protocol.Adapter]AcpAdapterDriver{protocol.AdapterClaude: legacyDrv},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(context.Background()) }()

	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:     "att_legacy_mode_1",
		TaskID:        "tsk_legacy_mode",
		ProjectID:     protocol.ScratchProjectID,
		WBSessionID:   "ses_legacy_mode",
		LeaseToken:    "tok_legacy_1",
		Adapter:       protocol.AdapterClaude,
		Prompt:        "hi",
		DesiredConfig: map[string]string{"mode": "auto", "thought_level": "high"},
	})
	if err := r.WaitAttemptDone(ctx, "att_legacy_mode_1"); err != nil {
		t.Fatalf("wait legacy mode attempt: %v", err)
	}
	if len(legacyDrv.setModeCalls) != 1 || legacyDrv.setModeCalls[0] != "agent" {
		t.Fatalf("expected SetSessionMode('agent') via legacy Modes, got %+v", legacyDrv.setModeCalls)
	}
	if len(legacyDrv.setConfigCalls) != 1 || legacyDrv.setConfigCalls[0].id != "effort" || legacyDrv.setConfigCalls[0].val != "high" {
		t.Fatalf("expected SetConfigOption('effort','high'), got %+v", legacyDrv.setConfigCalls)
	}

	// 3. Explicitly requested unknown config key MUST return error (not silently swallowed!)
	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:     "att_bad_cfg",
		TaskID:        "tsk_bad_cfg",
		ProjectID:     protocol.ScratchProjectID,
		WBSessionID:   "ses_bad_cfg",
		LeaseToken:    "tok_bad_cfg",
		Adapter:       protocol.AdapterClaude,
		Prompt:        "hi",
		DesiredConfig: map[string]string{"unsupported_opt": "foo"},
	})
	if err := r.WaitAttemptDone(ctx, "att_bad_cfg"); err != nil {
		t.Fatalf("wait att_bad_cfg: %v", err)
	}
	_, badItems, _ := r.outbox.ReadUnacked(ctx, "att_bad_cfg", 0, 50)
	badFinal := ""
	for _, it := range badItems {
		if it.FrameType == protocol.OutboxFrameState {
			if st, ok := it.Payload["state"].(string); ok {
				badFinal = st
			}
		}
	}
	if badFinal != string(protocol.AttemptFailed) {
		t.Fatalf("expected explicit unsupported config option to fail attempt, got %q", badFinal)
	}
}

func TestSessionSlot_ProjectEnvChangeRestartsScopedDriver(t *testing.T) {
	ctx := context.Background()
	var (
		mu      sync.Mutex
		created []*stubAdapterDriver
		envs    []map[string]string
	)
	baseDrv := &stubAdapterDriver{}
	r, err := New(ctx, Config{
		MachineID:  "test-node",
		DataDir:    t.TempDir(),
		SocketPath: "none",
		Drivers:    map[protocol.Adapter]AcpAdapterDriver{protocol.AdapterAgy: baseDrv},
		ScopedDriverFactory: func(_ protocol.Adapter, projectEnv map[string]string) (AcpAdapterDriver, error) {
			mu.Lock()
			defer mu.Unlock()
			cp := map[string]string{}
			for k, v := range projectEnv {
				cp[k] = v
			}
			d := &stubAdapterDriver{}
			created = append(created, d)
			envs = append(envs, cp)
			return d, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(context.Background()) }()

	sharedDir := t.TempDir()

	// Turn 1: ProjectEnv v1 -> spawns scopedDriver #1
	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:    "att_env_1",
		TaskID:       "tsk_env",
		ProjectID:    protocol.ScratchProjectID,
		WBSessionID:  "ses_env",
		WorktreePath: sharedDir,
		LeaseToken:   "tok_1",
		Adapter:      protocol.AdapterAgy,
		ProjectEnv:   map[string]string{"API_KEY": "v1"},
		Prompt:       "turn 1",
	})
	if err := r.WaitAttemptDone(ctx, "att_env_1"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(created) != 1 || created[0].closed {
		t.Fatalf("turn 1: expected 1 open scoped driver, got count=%d", len(created))
	}
	mu.Unlock()

	// Turn 2: SAME ProjectEnv v1 -> reuses scopedDriver #1
	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:    "att_env_2",
		TaskID:       "tsk_env",
		ProjectID:    protocol.ScratchProjectID,
		WBSessionID:  "ses_env",
		WorktreePath: sharedDir,
		LeaseToken:   "tok_2",
		Adapter:      protocol.AdapterAgy,
		ProjectEnv:   map[string]string{"API_KEY": "v1"},
		Prompt:       "turn 2",
	})
	if err := r.WaitAttemptDone(ctx, "att_env_2"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(created) != 1 || created[0].closed {
		t.Fatalf("turn 2: expected scoped driver #1 reused, got count=%d closed=%v", len(created), created[0].closed)
	}
	mu.Unlock()

	// Turn 3: CHANGED ProjectEnv v2 -> closes scopedDriver #1 and spawns scopedDriver #2!
	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:    "att_env_3",
		TaskID:       "tsk_env",
		ProjectID:    protocol.ScratchProjectID,
		WBSessionID:  "ses_env",
		WorktreePath: sharedDir,
		LeaseToken:   "tok_3",
		Adapter:      protocol.AdapterAgy,
		ProjectEnv:   map[string]string{"API_KEY": "v2"},
		Prompt:       "turn 3",
	})
	if err := r.WaitAttemptDone(ctx, "att_env_3"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(created) != 2 {
		t.Fatalf("turn 3: expected new scoped driver #2 after ProjectEnv change, got count=%d", len(created))
	}
	if !created[0].closed {
		t.Fatalf("turn 3: expected old scoped driver #1 to be closed")
	}
	if created[1].closed || envs[1]["API_KEY"] != "v2" {
		t.Fatalf("turn 3: expected scoped driver #2 active with v2 env, got closed=%v env=%+v", created[1].closed, envs[1])
	}
}

func TestProbeACPCapabilities_SessionNewFailureReportsErrorNotReady(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake ACP test is POSIX-only")
	}
	tmp := t.TempDir()

	writeFakeACP := func(name, sessionErrJSON string) string {
		p := filepath.Join(tmp, name)
		script := `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    *'"initialize"'*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake","version":"0.1.0"}}}\n' "$id"
      ;;
    *'"session/new"'*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
      printf '{"jsonrpc":"2.0","id":%s,"error":` + sessionErrJSON + `}\n' "$id"
      ;;
  esac
done
`
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Case 1: Generic session/new protocol error -> MUST report AgentStateError (NOT ready!)
	errBin := writeFakeACP("fake-err-acp", `{"code":-32000,"message":"internal session bootstrap failed"}`)
	mgr1 := NewAgentManager(AgentManagerConfig{
		DataDir:  tmp,
		FlagBins: map[protocol.Adapter]string{protocol.AdapterClaude: errBin},
	})
	p1 := mgr1.ProbeOne(context.Background(), protocol.AdapterClaude)
	if p1.State != protocol.AgentStateError {
		t.Fatalf("expected AgentStateError when session/new fails, got state=%q detail=%q", p1.State, p1.Detail)
	}
	if !strings.Contains(p1.Detail, "session/new") {
		t.Fatalf("expected clear detail mentioning session/new, got %q", p1.Detail)
	}

	// Case 2: Auth error on session/new -> MUST report AgentStateNeedsLogin
	authBin := writeFakeACP("fake-auth-acp", `{"code":-32001,"message":"authentication required: please log in"}`)
	mgr2 := NewAgentManager(AgentManagerConfig{
		DataDir:  tmp,
		FlagBins: map[protocol.Adapter]string{protocol.AdapterCodex: authBin},
	})
	p2 := mgr2.ProbeOne(context.Background(), protocol.AdapterCodex)
	if p2.State != protocol.AgentStateNeedsLogin {
		t.Fatalf("expected AgentStateNeedsLogin when session/new fails with auth error, got state=%q detail=%q", p2.State, p2.Detail)
	}
	_ = json.Valid
}

func TestBridgeDefaultFullModeStillRequiresHumanApproval(t *testing.T) {
	ctx := context.Background()
	permCalled := make(chan struct{}, 1)
	drv := &stubAdapterDriver{
		sessionModes: &acp.SessionModeState{
			CurrentModeId: "bypassPermissions",
			AvailableModes: []acp.SessionMode{
				{Id: "bypassPermissions", Name: "Full Access"},
			},
		},
	}
	drv.onPrompt = func(pctx context.Context, sid acp.SessionId, _, _ string) (acp.PromptResponse, error) {
		close(permCalled)
		// Bridge invokes session/request_permission while user did NOT explicitly choose full mode
		resp, err := drv.permHandler(pctx, acp.RequestPermissionRequest{
			SessionId: sid,
			ToolCall: acp.ToolCallUpdate{
				ToolCallId: "tc_perm_1",
				Name:       acp.PatchSet("run_command"),
			},
			Options: []acp.PermissionOption{
				{OptionId: "allow_once", Name: "Allow Once", Kind: acp.PermissionOptionKindAllowOnce},
				{OptionId: "reject_once", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
			},
		})
		if err != nil {
			return acp.PromptResponse{}, err
		}
		if resp.Outcome.Selected == nil || string(resp.Outcome.Selected.OptionId) != "reject_once" {
			t.Errorf("expected human decision reject_once, got %+v", resp.Outcome)
		}
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	r, err := New(ctx, Config{
		MachineID:  "test-node",
		DataDir:    t.TempDir(),
		SocketPath: "none",
		Driver:     drv,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(context.Background()) }()

	// User sends with empty DesiredConfig (did NOT explicitly opt into full/bypassPermissions)
	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:   "att_perm_human",
		TaskID:      "tsk_perm_human",
		ProjectID:   protocol.ScratchProjectID,
		WBSessionID: "ses_perm_human",
		LeaseToken:  "tok_perm_human",
		Adapter:     protocol.AdapterAgy,
		Prompt:      "run something",
	})

	select {
	case <-permCalled:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for permission request")
	}

	// Poll outbox for approval_request (MUST NOT have been auto-allowed!)
	var approvalID, payloadSHA string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && approvalID == "" {
		_, items, _ := r.outbox.ReadUnacked(ctx, "att_perm_human", 0, 100)
		for _, it := range items {
			if it.FrameType == protocol.OutboxFrameApprovalRequest {
				approvalID, _ = it.Payload["approvalId"].(string)
				payloadSHA, _ = it.Payload["payloadSha256"].(string)
				break
			}
			if it.FrameType == protocol.OutboxFrameEvents && it.Payload["kind"] == string(protocol.EventApprovalConsumed) {
				t.Fatalf("bridge default bypassPermissions MUST NOT auto-allow permission without explicit user mode")
			}
		}
		if approvalID == "" {
			time.Sleep(25 * time.Millisecond)
		}
	}
	if approvalID == "" {
		t.Fatalf("expected approval_request enqueued for human approval")
	}

	// Settle via human decision so attempt finishes cleanly
	r.HandleApprovalDecision(protocol.ApprovalDecisionPayload{
		ApprovalID:    approvalID,
		DecisionID:    "dec_perm_1",
		AttemptID:     "att_perm_human",
		OptionID:      "reject_once",
		PayloadSHA256: payloadSHA,
	})
	if err := r.WaitAttemptDone(ctx, "att_perm_human"); err != nil {
		t.Fatalf("wait att_perm_human: %v", err)
	}
}

func TestWorktreeAutoDeliver_ShowUntrackedFilesNoAndImmutableHeadTree(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	mirrorDir := filepath.Join(tmp, "mirror.git")
	dataDir := filepath.Join(tmp, "data")

	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v in %s failed: %v\n%s", args, dir, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	runGit(repoDir, "init", "-b", "main")
	runGit(repoDir, "config", "user.email", "tester@example.com")
	runGit(repoDir, "config", "user.name", "Orbit Tester")
	// Explicitly hide untracked files in git status config!
	runGit(repoDir, "config", "status.showUntrackedFiles", "no")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(repoDir, "add", ".")
	runGit(repoDir, "commit", "-m", "initial")
	baseSHA := runGit(repoDir, "rev-parse", "HEAD")

	if err := gitops.EnsureBareMirror(ctx, mirrorDir); err != nil {
		t.Fatalf("ensure bare mirror: %v", err)
	}

	drv := &stubAdapterDriver{}
	var capturedWT string
	drv.onPrompt = func(_ context.Context, _ acp.SessionId, wt, _ string) (acp.PromptResponse, error) {
		capturedWT = wt
		// Create ONLY a brand-new untracked file while status.showUntrackedFiles=no is configured
		if err := os.WriteFile(filepath.Join(wt, "untracked_only.txt"), []byte("new untracked content\n"), 0o644); err != nil {
			t.Errorf("write untracked_only.txt: %v", err)
		}
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	r, err := New(ctx, Config{
		MachineID:  "test-node",
		DataDir:    dataDir,
		SocketPath: "none",
		Driver:     drv,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(context.Background()) }()

	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:      "att_untracked_no",
		TaskID:         "tsk_untracked_no",
		ProjectID:      "proj_untracked",
		WBSessionID:    "ses_untracked_no",
		LeaseToken:     "tok_untracked_no",
		Adapter:        protocol.AdapterAgy,
		Isolation:      protocol.IsolationWorktree,
		RepoPath:       repoDir,
		MirrorBarePath: mirrorDir,
		BaseSHA:        baseSHA,
		Prompt:         "add untracked file",
	})
	if err := r.WaitAttemptDone(ctx, "att_untracked_no"); err != nil {
		t.Fatalf("wait att_untracked_no: %v", err)
	}

	_, items, err := r.outbox.ReadUnacked(ctx, "att_untracked_no", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var headSHA string
	for _, it := range items {
		if it.FrameType == protocol.OutboxFrameDeliveryPublished {
			headSHA, _ = it.Payload["headSha"].(string)
		}
	}
	if headSHA == "" {
		t.Fatalf("expected auto-delivery of untracked file even with status.showUntrackedFiles=no")
	}
	// Verify immutable commitTreeHash matches current worktree before extra edit, and differs after extra edit
	pkgTree, err := commitTreeHash(ctx, capturedWT, headSHA)
	if err != nil || pkgTree == "" {
		t.Fatalf("commitTreeHash failed: tree=%q err=%v", pkgTree, err)
	}
	curTree1, err := worktreeTreeHash(ctx, capturedWT)
	if err != nil || curTree1 != pkgTree {
		t.Fatalf("expected worktreeTreeHash == commitTreeHash(headSHA): cur=%q pkg=%q err=%v", curTree1, pkgTree, err)
	}
	if err := os.WriteFile(filepath.Join(capturedWT, "after_pkg.txt"), []byte("extra\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	curTree2, err := worktreeTreeHash(ctx, capturedWT)
	if err != nil || curTree2 == pkgTree {
		t.Fatalf("expected worktreeTreeHash to diverge from immutable headSHA^{tree} after post-package edit")
	}
}

func TestRetryPendingDelivery_PermanentBlobErrorFailsAndReleasesSlot(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")

	fakeHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && strings.HasPrefix(r.URL.Path, "/api/blobs/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/blobs/") {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`attachment too large`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer fakeHub.Close()

	r, err := New(ctx, Config{
		MachineID:             "remote-node",
		HubHTTPURL:            fakeHub.URL,
		DataDir:               dataDir,
		SocketPath:            "none",
		Driver:                &stubAdapterDriver{},
		DeliveryRetryInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(context.Background()) }()

	blobPath := filepath.Join(tmp, "blob.bin")
	if err := os.WriteFile(blobPath, []byte("123456"), 0o600); err != nil {
		t.Fatal(err)
	}
	attCtx, attCancel := context.WithCancel(ctx)
	att := &attemptContext{
		dispatch: protocol.AttemptDispatchFrame{
			AttemptID:  "att_perm_blob",
			TaskID:     "tsk_perm_blob",
			ProjectID:  "proj_blob",
			LeaseToken: "tok_perm_blob",
		},
		cancelFn:       attCancel,
		doneCh:         make(chan struct{}),
		promptDoneCh:   make(chan struct{}),
		promptReturned: true,
		tracker:        NewActiveSwarmTracker("att_perm_blob"),
	}
	r.mu.Lock()
	r.attempts[att.dispatch.AttemptID] = att
	r.mu.Unlock()

	if err := r.outbox.SavePendingDelivery(ctx, PendingDelivery{
		DeliveryID:     "dlv_blob_413",
		AttemptID:      att.dispatch.AttemptID,
		LeaseToken:     att.dispatch.LeaseToken,
		TaskID:         att.dispatch.TaskID,
		ProjectID:      att.dispatch.ProjectID,
		RepoRoot:       tmp,
		MirrorBarePath: "orbit-hub://proj_blob",
		ReviewBaseSHA:  "abc",
		HeadSHA:        "def",
		Summary:        "blob fail",
		IsFinal:        true,
		StagedBlobs: []gitops.PackagedBlob{
			{Item: protocol.BlobManifestItem{SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Size: 6}, LocalPath: blobPath},
		},
	}); err != nil {
		t.Fatal(err)
	}

	r.retryPendingDeliveries(ctx)

	select {
	case <-attCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("expected permanent 413 blob error to finalize and release waiting attempt slot")
	}

	// Pending delivery must be marked emitted so it never retries forever
	rem, err := r.outbox.ListUnemittedDeliveries(ctx)
	if err != nil || len(rem) != 0 {
		t.Fatalf("expected 0 unemitted deliveries after permanent blob rejection, got %d err=%v", len(rem), err)
	}
	_, items, _ := r.outbox.ReadUnacked(ctx, att.dispatch.AttemptID, 0, 20)
	sawFailed := false
	for _, it := range items {
		if it.FrameType == protocol.OutboxFrameState && it.Payload["state"] == string(protocol.AttemptFailed) && it.Payload["errorCode"] == "DELIVERY_BLOB_REJECTED" {
			sawFailed = true
		}
	}
	if !sawFailed {
		t.Fatalf("expected DELIVERY_BLOB_REJECTED failed state in outbox, got %+v", items)
	}
}

func TestAutoDeliverTransientError_NeedsVerificationWaitsForDelivery(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	dataDir := filepath.Join(tmp, "data")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	runGit(repoDir, "init", "-b", "main")
	runGit(repoDir, "config", "user.email", "tester@example.com")
	runGit(repoDir, "config", "user.name", "Orbit Tester")
	if err := os.WriteFile(filepath.Join(repoDir, "main.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(repoDir, "add", ".")
	runGit(repoDir, "commit", "-m", "init")
	baseSHA := runGit(repoDir, "rev-parse", "HEAD")

	var puts atomic.Int32
	fakeHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/mirror-heads") {
			_, _ = w.Write([]byte(`{"heads":[]}`))
			return
		}
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/bundle") {
			if puts.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer fakeHub.Close()

	drv := &stubAdapterDriver{}
	drv.onPrompt = func(_ context.Context, sid acp.SessionId, wt, _ string) (acp.PromptResponse, error) {
		// Record an unresolved non-zero tool failure via session update AND edit worktree
		_ = drv.updateHandler(ctx, acp.SessionNotification{
			SessionId: sid,
			Update: acp.SessionUpdate{
				ToolCall: &acp.SessionUpdateToolCall{
					ToolCallId: "tc_fail_1",
					Title:      "run_tests",
					Status:     acp.ToolCallStatusCompleted,
					RawInput:   json.RawMessage(`{"command":"go test ./..."}`),
					RawOutput:  json.RawMessage(`{"exitCode":1}`),
				},
			},
		})
		_ = os.WriteFile(filepath.Join(wt, "main.txt"), []byte("v2 with test failure\n"), 0o644)
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	r, err := New(ctx, Config{
		MachineID:             "remote-node",
		HubHTTPURL:            fakeHub.URL,
		DataDir:               dataDir,
		SocketPath:            "none",
		Driver:                drv,
		DeliveryRetryInterval: 80 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(context.Background()) }()

	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:      "att_nv_retry",
		TaskID:         "tsk_nv_retry",
		ProjectID:      "proj_nv",
		WBSessionID:    "ses_nv_retry",
		LeaseToken:     "tok_nv_retry",
		Adapter:        protocol.AdapterAgy,
		Isolation:      protocol.IsolationWorktree,
		RepoPath:       repoDir,
		MirrorBarePath: "orbit-hub://proj_nv",
		BaseSHA:        baseSHA,
		Prompt:         "edit with failing tool",
	})
	waitCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := r.WaitAttemptDone(waitCtx, "att_nv_retry"); err != nil {
		t.Fatalf("wait att_nv_retry: %v", err)
	}

	_, items, _ := r.outbox.ReadUnacked(ctx, "att_nv_retry", 0, 100)
	dlvSeq := int64(0)
	nvSeq := int64(0)
	for _, it := range items {
		if it.FrameType == protocol.OutboxFrameDeliveryPublished {
			dlvSeq = it.OutboxSeq
		}
		if it.FrameType == protocol.OutboxFrameState && it.Payload["state"] == string(protocol.AttemptNeedsVerification) {
			nvSeq = it.OutboxSeq
		}
	}
	if dlvSeq == 0 || nvSeq == 0 || dlvSeq >= nvSeq {
		t.Fatalf("expected delivery_published strictly BEFORE needs_verification, got dlvSeq=%d nvSeq=%d", dlvSeq, nvSeq)
	}
}

func TestScopedDriverCleanedOnConfigFailureAndRunnerClose(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var created []*stubAdapterDriver

	r, err := New(ctx, Config{
		MachineID:  "test-node",
		DataDir:    t.TempDir(),
		SocketPath: "none",
		Driver:     &stubAdapterDriver{},
		ScopedDriverFactory: func(_ protocol.Adapter, _ map[string]string) (AcpAdapterDriver, error) {
			d := &stubAdapterDriver{
				sessionOpts: []acp.SessionConfigOption{
					{
						Select: &acp.SessionConfigOptionSelect{
							Id:           "model",
							CurrentValue: "default-model",
							Options: acp.SessionConfigSelectOptions{
								Ungrouped: &acp.SessionConfigSelectOptionsUngrouped{{Value: "default-model", Name: "Default"}},
							},
						},
					},
				},
			}
			mu.Lock()
			created = append(created, d)
			mu.Unlock()
			return d, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// 1. Config apply fails -> newly started scopedDriver MUST be closed immediately!
	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:     "att_scoped_fail",
		TaskID:        "tsk_scoped_fail",
		ProjectID:     protocol.ScratchProjectID,
		WBSessionID:   "ses_scoped_fail",
		LeaseToken:    "tok_scoped_fail",
		Adapter:       protocol.AdapterAgy,
		ProjectEnv:    map[string]string{"FOO": "bar"},
		DesiredConfig: map[string]string{"unknown_key": "bad"},
		Prompt:        "fail config",
	})
	if err := r.WaitAttemptDone(ctx, "att_scoped_fail"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(created) != 1 || !created[0].closed {
		mu.Unlock()
		t.Fatalf("expected scoped driver #0 to be closed immediately on config apply failure, created=%d", len(created))
	}
	mu.Unlock()

	// 2. Successful scopedDriver stays live on slot, then Runner.Close MUST close it!
	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:   "att_scoped_ok",
		TaskID:      "tsk_scoped_ok",
		ProjectID:   protocol.ScratchProjectID,
		WBSessionID: "ses_scoped_ok",
		LeaseToken:  "tok_scoped_ok",
		Adapter:     protocol.AdapterAgy,
		ProjectEnv:  map[string]string{"FOO": "ok"},
		Prompt:      "succeed",
	})
	if err := r.WaitAttemptDone(ctx, "att_scoped_ok"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(created) != 2 || created[1].closed {
		mu.Unlock()
		t.Fatalf("expected scoped driver #1 to remain open before Runner.Close, created=%d", len(created))
	}
	mu.Unlock()

	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("Runner.Close: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !created[1].closed {
		t.Fatalf("expected Runner.Close to close remaining slot scopedDriver #1")
	}
}

func TestExplicitFullPermissionAndAppliedModeMatrix(t *testing.T) {
	ctx := context.Background()

	runCase := func(
		t *testing.T,
		name string,
		availModes []acp.SessionMode,
		desired map[string]string,
		explicitFull bool,
		wantAutoAllow bool,
	) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			permCalled := make(chan struct{}, 1)
			drv := &stubAdapterDriver{
				sessionModes: &acp.SessionModeState{
					CurrentModeId:  availModes[0].Id,
					AvailableModes: availModes,
				},
			}
			drv.onPrompt = func(pctx context.Context, sid acp.SessionId, _, _ string) (acp.PromptResponse, error) {
				close(permCalled)
				resp, err := drv.permHandler(pctx, acp.RequestPermissionRequest{
					SessionId: sid,
					ToolCall: acp.ToolCallUpdate{
						ToolCallId: "tc_perm_matrix",
						Name:       acp.PatchSet("run_command"),
					},
					Options: []acp.PermissionOption{
						{OptionId: "allow_once", Name: "Allow Once", Kind: acp.PermissionOptionKindAllowOnce},
						{OptionId: "reject_once", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
					},
				})
				if err != nil {
					return acp.PromptResponse{}, err
				}
				if wantAutoAllow {
					if resp.Outcome.Selected == nil || string(resp.Outcome.Selected.OptionId) != "allow_once" {
						t.Errorf("expected auto-allow allow_once, got %+v", resp.Outcome)
					}
				} else {
					if resp.Outcome.Selected == nil || string(resp.Outcome.Selected.OptionId) != "reject_once" {
						t.Errorf("expected human decision reject_once, got %+v", resp.Outcome)
					}
				}
				return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
			}

			r, err := New(ctx, Config{
				MachineID:  "test-node",
				DataDir:    t.TempDir(),
				SocketPath: "none",
				Driver:     drv,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close(context.Background()) }()

			attID := "att_matrix_" + name
			r.HandleDispatch(protocol.AttemptDispatchFrame{
				AttemptID:              attID,
				TaskID:                 "tsk_matrix_" + name,
				ProjectID:              protocol.ScratchProjectID,
				WBSessionID:            "ses_matrix_" + name,
				LeaseToken:             "tok_matrix_" + name,
				Adapter:                protocol.AdapterAgy,
				DesiredConfig:          desired,
				ExplicitFullPermission: explicitFull,
				Prompt:                 "run tool",
			})

			select {
			case <-permCalled:
			case <-time.After(3 * time.Second):
				t.Fatalf("timed out waiting for permission call")
			}

			if wantAutoAllow {
				if err := r.WaitAttemptDone(ctx, attID); err != nil {
					t.Fatalf("wait auto-allow attempt: %v", err)
				}
				_, items, _ := r.outbox.ReadUnacked(ctx, attID, 0, 50)
				sawAuto := false
				for _, it := range items {
					if it.FrameType == protocol.OutboxFrameApprovalRequest {
						t.Fatalf("did not expect human approval_request when explicitly full & applied full")
					}
					if it.FrameType == protocol.OutboxFrameEvents && it.Payload["kind"] == string(protocol.EventApprovalConsumed) {
						sawAuto = true
					}
				}
				if !sawAuto {
					t.Fatalf("expected policyAutoAllowed audit event")
				}
				return
			}

			var approvalID, payloadSHA string
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) && approvalID == "" {
				_, items, _ := r.outbox.ReadUnacked(ctx, attID, 0, 50)
				for _, it := range items {
					if it.FrameType == protocol.OutboxFrameApprovalRequest {
						approvalID, _ = it.Payload["approvalId"].(string)
						payloadSHA, _ = it.Payload["payloadSha256"].(string)
						break
					}
					if it.FrameType == protocol.OutboxFrameEvents && it.Payload["kind"] == string(protocol.EventApprovalConsumed) {
						t.Fatalf("case %s MUST NOT auto-allow permission", name)
					}
				}
				if approvalID == "" {
					time.Sleep(25 * time.Millisecond)
				}
			}
			if approvalID == "" {
				t.Fatalf("case %s expected human approval_request", name)
			}
			r.HandleApprovalDecision(protocol.ApprovalDecisionPayload{
				ApprovalID:    approvalID,
				DecisionID:    "dec_" + name,
				AttemptID:     attID,
				OptionID:      "reject_once",
				PayloadSHA256: payloadSHA,
			})
			if err := r.WaitAttemptDone(ctx, attID); err != nil {
				t.Fatalf("wait human-approval attempt: %v", err)
			}
		})
	}

	// 1. Inherited mode=bypassPermissions without ExplicitFullPermission -> MUST require human approval
	runCase(t, "inherited_bypass_no_explicit",
		[]acp.SessionMode{{Id: "bypassPermissions", Name: "Full"}},
		map[string]string{"mode": "bypassPermissions"},
		false,
		false,
	)

	// 2. ExplicitFullPermission=true + mode=full, but bridge only supports auto (downgraded to auto) -> MUST require human approval
	runCase(t, "explicit_full_mapped_to_auto",
		[]acp.SessionMode{{Id: "default", Name: "Default"}, {Id: "auto", Name: "Auto"}},
		map[string]string{"mode": "full"},
		true,
		false,
	)

	// 3. ExplicitFullPermission=true + mode=full AND bridge actually applies bypassPermissions -> auto-allowed!
	runCase(t, "explicit_full_applied_bypass",
		[]acp.SessionMode{{Id: "default", Name: "Default"}, {Id: "bypassPermissions", Name: "Full"}},
		map[string]string{"mode": "full"},
		true,
		true,
	)
}

func TestWorktreeAutoDeliver_MidDeliverThenRevertToBasePublishesFinalSnapshot(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	mirrorDir := filepath.Join(tmp, "mirror.git")
	dataDir := filepath.Join(tmp, "data")

	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	runGit(repoDir, "init", "-b", "main")
	runGit(repoDir, "config", "user.email", "tester@example.com")
	runGit(repoDir, "config", "user.name", "Orbit Tester")
	if err := os.WriteFile(filepath.Join(repoDir, "base.txt"), []byte("clean\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(repoDir, "add", ".")
	runGit(repoDir, "commit", "-m", "initial")
	baseSHA := runGit(repoDir, "rev-parse", "HEAD")

	if err := gitops.EnsureBareMirror(ctx, mirrorDir); err != nil {
		t.Fatal(err)
	}

	drv := &stubAdapterDriver{}
	var r *Runner
	drv.onPrompt = func(pctx context.Context, _ acp.SessionId, wt, _ string) (acp.PromptResponse, error) {
		tempFile := filepath.Join(wt, "temporary_scratch.txt")
		if err := os.WriteFile(tempFile, []byte("mid turn content\n"), 0o644); err != nil {
			t.Errorf("write tempFile: %v", err)
		}
		if _, err := r.HandleLocalDeliver(pctx, LocalDeliverRequest{
			AttemptID: "att_revert_base",
			Summary:   "mid delivery with temp file",
		}); err != nil {
			t.Errorf("mid HandleLocalDeliver: %v", err)
		}
		// Now delete the temporary file so the worktree reverts back to baseSHA!
		if err := os.Remove(tempFile); err != nil {
			t.Errorf("remove tempFile: %v", err)
		}
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	var err error
	r, err = New(ctx, Config{
		MachineID:  "test-node",
		DataDir:    dataDir,
		SocketPath: "none",
		Driver:     drv,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(context.Background()) }()

	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:      "att_revert_base",
		TaskID:         "tsk_revert_base",
		ProjectID:      "proj_revert",
		WBSessionID:    "ses_revert_base",
		LeaseToken:     "tok_revert_base",
		Adapter:        protocol.AdapterAgy,
		Isolation:      protocol.IsolationWorktree,
		RepoPath:       repoDir,
		MirrorBarePath: mirrorDir,
		BaseSHA:        baseSHA,
		Prompt:         "add file, deliver, then remove it back to base",
	})
	if err := r.WaitAttemptDone(ctx, "att_revert_base"); err != nil {
		t.Fatalf("wait att_revert_base: %v", err)
	}

	_, items, err := r.outbox.ReadUnacked(ctx, "att_revert_base", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var dlvs []map[string]any
	var finalDlvID string
	for _, it := range items {
		if it.FrameType == protocol.OutboxFrameDeliveryPublished {
			dlvs = append(dlvs, it.Payload)
		}
		if it.FrameType == protocol.OutboxFrameState && it.Payload["state"] == string(protocol.AttemptSucceeded) {
			finalDlvID, _ = it.Payload["deliveryId"].(string)
		}
	}
	if len(dlvs) != 2 {
		t.Fatalf("expected 2 deliveries (mid + final revert-to-base), got %d: %+v", len(dlvs), dlvs)
	}
	finalDlv := dlvs[1]
	if finalDlv["headSha"] != baseSHA {
		t.Fatalf("expected final revert delivery headSha == baseSHA (%s), got %v", baseSHA, finalDlv["headSha"])
	}
	if finalDlvID != finalDlv["deliveryId"] {
		t.Fatalf("expected succeeded state deliveryId=%v, got %q", finalDlv["deliveryId"], finalDlvID)
	}
	// Verify the git tree of the final delivery ref in bare mirror does NOT contain temporary_scratch.txt
	finalRef, _ := finalDlv["gitRef"].(string)
	treeFiles := runGit(mirrorDir, "ls-tree", "-r", "--name-only", finalRef)
	if strings.Contains(treeFiles, "temporary_scratch.txt") {
		t.Fatalf("final delivery ref %s still contains deleted temporary_scratch.txt:\n%s", finalRef, treeFiles)
	}
}

func TestMidTurnModeDowngradeRevokesAutoApprove(t *testing.T) {
	ctx := context.Background()

	runDowngrade := func(t *testing.T, name string, sendDowngrade func(drv *stubAdapterDriver, sid acp.SessionId)) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			permCalled := make(chan struct{}, 1)
			drv := &stubAdapterDriver{
				sessionModes: &acp.SessionModeState{
					CurrentModeId: "default",
					AvailableModes: []acp.SessionMode{
						{Id: "default", Name: "Default"},
						{Id: "auto", Name: "Auto"},
						{Id: "bypassPermissions", Name: "Full"},
					},
				},
			}
			drv.onPrompt = func(pctx context.Context, sid acp.SessionId, _, _ string) (acp.PromptResponse, error) {
				// 1. Mid-turn ACP downgrades the session's active mode away from full/bypass
				sendDowngrade(drv, sid)

				// 2. Subsequent permission request MUST go to human approval (NOT auto-allowed!)
				close(permCalled)
				resp, err := drv.permHandler(pctx, acp.RequestPermissionRequest{
					SessionId: sid,
					ToolCall: acp.ToolCallUpdate{
						ToolCallId: "tc_after_downgrade",
						Name:       acp.PatchSet("run_command"),
					},
					Options: []acp.PermissionOption{
						{OptionId: "allow_once", Name: "Allow Once", Kind: acp.PermissionOptionKindAllowOnce},
						{OptionId: "reject_once", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
					},
				})
				if err != nil {
					return acp.PromptResponse{}, err
				}
				if resp.Outcome.Selected == nil || string(resp.Outcome.Selected.OptionId) != "reject_once" {
					t.Errorf("expected human decision reject_once after mid-turn downgrade, got %+v", resp.Outcome)
				}
				return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
			}

			r, err := New(ctx, Config{
				MachineID:  "test-node",
				DataDir:    t.TempDir(),
				SocketPath: "none",
				Driver:     drv,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close(context.Background()) }()

			attID := "att_downgrade_" + name
			// Start turn with ExplicitFullPermission=true and mode=full (applied as bypassPermissions)
			r.HandleDispatch(protocol.AttemptDispatchFrame{
				AttemptID:              attID,
				TaskID:                 "tsk_downgrade_" + name,
				ProjectID:              protocol.ScratchProjectID,
				WBSessionID:            "ses_downgrade_" + name,
				LeaseToken:             "tok_downgrade_" + name,
				Adapter:                protocol.AdapterAgy,
				DesiredConfig:          map[string]string{"mode": "full"},
				ExplicitFullPermission: true,
				Prompt:                 "start full then downgrade",
			})

			select {
			case <-permCalled:
			case <-time.After(3 * time.Second):
				t.Fatalf("timed out waiting for post-downgrade permission request")
			}

			var approvalID, payloadSHA string
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) && approvalID == "" {
				_, items, _ := r.outbox.ReadUnacked(ctx, attID, 0, 50)
				for _, it := range items {
					if it.FrameType == protocol.OutboxFrameApprovalRequest {
						approvalID, _ = it.Payload["approvalId"].(string)
						payloadSHA, _ = it.Payload["payloadSha256"].(string)
						break
					}
					if it.FrameType == protocol.OutboxFrameEvents && it.Payload["kind"] == string(protocol.EventApprovalConsumed) {
						t.Fatalf("case %s MUST NOT auto-allow after mid-turn downgrade", name)
					}
				}
				if approvalID == "" {
					time.Sleep(25 * time.Millisecond)
				}
			}
			if approvalID == "" {
				t.Fatalf("case %s expected human approval_request after downgrade", name)
			}
			r.HandleApprovalDecision(protocol.ApprovalDecisionPayload{
				ApprovalID:    approvalID,
				DecisionID:    "dec_down_" + name,
				AttemptID:     attID,
				OptionID:      "reject_once",
				PayloadSHA256: payloadSHA,
			})
			if err := r.WaitAttemptDone(ctx, attID); err != nil {
				t.Fatalf("wait attempt: %v", err)
			}
		})
	}

	runDowngrade(t, "current_mode_update", func(drv *stubAdapterDriver, sid acp.SessionId) {
		_ = drv.updateHandler(ctx, acp.SessionNotification{
			SessionId: sid,
			Update: acp.SessionUpdate{
				CurrentModeUpdate: &acp.SessionCurrentModeUpdate{
					CurrentModeId: "auto",
				},
			},
		})
	})

	runDowngrade(t, "config_option_update", func(drv *stubAdapterDriver, sid acp.SessionId) {
		cat := acp.SessionConfigOptionCategoryMode
		_ = drv.updateHandler(ctx, acp.SessionNotification{
			SessionId: sid,
			Update: acp.SessionUpdate{
				ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{
					ConfigOptions: []acp.SessionConfigOption{
						{
							Select: &acp.SessionConfigOptionSelect{
								Id:           "mode",
								Category:     &cat,
								CurrentValue: "default",
							},
						},
					},
				},
			},
		})
	})

	// Case 3: ConfigOption-only session (no legacy sessionModes) starts in bypassPermissions, then receives
	// a ConfigOptionUpdate that REMOVES the mode option completely -> appliedMode MUST clear to "" so
	// subsequent session/request_permission goes to human approval!
	t.Run("config_option_update_removes_mode_clears_applied_mode", func(t *testing.T) {
		permCalled := make(chan struct{}, 1)
		catMode := acp.SessionConfigOptionCategoryMode
		catModel := acp.SessionConfigOptionCategoryModel
		drv := &stubAdapterDriver{
			sessionOpts: []acp.SessionConfigOption{
				{
					Select: &acp.SessionConfigOptionSelect{
						Id:           "mode",
						Category:     &catMode,
						CurrentValue: "default",
						Options: acp.SessionConfigSelectOptions{
							Ungrouped: &acp.SessionConfigSelectOptionsUngrouped{
								{Value: "default", Name: "Default"},
								{Value: "bypassPermissions", Name: "Full"},
							},
						},
					},
				},
			},
		}
		drv.onPrompt = func(pctx context.Context, sid acp.SessionId, _, _ string) (acp.PromptResponse, error) {
			// Replace ConfigOptions with a list that contains NO mode option
			_ = drv.updateHandler(ctx, acp.SessionNotification{
				SessionId: sid,
				Update: acp.SessionUpdate{
					ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{
						ConfigOptions: []acp.SessionConfigOption{
							{
								Select: &acp.SessionConfigOptionSelect{
									Id:           "model",
									Category:     &catModel,
									CurrentValue: "gpt-test",
								},
							},
						},
					},
				},
			})

			close(permCalled)
			resp, err := drv.permHandler(pctx, acp.RequestPermissionRequest{
				SessionId: sid,
				ToolCall: acp.ToolCallUpdate{
					ToolCallId: "tc_after_mode_removed",
					Name:       acp.PatchSet("run_command"),
				},
				Options: []acp.PermissionOption{
					{OptionId: "allow_once", Name: "Allow Once", Kind: acp.PermissionOptionKindAllowOnce},
					{OptionId: "reject_once", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
				},
			})
			if err != nil {
				return acp.PromptResponse{}, err
			}
			if resp.Outcome.Selected == nil || string(resp.Outcome.Selected.OptionId) != "reject_once" {
				t.Errorf("expected human decision reject_once after mode removed from configOptions, got %+v", resp.Outcome)
			}
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}

		r, err := New(ctx, Config{
			MachineID:  "test-node",
			DataDir:    t.TempDir(),
			SocketPath: "none",
			Driver:     drv,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Close(context.Background()) }()

		attID := "att_mode_removed"
		r.HandleDispatch(protocol.AttemptDispatchFrame{
			AttemptID:              attID,
			TaskID:                 "tsk_mode_removed",
			ProjectID:              protocol.ScratchProjectID,
			WBSessionID:            "ses_mode_removed",
			LeaseToken:             "tok_mode_removed",
			Adapter:                protocol.AdapterAgy,
			DesiredConfig:          map[string]string{"mode": "full"},
			ExplicitFullPermission: true,
			Prompt:                 "start full then remove mode option",
		})

		select {
		case <-permCalled:
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for permission request")
		}

		var approvalID, payloadSHA string
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && approvalID == "" {
			_, items, _ := r.outbox.ReadUnacked(ctx, attID, 0, 50)
			for _, it := range items {
				if it.FrameType == protocol.OutboxFrameApprovalRequest {
					approvalID, _ = it.Payload["approvalId"].(string)
					payloadSHA, _ = it.Payload["payloadSha256"].(string)
					break
				}
				if it.FrameType == protocol.OutboxFrameEvents && it.Payload["kind"] == string(protocol.EventApprovalConsumed) {
					t.Fatalf("MUST NOT auto-allow after config_option_update removed mode option")
				}
			}
			if approvalID == "" {
				time.Sleep(25 * time.Millisecond)
			}
		}
		if approvalID == "" {
			t.Fatalf("expected human approval_request after mode option was removed")
		}
		r.HandleApprovalDecision(protocol.ApprovalDecisionPayload{
			ApprovalID:    approvalID,
			DecisionID:    "dec_mode_removed",
			AttemptID:     attID,
			OptionID:      "reject_once",
			PayloadSHA256: payloadSHA,
		})
		if err := r.WaitAttemptDone(ctx, attID); err != nil {
			t.Fatalf("wait attempt: %v", err)
		}
	})
}
