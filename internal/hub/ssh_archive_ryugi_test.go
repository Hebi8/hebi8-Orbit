package hub_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/gitops"
	"github.com/Hebi8/hebi8-Orbit/internal/hub"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/sshcfg"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

type funcRyugiRunner struct {
	fn func(ctx context.Context, args ...string) ([]byte, error)
}

func (f funcRyugiRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	return f.fn(ctx, args...)
}

func TestDefaultRyugiPortIsolated(t *testing.T) {
	cfg := protocol.DefaultHubConfig()
	if cfg.RyugiPort != 27329 {
		t.Fatalf("expected DefaultHubConfig().RyugiPort == 27329, got %d", cfg.RyugiPort)
	}
}

func TestHubSSHCandidatesLoopbackVsRemote(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(tmpDir, "orbit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfgDir := filepath.Join(tmpDir, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	machinesYAML := `schemaVersion: 1
machines:
  gpu-box:
    displayName: GPU Box
    transport: remote_ws
    tailscaleHost: gpu-box.example.ts.net
    capabilities: [gpu]
`
	if err := os.WriteFile(filepath.Join(cfgDir, "machines.yaml"), []byte(machinesYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	sshDir := filepath.Join(tmpDir, "ssh")
	if err := os.MkdirAll(sshDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sshCfgPath := filepath.Join(sshDir, "config")
	if err := os.WriteFile(sshCfgPath, []byte("Host gpu-alias\n  HostName gpu-box.example.ts.net\n  User dev\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	hubCfg := protocol.DefaultHubConfig()
	hubCfg.DataDir = tmpDir
	hubCfg.ConfigDir = cfgDir
	hubCfg.DevOpen = true

	srv, err := hub.NewServer(ctx, hub.ServerOptions{
		HubConfig: hubCfg,
		Store:     st,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	srv.SetSSHConfigOptions(sshcfg.MatchOptions{
		ConfigPath:     sshCfgPath,
		SSHDir:         sshDir,
		DisableSSHExec: true,
		DisableProbe:   true,
		DisableDNS:     true,
	})

	// 1. Direct loopback request resolves local ~/.ssh/config
	reqLocal := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4310/api/machines/gpu-box/ssh-candidates", nil)
	reqLocal.RemoteAddr = "127.0.0.1:51234"
	recLocal := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recLocal, reqLocal)
	if recLocal.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recLocal.Code, recLocal.Body.String())
	}
	var localResp protocol.SSHCandidatesResponse
	if err := json.Unmarshal(recLocal.Body.Bytes(), &localResp); err != nil {
		t.Fatal(err)
	}
	if !localResp.LocalConfigAvailable || localResp.Recommended != "gpu-alias" {
		t.Fatalf("expected localConfigAvailable=true & recommended=gpu-alias, got %+v", localResp)
	}

	// 2. Proxied/remote browser request MUST return localConfigAvailable=false
	rawToken, _, err := st.CreateClientToken(ctx, "Remote Browser", "client", "")
	if err != nil {
		t.Fatal(err)
	}
	reqRemote := httptest.NewRequest(http.MethodGet, "https://hub.example.ts.net/api/machines/gpu-box/ssh-candidates", nil)
	reqRemote.RemoteAddr = "127.0.0.1:51235"
	reqRemote.Header.Set("Authorization", "Bearer "+rawToken)
	reqRemote.Header.Set("X-Forwarded-For", "198.51.100.42")
	recRemote := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recRemote, reqRemote)
	if recRemote.Code != http.StatusOK {
		t.Fatalf("expected 200 for remote request, got %d: %s", recRemote.Code, recRemote.Body.String())
	}
	var remoteResp protocol.SSHCandidatesResponse
	if err := json.Unmarshal(recRemote.Body.Bytes(), &remoteResp); err != nil {
		t.Fatal(err)
	}
	if remoteResp.LocalConfigAvailable || remoteResp.Recommended != "" || len(remoteResp.AllHosts) != 0 {
		t.Fatalf("remote request must not leak Hub's ~/.ssh/config, got %+v", remoteResp)
	}
}

func TestHubTaskArchiveCancelActiveAndDeleteAPI(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(tmpDir, "orbit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	hubCfg := protocol.DefaultHubConfig()
	hubCfg.DataDir = tmpDir
	hubCfg.DevOpen = true

	srv, err := hub.NewServer(ctx, hub.ServerOptions{
		HubConfig: hubCfg,
		Store:     st,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	// Create an active _scratch task
	created, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     protocol.ScratchProjectID,
		Title:         "Active Chat",
		Goal:          "Run something",
		Adapter:       protocol.AdapterAgy,
		CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	tid := created.Task.TaskID

	// 1. Archive without cancelActive -> 409 ERR_TASK_ACTIVE
	reqArchFail := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4310/api/tasks/"+tid+"/archive", bytes.NewReader([]byte(`{}`)))
	reqArchFail.RemoteAddr = "127.0.0.1:50001"
	reqArchFail.Header.Set("X-Orbit-Request", "1")
	recArchFail := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recArchFail, reqArchFail)
	if recArchFail.Code != http.StatusConflict || !strings.Contains(recArchFail.Body.String(), "ERR_TASK_ACTIVE") {
		t.Fatalf("expected 409 ERR_TASK_ACTIVE, got %d: %s", recArchFail.Code, recArchFail.Body.String())
	}

	// 2. Archive with cancelActive=true -> 200 OK
	reqArchOK := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4310/api/tasks/"+tid+"/archive", bytes.NewReader([]byte(`{"cancelActive":true}`)))
	reqArchOK.RemoteAddr = "127.0.0.1:50002"
	reqArchOK.Header.Set("X-Orbit-Request", "1")
	recArchOK := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recArchOK, reqArchOK)
	if recArchOK.Code != http.StatusOK {
		t.Fatalf("expected 200 on archive with cancelActive, got %d: %s", recArchOK.Code, recArchOK.Body.String())
	}

	// 3. GET /api/tasks?archived=only returns it
	reqList := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4310/api/tasks?archived=only", nil)
	reqList.RemoteAddr = "127.0.0.1:50003"
	recList := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recList, reqList)
	if recList.Code != http.StatusOK || !strings.Contains(recList.Body.String(), tid) {
		t.Fatalf("expected archived task in ?archived=only, got %d: %s", recList.Code, recList.Body.String())
	}

	// 4. DELETE /api/tasks/{taskId} permanently deletes it
	reqDel := httptest.NewRequest(http.MethodDelete, "http://127.0.0.1:4310/api/tasks/"+tid, nil)
	reqDel.RemoteAddr = "127.0.0.1:50004"
	reqDel.Header.Set("X-Orbit-Request", "1")
	recDel := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recDel, reqDel)
	if recDel.Code != http.StatusOK {
		t.Fatalf("expected 200 on delete, got %d: %s", recDel.Code, recDel.Body.String())
	}
}

func TestRyugiSelfHealAndContentPathTraversalGuard(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	t.Cleanup(func() {
		_ = gitops.RemoveFrozenDir(filepath.Join(tmpDir, "deliveries"))
	})

	// Create a real local git repo + bare mirror containing a Ryugi html + document + whiteboard artifact.
	workRepo := filepath.Join(tmpDir, "work")
	mirrorRepo := filepath.Join(tmpDir, "git", "demo.git")
	if err := os.MkdirAll(workRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	runGit(workRepo, "init", "-b", "main")
	runGit(workRepo, "config", "user.email", "dev@example.com")
	runGit(workRepo, "config", "user.name", "Dev User")
	if err := os.WriteFile(filepath.Join(workRepo, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(workRepo, "add", ".")
	runGit(workRepo, "commit", "-m", "init")
	baseSHA := runGit(workRepo, "rev-parse", "HEAD")

	htmlDir := filepath.Join(workRepo, ".ryugi", "artifacts", "html", "landing")
	docDir := filepath.Join(workRepo, ".ryugi", "artifacts", "document", "spec")
	if err := os.MkdirAll(htmlDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(docDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(htmlDir, "index.html"), []byte("<h1>Hello Ryugi</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docDir, "doc.xml"), []byte("<document><heading>Spec</heading><p>Body</p></document>"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(workRepo, "add", ".")
	runGit(workRepo, "commit", "-m", "add ryugi artifacts")
	headSHA := runGit(workRepo, "rev-parse", "HEAD")

	if err := gitops.EnsureBareMirror(ctx, mirrorRepo); err != nil {
		t.Fatal(err)
	}
	deliveryID := "dlv_ryugi_heal_1"
	if err := gitops.PushDeliveryRef(ctx, workRepo, mirrorRepo, deliveryID, headSHA); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(tmpDir, "orbit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfgDir := filepath.Join(tmpDir, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	projectsYAML := fmt.Sprintf(`schemaVersion: 1
projects:
  demo:
    displayName: Demo
    repoIdentity: git:demo
    defaultBranch: main
    defaultCapability: primary-dev
    defaultAdapter: agy
    ideScheme: cursor
    anchors:
      primary-dev:
        machine: dev-node
        repoPath: %s
`, workRepo)
	machinesYAML := `schemaVersion: 1
machines:
  dev-node:
    displayName: Dev Node
    transport: local_ws
    capabilities: [primary-dev]
`
	if err := os.WriteFile(filepath.Join(cfgDir, "projects.yaml"), []byte(projectsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "machines.yaml"), []byte(machinesYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	var startCount atomic.Int32
	var commentListCalls atomic.Int32
	mockRunner := funcRyugiRunner{
		fn: func(ctx context.Context, args ...string) ([]byte, error) {
			if len(args) > 0 && args[0] == "start" {
				n := startCount.Add(1)
				return []byte(fmt.Sprintf(
					`{"schema":"ryugi.cli/v1","command":"start","outcome":"success","data":{"url":"http://127.0.0.1:27329/html/landing?key=freshKey%d"}}`,
					n,
				)), nil
			}
			if len(args) >= 2 && args[0] == "comment" && args[1] == "list" {
				c := commentListCalls.Add(1)
				if c == 1 {
					// Simulate server not running on first sync attempt!
					return []byte(`{"schema":"ryugi.cli/v1","command":"comment list","outcome":"error","error":{"code":"E_SERVER_NOT_RUNNING","message":"ryugi server is not running"}}`), fmt.Errorf("exit status 1")
				}
				return []byte(`{"schema":"ryugi.cli/v1","command":"comment list","outcome":"success","data":{"artifact":"html/landing","sections":{"threads":{"items":[],"receipt":""}}}}`), nil
			}
			if len(args) > 0 && args[0] == "changes" {
				return []byte(`{"schema":"ryugi.cli/v1","command":"changes","outcome":"success","data":{"empty":true,"diff":"","receipt":""}}`), nil
			}
			return []byte(`{"schema":"ryugi.cli/v1","command":"ack","outcome":"success","data":{}}`), nil
		},
	}

	hubCfg := protocol.DefaultHubConfig()
	hubCfg.DataDir = tmpDir
	hubCfg.ConfigDir = cfgDir
	hubCfg.DevOpen = true

	srv, err := hub.NewServer(ctx, hub.ServerOptions{
		HubConfig:   hubCfg,
		Store:       st,
		RyugiRunner: mockRunner,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	// Import delivery
	impRes, err := st.CreateManualImportDelivery(ctx, store.ManualImportParams{
		DeliveryID:      deliveryID,
		ProjectID:       "demo",
		Title:           "Ryugi Test",
		Summary:         "Add artifacts",
		SourceMachineID: "dev-node",
		RepoIdentity:    "git:demo",
		BaseSHA:         baseSHA,
		HeadSHA:         headSHA,
		GitRef:          "refs/orbit/deliveries/" + deliveryID,
		Kind:            protocol.DeliveryMixed,
		Manifest: protocol.DeliveryManifest{
			FilesChanged:   2,
			RyugiArtifacts: []string{"document/spec", "html/landing"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = impRes

	// Initial registration (startCount becomes 2: one per artifact)
	initialBindings, err := srv.RyugiBridge().RegisterDelivery(ctx, deliveryID)
	if err != nil || len(initialBindings) != 2 {
		t.Fatalf("initial register failed: bindings=%d err=%v", len(initialBindings), err)
	}
	var htmlBinding protocol.RyugiBindingView
	for _, b := range initialBindings {
		if b.ArtifactLocator == "html/landing" {
			htmlBinding = b
		}
	}
	if htmlBinding.BindingID == "" {
		t.Fatalf("missing html/landing binding")
	}
	oldURL := htmlBinding.OpenURL

	// SyncBinding triggers E_SERVER_NOT_RUNNING on first comment list -> self-heals via RegisterDelivery -> retries & succeeds!
	if _, err := srv.RyugiBridge().SyncBinding(ctx, htmlBinding.BindingID); err != nil {
		t.Fatalf("expected SyncBinding to self-heal after E_SERVER_NOT_RUNNING, got err=%v", err)
	}
	if startCount.Load() <= 2 {
		t.Fatalf("expected RegisterDelivery to be re-invoked during self-heal, startCount=%d", startCount.Load())
	}
	_, refreshedBindings, _, _, err := st.GetDeliveryDetail(ctx, deliveryID)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range refreshedBindings {
		if b.BindingID == htmlBinding.BindingID && b.OpenURL == oldURL {
			t.Fatalf("expected OpenURL to be refreshed after self-heal, still %q", b.OpenURL)
		}
	}

	// Test POST /api/deliveries/{deliveryId}/ryugi/open
	reqOpen := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4310/api/deliveries/"+deliveryID+"/ryugi/open?bindingId="+htmlBinding.BindingID, bytes.NewReader([]byte(`{}`)))
	reqOpen.RemoteAddr = "127.0.0.1:50010"
	reqOpen.Header.Set("X-Orbit-Request", "1")
	recOpen := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recOpen, reqOpen)
	if recOpen.Code != http.StatusOK {
		t.Fatalf("expected 200 on ryugi/open, got %d: %s", recOpen.Code, recOpen.Body.String())
	}
	var openResp protocol.RyugiOpenResponse
	if err := json.Unmarshal(recOpen.Body.Bytes(), &openResp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(openResp.OpenURL, "?key=freshKey") {
		t.Fatalf("expected fresh key in openUrl, got %q", openResp.OpenURL)
	}

	// Test GET /api/deliveries/{deliveryId}/ryugi/{bindingId}/content for valid HTML artifact
	reqContent := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4310/api/deliveries/"+deliveryID+"/ryugi/"+htmlBinding.BindingID+"/content", nil)
	reqContent.RemoteAddr = "127.0.0.1:50011"
	recContent := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recContent, reqContent)
	if recContent.Code != http.StatusOK {
		t.Fatalf("expected 200 on ryugi content, got %d: %s", recContent.Code, recContent.Body.String())
	}
	var contentResp protocol.RyugiArtifactContentResponse
	if err := json.Unmarshal(recContent.Body.Bytes(), &contentResp); err != nil {
		t.Fatal(err)
	}
	if contentResp.ContentType != "text/html" || !strings.Contains(contentResp.Text, "<h1>Hello Ryugi</h1>") {
		t.Fatalf("unexpected content response: %+v", contentResp)
	}

	// Test path traversal protection: tamper locator in DB to "../../outside"
	secretPath := filepath.Join(tmpDir, "secret.txt")
	if err := os.WriteFile(secretPath, []byte("TOP_SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE ryugi_bindings SET artifact_locator = '../../../secret.txt' WHERE binding_id = ?`, htmlBinding.BindingID); err != nil {
		t.Fatal(err)
	}
	reqTrav := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4310/api/deliveries/"+deliveryID+"/ryugi/"+htmlBinding.BindingID+"/content", nil)
	reqTrav.RemoteAddr = "127.0.0.1:50012"
	recTrav := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recTrav, reqTrav)
	if recTrav.Code != http.StatusBadRequest || !strings.Contains(recTrav.Body.String(), "ERR_INVALID_ARTIFACT_PATH") {
		t.Fatalf("expected 400 ERR_INVALID_ARTIFACT_PATH on path traversal, got %d: %s", recTrav.Code, recTrav.Body.String())
	}

	// Test symlink escape protection: point index.html inside a valid artifact dir to secretPath outside frozenDir
	escapeDir := filepath.Join(htmlBinding.FrozenProjectDir, ".ryugi", "artifacts", "html", "escape")
	_ = os.Chmod(filepath.Join(htmlBinding.FrozenProjectDir, ".ryugi", "artifacts"), 0o755)
	_ = os.Chmod(filepath.Join(htmlBinding.FrozenProjectDir, ".ryugi", "artifacts", "html"), 0o755)
	if err := os.MkdirAll(escapeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secretPath, filepath.Join(escapeDir, "index.html")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE ryugi_bindings SET artifact_locator = 'html/escape' WHERE binding_id = ?`, htmlBinding.BindingID); err != nil {
		t.Fatal(err)
	}
	reqSym := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4310/api/deliveries/"+deliveryID+"/ryugi/"+htmlBinding.BindingID+"/content", nil)
	reqSym.RemoteAddr = "127.0.0.1:50013"
	recSym := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recSym, reqSym)
	if recSym.Code != http.StatusBadRequest || !strings.Contains(recSym.Body.String(), "ERR_INVALID_ARTIFACT_PATH") {
		t.Fatalf("expected 400 ERR_INVALID_ARTIFACT_PATH on symlink escape, got %d: %s", recSym.Code, recSym.Body.String())
	}

	// P1-9: Forging frozen_project_dir in ryugi_bindings to point outside dataDir/deliveries MUST NOT be trusted
	outsideArt := filepath.Join(tmpDir, "outside_proj", ".ryugi", "artifacts", "html", "landing")
	if err := os.MkdirAll(outsideArt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideArt, "index.html"), []byte("<h1>FORGED_OUTSIDE</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE ryugi_bindings SET artifact_locator = 'html/landing', frozen_project_dir = ? WHERE binding_id = ?`, filepath.Join(tmpDir, "outside_proj"), htmlBinding.BindingID); err != nil {
		t.Fatal(err)
	}
	reqForged := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4310/api/deliveries/"+deliveryID+"/ryugi/"+htmlBinding.BindingID+"/content", nil)
	reqForged.RemoteAddr = "127.0.0.1:50014"
	recForged := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recForged, reqForged)
	if recForged.Code != http.StatusOK || strings.Contains(recForged.Body.String(), "FORGED_OUTSIDE") || !strings.Contains(recForged.Body.String(), "Hello Ryugi") {
		t.Fatalf("expected ReadBindingContent to read only trusted dataDir/deliveries/%s, got %d: %s", deliveryID, recForged.Code, recForged.Body.String())
	}

	// Clean up injected escape dir before singleflight test so snapshot has exactly the 2 original artifacts
	_ = os.Chmod(filepath.Join(htmlBinding.FrozenProjectDir, ".ryugi", "artifacts", "html"), 0o755)
	_ = os.RemoveAll(escapeDir)

	// Item 5: Two-layer os.Root MUST reject an intra-snapshot relative symlink pointing outside the artifact subdir
	// (e.g. pointing to a repo file like README.md inside the SAME frozenDir)!
	_ = os.Chmod(htmlBinding.FrozenProjectDir, 0o755)
	intraSecret := filepath.Join(htmlBinding.FrozenProjectDir, "README.md")
	if err := os.WriteFile(intraSecret, []byte("INTRA_SNAPSHOT_SECRET_CODE"), 0o600); err != nil {
		t.Fatal(err)
	}
	intraDir := filepath.Join(htmlBinding.FrozenProjectDir, ".ryugi", "artifacts", "html", "intra")
	if err := os.MkdirAll(intraDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "..", "..", "README.md"), filepath.Join(intraDir, "index.html")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE ryugi_bindings SET artifact_locator = 'html/intra' WHERE binding_id = ?`, htmlBinding.BindingID); err != nil {
		t.Fatal(err)
	}
	reqIntra := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4310/api/deliveries/"+deliveryID+"/ryugi/"+htmlBinding.BindingID+"/content", nil)
	reqIntra.RemoteAddr = "127.0.0.1:50015"
	recIntra := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recIntra, reqIntra)
	if recIntra.Code != http.StatusBadRequest || !strings.Contains(recIntra.Body.String(), "ERR_INVALID_ARTIFACT_PATH") || strings.Contains(recIntra.Body.String(), "INTRA_SNAPSHOT_SECRET_CODE") {
		t.Fatalf("Item 5 REGRESSION: intra-snapshot relative symlink outside artifact dir was NOT blocked! got %d: %s", recIntra.Code, recIntra.Body.String())
	}
	// Restore clean locator and remove temporary intra dir before singleflight test
	_ = os.RemoveAll(intraDir)
	_ = os.Remove(intraSecret)
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE ryugi_bindings SET artifact_locator = 'html/landing' WHERE binding_id = ?`, htmlBinding.BindingID); err != nil {
		t.Fatal(err)
	}

	// Item 9: SyncBinding on an archived task MUST skip annotation write AND preserve receipt (never call ack!)
	var ackCalled atomic.Int32
	archRunner := funcRyugiRunner{
		fn: func(ctx context.Context, args ...string) ([]byte, error) {
			if len(args) >= 2 && args[0] == "comment" && args[1] == "list" {
				return []byte(`{"schema":"ryugi.cli/v1","command":"comment list","outcome":"success","data":{"artifact":"html/landing","sections":{"threads":{"items":[{"id":"th_arch_1","anchor":{},"status":"open","messages":[{"id":"m1","author":"human","name":"User","text":"comment after archive","at":"2026-11-26T00:00:00Z"}]}],"receipt":"rcpt_preserve_me"}}}}`), nil
			}
			if len(args) > 0 && args[0] == "ack" {
				ackCalled.Add(1)
				return []byte(`{"schema":"ryugi.cli/v1","command":"ack","outcome":"success","data":{}}`), nil
			}
			return []byte(`{"schema":"ryugi.cli/v1","command":"ok","outcome":"success","data":{}}`), nil
		},
	}
	archBridge := hub.NewRyugiBridge(hub.RyugiBridgeConfig{
		Store:        st,
		Runner:       archRunner,
		DataDir:      tmpDir,
		Host:         "127.0.0.1",
		Port:         27329,
		PollInterval: time.Hour,
	})
	// Archive the task owning deliveryID -> SyncBinding must skip writes and never ack!
	dlvRec, _, _, _, err := st.GetDeliveryDetail(ctx, deliveryID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ArchiveTask(ctx, dlvRec.TaskID, "tester"); err != nil {
		t.Fatal(err)
	}
	syncRes, err := archBridge.SyncBinding(ctx, htmlBinding.BindingID)
	if err != nil {
		t.Fatalf("expected archived SyncBinding to skip cleanly, got err=%v", err)
	}
	if syncRes.NewAnnotations != 0 || syncRes.ReceiptsAcked != 0 || ackCalled.Load() != 0 {
		t.Fatalf("Item 9 REGRESSION: archived task SyncBinding wrote or acked! res=%+v ackCalls=%d", syncRes, ackCalled.Load())
	}
	// Unarchive so subsequent singleflight test works cleanly
	if _, _, err := st.UnarchiveTask(ctx, dlvRec.TaskID, "tester"); err != nil {
		t.Fatal(err)
	}

	// P2-14: Concurrent RegisterDelivery calls for the same deliveryID coalesce into one ryugi start pass
	var concurrentStarts atomic.Int32
	slowRunner := funcRyugiRunner{
		fn: func(ctx context.Context, args ...string) ([]byte, error) {
			if len(args) > 0 && args[0] == "start" {
				hasArtifact := false
				for _, a := range args {
					if a == "--artifact" {
						hasArtifact = true
						break
					}
				}
				if hasArtifact {
					time.Sleep(40 * time.Millisecond)
					n := concurrentStarts.Add(1)
					return []byte(fmt.Sprintf(`{"schema":"ryugi.cli/v1","command":"start","outcome":"success","data":{"url":"http://127.0.0.1:27329/html/landing?key=sf%d"}}`, n)), nil
				}
				return []byte(`{"schema":"ryugi.cli/v1","command":"start","outcome":"success","data":{"status":"running"}}`), nil
			}
			return []byte(`{"schema":"ryugi.cli/v1","command":"ok","outcome":"success","data":{}}`), nil
		},
	}
	sfBridge := hub.NewRyugiBridge(hub.RyugiBridgeConfig{
		Store:        st,
		Runner:       slowRunner,
		DataDir:      tmpDir,
		Host:         "127.0.0.1",
		Port:         27329,
		PollInterval: time.Hour,
	})

	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, rErr := sfBridge.RegisterDelivery(ctx, deliveryID)
			errs <- rErr
		}()
	}
	wg.Wait()
	close(errs)
	for rErr := range errs {
		if rErr != nil {
			t.Fatalf("singleflight RegisterDelivery error: %v", rErr)
		}
	}
	// 2 artifacts in delivery -> singleflight should invoke artifact start exactly 2 times (not 12!)
	if got := concurrentStarts.Load(); got != 2 {
		t.Fatalf("expected singleflight to run 2 ryugi artifact start calls total, got %d", got)
	}
}

func TestHubDeleteTaskIgnoresForgedExternalFrozenDir(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	outsideDir := filepath.Join(tmpDir, "outside-secret")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	secretFile := filepath.Join(outsideDir, "important.txt")
	if err := os.WriteFile(secretFile, []byte("DO_NOT_DELETE"), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(dataDir, "orbit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfgDir := filepath.Join(tmpDir, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	machinesYAML := `schemaVersion: 1
machines:
  dev-node:
    displayName: Dev Node
    transport: local_ws
    capabilities: [primary-dev]
`
	if err := os.WriteFile(filepath.Join(cfgDir, "machines.yaml"), []byte(machinesYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	hubCfg := protocol.DefaultHubConfig()
	hubCfg.DataDir = dataDir
	hubCfg.ConfigDir = cfgDir
	hubCfg.DevOpen = true
	srv, err := hub.NewServer(ctx, hub.ServerOptions{HubConfig: hubCfg, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	cr, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:          protocol.ScratchProjectID,
		Title:              "Forged Delivery Dir Task",
		Goal:               "test safe delete",
		RequiredCapability: "primary-dev",
		Adapter:            protocol.AdapterAgy,
		MachineID:          "dev-node",
		CreateAttempt:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE attempts SET state = 'succeeded' WHERE attempt_id = ?`, cr.Attempt.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE tasks SET state = 'done', active_attempt_id = NULL WHERE task_id = ?`, cr.Task.TaskID); err != nil {
		t.Fatal(err)
	}
	deliveryID := "release.1"
	trustedDir := filepath.Join(dataDir, "deliveries", deliveryID)
	if err := os.MkdirAll(trustedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trustedDir, "snap.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Forge frozen_snapshot_dir in DB to point to outsideDir!
	if _, err := st.WriteDB().ExecContext(ctx, `
		INSERT INTO deliveries (
			delivery_id, project_id, task_id, attempt_id, source_machine_id,
			repo_identity, base_sha, head_sha, git_ref, kind, summary, manifest_json,
			frozen_snapshot_dir, created_at
		) VALUES (?, ?, ?, ?, 'dev-node', 'virtual:_scratch', 'base', 'head', 'refs/orbit/deliveries/release.1', 'mixed', 'Forged', '{}', ?, ?)
	`, deliveryID, protocol.ScratchProjectID, cr.Task.TaskID, cr.Attempt.AttemptID, outsideDir, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}

	reqDel := httptest.NewRequest(http.MethodDelete, "http://127.0.0.1:4310/api/tasks/"+cr.Task.TaskID, nil)
	reqDel.RemoteAddr = "127.0.0.1:50020"
	reqDel.Header.Set("X-Orbit-Request", "1")
	recDel := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recDel, reqDel)
	if recDel.Code != http.StatusOK {
		t.Fatalf("expected 200 on delete, got %d: %s", recDel.Code, recDel.Body.String())
	}
	// Outside directory MUST remain untouched!
	if _, err := os.Stat(secretFile); err != nil {
		t.Fatalf("CRITICAL P1-1 REGRESSION: external directory file %s was deleted! err=%v", secretFile, err)
	}
	// Trusted dataDir/deliveries/<deliveryId> SHOULD be cleaned up!
	if _, err := os.Stat(trustedDir); !os.IsNotExist(err) {
		t.Fatalf("expected trusted delivery dir %s to be deleted, err=%v", trustedDir, err)
	}

	// Final-3 (SQLite pending_delivery_cleanups + same-ID re-import mutual exclusion):
	// 1. Seed a real git project + mirror so manual-import can be exercised end-to-end for "_release".
	workRepo := filepath.Join(tmpDir, "work-repo")
	mirrorRepo := filepath.Join(dataDir, "git", "demo-clean.git")
	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(workRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(workRepo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(workRepo, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(workRepo, "add", ".")
	runGit(workRepo, "commit", "-m", "base")
	baseSHA := runGit(workRepo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(workRepo, "a.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(workRepo, "add", ".")
	runGit(workRepo, "commit", "-m", "head")
	headSHA := runGit(workRepo, "rev-parse", "HEAD")
	if err := gitops.EnsureBareMirror(ctx, mirrorRepo); err != nil {
		t.Fatal(err)
	}
	retryDlvID := "_release"
	if err := gitops.PushDeliveryRef(ctx, workRepo, mirrorRepo, retryDlvID, headSHA); err != nil {
		t.Fatal(err)
	}
	projectsYAML := fmt.Sprintf(`schemaVersion: 1
projects:
  demo-clean:
    displayName: Demo Clean
    repoIdentity: git:demo-clean
    defaultBranch: main
    defaultCapability: primary-dev
    defaultAdapter: agy
    ideScheme: cursor
    anchors:
      primary-dev:
        machine: dev-node
        repoPath: %s
`, workRepo)
	if err := os.WriteFile(filepath.Join(cfgDir, "projects.yaml"), []byte(projectsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}

	cr2, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:          "demo-clean",
		Title:              "Pending Cleanup Delivery Task",
		Goal:               "test pending cleanup",
		RequiredCapability: "primary-dev",
		Adapter:            protocol.AdapterAgy,
		MachineID:          "dev-node",
		CreateAttempt:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE attempts SET state = 'succeeded' WHERE attempt_id = ?`, cr2.Attempt.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE tasks SET state = 'done', active_attempt_id = NULL WHERE task_id = ?`, cr2.Task.TaskID); err != nil {
		t.Fatal(err)
	}
	deliveriesRoot := filepath.Join(dataDir, "deliveries")
	retryDir := filepath.Join(deliveriesRoot, retryDlvID)
	if err := os.MkdirAll(retryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(retryDir, "old_snap.txt"), []byte("OLD_SNAPSHOT"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `
		INSERT INTO deliveries (
			delivery_id, project_id, task_id, attempt_id, source_machine_id,
			repo_identity, base_sha, head_sha, git_ref, kind, summary, manifest_json,
			frozen_snapshot_dir, created_at
		) VALUES (?, 'demo-clean', ?, ?, 'dev-node', 'git:demo-clean', ?, ?, 'refs/orbit/deliveries/_release', 'mixed', 'Old', '{}', ?, ?)
	`, retryDlvID, cr2.Task.TaskID, cr2.Attempt.AttemptID, baseSHA, headSHA, retryDir, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}

	// Simulate RemoveFrozenDir failure during DELETE -> SQLite pending_delivery_cleanups MUST retain "_release"!
	srv.SetRemoveFrozenDirHook(func(dir string) error {
		return fmt.Errorf("simulated permission denied removing %s", dir)
	})
	reqDel2 := httptest.NewRequest(http.MethodDelete, "http://127.0.0.1:4310/api/tasks/"+cr2.Task.TaskID, nil)
	reqDel2.RemoteAddr = "127.0.0.1:50025"
	reqDel2.Header.Set("X-Orbit-Request", "1")
	recDel2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recDel2, reqDel2)
	if recDel2.Code != http.StatusOK {
		t.Fatalf("expected 200 on delete for _release, got %d: %s", recDel2.Code, recDel2.Body.String())
	}
	if hasPending, pErr := st.HasPendingDeliveryCleanup(ctx, retryDlvID); pErr != nil || !hasPending {
		t.Fatalf("expected pending_delivery_cleanups row retained after failed removal, has=%v err=%v", hasPending, pErr)
	}

	// While removal still fails, re-importing the SAME deliveryId "_release" MUST be blocked with 409 ERR_DELIVERY_CLEANUP_PENDING!
	importPayload, _ := json.Marshal(protocol.ManualImportRequest{
		DeliveryID:      retryDlvID,
		ProjectID:       "demo-clean",
		Title:           "Reimported _release",
		Summary:         "new snapshot",
		SourceMachineID: "dev-node",
		BaseSHA:         baseSHA,
		HeadSHA:         headSHA,
		GitRef:          "refs/orbit/deliveries/" + retryDlvID,
		Kind:            protocol.DeliveryMixed,
	})
	reqReimpFail := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4310/api/deliveries/manual-import", bytes.NewReader(importPayload))
	reqReimpFail.RemoteAddr = "127.0.0.1:50026"
	reqReimpFail.Header.Set("X-Orbit-Request", "1")
	recReimpFail := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recReimpFail, reqReimpFail)
	if recReimpFail.Code != http.StatusConflict || !strings.Contains(recReimpFail.Body.String(), "ERR_DELIVERY_CLEANUP_PENDING") {
		t.Fatalf("expected 409 ERR_DELIVERY_CLEANUP_PENDING on re-import when cleanup fails, got %d: %s", recReimpFail.Code, recReimpFail.Body.String())
	}

	// Clear failing hook -> re-import cleans OLD dir first, clears pending_delivery_cleanups, and succeeds (201)!
	srv.SetRemoveFrozenDirHook(nil)
	reqReimpOK := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4310/api/deliveries/manual-import", bytes.NewReader(importPayload))
	reqReimpOK.RemoteAddr = "127.0.0.1:50027"
	reqReimpOK.Header.Set("X-Orbit-Request", "1")
	recReimpOK := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recReimpOK, reqReimpOK)
	if recReimpOK.Code != http.StatusCreated {
		t.Fatalf("expected 201 on same-ID re-import after cleanup succeeds, got %d: %s", recReimpOK.Code, recReimpOK.Body.String())
	}
	if _, err := os.Stat(filepath.Join(retryDir, "old_snap.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected old_snap.txt removed before re-import, err=%v", err)
	}
	if hasPending, pErr := st.HasPendingDeliveryCleanup(ctx, retryDlvID); pErr != nil || hasPending {
		t.Fatalf("expected pending_delivery_cleanups cleared after re-import cleanup, has=%v err=%v", hasPending, pErr)
	}

	// Simulate new snapshot written for re-imported "_release", AND even if a stale row existed,
	// startup retry MUST NOT delete an active delivery's snapshot!
	if err := os.MkdirAll(retryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	newSnapFile := filepath.Join(retryDir, "new_snap.txt")
	if err := os.WriteFile(newSnapFile, []byte("NEW_SNAPSHOT"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv2, err := hub.NewServer(ctx, hub.ServerOptions{HubConfig: hubCfg, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	defer srv2.Shutdown()
	if _, err := os.Stat(newSnapFile); err != nil {
		t.Fatalf("REGRESSION: startup retry deleted re-imported delivery snapshot! err=%v", err)
	}
}

func TestRunnerOutboxBlocksSameDeliveryIDWhileCleanupPending(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	t.Cleanup(func() { _ = gitops.RemoveFrozenDir(filepath.Join(dataDir, "deliveries")) })
	cfgDir := filepath.Join(tmpDir, "config")
	workRepo := filepath.Join(tmpDir, "work-repo")
	mirrorRepo := filepath.Join(dataDir, "git", "demo-outbox.git")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workRepo, 0o755); err != nil {
		t.Fatal(err)
	}

	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	runGit(workRepo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(workRepo, "f.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(workRepo, "add", ".")
	runGit(workRepo, "commit", "-m", "base")
	baseSHA := runGit(workRepo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(workRepo, "f.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(workRepo, "add", ".")
	runGit(workRepo, "commit", "-m", "head")
	headSHA := runGit(workRepo, "rev-parse", "HEAD")

	dlvID := "dlv_reuse_outbox"
	if err := gitops.EnsureBareMirror(ctx, mirrorRepo); err != nil {
		t.Fatal(err)
	}
	if err := gitops.PushDeliveryRef(ctx, workRepo, mirrorRepo, dlvID, headSHA); err != nil {
		t.Fatal(err)
	}

	machinesYAML := `schemaVersion: 1
machines:
  dev-node:
    displayName: Dev Node
    transport: local_ws
    capabilities: [primary-dev]
`
	projectsYAML := fmt.Sprintf(`schemaVersion: 1
projects:
  demo-outbox:
    displayName: Demo Outbox
    repoIdentity: git:demo-outbox
    defaultBranch: main
    defaultCapability: primary-dev
    defaultAdapter: agy
    ideScheme: cursor
    anchors:
      primary-dev:
        machine: dev-node
        repoPath: %s
`, workRepo)
	if err := os.WriteFile(filepath.Join(cfgDir, "machines.yaml"), []byte(machinesYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "projects.yaml"), []byte(projectsYAML), 0o600); err != nil {
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
	srv, err := hub.NewServer(ctx, hub.ServerOptions{HubConfig: hubCfg, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	// 1. Seed task 1 with delivery dlv_reuse_outbox and delete it while disk removal fails -> remains in pending_delivery_cleanups.
	t1, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:          "demo-outbox",
		Title:              "Old Task",
		Goal:               "old",
		RequiredCapability: "primary-dev",
		Adapter:            protocol.AdapterAgy,
		MachineID:          "dev-node",
		CreateAttempt:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE attempts SET state = 'succeeded' WHERE attempt_id = ?`, t1.Attempt.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE tasks SET state = 'done', active_attempt_id = NULL WHERE task_id = ?`, t1.Task.TaskID); err != nil {
		t.Fatal(err)
	}
	oldFrozenDir := filepath.Join(dataDir, "deliveries", dlvID)
	if err := os.MkdirAll(oldFrozenDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `
		INSERT INTO deliveries (
			delivery_id, project_id, task_id, attempt_id, source_machine_id,
			repo_identity, base_sha, head_sha, git_ref, kind, summary, manifest_json,
			frozen_snapshot_dir, created_at
		) VALUES (?, 'demo-outbox', ?, ?, 'dev-node', 'git:demo-outbox', ?, ?, ?, 'mixed', 'Old', '{}', ?, ?)
	`, dlvID, t1.Task.TaskID, t1.Attempt.AttemptID, baseSHA, headSHA, "refs/orbit/deliveries/"+dlvID, oldFrozenDir, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}

	srv.SetRemoveFrozenDirHook(func(dir string) error {
		return fmt.Errorf("simulated cleanup failure for %s", dir)
	})
	reqDel := httptest.NewRequest(http.MethodDelete, "http://127.0.0.1:4310/api/tasks/"+t1.Task.TaskID, nil)
	reqDel.RemoteAddr = "127.0.0.1:50030"
	reqDel.Header.Set("X-Orbit-Request", "1")
	recDel := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recDel, reqDel)
	if recDel.Code != http.StatusOK {
		t.Fatalf("expected 200 on delete, got %d: %s", recDel.Code, recDel.Body.String())
	}

	// 2. Runner attempts to publish the SAME deliveryId via outbox on a new active attempt -> MUST be rejected with ErrDeliveryCleanupPending!
	t2, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:          "demo-outbox",
		Title:              "New Runner Task",
		Goal:               "reuse same delivery id",
		RequiredCapability: "primary-dev",
		Adapter:            protocol.AdapterAgy,
		MachineID:          "dev-node",
		CreateAttempt:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.BindAttemptRunnerInstance(ctx, t2.Attempt.AttemptID, "ri_outbox_1"); err != nil {
		t.Fatal(err)
	}
	_, ingestErr := st.IngestOutboxBatch(ctx, "dev-node", "ri_outbox_1", protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  t2.Attempt.AttemptID,
		LeaseToken: t2.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{
				OutboxSeq: 1,
				EventID:   "ev_dlv_reuse_1",
				FrameType: protocol.OutboxFrameDeliveryPublished,
				TS:        time.Now().UnixMilli(),
				Payload: map[string]any{
					"deliveryId": dlvID,
					"baseSha":    baseSHA,
					"headSha":    headSHA,
					"gitRef":     "refs/orbit/deliveries/" + dlvID,
					"summary":    "attempting to reuse pending-cleanup id",
				},
			},
		},
	})
	if !errors.Is(ingestErr, store.ErrDeliveryCleanupPending) {
		t.Fatalf("expected ErrDeliveryCleanupPending from runner outbox reusing %s, got %v", dlvID, ingestErr)
	}

	// 3. Verify deliveries table still has 0 rows for dlvID AND pending_delivery_cleanups still holds dlvID!
	var dlvCount int
	if err := st.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE delivery_id = ?`, dlvID).Scan(&dlvCount); err != nil || dlvCount != 0 {
		t.Fatalf("expected 0 deliveries rows for %s, got %d err=%v", dlvID, dlvCount, err)
	}
	if hasPending, pErr := st.HasPendingDeliveryCleanup(ctx, dlvID); pErr != nil || !hasPending {
		t.Fatalf("expected pending_delivery_cleanups retained for %s, has=%v err=%v", dlvID, hasPending, pErr)
	}

	// 4. Late-arrival outbox path (attempt already expired) MUST ALSO reject reusing dlvID!
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE attempts SET state = 'expired', finished_at = ? WHERE attempt_id = ?`, time.Now().UnixMilli(), t2.Attempt.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE tasks SET state = 'failed', active_attempt_id = NULL WHERE task_id = ?`, t2.Task.TaskID); err != nil {
		t.Fatal(err)
	}
	_, lateErr := st.IngestOutboxBatch(ctx, "dev-node", "ri_outbox_1", protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  t2.Attempt.AttemptID,
		LeaseToken: t2.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{
				OutboxSeq: 1,
				EventID:   "ev_dlv_reuse_late",
				FrameType: protocol.OutboxFrameDeliveryPublished,
				TS:        time.Now().UnixMilli(),
				Payload: map[string]any{
					"deliveryId": dlvID,
					"baseSha":    baseSHA,
					"headSha":    headSHA,
					"gitRef":     "refs/orbit/deliveries/" + dlvID,
					"summary":    "late arrival reuse",
				},
			},
		},
	})
	if !errors.Is(lateErr, store.ErrDeliveryCleanupPending) {
		t.Fatalf("expected ErrDeliveryCleanupPending on late-arrival outbox reusing %s, got %v", dlvID, lateErr)
	}
	if err := st.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE delivery_id = ?`, dlvID).Scan(&dlvCount); err != nil || dlvCount != 0 {
		t.Fatalf("expected 0 deliveries rows after rejected late arrival, got %d err=%v", dlvCount, err)
	}
}

func TestManualImportRejectsConcurrentDeleteBetweenPrecheckAndInsertTx(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	t.Cleanup(func() { _ = gitops.RemoveFrozenDir(filepath.Join(dataDir, "deliveries")) })
	cfgDir := filepath.Join(tmpDir, "config")
	workRepo := filepath.Join(tmpDir, "work-repo")
	mirrorRepo := filepath.Join(dataDir, "git", "demo-race.git")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workRepo, 0o755); err != nil {
		t.Fatal(err)
	}

	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	runGit(workRepo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(workRepo, "g.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(workRepo, "add", ".")
	runGit(workRepo, "commit", "-m", "base")
	baseSHA := runGit(workRepo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(workRepo, "g.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(workRepo, "add", ".")
	runGit(workRepo, "commit", "-m", "head")
	headSHA := runGit(workRepo, "rev-parse", "HEAD")

	dlvID := "dlv_race_gap"
	if err := gitops.EnsureBareMirror(ctx, mirrorRepo); err != nil {
		t.Fatal(err)
	}
	if err := gitops.PushDeliveryRef(ctx, workRepo, mirrorRepo, dlvID, headSHA); err != nil {
		t.Fatal(err)
	}

	machinesYAML := `schemaVersion: 1
machines:
  dev-node:
    displayName: Dev Node
    transport: local_ws
    capabilities: [primary-dev]
`
	projectsYAML := fmt.Sprintf(`schemaVersion: 1
projects:
  demo-race:
    displayName: Demo Race
    repoIdentity: git:demo-race
    defaultBranch: main
    defaultCapability: primary-dev
    defaultAdapter: agy
    ideScheme: cursor
    anchors:
      primary-dev:
        machine: dev-node
        repoPath: %s
`, workRepo)
	if err := os.WriteFile(filepath.Join(cfgDir, "machines.yaml"), []byte(machinesYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "projects.yaml"), []byte(projectsYAML), 0o600); err != nil {
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
	srv, err := hub.NewServer(ctx, hub.ServerOptions{HubConfig: hubCfg, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	// Seed existing task owning dlv_race_gap + old frozen dir (NOT deleted yet -> pending_delivery_cleanups is empty during precheck!).
	tOld, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:          "demo-race",
		Title:              "Concurrent Delete Target",
		Goal:               "old",
		RequiredCapability: "primary-dev",
		Adapter:            protocol.AdapterAgy,
		MachineID:          "dev-node",
		CreateAttempt:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE attempts SET state = 'succeeded' WHERE attempt_id = ?`, tOld.Attempt.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE tasks SET state = 'done', active_attempt_id = NULL WHERE task_id = ?`, tOld.Task.TaskID); err != nil {
		t.Fatal(err)
	}
	oldDir := filepath.Join(dataDir, "deliveries", dlvID)
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDir, "stale.txt"), []byte("STALE_OLD_CONTENT"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `
		INSERT INTO deliveries (
			delivery_id, project_id, task_id, attempt_id, source_machine_id,
			repo_identity, base_sha, head_sha, git_ref, kind, summary, manifest_json,
			frozen_snapshot_dir, created_at
		) VALUES (?, 'demo-race', ?, ?, 'dev-node', 'git:demo-race', ?, ?, ?, 'mixed', 'Old', '{}', ?, ?)
	`, dlvID, tOld.Task.TaskID, tOld.Attempt.AttemptID, baseSHA, headSHA, "refs/orbit/deliveries/"+dlvID, oldDir, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}

	// Inject DeleteTask right AFTER handleManualImport's pre-check (which saw NO pending cleanup)
	// and right BEFORE CreateManualImportDelivery's write transaction!
	var hookFired atomic.Bool
	srv.SetManualImportBeforeInsertHook(func() {
		if hookFired.CompareAndSwap(false, true) {
			if _, _, _, dErr := st.DeleteTask(ctx, tOld.Task.TaskID, "concurrent-deleter"); dErr != nil {
				t.Errorf("concurrent DeleteTask in hook failed: %v", dErr)
			}
		}
	})

	importBody, _ := json.Marshal(protocol.ManualImportRequest{
		DeliveryID:      dlvID,
		ProjectID:       "demo-race",
		Title:           "Reimport during delete race",
		Summary:         "new import",
		SourceMachineID: "dev-node",
		BaseSHA:         baseSHA,
		HeadSHA:         headSHA,
		GitRef:          "refs/orbit/deliveries/" + dlvID,
		Kind:            protocol.DeliveryMixed,
	})
	reqImp := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4310/api/deliveries/manual-import", bytes.NewReader(importBody))
	reqImp.RemoteAddr = "127.0.0.1:50031"
	reqImp.Header.Set("X-Orbit-Request", "1")
	recImp := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recImp, reqImp)

	if !hookFired.Load() {
		t.Fatalf("expected manualImportBeforeInsertHook to fire")
	}
	if recImp.Code != http.StatusConflict || !strings.Contains(recImp.Body.String(), "ERR_DELIVERY_CLEANUP_PENDING") {
		t.Fatalf("expected 409 ERR_DELIVERY_CLEANUP_PENDING when DeleteTask committed between precheck and insert tx, got %d: %s", recImp.Code, recImp.Body.String())
	}

	// Verify NO new delivery row exists AND pending_delivery_cleanups still tracks dlvID!
	var dlvCount int
	if err := st.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE delivery_id = ?`, dlvID).Scan(&dlvCount); err != nil || dlvCount != 0 {
		t.Fatalf("expected 0 deliveries rows after rejected race re-import, got %d err=%v", dlvCount, err)
	}
	if hasPending, pErr := st.HasPendingDeliveryCleanup(ctx, dlvID); pErr != nil || !hasPending {
		t.Fatalf("expected pending_delivery_cleanups still set for %s, has=%v err=%v", dlvID, hasPending, pErr)
	}

	// Subsequent retry clears oldDir first, removes pending_delivery_cleanups, and succeeds (201)!
	srv.SetManualImportBeforeInsertHook(nil)
	reqRetry := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4310/api/deliveries/manual-import", bytes.NewReader(importBody))
	reqRetry.RemoteAddr = "127.0.0.1:50032"
	reqRetry.Header.Set("X-Orbit-Request", "1")
	recRetry := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recRetry, reqRetry)
	if recRetry.Code != http.StatusCreated {
		t.Fatalf("expected 201 on clean retry after race, got %d: %s", recRetry.Code, recRetry.Body.String())
	}
	if _, err := os.Stat(filepath.Join(oldDir, "stale.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected stale.txt deleted before retry re-import succeeded, err=%v", err)
	}
}

func TestRyugiExportRenameInterleavedWithDeleteAndSameIDReimport(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	t.Cleanup(func() { _ = gitops.RemoveFrozenDir(filepath.Join(dataDir, "deliveries")) })
	cfgDir := filepath.Join(tmpDir, "config")
	oldRepo := filepath.Join(tmpDir, "repo-old")
	newRepo := filepath.Join(tmpDir, "repo-new")
	oldMirror := filepath.Join(dataDir, "git", "proj-old.git")
	newMirror := filepath.Join(dataDir, "git", "proj-new.git")
	for _, d := range []string{cfgDir, oldRepo, newRepo} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}

	initRepoWithHTML := func(repoDir, mirrorDir, dlvID, htmlBody string) (string, string) {
		t.Helper()
		runGit(repoDir, "init", "-b", "main")
		if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# init\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(repoDir, "add", ".")
		runGit(repoDir, "commit", "-m", "base")
		base := runGit(repoDir, "rev-parse", "HEAD")

		htmlDir := filepath.Join(repoDir, ".ryugi", "artifacts", "html", "demo")
		if err := os.MkdirAll(htmlDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(htmlDir, "index.html"), []byte(htmlBody), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(repoDir, "add", ".")
		runGit(repoDir, "commit", "-m", "add html artifact")
		head := runGit(repoDir, "rev-parse", "HEAD")

		if err := gitops.EnsureBareMirror(ctx, mirrorDir); err != nil {
			t.Fatal(err)
		}
		if err := gitops.PushDeliveryRef(ctx, repoDir, mirrorDir, dlvID, head); err != nil {
			t.Fatal(err)
		}
		return base, head
	}

	dlvID := "dlv_shared_race"
	oldBaseSHA, oldHeadSHA := initRepoWithHTML(oldRepo, oldMirror, dlvID, "<h1>OLD_PROJECT_CONTENT_DO_NOT_LEAK</h1>")
	newBaseSHA, newHeadSHA := initRepoWithHTML(newRepo, newMirror, dlvID, "<h1>NEW_PROJECT_CONTENT_VERIFIED</h1>")

	machinesYAML := `schemaVersion: 1
machines:
  dev-node:
    displayName: Dev Node
    transport: local_ws
    capabilities: [primary-dev]
`
	projectsYAML := fmt.Sprintf(`schemaVersion: 1
projects:
  proj-old:
    displayName: Old Project
    repoIdentity: git:proj-old
    defaultBranch: main
    defaultCapability: primary-dev
    defaultAdapter: agy
    ideScheme: cursor
    anchors:
      primary-dev:
        machine: dev-node
        repoPath: %s
  proj-new:
    displayName: New Project
    repoIdentity: git:proj-new
    defaultBranch: main
    defaultCapability: primary-dev
    defaultAdapter: agy
    ideScheme: cursor
    anchors:
      primary-dev:
        machine: dev-node
        repoPath: %s
`, oldRepo, newRepo)
	if err := os.WriteFile(filepath.Join(cfgDir, "machines.yaml"), []byte(machinesYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "projects.yaml"), []byte(projectsYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(dataDir, "orbit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var startSeq atomic.Int32
	mockRyugi := funcRyugiRunner{
		fn: func(ctx context.Context, args ...string) ([]byte, error) {
			if len(args) > 0 && args[0] == "start" {
				n := startSeq.Add(1)
				return []byte(fmt.Sprintf(`{"schema":"ryugi.cli/v1","command":"start","outcome":"success","data":{"url":"http://127.0.0.1:27329/html/demo?key=k%d"}}`, n)), nil
			}
			return []byte(`{"schema":"ryugi.cli/v1","command":"ok","outcome":"success","data":{}}`), nil
		},
	}

	hubCfg := protocol.DefaultHubConfig()
	hubCfg.DataDir = dataDir
	hubCfg.ConfigDir = cfgDir
	hubCfg.DevOpen = true
	srv, err := hub.NewServer(ctx, hub.ServerOptions{
		HubConfig:   hubCfg,
		Store:       st,
		RyugiRunner: mockRyugi,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	// 1. Seed old delivery under proj-old WITHOUT exporting snapshot yet (final frozen dir does not exist).
	oldImp, err := st.CreateManualImportDelivery(ctx, store.ManualImportParams{
		DeliveryID:      dlvID,
		ProjectID:       "proj-old",
		Title:           "Old Delivery",
		Summary:         "old html",
		SourceMachineID: "dev-node",
		RepoIdentity:    "git:proj-old",
		BaseSHA:         oldBaseSHA,
		HeadSHA:         oldHeadSHA,
		GitRef:          "refs/orbit/deliveries/" + dlvID,
		Kind:            protocol.DeliveryMixed,
		Manifest: protocol.DeliveryManifest{
			FilesChanged:   1,
			RyugiArtifacts: []string{"html/demo"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	oldTaskID := oldImp.Task.TaskID

	// 2. Install Rename pause hook that triggers ONLY on the first (old) export!
	renamePausedCh := make(chan struct{})
	releaseRenameCh := make(chan struct{})
	var pausedOnce atomic.Bool
	gitops.SetBeforeSnapshotRenameHook(func(id, stagingDir, targetDir string) {
		if id == dlvID && pausedOnce.CompareAndSwap(false, true) {
			close(renamePausedCh)
			<-releaseRenameCh
		}
	})
	defer gitops.SetBeforeSnapshotRenameHook(nil)

	// 3. Start old delivery registration (Goroutine 1) and wait until it pauses right before os.Rename.
	oldRegDone := make(chan error, 1)
	go func() {
		_, regErr := srv.RyugiBridge().RegisterDelivery(ctx, dlvID)
		oldRegDone <- regErr
	}()

	select {
	case <-renamePausedCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for old export to pause before os.Rename")
	}

	finalFrozenDir := filepath.Join(dataDir, "deliveries", dlvID)
	if _, statErr := os.Stat(finalFrozenDir); !os.IsNotExist(statErr) {
		t.Fatalf("expected final frozen dir %s not to exist yet while paused before Rename", finalFrozenDir)
	}

	// 4. While old export is paused before Rename, launch DeleteTask(oldTaskID) (Goroutine 2).
	delDone := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodDelete, "http://127.0.0.1:4310/api/tasks/"+oldTaskID, nil)
		req.RemoteAddr = "127.0.0.1:50040"
		req.Header.Set("X-Orbit-Request", "1")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		delDone <- rec.Code
	}()

	// Wait until DeleteTask commits SQLite tx (inserting dlvID into pending_delivery_cleanups).
	deadline := time.Now().Add(3 * time.Second)
	for {
		hasPending, _ := st.HasPendingDeliveryCleanup(ctx, dlvID)
		if hasPending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for DeleteTask to commit pending_delivery_cleanups")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 5. While old export is STILL paused, launch same-ID manual-import for proj-new (Goroutine 3).
	newImpDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		body, _ := json.Marshal(protocol.ManualImportRequest{
			DeliveryID:      dlvID,
			ProjectID:       "proj-new",
			Title:           "New Project Delivery",
			Summary:         "new html",
			SourceMachineID: "dev-node",
			BaseSHA:         newBaseSHA,
			HeadSHA:         newHeadSHA,
			GitRef:          "refs/orbit/deliveries/" + dlvID,
			Kind:            protocol.DeliveryMixed,
		})
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4310/api/deliveries/manual-import", bytes.NewReader(body))
		req.RemoteAddr = "127.0.0.1:50041"
		req.Header.Set("X-Orbit-Request", "1")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		newImpDone <- rec
	}()

	// Verify BOTH DeleteTask cleanup AND proj-new re-import remain blocked on the shared per-deliveryID mutex
	// and do NOT prematurely clear pending_delivery_cleanups while old Rename is in flight!
	time.Sleep(80 * time.Millisecond)
	select {
	case code := <-delDone:
		t.Fatalf("REGRESSION: DeleteTask cleanup returned prematurely (%d) while old export Rename was still in flight!", code)
	default:
	}
	select {
	case rec := <-newImpDone:
		t.Fatalf("REGRESSION: proj-new re-import returned prematurely (%d: %s) while old export Rename was still in flight!", rec.Code, rec.Body.String())
	default:
	}
	if hasPending, _ := st.HasPendingDeliveryCleanup(ctx, dlvID); !hasPending {
		t.Fatalf("REGRESSION: pending_delivery_cleanups was cleared before in-flight export finished!")
	}

	// 6. Release old export's Rename!
	close(releaseRenameCh)

	oldRegErr := <-oldRegDone
	if oldRegErr == nil {
		t.Fatalf("expected old RegisterDelivery to fail saving bindings after task deletion, got nil")
	}
	delCode := <-delDone
	if delCode != http.StatusOK {
		t.Fatalf("expected DeleteTask HTTP 200, got %d", delCode)
	}
	recNewImp := <-newImpDone
	if recNewImp.Code != http.StatusCreated {
		t.Fatalf("expected proj-new manual-import HTTP 201, got %d: %s", recNewImp.Code, recNewImp.Body.String())
	}
	var newImpResp protocol.ManualImportResponse
	if err := json.Unmarshal(recNewImp.Body.Bytes(), &newImpResp); err != nil {
		t.Fatal(err)
	}
	if newImpResp.Delivery.ProjectID != "proj-new" || newImpResp.Delivery.HeadSHA != newHeadSHA || newImpResp.Task.TaskID == oldTaskID {
		t.Fatalf("unexpected new delivery identity: %+v", newImpResp.Delivery)
	}
	if len(newImpResp.RyugiBindings) != 1 || newImpResp.RyugiBindings[0].ProjectID != "proj-new" || newImpResp.RyugiBindings[0].TaskID != newImpResp.Task.TaskID {
		t.Fatalf("REGRESSION: Ryugi binding does not belong strictly to proj-new! got %+v", newImpResp.RyugiBindings)
	}

	// 7. Verify pending_delivery_cleanups is cleared, disk content is strictly NEW_PROJECT_CONTENT_VERIFIED,
	//    and GET /content strictly serves NEW_PROJECT_CONTENT_VERIFIED!
	if hasPending, pErr := st.HasPendingDeliveryCleanup(ctx, dlvID); pErr != nil || hasPending {
		t.Fatalf("expected pending_delivery_cleanups cleared, has=%v err=%v", hasPending, pErr)
	}
	diskHTML, err := os.ReadFile(filepath.Join(finalFrozenDir, ".ryugi", "artifacts", "html", "demo", "index.html"))
	if err != nil {
		t.Fatalf("read final frozen html: %v", err)
	}
	if !strings.Contains(string(diskHTML), "NEW_PROJECT_CONTENT_VERIFIED") || strings.Contains(string(diskHTML), "OLD_PROJECT_CONTENT_DO_NOT_LEAK") {
		t.Fatalf("REGRESSION: final frozen snapshot on disk contains stale old content! got %q", string(diskHTML))
	}

	reqContent := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4310/api/deliveries/"+dlvID+"/ryugi/"+newImpResp.RyugiBindings[0].BindingID+"/content", nil)
	reqContent.RemoteAddr = "127.0.0.1:50042"
	recContent := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recContent, reqContent)
	if recContent.Code != http.StatusOK || !strings.Contains(recContent.Body.String(), "NEW_PROJECT_CONTENT_VERIFIED") || strings.Contains(recContent.Body.String(), "OLD_PROJECT_CONTENT_DO_NOT_LEAK") {
		t.Fatalf("REGRESSION: /content returned wrong content: %d %s", recContent.Code, recContent.Body.String())
	}

	// 8. Also verify SaveRyugiBindingsForOwner rejects mismatched ownership directly at store layer!
	if _, _, err := st.SaveRyugiBindingsForOwner(ctx, dlvID, &store.DeliveryOwnerExpectation{
		ProjectID: "proj-old",
		TaskID:    oldTaskID,
		HeadSHA:   oldHeadSHA,
	}, finalFrozenDir, protocol.RyugiSyncReady, newImpResp.RyugiBindings); !errors.Is(err, store.ErrOwnershipMismatch) {
		t.Fatalf("expected ErrOwnershipMismatch saving bindings with stale owner, got %v", err)
	}
}

func TestHubArchiveProtectsActiveHebi8MainAndDispatchedAttempts(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(tmpDir, "orbit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfgDir := filepath.Join(tmpDir, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	machinesYAML := `schemaVersion: 1
machines:
  dev-node:
    displayName: Dev Node
    transport: local_ws
    capabilities: [primary-dev]
`
	if err := os.WriteFile(filepath.Join(cfgDir, "machines.yaml"), []byte(machinesYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	hubCfg := protocol.DefaultHubConfig()
	hubCfg.DataDir = tmpDir
	hubCfg.ConfigDir = cfgDir
	hubCfg.DevOpen = true
	srv, err := hub.NewServer(ctx, hub.ServerOptions{HubConfig: hubCfg, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	// 1. Hebi8 main task with an active attempt MUST return 409 ERR_FORBIDDEN on archive(cancelActive:true)
	//    AND must NOT cancel its active attempt!
	mainTask, _, err := st.EnsureHebi8MainTask(ctx, protocol.AdapterAgy)
	if err != nil {
		t.Fatal(err)
	}
	_, attRec, _, err := st.CreateNextAttempt(ctx, store.CreateNextAttemptParams{
		TaskID:      mainTask.TaskID,
		TriggerKind: protocol.TriggerFollowup,
		Adapter:     protocol.AdapterAgy,
		MachineID:   "dev-node",
	})
	if err != nil {
		t.Fatal(err)
	}
	reqArchMain := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4310/api/tasks/"+mainTask.TaskID+"/archive", bytes.NewReader([]byte(`{"cancelActive":true}`)))
	reqArchMain.RemoteAddr = "127.0.0.1:50021"
	reqArchMain.Header.Set("X-Orbit-Request", "1")
	recArchMain := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recArchMain, reqArchMain)
	if recArchMain.Code != http.StatusConflict || !strings.Contains(recArchMain.Body.String(), "ERR_FORBIDDEN") {
		t.Fatalf("expected 409 ERR_FORBIDDEN archiving active Hebi8 main, got %d: %s", recArchMain.Code, recArchMain.Body.String())
	}
	reloadedMain, err := st.GetTask(ctx, mainTask.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if reloadedMain.ActiveAttemptID != attRec.AttemptID {
		t.Fatalf("P1-4 REGRESSION: active attempt on Hebi8 main was cancelled before protection check! got activeAttemptId=%q", reloadedMain.ActiveAttemptID)
	}

	// 2. Ordinary task with an ALREADY DISPATCHED attempt (runner_instance_id set) MUST NOT force-archive immediately
	ord, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:          protocol.ScratchProjectID,
		Title:              "Running Dispatched Task",
		Goal:               "busy",
		RequiredCapability: "primary-dev",
		Adapter:            protocol.AdapterAgy,
		MachineID:          "dev-node",
		CreateAttempt:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE attempts SET state = 'running', runner_instance_id = 'run_inst_1' WHERE attempt_id = ?`, ord.Attempt.AttemptID); err != nil {
		t.Fatal(err)
	}
	reqArchOrd := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4310/api/tasks/"+ord.Task.TaskID+"/archive", bytes.NewReader([]byte(`{"cancelActive":true}`)))
	reqArchOrd.RemoteAddr = "127.0.0.1:50022"
	reqArchOrd.Header.Set("X-Orbit-Request", "1")
	recArchOrd := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recArchOrd, reqArchOrd)
	if recArchOrd.Code != http.StatusConflict || !strings.Contains(recArchOrd.Body.String(), "ERR_TASK_ACTIVE") {
		t.Fatalf("expected 409 ERR_TASK_ACTIVE for dispatched running attempt until runner confirms stop, got %d: %s", recArchOrd.Code, recArchOrd.Body.String())
	}

	// 3. Archived ordinary task rejects /followup, /retry, and /dispatch-annotations with 409 ERR_TASK_ARCHIVED
	idle, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:          protocol.ScratchProjectID,
		Title:              "Archived Guard Task",
		Goal:               "idle",
		RequiredCapability: "primary-dev",
		Adapter:            protocol.AdapterAgy,
		MachineID:          "dev-node",
		CreateAttempt:      false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ArchiveTask(ctx, idle.Task.TaskID, "tester"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		body string
	}{
		{"/api/tasks/" + idle.Task.TaskID + "/followup", `{"message":"wake up","mode":"queue"}`},
		{"/api/tasks/" + idle.Task.TaskID + "/retry", `{}`},
		{"/api/tasks/" + idle.Task.TaskID + "/dispatch-annotations", `{"annotationIds":["ann_1"]}`},
	} {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4310"+tc.path, bytes.NewReader([]byte(tc.body)))
		req.RemoteAddr = "127.0.0.1:50023"
		req.Header.Set("X-Orbit-Request", "1")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "ERR_TASK_ARCHIVED") {
			t.Fatalf("expected 409 ERR_TASK_ARCHIVED on %s, got %d: %s", tc.path, rec.Code, rec.Body.String())
		}
	}

	// Item 4: Creating a subtask under an archived parent via HTTP MUST return 409 ERR_TASK_ARCHIVED
	subBody := fmt.Sprintf(`{"parentTaskId":%q,"goal":"subtask under archived","adapter":"agy"}`, idle.Task.TaskID)
	reqSub := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4310/api/projects/_scratch/tasks", bytes.NewReader([]byte(subBody)))
	reqSub.RemoteAddr = "127.0.0.1:50024"
	reqSub.Header.Set("X-Orbit-Request", "1")
	recSub := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recSub, reqSub)
	if recSub.Code != http.StatusConflict || !strings.Contains(recSub.Body.String(), "ERR_TASK_ARCHIVED") {
		t.Fatalf("Item 4 REGRESSION: expected 409 ERR_TASK_ARCHIVED creating subtask under archived parent, got %d: %s", recSub.Code, recSub.Body.String())
	}
}
