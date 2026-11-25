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
