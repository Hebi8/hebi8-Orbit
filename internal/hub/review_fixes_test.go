package hub_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Hebi8/hebi8-Orbit/internal/hub"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

func setupSecuredHub(t *testing.T, devToken string) (*hub.Server, *store.Store, *httptest.Server, string) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "config")
	dataDir := filepath.Join(tmp, "data")
	_ = os.MkdirAll(cfgDir, 0o755)

	runnerTok := "hub-local-runner-secret"
	sum := sha256.Sum256([]byte(runnerTok))
	tokHash := "sha256:" + hex.EncodeToString(sum[:])

	machinesYAML := fmt.Sprintf(`schemaVersion: 1
machines:
  hub-node:
    displayName: "hub-node"
    transport: local_ws
    tokenHash: %q
    tailscaleHost: "orbit.example.ts.net"
    capabilities: [primary-dev]
    adapters:
      agy:
        bin: "agy"
`, tokHash)
	projectsYAML := `schemaVersion: 1
projects:
  demo-app:
    displayName: "demo-app"
    repoIdentity: "github.com/example-org/demo-app"
    defaultBranch: "main"
    defaultCapability: "primary-dev"
    defaultAdapter: "agy"
    anchors:
      primary-dev:
        machine: "hub-node"
        repoPath: "/tmp/demo-app"
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
	if devToken == "" {
		cfg.DevOpen = true
	} else {
		cfg.DevOpen = false
		cfg.DevToken = devToken
	}

	srv, err := hub.NewServer(ctx, hub.ServerOptions{
		HubConfig: cfg,
		Store:     st,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, st, ts, runnerTok
}

func doHubJSON(t *testing.T, ts *httptest.Server, method, path string, body any, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, ts.URL+path, r)
	if err != nil {
		t.Fatal(err)
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
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, raw
}

func TestB5_RunnerWSAuthAndOriginRejection(t *testing.T) {
	ctx := context.Background()
	_, _, ts, runnerTok := setupSecuredHub(t, "")
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/runner"

	// 1. Missing token on local_ws -> 401
	_, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"X-Orbit-Machine-Id": []string{"hub-node"}},
	})
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without bearer token on local_ws, err=%v resp=%v", err, resp)
	}

	// 2. Wrong token on local_ws -> 401
	_, resp, err = websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"X-Orbit-Machine-Id": []string{"hub-node"},
			"Authorization":      []string{"Bearer wrong-secret"},
		},
	})
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong token on local_ws, err=%v resp=%v", err, resp)
	}

	// 3. Valid token + browser Origin header -> 403 unconditionally rejected!
	_, resp, err = websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"X-Orbit-Machine-Id": []string{"hub-node"},
			"Authorization":      []string{"Bearer " + runnerTok},
			"Origin":             []string{"http://evil.example"},
		},
	})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 when Origin header present on /ws/runner, err=%v resp=%v", err, resp)
	}

	// 4. Valid token without Origin header -> upgrades cleanly!
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"X-Orbit-Machine-Id": []string{"hub-node"},
			"Authorization":      []string{"Bearer " + runnerTok},
		},
	})
	if err != nil {
		t.Fatalf("expected valid local_ws runner auth to succeed, got %v", err)
	}
	conn.CloseNow()
}

func TestB18_HubRejectsUnimplementedAdapters(t *testing.T) {
	_, _, ts, _ := setupSecuredHub(t, "")

	for _, adapter := range []protocol.Adapter{protocol.AdapterClaude, protocol.AdapterCodex} {
		resp, raw := doHubJSON(t, ts, http.MethodPost, "/api/projects/demo-app/tasks", protocol.CreateTaskRequest{
			Title:   "Try " + string(adapter),
			Goal:    "noop",
			Adapter: adapter,
		}, nil)
		if resp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("adapter=%s expected 501, got %d: %s", adapter, resp.StatusCode, string(raw))
		}
	}

	// PA create_task targeting unimplemented adapter MUST also return 501!
	respTh, rawTh := doHubJSON(t, ts, http.MethodPost, "/api/pa/threads", protocol.CreatePAThreadRequest{Title: "PA"}, nil)
	if respTh.StatusCode != http.StatusCreated {
		t.Fatalf("create pa thread failed: %s", string(rawTh))
	}
	var th struct {
		Thread protocol.PAThread `json:"thread"`
	}
	_ = json.Unmarshal(rawTh, &th)

	resp, raw := doHubJSON(t, ts, http.MethodPost, "/api/pa/chat", protocol.PAChatRequest{
		ThreadID: th.Thread.PAThreadID,
		Message:  "start claude task",
		Action: &protocol.PAChatActionRequest{
			Action:         "create_task",
			ProjectID:      "demo-app",
			Goal:           "try claude",
			Adapter:        protocol.AdapterClaude,
			IdempotencyKey: "pa-b18-claude",
		},
	}, nil)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("PA create_task adapter=claude expected 501, got %d: %s", resp.StatusCode, string(raw))
	}
}

func TestS5_PAWriteIdempotencyInvalidThreadAndResetConcurrency(t *testing.T) {
	_, st, ts, _ := setupSecuredHub(t, "")
	ctx := context.Background()

	// 1. Non-existent paThreadId with write action -> 404 BEFORE any side effect (ZERO tasks created)!
	resp, raw := doHubJSON(t, ts, http.MethodPost, "/api/pa/chat", protocol.PAChatRequest{
		ThreadID: "pat_does_not_exist",
		Message:  "create task on bad thread",
		Action: &protocol.PAChatActionRequest{
			Action: "create_task", ProjectID: "demo-app", Goal: "should not exist", IdempotencyKey: "k-bad-thread",
		},
	}, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for invalid paThreadId, got %d: %s", resp.StatusCode, string(raw))
	}
	var taskCount int
	_ = st.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks`).Scan(&taskCount)
	if taskCount != 0 {
		t.Fatalf("invalid paThreadId leaked side effect: created %d tasks", taskCount)
	}

	// Create valid PA thread
	respTh, rawTh := doHubJSON(t, ts, http.MethodPost, "/api/pa/threads", protocol.CreatePAThreadRequest{Title: "Main PA"}, nil)
	if respTh.StatusCode != http.StatusCreated {
		t.Fatalf("create thread failed: %s", string(rawTh))
	}
	var th struct {
		Thread protocol.PAThread `json:"thread"`
	}
	_ = json.Unmarshal(rawTh, &th)

	// 2. Write action without idempotencyKey -> 400!
	resp, raw = doHubJSON(t, ts, http.MethodPost, "/api/pa/chat", protocol.PAChatRequest{
		ThreadID: th.Thread.PAThreadID,
		Message:  "missing idem key",
		Action: &protocol.PAChatActionRequest{
			Action: "create_task", ProjectID: "demo-app", Goal: "missing key",
		},
	}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 without idempotencyKey, got %d: %s", resp.StatusCode, string(raw))
	}

	// 3. First write action with idempotencyKey succeeds -> creates 1 task.
	resp, raw = doHubJSON(t, ts, http.MethodPost, "/api/pa/chat", protocol.PAChatRequest{
		ThreadID: th.Thread.PAThreadID,
		Message:  "create once",
		Action: &protocol.PAChatActionRequest{
			Action: "create_task", ProjectID: "demo-app", Goal: "idempotent task", IdempotencyKey: "pa-idem-1",
		},
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first create_task failed (%d): %s", resp.StatusCode, string(raw))
	}

	// 4. Replay with same idempotencyKey + same action -> succeeds WITHOUT creating a duplicate task!
	resp, raw = doHubJSON(t, ts, http.MethodPost, "/api/pa/chat", protocol.PAChatRequest{
		ThreadID: th.Thread.PAThreadID,
		Message:  "create replay",
		Action: &protocol.PAChatActionRequest{
			Action: "create_task", ProjectID: "demo-app", Goal: "idempotent task", IdempotencyKey: "pa-idem-1",
		},
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replay create_task failed (%d): %s", resp.StatusCode, string(raw))
	}
	_ = st.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks`).Scan(&taskCount)
	if taskCount != 1 {
		t.Fatalf("idempotency replay created duplicate task: count=%d", taskCount)
	}

	// 5. Conflicting action reusing same idempotencyKey -> 409 Conflict!
	resp, raw = doHubJSON(t, ts, http.MethodPost, "/api/pa/chat", protocol.PAChatRequest{
		ThreadID: th.Thread.PAThreadID,
		Message:  "conflict action",
		Action: &protocol.PAChatActionRequest{
			Action: "cancel_task", TaskID: "tsk_fake", IdempotencyKey: "pa-idem-1",
		},
	}, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 on conflicting idempotencyKey action, got %d: %s", resp.StatusCode, string(raw))
	}

	// 6. Concurrent reset-session calls do not race!
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _ := doHubJSON(t, ts, http.MethodPost, "/api/pa/reset-session", nil, nil)
			if r.StatusCode != http.StatusOK && r.StatusCode != http.StatusCreated {
				t.Errorf("reset-session failed: %d", r.StatusCode)
			}
		}()
	}
	wg.Wait()
}

func TestS6_WSTicketSingleUseAndMachineStateEvent(t *testing.T) {
	ctx := context.Background()
	_, st, ts, _ := setupSecuredHub(t, "dev-secret-ui-token")
	authHdr := map[string]string{"X-Orbit-Dev-Token": "dev-secret-ui-token"}

	// 1. Issue single-use WS ticket via POST /api/ws-ticket
	resp, raw := doHubJSON(t, ts, http.MethodPost, "/api/ws-ticket", nil, authHdr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/ws-ticket failed (%d): %s", resp.StatusCode, string(raw))
	}
	var tick protocol.WSTicketResponse
	_ = json.Unmarshal(raw, &tick)
	if tick.Ticket == "" || tick.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("invalid ws ticket response: %+v", tick)
	}

	// 2. First /ws/client?ticket=... upgrades cleanly (WITHOUT putting devToken in query string!)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/client?ticket=" + tick.Ticket
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{ts.URL}},
	})
	if err != nil {
		t.Fatalf("first WS dial with single-use ticket failed: %v", err)
	}
	conn.CloseNow()

	// 3. Reusing the SAME ticket a second time MUST be rejected (401)!
	_, httpResp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{ts.URL}},
	})
	if err == nil || httpResp == nil || httpResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 reusing consumed WS ticket, err=%v resp=%v", err, httpResp)
	}

	// 4. SetMachineStatus emits machine.state_changed event
	ev, err := st.SetMachineStatus(ctx, "hub-node", protocol.MachineOffline)
	if err != nil || ev == nil || ev.Kind != protocol.EventMachineStateChanged {
		t.Fatalf("expected machine.state_changed event, got ev=%+v err=%v", ev, err)
	}
}

func TestB16_BoardWorkerStreamAndChildTaskRouting(t *testing.T) {
	ctx := context.Background()
	_, st, ts, _ := setupSecuredHub(t, "")

	// Create parent task and a virtual child worker task linked via parent_task_id + board_members.worker_task_id
	parent, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: "demo-app", Title: "Parent", Goal: "Coordinate", MachineID: "hub-node", Adapter: protocol.AdapterAgy, CreateAttempt: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: "demo-app", Title: "Worker Child", Goal: "Build UI", MachineID: "hub-node", Adapter: protocol.AdapterAgy, CreateAttempt: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	err = st.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET parent_task_id = ? WHERE task_id = ?`, parent.Task.TaskID, child.Task.TaskID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO board_members (task_id, project_id, member_name, machine_id, adapter, state, last_consumed_seq, worker_task_id, updated_at)
			VALUES (?, 'demo-app', 'child-worker', 'hub-node', 'agy', 'working', 0, ?, ?)
		`, parent.Task.TaskID, child.Task.TaskID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO board_members (task_id, project_id, member_name, machine_id, adapter, state, last_consumed_seq, worker_task_id, updated_at)
			VALUES (?, 'demo-app', 'native-worker', 'hub-node', 'agy', 'working', 0, NULL, ?)
		`, parent.Task.TaskID, now); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Post @child-worker on parent board -> routes followup to child task AND marks board message consumed!
	resp, raw := doHubJSON(t, ts, http.MethodPost, "/api/tasks/"+parent.Task.TaskID+"/board/messages", protocol.PostBoardMessageRequest{
		AuthorName:    "human",
		TargetMention: "@child-worker",
		Body:          "Please add dark mode",
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post board @child-worker failed (%d): %s", resp.StatusCode, string(raw))
	}
	var msg protocol.BoardMessage
	_ = json.Unmarshal(raw, &msg)
	if msg.Status != protocol.BoardMessageConsumed {
		t.Fatalf("expected board message to child-worker marked consumed, got %+v", msg)
	}

	// 2. Worker stream for child-worker reports privateStreamAvailable=true with child task events!
	resp, raw = doHubJSON(t, ts, http.MethodGet, "/api/tasks/"+parent.Task.TaskID+"/workers/child-worker/stream", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get child-worker stream failed (%d): %s", resp.StatusCode, string(raw))
	}
	var childStream protocol.WorkerStreamResponse
	_ = json.Unmarshal(raw, &childStream)
	if !childStream.PrivateStreamAvailable || len(childStream.Events) == 0 {
		t.Fatalf("expected child-worker privateStreamAvailable=true with events, got %+v", childStream)
	}

	// 3. Worker stream for native-worker honestly reports privateStreamAvailable=false + non-empty streamNotice!
	resp, raw = doHubJSON(t, ts, http.MethodGet, "/api/tasks/"+parent.Task.TaskID+"/workers/native-worker/stream", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get native-worker stream failed (%d): %s", resp.StatusCode, string(raw))
	}
	var nativeStream protocol.WorkerStreamResponse
	_ = json.Unmarshal(raw, &nativeStream)
	if nativeStream.PrivateStreamAvailable || nativeStream.StreamNotice == "" {
		t.Fatalf("expected native-worker privateStreamAvailable=false with streamNotice, got %+v", nativeStream)
	}
}
