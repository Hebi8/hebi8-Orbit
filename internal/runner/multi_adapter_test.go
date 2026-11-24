package runner

import (
	"context"
	"sync"
	"testing"

	acp "github.com/coder/acp-go-sdk"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

type stubAdapterDriver struct {
	mu             sync.Mutex
	started        bool
	closed         bool
	metaSP         bool
	sessionOpts    []acp.SessionConfigOption
	newSessions    int
	prompts        []string
	setConfigCalls []struct{ id, val string }
	live           map[acp.SessionId]bool
	nextSeq        int
}

func (s *stubAdapterDriver) Start(_ context.Context, _ SessionUpdateHandler, _ PermissionHandler) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = true
	if s.live == nil {
		s.live = map[acp.SessionId]bool{}
	}
	return nil
}

func (s *stubAdapterDriver) Close(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *stubAdapterDriver) SupportsResume() bool           { return true }
func (s *stubAdapterDriver) SupportsLoad() bool             { return true }
func (s *stubAdapterDriver) SupportsMetaSystemPrompt() bool { return s.metaSP }
func (s *stubAdapterDriver) IsSessionLiveInMemory(sid acp.SessionId) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live[sid]
}

func (s *stubAdapterDriver) NewSession(_ context.Context, _, _ string) (acp.NewSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.newSessions++
	s.nextSeq++
	sid := acp.SessionId("sid_" + string(rune('a'+s.nextSeq)))
	s.live[sid] = true
	return acp.NewSessionResponse{SessionId: sid, ConfigOptions: s.sessionOpts}, nil
}

func (s *stubAdapterDriver) ResumeSession(_ context.Context, sid acp.SessionId, _ string) (acp.ResumeSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live[sid] = true
	return acp.ResumeSessionResponse{ConfigOptions: s.sessionOpts}, nil
}

func (s *stubAdapterDriver) LoadSession(_ context.Context, sid acp.SessionId, _ string) (acp.LoadSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live[sid] = true
	return acp.LoadSessionResponse{ConfigOptions: s.sessionOpts}, nil
}

func (s *stubAdapterDriver) CloseSession(_ context.Context, sid acp.SessionId) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.live, sid)
	return nil
}

func (s *stubAdapterDriver) SetConfigOption(_ context.Context, _ acp.SessionId, configID, value string) (acp.SetSessionConfigOptionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setConfigCalls = append(s.setConfigCalls, struct{ id, val string }{id: configID, val: value})
	updated := make([]acp.SessionConfigOption, 0, len(s.sessionOpts))
	for _, o := range s.sessionOpts {
		if o.Select != nil && string(o.Select.Id) == configID {
			cp := *o.Select
			cp.CurrentValue = acp.SessionConfigValueId(value)
			updated = append(updated, acp.SessionConfigOption{Select: &cp})
		} else {
			updated = append(updated, o)
		}
	}
	return acp.SetSessionConfigOptionResponse{ConfigOptions: updated}, nil
}

func (s *stubAdapterDriver) Prompt(_ context.Context, _ acp.SessionId, promptText string) (acp.PromptResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompts = append(s.prompts, promptText)
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (s *stubAdapterDriver) Cancel(_ context.Context, _ acp.SessionId) error { return nil }

func makeSelectOpt(id string, cat acp.SessionConfigOptionCategory, cur string, vals ...string) acp.SessionConfigOption {
	opts := make(acp.SessionConfigSelectOptionsUngrouped, 0, len(vals))
	for _, v := range vals {
		opts = append(opts, acp.SessionConfigSelectOption{Value: acp.SessionConfigValueId(v), Name: v})
	}
	return acp.SessionConfigOption{
		Select: &acp.SessionConfigOptionSelect{
			Id:           acp.SessionConfigId(id),
			Category:     &cat,
			CurrentValue: acp.SessionConfigValueId(cur),
			Options:      acp.SessionConfigSelectOptions{Ungrouped: &opts},
		},
	}
}

func TestResolveConfigOptionTarget_CrossAdapterMapping(t *testing.T) {
	catThought := acp.SessionConfigOptionCategoryThoughtLevel
	catMode := acp.SessionConfigOptionCategoryMode
	catModel := acp.SessionConfigOptionCategoryModel

	claudeOpts := []acp.SessionConfigOption{
		makeSelectOpt("model", catModel, "opus", "default", "opus", "sonnet"),
		makeSelectOpt("effort", catThought, "xhigh", "default", "low", "medium", "high", "xhigh"),
		makeSelectOpt("mode", catMode, "auto", "default", "acceptEdits", "plan", "auto", "bypassPermissions"),
	}
	codexOpts := []acp.SessionConfigOption{
		makeSelectOpt("model", catModel, "gpt-6-sol", "gpt-6.2-sol", "gpt-6-sol"),
		makeSelectOpt("reasoning_effort", catThought, "max", "low", "medium", "high", "xhigh", "max"),
		makeSelectOpt("mode", catMode, "agent", "read-only", "workspace-write", "agent", "agent-full-access"),
	}

	id, val := resolveConfigOptionTarget(claudeOpts, "thought_level", "high")
	if id != "effort" || val != "high" {
		t.Fatalf("claude thought_level -> got (%q,%q), want (effort,high)", id, val)
	}
	id, val = resolveConfigOptionTarget(claudeOpts, "mode", "full")
	if id != "mode" || val != "bypassPermissions" {
		t.Fatalf("claude mode=full -> got (%q,%q), want (mode,bypassPermissions)", id, val)
	}
	id, val = resolveConfigOptionTarget(codexOpts, "thought_level", "high")
	if id != "reasoning_effort" || val != "high" {
		t.Fatalf("codex thought_level -> got (%q,%q), want (reasoning_effort,high)", id, val)
	}
	id, val = resolveConfigOptionTarget(codexOpts, "mode", "auto")
	if id != "mode" || val != "agent" {
		t.Fatalf("codex mode=auto -> got (%q,%q), want (mode,agent)", id, val)
	}
	id, val = resolveConfigOptionTarget(codexOpts, "mode", "full")
	if id != "mode" || val != "agent-full-access" {
		t.Fatalf("codex mode=full -> got (%q,%q), want (mode,agent-full-access)", id, val)
	}
}

func TestMultiAdapterRunner_RoutesAndSwitchesAdaptersCleanly(t *testing.T) {
	ctx := context.Background()
	catThought := acp.SessionConfigOptionCategoryThoughtLevel
	catMode := acp.SessionConfigOptionCategoryMode

	agyDrv := &stubAdapterDriver{metaSP: true}
	claudeDrv := &stubAdapterDriver{
		metaSP: false,
		sessionOpts: []acp.SessionConfigOption{
			makeSelectOpt("effort", catThought, "xhigh", "low", "medium", "high", "xhigh"),
			makeSelectOpt("mode", catMode, "auto", "default", "auto", "bypassPermissions"),
		},
	}
	codexDrv := &stubAdapterDriver{
		metaSP: false,
		sessionOpts: []acp.SessionConfigOption{
			makeSelectOpt("reasoning_effort", catThought, "max", "low", "medium", "high", "max"),
			makeSelectOpt("mode", catMode, "agent", "read-only", "workspace-write", "agent", "agent-full-access"),
		},
	}

	r, err := New(ctx, Config{
		MachineID:  "hub-node",
		DataDir:    t.TempDir(),
		SocketPath: "none",
		Drivers: map[protocol.Adapter]AcpAdapterDriver{
			protocol.AdapterAgy:    agyDrv,
			protocol.AdapterClaude: claudeDrv,
			protocol.AdapterCodex:  codexDrv,
		},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatalf("runner.Start: %v", err)
	}
	defer func() { _ = r.Close(context.Background()) }()

	// Primary agy starts eagerly; secondary claude/codex start lazily on first dispatch.
	if !agyDrv.started || claudeDrv.started || codexDrv.started {
		t.Fatalf("unexpected startup states: agy=%v claude=%v codex=%v", agyDrv.started, claudeDrv.started, codexDrv.started)
	}

	// 1. Dispatch Claude attempt with logical thought_level=high, mode=full
	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:          "att_claude_1",
		TaskID:             "tsk_multi_1",
		ProjectID:          protocol.ScratchProjectID,
		WBSessionID:        "ses_multi_1",
		LeaseToken:         "tok_1",
		Adapter:            protocol.AdapterClaude,
		Prompt:             "hello claude",
		SystemPromptAppend: "contract text",
		DesiredConfig:      map[string]string{"thought_level": "high", "mode": "full"},
	})
	if err := r.WaitAttemptDone(ctx, "att_claude_1"); err != nil {
		t.Fatalf("wait claude: %v", err)
	}
	if !claudeDrv.started || len(claudeDrv.prompts) != 1 {
		t.Fatalf("expected claude driver to handle attempt, started=%v prompts=%d", claudeDrv.started, len(claudeDrv.prompts))
	}
	if len(agyDrv.prompts) != 0 || len(codexDrv.prompts) != 0 {
		t.Fatalf("other drivers must not be invoked: agy=%d codex=%d", len(agyDrv.prompts), len(codexDrv.prompts))
	}
	// Verify contract prefix was prepended since claudeDrv.metaSP == false
	if claudeDrv.prompts[0] == "hello claude" {
		t.Fatalf("expected <orbit_contract> prefix for non-meta adapter, got %q", claudeDrv.prompts[0])
	}
	if len(claudeDrv.setConfigCalls) != 2 {
		t.Fatalf("expected 2 config calls on claude, got %+v", claudeDrv.setConfigCalls)
	}

	// 2. Switch SAME wbSessionID to Codex -> MUST open fresh codex session & map mode=auto -> agent!
	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:     "att_codex_2",
		TaskID:        "tsk_multi_1",
		ProjectID:     protocol.ScratchProjectID,
		WBSessionID:   "ses_multi_1",
		LeaseToken:    "tok_2",
		Adapter:       protocol.AdapterCodex,
		Prompt:        "hello codex",
		DesiredConfig: map[string]string{"thought_level": "high", "mode": "auto"},
	})
	if err := r.WaitAttemptDone(ctx, "att_codex_2"); err != nil {
		t.Fatalf("wait codex: %v", err)
	}
	if !codexDrv.started || codexDrv.newSessions != 1 || len(codexDrv.prompts) != 1 {
		t.Fatalf("expected codex driver to open fresh session, started=%v new=%d prompts=%d",
			codexDrv.started, codexDrv.newSessions, len(codexDrv.prompts))
	}
	seenReasoning, seenAgent := false, false
	for _, c := range codexDrv.setConfigCalls {
		if c.id == "reasoning_effort" && c.val == "high" {
			seenReasoning = true
		}
		if c.id == "mode" && c.val == "agent" {
			seenAgent = true
		}
	}
	if !seenReasoning || !seenAgent {
		t.Fatalf("codex config mapping mismatch: %+v", codexDrv.setConfigCalls)
	}
}
