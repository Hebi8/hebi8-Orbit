package runner_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/runner"
)

func TestRunner_DelegateSocketTokenIsolationAndAntiSpoofing(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	var mu sync.Mutex
	var capturedBody map[string]any
	var capturedMachine, capturedAuth string
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		capturedMachine = r.Header.Get("X-Orbit-Machine-Id")
		capturedAuth = r.Header.Get("Authorization")
		capturedBody = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer hubSrv.Close()

	// Create a fake executable for orbit binary path so MCP server injection also activates
	fakeOrbitBin := filepath.Join(tmp, "h8o")
	if err := os.WriteFile(fakeOrbitBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	promptStarted := make(chan struct{})
	releasePrompt := make(chan struct{})
	drv := NewFakeAcpDriver()
	drv.OnPrompt = func(pctx context.Context, d *FakeAcpDriver, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
		close(promptStarted)
		<-releasePrompt
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	sockPath := filepath.Join(tmp, "runner.sock")
	r, err := runner.New(ctx, runner.Config{
		MachineID:    "hub-node",
		HubHTTPURL:   hubSrv.URL,
		BearerToken:  "runner-tok-1",
		DataDir:      tmp,
		SocketPath:   sockPath,
		OrbitBinPath: fakeOrbitBin,
		Driver:       drv,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(context.Background()) }()

	// Dispatch attempt for Session A and wait until prompt is live
	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:   "att_A1",
		TaskID:      "tsk_A",
		ProjectID:   protocol.ScratchProjectID,
		LeaseToken:  "lease_A1",
		Adapter:     protocol.AdapterAgy,
		WBSessionID: "ses_A",
		Prompt:      "hello",
	})
	select {
	case <-promptStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for prompt to start")
	}

	tokA := r.DelegateTokenForSession("ses_A")
	if len(tokA) < 32 {
		t.Fatalf("expected high-entropy delegate token for ses_A, got %q", tokA)
	}

	unixClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
			},
		},
	}

	// 1. Missing token -> 401
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://orbit-runner/delegate/spawn", bytes.NewReader([]byte(`{"goal":"x"}`)))
	resp, err := unixClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", resp.StatusCode)
	}

	// 2. Spoofed / case-variant / duplicate / unknown identity fields MUST be rejected with 400!
	for _, badBody := range []string{
		`{"parentTaskId":"tsk_VICTIM","goal":"x"}`,
		`{"ParentTaskId":"tsk_VICTIM","goal":"x"}`,
		`{"parenttaskid":"tsk_VICTIM","goal":"x"}`,
		`{"parentAttemptId":"att_VICTIM","goal":"x"}`,
		`{"delegatedByAttemptId":"att_VICTIM","goal":"x"}`,
		`{"goal":"first","GOAL":"second"}`,
		`{"goal":"x","unknownExtra":true}`,
	} {
		req, _ = http.NewRequestWithContext(ctx, http.MethodPost, "http://orbit-runner/delegate/spawn", bytes.NewReader([]byte(badBody)))
		req.Header.Set("X-Orbit-Delegate-Token", tokA)
		resp, err = unixClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 for spoofed/unknown payload %s, got %d", badBody, resp.StatusCode)
		}
	}

	// 3. Valid typed request while attempt is active -> forwarded with authoritative parentTaskId/parentAttemptId/delegatedByAttemptId
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, "http://orbit-runner/delegate/spawn", bytes.NewReader([]byte(`{"goal":"do subtask","role":"researcher"}`)))
	req.Header.Set("X-Orbit-Delegate-Token", tokA)
	resp, err = unixClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 forwarded response, got %d", resp.StatusCode)
	}

	mu.Lock()
	if capturedMachine != "hub-node" || capturedAuth != "Bearer runner-tok-1" {
		t.Fatalf("unexpected runner auth headers: machine=%q auth=%q", capturedMachine, capturedAuth)
	}
	if capturedBody["parentTaskId"] != "tsk_A" || capturedBody["parentAttemptId"] != "att_A1" || capturedBody["delegatedByAttemptId"] != "att_A1" {
		t.Fatalf("runner failed to bind authoritative identity: %+v", capturedBody)
	}
	mu.Unlock()

	// 4. Finish attempt -> token MUST be rejected (401) because attempt is no longer active
	close(releasePrompt)
	if err := r.WaitAttemptDone(ctx, "att_A1"); err != nil {
		t.Fatal(err)
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, "http://orbit-runner/delegate/list", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("X-Orbit-Delegate-Token", tokA)
	resp, err = unixClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 after attempt completed, got %d", resp.StatusCode)
	}

	// 5. Closing session revokes delegate token -> still 401
	closed, err := r.CloseSessionSafely(ctx, "ses_A")
	if err != nil || !closed {
		t.Fatalf("expected ses_A to close cleanly (closed=%v err=%v)", closed, err)
	}
	if got := r.DelegateTokenForSession("ses_A"); got != "" {
		t.Fatalf("expected empty token after session close, got %q", got)
	}
}

type mcpTestPipeAgent struct {
	mu            sync.Mutex
	rejectWithMCP bool
	rejectErr     error
	newCalls      []acp.NewSessionRequest
	resumeCalls   []acp.ResumeSessionRequest
	loadCalls     []acp.LoadSessionRequest
	onPrompt      func(context.Context, acp.PromptRequest) (acp.PromptResponse, error)
}

func (a *mcpTestPipeAgent) Authenticate(context.Context, acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}
func (a *mcpTestPipeAgent) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentCapabilities: acp.AgentCapabilities{
			LoadSession: true,
			SessionCapabilities: acp.SessionCapabilities{
				Resume: &acp.SessionResumeCapabilities{},
			},
		},
	}, nil
}
func (a *mcpTestPipeAgent) Logout(context.Context, acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, nil
}
func (a *mcpTestPipeAgent) Cancel(context.Context, acp.CancelNotification) error { return nil }
func (a *mcpTestPipeAgent) CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, nil
}
func (a *mcpTestPipeAgent) DeleteSession(context.Context, acp.DeleteSessionRequest) (acp.DeleteSessionResponse, error) {
	return acp.DeleteSessionResponse{}, nil
}
func (a *mcpTestPipeAgent) ListSessions(context.Context, acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, nil
}
func (a *mcpTestPipeAgent) NewSession(_ context.Context, req acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	a.mu.Lock()
	a.newCalls = append(a.newCalls, req)
	reject := a.rejectWithMCP && len(req.McpServers) > 0
	rerr := a.rejectErr
	a.mu.Unlock()
	if reject {
		if rerr != nil {
			return acp.NewSessionResponse{}, rerr
		}
		return acp.NewSessionResponse{}, errors.New("mcpServers unsupported")
	}
	return acp.NewSessionResponse{SessionId: "mcp-sid-1"}, nil
}
func (a *mcpTestPipeAgent) ResumeSession(_ context.Context, req acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	a.mu.Lock()
	a.resumeCalls = append(a.resumeCalls, req)
	reject := a.rejectWithMCP && len(req.McpServers) > 0
	rerr := a.rejectErr
	a.mu.Unlock()
	if reject {
		if rerr != nil {
			return acp.ResumeSessionResponse{}, rerr
		}
		return acp.ResumeSessionResponse{}, errors.New("resume mcpServers unsupported")
	}
	return acp.ResumeSessionResponse{}, nil
}
func (a *mcpTestPipeAgent) LoadSession(_ context.Context, req acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	a.mu.Lock()
	a.loadCalls = append(a.loadCalls, req)
	reject := a.rejectWithMCP && len(req.McpServers) > 0
	rerr := a.rejectErr
	a.mu.Unlock()
	if reject {
		if rerr != nil {
			return acp.LoadSessionResponse{}, rerr
		}
		return acp.LoadSessionResponse{}, errors.New("load mcpServers unsupported")
	}
	return acp.LoadSessionResponse{}, nil
}
func (a *mcpTestPipeAgent) SetSessionConfigOption(context.Context, acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, nil
}
func (a *mcpTestPipeAgent) SetSessionMode(context.Context, acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, nil
}
func (a *mcpTestPipeAgent) Prompt(ctx context.Context, req acp.PromptRequest) (acp.PromptResponse, error) {
	if a.onPrompt != nil {
		return a.onPrompt(ctx, req)
	}
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func TestAgyAcpDriver_MCPInjectionAndGracefulFallback(t *testing.T) {
	for _, tc := range []struct {
		name          string
		rejectWithMCP bool
		wantAttempts  int
	}{
		{name: "inject_succeeds", rejectWithMCP: false, wantAttempts: 1},
		{name: "fallback_without_mcp", rejectWithMCP: true, wantAttempts: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			agentToRunnerR, agentToRunnerW := io.Pipe()
			runnerToAgentR, runnerToAgentW := io.Pipe()
			defer agentToRunnerR.Close()
			defer agentToRunnerW.Close()
			defer runnerToAgentR.Close()
			defer runnerToAgentW.Close()

			agent := &mcpTestPipeAgent{rejectWithMCP: tc.rejectWithMCP}
			_ = acp.NewAgentSideConnection(agent, agentToRunnerW, runnerToAgentR)

			drv := runner.NewStreamAcpDriver(runner.AgyDriverConfig{Adapter: protocol.AdapterAgy}, runnerToAgentW, agentToRunnerR)
			if err := drv.Start(ctx, nil, nil); err != nil {
				t.Fatalf("driver start: %v", err)
			}
			defer func() { _ = drv.Close(context.Background()) }()

			mcpServers := []acp.McpServer{
				{
					Stdio: &acp.McpServerStdio{
						Name:    "orbit",
						Command: "/usr/local/bin/orbit",
						Args:    []string{"mcp-serve"},
						Env: []acp.EnvVariable{
							{Name: "ORBIT_RUNNER_SOCK", Value: "/tmp/orbit.sock"},
							{Name: "ORBIT_TASK_ID", Value: "tsk_1"},
							{Name: "ORBIT_PROJECT_ID", Value: "demo"},
							{Name: "ORBIT_DELEGATE_TOKEN", Value: "tok_1"},
						},
					},
				},
			}
			mcpCtx := runner.WithSessionMCPServers(ctx, mcpServers)

			resp, err := drv.NewSession(mcpCtx, t.TempDir(), "")
			if err != nil {
				t.Fatalf("NewSession failed: %v", err)
			}
			if resp.SessionId != "mcp-sid-1" {
				t.Fatalf("unexpected sid: %s", resp.SessionId)
			}
			if _, err := drv.ResumeSession(mcpCtx, resp.SessionId, t.TempDir()); err != nil {
				t.Fatalf("ResumeSession failed: %v", err)
			}
			if _, err := drv.LoadSession(mcpCtx, resp.SessionId, t.TempDir()); err != nil {
				t.Fatalf("LoadSession failed: %v", err)
			}

			agent.mu.Lock()
			defer agent.mu.Unlock()
			if len(agent.newCalls) != tc.wantAttempts {
				t.Fatalf("expected %d NewSession calls, got %d", tc.wantAttempts, len(agent.newCalls))
			}
			if len(agent.newCalls[0].McpServers) != 1 || agent.newCalls[0].McpServers[0].Stdio == nil || agent.newCalls[0].McpServers[0].Stdio.Name != "orbit" {
				t.Fatalf("first NewSession call did not inject orbit stdio MCP server: %+v", agent.newCalls[0].McpServers)
			}
			if tc.rejectWithMCP && len(agent.newCalls[1].McpServers) != 0 {
				t.Fatalf("fallback NewSession call must pass empty McpServers, got %d", len(agent.newCalls[1].McpServers))
			}
			if len(agent.resumeCalls) != tc.wantAttempts || len(agent.loadCalls) != tc.wantAttempts {
				t.Fatalf("expected %d Resume/Load attempts, got resume=%d load=%d", tc.wantAttempts, len(agent.resumeCalls), len(agent.loadCalls))
			}
		})
	}
}

func TestSwarmTracker_NativeSubagentObservationPreservesSwarmInvariants(t *testing.T) {
	tr := runner.NewActiveSwarmTracker("att_1")

	// 1. invoke_subagent in_progress -> projects working native_subagent WITHOUT touching activeWorkers
	name := "invoke_subagent"
	proj := tr.ObserveToolCall(&acp.SessionUpdateToolCall{
		ToolCallId: "tc_sub_1",
		Name:       &name,
		Status:     acp.ToolCallStatusInProgress,
		RawInput:   json.RawMessage(`{"Role":"Codebase Researcher","Prompt":"Inspect auth middleware"}`),
	})
	if len(proj.Members) != 1 {
		t.Fatalf("expected 1 native_subagent working projection, got %+v", proj.Members)
	}
	if proj.Members[0]["sourceKind"] != "native_subagent" || proj.Members[0]["state"] != string(protocol.BoardMemberWorking) || proj.Members[0]["role"] != "Codebase Researcher" {
		t.Fatalf("unexpected working native_subagent member: %+v", proj.Members[0])
	}
	if tr.HasActiveWorkers() || tr.ActiveWorkerCount() != 0 {
		t.Fatalf("native_subagent MUST NOT increment activeWorkers")
	}

	// 2. invoke_subagent completed with conversationId -> projects stopped with externalSessionId
	rawOut := json.RawMessage(`{"conversationId":"conv_xyz_123"}`)
	proj2 := tr.ObserveToolCallUpdate(&acp.SessionToolCallUpdate{
		ToolCallId: "tc_sub_1",
		Status:     acp.PatchSet(acp.ToolCallStatusCompleted),
		RawOutput:  acp.PatchSet(rawOut),
	})
	if len(proj2.Members) != 1 || proj2.Members[0]["state"] != string(protocol.BoardMemberStopped) || proj2.Members[0]["externalSessionId"] != "conv_xyz_123" {
		t.Fatalf("unexpected completed native_subagent member: %+v", proj2.Members)
	}

	// 3. Claude Agent tool (completed in one frame) -> projects stopped native_subagent
	agentName := "Agent"
	proj3 := tr.ObserveToolCall(&acp.SessionUpdateToolCall{
		ToolCallId: "tc_claude_1",
		Name:       &agentName,
		Status:     acp.ToolCallStatusCompleted,
		RawInput:   json.RawMessage(`{"subagent_type":"Explore","description":"Find API routes","prompt":"Locate all routes"}`),
		RawOutput:  json.RawMessage(`{"agentId":"claude_ag_1"}`),
	})
	if len(proj3.Members) != 1 || proj3.Members[0]["sourceKind"] != "native_subagent" || proj3.Members[0]["role"] != "Explore" || proj3.Members[0]["externalSessionId"] != "claude_ag_1" {
		t.Fatalf("unexpected Claude Agent projection: %+v", proj3.Members)
	}

	// 4. manage_subagents kill updates tracked native_subagent to killed
	killName := "manage_subagents"
	proj4 := tr.ObserveToolCall(&acp.SessionUpdateToolCall{
		ToolCallId: "tc_kill_1",
		Name:       &killName,
		Status:     acp.ToolCallStatusCompleted,
		RawInput:   json.RawMessage(`{"Action":"kill","ConversationIds":["conv_xyz_123"]}`),
	})
	if len(proj4.Members) != 1 || proj4.Members[0]["state"] != string(protocol.BoardMemberKilled) {
		t.Fatalf("unexpected manage_subagents kill projection: %+v", proj4.Members)
	}

	// Verify swarm invariants remain untouched
	activeCount, usedSwarm, _, _ := tr.Stats()
	if activeCount != 0 || usedSwarm {
		t.Fatalf("expected activeCount=0 and usedSwarm=false after native subagent tools, got active=%d usedSwarm=%v", activeCount, usedSwarm)
	}

	// 5. Real swarm add still works strictly on completed
	swarmName := "swarm"
	projInProg := tr.ObserveToolCall(&acp.SessionUpdateToolCall{
		ToolCallId: "tc_sw_1",
		Name:       &swarmName,
		Status:     acp.ToolCallStatusInProgress,
		RawInput:   json.RawMessage(`{"action":"add","name":"worker-a","brief":"Do work"}`),
	})
	if len(projInProg.Members) != 0 || tr.ActiveWorkerCount() != 0 {
		t.Fatalf("in_progress swarm add must not mutate activeWorkers")
	}
	projDone := tr.ObserveToolCallUpdate(&acp.SessionToolCallUpdate{
		ToolCallId: "tc_sw_1",
		Status:     acp.PatchSet(acp.ToolCallStatusCompleted),
	})
	if len(projDone.Members) != 1 || tr.ActiveWorkerCount() != 1 {
		t.Fatalf("completed swarm add should increment activeWorkers to 1, got %d (members=%+v)", tr.ActiveWorkerCount(), projDone.Members)
	}
}

func TestRunner_AutoAllowOrbitBuiltinMCPTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tmp := t.TempDir()
	fakeOrbitBin := filepath.Join(tmp, "h8o")
	if err := os.WriteFile(fakeOrbitBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	drv := NewFakeAcpDriver()
	var r *runner.Runner
	var spawnResp, listResp acp.RequestPermissionResponse
	var staticOrbitBlocked, nonOrbitBlocked atomic.Bool
	var capturedSrvName atomic.Value

	drv.OnPrompt = func(pctx context.Context, d *FakeAcpDriver, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
		srvName := r.OrbitMCPServerNameForSession("ses_mcp_perm")
		capturedSrvName.Store(srvName)
		opts := []acp.PermissionOption{
			{OptionId: "allow", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
			{OptionId: "deny", Name: "Deny", Kind: acp.PermissionOptionKindRejectOnce},
		}
		var err error
		// 1. agy-prefixed Orbit spawn tool matching this session's randomized server name -> MUST auto-allow!
		spawnResp, err = d.AskPermission(pctx, acp.RequestPermissionRequest{
			SessionId: sid,
			ToolCall: acp.ToolCallUpdate{
				ToolCallId: "tc_orbit_spawn",
				Name:       acp.PatchSet("mcp_" + srvName + "_orbit_spawn_child"),
				Title:      acp.PatchSet("Run mcp_" + srvName + "_orbit_spawn_child"),
			},
			Options: opts,
		})
		if err != nil {
			return acp.PromptResponse{}, err
		}
		// 2. claude-prefixed Orbit list tool matching this session's randomized server name -> MUST auto-allow!
		listResp, err = d.AskPermission(pctx, acp.RequestPermissionRequest{
			SessionId: sid,
			ToolCall: acp.ToolCallUpdate{
				ToolCallId: "tc_orbit_list",
				Name:       acp.PatchSet("mcp__" + srvName + "__orbit_list_children"),
			},
			Options: opts,
		})
		if err != nil {
			return acp.PromptResponse{}, err
		}
		// 3. Static external server named "orbit" MUST NOT auto-allow (prevents external MCP server spoofing!)
		cStatic, cancelStatic := context.WithTimeout(pctx, 200*time.Millisecond)
		staticResp, _ := d.AskPermission(cStatic, acp.RequestPermissionRequest{
			SessionId: sid,
			ToolCall: acp.ToolCallUpdate{
				ToolCallId: "tc_static_orbit",
				Name:       acp.PatchSet("mcp_orbit_orbit_spawn_child"),
			},
			Options: opts,
		})
		cancelStatic()
		if staticResp.Outcome.Selected == nil {
			staticOrbitBlocked.Store(true)
		}
		// 4. Non-Orbit MCP tool MUST NOT auto-allow (must block waiting for human approval until ctx cancels)
		subCtx, subCancel := context.WithTimeout(pctx, 200*time.Millisecond)
		defer subCancel()
		nonOrbitResp, _ := d.AskPermission(subCtx, acp.RequestPermissionRequest{
			SessionId: sid,
			ToolCall: acp.ToolCallUpdate{
				ToolCallId: "tc_other_mcp",
				Name:       acp.PatchSet("mcp_other_drop_database"),
			},
			Options: opts,
		})
		if nonOrbitResp.Outcome.Selected == nil {
			nonOrbitBlocked.Store(true)
		}
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	var err error
	r, err = runner.New(ctx, runner.Config{
		MachineID:    "hub-node",
		DataDir:      tmp,
		SocketPath:   filepath.Join(tmp, "runner.sock"),
		OrbitBinPath: fakeOrbitBin,
		Driver:       drv,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(context.Background()) }()

	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:              "att_mcp_perm",
		TaskID:                 "tsk_mcp_perm",
		ProjectID:              protocol.ScratchProjectID,
		LeaseToken:             "lease_mcp_perm",
		Adapter:                protocol.AdapterAgy,
		WBSessionID:            "ses_mcp_perm",
		DesiredConfig:          map[string]string{"mode": "auto"},
		ExplicitFullPermission: false,
		Prompt:                 "spawn children",
	})
	if err := r.WaitAttemptDone(ctx, "att_mcp_perm"); err != nil {
		t.Fatalf("attempt failed: %v", err)
	}

	srvName, _ := capturedSrvName.Load().(string)
	if !strings.HasPrefix(srvName, "orbit") || len(srvName) != len("orbit")+8 {
		t.Fatalf("expected randomized per-session server name orbit<8hex>, got %q", srvName)
	}
	if spawnResp.Outcome.Selected == nil || spawnResp.Outcome.Selected.OptionId != "allow" {
		t.Fatalf("expected dynamic %s spawn tool to auto-allow with 'allow', got: %+v", srvName, spawnResp.Outcome)
	}
	if listResp.Outcome.Selected == nil || listResp.Outcome.Selected.OptionId != "allow" {
		t.Fatalf("expected dynamic %s list tool to auto-allow with 'allow', got: %+v", srvName, listResp.Outcome)
	}
	if !staticOrbitBlocked.Load() {
		t.Fatalf("external static 'mcp_orbit_orbit_spawn_child' MUST NOT be auto-allowed")
	}
	if !nonOrbitBlocked.Load() {
		t.Fatalf("non-Orbit MCP tool MUST NOT be auto-allowed in auto mode")
	}
}

func TestRunner_MCPAutoAllowHardeningAndPlanSocketGuard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	tmp := t.TempDir()
	fakeOrbitBin := filepath.Join(tmp, "h8o")
	if err := os.WriteFile(fakeOrbitBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer hubSrv.Close()

	// Case 1: Degraded session (orbitMCPAttached=false) MUST route even mcp_orbit_orbit_spawn_child to human approval
	t.Run("degraded_session_requires_human_approval", func(t *testing.T) {
		drv := NewFakeAcpDriver()
		drv.SimulateMCPFallback = true
		var blocked atomic.Bool
		drv.OnPrompt = func(pctx context.Context, d *FakeAcpDriver, sid acp.SessionId, _ string) (acp.PromptResponse, error) {
			subCtx, subCancel := context.WithTimeout(pctx, 200*time.Millisecond)
			defer subCancel()
			resp, _ := d.AskPermission(subCtx, acp.RequestPermissionRequest{
				SessionId: sid,
				ToolCall: acp.ToolCallUpdate{
					ToolCallId: "tc_degraded",
					Name:       acp.PatchSet("mcp_orbit_orbit_spawn_child"),
				},
				Options: []acp.PermissionOption{{OptionId: "allow", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce}},
			})
			if resp.Outcome.Selected == nil {
				blocked.Store(true)
			}
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}
		r, err := runner.New(ctx, runner.Config{
			MachineID: "hub-node", DataDir: t.TempDir(), SocketPath: filepath.Join(t.TempDir(), "r.sock"),
			OrbitBinPath: fakeOrbitBin, Driver: drv,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Close(context.Background()) }()
		r.HandleDispatch(protocol.AttemptDispatchFrame{
			AttemptID: "att_deg", TaskID: "tsk_deg", ProjectID: protocol.ScratchProjectID, LeaseToken: "l1",
			Adapter: protocol.AdapterAgy, WBSessionID: "ses_deg", DesiredConfig: map[string]string{"mode": "auto"}, Prompt: "hi",
		})
		if err := r.WaitAttemptDone(ctx, "att_deg"); err != nil {
			t.Fatal(err)
		}
		if !blocked.Load() {
			t.Fatal("degraded session without attached Orbit MCP MUST NOT auto-allow")
		}
	})

	// Case 2: Title-only spoof & bare unnamespaced name MUST NOT auto-allow; plan mode blocks write MCP + Socket writes
	t.Run("spoof_rejected_and_plan_mode_blocks_socket_writes", func(t *testing.T) {
		sockPath := filepath.Join(t.TempDir(), "plan.sock")
		drv := NewFakeAcpDriver()
		var r *runner.Runner
		promptReady := make(chan struct{})
		releasePrompt := make(chan struct{})
		var titleBlocked, bareBlocked, planSpawnBlocked, planListAllowed atomic.Bool

		drv.OnPrompt = func(pctx context.Context, d *FakeAcpDriver, sid acp.SessionId, _ string) (acp.PromptResponse, error) {
			srvName := r.OrbitMCPServerNameForSession("ses_plan")
			opts := []acp.PermissionOption{{OptionId: "allow", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce}}
			// Title-only spoof (Name absent) -> MUST NOT auto-allow
			c1, cancel1 := context.WithTimeout(pctx, 180*time.Millisecond)
			r1, _ := d.AskPermission(c1, acp.RequestPermissionRequest{
				SessionId: sid,
				ToolCall:  acp.ToolCallUpdate{ToolCallId: "tc_title", Title: acp.PatchSet("Run mcp_" + srvName + "_orbit_list_children")},
				Options:   opts,
			})
			cancel1()
			if r1.Outcome.Selected == nil {
				titleBlocked.Store(true)
			}
			// Bare unnamespaced name -> MUST NOT auto-allow
			c2, cancel2 := context.WithTimeout(pctx, 180*time.Millisecond)
			r2, _ := d.AskPermission(c2, acp.RequestPermissionRequest{
				SessionId: sid,
				ToolCall:  acp.ToolCallUpdate{ToolCallId: "tc_bare", Name: acp.PatchSet("orbit_list_children")},
				Options:   opts,
			})
			cancel2()
			if r2.Outcome.Selected == nil {
				bareBlocked.Store(true)
			}
			// Plan mode: read-only orbit_list_children auto-allows, write orbit_spawn_child MUST NOT auto-allow
			r3, _ := d.AskPermission(pctx, acp.RequestPermissionRequest{
				SessionId: sid,
				ToolCall:  acp.ToolCallUpdate{ToolCallId: "tc_plan_list", Name: acp.PatchSet("mcp_" + srvName + "_orbit_list_children")},
				Options:   opts,
			})
			if r3.Outcome.Selected != nil && r3.Outcome.Selected.OptionId == "allow" {
				planListAllowed.Store(true)
			}
			c4, cancel4 := context.WithTimeout(pctx, 180*time.Millisecond)
			r4, _ := d.AskPermission(c4, acp.RequestPermissionRequest{
				SessionId: sid,
				ToolCall:  acp.ToolCallUpdate{ToolCallId: "tc_plan_spawn", Name: acp.PatchSet("mcp_" + srvName + "_orbit_spawn_child")},
				Options:   opts,
			})
			cancel4()
			if r4.Outcome.Selected == nil {
				planSpawnBlocked.Store(true)
			}
			close(promptReady)
			<-releasePrompt
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}

		var err error
		r, err = runner.New(ctx, runner.Config{
			MachineID: "hub-node", HubHTTPURL: hubSrv.URL, BearerToken: "tok",
			DataDir: t.TempDir(), SocketPath: sockPath, OrbitBinPath: fakeOrbitBin, Driver: drv,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Close(context.Background()) }()

		r.HandleDispatch(protocol.AttemptDispatchFrame{
			AttemptID: "att_plan", TaskID: "tsk_plan", ProjectID: protocol.ScratchProjectID, LeaseToken: "l_plan",
			Adapter: protocol.AdapterAgy, WBSessionID: "ses_plan", DesiredConfig: map[string]string{"mode": "plan"}, Prompt: "plan only",
		})
		select {
		case <-promptReady:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for plan prompt checks")
		}

		tok := r.DelegateTokenForSession("ses_plan")
		unixClient := &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
				},
			},
		}
		// Socket write actions (spawn/send/cancel) MUST be rejected with 403 in plan mode
		for _, tc := range []struct {
			act  string
			body string
		}{
			{"spawn", `{"goal":"nope"}`},
			{"send", `{"childTaskId":"tsk_c1","message":"hi"}`},
			{"cancel", `{"childTaskId":"tsk_c1"}`},
		} {
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://orbit-runner/delegate/"+tc.act, bytes.NewReader([]byte(tc.body)))
			req.Header.Set("X-Orbit-Delegate-Token", tok)
			resp, err := unixClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("expected 403 for /delegate/%s in plan mode, got %d", tc.act, resp.StatusCode)
			}
		}
		// Socket read actions (list/read) MUST succeed (200) in plan mode
		for _, tc := range []struct {
			act  string
			body string
		}{
			{"list", `{}`},
			{"read", `{"childTaskId":"tsk_c1"}`},
		} {
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://orbit-runner/delegate/"+tc.act, bytes.NewReader([]byte(tc.body)))
			req.Header.Set("X-Orbit-Delegate-Token", tok)
			resp, err := unixClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("expected 200 for /delegate/%s in plan mode, got %d", tc.act, resp.StatusCode)
			}
		}

		close(releasePrompt)
		if err := r.WaitAttemptDone(ctx, "att_plan"); err != nil {
			t.Fatal(err)
		}
		if !titleBlocked.Load() || !bareBlocked.Load() || !planSpawnBlocked.Load() || !planListAllowed.Load() {
			t.Fatalf("unexpected policy flags: titleBlocked=%v bareBlocked=%v planSpawnBlocked=%v planListAllowed=%v",
				titleBlocked.Load(), bareBlocked.Load(), planSpawnBlocked.Load(), planListAllowed.Load())
		}
	})
}

func TestRunner_DelegateTokenRevokedOnConfigFailureAndCloseError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tmp := t.TempDir()
	fakeOrbitBin := filepath.Join(tmp, "h8o")
	if err := os.WriteFile(fakeOrbitBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// 1. Config apply failure MUST revoke delegate token immediately
	t.Run("config_failure_revokes_token", func(t *testing.T) {
		sockPath := filepath.Join(t.TempDir(), "cfgfail.sock")
		drv := NewFakeAcpDriver()
		drv.OnSetConfigOption = func(context.Context, acp.SessionId, string, string) (acp.SetSessionConfigOptionResponse, error) {
			return acp.SetSessionConfigOptionResponse{}, errors.New("config rejected")
		}
		r, err := runner.New(ctx, runner.Config{
			MachineID: "hub-node", DataDir: t.TempDir(), SocketPath: sockPath, OrbitBinPath: fakeOrbitBin, Driver: drv,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Close(context.Background()) }()

		r.HandleDispatch(protocol.AttemptDispatchFrame{
			AttemptID: "att_cf", TaskID: "tsk_cf", ProjectID: protocol.ScratchProjectID, LeaseToken: "l_cf",
			Adapter: protocol.AdapterAgy, WBSessionID: "ses_cf", DesiredConfig: map[string]string{"mode": "auto"}, Prompt: "hi",
		})
		_ = r.WaitAttemptDone(ctx, "att_cf")
		if tok := r.DelegateTokenForSession("ses_cf"); tok != "" {
			t.Fatalf("expected delegate token revoked on config failure, still got %q", tok)
		}
	})

	// 2. CloseSession error during eviction MUST still revoke delegate token -> 401
	t.Run("close_session_error_revokes_token", func(t *testing.T) {
		sockPath := filepath.Join(t.TempDir(), "closeerr.sock")
		drv := NewFakeAcpDriver()
		drv.OnPrompt = func(context.Context, *FakeAcpDriver, acp.SessionId, string) (acp.PromptResponse, error) {
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}
		drv.OnCloseSession = func(context.Context, acp.SessionId) error {
			return errors.New("close rpc transport error")
		}
		r, err := runner.New(ctx, runner.Config{
			MachineID: "hub-node", DataDir: t.TempDir(), SocketPath: sockPath, OrbitBinPath: fakeOrbitBin, Driver: drv,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Close(context.Background()) }()

		r.HandleDispatch(protocol.AttemptDispatchFrame{
			AttemptID: "att_ce", TaskID: "tsk_ce", ProjectID: protocol.ScratchProjectID, LeaseToken: "l_ce",
			Adapter: protocol.AdapterAgy, WBSessionID: "ses_ce", Prompt: "hi",
		})
		if err := r.WaitAttemptDone(ctx, "att_ce"); err != nil {
			t.Fatal(err)
		}
		oldTok := r.DelegateTokenForSession("ses_ce")
		if oldTok == "" {
			t.Fatal("expected delegate token before CloseSessionSafely")
		}
		if closed, err := r.CloseSessionSafely(ctx, "ses_ce"); err == nil || closed {
			t.Fatalf("expected CloseSessionSafely error, got closed=%v err=%v", closed, err)
		}
		if tok := r.DelegateTokenForSession("ses_ce"); tok != "" {
			t.Fatalf("expected delegate token revoked despite CloseSession error, still got %q", tok)
		}
		status, _, _ := r.HandleLocalDelegate(ctx, "list", oldTok, []byte(`{}`))
		if status != http.StatusUnauthorized {
			t.Fatalf("expected 401 for revoked token after CloseSession error, got %d", status)
		}
	})
}

func TestRunner_AdapterSwitchOnSameSlotRotatesDelegateToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tmp := t.TempDir()
	fakeOrbitBin := filepath.Join(tmp, "h8o")
	if err := os.WriteFile(fakeOrbitBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer hubSrv.Close()

	claudeDrv := NewFakeAcpDriver()
	codexDrv := NewFakeAcpDriver()
	codexPromptReady := make(chan struct{})
	codexRelease := make(chan struct{})
	claudeDrv.OnPrompt = func(context.Context, *FakeAcpDriver, acp.SessionId, string) (acp.PromptResponse, error) {
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	codexDrv.OnPrompt = func(context.Context, *FakeAcpDriver, acp.SessionId, string) (acp.PromptResponse, error) {
		close(codexPromptReady)
		<-codexRelease
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	r, err := runner.New(ctx, runner.Config{
		MachineID:         "hub-node",
		HubHTTPURL:        hubSrv.URL,
		BearerToken:       "tok",
		DataDir:           tmp,
		SocketPath:        filepath.Join(tmp, "rot.sock"),
		OrbitBinPath:      fakeOrbitBin,
		SupportedAdapters: []protocol.Adapter{protocol.AdapterClaude, protocol.AdapterCodex},
		Drivers: map[protocol.Adapter]runner.AcpAdapterDriver{
			protocol.AdapterClaude: claudeDrv,
			protocol.AdapterCodex:  codexDrv,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(context.Background()) }()

	// Attempt 1 on ses_rot with claude
	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:   "att_rot_1",
		TaskID:      "tsk_rot",
		ProjectID:   protocol.ScratchProjectID,
		LeaseToken:  "l_rot_1",
		Adapter:     protocol.AdapterClaude,
		WBSessionID: "ses_rot",
		Prompt:      "turn 1 on claude",
	})
	if err := r.WaitAttemptDone(ctx, "att_rot_1"); err != nil {
		t.Fatal(err)
	}
	tok1 := r.DelegateTokenForSession("ses_rot")
	srv1 := r.OrbitMCPServerNameForSession("ses_rot")
	if tok1 == "" || srv1 == "" {
		t.Fatalf("expected non-empty tok1 & srv1 after claude turn, got tok=%q srv=%q", tok1, srv1)
	}

	// Attempt 2 on SAME ses_rot switched to codex -> MUST rotate delegateToken & MCP server name and invalidate tok1!
	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:   "att_rot_2",
		TaskID:      "tsk_rot",
		ProjectID:   protocol.ScratchProjectID,
		LeaseToken:  "l_rot_2",
		Adapter:     protocol.AdapterCodex,
		WBSessionID: "ses_rot",
		Prompt:      "turn 2 on codex",
	})
	select {
	case <-codexPromptReady:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for codex prompt to start")
	}

	tok2 := r.DelegateTokenForSession("ses_rot")
	srv2 := r.OrbitMCPServerNameForSession("ses_rot")
	if tok2 == "" || tok2 == tok1 {
		t.Fatalf("expected rotated delegateToken on adapter switch, tok1=%q tok2=%q", tok1, tok2)
	}
	if srv2 == "" || srv2 == srv1 {
		t.Fatalf("expected rotated MCP server name on adapter switch, srv1=%q srv2=%q", srv1, srv2)
	}

	// While codex attempt 2 is live: old claude tok1 MUST return 401; new codex tok2 MUST return 200!
	st1, _, _ := r.HandleLocalDelegate(ctx, "list", tok1, []byte(`{}`))
	if st1 != http.StatusUnauthorized {
		t.Fatalf("expected 401 for old claude delegateToken after adapter switch, got %d", st1)
	}
	st2, _, _ := r.HandleLocalDelegate(ctx, "list", tok2, []byte(`{}`))
	if st2 != http.StatusOK {
		t.Fatalf("expected 200 for new codex delegateToken after adapter switch, got %d", st2)
	}

	close(codexRelease)
	if err := r.WaitAttemptDone(ctx, "att_rot_2"); err != nil {
		t.Fatal(err)
	}
}

func extractDelegateTokenFromMCPCtx(ctx context.Context) string {
	for _, srv := range runner.SessionMCPServersFromContext(ctx) {
		if srv.Stdio == nil {
			continue
		}
		for _, env := range srv.Stdio.Env {
			if env.Name == "ORBIT_DELEGATE_TOKEN" {
				return env.Value
			}
		}
	}
	return ""
}

func TestRunner_ColdRecoveryRevokesFailedResumeAndLoadTokens(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tmp := t.TempDir()
	fakeOrbitBin := filepath.Join(tmp, "h8o")
	if err := os.WriteFile(fakeOrbitBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer hubSrv.Close()

	drv := NewFakeAcpDriver()
	var mu sync.Mutex
	var resumeTok, loadTok, newTok string
	promptReady := make(chan struct{})
	releasePrompt := make(chan struct{})
	var turnCount atomic.Int32

	drv.OnResumeSession = func(rctx context.Context, _ acp.SessionId, _ string) error {
		mu.Lock()
		resumeTok = extractDelegateTokenFromMCPCtx(rctx)
		mu.Unlock()
		return errors.New("resume session not found")
	}
	drv.OnLoadSession = func(lctx context.Context, _ acp.SessionId, _ string) error {
		mu.Lock()
		loadTok = extractDelegateTokenFromMCPCtx(lctx)
		mu.Unlock()
		return errors.New("load session corrupted")
	}
	drv.OnNewSession = func(nctx context.Context, _, _ string) {
		mu.Lock()
		newTok = extractDelegateTokenFromMCPCtx(nctx)
		mu.Unlock()
	}
	drv.OnPrompt = func(pctx context.Context, _ *FakeAcpDriver, _ acp.SessionId, _ string) (acp.PromptResponse, error) {
		if turnCount.Add(1) == 2 {
			close(promptReady)
			select {
			case <-releasePrompt:
			case <-pctx.Done():
				return acp.PromptResponse{}, pctx.Err()
			}
		}
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	r, err := runner.New(ctx, runner.Config{
		MachineID:    "hub-node",
		HubHTTPURL:   hubSrv.URL,
		BearerToken:  "tok",
		DataDir:      tmp,
		SocketPath:   filepath.Join(tmp, "cold.sock"),
		OrbitBinPath: fakeOrbitBin,
		Driver:       drv,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(context.Background()) }()

	workDir := filepath.Join(tmp, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Turn 1: establishes slot with providerSessionID & cwd
	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:     "att_cold_1",
		TaskID:        "tsk_cold",
		ProjectID:     "demo-plain",
		LeaseToken:    "l_cold_1",
		Adapter:       protocol.AdapterAgy,
		WBSessionID:   "ses_cold",
		IsPlainFolder: true,
		RepoPath:      workDir,
		Prompt:        "turn 1",
	})
	if err := r.WaitAttemptDone(ctx, "att_cold_1"); err != nil {
		t.Fatal(err)
	}
	_, sid1, cwd1, _ := r.SessionStateSnapshot("ses_cold")
	if sid1 == "" || cwd1 == "" {
		t.Fatalf("expected sid1 & cwd1 after turn 1, got sid=%q cwd=%q", sid1, cwd1)
	}

	// Simulate driver process restart (sid1 no longer live in memory) so Turn 2 enters Resume -> Load -> NewSession fallback!
	drv.mu.Lock()
	delete(drv.liveSessions, sid1)
	drv.mu.Unlock()

	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:          "att_cold_2",
		TaskID:             "tsk_cold",
		ProjectID:          "demo-plain",
		LeaseToken:         "l_cold_2",
		Adapter:            protocol.AdapterAgy,
		WBSessionID:        "ses_cold",
		IsPlainFolder:      true,
		RepoPath:           workDir,
		ProviderSessionID:  string(sid1),
		ExpectedSessionCWD: cwd1,
		Prompt:             "turn 2 cold recovery",
	})
	select {
	case <-promptReady:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cold-recovered turn 2 prompt")
	}

	mu.Lock()
	rTok, lTok, nTok := resumeTok, loadTok, newTok
	mu.Unlock()
	if rTok == "" || lTok == "" || nTok == "" {
		t.Fatalf("expected distinct tokens captured for resume/load/new, got resume=%q load=%q new=%q", rTok, lTok, nTok)
	}
	if rTok == lTok || rTok == nTok || lTok == nTok {
		t.Fatalf("resume, load, and fallback newSession MUST each mint a fresh delegateToken (resume=%q load=%q new=%q)", rTok, lTok, nTok)
	}

	// While recovered session is actively running: tokens handed to failed ResumeSession & LoadSession MUST return 401;
	// only the winning NewSession token returns 200!
	if st, _, _ := r.HandleLocalDelegate(ctx, "list", rTok, []byte(`{}`)); st != http.StatusUnauthorized {
		t.Fatalf("expected 401 for failed ResumeSession delegateToken, got %d", st)
	}
	if st, _, _ := r.HandleLocalDelegate(ctx, "list", lTok, []byte(`{}`)); st != http.StatusUnauthorized {
		t.Fatalf("expected 401 for failed LoadSession delegateToken, got %d", st)
	}
	if st, _, _ := r.HandleLocalDelegate(ctx, "list", nTok, []byte(`{}`)); st != http.StatusOK {
		t.Fatalf("expected 200 for active fallback NewSession delegateToken, got %d", st)
	}

	close(releasePrompt)
	if err := r.WaitAttemptDone(ctx, "att_cold_2"); err != nil {
		t.Fatal(err)
	}
}

func TestRunner_DriverMCPFallbackRevokesInitialDelegateToken(t *testing.T) {
	for _, tc := range []struct {
		name          string
		rejectWithMCP bool
		wantStatus    int
		wantNewCalls  int
	}{
		{name: "fallback_without_mcp_revokes_initial_token", rejectWithMCP: true, wantStatus: http.StatusUnauthorized, wantNewCalls: 2},
		{name: "attached_mcp_allows_token", rejectWithMCP: false, wantStatus: http.StatusOK, wantNewCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			tmp := t.TempDir()
			fakeOrbitBin := filepath.Join(tmp, "h8o")
			if err := os.WriteFile(fakeOrbitBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}

			hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			}))
			defer hubSrv.Close()

			agentToRunnerR, agentToRunnerW := io.Pipe()
			runnerToAgentR, runnerToAgentW := io.Pipe()
			defer agentToRunnerR.Close()
			defer agentToRunnerW.Close()
			defer runnerToAgentR.Close()
			defer runnerToAgentW.Close()

			promptReady := make(chan struct{})
			releasePrompt := make(chan struct{})
			agent := &mcpTestPipeAgent{
				rejectWithMCP: tc.rejectWithMCP,
				onPrompt: func(pctx context.Context, _ acp.PromptRequest) (acp.PromptResponse, error) {
					close(promptReady)
					select {
					case <-releasePrompt:
					case <-pctx.Done():
						return acp.PromptResponse{}, pctx.Err()
					}
					return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
				},
			}
			_ = acp.NewAgentSideConnection(agent, agentToRunnerW, runnerToAgentR)

			drv := runner.NewStreamAcpDriver(runner.AgyDriverConfig{Adapter: protocol.AdapterAgy}, runnerToAgentW, agentToRunnerR)
			r, err := runner.New(ctx, runner.Config{
				MachineID:    "hub-node",
				HubHTTPURL:   hubSrv.URL,
				BearerToken:  "tok",
				DataDir:      tmp,
				SocketPath:   filepath.Join(tmp, "fb.sock"),
				OrbitBinPath: fakeOrbitBin,
				Driver:       drv,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close(context.Background()) }()

			r.HandleDispatch(protocol.AttemptDispatchFrame{
				AttemptID:   "att_fb_1",
				TaskID:      "tsk_fb",
				ProjectID:   protocol.ScratchProjectID,
				LeaseToken:  "l_fb_1",
				Adapter:     protocol.AdapterAgy,
				WBSessionID: "ses_fb",
				Prompt:      "run",
			})
			select {
			case <-promptReady:
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for prompt to enter running state")
			}

			// Extract the delegate token handed to the FIRST NewSession call (with McpServers)
			agent.mu.Lock()
			gotNewCalls := len(agent.newCalls)
			var initialTok string
			if gotNewCalls > 0 && len(agent.newCalls[0].McpServers) > 0 && agent.newCalls[0].McpServers[0].Stdio != nil {
				for _, env := range agent.newCalls[0].McpServers[0].Stdio.Env {
					if env.Name == "ORBIT_DELEGATE_TOKEN" {
						initialTok = env.Value
					}
				}
			}
			agent.mu.Unlock()

			if gotNewCalls != tc.wantNewCalls {
				t.Fatalf("expected %d NewSession calls, got %d", tc.wantNewCalls, gotNewCalls)
			}
			if initialTok == "" {
				t.Fatal("expected initial NewSession call to receive ORBIT_DELEGATE_TOKEN")
			}

			// While session is actively running, call Socket /delegate/list with the initial token
			st, _, _ := r.HandleLocalDelegate(ctx, "list", initialTok, []byte(`{}`))
			if st != tc.wantStatus {
				t.Fatalf("expected HTTP %d for initial delegate token, got %d", tc.wantStatus, st)
			}
			if tc.rejectWithMCP {
				if tok := r.DelegateTokenForSession("ses_fb"); tok != "" {
					t.Fatalf("expected empty slot delegate token after MCP fallback, got %q", tok)
				}
				if srv := r.OrbitMCPServerNameForSession("ses_fb"); srv != "" {
					t.Fatalf("expected empty slot MCP server name after MCP fallback, got %q", srv)
				}
			}

			close(releasePrompt)
			if err := r.WaitAttemptDone(ctx, "att_fb_1"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAgyAcpDriver_NoMCPFallbackOnTransportOrTimeoutErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rejectErr error
	}{
		{name: "timeout", rejectErr: errors.New("context deadline exceeded: upstream timeout")},
		{name: "mcp_startup_exit_status", rejectErr: errors.New("failed to start mcp server: exit status 1")},
		{name: "mcp_spawn_permission_denied", rejectErr: errors.New("spawn h8o mcp-serve: permission denied")},
		{name: "non_mcp_invalid_params_unknown_field", rejectErr: errors.New("invalid params: unknown field foo")},
		{name: "non_mcp_jsonrpc_32602", rejectErr: errors.New("rpc error -32602: invalid params for cwd")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			agentToRunnerR, agentToRunnerW := io.Pipe()
			runnerToAgentR, runnerToAgentW := io.Pipe()
			defer agentToRunnerR.Close()
			defer agentToRunnerW.Close()
			defer runnerToAgentR.Close()
			defer runnerToAgentW.Close()

			agent := &mcpTestPipeAgent{
				rejectWithMCP: true,
				rejectErr:     tc.rejectErr,
			}
			_ = acp.NewAgentSideConnection(agent, agentToRunnerW, runnerToAgentR)

			drv := runner.NewStreamAcpDriver(runner.AgyDriverConfig{Adapter: protocol.AdapterAgy}, runnerToAgentW, agentToRunnerR)
			if err := drv.Start(ctx, nil, nil); err != nil {
				t.Fatalf("driver start: %v", err)
			}
			defer func() { _ = drv.Close(context.Background()) }()

			var attached bool
			mcpCtx := runner.WithSessionMCPAttachReport(runner.WithSessionMCPServers(ctx, []acp.McpServer{
				{Stdio: &acp.McpServerStdio{Name: "orbit12345678", Command: "/bin/true", Args: []string{"mcp-serve"}}},
			}), &attached)

			if _, err := drv.NewSession(mcpCtx, t.TempDir(), ""); err == nil {
				t.Fatalf("expected error without fallback for %s", tc.name)
			}
			agent.mu.Lock()
			defer agent.mu.Unlock()
			if len(agent.newCalls) != 1 {
				t.Fatalf("%s error MUST NOT retry without MCP servers, got %d NewSession calls", tc.name, len(agent.newCalls))
			}
			if attached {
				t.Fatal("attached flag must remain false on failure")
			}
		})
	}
}
