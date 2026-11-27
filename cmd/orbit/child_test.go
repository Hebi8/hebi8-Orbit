package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

func TestCLITasksNewAndChildCommands_HubHTTPAndSocketProxy(t *testing.T) {
	ctx := context.Background()

	var gotCreateTask protocol.CreateTaskRequest
	var gotCreateChild protocol.CreateChildTaskRequest
	var gotFollowup protocol.FollowupTaskRequest
	var gotCancel protocol.CancelTaskRequest

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/projects/demo-app/tasks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotCreateTask)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(protocol.CreateTaskResponse{
			Task: protocol.TaskSummary{
				TaskID:           "tsk_sub_1",
				ProjectID:        "demo-app",
				ParentTaskID:     gotCreateTask.ParentTaskID,
				DelegatedRole:    gotCreateTask.DelegatedRole,
				State:            protocol.TaskStateQueued,
				PreferredAdapter: protocol.AdapterAgy,
			},
			Attempt: &protocol.AttemptView{AttemptID: "att_1", MachineID: gotCreateTask.MachineID},
		})
	})
	mux.HandleFunc("POST /api/tasks/tsk_parent/children", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotCreateChild)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(protocol.CreateTaskResponse{
			Task: protocol.TaskSummary{
				TaskID:           "tsk_child_http",
				ProjectID:        "demo-app",
				ParentTaskID:     "tsk_parent",
				DelegatedRole:    gotCreateChild.Role,
				State:            protocol.TaskStateRunning,
				PreferredAdapter: protocol.AdapterCodex,
			},
			Attempt: &protocol.AttemptView{AttemptID: "att_c1", MachineID: "hub-node"},
		})
	})
	mux.HandleFunc("GET /api/tasks/tsk_parent/children", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.ListChildrenResponse{
			ParentTaskID: "tsk_parent",
			Children: []protocol.ChildTaskView{
				{TaskID: "tsk_child_http", Title: "Worker A", DelegatedRole: "backend", State: protocol.TaskStateRunning, MachineID: "hub-node"},
			},
		})
	})
	mux.HandleFunc("GET /api/tasks/tsk_child_http/transcript", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.TaskTranscriptResponse{
			TaskID:    "tsk_child_http",
			ProjectID: "demo-app",
			Title:     "Worker A",
			State:     protocol.TaskStateInReview,
			Turns: []protocol.TaskTranscriptTurn{
				{AttemptID: "att_c1", AttemptNo: 1, TriggerKind: protocol.TriggerInitial, State: protocol.AttemptSucceeded, Prompt: "Do A", Reply: "Done A", ToolCount: 2},
			},
		})
	})
	mux.HandleFunc("POST /api/tasks/tsk_child_http/followup", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotFollowup)
		_ = json.NewEncoder(w).Encode(protocol.FollowupTaskResponse{Mode: gotFollowup.Mode})
	})
	mux.HandleFunc("POST /api/tasks/tsk_child_http/cancel", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotCancel)
		_ = json.NewEncoder(w).Encode(protocol.CancelTaskResponse{TaskID: "tsk_child_http", CancelSent: true})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 1. Test `orbit tasks new` with --parent, --machine, --role, --worktree=false
	var out, errOut bytes.Buffer
	err := run(ctx, []string{
		"tasks", "new",
		"--hub", srv.URL,
		"--project", "demo-app",
		"--parent", "tsk_parent",
		"--machine", "worker-1",
		"--role", "reviewer",
		"--worktree=false",
		"--goal", "Review PR",
	}, &out, &errOut)
	if err != nil {
		t.Fatalf("tasks new failed: %v (stderr=%s)", err, errOut.String())
	}
	if gotCreateTask.ParentTaskID != "tsk_parent" || gotCreateTask.MachineID != "worker-1" ||
		gotCreateTask.DelegatedRole != "reviewer" || gotCreateTask.UseWorktree == nil || *gotCreateTask.UseWorktree != false {
		t.Fatalf("unexpected CreateTaskRequest: %+v", gotCreateTask)
	}

	// 2. Test `orbit child` against Hub HTTP API
	out.Reset()
	if err := run(ctx, []string{"child", "spawn", "--hub", srv.URL, "--parent", "tsk_parent", "--role", "backend", "--goal", "Build API"}, &out, &errOut); err != nil {
		t.Fatalf("child spawn http failed: %v", err)
	}
	if !strings.Contains(out.String(), "tsk_child_http") || gotCreateChild.Role != "backend" {
		t.Fatalf("unexpected child spawn output/req: out=%q req=%+v", out.String(), gotCreateChild)
	}

	out.Reset()
	if err := run(ctx, []string{"child", "ls", "--hub", srv.URL, "--parent", "tsk_parent"}, &out, &errOut); err != nil {
		t.Fatalf("child ls http failed: %v", err)
	}
	if !strings.Contains(out.String(), "tsk_child_http") || !strings.Contains(out.String(), "Worker A") {
		t.Fatalf("unexpected child ls output: %q", out.String())
	}

	out.Reset()
	if err := run(ctx, []string{"child", "show", "--hub", srv.URL, "tsk_child_http", "--limit", "5"}, &out, &errOut); err != nil {
		t.Fatalf("child show http failed: %v", err)
	}
	if !strings.Contains(out.String(), "Done A") {
		t.Fatalf("unexpected child show output: %q", out.String())
	}

	out.Reset()
	if err := run(ctx, []string{"child", "send", "--hub", srv.URL, "tsk_child_http", "--steer", "--message", "Switch focus"}, &out, &errOut); err != nil {
		t.Fatalf("child send http failed: %v", err)
	}
	if gotFollowup.Mode != protocol.FollowupModeSteer || gotFollowup.Message != "Switch focus" {
		t.Fatalf("unexpected child send followup: %+v", gotFollowup)
	}

	out.Reset()
	if err := run(ctx, []string{"child", "cancel", "--hub", srv.URL, "tsk_child_http", "--reason", "abort"}, &out, &errOut); err != nil {
		t.Fatalf("child cancel http failed: %v", err)
	}
	if gotCancel.Reason != "abort" {
		t.Fatalf("unexpected child cancel: %+v", gotCancel)
	}

	// 3. Test `orbit child` auto-routing via ORBIT_RUNNER_SOCK + ORBIT_DELEGATE_TOKEN
	sockPath := filepath.Join(t.TempDir(), "delegate.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = ln.Close()
		_ = os.Remove(sockPath)
	}()

	var sockSpawnCalled bool
	sockMux := http.NewServeMux()
	sockMux.HandleFunc("/delegate/spawn", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Orbit-Delegate-Token") != "tok_env_123" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		sockSpawnCalled = true
		_ = json.NewEncoder(w).Encode(protocol.CreateTaskResponse{
			Task: protocol.TaskSummary{
				TaskID:           "tsk_child_sock",
				ParentTaskID:     "tsk_parent",
				ProjectID:        "demo-app",
				State:            protocol.TaskStateQueued,
				PreferredAdapter: protocol.AdapterAgy,
			},
		})
	})
	sockSrv := &http.Server{Handler: sockMux}
	go func() { _ = sockSrv.Serve(ln) }()
	defer func() { _ = sockSrv.Close() }()

	t.Setenv("ORBIT_RUNNER_SOCK", sockPath)
	t.Setenv("ORBIT_DELEGATE_TOKEN", "tok_env_123")

	out.Reset()
	if err := run(ctx, []string{"child", "spawn", "--goal", "Via socket"}, &out, &errOut); err != nil {
		t.Fatalf("child spawn via socket failed: %v", err)
	}
	if !sockSpawnCalled || !strings.Contains(out.String(), "tsk_child_sock") {
		t.Fatalf("expected child spawn to route through ORBIT_RUNNER_SOCK, called=%v out=%q", sockSpawnCalled, out.String())
	}
}

func TestCLIBoard_DelegateSocketAlwaysUsedWithParentFlag(t *testing.T) {
	ctx := context.Background()

	var httpHits int
	httpMux := http.NewServeMux()
	httpMux.HandleFunc("GET /api/tasks/tsk_ext/board", func(w http.ResponseWriter, r *http.Request) {
		httpHits++
		_ = json.NewEncoder(w).Encode(protocol.TaskBoardResponse{
			TaskID: "tsk_ext",
			Messages: []protocol.BoardMessage{
				{Seq: 1, MessageID: "bms_http_1", TaskID: "tsk_ext", AuthorName: "human", Body: "from http"},
			},
		})
	})
	httpMux.HandleFunc("POST /api/tasks/tsk_ext/board/messages", func(w http.ResponseWriter, r *http.Request) {
		httpHits++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(protocol.BoardMessage{
			Seq: 2, MessageID: "bms_http_2", TaskID: "tsk_ext", AuthorName: "human", Body: "posted http",
		})
	})
	httpSrv := httptest.NewServer(httpMux)
	defer httpSrv.Close()

	// 1. Outside delegate context (no ORBIT_RUNNER_SOCK): falls back to REST HTTP client.
	t.Setenv("ORBIT_RUNNER_SOCK", "")
	t.Setenv("ORBIT_DELEGATE_TOKEN", "")
	var out, errOut bytes.Buffer
	if err := run(ctx, []string{"child", "board", "read", "--hub", httpSrv.URL, "--parent", "tsk_ext"}, &out, &errOut); err != nil {
		t.Fatalf("outside-session board read failed: %v", err)
	}
	if !strings.Contains(out.String(), "from http") {
		t.Fatalf("unexpected REST board read output: %q", out.String())
	}
	out.Reset()
	if err := run(ctx, []string{"child", "board", "post", "--hub", httpSrv.URL, "--parent", "tsk_ext", "--body", "hi rest"}, &out, &errOut); err != nil {
		t.Fatalf("outside-session board post failed: %v", err)
	}
	if httpHits != 2 {
		t.Fatalf("expected 2 REST calls outside delegate context, got %d", httpHits)
	}

	// 2. Inside delegate context: EVEN WITH --parent specified, ALWAYS routes over Runner socket
	//    passing boardTaskId to Hub/Runner (preserving plan read-only & single-depth scope checks).
	sockPath := filepath.Join(t.TempDir(), "delegate-board.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = ln.Close()
		_ = os.Remove(sockPath)
	}()

	var gotReadPayload, gotPostPayload map[string]any
	blockPost := false
	sockMux := http.NewServeMux()
	sockMux.HandleFunc("/delegate/board-read", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Orbit-Delegate-Token") != "tok_board_1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		gotReadPayload = nil
		_ = json.NewDecoder(r.Body).Decode(&gotReadPayload)
		_ = json.NewEncoder(w).Encode(protocol.TaskBoardResponse{
			TaskID: "tsk_parent",
			Messages: []protocol.BoardMessage{
				{Seq: 5, MessageID: "bms_sock_1", TaskID: "tsk_parent", AuthorName: "Worker", Body: "from socket"},
			},
		})
	})
	sockMux.HandleFunc("/delegate/board-post", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Orbit-Delegate-Token") != "tok_board_1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		gotPostPayload = nil
		_ = json.NewDecoder(r.Body).Decode(&gotPostPayload)
		if blockPost {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"delegate action \"board-post\" is forbidden in plan mode"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(protocol.BoardMessage{
			Seq: 6, MessageID: "bms_sock_2", TaskID: "tsk_parent", AuthorName: "Worker", Body: "posted socket",
		})
	})
	sockSrv := &http.Server{Handler: sockMux}
	go func() { _ = sockSrv.Serve(ln) }()
	defer func() { _ = sockSrv.Close() }()

	t.Setenv("ORBIT_RUNNER_SOCK", sockPath)
	t.Setenv("ORBIT_DELEGATE_TOKEN", "tok_board_1")
	t.Setenv("ORBIT_HUB_URL", httpSrv.URL)

	out.Reset()
	if err := run(ctx, []string{"child", "board", "read", "--parent", "tsk_other_parent", "--limit", "15"}, &out, &errOut); err != nil {
		t.Fatalf("delegate board read with --parent failed: %v", err)
	}
	if httpHits != 2 {
		t.Fatalf("delegate board read bypassed socket and hit REST API! httpHits=%d", httpHits)
	}
	if gotReadPayload["boardTaskId"] != "tsk_other_parent" || gotReadPayload["limit"] != float64(15) {
		t.Fatalf("expected boardTaskId=tsk_other_parent via socket, got %+v", gotReadPayload)
	}

	out.Reset()
	if err := run(ctx, []string{"child", "board", "post", "--parent", "tsk_other_parent", "--target", "@parent", "--body", "sync check"}, &out, &errOut); err != nil {
		t.Fatalf("delegate board post with --parent failed: %v", err)
	}
	if httpHits != 2 {
		t.Fatalf("delegate board post bypassed socket and hit REST API! httpHits=%d", httpHits)
	}
	if gotPostPayload["boardTaskId"] != "tsk_other_parent" || gotPostPayload["targetMention"] != "@parent" || gotPostPayload["body"] != "sync check" {
		t.Fatalf("expected board-post payload with boardTaskId via socket, got %+v", gotPostPayload)
	}

	// Plan mode / Hub 403 rejection over socket must fail cleanly without falling back to REST.
	blockPost = true
	out.Reset()
	if err := run(ctx, []string{"child", "board", "post", "--parent", "tsk_other_parent", "--body", "forbidden in plan"}, &out, &errOut); err == nil {
		t.Fatal("expected error when delegate socket returns 403 in plan mode")
	}
	if httpHits != 2 {
		t.Fatalf("plan-mode rejection must not fall back to REST API, httpHits=%d", httpHits)
	}
}
