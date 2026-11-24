package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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
	SupportedAdapters       []protocol.Adapter
	MaxLiveWorkers          int
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
	mu                   sync.Mutex
	wbSessionID          string
	providerSessionID    acp.SessionId
	cwd                  string
	runtimeState         protocol.SessionRuntimeState
	inFlightPrompt       bool
	promptOwnerAttemptID string
	locked               bool
	lastActivityAt       time.Time
}

type pendingApprovalHandle struct {
	approvalID    string
	attemptID     string
	payloadSHA256 string
	options       []protocol.ApprovalOption
	ch            chan protocol.ApprovalDecisionPayload
	writeMeta     PermissionWriteMeta
	hasWriteMeta  bool
}

type attemptContext struct {
	mu                      sync.Mutex
	dispatch                protocol.AttemptDispatchFrame
	slot                    *sessionSlot
	worktreePath            string
	baseSHA                 string
	state                   protocol.AttemptState
	resumeState             protocol.ResumeState
	providerSessionID       acp.SessionId
	promptReturned          bool
	promptDoneCh            chan struct{}
	promptDoneOnce          sync.Once
	hasDelivery             bool
	finalRequested          bool
	explicitFinalDeliveryID string
	finalizing              bool
	finalized               bool
	heldResources           []string
	tracker                 *ActiveSwarmTracker
	pendingApprovals        map[string]*pendingApprovalHandle
	ackPendingWG            sync.WaitGroup
	swarmIdleTimeout        time.Duration
	swarmIdleTimer          *time.Timer
	cancelFn                context.CancelFunc
	doneCh                  chan struct{}
}

// Runner is the pure-Go thin ACP runner bridging Hub (/ws/runner) and AcpAdapterDriver.
type Runner struct {
	cfg        Config
	instanceID string
	outbox     *LocalOutbox
	driver     AcpAdapterDriver
	coalescer  *ChunkCoalescer
	sockSrv    *localSocketServer
	httpClient *http.Client
	deliverMu  sync.Mutex

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
	if len(cfg.SupportedAdapters) == 0 {
		cfg.SupportedAdapters = append([]protocol.Adapter(nil), protocol.Phase1ImplementedSessionAdapters...)
	}
	if strings.TrimSpace(cfg.HubHTTPURL) == "" && strings.TrimSpace(cfg.HubWSURL) != "" {
		cfg.HubHTTPURL = deriveHubHTTPURL(cfg.HubWSURL)
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
		httpClient:    &http.Client{Timeout: 30 * time.Second},
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

func deriveHubHTTPURL(wsRaw string) string {
	u, err := url.Parse(strings.TrimSpace(wsRaw))
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := "http"
	if strings.EqualFold(u.Scheme, "wss") {
		scheme = "https"
	}
	return scheme + "://" + u.Host
}

func (r *Runner) supportedAdapters() []protocol.Adapter {
	return append([]protocol.Adapter(nil), r.cfg.SupportedAdapters...)
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
		promptDoneCh:     make(chan struct{}),
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

	// B18: Reject unsupported session adapters explicitly instead of silently running agy.
	if protocol.SessionAdapter(dispatch.Adapter).IsValid() && !slices.Contains(r.cfg.SupportedAdapters, dispatch.Adapter) {
		_ = r.finalizeAttempt(att, protocol.AttemptFailed, map[string]any{
			"state":       string(protocol.AttemptFailed),
			"errorCode":   "UNSUPPORTED_ADAPTER",
			"errorDetail": fmt.Sprintf("runner %s does not support adapter %q (supported: %v)", r.cfg.MachineID, dispatch.Adapter, r.cfg.SupportedAdapters),
		})
		return
	}

	// 1. Transition to preparing
	r.setAttemptState(att, protocol.AttemptPreparing, map[string]any{
		"state": string(protocol.AttemptPreparing),
	})

	// 2. Acquire exclusive machine resources atomically in lexicographical order
	if len(dispatch.RequiredResources) > 0 {
		for {
			if err := ctx.Err(); err != nil {
				_ = r.finalizeAttempt(att, protocol.AttemptCancelled, map[string]any{
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
				_ = r.finalizeAttempt(att, protocol.AttemptCancelled, map[string]any{
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
		_ = r.finalizeAttempt(att, protocol.AttemptFailed, map[string]any{
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
	// S1: Enforce MaxLiveWorkers by evicting the least-recently-used idle session before starting a new worker.
	if r.cfg.MaxLiveWorkers > 0 {
		r.evictIdleSessionsIfOverLimit(ctx, wbSessionID)
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
		_ = r.finalizeAttempt(att, protocol.AttemptFailed, map[string]any{
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
	slot.promptOwnerAttemptID = dispatch.AttemptID
	slot.lastActivityAt = time.Now()

	att.mu.Lock()
	att.slot = slot
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

	// B2: Only clear slot.inFlightPrompt if THIS attempt still owns the in-flight prompt!
	slot.mu.Lock()
	if slot.promptOwnerAttemptID == dispatch.AttemptID {
		slot.inFlightPrompt = false
		slot.promptOwnerAttemptID = ""
	}
	slot.lastActivityAt = time.Now()
	slot.mu.Unlock()

	att.mu.Lock()
	alreadyFinal := att.finalized
	att.promptReturned = true
	finalReq := att.finalRequested
	explicitDlvID := att.explicitFinalDeliveryID
	hasDelivery := att.hasDelivery
	att.mu.Unlock()
	att.promptDoneOnce.Do(func() { close(att.promptDoneCh) })

	if alreadyFinal {
		return
	}

	// B2: If `orbit deliver --final` ran during the in-flight Prompt(), deferring finalization until
	// Prompt() returns guarantees exclusive sessionOwner ownership and prevents overlapping turns!
	if finalReq {
		att.ackPendingWG.Wait()
		r.coalescer.FlushAttempt(dispatch.AttemptID)
		activeWorkers := att.tracker.ActiveWorkerCount()
		sessionLocked := false
		if activeWorkers > 0 {
			r.isolateAndCloseSession(slot, sid)
			sessionLocked = true
		}
		_, _, rejCount, nonZeroCount := att.tracker.Stats()
		unresRej, unresNonZero := att.tracker.UnresolvedCounts()
		payload := map[string]any{
			"state":                       string(protocol.AttemptSucceeded),
			"completionSource":            string(protocol.CompletionExplicitDeliver),
			"deliveryId":                  explicitDlvID,
			"sessionLocked":               sessionLocked,
			"rejectedToolCount":           rejCount,
			"nonZeroExitCount":            nonZeroCount,
			"unresolvedRejectedToolCount": unresRej,
			"unresolvedNonZeroExitCount":  unresNonZero,
		}
		if promptResp.Usage != nil {
			payload["tokenUsage"] = map[string]any{
				"input":      promptResp.Usage.InputTokens,
				"output":     promptResp.Usage.OutputTokens,
				"cachedRead": promptResp.Usage.CachedReadTokens,
				"thought":    promptResp.Usage.ThoughtTokens,
			}
		}
		_ = r.finalizeAttempt(att, protocol.AttemptSucceeded, payload)
		return
	}

	if promptErr != nil {
		r.coalescer.FlushAttempt(dispatch.AttemptID)
		if errors.Is(ctx.Err(), context.Canceled) {
			sessionLocked := false
			if att.tracker.HasActiveWorkers() {
				r.isolateAndCloseSession(slot, sid)
				sessionLocked = true
			}
			_ = r.finalizeAttempt(att, protocol.AttemptCancelled, map[string]any{
				"state":         string(protocol.AttemptCancelled),
				"sessionLocked": sessionLocked,
				"errorCode":     "CANCELLED",
				"errorDetail":   "attempt cancelled",
			})
			return
		}
		_ = r.finalizeAttempt(att, protocol.AttemptFailed, map[string]any{
			"state":       string(protocol.AttemptFailed),
			"errorCode":   "ACP_PROMPT_FAILED",
			"errorDetail": promptErr.Error(),
		})
		return
	}

	// B1: Explicitly adjudicate ACP StopReason — refusal / max_tokens / max_turn_requests CANNOT succeed!
	switch promptResp.StopReason {
	case acp.StopReasonCancelled:
		r.coalescer.FlushAttempt(dispatch.AttemptID)
		sessionLocked := false
		if att.tracker.HasActiveWorkers() {
			r.isolateAndCloseSession(slot, sid)
			sessionLocked = true
		}
		_ = r.finalizeAttempt(att, protocol.AttemptCancelled, map[string]any{
			"state":         string(protocol.AttemptCancelled),
			"stopReason":    string(promptResp.StopReason),
			"sessionLocked": sessionLocked,
			"errorCode":     "CANCELLED",
			"errorDetail":   "acp stopReason=cancelled",
		})
		return

	case acp.StopReasonRefusal, acp.StopReasonMaxTokens, acp.StopReasonMaxTurnRequests:
		r.coalescer.FlushAttempt(dispatch.AttemptID)
		_ = r.finalizeAttempt(att, protocol.AttemptFailed, map[string]any{
			"state":       string(protocol.AttemptFailed),
			"stopReason":  string(promptResp.StopReason),
			"errorCode":   "ABNORMAL_STOP_REASON",
			"errorDetail": fmt.Sprintf("agent stopped with %s", promptResp.StopReason),
		})
		return

	case acp.StopReasonEndTurn, "":
		// Proceed with normal completion / swarm monitoring checks below.

	default:
		r.coalescer.FlushAttempt(dispatch.AttemptID)
		_ = r.finalizeAttempt(att, protocol.AttemptFailed, map[string]any{
			"state":       string(protocol.AttemptFailed),
			"stopReason":  string(promptResp.StopReason),
			"errorCode":   "UNKNOWN_STOP_REASON",
			"errorDetail": fmt.Sprintf("unexpected stopReason %q", promptResp.StopReason),
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
	// Wait for any in-flight approval_ack outbox writes so approval_ack strictly precedes terminal state.
	att.ackPendingWG.Wait()
	r.coalescer.FlushAttempt(dispatch.AttemptID)
	_, _, rejCount, nonZeroCount := att.tracker.Stats()
	unresRej, unresNonZero := att.tracker.UnresolvedCounts()

	// B1: Non-_scratch project turns MUST have a verified delivery before succeeding;
	// pure _scratch freeform chat may succeed directly on clean end_turn.
	if dispatch.ProjectID != protocol.ScratchProjectID && !hasDelivery {
		_ = r.finalizeAttempt(att, protocol.AttemptFailed, map[string]any{
			"state":                       string(protocol.AttemptFailed),
			"stopReason":                  string(acp.StopReasonEndTurn),
			"errorCode":                   "NO_VERIFIED_DELIVERY",
			"errorDetail":                 "项目交付轮未运行 `orbit deliver` 产出已核验交付，不能标记为成功",
			"rejectedToolCount":           rejCount,
			"nonZeroExitCount":            nonZeroCount,
			"unresolvedRejectedToolCount": unresRej,
			"unresolvedNonZeroExitCount":  unresNonZero,
		})
		return
	}
	if unresRej > 0 || unresNonZero > 0 {
		r.isolateAndCloseSession(slot, sid)
		_ = r.finalizeAttempt(att, protocol.AttemptNeedsVerification, map[string]any{
			"state":                       string(protocol.AttemptNeedsVerification),
			"stopReason":                  string(acp.StopReasonEndTurn),
			"completionSource":            string(protocol.CompletionPromptEndTurn),
			"sessionLocked":               true,
			"errorCode":                   "UNRESOLVED_TOOL_FAILURE",
			"errorDetail":                 "本轮存在未解决的工具拒绝或非零失败，请人工核对后再决定是否续跑",
			"rejectedToolCount":           rejCount,
			"nonZeroExitCount":            nonZeroCount,
			"unresolvedRejectedToolCount": unresRej,
			"unresolvedNonZeroExitCount":  unresNonZero,
		})
		return
	}

	payload := map[string]any{
		"state":                       string(protocol.AttemptSucceeded),
		"stopReason":                  string(acp.StopReasonEndTurn),
		"completionSource":            string(protocol.CompletionPromptEndTurn),
		"rejectedToolCount":           rejCount,
		"nonZeroExitCount":            nonZeroCount,
		"unresolvedRejectedToolCount": unresRej,
		"unresolvedNonZeroExitCount":  unresNonZero,
	}
	if promptResp.Usage != nil {
		payload["tokenUsage"] = map[string]any{
			"input":      promptResp.Usage.InputTokens,
			"output":     promptResp.Usage.OutputTokens,
			"cachedRead": promptResp.Usage.CachedReadTokens,
			"thought":    promptResp.Usage.ThoughtTokens,
		}
	}
	_ = r.finalizeAttempt(att, protocol.AttemptSucceeded, payload)
}

func (r *Runner) isolateAndCloseSession(slot *sessionSlot, sid acp.SessionId) {
	if slot != nil {
		slot.mu.Lock()
		slot.locked = true
		slot.runtimeState = protocol.SessionDead
		if sid == "" {
			sid = slot.providerSessionID
		}
		slot.mu.Unlock()
	}
	if sid != "" {
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = r.driver.CloseSession(cctx, sid)
		cancel()
	}
}

func (r *Runner) evictIdleSessionsIfOverLimit(ctx context.Context, excludeWBSessionID string) {
	type cand struct {
		wbID       string
		lastActive time.Time
	}
	r.mu.Lock()
	slots := make([]*sessionSlot, 0, len(r.sessions))
	for _, s := range r.sessions {
		slots = append(slots, s)
	}
	r.mu.Unlock()

	liveCount := 0
	var idleCands []cand
	for _, s := range slots {
		s.mu.Lock()
		isLive := s.runtimeState == protocol.SessionLive && s.providerSessionID != ""
		inFlight := s.inFlightPrompt
		wbID := s.wbSessionID
		lastAct := s.lastActivityAt
		s.mu.Unlock()
		if !isLive {
			continue
		}
		liveCount++
		if wbID != excludeWBSessionID && !inFlight {
			idleCands = append(idleCands, cand{wbID: wbID, lastActive: lastAct})
		}
	}
	if liveCount < r.cfg.MaxLiveWorkers {
		return
	}
	sort.Slice(idleCands, func(i, j int) bool {
		return idleCands[i].lastActive.Before(idleCands[j].lastActive)
	})
	for _, c := range idleCands {
		if liveCount < r.cfg.MaxLiveWorkers {
			break
		}
		if closed, _ := r.CloseSessionSafely(ctx, c.wbID); closed {
			liveCount--
		}
	}
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
		// S1: Close the leaked ACP session on config failure so workers don't accumulate.
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = r.driver.CloseSession(cctx, sid)
		cancel()
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
	sid := att.providerSessionID
	att.mu.Unlock()

	// Lock & close the session so future attempts NEVER silently reuse a timed-out swarm session and no worker leaks (S1)!
	r.isolateAndCloseSession(slot, sid)

	r.coalescer.FlushAttempt(att.dispatch.AttemptID)
	_ = r.finalizeAttempt(att, protocol.AttemptNeedsVerification, map[string]any{
		"state":            string(protocol.AttemptNeedsVerification),
		"completionSource": string(protocol.CompletionQuietTimeout),
		"sessionLocked":    true,
		"errorCode":        "SWARM_IDLE_TIMEOUT",
		"errorDetail":      "Swarm 静默超时未收到显式 orbit deliver --final，已保留全部尾帧并锁定旧会话，请人工核对时间线",
	})
}

// HandleCancel cancels an active attempt (including swarm_monitoring) via ACP session/cancel.
// B2: Waits briefly for in-flight Prompt() exit and isolates/closes old SID if prompt or swarm workers remain active.
// B10: Confirms cancellation for unknown leased attempts never accepted by this runner.
func (r *Runner) HandleCancel(frame protocol.AttemptCancelFrame) {
	r.mu.Lock()
	att, ok := r.attempts[frame.AttemptID]
	r.mu.Unlock()
	if !ok {
		if frame.AttemptID != "" && frame.LeaseToken != "" {
			if hasCur, err := r.outbox.HasCursor(r.runCtx, frame.AttemptID); err == nil && !hasCur {
				_, _ = r.enqueueOutbox(context.Background(), frame.AttemptID, frame.LeaseToken, protocol.OutboxFrameState, map[string]any{
					"state":       string(protocol.AttemptCancelled),
					"errorCode":   "CANCELLED",
					"errorDetail": "attempt never accepted by runner",
				})
			}
		}
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
	slot := att.slot
	cancelFn := att.cancelFn
	promptReturned := att.promptReturned
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
	if cancelFn != nil {
		cancelFn()
	}

	// B2: Wait briefly for an in-flight Prompt() to wind down cleanly before releasing session ownership.
	if sid != "" && !promptReturned {
		select {
		case <-att.promptDoneCh:
		case <-time.After(1500 * time.Millisecond):
		}
	}

	att.mu.Lock()
	stillRunning := sid != "" && !att.promptReturned
	att.mu.Unlock()

	sessionLocked := false
	if stillRunning || att.tracker.HasActiveWorkers() {
		// Old prompt hasn't returned or swarm workers are still active -> isolate & close old SID!
		r.isolateAndCloseSession(slot, sid)
		sessionLocked = true
	}

	r.coalescer.FlushAttempt(att.dispatch.AttemptID)
	_ = r.finalizeAttempt(att, protocol.AttemptCancelled, map[string]any{
		"state":         string(protocol.AttemptCancelled),
		"sessionLocked": sessionLocked,
		"errorCode":     "CANCELLED",
		"errorDetail":   "cancelled by hub",
	})
}

// HandleLocalDeliver handles `orbit deliver --summary "..." [--final] [--resume] [--delivery-id ...]` over ORBIT_RUNNER_SOCK.
// B2: Never tears down sessionOwner while Prompt() is still in-flight; defers --final until Prompt() returns,
// and isolates old SID if --final occurs with active swarm workers.
// B12: Stages and uploads pkg.Blobs to Hub /api/blobs/:sha256 before emitting delivery_published.
// B13: Reuses existing PendingDelivery on --resume/--delivery-id retry without re-committing, and emits
// delivery_published (+ optional terminal state) + emitted=1 atomically inside one local SQLite tx.
func (r *Runner) HandleLocalDeliver(ctx context.Context, req LocalDeliverRequest) (LocalDeliverResponse, error) {
	r.deliverMu.Lock()
	defer r.deliverMu.Unlock()

	var resp LocalDeliverResponse
	att, err := r.findAttemptForLocalDeliver(ctx, req)
	if err != nil {
		return resp, err
	}

	att.mu.Lock()
	dispatch := att.dispatch
	worktreePath := att.worktreePath
	baseSHA := att.baseSHA
	sid := att.providerSessionID
	slot := att.slot
	att.mu.Unlock()

	summary := strings.TrimSpace(req.Summary)
	if summary == "" {
		summary = "Agent delivery"
	}

	if dispatch.ProjectID == protocol.ScratchProjectID || dispatch.MirrorBarePath == "" {
		dlvID := strings.TrimSpace(req.DeliveryID)
		if dlvID == "" {
			dlvID = "dlv_" + ulid.Make().String()
		}
		r.coalescer.FlushAttempt(dispatch.AttemptID)
		if _, err := r.enqueueOutbox(ctx, dispatch.AttemptID, dispatch.LeaseToken, protocol.OutboxFrameEvents, map[string]any{
			"kind":       string(protocol.EventDeliveryPublished),
			"visibility": string(protocol.VisibilityTimeline),
			"actorKind":  string(protocol.ActorAgent),
			"actorId":    dispatch.Adapter,
			"payload": map[string]any{
				"deliveryId": dlvID,
				"summary":    summary,
				"final":      req.Final,
			},
		}); err != nil {
			return resp, fmt.Errorf("enqueue scratch delivery event: %w", err)
		}

		activeWorkers := att.tracker.ActiveWorkerCount()
		att.mu.Lock()
		att.hasDelivery = true
		promptReturned := att.promptReturned
		if !promptReturned && req.Final {
			att.finalRequested = true
			att.explicitFinalDeliveryID = dlvID
		}
		att.mu.Unlock()

		finalized := false
		if promptReturned && (req.Final || activeWorkers == 0) {
			sessionLocked := false
			if activeWorkers > 0 {
				r.isolateAndCloseSession(slot, sid)
				sessionLocked = true
			}
			_, _, rejCount, nonZeroCount := att.tracker.Stats()
			unresRej, unresNonZero := att.tracker.UnresolvedCounts()
			if err := r.finalizeAttempt(att, protocol.AttemptSucceeded, map[string]any{
				"state":                       string(protocol.AttemptSucceeded),
				"completionSource":            string(protocol.CompletionExplicitDeliver),
				"deliveryId":                  dlvID,
				"sessionLocked":               sessionLocked,
				"rejectedToolCount":           rejCount,
				"nonZeroExitCount":            nonZeroCount,
				"unresolvedRejectedToolCount": unresRej,
				"unresolvedNonZeroExitCount":  unresNonZero,
			}); err != nil {
				return resp, fmt.Errorf("finalize scratch attempt: %w", err)
			}
			finalized = true
		}

		return LocalDeliverResponse{
			DeliveryID:  dlvID,
			AttemptID:   dispatch.AttemptID,
			TaskID:      dispatch.TaskID,
			ProjectID:   dispatch.ProjectID,
			BaseSHA:     baseSHA,
			HeadSHA:     "scratch",
			Finalized:   finalized,
			ActiveSwarm: activeWorkers,
		}, nil
	}

	// B13: Reuse previously saved PendingDelivery on --resume / matching --delivery-id without re-committing!
	var pd PendingDelivery
	reusedPending := false
	reqDlvID := strings.TrimSpace(req.DeliveryID)
	if reqDlvID != "" {
		if existing, found, err := r.outbox.GetPendingDelivery(ctx, reqDlvID); err != nil {
			return resp, fmt.Errorf("lookup pending delivery %s: %w", reqDlvID, err)
		} else if found {
			if existing.AttemptID != dispatch.AttemptID {
				return resp, fmt.Errorf("delivery %s belongs to attempt %s, not %s", reqDlvID, existing.AttemptID, dispatch.AttemptID)
			}
			pd = existing
			reusedPending = true
			if req.Final && !pd.IsFinal {
				pd.IsFinal = true
				_ = r.outbox.UpdatePendingDeliveryFinal(ctx, pd.DeliveryID, true)
			}
		}
	} else if req.Resume {
		list, err := r.outbox.ListUnemittedDeliveries(ctx)
		if err != nil {
			return resp, fmt.Errorf("list unemitted deliveries: %w", err)
		}
		for i := len(list) - 1; i >= 0; i-- {
			if list[i].AttemptID == dispatch.AttemptID {
				pd = list[i]
				reusedPending = true
				if req.Final && !pd.IsFinal {
					pd.IsFinal = true
					_ = r.outbox.UpdatePendingDeliveryFinal(ctx, pd.DeliveryID, true)
				}
				break
			}
		}
		if !reusedPending {
			return resp, fmt.Errorf("no pending unemitted delivery found to resume for attempt %s", dispatch.AttemptID)
		}
	}

	if !reusedPending {
		dlvID := reqDlvID
		if dlvID == "" {
			dlvID = "dlv_" + ulid.Make().String()
		}
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
		stagedBlobs, err := r.stagePackagedBlobs(pkg.Blobs)
		if err != nil {
			return resp, fmt.Errorf("stage delivery attachments: %w", err)
		}
		pd = PendingDelivery{
			DeliveryID:     dlvID,
			AttemptID:      dispatch.AttemptID,
			LeaseToken:     dispatch.LeaseToken,
			TaskID:         dispatch.TaskID,
			ProjectID:      dispatch.ProjectID,
			RepoRoot:       pkg.RepoRoot,
			MirrorBarePath: dispatch.MirrorBarePath,
			ReviewBaseSHA:  pkg.ReviewBaseSHA,
			HeadSHA:        pkg.HeadSHA,
			Summary:        summary,
			Manifest:       pkg.Manifest,
			StagedBlobs:    stagedBlobs,
			IsFinal:        req.Final,
		}
		if err := r.outbox.SavePendingDelivery(ctx, pd); err != nil {
			return resp, fmt.Errorf("save pending delivery: %w", err)
		}
	}

	resp = LocalDeliverResponse{
		DeliveryID:   pd.DeliveryID,
		AttemptID:    dispatch.AttemptID,
		TaskID:       dispatch.TaskID,
		ProjectID:    dispatch.ProjectID,
		BaseSHA:      pd.ReviewBaseSHA,
		HeadSHA:      pd.HeadSHA,
		FilesChanged: pd.Manifest.FilesChanged,
		Insertions:   pd.Manifest.Insertions,
		Deletions:    pd.Manifest.Deletions,
	}

	// B12: Upload all packaged blobs to Hub /api/blobs/:sha256 BEFORE emitting delivery_published!
	if err := r.uploadStagedBlobs(ctx, pd.StagedBlobs); err != nil {
		return resp, fmt.Errorf("upload delivery blobs (saved locally as %s for retry): %w", pd.DeliveryID, err)
	}

	// Push delivery ref to bare mirror (protected by pre-receive hook)
	if !pd.Pushed {
		if err := gitops.PushDeliveryRef(ctx, pd.RepoRoot, pd.MirrorBarePath, pd.DeliveryID, pd.HeadSHA); err != nil {
			return resp, fmt.Errorf("push delivery ref (saved locally as %s for retry): %w", pd.DeliveryID, err)
		}
		if err := r.outbox.MarkDeliveryPushed(ctx, pd.DeliveryID); err != nil {
			return resp, fmt.Errorf("mark delivery pushed: %w", err)
		}
		pd.Pushed = true
	}

	// Flush any buffered chunks before publishing delivery
	r.coalescer.FlushAttempt(dispatch.AttemptID)

	activeWorkers := att.tracker.ActiveWorkerCount()
	att.mu.Lock()
	promptReturned := att.promptReturned
	alreadyFinal := att.finalized
	att.mu.Unlock()

	shouldFinalizeNow := promptReturned && !alreadyFinal && (req.Final || pd.IsFinal || activeWorkers == 0)

	dlvPayload := map[string]any{
		"deliveryId": pd.DeliveryID,
		"baseSha":    pd.ReviewBaseSHA,
		"headSha":    pd.HeadSHA,
		"gitRef":     "refs/orbit/deliveries/" + pd.DeliveryID,
		"summary":    pd.Summary,
		"manifest":   pd.Manifest,
	}

	var finalPayload map[string]any
	sessionLocked := false
	if shouldFinalizeNow {
		att.ackPendingWG.Wait()
		if activeWorkers > 0 {
			// B2: --final while swarm workers are still active -> isolate & close old SID before releasing!
			r.isolateAndCloseSession(slot, sid)
			sessionLocked = true
		}
		_, _, rejCount, nonZeroCount := att.tracker.Stats()
		unresRej, unresNonZero := att.tracker.UnresolvedCounts()
		finalPayload = map[string]any{
			"state":                       string(protocol.AttemptSucceeded),
			"completionSource":            string(protocol.CompletionExplicitDeliver),
			"deliveryId":                  pd.DeliveryID,
			"sessionLocked":               sessionLocked,
			"rejectedToolCount":           rejCount,
			"nonZeroExitCount":            nonZeroCount,
			"unresolvedRejectedToolCount": unresRej,
			"unresolvedNonZeroExitCount":  unresNonZero,
		}
	}

	// B13 & B19: Atomically persist delivery_published (+ final state if finalizing now) + emitted=1 in ONE SQLite tx!
	if !pd.Emitted {
		emitted, err := r.outbox.EmitPendingDeliveryAtomically(
			context.Background(),
			pd.DeliveryID,
			dispatch.AttemptID,
			dispatch.LeaseToken,
			"evt_"+ulid.Make().String(),
			dlvPayload,
			shouldFinalizeNow,
			"evt_"+ulid.Make().String(),
			finalPayload,
		)
		if err != nil {
			return resp, fmt.Errorf("emit delivery atomically: %w", err)
		}
		if emitted {
			r.wakeOutboxPump()
		}
	} else if shouldFinalizeNow {
		if err := r.finalizeAttempt(att, protocol.AttemptSucceeded, finalPayload); err != nil {
			return resp, fmt.Errorf("finalize attempt: %w", err)
		}
	}

	finalized := false
	if shouldFinalizeNow {
		// Atomic transaction already persisted terminal state -> now safely finalize in-memory state & release resources!
		r.completeInMemoryFinalize(att, protocol.AttemptSucceeded)
		finalized = true
	} else {
		att.mu.Lock()
		att.hasDelivery = true
		if !promptReturned && (req.Final || pd.IsFinal) {
			att.finalRequested = true
			att.explicitFinalDeliveryID = pd.DeliveryID
		}
		att.mu.Unlock()
	}

	resp = LocalDeliverResponse{
		DeliveryID:   pd.DeliveryID,
		AttemptID:    dispatch.AttemptID,
		TaskID:       dispatch.TaskID,
		ProjectID:    dispatch.ProjectID,
		BaseSHA:      pd.ReviewBaseSHA,
		HeadSHA:      pd.HeadSHA,
		Finalized:    finalized,
		ActiveSwarm:  activeWorkers,
		FilesChanged: pd.Manifest.FilesChanged,
		Insertions:   pd.Manifest.Insertions,
		Deletions:    pd.Manifest.Deletions,
	}
	return resp, nil
}

func (r *Runner) stagePackagedBlobs(blobs []gitops.PackagedBlob) ([]gitops.PackagedBlob, error) {
	if len(blobs) == 0 {
		return nil, nil
	}
	stageDir := filepath.Join(r.cfg.DataDir, "runner-staged-blobs")
	if err := os.MkdirAll(stageDir, 0o700); err != nil {
		return nil, err
	}
	out := make([]gitops.PackagedBlob, 0, len(blobs))
	for _, b := range blobs {
		dest := filepath.Join(stageDir, b.Item.SHA256)
		if fi, err := os.Stat(dest); err == nil && fi.Size() == b.Item.Size {
			out = append(out, gitops.PackagedBlob{Item: b.Item, LocalPath: dest})
			continue
		}
		src, err := os.Open(b.LocalPath)
		if err != nil {
			return nil, fmt.Errorf("open attachment %s: %w", b.LocalPath, err)
		}
		tmp := dest + ".tmp-" + ulid.Make().String()
		dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			_ = src.Close()
			return nil, err
		}
		h := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(dst, h), src)
		_ = src.Close()
		closeErr := dst.Close()
		if copyErr != nil {
			_ = os.Remove(tmp)
			return nil, copyErr
		}
		if closeErr != nil {
			_ = os.Remove(tmp)
			return nil, closeErr
		}
		actual := hex.EncodeToString(h.Sum(nil))
		if actual != b.Item.SHA256 || n != b.Item.Size {
			_ = os.Remove(tmp)
			return nil, fmt.Errorf("attachment %s changed during staging", b.LocalPath)
		}
		if err := os.Rename(tmp, dest); err != nil {
			_ = os.Remove(tmp)
			return nil, err
		}
		out = append(out, gitops.PackagedBlob{Item: b.Item, LocalPath: dest})
	}
	return out, nil
}

func (r *Runner) uploadStagedBlobs(ctx context.Context, blobs []gitops.PackagedBlob) error {
	if len(blobs) == 0 {
		return nil
	}
	hubHTTP := strings.TrimRight(strings.TrimSpace(r.cfg.HubHTTPURL), "/")
	if hubHTTP == "" {
		return errors.New("runner HubHTTPURL not configured; cannot upload delivery blobs")
	}
	seen := map[string]bool{}
	for _, b := range blobs {
		if seen[b.Item.SHA256] {
			continue
		}
		seen[b.Item.SHA256] = true

		blobURL := hubHTTP + "/api/blobs/" + b.Item.SHA256
		headReq, err := http.NewRequestWithContext(ctx, http.MethodHead, blobURL, nil)
		if err != nil {
			return err
		}
		r.setRunnerHTTPAuthHeaders(headReq)
		headResp, err := r.httpClient.Do(headReq)
		if err != nil {
			return fmt.Errorf("head blob %s: %w", b.Item.SHA256, err)
		}
		_ = headResp.Body.Close()
		if headResp.StatusCode == http.StatusOK {
			continue
		}

		f, err := os.Open(b.LocalPath)
		if err != nil {
			return fmt.Errorf("open staged blob %s: %w", b.LocalPath, err)
		}
		putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, blobURL, f)
		if err != nil {
			_ = f.Close()
			return err
		}
		putReq.ContentLength = b.Item.Size
		putReq.Header.Set("Content-Type", "application/octet-stream")
		r.setRunnerHTTPAuthHeaders(putReq)
		putResp, err := r.httpClient.Do(putReq)
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("put blob %s: %w", b.Item.SHA256, err)
		}
		raw, _ := io.ReadAll(putResp.Body)
		_ = putResp.Body.Close()
		if putResp.StatusCode < 200 || putResp.StatusCode >= 300 {
			return fmt.Errorf("put blob %s failed (%d): %s", b.Item.SHA256, putResp.StatusCode, strings.TrimSpace(string(raw)))
		}
	}
	return nil
}

func (r *Runner) setRunnerHTTPAuthHeaders(req *http.Request) {
	req.Header.Set("X-Orbit-Machine-Id", r.cfg.MachineID)
	req.Header.Set("X-Orbit-Request", "1")
	if tok := strings.TrimSpace(r.cfg.BearerToken); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
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
		// B15: Flush any buffered thought/message chunks strictly BEFORE tool call events!
		r.coalescer.FlushAttempt(attemptID)
		proj := att.tracker.ObserveToolCall(u.ToolCall)
		name := ""
		if u.ToolCall.Name != nil {
			name = *u.ToolCall.Name
		}
		evPayload := map[string]any{
			"toolCallId":  string(u.ToolCall.ToolCallId),
			"name":        name,
			"title":       u.ToolCall.Title,
			"status":      string(u.ToolCall.Status),
			"unsolicited": unsolicited,
		}
		if wn := att.tracker.WorkerNameForToolCall(string(u.ToolCall.ToolCallId)); wn != "" {
			evPayload["workerName"] = wn
		}
		_, _ = r.enqueueOutbox(r.runCtx, attemptID, leaseToken, protocol.OutboxFrameEvents, map[string]any{
			"kind":       string(protocol.EventACPToolCall),
			"visibility": string(protocol.VisibilityRawACP),
			"actorKind":  string(protocol.ActorAgent),
			"actorId":    string(att.dispatch.Adapter),
			"payload":    evPayload,
		})
		r.emitSwarmProjections(attemptID, leaseToken, proj)

	case u.ToolCallUpdate != nil:
		r.coalescer.FlushAttempt(attemptID)
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
		if wn := att.tracker.WorkerNameForToolCall(string(u.ToolCallUpdate.ToolCallId)); wn != "" {
			evPayload["workerName"] = wn
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
		r.coalescer.FlushAttempt(attemptID)
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
		r.coalescer.FlushAttempt(attemptID)
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

	writeMeta, hasWriteMeta := PermissionWriteMetaFromContext(ctx)

	approvalID := "apr_" + ulid.Make().String()
	handle := &pendingApprovalHandle{
		approvalID:    approvalID,
		attemptID:     attemptID,
		payloadSHA256: payloadSHA,
		options:       options,
		ch:            make(chan protocol.ApprovalDecisionPayload, 1),
		writeMeta:     writeMeta,
		hasWriteMeta:  hasWriteMeta,
	}

	att.mu.Lock()
	if att.finalized {
		att.mu.Unlock()
		return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
	}
	prevState := att.state
	prevResume := att.resumeState
	firstPending := len(att.pendingApprovals) == 0
	if firstPending {
		if att.state == protocol.AttemptSwarmMonitoring {
			att.resumeState = protocol.ResumeStateSwarmMonitoring
			// B7: Pause swarm idle timer while waiting on human approval; it will be re-armed on resume.
			if att.swarmIdleTimer != nil {
				att.swarmIdleTimer.Stop()
			}
		} else {
			att.resumeState = protocol.ResumeStateRunning
		}
		att.state = protocol.AttemptWaitingApproval
	}
	resumeState := att.resumeState
	att.pendingApprovals[approvalID] = handle
	att.mu.Unlock()

	r.coalescer.FlushAttempt(attemptID)
	reqPayload := map[string]any{
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
	}
	if hasWriteMeta && writeMeta.RPCID != "" {
		reqPayload["acpRpcId"] = writeMeta.RPCID
	}
	// B19: Roll back in-memory pending approval/state if Outbox persistence fails!
	if _, err := r.enqueueOutbox(r.runCtx, attemptID, leaseToken, protocol.OutboxFrameApprovalRequest, reqPayload); err != nil {
		att.mu.Lock()
		delete(att.pendingApprovals, approvalID)
		if firstPending && len(att.pendingApprovals) == 0 {
			att.state = prevState
			att.resumeState = prevResume
		}
		slot := att.slot
		restoredSwarm := att.state == protocol.AttemptSwarmMonitoring
		att.mu.Unlock()
		if restoredSwarm {
			r.armSwarmIdleTimer(att, slot)
		}
		return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, fmt.Errorf("persist approval_request outbox: %w", err)
	}

	// Wait for human decision via approval.decision or hub.welcome.reconcile.redeliverApprovals
	select {
	case dec, ok := <-handle.ch:
		if !ok || dec.Expired || dec.OptionID == "" {
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
// waiting ACP permission handle, waits for the real ACP stdio write boundary (B6), records
// local handled_approvals, emits approval_ack via Outbox, and re-arms swarmIdleTimer (B7).
func (r *Runner) HandleApprovalDecision(dec protocol.ApprovalDecisionPayload) {
	if dec.ApprovalID == "" || dec.DecisionID == "" {
		return
	}

	// B7: Hub expiry unblock signal releases the hanging ACP RequestPermission with Cancelled outcome.
	if dec.Expired {
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
		delete(att.pendingApprovals, dec.ApprovalID)
		remaining := len(att.pendingApprovals)
		rearmSwarm := false
		slot := att.slot
		if remaining == 0 && att.state == protocol.AttemptWaitingApproval {
			if att.resumeState == protocol.ResumeStateSwarmMonitoring {
				att.state = protocol.AttemptSwarmMonitoring
				rearmSwarm = true
			} else {
				att.state = protocol.AttemptRunning
			}
		}
		att.mu.Unlock()
		if rearmSwarm {
			r.armSwarmIdleTimer(att, slot)
		}
		select {
		case handle.ch <- dec:
		default:
		}
		return
	}

	// 1. Idempotency check on decisionId
	if prev, found, err := r.outbox.LookupHandledApproval(r.runCtx, dec.DecisionID); err == nil && found {
		r.mu.Lock()
		att := r.attempts[dec.AttemptID]
		r.mu.Unlock()
		if att != nil && (prev.AttemptID == "" || prev.AttemptID == att.dispatch.AttemptID) {
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
	rearmSwarm := false
	slot := att.slot
	if remaining == 0 && att.state == protocol.AttemptWaitingApproval {
		if restoreState == protocol.ResumeStateSwarmMonitoring {
			att.state = protocol.AttemptSwarmMonitoring
			rearmSwarm = true
		} else {
			att.state = protocol.AttemptRunning
		}
	}
	leaseToken := att.dispatch.LeaseToken
	att.ackPendingWG.Add(1)
	att.mu.Unlock()
	defer att.ackPendingWG.Done()

	// B7: Immediately re-arm swarmIdleTimer when the last pending approval settles back to swarm_monitoring!
	if rearmSwarm {
		r.armSwarmIdleTimer(att, slot)
	}

	// 3. Deliver decision to waiting ACP RequestPermission goroutine
	select {
	case handle.ch <- dec:
	default:
	}

	// B6: When backed by a real ACP stdio connection, wait strictly until acp-go-sdk finishes
	// writing the JSON-RPC permission response bytes to the underlying stdio Writer!
	if handle.hasWriteMeta && handle.writeMeta.WriteDoneCh != nil {
		select {
		case writeErr, ok := <-handle.writeMeta.WriteDoneCh:
			if !ok || writeErr != nil {
				// Underlying ACP stdio write failed -> do NOT record handled_approvals or send approval_ack!
				return
			}
		case <-r.runCtx.Done():
			return
		case <-time.After(10 * time.Second):
			return
		}
	}

	// Deterministic R3 crash-window test hook: crash right AFTER real ACP write, BEFORE local record/ack
	if r.cfg.OnAfterACPApprovalWrite != nil && !r.cfg.OnAfterACPApprovalWrite(dec.ApprovalID) {
		return
	}

	// 4. Persist locally for decisionId idempotency & reconnect reconciliation
	if err := r.outbox.RecordHandledApproval(r.runCtx, dec.DecisionID, dec.ApprovalID, dec.AttemptID, outcome, dec.OptionID); err != nil {
		return
	}

	// 5. Emit approval_ack via ordered Outbox
	_, _ = r.enqueueOutbox(r.runCtx, dec.AttemptID, leaseToken, protocol.OutboxFrameApprovalAck, map[string]any{
		"approvalId": dec.ApprovalID,
		"decisionId": dec.DecisionID,
		"outcome":    string(outcome),
	})
}

// defaultPermissionPolicy implements B5 strict verifiable worktree allowlist:
// - Explicit mode=default -> always human approval.
// - Missing kind, kind=other, delete/move/fetch/switch_mode -> ALWAYS human approval.
// - read / search / edit -> ONLY auto-allowed when worktreePath is set, non-empty locations are
//   present, and EVERY location resolves strictly inside worktreePath (excluding .git internals).
// - execute -> ALWAYS human approval EXCEPT strict single non-chained whitelisted local commands
//   (orbit deliver, read-only git, go test/vet/build/list) with any declared locations inside worktree.
// - think -> safe to auto-allow.
func defaultPermissionPolicy(worktreePath, desiredMode string, req acp.RequestPermissionRequest) (string, bool) {
	if strings.EqualFold(desiredMode, "default") {
		return "", false
	}

	allowOptID := ""
	for _, opt := range req.Options {
		if opt.Kind == acp.PermissionOptionKindAllowOnce {
			allowOptID = string(opt.OptionId)
			break
		}
	}
	if allowOptID == "" {
		return "", false
	}

	kind, hasKind := req.ToolCall.Kind.Value()
	if !hasKind || kind == "" || kind == acp.ToolKindOther {
		return "", false
	}

	locs, hasLocs := req.ToolCall.Locations.Value()
	allLocationsInWorktree := func(requireNonEmpty bool, forbidGitInternals bool) bool {
		if strings.TrimSpace(worktreePath) == "" {
			return false
		}
		if requireNonEmpty && (!hasLocs || len(locs) == 0) {
			return false
		}
		if !hasLocs || len(locs) == 0 {
			return true
		}
		canonWT := canonicalWorktreeCheckPath(worktreePath)
		if canonWT == "" {
			return false
		}
		for _, loc := range locs {
			p := strings.TrimSpace(loc.Path)
			if p == "" {
				return false
			}
			if !filepath.IsAbs(p) {
				p = filepath.Join(canonWT, p)
			}
			canonLoc := canonicalWorktreeCheckPath(p)
			if canonLoc == "" {
				return false
			}
			rel, err := filepath.Rel(canonWT, canonLoc)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
				return false
			}
			if forbidGitInternals {
				slashed := filepath.ToSlash(rel)
				if slashed == ".git" || strings.HasPrefix(slashed, ".git/") {
					return false
				}
			}
		}
		return true
	}

	switch kind {
	case acp.ToolKindThink:
		return allowOptID, true

	case acp.ToolKindRead, acp.ToolKindSearch:
		if allLocationsInWorktree(true, false) {
			return allowOptID, true
		}
		return "", false

	case acp.ToolKindEdit:
		if allLocationsInWorktree(true, true) {
			return allowOptID, true
		}
		return "", false

	case acp.ToolKindExecute:
		if !allLocationsInWorktree(false, false) {
			return "", false
		}
		if isWhitelistedLocalCommand(req) {
			return allowOptID, true
		}
		return "", false

	default:
		return "", false
	}
}

func canonicalWorktreeCheckPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(real)
	}
	// Target file may not exist yet (e.g., new file edit) -> resolve nearest existing ancestor.
	dir := filepath.Dir(abs)
	base := filepath.Base(abs)
	if realDir, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Clean(filepath.Join(realDir, base))
	}
	return filepath.Clean(abs)
}

func isWhitelistedLocalCommand(req acp.RequestPermissionRequest) bool {
	rawIn, ok := req.ToolCall.RawInput.Value()
	if !ok || len(rawIn) == 0 {
		return false
	}
	var m map[string]any
	if err := json.Unmarshal(rawIn, &m); err != nil {
		return false
	}
	cmd := ""
	for _, k := range []string{"command", "cmd", "CommandLine"} {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			cmd = strings.TrimSpace(s)
			break
		}
	}
	if cmd == "" {
		return false
	}
	// Reject any shell chaining, redirection, subshell, or multi-line command injection.
	for _, meta := range []string{"&&", "||", ";", "|", "`", "$(", ">", "<", "\n", "\r"} {
		if strings.Contains(cmd, meta) {
			return false
		}
	}
	fields := strings.Fields(cmd)
	if len(fields) < 2 {
		return false
	}
	bin := filepath.Base(fields[0])
	sub := fields[1]
	switch bin {
	case "orbit":
		return sub == "deliver" || sub == "status" || sub == "brief"
	case "git":
		switch sub {
		case "status", "diff", "log", "show", "rev-parse", "ls-files":
			return true
		default:
			return false
		}
	case "go":
		switch sub {
		case "test", "vet", "build", "list":
			return true
		default:
			return false
		}
	default:
		return false
	}
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
	attemptID := att.dispatch.AttemptID
	leaseToken := att.dispatch.LeaseToken
	att.mu.Unlock()

	r.coalescer.FlushAttempt(attemptID)
	if _, err := r.enqueueOutbox(r.runCtx, attemptID, leaseToken, protocol.OutboxFrameState, payload); err != nil {
		return
	}

	att.mu.Lock()
	if !att.finalized {
		att.state = st
	}
	att.mu.Unlock()
}

// finalizeAttempt writes the terminal state frame to the local SQLite Outbox FIRST, and ONLY
// after durable persistence mutates att.finalized, releases resources, clears sessionOwner,
// and cancels the attempt context (B19).
func (r *Runner) finalizeAttempt(att *attemptContext, terminalState protocol.AttemptState, payload map[string]any) error {
	att.mu.Lock()
	if att.finalized || att.finalizing {
		att.mu.Unlock()
		return nil
	}
	att.finalizing = true
	attemptID := att.dispatch.AttemptID
	leaseToken := att.dispatch.LeaseToken
	att.mu.Unlock()

	r.coalescer.FlushAttempt(attemptID)
	if _, err := r.enqueueOutbox(context.Background(), attemptID, leaseToken, protocol.OutboxFrameState, payload); err != nil {
		att.mu.Lock()
		att.finalizing = false
		att.mu.Unlock()
		return err
	}

	r.completeInMemoryFinalize(att, terminalState)
	return nil
}

func (r *Runner) completeInMemoryFinalize(att *attemptContext, terminalState protocol.AttemptState) {
	att.mu.Lock()
	if att.finalized {
		att.mu.Unlock()
		return
	}
	att.finalized = true
	att.finalizing = false
	att.state = terminalState
	if att.swarmIdleTimer != nil {
		att.swarmIdleTimer.Stop()
	}
	sid := att.providerSessionID
	attemptID := att.dispatch.AttemptID
	cancelFn := att.cancelFn
	att.mu.Unlock()

	r.releaseAttemptResources(att)

	r.mu.Lock()
	if sid != "" && r.sessionOwner[sid] == attemptID {
		delete(r.sessionOwner, sid)
	}
	r.mu.Unlock()

	if cancelFn != nil {
		cancelFn()
	}
}

// HandleBoardMessage delivers a Hub board.message @mention into an active swarm_monitoring attempt (B16).
func (r *Runner) HandleBoardMessage(frame protocol.BoardMessageDispatchFrame) {
	r.mu.Lock()
	att := r.attempts[frame.AttemptID]
	r.mu.Unlock()
	if att == nil {
		return
	}

	att.mu.Lock()
	if att.finalized || att.state != protocol.AttemptSwarmMonitoring || att.providerSessionID == "" || att.slot == nil {
		att.mu.Unlock()
		return
	}
	sid := att.providerSessionID
	slot := att.slot
	leaseToken := att.dispatch.LeaseToken
	att.mu.Unlock()

	slot.mu.Lock()
	if slot.locked || slot.inFlightPrompt || slot.providerSessionID != sid {
		slot.mu.Unlock()
		return
	}
	slot.inFlightPrompt = true
	slot.promptOwnerAttemptID = att.dispatch.AttemptID
	slot.mu.Unlock()

	msgText := fmt.Sprintf("[Board Message #%d from %s targeting @%s]\n%s", frame.Seq, frame.AuthorName, frame.TargetMention, frame.Body)
	pctx, cancel := context.WithTimeout(r.runCtx, 2*time.Minute)
	_, err := r.driver.Prompt(pctx, sid, msgText)
	cancel()

	slot.mu.Lock()
	if slot.promptOwnerAttemptID == att.dispatch.AttemptID {
		slot.inFlightPrompt = false
		slot.promptOwnerAttemptID = ""
	}
	slot.lastActivityAt = time.Now()
	slot.mu.Unlock()

	if err == nil {
		r.resetSwarmIdleTimerIfMonitoring(att)
		_, _ = r.enqueueOutbox(r.runCtx, frame.AttemptID, leaseToken, protocol.OutboxFrameBoardAck, map[string]any{
			"messageId":  frame.MessageID,
			"memberName": frame.TargetMention,
			"seq":        frame.Seq,
		})
	}
}

func (r *Runner) pruneFinalizedAttemptIfDrained(ctx context.Context, attemptID string) {
	r.mu.Lock()
	att, ok := r.attempts[attemptID]
	r.mu.Unlock()
	if !ok {
		return
	}
	att.mu.Lock()
	finalized := att.finalized
	att.mu.Unlock()
	if !finalized {
		return
	}
	select {
	case <-att.doneCh:
	default:
		return
	}
	hasUnacked, err1 := r.outbox.HasUnackedItems(ctx, attemptID)
	hasUnemitted, err2 := r.outbox.HasUnemittedDeliveriesForAttempt(ctx, attemptID)
	if err1 == nil && err2 == nil && !hasUnacked && !hasUnemitted {
		r.mu.Lock()
		if cur, exists := r.attempts[attemptID]; exists && cur == att {
			delete(r.attempts, attemptID)
		}
		r.mu.Unlock()
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
		r.pruneFinalizedAttemptIfDrained(ctx, aid)
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
					Expired:       f.Expired,
				})
			}
		case protocol.RunnerMsgBoardMessage:
			var f protocol.BoardMessageDispatchFrame
			if json.Unmarshal(data, &f) == nil {
				go r.HandleBoardMessage(f)
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
				r.pruneFinalizedAttemptIfDrained(ctx, f.AttemptID)
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
	type snapAtt struct {
		attemptID  string
		leaseToken string
		state      protocol.AttemptState
		finalized  bool
		approvals  []string
	}
	snaps := make([]snapAtt, 0, len(r.attempts))
	for _, a := range r.attempts {
		a.mu.Lock()
		s := snapAtt{
			attemptID:  a.dispatch.AttemptID,
			leaseToken: a.dispatch.LeaseToken,
			state:      a.state,
			finalized:  a.finalized,
		}
		if !a.finalized {
			for aprID := range a.pendingApprovals {
				s.approvals = append(s.approvals, aprID)
			}
		}
		a.mu.Unlock()
		snaps = append(snaps, s)
	}
	r.mu.Unlock()

	liveAttempts := make([]protocol.LiveAttemptInfo, 0, len(snaps))
	liveApprovals := []string{}
	for _, s := range snaps {
		if !s.finalized {
			liveAttempts = append(liveAttempts, protocol.LiveAttemptInfo{
				AttemptID:  s.attemptID,
				LeaseToken: s.leaseToken,
				State:      s.state,
			})
			liveApprovals = append(liveApprovals, s.approvals...)
			continue
		}
		// B3: Report locally finalized attempts that STILL have unacked outbox frames or unemitted
		// deliveries so Hub drains formal delivery/state instead of marking them expired on reconnect!
		hasUnacked, _ := r.outbox.HasUnackedItems(ctx, s.attemptID)
		hasUnemitted, _ := r.outbox.HasUnemittedDeliveriesForAttempt(ctx, s.attemptID)
		if hasUnacked || hasUnemitted {
			liveAttempts = append(liveAttempts, protocol.LiveAttemptInfo{
				AttemptID:    s.attemptID,
				LeaseToken:   s.leaseToken,
				State:        s.state,
				PendingFinal: true,
			})
		}
	}

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
		Adapters:                 r.supportedAdapters(),
		LiveAttempts:             liveAttempts,
		LivePendingApprovalIDs:   liveApprovals,
		HandledApprovalDecisions: handled,
		Resources:                resources,
	}
}

func (r *Runner) buildHeartbeat(ctx context.Context) protocol.RunnerHeartbeatFrame {
	r.mu.Lock()
	type snapLease struct {
		attemptID  string
		leaseToken string
		finalized  bool
	}
	snaps := make([]snapLease, 0, len(r.attempts))
	for _, a := range r.attempts {
		a.mu.Lock()
		snaps = append(snaps, snapLease{
			attemptID:  a.dispatch.AttemptID,
			leaseToken: a.dispatch.LeaseToken,
			finalized:  a.finalized,
		})
		a.mu.Unlock()
	}
	r.mu.Unlock()

	leases := make([]protocol.LiveAttemptLease, 0, len(snaps))
	for _, s := range snaps {
		if !s.finalized {
			leases = append(leases, protocol.LiveAttemptLease{
				AttemptID:  s.attemptID,
				LeaseToken: s.leaseToken,
			})
			continue
		}
		if hasUnacked, _ := r.outbox.HasUnackedItems(ctx, s.attemptID); hasUnacked {
			leases = append(leases, protocol.LiveAttemptLease{
				AttemptID:  s.attemptID,
				LeaseToken: s.leaseToken,
			})
		}
	}

	resources := map[string]protocol.ResourceState{}
	if r.cfg.ResourceProbe != nil {
		resources = r.cfg.ResourceProbe(ctx)
	}
	return protocol.RunnerHeartbeatFrame{
		Type:                   protocol.RunnerMsgHeartbeat,
		MachineID:              r.cfg.MachineID,
		RunnerInstanceID:       r.instanceID,
		Adapters:               r.supportedAdapters(),
		LiveAttemptLeaseTokens: leases,
		Resources:              resources,
	}
}

func (r *Runner) retryPendingDeliveries(ctx context.Context) {
	r.deliverMu.Lock()
	defer r.deliverMu.Unlock()

	list, err := r.outbox.ListUnemittedDeliveries(ctx)
	if err != nil {
		return
	}
	for _, pd := range list {
		// B12: Upload any staged blobs first before pushing / emitting.
		if err := r.uploadStagedBlobs(ctx, pd.StagedBlobs); err != nil {
			continue
		}
		if !pd.Pushed {
			if err := gitops.PushDeliveryRef(ctx, pd.RepoRoot, pd.MirrorBarePath, pd.DeliveryID, pd.HeadSHA); err != nil {
				continue
			}
			_ = r.outbox.MarkDeliveryPushed(ctx, pd.DeliveryID)
			pd.Pushed = true
		}

		r.mu.Lock()
		att := r.attempts[pd.AttemptID]
		r.mu.Unlock()

		leaseToken := pd.LeaseToken
		if leaseToken == "" && att != nil {
			leaseToken = att.dispatch.LeaseToken
		}
		if leaseToken == "" {
			leaseToken, _, _ = r.outbox.ReadUnacked(ctx, pd.AttemptID, 0, 1)
		}
		if leaseToken == "" {
			continue
		}

		dlvPayload := map[string]any{
			"deliveryId": pd.DeliveryID,
			"baseSha":    pd.ReviewBaseSHA,
			"headSha":    pd.HeadSHA,
			"gitRef":     "refs/orbit/deliveries/" + pd.DeliveryID,
			"summary":    pd.Summary,
			"manifest":   pd.Manifest,
		}

		emitFinal := false
		var finalPayload map[string]any
		if pd.IsFinal {
			if att == nil {
				emitFinal = true
				finalPayload = map[string]any{
					"state":            string(protocol.AttemptSucceeded),
					"completionSource": string(protocol.CompletionExplicitDeliver),
					"deliveryId":       pd.DeliveryID,
				}
			} else {
				att.mu.Lock()
				promptReturned := att.promptReturned
				alreadyFinal := att.finalized
				sid := att.providerSessionID
				slot := att.slot
				att.mu.Unlock()
				if promptReturned && !alreadyFinal {
					emitFinal = true
					sessionLocked := false
					if att.tracker.ActiveWorkerCount() > 0 {
						r.isolateAndCloseSession(slot, sid)
						sessionLocked = true
					}
					_, _, rejCount, nonZeroCount := att.tracker.Stats()
					unresRej, unresNonZero := att.tracker.UnresolvedCounts()
					finalPayload = map[string]any{
						"state":                       string(protocol.AttemptSucceeded),
						"completionSource":            string(protocol.CompletionExplicitDeliver),
						"deliveryId":                  pd.DeliveryID,
						"sessionLocked":               sessionLocked,
						"rejectedToolCount":           rejCount,
						"nonZeroExitCount":            nonZeroCount,
						"unresolvedRejectedToolCount": unresRej,
						"unresolvedNonZeroExitCount":  unresNonZero,
					}
				}
			}
		}

		emitted, err := r.outbox.EmitPendingDeliveryAtomically(
			context.Background(),
			pd.DeliveryID,
			pd.AttemptID,
			leaseToken,
			"evt_"+ulid.Make().String(),
			dlvPayload,
			emitFinal,
			"evt_"+ulid.Make().String(),
			finalPayload,
		)
		if err != nil {
			continue
		}
		if emitted {
			r.wakeOutboxPump()
		}
		if att != nil {
			if emitFinal {
				r.completeInMemoryFinalize(att, protocol.AttemptSucceeded)
			} else {
				att.mu.Lock()
				att.hasDelivery = true
				if pd.IsFinal && !att.promptReturned {
					att.finalRequested = true
					att.explicitFinalDeliveryID = pd.DeliveryID
				}
				att.mu.Unlock()
			}
		}
	}
}

func (r *Runner) outboxPumpLoop() {
	defer r.wg.Done()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	// B13: Periodically retry failed pending deliveries even while WS stays continuously connected.
	deliveryRetryTicker := time.NewTicker(2 * time.Second)
	defer deliveryRetryTicker.Stop()

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
		case <-deliveryRetryTicker.C:
			r.retryPendingDeliveries(r.runCtx)
			flush()
		}
	}
}
