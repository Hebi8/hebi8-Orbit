package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Hebi8/hebi8-Orbit/internal/gitops"
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

	// Verify /api/health exposes opaque discoveryId without leaking workspace details
	hReq := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	hRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(hRec, hReq)
	if hRec.Code != http.StatusOK {
		t.Fatalf("health failed: %d", hRec.Code)
	}
	var hDoc map[string]any
	if err := json.Unmarshal(hRec.Body.Bytes(), &hDoc); err != nil {
		t.Fatal(err)
	}
	if disc, _ := hDoc["discoveryId"].(string); !strings.HasPrefix(disc, "orb-") {
		t.Fatalf("expected opaque discoveryId on /api/health, got %+v", hDoc)
	}
	if _, leaked := hDoc["workspaceName"]; leaked {
		t.Fatalf("health endpoint must not leak workspaceName: %+v", hDoc)
	}

	// Verify loopback proxy matching helper handles localhost / 127.0.0.1 / [::1] equivalently
	if !sameLoopbackProxyTarget("http://localhost:4310", "127.0.0.1:4310") {
		t.Fatalf("expected localhost:4310 to match 127.0.0.1:4310")
	}
	if !sameLoopbackProxyTarget("http://[::1]:4310/", "127.0.0.1:4310") {
		t.Fatalf("expected [::1]:4310 to match 127.0.0.1:4310")
	}
	if sameLoopbackProxyTarget("http://127.0.0.1:4311", "127.0.0.1:4310") {
		t.Fatalf("different ports must not match")
	}
	if !isLoopbackOrigin("http://127.0.0.1:4310") || !isLoopbackOrigin("http://localhost:4311") {
		t.Fatalf("expected loopback origins to be detected")
	}
	if isLoopbackOrigin("https://example.ts.net") {
		t.Fatalf("tailnet origin must not be classified as loopback")
	}
}

func TestV4ProjectPartPatchEmptyPartWorkerAllowedRootsAndDirs(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	cfgDir := filepath.Join(tmpDir, "cfg")
	dataDir := filepath.Join(tmpDir, "data")
	workRoot := filepath.Join(tmpDir, "worker-root")
	subDir := filepath.Join(workRoot, "app-service")
	sampleFile := filepath.Join(workRoot, "readme.txt")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sampleFile, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	remoteTok := "worker-rpc-tok-v4"
	tokSum := sha256.Sum256([]byte(remoteTok))
	tokHash := "sha256:" + hex.EncodeToString(tokSum[:])

	if err := os.WriteFile(filepath.Join(cfgDir, "machines.yaml"), []byte(`schemaVersion: 1
machines:
  hub-node:
    displayName: "hub-node"
    transport: "local_ws"
    capabilities: ["primary-dev"]
  aux-node:
    displayName: "aux-node"
    transport: "remote_ws"
    tokenHash: "`+tokHash+`"
    capabilities: ["worker"]
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
			DevToken:   "v4-test-tok",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	doReq := func(method, path string, body any) *http.Response {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			if err := json.NewEncoder(&buf).Encode(body); err != nil {
				t.Fatal(err)
			}
		}
		req, err := http.NewRequest(method, ts.URL+path, &buf)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Orbit-Request", "1")
		req.Header.Set("X-Orbit-Dev-Token", "v4-test-tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// 1. Local hub-node without allowedRoots: path="" and path="~" both land on homePath
	for _, q := range []string{"", "?path=~"} {
		resp := doReq(http.MethodGet, "/api/machines/hub-node/dirs"+q, nil)
		var dirs protocol.ListDirsResponse
		_ = json.NewDecoder(resp.Body).Decode(&dirs)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || dirs.Path == "" || dirs.Path != dirs.HomePath {
			t.Fatalf("expected dirs(%q) to land on homePath, got status=%d %+v", q, resp.StatusCode, dirs)
		}
	}

	// 2. PATCH /api/machines/aux-node: set role=worker + allowedRoots=[workRoot] and verify returned
	workerRole := protocol.MachineRoleWorker
	resp := doReq(http.MethodPatch, "/api/machines/aux-node", protocol.UpdateMachineRequest{
		Role:         &workerRole,
		AllowedRoots: []string{workRoot},
	})
	var machList protocol.ListMachinesResponse
	_ = json.NewDecoder(resp.Body).Decode(&machList)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH worker machine failed: %d", resp.StatusCode)
	}
	foundAux := false
	for _, m := range machList.Machines {
		if m.MachineID == "aux-node" {
			foundAux = true
			if m.Role != protocol.MachineRoleWorker || len(m.AllowedRoots) != 1 || m.AllowedRoots[0] != workRoot {
				t.Fatalf("unexpected aux-node after PATCH: %+v", m)
			}
		}
	}
	if !foundAux {
		t.Fatalf("aux-node missing from PATCH response")
	}

	// 3. Connect a mock remote runner WS for aux-node implementing list_dirs RPC using gitops.ListSubdirs + ClassifyBrowseError
	wsCtx, wsCancel := context.WithTimeout(ctx, 15*time.Second)
	defer wsCancel()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/runner?machineId=aux-node"
	conn, _, err := websocket.Dial(wsCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + remoteTok}},
	})
	if err != nil {
		t.Fatalf("dial aux-node runner ws: %v", err)
	}
	defer conn.CloseNow()

	hello, _ := json.Marshal(protocol.RunnerHelloFrame{
		Type:             protocol.RunnerMsgHello,
		ProtocolVersion:  1,
		MachineID:        "aux-node",
		RunnerInstanceID: "ri_aux_1",
		RunnerVersion:    "0.4.0-test",
		Capabilities:     []string{"worker", protocol.RunnerCapFSV1, protocol.RunnerCapFSPlainV1},
	})
	if err := conn.Write(wsCtx, websocket.MessageText, hello); err != nil {
		t.Fatal(err)
	}
	// Read welcome frame
	if _, _, err := conn.Read(wsCtx); err != nil {
		t.Fatal(err)
	}

	// Serve runner RPC frames in background
	go func() {
		for {
			_, raw, err := conn.Read(wsCtx)
			if err != nil {
				return
			}
			var env struct {
				Type protocol.RunnerWSMessageType `json:"type"`
			}
			if json.Unmarshal(raw, &env) != nil || env.Type != protocol.RunnerMsgRPC {
				continue
			}
			var rpc protocol.RunnerRPCFrame
			if json.Unmarshal(raw, &rpc) != nil {
				continue
			}
			res := protocol.RunnerRPCResultFrame{
				Type:      protocol.RunnerMsgRPCResult,
				RequestID: rpc.RequestID,
			}
			if rpc.Op == "list_dirs" {
				pathArg, _ := rpc.Args["path"].(string)
				var roots []string
				if rawRoots, ok := rpc.Args["allowedRoots"].([]any); ok {
					for _, item := range rawRoots {
						if s, ok := item.(string); ok {
							roots = append(roots, s)
						}
					}
				}
				out, lErr := gitops.ListSubdirs(pathArg, roots, false)
				if lErr != nil {
					code, _ := gitops.ClassifyBrowseError(lErr)
					res.Error = lErr.Error()
					res.Data = map[string]any{"errorCode": code}
				} else {
					res.OK = true
					b, _ := json.Marshal(out)
					_ = json.Unmarshal(b, &res.Data)
				}
			}
			b, _ := json.Marshal(res)
			_ = conn.Write(wsCtx, websocket.MessageText, b)
		}
	}()

	// Remote worker with allowedRoots=[workRoot]:
	// - empty path lands cleanly on workRoot (does NOT fail with 403!)
	resp = doReq(http.MethodGet, "/api/machines/aux-node/dirs", nil)
	var workerDirs protocol.ListDirsResponse
	_ = json.NewDecoder(resp.Body).Decode(&workerDirs)
	resp.Body.Close()
	realWorkRoot, _ := filepath.EvalSymlinks(workRoot)
	if resp.StatusCode != http.StatusOK || workerDirs.Path != realWorkRoot || len(workerDirs.Entries) != 1 || workerDirs.Entries[0].Name != "app-service" {
		t.Fatalf("expected remote worker empty path to land on %s, got status=%d %+v", realWorkRoot, resp.StatusCode, workerDirs)
	}

	// - non-existent path inside allowed root -> 404 ERR_DIR_NOT_FOUND
	missingPath := filepath.Join(workRoot, "does-not-exist")
	resp = doReq(http.MethodGet, "/api/machines/aux-node/dirs?path="+url.QueryEscape(missingPath), nil)
	var errBody protocol.APIError
	_ = json.NewDecoder(resp.Body).Decode(&errBody)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || errBody.Code != "ERR_DIR_NOT_FOUND" {
		t.Fatalf("expected 404 ERR_DIR_NOT_FOUND, got %d %+v", resp.StatusCode, errBody)
	}

	// - file path inside allowed root -> 400 ERR_NOT_A_DIRECTORY
	resp = doReq(http.MethodGet, "/api/machines/aux-node/dirs?path="+url.QueryEscape(sampleFile), nil)
	errBody = protocol.APIError{}
	_ = json.NewDecoder(resp.Body).Decode(&errBody)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || errBody.Code != "ERR_NOT_A_DIRECTORY" {
		t.Fatalf("expected 400 ERR_NOT_A_DIRECTORY, got %d %+v", resp.StatusCode, errBody)
	}

	// - out-of-root path -> 403 ERR_PATH_OUTSIDE_ROOT
	resp = doReq(http.MethodGet, "/api/machines/aux-node/dirs?path=/etc", nil)
	errBody = protocol.APIError{}
	_ = json.NewDecoder(resp.Body).Decode(&errBody)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || errBody.Code != "ERR_PATH_OUTSIDE_ROOT" {
		t.Fatalf("expected 403 ERR_PATH_OUTSIDE_ROOT, got %d %+v", resp.StatusCode, errBody)
	}

	// - clearing allowedRoots via PATCH {"allowedRoots": []} persists empty list
	resp = doReq(http.MethodPatch, "/api/machines/aux-node", map[string]any{"allowedRoots": []string{}})
	machList = protocol.ListMachinesResponse{}
	_ = json.NewDecoder(resp.Body).Decode(&machList)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear allowedRoots failed: %d", resp.StatusCode)
	}
	for _, m := range machList.Machines {
		if m.MachineID == "aux-node" && len(m.AllowedRoots) != 0 {
			t.Fatalf("expected cleared allowedRoots, got %+v", m.AllowedRoots)
		}
	}

	// 4. PATCH /api/projects/{projectId}/parts/{partId} & DELETE last location keeps empty part
	if _, err := st.UpdateMachineMetadata(ctx, "hub-node", protocol.UpdateMachineRequest{
		AllowedRoots: []string{workRoot},
	}, "human"); err != nil {
		t.Fatal(err)
	}
	resp = doReq(http.MethodPost, "/api/projects", protocol.CreateProjectRequest{
		MachineID:   "hub-node",
		RepoPath:    subDir,
		DisplayName: "V4 Proj",
		AllowFolder: true,
	})
	var createdProj protocol.CreateProjectResponse
	_ = json.NewDecoder(resp.Body).Decode(&createdProj)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create project failed: %d", resp.StatusCode)
	}
	projID := createdProj.Project.ProjectID
	partID := createdProj.Project.Parts[0].PartID
	locID := createdProj.Project.Parts[0].Locations[0].LocationID

	renamed := "Renamed Service Part"
	order := 2
	resp = doReq(http.MethodPatch, "/api/projects/"+projID+"/parts/"+partID, protocol.UpdateProjectPartRequest{
		Name:      &renamed,
		SortOrder: &order,
	})
	var patchPartResp struct {
		Project protocol.ProjectView     `json:"project"`
		Part    protocol.ProjectPartView `json:"part"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&patchPartResp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || patchPartResp.Part.Name != renamed || patchPartResp.Part.SortOrder != 2 {
		t.Fatalf("PATCH part failed: status=%d %+v", resp.StatusCode, patchPartResp)
	}

	// Edit location absPath in-place via PATCH
	subDir2 := filepath.Join(workRoot, "app-service-v2")
	if err := os.MkdirAll(subDir2, 0o755); err != nil {
		t.Fatal(err)
	}
	resp = doReq(http.MethodPatch, "/api/projects/"+projID+"/parts/"+partID+"/locations/"+locID, protocol.UpdateProjectLocationRequest{
		AbsPath: &subDir2,
	})
	var patchLocResp struct {
		Project protocol.ProjectView `json:"project"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&patchLocResp)
	resp.Body.Close()
	realSubDir2, _ := filepath.EvalSymlinks(subDir2)
	if resp.StatusCode != http.StatusOK || len(patchLocResp.Project.Parts) != 1 || len(patchLocResp.Project.Parts[0].Locations) != 1 || patchLocResp.Project.Parts[0].Locations[0].AbsPath != realSubDir2 {
		t.Fatalf("PATCH location absPath failed: status=%d %+v", resp.StatusCode, patchLocResp)
	}

	// Delete the only location in the part -> part MUST survive with empty locations array
	resp = doReq(http.MethodDelete, "/api/projects/"+projID+"/parts/"+partID+"/locations/"+locID, nil)
	var delLocResp struct {
		Project protocol.ProjectView `json:"project"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&delLocResp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(delLocResp.Project.Parts) != 1 || len(delLocResp.Project.Parts[0].Locations) != 0 {
		t.Fatalf("expected empty part preserved after deleting last location, got status=%d %+v", resp.StatusCode, delLocResp.Project.Parts)
	}
}
