package hub_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hebi8/hebi8-Orbit/internal/hub"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

func setupPhase1Hub(t *testing.T, repoPath string) (*hub.Server, *store.Store, *httptest.Server) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "config")
	dataDir := filepath.Join(tmp, "data")
	_ = os.MkdirAll(cfgDir, 0o755)

	machinesYAML := `schemaVersion: 1
machines:
  hub-node:
    displayName: "hub-node"
    transport: local_ws
    tailscaleHost: "orbit.example.ts.net"
    capabilities: [primary-dev]
  deploy-vps:
    displayName: "deploy-vps"
    transport: ssh_outbound
    sshAlias: "deploy-vps"
    capabilities: [deploy-node]
`
	projectsYAML := `schemaVersion: 1
projects:
  demo-app:
    displayName: "demo-app"
    repoIdentity: "github.com/example-org/demo-app"
    defaultBranch: "main"
    defaultCapability: "primary-dev"
    defaultAdapter: "agy"
    defaultModelConfig:
      model: "gemini-4-neon"
      thought_level: "high"
      mode: "auto"
    ideScheme: "cursor"
    anchors:
      primary-dev:
        machine: "hub-node"
        repoPath: "` + filepath.ToSlash(repoPath) + `"
      deploy-node:
        machine: "deploy-vps"
        remoteDir: "/srv/demo-app"
        deployStrategy:
          kind: "ssh_push_and_switch"
          targetEnv: "deploy-vps:prod"
          requireHumanApproval: true
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
	return srv, st, ts
}

func doJSONReq(t *testing.T, method, url string, body any, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet && method != http.MethodHead {
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

func TestPhase1API_ScratchBindDirtyImportSourcesBoardQueue(t *testing.T) {
	// Prepare a real temporary git repo with dirty uncommitted changes
	repoDir := filepath.Join(t.TempDir(), "repo")
	_ = os.MkdirAll(repoDir, 0o755)
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=Tester", "GIT_AUTHOR_EMAIL=tester@example.com",
		"GIT_COMMITTER_NAME=Tester", "GIT_COMMITTER_EMAIL=tester@example.com",
	)
	runG := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = gitEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
		return string(out)
	}
	runG("init", "-b", "main")
	_ = os.WriteFile(filepath.Join(repoDir, "app.go"), []byte("package main\n"), 0o644)
	runG("add", ".")
	runG("commit", "-m", "init")
	_ = os.WriteFile(filepath.Join(repoDir, "app.go"), []byte("package main\n// dirty edit\n"), 0o644)
	statusBefore := runG("status", "--porcelain")

	_, _, ts := setupPhase1Hub(t, repoDir)

	// 1. Bootstrap returns _scratch, activeTasks, paThreads, paState, snapshotSeq
	resp, raw := doJSONReq(t, http.MethodGet, ts.URL+"/api/bootstrap", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bootstrap status %d: %s", resp.StatusCode, string(raw))
	}
	var boot protocol.BootstrapResponse
	_ = json.Unmarshal(raw, &boot)
	hasScratch := false
	for _, p := range boot.Projects {
		if p.ProjectID == protocol.ScratchProjectID {
			hasScratch = true
		}
	}
	if !hasScratch {
		t.Fatalf("expected _scratch in bootstrap projects: %+v", boot.Projects)
	}

	// 2. Create task in _scratch with Sources, then bind to demo-app
	resp, raw = doJSONReq(t, http.MethodPost, ts.URL+"/api/projects/_scratch/tasks", protocol.CreateTaskRequest{
		Goal:    "Explore feature",
		Adapter: protocol.AdapterManual, // idle so bind-project is allowed immediately
		Sources: []protocol.CreateTaskSourceInput{{Kind: protocol.TaskSourceLink, Label: "Spec", URIOrRef: "https://example.com/spec"}},
	}, map[string]string{"Idempotency-Key": "scratch-k1"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create scratch task failed (%d): %s", resp.StatusCode, string(raw))
	}
	var createdScratch protocol.CreateTaskResponse
	_ = json.Unmarshal(raw, &createdScratch)
	if createdScratch.Task.ProjectID != protocol.ScratchProjectID || len(createdScratch.Sources) != 1 {
		t.Fatalf("unexpected scratch task: %+v", createdScratch)
	}

	// Re-submit with same Idempotency-Key -> 200 Created=false
	resp, raw = doJSONReq(t, http.MethodPost, ts.URL+"/api/projects/_scratch/tasks", protocol.CreateTaskRequest{
		Goal:    "Explore feature",
		Adapter: protocol.AdapterManual,
	}, map[string]string{"Idempotency-Key": "scratch-k1"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on idempotent task replay, got %d: %s", resp.StatusCode, string(raw))
	}

	// Bind _scratch task -> demo-app
	resp, raw = doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+createdScratch.Task.TaskID+"/bind-project", protocol.BindScratchTaskRequest{
		ProjectID: "demo-app",
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bind-project failed (%d): %s", resp.StatusCode, string(raw))
	}
	var bindResp protocol.BindScratchTaskResponse
	_ = json.Unmarshal(raw, &bindResp)
	if bindResp.ToProjectID != "demo-app" || bindResp.Task.ProjectID != "demo-app" {
		t.Fatalf("unexpected bind response: %+v", bindResp)
	}

	// 3. Import dirty workspace on task creation -> zero pollution on repoDir!
	resp, raw = doJSONReq(t, http.MethodPost, ts.URL+"/api/projects/demo-app/tasks", protocol.CreateTaskRequest{
		Goal:                 "Continue from dirty workspace",
		ImportDirtyWorkspace: true,
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("dirty import create task failed (%d): %s", resp.StatusCode, string(raw))
	}
	if statusAfter := runG("status", "--porcelain"); statusBefore != statusAfter {
		t.Fatalf("dirty workspace import polluted source repo: before=%q after=%q", statusBefore, statusAfter)
	}
	var dirtyTask protocol.CreateTaskResponse
	_ = json.Unmarshal(raw, &dirtyTask)
	if dirtyTask.Attempt == nil || dirtyTask.Attempt.BaseSHA == "" {
		t.Fatalf("expected initial attempt with non-empty baseSha from dirty snapshot: %+v", dirtyTask)
	}

	// 4. Sources CRUD, Queue CRUD, Board + Worker Stream
	tid := dirtyTask.Task.TaskID
	resp, raw = doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+tid+"/sources", protocol.CreateTaskSourceInput{
		Kind: protocol.TaskSourceFile, Label: "app.go", URIOrRef: "app.go",
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add source failed (%d): %s", resp.StatusCode, string(raw))
	}
	var src protocol.TaskSource
	_ = json.Unmarshal(raw, &src)

	resp, _ = doJSONReq(t, http.MethodDelete, ts.URL+"/api/tasks/"+tid+"/sources/"+src.SourceID, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete source failed (%d)", resp.StatusCode)
	}

	resp, raw = doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+tid+"/board/messages", protocol.PostBoardMessageRequest{
		AuthorName:    "human",
		TargetMention: "@ui-coder",
		Body:          "Please update header alignment",
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post board msg failed (%d): %s", resp.StatusCode, string(raw))
	}
	resp, raw = doJSONReq(t, http.MethodGet, ts.URL+"/api/tasks/"+tid+"/board", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get board failed (%d): %s", resp.StatusCode, string(raw))
	}
	var board protocol.TaskBoardResponse
	_ = json.Unmarshal(raw, &board)
	if len(board.Messages) != 1 || board.Messages[0].TargetMention != "@ui-coder" {
		t.Fatalf("unexpected board response: %+v", board)
	}

	resp, raw = doJSONReq(t, http.MethodGet, ts.URL+"/api/tasks/"+tid+"/workers/ui-coder/stream", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("worker stream failed (%d): %s", resp.StatusCode, string(raw))
	}
	var wsResp protocol.WorkerStreamResponse
	_ = json.Unmarshal(raw, &wsResp)
	if len(wsResp.Events) == 0 {
		t.Fatalf("expected worker stream to include @ui-coder board message event")
	}

	// Queue CRUD while attempt is active
	resp, raw = doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+tid+"/queue", protocol.CreatePendingInputRequest{
		Body: "follow up A",
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("enqueue failed (%d): %s", resp.StatusCode, string(raw))
	}
	var q1 protocol.PendingInput
	_ = json.Unmarshal(raw, &q1)

	resp, raw = doJSONReq(t, http.MethodPatch, ts.URL+"/api/tasks/"+tid+"/queue/"+q1.InputID, protocol.UpdatePendingInputRequest{
		Body: "follow up A edited",
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch queue failed (%d): %s", resp.StatusCode, string(raw))
	}
	resp, _ = doJSONReq(t, http.MethodDelete, ts.URL+"/api/tasks/"+tid+"/queue/"+q1.InputID, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete queue failed (%d)", resp.StatusCode)
	}
}

func TestPhase1API_PABriefChatWatermarkAndSSHDeployGuard(t *testing.T) {
	srv, _, ts := setupPhase1Hub(t, t.TempDir())

	// 1. PA direct REST attempt to deploy-node MUST be blocked with 403 post-route!
	resp, raw := doJSONReq(t, http.MethodPost, ts.URL+"/api/projects/demo-app/tasks", protocol.CreateTaskRequest{
		Goal:               "Deploy directly to prod",
		RequiredCapability: "deploy-node",
	}, map[string]string{"X-Orbit-Actor-Kind": "pa"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for PA creating deploy-node task, got %d: %s", resp.StatusCode, string(raw))
	}

	// 2. PA chat action create_task targeting deploy-node MUST also be blocked with 403!
	resp, raw = doJSONReq(t, http.MethodPost, ts.URL+"/api/pa/chat", protocol.PAChatRequest{
		Message: "Deploy now",
		Action: &protocol.PAChatActionRequest{
			Action:             "create_task",
			ProjectID:          "demo-app",
			Goal:               "Direct deploy",
			RequiredCapability: "deploy-node",
		},
	}, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for PA chat deploy-node action, got %d: %s", resp.StatusCode, string(raw))
	}

	// 3. Create a normal task via PA chat -> allowed!
	resp, raw = doJSONReq(t, http.MethodPost, ts.URL+"/api/pa/chat", protocol.PAChatRequest{
		Message: "Please spin up a normal dev task",
		Action: &protocol.PAChatActionRequest{
			Action:         "create_task",
			ProjectID:      "demo-app",
			Goal:           "Refactor handler",
			IdempotencyKey: "pa-task-1",
		},
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for PA normal create_task, got %d: %s", resp.StatusCode, string(raw))
	}

	// 4. Test locked upper bound H: inject an H+1 event DURING PA chat turn processing!
	// Turn A locks H, hook creates a brand new task (producing H+1 event), Turn A commits watermark STRICTLY to H,
	// and Turn B MUST observe the H+1 event without skipping it!
	injected := false
	srv.SetPABeforeCommitHook(func(fromSeq, highSeq int64) {
		if injected {
			return
		}
		injected = true
		doJSONReq(t, http.MethodPost, ts.URL+"/api/projects/_scratch/tasks", protocol.CreateTaskRequest{
			Goal:    "Concurrent H+1 event during PA thinking",
			Adapter: protocol.AdapterManual,
		}, nil)
	})
	defer srv.SetPABeforeCommitHook(nil)

	resp, raw = doJSONReq(t, http.MethodPost, ts.URL+"/api/pa/chat", protocol.PAChatRequest{
		Message: "What happened so far?",
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PA turn A failed (%d): %s", resp.StatusCode, string(raw))
	}
	var turnA protocol.PAChatResponse
	_ = json.Unmarshal(raw, &turnA)
	if turnA.PAState.LastReadEventSeq != turnA.DeltaToSeq {
		t.Fatalf("expected PA watermark pinned to H=%d, got %d", turnA.DeltaToSeq, turnA.PAState.LastReadEventSeq)
	}

	// Turn B must pick up the H+1 event created while Turn A was thinking!
	resp, raw = doJSONReq(t, http.MethodPost, ts.URL+"/api/pa/chat", protocol.PAChatRequest{
		ThreadID: turnA.Thread.PAThreadID,
		Message:  "Anything new?",
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PA turn B failed (%d): %s", resp.StatusCode, string(raw))
	}
	var turnB protocol.PAChatResponse
	_ = json.Unmarshal(raw, &turnB)
	if turnB.DeltaToSeq <= turnA.DeltaToSeq || turnB.DeltaEventCount == 0 {
		t.Fatalf("H+1 event was skipped! turnA.H=%d turnB=(from=%d to=%d count=%d)",
			turnA.DeltaToSeq, turnB.DeltaFromSeq, turnB.DeltaToSeq, turnB.DeltaEventCount)
	}
	if !strings.Contains(turnB.AssistantMessage.Content, "task.created") {
		t.Fatalf("expected turn B delta to report concurrent task.created event, got: %s", turnB.AssistantMessage.Content)
	}

	// 5. Stateless Brief Me
	resp, raw = doJSONReq(t, http.MethodPost, ts.URL+"/api/pa/brief", protocol.PABriefRequest{SinceHours: 24}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("brief failed (%d): %s", resp.StatusCode, string(raw))
	}
	var brief protocol.PABriefResponse
	_ = json.Unmarshal(raw, &brief)
	if brief.SnapshotSeq == 0 || len(brief.ActiveTasks) == 0 {
		t.Fatalf("unexpected brief response: %+v", brief)
	}

	// 6. List / Get / Create PA threads
	resp, raw = doJSONReq(t, http.MethodGet, ts.URL+"/api/pa/threads/"+turnA.Thread.PAThreadID, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get PA thread failed (%d): %s", resp.StatusCode, string(raw))
	}
	var detail protocol.PAThreadDetailResponse
	_ = json.Unmarshal(raw, &detail)
	if len(detail.Messages) < 4 {
		t.Fatalf("expected >=4 PA messages across 2 turns, got %d", len(detail.Messages))
	}
}
