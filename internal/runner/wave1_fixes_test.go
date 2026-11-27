package runner_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/runner"
)

func TestRunner_PlainFolderAndInPlaceRunConcurrentlyWithoutLocationLock(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	sharedDir := filepath.Join(tmp, "plain-folder")
	if err := os.MkdirAll(sharedDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var active atomic.Int32
	bothInside := make(chan struct{})
	var once sync.Once

	drv := NewFakeAcpDriver()
	drv.OnPrompt = func(pctx context.Context, d *FakeAcpDriver, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
		if active.Add(1) == 2 {
			once.Do(func() { close(bothInside) })
		}
		select {
		case <-bothInside:
		case <-time.After(4 * time.Second):
			t.Errorf("second attempt did not enter OnPrompt concurrently (location lock still blocking!)")
		}
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	r, err := runner.New(ctx, runner.Config{
		MachineID: "hub-node",
		DataDir:   tmp,
		Driver:    drv,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(context.Background()) }()

	for i, id := range []string{"att_pf_1", "att_pf_2"} {
		r.HandleDispatch(protocol.AttemptDispatchFrame{
			AttemptID:     id,
			TaskID:        "tsk_pf_" + id,
			ProjectID:     "proj_folder",
			LeaseToken:    "lease_" + id,
			Adapter:       protocol.AdapterAgy,
			WBSessionID:   "ses_pf_" + id,
			LocationID:    "loc_shared",
			Isolation:     protocol.IsolationInPlace,
			IsPlainFolder: true,
			RepoPath:      sharedDir,
			WorktreePath:  sharedDir,
			Prompt:        "parallel work " + string(rune('A'+i)),
		})
	}

	for _, id := range []string{"att_pf_1", "att_pf_2"} {
		if err := r.WaitAttemptDone(ctx, id); err != nil {
			t.Fatalf("attempt %s failed: %v", id, err)
		}
		_, items, err := r.Outbox().ReadUnacked(ctx, id, 0, 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if it.FrameType == protocol.OutboxFrameState && it.Payload["state"] == string(protocol.AttemptWaitingResource) {
				t.Fatalf("attempt %s unexpectedly entered waiting_resource for shared plain folder: %+v", id, it.Payload)
			}
		}
	}
}

func TestRunner_ExplicitResourceWaitDeduplicatesEvents(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	att1Started := make(chan struct{})
	releaseAtt1 := make(chan struct{})
	drv := NewFakeAcpDriver()
	drv.OnPrompt = func(pctx context.Context, d *FakeAcpDriver, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
		if promptText == "holder" {
			close(att1Started)
			<-releaseAtt1
		}
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	r, err := runner.New(ctx, runner.Config{
		MachineID: "hub-node",
		DataDir:   tmp,
		Driver:    drv,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(context.Background()) }()

	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:         "att_res_1",
		TaskID:            "tsk_res_1",
		ProjectID:         protocol.ScratchProjectID,
		LeaseToken:        "lease_res_1",
		Adapter:           protocol.AdapterAgy,
		WBSessionID:       "ses_res_1",
		RequiredResources: []string{"xcode_sim"},
		Prompt:            "holder",
	})
	select {
	case <-att1Started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for att_res_1 to acquire resource")
	}

	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:         "att_res_2",
		TaskID:            "tsk_res_2",
		ProjectID:         protocol.ScratchProjectID,
		LeaseToken:        "lease_res_2",
		Adapter:           protocol.AdapterAgy,
		WBSessionID:       "ses_res_2",
		RequiredResources: []string{"xcode_sim"},
		Prompt:            "waiter",
	})

	// Wait across >2 ticker intervals (ticker is 1s) to verify AttemptWaitingResource is NOT spammed every second.
	time.Sleep(2400 * time.Millisecond)
	close(releaseAtt1)

	if err := r.WaitAttemptDone(ctx, "att_res_1"); err != nil {
		t.Fatal(err)
	}
	if err := r.WaitAttemptDone(ctx, "att_res_2"); err != nil {
		t.Fatal(err)
	}

	_, items, err := r.Outbox().ReadUnacked(ctx, "att_res_2", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	waitingCount := 0
	sawSucceeded := false
	for _, it := range items {
		if it.FrameType == protocol.OutboxFrameState {
			if it.Payload["state"] == string(protocol.AttemptWaitingResource) {
				waitingCount++
			}
			if it.Payload["state"] == string(protocol.AttemptSucceeded) {
				sawSucceeded = true
			}
		}
	}
	if waitingCount != 1 {
		t.Fatalf("expected exactly 1 deduplicated waiting_resource event across >2s wait, got %d", waitingCount)
	}
	if !sawSucceeded {
		t.Fatalf("expected att_res_2 to succeed after att_res_1 released xcode_sim")
	}
}

func TestRunner_NonZeroExitSucceedsWhileToolRejectionStillNeedsVerification(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	drv := NewFakeAcpDriver()
	drv.OnPrompt = func(pctx context.Context, d *FakeAcpDriver, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
		switch promptText {
		case "grep-exit-1":
			_ = d.EmitUpdate(pctx, sid, acp.SessionUpdate{
				ToolCallUpdate: &acp.SessionToolCallUpdate{
					ToolCallId: "tc_grep_1",
					Status:     acp.PatchSet(acp.ToolCallStatusCompleted),
					RawOutput:  acp.PatchSet(json.RawMessage(`{"exit_code":1,"stderr":"no matches"}`)),
				},
			})
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil

		case "rejected-perm":
			_, _ = d.AskPermission(pctx, acp.RequestPermissionRequest{
				SessionId: sid,
				ToolCall: acp.ToolCallUpdate{
					ToolCallId: "tc_danger_1",
				},
				Options: []acp.PermissionOption{
					{OptionId: "allow_once", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
					{OptionId: "reject_once", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
				},
			})
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	r, err := runner.New(ctx, runner.Config{
		MachineID: "hub-node",
		DataDir:   tmp,
		Driver:    drv,
		PermissionPolicy: func(attWorktree string, desiredMode string, req acp.RequestPermissionRequest) (string, bool) {
			return "", false // force human approval so we can inject reject_once
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(context.Background()) }()

	// Case 1: Non-zero exit code on normal end_turn MUST succeed cleanly without sessionLocked.
	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:   "att_nonzero_ok",
		TaskID:      "tsk_nonzero_ok",
		ProjectID:   protocol.ScratchProjectID,
		LeaseToken:  "lease_nz_1",
		Adapter:     protocol.AdapterAgy,
		WBSessionID: "ses_nz_1",
		Prompt:      "grep-exit-1",
	})
	if err := r.WaitAttemptDone(ctx, "att_nonzero_ok"); err != nil {
		t.Fatal(err)
	}
	_, items1, err := r.Outbox().ReadUnacked(ctx, "att_nonzero_ok", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var fin1 *protocol.OutboxItem
	for i := range items1 {
		if items1[i].FrameType == protocol.OutboxFrameState {
			st, _ := items1[i].Payload["state"].(string)
			if !protocol.IsActiveAttemptState(protocol.AttemptState(st)) {
				fin1 = &items1[i]
			}
		}
	}
	if fin1 == nil {
		t.Fatalf("missing terminal state frame for att_nonzero_ok")
	}
	if fin1.Payload["state"] != string(protocol.AttemptSucceeded) {
		t.Fatalf("expected non-zero exit on end_turn to settle as succeeded, got state=%v payload=%+v", fin1.Payload["state"], fin1.Payload)
	}
	if locked, _ := fin1.Payload["sessionLocked"].(bool); locked {
		t.Fatalf("expected sessionLocked=false for non-zero shell exit, got true")
	}
	if nz, _ := fin1.Payload["nonZeroExitCount"].(float64); nz < 1 {
		t.Fatalf("expected nonZeroExitCount telemetry >= 1, got %v", fin1.Payload["nonZeroExitCount"])
	}

	// Case 2: Explicit tool permission rejection MUST still settle as needs_verification (UNRESOLVED_TOOL_FAILURE).
	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:   "att_reject_nv",
		TaskID:      "tsk_reject_nv",
		ProjectID:   protocol.ScratchProjectID,
		LeaseToken:  "lease_rej_1",
		Adapter:     protocol.AdapterAgy,
		WBSessionID: "ses_rej_1",
		Prompt:      "rejected-perm",
	})

	// Wait for approval_request in outbox, then deliver reject_once decision.
	var aprID, payloadSHA string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && aprID == "" {
		_, items, _ := r.Outbox().ReadUnacked(ctx, "att_reject_nv", 0, 50)
		for _, it := range items {
			if it.FrameType == protocol.OutboxFrameApprovalRequest {
				aprID, _ = it.Payload["approvalId"].(string)
				payloadSHA, _ = it.Payload["payloadSha256"].(string)
				break
			}
		}
		if aprID == "" {
			time.Sleep(25 * time.Millisecond)
		}
	}
	if aprID == "" {
		t.Fatal("timed out waiting for approval_request")
	}
	r.HandleApprovalDecision(protocol.ApprovalDecisionPayload{
		ApprovalID:    aprID,
		DecisionID:    "dec_rej_1",
		AttemptID:     "att_reject_nv",
		PayloadSHA256: payloadSHA,
		OptionID:      "reject_once",
		DecidedBy:     "human",
	})

	if err := r.WaitAttemptDone(ctx, "att_reject_nv"); err != nil {
		t.Fatal(err)
	}
	_, items2, err := r.Outbox().ReadUnacked(ctx, "att_reject_nv", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var fin2 *protocol.OutboxItem
	for i := range items2 {
		if items2[i].FrameType == protocol.OutboxFrameState {
			st, _ := items2[i].Payload["state"].(string)
			if !protocol.IsActiveAttemptState(protocol.AttemptState(st)) {
				fin2 = &items2[i]
			}
		}
	}
	if fin2 == nil {
		t.Fatalf("missing terminal state frame for att_reject_nv")
	}
	if fin2.Payload["state"] != string(protocol.AttemptNeedsVerification) || fin2.Payload["errorCode"] != "UNRESOLVED_TOOL_FAILURE" {
		t.Fatalf("expected explicit rejection to settle as needs_verification/UNRESOLVED_TOOL_FAILURE, got %+v", fin2.Payload)
	}
}

func TestRunner_LocalDelegateBoardReadPostAndPlanGuard(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	var mu sync.Mutex
	var lastPath string
	var lastBody map[string]any
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		lastPath = r.URL.Path
		lastBody = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&lastBody)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/runner/delegate/board-post" {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"messageId": "bm_1", "seq": 1})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"taskId": "tsk_parent", "messages": []any{}})
	}))
	defer hubSrv.Close()

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	drv := NewFakeAcpDriver()
	drv.OnPrompt = func(pctx context.Context, d *FakeAcpDriver, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
		started <- struct{}{}
		<-release
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	sockPath := filepath.Join(tmp, "runner-board.sock")
	r, err := runner.New(ctx, runner.Config{
		MachineID:   "hub-node",
		HubHTTPURL:  hubSrv.URL,
		BearerToken: "runner-tok-board",
		DataDir:     tmp,
		SocketPath:  sockPath,
		Driver:      drv,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		close(release)
		_ = r.Close(context.Background())
	}()

	// Dispatch normal session + plan/read-only session concurrently.
	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:   "att_norm",
		TaskID:      "tsk_norm",
		ProjectID:   protocol.ScratchProjectID,
		LeaseToken:  "lease_norm",
		Adapter:     protocol.AdapterAgy,
		WBSessionID: "ses_norm",
		Prompt:      "normal",
	})
	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:     "att_plan",
		TaskID:        "tsk_plan",
		ProjectID:     protocol.ScratchProjectID,
		LeaseToken:    "lease_plan",
		Adapter:       protocol.AdapterAgy,
		WBSessionID:   "ses_plan",
		DesiredConfig: map[string]string{"mode": "plan"},
		Prompt:        "plan",
	})
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for attempts to start")
		}
	}

	unixClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
			},
		},
	}

	postSock := func(tok, path, body string) int {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://orbit-runner"+path, bytes.NewReader([]byte(body)))
		req.Header.Set("X-Orbit-Delegate-Token", tok)
		resp, err := unixClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	tokNorm := r.DelegateTokenForSession("ses_norm")
	tokPlan := r.DelegateTokenForSession("ses_plan")

	// Spoofed parentTaskId must be rejected with 400.
	if st := postSock(tokNorm, "/delegate/board-read", `{"parentTaskId":"tsk_evil"}`); st != http.StatusBadRequest {
		t.Fatalf("expected 400 on spoofed parentTaskId, got %d", st)
	}
	// Normal session board-read -> 200 with authoritative parentTaskId/parentAttemptId.
	if st := postSock(tokNorm, "/delegate/board-read", `{"limit":20}`); st != http.StatusOK {
		t.Fatalf("expected 200 for board-read, got %d", st)
	}
	mu.Lock()
	if lastPath != "/api/runner/delegate/board-read" || lastBody["parentTaskId"] != "tsk_norm" || lastBody["parentAttemptId"] != "att_norm" {
		t.Fatalf("unexpected forwarded board-read: path=%s body=%+v", lastPath, lastBody)
	}
	mu.Unlock()

	// Normal session board-post -> 201.
	if st := postSock(tokNorm, "/delegate/board-post", `{"body":"shared update","targetMention":"@parent"}`); st != http.StatusCreated {
		t.Fatalf("expected 201 for board-post, got %d", st)
	}
	mu.Lock()
	if lastPath != "/api/runner/delegate/board-post" || lastBody["parentTaskId"] != "tsk_norm" || lastBody["parentAttemptId"] != "att_norm" || lastBody["body"] != "shared update" {
		t.Fatalf("unexpected forwarded board-post: path=%s body=%+v", lastPath, lastBody)
	}
	mu.Unlock()

	// Plan/read-only session: board-read allowed (200), board-post forbidden (403).
	if st := postSock(tokPlan, "/delegate/board-read", `{}`); st != http.StatusOK {
		t.Fatalf("expected plan mode board-read=200, got %d", st)
	}
	if st := postSock(tokPlan, "/delegate/board-post", `{"body":"should block"}`); st != http.StatusForbidden {
		t.Fatalf("expected plan mode board-post=403, got %d", st)
	}
}
