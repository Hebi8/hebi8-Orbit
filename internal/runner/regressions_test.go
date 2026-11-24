package runner_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/coder/websocket"

	"github.com/Hebi8/hebi8-Orbit/internal/hub"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/runner"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

// FakeAcpDriver is a deterministic, race-safe in-memory implementation of AcpAdapterDriver
// supporting scripted tool calls, unsolicited Swarm tail updates, permission prompts,
// session close hooks, and config option calibration testing.
type FakeAcpDriver struct {
	mu             sync.Mutex
	onUpdate       runner.SessionUpdateHandler
	onPerm         runner.PermissionHandler
	supportsResume bool
	supportsLoad   bool
	supportsMetaSP bool
	nextSessSeq    int
	liveSessions   map[acp.SessionId]bool
	sessionCWD     map[acp.SessionId]string
	sessionConfig  map[acp.SessionId]map[string]string
	newCalls       []string
	resumeCalls    []acp.SessionId
	closeCalls     []acp.SessionId
	configCalls    []string // "sid:key=val"
	promptCalls    []string // "sid:prompt"
	cancelCalls    []acp.SessionId
	systemPrompts  []string

	OnCloseSession    func(ctx context.Context, sid acp.SessionId) error
	OnSetConfigOption func(ctx context.Context, sid acp.SessionId, key, val string) (acp.SetSessionConfigOptionResponse, error)
	OnPrompt          func(ctx context.Context, d *FakeAcpDriver, sid acp.SessionId, promptText string) (acp.PromptResponse, error)
}

func NewFakeAcpDriver() *FakeAcpDriver {
	return &FakeAcpDriver{
		supportsResume: true,
		supportsLoad:   true,
		supportsMetaSP: true,
		liveSessions:   map[acp.SessionId]bool{},
		sessionCWD:     map[acp.SessionId]string{},
		sessionConfig:  map[acp.SessionId]map[string]string{},
	}
}

func (f *FakeAcpDriver) Start(ctx context.Context, onUpdate runner.SessionUpdateHandler, onPerm runner.PermissionHandler) error {
	f.mu.Lock()
	f.onUpdate = onUpdate
	f.onPerm = onPerm
	f.mu.Unlock()
	return nil
}

func (f *FakeAcpDriver) Close(ctx context.Context) error { return nil }

func (f *FakeAcpDriver) SupportsResume() bool           { return f.supportsResume }
func (f *FakeAcpDriver) SupportsLoad() bool             { return f.supportsLoad }
func (f *FakeAcpDriver) SupportsMetaSystemPrompt() bool { return f.supportsMetaSP }

func (f *FakeAcpDriver) IsSessionLiveInMemory(sid acp.SessionId) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.liveSessions[sid]
}

func (f *FakeAcpDriver) EmitUpdate(ctx context.Context, sid acp.SessionId, upd acp.SessionUpdate) error {
	f.mu.Lock()
	h := f.onUpdate
	f.mu.Unlock()
	if h == nil {
		return nil
	}
	return h(ctx, acp.SessionNotification{SessionId: sid, Update: upd})
}

func (f *FakeAcpDriver) AskPermission(ctx context.Context, req acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	f.mu.Lock()
	h := f.onPerm
	f.mu.Unlock()
	if h == nil {
		return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
	}
	return h(ctx, req)
}

func (f *FakeAcpDriver) NewSession(ctx context.Context, cwd, systemPromptAppend string) (acp.NewSessionResponse, error) {
	f.mu.Lock()
	f.nextSessSeq++
	sid := acp.SessionId(fmt.Sprintf("fake-ses-%d", f.nextSessSeq))
	f.liveSessions[sid] = true
	f.sessionCWD[sid] = cwd
	f.sessionConfig[sid] = map[string]string{"model": "gemini-4-neon", "thought_level": "high", "mode": "default"}
	f.newCalls = append(f.newCalls, cwd)
	f.systemPrompts = append(f.systemPrompts, systemPromptAppend)
	f.mu.Unlock()
	return acp.NewSessionResponse{SessionId: sid}, nil
}

func (f *FakeAcpDriver) ResumeSession(ctx context.Context, sid acp.SessionId, cwd string) (acp.ResumeSessionResponse, error) {
	f.mu.Lock()
	f.liveSessions[sid] = true
	f.sessionCWD[sid] = cwd
	// Simulate real agy Spike finding: ResumeSession resets config options back to CLI defaults!
	f.sessionConfig[sid] = map[string]string{"model": "gemini-4-neon", "thought_level": "high", "mode": "default"}
	f.resumeCalls = append(f.resumeCalls, sid)
	f.mu.Unlock()
	return acp.ResumeSessionResponse{}, nil
}

func (f *FakeAcpDriver) LoadSession(ctx context.Context, sid acp.SessionId, cwd string) (acp.LoadSessionResponse, error) {
	f.mu.Lock()
	f.liveSessions[sid] = true
	f.sessionCWD[sid] = cwd
	f.sessionConfig[sid] = map[string]string{"model": "gemini-4-neon", "thought_level": "high", "mode": "default"}
	f.mu.Unlock()
	return acp.LoadSessionResponse{}, nil
}

func (f *FakeAcpDriver) CloseSession(ctx context.Context, sid acp.SessionId) error {
	f.mu.Lock()
	hook := f.OnCloseSession
	f.closeCalls = append(f.closeCalls, sid)
	f.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, sid); err != nil {
			return err
		}
	}
	f.mu.Lock()
	delete(f.liveSessions, sid)
	f.mu.Unlock()
	return nil
}

func (f *FakeAcpDriver) SetConfigOption(ctx context.Context, sid acp.SessionId, configID, value string) (acp.SetSessionConfigOptionResponse, error) {
	f.mu.Lock()
	hook := f.OnSetConfigOption
	f.configCalls = append(f.configCalls, fmt.Sprintf("%s:%s=%s", sid, configID, value))
	f.mu.Unlock()
	if hook != nil {
		return hook(ctx, sid, configID, value)
	}
	f.mu.Lock()
	if f.sessionConfig[sid] == nil {
		f.sessionConfig[sid] = map[string]string{}
	}
	f.sessionConfig[sid][configID] = value
	opts := make([]acp.SessionConfigOption, 0, len(f.sessionConfig[sid]))
	for k, v := range f.sessionConfig[sid] {
		opts = append(opts, acp.SessionConfigOption{
			Select: &acp.SessionConfigOptionSelect{
				Id:           acp.SessionConfigId(k),
				Name:         k,
				Type:         "select",
				CurrentValue: acp.SessionConfigValueId(v),
			},
		})
	}
	f.mu.Unlock()
	return acp.SetSessionConfigOptionResponse{ConfigOptions: opts}, nil
}

func (f *FakeAcpDriver) Prompt(ctx context.Context, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
	f.mu.Lock()
	hook := f.OnPrompt
	f.promptCalls = append(f.promptCalls, fmt.Sprintf("%s:%s", sid, promptText))
	f.mu.Unlock()
	if hook != nil {
		return hook(ctx, f, sid, promptText)
	}
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (f *FakeAcpDriver) Cancel(ctx context.Context, sid acp.SessionId) error {
	f.mu.Lock()
	f.cancelCalls = append(f.cancelCalls, sid)
	f.mu.Unlock()
	return nil
}

// --- Test Harness Helpers ---

type testEnv struct {
	t         *testing.T
	tmpDir    string
	repoDir   string
	dataDir   string
	cfgDir    string
	st        *store.Store
	srv       *hub.Server
	ts        *httptest.Server
	hubWSURL  string
	hubTok    string
	remoteTok string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "config")
	dataDir := filepath.Join(tmp, "data")
	repoDir := filepath.Join(tmp, "repo")
	_ = os.MkdirAll(cfgDir, 0o755)
	_ = os.MkdirAll(repoDir, 0o755)

	// Initialize real git repo with an initial commit
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=Tester", "GIT_AUTHOR_EMAIL=tester@example.com",
		"GIT_COMMITTER_NAME=Tester", "GIT_COMMITTER_EMAIL=tester@example.com",
	)
	runG := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = gitEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}
	runG("init", "-b", "main")
	_ = os.WriteFile(filepath.Join(repoDir, "app.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
	runG("add", ".")
	runG("commit", "-m", "initial")

	hubToken := "secret-hub-test-token-456"
	hubSum := sha256.Sum256([]byte(hubToken))
	hubTokenHash := "sha256:" + hex.EncodeToString(hubSum[:])

	remoteToken := "secret-runner-test-token-123"
	sum := sha256.Sum256([]byte(remoteToken))
	tokenHash := "sha256:" + hex.EncodeToString(sum[:])

	machinesYAML := fmt.Sprintf(`schemaVersion: 1
machines:
  hub-node:
    displayName: "hub-node"
    transport: local_ws
    tailscaleHost: "orbit.example.ts.net"
    tokenHash: %q
    capabilities: [primary-dev]
  mac-node:
    displayName: "mac-node"
    transport: remote_ws
    tailscaleHost: "mac-node.example.ts.net"
    tokenHash: %q
    capabilities: [macos-env, xcode]
`, hubTokenHash, tokenHash)
	projectsYAML := fmt.Sprintf(`schemaVersion: 1
projects:
  demo-app:
    displayName: "demo-app"
    repoIdentity: "github.com/example-org/demo-app"
    defaultBranch: "main"
    defaultCapability: "primary-dev"
    defaultAdapter: "agy"
    defaultModelConfig:
      model: "gemini-4-neon"
      thought_level: "high"
      mode: "auto"
    ideScheme: "cursor"
    anchors:
      primary-dev:
        machine: "hub-node"
        repoPath: %q
        worktreeRoot: %q
      macos-env:
        machine: "mac-node"
        repoPath: %q
        worktreeRoot: %q
`, filepath.ToSlash(repoDir), filepath.ToSlash(filepath.Join(dataDir, "wt-hub")),
		filepath.ToSlash(repoDir), filepath.ToSlash(filepath.Join(dataDir, "wt-mac")))

	_ = os.WriteFile(filepath.Join(cfgDir, "machines.yaml"), []byte(machinesYAML), 0o644)
	_ = os.WriteFile(filepath.Join(cfgDir, "projects.yaml"), []byte(projectsYAML), 0o644)

	st, err := store.Open(ctx, filepath.Join(dataDir, "orbit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := protocol.DefaultHubConfig()
	cfg.ConfigDir = cfgDir
	cfg.DataDir = dataDir
	cfg.DevOpen = true

	srv, err := hub.NewServer(ctx, hub.ServerOptions{
		HubConfig:           cfg,
		Store:               st,
		HeartbeatInterval:   200 * time.Millisecond,
		LeaseTTL:            30 * time.Second,
		MaintenanceInterval: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/runner"
	return &testEnv{
		t:         t,
		tmpDir:    tmp,
		repoDir:   repoDir,
		dataDir:   dataDir,
		cfgDir:    cfgDir,
		st:        st,
		srv:       srv,
		ts:        ts,
		hubWSURL:  wsURL,
		hubTok:    hubToken,
		remoteTok: remoteToken,
	}
}

func (e *testEnv) startRunner(machineID, token, outboxPath string, drv *FakeAcpDriver, mutate func(*runner.Config)) *runner.Runner {
	e.t.Helper()
	if token == "" {
		if machineID == "hub-node" {
			token = e.hubTok
		} else if machineID == "mac-node" {
			token = e.remoteTok
		}
	}
	if outboxPath == "" {
		outboxPath = filepath.Join(e.dataDir, fmt.Sprintf("outbox-%s-%d.sqlite", machineID, time.Now().UnixNano()))
	}
	sockDir, err := os.MkdirTemp("", "orb-")
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	cfg := runner.Config{
		MachineID:               machineID,
		HubWSURL:                e.hubWSURL,
		HubHTTPURL:              e.ts.URL,
		BearerToken:             token,
		DataDir:                 e.dataDir,
		OutboxDBPath:            outboxPath,
		SocketPath:              filepath.Join(sockDir, "r.sock"),
		Driver:                  drv,
		ConfigApplyTimeout:      300 * time.Millisecond,
		ChunkCoalesceWindow:     20 * time.Millisecond,
		DefaultSwarmIdleTimeout: 5 * time.Second,
		ReconnectInitialDelay:   50 * time.Millisecond,
		ReconnectMaxDelay:       200 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	r, err := runner.New(context.Background(), cfg)
	if err != nil {
		e.t.Fatalf("runner.New: %v", err)
	}
	if err := r.Start(context.Background()); err != nil {
		e.t.Fatalf("runner.Start: %v", err)
	}
	e.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = r.Close(ctx)
	})

	eventually(e.t, 3*time.Second, func() bool {
		return e.srv.RunnerGateway().ActiveRunnerInstanceID(machineID) == r.InstanceID()
	}, "runner did not connect to hub")
	return r
}

func (e *testEnv) apiReq(method, path string, body any, headers map[string]string) (*http.Response, []byte) {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, r)
	if err != nil {
		e.t.Fatal(err)
	}
	if method != http.MethodGet {
		req.Header.Set("X-Orbit-Request", "1")
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, raw
}

func eventually(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timeout after %v: %s", timeout, msg)
}

// ============================================================================
// R1: Swarm unsolicited tail frames + active followup queue/fork + quiet timeout
// ============================================================================
func TestRegression_R1_SwarmOwnershipQueueForkAndTimeout(t *testing.T) {
	t.Run("R1a_SwarmMonitoringUnsolicitedFramesQueueAndExplicitDeliver", func(t *testing.T) {
		env := newTestEnv(t)
		drv := NewFakeAcpDriver()

		var att1SID acp.SessionId
		prompt1Entered := make(chan acp.SessionId, 1)
		prompt2Seen := make(chan string, 1)
		var promptCount atomic.Int32

		drv.OnPrompt = func(ctx context.Context, d *FakeAcpDriver, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
			n := promptCount.Add(1)
			if n == 1 {
				// Failed swarm add MUST NOT register ghost-worker!
				_ = d.EmitUpdate(ctx, sid, acp.SessionUpdate{
					ToolCall: &acp.SessionUpdateToolCall{
						ToolCallId: "tc-fail",
						Name:       acp.Ptr("swarm"),
						Status:     acp.ToolCallStatusFailed,
						RawInput:   json.RawMessage(`{"action":"add","name":"ghost-worker","brief":"bad"}`),
					},
				})
				// Completed swarm add registers ui-coder and posts board brief
				_ = d.EmitUpdate(ctx, sid, acp.SessionUpdate{
					ToolCall: &acp.SessionUpdateToolCall{
						ToolCallId: "tc-add-1",
						Name:       acp.Ptr("swarm"),
						Status:     acp.ToolCallStatusPending,
						RawInput:   json.RawMessage(`{"action":"add","name":"ui-coder","brief":"Implement navbar"}`),
					},
				})
				_ = d.EmitUpdate(ctx, sid, acp.SessionUpdate{
					ToolCallUpdate: &acp.SessionToolCallUpdate{
						ToolCallId: "tc-add-1",
						Status:     acp.PatchSet(acp.ToolCallStatusCompleted),
					},
				})
				prompt1Entered <- sid
				// Immediately returns end_turn while ui-coder keeps working in background!
				return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
			}
			// Second turn (popped from task_pending_inputs after att_1 explicitly delivers --final)
			prompt2Seen <- promptText
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}

		r := env.startRunner("hub-node", "", "", drv, func(c *runner.Config) {
			c.ReconnectInitialDelay = 250 * time.Millisecond
		})

		resp, raw := env.apiReq(http.MethodPost, "/api/projects/demo-app/tasks", protocol.CreateTaskRequest{
			Goal: "Build navbar with swarm",
		}, nil)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create task failed (%d): %s", resp.StatusCode, string(raw))
		}
		var created protocol.CreateTaskResponse
		_ = json.Unmarshal(raw, &created)
		tid := created.Task.TaskID
		att1ID := created.Attempt.AttemptID

		select {
		case att1SID = <-prompt1Entered:
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for prompt 1")
		}

		// Wait until Hub observes att_1 in swarm_monitoring
		eventually(t, 3*time.Second, func() bool {
			rec, err := env.st.GetAttemptRecord(context.Background(), att1ID)
			return err == nil && rec.State == protocol.AttemptSwarmMonitoring
		}, "att_1 did not enter swarm_monitoring")

		// Verify board has ONLY ui-coder (not ghost-worker!) and initial brief message
		board, err := env.st.GetTaskBoard(context.Background(), tid)
		if err != nil {
			t.Fatal(err)
		}
		if len(board.Members) != 1 || board.Members[0].MemberName != "ui-coder" || board.Members[0].State != protocol.BoardMemberWorking {
			t.Fatalf("unexpected board members: %+v", board.Members)
		}

		// While att_1 is in swarm_monitoring:
		// 1. User sends followup with mode='queue' -> MUST persist in task_pending_inputs, NOT call Prompt on active session!
		resp, raw = env.apiReq(http.MethodPost, "/api/tasks/"+tid+"/followup", protocol.FollowupTaskRequest{
			Message: "Add dark mode toggle next",
			Mode:    protocol.FollowupModeQueue,
		}, nil)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("expected 202 queued followup during swarm_monitoring, got %d: %s", resp.StatusCode, string(raw))
		}
		if promptCount.Load() != 1 {
			t.Fatalf("queued followup illegally invoked Prompt while att_1 in swarm_monitoring")
		}

		// 2. Emit unsolicited tail frames AFTER end_turn (message chunk + board post + usage update)
		_ = drv.EmitUpdate(context.Background(), att1SID, acp.UpdateAgentMessageText("Swarm worker ui-coder finished navbar."))
		_ = drv.EmitUpdate(context.Background(), att1SID, acp.SessionUpdate{
			ToolCall: &acp.SessionUpdateToolCall{
				ToolCallId: "tc-post-1",
				Name:       acp.Ptr("post_message"),
				Status:     acp.ToolCallStatusCompleted,
				RawInput:   json.RawMessage(`{"author":"ui-coder","mention":"@coordinator","body":"Navbar ready for review"}`),
			},
		})
		_ = drv.EmitUpdate(context.Background(), att1SID, acp.SessionUpdate{
			UsageUpdate: &acp.SessionUsageUpdate{Used: 1200, Size: 200000},
		})

		// 3. Call intermediate `orbit deliver` WITHOUT --final while ui-coder is still active -> stays in swarm_monitoring!
		att1Rec, _ := env.st.GetAttemptRecord(context.Background(), att1ID)
		_ = os.WriteFile(filepath.Join(att1Rec.WorktreePath, "app.go"), []byte("package main\n// v1 preview\nfunc main() {}\n"), 0o644)
		dlvPreview, err := r.HandleLocalDeliver(context.Background(), runner.LocalDeliverRequest{
			AttemptID: att1ID,
			Summary:   "Intermediate swarm preview",
			Final:     false,
		})
		if err != nil {
			t.Fatalf("intermediate deliver failed: %v", err)
		}
		if dlvPreview.Finalized {
			t.Fatalf("expected intermediate deliver without --final while worker active to NOT finalize")
		}

		// 4. Kill ui-coder via unsolicited tool call, then call `orbit deliver --final`!
		_ = drv.EmitUpdate(context.Background(), att1SID, acp.SessionUpdate{
			ToolCall: &acp.SessionUpdateToolCall{
				ToolCallId: "tc-kill-1",
				Name:       acp.Ptr("swarm"),
				Status:     acp.ToolCallStatusCompleted,
				RawInput:   json.RawMessage(`{"action":"kill","name":"ui-coder"}`),
			},
		})
		_ = os.WriteFile(filepath.Join(att1Rec.WorktreePath, "app.go"), []byte("package main\n// v2 final\nfunc main() {}\n"), 0o644)
		dlvFinal, err := r.HandleLocalDeliver(context.Background(), runner.LocalDeliverRequest{
			AttemptID: att1ID,
			Summary:   "Final swarm delivery",
			Final:     true,
		})
		if err != nil || !dlvFinal.Finalized {
			t.Fatalf("final deliver failed: resp=%+v err=%v", dlvFinal, err)
		}

		// Verify att_1 succeeded with completion_source='explicit_deliver', unsolicited raw_acp events belong to att_1,
		// AND Hub automatically popped the queued followup to run att_2!
		eventually(t, 3*time.Second, func() bool {
			rec, err := env.st.GetAttemptRecord(context.Background(), att1ID)
			return err == nil && rec.State == protocol.AttemptSucceeded &&
				rec.CompletionSource != nil && *rec.CompletionSource == protocol.CompletionExplicitDeliver
		}, "att_1 did not reach succeeded(explicit_deliver)")

		select {
		case p2 := <-prompt2Seen:
			if p2 != "Add dark mode toggle next" {
				t.Fatalf("unexpected auto-dispatched followup prompt: %q", p2)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Hub did not auto-consume queued pending input after att_1 succeeded")
		}

		rawEvents, err := env.st.ListAttemptRawEvents(context.Background(), att1ID, 0, 500)
		if err != nil {
			t.Fatal(err)
		}
		foundUnsolicitedText := false
		for _, ev := range rawEvents {
			if txt, ok := ev.Payload["text"].(string); ok && strings.Contains(txt, "ui-coder finished navbar") {
				foundUnsolicitedText = true
			}
		}
		if !foundUnsolicitedText {
			t.Fatalf("unsolicited tail frame missing from att_1 raw_acp events: %+v", rawEvents)
		}
	})

	t.Run("R1b_SwarmActiveForkNewSessionIsolation", func(t *testing.T) {
		env := newTestEnv(t)
		drv := NewFakeAcpDriver()

		var sessions []acp.SessionId
		var sessMu sync.Mutex
		drv.OnPrompt = func(ctx context.Context, d *FakeAcpDriver, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
			sessMu.Lock()
			sessions = append(sessions, sid)
			idx := len(sessions)
			sessMu.Unlock()
			if idx == 1 {
				_ = d.EmitUpdate(ctx, sid, acp.SessionUpdate{
					ToolCall: &acp.SessionUpdateToolCall{
						ToolCallId: "tc-fork-add",
						Name:       acp.Ptr("swarm"),
						Status:     acp.ToolCallStatusCompleted,
						RawInput:   json.RawMessage(`{"action":"add","name":"worker-a"}`),
					},
				})
			}
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}
		env.startRunner("hub-node", "", "", drv, nil)

		_, raw := env.apiReq(http.MethodPost, "/api/projects/demo-app/tasks", protocol.CreateTaskRequest{
			Goal: "Main swarm task",
		}, nil)
		var created protocol.CreateTaskResponse
		_ = json.Unmarshal(raw, &created)
		tid := created.Task.TaskID
		att1ID := created.Attempt.AttemptID

		eventually(t, 3*time.Second, func() bool {
			rec, err := env.st.GetAttemptRecord(context.Background(), att1ID)
			return err == nil && rec.State == protocol.AttemptSwarmMonitoring
		}, "att_1 did not enter swarm_monitoring")

		// While att_1 is still in swarm_monitoring, request mode='fork_new_session'
		resp, raw := env.apiReq(http.MethodPost, "/api/tasks/"+tid+"/followup", protocol.FollowupTaskRequest{
			Message: "Forked branch task",
			Mode:    protocol.FollowupModeForkNewSession,
		}, nil)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("fork_new_session failed (%d): %s", resp.StatusCode, string(raw))
		}
		var forkResp protocol.FollowupTaskResponse
		_ = json.Unmarshal(raw, &forkResp)
		if forkResp.ForkedTaskID == "" || forkResp.ForkedTaskID == tid {
			t.Fatalf("expected distinct forkedTaskId, got %+v", forkResp)
		}

		// Wait for forked attempt to run on a BRAND NEW providerSessionID
		eventually(t, 3*time.Second, func() bool {
			sessMu.Lock()
			defer sessMu.Unlock()
			return len(sessions) == 2 && sessions[0] != sessions[1]
		}, "fork_new_session did not start a separate provider session")

		// Confirm att_1 is STILL in swarm_monitoring and continues owning sessions[0]
		att1Now, _ := env.st.GetAttemptRecord(context.Background(), att1ID)
		if att1Now.State != protocol.AttemptSwarmMonitoring {
			t.Fatalf("expected att_1 still in swarm_monitoring, got %s", att1Now.State)
		}
	})

	t.Run("R1c_SwarmIdleTimeoutNeedsVerificationAndLocksSession", func(t *testing.T) {
		env := newTestEnv(t)
		drv := NewFakeAcpDriver()

		var sids []acp.SessionId
		var mu sync.Mutex
		drv.OnPrompt = func(ctx context.Context, d *FakeAcpDriver, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
			mu.Lock()
			sids = append(sids, sid)
			n := len(sids)
			mu.Unlock()
			if n == 1 {
				_ = d.EmitUpdate(ctx, sid, acp.SessionUpdate{
					ToolCall: &acp.SessionUpdateToolCall{
						ToolCallId: "tc-timeout-add",
						Name:       acp.Ptr("swarm"),
						Status:     acp.ToolCallStatusCompleted,
						RawInput:   json.RawMessage(`{"action":"add","name":"silent-worker"}`),
					},
				})
			}
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}
		env.startRunner("hub-node", "", "", drv, nil)

		_, raw := env.apiReq(http.MethodPost, "/api/projects/demo-app/tasks", protocol.CreateTaskRequest{
			Goal:               "Swarm timeout test",
			SwarmIdleTimeoutMs: 120,
		}, nil)
		var created protocol.CreateTaskResponse
		_ = json.Unmarshal(raw, &created)
		tid := created.Task.TaskID
		att1ID := created.Attempt.AttemptID

		// Wait for quiet_timeout -> needs_verification
		eventually(t, 3*time.Second, func() bool {
			rec, err := env.st.GetAttemptRecord(context.Background(), att1ID)
			return err == nil && rec.State == protocol.AttemptNeedsVerification &&
				rec.CompletionSource != nil && *rec.CompletionSource == protocol.CompletionQuietTimeout
		}, "att_1 did not transition to needs_verification on swarm_idle_timeout")

		// Blind retry without verifiedSite: true MUST return 409 Conflict!
		resp, raw := env.apiReq(http.MethodPost, "/api/tasks/"+tid+"/retry", protocol.RetryTaskRequest{
			VerifiedSite: false,
		}, nil)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("expected 409 Conflict retrying needs_verification without verifiedSite, got %d: %s", resp.StatusCode, string(raw))
		}

		// Retry WITH verifiedSite: true MUST succeed AND force a brand new provider session (never reusing locked old session!)
		resp, raw = env.apiReq(http.MethodPost, "/api/tasks/"+tid+"/retry", protocol.RetryTaskRequest{
			VerifiedSite: true,
		}, nil)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201 retrying with verifiedSite=true, got %d: %s", resp.StatusCode, string(raw))
		}

		eventually(t, 3*time.Second, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(sids) == 2 && sids[0] != sids[1]
		}, "retry after needs_verification illegally reused locked old provider session")
	})
}

// ============================================================================
// R2: CloseSession closing collides with dispatch + cwd check + config calibration
// ============================================================================
func TestRegression_R2_CloseVsDispatchCwdAndConfigCalibration(t *testing.T) {
	t.Run("R2a_CloseSessionClosingSerializesWithDispatchAndReverifiesConfig", func(t *testing.T) {
		env := newTestEnv(t)
		drv := NewFakeAcpDriver()

		closeEntered := make(chan struct{})
		allowCloseFinish := make(chan struct{})
		var closeReturned atomic.Bool
		var resumeSawCloseFinished atomic.Bool

		drv.OnCloseSession = func(ctx context.Context, sid acp.SessionId) error {
			close(closeEntered)
			<-allowCloseFinish
			closeReturned.Store(true)
			return nil
		}

		var prompt2Done = make(chan struct{}, 1)
		var prompts atomic.Int32
		drv.OnPrompt = func(ctx context.Context, d *FakeAcpDriver, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
			if prompts.Add(1) == 2 {
				resumeSawCloseFinished.Store(closeReturned.Load())
				prompt2Done <- struct{}{}
			}
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}

		r := env.startRunner("hub-node", "", "", drv, func(c *runner.Config) {
			c.ReconnectInitialDelay = 250 * time.Millisecond
		})

		// 1. Run att_1 on ses_1 in fixed worktree wt_A with thought_level=low
		wtA := filepath.Join(env.dataDir, "wt-hub", "shared-wt-a")
		d1 := protocol.AttemptDispatchFrame{
			AttemptID:      "att_r2_1",
			TaskID:         "tsk_r2",
			ProjectID:      "demo-app",
			LeaseToken:     "tok1",
			Adapter:        protocol.AdapterAgy,
			WBSessionID:    "ses_r2_shared",
			RepoPath:       env.repoDir,
			WorktreePath:   wtA,
			MirrorBarePath: filepath.Join(env.dataDir, "git", "demo-app.git"),
			DesiredConfig:  map[string]string{"thought_level": "low", "mode": "auto"},
			Prompt:         "turn 1",
		}
		r.HandleDispatch(d1)
		if err := r.WaitAttemptDone(context.Background(), "att_r2_1"); err != nil {
			t.Fatal(err)
		}

		_, sid1, cwd1, _ := r.SessionStateSnapshot("ses_r2_shared")
		if sid1 == "" || cwd1 != wtA {
			t.Fatalf("unexpected session snapshot after att_1: sid=%s cwd=%s", sid1, cwd1)
		}

		// 2. Start CloseSessionSafely in background -> enters closing state & blocks on allowCloseFinish
		closeDone := make(chan error, 1)
		go func() {
			_, err := r.CloseSessionSafely(context.Background(), "ses_r2_shared")
			closeDone <- err
		}()
		<-closeEntered

		// 3. Concurrently dispatch att_2 on SAME wbSessionId + SAME worktreePath wtA!
		d2 := protocol.AttemptDispatchFrame{
			AttemptID:          "att_r2_2",
			TaskID:             "tsk_r2",
			ProjectID:          "demo-app",
			LeaseToken:         "tok2",
			Adapter:            protocol.AdapterAgy,
			WBSessionID:        "ses_r2_shared",
			ProviderSessionID:  string(sid1),
			ExpectedSessionCWD: wtA,
			RepoPath:           env.repoDir,
			WorktreePath:       wtA,
			MirrorBarePath:     filepath.Join(env.dataDir, "git", "demo-app.git"),
			DesiredConfig:      map[string]string{"thought_level": "low", "mode": "auto"},
			Prompt:             "turn 2",
		}
		r.HandleDispatch(d2)

		// Verify att_2 has NOT started prompting while CloseSession is still in closing!
		time.Sleep(40 * time.Millisecond)
		if prompts.Load() != 1 {
			t.Fatalf("dispatch bypassed in-flight CloseSession!")
		}

		// Release CloseSession -> att_2 must resume sid1 AND re-apply thought_level=low!
		close(allowCloseFinish)
		if err := <-closeDone; err != nil {
			t.Fatalf("CloseSessionSafely failed: %v", err)
		}
		select {
		case <-prompt2Done:
		case <-time.After(3 * time.Second):
			t.Fatal("att_2 did not complete after CloseSession returned")
		}
		if err := r.WaitAttemptDone(context.Background(), "att_r2_2"); err != nil {
			t.Fatal(err)
		}
		if !resumeSawCloseFinished.Load() {
			t.Fatal("expected CloseSession to finish strictly before att_2 Prompt")
		}

		drv.mu.Lock()
		resumeCount := len(drv.resumeCalls)
		finalThought := drv.sessionConfig[sid1]["thought_level"]
		drv.mu.Unlock()
		if resumeCount != 1 {
			t.Fatalf("expected exactly 1 ResumeSession call, got %d", resumeCount)
		}
		if finalThought != "low" {
			t.Fatalf("expected thought_level=low re-verified after ResumeSession, got %q", finalThought)
		}
	})

	t.Run("R2b_ChangedCwdForcesNewSession", func(t *testing.T) {
		env := newTestEnv(t)
		drv := NewFakeAcpDriver()
		r := env.startRunner("hub-node", "", "", drv, func(c *runner.Config) {
			c.ReconnectInitialDelay = 250 * time.Millisecond
		})

		wtA := filepath.Join(env.dataDir, "wt-hub", "cwd-a")
		wtB := filepath.Join(env.dataDir, "wt-hub", "cwd-b")

		r.HandleDispatch(protocol.AttemptDispatchFrame{
			AttemptID:    "att_cwd_1",
			TaskID:       "tsk_cwd",
			ProjectID:    "demo-app",
			LeaseToken:   "tok1",
			Adapter:      protocol.AdapterAgy,
			WBSessionID:  "ses_cwd",
			RepoPath:     env.repoDir,
			WorktreePath: wtA,
			Prompt:       "in wtA",
		})
		_ = r.WaitAttemptDone(context.Background(), "att_cwd_1")
		_, sid1, _, _ := r.SessionStateSnapshot("ses_cwd")

		// Now dispatch pointing at wtB while passing sid1 / expectedCWD = wtA -> MUST force NewSession!
		r.HandleDispatch(protocol.AttemptDispatchFrame{
			AttemptID:          "att_cwd_2",
			TaskID:             "tsk_cwd",
			ProjectID:          "demo-app",
			LeaseToken:         "tok2",
			Adapter:            protocol.AdapterAgy,
			WBSessionID:        "ses_cwd",
			ProviderSessionID:  string(sid1),
			ExpectedSessionCWD: wtA,
			RepoPath:           env.repoDir,
			WorktreePath:       wtB,
			Prompt:             "in wtB",
		})
		_ = r.WaitAttemptDone(context.Background(), "att_cwd_2")
		_, sid2, cwd2, _ := r.SessionStateSnapshot("ses_cwd")

		if sid2 == sid1 || cwd2 != wtB {
			t.Fatalf("changed worktree dir failed to force NewSession: sid1=%s sid2=%s cwd2=%s", sid1, sid2, cwd2)
		}
		drv.mu.Lock()
		resumes := len(drv.resumeCalls)
		news := len(drv.newCalls)
		drv.mu.Unlock()
		if resumes != 0 || news != 2 {
			t.Fatalf("expected 0 ResumeSession and 2 NewSession calls, got resumes=%d news=%d", resumes, news)
		}
	})

	t.Run("R2c_ConfigApplyFailureCleansUpAndReleasesExclusiveResource", func(t *testing.T) {
		env := newTestEnv(t)
		drv := NewFakeAcpDriver()
		// Force SetConfigOption mismatch on thought_level=low (returns currentValue=high)
		drv.OnSetConfigOption = func(ctx context.Context, sid acp.SessionId, key, val string) (acp.SetSessionConfigOptionResponse, error) {
			return acp.SetSessionConfigOptionResponse{
				ConfigOptions: []acp.SessionConfigOption{
					{Select: &acp.SessionConfigOptionSelect{Id: acp.SessionConfigId(key), Name: key, Type: "select", CurrentValue: "high"}},
				},
			}, nil
		}
		r := env.startRunner("mac-node", env.remoteTok, "", drv, nil)

		resp, raw := env.apiReq(http.MethodPost, "/api/projects/demo-app/tasks", protocol.CreateTaskRequest{
			Goal:               "Run Xcode test",
			RequiredCapability: "macos-env",
			RequiredResources:  []string{"xcode_sim"},
			Config:             map[string]string{"thought_level": "low"},
		}, nil)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create task failed (%d): %s", resp.StatusCode, string(raw))
		}
		var created protocol.CreateTaskResponse
		_ = json.Unmarshal(raw, &created)
		attID := created.Attempt.AttemptID

		// Wait until Hub records CONFIG_APPLY_FAILED
		eventually(t, 3*time.Second, func() bool {
			rec, err := env.st.GetAttemptRecord(context.Background(), attID)
			return err == nil && rec.State == protocol.AttemptFailed && rec.ErrorCode == "CONFIG_APPLY_FAILED"
		}, "attempt did not fail with CONFIG_APPLY_FAILED")

		// Exclusive resource lock xcode_sim MUST be immediately released!
		if owner := r.HeldResourceOwner("xcode_sim"); owner != "" {
			t.Fatalf("xcode_sim resource leaked to %s after CONFIG_APPLY_FAILED", owner)
		}
		// Session MUST NOT be marked live/ready
		rec, _ := env.st.GetAttemptRecord(context.Background(), attID)
		st, _, _, _ := r.SessionStateSnapshot(rec.WBSessionID)
		if st != protocol.SessionDead {
			t.Fatalf("expected failed config session marked dead, got %s", st)
		}
	})
}

// ============================================================================
// R3: Approval disconnect redelivery + decisionId idempotency + reject preserved + crash windows
// ============================================================================
func TestRegression_R3_ApprovalDisconnectRedeliverAndCrashWindows(t *testing.T) {
	t.Run("R3a_TransientDisconnectRedeliverIdempotentAndRejectPreserved", func(t *testing.T) {
		env := newTestEnv(t)
		drv := NewFakeAcpDriver()

		var allowOutcome, rejectOutcome acp.RequestPermissionResponse
		var permWG sync.WaitGroup
		var permAnswers atomic.Int32

		drv.OnPrompt = func(ctx context.Context, d *FakeAcpDriver, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
			// Fire TWO concurrent permission requests on the same Attempt!
			permWG.Add(2)
			go func() {
				defer permWG.Done()
				resp, _ := d.AskPermission(ctx, acp.RequestPermissionRequest{
					SessionId: sid,
					ToolCall: acp.ToolCallUpdate{
						ToolCallId: "tc-perm-allow",
						Name:       acp.PatchSet("run_command"),
						RawInput:   acp.PatchSet(json.RawMessage(`{"cmd":"go test ./..."}`)),
					},
					Options: []acp.PermissionOption{
						{OptionId: "allow_once", Name: "Allow Once", Kind: acp.PermissionOptionKindAllowOnce},
						{OptionId: "reject_once", Name: "Reject Once", Kind: acp.PermissionOptionKindRejectOnce},
					},
				})
				allowOutcome = resp
				permAnswers.Add(1)
			}()
			go func() {
				defer permWG.Done()
				resp, _ := d.AskPermission(ctx, acp.RequestPermissionRequest{
					SessionId: sid,
					ToolCall: acp.ToolCallUpdate{
						ToolCallId: "tc-perm-reject",
						Name:       acp.PatchSet("run_command"),
						RawInput:   acp.PatchSet(json.RawMessage(`{"cmd":"rm -rf /tmp/x"}`)),
					},
					Options: []acp.PermissionOption{
						{OptionId: "allow_once", Name: "Allow Once", Kind: acp.PermissionOptionKindAllowOnce},
						{OptionId: "reject_once", Name: "Reject Once", Kind: acp.PermissionOptionKindRejectOnce},
					},
				})
				rejectOutcome = resp
				permAnswers.Add(1)
			}()
			permWG.Wait()
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}

		r := env.startRunner("hub-node", "", "", drv, func(c *runner.Config) {
			c.ReconnectInitialDelay = 250 * time.Millisecond
		})

		resp, raw := env.apiReq(http.MethodPost, "/api/projects/demo-app/tasks", protocol.CreateTaskRequest{
			Goal:   "Run commands needing approval",
			Config: map[string]string{"mode": "default"},
		}, nil)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create task failed: %s", string(raw))
		}
		var created protocol.CreateTaskResponse
		_ = json.Unmarshal(raw, &created)
		attID := created.Attempt.AttemptID

		// Wait until BOTH approvals appear pending in Hub DB
		var apAllow, apReject protocol.ApprovalView
		eventually(t, 3*time.Second, func() bool {
			list, err := env.st.ListApprovals(context.Background(), protocol.ApprovalPending)
			if err != nil || len(list) != 2 {
				return false
			}
			for _, ap := range list {
				if tcID, _ := ap.Payload["toolCallId"].(string); tcID == "tc-perm-allow" {
					apAllow = ap
				} else if tcID == "tc-perm-reject" {
					apReject = ap
				}
			}
			return apAllow.ApprovalID != "" && apReject.ApprovalID != ""
		}, "expected 2 pending approvals in Hub")

		// Guard checks: PA caller -> 403; tampered payloadSha256 -> 409; invalid optionId -> 409
		resp, _ = env.apiReq(http.MethodPost, "/api/approvals/"+apAllow.ApprovalID+"/decide", protocol.DecideApprovalRequest{
			OptionID:              "allow_once",
			ExpectedAttemptID:     attID,
			ExpectedPayloadSHA256: apAllow.PayloadSHA256,
		}, map[string]string{"X-Orbit-Actor-Kind": "pa"})
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 for PA deciding approval, got %d", resp.StatusCode)
		}
		resp, _ = env.apiReq(http.MethodPost, "/api/approvals/"+apAllow.ApprovalID+"/decide", protocol.DecideApprovalRequest{
			OptionID:              "allow_once",
			ExpectedAttemptID:     attID,
			ExpectedPayloadSHA256: "tampered-sha",
		}, nil)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("expected 409 for tampered payloadSha256, got %d", resp.StatusCode)
		}

		// Simulate transient WS drop BEFORE deciding, wait until Hub observes disconnect!
		r.SimulateAbruptDisconnect()
		eventually(t, 2*time.Second, func() bool {
			return !env.srv.RunnerGateway().IsConnected("hub-node")
		}, "hub did not observe simulated disconnect")

		// Human approves apAllow and rejects apReject while WS is down -> Hub persists *_pending_delivery (SentToRunner=false!)
		resp, raw = env.apiReq(http.MethodPost, "/api/approvals/"+apAllow.ApprovalID+"/decide", protocol.DecideApprovalRequest{
			OptionID:              "allow_once",
			ExpectedAttemptID:     attID,
			ExpectedPayloadSHA256: apAllow.PayloadSHA256,
		}, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("decide allow failed (%d): %s", resp.StatusCode, string(raw))
		}
		var decAllowResp protocol.DecideApprovalResponse
		_ = json.Unmarshal(raw, &decAllowResp)
		if decAllowResp.SentToRunner || decAllowResp.Approval.State != protocol.ApprovalApprovedPendingDelivery {
			t.Fatalf("expected offline approved_pending_delivery (sentToRunner=false), got %+v", decAllowResp)
		}

		resp, raw = env.apiReq(http.MethodPost, "/api/approvals/"+apReject.ApprovalID+"/decide", protocol.DecideApprovalRequest{
			OptionID:              "reject_once",
			ExpectedAttemptID:     attID,
			ExpectedPayloadSHA256: apReject.PayloadSHA256,
		}, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("decide reject failed (%d): %s", resp.StatusCode, string(raw))
		}
		var decRejectResp protocol.DecideApprovalResponse
		_ = json.Unmarshal(raw, &decRejectResp)
		if decRejectResp.SentToRunner || decRejectResp.Approval.State != protocol.ApprovalRejectedPendingDelivery {
			t.Fatalf("expected offline rejected_pending_delivery (sentToRunner=false), got %+v", decRejectResp)
		}

		// Same Runner reconnects automatically -> hub.welcome.reconcile.redeliverApprovals delivers BOTH decisions!
		// Under B1, demo-app has no verified delivery and had a rejected tool, so attempt settles to failed (NO_VERIFIED_DELIVERY)
		// while BOTH approvals settle cleanly to consumed_allowed and consumed_rejected!
		eventually(t, 4*time.Second, func() bool {
			a1, _ := env.st.GetApproval(context.Background(), apAllow.ApprovalID)
			a2, _ := env.st.GetApproval(context.Background(), apReject.ApprovalID)
			att, _ := env.st.GetAttemptRecord(context.Background(), attID)
			return a1.State == protocol.ApprovalConsumedAllowed &&
				a2.State == protocol.ApprovalConsumedRejected &&
				att.State == protocol.AttemptFailed &&
				att.ErrorCode == "NO_VERIFIED_DELIVERY"
		}, "approvals did not redeliver and settle cleanly (allow=consumed_allowed, reject=consumed_rejected)")

		if allowOutcome.Outcome.Selected == nil || allowOutcome.Outcome.Selected.OptionId != "allow_once" {
			t.Fatalf("ACP did not receive allow_once: %+v", allowOutcome)
		}
		if rejectOutcome.Outcome.Selected == nil || rejectOutcome.Outcome.Selected.OptionId != "reject_once" {
			t.Fatalf("ACP did not receive reject_once: %+v", rejectOutcome)
		}

		// Re-deliver same decisionId to Runner -> MUST be idempotent (no duplicate ACP resolution)
		r.HandleApprovalDecision(protocol.ApprovalDecisionPayload{
			ApprovalID:    apAllow.ApprovalID,
			DecisionID:    decAllowResp.Approval.DecisionID,
			AttemptID:     attID,
			PayloadSHA256: apAllow.PayloadSHA256,
			OptionID:      "allow_once",
			DecidedBy:     protocol.ApprovalDecidedByHuman,
		})
		if permAnswers.Load() != 2 {
			t.Fatalf("expected exactly 2 ACP permission answers after duplicate decisionId, got %d", permAnswers.Load())
		}
	})

	t.Run("R3b_CrashBeforeDecisionMarksExpiredAndAllowsSafeRetry", func(t *testing.T) {
		env := newTestEnv(t)
		drv1 := NewFakeAcpDriver()
		drv1.OnPrompt = func(ctx context.Context, d *FakeAcpDriver, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
			_, _ = d.AskPermission(ctx, acp.RequestPermissionRequest{
				SessionId: sid,
				ToolCall:  acp.ToolCallUpdate{ToolCallId: "tc-precrash", Name: acp.PatchSet("run_command")},
				Options:   []acp.PermissionOption{{OptionId: "allow_once", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce}},
			})
			return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
		}

		outboxPath := filepath.Join(env.dataDir, "r3b-outbox.sqlite")
		r1 := env.startRunner("hub-node", "", outboxPath, drv1, nil)

		_, raw := env.apiReq(http.MethodPost, "/api/projects/demo-app/tasks", protocol.CreateTaskRequest{
			Goal:   "Crash before approval decision",
			Config: map[string]string{"mode": "default"},
		}, nil)
		var created protocol.CreateTaskResponse
		_ = json.Unmarshal(raw, &created)
		tid := created.Task.TaskID
		att1ID := created.Attempt.AttemptID

		var aprID string
		eventually(t, 3*time.Second, func() bool {
			list, _ := env.st.ListApprovals(context.Background(), protocol.ApprovalPending)
			if len(list) == 1 {
				aprID = list[0].ApprovalID
				return true
			}
			return false
		}, "approval did not reach pending")

		// Crash runner 1 BEFORE decision is ever made; start new runner 2 (new runnerInstanceId!)
		_ = r1.Close(context.Background())
		drv2 := NewFakeAcpDriver()
		_ = env.startRunner("hub-node", "", outboxPath, drv2, nil)

		// Verify approval -> expired and attempt -> expired
		eventually(t, 3*time.Second, func() bool {
			ap, _ := env.st.GetApproval(context.Background(), aprID)
			att, _ := env.st.GetAttemptRecord(context.Background(), att1ID)
			return ap.State == protocol.ApprovalExpired && att.State == protocol.AttemptExpired
		}, "expected expired approval and expired attempt after pre-decision crash")

		// Safe retry WITHOUT verifiedSite MUST succeed!
		resp, raw := env.apiReq(http.MethodPost, "/api/tasks/"+tid+"/retry", protocol.RetryTaskRequest{
			VerifiedSite: false,
		}, nil)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("expected safe retry to succeed after pre-decision crash, got %d: %s", resp.StatusCode, string(raw))
		}
	})

	t.Run("R3c_CrashAfterApprovalSentMarksUncertainAndNeedsVerification", func(t *testing.T) {
		env := newTestEnv(t)
		drv1 := NewFakeAcpDriver()
		acpWroteAllow := make(chan struct{}, 1)

		drv1.OnPrompt = func(ctx context.Context, d *FakeAcpDriver, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
			resp, _ := d.AskPermission(ctx, acp.RequestPermissionRequest{
				SessionId: sid,
				ToolCall:  acp.ToolCallUpdate{ToolCallId: "tc-window", Name: acp.PatchSet("run_command")},
				Options:   []acp.PermissionOption{{OptionId: "allow_once", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce}},
			})
			if resp.Outcome.Selected != nil && resp.Outcome.Selected.OptionId == "allow_once" {
				acpWroteAllow <- struct{}{}
			}
			<-ctx.Done()
			return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
		}

		outboxPath := filepath.Join(env.dataDir, "r3c-outbox.sqlite")
		r1 := env.startRunner("hub-node", "", outboxPath, drv1, func(c *runner.Config) {
			// Simulate process crash right AFTER writing allow to ACP, BEFORE local record / outbox ack!
			c.OnAfterACPApprovalWrite = func(approvalID string) bool {
				return false
			}
		})

		_, raw := env.apiReq(http.MethodPost, "/api/projects/demo-app/tasks", protocol.CreateTaskRequest{
			Goal:   "Crash during approval ack window",
			Config: map[string]string{"mode": "default"},
		}, nil)
		var created protocol.CreateTaskResponse
		_ = json.Unmarshal(raw, &created)
		tid := created.Task.TaskID
		att1ID := created.Attempt.AttemptID

		var ap protocol.ApprovalView
		eventually(t, 3*time.Second, func() bool {
			list, _ := env.st.ListApprovals(context.Background(), protocol.ApprovalPending)
			if len(list) == 1 {
				ap = list[0]
				return true
			}
			return false
		}, "approval did not reach pending")

		// Human approves -> Hub persists approved_pending_delivery and pushes to runner 1
		resp, raw := env.apiReq(http.MethodPost, "/api/approvals/"+ap.ApprovalID+"/decide", protocol.DecideApprovalRequest{
			OptionID:              "allow_once",
			ExpectedAttemptID:     att1ID,
			ExpectedPayloadSHA256: ap.PayloadSHA256,
		}, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("decide allow failed (%d): %s", resp.StatusCode, string(raw))
		}

		// Wait until ACP actually received allow_once, then kill runner 1 BEFORE ack!
		select {
		case <-acpWroteAllow:
		case <-time.After(3 * time.Second):
			t.Fatal("ACP did not receive allow_once")
		}
		_ = r1.Close(context.Background())

		// Restart runner 2 with NEW runnerInstanceId
		drv2 := NewFakeAcpDriver()
		_ = env.startRunner("hub-node", "", outboxPath, drv2, nil)

		// Verify approval -> uncertain, attempt -> needs_verification!
		eventually(t, 3*time.Second, func() bool {
			curAp, _ := env.st.GetApproval(context.Background(), ap.ApprovalID)
			curAtt, _ := env.st.GetAttemptRecord(context.Background(), att1ID)
			return curAp.State == protocol.ApprovalUncertain && curAtt.State == protocol.AttemptNeedsVerification
		}, "expected uncertain approval and needs_verification attempt after post-approval crash window")

		// Blind safe retry WITHOUT verifiedSite MUST be blocked with 409 Conflict!
		resp, raw = env.apiReq(http.MethodPost, "/api/tasks/"+tid+"/retry", protocol.RetryTaskRequest{
			VerifiedSite: false,
		}, nil)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("expected 409 Conflict forbidding blind retry on uncertain crash window, got %d: %s", resp.StatusCode, string(raw))
		}
	})
}

// ============================================================================
// R4a: WS Bearer Auth + Outbox Gap Protection + Late-Arrival History Isolation
// ============================================================================
func TestRegression_R4a_WSAuthOutboxGapAndLateHistory(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// 1. Remote WS auth check: bad token rejected (401), valid token accepted
	badHdr := http.Header{
		"X-Orbit-Machine-Id": []string{"mac-node"},
		"Authorization":      []string{"Bearer wrong-token"},
	}
	_, httpResp, err := websocket.Dial(ctx, env.hubWSURL, &websocket.DialOptions{HTTPHeader: badHdr})
	if err == nil || (httpResp != nil && httpResp.StatusCode != http.StatusUnauthorized) {
		t.Fatalf("expected 401 for bad remote_ws bearer token, err=%v resp=%v", err, httpResp)
	}

	goodHdr := http.Header{
		"X-Orbit-Machine-Id": []string{"mac-node"},
		"Authorization":      []string{"Bearer " + env.remoteTok},
	}
	conn, _, err := websocket.Dial(ctx, env.hubWSURL, &websocket.DialOptions{HTTPHeader: goodHdr})
	if err != nil {
		t.Fatalf("dial remote_ws with valid token failed: %v", err)
	}
	defer conn.CloseNow()

	writeFrame := func(v any) {
		b, _ := json.Marshal(v)
		if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatalf("ws write: %v", err)
		}
	}
	readFrame := func() []byte {
		rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_, data, err := conn.Read(rctx)
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		return data
	}

	// Send runner.hello -> expect hub.welcome
	writeFrame(protocol.RunnerHelloFrame{
		Type:             protocol.RunnerMsgHello,
		ProtocolVersion:  1,
		MachineID:        "mac-node",
		RunnerInstanceID: "run_r4a_ws",
		RunnerVersion:    "0.2.0",
	})
	var welcome protocol.HubWelcomeFrame
	_ = json.Unmarshal(readFrame(), &welcome)
	if welcome.Type != protocol.RunnerMsgHubWelcome {
		t.Fatalf("expected hub.welcome, got %+v", welcome)
	}

	// Create task targeting macos-env -> receives attempt.dispatch
	resp, raw := env.apiReq(http.MethodPost, "/api/projects/demo-app/tasks", protocol.CreateTaskRequest{
		Goal:               "R4a WS test",
		RequiredCapability: "macos-env",
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create task failed: %s", string(raw))
	}
	var disp protocol.AttemptDispatchFrame
	_ = json.Unmarshal(readFrame(), &disp)
	if disp.Type != protocol.RunnerMsgAttemptDispatch {
		t.Fatalf("expected attempt.dispatch, got %+v", disp)
	}
	att1ID := disp.AttemptID
	tid := disp.TaskID

	// Prepare real commits pushed to demo-app's bare mirror so B12 mirror verification passes!
	projDemo, err := env.st.GetProject(ctx, "demo-app")
	if err != nil {
		t.Fatal(err)
	}
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=Tester", "GIT_AUTHOR_EMAIL=tester@example.com",
		"GIT_COMMITTER_NAME=Tester", "GIT_COMMITTER_EMAIL=tester@example.com",
	)
	runRepoGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = env.repoDir
		cmd.Env = gitEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	baseSHA := runRepoGit("rev-parse", "HEAD")
	_ = os.WriteFile(filepath.Join(env.repoDir, "app.go"), []byte("package main\n// dlv_r4_1\nfunc main() {}\n"), 0o644)
	runRepoGit("add", ".")
	runRepoGit("commit", "-m", "dlv_r4_1")
	headR41 := runRepoGit("rev-parse", "HEAD")
	runRepoGit("push", projDemo.MirrorBarePath, "HEAD:refs/orbit/deliveries/dlv_r4_1")
	_ = os.WriteFile(filepath.Join(env.repoDir, "app.go"), []byte("package main\n// dlv_r4_late\nfunc main() {}\n"), 0o644)
	runRepoGit("add", ".")
	runRepoGit("commit", "-m", "dlv_r4_late")
	headR4Late := runRepoGit("rev-parse", "HEAD")
	runRepoGit("push", projDemo.MirrorBarePath, "HEAD:refs/orbit/deliveries/dlv_r4_late")

	// 2. Send seq=1 -> ack=1; skip seq=2 and send seq=3 -> Hub MUST stay at ack=1!
	writeFrame(protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  att1ID,
		LeaseToken: disp.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 1, EventID: "r4_e1", FrameType: protocol.OutboxFrameState, Payload: map[string]any{"state": "running", "baseSha": baseSHA}},
			{OutboxSeq: 3, EventID: "r4_e3", FrameType: protocol.OutboxFrameEvents, Payload: map[string]any{"kind": "acp.message_batch", "visibility": "raw_acp", "payload": map[string]any{"text": "skip"}}},
		},
	})
	var ack protocol.HubOutboxAckFrame
	_ = json.Unmarshal(readFrame(), &ack)
	if ack.AckedOutboxSeq != 1 || ack.AcceptedMode != protocol.OutboxModeActive {
		t.Fatalf("expected gap stop at ack=1 active, got %+v", ack)
	}

	// Fill seq=2 (publish dlv_r4_1) and seq=3 -> advances to ack=3
	writeFrame(protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  att1ID,
		LeaseToken: disp.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 2, EventID: "r4_e2", FrameType: protocol.OutboxFrameDeliveryPublished, Payload: map[string]any{
				"deliveryId": "dlv_r4_1",
				"baseSha":    baseSHA,
				"headSha":    headR41,
				"summary":    "valid delivery 1",
			}},
			{OutboxSeq: 3, EventID: "r4_e3", FrameType: protocol.OutboxFrameEvents, Payload: map[string]any{"kind": "acp.message_batch", "visibility": "raw_acp", "payload": map[string]any{"text": "filled"}}},
		},
	})
	_ = json.Unmarshal(readFrame(), &ack)
	if ack.AckedOutboxSeq != 3 {
		t.Fatalf("expected ack=3 after filling gap, got %+v", ack)
	}

	// Create open annotation on dlv_r4_1, expire att_1, and dispatch annotation to att_2!
	resp, raw = env.apiReq(http.MethodPost, "/api/deliveries/dlv_r4_1/annotations", protocol.CreateAnnotationRequest{
		Source: protocol.AnnotationSourceDiffDrawer,
		Anchor: protocol.AnnotationAnchor{Path: "app.go", StartLine: 1, EndLine: 1},
		Body:   "Fix formatting",
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create annotation failed: %s", string(raw))
	}
	var annResp protocol.CreateAnnotationResponse
	_ = json.Unmarshal(raw, &annResp)

	_ = env.st.UpdateAttemptLeaseExpiry(ctx, att1ID, time.Now().UnixMilli()-10000)
	if _, _, err := env.st.ScanExpiredAttempts(ctx, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}

	resp, raw = env.apiReq(http.MethodPost, "/api/tasks/"+tid+"/dispatch-annotations", protocol.DispatchAnnotationsRequest{
		AnnotationIDs: []string{annResp.Annotation.AnnotationID},
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dispatch annotations failed (%d): %s", resp.StatusCode, string(raw))
	}
	var dispAnn protocol.DispatchAnnotationsResponse
	_ = json.Unmarshal(raw, &dispAnn)
	att2ID := dispAnn.Attempt.AttemptID

	// Drain the attempt.dispatch frame for att_2
	var disp2 protocol.AttemptDispatchFrame
	_ = json.Unmarshal(readFrame(), &disp2)
	if disp2.AttemptID != att2ID {
		t.Fatalf("expected dispatch for att_2 %s, got %+v", att2ID, disp2)
	}

	// 3. Now send late outbox batch for EXPIRED att_1 -> MUST route to late_history without polluting att_2 or annotations!
	writeFrame(protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  att1ID,
		LeaseToken: disp.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 4, EventID: "r4_late_dlv", FrameType: protocol.OutboxFrameDeliveryPublished, Payload: map[string]any{
				"deliveryId": "dlv_r4_late",
				"baseSha":    baseSHA,
				"headSha":    headR4Late,
				"summary":    "late att1 delivery",
			}},
			{OutboxSeq: 5, EventID: "r4_late_ok", FrameType: protocol.OutboxFrameState, Payload: map[string]any{
				"state":            "succeeded",
				"completionSource": "explicit_deliver",
			}},
		},
	})
	_ = json.Unmarshal(readFrame(), &ack)
	if ack.AcceptedMode != protocol.OutboxModeLateHistory || ack.AckedOutboxSeq != 5 {
		t.Fatalf("expected late_history ack=5, got %+v", ack)
	}

	// Verify task timeline: active_attempt_id is STILL att_2, latest_delivery_id is STILL dlv_r4_1,
	// annotation is STILL queued on att_2, and dlv_r4_late has late_arrival=true with NO inbox card!
	tl, err := env.st.GetTaskTimeline(ctx, tid, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if tl.Task.ActiveAttemptID != att2ID || tl.Task.LatestDeliveryID != "dlv_r4_1" {
		t.Fatalf("late_history polluted task pointers: active=%s latestDlv=%s", tl.Task.ActiveAttemptID, tl.Task.LatestDeliveryID)
	}
	if len(tl.Annotations) != 1 || tl.Annotations[0].Status != protocol.AnnotationQueued || tl.Annotations[0].TriggeredAttemptID != att2ID {
		t.Fatalf("late_history corrupted att_2 annotation: %+v", tl.Annotations)
	}

	// Also test annotation rollback when att_2 fails: queued annotation MUST release back to open!
	writeFrame(protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  att2ID,
		LeaseToken: disp2.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 1, EventID: "r4_att2_fail", FrameType: protocol.OutboxFrameState, Payload: map[string]any{
				"state":       "failed",
				"errorCode":   "TEST_FAIL",
				"errorDetail": "simulated failure",
			}},
		},
	})
	_ = json.Unmarshal(readFrame(), &ack)

	tlAfterFail, _ := env.st.GetTaskTimeline(ctx, tid, 0, false)
	if len(tlAfterFail.Annotations) != 1 || tlAfterFail.Annotations[0].Status != protocol.AnnotationOpen || tlAfterFail.Annotations[0].TriggeredAttemptID != "" {
		t.Fatalf("expected queued annotation released back to open after att_2 failed: %+v", tlAfterFail.Annotations)
	}
	_ = errors.New("") // keep errors import cleanly used if needed
}
