package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/runner"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

func TestHebi8LineStore_WriteImmutableNodeRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	ls := newHebi8LineStore(dir)

	node := protocol.Hebi8LineNode{
		V:              1,
		ID:             "seg_deadbeef",
		Kind:           protocol.Hebi8NodeSegment,
		Seq:            1,
		CoversFrom:     "2026-11-26T00:00:00Z",
		CoversTo:       "2026-11-26T00:10:00Z",
		Level:          0,
		Title:          "不可变测试节点",
		SummaryQuality: protocol.Hebi8SummaryWritten,
		CreatedAt:      "2026-11-26T00:10:01Z",
	}
	if err := ls.WriteImmutableNode(node, "原始摘要正文。"); err != nil {
		t.Fatalf("first WriteImmutableNode failed: %v", err)
	}

	// Second write with same ID MUST fail with errHebi8NodeImmutable and preserve original text
	err := ls.WriteImmutableNode(node, "试图篡改后的正文。")
	if !errors.Is(err, errHebi8NodeImmutable) {
		t.Fatalf("expected errHebi8NodeImmutable on duplicate write, got %v", err)
	}
	md, err := ls.ReadSummary("seg_deadbeef")
	if err != nil {
		t.Fatalf("ReadSummary failed: %v", err)
	}
	if md != "原始摘要正文。\n" {
		t.Fatalf("expected original summary intact, got %q", md)
	}
}

func TestTrimAtSentenceBoundaryAndFoldFullChild(t *testing.T) {
	// P2-11: Even when the sentence terminator is in the first 35% of the window, cut at sentence boundary (never mid-sentence).
	text := "第一句很短。第二句超级无敌长长长长长长长长长长长长长长长长长长长长长长长长长长长长长长长长没有结束。"
	got := trimAtSentenceBoundary(text, 30)
	if got != "第一句很短。" {
		t.Fatalf("expected cut at first sentence boundary, got %q", got)
	}

	// P2-11: When even the first sentence does not fit in budget, return explicit omission marker instead of cutting mid-sentence.
	noPunct := strings.Repeat("字", 80)
	gotOmit := trimAtSentenceBoundary(noPunct, 20)
	if gotOmit != "[已省略]" {
		t.Fatalf("expected [已省略] when no sentence boundary fits, got %q", gotOmit)
	}

	// P2-10: synthesizeFoldSummary must NOT truncate child .md originals at 1400 chars
	longChild := "子摘要头部。" + strings.Repeat("详情内容。", 400) + "UNIQUE_CHILD_END_MARKER。"
	batch := []protocol.Hebi8LineNode{
		{ID: "seg_00000001", Title: "长子段", CoversFrom: "2026-11-26T00:00:00Z", CoversTo: "2026-11-26T01:00:00Z"},
	}
	_, foldMD := synthesizeFoldSummary(batch, []string{longChild}, 1)
	if !strings.Contains(foldMD, "UNIQUE_CHILD_END_MARKER") {
		t.Fatalf("P2-10 REGRESSION: synthesizeFoldSummary truncated child summary (len=%d)", len(foldMD))
	}

	// Item 8: Version numbers (v2.10) and decimals (3.14) must NOT be treated as sentence terminators!
	verText := "升级到 v2.10 并核对 3.14 阈值且没有句末标点继续很长很长很长"
	if gotVer := trimAtSentenceBoundary(verText, 18); gotVer != "[已省略]" {
		t.Fatalf("Item 8 REGRESSION: version/decimal dot was treated as sentence boundary, got %q", gotVer)
	}
	verWithPeriod := "已完成 v2.10 发布。第二句讨论 3.14 参数调整且非常非常长没有结束"
	if gotVer2 := trimAtSentenceBoundary(verWithPeriod, 24); gotVer2 != "已完成 v2.10 发布。" {
		t.Fatalf("Item 8 REGRESSION: expected full first sentence including v2.10, got %q", gotVer2)
	}

	// Item 6 / Final-3: isSafeDeliveryID allows _release and release.1 while blocking traversal/leading dots
	for _, good := range []string{"dlv_01ABC", "release.1", "_release", "-dash_1.2", "v2.0-rc_1"} {
		if !isSafeDeliveryID(good) {
			t.Fatalf("expected isSafeDeliveryID(%q) == true", good)
		}
	}
	for _, bad := range []string{"", ".hidden", "..", "a..b", "a/b", `a\b`, "a b"} {
		if isSafeDeliveryID(bad) {
			t.Fatalf("expected isSafeDeliveryID(%q) == false", bad)
		}
	}
}

func TestFailUndispatchedAttemptDetachedSucceedsWithCanceledRequestContext(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	cfgDir := filepath.Join(tmpDir, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "machines.yaml"), []byte(`schemaVersion: 1
machines:
  hub-node:
    displayName: Hub Node
    transport: local_ws
    capabilities: [primary-dev]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(dataDir, "orbit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	hubCfg := protocol.DefaultHubConfig()
	hubCfg.DataDir = dataDir
	hubCfg.ConfigDir = cfgDir
	hubCfg.DevOpen = true
	srv, err := NewServer(ctx, ServerOptions{HubConfig: hubCfg, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	cr, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:          protocol.ScratchProjectID,
		Title:              "Canceled ctx cleanup test",
		Goal:               "test detached fail",
		RequiredCapability: "primary-dev",
		Adapter:            protocol.AdapterAgy,
		MachineID:          "hub-node",
		CreateAttempt:      true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Cancel the request context BEFORE invoking failUndispatchedAttemptDetached!
	reqCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := srv.failUndispatchedAttemptDetached(reqCtx, cr.Attempt.AttemptID, "ERR_HEBI8_OPENING", "request context canceled during opening"); err != nil {
		t.Fatalf("expected detached fail to succeed even with canceled request ctx, got %v", err)
	}

	reloadedTask, err := st.GetTask(ctx, cr.Task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if reloadedTask.ActiveAttemptID != "" {
		t.Fatalf("expected activeAttemptId cleared, got %q", reloadedTask.ActiveAttemptID)
	}
	reloadedAtt, err := st.GetAttemptRecord(ctx, cr.Attempt.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if reloadedAtt.State != protocol.AttemptFailed {
		t.Fatalf("expected attempt state=failed, got %s", reloadedAtt.State)
	}
}

func TestPhase4_SchedulerPromptByRole(t *testing.T) {
	// 1. Top-level normal project session keeps orbit_spawn_child delegation instructions
	topPrompt := buildOrbitSystemPromptAppend("tsk_top", "demo-app", "att_1", false, "", "", "")
	if !strings.Contains(topPrompt, "orbit_spawn_child") {
		t.Fatalf("expected top-level session prompt to include orbit_spawn_child, got: %s", topPrompt)
	}

	// 2. Child session forbids grandchild spawning and allows only sibling collaboration tools
	childPrompt := buildOrbitSystemPromptAppend("tsk_child", "demo-app", "att_2", false, "", "tsk_top", "backend")
	if strings.Contains(childPrompt, "orbit_spawn_child") {
		t.Fatalf("child session prompt must NOT suggest orbit_spawn_child, got: %s", childPrompt)
	}
	for _, want := range []string{"orbit_list_children", "orbit_send_child", "orbit_read_child", "grandchild"} {
		if !strings.Contains(childPrompt, want) {
			t.Fatalf("expected child session prompt to contain %q, got: %s", want, childPrompt)
		}
	}

	// 3. Hebi8 / PA session must NOT inject orbit_spawn_child and keeps cross-directory coordination guidance
	paPrompt := buildOrbitSystemPromptAppend("tsk_pa", protocol.PAProjectID, "att_3", false, "", "", "")
	if strings.Contains(paPrompt, "orbit_spawn_child") {
		t.Fatalf("PA/Hebi8 prompt must NOT include orbit_spawn_child, got: %s", paPrompt)
	}
	if !strings.Contains(paPrompt, "Cross-Directory") {
		t.Fatalf("expected PA/Hebi8 prompt to preserve cross-directory guidance, got: %s", paPrompt)
	}
}

func initInternalGitRepo(t *testing.T, dir, originURL string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "init", "-b", "main"},
		{"git", "config", "user.name", "Orbit Test"},
		{"git", "config", "user.email", "user@example.com"},
		{"git", "remote", "add", "origin", originURL},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v failed: %v\n%s", args, err, string(out))
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"git", "add", "."}, {"git", "commit", "-m", "init"}} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v failed: %v\n%s", args, err, string(out))
		}
	}
}

func TestPhase4_RebindLocationDispatchFrameMatchesPrepareWorktree(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	workRoot := filepath.Join(tmp, "work")
	repoA := filepath.Join(workRoot, "repo-a")
	repoB := filepath.Join(workRoot, "repo-b")
	initInternalGitRepo(t, repoA, "https://github.com/example/repo-a.git")
	initInternalGitRepo(t, repoB, "https://github.com/example/repo-b.git")

	cfgDir := filepath.Join(tmp, "config")
	dataDir := filepath.Join(tmp, "data")
	wtDataDir := filepath.Join(tmp, "runner-data")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "machines.yaml"), []byte(`schemaVersion: 1
machines:
  hub-node:
    displayName: "Hub Node"
    transport: "local_ws"
    capabilities: ["primary-dev"]
`), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(tmp, "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv, err := NewServer(ctx, ServerOptions{
		Store: st,
		HubConfig: protocol.HubConfig{
			ListenAddr: "127.0.0.1:0",
			DataDir:    dataDir,
			ConfigDir:  cfgDir,
			DevToken:   "test-dev-token",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	if _, err := st.UpdateMachineMetadata(ctx, "hub-node", protocol.UpdateMachineRequest{
		AllowedRoots: []string{workRoot},
	}, "human"); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	doReq := func(method, path string, body any) (*http.Response, []byte) {
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		req, _ := http.NewRequest(method, ts.URL+path, &buf)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Orbit-Dev-Token", "test-dev-token")
		req.Header.Set("X-Orbit-Request", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out bytes.Buffer
		_, _ = out.ReadFrom(resp.Body)
		return resp, out.Bytes()
	}

	// 1. Create single-location project bound to repoA
	resp, raw := doReq(http.MethodPost, "/api/projects", protocol.CreateProjectRequest{
		MachineID:   "hub-node",
		RepoPath:    repoA,
		DisplayName: "Rebind Worktree Proj",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create project failed (%d): %s", resp.StatusCode, string(raw))
	}
	var created protocol.CreateProjectResponse
	_ = json.Unmarshal(raw, &created)
	projID := created.Project.ProjectID
	part1ID := created.Project.Parts[0].PartID
	loc1ID := created.Project.Parts[0].Locations[0].LocationID

	// 2. PATCH single-location part absPath -> repoB
	resp, raw = doReq(http.MethodPatch, "/api/projects/"+projID+"/parts/"+part1ID+"/locations/"+loc1ID, protocol.UpdateProjectLocationRequest{
		AbsPath: &repoB,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH to repoB failed (%d): %s", resp.StatusCode, string(raw))
	}

	proj, err := st.GetProject(ctx, projID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(proj.RepoIdentity, "repo-b") || !strings.Contains(proj.Parts[0].RepoIdentity, "repo-b") {
		t.Fatalf("expected both project and part repo_identity synced to repo-b, got proj=%q part=%q", proj.RepoIdentity, proj.Parts[0].RepoIdentity)
	}

	// 3. Create task/attempt targeting part1 (repoB), build real dispatch frame, and run REAL runner.PrepareWorktree
	taskRes, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     projID,
		Title:         "Run on repoB",
		Goal:          "verify worktree after rebind",
		MachineID:     "hub-node",
		Adapter:       protocol.AdapterClaude,
		PartID:        part1ID,
		LocationID:    loc1ID,
		Isolation:     protocol.IsolationWorktree,
		Anchor:        &protocol.ProjectAnchorConfig{Machine: "hub-node", RepoPath: repoB},
		CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	frame1, err := srv.buildAttemptDispatchFrame(ctx, *taskRes.Attempt, proj, protocol.ProjectAnchorConfig{Machine: "hub-node", RepoPath: repoB}, "", 0, "", "")
	if err != nil {
		t.Fatalf("buildAttemptDispatchFrame(part1) failed: %v", err)
	}
	if !strings.Contains(frame1.RepoIdentity, "repo-b") {
		t.Fatalf("expected dispatch frame RepoIdentity for part1 to contain repo-b, got %q", frame1.RepoIdentity)
	}
	wtPath1, sha1, err := runner.PrepareWorktree(ctx, frame1, wtDataDir)
	if err != nil {
		t.Fatalf("PrepareWorktree(part1/repoB) failed after rebind: %v", err)
	}
	if wtPath1 == "" || sha1 == "" {
		t.Fatalf("expected non-empty worktree path & sha for repoB, got path=%q sha=%q", wtPath1, sha1)
	}

	// 4. Add second Part bound to repoA (multi-part project with distinct repo_identity per part) and verify dispatch resolves part2's repoA!
	proj2, part2, _, err := st.CreateProjectPart(ctx, projID, "part-a", protocol.ProjectPartGit, "github.com/example/repo-a", "hub-node", repoA, &protocol.LocationRuntimeState{Exists: true, IsGit: true}, "human")
	if err != nil {
		t.Fatal(err)
	}
	loc2ID := part2.Locations[0].LocationID
	taskRes2, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     projID,
		Title:         "Run on part2 repoA",
		Goal:          "verify multi-part repoIdentity dispatch",
		MachineID:     "hub-node",
		Adapter:       protocol.AdapterClaude,
		PartID:        part2.PartID,
		LocationID:    loc2ID,
		Isolation:     protocol.IsolationWorktree,
		Anchor:        &protocol.ProjectAnchorConfig{Machine: "hub-node", RepoPath: repoA},
		CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	frame2, err := srv.buildAttemptDispatchFrame(ctx, *taskRes2.Attempt, proj2, protocol.ProjectAnchorConfig{Machine: "hub-node", RepoPath: repoA}, "", 0, "", "")
	if err != nil {
		t.Fatalf("buildAttemptDispatchFrame(part2) failed: %v", err)
	}
	if !strings.Contains(frame2.RepoIdentity, "repo-a") {
		t.Fatalf("expected dispatch frame RepoIdentity for part2 to contain repo-a, got %q", frame2.RepoIdentity)
	}
	wtPath2, sha2, err := runner.PrepareWorktree(ctx, frame2, wtDataDir)
	if err != nil {
		t.Fatalf("PrepareWorktree(part2/repoA) failed: %v", err)
	}
	if wtPath2 == "" || sha2 == "" {
		t.Fatalf("expected non-empty worktree path & sha for repoA, got path=%q sha=%q", wtPath2, sha2)
	}
}
