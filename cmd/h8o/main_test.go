package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hebi8/hebi8-Orbit/internal/hub"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/runner"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

func setupCLITestEnv(t *testing.T) (string, string, string, *httptest.Server) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "config")
	dataDir := filepath.Join(tmp, "data")
	_ = os.MkdirAll(cfgDir, 0o755)

	_ = os.WriteFile(filepath.Join(cfgDir, "machines.yaml"), []byte(`schemaVersion: 1
machines:
  hub-node:
    displayName: "hub-node"
    transport: local_ws
    capabilities: [primary-dev]
`), 0o644)
	_ = os.WriteFile(filepath.Join(cfgDir, "projects.yaml"), []byte(`schemaVersion: 1
projects:
  demo-app:
    displayName: "demo-app"
    repoIdentity: "github.com/example-org/demo-app"
    defaultBranch: "main"
    defaultCapability: "primary-dev"
    defaultAdapter: "agy"
    ideScheme: "cursor"
`), 0o644)

	st, err := store.Open(ctx, filepath.Join(dataDir, "orbit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := protocol.DefaultHubConfig()
	cfg.ConfigDir = cfgDir
	cfg.DataDir = dataDir
	cfg.DevOpen = true

	srv, err := hub.NewServer(ctx, hub.ServerOptions{HubConfig: cfg, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	repo := filepath.Join(tmp, "repo")
	_ = os.MkdirAll(filepath.Join(repo, "pkg", "子目录"), 0o755)
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=Tester", "GIT_AUTHOR_EMAIL=tester@example.com",
		"GIT_COMMITTER_NAME=Tester", "GIT_COMMITTER_EMAIL=tester@example.com",
	)
	runG := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = gitEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}
	runG("init", "-b", "main")
	_ = os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n"), 0o644)
	runG("add", ".")
	runG("commit", "-m", "init")
	_ = os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n// dirty change\n"), 0o644)
	_ = os.WriteFile(filepath.Join(repo, "pkg", "子目录", "说明.md"), []byte("# 中文\n"), 0o644)

	return tmp, dataDir, repo, ts
}

func TestOrbitCLI_DeliverFromSubdirAndStatus(t *testing.T) {
	ctx := context.Background()
	tmp, dataDir, repo, ts := setupCLITestEnv(t)

	attachFile := filepath.Join(tmp, "shot.png")
	_ = os.WriteFile(attachFile, []byte("png-content"), 0o644)

	// Invoke deliver from a nested subdirectory!
	subDir := filepath.Join(repo, "pkg", "子目录")
	var out, errBuf bytes.Buffer
	err := run(ctx, []string{
		"deliver",
		"--project", "demo-app",
		"--repo", subDir,
		"--hub", ts.URL,
		"--data-dir", dataDir,
		"--mirror", filepath.Join(dataDir, "git", "demo-app.git"),
		"--summary", "CLI subdir test delivery",
		"--attach", attachFile + ":assets/shot.png",
		"--json",
	}, &out, &errBuf)
	if err != nil {
		t.Fatalf("orbit deliver failed: %v\nstderr=%s", err, errBuf.String())
	}

	var resp protocol.ManualImportResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("decode deliver json: %v\nraw=%s", err, out.String())
	}
	if resp.Delivery.ProjectID != "demo-app" || len(resp.Delivery.Manifest.Blobs) != 1 || resp.Delivery.Manifest.FilesChanged != 2 {
		t.Fatalf("unexpected deliver response: %+v", resp)
	}
	// Pending file must be cleaned up after success
	if _, err := os.Stat(pendingRecordPath(dataDir, resp.Delivery.DeliveryID)); !os.IsNotExist(err) {
		t.Fatalf("expected pending record to be removed after successful deliver")
	}

	out.Reset()
	errBuf.Reset()
	err = run(ctx, []string{"status", "--hub", ts.URL}, &out, &errBuf)
	if err != nil {
		t.Fatalf("orbit status failed: %v", err)
	}
	if !strings.Contains(out.String(), "demo-app") || !strings.Contains(out.String(), "Inbox (1 open)") {
		t.Fatalf("unexpected status output:\n%s", out.String())
	}
}

func TestOrbitCLI_ResumeReusesExactFixedHeadSHA(t *testing.T) {
	ctx := context.Background()
	tmp, dataDir, repo, ts := setupCLITestEnv(t)
	dlvID := "dlv_01RESUMETEST0000000001"

	attachFile := filepath.Join(tmp, "ephemeral.png")
	if err := os.WriteFile(attachFile, []byte("original-attachment-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. First attempt fails because hub URL is unreachable -> must persist cli-pending/<dlvId>.json + staged blob copy
	var out, errBuf bytes.Buffer
	err := run(ctx, []string{
		"deliver",
		"--project", "demo-app",
		"--repo", repo,
		"--delivery-id", dlvID,
		"--attach", attachFile + ":assets/ephemeral.png",
		"--hub", "http://127.0.0.1:1",
		"--data-dir", dataDir,
		"--mirror", filepath.Join(dataDir, "git", "demo-app.git"),
		"--summary", "Resumable delivery",
	}, &out, &errBuf)
	if err == nil {
		t.Fatalf("expected first deliver against unreachable hub to fail")
	}
	if !strings.Contains(errBuf.String(), "h8o deliver --resume "+dlvID) {
		t.Fatalf("expected stderr recovery hint with --resume, got:\n%s", errBuf.String())
	}

	pPath := pendingRecordPath(dataDir, dlvID)
	rec, err := loadPendingRecord(pPath)
	if err != nil {
		t.Fatalf("expected pending record saved at %s: %v", pPath, err)
	}
	originalHeadSHA := rec.HeadSHA
	stagedBlob := pendingBlobPath(dataDir, rec.Blobs[0].Item.SHA256)
	if _, err := os.Stat(stagedBlob); err != nil {
		t.Fatalf("expected staged pending blob at %s: %v", stagedBlob, err)
	}

	// 2. Re-invoking deliver WITHOUT --resume on existing pending deliveryId MUST fail and protect pending file!
	out.Reset()
	errBuf.Reset()
	err = run(ctx, []string{
		"deliver",
		"--project", "demo-app",
		"--repo", repo,
		"--delivery-id", dlvID,
		"--hub", ts.URL,
		"--data-dir", dataDir,
		"--mirror", filepath.Join(dataDir, "git", "demo-app.git"),
		"--summary", "Must not overwrite pending",
	}, &out, &errBuf)
	if err == nil || !strings.Contains(err.Error(), "--resume "+dlvID) {
		t.Fatalf("expected deliver without --resume on existing pending deliveryId to fail cleanly, got err=%v", err)
	}

	// 3. Mutate working tree AND delete original attachment file BEFORE --resume ->
	//    --resume MUST still upload staged attachment bytes and reuse exact originalHeadSHA!
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n// mutated after failure!\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(attachFile); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errBuf.Reset()
	err = run(ctx, []string{
		"deliver",
		"--resume", dlvID,
		"--hub", ts.URL,
		"--data-dir", dataDir,
		"--json",
	}, &out, &errBuf)
	if err != nil {
		t.Fatalf("orbit deliver --resume failed: %v\nstderr=%s", err, errBuf.String())
	}

	var resp protocol.ManualImportResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("decode resume json: %v", err)
	}
	if resp.Delivery.HeadSHA != originalHeadSHA || len(resp.Delivery.Manifest.Blobs) != 1 {
		t.Fatalf("CRITICAL: --resume mismatch! orig=%s resumed=%s blobs=%d", originalHeadSHA, resp.Delivery.HeadSHA, len(resp.Delivery.Manifest.Blobs))
	}
	if _, err := os.Stat(pPath); !os.IsNotExist(err) {
		t.Fatalf("expected pending file deleted after successful --resume")
	}
	if _, err := os.Stat(stagedBlob); !os.IsNotExist(err) {
		t.Fatalf("expected staged pending blob cleaned up after successful --resume")
	}
}

func TestOrbitCLI_BriefAndRunnerSocketDeliver(t *testing.T) {
	ctx := context.Background()
	tmp, _, _, ts := setupCLITestEnv(t)

	// 1. Test `orbit brief`
	var out, errBuf bytes.Buffer
	if err := run(ctx, []string{"brief", "--hub", ts.URL, "--json"}, &out, &errBuf); err != nil {
		t.Fatalf("orbit brief failed: %v\nstderr=%s", err, errBuf.String())
	}
	var brief protocol.PABriefResponse
	if err := json.Unmarshal(out.Bytes(), &brief); err != nil {
		t.Fatalf("decode brief json: %v\nraw=%s", err, out.String())
	}
	if brief.Summary == "" {
		t.Fatalf("expected non-empty brief summary: %+v", brief)
	}

	// 2. Test `orbit deliver --summary "..." --final` via ORBIT_RUNNER_SOCK
	sockPath := filepath.Join(tmp, "runner.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var received runner.LocalDeliverRequest
	mux := http.NewServeMux()
	mux.HandleFunc("/deliver", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(runner.LocalDeliverResponse{
			DeliveryID:   "dlv_sock_1",
			AttemptID:    "att_sock_1",
			TaskID:       "tsk_sock_1",
			ProjectID:    "demo-app",
			BaseSHA:      "1111111",
			HeadSHA:      "2222222",
			Finalized:    received.Final,
			FilesChanged: 1,
			Insertions:   5,
		})
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	t.Setenv("ORBIT_RUNNER_SOCK", sockPath)
	t.Setenv("ORBIT_ATTEMPT_ID", "att_sock_1")
	out.Reset()
	errBuf.Reset()
	if err := run(ctx, []string{"deliver", "--summary", "runner socket delivery", "--final", "--json"}, &out, &errBuf); err != nil {
		t.Fatalf("orbit deliver via socket failed: %v\nstderr=%s", err, errBuf.String())
	}
	if !received.Final || received.Summary != "runner socket delivery" || received.AttemptID != "att_sock_1" {
		t.Fatalf("unexpected runner socket request: %+v", received)
	}
	var sockResp runner.LocalDeliverResponse
	if err := json.Unmarshal(out.Bytes(), &sockResp); err != nil {
		t.Fatalf("decode socket deliver response: %v", err)
	}
	if sockResp.DeliveryID != "dlv_sock_1" || !sockResp.Finalized {
		t.Fatalf("unexpected socket deliver response: %+v", sockResp)
	}
}
