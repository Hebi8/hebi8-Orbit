package hub_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

func TestProjectWorktreePreferenceAndPAConversationReferences(t *testing.T) {
	srv, st, cfgDir, _ := setupTestHub(t, func(c *protocol.HubConfig) {
		c.DevToken = "test-dev-token"
	}, &fakeRyugiRunner{})

	rawRunnerTok := "test-runner-tok-123"
	sum := sha256.Sum256([]byte(rawRunnerTok))
	tokHash := "sha256:" + hex.EncodeToString(sum[:])

	machinesYAML := `schemaVersion: 1
machines:
  hub-node:
    displayName: "hub-node"
    transport: local_ws
    tokenHash: "` + tokHash + `"
    tailscaleHost: "orbit.example.ts.net"
    capabilities: [primary-dev]
`
	if err := os.WriteFile(filepath.Join(cfgDir, "machines.yaml"), []byte(machinesYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ReloadConfig(context.Background()); err != nil {
		t.Fatalf("reload config: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Connect a fake runner over WS so we can inspect the exact AttemptDispatchFrame prompts & isolation!
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/runner?machineId=hub-node"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + rawRunnerTok}},
	})
	if err != nil {
		t.Fatalf("dial runner ws: %v", err)
	}
	defer conn.CloseNow()

	hello, _ := json.Marshal(protocol.RunnerHelloFrame{
		Type:              protocol.RunnerMsgHello,
		ProtocolVersion:   1,
		MachineID:         "hub-node",
		RunnerInstanceID:  "ri_test_1",
		RunnerVersion:     "0.3.0-test",
		Capabilities:     []string{"primary-dev"},
		Adapters:         []protocol.Adapter{protocol.AdapterAgy, protocol.AdapterClaude},
	})
	if err := conn.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatal(err)
	}
	// Read welcome frame
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}

	authedReq := func(method, path string, body any) *http.Response {
		t.Helper()
		var r *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			r = bytes.NewReader(b)
		} else {
			r = bytes.NewReader(nil)
		}
		req, err := http.NewRequest(method, ts.URL+path, r)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Orbit-Dev-Token", "test-dev-token")
		req.Header.Set("X-Orbit-Request", "1")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	readDispatch := func() protocol.AttemptDispatchFrame {
		t.Helper()
		for {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				t.Fatalf("read ws frame: %v", err)
			}
			var env struct {
				Type protocol.RunnerWSMessageType `json:"type"`
			}
			_ = json.Unmarshal(raw, &env)
			if env.Type == protocol.RunnerMsgAttemptDispatch {
				var frame protocol.AttemptDispatchFrame
				if err := json.Unmarshal(raw, &frame); err != nil {
					t.Fatal(err)
				}
				return frame
			}
		}
	}

	// 1. Project task with useWorktree=false -> dispatched as in_place AND persisted on project location!
	useWTFalse := false
	resp := authedReq(http.MethodPost, "/api/projects/demo-app/tasks", protocol.CreateTaskRequest{
		Goal:        "修改 README 的架构说明章节",
		Adapter:     protocol.AdapterAgy,
		UseWorktree: &useWTFalse,
	})
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("create project task status=%d", resp.StatusCode)
	}
	var projRes protocol.CreateTaskResponse
	_ = json.NewDecoder(resp.Body).Decode(&projRes)
	resp.Body.Close()

	projFrame := readDispatch()
	if projFrame.Isolation != protocol.IsolationInPlace || projFrame.IsPlainFolder {
		t.Fatalf("expected git in_place isolation (isPlainFolder=false), got isolation=%q isPlainFolder=%v", projFrame.Isolation, projFrame.IsPlainFolder)
	}

	// Verify location isolation persisted as in_place in bootstrap
	bootResp := authedReq(http.MethodGet, "/api/bootstrap", nil)
	var boot protocol.BootstrapResponse
	_ = json.NewDecoder(bootResp.Body).Decode(&boot)
	bootResp.Body.Close()
	var demoProj *protocol.ProjectView
	for i := range boot.Projects {
		if boot.Projects[i].ProjectID == "demo-app" {
			demoProj = &boot.Projects[i]
		}
	}
	if demoProj == nil || len(demoProj.Parts) == 0 || len(demoProj.Parts[0].Locations) == 0 {
		t.Fatalf("expected demo-app project part/location in bootstrap")
	}
	if demoProj.Parts[0].Locations[0].Isolation != protocol.IsolationInPlace {
		t.Fatalf("expected persisted location isolation=in_place, got %q", demoProj.Parts[0].Locations[0].Isolation)
	}

	// Simulate an assistant reply event on the project task via the real outbox path so PA can cite its real text!
	if _, err := st.IngestOutboxBatch(ctx, "hub-node", "ri_test_1", protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  projRes.Attempt.AttemptID,
		LeaseToken: projFrame.LeaseToken,
		Items: []protocol.OutboxItem{
			{
				OutboxSeq: 1,
				EventID:   "ev_proj_reply_1",
				FrameType: protocol.OutboxFrameEvents,
				TS:        time.Now().UnixMilli(),
				Payload: map[string]any{
					"kind":       string(protocol.EventACPMessageBatch),
					"visibility": string(protocol.VisibilityRawACP),
					"actorKind":  string(protocol.ActorAgent),
					"actorId":    "agy",
					"payload": map[string]any{
						"text": "已完成 README 架构图更新，新增了 Token 鉴权流。",
					},
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// 2. Toggle useWorktree=true on next task -> dispatched as worktree AND persisted!
	useWTTrue := true
	resp2 := authedReq(http.MethodPost, "/api/projects/demo-app/tasks", protocol.CreateTaskRequest{
		Goal:        "在隔离 worktree 中重构认证模块",
		Adapter:     protocol.AdapterAgy,
		UseWorktree: &useWTTrue,
	})
	resp2.Body.Close()
	projFrame2 := readDispatch()
	if projFrame2.Isolation != protocol.IsolationWorktree {
		t.Fatalf("expected isolation=worktree, got %q", projFrame2.Isolation)
	}

	// 3. Create a real PA task referencing the first project task
	paResp := authedReq(http.MethodPost, "/api/projects/_pa/tasks", protocol.CreateTaskRequest{
		Goal:              "总结一下我引用的对话里改了什么",
		Adapter:           protocol.AdapterAgy,
		ReferencedTaskIDs: []string{projRes.Task.TaskID},
	})
	if paResp.StatusCode != http.StatusCreated && paResp.StatusCode != http.StatusOK {
		t.Fatalf("create PA task status=%d", paResp.StatusCode)
	}
	var paRes protocol.CreateTaskResponse
	_ = json.NewDecoder(paResp.Body).Decode(&paRes)
	paResp.Body.Close()

	paFrame := readDispatch()
	if paFrame.ProjectID != protocol.PAProjectID {
		t.Fatalf("expected _pa projectId, got %q", paFrame.ProjectID)
	}
	if paFrame.DesiredConfig["mode"] != "auto" {
		t.Fatalf("expected PA default mode=auto, got %q", paFrame.DesiredConfig["mode"])
	}
	if !strings.Contains(paFrame.Prompt, "已完成 README 架构图更新，新增了 Token 鉴权流") {
		t.Fatalf("expected referenced conversation assistant reply injected into PA prompt, got:\n%s", paFrame.Prompt)
	}

	// 4. Verify timeline returns refTasks metadata & GET /api/tasks lists tasks properly
	tlResp := authedReq(http.MethodGet, "/api/tasks/"+paRes.Task.TaskID+"/timeline", nil)
	var tl protocol.TaskTimelineResponse
	_ = json.NewDecoder(tlResp.Body).Decode(&tl)
	tlResp.Body.Close()
	if len(tl.Attempts) == 0 || len(tl.Attempts[0].RefTasks) != 1 || tl.Attempts[0].RefTasks[0].TaskID != projRes.Task.TaskID {
		t.Fatalf("expected AttemptView.RefTasks to contain %s, got %+v", projRes.Task.TaskID, tl.Attempts)
	}

	listResp := authedReq(http.MethodGet, "/api/tasks?excludeProject=_pa", nil)
	var listed struct {
		Tasks []protocol.TaskSummary `json:"tasks"`
	}
	_ = json.NewDecoder(listResp.Body).Decode(&listed)
	listResp.Body.Close()
	if len(listed.Tasks) < 2 {
		t.Fatalf("expected >=2 non-PA tasks from GET /api/tasks, got %d", len(listed.Tasks))
	}
	for _, tk := range listed.Tasks {
		if tk.ProjectID == protocol.PAProjectID {
			t.Fatalf("excludeProject=_pa should exclude PA tasks")
		}
	}
}
