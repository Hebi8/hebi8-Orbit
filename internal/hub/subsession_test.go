package hub_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Hebi8/hebi8-Orbit/internal/hub"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

func dialTestRunnerWS(t *testing.T, ctx context.Context, wsURL, machineID, instanceID, token string) *websocket.Conn {
	t.Helper()
	hdr := http.Header{}
	hdr.Set("X-Orbit-Machine-Id", machineID)
	hdr.Set("Authorization", "Bearer "+token)
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("dial runner ws: %v", err)
	}
	sendRunnerJSON(t, ctx, conn, protocol.RunnerHelloFrame{
		Type:             protocol.RunnerMsgHello,
		ProtocolVersion:  1,
		MachineID:        machineID,
		RunnerInstanceID: instanceID,
		RunnerVersion:    "0.3.2-test",
		Adapters:         []protocol.Adapter{protocol.AdapterAgy},
	})
	// Read hub.welcome
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, _, err := conn.Read(readCtx); err != nil {
		t.Fatalf("read hub.welcome: %v", err)
	}
	// Drain inbound frames in background so server write pump never blocks, capturing dispatches
	go func() {
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()
	return conn
}

func dialTestRunnerWSCaptureDispatches(t *testing.T, ctx context.Context, wsURL, machineID, instanceID, token string) (*websocket.Conn, <-chan protocol.AttemptDispatchFrame) {
	t.Helper()
	hdr := http.Header{}
	hdr.Set("X-Orbit-Machine-Id", machineID)
	hdr.Set("Authorization", "Bearer "+token)
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("dial runner ws: %v", err)
	}
	sendRunnerJSON(t, ctx, conn, protocol.RunnerHelloFrame{
		Type:             protocol.RunnerMsgHello,
		ProtocolVersion:  1,
		MachineID:        machineID,
		RunnerInstanceID: instanceID,
		RunnerVersion:    "0.3.2-test",
		Adapters:         []protocol.Adapter{protocol.AdapterAgy},
	})
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, _, err := conn.Read(readCtx); err != nil {
		t.Fatalf("read hub.welcome: %v", err)
	}
	dispatches := make(chan protocol.AttemptDispatchFrame, 32)
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var hdr protocol.RunnerFrameHeader
			if json.Unmarshal(data, &hdr) == nil && hdr.Type == protocol.RunnerMsgAttemptDispatch {
				var df protocol.AttemptDispatchFrame
				if json.Unmarshal(data, &df) == nil {
					dispatches <- df
				}
			}
		}
	}()
	return conn, dispatches
}

func sendRunnerJSON(t *testing.T, ctx context.Context, conn *websocket.Conn, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := conn.Write(wctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write runner frame: %v", err)
	}
}

func setupSubsessionHub(t *testing.T) (*hub.Server, *store.Store, *httptest.Server, string) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "config")
	dataDir := filepath.Join(tmp, "data")
	repoPath := filepath.Join(tmp, "repo")
	_ = os.MkdirAll(cfgDir, 0o755)
	_ = os.MkdirAll(repoPath, 0o755)

	runnerToken := "runner-secret-subsession"
	sum := sha256.Sum256([]byte(runnerToken))
	tokenHash := "sha256:" + hex.EncodeToString(sum[:])

	machinesYAML := `schemaVersion: 1
machines:
  hub-node:
    displayName: "hub-node"
    transport: local_ws
    tokenHash: "` + tokenHash + `"
    capabilities: [primary-dev]
  deploy-vps:
    displayName: "deploy-vps"
    transport: ssh_outbound
    sshAlias: "deploy-vps"
    capabilities: [deploy-node]
`
	projectsYAML := `schemaVersion: 1
projects:
  demo-app:
    displayName: "demo-app"
    repoIdentity: "github.com/example-org/demo-app"
    defaultBranch: "main"
    defaultCapability: "primary-dev"
    defaultAdapter: "agy"
    ideScheme: "cursor"
    anchors:
      primary-dev:
        machine: "hub-node"
        repoPath: "` + filepath.ToSlash(repoPath) + `"
      deploy-node:
        machine: "deploy-vps"
        remoteDir: "/srv/demo-app"
        deployStrategy:
          kind: "ssh_push_and_switch"
          targetEnv: "deploy-vps:prod"
          requireHumanApproval: true
`
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
		HubConfig:   cfg,
		Store:       st,
		RyugiRunner: &fakeRyugiRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, st, ts, runnerToken
}

func TestSubsession_HubAPIAndRunnerDelegateScopeGuards(t *testing.T) {
	ctx := context.Background()
	_, st, ts, runnerToken := setupSubsessionHub(t)

	// 1. Ensure Hebi8 main task and verify _pa CANNOT attach child sessions (409 ERR_PA_NO_SUBSESSIONS)
	hebi8Task, _, err := st.EnsureHebi8MainTask(ctx, protocol.AdapterAgy)
	if err != nil {
		t.Fatal(err)
	}

	resp, body := doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+hebi8Task.TaskID+"/children", protocol.CreateChildTaskRequest{
		Title: "Hebi8 Helper",
		Role:  "researcher",
		Goal:  "Check architecture options",
	}, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 rejecting child under _pa, got %d: %s", resp.StatusCode, string(body))
	}
	var paErr protocol.APIError
	_ = json.Unmarshal(body, &paErr)
	if paErr.Code != "ERR_PA_NO_SUBSESSIONS" {
		t.Fatalf("expected ERR_PA_NO_SUBSESSIONS, got %+v", paErr)
	}

	// Create a normal top-level root task to spawn children under
	rootA, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		Title:         "Root A",
		Goal:          "Main project work",
		MachineID:     "hub-node",
		CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+rootA.Task.TaskID+"/children", protocol.CreateChildTaskRequest{
		Title:     "Root A Helper",
		Role:      "researcher",
		ProjectID: protocol.ScratchProjectID,
		Goal:      "Check architecture options",
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("spawn child under rootA failed (%d): %s", resp.StatusCode, string(body))
	}
	var spawned protocol.CreateTaskResponse
	_ = json.Unmarshal(body, &spawned)
	if spawned.Task.ProjectID != protocol.ScratchProjectID || spawned.Task.ParentTaskID != rootA.Task.TaskID || spawned.Task.DelegatedRole != "researcher" {
		t.Fatalf("unexpected spawned child under rootA: %+v", spawned.Task)
	}

	// 2. Reject ssh_deploy via client API (both explicit adapter and deploy-node capability)
	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+rootA.Task.TaskID+"/children", protocol.CreateChildTaskRequest{
		ProjectID: "demo-app",
		Adapter:   protocol.AdapterSSHDeploy,
		Goal:      "Deploy prod",
	}, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for ssh_deploy child adapter, got %d: %s", resp.StatusCode, string(body))
	}
	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+rootA.Task.TaskID+"/children", protocol.CreateChildTaskRequest{
		ProjectID:          "demo-app",
		RequiredCapability: "deploy-node",
		Goal:               "Deploy prod via cap",
	}, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for deploy-node child route, got %d: %s", resp.StatusCode, string(body))
	}

	// 3. List children & transcript endpoints
	resp, body = doJSONReq(t, http.MethodGet, ts.URL+"/api/tasks/"+rootA.Task.TaskID+"/children", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list children failed (%d): %s", resp.StatusCode, string(body))
	}
	var listResp protocol.ListChildrenResponse
	_ = json.Unmarshal(body, &listResp)
	if len(listResp.Children) != 1 || listResp.Children[0].TaskID != spawned.Task.TaskID {
		t.Fatalf("unexpected children list: %+v", listResp)
	}

	resp, body = doJSONReq(t, http.MethodGet, ts.URL+"/api/tasks/"+spawned.Task.TaskID+"/transcript?limit=5", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("transcript failed (%d): %s", resp.StatusCode, string(body))
	}

	// 4. Runner delegate API: create unrelated task to test cross-session boundary enforcement
	otherRes, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		Title:         "Other Root",
		Goal:          "Unrelated task",
		MachineID:     "hub-node",
		CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	runnerHeaders := map[string]string{
		"X-Orbit-Machine-Id": "hub-node",
		"Authorization":      "Bearer " + runnerToken,
	}

	// Runner spawn with mismatched parentAttemptId -> 403
	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/runner/delegate/spawn", map[string]any{
		"parentTaskId":    rootA.Task.TaskID,
		"parentAttemptId": otherRes.Attempt.AttemptID,
		"goal":            "Spoofed attempt",
	}, runnerHeaders)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for mismatched parentAttemptId, got %d: %s", resp.StatusCode, string(body))
	}

	// Runner spawn valid child under otherRes
	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/runner/delegate/spawn", map[string]any{
		"parentTaskId":    otherRes.Task.TaskID,
		"parentAttemptId": otherRes.Attempt.AttemptID,
		"title":           "Backend Subtask",
		"role":            "backend",
		"goal":            "Implement feature X",
	}, runnerHeaders)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("runner delegate spawn failed (%d): %s", resp.StatusCode, string(body))
	}
	var runnerChild protocol.CreateTaskResponse
	_ = json.Unmarshal(body, &runnerChild)
	if runnerChild.Task.ProjectID != "demo-app" || runnerChild.Task.DelegatedByAttemptID != otherRes.Attempt.AttemptID {
		t.Fatalf("unexpected runner child: %+v", runnerChild.Task)
	}

	// Runner delegate list with includeTargets=true (requires live parentAttemptId)
	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/runner/delegate/list", map[string]any{
		"parentTaskId":    otherRes.Task.TaskID,
		"parentAttemptId": otherRes.Attempt.AttemptID,
		"includeTargets":  true,
	}, runnerHeaders)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("runner delegate list failed (%d): %s", resp.StatusCode, string(body))
	}
	var delList struct {
		Children []protocol.ChildTaskView `json:"children"`
		Machines []map[string]any         `json:"machines"`
		Projects []map[string]any         `json:"projects"`
	}
	_ = json.Unmarshal(body, &delList)
	if len(delList.Children) != 1 || len(delList.Machines) == 0 || len(delList.Projects) == 0 {
		t.Fatalf("unexpected runner delegate list response: %+v", delList)
	}

	// Cross-session access: otherRes trying to read/send/cancel Hebi8's child MUST return 403!
	for _, ep := range []struct {
		path string
		body map[string]any
	}{
		{"/api/runner/delegate/read", map[string]any{"parentTaskId": otherRes.Task.TaskID, "parentAttemptId": otherRes.Attempt.AttemptID, "childTaskId": spawned.Task.TaskID}},
		{"/api/runner/delegate/send", map[string]any{"parentTaskId": otherRes.Task.TaskID, "parentAttemptId": otherRes.Attempt.AttemptID, "childTaskId": spawned.Task.TaskID, "message": "hi"}},
		{"/api/runner/delegate/cancel", map[string]any{"parentTaskId": otherRes.Task.TaskID, "parentAttemptId": otherRes.Attempt.AttemptID, "childTaskId": spawned.Task.TaskID}},
	} {
		resp, body = doJSONReq(t, http.MethodPost, ts.URL+ep.path, ep.body, runnerHeaders)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 for cross-session %s, got %d: %s", ep.path, resp.StatusCode, string(body))
		}
	}

	// Valid descendant read, send (queue), and cancel (all with live parentAttemptId)
	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/runner/delegate/read", map[string]any{
		"parentTaskId":    otherRes.Task.TaskID,
		"parentAttemptId": otherRes.Attempt.AttemptID,
		"childTaskId":     runnerChild.Task.TaskID,
		"limit":           5,
	}, runnerHeaders)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delegate read failed (%d): %s", resp.StatusCode, string(body))
	}

	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/runner/delegate/send", map[string]any{
		"parentTaskId":    otherRes.Task.TaskID,
		"parentAttemptId": otherRes.Attempt.AttemptID,
		"childTaskId":     runnerChild.Task.TaskID,
		"message":         "Also add tests",
		"mode":            "queue",
	}, runnerHeaders)
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusCreated {
		t.Fatalf("delegate send failed (%d): %s", resp.StatusCode, string(body))
	}

	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/runner/delegate/cancel", map[string]any{
		"parentTaskId":    otherRes.Task.TaskID,
		"parentAttemptId": otherRes.Attempt.AttemptID,
		"childTaskId":     runnerChild.Task.TaskID,
		"reason":          "no longer needed",
	}, runnerHeaders)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delegate cancel failed (%d): %s", resp.StatusCode, string(body))
	}

	// P1 #1: All 5 /api/runner/delegate/* endpoints MUST return 403 when parentAttemptId is missing or terminal
	allFiveMissingAttempt := []struct {
		path string
		body map[string]any
	}{
		{"/api/runner/delegate/spawn", map[string]any{"parentTaskId": otherRes.Task.TaskID, "goal": "no att"}},
		{"/api/runner/delegate/list", map[string]any{"parentTaskId": otherRes.Task.TaskID}},
		{"/api/runner/delegate/send", map[string]any{"parentTaskId": otherRes.Task.TaskID, "childTaskId": runnerChild.Task.TaskID, "message": "hi"}},
		{"/api/runner/delegate/read", map[string]any{"parentTaskId": otherRes.Task.TaskID, "childTaskId": runnerChild.Task.TaskID}},
		{"/api/runner/delegate/cancel", map[string]any{"parentTaskId": otherRes.Task.TaskID, "childTaskId": runnerChild.Task.TaskID}},
	}
	for _, ep := range allFiveMissingAttempt {
		resp, body = doJSONReq(t, http.MethodPost, ts.URL+ep.path, ep.body, runnerHeaders)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 for missing parentAttemptId on %s, got %d: %s", ep.path, resp.StatusCode, string(body))
		}
	}

	// Finish otherRes's attempt -> ALL 5 endpoints MUST now return 403 even with that historical parentAttemptId!
	nowMs := time.Now().UnixMilli()
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE attempts SET state = 'succeeded', finished_at = ? WHERE attempt_id = ?`, nowMs, otherRes.Attempt.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE tasks SET state = 'in_review', active_attempt_id = NULL WHERE task_id = ?`, otherRes.Task.TaskID); err != nil {
		t.Fatal(err)
	}
	for _, ep := range allFiveMissingAttempt {
		ep.body["parentAttemptId"] = otherRes.Attempt.AttemptID
		resp, body = doJSONReq(t, http.MethodPost, ts.URL+ep.path, ep.body, runnerHeaders)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 for terminal parentAttemptId on %s, got %d: %s", ep.path, resp.StatusCode, string(body))
		}
	}

	// P2 #3: Generic POST /api/projects/{id}/tasks with parentTaskId MUST also forbid ssh_deploy & deploy-node!
	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/projects/demo-app/tasks", protocol.CreateTaskRequest{
		ParentTaskID: hebi8Task.TaskID,
		Adapter:      protocol.AdapterSSHDeploy,
		Goal:         "Deploy via generic endpoint",
	}, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for generic task create with parentTaskId+ssh_deploy, got %d: %s", resp.StatusCode, string(body))
	}
	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/projects/demo-app/tasks", protocol.CreateTaskRequest{
		ParentTaskID:       hebi8Task.TaskID,
		RequiredCapability: "deploy-node",
		Goal:               "Deploy via generic endpoint cap",
	}, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for generic task create with parentTaskId+deploy-node, got %d: %s", resp.StatusCode, string(body))
	}
}

func TestSubsession_AutoWakeupParentOnChildFinishAndGuards(t *testing.T) {
	ctx := context.Background()
	_, st, ts, runnerToken := setupSubsessionHub(t)

	// Connect a fake runner WebSocket so IngestOutboxBatch runs through the real Hub WS path
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/runner?machineId=hub-node"
	conn, dispatches := dialTestRunnerWSCaptureDispatches(t, ctx, wsURL, "hub-node", "run-wake-1", runnerToken)
	defer conn.CloseNow()

	// Create idle parent task (done state, no active attempt)
	parentRes, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     protocol.ScratchProjectID,
		Title:         "Coordinator Parent",
		Goal:          "Coordinate children",
		MachineID:     "hub-node",
		CreateAttempt: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Spawn child 1 (will succeed) and child 2 (will fail)
	resp, body := doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+parentRes.Task.TaskID+"/children", protocol.CreateChildTaskRequest{
		Title: "Child Success",
		Role:  "analyzer",
		Goal:  "Analyze logs",
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("spawn child 1 failed (%d): %s", resp.StatusCode, string(body))
	}
	var c1 protocol.CreateTaskResponse
	_ = json.Unmarshal(body, &c1)

	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+parentRes.Task.TaskID+"/children", protocol.CreateChildTaskRequest{
		Title: "Child Failure",
		Role:  "tester",
		Goal:  "Run flaky test",
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("spawn child 2 failed (%d): %s", resp.StatusCode, string(body))
	}
	var c2 protocol.CreateTaskResponse
	_ = json.Unmarshal(body, &c2)

	// Load lease tokens for c1 and c2
	att1, err := st.GetAttemptRecord(ctx, c1.Attempt.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	att2, err := st.GetAttemptRecord(ctx, c2.Attempt.AttemptID)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UnixMilli()
	// Send outbox batch finishing child 1 with assistant reply
	sendRunnerJSON(t, ctx, conn, protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  att1.AttemptID,
		LeaseToken: att1.LeaseToken,
		Items: []protocol.OutboxItem{
			{
				OutboxSeq: 1,
				EventID:   "evt_c1_msg",
				FrameType: protocol.OutboxFrameEvents,
				Payload: map[string]any{
					"kind":       string(protocol.EventACPMessageBatch),
					"visibility": string(protocol.VisibilityRawACP),
					"actorKind":  string(protocol.ActorAgent),
					"actorId":    "agy",
					"payload":    map[string]any{"text": "Found root cause in scheduler.go."},
				},
				TS: now,
			},
			{
				OutboxSeq: 2,
				EventID:   "evt_c1_fin",
				FrameType: protocol.OutboxFrameState,
				Payload: map[string]any{
					"state":            string(protocol.AttemptSucceeded),
					"stopReason":       "end_turn",
					"completionSource": string(protocol.CompletionPromptEndTurn),
				},
				TS: now + 1,
			},
		},
	})

	// Wait for parent task to be woken with TriggerChildResult attempt
	var parentTL protocol.TaskTimelineResponse
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		parentTL, err = st.GetTaskTimeline(ctx, parentRes.Task.TaskID, 0, false)
		if err == nil && len(parentTL.Attempts) >= 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(parentTL.Attempts) != 1 {
		t.Fatalf("expected parent to be woken with 1 child_result attempt, got %d", len(parentTL.Attempts))
	}
	wakeAtt := parentTL.Attempts[0]
	if wakeAtt.TriggerKind != protocol.TriggerChildResult {
		t.Fatalf("expected triggerKind=child_result, got %s", wakeAtt.TriggerKind)
	}
	if wakeAtt.ChildResult == nil || wakeAtt.ChildResult.ChildTaskID != c1.Task.TaskID ||
		wakeAtt.ChildResult.ChildState != protocol.AttemptSucceeded ||
		!strings.Contains(wakeAtt.ChildResult.Summary, "Found root cause") {
		t.Fatalf("unexpected ChildResult meta: %+v", wakeAtt.ChildResult)
	}

	// Verify EventTaskChildUpdated was emitted on parent timeline
	sawChildUpdated := false
	for _, ev := range parentTL.Events {
		if ev.Kind == protocol.EventTaskChildUpdated && ev.TaskID == parentRes.Task.TaskID {
			sawChildUpdated = true
			break
		}
	}
	if !sawChildUpdated {
		t.Fatalf("expected EventTaskChildUpdated on parent timeline events")
	}

	// Now parent is BUSY (active attempt wakeAtt). Finish child 2 as FAILED -> must queue into parent pending_inputs!
	sendRunnerJSON(t, ctx, conn, protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  att2.AttemptID,
		LeaseToken: att2.LeaseToken,
		Items: []protocol.OutboxItem{
			{
				OutboxSeq: 1,
				EventID:   "evt_c2_fail",
				FrameType: protocol.OutboxFrameState,
				Payload: map[string]any{
					"state":       string(protocol.AttemptFailed),
					"errorCode":   "TEST_FAILED",
					"errorDetail": "unit test timed out",
				},
				TS: now + 10,
			},
		},
	})

	var queued []protocol.PendingInput
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		queued, _ = st.ListPendingInputs(ctx, parentRes.Task.TaskID)
		if len(queued) == 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(queued) != 1 {
		t.Fatalf("expected failed child 2 wakeup to queue while parent busy, got %d pending inputs", len(queued))
	}

	// Finish parent's wakeAtt -> Hub auto-drains pending queue and creates Attempt #2 with TriggerChildResult & ChildState=failed!
	wakeRec, err := st.GetAttemptRecord(ctx, wakeAtt.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	sendRunnerJSON(t, ctx, conn, protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  wakeRec.AttemptID,
		LeaseToken: wakeRec.LeaseToken,
		Items: []protocol.OutboxItem{
			{
				OutboxSeq: 1,
				EventID:   "evt_parent_fin_1",
				FrameType: protocol.OutboxFrameState,
				Payload: map[string]any{
					"state":            string(protocol.AttemptSucceeded),
					"stopReason":       "end_turn",
					"completionSource": string(protocol.CompletionPromptEndTurn),
				},
				TS: now + 20,
			},
		},
	})

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		parentTL, _ = st.GetTaskTimeline(ctx, parentRes.Task.TaskID, 0, false)
		if len(parentTL.Attempts) >= 2 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(parentTL.Attempts) != 2 {
		t.Fatalf("expected parent attempt #2 from queued child_result, got %d attempts", len(parentTL.Attempts))
	}
	wakeAtt2 := parentTL.Attempts[1]
	if wakeAtt2.TriggerKind != protocol.TriggerChildResult || wakeAtt2.ChildResult == nil ||
		wakeAtt2.ChildResult.ChildTaskID != c2.Task.TaskID || wakeAtt2.ChildResult.ChildState != protocol.AttemptFailed ||
		!strings.Contains(wakeAtt2.ChildResult.ErrorDetail, "timed out") {
		t.Fatalf("unexpected queued child failure wake attempt: %+v (childResult=%+v)", wakeAtt2, wakeAtt2.ChildResult)
	}

	// Guard 1: archived parent must NOT be woken when child finishes
	archParent, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: protocol.ScratchProjectID,
		Title:     "Archived Parent",
		Goal:      "Will be archived",
	})
	if err != nil {
		t.Fatal(err)
	}
	archChild, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     protocol.ScratchProjectID,
		ParentTaskID:  archParent.Task.TaskID,
		Title:         "Child of Archived",
		Goal:          "Do work",
		MachineID:     "hub-node",
		CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE tasks SET archived_at = ? WHERE task_id = ?`, now, archParent.Task.TaskID); err != nil {
		t.Fatal(err)
	}
	sendRunnerJSON(t, ctx, conn, protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  archChild.Attempt.AttemptID,
		LeaseToken: archChild.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{
				OutboxSeq: 1,
				EventID:   "evt_arch_child_fin",
				FrameType: protocol.OutboxFrameState,
				Payload: map[string]any{
					"state":            string(protocol.AttemptSucceeded),
					"stopReason":       "end_turn",
					"completionSource": string(protocol.CompletionPromptEndTurn),
				},
				TS: now + 30,
			},
		},
	})

	// Guard 2: cancelled parent must NOT be woken when child finishes
	cancelParent, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: protocol.ScratchProjectID,
		Title:     "Cancelled Parent",
		Goal:      "Will be cancelled",
	})
	if err != nil {
		t.Fatal(err)
	}
	cancelChild, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     protocol.ScratchProjectID,
		ParentTaskID:  cancelParent.Task.TaskID,
		Title:         "Child of Cancelled",
		Goal:          "Do work",
		MachineID:     "hub-node",
		CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE tasks SET state = 'cancelled' WHERE task_id = ?`, cancelParent.Task.TaskID); err != nil {
		t.Fatal(err)
	}
	sendRunnerJSON(t, ctx, conn, protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  cancelChild.Attempt.AttemptID,
		LeaseToken: cancelChild.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{
				OutboxSeq: 1,
				EventID:   "evt_cancel_child_fin",
				FrameType: protocol.OutboxFrameState,
				Payload: map[string]any{
					"state":            string(protocol.AttemptSucceeded),
					"stopReason":       "end_turn",
					"completionSource": string(protocol.CompletionPromptEndTurn),
				},
				TS: now + 35,
			},
		},
	})

	time.Sleep(150 * time.Millisecond)
	archTL, err := st.GetTaskTimeline(ctx, archParent.Task.TaskID, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(archTL.Attempts) != 0 {
		t.Fatalf("archived parent should not be woken, got %d attempts", len(archTL.Attempts))
	}
	cancelTL, err := st.GetTaskTimeline(ctx, cancelParent.Task.TaskID, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(cancelTL.Attempts) != 0 {
		t.Fatalf("cancelled parent should not be woken, got %d attempts", len(cancelTL.Attempts))
	}

	// Verify SystemPromptAppend includes delegation guidance and child context on dispatched frames
	var sawChildPrompt, sawParentPrompt bool
	drainTimer := time.After(200 * time.Millisecond)
	for {
		select {
		case df := <-dispatches:
			if df.TaskID == c1.Task.TaskID {
				if !strings.Contains(df.SystemPromptAppend, "orbit_spawn_child") &&
					strings.Contains(df.SystemPromptAppend, "orbit_send_child") &&
					strings.Contains(df.SystemPromptAppend, parentRes.Task.TaskID) &&
					strings.Contains(df.SystemPromptAppend, "role=analyzer") {
					sawChildPrompt = true
				}
			}
			if df.TaskID == parentRes.Task.TaskID {
				if strings.Contains(df.SystemPromptAppend, "orbit_spawn_child") &&
					!strings.Contains(df.SystemPromptAppend, "Delegated Child Context:") {
					sawParentPrompt = true
				}
			}
		case <-drainTimer:
			goto donePromptCheck
		}
	}
donePromptCheck:
	if !sawChildPrompt || !sawParentPrompt {
		t.Fatalf("expected SystemPromptAppend delegation guidance on both parent (%v) and child (%v)", sawParentPrompt, sawChildPrompt)
	}
}

func TestSubsession_CrossMachineDelegateDeniedAndDurableNotificationRecovery(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "config")
	dataDir := filepath.Join(tmp, "data")
	_ = os.MkdirAll(cfgDir, 0o755)

	tokA := "runner-tok-nodeA"
	tokB := "runner-tok-nodeB"
	hashTok := func(tok string) string {
		s := sha256.Sum256([]byte(tok))
		return "sha256:" + hex.EncodeToString(s[:])
	}
	machinesYAML := `schemaVersion: 1
machines:
  node-a:
    displayName: "node-a"
    transport: local_ws
    tokenHash: "` + hashTok(tokA) + `"
    capabilities: [primary-dev]
  node-b:
    displayName: "node-b"
    transport: local_ws
    tokenHash: "` + hashTok(tokB) + `"
    capabilities: [primary-dev]
`
	_ = os.WriteFile(filepath.Join(cfgDir, "machines.yaml"), []byte(machinesYAML), 0o644)
	_ = os.WriteFile(filepath.Join(cfgDir, "projects.yaml"), []byte("schemaVersion: 1\nprojects: {}\n"), 0o644)

	st, err := store.Open(ctx, filepath.Join(dataDir, "orbit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := protocol.DefaultHubConfig()
	cfg.ConfigDir = cfgDir
	cfg.DataDir = dataDir
	cfg.DevOpen = true
	srv, err := hub.NewServer(ctx, hub.ServerOptions{HubConfig: cfg, Store: st, RyugiRunner: &fakeRyugiRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// 1. P1 #1: Parent attempt running on node-a; node-b trying to call ALL 5 delegate endpoints MUST get 403!
	parentOnA, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     protocol.ScratchProjectID,
		Title:         "Parent on A",
		Goal:          "Run on A",
		MachineID:     "node-a",
		CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	childOnA, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     protocol.ScratchProjectID,
		ParentTaskID:  parentOnA.Task.TaskID,
		Title:         "Child on A",
		Goal:          "Work on A",
		MachineID:     "node-a",
		CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	headersB := map[string]string{
		"X-Orbit-Machine-Id": "node-b",
		"Authorization":      "Bearer " + tokB,
	}
	for _, ep := range []struct {
		path string
		body map[string]any
	}{
		{"/api/runner/delegate/spawn", map[string]any{"parentTaskId": parentOnA.Task.TaskID, "parentAttemptId": parentOnA.Attempt.AttemptID, "goal": "hijack"}},
		{"/api/runner/delegate/list", map[string]any{"parentTaskId": parentOnA.Task.TaskID, "parentAttemptId": parentOnA.Attempt.AttemptID}},
		{"/api/runner/delegate/send", map[string]any{"parentTaskId": parentOnA.Task.TaskID, "parentAttemptId": parentOnA.Attempt.AttemptID, "childTaskId": childOnA.Task.TaskID, "message": "hijack"}},
		{"/api/runner/delegate/read", map[string]any{"parentTaskId": parentOnA.Task.TaskID, "parentAttemptId": parentOnA.Attempt.AttemptID, "childTaskId": childOnA.Task.TaskID}},
		{"/api/runner/delegate/cancel", map[string]any{"parentTaskId": parentOnA.Task.TaskID, "parentAttemptId": parentOnA.Attempt.AttemptID, "childTaskId": childOnA.Task.TaskID}},
	} {
		resp, body := doJSONReq(t, http.MethodPost, ts.URL+ep.path, ep.body, headersB)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 for cross-machine runner node-b on %s, got %d: %s", ep.path, resp.StatusCode, string(body))
		}
	}

	// 2. P1 #5 & #6: Expired child attempt + parent needs_verification retry recovery!
	// Put parentOnA into needs_verification so initial delivery defers (stays pending), then expire childOnA's lease!
	now := time.Now().UnixMilli()
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE attempts SET state = 'needs_verification', finished_at = ? WHERE attempt_id = ?`, now, parentOnA.Attempt.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE tasks SET state = 'in_review', active_attempt_id = NULL WHERE task_id = ?`, parentOnA.Task.TaskID); err != nil {
		t.Fatal(err)
	}
	// Expire childOnA via ScanExpiredAttempts (past lease_expires_at)
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE attempts SET lease_expires_at = ? WHERE attempt_id = ?`, now-10000, childOnA.Attempt.AttemptID); err != nil {
		t.Fatal(err)
	}
	expiredIDs, expEvents, err := st.ScanExpiredAttempts(ctx, now)
	if err != nil || len(expiredIDs) == 0 || len(expEvents) == 0 {
		t.Fatalf("expected childOnA to expire (ids=%v events=%d err=%v)", expiredIDs, len(expEvents), err)
	}

	// Notification row MUST exist in pending state; ProcessPendingChildNotifications while parent is needs_verification MUST leave it pending with backoff!
	pending, err := st.ListPendingChildParentNotifications(ctx, now, 0, 10)
	if err != nil || len(pending) != 1 || pending[0].ChildAttemptID != childOnA.Attempt.AttemptID || pending[0].ChildState != protocol.AttemptExpired {
		t.Fatalf("expected 1 pending expired child notification, got %+v (err=%v)", pending, err)
	}
	srv.ProcessPendingChildNotifications(ctx)
	var blockedStatus, blockedErr string
	var blockedRetry int
	var blockedNextRetry int64
	if err := st.ReadDB().QueryRowContext(ctx, `
		SELECT status, COALESCE(last_error, ''), retry_count, next_retry_at
		FROM child_parent_notifications WHERE child_attempt_id = ?
	`, childOnA.Attempt.AttemptID).Scan(&blockedStatus, &blockedErr, &blockedRetry, &blockedNextRetry); err != nil {
		t.Fatal(err)
	}
	if blockedStatus != "pending" || blockedErr == "" || blockedRetry != 1 || blockedNextRetry <= now {
		t.Fatalf("expected notification pending with backoff while parent needs_verification, got status=%q err=%q retry=%d next=%d", blockedStatus, blockedErr, blockedRetry, blockedNextRetry)
	}

	// Resolve parentOnA's needs_verification & clear backoff, then trigger recovery pass!
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE attempts SET state = 'succeeded' WHERE attempt_id = ?`, parentOnA.Attempt.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := st.ResetParentChildNotificationBackoff(ctx, parentOnA.Task.TaskID); err != nil {
		t.Fatal(err)
	}
	srv.ProcessPendingChildNotifications(ctx)

	// Notification MUST now be delivered (0 pending) and parentOnA MUST have a new child_result attempt!
	pendingAfterRecover, err := st.ListPendingChildParentNotifications(ctx, time.Now().UnixMilli()+60000, 0, 10)
	if err != nil || len(pendingAfterRecover) != 0 {
		t.Fatalf("expected 0 pending notifications after recovery, got %+v (err=%v)", pendingAfterRecover, err)
	}
	tlP, err := st.GetTaskTimeline(ctx, parentOnA.Task.TaskID, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	childResultCount := 0
	for _, a := range tlP.Attempts {
		if a.TriggerKind == protocol.TriggerChildResult {
			childResultCount++
		}
	}
	if childResultCount != 1 {
		t.Fatalf("expected exactly 1 child_result wakeup after expired child recovery, got %d", childResultCount)
	}
	// Re-running consumer MUST NOT duplicate wakeup
	srv.ProcessPendingChildNotifications(ctx)
	tlP2, _ := st.GetTaskTimeline(ctx, parentOnA.Task.TaskID, 0, false)
	if len(tlP2.Attempts) != len(tlP.Attempts) {
		t.Fatalf("re-running notification consumer duplicated attempts: before=%d after=%d", len(tlP.Attempts), len(tlP2.Attempts))
	}

	// 3. P1 #5: Parent cancelled concurrently before delivery -> notification marked 'skipped' and parent NOT restarted
	cancelParent, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: protocol.ScratchProjectID, Title: "Cancel Parent", Goal: "g", MachineID: "node-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	childOfCancelled, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: protocol.ScratchProjectID, ParentTaskID: cancelParent.Task.TaskID,
		Title: "Child Of Cancelled", Goal: "g", MachineID: "node-a", CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE attempts SET lease_expires_at = ? WHERE attempt_id = ?`, now-10000, childOfCancelled.Attempt.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ScanExpiredAttempts(ctx, now); err != nil {
		t.Fatal(err)
	}
	// Cancel parent BEFORE consumer runs
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE tasks SET state = 'cancelled' WHERE task_id = ?`, cancelParent.Task.TaskID); err != nil {
		t.Fatal(err)
	}
	srv.ProcessPendingChildNotifications(ctx)
	var notifStatus string
	if err := st.ReadDB().QueryRowContext(ctx, `SELECT status FROM child_parent_notifications WHERE child_attempt_id = ?`, childOfCancelled.Attempt.AttemptID).Scan(&notifStatus); err != nil {
		t.Fatal(err)
	}
	if notifStatus != "skipped" {
		t.Fatalf("expected cancelled parent notification status='skipped', got %q", notifStatus)
	}
	tlCancel, _ := st.GetTaskTimeline(ctx, cancelParent.Task.TaskID, 0, false)
	if len(tlCancel.Attempts) != 0 {
		t.Fatalf("cancelled parent must not be restarted, got %d attempts", len(tlCancel.Attempts))
	}
	// 4. P1 #6: Child entering needs_verification (uncertain approval during offline/expiry) also enqueues & wakes parent once!
	nvParent, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: protocol.ScratchProjectID, Title: "NV Parent", Goal: "g", MachineID: "node-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	nvChild, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: protocol.ScratchProjectID, ParentTaskID: nvParent.Task.TaskID,
		Title: "NV Child", Goal: "g", MachineID: "node-a", CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Seed an approved_pending_delivery approval with delivery_attempted=1 so lease expiry transitions child to needs_verification
	if _, err := st.WriteDB().ExecContext(ctx, `
		INSERT INTO approvals (
			approval_id, project_id, task_id, attempt_id, kind, action_name, target_env,
			input_sha, payload_json, payload_sha256, options_json, requested_by, state,
			delivery_attempted, expires_at, created_at
		) VALUES ('apr_nv_child', ?, ?, ?, 'acp_tool_permission', 'run', 'node-a', '', '{}', 'sha', '[]', 'agent', 'approved_pending_delivery', 1, ?, ?)
	`, protocol.ScratchProjectID, nvChild.Task.TaskID, nvChild.Attempt.AttemptID, now+60000, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE attempts SET lease_expires_at = ? WHERE attempt_id = ?`, now-10000, nvChild.Attempt.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ScanExpiredAttempts(ctx, now); err != nil {
		t.Fatal(err)
	}
	nvPending, err := st.ListPendingChildParentNotifications(ctx, time.Now().UnixMilli(), 0, 10)
	if err != nil || len(nvPending) != 1 || nvPending[0].ChildState != protocol.AttemptNeedsVerification {
		t.Fatalf("expected 1 pending needs_verification child notification, got %+v (err=%v)", nvPending, err)
	}
	srv.ProcessPendingChildNotifications(ctx)
	tlNV, err := st.GetTaskTimeline(ctx, nvParent.Task.TaskID, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(tlNV.Attempts) != 1 || tlNV.Attempts[0].TriggerKind != protocol.TriggerChildResult {
		t.Fatalf("expected nvParent woken once by needs_verification child, got %+v", tlNV.Attempts)
	}
}

func TestSubsession_NoHeadOfLineStarvationAcross65Notifications(t *testing.T) {
	ctx := context.Background()
	srv, st, ts, _ := setupSubsessionHub(t)

	// Parent A is blocked in needs_verification; Parent B is idle & ready.
	parentA, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     protocol.ScratchProjectID,
		Title:         "Blocked Parent A",
		Goal:          "blocked",
		MachineID:     "hub-node",
		CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	parentB, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: protocol.ScratchProjectID,
		Title:     "Ready Parent B",
		Goal:      "ready",
		MachineID: "hub-node",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE attempts SET state = 'needs_verification', finished_at = ? WHERE attempt_id = ?`, now, parentA.Attempt.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE tasks SET state = 'in_review', active_attempt_id = NULL WHERE task_id = ?`, parentA.Task.TaskID); err != nil {
		t.Fatal(err)
	}

	// Seed 64 finished children & pending notifications for Parent A (seq 1..64)
	var childATaskIDs []string
	for i := 1; i <= 64; i++ {
		cID := fmt.Sprintf("tsk_hol_a_%02d", i)
		aID := fmt.Sprintf("att_hol_a_%02d", i)
		childATaskIDs = append(childATaskIDs, cID)
		if _, err := st.WriteDB().ExecContext(ctx, `
			INSERT INTO tasks (
				task_id, project_id, parent_task_id, title, goal,
				required_capability, required_resources, preferred_adapter, desired_config_json,
				state, created_by, version, created_at, updated_at, delegation_depth, notify_parent_on_finish
			) VALUES (?, ?, ?, ?, 'g', 'primary-dev', '[]', 'agy', '{}', 'in_review', 'human', 1, ?, ?, 1, 1)
		`, cID, protocol.ScratchProjectID, parentA.Task.TaskID, fmt.Sprintf("Child A%02d", i), now+int64(i), now+int64(i)); err != nil {
			t.Fatal(err)
		}
		if _, err := st.WriteDB().ExecContext(ctx, `
			INSERT INTO attempts (
				attempt_id, task_id, project_id, attempt_no, machine_id, adapter,
				trigger_kind, trigger_payload_json, state, lease_token, lease_expires_at, started_at, finished_at
			) VALUES (?, ?, ?, 1, 'hub-node', 'agy', 'initial', '{}', 'succeeded', 'lt', ?, ?, ?)
		`, aID, cID, protocol.ScratchProjectID, now+60000, now+int64(i), now+int64(i)+1); err != nil {
			t.Fatal(err)
		}
		if _, err := st.WriteDB().ExecContext(ctx, `
			INSERT INTO child_parent_notifications (
				child_attempt_id, child_task_id, parent_task_id, child_state, status, created_at
			) VALUES (?, ?, ?, 'succeeded', 'pending', ?)
		`, aID, cID, parentA.Task.TaskID, now+int64(i)); err != nil {
			t.Fatal(err)
		}
	}

	// Seed 65th notification for ready Parent B (seq 65)
	childB, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     protocol.ScratchProjectID,
		ParentTaskID:  parentB.Task.TaskID,
		Title:         "Child B65",
		Goal:          "work B",
		MachineID:     "hub-node",
		CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE attempts SET state = 'succeeded', finished_at = ? WHERE attempt_id = ?`, now+100, childB.Attempt.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE tasks SET state = 'in_review', active_attempt_id = NULL WHERE task_id = ?`, childB.Task.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `
		INSERT INTO child_parent_notifications (
			child_attempt_id, child_task_id, parent_task_id, child_state, status, created_at
		) VALUES (?, ?, ?, 'succeeded', 'pending', ?)
	`, childB.Attempt.AttemptID, childB.Task.TaskID, parentB.Task.TaskID, now+100); err != nil {
		t.Fatal(err)
	}

	// Single drain pass MUST immediately deliver Parent B's 65th notification without being starved by Parent A's 64 blocked rows!
	srv.ProcessPendingChildNotifications(ctx)

	tlB, err := st.GetTaskTimeline(ctx, parentB.Task.TaskID, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(tlB.Attempts) != 1 || tlB.Attempts[0].TriggerKind != protocol.TriggerChildResult ||
		tlB.Attempts[0].ChildResult == nil || tlB.Attempts[0].ChildResult.ChildTaskID != childB.Task.TaskID {
		t.Fatalf("expected Parent B's 65th notification delivered immediately without HOL starvation, got attempts=%+v", tlB.Attempts)
	}

	// All 64 rows for Parent A must still be pending, with row #1 carrying retry_count=1 and next_retry_at > now
	var aPending int
	if err := st.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM child_parent_notifications WHERE parent_task_id = ? AND status = 'pending'`, parentA.Task.TaskID).Scan(&aPending); err != nil {
		t.Fatal(err)
	}
	if aPending != 64 {
		t.Fatalf("expected all 64 Parent A notifications still pending, got %d", aPending)
	}
	var headRetry int
	var headNext int64
	if err := st.ReadDB().QueryRowContext(ctx, `SELECT retry_count, next_retry_at FROM child_parent_notifications WHERE child_attempt_id = 'att_hol_a_01'`).Scan(&headRetry, &headNext); err != nil {
		t.Fatal(err)
	}
	if headRetry != 1 || headNext <= now {
		t.Fatalf("expected head row backoff set (retry=%d next=%d now=%d)", headRetry, headNext, now)
	}

	// Unlock Parent A with real verifiedSite=true followup -> resets backoff immediately and drains all 64 notifications in strict FIFO order!
	resp, body := doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+parentA.Task.TaskID+"/followup", protocol.FollowupTaskRequest{
		Message:           "verified site, proceed",
		Mode:              protocol.FollowupModeQueue,
		VerifiedSite:      true,
		VerifiedAttemptID: parentA.Attempt.AttemptID,
	}, nil)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("verified followup on Parent A failed (%d): %s", resp.StatusCode, string(body))
	}

	// Wait for background drain triggered by verifiedSite=true (under -race, 64 serialized SQLite txs take ~6-8s)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var rem int
		_ = st.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM child_parent_notifications WHERE parent_task_id = ? AND status = 'pending'`, parentA.Task.TaskID).Scan(&rem)
		if rem == 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	var remAfterUnlock int
	if err := st.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM child_parent_notifications WHERE parent_task_id = ? AND status = 'pending'`, parentA.Task.TaskID).Scan(&remAfterUnlock); err != nil {
		t.Fatal(err)
	}
	if remAfterUnlock != 0 {
		t.Fatalf("expected all 64 Parent A notifications drained immediately after verifiedSite unlock, remaining=%d", remAfterUnlock)
	}

	// Verify strict FIFO order (A01..A64) in Parent A's pending_inputs queue!
	queuedA, err := st.ListPendingInputs(ctx, parentA.Task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(queuedA) != 64 {
		t.Fatalf("expected 64 queued child_result inputs on Parent A, got %d", len(queuedA))
	}
	for i, pi := range queuedA {
		wantID := childATaskIDs[i]
		if !strings.Contains(pi.Body, wantID) {
			t.Fatalf("strict per-parent FIFO violated at index %d: expected %s in body %q", i, wantID, pi.Body)
		}
	}
}

func TestSubsession_CrossParentIdempotency409AndForkNoDeploy(t *testing.T) {
	ctx := context.Background()
	_, st, ts, runnerToken := setupSubsessionHub(t)

	p1, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: "demo-app", Title: "Parent 1", Goal: "p1", MachineID: "hub-node", CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: "demo-app", Title: "Parent 2", Goal: "p2", MachineID: "hub-node", CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	runnerHeaders := map[string]string{
		"X-Orbit-Machine-Id": "hub-node",
		"Authorization":      "Bearer " + runnerToken,
		"Idempotency-Key":    "dup-child-key",
	}
	resp, body := doJSONReq(t, http.MethodPost, ts.URL+"/api/runner/delegate/spawn", map[string]any{
		"parentTaskId":    p1.Task.TaskID,
		"parentAttemptId": p1.Attempt.AttemptID,
		"title":           "P1 Child",
		"goal":            "work for p1",
	}, runnerHeaders)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("p1 delegate spawn failed (%d): %s", resp.StatusCode, string(body))
	}

	// Same Idempotency-Key from P2 in the same project MUST return 409 Conflict (never leak P1's child)
	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/runner/delegate/spawn", map[string]any{
		"parentTaskId":    p2.Task.TaskID,
		"parentAttemptId": p2.Attempt.AttemptID,
		"title":           "P2 Child",
		"goal":            "work for p2",
	}, runnerHeaders)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for cross-parent idempotency collision, got %d: %s", resp.StatusCode, string(body))
	}

	// P2-3: fork_new_session on a busy task targeting ssh_deploy or deploy capability MUST return 403 ERR_DEPLOY_FORBIDDEN!
	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+p1.Task.TaskID+"/followup", protocol.FollowupTaskRequest{
		Message: "fork deploy adapter",
		Mode:    protocol.FollowupModeForkNewSession,
		Adapter: protocol.AdapterSSHDeploy,
	}, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for fork_new_session with ssh_deploy adapter, got %d: %s", resp.StatusCode, string(body))
	}
	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+p1.Task.TaskID+"/followup", protocol.FollowupTaskRequest{
		Message:            "fork deploy capability",
		Mode:               protocol.FollowupModeForkNewSession,
		RequiredCapability: "deploy-node",
	}, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for fork_new_session routing to ssh_outbound/deploy-node, got %d: %s", resp.StatusCode, string(body))
	}
}

func TestSubsession_ExpiredParentLeaseDeniedAcrossAllDelegateEndpoints(t *testing.T) {
	ctx := context.Background()
	_, st, ts, runnerToken := setupSubsessionHub(t)

	parent, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		Title:         "Parent With Expiring Lease",
		Goal:          "parent",
		MachineID:     "hub-node",
		CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		ParentTaskID:  parent.Task.TaskID,
		Title:         "Existing Child",
		Goal:          "child",
		MachineID:     "hub-node",
		CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Put parent attempt into state='running' with an EXPIRED lease_expires_at, WITHOUT running ScanExpiredAttempts!
	now := time.Now().UnixMilli()
	if _, err := st.WriteDB().ExecContext(ctx, `
		UPDATE attempts SET state = 'running', lease_expires_at = ? WHERE attempt_id = ?
	`, now-5000, parent.Attempt.AttemptID); err != nil {
		t.Fatal(err)
	}

	headers := map[string]string{
		"X-Orbit-Machine-Id": "hub-node",
		"Authorization":      "Bearer " + runnerToken,
	}
	for _, ep := range []struct {
		path string
		body map[string]any
	}{
		{"/api/runner/delegate/spawn", map[string]any{"parentTaskId": parent.Task.TaskID, "parentAttemptId": parent.Attempt.AttemptID, "goal": "should fail"}},
		{"/api/runner/delegate/list", map[string]any{"parentTaskId": parent.Task.TaskID, "parentAttemptId": parent.Attempt.AttemptID}},
		{"/api/runner/delegate/send", map[string]any{"parentTaskId": parent.Task.TaskID, "parentAttemptId": parent.Attempt.AttemptID, "childTaskId": child.Task.TaskID, "message": "should fail"}},
		{"/api/runner/delegate/read", map[string]any{"parentTaskId": parent.Task.TaskID, "parentAttemptId": parent.Attempt.AttemptID, "childTaskId": child.Task.TaskID}},
		{"/api/runner/delegate/cancel", map[string]any{"parentTaskId": parent.Task.TaskID, "parentAttemptId": parent.Attempt.AttemptID, "childTaskId": child.Task.TaskID}},
	} {
		resp, body := doJSONReq(t, http.MethodPost, ts.URL+ep.path, ep.body, headers)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 for expired parent lease on %s, got %d: %s", ep.path, resp.StatusCode, string(body))
		}
	}

	// Assert ZERO side effects
	kids, err := st.ListChildTasks(ctx, parent.Task.TaskID, true)
	if err != nil || len(kids) != 1 {
		t.Fatalf("expected still 1 child after expired-lease spawn denial, got %d (err=%v)", len(kids), err)
	}
	queued, err := st.ListPendingInputs(ctx, child.Task.TaskID)
	if err != nil || len(queued) != 0 {
		t.Fatalf("expected 0 queued inputs after expired-lease send denial, got %d (err=%v)", len(queued), err)
	}
	attRec, err := st.GetAttemptRecord(ctx, child.Attempt.AttemptID)
	if err != nil || attRec.State != protocol.AttemptLeased || attRec.CancelRequested {
		t.Fatalf("expected child attempt untouched after expired-lease cancel denial, got state=%s cancelReq=%v err=%v", attRec.State, attRec.CancelRequested, err)
	}
}

func TestSubsession_Hebi8ExcludesChildResultFromRecentUserPrompts(t *testing.T) {
	ctx := context.Background()
	srv, st, ts, runnerToken := setupSubsessionHub(t)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/runner?machineId=hub-node"
	conn, dispatches := dialTestRunnerWSCaptureDispatches(t, ctx, wsURL, "hub-node", "run-hebi8-p2", runnerToken)
	defer conn.CloseNow()

	hebi8Task, _, err := st.EnsureHebi8MainTask(ctx, protocol.AdapterAgy)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	// Seed 4 real user turns followed by 3 child_result system wake-up turns
	userPrompts := []string{"USER-PROMPT-1", "USER-PROMPT-2", "USER-PROMPT-3", "USER-PROMPT-4"}
	attNo := 0
	for _, up := range userPrompts {
		attNo++
		aid := "att_hebi8_u_" + up
		tpJSON, _ := json.Marshal(map[string]any{"prompt": up})
		if _, err := st.WriteDB().ExecContext(ctx, `
			INSERT INTO attempts (
				attempt_id, task_id, project_id, attempt_no, machine_id, adapter,
				trigger_kind, trigger_payload_json, state, lease_token, lease_expires_at, started_at, finished_at
			) VALUES (?, ?, ?, ?, 'hub-node', 'agy', 'followup', ?, 'succeeded', 'lt', ?, ?, ?)
		`, aid, hebi8Task.TaskID, protocol.PAProjectID, attNo, string(tpJSON), now+60000, now+int64(attNo), now+int64(attNo)+1); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 3; i++ {
		attNo++
		aid := "att_hebi8_cr_" + string(rune('0'+i))
		tpJSON, _ := json.Marshal(map[string]any{"prompt": "[Orbit 子会话回传] system wake " + string(rune('0'+i))})
		if _, err := st.WriteDB().ExecContext(ctx, `
			INSERT INTO attempts (
				attempt_id, task_id, project_id, attempt_no, machine_id, adapter,
				trigger_kind, trigger_payload_json, state, lease_token, lease_expires_at, started_at, finished_at
			) VALUES (?, ?, ?, ?, 'hub-node', 'agy', 'child_result', ?, 'succeeded', 'lt', ?, ?, ?)
		`, aid, hebi8Task.TaskID, protocol.PAProjectID, attNo, string(tpJSON), now+60000, now+int64(attNo), now+int64(attNo)+1); err != nil {
			t.Fatal(err)
		}
	}

	// Trigger a real Hebi8 fold via POST /api/hebi8/fold
	resp, body := doJSONReq(t, http.MethodPost, ts.URL+"/api/hebi8/fold", map[string]any{"reason": "test child_result exclusion"}, nil)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/hebi8/fold failed (%d): %s", resp.StatusCode, string(body))
	}

	// Send next user follow-up so Hub builds the real 6-part Fold-Rollover opening and dispatches it to runner!
	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+hebi8Task.TaskID+"/followup", protocol.FollowupTaskRequest{
		Message: "next real user question",
		Mode:    protocol.FollowupModeQueue,
	}, nil)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("post-fold followup failed (%d): %s", resp.StatusCode, string(body))
	}

	// Inspect the dispatched opening prompt:
	// - all 4 real user prompts MUST be preserved in Recent User Prompts
	// - tail turns MUST label child_result as "子会话回传：" (never "用户：[Orbit 子会话回传]") and retain real user turn USER-PROMPT-4!
	select {
	case df := <-dispatches:
		for _, up := range userPrompts {
			if !strings.Contains(df.Prompt, up) {
				t.Fatalf("expected Hebi8 fold opening to preserve %q without eviction by child_result turns; got prompt:\n%s", up, df.Prompt)
			}
		}
		if strings.Contains(df.Prompt, "用户：[Orbit 子会话回传]") {
			t.Fatalf("child_result turn must NOT be labeled as '用户：' in Hebi8 fold opening; got:\n%s", df.Prompt)
		}
		if !strings.Contains(df.Prompt, "子会话回传：[Orbit 子会话回传]") {
			t.Fatalf("expected child_result tail turn to be labeled '子会话回传：' in Hebi8 fold opening; got:\n%s", df.Prompt)
		}
		if !strings.Contains(df.Prompt, "用户：USER-PROMPT-4") {
			t.Fatalf("expected verbatim tail to preserve latest real user Q&A ('用户：USER-PROMPT-4'); got:\n%s", df.Prompt)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for post-fold Hebi8 dispatch")
	}

	// While a normal parent task is busy with an active attempt, finish a child task -> queued pending input MUST have source='system'!
	busyParent, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		Title:         "Busy Parent",
		Goal:          "working",
		MachineID:     "hub-node",
		CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     protocol.ScratchProjectID,
		ParentTaskID:  busyParent.Task.TaskID,
		Title:         "Parent Child",
		Goal:          "check",
		MachineID:     "hub-node",
		CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE attempts SET lease_expires_at = ? WHERE attempt_id = ?`, now-10000, child.Attempt.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ScanExpiredAttempts(ctx, now); err != nil {
		t.Fatal(err)
	}
	srv.ProcessPendingChildNotifications(ctx)

	var queuedSource string
	if err := st.ReadDB().QueryRowContext(ctx, `
		SELECT source FROM task_pending_inputs WHERE task_id = ? AND state = 'queued' ORDER BY created_at DESC LIMIT 1
	`, busyParent.Task.TaskID).Scan(&queuedSource); err != nil {
		t.Fatalf("query queued child_result pending input: %v", err)
	}
	if queuedSource != string(protocol.PendingSourceSystem) {
		t.Fatalf("expected queued child_result pending input source='system', got %q", queuedSource)
	}
}

func TestSubsession_V4SingleLayerPANoSubsessionsAndSiblingCollaboration(t *testing.T) {
	ctx := context.Background()
	_, st, ts, runnerToken := setupSubsessionHub(t)

	runnerHeaders := map[string]string{
		"X-Orbit-Machine-Id": "hub-node",
		"Authorization":      "Bearer " + runnerToken,
	}

	// 1. PA/Hebi8 cannot spawn children via client API, runner delegate API, or fork_new_session
	hebi8Task, _, err := st.EnsureHebi8MainTask(ctx, protocol.AdapterAgy)
	if err != nil {
		t.Fatal(err)
	}
	_, hebi8Att, _, err := st.CreateNextAttempt(ctx, store.CreateNextAttemptParams{
		TaskID:      hebi8Task.TaskID,
		MachineID:   "hub-node",
		Adapter:     protocol.AdapterAgy,
		TriggerKind: protocol.TriggerFollowup,
		Prompt:      "active hebi8",
		LeaseTTLMs:  60000,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, body := doJSONReq(t, http.MethodPost, ts.URL+"/api/runner/delegate/spawn", map[string]any{
		"parentTaskId":    hebi8Task.TaskID,
		"parentAttemptId": hebi8Att.AttemptID,
		"goal":            "blocked pa child",
	}, runnerHeaders)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "ERR_PA_NO_SUBSESSIONS") {
		t.Fatalf("expected 409 ERR_PA_NO_SUBSESSIONS on runner delegate spawn from _pa, got %d: %s", resp.StatusCode, string(body))
	}
	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+hebi8Task.TaskID+"/followup", protocol.FollowupTaskRequest{
		Message: "fork from hebi8",
		Mode:    protocol.FollowupModeForkNewSession,
	}, nil)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "ERR_PA_NO_SUBSESSIONS") {
		t.Fatalf("expected 409 ERR_PA_NO_SUBSESSIONS on fork_new_session from _pa, got %d: %s", resp.StatusCode, string(body))
	}

	// 2. Setup two parents (rootA with childA1 & childA2, rootB with childB1)
	rootA, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		Title:         "Root A",
		Goal:          "Root A goal",
		MachineID:     "hub-node",
		CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	childA1, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		ParentTaskID:  rootA.Task.TaskID,
		DelegatedRole: "backend",
		Title:         "Child A1",
		Goal:          "Backend work",
		MachineID:     "hub-node",
		CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	childA2, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		ParentTaskID:  rootA.Task.TaskID,
		DelegatedRole: "frontend",
		Title:         "Child A2",
		Goal:          "Frontend work",
		MachineID:     "hub-node",
		CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	rootB, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		Title:         "Root B",
		Goal:          "Root B goal",
		MachineID:     "hub-node",
		CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	childB1, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		ParentTaskID:  rootB.Task.TaskID,
		Title:         "Child B1",
		Goal:          "Unrelated child",
		MachineID:     "hub-node",
		CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 3. Max depth = 1: childA1 CANNOT spawn grandchild via client API, runner delegate spawn, OR fork_new_session
	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+childA1.Task.TaskID+"/children", protocol.CreateChildTaskRequest{
		Goal: "grandchild via client",
	}, nil)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "ERR_MAX_SUBSESSION_DEPTH") {
		t.Fatalf("expected 409 ERR_MAX_SUBSESSION_DEPTH on client spawn grandchild, got %d: %s", resp.StatusCode, string(body))
	}
	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/runner/delegate/spawn", map[string]any{
		"parentTaskId":    childA1.Task.TaskID,
		"parentAttemptId": childA1.Attempt.AttemptID,
		"goal":            "grandchild via runner",
	}, runnerHeaders)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "ERR_MAX_SUBSESSION_DEPTH") {
		t.Fatalf("expected 409 ERR_MAX_SUBSESSION_DEPTH on runner spawn grandchild, got %d: %s", resp.StatusCode, string(body))
	}
	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+childA1.Task.TaskID+"/followup", protocol.FollowupTaskRequest{
		Message: "fork grandchild",
		Mode:    protocol.FollowupModeForkNewSession,
	}, nil)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "ERR_MAX_SUBSESSION_DEPTH") {
		t.Fatalf("expected 409 ERR_MAX_SUBSESSION_DEPTH on fork_new_session from child, got %d: %s", resp.StatusCode, string(body))
	}

	// 4. Sibling list: childA1 sees childA2 (not self), scope="siblings", and NO spawn targets
	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/runner/delegate/list", map[string]any{
		"parentTaskId":    childA1.Task.TaskID,
		"parentAttemptId": childA1.Attempt.AttemptID,
		"includeTargets":  true,
	}, runnerHeaders)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sibling list failed (%d): %s", resp.StatusCode, string(body))
	}
	var sibList struct {
		ParentTaskID string                   `json:"parentTaskId"`
		Scope        string                   `json:"scope"`
		Children     []protocol.ChildTaskView `json:"children"`
		Machines     []map[string]any         `json:"machines"`
		Projects     []map[string]any         `json:"projects"`
	}
	if err := json.Unmarshal(body, &sibList); err != nil {
		t.Fatal(err)
	}
	if sibList.Scope != "siblings" || sibList.ParentTaskID != rootA.Task.TaskID || len(sibList.Children) != 1 || sibList.Children[0].TaskID != childA2.Task.TaskID {
		t.Fatalf("unexpected sibling list response: %+v", sibList)
	}
	if len(sibList.Machines) != 0 || len(sibList.Projects) != 0 {
		t.Fatalf("expected no spawn targets for child caller, got machines=%d projects=%d", len(sibList.Machines), len(sibList.Projects))
	}

	// 5. Sibling read: childA1 can read childA2 transcript
	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/runner/delegate/read", map[string]any{
		"parentTaskId":    childA1.Task.TaskID,
		"parentAttemptId": childA1.Attempt.AttemptID,
		"childTaskId":     childA2.Task.TaskID,
		"limit":           5,
	}, runnerHeaders)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sibling read failed (%d): %s", resp.StatusCode, string(body))
	}

	// 6. Sibling send: childA1 can send a queued follow-up to childA2
	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/runner/delegate/send", map[string]any{
		"parentTaskId":    childA1.Task.TaskID,
		"parentAttemptId": childA1.Attempt.AttemptID,
		"childTaskId":     childA2.Task.TaskID,
		"message":         "API contract is ready at /api/v4",
		"mode":            "queue",
	}, runnerHeaders)
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusCreated {
		t.Fatalf("sibling send failed (%d): %s", resp.StatusCode, string(body))
	}
	queued, err := st.ListPendingInputs(ctx, childA2.Task.TaskID)
	if err != nil || len(queued) != 1 || queued[0].Payload["delegatedFromTaskId"] != childA1.Task.TaskID || queued[0].Payload["delegatedRelation"] != "sibling" {
		t.Fatalf("unexpected sibling queued input: %+v err=%v", queued, err)
	}

	// 7. Sibling cancel MUST be forbidden (403)
	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/runner/delegate/cancel", map[string]any{
		"parentTaskId":    childA1.Task.TaskID,
		"parentAttemptId": childA1.Attempt.AttemptID,
		"childTaskId":     childA2.Task.TaskID,
		"reason":          "not allowed",
	}, runnerHeaders)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 cancelling sibling task, got %d: %s", resp.StatusCode, string(body))
	}

	// 8. Cross-parent, parent, and self targets MUST all be forbidden (403)
	for _, targetID := range []string{childB1.Task.TaskID, rootA.Task.TaskID, childA1.Task.TaskID} {
		for _, path := range []string{"/api/runner/delegate/read", "/api/runner/delegate/send", "/api/runner/delegate/cancel"} {
			payload := map[string]any{
				"parentTaskId":    childA1.Task.TaskID,
				"parentAttemptId": childA1.Attempt.AttemptID,
				"childTaskId":     targetID,
				"message":         "should fail",
			}
			resp, body = doJSONReq(t, http.MethodPost, ts.URL+path, payload, runnerHeaders)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("expected 403 for %s target=%s, got %d: %s", path, targetID, resp.StatusCode, string(body))
			}
		}
	}
}
