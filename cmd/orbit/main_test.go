package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	_, dataDir, repo, ts := setupCLITestEnv(t)
	dlvID := "dlv_01RESUMETEST0000000001"

	// 1. First attempt fails because hub URL is unreachable -> must persist cli-pending/<dlvId>.json
	var out, errBuf bytes.Buffer
	err := run(ctx, []string{
		"deliver",
		"--project", "demo-app",
		"--repo", repo,
		"--delivery-id", dlvID,
		"--hub", "http://127.0.0.1:1",
		"--data-dir", dataDir,
		"--mirror", filepath.Join(dataDir, "git", "demo-app.git"),
		"--summary", "Resumable delivery",
	}, &out, &errBuf)
	if err == nil {
		t.Fatalf("expected first deliver against unreachable hub to fail")
	}
	if !strings.Contains(errBuf.String(), "orbit deliver --resume "+dlvID) {
		t.Fatalf("expected stderr recovery hint with --resume, got:\n%s", errBuf.String())
	}

	pPath := pendingRecordPath(dataDir, dlvID)
	rec, err := loadPendingRecord(pPath)
	if err != nil {
		t.Fatalf("expected pending record saved at %s: %v", pPath, err)
	}
	originalHeadSHA := rec.HeadSHA

	// 2. Mutate working tree AFTER packaging -> --resume MUST NOT re-commit or change HeadSHA!
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n// mutated after failure!\n"), 0o644); err != nil {
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
	if resp.Delivery.HeadSHA != originalHeadSHA {
		t.Fatalf("CRITICAL: --resume changed headSha! orig=%s resumed=%s", originalHeadSHA, resp.Delivery.HeadSHA)
	}
	if _, err := os.Stat(pPath); !os.IsNotExist(err) {
		t.Fatalf("expected pending file deleted after successful --resume")
	}
}
