package hub_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

func dialClientWSAtSeq(t *testing.T, ctx context.Context, baseHTTPURL string, sinceSeq int64) *websocket.Conn {
	t.Helper()
	wsURL := fmt.Sprintf("ws%s/ws/client?sinceSeq=%d", strings.TrimPrefix(baseHTTPURL, "http"), sinceSeq)
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial client ws: %v", err)
	}
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		_, data, err := conn.Read(readCtx)
		if err != nil {
			_ = conn.CloseNow()
			t.Fatalf("read replay_end: %v", err)
		}
		var msg protocol.WSServerMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			_ = conn.CloseNow()
			t.Fatal(err)
		}
		if msg.Type == protocol.WSMessageReplayEnd {
			return conn
		}
	}
}

func sendClientWSControl(t *testing.T, ctx context.Context, conn *websocket.Conn, msg protocol.WSClientMessage) {
	t.Helper()
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := conn.Write(wctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write client ws control: %v", err)
	}
}

func readUntilWatchAck(t *testing.T, ctx context.Context, conn *websocket.Conn, wantAttemptIDs []string) (protocol.WSServerMessage, []protocol.EventRecord) {
	t.Helper()
	wantSorted := append([]string(nil), wantAttemptIDs...)
	slices.Sort(wantSorted)
	var seenEvents []protocol.EventRecord
	for {
		rctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		_, data, err := conn.Read(rctx)
		cancel()
		if err != nil {
			t.Fatalf("waiting for watch_ack %v failed: %v", wantSorted, err)
		}
		var msg protocol.WSServerMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type == protocol.WSMessageEvent && msg.Event != nil {
			seenEvents = append(seenEvents, *msg.Event)
			continue
		}
		if msg.Type == protocol.WSMessageWatchAck && slices.Equal(msg.AttemptIDs, wantSorted) {
			return msg, seenEvents
		}
	}
}

func fetchAttemptRawHTTP(t *testing.T, baseURL, attemptID string, afterSeq int64) []protocol.EventRecord {
	t.Helper()
	url := fmt.Sprintf("%s/api/attempts/%s/raw?afterSeq=%d&limit=500", baseURL, attemptID, afterSeq)
	resp, body := doJSONReq(t, http.MethodGet, url, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s failed: %d %s", url, resp.StatusCode, string(body))
	}
	var out protocol.AttemptRawEventsResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode raw response: %v", err)
	}
	return out.Events
}

func TestClientWS_WatchAttemptsStreamsRawACPOnDemandOnly(t *testing.T) {
	srv, st, ts, runnerToken := setupSubsessionHub(t)
	ctx := context.Background()

	runnerWSURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/runner"
	runnerConn := dialTestRunnerWS(t, ctx, runnerWSURL, "hub-node", "ri_watch_stream", runnerToken)
	defer runnerConn.CloseNow()

	task1, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		Title:         "Watched Streaming Task",
		Goal:          "Stream chunks live",
		MachineID:     "hub-node",
		Adapter:       protocol.AdapterAgy,
		CreateAttempt: true,
		LeaseTTLMs:    60000,
	})
	if err != nil {
		t.Fatal(err)
	}
	task2, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		Title:         "Unwatched Background Task",
		Goal:          "Background chunks",
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

	watcherConn := dialClientWSAtSeq(t, ctx, ts.URL, headSeq)
	defer watcherConn.CloseNow()
	idleConn := dialClientWSAtSeq(t, ctx, ts.URL, headSeq)
	defer idleConn.CloseNow()

	// watcherConn subscribes ONLY to task1's attempt and receives watch_ack; idleConn stays unsubscribed.
	sendClientWSControl(t, ctx, watcherConn, protocol.WSClientMessage{
		Type:       protocol.WSClientWatchAttempts,
		AttemptIDs: []string{task1.Attempt.AttemptID},
	})
	ack1, _ := readUntilWatchAck(t, ctx, watcherConn, []string{task1.Attempt.AttemptID})
	if ack1.Seq < headSeq {
		t.Fatalf("expected watch_ack seq >= %d, got %d", headSeq, ack1.Seq)
	}
	if srv.EventBroker().AttemptWatcherCount(task1.Attempt.AttemptID) != 1 {
		t.Fatalf("expected watcher count 1 after watch_ack")
	}

	now := time.Now().UnixMilli()
	// Emit streaming raw_acp events on watched task1 (thought + tool_call + tool_call_update + message_batch).
	sendRunnerJSON(t, ctx, runnerConn, protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  task1.Attempt.AttemptID,
		LeaseToken: task1.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{
				OutboxSeq: 1,
				EventID:   "ev_watch_thought_1",
				FrameType: protocol.OutboxFrameEvents,
				TS:        now,
				Payload: map[string]any{
					"kind":       string(protocol.EventACPThoughtBatch),
					"visibility": string(protocol.VisibilityRawACP),
					"actorKind":  string(protocol.ActorAgent),
					"actorId":    "agy",
					"payload":    map[string]any{"text": "thinking..."},
				},
			},
			{
				OutboxSeq: 2,
				EventID:   "ev_watch_tool_1",
				FrameType: protocol.OutboxFrameEvents,
				TS:        now + 1,
				Payload: map[string]any{
					"kind":       string(protocol.EventACPToolCall),
					"visibility": string(protocol.VisibilityRawACP),
					"actorKind":  string(protocol.ActorAgent),
					"actorId":    "agy",
					"payload":    map[string]any{"toolCallId": "tc_1", "name": "read_file", "status": "in_progress"},
				},
			},
			{
				OutboxSeq: 3,
				EventID:   "ev_watch_tool_upd_1",
				FrameType: protocol.OutboxFrameEvents,
				TS:        now + 2,
				Payload: map[string]any{
					"kind":       string(protocol.EventACPToolCallUpdate),
					"visibility": string(protocol.VisibilityRawACP),
					"actorKind":  string(protocol.ActorAgent),
					"actorId":    "agy",
					"payload":    map[string]any{"toolCallId": "tc_1", "status": "completed"},
				},
			},
			{
				OutboxSeq: 4,
				EventID:   "ev_watch_msg_1",
				FrameType: protocol.OutboxFrameEvents,
				TS:        now + 3,
				Payload: map[string]any{
					"kind":       string(protocol.EventACPMessageBatch),
					"visibility": string(protocol.VisibilityRawACP),
					"actorKind":  string(protocol.ActorAgent),
					"actorId":    "agy",
					"payload":    map[string]any{"text": "streamed answer chunk"},
				},
			},
		},
	})

	// Emit raw_acp message_batch on UNWATCHED task2 + a non-raw state transition on task2 as a sentinel.
	sendRunnerJSON(t, ctx, runnerConn, protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  task2.Attempt.AttemptID,
		LeaseToken: task2.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{
				OutboxSeq: 1,
				EventID:   "ev_unwatched_msg_1",
				FrameType: protocol.OutboxFrameEvents,
				TS:        now + 4,
				Payload: map[string]any{
					"kind":       string(protocol.EventACPMessageBatch),
					"visibility": string(protocol.VisibilityRawACP),
					"actorKind":  string(protocol.ActorAgent),
					"actorId":    "agy",
					"payload":    map[string]any{"text": "must not be pushed to watcher or idle"},
				},
			},
			{
				OutboxSeq: 2,
				EventID:   "ev_task2_running",
				FrameType: protocol.OutboxFrameState,
				TS:        now + 5,
				Payload:   map[string]any{"state": string(protocol.AttemptRunning)},
			},
		},
	})

	// 1. watcherConn MUST receive all 4 task1 raw_acp events in strict ascending seq order, plus task2's timeline sentinel,
	//    and MUST NOT receive task2's raw_acp message_batch.
	var watcherRawKinds []protocol.EventKind
	var prevSeq int64
	gotTask2State := false
	for !gotTask2State {
		rctx, rcancel := context.WithTimeout(ctx, 4*time.Second)
		_, data, err := watcherConn.Read(rctx)
		rcancel()
		if err != nil {
			t.Fatalf("watcherConn read failed: %v", err)
		}
		var msg protocol.WSServerMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type != protocol.WSMessageEvent || msg.Event == nil {
			continue
		}
		ev := msg.Event
		if ev.Seq <= prevSeq {
			t.Fatalf("watcherConn received non-monotonic seq %d after %d", ev.Seq, prevSeq)
		}
		prevSeq = ev.Seq
		if ev.AttemptID == task2.Attempt.AttemptID && ev.Visibility == protocol.VisibilityRawACP {
			t.Fatalf("watcherConn received unwatched raw_acp event: %+v", ev)
		}
		if ev.AttemptID == task1.Attempt.AttemptID && ev.Visibility == protocol.VisibilityRawACP {
			watcherRawKinds = append(watcherRawKinds, ev.Kind)
		}
		if ev.AttemptID == task2.Attempt.AttemptID && ev.Kind == protocol.EventAttemptStateChanged {
			gotTask2State = true
		}
	}
	wantKinds := []protocol.EventKind{
		protocol.EventACPThoughtBatch,
		protocol.EventACPToolCall,
		protocol.EventACPToolCallUpdate,
		protocol.EventACPMessageBatch,
	}
	if !slices.Equal(watcherRawKinds, wantKinds) {
		t.Fatalf("watcherConn expected %v, got %v", wantKinds, watcherRawKinds)
	}

	// 2. idleConn MUST receive ONLY task2's timeline sentinel and ZERO raw_acp or watch_ack frames.
	gotIdleSentinel := false
	for !gotIdleSentinel {
		rctx, rcancel := context.WithTimeout(ctx, 4*time.Second)
		_, data, err := idleConn.Read(rctx)
		rcancel()
		if err != nil {
			t.Fatalf("idleConn read failed: %v", err)
		}
		var msg protocol.WSServerMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type == protocol.WSMessageWatchAck {
			t.Fatalf("unsubscribed idleConn must not receive watch_ack")
		}
		if msg.Type != protocol.WSMessageEvent || msg.Event == nil {
			continue
		}
		if msg.Event.Visibility == protocol.VisibilityRawACP {
			t.Fatalf("idleConn MUST NOT receive raw_acp event: %+v", msg.Event)
		}
		if msg.Event.AttemptID == task2.Attempt.AttemptID && msg.Event.Kind == protocol.EventAttemptStateChanged {
			gotIdleSentinel = true
		}
	}

	// 3. Unwatching task1 emits watch_ack([]) and stops pushing subsequent raw_acp events to watcherConn.
	sendClientWSControl(t, ctx, watcherConn, protocol.WSClientMessage{
		Type:       protocol.WSClientWatchAttempts,
		AttemptIDs: []string{},
	})
	_, _ = readUntilWatchAck(t, ctx, watcherConn, []string{})
	if srv.EventBroker().AttemptWatcherCount(task1.Attempt.AttemptID) != 0 {
		t.Fatalf("expected watcher count 0 after unwatch ack")
	}

	sendRunnerJSON(t, ctx, runnerConn, protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  task1.Attempt.AttemptID,
		LeaseToken: task1.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{
				OutboxSeq: 5,
				EventID:   "ev_watch_msg_after_unwatch",
				FrameType: protocol.OutboxFrameEvents,
				TS:        now + 10,
				Payload: map[string]any{
					"kind":       string(protocol.EventACPMessageBatch),
					"visibility": string(protocol.VisibilityRawACP),
					"actorKind":  string(protocol.ActorAgent),
					"actorId":    "agy",
					"payload":    map[string]any{"text": "should not arrive after unwatch"},
				},
			},
			{
				OutboxSeq: 6,
				EventID:   "ev_task1_running",
				FrameType: protocol.OutboxFrameState,
				TS:        now + 11,
				Payload:   map[string]any{"state": string(protocol.AttemptRunning)},
			},
		},
	})

	for {
		rctx, rcancel := context.WithTimeout(ctx, 4*time.Second)
		_, data, err := watcherConn.Read(rctx)
		rcancel()
		if err != nil {
			t.Fatalf("watcherConn read after unwatch failed: %v", err)
		}
		var msg protocol.WSServerMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type != protocol.WSMessageEvent || msg.Event == nil {
			continue
		}
		if msg.Event.EventID == "ev_watch_msg_after_unwatch" {
			t.Fatalf("watcherConn received raw_acp chunk after unwatch")
		}
		if msg.Event.AttemptID == task1.Attempt.AttemptID && msg.Event.Kind == protocol.EventAttemptStateChanged {
			break
		}
	}
}

func TestClientWS_WatchAckClosesReplayAndDynamicWatchGapWindow(t *testing.T) {
	_, st, ts, runnerToken := setupSubsessionHub(t)
	ctx := context.Background()

	runnerWSURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/runner"
	runnerConn := dialTestRunnerWS(t, ctx, runnerWSURL, "hub-node", "ri_watch_gap", runnerToken)
	defer runnerConn.CloseNow()

	task1, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		Title:         "Gap Window Task",
		Goal:          "No dropped raw chunks",
		MachineID:     "hub-node",
		Adapter:       protocol.AdapterAgy,
		CreateAttempt: true,
		LeaseTTLMs:    60000,
	})
	if err != nil {
		t.Fatal(err)
	}
	task2, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		Title:         "Other Task Advancing Global Seq",
		Goal:          "Bump lastSentSeq",
		MachineID:     "hub-node",
		Adapter:       protocol.AdapterAgy,
		CreateAttempt: true,
		LeaseTTLMs:    60000,
	})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UnixMilli()
	// 1. Seed chunk 1 ("c1") and simulate client initial HTTP fetch up to httpCursor = c1.Seq (analogous to HTTP=100).
	sendRunnerJSON(t, ctx, runnerConn, protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  task1.Attempt.AttemptID,
		LeaseToken: task1.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{
				OutboxSeq: 1,
				EventID:   "ev_gap_c1",
				FrameType: protocol.OutboxFrameEvents,
				TS:        now,
				Payload: map[string]any{
					"kind":       string(protocol.EventACPMessageBatch),
					"visibility": string(protocol.VisibilityRawACP),
					"actorKind":  string(protocol.ActorAgent),
					"actorId":    "agy",
					"payload":    map[string]any{"text": "chunk-100"},
				},
			},
		},
	})

	var initialHTTP []protocol.EventRecord
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		initialHTTP = fetchAttemptRawHTTP(t, ts.URL, task1.Attempt.AttemptID, 0)
		if len(initialHTTP) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(initialHTTP) != 1 {
		t.Fatalf("expected 1 initial raw chunk, got %d", len(initialHTTP))
	}
	httpCursor := initialHTTP[0].Seq

	// 2. Connect WS WITHOUT watching task1 yet (or before watch_attempts takes effect).
	clientConn := dialClientWSAtSeq(t, ctx, ts.URL, httpCursor)
	defer clientConn.CloseNow()

	// 3. Emit chunk 2 ("chunk-101") on task1 AND a non-raw state event on task2 so clientConn's global lastSentSeq
	//    advances strictly PAST chunk-101 while task1 is still unwatched (exact dynamic-watch gap scenario!).
	sendRunnerJSON(t, ctx, runnerConn, protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  task1.Attempt.AttemptID,
		LeaseToken: task1.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{
				OutboxSeq: 2,
				EventID:   "ev_gap_c2",
				FrameType: protocol.OutboxFrameEvents,
				TS:        now + 1,
				Payload: map[string]any{
					"kind":       string(protocol.EventACPMessageBatch),
					"visibility": string(protocol.VisibilityRawACP),
					"actorKind":  string(protocol.ActorAgent),
					"actorId":    "agy",
					"payload":    map[string]any{"text": "chunk-101"},
				},
			},
		},
	})
	sendRunnerJSON(t, ctx, runnerConn, protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  task2.Attempt.AttemptID,
		LeaseToken: task2.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{
				OutboxSeq: 1,
				EventID:   "ev_gap_other_state",
				FrameType: protocol.OutboxFrameState,
				TS:        now + 2,
				Payload:   map[string]any{"state": string(protocol.AttemptRunning)},
			},
		},
	})

	// Wait until clientConn receives task2's state event (proving Hub's lastSentSeq has already passed chunk-101!).
	var advancedGlobalSeq int64
	for {
		rctx, rcancel := context.WithTimeout(ctx, 4*time.Second)
		_, data, err := clientConn.Read(rctx)
		rcancel()
		if err != nil {
			t.Fatalf("read task2 sentinel: %v", err)
		}
		var msg protocol.WSServerMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type == protocol.WSMessageEvent && msg.Event != nil && msg.Event.EventID == "ev_gap_other_state" {
			advancedGlobalSeq = msg.Event.Seq
			break
		}
	}

	// 4. Now client dynamically subscribes to task1 -> Hub MUST emit watch_ack with seq >= advancedGlobalSeq.
	sendClientWSControl(t, ctx, clientConn, protocol.WSClientMessage{
		Type:       protocol.WSClientWatchAttempts,
		AttemptIDs: []string{task1.Attempt.AttemptID},
	})
	ack, _ := readUntilWatchAck(t, ctx, clientConn, []string{task1.Attempt.AttemptID})
	if ack.Seq < advancedGlobalSeq {
		t.Fatalf("expected watch_ack.Seq >= %d, got %d", advancedGlobalSeq, ack.Seq)
	}

	// 5. Runner emits chunk 3 ("chunk-102") -> WS pushes chunk-102 immediately BEFORE client completes HTTP catchup.
	sendRunnerJSON(t, ctx, runnerConn, protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  task1.Attempt.AttemptID,
		LeaseToken: task1.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{
				OutboxSeq: 3,
				EventID:   "ev_gap_c3",
				FrameType: protocol.OutboxFrameEvents,
				TS:        now + 3,
				Payload: map[string]any{
					"kind":       string(protocol.EventACPMessageBatch),
					"visibility": string(protocol.VisibilityRawACP),
					"actorKind":  string(protocol.ActorAgent),
					"actorId":    "agy",
					"payload":    map[string]any{"text": "chunk-102"},
				},
			},
		},
	})

	var wsChunk3 protocol.EventRecord
	for {
		rctx, rcancel := context.WithTimeout(ctx, 4*time.Second)
		_, data, err := clientConn.Read(rctx)
		rcancel()
		if err != nil {
			t.Fatalf("read live chunk-102: %v", err)
		}
		var msg protocol.WSServerMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type == protocol.WSMessageEvent && msg.Event != nil && msg.Event.EventID == "ev_gap_c3" {
			wsChunk3 = *msg.Event
			break
		}
	}

	// 6. Client honors watch_ack by catching up over HTTP starting from its independent httpCursor (NOT wsChunk3.Seq!).
	catchup := fetchAttemptRawHTTP(t, ts.URL, task1.Attempt.AttemptID, httpCursor)
	bySeq := map[int64]protocol.EventRecord{
		initialHTTP[0].Seq: initialHTTP[0],
		wsChunk3.Seq:       wsChunk3,
	}
	for _, ev := range catchup {
		bySeq[ev.Seq] = ev
	}
	var gotIDs []string
	var seqs []int64
	for seq := range bySeq {
		seqs = append(seqs, seq)
	}
	slices.Sort(seqs)
	for _, seq := range seqs {
		gotIDs = append(gotIDs, bySeq[seq].EventID)
	}
	wantIDs := []string{"ev_gap_c1", "ev_gap_c2", "ev_gap_c3"}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("gap recovery mismatch: got %v, want %v", gotIDs, wantIDs)
	}
}

func TestStore_MonotonicTaskVersionWatermarkAdvancesAcrossSkewedRunnerClocks(t *testing.T) {
	_, st, ts, runnerToken := setupSubsessionHub(t)
	ctx := context.Background()

	runnerWSURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/runner"
	runnerConn := dialTestRunnerWS(t, ctx, runnerWSURL, "hub-node", "ri_version_wm", runnerToken)
	defer runnerConn.CloseNow()

	parent, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		Title:         "Parent Watermark",
		Goal:          "Check monotonic version",
		MachineID:     "hub-node",
		Adapter:       protocol.AdapterAgy,
		CreateAttempt: true,
		LeaseTTLMs:    60000,
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		ParentTaskID:  parent.Task.TaskID,
		Title:         "Child Watermark",
		Goal:          "Skewed runner clock child",
		MachineID:     "hub-node",
		Adapter:       protocol.AdapterAgy,
		CreateAttempt: true,
		LeaseTTLMs:    60000,
	})
	if err != nil {
		t.Fatal(err)
	}

	if parent.Task.Version < 1 || child.Task.Version < 1 {
		t.Fatalf("expected initial versions >= 1, got parent=%d child=%d", parent.Task.Version, child.Task.Version)
	}
	vChildBefore := child.Task.Version

	// Runner reports attempt completion with a clock skewed 1 hour INTO THE PAST.
	pastSkewTS := time.Now().Add(-1 * time.Hour).UnixMilli()
	sendRunnerJSON(t, ctx, runnerConn, protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  child.Attempt.AttemptID,
		LeaseToken: child.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{
				OutboxSeq: 1,
				EventID:   "ev_child_skewed_done",
				FrameType: protocol.OutboxFrameState,
				TS:        pastSkewTS,
				Payload:   map[string]any{"state": string(protocol.AttemptSucceeded)},
			},
		},
	})

	var childAfter protocol.ChildTaskView
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		kids, err := st.ListChildTasks(ctx, parent.Task.TaskID, false)
		if err == nil && len(kids) == 1 && kids[0].State == protocol.TaskStateInReview {
			childAfter = kids[0]
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if childAfter.State != protocol.TaskStateInReview {
		t.Fatalf("child did not transition to in_review")
	}
	if childAfter.Version <= vChildBefore {
		t.Fatalf("expected child Version to strictly increase past %d despite skewed Runner TS, got %d", vChildBefore, childAfter.Version)
	}
	// Hub also keeps UpdatedAt on Hub server clock (not Runner's 1-hour-past TS).
	if childAfter.UpdatedAt <= pastSkewTS+30*60*1000 {
		t.Fatalf("expected Hub server UpdatedAt (not Runner pastSkewTS=%d), got %d", pastSkewTS, childAfter.UpdatedAt)
	}
}
