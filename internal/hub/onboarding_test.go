package hub_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Hebi8/hebi8-Orbit/internal/hub"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

func newOnboardingTestServer(t *testing.T, writeYAML bool) (*hub.Server, *httptest.Server, string, string) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "cfg")
	dataDir := filepath.Join(tmp, "data")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}

	if writeYAML {
		machinesYAML := `schemaVersion: 1
machines:
  primary-box:
    displayName: "primary-box"
    transport: local_ws
    tokenHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    capabilities: [primary-dev]
    adapters:
      agy:
        bin: /usr/bin/true
        maxLiveWorkers: 6
  remote-mac:
    displayName: "remote-mac"
    transport: remote_ws
    capabilities: [macos-env]
`
		if err := os.WriteFile(filepath.Join(cfgDir, "machines.yaml"), []byte(machinesYAML), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	st, err := store.Open(ctx, filepath.Join(dataDir, "orbit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := protocol.DefaultHubConfig()
	cfg.ConfigDir = cfgDir
	cfg.DataDir = dataDir
	cfg.DevToken = "test-dev-token-secret"
	cfg.HubOrigin = "https://orbit.example.ts.net"
	cfg.AllowedTailscaleLogins = []string{"alice@example.com"}
	cfg.RyugiPollIntervalSeconds = 0

	srv, err := hub.NewServer(ctx, hub.ServerOptions{
		HubConfig: cfg,
		Store:     st,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		srv.Shutdown()
		ts.Close()
	})
	return srv, ts, cfgDir, dataDir
}

func doOnbJSONReq(t *testing.T, ts *httptest.Server, method, path string, body any, headers map[string]string) *http.Response {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, ts.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestPairingBindAndDynamicRunnerConnect(t *testing.T) {
	_, ts, _, _ := newOnboardingTestServer(t, true)
	devHdrs := map[string]string{
		"X-Orbit-Dev-Token": "test-dev-token-secret",
		"X-Orbit-Request":   "1",
	}

	// 1. Fresh DB with YAML loaded after Open -> needsOnboarding starts true; test bindMachineId on existing YAML machine lacking tokenHash ("remote-mac")
	resp := doOnbJSONReq(t, ts, http.MethodPost, "/api/pairing/codes", protocol.CreatePairingCodeRequest{
		Scope:         protocol.PairingScopeBoth,
		BindMachineID: "remote-mac",
	}, devHdrs)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create pairing code status=%d", resp.StatusCode)
	}
	var created protocol.CreatePairingCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if len(created.Code) != 9 || created.Code[4] != '-' {
		t.Fatalf("expected XXXX-XXXX pairing code, got %q", created.Code)
	}

	// 2. Claim with browser Origin header MUST be rejected (403)
	badOrigResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/pairing/claim", protocol.ClaimPairingRequest{
		Code:       created.Code,
		WantRunner: true,
		WantClient: true,
	}, map[string]string{
		"Origin":          "https://evil.example.com",
		"X-Orbit-Request": "1",
	})
	badOrigResp.Body.Close()
	if badOrigResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for Origin on claim, got %d", badOrigResp.StatusCode)
	}

	// 3. Legitimate server/CLI claim (no Origin, lowercase & fuzzy O/I normalization)
	claimResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/pairing/claim", protocol.ClaimPairingRequest{
		Code:       strings.ToLower(strings.ReplaceAll(created.Code, "-", "")),
		WantRunner: true,
		WantClient: true,
		Machine: &protocol.ClaimMachineInfo{
			ProposedID: "ignored-because-bound",
			OS:         "darwin",
			Arch:       "arm64",
		},
	}, map[string]string{
		"X-Orbit-Request": "1",
	})
	defer claimResp.Body.Close()
	if claimResp.StatusCode != http.StatusOK {
		t.Fatalf("claim status=%d", claimResp.StatusCode)
	}
	var claimed protocol.ClaimPairingResponse
	if err := json.NewDecoder(claimResp.Body).Decode(&claimed); err != nil {
		t.Fatal(err)
	}
	if claimed.MachineID != "remote-mac" {
		t.Fatalf("expected bound machineId remote-mac, got %q", claimed.MachineID)
	}
	if !strings.HasPrefix(claimed.RunnerToken, "ort_") || !strings.HasPrefix(claimed.ClientToken, "ock_") {
		t.Fatalf("expected ort_ and ock_ tokens, got runner=%q client=%q", claimed.RunnerToken, claimed.ClientToken)
	}

	// 4. Replay of same code MUST fail (401)
	replayResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/pairing/claim", protocol.ClaimPairingRequest{
		Code:       created.Code,
		WantRunner: true,
	}, map[string]string{"X-Orbit-Request": "1"})
	replayResp.Body.Close()
	if replayResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on replay, got %d", replayResp.StatusCode)
	}

	// 5. Client token authenticates API requests; invalid Bearer NEVER falls through to Tailscale/devToken!
	bootOK := doOnbJSONReq(t, ts, http.MethodGet, "/api/bootstrap", nil, map[string]string{
		"Authorization": "Bearer " + claimed.ClientToken,
	})
	bootOK.Body.Close()
	if bootOK.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with ock_ token, got %d", bootOK.StatusCode)
	}

	badBearer := doOnbJSONReq(t, ts, http.MethodGet, "/api/bootstrap", nil, map[string]string{
		"Authorization":        "Bearer ock_invalid_token_value_1234567890",
		"Tailscale-User-Login": "alice@example.com",
		"X-Orbit-Dev-Token":    "test-dev-token-secret",
	})
	badBearer.Body.Close()
	if badBearer.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid Bearer must return 401 without fallthrough, got %d", badBearer.StatusCode)
	}

	// 6. Connect /ws/runner as remote-mac using freshly minted DB credential WITHOUT restarting Hub or editing YAML!
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/runner"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"X-Orbit-Machine-Id": []string{"remote-mac"},
			"Authorization":      []string{"Bearer " + claimed.RunnerToken},
		},
	})
	if err != nil {
		t.Fatalf("paired runner WS dial failed: %v", err)
	}
	defer conn.CloseNow()

	helloBytes, _ := json.Marshal(protocol.RunnerHelloFrame{
		Type:             protocol.RunnerMsgHello,
		ProtocolVersion:  1,
		MachineID:        "remote-mac",
		RunnerInstanceID: "run_test_mac",
		RunnerVersion:    "0.3.0",
		Capabilities:     []string{protocol.RunnerCapAgentsV1, protocol.RunnerCapFSV1},
		AgentProbes: []protocol.AgentProbe{
			{Adapter: protocol.AdapterAgy, State: protocol.AgentStateReady, Enabled: true, BridgeBin: "/usr/local/bin/agy"},
			{Adapter: protocol.AdapterClaude, State: protocol.AgentStateMissingBridge, Enabled: true, MainCLI: "/opt/homebrew/bin/claude", HintCode: "missing_bridge"},
		},
		Resources: map[string]protocol.ResourceState{},
	})
	if err := conn.Write(ctx, websocket.MessageText, helloBytes); err != nil {
		t.Fatal(err)
	}
	_, rawWelcome, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var welcome protocol.HubWelcomeFrame
	if err := json.Unmarshal(rawWelcome, &welcome); err != nil || welcome.Type != protocol.RunnerMsgHubWelcome {
		t.Fatalf("expected hub.welcome, got %s", string(rawWelcome))
	}

	// Verify pairing status reports claimed + online=true
	stResp := doOnbJSONReq(t, ts, http.MethodGet, "/api/pairing/codes/"+created.ID, nil, devHdrs)
	defer stResp.Body.Close()
	var stBody protocol.PairingCodeStatusResponse
	if err := json.NewDecoder(stResp.Body).Decode(&stBody); err != nil {
		t.Fatal(err)
	}
	if stBody.State != protocol.PairingCodeClaimed || !stBody.Online || stBody.MachineID != "remote-mac" {
		t.Fatalf("unexpected pairing status: %+v", stBody)
	}

	// Verify YAML-managed field lock returns 409 ERR_FILE_MANAGED
	lockResp := doOnbJSONReq(t, ts, http.MethodPatch, "/api/machines/primary-box/agents/agy", map[string]any{
		"bin": "/bin/echo",
	}, devHdrs)
	lockResp.Body.Close()
	if lockResp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 ERR_FILE_MANAGED for YAML locked bin, got %d", lockResp.StatusCode)
	}

	// Verify client token (ock_) CANNOT open /ws/runner
	_, httpResp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"X-Orbit-Machine-Id": []string{"remote-mac"},
			"Authorization":      []string{"Bearer " + claimed.ClientToken},
		},
	})
	if err == nil || (httpResp != nil && httpResp.StatusCode != http.StatusUnauthorized) {
		t.Fatalf("expected ock_ token to be rejected on /ws/runner, err=%v resp=%v", err, httpResp)
	}
}

func TestPairingRateLimitAndAuthHandoff(t *testing.T) {
	_, ts, _, _ := newOnboardingTestServer(t, false)
	devHdrs := map[string]string{
		"X-Orbit-Dev-Token": "test-dev-token-secret",
		"X-Orbit-Request":   "1",
	}

	// 10 bad claims return 401; 11th returns 429
	for i := 1; i <= 11; i++ {
		r := doOnbJSONReq(t, ts, http.MethodPost, "/api/pairing/claim", protocol.ClaimPairingRequest{
			Code:       "AAAA-BBBB",
			WantClient: true,
		}, map[string]string{"X-Orbit-Request": "1"})
		r.Body.Close()
		if i <= 10 && r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401, got %d", i, r.StatusCode)
		}
		if i == 11 && r.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("attempt 11: expected 429 rate limit, got %d", r.StatusCode)
		}
	}

	// Auth handoff: create -> claim once -> replay rejected
	hResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/auth/handoff", nil, devHdrs)
	defer hResp.Body.Close()
	if hResp.StatusCode != http.StatusOK {
		t.Fatalf("create handoff status=%d", hResp.StatusCode)
	}
	var handoff protocol.AuthHandoffResponse
	if err := json.NewDecoder(hResp.Body).Decode(&handoff); err != nil {
		t.Fatal(err)
	}

	cResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/auth/handoff/claim", protocol.ClaimAuthHandoffRequest{
		Code: handoff.Code,
	}, map[string]string{"X-Orbit-Request": "1"})
	defer cResp.Body.Close()
	if cResp.StatusCode != http.StatusOK {
		t.Fatalf("claim handoff status=%d", cResp.StatusCode)
	}
	var claimed protocol.ClaimAuthHandoffResponse
	if err := json.NewDecoder(cResp.Body).Decode(&claimed); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(claimed.ClientToken, "ock_") {
		t.Fatalf("expected ock_ token from handoff, got %q", claimed.ClientToken)
	}

	// Replay handoff MUST fail
	rResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/auth/handoff/claim", protocol.ClaimAuthHandoffRequest{
		Code: handoff.Code,
	}, map[string]string{"X-Orbit-Request": "1"})
	rResp.Body.Close()
	if rResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on handoff replay, got %d", rResp.StatusCode)
	}
}

func TestDynamicProjectAndOnboardingFlow(t *testing.T) {
	_, ts, _, _ := newOnboardingTestServer(t, true)
	devHdrs := map[string]string{
		"X-Orbit-Dev-Token": "test-dev-token-secret",
		"X-Orbit-Request":   "1",
	}

	// Create a real temp git repo under HOME (or skip if HOME isn't writable)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	repoDir, err := os.MkdirTemp(home, ".orbit-test-repo-*")
	if err != nil {
		t.Skipf("cannot create temp dir under home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoDir) })
	if out, err := exec.Command("git", "init", repoDir).CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v %s", err, string(out))
	}

	// Path traversal outside $HOME must be rejected
	badDir := doOnbJSONReq(t, ts, http.MethodGet, "/api/machines/primary-box/dirs?path=/etc", nil, devHdrs)
	badDir.Body.Close()
	if badDir.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for /etc outside HOME, got %d", badDir.StatusCode)
	}

	// Dynamic project create via API
	createResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/projects", protocol.CreateProjectRequest{
		MachineID:   "primary-box",
		RepoPath:    repoDir,
		DisplayName: "Demo Repo",
	}, devHdrs)
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create project status=%d", createResp.StatusCode)
	}
	var projOut protocol.CreateProjectResponse
	if err := json.NewDecoder(createResp.Body).Decode(&projOut); err != nil {
		t.Fatal(err)
	}
	if projOut.Project.Source != protocol.ProjectSourceUI {
		t.Fatalf("expected source=ui, got %q", projOut.Project.Source)
	}

	// Complete onboarding
	doneResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/onboarding/complete", protocol.CompleteOnboardingRequest{}, devHdrs)
	defer doneResp.Body.Close()
	if doneResp.StatusCode != http.StatusOK {
		t.Fatalf("complete onboarding status=%d", doneResp.StatusCode)
	}
	var doneOut protocol.CompleteOnboardingResponse
	if err := json.NewDecoder(doneResp.Body).Decode(&doneOut); err != nil {
		t.Fatal(err)
	}
	if doneOut.Setup.NeedsOnboarding {
		t.Fatalf("expected needsOnboarding=false after completion")
	}

	// Delete UI project succeeds
	delResp := doOnbJSONReq(t, ts, http.MethodDelete, "/api/projects/"+projOut.Project.ProjectID, nil, devHdrs)
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete UI project status=%d", delResp.StatusCode)
	}
}

func TestCodexReviewInvariants(t *testing.T) {
	_, ts, _, _ := newOnboardingTestServer(t, true)
	devHdrs := map[string]string{
		"X-Orbit-Dev-Token": "test-dev-token-secret",
		"X-Orbit-Request":   "1",
	}

	// 1. PA actor MUST be forbidden from creating pairing codes (#8b)
	paHdrs := map[string]string{
		"X-Orbit-Dev-Token":  "test-dev-token-secret",
		"X-Orbit-Request":    "1",
		"X-Orbit-Actor-Kind": "pa",
	}
	paResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/pairing/codes", protocol.CreatePairingCodeRequest{
		Scope: protocol.PairingScopeBoth,
	}, paHdrs)
	paResp.Body.Close()
	if paResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for PA pairing creation, got %d", paResp.StatusCode)
	}

	// 2. Runner-only pairing code emits --runner-only command & accepts wantClient=false (#8a)
	roResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/pairing/codes", protocol.CreatePairingCodeRequest{
		Scope: protocol.PairingScopeRunner,
	}, devHdrs)
	defer roResp.Body.Close()
	var roCreated protocol.CreatePairingCodeResponse
	if err := json.NewDecoder(roResp.Body).Decode(&roCreated); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(roCreated.JoinCommand, "--runner-only") {
		t.Fatalf("expected --runner-only in runner-scoped joinCommand, got %q", roCreated.JoinCommand)
	}

	// 3. YAML machine PATCH attempting to alter managed fields returns 409 ERR_FILE_MANAGED (#13)
	yamlPatch := doOnbJSONReq(t, ts, http.MethodPatch, "/api/machines/primary-box", map[string]any{
		"transport": "remote_ws",
	}, devHdrs)
	yamlPatch.Body.Close()
	if yamlPatch.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 ERR_FILE_MANAGED for YAML transport patch, got %d", yamlPatch.StatusCode)
	}

	// 4. Pair a dynamic machine, mint a pre-bound code AND an auth handoff from its client token,
	//    then delete the machine -> BOTH pending bound pairing code AND handoff must be rejected (#9).
	pairCreate := doOnbJSONReq(t, ts, http.MethodPost, "/api/pairing/codes", protocol.CreatePairingCodeRequest{
		Scope: protocol.PairingScopeBoth,
	}, devHdrs)
	defer pairCreate.Body.Close()
	var pCreated protocol.CreatePairingCodeResponse
	if err := json.NewDecoder(pairCreate.Body).Decode(&pCreated); err != nil {
		t.Fatal(err)
	}
	claimResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/pairing/claim", protocol.ClaimPairingRequest{
		Code:       pCreated.Code,
		WantRunner: true,
		WantClient: true,
		Machine:    &protocol.ClaimMachineInfo{ProposedID: "ephemeral-mac"},
	}, map[string]string{"X-Orbit-Request": "1"})
	defer claimResp.Body.Close()
	var pClaimed protocol.ClaimPairingResponse
	if err := json.NewDecoder(claimResp.Body).Decode(&pClaimed); err != nil {
		t.Fatal(err)
	}

	// Create a second pre-bound code for ephemeral-mac AND an auth handoff from its clientToken
	boundCreate := doOnbJSONReq(t, ts, http.MethodPost, "/api/pairing/codes", protocol.CreatePairingCodeRequest{
		Scope:         protocol.PairingScopeRunner,
		BindMachineID: pClaimed.MachineID,
	}, devHdrs)
	defer boundCreate.Body.Close()
	var boundCode protocol.CreatePairingCodeResponse
	if err := json.NewDecoder(boundCreate.Body).Decode(&boundCode); err != nil {
		t.Fatal(err)
	}

	hoResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/auth/handoff", nil, map[string]string{
		"Authorization":   "Bearer " + pClaimed.ClientToken,
		"X-Orbit-Request": "1",
	})
	defer hoResp.Body.Close()
	var ho protocol.AuthHandoffResponse
	if err := json.NewDecoder(hoResp.Body).Decode(&ho); err != nil {
		t.Fatal(err)
	}

	// Delete ephemeral-mac
	rmResp := doOnbJSONReq(t, ts, http.MethodDelete, "/api/machines/"+pClaimed.MachineID, nil, devHdrs)
	rmResp.Body.Close()
	if rmResp.StatusCode != http.StatusOK {
		t.Fatalf("delete dynamic machine status=%d", rmResp.StatusCode)
	}

	// Pre-bound code MUST now fail (revoked inside delete tx)
	reClaim := doOnbJSONReq(t, ts, http.MethodPost, "/api/pairing/claim", protocol.ClaimPairingRequest{
		Code:       boundCode.Code,
		WantRunner: true,
	}, map[string]string{"X-Orbit-Request": "1"})
	reClaim.Body.Close()
	if reClaim.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 claiming revoked pre-bound code after machine delete, got %d", reClaim.StatusCode)
	}

	// Handoff minted before delete MUST now fail (source token & machine revoked)
	hoClaim := doOnbJSONReq(t, ts, http.MethodPost, "/api/auth/handoff/claim", protocol.ClaimAuthHandoffRequest{
		Code: ho.Code,
	}, map[string]string{"X-Orbit-Request": "1"})
	hoClaim.Body.Close()
	if hoClaim.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 claiming handoff after machine delete, got %d", hoClaim.StatusCode)
	}
}
