package hub_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Hebi8/hebi8-Orbit/internal/gitops"
	"github.com/Hebi8/hebi8-Orbit/internal/hub"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

type fakeRyugiRunner struct {
	mu      sync.Mutex
	calls   [][]string
	handler func(args []string) ([]byte, error)
}

func (f *fakeRyugiRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	cp := append([]string{}, args...)
	f.calls = append(f.calls, cp)
	h := f.handler
	f.mu.Unlock()
	if h != nil {
		return h(cp)
	}
	return []byte(`{"schema":"ryugi.cli/v1","outcome":"success","data":{}}`), nil
}

func (f *fakeRyugiRunner) recordedCalls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	copy(out, f.calls)
	return out
}

func setupTestHub(t *testing.T, cfgMut func(*protocol.HubConfig), ryugiRunner hub.RyugiRunner) (*hub.Server, *store.Store, string, string) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "config")
	dataDir := filepath.Join(tmp, "data")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}

	machinesYAML := `schemaVersion: 1
machines:
  hub-node:
    displayName: "hub-node"
    transport: local_ws
    tailscaleHost: "orbit.example.ts.net"
    capabilities: [primary-dev]
`
	projectsYAML := `schemaVersion: 1
projects:
  demo-app:
    displayName: "demo-app"
    repoIdentity: "github.com/example-org/demo-app"
    defaultBranch: "main"
    defaultCapability: "primary-dev"
    defaultAdapter: "agy"
    ideScheme: "cursor"
    anchors:
      primary-dev:
        machine: "hub-node"
        repoPath: "/home/orbit/dev/demo-app"
`
	if err := os.WriteFile(filepath.Join(cfgDir, "machines.yaml"), []byte(machinesYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "projects.yaml"), []byte(projectsYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(dataDir, "orbit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := protocol.DefaultHubConfig()
	cfg.ConfigDir = cfgDir
	cfg.DataDir = dataDir
	cfg.HubOrigin = "https://orbit.example.ts.net"
	cfg.AllowedTailscaleLogins = []string{"user@example.com"}
	cfg.RyugiPublicOrigin = "https://orbit.example.ts.net:8443"
	cfg.DevOpen = false
	if cfgMut != nil {
		cfgMut(&cfg)
	}

	srv, err := hub.NewServer(ctx, hub.ServerOptions{
		HubConfig:   cfg,
		Store:       st,
		RyugiRunner: ryugiRunner,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, st, cfgDir, dataDir
}

func TestAuthAndCrossOriginDefense(t *testing.T) {
	srv, st, _, _ := setupTestHub(t, nil, &fakeRyugiRunner{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. Unauthenticated request -> 401
	resp, err := http.Get(ts.URL + "/api/bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without credentials, got %d", resp.StatusCode)
	}

	// 2. Non-whitelisted Tailscale login alone -> 401
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/bootstrap", nil)
	req.Header.Set("Tailscale-User-Login", "intruder@example.com")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for non-whitelisted login, got %d", resp.StatusCode)
	}

	// 3. Even whitelisted Tailscale-User-Login ALONE MUST return 401 (no pairing bypass!)
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/bootstrap", nil)
	req.Header.Set("Tailscale-User-Login", "user@example.com")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for standalone Tailscale-User-Login without paired client token, got %d", resp.StatusCode)
	}

	// Mint a paired client token for subsequent auth / CSRF / Origin checks
	clientTok, tokenID, err := st.CreateClientToken(context.Background(), "Test Browser", "browser", "")
	if err != nil {
		t.Fatal(err)
	}

	// 3b. Paired client token with non-whitelisted Tailscale-User-Login -> 401 (extra defense-in-depth)
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/bootstrap", nil)
	req.Header.Set("Authorization", "Bearer "+clientTok)
	req.Header.Set("Tailscale-User-Login", "intruder@example.com")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for paired token with disallowed Tailscale login, got %d", resp.StatusCode)
	}

	// 3c. Paired client token GET -> 200
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/bootstrap", nil)
	req.Header.Set("Authorization", "Bearer "+clientTok)
	req.Header.Set("Tailscale-User-Login", "user@example.com")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for paired client token, got %d", resp.StatusCode)
	}

	// 4. Write without X-Orbit-Request: 1 -> 403
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/admin/gc-pause", strings.NewReader(`{"paused":true}`))
	req.Header.Set("Authorization", "Bearer "+clientTok)
	req.Header.Set("Origin", "https://orbit.example.ts.net")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 without X-Orbit-Request header, got %d", resp.StatusCode)
	}

	// 5. Write from Ryugi preview origin (:8443) -> 403 even with X-Orbit-Request: 1
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/admin/gc-pause", strings.NewReader(`{"paused":true}`))
	req.Header.Set("Authorization", "Bearer "+clientTok)
	req.Header.Set("Origin", "https://orbit.example.ts.net:8443")
	req.Header.Set("X-Orbit-Request", "1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-origin :8443 write, got %d", resp.StatusCode)
	}

	// 6. CORS preflight from :8443 -> 403 and NO Access-Control-Allow-Origin header
	req, _ = http.NewRequest(http.MethodOptions, ts.URL+"/api/admin/gc-pause", nil)
	req.Header.Set("Origin", "https://orbit.example.ts.net:8443")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("expected 403 with no ACAO on cross-origin OPTIONS, got %d ACAO=%q", resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
	}

	// 7. Write with exact Origin + X-Orbit-Request: 1 -> 200
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/admin/gc-pause", strings.NewReader(`{"paused":true,"reason":"backup"}`))
	req.Header.Set("Authorization", "Bearer "+clientTok)
	req.Header.Set("Origin", "https://orbit.example.ts.net")
	req.Header.Set("X-Orbit-Request", "1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !srv.IsGCPaused() {
		t.Fatalf("expected 200 and gcPaused=true, got %d paused=%v", resp.StatusCode, srv.IsGCPaused())
	}

	// 8. WebSocket handshake with mismatched Origin (:8443) -> 403 before upgrade
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/client?sinceSeq=0"
	_, wsResp, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": []string{"Bearer " + clientTok},
			"Origin":        []string{"https://orbit.example.ts.net:8443"},
		},
	})
	if err == nil || wsResp == nil || wsResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected WS 403 for :8443 origin, got err=%v resp=%v", err, wsResp)
	}

	// 9. Revoking client device token immediately invalidates further requests (401)
	req, _ = http.NewRequest(http.MethodDelete, ts.URL+"/api/clients/"+tokenID, nil)
	req.Header.Set("Authorization", "Bearer "+clientTok)
	req.Header.Set("Origin", "https://orbit.example.ts.net")
	req.Header.Set("X-Orbit-Request", "1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 revoking client device, got %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/bootstrap", nil)
	req.Header.Set("Authorization", "Bearer "+clientTok)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 after revoking client token, got %d", resp.StatusCode)
	}

	// 10. ValidateListenAddr unconditionally rejects non-loopback EVEN when DevOpen == true (Item #6)
	for _, devOpen := range []bool{false, true} {
		for _, badAddr := range []string{"0.0.0.0:4310", ":4310", "[::]:4310", "192.168.1.10:4310"} {
			badCfg := protocol.DefaultHubConfig()
			badCfg.DevOpen = devOpen
			badCfg.ListenAddr = badAddr
			if err := hub.ValidateListenAddr(badCfg); err == nil {
				t.Fatalf("expected ValidateListenAddr(devOpen=%v) to reject non-loopback %q", devOpen, badAddr)
			}
		}
		for _, okAddr := range []string{"127.0.0.1:4310", "[::1]:4310", "localhost:4310"} {
			okCfg := protocol.DefaultHubConfig()
			okCfg.DevOpen = devOpen
			okCfg.ListenAddr = okAddr
			if err := hub.ValidateListenAddr(okCfg); err != nil {
				t.Fatalf("expected ValidateListenAddr(devOpen=%v) to allow loopback %q, got %v", devOpen, okAddr, err)
			}
		}
	}

	// 11. Local CLI write via X-Orbit-Dev-Token WITHOUT Origin succeeds on loopback
	srvTok, _, _, _ := setupTestHub(t, func(c *protocol.HubConfig) {
		c.DevToken = "secret-cli-tok"
	}, &fakeRyugiRunner{})
	tsTok := httptest.NewServer(srvTok.Handler())
	defer tsTok.Close()
	req, _ = http.NewRequest(http.MethodPost, tsTok.URL+"/api/admin/gc-pause", strings.NewReader(`{"paused":true}`))
	req.Header.Set("X-Orbit-Dev-Token", "secret-cli-tok")
	req.Header.Set("X-Orbit-Request", "1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for loopback CLI dev-token write without Origin, got %d", resp.StatusCode)
	}
}

func TestBlobsStreamingSHA256AndAnnotationsIdempotency(t *testing.T) {
	srv, _, _, dataDir := setupTestHub(t, func(c *protocol.HubConfig) {
		c.DevOpen = true
	}, &fakeRyugiRunner{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. Blob PUT with tampered body -> 400
	payload := []byte("hello content-addressed blob")
	sum := sha256.Sum256(payload)
	goodHex := hex.EncodeToString(sum[:])

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/blobs/"+goodHex, bytes.NewReader([]byte("tampered")))
	req.Header.Set("X-Orbit-Request", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for sha256 mismatch, got %d", resp.StatusCode)
	}

	// 2. HTML Blob PUT valid -> 200, HEAD -> 200, GET -> exact bytes with safe octet-stream + attachment headers (Blocking #5)
	htmlPayload := []byte("<!doctype html><html><body><script>alert(1)</script></body></html>")
	htmlSum := sha256.Sum256(htmlPayload)
	htmlHex := hex.EncodeToString(htmlSum[:])
	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/api/blobs/"+htmlHex, bytes.NewReader(htmlPayload))
	req.Header.Set("X-Orbit-Request", "1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for HTML blob PUT, got %d", resp.StatusCode)
	}
	htmlGet, err := http.Get(ts.URL + "/api/blobs/" + htmlHex)
	if err != nil {
		t.Fatal(err)
	}
	htmlGet.Body.Close()
	if htmlGet.Header.Get("Content-Type") != "application/octet-stream" ||
		!strings.Contains(htmlGet.Header.Get("Content-Disposition"), "attachment;") ||
		htmlGet.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("unsafe blob response headers: ct=%q cd=%q nosniff=%q",
			htmlGet.Header.Get("Content-Type"),
			htmlGet.Header.Get("Content-Disposition"),
			htmlGet.Header.Get("X-Content-Type-Options"))
	}

	// Verify HTTP Range support via http.ServeContent while keeping octet-stream + attachment (Item #11)
	rangeReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/blobs/"+htmlHex, nil)
	rangeReq.Header.Set("Range", "bytes=0-8")
	rangeResp, err := http.DefaultClient.Do(rangeReq)
	if err != nil {
		t.Fatal(err)
	}
	rangeBytes, _ := io.ReadAll(rangeResp.Body)
	rangeResp.Body.Close()
	if rangeResp.StatusCode != http.StatusPartialContent || string(rangeBytes) != "<!doctype" ||
		rangeResp.Header.Get("Content-Type") != "application/octet-stream" ||
		!strings.Contains(rangeResp.Header.Get("Content-Disposition"), "attachment;") {
		t.Fatalf("unexpected Range response: status=%d body=%q ct=%q cd=%q",
			rangeResp.StatusCode, string(rangeBytes), rangeResp.Header.Get("Content-Type"), rangeResp.Header.Get("Content-Disposition"))
	}

	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/api/blobs/"+goodHex, bytes.NewReader(payload))
	req.Header.Set("X-Orbit-Request", "1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for valid blob PUT, got %d", resp.StatusCode)
	}

	headResp, err := http.Head(ts.URL + "/api/blobs/" + goodHex)
	if err != nil {
		t.Fatal(err)
	}
	headResp.Body.Close()
	if headResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for blob HEAD, got %d", headResp.StatusCode)
	}

	getResp, err := http.Get(ts.URL + "/api/blobs/" + goodHex)
	if err != nil {
		t.Fatal(err)
	}
	gotBytes, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if string(gotBytes) != string(payload) {
		t.Fatalf("blob GET mismatch: got %q", string(gotBytes))
	}

	// 3. Create real git commit, push to bare mirror, call manual-import + create annotation with Idempotency-Key
	repo := t.TempDir()
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=Tester", "GIT_AUTHOR_EMAIL=tester@example.com",
		"GIT_COMMITTER_NAME=Tester", "GIT_COMMITTER_EMAIL=tester@example.com",
	)
	mustGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = gitEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	mustGit("init", "-b", "main")
	_ = os.WriteFile(filepath.Join(repo, "app.go"), []byte("package main\nfunc main() {}\n"), 0o644)
	mustGit("add", ".")
	mustGit("commit", "-m", "init")
	_ = os.WriteFile(filepath.Join(repo, "app.go"), []byte("package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"hi\") }\n"), 0o644)

	dlvID := "dlv_01HUBTEST0000000000001"
	pkg, err := gitops.PackageCheckpoint(context.Background(), gitops.PackageOptions{
		RepoDir:    repo,
		DeliveryID: dlvID,
	})
	if err != nil {
		t.Fatal(err)
	}
	mirrorPath := filepath.Join(dataDir, "git", "demo-app.git")
	if err := gitops.PushDeliveryRef(context.Background(), repo, mirrorPath, dlvID, pkg.HeadSHA); err != nil {
		t.Fatal(err)
	}

	// Open WebSocket client BEFORE manual-import to verify replay_end + live event push
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/client?sinceSeq=0"
	wsConn, _, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{ts.URL}},
	})
	if err != nil {
		t.Fatalf("ws dial failed: %v", err)
	}
	defer wsConn.CloseNow()

	// Read initial replay_end
	ctxRead, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelRead()
	_, firstMsgBytes, err := wsConn.Read(ctxRead)
	if err != nil {
		t.Fatalf("ws read replay_end failed: %v", err)
	}
	var firstMsg protocol.WSServerMessage
	if err := json.Unmarshal(firstMsgBytes, &firstMsg); err != nil || firstMsg.Type != protocol.WSMessageReplayEnd {
		t.Fatalf("expected replay_end frame, got %s", string(firstMsgBytes))
	}

	// Submit manual-import
	importBody, _ := json.Marshal(protocol.ManualImportRequest{
		DeliveryID: dlvID,
		ProjectID:  "demo-app",
		Title:      "Test Delivery",
		Summary:    "Add greeting",
		BaseSHA:    pkg.ReviewBaseSHA,
		HeadSHA:    pkg.HeadSHA,
		GitRef:     "refs/orbit/deliveries/" + dlvID,
		Blobs:      []protocol.BlobManifestItem{{Path: "notes/extra.bin", SHA256: goodHex, Size: int64(len(payload))}},
	})
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/deliveries/manual-import", bytes.NewReader(importBody))
	req.Header.Set("X-Orbit-Request", "1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var importResp protocol.ManualImportResponse
	if err := json.NewDecoder(resp.Body).Decode(&importResp); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || !importResp.Created || importResp.Delivery.DeliveryID != dlvID {
		t.Fatalf("expected 201 created manual-import, got %d %+v", resp.StatusCode, importResp)
	}

	// Idempotent replay with identical fields -> 200 OK, created=false
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/deliveries/manual-import", bytes.NewReader(importBody))
	req.Header.Set("X-Orbit-Request", "1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var replayResp protocol.ManualImportResponse
	_ = json.NewDecoder(resp.Body).Decode(&replayResp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || replayResp.Created {
		t.Fatalf("expected 200 idempotent manual-import replay with created=false, got %d %+v", resp.StatusCode, replayResp)
	}

	// Conflicting manual-import reusing same deliveryId with different baseSha -> 409 Conflict (Blocking #3)
	conflictBody, _ := json.Marshal(protocol.ManualImportRequest{
		DeliveryID: dlvID,
		ProjectID:  "demo-app",
		Summary:    "conflict",
		BaseSHA:    pkg.HeadSHA, // different baseSha!
		HeadSHA:    pkg.HeadSHA,
		GitRef:     "refs/orbit/deliveries/" + dlvID,
	})
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/deliveries/manual-import", bytes.NewReader(conflictBody))
	req.Header.Set("X-Orbit-Request", "1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for mismatched existing deliveryId, got %d", resp.StatusCode)
	}

	// Conflicting manual-import reusing same deliveryId with different summary/manifest -> 409 Conflict (Item #5)
	conflictSummaryBody, _ := json.Marshal(protocol.ManualImportRequest{
		DeliveryID: dlvID,
		ProjectID:  "demo-app",
		Summary:    "tampered summary",
		BaseSHA:    pkg.ReviewBaseSHA,
		HeadSHA:    pkg.HeadSHA,
		GitRef:     "refs/orbit/deliveries/" + dlvID,
		Blobs:      []protocol.BlobManifestItem{{Path: "notes/extra.bin", SHA256: goodHex, Size: int64(len(payload))}},
	})
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/deliveries/manual-import", bytes.NewReader(conflictSummaryBody))
	req.Header.Set("X-Orbit-Request", "1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for mismatched summary on existing deliveryId, got %d", resp.StatusCode)
	}

	// Conflicting manual-import reusing same deliveryId with different blobs manifest -> 409 Conflict (Item #5)
	conflictManifestBody, _ := json.Marshal(protocol.ManualImportRequest{
		DeliveryID: dlvID,
		ProjectID:  "demo-app",
		Summary:    "Add greeting",
		BaseSHA:    pkg.ReviewBaseSHA,
		HeadSHA:    pkg.HeadSHA,
		GitRef:     "refs/orbit/deliveries/" + dlvID,
		Blobs:      nil, // missing blob from original manifest
	})
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/deliveries/manual-import", bytes.NewReader(conflictManifestBody))
	req.Header.Set("X-Orbit-Request", "1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for mismatched manifest blobs on existing deliveryId, got %d", resp.StatusCode)
	}

	// Complete review endpoint resolves inbox item
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/deliveries/"+dlvID+"/complete-review", nil)
	req.Header.Set("X-Orbit-Request", "1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var crResp protocol.CompleteReviewResponse
	_ = json.NewDecoder(resp.Body).Decode(&crResp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || crResp.InboxState != protocol.InboxResolved {
		t.Fatalf("expected 200 resolved complete-review, got %d %+v", resp.StatusCode, crResp)
	}

	// Verify WS receives live events (task.created + delivery.published)
	receivedDeliveryEvent := false
	for i := 0; i < 4; i++ {
		c2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
		_, msgBytes, err := wsConn.Read(c2)
		cancel2()
		if err != nil {
			t.Fatalf("ws live read failed: %v", err)
		}
		var m protocol.WSServerMessage
		_ = json.Unmarshal(msgBytes, &m)
		if m.Type == protocol.WSMessageEvent && m.Event != nil && m.Event.Kind == protocol.EventDeliveryPublished {
			receivedDeliveryEvent = true
			break
		}
	}
	if !receivedDeliveryEvent {
		t.Fatalf("expected live delivery.published event over /ws/client")
	}

	// Test annotation creation: mismatched projectId rejected 400
	badAnnBody := `{"projectId":"wrong-proj","body":"bad","anchor":{"path":"app.go","side":"new","startLine":2,"endLine":2}}`
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/deliveries/"+dlvID+"/annotations", strings.NewReader(badAnnBody))
	req.Header.Set("X-Orbit-Request", "1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for mismatched projectId on annotation, got %d", resp.StatusCode)
	}

	// Test annotation creation with Idempotency-Key -> first 201, retry 200 same ID
	goodAnnBody := `{"body":"Check error handling","anchor":{"path":"app.go","side":"new","startLine":3,"endLine":3}}`
	var firstAnnID string
	for attempt := 1; attempt <= 2; attempt++ {
		req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/deliveries/"+dlvID+"/annotations", strings.NewReader(goodAnnBody))
		req.Header.Set("X-Orbit-Request", "1")
		req.Header.Set("Idempotency-Key", "diff-key-1")
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var annResp protocol.CreateAnnotationResponse
		_ = json.NewDecoder(resp.Body).Decode(&annResp)
		resp.Body.Close()
		if attempt == 1 {
			if resp.StatusCode != http.StatusCreated || !annResp.Created {
				t.Fatalf("expected 201 created on first call, got %d %+v", resp.StatusCode, annResp)
			}
			firstAnnID = annResp.Annotation.AnnotationID
			if annResp.Annotation.ProjectID != "demo-app" || annResp.Annotation.TaskID != importResp.Task.TaskID {
				t.Fatalf("expected derived project/task ownership, got %+v", annResp.Annotation)
			}
		} else {
			if resp.StatusCode != http.StatusOK || annResp.Created || annResp.Annotation.AnnotationID != firstAnnID {
				t.Fatalf("expected 200 idempotent replay with same id %s, got %d %+v", firstAnnID, resp.StatusCode, annResp)
			}
		}
	}
}

func TestRyugiBridge_URLRewritePaginationCommitBeforeAckAndStateHashDedup(t *testing.T) {
	// 1. Verify origin rewrite preserves path + query + key + fragment
	rewritten := hub.RewriteRyugiURL("http://localhost:27319/document/AbCdEfGhIjKlMnOpQrSt1?key=secret123&view=split#p1", "https://orbit.example.ts.net:8443")
	expected := "https://orbit.example.ts.net:8443/document/AbCdEfGhIjKlMnOpQrSt1?key=secret123&view=split#p1"
	if rewritten != expected {
		t.Fatalf("RewriteRyugiURL mismatch:\ngot:  %s\nwant: %s", rewritten, expected)
	}

	// 2. Test paginated comment list + document changes + asks state-hash dedup + commit-before-ack
	var mu sync.Mutex
	ackOrder := []string{}
	askChoice := "option-a"
	threadsAcked := false
	changesAcked := false

	fake := &fakeRyugiRunner{
		handler: func(args []string) ([]byte, error) {
			mu.Lock()
			defer mu.Unlock()
			joined := strings.Join(args, " ")
			switch {
			case strings.HasPrefix(joined, "start "):
				return []byte(`{"schema":"ryugi.cli/v1","command":"start","outcome":"success","data":{"url":"http://localhost:27319/document/TestId123456789012345?key=k1"}}`), nil

			case strings.HasPrefix(joined, "comment list --sections all"):
				if threadsAcked {
					return []byte(`{"schema":"ryugi.cli/v1","command":"comment.list","outcome":"success","data":{"artifact":"document/spec","sections":{"threads":{"items":[],"page":{"hasMore":false,"nextCursor":null}},"asks":{"items":[{"id":"q-draft","state":"draft","choice":"x"},{"id":"q-sub","state":"submitted","choice":"` + askChoice + `"}],"page":{"hasMore":false,"nextCursor":null}}}}}`), nil
				}
				return []byte(`{"schema":"ryugi.cli/v1","command":"comment.list","outcome":"success","data":{"artifact":"document/spec","sections":{"threads":{"items":[{"id":"t1","anchor":{"blockId":"p1"},"status":"open","messages":[{"id":"m1","author":"human","text":"First page note"}]}],"snapshot":"snap-1","receipt":"rcpt-thread-1","page":{"hasMore":true,"nextCursor":"cur-2","snapshot":"snap-1"}},"asks":{"items":[{"id":"q-draft","state":"draft","choice":"x"},{"id":"q-sub","state":"submitted","choice":"` + askChoice + `"}],"page":{"hasMore":false,"nextCursor":null}}}}}`), nil

			case strings.HasPrefix(joined, "comment list --section threads") && strings.Contains(joined, "--cursor cur-2"):
				return []byte(`{"schema":"ryugi.cli/v1","command":"comment.list","outcome":"success","data":{"artifact":"document/spec","sections":{"threads":{"items":[{"id":"t2","anchor":{"blockId":"p2"},"status":"open","messages":[{"id":"m2","author":"human","text":"Second page note"}]}],"snapshot":"snap-1","receipt":"rcpt-thread-1","page":{"hasMore":false,"nextCursor":null,"snapshot":"snap-1"}}}}}`), nil

			case strings.HasPrefix(joined, "changes --detail"):
				if changesAcked {
					return []byte(`{"schema":"ryugi.cli/v1","command":"changes","outcome":"success","data":{"artifact":"document/spec","empty":true,"diff":""}}`), nil
				}
				return []byte(`{"schema":"ryugi.cli/v1","command":"changes","outcome":"success","data":{"artifact":"document/spec","empty":false,"diff":"- old line\n+ new line","receipt":"rcpt-changes-1"}}`), nil

			case strings.HasPrefix(joined, "comment ack"):
				threadsAcked = true
				ackOrder = append(ackOrder, "comment.ack")
				return []byte(`{"schema":"ryugi.cli/v1","command":"comment.ack","outcome":"success","data":{"status":"acknowledged"}}`), nil

			case strings.HasPrefix(joined, "changes ack"):
				changesAcked = true
				ackOrder = append(ackOrder, "changes.ack")
				return []byte(`{"schema":"ryugi.cli/v1","command":"changes.ack","outcome":"success","data":{"status":"acknowledged"}}`), nil
			}
			return []byte(`{"schema":"ryugi.cli/v1","outcome":"success","data":{}}`), nil
		},
	}

	srv, st, _, dataDir := setupTestHub(t, func(c *protocol.HubConfig) {
		c.DevOpen = true
	}, fake)
	ctx := context.Background()

	// Seed a delivery with a document/spec artifact in the bare mirror
	repo := t.TempDir()
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=Tester", "GIT_AUTHOR_EMAIL=tester@example.com",
		"GIT_COMMITTER_NAME=Tester", "GIT_COMMITTER_EMAIL=tester@example.com",
	)
	runG := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = gitEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	runG("init", "-b", "main")
	artDir := filepath.Join(repo, ".ryugi", "artifacts", "document", "spec")
	_ = os.MkdirAll(artDir, 0o755)
	_ = os.WriteFile(filepath.Join(artDir, "doc.xml"), []byte("<doc><p id=\"p1\">Hello</p></doc>\n"), 0o644)
	runG("add", ".")
	runG("commit", "-m", "add doc")

	dlvID := "dlv_01RYUGITEST00000000001"
	pkg, err := gitops.PackageCheckpoint(ctx, gitops.PackageOptions{RepoDir: repo, DeliveryID: dlvID})
	if err != nil {
		t.Fatal(err)
	}
	mirrorPath := filepath.Join(dataDir, "git", "demo-app.git")
	if err := gitops.PushDeliveryRef(ctx, repo, mirrorPath, dlvID, pkg.HeadSHA); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gitops.RemoveFrozenDir(filepath.Join(dataDir, "deliveries", dlvID)) })

	_, err = st.CreateManualImportDelivery(ctx, store.ManualImportParams{
		DeliveryID:   dlvID,
		ProjectID:    "demo-app",
		Title:        "Spec Delivery",
		Summary:      "Doc review",
		RepoIdentity: "github.com/example-org/demo-app",
		BaseSHA:      pkg.ReviewBaseSHA,
		HeadSHA:      pkg.HeadSHA,
		GitRef:       "refs/orbit/deliveries/" + dlvID,
		Manifest:     pkg.Manifest,
	})
	if err != nil {
		t.Fatal(err)
	}

	bindings, err := srv.RyugiBridge().RegisterDelivery(ctx, dlvID)
	if err != nil {
		t.Fatalf("RegisterDelivery failed: %v", err)
	}
	if len(bindings) != 1 || !strings.HasPrefix(bindings[0].OpenURL, "https://orbit.example.ts.net:8443/document/") || !strings.Contains(bindings[0].OpenURL, "?key=k1") {
		t.Fatalf("unexpected registered binding: %+v", bindings)
	}

	// First sync: should pull page 1 (t1), page 2 (t2), submitted ask (q-sub; NOT q-draft!), and document changes -> 4 annotations + 2 acks!
	syncResp, err := srv.RyugiBridge().SyncDelivery(ctx, dlvID)
	if err != nil {
		t.Fatalf("SyncDelivery #1 failed: %v", err)
	}
	if syncResp.NewAnnotations != 4 || syncResp.ReceiptsAcked != 2 {
		t.Fatalf("expected 4 new annotations (2 threads + 1 ask + 1 change) and 2 acks, got %+v", syncResp)
	}
	if len(ackOrder) != 2 || ackOrder[0] != "comment.ack" || ackOrder[1] != "changes.ack" {
		t.Fatalf("unexpected ack sequence: %v", ackOrder)
	}

	// Verify DB actually has all 4 annotations committed
	_, _, anns, _, err := st.GetDeliveryDetail(ctx, dlvID)
	if err != nil {
		t.Fatal(err)
	}
	if len(anns) != 4 {
		t.Fatalf("expected 4 committed annotations in SQLite, got %d", len(anns))
	}

	// Second sync with unchanged askChoice -> 0 new annotations, 0 acks (state-hash dedup!)
	syncResp2, err := srv.RyugiBridge().SyncDelivery(ctx, dlvID)
	if err != nil {
		t.Fatalf("SyncDelivery #2 failed: %v", err)
	}
	if syncResp2.NewAnnotations != 0 || syncResp2.ReceiptsAcked != 0 {
		t.Fatalf("expected 0 new annotations on unchanged re-poll, got %+v", syncResp2)
	}

	// Third sync after human updates askChoice -> exactly 1 new annotation!
	mu.Lock()
	askChoice = "option-b"
	mu.Unlock()
	syncResp3, err := srv.RyugiBridge().SyncDelivery(ctx, dlvID)
	if err != nil {
		t.Fatalf("SyncDelivery #3 failed: %v", err)
	}
	if syncResp3.NewAnnotations != 1 {
		t.Fatalf("expected 1 new annotation after ask state changed, got %+v", syncResp3)
	}
}

func TestRyugiBridge_NoAckWhenSQLiteCommitFails(t *testing.T) {
	ackCalled := false
	fake := &fakeRyugiRunner{
		handler: func(args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			switch {
			case strings.HasPrefix(joined, "start "):
				return []byte(`{"schema":"ryugi.cli/v1","command":"start","outcome":"success","data":{"url":"http://localhost:27319/document/FailTest1234567890123"}}`), nil
			case strings.HasPrefix(joined, "comment list "):
				return []byte(`{"schema":"ryugi.cli/v1","command":"comment.list","outcome":"success","data":{"artifact":"document/spec","sections":{"threads":{"items":[{"id":"t1","anchor":{"blockId":"p1"},"status":"open","messages":[{"id":"m1","author":"human","text":"must not ack on db failure"}]}],"receipt":"rcpt-must-not-ack","page":{"hasMore":false,"nextCursor":null}}}}}`), nil
			case strings.HasPrefix(joined, "changes "):
				return []byte(`{"schema":"ryugi.cli/v1","command":"changes","outcome":"success","data":{"artifact":"document/spec","empty":true,"diff":""}}`), nil
			case strings.Contains(joined, "ack"):
				ackCalled = true
				return []byte(`{"schema":"ryugi.cli/v1","outcome":"success","data":{"status":"acknowledged"}}`), nil
			}
			return []byte(`{"schema":"ryugi.cli/v1","outcome":"success","data":{}}`), nil
		},
	}

	srv, st, _, dataDir := setupTestHub(t, func(c *protocol.HubConfig) { c.DevOpen = true }, fake)
	ctx := context.Background()

	// Seed a delivery with a document/spec artifact
	repo := t.TempDir()
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
	artDir := filepath.Join(repo, ".ryugi", "artifacts", "document", "spec")
	_ = os.MkdirAll(artDir, 0o755)
	_ = os.WriteFile(filepath.Join(artDir, "doc.xml"), []byte("<doc><p id=\"p1\">Hello</p></doc>\n"), 0o644)
	runG("add", ".")
	runG("commit", "-m", "add doc")

	dlvID := "dlv_01NOACKONFAIL000000001"
	pkg, err := gitops.PackageCheckpoint(ctx, gitops.PackageOptions{RepoDir: repo, DeliveryID: dlvID})
	if err != nil {
		t.Fatal(err)
	}
	if err := gitops.PushDeliveryRef(ctx, repo, filepath.Join(dataDir, "git", "demo-app.git"), dlvID, pkg.HeadSHA); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gitops.RemoveFrozenDir(filepath.Join(dataDir, "deliveries", dlvID)) })

	_, err = st.CreateManualImportDelivery(ctx, store.ManualImportParams{
		DeliveryID:   dlvID,
		ProjectID:    "demo-app",
		Title:        "No-Ack Test",
		Summary:      "Verify DB failure prevents ack",
		RepoIdentity: "github.com/example-org/demo-app",
		BaseSHA:      pkg.ReviewBaseSHA,
		HeadSHA:      pkg.HeadSHA,
		GitRef:       "refs/orbit/deliveries/" + dlvID,
		Manifest:     pkg.Manifest,
	})
	if err != nil {
		t.Fatal(err)
	}

	bindings, err := srv.RyugiBridge().RegisterDelivery(ctx, dlvID)
	if err != nil || len(bindings) != 1 {
		t.Fatalf("RegisterDelivery failed: %v", err)
	}

	// Close ONLY the write DB so read succeeds (fetching binding), Ryugi CLI returns receipt,
	// and SQLite write tx COMMIT fails!
	_ = st.WriteDB().Close()

	_, err = srv.RyugiBridge().SyncBinding(ctx, bindings[0].BindingID)
	if err == nil {
		t.Fatalf("expected SyncBinding to fail when SQLite write DB is closed")
	}
	if ackCalled {
		t.Fatalf("CRITICAL VIOLATION: ryugi ack was called even though SQLite COMMIT failed!")
	}
}

// Blocking #10 & #11 & #1:
// - Two paginated comment pages with DIFFERENT receipts -> BOTH receipts acked
// - Malformed thread JSON -> returns error and invokes ZERO acks
// - Document changes error surfaced verbatim in Errors[] (never silently swallowed)
// - A -> B -> A ask transition emits 3 distinct annotations (v1, v2, v3)
// - GC pause blocks manual Ryugi sync with ErrSyncPaused (409)
func TestRyugiBridge_MultiPageReceiptsMalformedNoAckChangesErrorAndABATransition(t *testing.T) {
	var mu sync.Mutex
	ackedReceipts := []string{}
	mode := "two_pages_aba"
	askChoice := "A"

	fake := &fakeRyugiRunner{
		handler: func(args []string) ([]byte, error) {
			mu.Lock()
			defer mu.Unlock()
			joined := strings.Join(args, " ")
			switch {
			case strings.HasPrefix(joined, "start "):
				return []byte(`{"schema":"ryugi.cli/v1","command":"start","outcome":"success","data":{"url":"http://localhost:27319/document/MultiRcpt123456789012"}}`), nil

			case strings.HasPrefix(joined, "comment list --sections all"):
				if mode == "malformed" {
					return []byte(`{"schema":"ryugi.cli/v1","command":"comment.list","outcome":"success","data":{"artifact":"document/spec","sections":{"threads":{"items":["not-an-object"],"receipt":"rcpt-bad-must-not-ack","page":{"hasMore":false,"nextCursor":null}}}}}`), nil
				}
				if mode == "two_pages_aba" {
					return []byte(`{"schema":"ryugi.cli/v1","command":"comment.list","outcome":"success","data":{"artifact":"document/spec","sections":{"threads":{"items":[{"id":"t1","anchor":{"blockId":"p1"},"status":"open","messages":[{"id":"m1","author":"human","text":"Page 1 note"}]}],"snapshot":"s1","receipt":"rcpt-page-1","page":{"hasMore":true,"nextCursor":"cur-p2","snapshot":"s1"}},"asks":{"items":[{"id":"q1","state":"submitted","choice":"` + askChoice + `"}],"page":{"hasMore":false,"nextCursor":null}}}}}`), nil
				}
				return []byte(`{"schema":"ryugi.cli/v1","command":"comment.list","outcome":"success","data":{"artifact":"document/spec","sections":{"threads":{"items":[],"page":{"hasMore":false,"nextCursor":null}},"asks":{"items":[{"id":"q1","state":"submitted","choice":"` + askChoice + `"}],"page":{"hasMore":false,"nextCursor":null}}}}}`), nil

			case strings.HasPrefix(joined, "comment list --section threads") && strings.Contains(joined, "--cursor cur-p2"):
				return []byte(`{"schema":"ryugi.cli/v1","command":"comment.list","outcome":"success","data":{"artifact":"document/spec","sections":{"threads":{"items":[{"id":"t2","anchor":{"blockId":"p2"},"status":"open","messages":[{"id":"m2","author":"human","text":"Page 2 note"}]}],"snapshot":"s1","receipt":"rcpt-page-2","page":{"hasMore":false,"nextCursor":null,"snapshot":"s1"}}}}}`), nil

			case strings.HasPrefix(joined, "changes --detail"):
				if mode == "changes_error" {
					return []byte(`{"schema":"ryugi.cli/v1","command":"changes","outcome":"error","error":{"code":"E_REVIEW_FAILED","message":"EACCES: cannot read doc.xml"}}`), nil
				}
				return []byte(`{"schema":"ryugi.cli/v1","command":"changes","outcome":"success","data":{"artifact":"document/spec","empty":true,"diff":""}}`), nil

			case strings.HasPrefix(joined, "comment ack"):
				for i := 0; i < len(args)-1; i++ {
					if args[i] == "--receipt" {
						ackedReceipts = append(ackedReceipts, args[i+1])
					}
				}
				return []byte(`{"schema":"ryugi.cli/v1","command":"comment.ack","outcome":"success","data":{"status":"acknowledged"}}`), nil
			}
			return []byte(`{"schema":"ryugi.cli/v1","outcome":"success","data":{}}`), nil
		},
	}

	srv, st, _, dataDir := setupTestHub(t, func(c *protocol.HubConfig) { c.DevOpen = true }, fake)
	ctx := context.Background()

	repo := t.TempDir()
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
	artDir := filepath.Join(repo, ".ryugi", "artifacts", "document", "spec")
	_ = os.MkdirAll(artDir, 0o755)
	_ = os.WriteFile(filepath.Join(artDir, "doc.xml"), []byte("<doc><p id=\"p1\">Hello</p></doc>\n"), 0o644)
	runG("add", ".")
	runG("commit", "-m", "add doc")

	dlvID := "dlv_01MULTIRCPT000000001"
	pkg, err := gitops.PackageCheckpoint(ctx, gitops.PackageOptions{RepoDir: repo, DeliveryID: dlvID})
	if err != nil {
		t.Fatal(err)
	}
	if err := gitops.PushDeliveryRef(ctx, repo, filepath.Join(dataDir, "git", "demo-app.git"), dlvID, pkg.HeadSHA); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gitops.RemoveFrozenDir(filepath.Join(dataDir, "deliveries", dlvID)) })

	_, err = st.CreateManualImportDelivery(ctx, store.ManualImportParams{
		DeliveryID:   dlvID,
		ProjectID:    "demo-app",
		Title:        "Multi-Receipt Test",
		Summary:      "Test multi-page receipts & A->B->A",
		RepoIdentity: "github.com/example-org/demo-app",
		BaseSHA:      pkg.ReviewBaseSHA,
		HeadSHA:      pkg.HeadSHA,
		GitRef:       "refs/orbit/deliveries/" + dlvID,
		Manifest:     pkg.Manifest,
	})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := srv.RyugiBridge().RegisterDelivery(ctx, dlvID)
	if err != nil || len(bindings) != 1 {
		t.Fatalf("RegisterDelivery failed: %v", err)
	}

	// 1. Two pages with DIFFERENT receipts + initial ask = A -> BOTH rcpt-page-1 & rcpt-page-2 MUST be acked!
	r1, err := srv.RyugiBridge().SyncDelivery(ctx, dlvID)
	if err != nil || r1.NewAnnotations != 3 || r1.ReceiptsAcked != 2 {
		t.Fatalf("expected 3 new annotations (2 threads + ask A) and 2 acks, got %+v err=%v", r1, err)
	}
	if len(ackedReceipts) != 2 || ackedReceipts[0] != "rcpt-page-1" || ackedReceipts[1] != "rcpt-page-2" {
		t.Fatalf("expected both page receipts acked in order, got %v", ackedReceipts)
	}

	// 2. A -> B -> A state transition: B emits v2, returning to A emits v3!
	mu.Lock()
	mode = "aba_only"
	askChoice = "B"
	mu.Unlock()
	rB, err := srv.RyugiBridge().SyncDelivery(ctx, dlvID)
	if err != nil || rB.NewAnnotations != 1 {
		t.Fatalf("expected 1 new annotation for A->B, got %+v err=%v", rB, err)
	}
	mu.Lock()
	askChoice = "A"
	mu.Unlock()
	rA2, err := srv.RyugiBridge().SyncDelivery(ctx, dlvID)
	if err != nil || rA2.NewAnnotations != 1 {
		t.Fatalf("expected 1 new annotation for B->A reversion, got %+v err=%v", rA2, err)
	}
	// Repeating A again without change -> 0 new annotations
	rA3, err := srv.RyugiBridge().SyncDelivery(ctx, dlvID)
	if err != nil || rA3.NewAnnotations != 0 {
		t.Fatalf("expected 0 new annotations for unchanged A, got %+v err=%v", rA3, err)
	}

	// 3. Document changes error MUST be surfaced verbatim in Errors[] (not swallowed)
	mu.Lock()
	mode = "changes_error"
	mu.Unlock()
	rErr, err := srv.RyugiBridge().SyncDelivery(ctx, dlvID)
	if err != nil || len(rErr.Errors) == 0 || !strings.Contains(rErr.Errors[0], "EACCES") {
		t.Fatalf("expected document changes EACCES surfaced in Errors[], got %+v err=%v", rErr, err)
	}

	// 4. Malformed thread item -> SyncBinding returns error and ackedReceipts count stays 2!
	mu.Lock()
	mode = "malformed"
	mu.Unlock()
	if _, err := srv.RyugiBridge().SyncBinding(ctx, bindings[0].BindingID); err == nil {
		t.Fatalf("expected SyncBinding to fail on malformed thread JSON")
	}
	if len(ackedReceipts) != 2 {
		t.Fatalf("expected zero additional acks on malformed thread, got %v", ackedReceipts)
	}

	// 5. GC pause pauses Ryugi sync with 409 Conflict
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/admin/gc-pause", strings.NewReader(`{"paused":true,"reason":"backup"}`))
	req.Header.Set("X-Orbit-Request", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/deliveries/"+dlvID+"/ryugi/sync", nil)
	req.Header.Set("X-Orbit-Request", "1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict while GC pause is active, got %d", resp.StatusCode)
	}
}

// Blocking #4: Verify >1000 event paginated WS replay AND burst live tailing with ZERO dropped/skipped events
func TestWebSocket_PaginatedReplayOver1000AndLiveBurstNoDrops(t *testing.T) {
	srv, st, _, _ := setupTestHub(t, func(c *protocol.HubConfig) { c.DevOpen = true }, &fakeRyugiRunner{})
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// Seed 1,250 timeline events BEFORE WS connects (exceeds old 1000 cap & multi-pages 200-row chunks)
	const initialCount = 1250
	err := st.WithWriteTx(ctx, func(tx *sql.Tx) error {
		for i := 1; i <= initialCount; i++ {
			_, err := tx.Exec(`
				INSERT INTO events (event_id, stream_id, project_id, actor_kind, actor_id, kind, visibility, payload_json, created_at)
				VALUES (?, 'project:demo-app', 'demo-app', 'system', 'sys', 'task.state_changed', 'timeline', '{}', ?)
			`, fmt.Sprintf("evt_ws_init_%04d", i), now+int64(i))
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed 1250 events failed: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/client?sinceSeq=0"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{ts.URL}},
	})
	if err != nil {
		t.Fatalf("ws dial failed: %v", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	// Read all replay frames until replay_end
	var replayed []int64
	for {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, data, err := conn.Read(rctx)
		cancel()
		if err != nil {
			t.Fatalf("read replay frame failed: %v", err)
		}
		var msg protocol.WSServerMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type == protocol.WSMessageReplay {
			for _, ev := range msg.Events {
				replayed = append(replayed, ev.Seq)
			}
		} else if msg.Type == protocol.WSMessageReplayEnd {
			break
		}
	}
	if len(replayed) != initialCount {
		t.Fatalf("expected all %d replayed events without truncation, got %d", initialCount, len(replayed))
	}
	for i := 1; i < len(replayed); i++ {
		if replayed[i] <= replayed[i-1] {
			t.Fatalf("replayed seq not strictly ascending at %d: %d <= %d", i, replayed[i], replayed[i-1])
		}
	}

	// Burst-insert 320 live events (exceeds old 256-slot channel buffer!) and notify broker
	const burstCount = 320
	go func() {
		for i := 1; i <= burstCount; i++ {
			_ = st.WithWriteTx(context.Background(), func(tx *sql.Tx) error {
				_, err := tx.Exec(`
					INSERT INTO events (event_id, stream_id, project_id, actor_kind, actor_id, kind, visibility, payload_json, created_at)
					VALUES (?, 'project:demo-app', 'demo-app', 'system', 'sys', 'task.state_changed', 'timeline', '{}', ?)
				`, fmt.Sprintf("evt_ws_live_%04d", i), now+int64(initialCount+i))
				return err
			})
			srv.EventBroker().Notify()
		}
	}()

	lastSeq := replayed[len(replayed)-1]
	liveReceived := 0
	for liveReceived < burstCount {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, data, err := conn.Read(rctx)
		cancel()
		if err != nil {
			t.Fatalf("read live event #%d failed: %v", liveReceived, err)
		}
		var msg protocol.WSServerMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type == protocol.WSMessageEvent && msg.Event != nil {
			if msg.Event.Seq != lastSeq+1 {
				t.Fatalf("expected contiguous live seq %d, got %d", lastSeq+1, msg.Event.Seq)
			}
			lastSeq = msg.Event.Seq
			liveReceived++
		}
	}
}

// Items #1, #2, #5, #10:
// - Startup CalibrateActiveBindings upgrades old 0555 <artifactDir> to 0755 preserving comments.json
// - Exact manual-import replay of ready delivery skips redundant `ryugi start` and duplicate events
// - Human message missing stable msg.ID fails fast and never acks
// - gc-pause drains in-flight SyncBinding before returning 200 OK
func TestRyugiBridge_StartupCalibrationReadyReplayMissingMsgIDAndGCPauseDrain(t *testing.T) {
	var mu sync.Mutex
	startCount := 0
	ackCount := 0
	missingMsgID := false

	inFlightEntered := make(chan struct{})
	releaseInFlight := make(chan struct{})
	blockNextList := false

	fake := &fakeRyugiRunner{
		handler: func(args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			mu.Lock()
			if strings.HasPrefix(joined, "start ") {
				startCount++
				mu.Unlock()
				return []byte(`{"schema":"ryugi.cli/v1","command":"start","outcome":"success","data":{"url":"http://localhost:27319/document/CalibDrain12345678901"}}`), nil
			}
			if strings.HasPrefix(joined, "comment list ") {
				shouldBlock := blockNextList
				isMissingID := missingMsgID
				blockNextList = false
				mu.Unlock()
				if shouldBlock {
					close(inFlightEntered)
					<-releaseInFlight
				}
				if isMissingID {
					return []byte(`{"schema":"ryugi.cli/v1","command":"comment.list","outcome":"success","data":{"artifact":"document/spec","sections":{"threads":{"items":[{"id":"t1","anchor":{"blockId":"p1"},"status":"open","messages":[{"id":"","author":"human","text":"no id must fail"}]}],"receipt":"rcpt-missing-id","page":{"hasMore":false,"nextCursor":null}}}}}`), nil
				}
				return []byte(`{"schema":"ryugi.cli/v1","command":"comment.list","outcome":"success","data":{"artifact":"document/spec","sections":{"threads":{"items":[{"id":"t_ok","anchor":{"blockId":"p1"},"status":"open","messages":[{"id":"m_ok","author":"human","text":"valid note"}]}],"receipt":"rcpt-drain-1","page":{"hasMore":false,"nextCursor":null}}}}}`), nil
			}
			if strings.HasPrefix(joined, "changes ") {
				mu.Unlock()
				return []byte(`{"schema":"ryugi.cli/v1","command":"changes","outcome":"success","data":{"artifact":"document/spec","empty":true,"diff":""}}`), nil
			}
			if strings.HasPrefix(joined, "comment ack") {
				ackCount++
				mu.Unlock()
				return []byte(`{"schema":"ryugi.cli/v1","command":"comment.ack","outcome":"success","data":{"status":"acknowledged"}}`), nil
			}
			mu.Unlock()
			return []byte(`{"schema":"ryugi.cli/v1","outcome":"success","data":{}}`), nil
		},
	}

	srv, st, cfgDir, dataDir := setupTestHub(t, func(c *protocol.HubConfig) { c.DevOpen = true }, fake)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ctx := context.Background()

	// Seed repo + delivery with Ryugi artifact
	repo := t.TempDir()
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
	artDir := filepath.Join(repo, ".ryugi", "artifacts", "document", "spec")
	_ = os.MkdirAll(artDir, 0o755)
	_ = os.WriteFile(filepath.Join(artDir, "doc.xml"), []byte("<doc><p id=\"p1\">Hello</p></doc>\n"), 0o644)
	runG("add", ".")
	runG("commit", "-m", "add doc")

	dlvID := "dlv_01DRAINANDCALIB00001"
	pkg, err := gitops.PackageCheckpoint(ctx, gitops.PackageOptions{RepoDir: repo, DeliveryID: dlvID})
	if err != nil {
		t.Fatal(err)
	}
	if err := gitops.PushDeliveryRef(ctx, repo, filepath.Join(dataDir, "git", "demo-app.git"), dlvID, pkg.HeadSHA); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gitops.RemoveFrozenDir(filepath.Join(dataDir, "deliveries", dlvID)) })

	importBody, _ := json.Marshal(protocol.ManualImportRequest{
		DeliveryID: dlvID,
		ProjectID:  "demo-app",
		Title:      "Calib & Drain",
		Summary:    "Check startup calibration and ready replay",
		BaseSHA:    pkg.ReviewBaseSHA,
		HeadSHA:    pkg.HeadSHA,
		GitRef:     "refs/orbit/deliveries/" + dlvID,
	})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/deliveries/manual-import", bytes.NewReader(importBody))
	req.Header.Set("X-Orbit-Request", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("initial manual-import failed: err=%v resp=%v", err, resp)
	}
	resp.Body.Close()

	// Item #5: exact idempotent replay while ryugi_sync_state=='ready' MUST NOT call `ryugi start` again!
	seqBeforeReplay, _ := st.MaxEventSeq(ctx)
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/deliveries/manual-import", bytes.NewReader(importBody))
	req.Header.Set("X-Orbit-Request", "1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("replay manual-import failed: err=%v resp=%v", err, resp)
	}
	var replayResp protocol.ManualImportResponse
	_ = json.NewDecoder(resp.Body).Decode(&replayResp)
	resp.Body.Close()
	seqAfterReplay, _ := st.MaxEventSeq(ctx)
	if startCount != 1 || len(replayResp.RyugiBindings) != 1 || seqAfterReplay != seqBeforeReplay {
		t.Fatalf("expected ready replay to skip ryugi start and extra events: startCount=%d bindings=%d seq=%d->%d",
			startCount, len(replayResp.RyugiBindings), seqBeforeReplay, seqAfterReplay)
	}

	// Item #10: Human message missing stable msg.ID MUST return error and NOT ack!
	mu.Lock()
	missingMsgID = true
	mu.Unlock()
	if _, err := srv.RyugiBridge().SyncBinding(ctx, replayResp.RyugiBindings[0].BindingID); err == nil {
		t.Fatalf("expected SyncBinding to reject human message missing stable msg.ID")
	}
	if ackCount != 0 {
		t.Fatalf("expected 0 acks when human message missing msg.ID, got %d", ackCount)
	}

	// Item #2: gc-pause MUST wait until in-flight SyncBinding completes its read->commit->ack before returning!
	mu.Lock()
	missingMsgID = false
	blockNextList = true
	mu.Unlock()

	syncDone := make(chan error, 1)
	go func() {
		_, err := srv.RyugiBridge().SyncBinding(ctx, replayResp.RyugiBindings[0].BindingID)
		syncDone <- err
	}()
	<-inFlightEntered

	pauseDone := make(chan int, 1)
	go func() {
		pReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/admin/gc-pause", strings.NewReader(`{"paused":true,"reason":"backup"}`))
		pReq.Header.Set("X-Orbit-Request", "1")
		pResp, err := http.DefaultClient.Do(pReq)
		if err != nil {
			pauseDone <- 0
			return
		}
		pResp.Body.Close()
		pauseDone <- pResp.StatusCode
	}()

	// Verify gc-pause is STILL waiting while SyncBinding is in-flight
	select {
	case code := <-pauseDone:
		t.Fatalf("gc-pause returned prematurely (%d) while SyncBinding was still in-flight!", code)
	case <-time.After(80 * time.Millisecond):
	}

	// Release in-flight sync -> both SyncBinding and gc-pause must finish cleanly, with ackCount == 1 BEFORE pause returns
	close(releaseInFlight)
	if err := <-syncDone; err != nil {
		t.Fatalf("in-flight SyncBinding failed: %v", err)
	}
	if code := <-pauseDone; code != http.StatusOK {
		t.Fatalf("expected gc-pause 200 after drain, got %d", code)
	}
	mu.Lock()
	gotAck := ackCount
	mu.Unlock()
	if gotAck != 1 {
		t.Fatalf("expected in-flight sync to finish ack before gc-pause returned, got ackCount=%d", gotAck)
	}

	// Item #1: Simulate legacy 0555 <artifactDir> with existing comments.json, then restart NewServer -> auto-calibrates to 0755 preserving comments.json!
	frozenSpecDir := filepath.Join(dataDir, "deliveries", dlvID, ".ryugi", "artifacts", "document", "spec")
	commentsPath := filepath.Join(frozenSpecDir, "comments.json")
	if err := os.WriteFile(commentsPath, []byte(`{"version":1,"threads":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(frozenSpecDir, 0o555); err != nil {
		t.Fatal(err)
	}

	restartCfg := protocol.DefaultHubConfig()
	restartCfg.ConfigDir = cfgDir
	restartCfg.DataDir = dataDir
	restartCfg.DevOpen = true
	if _, err := hub.NewServer(ctx, hub.ServerOptions{
		HubConfig:   restartCfg,
		Store:       st,
		RyugiRunner: fake,
	}); err != nil {
		t.Fatalf("restarted NewServer calibration failed: %v", err)
	}
	stDir, err := os.Stat(frozenSpecDir)
	if err != nil || stDir.Mode().Perm() != 0o755 {
		t.Fatalf("expected startup calibration to restore <artifactDir> 0755, got perm=%04o err=%v", stDir.Mode().Perm(), err)
	}
	if raw, err := os.ReadFile(commentsPath); err != nil || string(raw) != `{"version":1,"threads":[]}` {
		t.Fatalf("expected comments.json preserved across startup calibration, got %q err=%v", string(raw), err)
	}
}
