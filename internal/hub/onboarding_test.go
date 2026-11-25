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
	"strconv"
	"strings"
	"sync"
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

func TestBrowserPairClientAndDeviceManagement(t *testing.T) {
	_, ts, _, _ := newOnboardingTestServer(t, false)
	devHdrs := map[string]string{
		"X-Orbit-Dev-Token": "test-dev-token-secret",
		"X-Orbit-Request":   "1",
	}

	// Count machines before browser pairing
	mBeforeResp := doOnbJSONReq(t, ts, http.MethodGet, "/api/machines", nil, devHdrs)
	defer mBeforeResp.Body.Close()
	var mBefore protocol.ListMachinesResponse
	if err := json.NewDecoder(mBeforeResp.Body).Decode(&mBefore); err != nil {
		t.Fatal(err)
	}

	// Create client-only pairing code
	codeResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/pairing/codes", protocol.CreatePairingCodeRequest{
		Scope: protocol.PairingScopeClient,
	}, devHdrs)
	defer codeResp.Body.Close()
	if codeResp.StatusCode != http.StatusCreated {
		t.Fatalf("create code status=%d", codeResp.StatusCode)
	}
	var created protocol.CreatePairingCodeResponse
	if err := json.NewDecoder(codeResp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	// Cross-origin pair-client MUST be rejected
	badOrig := doOnbJSONReq(t, ts, http.MethodPost, "/api/auth/pair-client", protocol.PairClientRequest{
		Code:       created.Code,
		DeviceName: "Evil Browser",
	}, map[string]string{
		"Origin":          "https://evil.example.com",
		"X-Orbit-Request": "1",
	})
	badOrig.Body.Close()
	if badOrig.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-origin pair-client, got %d", badOrig.StatusCode)
	}

	// Same-origin browser pair-client succeeds and returns ock_ client token
	pairResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/auth/pair-client", protocol.PairClientRequest{
		Code:       created.Code,
		DeviceName: "My MacBook Chrome",
	}, map[string]string{
		"Origin":          "https://orbit.example.ts.net",
		"X-Orbit-Request": "1",
	})
	defer pairResp.Body.Close()
	if pairResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on pair-client, got %d", pairResp.StatusCode)
	}
	var paired protocol.PairClientResponse
	if err := json.NewDecoder(pairResp.Body).Decode(&paired); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(paired.ClientToken, "ock_") || paired.TokenID == "" || paired.DeviceName != "My MacBook Chrome" {
		t.Fatalf("unexpected pair-client response: %+v", paired)
	}

	// Browser pairing MUST NOT register a runner/host machine
	mAfterResp := doOnbJSONReq(t, ts, http.MethodGet, "/api/machines", nil, devHdrs)
	defer mAfterResp.Body.Close()
	var mAfter protocol.ListMachinesResponse
	if err := json.NewDecoder(mAfterResp.Body).Decode(&mAfter); err != nil {
		t.Fatal(err)
	}
	if len(mAfter.Machines) != len(mBefore.Machines) {
		t.Fatalf("pair-client should not create execution machines: before=%d after=%d", len(mBefore.Machines), len(mAfter.Machines))
	}

	// Paired browser can access /api/clients and sees itself marked as isCurrent=true
	clientHdrs := map[string]string{
		"Authorization":   "Bearer " + paired.ClientToken,
		"Origin":          "https://orbit.example.ts.net",
		"X-Orbit-Request": "1",
	}
	cliResp := doOnbJSONReq(t, ts, http.MethodGet, "/api/clients", nil, clientHdrs)
	defer cliResp.Body.Close()
	if cliResp.StatusCode != http.StatusOK {
		t.Fatalf("list clients status=%d", cliResp.StatusCode)
	}
	var cliList protocol.ListClientsResponse
	if err := json.NewDecoder(cliResp.Body).Decode(&cliList); err != nil {
		t.Fatal(err)
	}
	foundCurrent := false
	for _, c := range cliList.Clients {
		if c.ID == paired.TokenID && c.IsCurrent && c.Name == "My MacBook Chrome" {
			foundCurrent = true
		}
	}
	if !foundCurrent {
		t.Fatalf("expected paired client %s with isCurrent=true in %+v", paired.TokenID, cliList.Clients)
	}

	// Logout immediately invalidates the client's own token
	logoutResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/auth/logout", map[string]any{}, clientHdrs)
	logoutResp.Body.Close()
	if logoutResp.StatusCode != http.StatusOK {
		t.Fatalf("logout status=%d", logoutResp.StatusCode)
	}
	afterLogout := doOnbJSONReq(t, ts, http.MethodGet, "/api/bootstrap", nil, clientHdrs)
	afterLogout.Body.Close()
	if afterLogout.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 after logout, got %d", afterLogout.StatusCode)
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

func TestClientTokenRevocationDisconnectsWSAndInvalidatesWSTickets(t *testing.T) {
	_, ts, _, _ := newOnboardingTestServer(t, true)
	devHdrs := map[string]string{
		"X-Orbit-Dev-Token": "test-dev-token-secret",
		"X-Orbit-Request":   "1",
	}

	pairClient := func(name string) protocol.PairClientResponse {
		t.Helper()
		cResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/pairing/codes", protocol.CreatePairingCodeRequest{
			Scope: protocol.PairingScopeClient,
		}, devHdrs)
		defer cResp.Body.Close()
		if cResp.StatusCode != http.StatusCreated {
			t.Fatalf("create client pairing code status=%d", cResp.StatusCode)
		}
		var created protocol.CreatePairingCodeResponse
		if err := json.NewDecoder(cResp.Body).Decode(&created); err != nil {
			t.Fatal(err)
		}
		pResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/auth/pair-client", protocol.PairClientRequest{
			Code:       created.Code,
			DeviceName: name,
		}, map[string]string{"X-Orbit-Request": "1"})
		defer pResp.Body.Close()
		if pResp.StatusCode != http.StatusOK {
			t.Fatalf("pair-client status=%d", pResp.StatusCode)
		}
		var paired protocol.PairClientResponse
		if err := json.NewDecoder(pResp.Body).Decode(&paired); err != nil {
			t.Fatal(err)
		}
		return paired
	}

	issueTicket := func(clientToken string) string {
		t.Helper()
		resp := doOnbJSONReq(t, ts, http.MethodPost, "/api/ws-ticket", nil, map[string]string{
			"Authorization":   "Bearer " + clientToken,
			"X-Orbit-Request": "1",
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("ws-ticket status=%d", resp.StatusCode)
		}
		var out protocol.WSTicketResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out.Ticket
	}

	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")

	// Case 1: DELETE /api/clients/{clientId} immediately disconnects active /ws/client AND invalidates unconsumed tickets.
	paired1 := pairClient("laptop-browser")
	ticketLive := issueTicket(paired1.ClientToken)
	ticketUnused := issueTicket(paired1.ClientToken)

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	ws1, _, err := websocket.Dial(dialCtx, wsBase+"/ws/client?ticket="+ticketLive, nil)
	if err != nil {
		t.Fatalf("dial /ws/client with ticketLive failed: %v", err)
	}
	defer ws1.CloseNow()

	// Drain until replay_end so we know the connection is fully registered and streaming.
	readReplayEnd := func(conn *websocket.Conn) {
		t.Helper()
		rctx, rcancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer rcancel()
		for {
			_, data, err := conn.Read(rctx)
			if err != nil {
				t.Fatalf("reading replay_end: %v", err)
			}
			var msg protocol.WSServerMessage
			if err := json.Unmarshal(data, &msg); err == nil && msg.Type == protocol.WSMessageReplayEnd {
				return
			}
		}
	}
	readReplayEnd(ws1)

	// Revoke client token via DELETE /api/clients/{clientId}
	revResp := doOnbJSONReq(t, ts, http.MethodDelete, "/api/clients/"+paired1.TokenID, nil, devHdrs)
	revResp.Body.Close()
	if revResp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /api/clients/%s status=%d", paired1.TokenID, revResp.StatusCode)
	}

	// Active WS MUST be disconnected immediately by server.
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	if _, _, readErr := ws1.Read(closeCtx); readErr == nil {
		t.Fatalf("expected active /ws/client connection to be closed immediately after revoke")
	}

	// Unconsumed ticket issued BEFORE revocation MUST be rejected on /ws/client.
	_, httpResp, dialErr := websocket.Dial(dialCtx, wsBase+"/ws/client?ticket="+ticketUnused, nil)
	if dialErr == nil {
		t.Fatalf("expected /ws/client dial with revoked pre-issued ticket to fail")
	}
	if httpResp != nil && httpResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for revoked ticket, got %d", httpResp.StatusCode)
	}

	// Case 2: POST /api/auth/logout also immediately disconnects active /ws/client AND invalidates unconsumed tickets.
	paired2 := pairClient("phone-browser")
	ticketLive2 := issueTicket(paired2.ClientToken)
	ticketUnused2 := issueTicket(paired2.ClientToken)

	ws2, _, err := websocket.Dial(dialCtx, wsBase+"/ws/client?ticket="+ticketLive2, nil)
	if err != nil {
		t.Fatalf("dial ws2 failed: %v", err)
	}
	defer ws2.CloseNow()
	readReplayEnd(ws2)

	outResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/auth/logout", nil, map[string]string{
		"Authorization":   "Bearer " + paired2.ClientToken,
		"X-Orbit-Request": "1",
	})
	outResp.Body.Close()
	if outResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/auth/logout status=%d", outResp.StatusCode)
	}

	closeCtx2, closeCancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel2()
	if _, _, readErr := ws2.Read(closeCtx2); readErr == nil {
		t.Fatalf("expected active /ws/client connection to close immediately after logout")
	}
	_, httpResp2, dialErr2 := websocket.Dial(dialCtx, wsBase+"/ws/client?ticket="+ticketUnused2, nil)
	if dialErr2 == nil {
		t.Fatalf("expected /ws/client dial with pre-logout ticket to fail")
	}
	if httpResp2 != nil && httpResp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for pre-logout ticket, got %d", httpResp2.StatusCode)
	}
}

func TestDeletePairedMachineDisconnectsClientWSAndInvalidatesTicket(t *testing.T) {
	_, ts, _, _ := newOnboardingTestServer(t, true)
	devHdrs := map[string]string{
		"X-Orbit-Dev-Token": "test-dev-token-secret",
		"X-Orbit-Request":   "1",
	}

	// 1. Issue pairing code and claim BOTH runner + client tokens for a paired machine
	codeResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/pairing/codes", protocol.CreatePairingCodeRequest{
		Scope: protocol.PairingScopeBoth,
	}, devHdrs)
	var created protocol.CreatePairingCodeResponse
	if err := json.NewDecoder(codeResp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	codeResp.Body.Close()

	claimResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/pairing/claim", protocol.ClaimPairingRequest{
		Code:       created.Code,
		WantRunner: true,
		WantClient: true,
		Machine: &protocol.ClaimMachineInfo{
			ProposedID:  "paired-ws-node",
			DisplayName: "Paired WS Node",
			OS:          "linux",
			Arch:        "amd64",
		},
	}, map[string]string{"X-Orbit-Request": "1"})
	var claimed protocol.ClaimPairingResponse
	if err := json.NewDecoder(claimResp.Body).Decode(&claimed); err != nil {
		t.Fatal(err)
	}
	claimResp.Body.Close()
	if claimResp.StatusCode != http.StatusOK || claimed.ClientToken == "" || claimed.MachineID == "" {
		t.Fatalf("claim failed: status=%d resp=%+v", claimResp.StatusCode, claimed)
	}

	issueTicket := func(clientToken string) string {
		t.Helper()
		resp := doOnbJSONReq(t, ts, http.MethodPost, "/api/ws-ticket", nil, map[string]string{
			"Authorization":   "Bearer " + clientToken,
			"X-Orbit-Request": "1",
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /api/ws-ticket status=%d", resp.StatusCode)
		}
		var out protocol.WSTicketResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out.Ticket
	}

	ticketLive := issueTicket(claimed.ClientToken)
	ticketUnused := issueTicket(claimed.ClientToken)

	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()

	wsConn, _, err := websocket.Dial(dialCtx, wsBase+"/ws/client?ticket="+ticketLive, nil)
	if err != nil {
		t.Fatalf("dial /ws/client failed: %v", err)
	}
	defer wsConn.CloseNow()

	// Wait until replay_end so connection is registered in hub client registry
	rctx, rcancel := context.WithTimeout(context.Background(), 3*time.Second)
	for {
		_, data, err := wsConn.Read(rctx)
		if err != nil {
			rcancel()
			t.Fatalf("reading replay_end: %v", err)
		}
		var msg protocol.WSServerMessage
		if json.Unmarshal(data, &msg) == nil && msg.Type == protocol.WSMessageReplayEnd {
			break
		}
	}
	rcancel()

	// 2. DELETE /api/machines/{machineId} MUST immediately close the paired client's active WS with 1008
	//    AND invalidate any unconsumed WS ticket issued before deletion!
	delResp := doOnbJSONReq(t, ts, http.MethodDelete, "/api/machines/"+claimed.MachineID, nil, devHdrs)
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /api/machines/%s status=%d", claimed.MachineID, delResp.StatusCode)
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	_, _, readErr := wsConn.Read(closeCtx)
	if readErr == nil {
		t.Fatalf("expected active /ws/client to disconnect immediately on machine deletion")
	}
	if status := websocket.CloseStatus(readErr); status != websocket.StatusPolicyViolation {
		t.Fatalf("expected close status 1008 (StatusPolicyViolation), got %v (err=%v)", status, readErr)
	}

	_, httpResp, dialErr := websocket.Dial(dialCtx, wsBase+"/ws/client?ticket="+ticketUnused, nil)
	if dialErr == nil {
		t.Fatalf("expected unused pre-deletion ticket to fail /ws/client dial")
	}
	if httpResp != nil && httpResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for pre-deletion ticket, got %d", httpResp.StatusCode)
	}
}

func TestRevokeClientWSSyncCancelsAndAbortsReplay(t *testing.T) {
	_, ts, _, dataDir := newOnboardingTestServer(t, true)
	ctx := context.Background()
	st, err := store.Open(ctx, dataDir+"/orbit.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	devHdrs := map[string]string{
		"X-Orbit-Dev-Token": "test-dev-token-secret",
		"X-Orbit-Request":   "1",
	}

	// Seed 400 timeline events so /ws/client?sinceSeq=0 requires multiple paginated replay batches
	tx, err := st.WriteDB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 400; i++ {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO events (event_id, stream_id, project_id, actor_kind, actor_id, kind, visibility, payload_json, created_at)
			VALUES (?, 'global', '_scratch', 'system', 'test', 'machine.state_changed', 'timeline', '{}', ?)
		`, "evt_replay_abort_"+strings.Repeat("0", 4-len(strconv.Itoa(i)))+strconv.Itoa(i), int64(1700000000000+i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Pair a client
	codeResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/pairing/codes", protocol.CreatePairingCodeRequest{
		Scope: protocol.PairingScopeClient,
	}, devHdrs)
	var created protocol.CreatePairingCodeResponse
	_ = json.NewDecoder(codeResp.Body).Decode(&created)
	codeResp.Body.Close()

	pairResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/auth/pair-client", protocol.PairClientRequest{
		Code:       created.Code,
		DeviceName: "replay-abort-client",
	}, map[string]string{"X-Orbit-Request": "1"})
	var paired protocol.PairClientResponse
	_ = json.NewDecoder(pairResp.Body).Decode(&paired)
	pairResp.Body.Close()

	tResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/ws-ticket", nil, map[string]string{
		"Authorization":   "Bearer " + paired.ClientToken,
		"X-Orbit-Request": "1",
	})
	var tick protocol.WSTicketResponse
	_ = json.NewDecoder(tResp.Body).Decode(&tick)
	tResp.Body.Close()

	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	wsConn, _, err := websocket.Dial(dialCtx, wsBase+"/ws/client?sinceSeq=0&ticket="+tick.Ticket, nil)
	if err != nil {
		t.Fatalf("dial /ws/client failed: %v", err)
	}
	defer wsConn.CloseNow()

	// Read first replay page, then immediately revoke token while remaining pages are pending!
	if _, _, err := wsConn.Read(dialCtx); err != nil {
		t.Fatalf("read first replay page: %v", err)
	}
	revStart := time.Now()
	revResp := doOnbJSONReq(t, ts, http.MethodDelete, "/api/clients/"+paired.TokenID, nil, devHdrs)
	revResp.Body.Close()
	if revResp.StatusCode != http.StatusOK {
		t.Fatalf("revoke status=%d", revResp.StatusCode)
	}
	if time.Since(revStart) > 2*time.Second {
		t.Fatalf("revoke HTTP handler blocked too long: %v", time.Since(revStart))
	}

	// Drain until connection closes: MUST receive 1008 Policy Violation promptly (never hang).
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	var closedErr error
	for i := 0; i < 20; i++ {
		_, _, err := wsConn.Read(closeCtx)
		if err != nil {
			closedErr = err
			break
		}
	}
	if closedErr == nil {
		t.Fatalf("expected WS replay stream to abort and close after revocation")
	}
	if st := websocket.CloseStatus(closedErr); st != websocket.StatusPolicyViolation {
		t.Fatalf("expected 1008 Policy Violation on aborted replay, got %v (err=%v)", st, closedErr)
	}
}

func TestRevokeClientWSSyncBarrierBlocksInFlightReplayWrite(t *testing.T) {
	srv, ts, _, dataDir := newOnboardingTestServer(t, true)
	ctx := context.Background()
	st, err := store.Open(ctx, dataDir+"/orbit.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	devHdrs := map[string]string{
		"X-Orbit-Dev-Token": "test-dev-token-secret",
		"X-Orbit-Request":   "1",
	}

	// Seed 10 events so replay has a page ready to write
	for i := 1; i <= 10; i++ {
		if _, err := st.WriteDB().ExecContext(ctx, `
			INSERT INTO events (event_id, stream_id, project_id, actor_kind, actor_id, kind, visibility, payload_json, created_at)
			VALUES (?, 'global', '_scratch', 'system', 'test', 'machine.state_changed', 'timeline', '{}', ?)
		`, "evt_barrier_"+strconv.Itoa(i), int64(1700000000000+i)); err != nil {
			t.Fatal(err)
		}
	}

	codeResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/pairing/codes", protocol.CreatePairingCodeRequest{
		Scope: protocol.PairingScopeClient,
	}, devHdrs)
	var created protocol.CreatePairingCodeResponse
	_ = json.NewDecoder(codeResp.Body).Decode(&created)
	codeResp.Body.Close()

	pairResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/auth/pair-client", protocol.PairClientRequest{
		Code:       created.Code,
		DeviceName: "barrier-client",
	}, map[string]string{"X-Orbit-Request": "1"})
	var paired protocol.PairClientResponse
	_ = json.NewDecoder(pairResp.Body).Decode(&paired)
	pairResp.Body.Close()

	tResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/ws-ticket", nil, map[string]string{
		"Authorization":   "Bearer " + paired.ClientToken,
		"X-Orbit-Request": "1",
	})
	var tick protocol.WSTicketResponse
	_ = json.NewDecoder(tResp.Body).Decode(&tick)
	tResp.Body.Close()

	// Pause replay AFTER passing the revoked check, RIGHT before writeData()!
	hookEntered := make(chan struct{})
	releaseHook := make(chan struct{})
	var enteredOnce sync.Once
	srv.SetWSBeforeReplayWriteHook(func() {
		enteredOnce.Do(func() {
			close(hookEntered)
			<-releaseHook
		})
	})
	defer srv.SetWSBeforeReplayWriteHook(nil)

	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	wsConn, _, err := websocket.Dial(dialCtx, wsBase+"/ws/client?sinceSeq=0&ticket="+tick.Ticket, nil)
	if err != nil {
		t.Fatalf("dial /ws/client failed: %v", err)
	}
	defer wsConn.CloseNow()

	select {
	case <-hookEntered:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for replay hook")
	}

	// Revoke token while replay is paused right before writeData, and wait for revoke HTTP 200 to return!
	revResp := doOnbJSONReq(t, ts, http.MethodDelete, "/api/clients/"+paired.TokenID, nil, devHdrs)
	revResp.Body.Close()
	if revResp.StatusCode != http.StatusOK {
		t.Fatalf("revoke status=%d", revResp.StatusCode)
	}

	// Now release hook -> writeData() MUST acquire writeMu, see revoked==true, and NEVER write the replay page!
	close(releaseHook)

	readCtx, readCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readCancel()
	_, data, readErr := wsConn.Read(readCtx)
	if readErr == nil {
		t.Fatalf("expected ZERO frames written after revoke returned, but received frame: %s", string(data))
	}
	if st := websocket.CloseStatus(readErr); st != websocket.StatusPolicyViolation {
		t.Fatalf("expected direct 1008 Policy Violation without any replay frame, got status=%v err=%v", st, readErr)
	}
}

func TestRevokeClientWSConcurrentCallsWaitForSameWriteBarrier(t *testing.T) {
	srv, ts, _, dataDir := newOnboardingTestServer(t, true)
	ctx := context.Background()
	st, err := store.Open(ctx, dataDir+"/orbit.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	devHdrs := map[string]string{
		"X-Orbit-Dev-Token": "test-dev-token-secret",
		"X-Orbit-Request":   "1",
	}

	for i := 1; i <= 5; i++ {
		if _, err := st.WriteDB().ExecContext(ctx, `
			INSERT INTO events (event_id, stream_id, project_id, actor_kind, actor_id, kind, visibility, payload_json, created_at)
			VALUES (?, 'global', '_scratch', 'system', 'test', 'machine.state_changed', 'timeline', '{}', ?)
		`, "evt_concur_rev_"+strconv.Itoa(i), int64(1700000000000+i)); err != nil {
			t.Fatal(err)
		}
	}

	codeResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/pairing/codes", protocol.CreatePairingCodeRequest{
		Scope: protocol.PairingScopeClient,
	}, devHdrs)
	var created protocol.CreatePairingCodeResponse
	_ = json.NewDecoder(codeResp.Body).Decode(&created)
	codeResp.Body.Close()

	pairResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/auth/pair-client", protocol.PairClientRequest{
		Code:       created.Code,
		DeviceName: "concurrent-revoke-client",
	}, map[string]string{"X-Orbit-Request": "1"})
	var paired protocol.PairClientResponse
	_ = json.NewDecoder(pairResp.Body).Decode(&paired)
	pairResp.Body.Close()

	tResp := doOnbJSONReq(t, ts, http.MethodPost, "/api/ws-ticket", nil, map[string]string{
		"Authorization":   "Bearer " + paired.ClientToken,
		"X-Orbit-Request": "1",
	})
	var tick protocol.WSTicketResponse
	_ = json.NewDecoder(tResp.Body).Decode(&tick)
	tResp.Body.Close()

	// Pause replay INSIDE writeData() while writeMu is held!
	insideWrite := make(chan struct{})
	releaseWrite := make(chan struct{})
	var once sync.Once
	srv.SetWSInsideWriteHook(func() {
		once.Do(func() {
			close(insideWrite)
			<-releaseWrite
		})
	})
	defer srv.SetWSInsideWriteHook(nil)

	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	wsConn, _, err := websocket.Dial(dialCtx, wsBase+"/ws/client?sinceSeq=0&ticket="+tick.Ticket, nil)
	if err != nil {
		t.Fatalf("dial /ws/client failed: %v", err)
	}
	defer wsConn.CloseNow()

	select {
	case <-insideWrite:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for writeMu hook")
	}

	// Launch first revoke (holds inFlightRevokes[tokenID], waits on writeMu barrier)
	rev1Done := make(chan int, 1)
	go func() {
		resp := doOnbJSONReq(t, ts, http.MethodDelete, "/api/clients/"+paired.TokenID, nil, devHdrs)
		resp.Body.Close()
		rev1Done <- resp.StatusCode
	}()

	// Give rev1 a moment to enter inFlightRevokes, then launch a CONCURRENT second revoke for the same tokenID
	time.Sleep(50 * time.Millisecond)
	rev2Done := make(chan int, 1)
	go func() {
		resp := doOnbJSONReq(t, ts, http.MethodDelete, "/api/clients/"+paired.TokenID, nil, devHdrs)
		resp.Body.Close()
		rev2Done <- resp.StatusCode
	}()

	// NEITHER revoke #1 NOR concurrent revoke #2 may return before writeMu barrier is released!
	select {
	case code := <-rev1Done:
		t.Fatalf("revoke #1 returned early (%d) while writeMu barrier was still held!", code)
	case code := <-rev2Done:
		t.Fatalf("concurrent revoke #2 returned early (%d) before in-flight writeMu barrier finished!", code)
	case <-time.After(150 * time.Millisecond):
	}

	// Release writeMu barrier -> BOTH revokes now complete cleanly with HTTP 200!
	close(releaseWrite)
	for i, ch := range []chan int{rev1Done, rev2Done} {
		select {
		case code := <-ch:
			if code != http.StatusOK {
				t.Fatalf("revoke #%d status=%d want 200", i+1, code)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("revoke #%d timed out after releasing writeMu barrier", i+1)
		}
	}

	// And client receives direct 1008 Policy Violation without receiving the paused replay frame!
	readCtx, readCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readCancel()
	_, data, readErr := wsConn.Read(readCtx)
	if readErr == nil {
		t.Fatalf("expected ZERO frames written after revoke, got: %s", string(data))
	}
	if st := websocket.CloseStatus(readErr); st != websocket.StatusPolicyViolation {
		t.Fatalf("expected 1008 Policy Violation, got status=%v err=%v", st, readErr)
	}
}
