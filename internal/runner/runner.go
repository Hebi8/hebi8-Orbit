package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	OrbitBinPath            string
	RunnerVersion           string
	SupportedAdapters       []protocol.Adapter
	MaxLiveWorkers          int
	Driver                  AcpAdapterDriver
	Drivers                 map[protocol.Adapter]AcpAdapterDriver
	ConfigApplyTimeout      time.Duration
	ChunkCoalesceWindow     time.Duration
	DefaultSwarmIdleTimeout time.Duration
	DeliveryRetryInterval   time.Duration
	IdleCloseSeconds        int
	ReconnectInitialDelay   time.Duration
	ReconnectMaxDelay       time.Duration
	PermissionPolicy        PermissionDecisionPolicy
	ResourceProbe           ResourceProbeFunc
	AgentManager            *AgentManager
	QuotaCollector          *QuotaCollector
	ScopedDriverFactory     func(adapter protocol.Adapter, projectEnv map[string]string) (AcpAdapterDriver, error)
	AllowedBrowseRoots      []string
	// Test hooks for deterministic R3 crash simulation
	OnAfterACPApprovalWrite func(approvalID string) bool
}

type sessionSlot struct {
	mu                   sync.Mutex
	wbSessionID          string
	adapter              protocol.Adapter
	driver               AcpAdapterDriver
	scopedDriver         bool
	projectEnvHash       string
	providerSessionID    acp.SessionId
	sessionOpts          []acp.SessionConfigOption
	sessionModes         *acp.SessionModeState
	appliedMode          string
	cwd                  string
	runtimeState         protocol.SessionRuntimeState
	inFlightPrompt       bool
	promptOwnerAttemptID string
	delegateToken        string
	orbitMCPAttached     bool
	orbitMCPServerName   string
	locked               bool
	lastActivityAt       time.Time
}

type delegateTokenEntry struct {
	Token       string
	WBSessionID string
	TaskID      string
	ProjectID   string
	AttemptID   string
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
	driver                  AcpAdapterDriver
	worktreePath            string
	baseSHA                 string
	beforeFolderSnap        *gitops.FolderSnapshot
	state                   protocol.AttemptState
	resumeState             protocol.ResumeState
	providerSessionID        acp.SessionId
	appliedMode              string
	promptReturned           bool
	inFlightPrompts         int
	lastStopReason          acp.StopReason
	promptDoneCh            chan struct{}
	promptDoneOnce          sync.Once
	hasDelivery             bool
	lastDeliveredFingerprint string
	finalRequested          bool
	explicitFinalDeliveryID string
	finalizing              bool
	finalized               bool
	heldResources           []string
	reservedWorker          bool
	reservedWorkerAdapter   protocol.Adapter
	tracker                 *ActiveSwarmTracker
	pendingApprovals        map[string]*pendingApprovalHandle
	ackPendingWG            sync.WaitGroup
	boardQueue              []protocol.BoardMessageDispatchFrame
	boardDelivered          map[string]bool
	boardQueuedIDs          map[string]bool
	boardDraining           bool
	swarmIdleTimeout        time.Duration
	swarmIdleTimer          *time.Timer
	cancelFn                context.CancelFunc
	doneCh                  chan struct{}
}

type liveSlotMeta struct {
	adapter    protocol.Adapter
	live       bool
	inFlight   bool
	locked     bool
	lastActive time.Time
}

// Runner is the pure-Go thin ACP runner bridging Hub (/ws/runner) and AcpAdapterDriver(s).
type Runner struct {
	cfg            Config
	instanceID     string
	outbox         *LocalOutbox
	driver         AcpAdapterDriver
	drivers        map[protocol.Adapter]AcpAdapterDriver
	driverBins     map[protocol.Adapter]string
	retiredDrivers []AcpAdapterDriver
	driverMu       sync.Mutex
	driverStarted  map[protocol.Adapter]bool
	supportedAdps  []protocol.Adapter
	agentMgr       *AgentManager
	quotaCollector *QuotaCollector
	quotaRefreshMu sync.Mutex
	lastPostQuota  time.Time
	coalescer      *ChunkCoalescer
	sockSrv        *localSocketServer
	httpClient     *http.Client
	deliverMu      sync.Mutex

	mu                sync.Mutex
	sessions          map[string]*sessionSlot
	slotMeta          map[string]liveSlotMeta
	attempts          map[string]*attemptContext
	sessionOwner      map[acp.SessionId]string
	delegateTokens    map[string]*delegateTokenEntry
	resourceLocks     map[string]string // resourceName -> attemptID
	cancelledAttempts map[string]bool   // B10: attemptID cancelled before/during dispatch
	reservedWorkers   int               // S1: atomic hard-cap reservation for global MaxLiveWorkers
	reservedByAdapter map[protocol.Adapter]int

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
	drivers := make(map[protocol.Adapter]AcpAdapterDriver, len(cfg.Drivers)+1)
	for adp, drv := range cfg.Drivers {
		if drv != nil {
			drivers[adp] = drv
		}
	}
	if cfg.Driver != nil {
		defaultAdp := protocol.AdapterAgy
		if len(cfg.SupportedAdapters) == 1 {
			defaultAdp = cfg.SupportedAdapters[0]
		}
		if _, exists := drivers[defaultAdp]; !exists {
			drivers[defaultAdp] = cfg.Driver
		}
	}
	if len(drivers) == 0 && cfg.AgentManager == nil {
		return nil, errors.New("at least one acp driver is required")
	}
	primaryDriver := cfg.Driver
	if primaryDriver == nil {
		if d, ok := drivers[protocol.AdapterAgy]; ok {
			primaryDriver = d
		} else {
			for _, d := range drivers {
				primaryDriver = d
				break
			}
		}
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
		cfg.RunnerVersion = "0.2.0"
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
	if cfg.DeliveryRetryInterval <= 0 {
		cfg.DeliveryRetryInterval = 2 * time.Second
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
		derived := make([]protocol.Adapter, 0, len(drivers))
		for _, a := range []protocol.Adapter{protocol.AdapterAgy, protocol.AdapterClaude, protocol.AdapterCodex} {
			if _, ok := drivers[a]; ok {
				derived = append(derived, a)
			}
		}
		for a := range drivers {
			if !slices.Contains(derived, a) {
				derived = append(derived, a)
			}
		}
		cfg.SupportedAdapters = derived
	}
	if strings.TrimSpace(cfg.HubHTTPURL) == "" && strings.TrimSpace(cfg.HubWSURL) != "" {
		cfg.HubHTTPURL = deriveHubHTTPURL(cfg.HubWSURL)
	}

	outbox, err := OpenLocalOutbox(ctx, cfg.OutboxDBPath)
	if err != nil {
		return nil, err
	}

	qc := cfg.QuotaCollector
	if qc == nil && cfg.AgentManager != nil {
		qc = NewQuotaCollector(QuotaCollectorConfig{
			ResolveMainCLI: cfg.AgentManager.ResolveMainCLIBin,
			MergedPATH:     cfg.AgentManager.MergedPATH(),
		})
	}

	r := &Runner{
		cfg:               cfg,
		instanceID:        "run_" + ulid.Make().String(),
		outbox:            outbox,
		driver:            primaryDriver,
		drivers:           drivers,
		driverBins:        map[protocol.Adapter]string{},
		driverStarted:     map[protocol.Adapter]bool{},
		supportedAdps:     append([]protocol.Adapter(nil), cfg.SupportedAdapters...),
		agentMgr:          cfg.AgentManager,
		quotaCollector:    qc,
		httpClient:        &http.Client{Timeout: 30 * time.Second},
		sessions:          map[string]*sessionSlot{},
		slotMeta:          map[string]liveSlotMeta{},
		attempts:          map[string]*attemptContext{},
		sessionOwner:      map[acp.SessionId]string{},
		delegateTokens:    map[string]*delegateTokenEntry{},
		resourceLocks:     map[string]string{},
		cancelledAttempts: map[string]bool{},
		reservedByAdapter: map[protocol.Adapter]int{},
		hbPeriod:          10 * time.Second,
		outboxWakeCh:      make(chan struct{}, 1),
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
	r.driverMu.Lock()
	defer r.driverMu.Unlock()
	return append([]protocol.Adapter(nil), r.supportedAdps...)
}

// InstanceID returns this runner process's unique UUID (changes on every restart).
func (r *Runner) InstanceID() string { return r.instanceID }

// SocketPath returns the local Unix domain socket path for `orbit deliver`.
func (r *Runner) SocketPath() string { return r.cfg.SocketPath }

// Outbox exposes the local persistent outbox (primarily for tests/diagnostics).
func (r *Runner) Outbox() *LocalOutbox { return r.outbox }

func (r *Runner) driverFor(adapter protocol.Adapter) (AcpAdapterDriver, error) {
	if adapter == "" {
		adapter = protocol.AdapterAgy
	}
	r.driverMu.Lock()
	defer r.driverMu.Unlock()
	if drv, ok := r.drivers[adapter]; ok && drv != nil {
		return drv, nil
	}
	return nil, fmt.Errorf("adapter %q is not configured or disabled on runner %s", adapter, r.cfg.MachineID)
}

func (r *Runner) ensureDriverStarted(ctx context.Context, adapter protocol.Adapter) (AcpAdapterDriver, error) {
	if adapter == "" {
		adapter = protocol.AdapterAgy
	}
	r.driverMu.Lock()
	defer r.driverMu.Unlock()
	drv, ok := r.drivers[adapter]
	if !ok || drv == nil {
		return nil, fmt.Errorf("adapter %q is not configured or disabled on runner %s", adapter, r.cfg.MachineID)
	}
	if r.driverStarted[adapter] {
		return drv, nil
	}
	startCtx := r.runCtx
	if startCtx == nil {
		startCtx = ctx
	}
	if err := drv.Start(startCtx, r.handleSessionUpdate, r.handlePermissionRequest); err != nil {
		return nil, fmt.Errorf("start %s acp driver: %w", adapter, err)
	}
	r.driverStarted[adapter] = true
	return drv, nil
}

// syncDriversFromAgentManager refreshes configured drivers and ready adapters from AgentManager probes
// without killing live running attempts; replaced/removed drivers are retired and closed once idle.
func (r *Runner) syncDriversFromAgentManager(probes []protocol.AgentProbe) {
	if r.agentMgr == nil {
		return
	}
	ready := make([]protocol.Adapter, 0, len(probes))
	r.driverMu.Lock()
	for _, p := range probes {
		if !p.Enabled || p.State != protocol.AgentStateReady {
			// Refuse future dispatches for disabled/unready adapters while leaving in-flight sessions undisturbed.
			if oldDrv := r.drivers[p.Adapter]; oldDrv != nil && r.driverStarted[p.Adapter] {
				r.retiredDrivers = append(r.retiredDrivers, oldDrv)
			}
			delete(r.drivers, p.Adapter)
			delete(r.driverBins, p.Adapter)
			delete(r.driverStarted, p.Adapter)
			continue
		}
		ready = append(ready, p.Adapter)
		if drv, sig, ok := r.agentMgr.BuildDriverFor(p.Adapter); ok {
			if existingSig, has := r.driverBins[p.Adapter]; has && existingSig == sig && r.drivers[p.Adapter] != nil {
				continue
			}
			if oldDrv := r.drivers[p.Adapter]; oldDrv != nil && r.driverStarted[p.Adapter] {
				r.retiredDrivers = append(r.retiredDrivers, oldDrv)
			}
			r.drivers[p.Adapter] = drv
			r.driverBins[p.Adapter] = sig
			delete(r.driverStarted, p.Adapter)
		}
	}
	r.supportedAdps = ready
	r.driverMu.Unlock()

	r.sweepRetiredDrivers()
}

func (r *Runner) sweepRetiredDrivers() {
	r.driverMu.Lock()
	hasRetired := len(r.retiredDrivers) > 0
	r.driverMu.Unlock()
	if !hasRetired {
		return
	}

	// Snapshot sessions & attempts under r.mu, then inspect slot/attempt locks OUTSIDE r.mu
	// to strictly preserve lock ordering (slot.mu / att.mu -> r.mu) and avoid ABBA deadlocks.
	r.mu.Lock()
	sessions := make([]*sessionSlot, 0, len(r.sessions))
	for _, s := range r.sessions {
		sessions = append(sessions, s)
	}
	attempts := make([]*attemptContext, 0, len(r.attempts))
	for _, a := range r.attempts {
		attempts = append(attempts, a)
	}
	r.mu.Unlock()

	inUse := map[AcpAdapterDriver]bool{}
	for _, s := range sessions {
		s.mu.Lock()
		if s.driver != nil && s.runtimeState != protocol.SessionDead && s.runtimeState != protocol.SessionIdleClosed {
			inUse[s.driver] = true
		}
		s.mu.Unlock()
	}
	for _, a := range attempts {
		a.mu.Lock()
		if a.driver != nil && !a.finalized {
			inUse[a.driver] = true
		}
		a.mu.Unlock()
	}

	r.driverMu.Lock()
	var toClose []AcpAdapterDriver
	keep := r.retiredDrivers[:0]
	for _, drv := range r.retiredDrivers {
		if drv == nil {
			continue
		}
		if inUse[drv] {
			keep = append(keep, drv)
		} else {
			toClose = append(toClose, drv)
		}
	}
	r.retiredDrivers = keep
	r.driverMu.Unlock()

	for _, drv := range toClose {
		cctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = drv.Close(cctx)
		cancel()
	}
}

// Start boots the primary ACP driver (or probes managed adapters lazily), opens the local Unix domain socket,
// and connects to Hub /ws/runner.
func (r *Runner) Start(ctx context.Context) error {
	r.runCtx, r.runCancel = context.WithCancel(ctx)

	if r.agentMgr != nil {
		// Probe all adapters upfront (capped ~10s) so runner.hello advertises accurate AgentProbes immediately,
		// while never failing runner startup if 0 adapters are ready yet.
		probeCtx, cancelProbe := context.WithTimeout(ctx, 15*time.Second)
		probes := r.agentMgr.ProbeAll(probeCtx)
		cancelProbe()
		r.syncDriversFromAgentManager(probes)
	} else {
		primaryAdapter := protocol.AdapterAgy
		r.driverMu.Lock()
		_, hasPrimary := r.drivers[primaryAdapter]
		if !hasPrimary && len(r.supportedAdps) > 0 {
			primaryAdapter = r.supportedAdps[0]
			_, hasPrimary = r.drivers[primaryAdapter]
		}
		r.driverMu.Unlock()
		if hasPrimary {
			if _, err := r.ensureDriverStarted(r.runCtx, primaryAdapter); err != nil {
				return err
			}
		}
	}

	if r.cfg.SocketPath != "" && r.cfg.SocketPath != "none" {
		sockSrv, err := startLocalSocketServer(r.cfg.SocketPath, r.HandleLocalDeliver, r.HandleLocalDelegate)
		if err != nil {
			_ = r.closeAllDrivers(ctx)
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

func (r *Runner) closeAllDrivers(ctx context.Context) error {
	r.driverMu.Lock()
	toClose := make([]AcpAdapterDriver, 0, len(r.driverStarted))
	seen := map[AcpAdapterDriver]bool{}
	for adp, started := range r.driverStarted {
		if !started {
			continue
		}
		if drv := r.drivers[adp]; drv != nil && !seen[drv] {
			seen[drv] = true
			toClose = append(toClose, drv)
		}
		delete(r.driverStarted, adp)
	}
	for _, drv := range r.retiredDrivers {
		if drv != nil && !seen[drv] {
			seen[drv] = true
			toClose = append(toClose, drv)
		}
	}
	r.retiredDrivers = nil
	r.driverMu.Unlock()

	// Also collect and close any project-scoped drivers owned by live or dead session slots.
	r.mu.Lock()
	slots := make([]*sessionSlot, 0, len(r.sessions))
	for _, slot := range r.sessions {
		slots = append(slots, slot)
	}
	r.mu.Unlock()
	for _, slot := range slots {
		slot.mu.Lock()
		if slot.scopedDriver && slot.driver != nil {
			if !seen[slot.driver] {
				seen[slot.driver] = true
				toClose = append(toClose, slot.driver)
			}
			slot.driver = nil
			slot.scopedDriver = false
			slot.projectEnvHash = ""
		}
		slot.mu.Unlock()
	}

	var errs []error
	for _, drv := range toClose {
		if err := drv.Close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Close gracefully terminates WS connections, active attempts, local socket, ACP subprocesses, and SQLite outbox.
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

	r.mu.Lock()
	r.delegateTokens = map[string]*delegateTokenEntry{}
	r.mu.Unlock()

	var err1, err2, err3 error
	if r.sockSrv != nil {
		err1 = r.sockSrv.Close()
	}
	err2 = r.closeAllDrivers(ctx)
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
// B10: Rejects any attemptId already cancelled locally (in-memory or persisted in Outbox cancelled_attempts).
func (r *Runner) HandleDispatch(dispatch protocol.AttemptDispatchFrame) {
	if cancelled, err := r.outbox.IsAttemptCancelled(r.runCtx, dispatch.AttemptID); err == nil && cancelled {
		return
	}
	r.mu.Lock()
	if r.cancelledAttempts[dispatch.AttemptID] {
		r.mu.Unlock()
		return
	}
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
		boardDelivered:   map[string]bool{},
		boardQueuedIDs:   map[string]bool{},
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
	// B19: Resources are released by completeInMemoryFinalize strictly AFTER durable Outbox persistence;
	// this defer only cleans up temporary worker reservations or shutdown exits when Runner itself stops.
	defer func() {
		r.releaseWorkerReservation(att)
		if r.runCtx.Err() != nil {
			r.releaseAttemptResources(att)
		}
	}()

	// B18: Reject unsupported session adapters explicitly instead of silently running agy.
	supported := r.supportedAdapters()
	if protocol.SessionAdapter(dispatch.Adapter).IsValid() && !slices.Contains(supported, dispatch.Adapter) {
		r.finalizeAttemptDurable(att, protocol.AttemptFailed, map[string]any{
			"state":       string(protocol.AttemptFailed),
			"errorCode":   "UNSUPPORTED_ADAPTER",
			"errorDetail": fmt.Sprintf("runner %s does not support adapter %q (supported: %v)", r.cfg.MachineID, dispatch.Adapter, supported),
		})
		return
	}

	// 1. Transition to preparing
	r.setAttemptState(att, protocol.AttemptPreparing, map[string]any{
		"state": string(protocol.AttemptPreparing),
	})

	// 2. Acquire explicitly configured exclusive machine resources atomically in lexicographical order
	isInPlace := dispatch.IsPlainFolder || dispatch.Isolation == protocol.IsolationInPlace
	reqResources := append([]string(nil), dispatch.RequiredResources...)
	if len(reqResources) > 0 {
		waited := false
		lastWaitingReason := ""
		for {
			if err := ctx.Err(); err != nil {
				r.finalizeAttemptDurable(att, protocol.AttemptCancelled, map[string]any{
					"state":       string(protocol.AttemptCancelled),
					"errorCode":   "CANCELLED",
					"errorDetail": "cancelled while waiting for resources",
				})
				return
			}
			ok, reason := r.tryAcquireResources(ctx, att, reqResources)
			if ok {
				break
			}
			if !waited || reason != lastWaitingReason {
				waited = true
				lastWaitingReason = reason
				r.setAttemptState(att, protocol.AttemptWaitingResource, map[string]any{
					"state":         string(protocol.AttemptWaitingResource),
					"waitingReason": reason,
				})
			}
			select {
			case <-ctx.Done():
				r.finalizeAttemptDurable(att, protocol.AttemptCancelled, map[string]any{
					"state":       string(protocol.AttemptCancelled),
					"errorCode":   "CANCELLED",
					"errorDetail": "cancelled while waiting for resources",
				})
				return
			case <-time.After(1 * time.Second):
			}
		}
		if waited {
			r.setAttemptState(att, protocol.AttemptPreparing, map[string]any{
				"state": string(protocol.AttemptPreparing),
			})
		}
	}

	// 3. Prepare isolated worktree and fixed review base SHA
	worktreeDispatch := dispatch
	var inputBundleCleanup func()
	if dispatch.InputDelivery != nil && isHubBundleMirror(dispatch.MirrorBarePath) {
		pid := hubBundleProjectID(dispatch.MirrorBarePath, dispatch.ProjectID)
		bPath, clean, bErr := r.downloadInputDeliveryBundle(ctx, pid, dispatch.InputDelivery.DeliveryID)
		if bErr != nil {
			r.finalizeAttemptDurable(att, protocol.AttemptFailed, map[string]any{
				"state":       string(protocol.AttemptFailed),
				"errorCode":   "WORKTREE_PREPARE_FAILED",
				"errorDetail": bErr.Error(),
			})
			return
		}
		inputBundleCleanup = clean
		worktreeDispatch.MirrorBarePath = bPath
	}
	worktreePath, baseSHA, err := PrepareWorktree(ctx, worktreeDispatch, r.cfg.DataDir)
	if inputBundleCleanup != nil {
		inputBundleCleanup()
	}
	if err != nil {
		r.finalizeAttemptDurable(att, protocol.AttemptFailed, map[string]any{
			"state":       string(protocol.AttemptFailed),
			"errorCode":   "WORKTREE_PREPARE_FAILED",
			"errorDetail": err.Error(),
		})
		return
	}
	var beforeSnap *gitops.FolderSnapshot
	if isInPlace {
		if snap, snapErr := gitops.CaptureFolderSnapshot(worktreePath); snapErr == nil {
			beforeSnap = snap
		}
	}
	att.mu.Lock()
	att.worktreePath = worktreePath
	att.baseSHA = baseSHA
	att.beforeFolderSnap = beforeSnap
	att.mu.Unlock()

	// 4. Per-wbSessionId single-flight serialization (prevents close vs dispatch race!)
	wbSessionID := dispatch.WBSessionID
	if wbSessionID == "" {
		wbSessionID = "ses_" + dispatch.AttemptID
	}
	// S1: Atomically enforce effective MaxLiveWorkers hard cap (per-agent setting or runner config).
	if r.effectiveMaxLiveWorkers(dispatch.Adapter) > 0 {
		if err := r.reserveLiveWorkerSlot(ctx, att, wbSessionID); err != nil {
			r.finalizeAttemptDurable(att, protocol.AttemptCancelled, map[string]any{
				"state":       string(protocol.AttemptCancelled),
				"errorCode":   "CANCELLED",
				"errorDetail": "cancelled while waiting for available worker slot",
			})
			return
		}
	}
	slot := r.getOrCreateSessionSlot(wbSessionID)

	slot.mu.Lock()
	sid, promptText, err := r.ensureSessionReadyLocked(ctx, att, slot)
	if err != nil {
		slot.runtimeState = protocol.SessionDead
		slot.orbitMCPAttached = false
		r.revokeSessionDelegateTokenLocked(slot)
		slot.mu.Unlock()
		r.releaseWorkerReservation(att)
		errCode := "SESSION_PREPARE_FAILED"
		if strings.Contains(err.Error(), "CONFIG_APPLY_FAILED") {
			errCode = "CONFIG_APPLY_FAILED"
		}
		r.finalizeAttemptDurable(att, protocol.AttemptFailed, map[string]any{
			"state":        string(protocol.AttemptFailed),
			"worktreePath": worktreePath,
			"baseSha":      baseSHA,
			"errorCode":    errCode,
			"errorDetail":  err.Error(),
		})
		return
	}

	slot.adapter = dispatch.Adapter
	if slot.adapter == "" {
		slot.adapter = protocol.AdapterAgy
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
	att.inFlightPrompts++
	att.promptReturned = false
	att.mu.Unlock()

	r.mu.Lock()
	r.sessionOwner[sid] = dispatch.AttemptID
	r.slotMeta[wbSessionID] = liveSlotMeta{
		adapter:    slot.adapter,
		live:       true,
		inFlight:   true,
		locked:     slot.locked,
		lastActive: slot.lastActivityAt,
	}
	r.mu.Unlock()
	slot.mu.Unlock()
	// Session is now live and counted under slotMeta -> release pre-creation reservation
	r.releaseWorkerReservation(att)

	// 5. Enter running state and send ACP session/prompt
	r.setAttemptState(att, protocol.AttemptRunning, map[string]any{
		"state":             string(protocol.AttemptRunning),
		"providerSessionId": string(sid),
		"worktreePath":      worktreePath,
		"baseSha":           baseSHA,
	})

	att.mu.Lock()
	drv := att.driver
	att.mu.Unlock()
	var drvErr error
	if drv == nil {
		drv, drvErr = r.driverFor(dispatch.Adapter)
	}
	var promptResp acp.PromptResponse
	var promptErr error
	if drvErr != nil {
		promptErr = drvErr
	} else {
		promptResp, promptErr = drv.Prompt(ctx, sid, promptText)
	}

	// B2: Only clear slot.inFlightPrompt if THIS attempt still owns the in-flight prompt!
	slot.mu.Lock()
	if slot.promptOwnerAttemptID == dispatch.AttemptID {
		slot.inFlightPrompt = false
		slot.promptOwnerAttemptID = ""
	}
	slot.lastActivityAt = time.Now()
	r.syncSlotMetaLocked(slot)
	slot.mu.Unlock()

	att.mu.Lock()
	alreadyFinal := att.finalized
	if att.inFlightPrompts > 0 {
		att.inFlightPrompts--
	}
	att.promptReturned = (att.inFlightPrompts == 0)
	att.lastStopReason = promptResp.StopReason
	finalReq := att.finalRequested
	explicitDlvID := att.explicitFinalDeliveryID
	hasDelivery := att.hasDelivery
	att.mu.Unlock()
	att.promptDoneOnce.Do(func() { close(att.promptDoneCh) })

	if alreadyFinal {
		return
	}

	// B1: ALWAYS adjudicate promptErr and ACP StopReason FIRST — an earlier in-flight `orbit deliver --final`
	// MUST NEVER promote an errored, cancelled, refused, or truncated prompt into `succeeded`!
	if promptErr != nil {
		r.coalescer.FlushAttempt(dispatch.AttemptID)
		if errors.Is(ctx.Err(), context.Canceled) {
			sessionLocked := false
			if att.tracker.HasActiveWorkers() {
				r.isolateAndCloseSession(slot, sid)
				sessionLocked = true
			}
			r.finalizeAttemptDurable(att, protocol.AttemptCancelled, map[string]any{
				"state":         string(protocol.AttemptCancelled),
				"sessionLocked": sessionLocked,
				"errorCode":     "CANCELLED",
				"errorDetail":   "attempt cancelled",
			})
			return
		}
		r.finalizeAttemptDurable(att, protocol.AttemptFailed, map[string]any{
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
		r.finalizeAttemptDurable(att, protocol.AttemptCancelled, map[string]any{
			"state":         string(protocol.AttemptCancelled),
			"stopReason":    string(promptResp.StopReason),
			"sessionLocked": sessionLocked,
			"errorCode":     "CANCELLED",
			"errorDetail":   "acp stopReason=cancelled",
		})
		return

	case acp.StopReasonRefusal, acp.StopReasonMaxTokens, acp.StopReasonMaxTurnRequests:
		r.coalescer.FlushAttempt(dispatch.AttemptID)
		r.finalizeAttemptDurable(att, protocol.AttemptFailed, map[string]any{
			"state":       string(protocol.AttemptFailed),
			"stopReason":  string(promptResp.StopReason),
			"errorCode":   "ABNORMAL_STOP_REASON",
			"errorDetail": fmt.Sprintf("agent stopped with %s", promptResp.StopReason),
		})
		return

	case acp.StopReasonEndTurn, "":
		// Proceed with explicit final delivery / swarm monitoring / normal completion checks below.

	default:
		r.coalescer.FlushAttempt(dispatch.AttemptID)
		r.finalizeAttemptDurable(att, protocol.AttemptFailed, map[string]any{
			"state":       string(protocol.AttemptFailed),
			"stopReason":  string(promptResp.StopReason),
			"errorCode":   "UNKNOWN_STOP_REASON",
			"errorDetail": fmt.Sprintf("unexpected stopReason %q", promptResp.StopReason),
		})
		return
	}

	// B2: If `orbit deliver --final` ran during the in-flight Prompt() AND the prompt ended cleanly (end_turn),
	// finalize now after Prompt() has released exclusive sessionOwner ownership.
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
			"stopReason":                  string(acp.StopReasonEndTurn),
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
		r.finalizeAttemptDurable(att, protocol.AttemptSucceeded, payload)
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

	// Orbit infra auto-wrapup: for isolated Git worktree turns, automatically package and publish
	// a checkpoint delivery whenever files changed vs baseSHA — including follow-up edits made
	// AFTER an earlier mid-turn `orbit deliver`. Never treat worktree check failures as clean!
	autoDeliveryID := ""
	if !protocol.IsVirtualProjectID(dispatch.ProjectID) && !isInPlace && strings.TrimSpace(dispatch.MirrorBarePath) != "" {
		if hasPendingFinal, err := r.outbox.HasUnemittedFinalDelivery(ctx, dispatch.AttemptID); err == nil && hasPendingFinal {
			go r.retryPendingDeliveries(r.runCtx)
			<-ctx.Done()
			return
		}
		att.mu.Lock()
		lastDeliveredFP := att.lastDeliveredFingerprint
		att.mu.Unlock()

		var (
			changed bool
			chkErr  error
		)
		if !hasDelivery {
			changed, chkErr = worktreeHasChanges(ctx, worktreePath, baseSHA)
		} else {
			curFP, fpErr := worktreeStateFingerprint(ctx, worktreePath)
			if fpErr != nil {
				chkErr = fpErr
			} else {
				changed = (lastDeliveredFP == "" || curFP != lastDeliveredFP)
			}
		}
		if chkErr != nil {
			r.finalizeAttemptDurable(att, protocol.AttemptFailed, map[string]any{
				"state":                       string(protocol.AttemptFailed),
				"stopReason":                  string(acp.StopReasonEndTurn),
				"errorCode":                   "WORKTREE_CHECK_FAILED",
				"errorDetail":                 fmt.Sprintf("检查工作树状态失败: %v", chkErr),
				"rejectedToolCount":           rejCount,
				"nonZeroExitCount":            nonZeroCount,
				"unresolvedRejectedToolCount": unresRej,
				"unresolvedNonZeroExitCount":  unresNonZero,
			})
			return
		}
		if changed {
			termState := protocol.AttemptSucceeded
			termErrCode := ""
			termErrDetail := ""
			var finalPayload map[string]any
			if unresRej > 0 {
				termState = protocol.AttemptNeedsVerification
				termErrCode = "UNRESOLVED_TOOL_FAILURE"
				termErrDetail = "本轮存在未解决的工具审批拒绝，请人工核对后再决定是否续跑"
				finalPayload = map[string]any{
					"state":                       string(protocol.AttemptNeedsVerification),
					"stopReason":                  string(acp.StopReasonEndTurn),
					"completionSource":            string(protocol.CompletionPromptEndTurn),
					"sessionLocked":               true,
					"errorCode":                   termErrCode,
					"errorDetail":                 termErrDetail,
					"rejectedToolCount":           rejCount,
					"nonZeroExitCount":            nonZeroCount,
					"unresolvedRejectedToolCount": unresRej,
					"unresolvedNonZeroExitCount":  unresNonZero,
				}
			} else {
				finalPayload = map[string]any{
					"state":                       string(protocol.AttemptSucceeded),
					"stopReason":                  string(acp.StopReasonEndTurn),
					"completionSource":            string(protocol.CompletionPromptEndTurn),
					"rejectedToolCount":           rejCount,
					"nonZeroExitCount":            nonZeroCount,
					"unresolvedRejectedToolCount": unresRej,
					"unresolvedNonZeroExitCount":  unresNonZero,
				}
				if promptResp.Usage != nil {
					finalPayload["tokenUsage"] = map[string]any{
						"input":      promptResp.Usage.InputTokens,
						"output":     promptResp.Usage.OutputTokens,
						"cachedRead": promptResp.Usage.CachedReadTokens,
						"thought":    promptResp.Usage.ThoughtTokens,
					}
				}
			}
			dlvID, pendingRetry, finalizedAtomically, dlvErr := r.autoDeliverWorktree(ctx, att, slot, sid, hasDelivery, termState, termErrCode, termErrDetail, finalPayload)
			if dlvErr != nil {
				r.finalizeAttemptDurable(att, protocol.AttemptFailed, map[string]any{
					"state":                       string(protocol.AttemptFailed),
					"stopReason":                  string(acp.StopReasonEndTurn),
					"errorCode":                   "AUTO_DELIVERY_FAILED",
					"errorDetail":                 fmt.Sprintf("自动打包工作树变更失败: %v", dlvErr),
					"rejectedToolCount":           rejCount,
					"nonZeroExitCount":            nonZeroCount,
					"unresolvedRejectedToolCount": unresRej,
					"unresolvedNonZeroExitCount":  unresNonZero,
				})
				return
			}
			if pendingRetry {
				// Transient network failure after checkpoint was durably saved to pending_deliveries:
				// wait for retryPendingDeliveries to publish the delivery FIRST and then emit the
				// persisted terminal state (succeeded OR needs_verification) atomically.
				go r.retryPendingDeliveries(r.runCtx)
				<-ctx.Done()
				return
			}
			if finalizedAtomically {
				return
			}
			autoDeliveryID = dlvID
		}
	}
	if unresRej > 0 {
		r.isolateAndCloseSession(slot, sid)
		nvPayload := map[string]any{
			"state":                       string(protocol.AttemptNeedsVerification),
			"stopReason":                  string(acp.StopReasonEndTurn),
			"completionSource":            string(protocol.CompletionPromptEndTurn),
			"sessionLocked":               true,
			"errorCode":                   "UNRESOLVED_TOOL_FAILURE",
			"errorDetail":                 "本轮存在未解决的工具审批拒绝，请人工核对后再决定是否续跑",
			"rejectedToolCount":           rejCount,
			"nonZeroExitCount":            nonZeroCount,
			"unresolvedRejectedToolCount": unresRej,
			"unresolvedNonZeroExitCount":  unresNonZero,
		}
		if autoDeliveryID != "" {
			nvPayload["deliveryId"] = autoDeliveryID
		}
		r.finalizeAttemptDurable(att, protocol.AttemptNeedsVerification, nvPayload)
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
	if autoDeliveryID != "" {
		payload["deliveryId"] = autoDeliveryID
	}
	if isInPlace {
		att.mu.Lock()
		bSnap := att.beforeFolderSnap
		wt := att.worktreePath
		att.mu.Unlock()
		if afterSnap, snapErr := gitops.CaptureFolderSnapshot(wt); snapErr == nil {
			fc := gitops.DiffFolderSnapshots(bSnap, afterSnap)
			if len(fc.Files) > 0 {
				payload["folderChanges"] = fc
			}
		}
	}
	if promptResp.Usage != nil {
		payload["tokenUsage"] = map[string]any{
			"input":      promptResp.Usage.InputTokens,
			"output":     promptResp.Usage.OutputTokens,
			"cachedRead": promptResp.Usage.CachedReadTokens,
			"thought":    promptResp.Usage.ThoughtTokens,
		}
	}
	r.finalizeAttemptDurable(att, protocol.AttemptSucceeded, payload)
}

func (r *Runner) syncSlotMetaLocked(slot *sessionSlot) {
	if slot == nil || slot.wbSessionID == "" {
		return
	}
	meta := liveSlotMeta{
		adapter:    slot.adapter,
		live:       slot.runtimeState == protocol.SessionLive && slot.providerSessionID != "",
		inFlight:   slot.inFlightPrompt,
		locked:     slot.locked,
		lastActive: slot.lastActivityAt,
	}
	r.mu.Lock()
	if meta.live {
		r.slotMeta[slot.wbSessionID] = meta
	} else {
		delete(r.slotMeta, slot.wbSessionID)
	}
	r.mu.Unlock()
}

func (r *Runner) isolateAndCloseSession(slot *sessionSlot, sid acp.SessionId) {
	var adapter protocol.Adapter
	var pinnedDrv AcpAdapterDriver
	var isScoped bool
	if slot != nil {
		slot.mu.Lock()
		slot.locked = true
		slot.runtimeState = protocol.SessionDead
		slot.orbitMCPAttached = false
		r.revokeSessionDelegateTokenLocked(slot)
		adapter = slot.adapter
		pinnedDrv = slot.driver
		isScoped = slot.scopedDriver
		slot.driver = nil
		slot.scopedDriver = false
		slot.projectEnvHash = ""
		slot.sessionOpts = nil
		slot.sessionModes = nil
		r.revokeSessionDelegateTokenLocked(slot)
		if sid == "" {
			sid = slot.providerSessionID
		}
		r.syncSlotMetaLocked(slot)
		slot.mu.Unlock()
	}
	if sid != "" {
		drv := pinnedDrv
		if drv == nil {
			drv, _ = r.driverFor(adapter)
		}
		if drv != nil {
			cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = drv.CloseSession(cctx, sid)
			cancel()
		}
	}
	if isScoped && pinnedDrv != nil {
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = pinnedDrv.Close(cctx)
		cancel()
	}
	r.sweepRetiredDrivers()
}

func (r *Runner) releaseWorkerReservation(att *attemptContext) {
	if att == nil {
		return
	}
	att.mu.Lock()
	had := att.reservedWorker
	adp := att.reservedWorkerAdapter
	att.reservedWorker = false
	att.reservedWorkerAdapter = ""
	att.mu.Unlock()
	if !had {
		return
	}
	r.mu.Lock()
	if r.reservedWorkers > 0 {
		r.reservedWorkers--
	}
	if adp != "" && r.reservedByAdapter[adp] > 0 {
		r.reservedByAdapter[adp]--
	}
	r.mu.Unlock()
}

func (r *Runner) effectiveMaxLiveWorkers(adapter protocol.Adapter) int {
	globalCap, adapterCap := r.workerCaps(adapter)
	if adapterCap > 0 && (globalCap <= 0 || adapterCap < globalCap) {
		return adapterCap
	}
	return globalCap
}

func (r *Runner) workerCaps(adapter protocol.Adapter) (globalCap int, adapterCap int) {
	if adapter == "" {
		adapter = protocol.AdapterAgy
	}
	globalCap = r.cfg.MaxLiveWorkers
	if r.agentMgr != nil {
		adapterCap = r.agentMgr.EffectiveMaxWorkers(adapter)
	}
	return globalCap, adapterCap
}

// reserveLiveWorkerSlot atomically enforces BOTH per-adapter and global MaxLiveWorkers caps (S1)
// using r.slotMeta under r.mu ONLY (zero r.mu -> slot.mu nesting, eliminating ABBA deadlocks!).
func (r *Runner) reserveLiveWorkerSlot(ctx context.Context, att *attemptContext, wbSessionID string) error {
	targetAdp := att.dispatch.Adapter
	if targetAdp == "" {
		targetAdp = protocol.AdapterAgy
	}
	globalCap, adapterCap := r.workerCaps(targetAdp)
	if globalCap <= 0 && adapterCap <= 0 {
		return nil
	}
	type cand struct {
		wbID       string
		lastActive time.Time
	}
	notifiedWait := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		globalCap, adapterCap = r.workerCaps(targetAdp)
		if globalCap <= 0 && adapterCap <= 0 {
			return nil
		}

		r.mu.Lock()
		if meta, ok := r.slotMeta[wbSessionID]; ok && meta.live && !meta.locked && (meta.adapter == "" || meta.adapter == targetAdp) {
			r.mu.Unlock()
			return nil
		}

		totalLive := 0
		adapterLive := 0
		var allIdle []cand
		var adapterIdle []cand
		for wbID, meta := range r.slotMeta {
			if !meta.live {
				continue
			}
			totalLive++
			if meta.adapter == targetAdp {
				adapterLive++
			}
			if wbID != wbSessionID && !meta.inFlight {
				c := cand{wbID: wbID, lastActive: meta.lastActive}
				allIdle = append(allIdle, c)
				if meta.adapter == targetAdp {
					adapterIdle = append(adapterIdle, c)
				}
			}
		}

		adapterOK := adapterCap <= 0 || (adapterLive+r.reservedByAdapter[targetAdp] < adapterCap)
		globalOK := globalCap <= 0 || (totalLive+r.reservedWorkers < globalCap)
		if adapterOK && globalOK {
			r.reservedWorkers++
			r.reservedByAdapter[targetAdp]++
			r.mu.Unlock()
			att.mu.Lock()
			att.reservedWorker = true
			att.reservedWorkerAdapter = targetAdp
			att.mu.Unlock()
			if notifiedWait {
				r.setAttemptState(att, protocol.AttemptPreparing, map[string]any{
					"state": string(protocol.AttemptPreparing),
				})
			}
			return nil
		}
		r.mu.Unlock()

		// Evict only matching-adapter idle slots if per-adapter cap is full; evict any idle slot if global cap is full.
		evictPool := allIdle
		waitCap := globalCap
		if !adapterOK {
			evictPool = adapterIdle
			waitCap = adapterCap
		}
		evicted := false
		if len(evictPool) > 0 {
			sort.Slice(evictPool, func(i, j int) bool {
				return evictPool[i].lastActive.Before(evictPool[j].lastActive)
			})
			for _, c := range evictPool {
				if closed, _ := r.CloseSessionSafely(ctx, c.wbID); closed {
					evicted = true
					break
				}
			}
		}
		if evicted {
			continue
		}

		if !notifiedWait {
			notifiedWait = true
			r.setAttemptState(att, protocol.AttemptWaitingResource, map[string]any{
				"state":         string(protocol.AttemptWaitingResource),
				"waitingReason": fmt.Sprintf("等待可用 Worker 槽位（上限 %d）", waitCap),
			})
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// ensureSessionReadyLocked implements Spike Boundary #2 verbatim:
// - Hard check: reuse requires same machine, same adapter, unlocked slot, and expectedSessionCwd == worktreePath.
// - On cold restore: ResumeSession (0 replay) -> LoadSession (swallow replay) -> NewSession.
// - IRON LAW: Always calls SetConfigOption per desired option with 10s timeout and verifies currentValue!
func (r *Runner) ensureSessionReadyLocked(ctx context.Context, att *attemptContext, slot *sessionSlot) (acp.SessionId, string, error) {
	d := att.dispatch
	adapter := d.Adapter
	if adapter == "" {
		adapter = protocol.AdapterAgy
	}
	att.mu.Lock()
	worktreePath := att.worktreePath
	att.mu.Unlock()

	expectedSID := acp.SessionId(d.ProviderSessionID)
	expectedCWD := d.ExpectedSessionCWD
	if expectedSID == "" && slot.providerSessionID != "" {
		expectedSID = slot.providerSessionID
		expectedCWD = slot.cwd
	}

	desiredEnvHash := hashProjectEnv(d.ProjectEnv)
	sameAdapter := slot.adapter == "" || slot.adapter == adapter
	canReuse := !slot.locked &&
		sameAdapter &&
		expectedSID != "" &&
		expectedCWD != "" &&
		filepath.Clean(expectedCWD) == filepath.Clean(worktreePath)

	envChanged := slot.driver != nil && slot.projectEnvHash != desiredEnvHash
	adapterChanged := slot.driver != nil && slot.adapter != "" && slot.adapter != adapter
	needReplaceScoped := slot.scopedDriver && slot.driver != nil && (!canReuse || envChanged)
	if envChanged || adapterChanged || needReplaceScoped {
		oldDrv := slot.driver
		oldScoped := slot.scopedDriver
		oldSID := slot.providerSessionID
		slot.driver = nil
		slot.scopedDriver = false
		slot.projectEnvHash = ""
		slot.providerSessionID = ""
		slot.sessionOpts = nil
		slot.sessionModes = nil
		r.revokeSessionDelegateTokenLocked(slot)
		if oldSID != "" && oldDrv != nil {
			cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = oldDrv.CloseSession(cctx, oldSID)
			cancel()
		}
		if oldScoped && oldDrv != nil {
			cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = oldDrv.Close(cctx)
			cancel()
		}
	}

	var drv AcpAdapterDriver
	var err error
	createdScoped := false
	if canReuse && !envChanged && slot.driver != nil && slot.scopedDriver {
		drv = slot.driver
	} else if len(d.ProjectEnv) > 0 && (r.cfg.ScopedDriverFactory != nil || r.agentMgr != nil) {
		var scopedDrv AcpAdapterDriver
		if r.cfg.ScopedDriverFactory != nil {
			scopedDrv, err = r.cfg.ScopedDriverFactory(adapter, d.ProjectEnv)
			if err != nil {
				return "", "", fmt.Errorf("build %s acp driver with project env: %w", adapter, err)
			}
		} else {
			var ok bool
			scopedDrv, _, ok = r.agentMgr.BuildDriverWithEnvFor(adapter, d.ProjectEnv)
			if !ok || scopedDrv == nil {
				return "", "", fmt.Errorf("adapter %q is not configured or disabled on runner %s", adapter, r.cfg.MachineID)
			}
		}
		startCtx := r.runCtx
		if startCtx == nil {
			startCtx = ctx
		}
		if err := scopedDrv.Start(startCtx, r.handleSessionUpdate, r.handlePermissionRequest); err != nil {
			cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = scopedDrv.Close(cctx)
			cancel()
			return "", "", fmt.Errorf("start %s acp driver with project env: %w", adapter, err)
		}
		drv = scopedDrv
		slot.scopedDriver = true
		createdScoped = true
	} else {
		drv, err = r.ensureDriverStarted(ctx, adapter)
		if err != nil {
			return "", "", err
		}
		if canReuse && !envChanged && slot.driver != nil && slot.driver == drv {
			drv = slot.driver
		}
		slot.scopedDriver = false
	}
	slot.driver = drv
	slot.projectEnvHash = desiredEnvHash
	att.mu.Lock()
	att.driver = drv
	att.mu.Unlock()

	cleanupNewScoped := func() {
		if !createdScoped || drv == nil {
			return
		}
		slot.driver = nil
		slot.scopedDriver = false
		slot.projectEnvHash = ""
		slot.sessionOpts = nil
		slot.sessionModes = nil
		att.mu.Lock()
		if att.driver == drv {
			att.driver = nil
		}
		att.mu.Unlock()
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = drv.Close(cctx)
		cancel()
	}

	promptText := d.Prompt
	prependContract := false

	reusingLiveInMemory := canReuse && !envChanged && drv.IsSessionLiveInMemory(expectedSID)
	var (
		delegateToken string
		mcpServerName string
		mcpServers    []acp.McpServer
		mcpAttached   bool
	)
	// prepareFreshSessionMCP mints a brand-new delegateToken and per-session MCP server name
	// for a single ACP session open/resume/load attempt, after revoking any prior token.
	prepareFreshSessionMCP := func() context.Context {
		r.revokeSessionDelegateTokenLocked(slot)
		delegateToken = r.ensureSessionDelegateTokenLocked(slot, d)
		mcpServerName = generateOrbitMCPServerName()
		mcpServers = r.buildOrbitMCPServers(d, delegateToken, mcpServerName)
		mcpAttached = false
		return WithSessionMCPAttachReport(WithSessionMCPServers(ctx, mcpServers), &mcpAttached)
	}

	var sid acp.SessionId
	sessionOpts := slot.sessionOpts
	sessionModes := slot.sessionModes
	openedSession := false
	switch {
	case !canReuse:
		openedSession = true
		sysAppend := d.SystemPromptAppend
		if !drv.SupportsMetaSystemPrompt() && sysAppend != "" {
			prependContract = true
			sysAppend = ""
		}
		sessionCtx := prepareFreshSessionMCP()
		resp, err := drv.NewSession(sessionCtx, worktreePath, sysAppend)
		if err != nil {
			r.revokeSessionDelegateTokenLocked(slot)
			cleanupNewScoped()
			return "", "", fmt.Errorf("new session (%s): %w", adapter, err)
		}
		sid = resp.SessionId
		sessionOpts = resp.ConfigOptions
		sessionModes = resp.Modes

	case !reusingLiveInMemory:
		openedSession = true
		resumed := false
		if drv.SupportsResume() {
			sessionCtx := prepareFreshSessionMCP()
			if resp, err := drv.ResumeSession(sessionCtx, expectedSID, worktreePath); err == nil {
				sid = expectedSID
				sessionOpts = resp.ConfigOptions
				sessionModes = resp.Modes
				resumed = true
			} else {
				r.revokeSessionDelegateTokenLocked(slot)
			}
		}
		if !resumed && drv.SupportsLoad() {
			sessionCtx := prepareFreshSessionMCP()
			if resp, err := drv.LoadSession(sessionCtx, expectedSID, worktreePath); err == nil {
				sid = expectedSID
				sessionOpts = resp.ConfigOptions
				sessionModes = resp.Modes
				resumed = true
			} else {
				r.revokeSessionDelegateTokenLocked(slot)
			}
		}
		if !resumed {
			sysAppend := d.SystemPromptAppend
			if !drv.SupportsMetaSystemPrompt() && sysAppend != "" {
				prependContract = true
				sysAppend = ""
			}
			sessionCtx := prepareFreshSessionMCP()
			resp, err := drv.NewSession(sessionCtx, worktreePath, sysAppend)
			if err != nil {
				r.revokeSessionDelegateTokenLocked(slot)
				cleanupNewScoped()
				return "", "", fmt.Errorf("fallback new session (%s): %w", adapter, err)
			}
			sid = resp.SessionId
			sessionOpts = resp.ConfigOptions
			sessionModes = resp.Modes
		}

	default:
		if slot.orbitMCPAttached {
			delegateToken = r.ensureSessionDelegateTokenLocked(slot, d)
		} else {
			r.revokeSessionDelegateTokenLocked(slot)
		}
		sid = expectedSID
	}
	if openedSession {
		if len(mcpServers) > 0 && mcpAttached {
			slot.orbitMCPAttached = true
			slot.orbitMCPServerName = mcpServerName
		} else {
			// Driver either did not attach Orbit MCP or fell back internally to empty McpServers:
			// immediately revoke the delegate token handed to the failed initial request.
			r.revokeSessionDelegateTokenLocked(slot)
		}
	}
	slot.sessionOpts = sessionOpts
	slot.sessionModes = sessionModes

	// IRON LAW: Re-apply and strictly verify every desired config option before sending prompt
	updatedOpts, appliedMode, err := r.applyAndVerifyConfig(ctx, drv, sid, d.DesiredConfig, sessionOpts, sessionModes)
	if err != nil {
		// S1: Close the leaked ACP session (and any newly created project-scoped driver) on config failure,
		// and unconditionally revoke the session delegate token.
		slot.orbitMCPAttached = false
		r.revokeSessionDelegateTokenLocked(slot)
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = drv.CloseSession(cctx, sid)
		cancel()
		cleanupNewScoped()
		return "", "", fmt.Errorf("CONFIG_APPLY_FAILED: %w", err)
	}
	if len(updatedOpts) > 0 {
		slot.sessionOpts = updatedOpts
	}
	slot.appliedMode = appliedMode
	att.mu.Lock()
	att.appliedMode = appliedMode
	att.mu.Unlock()

	if prependContract && strings.TrimSpace(d.SystemPromptAppend) != "" {
		promptText = "<orbit_contract>\n" + d.SystemPromptAppend + "\n</orbit_contract>\n\n" + promptText
	}
	return sid, promptText, nil
}

func hashProjectEnv(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		if tk := strings.TrimSpace(k); tk != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		tk := strings.TrimSpace(k)
		v := env[k]
		_, _ = fmt.Fprintf(h, "%d:%s=%d:%s\n", len(tk), tk, len(v), v)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func currentSessionMode(opts []acp.SessionConfigOption, modes *acp.SessionModeState) string {
	for _, opt := range opts {
		if opt.Select == nil {
			continue
		}
		cat := ""
		if opt.Select.Category != nil {
			cat = configCategoryString(*opt.Select.Category)
		}
		if cat == "mode" || string(opt.Select.Id) == "mode" {
			return strings.TrimSpace(string(opt.Select.CurrentValue))
		}
	}
	if modes != nil && strings.TrimSpace(string(modes.CurrentModeId)) != "" {
		return strings.TrimSpace(string(modes.CurrentModeId))
	}
	return ""
}

func (r *Runner) applyAndVerifyConfig(
	ctx context.Context,
	drv AcpAdapterDriver,
	sid acp.SessionId,
	desired map[string]string,
	sessionOpts []acp.SessionConfigOption,
	sessionModes *acp.SessionModeState,
) ([]acp.SessionConfigOption, string, error) {
	appliedMode := currentSessionMode(sessionOpts, sessionModes)
	if len(desired) == 0 {
		return sessionOpts, appliedMode, nil
	}
	keys := make([]string, 0, len(desired))
	for k, v := range desired {
		if strings.TrimSpace(k) != "" && strings.TrimSpace(v) != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	currentOpts := sessionOpts
	for _, k := range keys {
		want := strings.TrimSpace(desired[k])
		matched, resolvedID, resolvedVal := resolveConfigOptionTarget(currentOpts, k, want)

		// Legacy ACP native Modes fallback (session/set_mode) when mode is not advertised in configOptions
		if matched == nil && k == "mode" && sessionModes != nil && len(sessionModes.AvailableModes) > 0 {
			modeID, ok := resolveLegacySessionMode(sessionModes.AvailableModes, want)
			if !ok {
				return currentOpts, appliedMode, fmt.Errorf("mode %q is not supported by session", want)
			}
			optCtx, cancel := context.WithTimeout(ctx, r.cfg.ConfigApplyTimeout)
			_, err := drv.SetSessionMode(optCtx, sid, modeID)
			cancel()
			if err != nil {
				return currentOpts, appliedMode, fmt.Errorf("set session mode %q (resolved %q) failed: %w", want, modeID, err)
			}
			sessionModes.CurrentModeId = acp.SessionModeId(modeID)
			appliedMode = modeID
			continue
		}

		// If the session explicitly advertised configOptions (or legacy modes) and this explicit key doesn't exist, fail clearly instead of silently swallowing.
		if matched == nil && (len(currentOpts) > 0 || (k == "mode" && sessionModes != nil)) {
			return currentOpts, appliedMode, fmt.Errorf("config option %q=%q is not supported by session", k, want)
		}

		optCtx, cancel := context.WithTimeout(ctx, r.cfg.ConfigApplyTimeout)
		resp, err := drv.SetConfigOption(optCtx, sid, resolvedID, resolvedVal)
		cancel()
		if err != nil {
			return currentOpts, appliedMode, fmt.Errorf("configId %q (resolved %q=%q) failed: %w", k, resolvedID, resolvedVal, err)
		}
		if len(resp.ConfigOptions) > 0 {
			currentOpts = resp.ConfigOptions
		}
		if !verifyConfigOptionApplied(resp.ConfigOptions, resolvedID, resolvedVal) {
			return currentOpts, appliedMode, fmt.Errorf("configId %q (resolved %q) verification mismatch (expected %q)", k, resolvedID, resolvedVal)
		}
		isModeOpt := k == "mode" || resolvedID == "mode" || (matched != nil && matched.Category != nil && configCategoryString(*matched.Category) == "mode")
		if isModeOpt {
			appliedMode = resolvedVal
		}
	}
	if cur := currentSessionMode(currentOpts, sessionModes); cur != "" {
		appliedMode = cur
	}
	return currentOpts, appliedMode, nil
}

func resolveLegacySessionMode(avail []acp.SessionMode, want string) (string, bool) {
	if len(avail) == 0 {
		return "", false
	}
	ids := make([]string, 0, len(avail))
	for _, m := range avail {
		id := string(m.Id)
		ids = append(ids, id)
		if id == want {
			return id, true
		}
	}
	for _, cand := range normalizedModeCandidates(want) {
		if slices.Contains(ids, cand) {
			return cand, true
		}
	}
	return "", false
}

// resolveConfigOptionTarget maps logical Orbit config keys/values (e.g. thought_level, mode=auto/ask/full)
// onto the active ACP server's advertised configOptions (e.g. Claude's `effort` / `bypassPermissions`,
// Codex's `reasoning_effort` / `agent` / `agent-full-access`, Agy's `thought_level` / `accept-edits`).
func resolveConfigOptionTarget(opts []acp.SessionConfigOption, desiredKey, desiredVal string) (*acp.SessionConfigOptionSelect, string, string) {
	if len(opts) == 0 {
		return nil, desiredKey, desiredVal
	}
	var matched *acp.SessionConfigOptionSelect
	// 1. Exact ID match first
	for _, o := range opts {
		if o.Select != nil && string(o.Select.Id) == desiredKey {
			matched = o.Select
			break
		}
	}
	// 2. Category match fallback (e.g. Orbit "thought_level" or "effort" -> category "thought_level")
	if matched == nil {
		wantCat := desiredKey
		if wantCat == "effort" || wantCat == "reasoning_effort" {
			wantCat = "thought_level"
		}
		for _, o := range opts {
			if o.Select != nil && o.Select.Category != nil && configCategoryString(*o.Select.Category) == wantCat {
				matched = o.Select
				break
			}
		}
	}
	// 3. Alias ID match when ACP adapter omits category (must mirror extractSessionCapabilities)
	if matched == nil && (desiredKey == "thought_level" || desiredKey == "effort" || desiredKey == "reasoning_effort") {
		for _, o := range opts {
			if o.Select == nil {
				continue
			}
			id := string(o.Select.Id)
			if id == "thought_level" || id == "effort" || id == "reasoning_effort" {
				matched = o.Select
				break
			}
		}
	}
	if matched == nil {
		return nil, desiredKey, desiredVal
	}

	allowed := collectSelectOptionValues(matched.Options)
	if len(allowed) == 0 || slices.Contains(allowed, desiredVal) {
		return matched, string(matched.Id), desiredVal
	}

	// Normalized cross-adapter alias mapping ONLY when the option explicitly advertises the mapped value
	cat := ""
	if matched.Category != nil {
		cat = configCategoryString(*matched.Category)
	}
	if cat == "mode" || string(matched.Id) == "mode" {
		for _, cand := range normalizedModeCandidates(desiredVal) {
			if slices.Contains(allowed, cand) {
				return matched, string(matched.Id), cand
			}
		}
	}
	return matched, string(matched.Id), desiredVal
}

func configCategoryString(cat acp.SessionConfigOptionCategory) string {
	return string(cat)
}

func collectSelectOptionValues(opts acp.SessionConfigSelectOptions) []string {
	var out []string
	if opts.Ungrouped != nil {
		for _, v := range *opts.Ungrouped {
			out = append(out, string(v.Value))
		}
	}
	if opts.Grouped != nil {
		for _, g := range *opts.Grouped {
			for _, v := range g.Options {
				out = append(out, string(v.Value))
			}
		}
	}
	return out
}

func normalizedModeCandidates(raw string) []string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "ask", "default":
		return []string{"default", "workspace-write", "read-only"}
	case "auto":
		return []string{"auto", "agent", "acceptEdits", "accept-edits"}
	case "full", "bypass", "bypasspermissions", "agent-full-access":
		return []string{"bypassPermissions", "agent-full-access", "auto"}
	case "accept-edits", "acceptedits":
		return []string{"accept-edits", "acceptEdits", "workspace-write"}
	case "plan":
		return []string{"plan", "read-only"}
	default:
		return nil
	}
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

	if slot.inFlightPrompt {
		return false, nil
	}
	if slot.providerSessionID == "" || slot.runtimeState == protocol.SessionIdleClosed || slot.runtimeState == protocol.SessionDead {
		slot.orbitMCPAttached = false
		r.revokeSessionDelegateTokenLocked(slot)
		if slot.scopedDriver && slot.driver != nil {
			drv := slot.driver
			slot.driver = nil
			slot.scopedDriver = false
			slot.projectEnvHash = ""
			cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = drv.Close(cctx)
			cancel()
		}
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

	// Mark 'closing' INSIDE the per-session lock and revoke delegate token unconditionally
	// so neither driverFor failure nor CloseSession failure can leave a live token behind.
	slot.runtimeState = protocol.SessionClosing
	slot.orbitMCPAttached = false
	r.revokeSessionDelegateTokenLocked(slot)
	drv := slot.driver
	isScoped := slot.scopedDriver
	slot.driver = nil
	slot.scopedDriver = false
	if drv == nil {
		var err error
		drv, err = r.driverFor(slot.adapter)
		if err != nil {
			slot.runtimeState = protocol.SessionDead
			r.syncSlotMetaLocked(slot)
			return false, err
		}
	}
	err := drv.CloseSession(ctx, slot.providerSessionID)
	if isScoped {
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = drv.Close(cctx)
		cancel()
	}
	if err != nil {
		slot.runtimeState = protocol.SessionDead
		r.syncSlotMetaLocked(slot)
		go r.sweepRetiredDrivers()
		return false, err
	}
	slot.runtimeState = protocol.SessionIdleClosed
	r.syncSlotMetaLocked(slot)
	go r.sweepRetiredDrivers()
	return true, nil
}

// DelegateTokenForSession returns the active per-session delegate token for wbSessionID (for diagnostics/tests).
func (r *Runner) DelegateTokenForSession(wbSessionID string) string {
	r.mu.Lock()
	slot, ok := r.sessions[wbSessionID]
	r.mu.Unlock()
	if !ok || slot == nil {
		return ""
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	return slot.delegateToken
}

// OrbitMCPServerNameForSession returns the active randomized Orbit MCP server name for wbSessionID (for diagnostics/tests).
func (r *Runner) OrbitMCPServerNameForSession(wbSessionID string) string {
	r.mu.Lock()
	slot, ok := r.sessions[wbSessionID]
	r.mu.Unlock()
	if !ok || slot == nil {
		return ""
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	return slot.orbitMCPServerName
}

func generateOrbitMCPServerName() string {
	var raw [4]byte
	_, _ = rand.Read(raw[:])
	return "orbit" + hex.EncodeToString(raw[:])
}

func isSupportedMCPNamespaceAdapter(adapter protocol.Adapter) bool {
	switch adapter {
	case protocol.AdapterAgy, protocol.AdapterClaude, protocol.AdapterCodex:
		return true
	default:
		return false
	}
}

func (r *Runner) ensureSessionDelegateTokenLocked(slot *sessionSlot, d protocol.AttemptDispatchFrame) string {
	if slot == nil || d.TaskID == "" {
		return ""
	}
	tok := slot.delegateToken
	if tok == "" {
		var raw [32]byte
		_, _ = rand.Read(raw[:])
		tok = hex.EncodeToString(raw[:])
		slot.delegateToken = tok
	}
	r.mu.Lock()
	if r.delegateTokens == nil {
		r.delegateTokens = map[string]*delegateTokenEntry{}
	}
	r.delegateTokens[tok] = &delegateTokenEntry{
		Token:       tok,
		WBSessionID: slot.wbSessionID,
		TaskID:      d.TaskID,
		ProjectID:   d.ProjectID,
		AttemptID:   d.AttemptID,
	}
	r.mu.Unlock()
	return tok
}

func (r *Runner) revokeSessionDelegateTokenLocked(slot *sessionSlot) {
	if slot == nil {
		return
	}
	slot.orbitMCPAttached = false
	slot.orbitMCPServerName = ""
	if slot.delegateToken == "" {
		return
	}
	tok := slot.delegateToken
	slot.delegateToken = ""
	r.mu.Lock()
	delete(r.delegateTokens, tok)
	r.mu.Unlock()
}

func (r *Runner) resolveOrbitBinaryPath() string {
	if p := strings.TrimSpace(r.cfg.OrbitBinPath); p != "" {
		if abs, err := filepath.Abs(p); err == nil {
			if fi, err := os.Stat(abs); err == nil && !fi.IsDir() {
				return abs
			}
		}
	}
	if exe, err := os.Executable(); err == nil && exe != "" {
		cand := filepath.Join(filepath.Dir(exe), "orbit")
		if runtime.GOOS == "windows" {
			cand += ".exe"
		}
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand
		}
	}
	if p, err := exec.LookPath("orbit"); err == nil && p != "" {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	return ""
}

func (r *Runner) buildOrbitMCPServers(d protocol.AttemptDispatchFrame, delegateToken, serverName string) []acp.McpServer {
	serverName = strings.TrimSpace(serverName)
	if delegateToken == "" || serverName == "" || r.cfg.SocketPath == "" || r.cfg.SocketPath == "none" {
		return nil
	}
	orbitBin := r.resolveOrbitBinaryPath()
	if orbitBin == "" {
		return nil
	}
	absSock := r.cfg.SocketPath
	if r.sockSrv != nil && r.sockSrv.sockPath != "" {
		absSock = r.sockSrv.sockPath
	} else if p, err := filepath.Abs(absSock); err == nil {
		absSock = p
	}
	return []acp.McpServer{
		{
			Stdio: &acp.McpServerStdio{
				Name:    serverName,
				Command: orbitBin,
				Args:    []string{"mcp-serve"},
				Env: []acp.EnvVariable{
					{Name: "ORBIT_RUNNER_SOCK", Value: absSock},
					{Name: "ORBIT_TASK_ID", Value: d.TaskID},
					{Name: "ORBIT_PROJECT_ID", Value: d.ProjectID},
					{Name: "ORBIT_DELEGATE_TOKEN", Value: delegateToken},
				},
			},
		},
	}
}

// Strict caller-side DTOs for local Runner Socket /delegate/* endpoints.
// Deliberately omit parentTaskId / parentAttemptId / delegatedByAttemptId so DisallowUnknownFields
// rejects any caller attempt to inject or shadow authoritative session identity (including case variants).
type localDelegateSpawnInput struct {
	Title              string            `json:"title,omitempty"`
	Goal               string            `json:"goal"`
	Role               string            `json:"role,omitempty"`
	ProjectID          string            `json:"projectId,omitempty"`
	MachineID          string            `json:"machineId,omitempty"`
	PartID             string            `json:"partId,omitempty"`
	LocationID         string            `json:"locationId,omitempty"`
	UseWorktree        *bool             `json:"useWorktree,omitempty"`
	RequiredCapability string            `json:"requiredCapability,omitempty"`
	Adapter            protocol.Adapter  `json:"adapter,omitempty"`
	Config             map[string]string `json:"config,omitempty"`
	NotifyParent       *bool             `json:"notifyParent,omitempty"`
	IdempotencyKey     string            `json:"idempotencyKey,omitempty"`
}

type localDelegateListInput struct {
	IncludeArchived bool `json:"includeArchived,omitempty"`
	IncludeTargets  bool `json:"includeTargets,omitempty"`
}

type localDelegateSendInput struct {
	ChildTaskID string                `json:"childTaskId"`
	Message     string                `json:"message"`
	Mode        protocol.FollowupMode `json:"mode,omitempty"`
}

type localDelegateReadInput struct {
	ChildTaskID string `json:"childTaskId"`
	Limit       int    `json:"limit,omitempty"`
}

type localDelegateCancelInput struct {
	ChildTaskID string `json:"childTaskId"`
	Reason      string `json:"reason,omitempty"`
}

type localDelegateBoardReadInput struct {
	BeforeSeq int64 `json:"beforeSeq,omitempty"`
	Limit     int   `json:"limit,omitempty"`
}

type localDelegateBoardPostInput struct {
	Body          string `json:"body"`
	TargetMention string `json:"targetMention,omitempty"`
}

type hubDelegateSpawnPayload struct {
	ParentTaskID    string `json:"parentTaskId"`
	ParentAttemptID string `json:"parentAttemptId"`
	protocol.CreateChildTaskRequest
}

type hubDelegateListPayload struct {
	ParentTaskID    string `json:"parentTaskId"`
	ParentAttemptID string `json:"parentAttemptId"`
	IncludeArchived bool   `json:"includeArchived,omitempty"`
	IncludeTargets  bool   `json:"includeTargets,omitempty"`
}

type hubDelegateSendPayload struct {
	ParentTaskID    string                `json:"parentTaskId"`
	ParentAttemptID string                `json:"parentAttemptId"`
	ChildTaskID     string                `json:"childTaskId"`
	Message         string                `json:"message"`
	Mode            protocol.FollowupMode `json:"mode,omitempty"`
}

type hubDelegateReadPayload struct {
	ParentTaskID    string `json:"parentTaskId"`
	ParentAttemptID string `json:"parentAttemptId"`
	ChildTaskID     string `json:"childTaskId"`
	Limit           int    `json:"limit,omitempty"`
}

type hubDelegateCancelPayload struct {
	ParentTaskID    string `json:"parentTaskId"`
	ParentAttemptID string `json:"parentAttemptId"`
	ChildTaskID     string `json:"childTaskId"`
	Reason          string `json:"reason,omitempty"`
}

type hubDelegateBoardReadPayload struct {
	ParentTaskID    string `json:"parentTaskId"`
	ParentAttemptID string `json:"parentAttemptId"`
	BeforeSeq       int64  `json:"beforeSeq,omitempty"`
	Limit           int    `json:"limit,omitempty"`
}

type hubDelegateBoardPostPayload struct {
	ParentTaskID    string `json:"parentTaskId"`
	ParentAttemptID string `json:"parentAttemptId"`
	Body            string `json:"body"`
	TargetMention   string `json:"targetMention,omitempty"`
}

func decodeStrictLocalDelegateJSON(rawBody []byte, dst any) error {
	trimmed := bytes.TrimSpace(rawBody)
	if len(trimmed) == 0 {
		return nil
	}
	// Reject duplicate keys at the top-level JSON object before strict typed decoding.
	if err := rejectDuplicateTopLevelJSONKeys(trimmed); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("unexpected trailing JSON content")
	}
	return nil
}

func rejectDuplicateTopLevelJSONKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return errors.New("request body must be a JSON object")
	}
	seen := map[string]bool{}
	for dec.More() {
		kTok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := kTok.(string)
		if !ok {
			return errors.New("invalid JSON object key")
		}
		lower := strings.ToLower(key)
		if seen[lower] {
			return fmt.Errorf("duplicate or conflicting JSON field %q", key)
		}
		seen[lower] = true
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return err
		}
	}
	return nil
}

// HandleLocalDelegate validates the per-session delegate token and live session/attempt state
// on ORBIT_RUNNER_SOCK, enforces plan/read-only write guards, strictly decodes caller fields,
// and forwards a freshly constructed typed payload with authoritative parentTaskId/parentAttemptId to Hub.
func (r *Runner) HandleLocalDelegate(ctx context.Context, action, delegateToken string, rawBody []byte) (int, []byte, error) {
	delegateToken = strings.TrimSpace(delegateToken)
	if delegateToken == "" {
		return http.StatusUnauthorized, nil, errors.New("missing X-Orbit-Delegate-Token")
	}
	r.mu.Lock()
	entry, ok := r.delegateTokens[delegateToken]
	var snap delegateTokenEntry
	var slot *sessionSlot
	var att *attemptContext
	if ok && entry != nil {
		snap = *entry
		slot = r.sessions[snap.WBSessionID]
		att = r.attempts[snap.AttemptID]
	}
	r.mu.Unlock()
	if !ok || snap.TaskID == "" || snap.AttemptID == "" || slot == nil || att == nil {
		return http.StatusUnauthorized, nil, errors.New("invalid or expired delegate token")
	}

	slot.mu.Lock()
	slotLive := slot.runtimeState != protocol.SessionDead &&
		slot.runtimeState != protocol.SessionIdleClosed &&
		slot.orbitMCPAttached &&
		slot.delegateToken == delegateToken
	slotMode := slot.appliedMode
	slot.mu.Unlock()
	if !slotLive {
		return http.StatusUnauthorized, nil, errors.New("session is closed or dead")
	}

	att.mu.Lock()
	attActive := !att.finalized && slices.Contains(protocol.ActiveAttemptStates, att.state)
	attMode := att.appliedMode
	desiredMode := att.dispatch.DesiredConfig["mode"]
	att.mu.Unlock()
	if !attActive {
		return http.StatusUnauthorized, nil, errors.New("session has no active attempt")
	}

	// Independently enforce read-only/plan session mode at the Socket execution boundary for write actions.
	effectiveMode := attMode
	if effectiveMode == "" {
		effectiveMode = slotMode
	}
	if effectiveMode == "" {
		effectiveMode = desiredMode
	}
	if (action == "spawn" || action == "send" || action == "cancel" || action == "board-post") && isReadOnlySessionMode(effectiveMode) {
		return http.StatusForbidden, nil, fmt.Errorf("delegate action %q is forbidden in %s mode", action, effectiveMode)
	}

	if strings.TrimSpace(r.cfg.HubHTTPURL) == "" {
		return http.StatusServiceUnavailable, nil, errors.New("hub HTTP URL is not configured on runner")
	}

	var upstreamPayload any
	switch action {
	case "spawn":
		var in localDelegateSpawnInput
		if err := decodeStrictLocalDelegateJSON(rawBody, &in); err != nil {
			return http.StatusBadRequest, nil, fmt.Errorf("invalid delegate spawn JSON: %w", err)
		}
		upstreamPayload = hubDelegateSpawnPayload{
			ParentTaskID:    snap.TaskID,
			ParentAttemptID: snap.AttemptID,
			CreateChildTaskRequest: protocol.CreateChildTaskRequest{
				Title:                in.Title,
				Goal:                 in.Goal,
				Role:                 in.Role,
				DelegatedByAttemptID: snap.AttemptID,
				ProjectID:            in.ProjectID,
				MachineID:            in.MachineID,
				PartID:               in.PartID,
				LocationID:           in.LocationID,
				UseWorktree:          in.UseWorktree,
				RequiredCapability:   in.RequiredCapability,
				Adapter:              in.Adapter,
				Config:               in.Config,
				NotifyParent:         in.NotifyParent,
				IdempotencyKey:       in.IdempotencyKey,
			},
		}
	case "list":
		var in localDelegateListInput
		if err := decodeStrictLocalDelegateJSON(rawBody, &in); err != nil {
			return http.StatusBadRequest, nil, fmt.Errorf("invalid delegate list JSON: %w", err)
		}
		upstreamPayload = hubDelegateListPayload{
			ParentTaskID:    snap.TaskID,
			ParentAttemptID: snap.AttemptID,
			IncludeArchived: in.IncludeArchived,
			IncludeTargets:  in.IncludeTargets,
		}
	case "send":
		var in localDelegateSendInput
		if err := decodeStrictLocalDelegateJSON(rawBody, &in); err != nil {
			return http.StatusBadRequest, nil, fmt.Errorf("invalid delegate send JSON: %w", err)
		}
		upstreamPayload = hubDelegateSendPayload{
			ParentTaskID:    snap.TaskID,
			ParentAttemptID: snap.AttemptID,
			ChildTaskID:     in.ChildTaskID,
			Message:         in.Message,
			Mode:            in.Mode,
		}
	case "read":
		var in localDelegateReadInput
		if err := decodeStrictLocalDelegateJSON(rawBody, &in); err != nil {
			return http.StatusBadRequest, nil, fmt.Errorf("invalid delegate read JSON: %w", err)
		}
		upstreamPayload = hubDelegateReadPayload{
			ParentTaskID:    snap.TaskID,
			ParentAttemptID: snap.AttemptID,
			ChildTaskID:     in.ChildTaskID,
			Limit:           in.Limit,
		}
	case "cancel":
		var in localDelegateCancelInput
		if err := decodeStrictLocalDelegateJSON(rawBody, &in); err != nil {
			return http.StatusBadRequest, nil, fmt.Errorf("invalid delegate cancel JSON: %w", err)
		}
		upstreamPayload = hubDelegateCancelPayload{
			ParentTaskID:    snap.TaskID,
			ParentAttemptID: snap.AttemptID,
			ChildTaskID:     in.ChildTaskID,
			Reason:          in.Reason,
		}
	case "board-read":
		var in localDelegateBoardReadInput
		if err := decodeStrictLocalDelegateJSON(rawBody, &in); err != nil {
			return http.StatusBadRequest, nil, fmt.Errorf("invalid delegate board-read JSON: %w", err)
		}
		upstreamPayload = hubDelegateBoardReadPayload{
			ParentTaskID:    snap.TaskID,
			ParentAttemptID: snap.AttemptID,
			BeforeSeq:       in.BeforeSeq,
			Limit:           in.Limit,
		}
	case "board-post":
		var in localDelegateBoardPostInput
		if err := decodeStrictLocalDelegateJSON(rawBody, &in); err != nil {
			return http.StatusBadRequest, nil, fmt.Errorf("invalid delegate board-post JSON: %w", err)
		}
		upstreamPayload = hubDelegateBoardPostPayload{
			ParentTaskID:    snap.TaskID,
			ParentAttemptID: snap.AttemptID,
			Body:            in.Body,
			TargetMention:   in.TargetMention,
		}
	default:
		return http.StatusNotFound, nil, fmt.Errorf("unknown delegate action %q", action)
	}

	forwarded, err := json.Marshal(upstreamPayload)
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	targetURL := strings.TrimRight(r.cfg.HubHTTPURL, "/") + "/api/runner/delegate/" + action
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(forwarded))
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Orbit-Machine-Id", r.cfg.MachineID)
	if tok := strings.TrimSpace(r.cfg.BearerToken); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return http.StatusBadGateway, nil, fmt.Errorf("forward delegate %s to hub: %w", action, err)
	}
	defer resp.Body.Close()
	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return http.StatusBadGateway, nil, fmt.Errorf("read hub delegate response: %w", err)
	}
	return resp.StatusCode, respBytes, nil
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
	r.finalizeAttemptDurable(att, protocol.AttemptNeedsVerification, map[string]any{
		"state":            string(protocol.AttemptNeedsVerification),
		"completionSource": string(protocol.CompletionQuietTimeout),
		"sessionLocked":    true,
		"errorCode":        "SWARM_IDLE_TIMEOUT",
		"errorDetail":      "Swarm 静默超时未收到显式 orbit deliver --final，已保留全部尾帧并锁定旧会话，请人工核对时间线",
	})
}

// HandleCancel cancels an active attempt (including swarm_monitoring) via ACP session/cancel.
// B2: Waits briefly for in-flight Prompt() exit and isolates/closes old SID if prompt or swarm workers remain active.
// B10: Durably records cancelled attemptId (memory + Outbox SQLite) even when HandleCancel arrives before HandleDispatch!
func (r *Runner) HandleCancel(frame protocol.AttemptCancelFrame) {
	if frame.AttemptID != "" {
		_ = r.outbox.RecordCancelledAttempt(context.Background(), frame.AttemptID, frame.LeaseToken)
	}
	r.mu.Lock()
	if frame.AttemptID != "" {
		r.cancelledAttempts[frame.AttemptID] = true
	}
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
	pinnedDrv := att.driver
	cancelFn := att.cancelFn
	hasInFlight := att.inFlightPrompts > 0 || !att.promptReturned
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
		drv := pinnedDrv
		if drv == nil && slot != nil {
			slot.mu.Lock()
			drv = slot.driver
			slot.mu.Unlock()
		}
		if drv == nil {
			drv, _ = r.driverFor(att.dispatch.Adapter)
		}
		if drv != nil {
			cctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = drv.Cancel(cctx, sid)
			cancel()
		}
	}
	if cancelFn != nil {
		cancelFn()
	}

	// B2: Wait briefly for an in-flight Prompt() to wind down cleanly before releasing session ownership.
	if sid != "" && hasInFlight {
		select {
		case <-att.promptDoneCh:
		case <-time.After(1500 * time.Millisecond):
		}
	}

	att.mu.Lock()
	stillRunning := sid != "" && (att.inFlightPrompts > 0 || !att.promptReturned)
	att.mu.Unlock()

	sessionLocked := false
	if stillRunning || att.tracker.HasActiveWorkers() {
		// Old prompt hasn't returned or swarm workers are still active -> isolate & close old SID!
		r.isolateAndCloseSession(slot, sid)
		sessionLocked = true
	}

	r.coalescer.FlushAttempt(att.dispatch.AttemptID)
	r.finalizeAttemptDurable(att, protocol.AttemptCancelled, map[string]any{
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

	if protocol.IsVirtualProjectID(dispatch.ProjectID) || dispatch.MirrorBarePath == "" {
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
		noInFlightPrompt := att.promptReturned && att.inFlightPrompts == 0
		if !noInFlightPrompt && req.Final {
			att.finalRequested = true
			att.explicitFinalDeliveryID = dlvID
		}
		att.mu.Unlock()

		finalized := false
		if noInFlightPrompt && (req.Final || activeWorkers == 0) {
			sessionLocked := false
			if activeWorkers > 0 {
				r.isolateAndCloseSession(slot, sid)
				sessionLocked = true
			}
			_, _, rejCount, nonZeroCount := att.tracker.Stats()
			unresRej, unresNonZero := att.tracker.UnresolvedCounts()
			if err := r.finalizeAttempt(att, protocol.AttemptSucceeded, map[string]any{
				"state":                       string(protocol.AttemptSucceeded),
				"stopReason":                  string(acp.StopReasonEndTurn),
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

	// Push delivery ref to bare mirror (local mirror or Hub git bundle endpoint for remote_ws)
	if !pd.Pushed {
		if err := r.pushDeliveryRefOrBundle(ctx, pd.ProjectID, pd.RepoRoot, pd.MirrorBarePath, pd.DeliveryID, pd.HeadSHA); err != nil {
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
	noInFlightPrompt := att.promptReturned && att.inFlightPrompts == 0
	alreadyFinal := att.finalized
	att.mu.Unlock()

	shouldFinalizeNow := noInFlightPrompt && !alreadyFinal && (req.Final || pd.IsFinal || activeWorkers == 0)

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
			"stopReason":                  string(acp.StopReasonEndTurn),
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
		deliveredFP, _ := commitTreeHash(ctx, pd.RepoRoot, pd.HeadSHA)
		att.mu.Lock()
		att.hasDelivery = true
		if deliveredFP != "" {
			att.lastDeliveredFingerprint = deliveredFP
		}
		if !noInFlightPrompt && (req.Final || pd.IsFinal) {
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

// worktreeHasChanges reports whether an isolated Git worktree has uncommitted edits, untracked files
// (even when status.showUntrackedFiles=no is set), or commits ahead of baseSHA.
// Any git error is returned (never silently treated as clean).
func worktreeHasChanges(ctx context.Context, worktreePath, baseSHA string) (bool, error) {
	if strings.TrimSpace(worktreePath) == "" {
		return false, nil
	}
	if strings.TrimSpace(baseSHA) != "" {
		curTree, err := worktreeTreeHash(ctx, worktreePath)
		if err != nil {
			return false, err
		}
		baseTree, err := commitTreeHash(ctx, worktreePath, baseSHA)
		if err != nil {
			return false, err
		}
		if curTree != baseTree {
			return true, nil
		}
		head, err := runGitCmdTrim(ctx, worktreePath, "rev-parse", "--verify", "HEAD^{commit}")
		if err != nil {
			return false, err
		}
		if head != "" && head != strings.TrimSpace(baseSHA) {
			return true, nil
		}
		return false, nil
	}
	status, err := runGitCmdTrim(ctx, worktreePath, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return false, err
	}
	return status != "", nil
}

// autoDeliverWorktree automatically packages, uploads, pushes, and emits a checkpoint delivery
// for an isolated Git worktree turn when files changed (either with no manual deliver or after a mid-turn deliver).
// Returns (deliveryID, pendingRetry, finalizedAtomically, err):
// - When pendingRetry is true, the checkpoint has been saved durably in pending_deliveries with its target terminalState.
// - When finalizedAtomically is true, BOTH delivery_published AND the terminal state frame were written in ONE atomic outbox tx.
func (r *Runner) autoDeliverWorktree(
	ctx context.Context,
	att *attemptContext,
	slot *sessionSlot,
	sid acp.SessionId,
	hasMidDelivery bool,
	termState protocol.AttemptState,
	termErrCode, termErrDetail string,
	finalPayload map[string]any,
) (string, bool, bool, error) {
	r.deliverMu.Lock()
	defer r.deliverMu.Unlock()

	att.mu.Lock()
	dispatch := att.dispatch
	worktreePath := att.worktreePath
	baseSHA := att.baseSHA
	att.mu.Unlock()

	dlvID := "dlv_" + ulid.Make().String()
	pkg, err := gitops.PackageCheckpoint(ctx, gitops.PackageOptions{
		RepoDir:       worktreePath,
		DeliveryID:    dlvID,
		TaskID:        dispatch.TaskID,
		AttemptID:     dispatch.AttemptID,
		ReviewBaseSHA: baseSHA,
	})
	if err != nil {
		return "", false, false, fmt.Errorf("package checkpoint: %w", err)
	}
	// When no mid-turn delivery occurred and working tree matches baseSHA with zero diff, skip publishing.
	// When a mid-turn delivery ALREADY occurred and the working tree was later reverted back to baseSHA,
	// we MUST still publish this final empty-diff snapshot so latest_delivery_id reflects the clean final state!
	if !hasMidDelivery && pkg.Manifest.FilesChanged == 0 && len(pkg.Manifest.Blobs) == 0 && len(pkg.Manifest.RyugiArtifacts) == 0 && pkg.HeadSHA == pkg.ReviewBaseSHA {
		return "", false, false, nil
	}

	stagedBlobs, err := r.stagePackagedBlobs(pkg.Blobs)
	if err != nil {
		return "", false, false, fmt.Errorf("stage delivery attachments: %w", err)
	}
	summary := fmt.Sprintf("自动保存变更（%d 个文件）", pkg.Manifest.FilesChanged)
	if pkg.Manifest.FilesChanged == 0 {
		if hasMidDelivery && pkg.HeadSHA == pkg.ReviewBaseSHA {
			summary = "自动保存工作树最终状态（已恢复基线）"
		} else {
			summary = "自动保存工作树变更"
		}
	}
	if termState == "" {
		termState = protocol.AttemptSucceeded
	}
	pd := PendingDelivery{
		DeliveryID:          dlvID,
		AttemptID:           dispatch.AttemptID,
		LeaseToken:          dispatch.LeaseToken,
		TaskID:              dispatch.TaskID,
		ProjectID:           dispatch.ProjectID,
		RepoRoot:            pkg.RepoRoot,
		MirrorBarePath:      dispatch.MirrorBarePath,
		ReviewBaseSHA:       pkg.ReviewBaseSHA,
		HeadSHA:             pkg.HeadSHA,
		Summary:             summary,
		Manifest:            pkg.Manifest,
		StagedBlobs:         stagedBlobs,
		IsFinal:             true,
		TerminalState:       termState,
		TerminalErrorCode:   termErrCode,
		TerminalErrorDetail: termErrDetail,
	}
	if err := r.outbox.SavePendingDelivery(ctx, pd); err != nil {
		return "", false, false, fmt.Errorf("save pending delivery: %w", err)
	}
	if err := r.uploadStagedBlobs(ctx, pd.StagedBlobs); err != nil {
		if isPermanentBundleErr(err) {
			_ = r.outbox.MarkDeliveryEmitted(ctx, pd.DeliveryID)
			return "", false, false, fmt.Errorf("upload delivery blobs: %w", err)
		}
		return pd.DeliveryID, true, false, nil
	}
	if err := r.pushDeliveryRefOrBundle(ctx, pd.ProjectID, pd.RepoRoot, pd.MirrorBarePath, pd.DeliveryID, pd.HeadSHA); err != nil {
		if isPermanentBundleErr(err) || !isHubBundleMirror(pd.MirrorBarePath) {
			_ = r.outbox.MarkDeliveryEmitted(ctx, pd.DeliveryID)
			return "", false, false, fmt.Errorf("push delivery ref: %w", err)
		}
		return pd.DeliveryID, true, false, nil
	}
	if err := r.outbox.MarkDeliveryPushed(ctx, pd.DeliveryID); err != nil {
		return "", false, false, fmt.Errorf("mark delivery pushed: %w", err)
	}

	dlvPayload := map[string]any{
		"deliveryId": pd.DeliveryID,
		"baseSha":    pd.ReviewBaseSHA,
		"headSha":    pd.HeadSHA,
		"gitRef":     "refs/orbit/deliveries/" + pd.DeliveryID,
		"summary":    pd.Summary,
		"manifest":   pd.Manifest,
	}

	if termState == protocol.AttemptNeedsVerification {
		r.isolateAndCloseSession(slot, sid)
	}
	if finalPayload == nil {
		finalPayload = map[string]any{
			"state": string(termState),
		}
	}
	finalPayload["deliveryId"] = pd.DeliveryID

	emitted, err := r.outbox.EmitPendingDeliveryAtomically(
		context.Background(),
		pd.DeliveryID,
		dispatch.AttemptID,
		dispatch.LeaseToken,
		"evt_"+ulid.Make().String(),
		dlvPayload,
		true,
		"evt_"+ulid.Make().String(),
		finalPayload,
	)
	if err != nil {
		return "", false, false, fmt.Errorf("emit delivery atomically: %w", err)
	}
	if emitted {
		r.wakeOutboxPump()
	}
	deliveredFP, _ := commitTreeHash(ctx, pd.RepoRoot, pd.HeadSHA)
	att.mu.Lock()
	att.hasDelivery = true
	if deliveredFP != "" {
		att.lastDeliveredFingerprint = deliveredFP
	}
	att.mu.Unlock()
	r.completeInMemoryFinalize(att, termState)
	return pd.DeliveryID, false, true, nil
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
		if headResp.StatusCode == http.StatusBadRequest || headResp.StatusCode == http.StatusForbidden || headResp.StatusCode == http.StatusRequestEntityTooLarge || headResp.StatusCode == http.StatusConflict {
			return errBundlePermanent{
				status: headResp.StatusCode,
				msg:    fmt.Sprintf("head blob %s rejected (%d)", b.Item.SHA256, headResp.StatusCode),
			}
		}

		f, err := os.Open(b.LocalPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return errBundlePermanent{
					status: http.StatusGone,
					msg:    fmt.Sprintf("staged blob missing %s: %v", b.LocalPath, err),
				}
			}
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
		if putResp.StatusCode == http.StatusBadRequest || putResp.StatusCode == http.StatusForbidden || putResp.StatusCode == http.StatusRequestEntityTooLarge || putResp.StatusCode == http.StatusConflict {
			return errBundlePermanent{
				status: putResp.StatusCode,
				msg:    fmt.Sprintf("put blob %s rejected (%d): %s", b.Item.SHA256, putResp.StatusCode, strings.TrimSpace(string(raw))),
			}
		}
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

	case u.CurrentModeUpdate != nil:
		newMode := strings.TrimSpace(string(u.CurrentModeUpdate.CurrentModeId))
		att.mu.Lock()
		att.appliedMode = newMode
		slot := att.slot
		att.mu.Unlock()
		if slot != nil {
			slot.mu.Lock()
			slot.appliedMode = newMode
			if slot.sessionModes != nil {
				slot.sessionModes.CurrentModeId = u.CurrentModeUpdate.CurrentModeId
			}
			slot.mu.Unlock()
		}

	case u.ConfigOptionUpdate != nil:
		newMode := currentSessionMode(u.ConfigOptionUpdate.ConfigOptions, nil)
		att.mu.Lock()
		slot := att.slot
		att.mu.Unlock()
		effectiveMode := newMode
		if slot != nil {
			slot.mu.Lock()
			slot.sessionOpts = u.ConfigOptionUpdate.ConfigOptions
			if effectiveMode == "" && slot.sessionModes != nil && strings.TrimSpace(string(slot.sessionModes.CurrentModeId)) != "" {
				effectiveMode = strings.TrimSpace(string(slot.sessionModes.CurrentModeId))
			}
			slot.appliedMode = effectiveMode
			slot.mu.Unlock()
		}
		att.mu.Lock()
		att.appliedMode = effectiveMode
		att.mu.Unlock()

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
	explicitFull := att.dispatch.ExplicitFullPermission
	appliedMode := att.appliedMode
	leaseToken := att.dispatch.LeaseToken
	baseSHA := att.baseSHA
	slot := att.slot
	att.mu.Unlock()

	hasOrbitMCP := false
	orbitMCPServerName := ""
	slotAdapter := protocol.Adapter("")
	if slot != nil {
		slot.mu.Lock()
		hasOrbitMCP = slot.orbitMCPAttached && slot.delegateToken != "" && slot.orbitMCPServerName != ""
		orbitMCPServerName = slot.orbitMCPServerName
		slotAdapter = slot.adapter
		slot.mu.Unlock()
	}

	// P0: Auto-approve ONLY when the current turn was explicitly authorized with ExplicitFullPermission
	// AND the mode that actually took effect on the ACP session is a full-bypass mode (never inherited
	// from old turns or downgraded from full -> auto by the bridge).
	policyMode := ""
	if explicitFull && isFullBypassMode(appliedMode) {
		policyMode = appliedMode
	}

	autoOpt, allow := r.cfg.PermissionPolicy(worktreePath, policyMode, req)
	orbitMCPTool := ""
	if (!allow || autoOpt == "") && hasOrbitMCP && isSupportedMCPNamespaceAdapter(slotAdapter) {
		if toolName, ok := matchOrbitBuiltinMCPTool(req, orbitMCPServerName); ok {
			if isReadOnlyOrbitMCPTool(toolName) || !isReadOnlySessionMode(appliedMode) {
				if optID, found := selectAllowPermissionOption(req.Options); found {
					autoOpt = optID
					allow = true
					orbitMCPTool = toolName
				}
			}
		}
	}

	if allow && autoOpt != "" {
		r.coalescer.FlushAttempt(attemptID)
		auditPayload := map[string]any{
			"policyAutoAllowed": true,
			"optionId":          autoOpt,
			"toolCallId":        string(req.ToolCall.ToolCallId),
		}
		if orbitMCPTool != "" {
			auditPayload["orbitBuiltinMcpTool"] = orbitMCPTool
		}
		_, _ = r.enqueueOutbox(r.runCtx, attemptID, leaseToken, protocol.OutboxFrameEvents, map[string]any{
			"kind":       string(protocol.EventApprovalConsumed),
			"visibility": string(protocol.VisibilityAudit),
			"actorKind":  string(protocol.ActorRunner),
			"actorId":    r.cfg.MachineID,
			"payload":    auditPayload,
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

func isFullBypassMode(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "full", "bypass", "bypasspermissions", "agent-full-access", "yolo":
		return true
	default:
		return false
	}
}

func isReadOnlySessionMode(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "plan", "read-only", "readonly":
		return true
	default:
		return false
	}
}

var orbitBuiltinMCPTools = map[string]bool{
	"orbit_spawn_child":   true,
	"orbit_list_children": true,
	"orbit_send_child":    true,
	"orbit_read_child":    true,
	"orbit_cancel_child":  true,
	"orbit_board_read":    true,
	"orbit_board_post":    true,
}

func isReadOnlyOrbitMCPTool(canonical string) bool {
	return canonical == "orbit_list_children" || canonical == "orbit_read_child" || canonical == "orbit_board_read"
}

// matchOrbitBuiltinMCPTool recognizes ONLY Orbit's 5 first-party stdio MCP delegation tools
// exposed by `orbit mcp-serve` under the session's randomized per-session server name
// (e.g. "orbit7c4e91a2", across agy/claude/codex naming conventions).
// Strictly inspects structured ToolCall.Name (never Title), requires the exact per-session
// server namespace prefix (rejecting static "orbit" or foreign server names), and excludes
// execute/edit/delete/move tool kinds.
func matchOrbitBuiltinMCPTool(req acp.RequestPermissionRequest, serverName string) (string, bool) {
	srv := strings.ToLower(strings.TrimSpace(serverName))
	if srv == "" || srv == "orbit" {
		return "", false
	}
	if kind, ok := req.ToolCall.Kind.Value(); ok {
		switch kind {
		case acp.ToolKindExecute, acp.ToolKindEdit, acp.ToolKindDelete, acp.ToolKindMove:
			return "", false
		}
	}
	raw, ok := req.ToolCall.Name.Value()
	if !ok {
		return "", false
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	lower := strings.ToLower(raw)
	matchedPrefix := false
	for _, prefix := range []string{
		"mcp_" + srv + "_",
		"mcp__" + srv + "__",
		srv + ":",
		srv + "/",
		srv + ".",
	} {
		if strings.HasPrefix(lower, prefix) {
			lower = strings.TrimPrefix(lower, prefix)
			matchedPrefix = true
			break
		}
	}
	if !matchedPrefix {
		return "", false
	}
	if orbitBuiltinMCPTools[lower] {
		return lower, true
	}
	return "", false
}

func selectAllowPermissionOption(options []acp.PermissionOption) (string, bool) {
	for _, opt := range options {
		if opt.Kind == acp.PermissionOptionKindAllowOnce {
			return string(opt.OptionId), true
		}
	}
	for _, opt := range options {
		if opt.Kind == acp.PermissionOptionKindAllowAlways {
			return string(opt.OptionId), true
		}
	}
	return "", false
}

// defaultPermissionPolicy defers permission gating to the ACP agent's own configured `mode`
// (default, accept-edits, auto, agent, plan, read-only, workspace-write, bypassPermissions, agent-full-access).
// Whenever the ACP agent sends `session/request_permission`, Orbit forwards the real request & options
// to the human operator, EXCEPT when the user explicitly authorized full-bypass mode AND that mode
// actually took effect on the session.
func defaultPermissionPolicy(_ string, effectiveFullMode string, req acp.RequestPermissionRequest) (string, bool) {
	if isFullBypassMode(effectiveFullMode) {
		return selectAllowPermissionOption(req.Options)
	}
	return "", false
}

// canonicalWorktreeCheckPath resolves symlinks even when multiple parent directories of a new file
// do not exist yet by walking UP to the deepest existing ancestor, resolving symlinks on that ancestor,
// and rejoining the remaining path segments. Never falls back to an un-resolved path (B5).
func canonicalWorktreeCheckPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(real)
	}
	cur := filepath.Clean(abs)
	var tail []string
	for {
		parent := filepath.Dir(cur)
		base := filepath.Base(cur)
		if parent == cur {
			// Reached filesystem root without resolving
			return ""
		}
		if base == ".." {
			return ""
		}
		tail = append([]string{base}, tail...)
		cur = parent
		if realAnc, err := filepath.EvalSymlinks(cur); err == nil {
			parts := append([]string{realAnc}, tail...)
			return filepath.Clean(filepath.Join(parts...))
		}
	}
}

func isPathWithinWorktree(canonWT, rawPath string, forbidGitInternals bool) bool {
	p := strings.TrimSpace(rawPath)
	if p == "" || canonWT == "" {
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
	return true
}

// isWhitelistedLocalCommand strictly verifies worktree-confined safe commands (B5).
// Anything whose executable, working directory, or flags cannot be positively proven confined
// to canonWT returns false so it routes to explicit human approval.
func isWhitelistedLocalCommand(req acp.RequestPermissionRequest, canonWT string) bool {
	if canonWT == "" {
		return false
	}
	rawIn, ok := req.ToolCall.RawInput.Value()
	if !ok || len(rawIn) == 0 {
		return false
	}
	var m map[string]any
	if err := json.Unmarshal(rawIn, &m); err != nil {
		return false
	}
	// If tool input specifies an explicit working directory, it MUST resolve strictly inside canonWT.
	for _, ck := range []string{"cwd", "Cwd", "workdir", "working_directory", "dir"} {
		if s, ok := m[ck].(string); ok && strings.TrimSpace(s) != "" {
			if !isPathWithinWorktree(canonWT, s, true) {
				return false
			}
		}
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
	// Reject shell metacharacters, subshells, env expansion, redirection, globbing, tilde, or multi-line injection.
	for _, meta := range []string{"&&", "||", ";", "|", "`", "$", ">", "<", "\n", "\r", "(", ")", "~", "&"} {
		if strings.Contains(cmd, meta) {
			return false
		}
	}
	fields := strings.Fields(cmd)
	if len(fields) < 2 {
		return false
	}
	// Require bare trusted executable name (never ./git, /tmp/go, etc.).
	exe := fields[0]
	if strings.ContainsAny(exe, "/\\") {
		return false
	}
	sub := fields[1]
	args := fields[2:]

	// Common argument path hygiene: reject parent traversal segments (`..`) or paths outside canonWT.
	hasParentTraversal := func(s string) bool {
		if s == ".." {
			return true
		}
		norm := strings.ReplaceAll(s, "\\", "/")
		for _, seg := range strings.Split(norm, "/") {
			if seg == ".." {
				return true
			}
		}
		return false
	}
	checkArgPaths := func(argList []string, forbidGit bool) bool {
		for _, a := range argList {
			trimmed := strings.Trim(a, `"'`)
			if hasParentTraversal(trimmed) {
				return false
			}
			if strings.HasPrefix(trimmed, "/") && !isPathWithinWorktree(canonWT, trimmed, forbidGit) {
				return false
			}
			if idx := strings.Index(trimmed, "=/"); idx >= 0 {
				if !isPathWithinWorktree(canonWT, trimmed[idx+1:], forbidGit) {
					return false
				}
			}
			if idx := strings.Index(trimmed, "="); idx >= 0 {
				if hasParentTraversal(trimmed[idx+1:]) {
					return false
				}
			}
		}
		return true
	}

	switch exe {
	case "orbit":
		if sub != "deliver" && sub != "status" && sub != "brief" {
			return false
		}
		for _, a := range args {
			lower := strings.ToLower(a)
			for _, bad := range []string{"--repo", "--mirror", "--data-dir", "--config-dir", "--hub", "--sock", "--token"} {
				if lower == bad || strings.HasPrefix(lower, bad+"=") {
					return false
				}
			}
		}
		return checkArgPaths(args, true)

	case "git":
		switch sub {
		case "status", "diff", "log", "show", "rev-parse", "ls-files":
		default:
			return false
		}
		for _, a := range args {
			for _, bad := range []string{"-C", "-c", "--git-dir", "--work-tree", "--exec-path", "--output", "-o", "--ext-diff", "--textconv", "--no-index"} {
				if a == bad || strings.HasPrefix(a, bad+"=") {
					return false
				}
			}
		}
		return checkArgPaths(args, false)

	case "go":
		switch sub {
		case "test", "vet", "list":
		default:
			return false
		}
		for _, a := range args {
			for _, bad := range []string{"-exec", "-toolexec", "-o", "-C", "-overlay", "-modfile", "-pkgdir"} {
				if a == bad || strings.HasPrefix(a, bad+"=") {
					return false
				}
			}
		}
		return checkArgPaths(args, true)

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

// finalizeAttemptDurable retries persisting the terminal state to the local Outbox SQLite
// until durable persistence succeeds (or the Runner itself shuts down), ensuring a transient
// Outbox write error NEVER drops terminal state or leaks resources/heartbeat leases (B19).
func (r *Runner) finalizeAttemptDurable(att *attemptContext, terminalState protocol.AttemptState, payload map[string]any) {
	backoff := 100 * time.Millisecond
	for {
		if err := r.finalizeAttempt(att, terminalState, payload); err == nil {
			return
		}
		select {
		case <-r.runCtx.Done():
			return
		case <-time.After(backoff):
			if backoff < 500*time.Millisecond {
				backoff *= 2
			}
		}
	}
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

	r.releaseWorkerReservation(att)
	r.releaseAttemptResources(att)

	r.mu.Lock()
	if sid != "" && r.sessionOwner[sid] == attemptID {
		delete(r.sessionOwner, sid)
	}
	r.mu.Unlock()

	if cancelFn != nil {
		cancelFn()
	}

	if r.quotaCollector != nil && r.runCtx != nil && r.runCtx.Err() == nil {
		r.quotaRefreshMu.Lock()
		now := time.Now()
		shouldRefresh := now.Sub(r.lastPostQuota) >= 30*time.Second
		if shouldRefresh {
			r.lastPostQuota = now
		}
		r.quotaRefreshMu.Unlock()
		if shouldRefresh {
			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				qctx, qcancel := context.WithTimeout(r.runCtx, 20*time.Second)
				defer qcancel()
				r.refreshAndReportQuotas(qctx, true)
			}()
		}
	}
}

// HandleBoardMessage queues a Hub board.message @mention into an active swarm_monitoring attempt
// and drains messages serially in #seq order (deduped by messageId, NEVER dropping a second message!) (B16 & B2).
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
	if frame.MessageID != "" {
		if att.boardDelivered[frame.MessageID] || att.boardQueuedIDs[frame.MessageID] {
			att.mu.Unlock()
			return
		}
		att.boardQueuedIDs[frame.MessageID] = true
	}
	att.boardQueue = append(att.boardQueue, frame)
	if att.boardDraining {
		att.mu.Unlock()
		return
	}
	att.boardDraining = true
	att.mu.Unlock()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.drainBoardMessages(att)
	}()
}

func (r *Runner) drainBoardMessages(att *attemptContext) {
	for {
		att.mu.Lock()
		if att.finalized || att.state != protocol.AttemptSwarmMonitoring || len(att.boardQueue) == 0 {
			att.boardDraining = false
			att.mu.Unlock()
			return
		}
		frame := att.boardQueue[0]
		att.boardQueue = att.boardQueue[1:]
		sid := att.providerSessionID
		slot := att.slot
		leaseToken := att.dispatch.LeaseToken
		attemptID := att.dispatch.AttemptID
		att.mu.Unlock()

		// Wait cleanly if another prompt currently holds slot.inFlightPrompt instead of dropping the message.
		acquired := false
		for !acquired {
			if r.runCtx.Err() != nil {
				att.mu.Lock()
				att.boardDraining = false
				att.mu.Unlock()
				return
			}
			att.mu.Lock()
			if att.finalized || att.state != protocol.AttemptSwarmMonitoring {
				att.boardDraining = false
				att.mu.Unlock()
				return
			}
			att.mu.Unlock()

			slot.mu.Lock()
			if slot.locked || slot.providerSessionID != sid {
				slot.mu.Unlock()
				att.mu.Lock()
				att.boardDraining = false
				att.mu.Unlock()
				return
			}
			if !slot.inFlightPrompt {
				slot.inFlightPrompt = true
				slot.promptOwnerAttemptID = attemptID
				att.mu.Lock()
				att.inFlightPrompts++
				att.promptReturned = false
				att.mu.Unlock()
				acquired = true
			}
			slot.mu.Unlock()
			if !acquired {
				select {
				case <-r.runCtx.Done():
					att.mu.Lock()
					att.boardDraining = false
					att.mu.Unlock()
					return
				case <-time.After(30 * time.Millisecond):
				}
			}
		}

		msgText := fmt.Sprintf("[Board Message #%d from %s targeting @%s]\n%s", frame.Seq, frame.AuthorName, frame.TargetMention, frame.Body)
		pctx, cancel := context.WithTimeout(r.runCtx, 2*time.Minute)
		var resp acp.PromptResponse
		var err error
		att.mu.Lock()
		drv := att.driver
		att.mu.Unlock()
		if drv == nil {
			slot.mu.Lock()
			drv = slot.driver
			slot.mu.Unlock()
		}
		if drv == nil {
			drv, err = r.driverFor(att.dispatch.Adapter)
		}
		if err == nil && drv != nil {
			resp, err = drv.Prompt(pctx, sid, msgText)
		}
		cancel()

		slot.mu.Lock()
		if slot.promptOwnerAttemptID == attemptID {
			slot.inFlightPrompt = false
			slot.promptOwnerAttemptID = ""
		}
		slot.lastActivityAt = time.Now()
		r.syncSlotMetaLocked(slot)
		slot.mu.Unlock()

		att.mu.Lock()
		if att.inFlightPrompts > 0 {
			att.inFlightPrompts--
		}
		att.promptReturned = (att.inFlightPrompts == 0)
		finalReq := att.finalRequested
		explicitDlvID := att.explicitFinalDeliveryID
		alreadyFinal := att.finalized
		att.mu.Unlock()

		cleanTurn := err == nil && (resp.StopReason == acp.StopReasonEndTurn || resp.StopReason == "")
		if cleanTurn {
			att.mu.Lock()
			if frame.MessageID != "" {
				att.boardDelivered[frame.MessageID] = true
				delete(att.boardQueuedIDs, frame.MessageID)
			}
			att.mu.Unlock()
			r.resetSwarmIdleTimerIfMonitoring(att)
			// B16: Advance cursor honestly — native swarm board messages are delivered into the
			// coordinator ACP session, so advance the coordinator cursor rather than fabricating
			// private target-worker consumption without evidence.
			ackMember := "coordinator"
			if strings.EqualFold(strings.TrimPrefix(strings.TrimSpace(frame.TargetMention), "@"), "coordinator") {
				ackMember = "coordinator"
			}
			_, _ = r.enqueueOutbox(r.runCtx, frame.AttemptID, leaseToken, protocol.OutboxFrameBoardAck, map[string]any{
				"messageId":  frame.MessageID,
				"memberName": ackMember,
				"seq":        frame.Seq,
			})
		} else {
			att.mu.Lock()
			if frame.MessageID != "" {
				delete(att.boardQueuedIDs, frame.MessageID)
			}
			att.mu.Unlock()
		}

		// B2: If `orbit deliver --final` executed during this board prompt and prompt ended cleanly,
		// finalize NOW after releasing slot.inFlightPrompt so SID is never reused early!
		if finalReq && !alreadyFinal && cleanTurn {
			att.ackPendingWG.Wait()
			r.coalescer.FlushAttempt(attemptID)
			activeWorkers := att.tracker.ActiveWorkerCount()
			sessionLocked := false
			if activeWorkers > 0 {
				r.isolateAndCloseSession(slot, sid)
				sessionLocked = true
			}
			_, _, rejCount, nonZeroCount := att.tracker.Stats()
			unresRej, unresNonZero := att.tracker.UnresolvedCounts()
			r.finalizeAttemptDurable(att, protocol.AttemptSucceeded, map[string]any{
				"state":                       string(protocol.AttemptSucceeded),
				"stopReason":                  string(acp.StopReasonEndTurn),
				"completionSource":            string(protocol.CompletionExplicitDeliver),
				"deliveryId":                  explicitDlvID,
				"sessionLocked":               sessionLocked,
				"rejectedToolCount":           rejCount,
				"nonZeroExitCount":            nonZeroCount,
				"unresolvedRejectedToolCount": unresRej,
				"unresolvedNonZeroExitCount":  unresNonZero,
			})
		}
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
	for _, pendingBM := range welcome.Reconcile.PendingBoardMessages {
		r.HandleBoardMessage(pendingBM)
	}

	// Retry any offline pending deliveries and wake outbox flush immediately
	r.retryPendingDeliveries(ctx)
	r.wakeOutboxPump()

	connCtx, cancelConn := context.WithCancel(ctx)
	defer cancelConn()

	var ioWG sync.WaitGroup
	// Serialized config-apply & periodic re-probe worker bound to this connection's lifecycle
	var configCh chan protocol.RunnerConfigFrame
	if r.agentMgr != nil {
		configCh = make(chan protocol.RunnerConfigFrame, 16)
		if welcome.Config != nil {
			r.agentMgr.ApplyHubConfig(*welcome.Config)
			configCh <- protocol.RunnerConfigFrame{
				Type:      protocol.RunnerMsgConfig,
				ConfigRev: welcome.Config.ConfigRev,
				Agents:    welcome.Config.Agents,
			}
		}
		ioWG.Add(1)
		go func() {
			defer ioWG.Done()
			ticker := time.NewTicker(10 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-connCtx.Done():
					return
				case cfg := <-configCh:
					// Drain any coalesced newer config frames queued behind cfg so we apply monotonically
					latest := cfg
					for {
						select {
						case next := <-configCh:
							if next.ConfigRev >= latest.ConfigRev {
								latest = next
							}
						default:
							goto applyLatest
						}
					}
				applyLatest:
					if applied, _ := r.agentMgr.ApplyHubConfig(protocol.RunnerConfigPayload{
						ConfigRev: latest.ConfigRev,
						Agents:    latest.Agents,
					}); !applied {
						continue
					}
					if probes, rev, fresh := r.agentMgr.ProbeAllWithRev(connCtx); fresh {
						r.syncDriversFromAgentManager(probes)
						r.sendAgentsReport(probes, rev)
					}
				case <-ticker.C:
					if probes, rev, fresh := r.agentMgr.ProbeAllWithRev(connCtx); fresh {
						r.syncDriversFromAgentManager(probes)
						r.sendAgentsReport(probes, rev)
					}
				}
			}
		}()
	}
	if r.quotaCollector != nil {
		ioWG.Add(1)
		go func() {
			defer ioWG.Done()
			// Non-blocking startup quota collection + periodic refresh every 3 minutes.
			r.refreshAndReportQuotas(connCtx, false)
			ticker := time.NewTicker(3 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-connCtx.Done():
					return
				case <-ticker.C:
					r.refreshAndReportQuotas(connCtx, true)
				}
			}
		}()
	}
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
		case protocol.RunnerMsgConfig:
			var f protocol.RunnerConfigFrame
			if json.Unmarshal(data, &f) == nil && configCh != nil {
				// Apply Hub config revision immediately in order, then queue serialized re-probe.
				r.agentMgr.ApplyHubConfig(protocol.RunnerConfigPayload{
					ConfigRev: f.ConfigRev,
					Agents:    f.Agents,
				})
				select {
				case configCh <- f:
				default:
					// Drop stale queued item and enqueue latest
					select {
					case <-configCh:
					default:
					}
					select {
					case configCh <- f:
					default:
					}
				}
			}
		case protocol.RunnerMsgRPC:
			var f protocol.RunnerRPCFrame
			if json.Unmarshal(data, &f) == nil {
				ioWG.Add(1)
				go func(req protocol.RunnerRPCFrame) {
					defer ioWG.Done()
					r.handleRunnerRPC(connCtx, req)
				}(f)
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

	hostname, _ := os.Hostname()
	var agentProbes []protocol.AgentProbe
	var agentQuotas []protocol.AgentQuotaSnapshot
	var configRev int64
	caps := []string{protocol.RunnerCapFSV1, protocol.RunnerCapBundleV1, protocol.RunnerCapFSPlainV1}
	if r.agentMgr != nil {
		caps = append(caps, protocol.RunnerCapAgentsV1)
		agentProbes = r.agentMgr.SnapshotProbes()
		configRev = r.agentMgr.ConfigRev()
	}
	if r.quotaCollector != nil {
		caps = append(caps, protocol.RunnerCapQuotasV1)
		agentQuotas = r.quotaCollector.Snapshot()
	}

	return protocol.RunnerHelloFrame{
		Type:                     protocol.RunnerMsgHello,
		ProtocolVersion:          1,
		MachineID:                r.cfg.MachineID,
		RunnerInstanceID:         r.instanceID,
		RunnerVersion:            r.cfg.RunnerVersion,
		OS:                       runtime.GOOS,
		Arch:                     runtime.GOARCH,
		Hostname:                 hostname,
		Capabilities:             caps,
		Adapters:                 r.supportedAdapters(),
		AgentProbes:              agentProbes,
		AgentQuotas:              agentQuotas,
		ConfigRev:                configRev,
		LiveAttempts:             liveAttempts,
		LivePendingApprovalIDs:   liveApprovals,
		HandledApprovalDecisions: handled,
		Resources:                resources,
	}
}

func (r *Runner) sendAgentsReport(probes []protocol.AgentProbe, configRev int64) {
	if r.agentMgr == nil {
		return
	}
	_ = r.sendWSJSON(protocol.RunnerAgentsReportFrame{
		Type:      protocol.RunnerMsgAgentsReport,
		MachineID: r.cfg.MachineID,
		ConfigRev: configRev,
		Agents:    probes,
	})
}

func (r *Runner) sendQuotasReport(quotas []protocol.AgentQuotaSnapshot) {
	if r.quotaCollector == nil {
		return
	}
	_ = r.sendWSJSON(protocol.RunnerQuotasReportFrame{
		Type:      protocol.RunnerMsgQuotasReport,
		MachineID: r.cfg.MachineID,
		Quotas:    quotas,
	})
}

func (r *Runner) refreshAndReportQuotas(ctx context.Context, force bool) []protocol.AgentQuotaSnapshot {
	if r.quotaCollector == nil {
		return nil
	}
	quotas := r.quotaCollector.CollectAll(ctx, protocol.Phase1ImplementedSessionAdapters, force)
	if ctx.Err() == nil && len(quotas) > 0 {
		r.sendQuotasReport(quotas)
	}
	return quotas
}

func (r *Runner) handleRunnerRPC(ctx context.Context, req protocol.RunnerRPCFrame) {
	res := protocol.RunnerRPCResultFrame{
		Type:      protocol.RunnerMsgRPCResult,
		RequestID: req.RequestID,
	}

	switch req.Op {
	case "probe":
		if r.agentMgr == nil {
			res.Error = "agent manager not enabled"
		} else {
			probes, rev, fresh := r.agentMgr.ProbeAllWithRev(ctx)
			if fresh {
				r.syncDriversFromAgentManager(probes)
				r.sendAgentsReport(probes, rev)
			} else {
				probes = r.agentMgr.SnapshotProbes()
			}
			data := map[string]any{"agents": probes}
			if r.quotaCollector != nil {
				if quotas := r.quotaCollector.Snapshot(); len(quotas) > 0 {
					data["agentQuotas"] = quotas
				}
				if r.runCtx != nil && r.runCtx.Err() == nil {
					r.wg.Add(1)
					go func() {
						defer r.wg.Done()
						qctx, qcancel := context.WithTimeout(r.runCtx, 20*time.Second)
						defer qcancel()
						r.refreshAndReportQuotas(qctx, true)
					}()
				}
			}
			res.OK = true
			res.Data = data
		}

	case "probe_quotas":
		force := true
		if v, ok := req.Args["force"].(bool); ok {
			force = v
		}
		if r.quotaCollector == nil {
			res.OK = true
			res.Data = map[string]any{"agentQuotas": []protocol.AgentQuotaSnapshot{}}
		} else {
			quotas := r.refreshAndReportQuotas(ctx, force)
			if quotas == nil {
				quotas = []protocol.AgentQuotaSnapshot{}
			}
			res.OK = true
			res.Data = map[string]any{"agentQuotas": quotas}
		}

	case "test_agent":
		if r.agentMgr == nil {
			res.Error = "agent manager not enabled"
		} else {
			adpStr, _ := req.Args["adapter"].(string)
			probe, rev, err := r.agentMgr.TestAgent(ctx, protocol.Adapter(adpStr))
			probes := r.agentMgr.SnapshotProbes()
			if r.agentMgr.ConfigRev() == rev {
				r.syncDriversFromAgentManager(probes)
				r.sendAgentsReport(probes, rev)
			}
			res.Data = map[string]any{"probe": probe, "agents": probes}
			if err != nil {
				res.Error = err.Error()
			} else {
				res.OK = true
			}
		}

	case "install_bridge":
		if r.agentMgr == nil {
			res.Error = "agent manager not enabled"
		} else {
			adpStr, _ := req.Args["adapter"].(string)
			probe, rev, err := r.agentMgr.InstallBridge(ctx, protocol.Adapter(adpStr))
			probes := r.agentMgr.SnapshotProbes()
			if r.agentMgr.ConfigRev() == rev {
				r.syncDriversFromAgentManager(probes)
				r.sendAgentsReport(probes, rev)
			}
			res.Data = map[string]any{"probe": probe, "agents": probes}
			if err != nil {
				res.Error = err.Error()
			} else {
				res.OK = true
			}
		}

	case "list_dirs":
		pathArg, _ := req.Args["path"].(string)
		out, err := gitops.ListSubdirs(pathArg, r.effectiveBrowseRoots(req.Args), false)
		if err != nil {
			code, _ := gitops.ClassifyBrowseError(err)
			res.Error = err.Error()
			res.Data = map[string]any{"errorCode": code}
		} else {
			res.OK = true
			raw, _ := json.Marshal(out)
			_ = json.Unmarshal(raw, &res.Data)
		}

	case "check_repo":
		pathArg, _ := req.Args["path"].(string)
		out := gitops.CheckGitRepo(ctx, pathArg, r.effectiveBrowseRoots(req.Args))
		raw, _ := json.Marshal(out)
		_ = json.Unmarshal(raw, &res.Data)
		res.OK = out.Valid
		if !out.Valid {
			res.Error = out.Error
		}

	case "inspect_dir":
		pathArg, _ := req.Args["path"].(string)
		out := gitops.InspectDirectory(ctx, pathArg, r.effectiveBrowseRoots(req.Args))
		raw, _ := json.Marshal(out)
		_ = json.Unmarshal(raw, &res.Data)
		res.OK = out.Valid
		if !out.Valid {
			res.Error = out.Error
		}

	case "scan_repos":
		repos, err := gitops.ScanGitSuggestions(ctx, r.effectiveBrowseRoots(req.Args))
		if err != nil {
			res.Error = err.Error()
		} else {
			res.OK = true
			res.Data = map[string]any{"repos": repos}
		}

	case "read_folder_file":
		rootDir, _ := req.Args["rootDir"].(string)
		relPath, _ := req.Args["relPath"].(string)
		data, name, err := gitops.ReadFolderFile(rootDir, relPath, r.effectiveBrowseRoots(req.Args), 20*1024*1024)
		if err != nil {
			res.Error = err.Error()
		} else {
			res.OK = true
			res.Data = map[string]any{
				"filename":      name,
				"contentBase64": base64.StdEncoding.EncodeToString(data),
				"sizeBytes":     len(data),
			}
		}

	default:
		res.Error = fmt.Sprintf("unknown rpc op %q", req.Op)
	}

	_ = r.sendWSJSON(res)
}

func (r *Runner) effectiveBrowseRoots(args map[string]any) []string {
	if len(r.cfg.AllowedBrowseRoots) > 0 {
		return r.cfg.AllowedBrowseRoots
	}
	if rawRoots, ok := args["allowedRoots"].([]any); ok && len(rawRoots) > 0 {
		out := make([]string, 0, len(rawRoots))
		for _, item := range rawRoots {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return r.cfg.AllowedBrowseRoots
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

func (r *Runner) failPermanentPendingDelivery(ctx context.Context, pd PendingDelivery, errorCode string, cause error) {
	r.mu.Lock()
	attFail := r.attempts[pd.AttemptID]
	r.mu.Unlock()

	leaseToken := pd.LeaseToken
	if leaseToken == "" && attFail != nil {
		leaseToken = attFail.dispatch.LeaseToken
	}
	if leaseToken == "" && pd.AttemptID != "" {
		leaseToken, _, _ = r.outbox.ReadUnacked(ctx, pd.AttemptID, 0, 1)
	}
	if pd.AttemptID != "" && leaseToken != "" {
		_, _ = r.enqueueOutbox(ctx, pd.AttemptID, leaseToken, protocol.OutboxFrameState, map[string]any{
			"state":       string(protocol.AttemptFailed),
			"errorCode":   errorCode,
			"errorDetail": cause.Error(),
		})
	}
	_ = r.outbox.MarkDeliveryEmitted(ctx, pd.DeliveryID)
	if attFail != nil {
		r.completeInMemoryFinalize(attFail, protocol.AttemptFailed)
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
			if isPermanentBundleErr(err) {
				r.failPermanentPendingDelivery(ctx, pd, "DELIVERY_BLOB_REJECTED", err)
			}
			continue
		}
		if !pd.Pushed {
			if err := r.pushDeliveryRefOrBundle(ctx, pd.ProjectID, pd.RepoRoot, pd.MirrorBarePath, pd.DeliveryID, pd.HeadSHA); err != nil {
				if isPermanentBundleErr(err) {
					// Surface permanent size/conflict failure instead of retrying forever (#8)
					r.failPermanentPendingDelivery(ctx, pd, "DELIVERY_BUNDLE_REJECTED", err)
				}
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
		termState := pd.TerminalState
		if termState == "" {
			termState = protocol.AttemptSucceeded
		}
		var finalPayload map[string]any
		if pd.IsFinal {
			if att == nil {
				emitFinal = true
				finalPayload = map[string]any{
					"state":            string(termState),
					"completionSource": string(protocol.CompletionExplicitDeliver),
					"deliveryId":       pd.DeliveryID,
				}
				if termState == protocol.AttemptNeedsVerification {
					finalPayload["completionSource"] = string(protocol.CompletionPromptEndTurn)
					finalPayload["sessionLocked"] = true
					errCode := pd.TerminalErrorCode
					if errCode == "" {
						errCode = "UNRESOLVED_TOOL_FAILURE"
					}
					errDetail := pd.TerminalErrorDetail
					if errDetail == "" {
						errDetail = "本轮存在未解决的工具审批拒绝，请人工核对后再决定是否续跑"
					}
					finalPayload["errorCode"] = errCode
					finalPayload["errorDetail"] = errDetail
				}
			} else {
				att.mu.Lock()
				noInFlightPrompt := att.promptReturned && att.inFlightPrompts == 0
				alreadyFinal := att.finalized
				sid := att.providerSessionID
				slot := att.slot
				att.mu.Unlock()
				if noInFlightPrompt && !alreadyFinal {
					emitFinal = true
					sessionLocked := false
					if termState == protocol.AttemptNeedsVerification || att.tracker.ActiveWorkerCount() > 0 {
						r.isolateAndCloseSession(slot, sid)
						sessionLocked = true
					}
					_, _, rejCount, nonZeroCount := att.tracker.Stats()
					unresRej, unresNonZero := att.tracker.UnresolvedCounts()
					compSrc := protocol.CompletionPromptEndTurn
					att.mu.Lock()
					if att.finalRequested {
						compSrc = protocol.CompletionExplicitDeliver
					}
					att.mu.Unlock()
					finalPayload = map[string]any{
						"state":                       string(termState),
						"stopReason":                  string(acp.StopReasonEndTurn),
						"completionSource":            string(compSrc),
						"deliveryId":                  pd.DeliveryID,
						"sessionLocked":               sessionLocked,
						"rejectedToolCount":           rejCount,
						"nonZeroExitCount":            nonZeroCount,
						"unresolvedRejectedToolCount": unresRej,
						"unresolvedNonZeroExitCount":  unresNonZero,
					}
					if termState == protocol.AttemptNeedsVerification {
						errCode := pd.TerminalErrorCode
						if errCode == "" {
							errCode = "UNRESOLVED_TOOL_FAILURE"
						}
						errDetail := pd.TerminalErrorDetail
						if errDetail == "" {
							errDetail = "本轮存在未解决的工具审批拒绝，请人工核对后再决定是否续跑"
						}
						finalPayload["errorCode"] = errCode
						finalPayload["errorDetail"] = errDetail
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
				r.completeInMemoryFinalize(att, termState)
			} else {
				deliveredFP, _ := commitTreeHash(ctx, pd.RepoRoot, pd.HeadSHA)
				att.mu.Lock()
				att.hasDelivery = true
				if deliveredFP != "" {
					att.lastDeliveredFingerprint = deliveredFP
				}
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
	deliveryRetryTicker := time.NewTicker(r.cfg.DeliveryRetryInterval)
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
