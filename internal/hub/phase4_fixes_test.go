package hub_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Hebi8/hebi8-Orbit/internal/hub"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

func TestPhase4_CheckpointWithdrawAndDismissedCompleteHTTP(t *testing.T) {
	_, st, ts, _ := setupSubsessionHub(t)
	ctx := context.Background()

	// Active task so follow-up is queued in task_pending_inputs
	created, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		Title:         "HTTP Checkpoint Task",
		Goal:          "Run",
		MachineID:     "hub-node",
		Adapter:       protocol.AdapterAgy,
		CreateAttempt: true,
		LeaseTTLMs:    60000,
	})
	if err != nil {
		t.Fatal(err)
	}
	dlvID := "dlv_http_cp_1"
	if _, err := st.CreateManualImportDelivery(ctx, store.ManualImportParams{
		DeliveryID:      dlvID,
		ProjectID:       "demo-app",
		TaskID:          created.Task.TaskID,
		Title:           "HTTP Checkpoint Task",
		Summary:         "Ready",
		SourceMachineID: "hub-node",
		BaseSHA:         "1111111111111111111111111111111111111111",
		HeadSHA:         "2222222222222222222222222222222222222222",
		GitRef:          "refs/orbit/deliveries/" + dlvID,
		Kind:            protocol.DeliveryCodeDiff,
		ActorID:         "human",
	}); err != nil {
		t.Fatal(err)
	}

	// 1. Mark Checkpoint -> Enqueue follow-up -> Withdraw follow-up -> reopen-review MUST return 409 ERR_CHECKPOINT_LOCKED
	resp, body := doJSONReq(t, http.MethodPost, ts.URL+"/api/deliveries/"+dlvID+"/complete-review", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("complete-review failed (%d): %s", resp.StatusCode, string(body))
	}
	subRes, err := st.SubmitFollowupAtomic(ctx, store.SubmitFollowupParams{
		TaskID:           created.Task.TaskID,
		Body:             "Queued after checkpoint",
		Mode:             protocol.FollowupModeQueue,
		MachineID:        "hub-node",
		Adapter:          protocol.AdapterAgy,
		AnchorCapability: "primary-dev",
		ActorKind:        protocol.ActorHuman,
		ActorID:          "human",
	})
	if err != nil || subRes.PendingInput == nil {
		t.Fatalf("expected queued input, got %+v err=%v", subRes, err)
	}
	if _, err := st.DeletePendingInput(ctx, created.Task.TaskID, subRes.PendingInput.InputID, "human"); err != nil {
		t.Fatal(err)
	}
	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/deliveries/"+dlvID+"/reopen-review", nil, nil)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "ERR_CHECKPOINT_LOCKED") {
		t.Fatalf("expected 409 ERR_CHECKPOINT_LOCKED after withdraw, got %d: %s", resp.StatusCode, string(body))
	}

	// 2. Dismissed inbox item -> complete-review returns 409 ERR_REVIEW_NOT_OPEN
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE inbox_items SET state = 'dismissed', resolved_at = NULL WHERE inbox_id = ?`, "inb:dlv:"+dlvID); err != nil {
		t.Fatal(err)
	}
	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/deliveries/"+dlvID+"/complete-review", nil, nil)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "ERR_REVIEW_NOT_OPEN") {
		t.Fatalf("expected 409 ERR_REVIEW_NOT_OPEN for dismissed delivery, got %d: %s", resp.StatusCode, string(body))
	}

	// 3. Missing inbox item -> complete-review returns 404
	if _, err := st.WriteDB().ExecContext(ctx, `DELETE FROM inbox_items WHERE inbox_id = ?`, "inb:dlv:"+dlvID); err != nil {
		t.Fatal(err)
	}
	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/deliveries/"+dlvID+"/complete-review", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for missing inbox item, got %d: %s", resp.StatusCode, string(body))
	}
}

func TestPhase4_LiveACPUsagePushedOverClientWSWithoutRawFlood(t *testing.T) {
	srv, st, ts, runnerToken := setupSubsessionHub(t)
	ctx := context.Background()

	// Connect runner WS so outbox batch goes through the real Hub runner gateway -> broker -> client WS path
	runnerWSURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/runner"
	runnerConn := dialTestRunnerWS(t, ctx, runnerWSURL, "hub-node", "ri_usage_ws", runnerToken)
	defer runnerConn.CloseNow()

	created, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		Title:         "Live Usage Task",
		Goal:          "Stream usage",
		MachineID:     "hub-node",
		Adapter:       protocol.AdapterAgy,
		CreateAttempt: true,
		LeaseTTLMs:    60000,
	})
	if err != nil {
		t.Fatal(err)
	}
	headSeq, err := st.MaxEventSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Connect client WS after current seq
	clientWSURL := fmt.Sprintf("ws%s/ws/client?sinceSeq=%d", strings.TrimPrefix(ts.URL, "http"), headSeq)
	clientConn, _, err := websocket.Dial(ctx, clientWSURL, nil)
	if err != nil {
		t.Fatalf("dial client ws: %v", err)
	}
	defer clientConn.CloseNow()

	// Wait for replay_end
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		_, data, err := clientConn.Read(readCtx)
		if err != nil {
			t.Fatalf("read replay_end: %v", err)
		}
		var msg protocol.WSServerMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type == protocol.WSMessageReplayEnd {
			break
		}
	}

	// Runner sends outbox batch containing BOTH raw acp.message_batch AND raw acp.usage
	now := time.Now().UnixMilli()
	sendRunnerJSON(t, ctx, runnerConn, protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  created.Attempt.AttemptID,
		LeaseToken: created.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{
				OutboxSeq: 1,
				EventID:   "ev_raw_msg_1",
				FrameType: protocol.OutboxFrameEvents,
				TS:        now,
				Payload: map[string]any{
					"kind":       string(protocol.EventACPMessageBatch),
					"visibility": string(protocol.VisibilityRawACP),
					"actorKind":  string(protocol.ActorAgent),
					"actorId":    "agy",
					"payload":    map[string]any{"text": "streaming chunk should not flood WS"},
				},
			},
			{
				OutboxSeq: 2,
				EventID:   "ev_raw_usage_1",
				FrameType: protocol.OutboxFrameEvents,
				TS:        now + 5,
				Payload: map[string]any{
					"kind":       string(protocol.EventACPUsage),
					"visibility": string(protocol.VisibilityRawACP),
					"actorKind":  string(protocol.ActorAgent),
					"actorId":    "agy",
					"payload":    map[string]any{"used": 42000, "size": 200000, "unsolicited": false},
				},
			},
		},
	})

	// Client WS MUST receive the lightweight acp.usage event (and NOT acp.message_batch!)
	gotUsage := false
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for !gotUsage {
		rctx, rcancel := context.WithTimeout(ctx, 4*time.Second)
		_, data, err := clientConn.Read(rctx)
		rcancel()
		if err != nil {
			t.Fatalf("expected live acp.usage frame on client WS, got err: %v", err)
		}
		var msg protocol.WSServerMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type != protocol.WSMessageEvent || msg.Event == nil {
			continue
		}
		if msg.Event.Kind == protocol.EventACPMessageBatch {
			t.Fatalf("raw acp.message_batch must NOT be pushed over client WS")
		}
		if msg.Event.Kind == protocol.EventACPUsage {
			if msg.Event.TaskID != created.Task.TaskID || msg.Event.Visibility == protocol.VisibilityRawACP {
				t.Fatalf("unexpected acp.usage WS event: %+v", msg.Event)
			}
			if used, _ := msg.Event.Payload["used"].(float64); int64(used) != 42000 {
				t.Fatalf("expected used=42000, got %+v", msg.Event.Payload)
			}
			if size, _ := msg.Event.Payload["size"].(float64); int64(size) != 200000 {
				t.Fatalf("expected size=200000, got %+v", msg.Event.Payload)
			}
			gotUsage = true
		}
	}
	_ = srv
}

func TestPhase4_LegacyMultiLevelAuthStrictOneHopAndDelegationProjection(t *testing.T) {
	_, st, ts, runnerToken := setupSubsessionHub(t)
	ctx := context.Background()
	runnerHeaders := map[string]string{
		"X-Orbit-Machine-Id": "hub-node",
		"Authorization":      "Bearer " + runnerToken,
	}

	root, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		Title:         "Root Task",
		Goal:          "Root goal",
		MachineID:     "hub-node",
		CreateAttempt: true,
		LeaseTTLMs:    60000,
	})
	if err != nil {
		t.Fatal(err)
	}
	child1, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		ParentTaskID:  root.Task.TaskID,
		Title:         "Child 1",
		Goal:          "Child 1 goal",
		MachineID:     "hub-node",
		CreateAttempt: true,
		LeaseTTLMs:    60000,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Child 2 starts IDLE so we can test direct idle attempt projection; Child 3 starts BUSY so we can test queued->dequeued projection!
	child2Idle, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		ParentTaskID:  root.Task.TaskID,
		Title:         "Child 2 Idle",
		Goal:          "Child 2 goal",
		MachineID:     "hub-node",
		CreateAttempt: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	child3Busy, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		ParentTaskID:  root.Task.TaskID,
		Title:         "Child 3 Busy",
		Goal:          "Child 3 goal",
		MachineID:     "hub-node",
		CreateAttempt: true,
		LeaseTTLMs:    60000,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Seed a legacy pre-v4 depth-2 grandchild directly into SQLite (historical records preserved, never deleted).
	now := time.Now().UnixMilli()
	grandchildID := "tsk_legacy_grandchild_1"
	grandchildAttID := "att_legacy_grandchild_1"
	if _, err := st.WriteDB().ExecContext(ctx, `
		INSERT INTO tasks (
			task_id, project_id, parent_task_id, title, goal,
			required_capability, required_resources, preferred_adapter,
			desired_config_json, state, active_attempt_id, created_by, version, created_at, updated_at
		) VALUES (?, 'demo-app', ?, 'Legacy Grandchild', 'Old depth-2 task', 'primary-dev', '[]', 'agy', '{}', 'running', ?, 'human', 1, ?, ?)
	`, grandchildID, child1.Task.TaskID, grandchildAttID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `
		INSERT INTO attempts (
			attempt_id, task_id, project_id, attempt_no, machine_id, adapter,
			trigger_kind, trigger_payload_json, state, lease_token, lease_expires_at, started_at
		) VALUES (?, ?, 'demo-app', 1, 'hub-node', 'agy', 'initial', '{}', 'running', 'tok_gc', ?, ?)
	`, grandchildAttID, grandchildID, now+60000, now); err != nil {
		t.Fatal(err)
	}

	// 1. Strict 1-hop authorization:
	//    - root -> grandchild: read / send / cancel ALL forbidden (403)
	//    - child1 -> grandchild: read / send / cancel ALL forbidden (403), sealing legacy depth-2 control paths!
	for _, caller := range []struct {
		name   string
		taskID string
		attID  string
	}{
		{"root", root.Task.TaskID, root.Attempt.AttemptID},
		{"child1", child1.Task.TaskID, child1.Attempt.AttemptID},
	} {
		for _, ep := range []string{"/api/runner/delegate/read", "/api/runner/delegate/send", "/api/runner/delegate/cancel"} {
			resp, body := doJSONReq(t, http.MethodPost, ts.URL+ep, map[string]any{
				"parentTaskId":    caller.taskID,
				"parentAttemptId": caller.attID,
				"childTaskId":     grandchildID,
				"message":         "must be denied",
				"reason":          "must be denied",
			}, runnerHeaders)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("expected 403 for %s -> legacy grandchild on %s, got %d: %s", caller.name, ep, resp.StatusCode, string(body))
			}
		}
	}

	// Verify legacy grandchild record is still preserved in DB
	if _, err := st.GetTask(ctx, grandchildID); err != nil {
		t.Fatalf("legacy grandchild record should be preserved, got err=%v", err)
	}

	// 2. Sibling send to IDLE sibling -> AttemptView projects delegatedFromTaskId/delegatedRelation AND sender gets delegation.sent event!
	resp, body := doJSONReq(t, http.MethodPost, ts.URL+"/api/runner/delegate/send", map[string]any{
		"parentTaskId":    child1.Task.TaskID,
		"parentAttemptId": child1.Attempt.AttemptID,
		"childTaskId":     child2Idle.Task.TaskID,
		"message":         "Schema is ready for integration",
		"mode":            "queue",
	}, runnerHeaders)
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusCreated {
		t.Fatalf("sibling send to idle child2 failed (%d): %s", resp.StatusCode, string(body))
	}
	tlChild2, err := st.GetTaskTimeline(ctx, child2Idle.Task.TaskID, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(tlChild2.Attempts) != 1 || tlChild2.Attempts[0].DelegatedFromTaskID != child1.Task.TaskID || tlChild2.Attempts[0].DelegatedRelation != "sibling" {
		t.Fatalf("expected idle sibling AttemptView delegation origin projected, got %+v", tlChild2.Attempts)
	}

	// 3. Sibling send to BUSY sibling -> queued -> after active attempt completes and ConsumeNextPendingInput runs, AttemptView STILL projects origin!
	resp, body = doJSONReq(t, http.MethodPost, ts.URL+"/api/runner/delegate/send", map[string]any{
		"parentTaskId":    child1.Task.TaskID,
		"parentAttemptId": child1.Attempt.AttemptID,
		"childTaskId":     child3Busy.Task.TaskID,
		"message":         "Queued sibling handoff",
		"mode":            "queue",
	}, runnerHeaders)
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusCreated {
		t.Fatalf("sibling send to busy child3 failed (%d): %s", resp.StatusCode, string(body))
	}
	// Finish child3Busy's initial attempt and consume its queued input
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE attempts SET state = 'succeeded', finished_at = ? WHERE attempt_id = ?`, now+10, child3Busy.Attempt.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE tasks SET state = 'in_review', active_attempt_id = NULL WHERE task_id = ?`, child3Busy.Task.TaskID); err != nil {
		t.Fatal(err)
	}
	_, dispatchedAtt, _, err := st.ConsumeNextPendingInput(ctx, store.ConsumeNextPendingInputParams{
		TaskID:     child3Busy.Task.TaskID,
		MachineID:  "hub-node",
		Adapter:    protocol.AdapterAgy,
		LeaseTTLMs: 60000,
	})
	if err != nil || dispatchedAtt == nil {
		t.Fatalf("ConsumeNextPendingInput failed: att=%+v err=%v", dispatchedAtt, err)
	}
	tlChild3, err := st.GetTaskTimeline(ctx, child3Busy.Task.TaskID, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(tlChild3.Attempts) != 2 || tlChild3.Attempts[1].DelegatedFromTaskID != child1.Task.TaskID || tlChild3.Attempts[1].DelegatedRelation != "sibling" {
		t.Fatalf("expected dequeued sibling AttemptView delegation origin projected, got %+v", tlChild3.Attempts)
	}

	// 4. Sender timeline MUST contain outbound delegation.sent events for both sends!
	tlSender, err := st.GetTaskTimeline(ctx, child1.Task.TaskID, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	sentTargets := map[string]string{}
	for _, ev := range tlSender.Events {
		if ev.Kind == protocol.EventDelegationSent && ev.AttemptID == child1.Attempt.AttemptID {
			tid, _ := ev.Payload["targetTaskId"].(string)
			rel, _ := ev.Payload["relation"].(string)
			sentTargets[tid] = rel
		}
	}
	if sentTargets[child2Idle.Task.TaskID] != "sibling" || sentTargets[child3Busy.Task.TaskID] != "sibling" {
		t.Fatalf("expected delegation.sent events on sender timeline for both targets, got %+v", sentTargets)
	}
}

func initTestGitRepo(t *testing.T, dir, originURL string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "user@example.com"},
		{"git", "config", "user.name", "Orbit Test"},
		{"git", "remote", "add", "origin", originURL},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v failed: %v\n%s", args, err, string(out))
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"git", "add", "."}, {"git", "commit", "-m", "init"}} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v failed: %v\n%s", args, err, string(out))
		}
	}
}

func TestPhase4_PatchLocationRepoIdentitySyncAndConflict(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	workRoot := filepath.Join(tmp, "work")
	repoA := filepath.Join(workRoot, "repo-a")
	repoB := filepath.Join(workRoot, "repo-b")
	plainDir := filepath.Join(workRoot, "plain-folder")
	initTestGitRepo(t, repoA, "https://github.com/example/repo-a.git")
	initTestGitRepo(t, repoB, "https://github.com/example/repo-b.git")
	if err := os.MkdirAll(plainDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cfgDir := filepath.Join(tmp, "config")
	dataDir := filepath.Join(tmp, "data")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "machines.yaml"), []byte(`schemaVersion: 1
machines:
  hub-node:
    displayName: "Hub Node"
    transport: "local_ws"
    capabilities: ["primary-dev"]
  mac-node:
    displayName: "Mac Node"
    transport: "local_ws"
    capabilities: ["primary-dev"]
`), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(tmp, "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv, err := hub.NewServer(ctx, hub.ServerOptions{
		Store: st,
		HubConfig: protocol.HubConfig{
			ListenAddr: "127.0.0.1:0",
			DataDir:    dataDir,
			ConfigDir:  cfgDir,
			DevToken:   "test-dev-token",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	if _, err := st.UpdateMachineMetadata(ctx, "hub-node", protocol.UpdateMachineRequest{
		AllowedRoots: []string{workRoot},
	}, "human"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateMachineMetadata(ctx, "mac-node", protocol.UpdateMachineRequest{
		AllowedRoots: []string{workRoot},
	}, "human"); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	doReq := func(method, path string, body any) (*http.Response, []byte) {
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		req, _ := http.NewRequest(method, ts.URL+path, &buf)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Orbit-Dev-Token", "test-dev-token")
		req.Header.Set("X-Orbit-Request", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out bytes.Buffer
		_, _ = out.ReadFrom(resp.Body)
		return resp, out.Bytes()
	}

	// Create project bound to repoA (single location)
	resp, raw := doReq(http.MethodPost, "/api/projects", protocol.CreateProjectRequest{
		MachineID:   "hub-node",
		RepoPath:    repoA,
		DisplayName: "Repo Sync Proj",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create project failed (%d): %s", resp.StatusCode, string(raw))
	}
	var created protocol.CreateProjectResponse
	_ = json.Unmarshal(raw, &created)
	projID := created.Project.ProjectID
	partID := created.Project.Parts[0].PartID
	loc1ID := created.Project.Parts[0].Locations[0].LocationID
	if !strings.Contains(created.Project.Parts[0].RepoIdentity, "repo-a") {
		t.Fatalf("expected initial repo_identity containing repo-a, got %q", created.Project.Parts[0].RepoIdentity)
	}

	// 1. Single-location part: PATCH absPath -> repoB MUST sync part.repo_identity to repo-b
	resp, raw = doReq(http.MethodPatch, "/api/projects/"+projID+"/parts/"+partID+"/locations/"+loc1ID, protocol.UpdateProjectLocationRequest{
		AbsPath: &repoB,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH to repoB failed (%d): %s", resp.StatusCode, string(raw))
	}
	var patchRes struct {
		Project protocol.ProjectView `json:"project"`
	}
	_ = json.Unmarshal(raw, &patchRes)
	if !strings.Contains(patchRes.Project.Parts[0].RepoIdentity, "repo-b") {
		t.Fatalf("expected part.repo_identity synced to repo-b, got %q", patchRes.Project.Parts[0].RepoIdentity)
	}

	// 2. Single-location part: PATCH absPath -> plainDir MUST clear part.repo_identity
	resp, raw = doReq(http.MethodPatch, "/api/projects/"+projID+"/parts/"+partID+"/locations/"+loc1ID, protocol.UpdateProjectLocationRequest{
		AbsPath: &plainDir,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH to plainDir failed (%d): %s", resp.StatusCode, string(raw))
	}
	patchRes = struct {
		Project protocol.ProjectView `json:"project"`
	}{}
	_ = json.Unmarshal(raw, &patchRes)
	if patchRes.Project.Parts[0].RepoIdentity != "" {
		t.Fatalf("expected part.repo_identity cleared for plain folder, got %q", patchRes.Project.Parts[0].RepoIdentity)
	}

	// Rebind back to repoA, add a second location (mac-node -> repoA), then PATCH loc2 -> repoB MUST return 409 ERR_REPO_IDENTITY_CONFLICT!
	resp, raw = doReq(http.MethodPatch, "/api/projects/"+projID+"/parts/"+partID+"/locations/"+loc1ID, protocol.UpdateProjectLocationRequest{
		AbsPath: &repoA,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH back to repoA failed (%d): %s", resp.StatusCode, string(raw))
	}
	_, loc2, _, err := st.CreateProjectLocation(ctx, projID, partID, protocol.CreateProjectLocationRequest{
		MachineID: "mac-node",
		AbsPath:   repoA,
		Isolation: protocol.IsolationWorktree,
	}, &protocol.LocationRuntimeState{Exists: true, IsGit: true}, "github.com/example/repo-a", "human")
	if err != nil {
		t.Fatal(err)
	}

	resp, raw = doReq(http.MethodPatch, "/api/projects/"+projID+"/parts/"+partID+"/locations/"+loc2.LocationID, protocol.UpdateProjectLocationRequest{
		AbsPath: &repoB,
	})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(raw), "ERR_REPO_IDENTITY_CONFLICT") {
		t.Fatalf("expected 409 ERR_REPO_IDENTITY_CONFLICT for multi-location conflict, got %d: %s", resp.StatusCode, string(raw))
	}
}

func TestPhase4_ArchivedTasksServerFilterSortAndFullCounts(t *testing.T) {
	_, st, ts, _ := setupSubsessionHub(t)
	ctx := context.Background()

	// Create 215 archived top-level tasks across demo-app (130) and _scratch (85), plus 5 archived child tasks
	// so we exceed the 200-row LIMIT and verify SQL-level filtering/sorting + untruncated top-level counts.
	baseTS := time.Now().UnixMilli() - 500_000
	tx, err := st.WriteDB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 215; i++ {
		pid := "demo-app"
		if i > 130 {
			pid = protocol.ScratchProjectID
		}
		tid := fmt.Sprintf("tsk_arch_%03d", i)
		title := fmt.Sprintf("Archived Item %03d", i)
		if i == 1 {
			title = "AAA Needle Oldest 001"
		}
		if i == 215 {
			title = "ZZZ Newest 215"
		}
		archAt := baseTS + int64(i*1000)
		updAt := baseTS + int64((216-i)*1000) // Reverse updated_at relative to archived_at!
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tasks (
				task_id, project_id, title, goal,
				required_capability, required_resources, preferred_adapter,
				desired_config_json, state, created_by, version, created_at, updated_at, archived_at
			) VALUES (?, ?, ?, ?, 'primary-dev', '[]', 'agy', '{}', 'done', 'human', 1, ?, ?, ?)
		`, tid, pid, title, "goal "+tid, baseTS, updAt, archAt); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	// Also insert 5 archived child tasks (must NOT inflate top-level projectCounts or totalCount)
	for c := 1; c <= 5; c++ {
		cid := fmt.Sprintf("tsk_arch_child_%d", c)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tasks (
				task_id, project_id, parent_task_id, title, goal,
				required_capability, required_resources, preferred_adapter,
				desired_config_json, state, created_by, version, created_at, updated_at, archived_at
			) VALUES (?, 'demo-app', 'tsk_arch_001', ?, 'child goal', 'primary-dev', '[]', 'agy', '{}', 'done', 'human', 1, ?, ?, ?)
		`, cid, fmt.Sprintf("Archived Child %d", c), baseTS, baseTS+999999, baseTS+999999); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// 1. Default archived=only&topLevelOnly=1&limit=200 returns 200 items, totalCount=215, exact projectCounts={demo-app:130, _scratch:85}
	resp, body := doJSONReq(t, http.MethodGet, ts.URL+"/api/tasks?archived=only&topLevelOnly=1&limit=200", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET archived tasks failed (%d): %s", resp.StatusCode, string(body))
	}
	var listRes protocol.ListTasksResponse
	if err := json.Unmarshal(body, &listRes); err != nil {
		t.Fatal(err)
	}
	if len(listRes.Tasks) != 200 {
		t.Fatalf("expected 200 tasks capped by limit, got %d", len(listRes.Tasks))
	}
	if listRes.TotalCount != 215 || listRes.ProjectCounts["demo-app"] != 130 || listRes.ProjectCounts[protocol.ScratchProjectID] != 85 {
		t.Fatalf("expected untruncated top-level counts total=215 demo=130 scratch=85, got total=%d counts=%+v", listRes.TotalCount, listRes.ProjectCounts)
	}
	if listRes.Tasks[0].TaskID != "tsk_arch_215" {
		t.Fatalf("expected newest archived_at first (tsk_arch_215), got %s", listRes.Tasks[0].TaskID)
	}

	// 2. Searching for the 215th-oldest task ("Needle Oldest 001", outside top-200!) MUST find it!
	resp, body = doJSONReq(t, http.MethodGet, ts.URL+"/api/tasks?archived=only&topLevelOnly=1&limit=200&q=Needle+Oldest+001", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search archived failed (%d): %s", resp.StatusCode, string(body))
	}
	listRes = protocol.ListTasksResponse{}
	_ = json.Unmarshal(body, &listRes)
	if len(listRes.Tasks) != 1 || listRes.Tasks[0].TaskID != "tsk_arch_001" {
		t.Fatalf("expected server-side search before LIMIT to find tsk_arch_001, got %+v", listRes.Tasks)
	}
	if listRes.TotalCount != 215 || listRes.ProjectCounts["demo-app"] != 130 {
		t.Fatalf("expected full projectCounts preserved during search, got total=%d counts=%+v", listRes.TotalCount, listRes.ProjectCounts)
	}

	// 3. Server-side sort=title puts "AAA Needle Oldest 001" first even though its archived_at is #215!
	resp, body = doJSONReq(t, http.MethodGet, ts.URL+"/api/tasks?archived=only&topLevelOnly=1&limit=10&sort=title", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sort=title failed (%d): %s", resp.StatusCode, string(body))
	}
	listRes = protocol.ListTasksResponse{}
	_ = json.Unmarshal(body, &listRes)
	if len(listRes.Tasks) != 10 || listRes.Tasks[0].TaskID != "tsk_arch_001" {
		t.Fatalf("expected sort=title to place tsk_arch_001 first, got %+v", listRes.Tasks)
	}

	// 4. Server-side sort=updated_at puts tsk_arch_001 first (highest updated_at)
	resp, body = doJSONReq(t, http.MethodGet, ts.URL+"/api/tasks?archived=only&topLevelOnly=1&limit=10&sort=updated_at", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sort=updated_at failed (%d): %s", resp.StatusCode, string(body))
	}
	listRes = protocol.ListTasksResponse{}
	_ = json.Unmarshal(body, &listRes)
	if len(listRes.Tasks) != 10 || listRes.Tasks[0].TaskID != "tsk_arch_001" {
		t.Fatalf("expected sort=updated_at to place tsk_arch_001 first, got %+v", listRes.Tasks)
	}

	// 5. Server-side projectId=_scratch returns all 85 scratch items while preserving full projectCounts
	resp, body = doJSONReq(t, http.MethodGet, ts.URL+"/api/tasks?archived=only&topLevelOnly=1&limit=200&projectId=_scratch", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("projectId filter failed (%d): %s", resp.StatusCode, string(body))
	}
	listRes = protocol.ListTasksResponse{}
	_ = json.Unmarshal(body, &listRes)
	if len(listRes.Tasks) != 85 || listRes.TotalCount != 215 || listRes.ProjectCounts["demo-app"] != 130 {
		t.Fatalf("expected 85 scratch tasks + full counts, got len=%d total=%d counts=%+v", len(listRes.Tasks), listRes.TotalCount, listRes.ProjectCounts)
	}
}
