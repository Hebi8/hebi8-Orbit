package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/coder/websocket"
	"github.com/oklog/ulid/v2"

	"github.com/Hebi8/hebi8-Orbit/internal/gitops"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

// PermissionDecisionPolicy decides whether an ACP RequestPermission call can be auto-allowed
// locally (recording an audit event) or requires explicit human approval via Hub.
type PermissionDecisionPolicy func(attWorktree string, desiredMode string, req acp.RequestPermissionRequest) (autoAllowOptionID string, autoAllow bool)

// ResourceProbeFunc probes exclusive machine resources (returns free/busy/unavailable).
type ResourceProbeFunc func(ctx context.Context) map[string]protocol.ResourceState

// Config configures a Runner instance.
type Config struct {
	MachineID               string
	HubWSURL                string
	HubHTTPURL              string
	BearerToken             string
	DataDir                 string
	OutboxDBPath            string
	SocketPath              string
	RunnerVersion           string
	Driver                  AcpAdapterDriver
	ConfigApplyTimeout      time.Duration
	ChunkCoalesceWindow     time.Duration
	DefaultSwarmIdleTimeout time.Duration
	IdleCloseSeconds        int
	ReconnectInitialDelay   time.Duration
	ReconnectMaxDelay       time.Duration
	PermissionPolicy        PermissionDecisionPolicy
	ResourceProbe           ResourceProbeFunc
	// Test hooks for deterministic R3 crash simulation
	OnAfterACPApprovalWrite func(approvalID string) bool
}

type sessionSlot struct {
	mu                sync.Mutex
	wbSessionID       string
	providerSessionID acp.SessionId
	cwd               string
	runtimeState      protocol.SessionRuntimeState
	inFlightPrompt    bool
	locked            bool
	lastActivityAt    time.Time
}

type pendingApprovalHandle struct {
	approvalID    string
	attemptID     string
	payloadSHA256 string
	options       []protocol.ApprovalOption
	ch            chan protocol.ApprovalDecisionPayload
}

type attemptContext struct {
	mu                sync.Mutex
	dispatch          protocol.AttemptDispatchFrame
	worktreePath      string
	baseSHA           string
	state             protocol.AttemptState
	resumeState       protocol.ResumeState
	providerSessionID acp.SessionId
	promptReturned    bool
	finalized         bool
	heldResources     []string
	tracker           *ActiveSwarmTracker
	pendingApprovals  map[string]*pendingApprovalHandle
	ackPendingWG      sync.WaitGroup
	swarmIdleTimeout  time.Duration
	swarmIdleTimer    *time.Timer
	cancelFn          context.CancelFunc
	doneCh            chan struct{}
}

// Runner is the pure-Go thin ACP runner bridging Hub (/ws/runner) and AcpAdapterDriver.
type Runner struct {
	cfg        Config
	instanceID string
	outbox     *LocalOutbox
	driver     AcpAdapterDriver
	coalescer  *ChunkCoalescer
	sockSrv    *localSocketServer

	mu            sync.Mutex
	sessions      map[string]*sessionSlot
	attempts      map[string]*attemptContext
	sessionOwner  map[acp.SessionId]string
	resourceLocks map[string]string // resourceName -> attemptID

	connMu   sync.Mutex
	wsConn   *websocket.Conn
	wsSendCh chan []byte
	wsDone   chan struct{}
	hbPeriod time.Duration

	outboxWakeCh chan struct{}
	runCtx       context.Context
	runCancel    context.CancelFunc
	wg           sync.WaitGroup
}

// New creates and initializes a Runner with its local SQLite Outbox and chunk coalescer.
func New(ctx context.Context, cfg Config) (*Runner, error) {
	if strings.TrimSpace(cfg.MachineID) == "" {
		return nil, errors.New("machineId is required")
	}
	if cfg.Driver == nil {
		return nil, errors.New("acp driver is required")
	}
	if cfg.DataDir == "" {
		cfg.DataDir = protocol.ExpandPath("~/.local/share/hebi8-orbit")
	} else {
		cfg.DataDir = protocol.ExpandPath(cfg.DataDir)
	}
	if cfg.OutboxDBPath == "" {
		cfg.OutboxDBPath = filepath.Join(cfg.DataDir, fmt.Sprintf("runner-outbox-%s.sqlite", cfg.MachineID))
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = filepath.Join(cfg.DataDir, fmt.Sprintf("runner-%s.sock", cfg.MachineID))
	}
	if cfg.RunnerVersion == "" {
		cfg.RunnerVersion = "0.2.0-phase1"
	}
	if cfg.ConfigApplyTimeout <= 0 {
		cfg.ConfigApplyTimeout = 10 * time.Second
	}
	if cfg.ChunkCoalesceWindow <= 0 {
		cfg.ChunkCoalesceWindow = 200 * time.Millisecond
	}
	if cfg.DefaultSwarmIdleTimeout <= 0 {
		cfg.DefaultSwarmIdleTimeout = 120 * time.Second
	}
	if cfg.ReconnectInitialDelay <= 0 {
		cfg.ReconnectInitialDelay = 1 * time.Second
	}
	if cfg.ReconnectMaxDelay <= 0 {
		cfg.ReconnectMaxDelay = 15 * time.Second
	}
	if cfg.PermissionPolicy == nil {
		cfg.PermissionPolicy = defaultPermissionPolicy
	}

	outbox, err := OpenLocalOutbox(ctx, cfg.OutboxDBPath)
	if err != nil {
		return nil, err
	}

	r := &Runner{
		cfg:           cfg,
		instanceID:    "run_" + ulid.Make().String(),
		outbox:        outbox,
		driver:        cfg.Driver,
		sessions:      map[string]*sessionSlot{},
		attempts:      map[string]*attemptContext{},
		sessionOwner:  map[acp.SessionId]string{},
		resourceLocks: map[string]string{},
		hbPeriod:      10 * time.Second,
		outboxWakeCh:  make(chan struct{}, 1),
	}
	r.coalescer = NewChunkCoalescer(cfg.ChunkCoalesceWindow, r.flushCoalescedChunk)
	return r, nil
}

// InstanceID returns this runner process's unique UUID (changes on every restart).
func (r *Runner) InstanceID() string { return r.instanceID }

// SocketPath returns the local Unix domain socket path for `orbit deliver`.
func (r *Runner) SocketPath() string { return r.cfg.SocketPath }

// Outbox exposes the local persistent outbox (primarily for tests/diagnostics).
func (r *Runner) Outbox() *LocalOutbox { return r.outbox }

// Start boots the ACP driver, opens the local Unix domain socket, and connects to Hub /ws/runner.
func (r *Runner) Start(ctx context.Context) error {
	r.runCtx, r.runCancel = context.WithCancel(ctx)

	if err := r.driver.Start(r.runCtx, r.handleSessionUpdate, r.handlePermissionRequest); err != nil {
		return fmt.Errorf("start acp driver: %w", err)
	}

	if r.cfg.SocketPath != "" && r.cfg.SocketPath != "none" {
		sockSrv, err := startLocalSocketServer(r.cfg.SocketPath, r.HandleLocalDeliver)
		if err != nil {
			_ = r.driver.Close(ctx)
			return err
		}
		r.sockSrv = sockSrv
	}

	if strings.TrimSpace(r.cfg.HubWSURL) != "" {
		r.wg.Add(1)
		go r.wsReconnectLoop()
	}

	r.wg.Add(1)
	go r.outboxPumpLoop()

	if r.cfg.IdleCloseSeconds > 0 {
		r.wg.Add(1)
		go r.idleSweepLoop()
	}

	return nil
}

// Close gracefully terminates WS connections, active attempts, local socket, ACP subprocess, and SQLite outbox.
func (r *Runner) Close(ctx context.Context) error {
	if r.runCancel != nil {
		r.runCancel()
	}
	r.coalescer.Close()

	// Cancel all active attempts cleanly
	r.mu.Lock()
	atts := make([]*attemptContext, 0, len(r.attempts))
	for _, a := range r.attempts {
		atts = append(atts, a)
	}
	r.mu.Unlock()
	for _, a := range atts {
		a.mu.Lock()
		if a.swarmIdleTimer != nil {
			a.swarmIdleTimer.Stop()
		}
		if a.cancelFn != nil {
			a.cancelFn()
		}
		a.mu.Unlock()
	}

	r.closeActiveWS(websocket.StatusGoingAway, "runner shutting down")
	r.wg.Wait()

	var err1, err2, err3 error
	if r.sockSrv != nil {
		err1 = r.sockSrv.Close()
	}
	err2 = r.driver.Close(ctx)
	err3 = r.outbox.Close()
	return errors.Join(err1, err2, err3)
}

// SimulateAbruptDisconnect tears down the current WS TCP connection immediately without terminating
// in-memory attempts or changing runnerInstanceId (for testing transient disconnect & redelivery in R3).
func (r *Runner) SimulateAbruptDisconnect() {
	r.connMu.Lock()
	conn := r.wsConn
	r.connMu.Unlock()
	if conn != nil {
		_ = conn.CloseNow()
	}
}

// WaitAttemptDone waits for a specific attempt goroutine to exit (helper for tests).
func (r *Runner) WaitAttemptDone(ctx context.Context, attemptID string) error {
	r.mu.Lock()
	a, ok := r.attempts[attemptID]
	r.mu.Unlock()
	if !ok {
		return nil
	}
	select {
	case <-a.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// HeldResourceOwner returns the attemptID holding resourceName, or "" if free.
func (r *Runner) HeldResourceOwner(resourceName string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resourceLocks[resourceName]
}

// SessionStateSnapshot inspects the internal state of wbSessionID (for concurrency tests R1/R2).
func (r *Runner) SessionStateSnapshot(wbSessionID string) (protocol.SessionRuntimeState, acp.SessionId, string, bool) {
	r.mu.Lock()
	slot, ok := r.sessions[wbSessionID]
	r.mu.Unlock()
	if !ok {
		return "", "", "", false
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	return slot.runtimeState, slot.providerSessionID, slot.cwd, slot.locked
}

func (r *Runner) getOrCreateSessionSlot(wbSessionID string) *sessionSlot {
	r.mu.Lock()
	defer r.mu.Unlock()
	slot, ok := r.sessions[wbSessionID]
	if !ok {
		slot = &sessionSlot{
			wbSessionID:    wbSessionID,
			runtimeState:   protocol.SessionStarting,
			lastActivityAt: time.Now(),
		}
		r.sessions[wbSessionID] = slot
	}
	return slot
}

// HandleDispatch accepts an attempt.dispatch frame (deduplicated by attemptId) and starts execution.
func (r *Runner) HandleDispatch(dispatch protocol.AttemptDispatchFrame) {
	r.mu.Lock()
	if _, exists := r.attempts[dispatch.AttemptID]; exists {
		r.mu.Unlock()
		return
	}
	actx, cancel := context.WithCancel(r.runCtx)
	swarmTimeout := r.cfg.DefaultSwarmIdleTimeout
	if dispatch.SwarmIdleTimeoutMs > 0 {
		swarmTimeout = time.Duration(dispatch.SwarmIdleTimeoutMs) * time.Millisecond
	}
	att := &attemptContext{
		dispatch:         dispatch,
		state:            protocol.AttemptLeased,
		tracker:          NewActiveSwarmTracker(dispatch.AttemptID),
		pendingApprovals: map[string]*pendingApprovalHandle{},
		swarmIdleTimeout: swarmTimeout,
		cancelFn:         cancel,
		doneCh:           make(chan struct{}),
	}
	r.attempts[dispatch.AttemptID] = att
	r.mu.Unlock()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer close(att.doneCh)
		r.executeAttempt(actx, att)
	}()
}

func (r *Runner) executeAttempt(ctx context.Context, att *attemptContext) {
	dispatch := att.dispatch
	defer r.releaseAttemptResources(att)

	// 1. Transition to preparing
	r.setAttemptState(att, protocol.AttemptPreparing, map[string]any{
		"state": string(protocol.AttemptPreparing),
	})

	// 2. Acquire exclusive machine resources atomically in lexicographical order
	if len(dispatch.RequiredResources) > 0 {
		for {
			if err := ctx.Err(); err != nil {
				r.finalizeAttempt(att, protocol.AttemptCancelled, map[string]any{
					"state":       string(protocol.AttemptCancelled),
					"errorCode":   "CANCELLED",
					"errorDetail": "cancelled while waiting for resources",
				})
				return
			}
			ok, reason := r.tryAcquireResources(ctx, att, dispatch.RequiredResources)
			if ok {
				break
			}
			r.setAttemptState(att, protocol.AttemptWaitingResource, map[string]any{
				"state":         string(protocol.AttemptWaitingResource),
				"waitingReason": reason,
			})
			select {
			case <-ctx.Done():
				r.finalizeAttempt(att, protocol.AttemptCancelled, map[string]any{
					"state":       string(protocol.AttemptCancelled),
					"errorCode":   "CANCELLED",
					"errorDetail": "cancelled while waiting for resources",
				})
				return
			case <-time.After(1 * time.Second):
			}
		}
		r.setAttemptState(att, protocol.AttemptPreparing, map[string]any{
			"state": string(protocol.AttemptPreparing),
		})
	}

	// 3. Prepare isolated worktree and fixed review base SHA
	worktreePath, baseSHA, err := PrepareWorktree(ctx, dispatch, r.cfg.DataDir)
	if err != nil {
		r.finalizeAttempt(att, protocol.AttemptFailed, map[string]any{
			"state":       string(protocol.AttemptFailed),
			"errorCode":   "WORKTREE_PREPARE_FAILED",
			"errorDetail": err.Error(),
		})
		return
	}
	att.mu.Lock()
	att.worktreePath = worktreePath
	att.baseSHA = baseSHA
	att.mu.Unlock()

	// 4. Per-wbSessionId single-flight serialization (prevents close vs dispatch race!)
	wbSessionID := dispatch.WBSessionID
	if wbSessionID == "" {
		wbSessionID = "ses_" + dispatch.AttemptID
	}
	slot := r.getOrCreateSessionSlot(wbSessionID)

	slot.mu.Lock()
	sid, promptText, err := r.ensureSessionReadyLocked(ctx, att, slot)
	if err != nil {
		slot.runtimeState = protocol.SessionDead
		slot.mu.Unlock()
		// Immediate release of held resources on CONFIG_APPLY_FAILED / session failure
		r.releaseAttemptResources(att)
		errCode := "SESSION_PREPARE_FAILED"
		if strings.Contains(err.Error(), "CONFIG_APPLY_FAILED") {
			errCode = "CONFIG_APPLY_FAILED"
		}
		r.finalizeAttempt(att, protocol.AttemptFailed, map[string]any{
			"state":        string(protocol.AttemptFailed),
			"worktreePath": worktreePath,
			"baseSha":      baseSHA,
			"errorCode":    errCode,
			"errorDetail":  err.Error(),
		})
		return
	}

	slot.providerSessionID = sid
	slot.cwd = worktreePath
	slot.runtimeState = protocol.SessionLive
	slot.inFlightPrompt = true
	slot.lastActivityAt = time.Now()

	att.mu.Lock()
	att.providerSessionID = sid
	att.mu.Unlock()

	r.mu.Lock()
	r.sessionOwner[sid] = dispatch.AttemptID
	r.mu.Unlock()
	slot.mu.Unlock()

	// 5. Enter running state and send ACP session/prompt
	r.setAttemptState(att, protocol.AttemptRunning, map[string]any{
		"state":             string(protocol.AttemptRunning),
		"providerSessionId": string(sid),
		"worktreePath":      worktreePath,
		"baseSha":           baseSHA,
	})

	promptResp, promptErr := r.driver.Prompt(ctx, sid, promptText)

	slot.mu.Lock()
	slot.inFlightPrompt = false
	slot.lastActivityAt = time.Now()
	slot.mu.Unlock()

	// Check if already finalized (e.g. orbit deliver --final ran inside the turn, or cancelled)
	att.mu.Lock()
	alreadyFinal := att.finalized
	att.promptReturned = true
	att.mu.Unlock()
	if alreadyFinal {
		return
	}

	if promptErr != nil {
		r.coalescer.FlushAttempt(dispatch.AttemptID)
		if errors.Is(ctx.Err(), context.Canceled) {
			r.finalizeAttempt(att, protocol.AttemptCancelled, map[string]any{
				"state":       string(protocol.AttemptCancelled),
				"errorCode":   "CANCELLED",
				"errorDetail": "attempt cancelled",
			})
			return
		}
		r.finalizeAttempt(att, protocol.AttemptFailed, map[string]any{
			"state":       string(protocol.AttemptFailed),
			"errorCode":   "ACP_PROMPT_FAILED",
			"errorDetail": promptErr.Error(),
		})
		return
	}

	if promptResp.StopReason == acp.StopReasonCancelled {
		r.coalescer.FlushAttempt(dispatch.AttemptID)
		r.finalizeAttempt(att, protocol.AttemptCancelled, map[string]any{
			"state":       string(protocol.AttemptCancelled),
			"errorCode":   "CANCELLED",
			"errorDetail": "acp stopReason=cancelled",
		})
		return
	}

	// 6. Spike Boundary #1: Check if native Swarm workers are still active after end_turn
	if att.tracker.HasActiveWorkers() {
		r.setAttemptState(att, protocol.AttemptSwarmMonitoring, map[string]any{
			"state":         string(protocol.AttemptSwarmMonitoring),
			"activeWorkers": att.tracker.ActiveWorkerNames(),
		})
		r.armSwarmIdleTimer(att, slot)
		// Stay alive keeping sessionOwner[sid] bound until explicit `orbit deliver --final`, cancel, or swarm_idle_timeout!
		<-ctx.Done()
		return
	}

	// Normal single-turn end_turn completion (record Boundary #5 counters honestly)
	// Wait for any in-flight approval_ack outbox writes so approval_ack strictly precedes state=succeeded.
	att.ackPendingWG.Wait()
	r.coalescer.FlushAttempt(dispatch.AttemptID)
	_, _, rejCount, nonZeroCount := att.tracker.Stats()
	payload := map[string]any{
		"state":             string(protocol.AttemptSucceeded),
		"completionSource":  string(protocol.CompletionPromptEndTurn),
		"rejectedToolCount": rejCount,
		"nonZeroExitCount":  nonZeroCount,
	}
	if promptResp.Usage != nil {
		payload["tokenUsage"] = map[string]any{
			"input":      promptResp.Usage.InputTokens,
			"output":     promptResp.Usage.OutputTokens,
			"cachedRead": promptResp.Usage.CachedReadTokens,
			"thought":    promptResp.Usage.ThoughtTokens,
		}
	}
	r.finalizeAttempt(att, protocol.AttemptSucceeded, payload)
}

// ensureSessionReadyLocked implements Spike Boundary #2 verbatim:
// - Hard check: reuse requires same machine, unlocked slot, and expectedSessionCwd == worktreePath.
// - On cold restore: ResumeSession (0 replay) -> LoadSession (swallow replay) -> NewSession.
// - IRON LAW: Always calls SetConfigOption per desired option with 10s timeout and verifies currentValue!
func (r *Runner) ensureSessionReadyLocked(ctx context.Context, att *attemptContext, slot *sessionSlot) (acp.SessionId, string, error) {
	d := att.dispatch
	att.mu.Lock()
	worktreePath := att.worktreePath
	att.mu.Unlock()

	expectedSID := acp.SessionId(d.ProviderSessionID)
	expectedCWD := d.ExpectedSessionCWD
	if expectedSID == "" && slot.providerSessionID != "" {
		expectedSID = slot.providerSessionID
		expectedCWD = slot.cwd
	}

	canReuse := !slot.locked &&
		expectedSID != "" &&
		expectedCWD != "" &&
		filepath.Clean(expectedCWD) == filepath.Clean(worktreePath)

	promptText := d.Prompt
	prependContract := false

	var sid acp.SessionId
	switch {
	case !canReuse:
		sysAppend := d.SystemPromptAppend
		if !r.driver.SupportsMetaSystemPrompt() && sysAppend != "" {
			prependContract = true
			sysAppend = ""
		}
		resp, err := r.driver.NewSession(ctx, worktreePath, sysAppend)
		if err != nil {
			return "", "", fmt.Errorf("new session: %w", err)
		}
		sid = resp.SessionId

	case !r.driver.IsSessionLiveInMemory(expectedSID):
		resumed := false
		if r.driver.SupportsResume() {
			if _, err := r.driver.ResumeSession(ctx, expectedSID, worktreePath); err == nil {
				sid = expectedSID
				resumed = true
			}
		}
		if !resumed && r.driver.SupportsLoad() {
			if _, err := r.driver.LoadSession(ctx, expectedSID, worktreePath); err == nil {
				sid = expectedSID
				resumed = true
			}
		}
		if !resumed {
			sysAppend := d.SystemPromptAppend
			if !r.driver.SupportsMetaSystemPrompt() && sysAppend != "" {
				prependContract = true
				sysAppend = ""
			}
			resp, err := r.driver.NewSession(ctx, worktreePath, sysAppend)
			if err != nil {
				return "", "", fmt.Errorf("fallback new session: %w", err)
			}
			sid = resp.SessionId
		}

	default:
		sid = expectedSID
	}

	// IRON LAW: Re-apply and strictly verify every desired config option before sending prompt
	if err := r.applyAndVerifyConfig(ctx, sid, d.DesiredConfig); err != nil {
		return "", "", fmt.Errorf("CONFIG_APPLY_FAILED: %w", err)
	}

	if prependContract && strings.TrimSpace(d.SystemPromptAppend) != "" {
		promptText = "<orbit_contract>\n" + d.SystemPromptAppend + "\n</orbit_contract>\n\n" + promptText
	}
	return sid, promptText, nil
}

func (r *Runner) applyAndVerifyConfig(ctx context.Context, sid acp.SessionId, desired map[string]string) error {
	if len(desired) == 0 {
		return nil
	}
	keys := make([]string, 0, len(desired))
	for k, v := range desired {
		if strings.TrimSpace(k) != "" && strings.TrimSpace(v) != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	for _, k := range keys {
		want := desired[k]
		optCtx, cancel := context.WithTimeout(ctx, r.cfg.ConfigApplyTimeout)
		resp, err := r.driver.SetConfigOption(optCtx, sid, k, want)
		cancel()
		if err != nil {
			return fmt.Errorf("configId %q=%q failed: %w", k, want, err)
		}
		if !verifyConfigOptionApplied(resp.ConfigOptions, k, want) {
			return fmt.Errorf("configId %q verification mismatch (expected %q)", k, want)
		}
	}
	return nil
}

func verifyConfigOptionApplied(opts []acp.SessionConfigOption, configID, want string) bool {
	for _, opt := range opts {
		if opt.Select != nil && string(opt.Select.Id) == configID {
			return string(opt.Select.CurrentValue) == want
		}
		if opt.Boolean != nil && string(opt.Boolean.Id) == configID {
			return fmt.Sprintf("%v", opt.Boolean.CurrentValue) == want
		}
	}
	return false
}

// CloseSessionSafely acquires the wbSessionId single-flight mutex, checks all 4 safety conditions,
// marks runtime_state='closing' inside the lock, and invokes CloseSession before releasing.
func (r *Runner) CloseSessionSafely(ctx context.Context, wbSessionID string) (bool, error) {
	r.mu.Lock()
	slot, ok := r.sessions[wbSessionID]
	r.mu.Unlock()
	if !ok {
		return false, nil
	}

	slot.mu.Lock()
	defer slot.mu.Unlock()

	if slot.providerSessionID == "" || slot.runtimeState == protocol.SessionIdleClosed || slot.runtimeState == protocol.SessionDead {
		return false, nil
	}
	if slot.inFlightPrompt {
		return false, nil
	}

	// Verify owning attempt (if any) is neither swarm_monitoring nor holding pending approvals
	r.mu.Lock()
	ownerAttemptID := r.sessionOwner[slot.providerSessionID]
	var ownerAtt *attemptContext
	if ownerAttemptID != "" {
		ownerAtt = r.attempts[ownerAttemptID]
	}
	r.mu.Unlock()

	if ownerAtt != nil {
		ownerAtt.mu.Lock()
		unsafe := !ownerAtt.finalized && (ownerAtt.state == protocol.AttemptSwarmMonitoring || len(ownerAtt.pendingApprovals) > 0)
		ownerAtt.mu.Unlock()
		if unsafe {
			return false, nil
		}
	}

	// Mark 'closing' INSIDE the per-session lock before calling CloseSession
	slot.runtimeState = protocol.SessionClosing
	err := r.driver.CloseSession(ctx, slot.providerSessionID)
	if err != nil {
		slot.runtimeState = protocol.SessionDead
		return false, err
	}
	slot.runtimeState = protocol.SessionIdleClosed
	return true, nil
}

func (r *Runner) armSwarmIdleTimer(att *attemptContext, slot *sessionSlot) {
	att.mu.Lock()
	defer att.mu.Unlock()
	if att.finalized || att.state != protocol.AttemptSwarmMonitoring {
		return
	}
	if att.swarmIdleTimer != nil {
		att.swarmIdleTimer.Stop()
	}
	timeout := att.swarmIdleTimeout
	att.swarmIdleTimer = time.AfterFunc(timeout, func() {
		r.onSwarmIdleTimeout(att, slot)
	})
}

func (r *Runner) resetSwarmIdleTimerIfMonitoring(att *attemptContext) {
	att.mu.Lock()
	if att.finalized || att.state != protocol.AttemptSwarmMonitoring || att.swarmIdleTimer == nil {
		att.mu.Unlock()
		return
	}
	att.swarmIdleTimer.Reset(att.swarmIdleTimeout)
	att.mu.Unlock()
}

func (r *Runner) onSwarmIdleTimeout(att *attemptContext, slot *sessionSlot) {
	att.mu.Lock()
	if att.finalized || att.state != protocol.AttemptSwarmMonitoring {
		att.mu.Unlock()
		return
	}
	att.mu.Unlock()

	// Lock the session so future attempts NEVER silently reuse a timed-out swarm session!
	slot.mu.Lock()
	slot.locked = true
	slot.runtimeState = protocol.SessionDead
	slot.mu.Unlock()

	r.coalescer.FlushAttempt(att.dispatch.AttemptID)
	r.finalizeAttempt(att, protocol.AttemptNeedsVerification, map[string]any{
		"state":            string(protocol.AttemptNeedsVerification),
		"completionSource": string(protocol.CompletionQuietTimeout),
		"sessionLocked":    true,
		"errorCode":        "SWARM_IDLE_TIMEOUT",
		"errorDetail":      "Swarm 静默超时未收到显式 orbit deliver --final，已保留全部尾帧并锁定旧会话，请人工核对时间线",
	})
}

// HandleCancel cancels an active attempt (including swarm_monitoring) via ACP session/cancel.
func (r *Runner) HandleCancel(frame protocol.AttemptCancelFrame) {
	r.mu.Lock()
	att, ok := r.attempts[frame.AttemptID]
	r.mu.Unlock()
	if !ok {
		return
	}

	att.mu.Lock()
	if att.finalized {
		att.mu.Unlock()
		return
	}
	if frame.LeaseToken != "" && att.dispatch.LeaseToken != frame.LeaseToken {
		att.mu.Unlock()
		return
	}
	sid := att.providerSessionID
	if att.swarmIdleTimer != nil {
		att.swarmIdleTimer.Stop()
	}
	// Cancel any waiting approval channels
	for id, h := range att.pendingApprovals {
		close(h.ch)
		delete(att.pendingApprovals, id)
	}
	att.mu.Unlock()

	if sid != "" {
		cctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = r.driver.Cancel(cctx, sid)
		cancel()
	}

	r.coalescer.FlushAttempt(att.dispatch.AttemptID)
	r.finalizeAttempt(att, protocol.AttemptCancelled, map[string]any{
		"state":       string(protocol.AttemptCancelled),
		"errorCode":   "CANCELLED",
		"errorDetail": "cancelled by hub",
	})
}

// HandleLocalDeliver handles `orbit deliver --summary "..." [--final]` arriving over ORBIT_RUNNER_SOCK.
func (r *Runner) HandleLocalDeliver(ctx context.Context, req LocalDeliverRequest) (LocalDeliverResponse, error) {
	var resp LocalDeliverResponse
	att, err := r.findAttemptForLocalDeliver(ctx, req)
	if err != nil {
		return resp, err
	}

	att.mu.Lock()
	dispatch := att.dispatch
	worktreePath := att.worktreePath
	baseSHA := att.baseSHA
	att.mu.Unlock()

	dlvID := "dlv_" + ulid.Make().String()
	summary := strings.TrimSpace(req.Summary)
	if summary == "" {
		summary = "Agent delivery"
	}

	var headSHA string
	var manifest protocol.DeliveryManifest

	if dispatch.ProjectID == protocol.ScratchProjectID || dispatch.MirrorBarePath == "" {
		// Scratch session without a git repo: record summary event directly
		headSHA = "scratch"
		manifest = protocol.DeliveryManifest{WorktreePath: worktreePath}
		_, _ = r.enqueueOutbox(ctx, dispatch.AttemptID, dispatch.LeaseToken, protocol.OutboxFrameEvents, map[string]any{
			"kind":       string(protocol.EventDeliveryPublished),
			"visibility": string(protocol.VisibilityTimeline),
			"actorKind":  string(protocol.ActorAgent),
			"actorId":    dispatch.Adapter,
			"payload": map[string]any{
				"deliveryId": dlvID,
				"summary":    summary,
				"final":      req.Final,
			},
		})
	} else {
		// Zero-pollution checkpoint packaging anchored to FIXED review baseSHA (att.baseSHA)
		pkg, err := gitops.PackageCheckpoint(ctx, gitops.PackageOptions{
			RepoDir:       worktreePath,
			DeliveryID:    dlvID,
			TaskID:        dispatch.TaskID,
			AttemptID:     dispatch.AttemptID,
			ReviewBaseSHA: baseSHA,
			Attachments:   req.Attachments,
		})
		if err != nil {
			return resp, fmt.Errorf("package checkpoint: %w", err)
		}
		headSHA = pkg.HeadSHA
		manifest = pkg.Manifest

		// Persist immutable pending delivery locally BEFORE push so network retries keep exact same SHA!
		pd := PendingDelivery{
			DeliveryID:     dlvID,
			AttemptID:      dispatch.AttemptID,
			TaskID:         dispatch.TaskID,
			ProjectID:      dispatch.ProjectID,
			RepoRoot:       pkg.RepoRoot,
			MirrorBarePath: dispatch.MirrorBarePath,
			ReviewBaseSHA:  pkg.ReviewBaseSHA,
			HeadSHA:        pkg.HeadSHA,
			Summary:        summary,
			Manifest:       pkg.Manifest,
			IsFinal:        req.Final,
		}
		if err := r.outbox.SavePendingDelivery(ctx, pd); err != nil {
			return resp, fmt.Errorf("save pending delivery: %w", err)
		}

		// Push delivery ref to bare mirror (protected by pre-receive hook)
		if err := gitops.PushDeliveryRef(ctx, pkg.RepoRoot, dispatch.MirrorBarePath, dlvID, pkg.HeadSHA); err != nil {
			return resp, fmt.Errorf("push delivery ref (saved locally for retry): %w", err)
		}
		_ = r.outbox.MarkDeliveryPushed(ctx, dlvID)

		// Flush any buffered chunks before publishing delivery
		r.coalescer.FlushAttempt(dispatch.AttemptID)

		if _, err := r.enqueueOutbox(ctx, dispatch.AttemptID, dispatch.LeaseToken, protocol.OutboxFrameDeliveryPublished, map[string]any{
			"deliveryId": dlvID,
			"baseSha":    pkg.ReviewBaseSHA,
			"headSha":    pkg.HeadSHA,
			"gitRef":     "refs/orbit/deliveries/" + dlvID,
			"summary":    summary,
			"manifest":   pkg.Manifest,
		}); err != nil {
			return resp, fmt.Errorf("enqueue delivery_published: %w", err)
		}
		_ = r.outbox.MarkDeliveryEmitted(ctx, dlvID)
	}

	activeWorkers := att.tracker.ActiveWorkerCount()
	shouldFinalize := req.Final || activeWorkers == 0

	if shouldFinalize {
		r.coalescer.FlushAttempt(dispatch.AttemptID)
		_, _, rejCount, nonZeroCount := att.tracker.Stats()
		r.finalizeAttempt(att, protocol.AttemptSucceeded, map[string]any{
			"state":             string(protocol.AttemptSucceeded),
			"completionSource":  string(protocol.CompletionExplicitDeliver),
			"deliveryId":        dlvID,
			"rejectedToolCount": rejCount,
			"nonZeroExitCount":  nonZeroCount,
		})
	}

	resp = LocalDeliverResponse{
		DeliveryID:   dlvID,
		AttemptID:    dispatch.AttemptID,
		TaskID:       dispatch.TaskID,
		ProjectID:    dispatch.ProjectID,
		BaseSHA:      baseSHA,
		HeadSHA:      headSHA,
		Finalized:    shouldFinalize,
		ActiveSwarm:  activeWorkers,
		FilesChanged: manifest.FilesChanged,
		Insertions:   manifest.Insertions,
		Deletions:    manifest.Deletions,
	}
	return resp, nil
}

func (r *Runner) findAttemptForLocalDeliver(ctx context.Context, req LocalDeliverRequest) (*attemptContext, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if req.AttemptID != "" {
		if a, ok := r.attempts[req.AttemptID]; ok {
			return a, nil
		}
		return nil, fmt.Errorf("active attempt %s not found on runner", req.AttemptID)
	}

	cwd := strings.TrimSpace(req.CWD)
	if cwd != "" {
		if top, err := gitops.ResolveRepoRoot(ctx, cwd); err == nil {
			cwd = top
		}
		if real, err := filepath.EvalSymlinks(cwd); err == nil {
			cwd = real
		}
		cwd = filepath.Clean(cwd)
		for _, a := range r.attempts {
			a.mu.Lock()
			wt := a.worktreePath
			finalized := a.finalized
			a.mu.Unlock()
			if finalized || wt == "" {
				continue
			}
			if realWT, err := filepath.EvalSymlinks(wt); err == nil {
				wt = realWT
			}
			wt = filepath.Clean(wt)
			if cwd == wt || strings.HasPrefix(cwd, wt+string(filepath.Separator)) {
				return a, nil
			}
		}
	}

	if req.TaskID != "" {
		for _, a := range r.attempts {
			a.mu.Lock()
			finalized := a.finalized
			a.mu.Unlock()
			if !finalized && a.dispatch.TaskID == req.TaskID {
				return a, nil
			}
		}
	}
	return nil, errors.New("could not resolve active attempt from attemptId or cwd")
}

// handleSessionUpdate is the persistent session-level ACP SessionUpdate callback (Boundary #1).
// Never unbinds when Prompt() returns so unsolicited Swarm tail frames keep attributing to the owning Attempt.
func (r *Runner) handleSessionUpdate(ctx context.Context, note acp.SessionNotification) error {
	r.mu.Lock()
	attemptID := r.sessionOwner[note.SessionId]
	att := r.attempts[attemptID]
	r.mu.Unlock()
	if att == nil {
		return nil
	}

	att.mu.Lock()
	if att.finalized {
		att.mu.Unlock()
		return nil
	}
	leaseToken := att.dispatch.LeaseToken
	unsolicited := att.promptReturned
	att.mu.Unlock()

	r.resetSwarmIdleTimerIfMonitoring(att)

	u := note.Update
	switch {
	case u.AgentMessageChunk != nil:
		if u.AgentMessageChunk.Content.Text != nil {
			r.coalescer.Append(attemptID, leaseToken, protocol.EventACPMessageBatch, u.AgentMessageChunk.Content.Text.Text, unsolicited)
		}

	case u.AgentThoughtChunk != nil:
		if u.AgentThoughtChunk.Content.Text != nil {
			r.coalescer.Append(attemptID, leaseToken, protocol.EventACPThoughtBatch, u.AgentThoughtChunk.Content.Text.Text, unsolicited)
		}

	case u.ToolCall != nil:
		proj := att.tracker.ObserveToolCall(u.ToolCall)
		name := ""
		if u.ToolCall.Name != nil {
			name = *u.ToolCall.Name
		}
		_, _ = r.enqueueOutbox(r.runCtx, attemptID, leaseToken, protocol.OutboxFrameEvents, map[string]any{
			"kind":       string(protocol.EventACPToolCall),
			"visibility": string(protocol.VisibilityRawACP),
			"actorKind":  string(protocol.ActorAgent),
			"actorId":    string(att.dispatch.Adapter),
			"payload": map[string]any{
				"toolCallId":  string(u.ToolCall.ToolCallId),
				"name":        name,
				"title":       u.ToolCall.Title,
				"status":      string(u.ToolCall.Status),
				"unsolicited": unsolicited,
			},
		})
		r.emitSwarmProjections(attemptID, leaseToken, proj)

	case u.ToolCallUpdate != nil:
		proj := att.tracker.ObserveToolCallUpdate(u.ToolCallUpdate)
		evPayload := map[string]any{
			"toolCallId":  string(u.ToolCallUpdate.ToolCallId),
			"unsolicited": unsolicited,
		}
		if v, ok := u.ToolCallUpdate.Name.Value(); ok {
			evPayload["name"] = v
		}
		if v, ok := u.ToolCallUpdate.Title.Value(); ok {
			evPayload["title"] = v
		}
		if v, ok := u.ToolCallUpdate.Status.Value(); ok {
			evPayload["status"] = string(v)
		}
		_, _ = r.enqueueOutbox(r.runCtx, attemptID, leaseToken, protocol.OutboxFrameEvents, map[string]any{
			"kind":       string(protocol.EventACPToolCall),
			"visibility": string(protocol.VisibilityRawACP),
			"actorKind":  string(protocol.ActorAgent),
			"actorId":    string(att.dispatch.Adapter),
			"payload":    evPayload,
		})
		r.emitSwarmProjections(attemptID, leaseToken, proj)

	case u.UsageUpdate != nil:
		kind := protocol.EventACPUsage
		if unsolicited {
			kind = protocol.EventACPUnsolicitedUpdate
		}
		_, _ = r.enqueueOutbox(r.runCtx, attemptID, leaseToken, protocol.OutboxFrameEvents, map[string]any{
			"kind":       string(kind),
			"visibility": string(protocol.VisibilityRawACP),
			"actorKind":  string(protocol.ActorAgent),
			"actorId":    string(att.dispatch.Adapter),
			"payload": map[string]any{
				"used":        u.UsageUpdate.Used,
				"size":        u.UsageUpdate.Size,
				"unsolicited": unsolicited,
			},
		})
	}
	return nil
}

func (r *Runner) emitSwarmProjections(attemptID, leaseToken string, proj SwarmProjection) {
	for _, m := range proj.Members {
		_, _ = r.enqueueOutbox(r.runCtx, attemptID, leaseToken, protocol.OutboxFrameBoardMember, m)
	}
	for _, msg := range proj.Messages {
		_, _ = r.enqueueOutbox(r.runCtx, attemptID, leaseToken, protocol.OutboxFrameBoardMessage, msg)
	}
}

// handlePermissionRequest implements Spike Boundary #4:
// - Safe worktree actions -> auto-allow + audit event.
// - Sensitive actions -> approval_request via Outbox, preserving resume_state across multi-concurrent approvals.
func (r *Runner) handlePermissionRequest(ctx context.Context, req acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	r.mu.Lock()
	attemptID := r.sessionOwner[req.SessionId]
	att := r.attempts[attemptID]
	r.mu.Unlock()
	if att == nil {
		return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
	}

	att.mu.Lock()
	worktreePath := att.worktreePath
	desiredMode := att.dispatch.DesiredConfig["mode"]
	leaseToken := att.dispatch.LeaseToken
	baseSHA := att.baseSHA
	att.mu.Unlock()

	if autoOpt, allow := r.cfg.PermissionPolicy(worktreePath, desiredMode, req); allow && autoOpt != "" {
		_, _ = r.enqueueOutbox(r.runCtx, attemptID, leaseToken, protocol.OutboxFrameEvents, map[string]any{
			"kind":       string(protocol.EventApprovalConsumed),
			"visibility": string(protocol.VisibilityAudit),
			"actorKind":  string(protocol.ActorRunner),
			"actorId":    r.cfg.MachineID,
			"payload": map[string]any{
				"policyAutoAllowed": true,
				"optionId":          autoOpt,
				"toolCallId":        string(req.ToolCall.ToolCallId),
			},
		})
		return acp.RequestPermissionResponse{
			Outcome: acp.NewRequestPermissionOutcomeSelected(acp.PermissionOptionId(autoOpt)),
		}, nil
	}

	// Build canonical options & payload digest for human approval
	options := make([]protocol.ApprovalOption, len(req.Options))
	for i, o := range req.Options {
		options[i] = protocol.ApprovalOption{
			ID:    string(o.OptionId),
			Label: o.Name,
			Kind:  string(o.Kind),
		}
	}
	actionName := "tool_permission"
	if name, ok := req.ToolCall.Name.Value(); ok && name != "" {
		actionName = name
	} else if title, ok := req.ToolCall.Title.Value(); ok && title != "" {
		actionName = title
	}
	toolPayload := map[string]any{
		"toolCallId": string(req.ToolCall.ToolCallId),
		"actionName": actionName,
	}
	if rawIn, ok := req.ToolCall.RawInput.Value(); ok && len(rawIn) > 0 {
		var parsed any
		if err := json.Unmarshal(rawIn, &parsed); err == nil {
			toolPayload["rawInput"] = parsed
		}
	}
	canonicalBytes, _ := json.Marshal(toolPayload)
	sum := sha256.Sum256(canonicalBytes)
	payloadSHA := hex.EncodeToString(sum[:])

	approvalID := "apr_" + ulid.Make().String()
	handle := &pendingApprovalHandle{
		approvalID:    approvalID,
		attemptID:     attemptID,
		payloadSHA256: payloadSHA,
		options:       options,
		ch:            make(chan protocol.ApprovalDecisionPayload, 1),
	}

	att.mu.Lock()
	if att.finalized {
		att.mu.Unlock()
		return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
	}
	if len(att.pendingApprovals) == 0 {
		if att.state == protocol.AttemptSwarmMonitoring {
			att.resumeState = protocol.ResumeStateSwarmMonitoring
		} else {
			att.resumeState = protocol.ResumeStateRunning
		}
		att.state = protocol.AttemptWaitingApproval
	}
	resumeState := att.resumeState
	att.pendingApprovals[approvalID] = handle
	att.mu.Unlock()

	r.coalescer.FlushAttempt(attemptID)
	_, _ = r.enqueueOutbox(r.runCtx, attemptID, leaseToken, protocol.OutboxFrameApprovalRequest, map[string]any{
		"approvalId":       approvalID,
		"kind":             string(protocol.ApprovalACPToolPermission),
		"actionName":       actionName,
		"targetEnv":        r.cfg.MachineID + ":worktree",
		"inputSha":         baseSHA,
		"payload":          toolPayload,
		"payloadSha256":    payloadSHA,
		"runnerInstanceId": r.instanceID,
		"options":          options,
		"requestedBy":      string(protocol.ApprovalRequestedByAgent),
		"resumeState":      string(resumeState),
	})

	// Wait for human decision via approval.decision or hub.welcome.reconcile.redeliverApprovals
	select {
	case dec, ok := <-handle.ch:
		if !ok || dec.OptionID == "" {
			return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
		}
		return acp.RequestPermissionResponse{
			Outcome: acp.NewRequestPermissionOutcomeSelected(acp.PermissionOptionId(dec.OptionID)),
		}, nil
	case <-ctx.Done():
		return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
	case <-r.runCtx.Done():
		return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
	}
}

// HandleApprovalDecision applies a Hub decision idempotently by decisionId, resolves the
// waiting ACP permission handle, records local handled_approvals, and emits approval_ack via Outbox.
func (r *Runner) HandleApprovalDecision(dec protocol.ApprovalDecisionPayload) {
	if dec.ApprovalID == "" || dec.DecisionID == "" {
		return
	}

	// 1. Idempotency check on decisionId
	if prev, found, err := r.outbox.LookupHandledApproval(r.runCtx, dec.DecisionID); err == nil && found {
		r.mu.Lock()
		att := r.attempts[dec.AttemptID]
		r.mu.Unlock()
		if att != nil {
			_, _ = r.enqueueOutbox(r.runCtx, att.dispatch.AttemptID, att.dispatch.LeaseToken, protocol.OutboxFrameApprovalAck, map[string]any{
				"approvalId": prev.ApprovalID,
				"decisionId": prev.DecisionID,
				"outcome":    string(prev.Outcome),
			})
		}
		return
	}

	// 2. Locate live pending handle
	r.mu.Lock()
	att := r.attempts[dec.AttemptID]
	r.mu.Unlock()
	if att == nil {
		return
	}

	att.mu.Lock()
	handle, ok := att.pendingApprovals[dec.ApprovalID]
	if !ok {
		att.mu.Unlock()
		return
	}
	if handle.payloadSHA256 != dec.PayloadSHA256 {
		att.mu.Unlock()
		return
	}
	var matchedOpt *protocol.ApprovalOption
	for i := range handle.options {
		if handle.options[i].ID == dec.OptionID {
			matchedOpt = &handle.options[i]
			break
		}
	}
	if matchedOpt == nil {
		att.mu.Unlock()
		return
	}

	outcome := protocol.ApprovalConsumedAllowed
	k := strings.ToLower(matchedOpt.Kind)
	idLower := strings.ToLower(matchedOpt.ID)
	if strings.HasPrefix(k, "reject") || strings.HasPrefix(k, "deny") || strings.HasPrefix(idLower, "reject") || strings.HasPrefix(idLower, "deny") {
		outcome = protocol.ApprovalConsumedRejected
		att.tracker.RecordToolRejection()
	}

	delete(att.pendingApprovals, dec.ApprovalID)
	remaining := len(att.pendingApprovals)
	restoreState := att.resumeState
	if remaining == 0 && att.state == protocol.AttemptWaitingApproval {
		if restoreState == protocol.ResumeStateSwarmMonitoring {
			att.state = protocol.AttemptSwarmMonitoring
		} else {
			att.state = protocol.AttemptRunning
		}
	}
	leaseToken := att.dispatch.LeaseToken
	att.ackPendingWG.Add(1)
	att.mu.Unlock()
	defer att.ackPendingWG.Done()

	// 3. Deliver decision to waiting ACP RequestPermission goroutine
	select {
	case handle.ch <- dec:
	default:
	}

	// Deterministic R3 crash-window test hook: crash right AFTER ACP write, BEFORE local record/ack
	if r.cfg.OnAfterACPApprovalWrite != nil && !r.cfg.OnAfterACPApprovalWrite(dec.ApprovalID) {
		return
	}

	// 4. Persist locally for decisionId idempotency & reconnect reconciliation
	_ = r.outbox.RecordHandledApproval(r.runCtx, dec.DecisionID, dec.ApprovalID, dec.AttemptID, outcome, dec.OptionID)

	// 5. Emit approval_ack via ordered Outbox
	_, _ = r.enqueueOutbox(r.runCtx, dec.AttemptID, leaseToken, protocol.OutboxFrameApprovalAck, map[string]any{
		"approvalId": dec.ApprovalID,
		"decisionId": dec.DecisionID,
		"outcome":    string(outcome),
	})
}

func defaultPermissionPolicy(worktreePath, desiredMode string, req acp.RequestPermissionRequest) (string, bool) {
	// Explicit mode=default always routes permission requests to human approval.
	if strings.EqualFold(desiredMode, "default") {
		return "", false
	}

	// Reject auto-allow if any referenced file location escapes the isolated worktree.
	if worktreePath != "" {
		if locs, ok := req.ToolCall.Locations.Value(); ok && len(locs) > 0 {
			absWT, _ := filepath.Abs(worktreePath)
			for _, loc := range locs {
				absLoc, err := filepath.Abs(loc.Path)
				if err != nil || (!strings.HasPrefix(absLoc, absWT+string(filepath.Separator)) && absLoc != absWT) {
					return "", false
				}
			}
		}
	}

	// Reject auto-allow for risky external/deploy/push shell commands.
	if rawIn, ok := req.ToolCall.RawInput.Value(); ok && len(rawIn) > 0 {
		lower := strings.ToLower(string(rawIn))
		for _, risky := range []string{"ssh ", "scp ", "git push", "rm -rf /", "sudo "} {
			if strings.Contains(lower, risky) {
				return "", false
			}
		}
	}

	kind, hasKind := req.ToolCall.Kind.Value()
	safeKind := !hasKind || kind == "" ||
		kind == acp.ToolKindRead || kind == acp.ToolKindEdit ||
		kind == acp.ToolKindSearch || kind == acp.ToolKindThink ||
		kind == acp.ToolKindExecute || kind == acp.ToolKindOther

	if safeKind {
		for _, opt := range req.Options {
			if opt.Kind == acp.PermissionOptionKindAllowOnce {
				return string(opt.OptionId), true
			}
		}
	}
	return "", false
}

func (r *Runner) tryAcquireResources(ctx context.Context, att *attemptContext, required []string) (bool, string) {
	sorted := append([]string(nil), required...)
	sort.Strings(sorted)

	// Check dynamic probe first if configured
	if r.cfg.ResourceProbe != nil {
		probed := r.cfg.ResourceProbe(ctx)
		for _, name := range sorted {
			if st, ok := probed[name]; ok && st.State != "free" {
				reason := st.Reason
				if reason == "" {
					reason = fmt.Sprintf("等待资源 %s 就绪 (%s)", name, st.State)
				}
				return false, reason
			}
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range sorted {
		if owner, busy := r.resourceLocks[name]; busy && owner != att.dispatch.AttemptID {
			return false, fmt.Sprintf("等待独占资源 %s（当前由 %s 占用）", name, owner)
		}
	}
	for _, name := range sorted {
		r.resourceLocks[name] = att.dispatch.AttemptID
	}
	att.mu.Lock()
	att.heldResources = sorted
	att.mu.Unlock()
	return true, ""
}

func (r *Runner) releaseAttemptResources(att *attemptContext) {
	att.mu.Lock()
	held := att.heldResources
	att.heldResources = nil
	att.mu.Unlock()
	if len(held) == 0 {
		return
	}
	r.mu.Lock()
	for _, name := range held {
		if r.resourceLocks[name] == att.dispatch.AttemptID {
			delete(r.resourceLocks, name)
		}
	}
	r.mu.Unlock()
}

func (r *Runner) setAttemptState(att *attemptContext, st protocol.AttemptState, payload map[string]any) {
	att.mu.Lock()
	if att.finalized {
		att.mu.Unlock()
		return
	}
	att.state = st
	attemptID := att.dispatch.AttemptID
	leaseToken := att.dispatch.LeaseToken
	att.mu.Unlock()

	_, _ = r.enqueueOutbox(r.runCtx, attemptID, leaseToken, protocol.OutboxFrameState, payload)
}

func (r *Runner) finalizeAttempt(att *attemptContext, terminalState protocol.AttemptState, payload map[string]any) {
	att.mu.Lock()
	if att.finalized {
		att.mu.Unlock()
		return
	}
	att.finalized = true
	att.state = terminalState
	if att.swarmIdleTimer != nil {
		att.swarmIdleTimer.Stop()
	}
	sid := att.providerSessionID
	attemptID := att.dispatch.AttemptID
	leaseToken := att.dispatch.LeaseToken
	cancelFn := att.cancelFn
	att.mu.Unlock()

	r.releaseAttemptResources(att)

	r.mu.Lock()
	if sid != "" && r.sessionOwner[sid] == attemptID {
		delete(r.sessionOwner, sid)
	}
	r.mu.Unlock()

	_, _ = r.enqueueOutbox(context.Background(), attemptID, leaseToken, protocol.OutboxFrameState, payload)
	if cancelFn != nil {
		cancelFn()
	}
}

func (r *Runner) flushCoalescedChunk(attemptID, leaseToken string, kind protocol.EventKind, text string, unsolicited bool) {
	_, _ = r.enqueueOutbox(context.Background(), attemptID, leaseToken, protocol.OutboxFrameEvents, map[string]any{
		"kind":       string(kind),
		"visibility": string(protocol.VisibilityRawACP),
		"actorKind":  string(protocol.ActorAgent),
		"actorId":    "acp",
		"payload": map[string]any{
			"text":        text,
			"unsolicited": unsolicited,
		},
	})
}

func (r *Runner) enqueueOutbox(
	ctx context.Context,
	attemptID, leaseToken string,
	frameType protocol.OutboxFrameType,
	payload map[string]any,
) (protocol.OutboxItem, error) {
	if ctx == nil || ctx.Err() != nil {
		ctx = context.Background()
	}
	evID := "evt_" + ulid.Make().String()
	item, err := r.outbox.Enqueue(ctx, attemptID, leaseToken, evID, frameType, payload)
	if err != nil {
		return item, err
	}
	r.wakeOutboxPump()
	return item, nil
}

func (r *Runner) wakeOutboxPump() {
	select {
	case r.outboxWakeCh <- struct{}{}:
	default:
	}
}

func (r *Runner) idleSweepLoop() {
	defer r.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	threshold := time.Duration(r.cfg.IdleCloseSeconds) * time.Second
	for {
		select {
		case <-r.runCtx.Done():
			return
		case <-ticker.C:
			r.mu.Lock()
			ids := make([]string, 0, len(r.sessions))
			for id := range r.sessions {
				ids = append(ids, id)
			}
			r.mu.Unlock()
			now := time.Now()
			for _, id := range ids {
				r.mu.Lock()
				slot := r.sessions[id]
				r.mu.Unlock()
				if slot == nil {
					continue
				}
				slot.mu.Lock()
				idle := !slot.inFlightPrompt && slot.runtimeState == protocol.SessionLive && now.Sub(slot.lastActivityAt) >= threshold
				slot.mu.Unlock()
				if idle {
					_, _ = r.CloseSessionSafely(r.runCtx, id)
				}
			}
		}
	}
}

func (r *Runner) wsReconnectLoop() {
	defer r.wg.Done()
	delay := r.cfg.ReconnectInitialDelay
	for {
		if r.runCtx.Err() != nil {
			return
		}
		err := r.connectAndServeWS(r.runCtx)
		if r.runCtx.Err() != nil {
			return
		}
		if err == nil {
			delay = r.cfg.ReconnectInitialDelay
		}
		select {
		case <-r.runCtx.Done():
			return
		case <-time.After(delay):
		}
		delay *= 2
		if delay > r.cfg.ReconnectMaxDelay {
			delay = r.cfg.ReconnectMaxDelay
		}
	}
}

func (r *Runner) connectAndServeWS(ctx context.Context) error {
	hdr := http.Header{}
	hdr.Set("X-Orbit-Machine-Id", r.cfg.MachineID)
	if strings.TrimSpace(r.cfg.BearerToken) != "" {
		hdr.Set("Authorization", "Bearer "+strings.TrimSpace(r.cfg.BearerToken))
	}

	dialCtx, cancelDial := context.WithTimeout(ctx, 10*time.Second)
	conn, _, err := websocket.Dial(dialCtx, r.cfg.HubWSURL, &websocket.DialOptions{HTTPHeader: hdr})
	cancelDial()
	if err != nil {
		return err
	}
	conn.SetReadLimit(16 << 20)
	defer conn.CloseNow()

	// Build and send runner.hello
	hello := r.buildRunnerHello(ctx)
	helloBytes, err := json.Marshal(hello)
	if err != nil {
		return err
	}
	wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
	err = conn.Write(wctx, websocket.MessageText, helloBytes)
	wcancel()
	if err != nil {
		return err
	}

	// Read hub.welcome
	rctx, rcancel := context.WithTimeout(ctx, 10*time.Second)
	_, rawWelcome, err := conn.Read(rctx)
	rcancel()
	if err != nil {
		return err
	}
	var welcome protocol.HubWelcomeFrame
	if err := json.Unmarshal(rawWelcome, &welcome); err != nil || welcome.Type != protocol.RunnerMsgHubWelcome {
		return fmt.Errorf("expected hub.welcome, got %s", string(rawWelcome))
	}

	// Register active WS connection + send queue
	sendCh := make(chan []byte, 256)
	doneCh := make(chan struct{})
	r.connMu.Lock()
	r.wsConn = conn
	r.wsSendCh = sendCh
	r.wsDone = doneCh
	if welcome.HeartbeatIntervalMs > 0 {
		r.hbPeriod = time.Duration(welcome.HeartbeatIntervalMs) * time.Millisecond
	}
	r.connMu.Unlock()

	defer func() {
		r.connMu.Lock()
		if r.wsConn == conn {
			r.wsConn = nil
			r.wsSendCh = nil
			r.wsDone = nil
		}
		r.connMu.Unlock()
		close(doneCh)
	}()

	// Apply hub.welcome reconciliation
	for aid, ackedSeq := range welcome.Reconcile.AckedOutboxByAttempt {
		_ = r.outbox.AckUpTo(ctx, aid, ackedSeq)
	}
	for _, abortID := range welcome.Reconcile.AbortAttemptIDs {
		r.HandleCancel(protocol.AttemptCancelFrame{AttemptID: abortID})
	}
	for _, redeliver := range welcome.Reconcile.RedeliverApprovals {
		r.HandleApprovalDecision(redeliver)
	}

	// Retry any offline pending deliveries and wake outbox flush immediately
	r.retryPendingDeliveries(ctx)
	r.wakeOutboxPump()

	connCtx, cancelConn := context.WithCancel(ctx)
	defer cancelConn()

	var ioWG sync.WaitGroup
	// Write pump
	ioWG.Add(1)
	go func() {
		defer ioWG.Done()
		defer cancelConn()
		for {
			select {
			case <-connCtx.Done():
				return
			case msg := <-sendCh:
				wctx, cancel := context.WithTimeout(connCtx, 10*time.Second)
				err := conn.Write(wctx, websocket.MessageText, msg)
				cancel()
				if err != nil {
					return
				}
			}
		}
	}()

	// Heartbeat loop
	ioWG.Add(1)
	go func() {
		defer ioWG.Done()
		ticker := time.NewTicker(r.hbPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-connCtx.Done():
				return
			case <-ticker.C:
				hb := r.buildHeartbeat(connCtx)
				_ = r.sendWSJSON(hb)
			}
		}
	}()

	// Read loop
	for {
		_, data, err := conn.Read(connCtx)
		if err != nil {
			break
		}
		var hdr protocol.RunnerFrameHeader
		if err := json.Unmarshal(data, &hdr); err != nil {
			continue
		}
		switch hdr.Type {
		case protocol.RunnerMsgAttemptDispatch:
			var f protocol.AttemptDispatchFrame
			if json.Unmarshal(data, &f) == nil {
				r.HandleDispatch(f)
			}
		case protocol.RunnerMsgAttemptCancel:
			var f protocol.AttemptCancelFrame
			if json.Unmarshal(data, &f) == nil {
				r.HandleCancel(f)
			}
		case protocol.RunnerMsgApprovalDecide:
			var f protocol.ApprovalDecisionFrame
			if json.Unmarshal(data, &f) == nil {
				r.HandleApprovalDecision(protocol.ApprovalDecisionPayload{
					ApprovalID:    f.ApprovalID,
					DecisionID:    f.DecisionID,
					AttemptID:     f.AttemptID,
					PayloadSHA256: f.PayloadSHA256,
					OptionID:      f.OptionID,
					DecidedBy:     f.DecidedBy,
				})
			}
		case protocol.RunnerMsgSessionClose:
			var f protocol.SessionCloseFrame
			if json.Unmarshal(data, &f) == nil {
				go func(wbID string) {
					_, _ = r.CloseSessionSafely(r.runCtx, wbID)
				}(f.WBSessionID)
			}
		case protocol.RunnerMsgOutboxAck:
			var f protocol.HubOutboxAckFrame
			if json.Unmarshal(data, &f) == nil {
				_ = r.outbox.AckUpTo(ctx, f.AttemptID, f.AckedOutboxSeq)
				r.wakeOutboxPump()
			}
		}
	}

	cancelConn()
	_ = conn.Close(websocket.StatusNormalClosure, "reconnect")
	ioWG.Wait()
	return nil
}

func (r *Runner) closeActiveWS(code websocket.StatusCode, reason string) {
	r.connMu.Lock()
	conn := r.wsConn
	r.connMu.Unlock()
	if conn != nil {
		_ = conn.Close(code, reason)
	}
}

func (r *Runner) sendWSJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	r.connMu.Lock()
	sendCh := r.wsSendCh
	doneCh := r.wsDone
	r.connMu.Unlock()
	if sendCh == nil || doneCh == nil {
		return errors.New("runner ws not connected")
	}
	select {
	case <-doneCh:
		return errors.New("runner ws closed")
	case sendCh <- data:
		return nil
	case <-time.After(3 * time.Second):
		return errors.New("runner ws send timeout")
	}
}

func (r *Runner) buildRunnerHello(ctx context.Context) protocol.RunnerHelloFrame {
	r.mu.Lock()
	liveAttempts := make([]protocol.LiveAttemptInfo, 0, len(r.attempts))
	liveApprovals := []string{}
	for _, a := range r.attempts {
		a.mu.Lock()
		if !a.finalized {
			liveAttempts = append(liveAttempts, protocol.LiveAttemptInfo{
				AttemptID:  a.dispatch.AttemptID,
				LeaseToken: a.dispatch.LeaseToken,
				State:      a.state,
			})
			for aprID := range a.pendingApprovals {
				liveApprovals = append(liveApprovals, aprID)
			}
		}
		a.mu.Unlock()
	}
	r.mu.Unlock()

	handled, _ := r.outbox.ListRecentHandledApprovals(ctx, 50)
	if handled == nil {
		handled = []protocol.HandledApprovalDecision{}
	}
	resources := map[string]protocol.ResourceState{}
	if r.cfg.ResourceProbe != nil {
		resources = r.cfg.ResourceProbe(ctx)
	}

	return protocol.RunnerHelloFrame{
		Type:                     protocol.RunnerMsgHello,
		ProtocolVersion:          1,
		MachineID:                r.cfg.MachineID,
		RunnerInstanceID:         r.instanceID,
		RunnerVersion:            r.cfg.RunnerVersion,
		LiveAttempts:             liveAttempts,
		LivePendingApprovalIDs:   liveApprovals,
		HandledApprovalDecisions: handled,
		Resources:                resources,
	}
}

func (r *Runner) buildHeartbeat(ctx context.Context) protocol.RunnerHeartbeatFrame {
	r.mu.Lock()
	leases := make([]protocol.LiveAttemptLease, 0, len(r.attempts))
	for _, a := range r.attempts {
		a.mu.Lock()
		if !a.finalized {
			leases = append(leases, protocol.LiveAttemptLease{
				AttemptID:  a.dispatch.AttemptID,
				LeaseToken: a.dispatch.LeaseToken,
			})
		}
		a.mu.Unlock()
	}
	r.mu.Unlock()

	resources := map[string]protocol.ResourceState{}
	if r.cfg.ResourceProbe != nil {
		resources = r.cfg.ResourceProbe(ctx)
	}
	return protocol.RunnerHeartbeatFrame{
		Type:                   protocol.RunnerMsgHeartbeat,
		MachineID:              r.cfg.MachineID,
		RunnerInstanceID:       r.instanceID,
		LiveAttemptLeaseTokens: leases,
		Resources:              resources,
	}
}

func (r *Runner) retryPendingDeliveries(ctx context.Context) {
	list, err := r.outbox.ListUnemittedDeliveries(ctx)
	if err != nil {
		return
	}
	for _, pd := range list {
		if !pd.Pushed {
			if err := gitops.PushDeliveryRef(ctx, pd.RepoRoot, pd.MirrorBarePath, pd.DeliveryID, pd.HeadSHA); err != nil {
				continue
			}
			_ = r.outbox.MarkDeliveryPushed(ctx, pd.DeliveryID)
		}
		r.mu.Lock()
		att := r.attempts[pd.AttemptID]
		r.mu.Unlock()
		leaseToken := ""
		if att != nil {
			leaseToken = att.dispatch.LeaseToken
		} else {
			leaseToken, _, _ = r.outbox.ReadUnacked(ctx, pd.AttemptID, 0, 1)
		}
		if leaseToken != "" {
			if _, err := r.enqueueOutbox(ctx, pd.AttemptID, leaseToken, protocol.OutboxFrameDeliveryPublished, map[string]any{
				"deliveryId": pd.DeliveryID,
				"baseSha":    pd.ReviewBaseSHA,
				"headSha":    pd.HeadSHA,
				"gitRef":     "refs/orbit/deliveries/" + pd.DeliveryID,
				"summary":    pd.Summary,
				"manifest":   pd.Manifest,
			}); err == nil {
				_ = r.outbox.MarkDeliveryEmitted(ctx, pd.DeliveryID)
			}
		}
	}
}

func (r *Runner) outboxPumpLoop() {
	defer r.wg.Done()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	flush := func() {
		r.connMu.Lock()
		connected := r.wsConn != nil
		r.connMu.Unlock()
		if !connected {
			return
		}
		summaries, err := r.outbox.ListAttemptsWithUnacked(r.runCtx)
		if err != nil {
			return
		}
		for _, s := range summaries {
			leaseToken, items, err := r.outbox.ReadUnacked(r.runCtx, s.AttemptID, s.AckedSeq, 50)
			if err != nil || len(items) == 0 {
				continue
			}
			_ = r.sendWSJSON(protocol.AttemptOutboxBatchFrame{
				Type:       protocol.RunnerMsgOutboxBatch,
				AttemptID:  s.AttemptID,
				LeaseToken: leaseToken,
				Items:      items,
			})
		}
	}

	for {
		select {
		case <-r.runCtx.Done():
			return
		case <-r.outboxWakeCh:
			flush()
		case <-ticker.C:
			flush()
		}
	}
}
