package hub_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

func TestProjectWorktreePreference_ValidateBeforePersistAndProjectWide(t *testing.T) {
	_, ts, _, dataDir := newOnboardingTestServer(t, true)
	ctx := context.Background()
	st, err := store.Open(ctx, dataDir+"/orbit.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	devHdrs := map[string]string{
		"X-Orbit-Dev-Token": "test-dev-token-secret",
		"X-Orbit-Request":   "1",
	}

	// Create a project with 2 Git locations on 2 machines (both start as worktree)
	repoDir := t.TempDir()
	proj, _, err := st.CreateDynamicProject(ctx, "proj_wt", protocol.ProjectConfig{
		DisplayName:       "WT Project",
		RepoIdentity:      "github.com/example/wt",
		DefaultBranch:     "main",
		DefaultCapability: "primary-dev",
		DefaultAdapter:    protocol.AdapterAgy,
		Anchors: map[string]protocol.ProjectAnchorConfig{
			"primary-dev": {Machine: "primary-box", RepoPath: repoDir},
		},
	}, dataDir+"/git/proj_wt.git", "tester")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	partID := proj.Parts[0].PartID
	if _, _, _, err := st.CreateProjectLocation(ctx, "proj_wt", partID, protocol.CreateProjectLocationRequest{
		MachineID: "remote-mac",
		AbsPath:   "/srv/remote/wt",
		Isolation: protocol.IsolationWorktree,
	}, &protocol.LocationRuntimeState{Exists: true, IsGit: true}, "", "tester"); err != nil {
		t.Fatalf("create second location: %v", err)
	}

	// 1. Invalid task creation (ImportDirtyWorkspace on remote_ws fails validation) MUST NOT mutate project isolation!
	useWTFalse := false
	badResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/projects/proj_wt/tasks", protocol.CreateTaskRequest{
		Goal:                 "should fail validation",
		MachineID:            "remote-mac",
		Adapter:              protocol.AdapterAgy,
		UseWorktree:          &useWTFalse,
		ImportDirtyWorkspace: true,
	}, devHdrs)
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for remote dirty workspace import, got %d", badResp.StatusCode)
	}
	projAfterBad, err := st.GetProject(ctx, "proj_wt")
	if err != nil {
		t.Fatal(err)
	}
	for _, loc := range projAfterBad.Parts[0].Locations {
		if loc.Isolation != protocol.IsolationWorktree {
			t.Fatalf("validation-failed request mutated location %s isolation to %s", loc.LocationID, loc.Isolation)
		}
	}

	// 2. Valid task creation with useWorktree=false MUST update ALL Git locations on the project uniformly!
	okResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/projects/proj_wt/tasks", protocol.CreateTaskRequest{
		Goal:        "valid in-place task",
		MachineID:   "primary-box",
		Adapter:     protocol.AdapterAgy,
		UseWorktree: &useWTFalse,
	}, devHdrs)
	okResp.Body.Close()
	if okResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 for valid task, got %d", okResp.StatusCode)
	}
	projAfterOK, err := st.GetProject(ctx, "proj_wt")
	if err != nil {
		t.Fatal(err)
	}
	if len(projAfterOK.Parts[0].Locations) != 2 {
		t.Fatalf("expected 2 locations, got %d", len(projAfterOK.Parts[0].Locations))
	}
	for _, loc := range projAfterOK.Parts[0].Locations {
		if loc.Isolation != protocol.IsolationInPlace {
			t.Fatalf("expected all project Git locations updated to in_place, location %s (%s) still %s",
				loc.LocationID, loc.MachineID, loc.Isolation)
		}
	}
}

func TestForkNewSessionInheritsLocationIsolationAndRefsAndInPlaceRemoteNo426(t *testing.T) {
	_, ts, _, dataDir := newOnboardingTestServer(t, true)
	ctx := context.Background()
	st, err := store.Open(ctx, dataDir+"/orbit.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	devHdrs := map[string]string{
		"X-Orbit-Dev-Token": "test-dev-token-secret",
		"X-Orbit-Request":   "1",
	}

	// Create a reference conversation
	refResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/projects/_scratch/tasks", protocol.CreateTaskRequest{
		Title: "Ref Discussion",
		Goal:  "Important design decision ABC-999",
	}, devHdrs)
	var refCreated protocol.CreateTaskResponse
	_ = json.NewDecoder(refResp.Body).Decode(&refCreated)
	refResp.Body.Close()

	// Create a dynamic project and an in-place task
	proj, _, err := st.CreateDynamicProject(ctx, "proj_fork", protocol.ProjectConfig{
		DisplayName:       "Fork Proj",
		RepoIdentity:      "github.com/example/fork",
		DefaultBranch:     "main",
		DefaultCapability: "primary-dev",
		DefaultAdapter:    protocol.AdapterAgy,
		Anchors: map[string]protocol.ProjectAnchorConfig{
			"primary-dev": {Machine: "primary-box", RepoPath: t.TempDir()},
		},
	}, dataDir+"/git/proj_fork.git", "tester")
	if err != nil {
		t.Fatal(err)
	}
	useWTFalse := false
	parentResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/projects/proj_fork/tasks", protocol.CreateTaskRequest{
		Title:       "Parent Task",
		Goal:        "Initial parent goal",
		MachineID:   "primary-box",
		PartID:      proj.Parts[0].PartID,
		LocationID:  proj.Parts[0].Locations[0].LocationID,
		UseWorktree: &useWTFalse,
	}, devHdrs)
	var parentCreated protocol.CreateTaskResponse
	_ = json.NewDecoder(parentResp.Body).Decode(&parentCreated)
	parentResp.Body.Close()
	if parentResp.StatusCode != http.StatusCreated {
		t.Fatalf("create parent status=%d", parentResp.StatusCode)
	}

	// Fork while active attempt is running, passing ReferencedTaskIDs
	forkResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/tasks/"+parentCreated.Task.TaskID+"/followup", protocol.FollowupTaskRequest{
		Message:           "Explore alternate approach",
		Mode:              protocol.FollowupModeForkNewSession,
		ReferencedTaskIDs: []string{refCreated.Task.TaskID},
	}, devHdrs)
	var forked protocol.FollowupTaskResponse
	_ = json.NewDecoder(forkResp.Body).Decode(&forked)
	forkResp.Body.Close()
	if forkResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 for fork_new_session, got %d", forkResp.StatusCode)
	}
	if forked.Attempt == nil {
		t.Fatalf("expected forked attempt created")
	}
	if forked.Attempt.PartID != proj.Parts[0].PartID || forked.Attempt.LocationID != proj.Parts[0].Locations[0].LocationID {
		t.Fatalf("forked attempt lost part/location: got part=%q loc=%q", forked.Attempt.PartID, forked.Attempt.LocationID)
	}
	if forked.Attempt.Isolation != protocol.IsolationInPlace {
		t.Fatalf("forked attempt lost isolation: got %q want in_place", forked.Attempt.Isolation)
	}
	if len(forked.Attempt.RefTasks) != 1 || forked.Attempt.RefTasks[0].TaskID != refCreated.Task.TaskID {
		t.Fatalf("forked attempt lost refTasks: got %+v", forked.Attempt.RefTasks)
	}
}

func TestTasksServerSearchAndLongConversationRefExtraction(t *testing.T) {
	_, ts, _, dataDir := newOnboardingTestServer(t, true)
	ctx := context.Background()
	st, err := store.Open(ctx, dataDir+"/orbit.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	devHdrs := map[string]string{
		"X-Orbit-Dev-Token": "test-dev-token-secret",
		"X-Orbit-Request":   "1",
	}

	// Create tasks with distinct titles/goals and verify GET /api/tasks?q=...
	t1Resp := doOnbJSONReq(t, ts, http.MethodPost, "/api/projects/_scratch/tasks", protocol.CreateTaskRequest{
		Title: "Quarterly Billing Audit",
		Goal:  "Review invoice pipeline",
	}, devHdrs)
	var t1 protocol.CreateTaskResponse
	_ = json.NewDecoder(t1Resp.Body).Decode(&t1)
	t1Resp.Body.Close()

	t2Resp := doOnbJSONReq(t, ts, http.MethodPost, "/api/projects/_scratch/tasks", protocol.CreateTaskRequest{
		Title: "Graphics Shader Tuning",
		Goal:  "Optimize fragment pass",
	}, devHdrs)
	t2Resp.Body.Close()

	qResp := doOnbJSONReq(t, ts, http.MethodGet, "/api/tasks?q="+url.QueryEscape("billing"), nil, devHdrs)
	var qOut struct {
		Tasks []protocol.TaskSummary `json:"tasks"`
	}
	_ = json.NewDecoder(qResp.Body).Decode(&qOut)
	qResp.Body.Close()
	if qResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/tasks?q=billing status=%d", qResp.StatusCode)
	}
	if len(qOut.Tasks) != 1 || qOut.Tasks[0].TaskID != t1.Task.TaskID {
		t.Fatalf("expected 1 search match (%s), got %+v", t1.Task.TaskID, qOut.Tasks)
	}

	// Insert >600 raw_acp streaming events on t1's attempt so GetTaskTimeline(includeRaw=true)'s 500-event cap would truncate them,
	// then verify a referencing task still extracts BOTH the start AND the final conclusion marker!
	attID := t1.Attempt.AttemptID
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE attempts SET state = 'running' WHERE attempt_id = ?`, attID); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 560; i++ {
		txt := fmt.Sprintf("chunk-%03d ", i)
		if i == 1 {
			txt = "START_MARKER_ALPHA "
		}
		if i == 560 {
			txt = "FINAL_CONCLUSION_OMEGA"
		}
		payload, _ := json.Marshal(map[string]any{"text": txt})
		if _, err := st.WriteDB().ExecContext(ctx, `
			INSERT INTO events (event_id, stream_id, project_id, task_id, attempt_id, actor_kind, actor_id, kind, visibility, payload_json, created_at)
			VALUES (?, ?, ?, ?, ?, 'agent', 'agy', ?, 'raw_acp', ?, ?)
		`, fmt.Sprintf("evt_long_%04d", i), "task:"+t1.Task.TaskID, t1.Task.ProjectID, t1.Task.TaskID, attID, string(protocol.EventACPMessageBatch), string(payload), int64(1700000000000+i)); err != nil {
			t.Fatalf("insert raw event %d: %v", i, err)
		}
	}

	// Verify ListAttemptRawEvents pagination retrieves all 560 chunks
	page1, err := st.ListAttemptRawEvents(ctx, attID, 0, 500)
	if err != nil || len(page1) != 500 {
		t.Fatalf("expected 500 events on page 1, got %d err=%v", len(page1), err)
	}
	page2, err := st.ListAttemptRawEvents(ctx, attID, page1[len(page1)-1].Seq, 500)
	if err != nil || len(page2) != 60 {
		t.Fatalf("expected 60 events on page 2, got %d err=%v", len(page2), err)
	}
	var full strings.Builder
	for _, ev := range append(page1, page2...) {
		if txt, ok := ev.Payload["text"].(string); ok {
			full.WriteString(txt)
		}
	}
	if !strings.Contains(full.String(), "START_MARKER_ALPHA") || !strings.Contains(full.String(), "FINAL_CONCLUSION_OMEGA") {
		t.Fatalf("paginated raw events missing expected markers")
	}
}

func TestIdempotentCreateTaskDoesNotRollbackWorktreePrefAndNewLocationInherits(t *testing.T) {
	_, ts, _, dataDir := newOnboardingTestServer(t, true)
	ctx := context.Background()
	st, err := store.Open(ctx, dataDir+"/orbit.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	devHdrs := map[string]string{
		"X-Orbit-Dev-Token": "test-dev-token-secret",
		"X-Orbit-Request":   "1",
	}
	proj, _, err := st.CreateDynamicProject(ctx, "proj_idemp_wt", protocol.ProjectConfig{
		DisplayName:       "Idemp WT",
		RepoIdentity:      "github.com/example/idemp-wt",
		DefaultBranch:     "main",
		DefaultCapability: "primary-dev",
		DefaultAdapter:    protocol.AdapterAgy,
		Anchors: map[string]protocol.ProjectAnchorConfig{
			"primary-dev": {Machine: "primary-box", RepoPath: t.TempDir()},
		},
	}, dataDir+"/git/proj_idemp_wt.git", "tester")
	if err != nil {
		t.Fatal(err)
	}

	useWTTrue := true
	useWTFalse := false

	// 1. Task 1 created with idempotencyKey="k1" and useWorktree=true
	r1 := doOnbJSONReq(t, ts, http.MethodPost, "/api/projects/proj_idemp_wt/tasks", protocol.CreateTaskRequest{
		Goal:           "task 1",
		IdempotencyKey: "k1",
		UseWorktree:    &useWTTrue,
	}, devHdrs)
	r1.Body.Close()
	if r1.StatusCode != http.StatusCreated {
		t.Fatalf("task 1 status=%d", r1.StatusCode)
	}

	// 2. Task 2 switches project preference to in_place (useWorktree=false)
	r2 := doOnbJSONReq(t, ts, http.MethodPost, "/api/projects/proj_idemp_wt/tasks", protocol.CreateTaskRequest{
		Goal:        "task 2",
		UseWorktree: &useWTFalse,
	}, devHdrs)
	r2.Body.Close()
	if r2.StatusCode != http.StatusCreated {
		t.Fatalf("task 2 status=%d", r2.StatusCode)
	}

	// 3. Replaying Task 1 with idempotencyKey="k1" & useWorktree=true MUST NOT roll back project to worktree!
	r1Replay := doOnbJSONReq(t, ts, http.MethodPost, "/api/projects/proj_idemp_wt/tasks", protocol.CreateTaskRequest{
		Goal:           "task 1",
		IdempotencyKey: "k1",
		UseWorktree:    &useWTTrue,
	}, devHdrs)
	r1Replay.Body.Close()
	if r1Replay.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on idempotent replay, got %d", r1Replay.StatusCode)
	}

	afterReplay, err := st.GetProject(ctx, "proj_idemp_wt")
	if err != nil {
		t.Fatal(err)
	}
	if got := afterReplay.Parts[0].Locations[0].Isolation; got != protocol.IsolationInPlace {
		t.Fatalf("idempotent replay rolled back project isolation: got %q want in_place", got)
	}

	// 4. Adding a new Git location without explicit isolation MUST inherit project's current in_place preference!
	_, newLoc, _, err := st.CreateProjectLocation(ctx, "proj_idemp_wt", proj.Parts[0].PartID, protocol.CreateProjectLocationRequest{
		MachineID: "remote-mac",
		AbsPath:   "/srv/remote/idemp-wt",
	}, &protocol.LocationRuntimeState{Exists: true, IsGit: true}, "", "tester")
	if err != nil {
		t.Fatalf("create second location: %v", err)
	}
	if newLoc.Isolation != protocol.IsolationInPlace {
		t.Fatalf("expected new Git location to inherit in_place, got %q", newLoc.Isolation)
	}
}

func TestFollowupCapabilitySwitchPropagatesPartLocationIsolation(t *testing.T) {
	_, ts, _, dataDir := newOnboardingTestServer(t, true)
	ctx := context.Background()
	st, err := store.Open(ctx, dataDir+"/orbit.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	devHdrs := map[string]string{
		"X-Orbit-Dev-Token": "test-dev-token-secret",
		"X-Orbit-Request":   "1",
	}

	// Create project with primary-box (in_place) and remote-mac (worktree) on two distinct parts
	proj, _, err := st.CreateDynamicProject(ctx, "proj_cap_switch", protocol.ProjectConfig{
		DisplayName:       "Cap Switch",
		RepoIdentity:      "github.com/example/cap-switch",
		DefaultBranch:     "main",
		DefaultCapability: "primary-dev",
		DefaultAdapter:    protocol.AdapterAgy,
		Anchors: map[string]protocol.ProjectAnchorConfig{
			"primary-dev": {Machine: "primary-box", RepoPath: t.TempDir()},
		},
	}, dataDir+"/git/proj_cap_switch.git", "tester")
	if err != nil {
		t.Fatal(err)
	}
	part1 := proj.Parts[0]
	loc1 := part1.Locations[0]
	// Set primary-box location to in_place
	isoInPlace := protocol.IsolationInPlace
	if _, _, err := st.UpdateProjectLocation(ctx, "proj_cap_switch", part1.PartID, loc1.LocationID, protocol.UpdateProjectLocationRequest{
		Isolation: &isoInPlace,
	}, "tester"); err != nil {
		t.Fatal(err)
	}
	// Create a second part on remote-mac with worktree isolation
	proj2, part2, _, err := st.CreateProjectPart(ctx, "proj_cap_switch", "macos-ios", protocol.ProjectPartGit, "", "", "", nil, "tester")
	if err != nil {
		t.Fatal(err)
	}
	_ = proj2
	_, loc2, _, err := st.CreateProjectLocation(ctx, "proj_cap_switch", part2.PartID, protocol.CreateProjectLocationRequest{
		MachineID: "remote-mac",
		AbsPath:   "/Users/dev/cap-switch",
		Isolation: protocol.IsolationWorktree,
	}, &protocol.LocationRuntimeState{Exists: true, IsGit: true}, "", "tester")
	if err != nil {
		t.Fatal(err)
	}

	// Start Task 1 on primary-box (active attempt stays leased/busy)
	tResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/projects/proj_cap_switch/tasks", protocol.CreateTaskRequest{
		Goal:       "initial work on linux",
		MachineID:  "primary-box",
		PartID:     part1.PartID,
		LocationID: loc1.LocationID,
	}, devHdrs)
	var created protocol.CreateTaskResponse
	_ = json.NewDecoder(tResp.Body).Decode(&created)
	tResp.Body.Close()
	if tResp.StatusCode != http.StatusCreated {
		t.Fatalf("create task status=%d", tResp.StatusCode)
	}

	// 1. Queued follow-up switching capability to macos-env (remote-mac)
	qResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/tasks/"+created.Task.TaskID+"/followup", protocol.FollowupTaskRequest{
		Message:            "run macOS check",
		Mode:               protocol.FollowupModeQueue,
		RequiredCapability: "macos-env",
	}, devHdrs)
	var qOut protocol.FollowupTaskResponse
	_ = json.NewDecoder(qResp.Body).Decode(&qOut)
	qResp.Body.Close()
	if qResp.StatusCode != http.StatusAccepted || qOut.PendingInput == nil {
		t.Fatalf("expected 202 queued followup, got status=%d resp=%+v", qResp.StatusCode, qOut)
	}

	// Mark attempt 1 succeeded and consume queued input -> MUST inherit remote-mac's part2/loc2/worktree!
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE attempts SET state = 'succeeded', finished_at = 1700000000000 WHERE attempt_id = ?`, created.Attempt.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE tasks SET active_attempt_id = NULL, state = 'in_review' WHERE task_id = ?`, created.Task.TaskID); err != nil {
		t.Fatal(err)
	}
	_, att2, _, err := st.ConsumeNextPendingInput(ctx, store.ConsumeNextPendingInputParams{
		TaskID:    created.Task.TaskID,
		MachineID: "primary-box",
		Adapter:   protocol.AdapterAgy,
	})
	if err != nil || att2 == nil {
		t.Fatalf("ConsumeNextPendingInput failed: att=%v err=%v", att2, err)
	}
	if att2.MachineID != "remote-mac" || att2.PartID != part2.PartID || att2.LocationID != loc2.LocationID || att2.Isolation != protocol.IsolationWorktree {
		t.Fatalf("queued capability switch lost route: machine=%q part=%q loc=%q iso=%q (want remote-mac/%s/%s/worktree)",
			att2.MachineID, att2.PartID, att2.LocationID, att2.Isolation, part2.PartID, loc2.LocationID)
	}

	// 2. Idle follow-up switching capability back to primary-dev (primary-box)
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE attempts SET state = 'succeeded', finished_at = 1700000001000 WHERE attempt_id = ?`, att2.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE tasks SET active_attempt_id = NULL, state = 'in_review' WHERE task_id = ?`, created.Task.TaskID); err != nil {
		t.Fatal(err)
	}
	immResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/tasks/"+created.Task.TaskID+"/followup", protocol.FollowupTaskRequest{
		Message:            "back to linux",
		Mode:               protocol.FollowupModeQueue,
		RequiredCapability: "primary-dev",
	}, devHdrs)
	var immOut protocol.FollowupTaskResponse
	_ = json.NewDecoder(immResp.Body).Decode(&immOut)
	immResp.Body.Close()
	if immResp.StatusCode != http.StatusCreated || immOut.Attempt == nil {
		t.Fatalf("expected 201 idle followup, got status=%d resp=%+v", immResp.StatusCode, immOut)
	}
	if immOut.Attempt.MachineID != "primary-box" || immOut.Attempt.PartID != part1.PartID || immOut.Attempt.LocationID != loc1.LocationID || immOut.Attempt.Isolation != protocol.IsolationInPlace {
		t.Fatalf("idle capability switch lost route: machine=%q part=%q loc=%q iso=%q (want primary-box/%s/%s/in_place)",
			immOut.Attempt.MachineID, immOut.Attempt.PartID, immOut.Attempt.LocationID, immOut.Attempt.Isolation, part1.PartID, loc1.LocationID)
	}
}

func pairTestRunner(t *testing.T, ts *httptest.Server, devHdrs map[string]string, proposedID string) (string, string) {
	t.Helper()
	codeResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/pairing/codes", protocol.CreatePairingCodeRequest{
		Scope: protocol.PairingScopeRunner,
	}, devHdrs)
	var created protocol.CreatePairingCodeResponse
	if err := json.NewDecoder(codeResp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	codeResp.Body.Close()
	claimResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/pairing/claim", protocol.ClaimPairingRequest{
		Code:       created.Code,
		WantRunner: true,
		Machine: &protocol.ClaimMachineInfo{
			ProposedID:  proposedID,
			DisplayName: proposedID,
			OS:          "linux",
			Arch:        "amd64",
		},
	}, map[string]string{"X-Orbit-Request": "1"})
	var claimed protocol.ClaimPairingResponse
	if err := json.NewDecoder(claimResp.Body).Decode(&claimed); err != nil {
		t.Fatal(err)
	}
	claimResp.Body.Close()
	if claimResp.StatusCode != http.StatusOK || claimed.RunnerToken == "" || claimed.MachineID == "" {
		t.Fatalf("pairTestRunner failed: status=%d resp=%+v", claimResp.StatusCode, claimed)
	}
	return claimed.MachineID, claimed.RunnerToken
}

func TestVirtualProjectAutoPicksReadyReportedAdapter(t *testing.T) {
	_, ts, _, _ := newOnboardingTestServer(t, true)
	devHdrs := map[string]string{
		"X-Orbit-Dev-Token": "test-dev-token-secret",
		"X-Orbit-Request":   "1",
	}

	machID, runnerTok := pairTestRunner(t, ts, devHdrs, "claude-only-node")

	// Connect a runner advertising ONLY claude (no agy)
	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	rc, _, err := websocket.Dial(dialCtx, wsBase+"/ws/runner", &websocket.DialOptions{
		HTTPHeader: http.Header{
			"X-Orbit-Machine-Id": []string{machID},
			"Authorization":      []string{"Bearer " + runnerTok},
		},
	})
	if err != nil {
		t.Fatalf("dial /ws/runner failed: %v", err)
	}
	defer rc.CloseNow()

	helloBytes, _ := json.Marshal(protocol.RunnerHelloFrame{
		Type:             protocol.RunnerMsgHello,
		ProtocolVersion:  1,
		MachineID:        machID,
		RunnerInstanceID: "run_claude_only",
		Adapters:         []protocol.Adapter{protocol.AdapterClaude},
		AgentProbes: []protocol.AgentProbe{
			{Adapter: protocol.AdapterClaude, State: protocol.AgentStateReady, Enabled: true},
		},
	})
	if err := rc.Write(dialCtx, websocket.MessageText, helloBytes); err != nil {
		t.Fatalf("send runner hello: %v", err)
	}
	// Wait for hub.welcome so runner registration is complete
	if _, _, err := rc.Read(dialCtx); err != nil {
		t.Fatalf("read hub.welcome: %v", err)
	}

	// POST /api/projects/_pa/tasks WITHOUT adapter MUST auto-pick claude instead of failing with agy!
	resp := doOnbJSONReq(t, ts, http.MethodPost, "/api/projects/_pa/tasks", protocol.CreateTaskRequest{
		Goal:      "summarize workspace",
		MachineID: machID,
	}, devHdrs)
	var created protocol.CreateTaskResponse
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || created.Attempt == nil {
		t.Fatalf("expected 201 for _pa auto-picked claude, got status=%d resp=%+v", resp.StatusCode, created)
	}
	if created.Attempt.Adapter != protocol.AdapterClaude {
		t.Fatalf("expected auto-picked adapter claude, got %q", created.Attempt.Adapter)
	}
}

func TestLongConversationRefExtraction_NotSwallowedByNonMessageEvents(t *testing.T) {
	_, ts, _, dataDir := newOnboardingTestServer(t, true)
	ctx := context.Background()
	st, err := store.Open(ctx, dataDir+"/orbit.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	devHdrs := map[string]string{
		"X-Orbit-Dev-Token": "test-dev-token-secret",
		"X-Orbit-Request":   "1",
	}

	// Create referenced conversation
	refResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/projects/_scratch/tasks", protocol.CreateTaskRequest{
		Title: "Heavy Tool Run",
		Goal:  "Audit 5000 tool calls",
	}, devHdrs)
	var refCreated protocol.CreateTaskResponse
	_ = json.NewDecoder(refResp.Body).Decode(&refCreated)
	refResp.Body.Close()
	attID := refCreated.Attempt.AttemptID

	// Bulk-insert 5200 non-message (acp.tool_call) raw events BEFORE 1 acp.message_batch reply
	tx, err := st.WriteDB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO events (event_id, stream_id, project_id, task_id, attempt_id, actor_kind, actor_id, kind, visibility, payload_json, created_at)
		VALUES (?, ?, ?, ?, ?, 'agent', 'agy', ?, 'raw_acp', ?, ?)
	`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5200; i++ {
		if _, err := stmt.ExecContext(ctx, fmt.Sprintf("evt_tc_%05d", i), "task:"+refCreated.Task.TaskID, refCreated.Task.ProjectID, refCreated.Task.TaskID, attID, string(protocol.EventACPToolCall), `{"name":"read_file"}`, int64(1700000000000+i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := stmt.ExecContext(ctx, "evt_msg_final", "task:"+refCreated.Task.TaskID, refCreated.Task.ProjectID, refCreated.Task.TaskID, attID, string(protocol.EventACPMessageBatch), `{"text":"FINAL_ANSWER_AFTER_5200_TOOLS"}`, int64(1700000010000)); err != nil {
		t.Fatal(err)
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Pair & connect runner to capture dispatched prompt of a new task referencing Heavy Tool Run
	machID, runnerTok := pairTestRunner(t, ts, devHdrs, "ref-capture-node")
	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	rc, _, err := websocket.Dial(dialCtx, wsBase+"/ws/runner", &websocket.DialOptions{
		HTTPHeader: http.Header{
			"X-Orbit-Machine-Id": []string{machID},
			"Authorization":      []string{"Bearer " + runnerTok},
		},
	})
	if err != nil {
		t.Fatalf("dial /ws/runner: %v", err)
	}
	defer rc.CloseNow()
	helloBytes, _ := json.Marshal(protocol.RunnerHelloFrame{
		Type:             protocol.RunnerMsgHello,
		ProtocolVersion:  1,
		MachineID:        machID,
		RunnerInstanceID: "run_ref_cap",
		Adapters:         []protocol.Adapter{protocol.AdapterAgy},
	})
	_ = rc.Write(dialCtx, websocket.MessageText, helloBytes)
	_, _, _ = rc.Read(dialCtx) // hub.welcome

	// Create task referencing refCreated
	askResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/projects/_scratch/tasks", protocol.CreateTaskRequest{
		Goal:              "What was the final answer?",
		MachineID:         machID,
		ReferencedTaskIDs: []string{refCreated.Task.TaskID},
	}, devHdrs)
	askResp.Body.Close()
	if askResp.StatusCode != http.StatusCreated {
		t.Fatalf("create referencing task status=%d", askResp.StatusCode)
	}

	// Read attempt.dispatch frame and verify the assistant reply was NOT swallowed by the 5200 tool calls!
	_, frameBytes, err := rc.Read(dialCtx)
	if err != nil {
		t.Fatalf("read dispatch frame: %v", err)
	}
	var disp protocol.AttemptDispatchFrame
	if err := json.Unmarshal(frameBytes, &disp); err != nil {
		t.Fatalf("decode dispatch: %v", err)
	}
	if !strings.Contains(disp.Prompt, "FINAL_ANSWER_AFTER_5200_TOOLS") {
		t.Fatalf("expected referenced prompt to contain assistant reply after 5200 tool calls, got:\n%s", disp.Prompt)
	}
}

func TestExplicitFullPermissionNotInheritedAcrossFollowup(t *testing.T) {
	_, ts, _, dataDir := newOnboardingTestServer(t, true)
	ctx := context.Background()
	st, err := store.Open(ctx, dataDir+"/orbit.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	devHdrs := map[string]string{
		"X-Orbit-Dev-Token": "test-dev-token-secret",
		"X-Orbit-Request":   "1",
	}

	machID, runnerTok := pairTestRunner(t, ts, devHdrs, "perm-node")
	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	rc, _, err := websocket.Dial(dialCtx, wsBase+"/ws/runner", &websocket.DialOptions{
		HTTPHeader: http.Header{
			"X-Orbit-Machine-Id": []string{machID},
			"Authorization":      []string{"Bearer " + runnerTok},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.CloseNow()
	helloBytes, _ := json.Marshal(protocol.RunnerHelloFrame{
		Type:             protocol.RunnerMsgHello,
		ProtocolVersion:  1,
		MachineID:        machID,
		RunnerInstanceID: "run_perm",
		Adapters:         []protocol.Adapter{protocol.AdapterAgy},
	})
	_ = rc.Write(dialCtx, websocket.MessageText, helloBytes)
	_, _, _ = rc.Read(dialCtx) // hub.welcome

	// Turn 1: created with ExplicitFullPermission=true and mode=bypassPermissions
	cResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/projects/_scratch/tasks", protocol.CreateTaskRequest{
		Goal:                   "turn 1 full",
		MachineID:              machID,
		Config:                 map[string]string{"mode": "bypassPermissions"},
		ExplicitFullPermission: true,
	}, devHdrs)
	var created protocol.CreateTaskResponse
	_ = json.NewDecoder(cResp.Body).Decode(&created)
	cResp.Body.Close()
	if cResp.StatusCode != http.StatusCreated {
		t.Fatalf("create task status=%d", cResp.StatusCode)
	}
	_, raw1, err := rc.Read(dialCtx)
	if err != nil {
		t.Fatal(err)
	}
	var disp1 protocol.AttemptDispatchFrame
	_ = json.Unmarshal(raw1, &disp1)
	if !disp1.ExplicitFullPermission {
		t.Fatalf("expected turn 1 dispatch ExplicitFullPermission=true")
	}

	// Mark turn 1 done, then follow up WITHOUT ExplicitFullPermission (even though task DB still has desired_config_json mode=bypassPermissions!)
	_, _ = st.WriteDB().ExecContext(ctx, `UPDATE attempts SET state = 'succeeded', finished_at = 1700000000000 WHERE attempt_id = ?`, created.Attempt.AttemptID)
	_, _ = st.WriteDB().ExecContext(ctx, `UPDATE tasks SET active_attempt_id = NULL, state = 'in_review' WHERE task_id = ?`, created.Task.TaskID)

	fResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/tasks/"+created.Task.TaskID+"/followup", protocol.FollowupTaskRequest{
		Message: "turn 2 without explicit full",
		Mode:    protocol.FollowupModeQueue,
	}, devHdrs)
	fResp.Body.Close()
	if fResp.StatusCode != http.StatusCreated {
		t.Fatalf("followup status=%d", fResp.StatusCode)
	}
	_, raw2, err := rc.Read(dialCtx)
	if err != nil {
		t.Fatal(err)
	}
	var disp2 protocol.AttemptDispatchFrame
	_ = json.Unmarshal(raw2, &disp2)
	if disp2.ExplicitFullPermission {
		t.Fatalf("turn 2 MUST NOT inherit ExplicitFullPermission=true from turn 1!")
	}
}

func TestYAMLSyncNewAnchorInheritsProjectWorktreePreference(t *testing.T) {
	_, ts, _, dataDir := newOnboardingTestServer(t, true)
	ctx := context.Background()
	st, err := store.Open(ctx, dataDir+"/orbit.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	devHdrs := map[string]string{
		"X-Orbit-Dev-Token": "test-dev-token-secret",
		"X-Orbit-Request":   "1",
	}

	projCfg := protocol.ProjectConfig{
		DisplayName:       "YAML Sync Proj",
		RepoIdentity:      "github.com/example/yaml-sync",
		DefaultBranch:     "main",
		DefaultCapability: "primary-dev",
		DefaultAdapter:    protocol.AdapterAgy,
		Anchors: map[string]protocol.ProjectAnchorConfig{
			"primary-dev": {Machine: "primary-box", RepoPath: t.TempDir()},
		},
	}
	if _, _, err := st.CreateDynamicProject(ctx, "proj_yaml_iso", projCfg, dataDir+"/git/proj_yaml_iso.git", "tester"); err != nil {
		t.Fatal(err)
	}
	// Turn off Worktree on the project (in_place)
	useWTFalse := false
	resp := doOnbJSONReq(t, ts, http.MethodPost, "/api/projects/proj_yaml_iso/tasks", protocol.CreateTaskRequest{
		Goal:        "set project to in_place",
		UseWorktree: &useWTFalse,
	}, devHdrs)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create task status=%d", resp.StatusCode)
	}

	// Now YAML registry sync adds a second Git anchor on remote-mac
	projCfg.Anchors["macos-env"] = protocol.ProjectAnchorConfig{
		Machine:  "remote-mac",
		RepoPath: "/Users/dev/yaml-sync",
	}
	if err := st.SyncRegistry(ctx, dataDir, protocol.ProjectsConfig{
		SchemaVersion: 1,
		Projects:      map[string]protocol.ProjectConfig{"proj_yaml_iso": projCfg},
	}, protocol.MachinesConfig{SchemaVersion: 1}); err != nil {
		t.Fatalf("SyncRegistry: %v", err)
	}
	proj, err := st.GetProject(ctx, "proj_yaml_iso")
	if err != nil {
		t.Fatal(err)
	}
	foundRemote := false
	for _, pt := range proj.Parts {
		for _, loc := range pt.Locations {
			if loc.MachineID == "remote-mac" {
				foundRemote = true
				if loc.Isolation != protocol.IsolationInPlace {
					t.Fatalf("YAML-synced new Git anchor location got isolation=%q, want inherited in_place", loc.Isolation)
				}
			}
		}
	}
	if !foundRemote {
		t.Fatalf("expected remote-mac location created by YAML sync")
	}
}

func TestCapabilitySwitchWithExplicitRepoPathDoesNotBorrowUnrelatedLocation(t *testing.T) {
	_, ts, _, dataDir := newOnboardingTestServer(t, true)
	ctx := context.Background()
	st, err := store.Open(ctx, dataDir+"/orbit.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	devHdrs := map[string]string{
		"X-Orbit-Dev-Token": "test-dev-token-secret",
		"X-Orbit-Request":   "1",
	}

	// Project with primary-box worktree location, AND an unrelated plain-folder (in_place) location on remote-mac,
	// while its `macos-env` anchor points to a DIFFERENT RepoPath `/Users/dev/real-git-repo`!
	proj, _, err := st.CreateDynamicProject(ctx, "proj_no_borrow", protocol.ProjectConfig{
		DisplayName:       "No Borrow Proj",
		RepoIdentity:      "github.com/example/no-borrow",
		DefaultBranch:     "main",
		DefaultCapability: "primary-dev",
		DefaultAdapter:    protocol.AdapterAgy,
		Anchors: map[string]protocol.ProjectAnchorConfig{
			"primary-dev": {Machine: "primary-box", RepoPath: t.TempDir()},
		},
	}, dataDir+"/git/proj_no_borrow.git", "tester")
	if err != nil {
		t.Fatal(err)
	}
	// Add an unrelated plain folder part on remote-mac (which forces in_place!)
	_, folderPart, _, err := st.CreateProjectPart(ctx, "proj_no_borrow", "docs-folder", protocol.ProjectPartFolder, "", "", "", nil, "tester")
	if err != nil {
		t.Fatal(err)
	}
	_, folderLoc, _, err := st.CreateProjectLocation(ctx, "proj_no_borrow", folderPart.PartID, protocol.CreateProjectLocationRequest{
		MachineID: "remote-mac",
		AbsPath:   "/Users/dev/unrelated-docs",
	}, &protocol.LocationRuntimeState{Exists: true, IsGit: false}, "", "tester")
	if err != nil {
		t.Fatal(err)
	}
	// Inject a macos-env anchor in projects.config_json pointing to `/Users/dev/real-git-repo` (not bound in project_locations)
	var rawCfg string
	if err := st.WriteDB().QueryRowContext(ctx, `SELECT config_json FROM projects WHERE project_id = 'proj_no_borrow'`).Scan(&rawCfg); err != nil {
		t.Fatal(err)
	}
	var cfg protocol.ProjectConfig
	_ = json.Unmarshal([]byte(rawCfg), &cfg)
	cfg.Anchors["macos-env"] = protocol.ProjectAnchorConfig{
		Machine:  "remote-mac",
		RepoPath: "/Users/dev/real-git-repo",
	}
	newCfgBytes, _ := json.Marshal(cfg)
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE projects SET config_json = ? WHERE project_id = 'proj_no_borrow'`, string(newCfgBytes)); err != nil {
		t.Fatal(err)
	}

	// Create task on primary-box, complete it, then follow-up with requiredCapability="macos-env"
	r1 := doOnbJSONReq(t, ts, http.MethodPost, "/api/projects/proj_no_borrow/tasks", protocol.CreateTaskRequest{
		Goal:       "initial on linux",
		MachineID:  "primary-box",
		PartID:     proj.Parts[0].PartID,
		LocationID: proj.Parts[0].Locations[0].LocationID,
	}, devHdrs)
	var created protocol.CreateTaskResponse
	_ = json.NewDecoder(r1.Body).Decode(&created)
	r1.Body.Close()
	_, _ = st.WriteDB().ExecContext(ctx, `UPDATE attempts SET state = 'succeeded', finished_at = 1700000000000 WHERE attempt_id = ?`, created.Attempt.AttemptID)
	_, _ = st.WriteDB().ExecContext(ctx, `UPDATE tasks SET active_attempt_id = NULL, state = 'in_review' WHERE task_id = ?`, created.Task.TaskID)

	fResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/tasks/"+created.Task.TaskID+"/followup", protocol.FollowupTaskRequest{
		Message:            "switch to macos-env",
		Mode:               protocol.FollowupModeQueue,
		RequiredCapability: "macos-env",
	}, devHdrs)
	var fOut protocol.FollowupTaskResponse
	_ = json.NewDecoder(fResp.Body).Decode(&fOut)
	fResp.Body.Close()
	if fResp.StatusCode != http.StatusCreated || fOut.Attempt == nil {
		t.Fatalf("followup status=%d resp=%+v", fResp.StatusCode, fOut)
	}
	if fOut.Attempt.LocationID == folderLoc.LocationID || fOut.Attempt.LocationID != "" {
		t.Fatalf("MUST NOT borrow unrelated location %s, got locationId=%q", folderLoc.LocationID, fOut.Attempt.LocationID)
	}
	if fOut.Attempt.Isolation != protocol.IsolationWorktree {
		t.Fatalf("expected project Git isolation worktree (NOT unrelated folder in_place), got %q", fOut.Attempt.Isolation)
	}
}

func TestGetQueueInputByIDReturnsDispatchedAttemptID(t *testing.T) {
	_, ts, _, dataDir := newOnboardingTestServer(t, true)
	ctx := context.Background()
	st, err := store.Open(ctx, dataDir+"/orbit.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	devHdrs := map[string]string{
		"X-Orbit-Dev-Token": "test-dev-token-secret",
		"X-Orbit-Request":   "1",
	}

	cResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/projects/_pa/tasks", protocol.CreateTaskRequest{
		Goal: "initial pa turn",
	}, devHdrs)
	var created protocol.CreateTaskResponse
	_ = json.NewDecoder(cResp.Body).Decode(&created)
	cResp.Body.Close()

	// Queue a follow-up while attempt 1 is active
	qResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/tasks/"+created.Task.TaskID+"/followup", protocol.FollowupTaskRequest{
		Message: "queued pa msg",
		Mode:    protocol.FollowupModeQueue,
	}, devHdrs)
	var qOut protocol.FollowupTaskResponse
	_ = json.NewDecoder(qResp.Body).Decode(&qOut)
	qResp.Body.Close()
	if qResp.StatusCode != http.StatusAccepted || qOut.PendingInput == nil {
		t.Fatalf("expected queued input, got status=%d", qResp.StatusCode)
	}
	inputID := qOut.PendingInput.InputID

	// GET /api/tasks/{taskId}/queue/{inputId} returns state=queued
	g1 := doOnbJSONReq(t, ts, http.MethodGet, "/api/tasks/"+created.Task.TaskID+"/queue/"+inputID, nil, devHdrs)
	var pi1 protocol.PendingInput
	_ = json.NewDecoder(g1.Body).Decode(&pi1)
	g1.Body.Close()
	if g1.StatusCode != http.StatusOK || pi1.State != protocol.PendingInputQueued || pi1.DispatchedAttemptID != "" {
		t.Fatalf("unexpected queued get: status=%d pi=%+v", g1.StatusCode, pi1)
	}

	// Finish attempt 1 and consume pending input
	_, _ = st.WriteDB().ExecContext(ctx, `UPDATE attempts SET state = 'succeeded', finished_at = 1700000000000 WHERE attempt_id = ?`, created.Attempt.AttemptID)
	_, _ = st.WriteDB().ExecContext(ctx, `UPDATE tasks SET active_attempt_id = NULL, state = 'in_review' WHERE task_id = ?`, created.Task.TaskID)
	_, att2, _, err := st.ConsumeNextPendingInput(ctx, store.ConsumeNextPendingInputParams{
		TaskID:    created.Task.TaskID,
		MachineID: "primary-box",
		Adapter:   protocol.AdapterAgy,
	})
	if err != nil || att2 == nil {
		t.Fatalf("consume failed: %v", err)
	}

	// GET /api/tasks/{taskId}/queue/{inputId} now returns state=dispatched and exact dispatchedAttemptId!
	g2 := doOnbJSONReq(t, ts, http.MethodGet, "/api/tasks/"+created.Task.TaskID+"/queue/"+inputID, nil, devHdrs)
	var pi2 protocol.PendingInput
	_ = json.NewDecoder(g2.Body).Decode(&pi2)
	g2.Body.Close()
	if g2.StatusCode != http.StatusOK || pi2.State != protocol.PendingInputDispatched || pi2.DispatchedAttemptID != att2.AttemptID {
		t.Fatalf("expected dispatched with attemptId=%s, got status=%d pi=%+v", att2.AttemptID, g2.StatusCode, pi2)
	}

	// Unknown inputId returns 404
	g404 := doOnbJSONReq(t, ts, http.MethodGet, "/api/tasks/"+created.Task.TaskID+"/queue/inp_nonexistent", nil, devHdrs)
	g404.Body.Close()
	if g404.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown inputId, got %d", g404.StatusCode)
	}
}

func TestSameMachineCapabilitySwitchDoesNotInheritFolderPartID(t *testing.T) {
	_, ts, _, dataDir := newOnboardingTestServer(t, true)
	ctx := context.Background()
	st, err := store.Open(ctx, dataDir+"/orbit.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	devHdrs := map[string]string{
		"X-Orbit-Dev-Token": "test-dev-token-secret",
		"X-Orbit-Request":   "1",
	}

	// Pair a real runner on same-box so we can inspect BOTH AttemptView AND dispatched AttemptDispatchFrame!
	machID, runnerTok := pairTestRunner(t, ts, devHdrs, "same-box")
	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	rc, _, err := websocket.Dial(dialCtx, wsBase+"/ws/runner", &websocket.DialOptions{
		HTTPHeader: http.Header{
			"X-Orbit-Machine-Id": []string{machID},
			"Authorization":      []string{"Bearer " + runnerTok},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.CloseNow()
	helloBytes, _ := json.Marshal(protocol.RunnerHelloFrame{
		Type:             protocol.RunnerMsgHello,
		ProtocolVersion:  1,
		MachineID:        machID,
		RunnerInstanceID: "run_same_box",
		Adapters:         []protocol.Adapter{protocol.AdapterAgy},
		Capabilities:     []string{protocol.RunnerCapBundleV1},
	})
	_ = rc.Write(dialCtx, websocket.MessageText, helloBytes)
	_, _, _ = rc.Read(dialCtx) // hub.welcome

	// Project has:
	// - part 1 (git, worktree) on primary-box
	// - part 2 (plain folder, in_place) bound on same-box at /srv/same-box/notes
	// - anchor `alt-git` ALSO on same-box pointing to /srv/same-box/real-git-repo (NOT bound in project_locations)
	if _, _, err := st.CreateDynamicProject(ctx, "proj_same_mach", protocol.ProjectConfig{
		DisplayName:       "Same Machine Switch",
		RepoIdentity:      "github.com/example/same-mach",
		DefaultBranch:     "main",
		DefaultCapability: "primary-dev",
		DefaultAdapter:    protocol.AdapterAgy,
		Anchors: map[string]protocol.ProjectAnchorConfig{
			"primary-dev": {Machine: "primary-box", RepoPath: t.TempDir()},
		},
	}, dataDir+"/git/proj_same_mach.git", "tester"); err != nil {
		t.Fatal(err)
	}
	_, folderPart, _, err := st.CreateProjectPart(ctx, "proj_same_mach", "same-box-notes", protocol.ProjectPartFolder, "", "", "", nil, "tester")
	if err != nil {
		t.Fatal(err)
	}
	_, folderLoc, _, err := st.CreateProjectLocation(ctx, "proj_same_mach", folderPart.PartID, protocol.CreateProjectLocationRequest{
		MachineID: machID,
		AbsPath:   "/srv/same-box/notes",
	}, &protocol.LocationRuntimeState{Exists: true, IsGit: false}, "", "tester")
	if err != nil {
		t.Fatal(err)
	}

	var rawCfg string
	if err := st.WriteDB().QueryRowContext(ctx, `SELECT config_json FROM projects WHERE project_id = 'proj_same_mach'`).Scan(&rawCfg); err != nil {
		t.Fatal(err)
	}
	var cfg protocol.ProjectConfig
	_ = json.Unmarshal([]byte(rawCfg), &cfg)
	cfg.Anchors["alt-git"] = protocol.ProjectAnchorConfig{
		Machine:  machID,
		RepoPath: "/srv/same-box/real-git-repo",
	}
	newCfgBytes, _ := json.Marshal(cfg)
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE projects SET config_json = ? WHERE project_id = 'proj_same_mach'`, string(newCfgBytes)); err != nil {
		t.Fatal(err)
	}

	// Turn 1: starts on same-box's plain-folder part (in_place)
	r1 := doOnbJSONReq(t, ts, http.MethodPost, "/api/projects/proj_same_mach/tasks", protocol.CreateTaskRequest{
		Goal:       "turn 1 in folder",
		MachineID:  machID,
		PartID:     folderPart.PartID,
		LocationID: folderLoc.LocationID,
	}, devHdrs)
	var created protocol.CreateTaskResponse
	_ = json.NewDecoder(r1.Body).Decode(&created)
	r1.Body.Close()
	if r1.StatusCode != http.StatusCreated {
		t.Fatalf("create turn 1 status=%d", r1.StatusCode)
	}
	_, _, _ = rc.Read(dialCtx) // drain turn 1 dispatch

	// Complete turn 1, then follow up switching capability to `alt-git` on the SAME machine!
	_, _ = st.WriteDB().ExecContext(ctx, `UPDATE attempts SET state = 'succeeded', finished_at = 1700000000000 WHERE attempt_id = ?`, created.Attempt.AttemptID)
	_, _ = st.WriteDB().ExecContext(ctx, `UPDATE tasks SET active_attempt_id = NULL, state = 'in_review' WHERE task_id = ?`, created.Task.TaskID)

	fResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/tasks/"+created.Task.TaskID+"/followup", protocol.FollowupTaskRequest{
		Message:            "turn 2 on alt-git",
		Mode:               protocol.FollowupModeQueue,
		RequiredCapability: "alt-git",
	}, devHdrs)
	var fOut protocol.FollowupTaskResponse
	_ = json.NewDecoder(fResp.Body).Decode(&fOut)
	fResp.Body.Close()
	if fResp.StatusCode != http.StatusCreated || fOut.Attempt == nil {
		t.Fatalf("followup status=%d resp=%+v", fResp.StatusCode, fOut)
	}
	if fOut.Attempt.PartID != "" || fOut.Attempt.LocationID != "" {
		t.Fatalf("same-machine explicit capability switch inherited stale folder part/location! part=%q loc=%q", fOut.Attempt.PartID, fOut.Attempt.LocationID)
	}
	if fOut.Attempt.Isolation != protocol.IsolationWorktree {
		t.Fatalf("expected isolation=worktree, got %q", fOut.Attempt.Isolation)
	}

	_, rawDisp, err := rc.Read(dialCtx)
	if err != nil {
		t.Fatalf("read turn 2 dispatch: %v", err)
	}
	var disp protocol.AttemptDispatchFrame
	_ = json.Unmarshal(rawDisp, &disp)
	if disp.IsPlainFolder || disp.Isolation != protocol.IsolationWorktree {
		t.Fatalf("expected dispatched frame IsPlainFolder=false Isolation=worktree, got isPlain=%v iso=%q", disp.IsPlainFolder, disp.Isolation)
	}
}
