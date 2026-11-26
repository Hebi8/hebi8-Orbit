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
