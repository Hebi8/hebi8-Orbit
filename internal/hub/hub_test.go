package hub_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	srv, _, _, _ := setupTestHub(t, nil, &fakeRyugiRunner{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. Missing Tailscale-User-Login -> 401
	resp, err := http.Get(ts.URL + "/api/bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without Tailscale header, got %d", resp.StatusCode)
	}

	// 2. Non-whitelisted Tailscale login -> 401
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

	// 3. Whitelisted Tailscale login GET -> 200
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/bootstrap", nil)
	req.Header.Set("Tailscale-User-Login", "user@example.com")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for whitelisted login, got %d", resp.StatusCode)
	}

	// 4. Write without X-Orbit-Request: 1 -> 403
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/admin/gc-pause", strings.NewReader(`{"paused":true}`))
	req.Header.Set("Tailscale-User-Login", "user@example.com")
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
	req.Header.Set("Tailscale-User-Login", "user@example.com")
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
	req.Header.Set("Tailscale-User-Login", "user@example.com")
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
			"Tailscale-User-Login": []string{"user@example.com"},
			"Origin":               []string{"https://orbit.example.ts.net:8443"},
		},
	})
	if err == nil || wsResp == nil || wsResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected WS 403 for :8443 origin, got err=%v resp=%v", err, wsResp)
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

	// 2. Blob PUT valid -> 200, HEAD -> 200, GET -> exact bytes
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
	if resp.StatusCode != http.StatusCreated || importResp.Delivery.DeliveryID != dlvID {
		t.Fatalf("expected 201 manual-import, got %d %+v", resp.StatusCode, importResp)
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
