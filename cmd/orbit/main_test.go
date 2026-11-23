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

func TestOrbitCLI_DeliverAndStatus(t *testing.T) {
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
	defer st.Close()

	cfg := protocol.DefaultHubConfig()
	cfg.ConfigDir = cfgDir
	cfg.DataDir = dataDir
	cfg.DevOpen = true

	srv, err := hub.NewServer(ctx, hub.ServerOptions{HubConfig: cfg, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Create sample git repo with dirty edit + external attachment
	repo := filepath.Join(tmp, "repo")
	_ = os.MkdirAll(repo, 0o755)
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

	attachFile := filepath.Join(tmp, "shot.png")
	_ = os.WriteFile(attachFile, []byte("png-content"), 0o644)

	var out, errBuf bytes.Buffer
	err = run(ctx, []string{
		"deliver",
		"--project", "demo-app",
		"--repo", repo,
		"--hub", ts.URL,
		"--mirror", filepath.Join(dataDir, "git", "demo-app.git"),
		"--summary", "CLI test delivery",
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
	if resp.Delivery.ProjectID != "demo-app" || len(resp.Delivery.Manifest.Blobs) != 1 {
		t.Fatalf("unexpected deliver response: %+v", resp)
	}

	// Test orbit status
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
