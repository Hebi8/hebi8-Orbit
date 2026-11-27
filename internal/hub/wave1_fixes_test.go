package hub_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Hebi8/hebi8-Orbit/internal/hub"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

func TestStoreOutbox_ClearsWaitingReasonOnLeaveWaitSuccessAndCancel(t *testing.T) {
	ctx := context.Background()
	_, st, ts, runnerToken := setupSubsessionHub(t)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/runner"
	conn := dialTestRunnerWS(t, ctx, wsURL, "hub-node", "run_wr_1", runnerToken)
	defer conn.CloseNow()

	// Create task -> initial attempt leased
	resp, raw := doJSONReq(t, http.MethodPost, ts.URL+"/api/projects/demo-app/tasks", protocol.CreateTaskRequest{
		Title: "Resource Wait Clear Test",
		Goal:  "run task",
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create task status=%d body=%s", resp.StatusCode, raw)
	}
	var created protocol.CreateTaskResponse
	_ = json.Unmarshal(raw, &created)
	att1, err := st.GetAttemptRecord(ctx, created.Attempt.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.TryBindAttemptDispatched(ctx, att1.AttemptID, "run_wr_1")

	now := time.Now().UnixMilli()
	// 1. Enter waiting_resource -> waiting_reason is set.
	if _, err := st.IngestOutboxBatch(ctx, "hub-node", "run_wr_1", protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  att1.AttemptID,
		LeaseToken: att1.LeaseToken,
		Items: []protocol.OutboxItem{
			{
				OutboxSeq: 1,
				EventID:   "evt_wr_1",
				FrameType: protocol.OutboxFrameState,
				Payload: map[string]any{
					"state":         string(protocol.AttemptWaitingResource),
					"waitingReason": "等待资源 xcode_sim（被其他任务占用）",
				},
				TS: now,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	tk, err := st.GetTask(ctx, created.Task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if tk.State != protocol.TaskStateWaitingResource || tk.WaitingReason == "" {
		t.Fatalf("expected waiting_resource with waiting_reason, got state=%s reason=%q", tk.State, tk.WaitingReason)
	}

	// 2. Leave waiting_resource -> running MUST clear waiting_reason.
	if _, err := st.IngestOutboxBatch(ctx, "hub-node", "run_wr_1", protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  att1.AttemptID,
		LeaseToken: att1.LeaseToken,
		Items: []protocol.OutboxItem{
			{
				OutboxSeq: 2,
				EventID:   "evt_wr_2",
				FrameType: protocol.OutboxFrameState,
				Payload:   map[string]any{"state": string(protocol.AttemptRunning)},
				TS:        now + 10,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	tk, _ = st.GetTask(ctx, created.Task.TaskID)
	if tk.State != protocol.TaskStateRunning || tk.WaitingReason != "" {
		t.Fatalf("expected running with empty waiting_reason, got state=%s reason=%q", tk.State, tk.WaitingReason)
	}

	// 3. Re-enter waiting_resource then jump directly to succeeded -> waiting_reason MUST be cleared.
	if _, err := st.IngestOutboxBatch(ctx, "hub-node", "run_wr_1", protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  att1.AttemptID,
		LeaseToken: att1.LeaseToken,
		Items: []protocol.OutboxItem{
			{
				OutboxSeq: 3,
				EventID:   "evt_wr_3",
				FrameType: protocol.OutboxFrameState,
				Payload: map[string]any{
					"state":         string(protocol.AttemptWaitingResource),
					"waitingReason": "等待资源 chrome_logged_in",
				},
				TS: now + 20,
			},
			{
				OutboxSeq: 4,
				EventID:   "evt_wr_4",
				FrameType: protocol.OutboxFrameState,
				Payload: map[string]any{
					"state":            string(protocol.AttemptSucceeded),
					"stopReason":       "end_turn",
					"completionSource": string(protocol.CompletionPromptEndTurn),
				},
				TS: now + 30,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	tk, _ = st.GetTask(ctx, created.Task.TaskID)
	if tk.State != protocol.TaskStateInReview || tk.WaitingReason != "" {
		t.Fatalf("expected in_review with empty waiting_reason after succeeded, got state=%s reason=%q", tk.State, tk.WaitingReason)
	}

	// 4. Followup attempt enters waiting_resource then cancelled -> waiting_reason MUST be cleared.
	resp2, raw2 := doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+created.Task.TaskID+"/followup", protocol.FollowupTaskRequest{
		Message: "second turn",
		Mode:    protocol.FollowupModeQueue,
	}, nil)
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("followup status=%d body=%s", resp2.StatusCode, raw2)
	}
	var fResp protocol.FollowupTaskResponse
	_ = json.Unmarshal(raw2, &fResp)
	att2, _ := st.GetAttemptRecord(ctx, fResp.Attempt.AttemptID)
	_, _ = st.TryBindAttemptDispatched(ctx, att2.AttemptID, "run_wr_1")

	if _, err := st.IngestOutboxBatch(ctx, "hub-node", "run_wr_1", protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  att2.AttemptID,
		LeaseToken: att2.LeaseToken,
		Items: []protocol.OutboxItem{
			{
				OutboxSeq: 1,
				EventID:   "evt_wr2_1",
				FrameType: protocol.OutboxFrameState,
				Payload: map[string]any{
					"state":         string(protocol.AttemptWaitingResource),
					"waitingReason": "等待资源 xcode_sim",
				},
				TS: now + 40,
			},
			{
				OutboxSeq: 2,
				EventID:   "evt_wr2_2",
				FrameType: protocol.OutboxFrameState,
				Payload:   map[string]any{"state": string(protocol.AttemptCancelled)},
				TS:        now + 50,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	tk, _ = st.GetTask(ctx, created.Task.TaskID)
	if tk.State != protocol.TaskStateCancelled || tk.WaitingReason != "" {
		t.Fatalf("expected cancelled with empty waiting_reason, got state=%s reason=%q", tk.State, tk.WaitingReason)
	}
}

func TestSubsession_MultiChildAutoWakeupAndQueueDrain(t *testing.T) {
	ctx := context.Background()
	_, st, ts, runnerToken := setupSubsessionHub(t)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/runner"
	conn, dispatches := dialTestRunnerWSCaptureDispatches(t, ctx, wsURL, "hub-node", "run_wake_1", runnerToken)
	defer conn.CloseNow()

	waitDispatch := func(wantTaskID string) protocol.AttemptDispatchFrame {
		t.Helper()
		deadline := time.After(6 * time.Second)
		for {
			select {
			case df := <-dispatches:
				if df.TaskID == wantTaskID {
					return df
				}
			case <-deadline:
				t.Fatalf("timed out waiting for dispatch on task %s", wantTaskID)
			}
		}
	}

	sendFinish := func(df protocol.AttemptDispatchFrame, reply string, extra map[string]any) {
		t.Helper()
		now := time.Now().UnixMilli()
		finPayload := map[string]any{
			"state":            string(protocol.AttemptSucceeded),
			"stopReason":       "end_turn",
			"completionSource": string(protocol.CompletionPromptEndTurn),
		}
		for k, v := range extra {
			finPayload[k] = v
		}
		sendRunnerJSON(t, ctx, conn, protocol.AttemptOutboxBatchFrame{
			Type:       protocol.RunnerMsgOutboxBatch,
			AttemptID:  df.AttemptID,
			LeaseToken: df.LeaseToken,
			Items: []protocol.OutboxItem{
				{
					OutboxSeq: 1,
					EventID:   "evt_msg_" + df.AttemptID,
					FrameType: protocol.OutboxFrameEvents,
					Payload: map[string]any{
						"kind":       string(protocol.EventACPMessageBatch),
						"visibility": string(protocol.VisibilityRawACP),
						"actorKind":  string(protocol.ActorAgent),
						"actorId":    "agy",
						"payload":    map[string]any{"text": reply},
					},
					TS: now,
				},
				{
					OutboxSeq: 2,
					EventID:   "evt_fin_" + df.AttemptID,
					FrameType: protocol.OutboxFrameState,
					Payload:   finPayload,
					TS:        now + 5,
				},
			},
		})
	}

	// 1. Create Parent task and finish turn 1 with a non-zero exit code (grep exit 1) -> MUST succeed, NOT lock!
	resp, raw := doJSONReq(t, http.MethodPost, ts.URL+"/api/projects/demo-app/tasks", protocol.CreateTaskRequest{
		Title: "Parent Coordinator",
		Goal:  "coordinate two child tasks",
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create parent status=%d body=%s", resp.StatusCode, raw)
	}
	var parentCreated protocol.CreateTaskResponse
	_ = json.Unmarshal(raw, &parentCreated)
	parentID := parentCreated.Task.TaskID
	parentDF1 := waitDispatch(parentID)

	sendFinish(parentDF1, "parent initial turn done", map[string]any{
		"nonZeroExitCount":           1,
		"unresolvedNonZeroExitCount": 1,
	})

	// Wait until parent settles into in_review with empty waiting_reason (NOT needs_verification!).
	settled := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tk, _ := st.GetTask(ctx, parentID)
		if tk.State == protocol.TaskStateInReview && tk.ActiveAttemptID == "" {
			if tk.WaitingReason != "" {
				t.Fatalf("parent should NOT have waiting_reason after non-zero exit end_turn, got %q", tk.WaitingReason)
			}
			settled = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !settled {
		t.Fatal("timed out waiting for parent turn 1 to settle in_review")
	}

	// 2. Spawn Child 1 and Child 2 under Parent.
	spawnChild := func(title, goal string) (string, protocol.AttemptDispatchFrame) {
		r, b := doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+parentID+"/children", protocol.CreateChildTaskRequest{
			Title: title,
			Goal:  goal,
		}, nil)
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("create child %s status=%d body=%s", title, r.StatusCode, b)
		}
		var out protocol.CreateTaskResponse
		_ = json.Unmarshal(b, &out)
		return out.Task.TaskID, waitDispatch(out.Task.TaskID)
	}
	child1ID, child1DF := spawnChild("Child One", "analyze part 1")
	child2ID, child2DF := spawnChild("Child Two", "analyze part 2")

	// 3. Child 1 finishes while Parent is idle -> Parent MUST immediately auto-wake with child_result!
	sendFinish(child1DF, "child 1 completed findings", nil)
	parentDF2 := waitDispatch(parentID)
	if !strings.Contains(parentDF2.Prompt, child1ID) {
		t.Fatalf("expected parent turn 2 wake prompt to reference child 1 (%s), got %q", child1ID, parentDF2.Prompt)
	}
	attP2, err := st.GetAttemptRecord(ctx, parentDF2.AttemptID)
	if err != nil || attP2.TriggerKind != protocol.TriggerChildResult {
		t.Fatalf("expected parent turn 2 trigger=child_result, got att=%+v err=%v", attP2, err)
	}

	// 4. While Parent turn 2 is STILL running, Child 2 finishes -> queued in Parent's pending queue.
	sendFinish(child2DF, "child 2 completed findings", nil)
	deadline = time.Now().Add(5 * time.Second)
	queuedForChild2 := false
	for time.Now().Before(deadline) {
		q, _ := st.ListPendingInputs(ctx, parentID)
		for _, item := range q {
			if strings.Contains(item.Body, child2ID) {
				queuedForChild2 = true
				break
			}
		}
		if queuedForChild2 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !queuedForChild2 {
		t.Fatalf("expected child 2 result to enqueue in parent's task_pending_inputs while parent turn 2 is running")
	}

	// 5. Parent finishes turn 2 cleanly -> Hub MUST automatically dequeue Child 2 and dispatch Parent turn 3!
	sendFinish(parentDF2, "processed child 1", nil)
	parentDF3 := waitDispatch(parentID)
	if !strings.Contains(parentDF3.Prompt, child2ID) {
		t.Fatalf("expected parent turn 3 auto-drained prompt to reference child 2 (%s), got %q", child2ID, parentDF3.Prompt)
	}
	attP3, err := st.GetAttemptRecord(ctx, parentDF3.AttemptID)
	if err != nil || attP3.TriggerKind != protocol.TriggerChildResult {
		t.Fatalf("expected parent turn 3 trigger=child_result, got att=%+v err=%v", attP3, err)
	}
}

func TestSubsession_SharedBoardReadPostAndCrossParentGuard(t *testing.T) {
	ctx := context.Background()
	srv, st, ts, runnerToken := setupSubsessionHub(t)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/runner"
	conn := dialTestRunnerWS(t, ctx, wsURL, "hub-node", "run_brd_1", runnerToken)
	defer conn.CloseNow()

	runnerHdrs := map[string]string{
		"X-Orbit-Machine-Id": "hub-node",
		"Authorization":      "Bearer " + runnerToken,
	}
	doRunnerReq := func(path string, payload any) (*http.Response, []byte) {
		return doJSONReq(t, http.MethodPost, ts.URL+path, payload, runnerHdrs)
	}

	// Create Parent A + Child A1 + Child A2, and Parent B + Child B1.
	createParent := func(title string) (string, string) {
		r, b := doJSONReq(t, http.MethodPost, ts.URL+"/api/projects/demo-app/tasks", protocol.CreateTaskRequest{
			Title: title,
			Goal:  "work",
		}, nil)
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("create parent %s: %d %s", title, r.StatusCode, b)
		}
		var out protocol.CreateTaskResponse
		_ = json.Unmarshal(b, &out)
		_, _ = st.TryBindAttemptDispatched(ctx, out.Attempt.AttemptID, "run_brd_1")
		return out.Task.TaskID, out.Attempt.AttemptID
	}
	createChild := func(parentID, title, role string) (string, string) {
		r, b := doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+parentID+"/children", protocol.CreateChildTaskRequest{
			Title: title,
			Role:  role,
			Goal:  "subtask",
		}, nil)
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("create child %s: %d %s", title, r.StatusCode, b)
		}
		var out protocol.CreateTaskResponse
		_ = json.Unmarshal(b, &out)
		_, _ = st.TryBindAttemptDispatched(ctx, out.Attempt.AttemptID, "run_brd_1")
		return out.Task.TaskID, out.Attempt.AttemptID
	}

	parentA, attPA := createParent("Parent A")
	childA1, attA1 := createChild(parentA, "Backend Worker", "backend")
	childA2, attA2 := createChild(parentA, "Frontend Worker", "frontend")
	parentB, _ := createParent("Parent B")
	childB1, attB1 := createChild(parentB, "Other Worker", "other")

	// Subscribe to broker to confirm board.message_posted broadcast signal.
	evCh, unsub := srv.EventBroker().Subscribe()
	defer unsub()

	// 1. Parent A posts to shared board.
	r1, b1 := doRunnerReq("/api/runner/delegate/board-post", map[string]any{
		"parentTaskId":    parentA,
		"parentAttemptId": attPA,
		"body":            "API contract v1 ready",
	})
	if r1.StatusCode != http.StatusCreated {
		t.Fatalf("parent board-post status=%d body=%s", r1.StatusCode, b1)
	}
	var msg1 protocol.BoardMessage
	_ = json.Unmarshal(b1, &msg1)
	if msg1.TaskID != parentA || msg1.AuthorName != "Parent A" {
		t.Fatalf("unexpected parent board message: %+v", msg1)
	}

	// Verify broker broadcast signal AND persisted board.message_posted event.
	select {
	case <-evCh:
	case <-time.After(2 * time.Second):
		t.Fatal("expected board.message_posted event broadcast")
	}
	tlResp, tlRaw := doJSONReq(t, http.MethodGet, ts.URL+"/api/tasks/"+parentA+"/timeline", nil, nil)
	if tlResp.StatusCode != http.StatusOK {
		t.Fatalf("get timeline status=%d body=%s", tlResp.StatusCode, tlRaw)
	}
	var tl protocol.TaskTimelineResponse
	_ = json.Unmarshal(tlRaw, &tl)
	sawBoardEv := false
	for _, ev := range tl.Events {
		if ev.Kind == protocol.EventBoardMessagePosted && ev.TaskID == parentA {
			sawBoardEv = true
		}
	}
	if !sawBoardEv {
		t.Fatalf("expected board.message_posted event in task:%s timeline", parentA)
	}

	// 2. Child A1 posts mentioning Child A2 on the SAME shared board.
	r2, b2 := doRunnerReq("/api/runner/delegate/board-post", map[string]any{
		"parentTaskId":    childA1,
		"parentAttemptId": attA1,
		"targetMention":   "Frontend Worker",
		"body":            "Schema generated, ready for UI",
	})
	if r2.StatusCode != http.StatusCreated {
		t.Fatalf("child board-post status=%d body=%s", r2.StatusCode, b2)
	}
	var msg2 protocol.BoardMessage
	_ = json.Unmarshal(b2, &msg2)
	if msg2.TaskID != parentA || !strings.Contains(msg2.AuthorName, "Backend Worker") {
		t.Fatalf("expected child post on parentA board with normalized author, got %+v", msg2)
	}

	// 3. Child A2 reads shared board and sees BOTH messages.
	r3, b3 := doRunnerReq("/api/runner/delegate/board-read", map[string]any{
		"parentTaskId":    childA2,
		"parentAttemptId": attA2,
		"limit":           20,
	})
	if r3.StatusCode != http.StatusOK {
		t.Fatalf("child board-read status=%d body=%s", r3.StatusCode, b3)
	}
	var board protocol.TaskBoardResponse
	_ = json.Unmarshal(b3, &board)
	if board.TaskID != parentA || len(board.Messages) != 2 {
		t.Fatalf("expected childA2 to read 2 messages from parentA board, got taskId=%s len=%d", board.TaskID, len(board.Messages))
	}

	// 4. Cross-parent guards: Child B1 MUST NOT read or post to Parent A's board.
	rCrossRead, _ := doRunnerReq("/api/runner/delegate/board-read", map[string]any{
		"parentTaskId":    childB1,
		"parentAttemptId": attB1,
		"boardTaskId":     parentA,
	})
	if rCrossRead.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-parent board-read, got %d", rCrossRead.StatusCode)
	}
	rCrossPost, _ := doRunnerReq("/api/runner/delegate/board-post", map[string]any{
		"parentTaskId":    childB1,
		"parentAttemptId": attB1,
		"boardTaskId":     parentA,
		"body":            "intrusion",
	})
	if rCrossPost.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-parent board-post, got %d", rCrossPost.StatusCode)
	}
	rCrossMention, _ := doRunnerReq("/api/runner/delegate/board-post", map[string]any{
		"parentTaskId":    childA1,
		"parentAttemptId": attA1,
		"targetMention":   childB1,
		"body":            "cross mention",
	})
	if rCrossMention.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-parent task mention, got %d", rCrossMention.StatusCode)
	}

	// 5. Human POST /api/tasks/{parentA}/board/messages with @Backend Worker persists, broadcasts, and queues on childA1.
	rHuman, bHuman := doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+parentA+"/board/messages", protocol.PostBoardMessageRequest{
		AuthorName:    "human",
		TargetMention: "Backend Worker",
		Body:          "Please double check pagination",
	}, nil)
	if rHuman.StatusCode != http.StatusCreated {
		t.Fatalf("human board post status=%d body=%s", rHuman.StatusCode, bHuman)
	}
	queuedChildA1, err := st.ListPendingInputs(ctx, childA1)
	if err != nil {
		t.Fatal(err)
	}
	if len(queuedChildA1) == 0 || !strings.Contains(queuedChildA1[0].Body, "Please double check pagination") {
		t.Fatalf("expected human @child board post to route to childA1 queue, got %+v", queuedChildA1)
	}
}

func TestHub_MachineQuotasReportBootstrapAndRefreshAPI(t *testing.T) {
	ctx := context.Background()
	_, _, ts, runnerToken := setupSubsessionHub(t)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/runner"
	hdr := http.Header{}
	hdr.Set("X-Orbit-Machine-Id", "hub-node")
	hdr.Set("Authorization", "Bearer "+runnerToken)
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("dial runner ws: %v", err)
	}
	defer conn.CloseNow()

	initialQuotas := []protocol.AgentQuotaSnapshot{
		{
			Adapter:   protocol.AdapterAgy,
			Available: true,
			FetchedAt: time.Now().UnixMilli(),
			Windows: []protocol.QuotaWindow{
				{Window: "5h", Group: "Gemini Models", UsedPercent: 25, RemainingPercent: 75},
			},
		},
	}
	refreshedQuotas := []protocol.AgentQuotaSnapshot{
		{
			Adapter:   protocol.AdapterAgy,
			Available: true,
			FetchedAt: time.Now().UnixMilli() + 1000,
			Windows: []protocol.QuotaWindow{
				{Window: "5h", Group: "Gemini Models", UsedPercent: 40, RemainingPercent: 60},
			},
		},
		{
			Adapter:   protocol.AdapterClaude,
			Available: true,
			FetchedAt: time.Now().UnixMilli() + 1000,
			Windows: []protocol.QuotaWindow{
				{Window: "5h", UsedPercent: 15, RemainingPercent: 85},
				{Window: "7d", UsedPercent: 10, RemainingPercent: 90},
			},
		},
	}

	sendRunnerJSON(t, ctx, conn, protocol.RunnerHelloFrame{
		Type:             protocol.RunnerMsgHello,
		ProtocolVersion:  1,
		MachineID:        "hub-node",
		RunnerInstanceID: "run_quota_1",
		RunnerVersion:    "0.3.2-test",
		Capabilities:     []string{protocol.RunnerCapAgentsV1, protocol.RunnerCapQuotasV1},
		Adapters:         []protocol.Adapter{protocol.AdapterAgy, protocol.AdapterClaude},
		AgentQuotas:      initialQuotas,
	})
	// Read hub.welcome
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if _, _, err := conn.Read(rctx); err != nil {
		cancel()
		t.Fatalf("read hub.welcome: %v", err)
	}
	cancel()

	// Respond to probe_quotas RPC on the runner WS.
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var h protocol.RunnerFrameHeader
			if json.Unmarshal(data, &h) == nil && h.Type == protocol.RunnerMsgRPC {
				var rpc protocol.RunnerRPCFrame
				if json.Unmarshal(data, &rpc) == nil && rpc.Op == "probe_quotas" {
					out, _ := json.Marshal(protocol.RunnerRPCResultFrame{
						Type:      protocol.RunnerMsgRPCResult,
						RequestID: rpc.RequestID,
						OK:        true,
						Data:      map[string]any{"agentQuotas": refreshedQuotas},
					})
					_ = conn.Write(ctx, websocket.MessageText, out)
				}
			}
		}
	}()

	// 1. Bootstrap returns initial hello quotas on both machine.agentQuotas and agents[agy].quota.
	resp, raw := doJSONReq(t, http.MethodGet, ts.URL+"/api/bootstrap", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bootstrap status=%d body=%s", resp.StatusCode, raw)
	}
	var boot protocol.BootstrapResponse
	_ = json.Unmarshal(raw, &boot)
	var hubM *protocol.MachineView
	for i := range boot.Machines {
		if boot.Machines[i].MachineID == "hub-node" {
			hubM = &boot.Machines[i]
			break
		}
	}
	if hubM == nil || len(hubM.AgentQuotas) != 1 || hubM.AgentQuotas[0].Windows[0].UsedPercent != 25 {
		t.Fatalf("expected initial agy quota (25%%) in bootstrap, got %+v", hubM)
	}
	foundAgentQuota := false
	for _, ag := range hubM.Agents {
		if ag.Adapter == protocol.AdapterAgy && ag.Quota != nil && len(ag.Quota.Windows) == 1 && ag.Quota.Windows[0].UsedPercent == 25 {
			foundAgentQuota = true
		}
	}
	if !foundAgentQuota {
		t.Fatalf("expected quota attached onto agents[agy].quota, got %+v", hubM.Agents)
	}

	// 2. Async runner.quotas_report updates persisted quotas.
	sendRunnerJSON(t, ctx, conn, protocol.RunnerQuotasReportFrame{
		Type:      protocol.RunnerMsgQuotasReport,
		MachineID: "hub-node",
		Quotas: []protocol.AgentQuotaSnapshot{
			{
				Adapter:   protocol.AdapterAgy,
				Available: true,
				FetchedAt: time.Now().UnixMilli() + 500,
				Windows:   []protocol.QuotaWindow{{Window: "5h", UsedPercent: 33, RemainingPercent: 67}},
			},
		},
	})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, b := doJSONReq(t, http.MethodGet, ts.URL+"/api/machines", nil, nil)
		var lm protocol.ListMachinesResponse
		_ = json.Unmarshal(b, &lm)
		for _, m := range lm.Machines {
			if m.MachineID == "hub-node" && len(m.AgentQuotas) == 1 && m.AgentQuotas[0].Windows[0].UsedPercent == 33 {
				goto testRefresh
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("timed out waiting for runner.quotas_report persistence")

testRefresh:
	// 3. POST /api/machines/hub-node/quotas/refresh invokes probe_quotas RPC and persists updated quotas.
	rRef, bRef := doJSONReq(t, http.MethodPost, ts.URL+"/api/machines/hub-node/quotas/refresh", map[string]any{}, nil)
	if rRef.StatusCode != http.StatusOK {
		t.Fatalf("quotas refresh status=%d body=%s", rRef.StatusCode, bRef)
	}
	var qResp protocol.MachineQuotasResponse
	_ = json.Unmarshal(bRef, &qResp)
	if qResp.MachineID != "hub-node" || len(qResp.AgentQuotas) != 2 || qResp.AgentQuotas[0].Windows[0].UsedPercent != 40 {
		t.Fatalf("unexpected quotas refresh response: %+v", qResp)
	}
}

func TestHub_DeleteYAMLAndSSHMachinesTombstoneAndOfflinePatch(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "config")
	dataDir := filepath.Join(tmp, "data")
	_ = os.MkdirAll(cfgDir, 0o755)

	runnerToken := "runner-secret-del"
	sum := sha256.Sum256([]byte(runnerToken))
	tokenHash := "sha256:" + hex.EncodeToString(sum[:])

	machinesYAML := `schemaVersion: 1
machines:
  hub-node:
    displayName: "hub-node"
    transport: local_ws
    tokenHash: "` + tokenHash + `"
    capabilities: [primary-dev]
  aux-node:
    displayName: "aux-node"
    transport: remote_ws
    sshAlias: "aux-old"
    capabilities: [primary-dev]
  deploy-vps:
    displayName: "deploy-vps"
    transport: ssh_outbound
    sshAlias: "vps-old"
    capabilities: [deploy-node]
`
	machPath := filepath.Join(cfgDir, "machines.yaml")
	if err := os.WriteFile(machPath, []byte(machinesYAML), 0o644); err != nil {
		t.Fatal(err)
	}

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

	// Set explicit dutyPriority containing aux-node.
	rDuty, bDuty := doJSONReq(t, http.MethodPatch, ts.URL+"/api/network/settings", protocol.UpdateNetworkSettingsRequest{
		DutyPriority: []string{"hub-node", "aux-node"},
	}, nil)
	if rDuty.StatusCode != http.StatusOK {
		t.Fatalf("update dutyPriority status=%d body=%s", rDuty.StatusCode, bDuty)
	}

	// 1. Deleting local_ws hub-node MUST be rejected with 409 ERR_CANNOT_DELETE_LOCAL.
	rLocal, bLocal := doJSONReq(t, http.MethodDelete, ts.URL+"/api/machines/hub-node", nil, nil)
	if rLocal.StatusCode != http.StatusConflict || !strings.Contains(string(bLocal), "ERR_CANNOT_DELETE_LOCAL") {
		t.Fatalf("expected 409 ERR_CANNOT_DELETE_LOCAL for hub-node, got %d %s", rLocal.StatusCode, bLocal)
	}

	// 2. PATCH offline remote_ws (aux-node) and ssh_outbound (deploy-vps) MUST succeed without ERR_MACHINE_OFFLINE.
	auxAlias := "Aux Offline Box"
	auxRole := protocol.MachineRoleWorker
	auxSSH := "aux-new"
	rPatchAux, bPatchAux := doJSONReq(t, http.MethodPatch, ts.URL+"/api/machines/aux-node", protocol.UpdateMachineRequest{
		DisplayAlias: &auxAlias,
		Role:         &auxRole,
		AllowedRoots: []string{"/home/orbit/work"},
		SSHAlias:     &auxSSH,
	}, nil)
	if rPatchAux.StatusCode != http.StatusOK {
		t.Fatalf("PATCH offline aux-node failed: %d %s", rPatchAux.StatusCode, bPatchAux)
	}
	var lm protocol.ListMachinesResponse
	_ = json.Unmarshal(bPatchAux, &lm)
	foundAux := false
	for _, m := range lm.Machines {
		if m.MachineID == "aux-node" {
			foundAux = true
			if m.DisplayAlias != auxAlias || m.Role != auxRole || m.SSHAlias != auxSSH || !slices.Contains(m.AllowedRoots, "/home/orbit/work") {
				t.Fatalf("offline PATCH did not persist on aux-node: %+v", m)
			}
		}
	}
	if !foundAux {
		t.Fatal("aux-node missing after PATCH")
	}

	vpsAlias := "Deploy VPS Renamed"
	vpsSSH := "deploy-prod"
	rPatchVPS, bPatchVPS := doJSONReq(t, http.MethodPatch, ts.URL+"/api/machines/deploy-vps", protocol.UpdateMachineRequest{
		DisplayAlias: &vpsAlias,
		SSHAlias:     &vpsSSH,
	}, nil)
	if rPatchVPS.StatusCode != http.StatusOK {
		t.Fatalf("PATCH ssh_outbound deploy-vps failed: %d %s", rPatchVPS.StatusCode, bPatchVPS)
	}
	_ = json.Unmarshal(bPatchVPS, &lm)
	for _, m := range lm.Machines {
		if m.MachineID == "deploy-vps" && (m.DisplayAlias != vpsAlias || m.SSHAlias != vpsSSH) {
			t.Fatalf("ssh_outbound PATCH did not persist on deploy-vps: %+v", m)
		}
	}

	// 3. DELETE YAML ssh_outbound (deploy-vps) and offline YAML remote_ws (aux-node) MUST both succeed!
	for _, id := range []string{"deploy-vps", "aux-node"} {
		rDel, bDel := doJSONReq(t, http.MethodDelete, ts.URL+"/api/machines/"+id, nil, nil)
		if rDel.StatusCode != http.StatusOK {
			t.Fatalf("DELETE /api/machines/%s failed: %d %s", id, rDel.StatusCode, bDel)
		}
	}

	// Verify both are gone from /api/machines and aux-node was pruned from dutyPriority.
	_, bList := doJSONReq(t, http.MethodGet, ts.URL+"/api/machines", nil, nil)
	_ = json.Unmarshal(bList, &lm)
	for _, m := range lm.Machines {
		if m.MachineID == "aux-node" || m.MachineID == "deploy-vps" {
			t.Fatalf("deleted YAML machine %s still visible in /api/machines", m.MachineID)
		}
	}
	_, bNet := doJSONReq(t, http.MethodGet, ts.URL+"/api/network/settings", nil, nil)
	var netView protocol.NetworkSettingsView
	_ = json.Unmarshal(bNet, &netView)
	if slices.Contains(netView.DutyPriority, "aux-node") {
		t.Fatalf("deleted machine aux-node still in dutyPriority: %+v", netView.DutyPriority)
	}

	// 4. Re-write original machines.yaml (simulating read-only YAML or external sync),
	//    run ReloadConfig AND restart a brand-new Hub server -> tombstone MUST prevent resurrection!
	if err := os.WriteFile(machPath, []byte(machinesYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ReloadConfig(ctx); err != nil {
		t.Fatalf("ReloadConfig after tombstone failed: %v", err)
	}
	_, bAfterReload := doJSONReq(t, http.MethodGet, ts.URL+"/api/machines", nil, nil)
	_ = json.Unmarshal(bAfterReload, &lm)
	for _, m := range lm.Machines {
		if m.MachineID == "aux-node" || m.MachineID == "deploy-vps" {
			t.Fatalf("ReloadConfig resurrected deleted YAML machine %s!", m.MachineID)
		}
	}

	// Simulate Hub restart against the exact same SQLite + restored machines.yaml.
	srv2, err := hub.NewServer(ctx, hub.ServerOptions{
		HubConfig:   cfg,
		Store:       st,
		RyugiRunner: &fakeRyugiRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv2.Shutdown)
	ts2 := httptest.NewServer(srv2.Handler())
	t.Cleanup(ts2.Close)

	_, bRestart := doJSONReq(t, http.MethodGet, ts2.URL+"/api/bootstrap", nil, nil)
	var boot2 protocol.BootstrapResponse
	_ = json.Unmarshal(bRestart, &boot2)
	for _, m := range boot2.Machines {
		if m.MachineID == "aux-node" || m.MachineID == "deploy-vps" {
			t.Fatalf("Hub restart resurrected deleted YAML machine %s!", m.MachineID)
		}
	}

	// 5. Re-pairing a machine with ProposedID="aux-node" MUST clear the tombstone and allocate "aux-node".
	rPair, bPair := doJSONReq(t, http.MethodPost, ts2.URL+"/api/pairing/codes", protocol.CreatePairingCodeRequest{
		Scope: protocol.PairingScopeRunner,
	}, nil)
	if rPair.StatusCode != http.StatusCreated {
		t.Fatalf("create pairing code status=%d body=%s", rPair.StatusCode, bPair)
	}
	var pCode protocol.CreatePairingCodeResponse
	_ = json.Unmarshal(bPair, &pCode)

	rClaim, bClaim := doJSONReq(t, http.MethodPost, ts2.URL+"/api/pairing/claim", protocol.ClaimPairingRequest{
		Code:       pCode.Code,
		WantRunner: true,
		Machine:    &protocol.ClaimMachineInfo{ProposedID: "aux-node", DisplayName: "Aux Re-Paired"},
	}, nil)
	if rClaim.StatusCode != http.StatusOK {
		t.Fatalf("claim pairing code status=%d body=%s", rClaim.StatusCode, bClaim)
	}
	var claimed protocol.ClaimPairingResponse
	_ = json.Unmarshal(bClaim, &claimed)
	if claimed.MachineID != "aux-node" {
		t.Fatalf("expected re-paired machine to reclaim ID aux-node, got %q", claimed.MachineID)
	}

	_, bFinal := doJSONReq(t, http.MethodGet, ts2.URL+"/api/machines", nil, nil)
	_ = json.Unmarshal(bFinal, &lm)
	foundRepaired := false
	for _, m := range lm.Machines {
		if m.MachineID == "aux-node" {
			foundRepaired = true
		}
	}
	if !foundRepaired {
		t.Fatalf("re-paired aux-node missing from /api/machines: %+v", lm.Machines)
	}
}
