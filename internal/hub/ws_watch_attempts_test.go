package hub_test

import (
	"context"
	"encoding/json"
	"fmt"
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

	// watcherConn subscribes ONLY to task1's attempt; idleConn does NOT subscribe to any attempt.
	sendClientWSControl(t, ctx, watcherConn, protocol.WSClientMessage{
		Type:       protocol.WSClientWatchAttempts,
		AttemptIDs: []string{task1.Attempt.AttemptID},
	})

	// Wait briefly so Hub processes watch_attempts before outbox frames arrive (deterministic even without wait,
	// since SetWatchedAttempts wakes drainLive, but ensures watch_attempts is active for live push latency test).
	deadlineSub := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadlineSub) {
		if srv.EventBroker().AttemptWatcherCount(task1.Attempt.AttemptID) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
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
	if len(watcherRawKinds) != len(wantKinds) {
		t.Fatalf("watcherConn expected %v, got %v", wantKinds, watcherRawKinds)
	}
	for i := range wantKinds {
		if watcherRawKinds[i] != wantKinds[i] {
			t.Fatalf("watcherConn raw event[%d] = %s, want %s", i, watcherRawKinds[i], wantKinds[i])
		}
	}

	// 2. idleConn MUST receive ONLY task2's timeline sentinel and ZERO raw_acp events.
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

	// 3. Unwatching task1 stops pushing subsequent raw_acp events to watcherConn.
	sendClientWSControl(t, ctx, watcherConn, protocol.WSClientMessage{
		Type:       protocol.WSClientWatchAttempts,
		AttemptIDs: []string{},
	})
	deadlineUnsub := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadlineUnsub) {
		if srv.EventBroker().AttemptWatcherCount(task1.Attempt.AttemptID) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
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
