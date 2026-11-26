package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

func TestCLICommands_VersionTasksApprovalsPAArtifactsDoctor(t *testing.T) {
	ctx := context.Background()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"version":"0.2.0"}`))
	})
	mux.HandleFunc("GET /api/bootstrap", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.BootstrapResponse{
			WorkspaceName: "Home LAN",
			ActiveTasks: []protocol.TaskSummary{
				{TaskID: "tsk_1", ProjectID: "demo-app", Title: "Demo task", State: protocol.TaskStateRunning, PreferredAdapter: protocol.AdapterClaude},
			},
		})
	})
	mux.HandleFunc("POST /api/projects/demo-app/tasks", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Orbit-Request") != "1" {
			t.Errorf("missing X-Orbit-Request header")
		}
		var req protocol.CreateTaskRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Adapter != protocol.AdapterCodex || req.Config["thought_level"] != "high" {
			t.Errorf("unexpected create task payload: %+v", req)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(protocol.CreateTaskResponse{
			Task: protocol.TaskSummary{
				TaskID:           "tsk_new",
				ProjectID:        "demo-app",
				Goal:             req.Goal,
				State:            protocol.TaskStateQueued,
				PreferredAdapter: req.Adapter,
			},
			Attempt: &protocol.AttemptView{AttemptID: "att_1", MachineID: "hub-node"},
		})
	})
	mux.HandleFunc("GET /api/approvals", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"approvals": []protocol.ApprovalView{
				{
					ApprovalID:    "apr_1",
					ProjectID:     "demo-app",
					AttemptID:     "att_1",
					PayloadSHA256: "sha_demo",
					State:         protocol.ApprovalPending,
					ActionName:    "run deploy",
					Options:       []protocol.ApprovalOption{{ID: "allow_once_opt", Kind: "allow_once"}},
				},
			},
		})
	})
	mux.HandleFunc("POST /api/approvals/apr_1/decide", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.DecideApprovalRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.OptionID != "allow_once_opt" || req.ExpectedAttemptID != "att_1" || req.ExpectedPayloadSHA256 != "sha_demo" {
			t.Errorf("unexpected decide payload: %+v", req)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/projects/_pa/tasks", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.CreateTaskRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Goal != "status?" || len(req.ReferencedTaskIDs) != 1 || req.ReferencedTaskIDs[0] != "tsk_1" {
			t.Errorf("unexpected _pa create task payload: %+v", req)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(protocol.CreateTaskResponse{
			Task: protocol.TaskSummary{
				TaskID:    "tsk_pa_1",
				ProjectID: protocol.PAProjectID,
				Goal:      req.Goal,
				State:     protocol.TaskStateRunning,
			},
			Attempt: &protocol.AttemptView{AttemptID: "att_pa_1", State: protocol.AttemptRunning},
		})
	})
	mux.HandleFunc("GET /api/tasks/tsk_pa_1/timeline", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.TaskTimelineResponse{
			Task: protocol.TaskSummary{TaskID: "tsk_pa_1", ProjectID: protocol.PAProjectID, State: protocol.TaskStateDone},
			Attempts: []protocol.AttemptView{
				{AttemptID: "att_pa_1", State: protocol.AttemptSucceeded},
			},
		})
	})
	mux.HandleFunc("GET /api/attempts/att_pa_1/raw", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.AttemptRawEventsResponse{
			AttemptID: "att_pa_1",
			Events: []protocol.EventRecord{
				{Seq: 1, Kind: protocol.EventACPMessageBatch, Payload: map[string]any{"text": "一切正常，1 个任务在跑。"}},
			},
		})
	})
	mux.HandleFunc("POST /api/pa/brief", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.PABriefResponse{
			Summary: "工作区简报：1 个进行中任务",
		})
	})
	mux.HandleFunc("POST /api/pa/chat", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.PAChatResponse{
			AssistantMessage: protocol.PAMessage{Content: "legacy reply"},
		})
	})
	mux.HandleFunc("GET /api/artifacts", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.ListArtifactsResponse{
			Artifacts: []protocol.GlobalArtifactView{
				{ArtifactID: "blob:abc:plan.md", Kind: "markdown", Title: "plan.md", ProjectID: "demo-app", ProjectName: "Demo", Locator: "docs/plan.md"},
			},
		})
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	runCmd := func(args ...string) string {
		t.Helper()
		var out, errBuf bytes.Buffer
		if err := run(ctx, args, &out, &errBuf); err != nil {
			t.Fatalf("orbit %v failed: %v (stderr=%s)", args, err, errBuf.String())
		}
		return out.String()
	}

	if out := runCmd("version"); !strings.Contains(out, "orbit ") {
		t.Fatalf("unexpected version output: %q", out)
	}
	if out := runCmd("tasks", "list", "--hub", ts.URL); !strings.Contains(out, "tsk_1") {
		t.Fatalf("expected tsk_1 in tasks list: %q", out)
	}
	if out := runCmd("tasks", "new", "--hub", ts.URL, "--project", "demo-app", "--adapter", "codex", "--effort", "high", "--goal", "write tests"); !strings.Contains(out, "tsk_new") {
		t.Fatalf("expected tsk_new in tasks new: %q", out)
	}
	if out := runCmd("approvals", "allow", "--hub", ts.URL, "apr_1"); !strings.Contains(out, "allow_once_opt") {
		t.Fatalf("expected allow_once_opt in approvals allow: %q", out)
	}
	if out := runCmd("pa", "--hub", ts.URL, "--ref", "tsk_1", "status?"); !strings.Contains(out, "一切正常") {
		t.Fatalf("unexpected pa output: %q", out)
	}
	if out := runCmd("pa", "--hub", ts.URL, "--brief"); !strings.Contains(out, "工作区简报") {
		t.Fatalf("unexpected pa --brief output: %q", out)
	}
	if out := runCmd("artifacts", "--hub", ts.URL); !strings.Contains(out, "plan.md") {
		t.Fatalf("expected plan.md in artifacts: %q", out)
	}
	if out := runCmd("doctor", "--hub", ts.URL); !strings.Contains(out, "[OK  ] hub") {
		t.Fatalf("expected hub OK in doctor: %q", out)
	}
	if out := runCmd("open", "--hub", ts.URL, "--print"); strings.TrimSpace(out) != ts.URL {
		t.Fatalf("expected %q from open --print, got %q", ts.URL, out)
	}
}

func TestResolveHubDefaultsReadsClientYAML(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "client.yaml")
	if err := os.WriteFile(cfgPath, []byte("hubUrl: https://orbit.example.ts.net/\n"), 0o600); err != nil {
		t.Fatalf("write client.yaml: %v", err)
	}
	t.Setenv("ORBIT_HUB_URL", "")
	t.Setenv("ORBIT_DEV_TOKEN", "")
	hubURL, _, _ := resolveHubDefaults(dir, "", "", "")
	if hubURL != "https://orbit.example.ts.net" {
		t.Fatalf("expected client.yaml hubUrl, got %q", hubURL)
	}
}

func TestCLIHebi8Commands(t *testing.T) {
	ctx := context.Background()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/hebi8/main", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.Hebi8MainResponse{
			Task: protocol.TaskSummary{
				TaskID:    "tsk_hebi8_main",
				ProjectID: protocol.PAProjectID,
				Title:     "Hebi8",
				State:     protocol.TaskStateDone,
			},
		})
	})
	mux.HandleFunc("POST /api/tasks/tsk_hebi8_main/followup", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.FollowupTaskRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Message != "hello hebi8" {
			t.Errorf("unexpected hebi8 message: %q", req.Message)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(protocol.FollowupTaskResponse{
			Mode:       protocol.FollowupModeQueue,
			Task:       protocol.TaskSummary{TaskID: "tsk_hebi8_main", ProjectID: protocol.PAProjectID, State: protocol.TaskStateRunning},
			Attempt:    &protocol.AttemptView{AttemptID: "att_h1", State: protocol.AttemptRunning},
			Dispatched: true,
		})
	})
	mux.HandleFunc("GET /api/tasks/tsk_hebi8_main/timeline", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.TaskTimelineResponse{
			Task: protocol.TaskSummary{TaskID: "tsk_hebi8_main", ProjectID: protocol.PAProjectID, State: protocol.TaskStateDone},
			Attempts: []protocol.AttemptView{
				{AttemptID: "att_h1", State: protocol.AttemptSucceeded},
			},
		})
	})
	mux.HandleFunc("GET /api/attempts/att_h1/raw", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.AttemptRawEventsResponse{
			AttemptID: "att_h1",
			Events: []protocol.EventRecord{
				{Seq: 1, Kind: protocol.EventACPMessageBatch, Payload: map[string]any{"text": "收到，我是 Hebi8。"}},
			},
		})
	})
	mux.HandleFunc("POST /api/hebi8/fold", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(protocol.Hebi8FoldResponse{
			TaskID: "tsk_hebi8_main",
			Segment: protocol.Hebi8LineNode{
				ID:           "seg_11223344",
				Kind:         protocol.Hebi8NodeSegment,
				Title:        "阶段复盘",
				AttemptRange: &protocol.Hebi8AttemptRange{TurnCount: 3},
			},
		})
	})
	mux.HandleFunc("GET /api/hebi8/line/seg_11223344/expand", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.Hebi8ExpandResponse{
			Node: protocol.Hebi8LineNode{
				ID:    "seg_11223344",
				Kind:  protocol.Hebi8NodeSegment,
				Level: 0,
				Title: "阶段复盘",
			},
			Summary:    "我整理了三个核心结论。",
			TotalTurns: 1,
			Turns: []protocol.Hebi8RawTurn{
				{AttemptID: "att_h1", AttemptNo: 1, UserPrompt: "hello hebi8", AssistantReply: "收到，我是 Hebi8。"},
			},
		})
	})
	mux.HandleFunc("POST /api/hebi8/line/seg_11223344/pin", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.Hebi8PinRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		pins := []string{}
		if req.Pinned {
			pins = []string{"seg_11223344"}
		}
		_ = json.NewEncoder(w).Encode(protocol.Hebi8PinResponse{
			NodeID: "seg_11223344",
			Pinned: req.Pinned,
			Pins:   pins,
		})
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	runCmd := func(args ...string) string {
		t.Helper()
		var out, errBuf bytes.Buffer
		if err := run(ctx, args, &out, &errBuf); err != nil {
			t.Fatalf("orbit %v failed: %v (stderr=%s)", args, err, errBuf.String())
		}
		return out.String()
	}

	if out := runCmd("hebi8", "--hub", ts.URL, "hello hebi8"); !strings.Contains(out, "我是 Hebi8") {
		t.Fatalf("unexpected hebi8 chat output: %q", out)
	}
	if out := runCmd("pa", "--hub", ts.URL, "hello hebi8"); !strings.Contains(out, "我是 Hebi8") {
		t.Fatalf("unexpected pa alias output: %q", out)
	}
	if out := runCmd("hebi8", "fold", "--hub", ts.URL); !strings.Contains(out, "seg_11223344") {
		t.Fatalf("unexpected hebi8 fold output: %q", out)
	}
	if out := runCmd("hebi8", "expand", "--hub", ts.URL, "seg_11223344"); !strings.Contains(out, "我整理了三个核心结论") || !strings.Contains(out, "hello hebi8") {
		t.Fatalf("unexpected hebi8 expand output: %q", out)
	}
	if out := runCmd("hebi8", "pin", "--hub", ts.URL, "seg_11223344"); !strings.Contains(out, "Pinned seg_11223344") {
		t.Fatalf("unexpected hebi8 pin output: %q", out)
	}
	if out := runCmd("hebi8", "pin", "--hub", ts.URL, "--unpin", "seg_11223344"); !strings.Contains(out, "Unpinned seg_11223344") {
		t.Fatalf("unexpected hebi8 unpin output: %q", out)
	}
}

func TestCLIHebi8MainErrorDoesNotFallbackAndPassesMachine(t *testing.T) {
	ctx := context.Background()

	// 1. 500 on GET /api/hebi8/main MUST return error and NEVER fall back to POST /api/projects/_pa/tasks
	createdLegacy := false
	mux500 := http.NewServeMux()
	mux500.HandleFunc("GET /api/hebi8/main", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"code":"ERR_INTERNAL","message":"db busy"}`, http.StatusInternalServerError)
	})
	mux500.HandleFunc("POST /api/projects/_pa/tasks", func(w http.ResponseWriter, r *http.Request) {
		createdLegacy = true
		http.Error(w, "should not be called", http.StatusTeapot)
	})
	ts500 := httptest.NewServer(mux500)
	defer ts500.Close()

	var out, errBuf bytes.Buffer
	if err := run(ctx, []string{"hebi8", "--hub", ts500.URL, "--no-wait", "hi"}, &out, &errBuf); err == nil {
		t.Fatalf("expected error on 500 from /api/hebi8/main")
	}
	if createdLegacy {
		t.Fatalf("P2-16 REGRESSION: orbit hebi8 fell back to POST /api/projects/_pa/tasks on HTTP 500")
	}

	// 2. 404 on GET /api/hebi8/main falls back cleanly to POST /api/projects/_pa/tasks
	mux404 := http.NewServeMux()
	mux404.HandleFunc("GET /api/hebi8/main", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux404.HandleFunc("POST /api/projects/_pa/tasks", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(protocol.CreateTaskResponse{
			Task:    protocol.TaskSummary{TaskID: "tsk_legacy_pa"},
			Attempt: &protocol.AttemptView{AttemptID: "att_l1"},
		})
	})
	ts404 := httptest.NewServer(mux404)
	defer ts404.Close()

	out.Reset()
	errBuf.Reset()
	if err := run(ctx, []string{"hebi8", "--hub", ts404.URL, "--no-wait", "hi"}, &out, &errBuf); err != nil {
		t.Fatalf("expected 404 legacy fallback to succeed, got %v", err)
	}
	if !strings.Contains(out.String(), "tsk_legacy_pa") {
		t.Fatalf("expected tsk_legacy_pa in output, got %q", out.String())
	}

	// 3. P1-7: --machine flag is forwarded in FollowupTaskRequest.MachineID
	var gotMachine string
	muxMachine := http.NewServeMux()
	muxMachine.HandleFunc("GET /api/hebi8/main", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.Hebi8MainResponse{
			Task: protocol.TaskSummary{TaskID: "tsk_hebi8_main", ProjectID: protocol.PAProjectID},
		})
	})
	muxMachine.HandleFunc("POST /api/tasks/tsk_hebi8_main/followup", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.FollowupTaskRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotMachine = req.MachineID
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(protocol.FollowupTaskResponse{
			Mode:    protocol.FollowupModeQueue,
			Task:    protocol.TaskSummary{TaskID: "tsk_hebi8_main"},
			Attempt: &protocol.AttemptView{AttemptID: "att_m1"},
		})
	})
	tsMachine := httptest.NewServer(muxMachine)
	defer tsMachine.Close()

	out.Reset()
	errBuf.Reset()
	if err := run(ctx, []string{"hebi8", "--hub", tsMachine.URL, "--machine", "gpu-box", "--no-wait", "hi"}, &out, &errBuf); err != nil {
		t.Fatalf("expected --machine followup to succeed, got %v", err)
	}
	if gotMachine != "gpu-box" {
		t.Fatalf("P1-7 REGRESSION: expected MachineID=gpu-box, got %q", gotMachine)
	}
}
