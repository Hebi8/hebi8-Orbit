package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

func TestV3ProjectCompositionSettingsSuggestionsAndFolderPromote(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	cfgDir := filepath.Join(tmpDir, "cfg")
	dataDir := filepath.Join(tmpDir, "data")
	workRoot := filepath.Join(tmpDir, "work")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create a plain folder and a git repo inside workRoot
	plainDir := filepath.Join(workRoot, "design-docs")
	if err := os.MkdirAll(plainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plainDir, "notes.md"), []byte("# Notes\nhello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	repoDir := filepath.Join(workRoot, "orbit-demo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, string(out))
		}
	}
	runGit(repoDir, "init", "-b", "main")
	runGit(repoDir, "config", "user.email", "test@example.com")
	runGit(repoDir, "config", "user.name", "Test")
	runGit(repoDir, "remote", "add", "origin", "https://github.com/example/orbit-demo.git")
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(repoDir, "add", ".")
	runGit(repoDir, "commit", "-m", "init")

	if err := os.WriteFile(filepath.Join(cfgDir, "machines.yaml"), []byte(`schemaVersion: 1
machines:
  devNode:
    displayName: "devNode"
    transport: "local_ws"
    capabilities: ["primary-dev"]
`), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(dataDir, "orbit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv, err := NewServer(ctx, ServerOptions{
		Store: st,
		HubConfig: protocol.HubConfig{
			ListenAddr: "127.0.0.1:4310",
			DataDir:    dataDir,
			ConfigDir:  cfgDir,
			DevToken:   "v3-test-tok",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	if _, err := st.UpdateMachineMetadata(ctx, "devNode", protocol.UpdateMachineRequest{
		AllowedRoots: []string{workRoot},
	}, "human"); err != nil {
		t.Fatal(err)
	}

	doJSON := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			if err := json.NewEncoder(&buf).Encode(body); err != nil {
				t.Fatal(err)
			}
		}
		req := httptest.NewRequest(method, path, &buf)
		req.RemoteAddr = "127.0.0.1:43100"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Orbit-Request", "1")
		req.Header.Set("X-Orbit-Dev-Token", "v3-test-tok")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	// 1. Create a plain-folder project via AllowFolder=true
	rec := doJSON(http.MethodPost, "/api/projects", protocol.CreateProjectRequest{
		MachineID:   "devNode",
		RepoPath:    plainDir,
		DisplayName: "设计文档",
		AllowFolder: true,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create plain-folder project failed: %d %s", rec.Code, rec.Body.String())
	}
	var createResp protocol.CreateProjectResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &createResp); err != nil {
		t.Fatal(err)
	}
	projID := createResp.Project.ProjectID
	if len(createResp.Project.Parts) != 1 || createResp.Project.Parts[0].Kind != protocol.ProjectPartFolder {
		t.Fatalf("expected 1 folder part, got %+v", createResp.Project.Parts)
	}

	// 2. Add a second Git part to the same project
	rec = doJSON(http.MethodPost, "/api/projects/"+projID+"/parts", protocol.CreateProjectPartRequest{
		Name:      "主仓库",
		MachineID: "devNode",
		AbsPath:   repoDir,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("add git part failed: %d %s", rec.Code, rec.Body.String())
	}

	// 3. Patch project settings (SystemPrompt + SharedEnv)
	prompt := "Use concise markdown."
	rec = doJSON(http.MethodPatch, "/api/projects/"+projID, protocol.UpdateProjectSettingsRequest{
		SystemPrompt: &prompt,
		SharedEnv: []protocol.ProjectEnvVar{
			{Key: "API_BASE", Value: "https://example.test", Secret: false},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch project settings failed: %d %s", rec.Code, rec.Body.String())
	}

	// 4. Refresh locations & check runtime state populated
	rec = doJSON(http.MethodPost, "/api/projects/"+projID+"/locations/refresh", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh locations failed: %d %s", rec.Code, rec.Body.String())
	}

	// 5. Network settings dutyPriority & discoveryId
	rec = doJSON(http.MethodPatch, "/api/network/settings", protocol.UpdateNetworkSettingsRequest{
		DutyPriority: []string{"devNode"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch network settings failed: %d %s", rec.Code, rec.Body.String())
	}
	var netView protocol.NetworkSettingsView
	if err := json.Unmarshal(rec.Body.Bytes(), &netView); err != nil {
		t.Fatal(err)
	}
	if netView.DiscoveryID == "" || netView.ActiveDutyMachineID != "devNode" {
		t.Fatalf("unexpected network settings view: %+v", netView)
	}

	// 6. Pairing code carries role + discoveryId + 6-digit fingerprint
	rec = doJSON(http.MethodPost, "/api/pairing/codes", protocol.CreatePairingCodeRequest{
		Scope: protocol.PairingScopeBoth,
		Role:  protocol.MachineRoleWorker,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create pairing code failed: %d %s", rec.Code, rec.Body.String())
	}
	var pairResp protocol.CreatePairingCodeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &pairResp); err != nil {
		t.Fatal(err)
	}
	if pairResp.Role != protocol.MachineRoleWorker || pairResp.DiscoveryID == "" || len(pairResp.Fingerprint) != 7 {
		t.Fatalf("unexpected pairing response: %+v", pairResp)
	}

	// 7. Create a plain-folder task, simulate folderChanges, download file, and promote to Artifacts!
	rec = doJSON(http.MethodPost, "/api/projects/"+projID+"/tasks", protocol.CreateTaskRequest{
		Goal:       "Update notes.md",
		PartID:     createResp.Project.Parts[0].PartID,
		LocationID: createResp.Project.Parts[0].Locations[0].LocationID,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create plain-folder task failed: %d %s", rec.Code, rec.Body.String())
	}
	var taskResp protocol.CreateTaskResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &taskResp); err != nil {
		t.Fatal(err)
	}
	if taskResp.Attempt == nil || taskResp.Attempt.Isolation != protocol.IsolationInPlace {
		t.Fatalf("expected in_place attempt, got %+v", taskResp.Attempt)
	}
	// Simulate completed attempt worktreePath + folderChanges
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE attempts SET worktree_path = ? WHERE attempt_id = ?`, plainDir, taskResp.Attempt.AttemptID); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAttemptFolderChanges(ctx, taskResp.Attempt.AttemptID, protocol.FolderChangesSummary{
		TotalAdds: 2,
		TotalDels: 0,
		Files:     []protocol.FolderChangeItem{{Path: "notes.md", Kind: "add", Insertions: 2}},
	}); err != nil {
		t.Fatal(err)
	}

	// Download file
	dlReq := httptest.NewRequest(http.MethodGet, "/api/tasks/"+taskResp.Task.TaskID+"/folder-file?path=notes.md", nil)
	dlReq.RemoteAddr = "127.0.0.1:43100"
	dlReq.Header.Set("X-Orbit-Dev-Token", "v3-test-tok")
	dlRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(dlRec, dlReq)
	if dlRec.Code != http.StatusOK || dlRec.Body.String() != "# Notes\nhello\n" {
		t.Fatalf("folder file download failed: %d %q", dlRec.Code, dlRec.Body.String())
	}

	// Promote to Artifacts
	rec = doJSON(http.MethodPost, "/api/tasks/"+taskResp.Task.TaskID+"/folder-changes/promote", map[string]any{})
	if rec.Code != http.StatusCreated {
		t.Fatalf("promote folder changes failed: %d %s", rec.Code, rec.Body.String())
	}
	var promResp protocol.PromoteFolderChangesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &promResp); err != nil {
		t.Fatal(err)
	}
	if !promResp.Promoted || len(promResp.Delivery.Manifest.Blobs) != 1 {
		t.Fatalf("unexpected promote response: %+v", promResp)
	}

	// Confirm promoted artifact is listed in Global Artifacts!
	artRec := doJSON(http.MethodGet, "/api/artifacts", nil)
	if artRec.Code != http.StatusOK {
		t.Fatalf("list artifacts failed: %d %s", artRec.Code, artRec.Body.String())
	}
	var artList protocol.ListArtifactsResponse
	if err := json.Unmarshal(artRec.Body.Bytes(), &artList); err != nil {
		t.Fatal(err)
	}
	if len(artList.Artifacts) == 0 {
		t.Fatalf("expected promoted artifact in /api/artifacts, got empty")
	}
}
