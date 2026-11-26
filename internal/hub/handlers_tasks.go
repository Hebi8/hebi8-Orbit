package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/oklog/ulid/v2"

	"github.com/Hebi8/hebi8-Orbit/internal/gitops"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

var safeDeliveryIDRegex = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,127}$`)

func isSafeDeliveryID(id string) bool {
	id = strings.TrimSpace(id)
	if !safeDeliveryIDRegex.MatchString(id) {
		return false
	}
	return !strings.Contains(id, "..")
}

type deliveryLockEntry struct {
	mu   sync.Mutex
	refs int
}

// deliveryLocker provides ref-counted per-deliveryID exclusive locks shared across Hub
// (cleanup / manual-import) and RyugiBridge (snapshot export / registration / content read).
// Lock hierarchy: syncOpMu -> bindingGate -> regFlight wait -> deliveryLocks.Lock(id) -> ensureMu -> SQLite tx.
type deliveryLocker struct {
	mu      sync.Mutex
	entries map[string]*deliveryLockEntry
}

func newDeliveryLocker() *deliveryLocker {
	return &deliveryLocker{entries: map[string]*deliveryLockEntry{}}
}

func (l *deliveryLocker) Lock(deliveryID string) func() {
	if l == nil {
		return func() {}
	}
	l.mu.Lock()
	e, ok := l.entries[deliveryID]
	if !ok {
		e = &deliveryLockEntry{}
		l.entries[deliveryID] = e
	}
	e.refs++
	l.mu.Unlock()

	e.mu.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			e.mu.Unlock()
			l.mu.Lock()
			e.refs--
			if e.refs == 0 {
				delete(l.entries, deliveryID)
			}
			l.mu.Unlock()
		})
	}
}

func (s *Server) removeFrozenDir(dir string) error {
	s.testHookMu.Lock()
	fn := s.removeFrozenDirFn
	s.testHookMu.Unlock()
	if fn != nil {
		return fn(dir)
	}
	return gitops.RemoveFrozenDir(dir)
}

func (s *Server) cleanupPendingDeliveryDir(ctx context.Context, deliveryID string) error {
	unlock := s.deliveryLocks.Lock(strings.TrimSpace(deliveryID))
	defer unlock()
	return s.cleanupPendingDeliveryDirLocked(ctx, deliveryID)
}

func (s *Server) cleanupPendingDeliveryDirLocked(ctx context.Context, deliveryID string) error {
	deliveryID = strings.TrimSpace(deliveryID)
	dataDir := s.hubConfigSnapshot().DataDir
	dir, ok := safeDeliveryFrozenDir(dataDir, deliveryID)
	if !ok {
		log.Printf("warn: skipping unsafe deliveryId %q during pending cleanup", deliveryID)
		return nil
	}
	shouldClean, err := s.store.ShouldCleanupDeliveryDir(ctx, deliveryID)
	if err != nil {
		return err
	}
	if !shouldClean {
		// Either already cleared or a new delivery row with this ID now exists: never delete its directory.
		return nil
	}
	if rmErr := s.removeFrozenDir(dir); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		log.Printf("warn: failed to remove frozen snapshot dir %s for delivery %s: %v", dir, deliveryID, rmErr)
		return rmErr
	}
	if clErr := s.store.ClearPendingDeliveryCleanup(ctx, deliveryID); clErr != nil {
		log.Printf("warn: failed to clear pending_delivery_cleanups for %s: %v", deliveryID, clErr)
		return clErr
	}
	return nil
}

func (s *Server) retryPendingDeliveryCleanups(ctx context.Context) {
	ids, err := s.store.ListPendingDeliveryCleanups(ctx)
	if err != nil {
		log.Printf("warn: list pending_delivery_cleanups failed: %v", err)
		return
	}
	for _, id := range ids {
		_ = s.cleanupPendingDeliveryDir(ctx, id)
	}
}

func (s *Server) failUndispatchedAttemptDetached(reqCtx context.Context, attemptID, errorCode, errorDetail string) error {
	failCtx, cancel := context.WithTimeout(context.WithoutCancel(reqCtx), 5*time.Second)
	defer cancel()
	failEvents, err := s.store.FailUndispatchedAttempt(failCtx, attemptID, errorCode, errorDetail)
	if err != nil {
		log.Printf("error: FailUndispatchedAttempt(%s, %s) failed: %v", attemptID, errorCode, err)
		return err
	}
	if len(failEvents) > 0 {
		s.broker.Broadcast(failEvents...)
	}
	return nil
}

func safeDeliveryFrozenDir(dataDir, deliveryID string) (string, bool) {
	deliveryID = strings.TrimSpace(deliveryID)
	if !isSafeDeliveryID(deliveryID) || strings.TrimSpace(dataDir) == "" {
		return "", false
	}
	base := filepath.Join(filepath.Clean(dataDir), "deliveries")
	target := filepath.Join(base, deliveryID)
	if !isSubPath(base, target) {
		return "", false
	}
	if fi, err := os.Lstat(target); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	return target, true
}

func (s *Server) createTaskInternal(
	ctx context.Context,
	projectID string,
	req protocol.CreateTaskRequest,
	actorKind protocol.ActorKind,
	actorID string,
	idempHeader string,
) (protocol.CreateTaskResponse, int, string, error) {
	resp, _, status, code, err := s.createTaskInternalWithPA(ctx, projectID, req, actorKind, actorID, idempHeader, nil)
	return resp, status, code, err
}

func (s *Server) createTaskInternalWithPA(
	ctx context.Context,
	projectID string,
	req protocol.CreateTaskRequest,
	actorKind protocol.ActorKind,
	actorID string,
	idempHeader string,
	paIdemp *store.PAActionIdempotencySpec,
) (protocol.CreateTaskResponse, *protocol.PAActionResult, int, string, error) {
	var resp protocol.CreateTaskResponse
	req.Goal = strings.TrimSpace(req.Goal)
	if req.Goal == "" {
		return resp, nil, http.StatusBadRequest, "ERR_EMPTY_GOAL", errors.New("goal is required")
	}
	if req.Adapter != "" && !req.Adapter.IsValid() {
		return resp, nil, http.StatusBadRequest, "ERR_INVALID_ADAPTER", fmt.Errorf("invalid adapter %q", req.Adapter)
	}

	proj, err := s.store.GetProject(ctx, projectID)
	if errors.Is(err, store.ErrNotFound) {
		return resp, nil, http.StatusNotFound, "ERR_PROJECT_NOT_FOUND", fmt.Errorf("project %s not found", projectID)
	}
	if err != nil {
		return resp, nil, http.StatusInternalServerError, "ERR_PROJECT_LOOKUP", err
	}

	route, err := s.resolveExecutionRouteWithLocation(proj, req.RequiredCapability, req.Adapter, req.MachineID, req.PartID, req.LocationID)
	if err != nil {
		return resp, nil, http.StatusBadRequest, "ERR_ROUTE_FAILED", err
	}
	if err := enforcePostRouteGuard(actorKind, route); err != nil {
		return resp, nil, http.StatusForbidden, "ERR_DEPLOY_FORBIDDEN", err
	}
	if st, code, err := s.validateAdapterSupport(route); err != nil {
		return resp, nil, st, code, err
	}
	// Resolve requested per-project worktree toggle in-memory first; persisted atomically inside
	// store.CreateTask AFTER idempotency checks confirm a genuinely new task.
	var desiredProjectIso protocol.LocationIsolation
	if req.UseWorktree != nil && !protocol.IsVirtualProjectID(projectID) && !route.IsPlainFolder {
		desiredProjectIso = protocol.IsolationInPlace
		if *req.UseWorktree {
			desiredProjectIso = protocol.IsolationWorktree
		}
		route.Isolation = desiredProjectIso
	}

	// If dispatching a dynamic UI remote Git project (in worktree mode) to an already-connected legacy Runner lacking bundle.v1, surface upgrade guidance early.
	if !route.IsPlainFolder && route.Isolation != protocol.IsolationInPlace && proj.Source == protocol.ProjectSourceUI &&
		route.MachineCfg.Transport != "" && route.MachineCfg.Transport != protocol.TransportLocalWS &&
		s.runners.IsConnected(route.MachineID) && !s.runners.HasCapability(route.MachineID, protocol.RunnerCapBundleV1) {
		return resp, nil, http.StatusUpgradeRequired, "ERR_RUNNER_UPGRADE_REQUIRED", fmt.Errorf(
			"machine %s is running an older orbit-runner without bundle.v1 delivery support; please upgrade orbit-runner on %s",
			route.MachineID, route.MachineID,
		)
	}

	// Merge project defaults -> machine agent defaultModel -> explicit request overrides.
	// Never apply a project's DefaultModelConfig.Model across a different adapter (e.g. agy model on claude/codex).
	desiredCfg := map[string]string{}
	s.mu.RLock()
	if pCfg, ok := s.projects.Projects[projectID]; ok {
		sameAdapter := pCfg.DefaultAdapter == "" || pCfg.DefaultAdapter == route.Adapter
		if sameAdapter && pCfg.DefaultModelConfig.Model != "" {
			desiredCfg["model"] = pCfg.DefaultModelConfig.Model
		}
		if sameAdapter && pCfg.DefaultModelConfig.ThoughtLevel != "" {
			desiredCfg["thought_level"] = pCfg.DefaultModelConfig.ThoughtLevel
		}
		if pCfg.DefaultModelConfig.Mode != "" {
			desiredCfg["mode"] = pCfg.DefaultModelConfig.Mode
		}
	}
	s.mu.RUnlock()
	if projectID == protocol.PAProjectID && desiredCfg["mode"] == "" {
		desiredCfg["mode"] = "auto"
	}
	if agentSettings, _, err := s.store.GetMachineAgentSettings(ctx, route.MachineID); err == nil {
		if ac, ok := agentSettings[string(route.Adapter)]; ok && strings.TrimSpace(ac.DefaultModel) != "" {
			desiredCfg["model"] = strings.TrimSpace(ac.DefaultModel)
		}
	}
	for k, v := range req.Config {
		if strings.TrimSpace(v) != "" {
			desiredCfg[k] = v
		}
	}

	triggerKind := protocol.TriggerInitial
	baseSHA := ""
	if req.ImportDirtyWorkspace && !protocol.IsVirtualProjectID(projectID) && strings.TrimSpace(route.Anchor.RepoPath) != "" {
		if route.MachineCfg.Transport != "" && route.MachineCfg.Transport != protocol.TransportLocalWS {
			return resp, nil, http.StatusBadRequest, "ERR_REMOTE_DIRTY_IMPORT_UNSUPPORTED", errors.New("远程机器项目暂不支持直接带入未提交改动，请先在该机器提交后再发起任务")
		}
		repoPath := protocol.ExpandPath(route.Anchor.RepoPath)
		snapID := "dlv_" + ulid.Make().String()
		pkg, err := gitops.PackageCheckpoint(ctx, gitops.PackageOptions{
			RepoDir:    repoPath,
			DeliveryID: snapID,
		})
		if err != nil {
			return resp, nil, http.StatusBadRequest, "ERR_DIRTY_IMPORT_FAILED", fmt.Errorf("import dirty workspace: %w", err)
		}
		if err := gitops.PushDeliveryRef(ctx, pkg.RepoRoot, proj.MirrorBarePath, snapID, pkg.HeadSHA); err != nil {
			return resp, nil, http.StatusInternalServerError, "ERR_DIRTY_IMPORT_PUSH", fmt.Errorf("push workspace snapshot: %w", err)
		}
		baseSHA = pkg.HeadSHA
		triggerKind = protocol.TriggerWorkspaceImport
	}

	idempKey := strings.TrimSpace(req.IdempotencyKey)
	if idempKey == "" {
		idempKey = strings.TrimSpace(idempHeader)
	}
	createdBy := protocol.TaskCreatedByHuman
	if actorKind == protocol.ActorPA {
		createdBy = protocol.TaskCreatedByPA
	}

	refMetas := s.resolveReferencedTaskMetas(ctx, req.ReferencedTaskIDs, "")
	triggerPayload := map[string]any{
		"prompt": req.Goal,
	}
	if req.ExplicitFullPermission {
		triggerPayload["explicitFullPermission"] = true
	}
	if len(refMetas) > 0 {
		triggerPayload["refTasks"] = refMetas
	}
	if req.SwarmIdleTimeoutMs > 0 {
		triggerPayload["swarmIdleTimeoutMs"] = req.SwarmIdleTimeoutMs
	}

	createAttempt := route.Adapter != protocol.AdapterSSHDeploy && route.Adapter != protocol.AdapterManual

	var anchorPtr *protocol.ProjectAnchorConfig
	if route.HasAnchor {
		a := route.Anchor
		anchorPtr = &a
	}

	res, err := s.store.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:           projectID,
		ParentTaskID:        strings.TrimSpace(req.ParentTaskID),
		Title:               req.Title,
		Goal:                req.Goal,
		RequiredCapability:  route.Capability,
		RequiredResources:   req.RequiredResources,
		Adapter:             route.Adapter,
		DesiredConfig:       desiredCfg,
		CreatedBy:           createdBy,
		IdempotencyKey:      idempKey,
		Sources:             req.Sources,
		MachineID:           route.MachineID,
		PartID:              route.PartID,
		LocationID:          route.LocationID,
		Isolation:           route.Isolation,
		RouteExplicit:       true,
		ProjectGitIsolation: desiredProjectIso,
		Anchor:              anchorPtr,
		AnchorCapability:    route.Capability,
		BaseSHA:             baseSHA,
		Prompt:              req.Goal,
		TriggerKind:         triggerKind,
		TriggerPayload:      triggerPayload,
		CreateAttempt:       createAttempt,
		LeaseTTLMs:          s.leaseTTL.Milliseconds(),
		ActorID:             actorID,
		ActorKind:           actorKind,
		PAIdempotency:       paIdemp,
	})
	if errors.Is(err, store.ErrNotFound) {
		return resp, nil, http.StatusNotFound, "ERR_NOT_FOUND", err
	}
	if errors.Is(err, store.ErrTaskArchived) {
		return resp, nil, http.StatusConflict, "ERR_TASK_ARCHIVED", err
	}
	if errors.Is(err, store.ErrConflict) {
		return resp, nil, http.StatusConflict, "ERR_CONFLICT", err
	}
	if errors.Is(err, store.ErrInvalidInput) {
		return resp, nil, http.StatusBadRequest, "ERR_INVALID_INPUT", err
	}
	if err != nil {
		return resp, nil, http.StatusInternalServerError, "ERR_CREATE_TASK", err
	}
	if res.ReplayedReceipt != nil {
		return resp, res.ReplayedReceipt, http.StatusOK, "", nil
	}

	if len(res.Events) > 0 {
		s.broker.Broadcast(res.Events...)
	}

	dispatched := false
	if res.Created && res.Attempt != nil {
		effectivePrompt, decErr := s.decorateAttemptPromptForAttempt(ctx, *res.Attempt, req.Goal, refMetas)
		if decErr != nil {
			log.Printf("hub: failed to build prompt opening for attempt %s: %v", res.Attempt.AttemptID, decErr)
			if failErr := s.failUndispatchedAttemptDetached(ctx, res.Attempt.AttemptID, "ERR_HEBI8_OPENING", decErr.Error()); failErr != nil {
				return resp, nil, http.StatusInternalServerError, "ERR_HEBI8_OPENING", fmt.Errorf("%w (cleanup failed: %v)", decErr, failErr)
			}
			return resp, nil, http.StatusInternalServerError, "ERR_HEBI8_OPENING", decErr
		}
		dispatched = s.dispatchAttemptRecord(ctx, *res.Attempt, effectivePrompt, req.SwarmIdleTimeoutMs)
	}

	var attView *protocol.AttemptView
	if res.Attempt != nil {
		v := res.Attempt.AttemptView
		attView = &v
	}

	resp = protocol.CreateTaskResponse{
		Task:       res.Task,
		Attempt:    attView,
		Sources:    res.Sources,
		Created:    res.Created,
		Dispatched: dispatched,
	}
	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
	}
	return resp, nil, status, "", nil
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	var req protocol.CreateTaskRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	resp, status, code, err := s.createTaskInternal(
		r.Context(),
		projectID,
		req,
		actorKindFromContext(r.Context()),
		actorFromContext(r.Context()),
		r.Header.Get("Idempotency-Key"),
	)
	if err != nil {
		writeAPIError(w, status, code, err.Error())
		return
	}
	writeJSON(w, status, resp)
}

func (s *Server) handleBindScratchTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	var req protocol.BindScratchTaskRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	task, ev, err := s.store.BindScratchTaskToProject(r.Context(), taskID, strings.TrimSpace(req.ProjectID), actorFromContext(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_NOT_FOUND", err.Error())
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeAPIError(w, http.StatusConflict, "ERR_CONFLICT", err.Error())
		return
	}
	if errors.Is(err, store.ErrInvalidInput) {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_INPUT", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_BIND_PROJECT", err.Error())
		return
	}
	if ev != nil {
		s.broker.Broadcast(*ev)
	}
	writeJSON(w, http.StatusOK, protocol.BindScratchTaskResponse{
		Task:          task,
		FromProjectID: protocol.ScratchProjectID,
		ToProjectID:   task.ProjectID,
	})
}

func (s *Server) handleGetAttemptRaw(w http.ResponseWriter, r *http.Request) {
	attemptID := chi.URLParam(r, "attemptId")
	var afterSeq int64
	if raw := r.URL.Query().Get("afterSeq"); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 0 {
			writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_AFTER_SEQ", "afterSeq must be non-negative")
			return
		}
		afterSeq = v
	}
	limit := 500
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			limit = v
		}
	}
	events, err := s.store.ListAttemptRawEvents(r.Context(), attemptID, afterSeq, limit)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_ATTEMPT_NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_ATTEMPT_RAW", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, protocol.AttemptRawEventsResponse{
		AttemptID: attemptID,
		Events:    events,
	})
}

func (s *Server) followupTaskInternal(
	ctx context.Context,
	taskID string,
	req protocol.FollowupTaskRequest,
	actorKind protocol.ActorKind,
	actorID string,
) (protocol.FollowupTaskResponse, int, string, error) {
	return s.followupTaskWithExtra(ctx, taskID, req, actorKind, actorID, nil)
}

func (s *Server) followupTaskWithExtra(
	ctx context.Context,
	taskID string,
	req protocol.FollowupTaskRequest,
	actorKind protocol.ActorKind,
	actorID string,
	payloadExtra map[string]any,
) (protocol.FollowupTaskResponse, int, string, error) {
	resp, _, status, code, err := s.followupTaskInternalWithPA(ctx, taskID, req, actorKind, actorID, payloadExtra, nil)
	return resp, status, code, err
}

func (s *Server) followupTaskInternalWithPA(
	ctx context.Context,
	taskID string,
	req protocol.FollowupTaskRequest,
	actorKind protocol.ActorKind,
	actorID string,
	payloadExtra map[string]any,
	paIdemp *store.PAActionIdempotencySpec,
) (protocol.FollowupTaskResponse, *protocol.PAActionResult, int, string, error) {
	var resp protocol.FollowupTaskResponse
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" {
		return resp, nil, http.StatusBadRequest, "ERR_EMPTY_MESSAGE", errors.New("message is required")
	}
	mode := req.Mode
	if mode == "" {
		mode = protocol.FollowupModeQueue
	}
	if !mode.IsValid() {
		return resp, nil, http.StatusBadRequest, "ERR_INVALID_MODE", fmt.Errorf("invalid followup mode %q", mode)
	}
	if req.Adapter != "" && !req.Adapter.IsValid() {
		return resp, nil, http.StatusBadRequest, "ERR_INVALID_ADAPTER", fmt.Errorf("invalid adapter %q", req.Adapter)
	}

	task, err := s.store.GetTask(ctx, taskID)
	if errors.Is(err, store.ErrNotFound) {
		return resp, nil, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err
	}
	if err != nil {
		return resp, nil, http.StatusInternalServerError, "ERR_TASK_LOOKUP", err
	}
	if task.ArchivedAt != nil {
		return resp, nil, http.StatusConflict, "ERR_TASK_ARCHIVED", errors.New("该会话已归档，请先恢复后再继续执行")
	}

	proj, err := s.store.GetProject(ctx, task.ProjectID)
	if err != nil {
		return resp, nil, http.StatusInternalServerError, "ERR_PROJECT_LOOKUP", err
	}

	refMetas := s.resolveReferencedTaskMetas(ctx, req.ReferencedTaskIDs, taskID)
	effPayloadExtra := payloadExtra
	if len(refMetas) > 0 {
		cloned := map[string]any{}
		for k, v := range payloadExtra {
			cloned[k] = v
		}
		cloned["refTasks"] = refMetas
		effPayloadExtra = cloned
	}

	// Preserve existing task machine/location/isolation pinning across follow-ups when caller didn't switch capability or machine.
	explicitMachine := strings.TrimSpace(req.MachineID)
	prevMachineID := ""
	prevPartID := ""
	prevLocID := ""
	var prevIsolation protocol.LocationIsolation
	if strings.TrimSpace(req.RequiredCapability) == "" {
		if tl, tlErr := s.store.GetTaskTimeline(ctx, taskID, 0, false); tlErr == nil && len(tl.Attempts) > 0 {
			last := tl.Attempts[len(tl.Attempts)-1]
			prevMachineID = last.MachineID
			prevPartID = last.PartID
			prevLocID = last.LocationID
			prevIsolation = last.Isolation
		}
	}
	targetMachineID := prevMachineID
	if explicitMachine != "" {
		if explicitMachine != prevMachineID {
			prevPartID = ""
			prevLocID = ""
			prevIsolation = ""
		}
		targetMachineID = explicitMachine
	}

	reqCap := strings.TrimSpace(req.RequiredCapability)
	if reqCap == "" {
		reqCap = task.RequiredCapability
	}
	reqAdapter := req.Adapter
	if reqAdapter == "" {
		reqAdapter = task.PreferredAdapter
	}
	route, err := s.resolveExecutionRouteWithLocation(proj, reqCap, reqAdapter, targetMachineID, prevPartID, prevLocID)
	if err != nil {
		if explicitMachine != "" {
			return resp, nil, http.StatusBadRequest, "ERR_ROUTE_FAILED", err
		}
		route, err = s.resolveExecutionRoute(proj, reqCap, reqAdapter)
		if err != nil {
			return resp, nil, http.StatusBadRequest, "ERR_ROUTE_FAILED", err
		}
	}
	if explicitMachine != "" && route.Adapter != protocol.AdapterManual && route.Adapter != protocol.AdapterSSHDeploy {
		if !s.runners.IsConnected(route.MachineID) {
			return resp, nil, http.StatusBadRequest, "ERR_MACHINE_OFFLINE", fmt.Errorf("machine %s is offline", route.MachineID)
		}
	}
	if err := enforcePostRouteGuard(actorKind, route); err != nil {
		return resp, nil, http.StatusForbidden, "ERR_DEPLOY_FORBIDDEN", err
	}
	if st, code, err := s.validateAdapterSupport(route); err != nil {
		return resp, nil, st, code, err
	}
	effectiveIsolation := route.Isolation
	if prevIsolation != "" && !route.IsPlainFolder {
		effectiveIsolation = prevIsolation
	}
	// If dispatching a dynamic UI remote Git project (in worktree mode) to an already-connected legacy Runner lacking bundle.v1, surface upgrade guidance early.
	if !protocol.IsVirtualProjectID(task.ProjectID) && !route.IsPlainFolder &&
		effectiveIsolation != protocol.IsolationInPlace &&
		proj.Source == protocol.ProjectSourceUI &&
		route.MachineCfg.Transport != "" && route.MachineCfg.Transport != protocol.TransportLocalWS &&
		s.runners.IsConnected(route.MachineID) && !s.runners.HasCapability(route.MachineID, protocol.RunnerCapBundleV1) {
		return resp, nil, http.StatusUpgradeRequired, "ERR_RUNNER_UPGRADE_REQUIRED", fmt.Errorf(
			"machine %s is running an older orbit-runner without bundle.v1 delivery support; please upgrade orbit-runner on %s",
			route.MachineID, route.MachineID,
		)
	}

	routeExplicit := strings.TrimSpace(req.RequiredCapability) != "" || explicitMachine != ""
	var anchorPtr *protocol.ProjectAnchorConfig
	if route.HasAnchor {
		a := route.Anchor
		anchorPtr = &a
	}

	// fork_new_session while task has an active attempt creates a linked child task immediately
	// seeded from task.LatestDeliveryID so the active session remains undisturbed.
	// S2: Inherit parent task's effective DesiredConfig, RequiredResources, location, isolation, and refs.
	if mode == protocol.FollowupModeForkNewSession && task.ActiveAttemptID != "" {
		createdBy := protocol.TaskCreatedByHuman
		if actorKind == protocol.ActorPA {
			createdBy = protocol.TaskCreatedByPA
		}
		parentCfg, parentRes, _ := s.store.GetTaskExecutionDefaults(ctx, task.TaskID)
		forkCfg := map[string]string{}
		for k, v := range parentCfg {
			forkCfg[k] = v
		}
		for k, v := range req.Config {
			if strings.TrimSpace(v) != "" {
				forkCfg[k] = v
			}
		}
		forkResources := req.RequiredResources
		if forkResources == nil {
			forkResources = append([]string(nil), parentRes...)
		}
		forkPartID := route.PartID
		if forkPartID == "" {
			forkPartID = prevPartID
		}
		forkLocID := route.LocationID
		if forkLocID == "" {
			forkLocID = prevLocID
		}
		forkTriggerPayload := map[string]any{
			"prompt":           req.Message,
			"forkedFromTaskId": task.TaskID,
		}
		if req.ExplicitFullPermission {
			forkTriggerPayload["explicitFullPermission"] = true
		}
		if len(refMetas) > 0 {
			forkTriggerPayload["refTasks"] = refMetas
		}
		forkRes, err := s.store.CreateTask(ctx, store.CreateTaskParams{
			ProjectID:          task.ProjectID,
			ParentTaskID:       task.TaskID,
			Title:              task.Title + " (分支)",
			Goal:               req.Message,
			RequiredCapability: route.Capability,
			RequiredResources:  forkResources,
			Adapter:            route.Adapter,
			DesiredConfig:      forkCfg,
			CreatedBy:          createdBy,
			MachineID:          route.MachineID,
			PartID:             forkPartID,
			LocationID:         forkLocID,
			Isolation:          effectiveIsolation,
			RouteExplicit:      routeExplicit,
			Anchor:             anchorPtr,
			AnchorCapability:   route.Capability,
			InputDeliveryID:    task.LatestDeliveryID,
			Prompt:             req.Message,
			TriggerKind:        protocol.TriggerFollowup,
			TriggerPayload:     forkTriggerPayload,
			CreateAttempt:      true,
			LeaseTTLMs:         s.leaseTTL.Milliseconds(),
			ActorID:            actorID,
			ActorKind:          actorKind,
			PAIdempotency:      paIdemp,
		})
		if errors.Is(err, store.ErrConflict) {
			return resp, nil, http.StatusConflict, "ERR_IDEMPOTENCY_CONFLICT", err
		}
		if err != nil {
			return resp, nil, http.StatusInternalServerError, "ERR_FORK_TASK", err
		}
		if forkRes.ReplayedReceipt != nil {
			return resp, forkRes.ReplayedReceipt, http.StatusOK, "", nil
		}
		if len(forkRes.Events) > 0 {
			s.broker.Broadcast(forkRes.Events...)
		}
		dispatched := false
		var attView *protocol.AttemptView
		if forkRes.Attempt != nil {
			effectivePrompt := s.decorateAttemptPrompt(ctx, task.ProjectID, req.Message, refMetas, task.ProjectID == protocol.PAProjectID)
			dispatched = s.dispatchAttemptRecord(ctx, *forkRes.Attempt, effectivePrompt, 0)
			v := forkRes.Attempt.AttemptView
			attView = &v
		}
		resp = protocol.FollowupTaskResponse{
			Mode:         mode,
			Task:         forkRes.Task,
			Attempt:      attView,
			ForkedTaskID: forkRes.Task.TaskID,
			Dispatched:   dispatched,
		}
		return resp, nil, http.StatusCreated, "", nil
	}

	// B9, B11, S2, NEW-3: Single BEGIN IMMEDIATE transaction decides busy vs idle, enforces needs_verification,
	// persists steer cancellation intent, stores server-only route_json, and commits PA idempotency receipt.
	src := protocol.PendingSourceHuman
	if actorKind == protocol.ActorPA {
		src = protocol.PendingSourcePA
	}
	subRes, err := s.store.SubmitFollowupAtomic(ctx, store.SubmitFollowupParams{
		TaskID:            taskID,
		Body:              req.Message,
		Mode:              mode,
		Source:            src,
		MachineID:         route.MachineID,
		Adapter:           route.Adapter,
		PartID:                 route.PartID,
		LocationID:             route.LocationID,
		Isolation:              effectiveIsolation,
		RouteExplicit:          routeExplicit,
		DesiredConfig:          req.Config,
		ExplicitFullPermission: req.ExplicitFullPermission,
		RequiredResources:      req.RequiredResources,
		Anchor:            anchorPtr,
		AnchorCapability:  route.Capability,
		VerifiedSite:      req.VerifiedSite,
		VerifiedAttemptID: strings.TrimSpace(req.VerifiedAttemptID),
		LeaseTTLMs:        s.leaseTTL.Milliseconds(),
		ActorKind:         actorKind,
		ActorID:           actorID,
		PayloadExtra:      effPayloadExtra,
		PAIdempotency:     paIdemp,
	})
	if errors.Is(err, store.ErrNotFound) {
		return resp, nil, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err
	}
	if errors.Is(err, store.ErrTaskArchived) {
		return resp, nil, http.StatusConflict, "ERR_TASK_ARCHIVED", errors.New("该会话已归档，请先恢复后再继续执行")
	}
	if errors.Is(err, store.ErrVerificationRequired) {
		return resp, nil, http.StatusConflict, "ERR_VERIFICATION_REQUIRED", err
	}
	if errors.Is(err, store.ErrConflict) {
		return resp, nil, http.StatusConflict, "ERR_CONFLICT", err
	}
	if errors.Is(err, store.ErrInvalidInput) {
		return resp, nil, http.StatusBadRequest, "ERR_INVALID_INPUT", err
	}
	if err != nil {
		return resp, nil, http.StatusInternalServerError, "ERR_FOLLOWUP", err
	}
	if subRes.ReplayedReceipt != nil {
		return resp, subRes.ReplayedReceipt, http.StatusOK, "", nil
	}

	if len(subRes.Events) > 0 {
		s.broker.Broadcast(subRes.Events...)
	}

	// If steer targeted a live dispatched attempt, send attempt.cancel immediately
	// (persisted cancel_requested=1 + steer_input_id ensures retry & auto-consume on cancel even if send fails).
	if subRes.ActiveToCancel != nil {
		_ = s.runners.CancelAttempt(subRes.ActiveToCancel.MachineID, protocol.AttemptCancelFrame{
			AttemptID:  subRes.ActiveToCancel.AttemptID,
			LeaseToken: subRes.ActiveToCancel.LeaseToken,
		})
		resp = protocol.FollowupTaskResponse{
			Mode:         mode,
			Task:         subRes.Task,
			PendingInput: subRes.PendingInput,
		}
		return resp, nil, http.StatusAccepted, "", nil
	}

	if subRes.PendingInput != nil {
		resp = protocol.FollowupTaskResponse{
			Mode:         mode,
			Task:         subRes.Task,
			PendingInput: subRes.PendingInput,
		}
		return resp, nil, http.StatusAccepted, "", nil
	}

	dispatched := false
	var attView *protocol.AttemptView
	if subRes.Attempt != nil {
		effectivePrompt, decErr := s.decorateAttemptPromptForAttempt(ctx, *subRes.Attempt, req.Message, refMetas)
		if decErr != nil {
			log.Printf("hub: failed to build fold opening for attempt %s: %v", subRes.Attempt.AttemptID, decErr)
			if failErr := s.failUndispatchedAttemptDetached(ctx, subRes.Attempt.AttemptID, "ERR_HEBI8_OPENING", decErr.Error()); failErr != nil {
				return resp, nil, http.StatusInternalServerError, "ERR_HEBI8_OPENING", fmt.Errorf("%w (cleanup failed: %v)", decErr, failErr)
			}
			return resp, nil, http.StatusInternalServerError, "ERR_HEBI8_OPENING", decErr
		}
		dispatched = s.dispatchAttemptRecord(ctx, *subRes.Attempt, effectivePrompt, 0)
		v := subRes.Attempt.AttemptView
		attView = &v
	}
	resp = protocol.FollowupTaskResponse{
		Mode:       mode,
		Task:       subRes.Task,
		Attempt:    attView,
		Dispatched: dispatched,
	}
	return resp, nil, http.StatusCreated, "", nil
}

func (s *Server) handleFollowupTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	var req protocol.FollowupTaskRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	resp, status, code, err := s.followupTaskInternal(r.Context(), taskID, req, actorKindFromContext(r.Context()), actorFromContext(r.Context()))
	if err != nil {
		writeAPIError(w, status, code, err.Error())
		return
	}
	writeJSON(w, status, resp)
}

func (s *Server) cancelTaskInternal(ctx context.Context, taskID, actorID, reason string) (protocol.CancelTaskResponse, int, string, error) {
	resp, _, status, code, err := s.cancelTaskInternalWithPA(ctx, taskID, actorID, reason, nil)
	return resp, status, code, err
}

func (s *Server) cancelTaskInternalWithPA(ctx context.Context, taskID, actorID, reason string, paIdemp *store.PAActionIdempotencySpec) (protocol.CancelTaskResponse, *protocol.PAActionResult, int, string, error) {
	var resp protocol.CancelTaskResponse
	// B10 & NEW-3: Perform idempotency check + cancel_requested=1 persistence + receipt storage in ONE tx!
	// Calling CancelTaskActiveAttemptWithIdempotency directly ensures replays succeed even after active_attempt_id is cleared.
	res, err := s.store.CancelTaskActiveAttemptWithIdempotency(ctx, taskID, actorID, reason, false, paIdemp)
	if errors.Is(err, store.ErrNotFound) {
		return resp, nil, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err
	}
	if errors.Is(err, store.ErrConflict) {
		return resp, nil, http.StatusConflict, "ERR_CONFLICT", err
	}
	if err != nil {
		return resp, nil, http.StatusInternalServerError, "ERR_CANCEL_TASK", err
	}
	if res.ReplayedReceipt != nil {
		return resp, res.ReplayedReceipt, http.StatusOK, "", nil
	}
	if len(res.Events) > 0 {
		s.broker.Broadcast(res.Events...)
	}

	cancelSent := false
	if res.NeedsRunnerSend && res.Attempt.MachineID != "" && s.runners.IsConnected(res.Attempt.MachineID) {
		if err := s.runners.CancelAttempt(res.Attempt.MachineID, protocol.AttemptCancelFrame{
			AttemptID:  res.Attempt.AttemptID,
			LeaseToken: res.Attempt.LeaseToken,
		}); err == nil {
			cancelSent = true
		}
	}

	immState := ""
	if res.CancelledNow {
		immState = string(protocol.AttemptCancelled)
	}
	resp = protocol.CancelTaskResponse{
		TaskID:         taskID,
		AttemptID:      res.Attempt.AttemptID,
		CancelSent:     cancelSent,
		ImmediateState: immState,
	}
	return resp, nil, http.StatusOK, "", nil
}

func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	var req protocol.CancelTaskRequest
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req)
	resp, status, code, err := s.cancelTaskInternal(r.Context(), taskID, actorFromContext(r.Context()), strings.TrimSpace(req.Reason))
	if err != nil {
		writeAPIError(w, status, code, err.Error())
		return
	}
	writeJSON(w, status, resp)
}

func (s *Server) handleRetryTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	var req protocol.RetryTaskRequest
	if r.Body != nil {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
			return
		}
	}

	task, err := s.store.GetTask(r.Context(), taskID)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_TASK_LOOKUP", err.Error())
		return
	}
	proj, err := s.store.GetProject(r.Context(), task.ProjectID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PROJECT_LOOKUP", err.Error())
		return
	}
	route, err := s.resolveExecutionRoute(proj, task.RequiredCapability, task.PreferredAdapter)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_ROUTE_FAILED", err.Error())
		return
	}
	if err := enforcePostRouteGuard(actorKindFromContext(r.Context()), route); err != nil {
		writeAPIError(w, http.StatusForbidden, "ERR_DEPLOY_FORBIDDEN", err.Error())
		return
	}
	if st, code, err := s.validateAdapterSupport(route); err != nil {
		writeAPIError(w, st, code, err.Error())
		return
	}

	var anchorPtr *protocol.ProjectAnchorConfig
	if route.HasAnchor {
		a := route.Anchor
		anchorPtr = &a
	}

	// B11: Pass empty Prompt so CreateNextAttempt preserves the original trigger prompt (e.g. annotation batch)
	// from the failed / needs_verification attempt and appends any retry note, falling back to task.Goal if absent.
	updatedTask, att, events, err := s.store.CreateNextAttempt(r.Context(), store.CreateNextAttemptParams{
		TaskID:            taskID,
		MachineID:         route.MachineID,
		Adapter:           route.Adapter,
		TriggerKind:       protocol.TriggerRetry,
		TriggerPayload:    map[string]any{"note": strings.TrimSpace(req.Note), "verifiedSite": req.VerifiedSite},
		Anchor:            anchorPtr,
		AnchorCapability:  route.Capability,
		VerifiedSite:      req.VerifiedSite,
		VerifiedAttemptID: strings.TrimSpace(req.VerifiedAttemptID),
		LeaseTTLMs:        s.leaseTTL.Milliseconds(),
		ActorKind:         actorKindFromContext(r.Context()),
		ActorID:           actorFromContext(r.Context()),
	})
	if errors.Is(err, store.ErrTaskArchived) {
		writeAPIError(w, http.StatusConflict, "ERR_TASK_ARCHIVED", "该会话已归档，请先恢复后再继续执行")
		return
	}
	if errors.Is(err, store.ErrVerificationRequired) {
		writeAPIError(w, http.StatusConflict, "ERR_VERIFICATION_REQUIRED", err.Error())
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeAPIError(w, http.StatusConflict, "ERR_TASK_BUSY", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_RETRY_TASK", err.Error())
		return
	}
	if len(events) > 0 {
		s.broker.Broadcast(events...)
	}
	dispatched := s.dispatchAttemptRecord(r.Context(), att, "", 0)
	writeJSON(w, http.StatusCreated, protocol.RetryTaskResponse{
		Task:       updatedTask,
		Attempt:    att.AttemptView,
		Dispatched: dispatched,
	})
}

func (s *Server) handleListQueue(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	items, err := s.store.ListPendingInputs(r.Context(), taskID)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_QUEUE_LIST", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleGetQueueInput(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	inputID := chi.URLParam(r, "inputId")
	pi, err := s.store.GetPendingInput(r.Context(), taskID, inputID)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_QUEUE_GET", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pi)
}

func (s *Server) handleEnqueueInput(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	var req protocol.CreatePendingInputRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	req.Body = strings.TrimSpace(req.Body)
	if req.Body == "" {
		writeAPIError(w, http.StatusBadRequest, "ERR_EMPTY_BODY", "body is required")
		return
	}
	pi, ev, err := s.store.EnqueuePendingInput(r.Context(), taskID, req.Kind, req.Source, req.Body, req.Payload, actorFromContext(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err.Error())
		return
	}
	if errors.Is(err, store.ErrTaskArchived) {
		writeAPIError(w, http.StatusConflict, "ERR_TASK_ARCHIVED", "该会话已归档，请先恢复后再继续执行")
		return
	}
	if errors.Is(err, store.ErrInvalidInput) {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_INPUT", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_QUEUE_ENQUEUE", err.Error())
		return
	}
	if ev != nil {
		s.broker.Broadcast(*ev)
	}
	writeJSON(w, http.StatusCreated, pi)
}

func (s *Server) handleUpdateQueueInput(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	inputID := chi.URLParam(r, "inputId")
	var req protocol.UpdatePendingInputRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	pi, ev, err := s.store.UpdatePendingInput(r.Context(), taskID, inputID, strings.TrimSpace(req.Body), req.Payload, actorFromContext(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_NOT_FOUND", err.Error())
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeAPIError(w, http.StatusConflict, "ERR_CONFLICT", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_QUEUE_UPDATE", err.Error())
		return
	}
	if ev != nil {
		s.broker.Broadcast(*ev)
	}
	writeJSON(w, http.StatusOK, pi)
}

func (s *Server) handleDeleteQueueInput(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	inputID := chi.URLParam(r, "inputId")
	ev, err := s.store.DeletePendingInput(r.Context(), taskID, inputID, actorFromContext(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_NOT_FOUND", err.Error())
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeAPIError(w, http.StatusConflict, "ERR_CONFLICT", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_QUEUE_DELETE", err.Error())
		return
	}
	if ev != nil {
		s.broker.Broadcast(*ev)
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "inputId": inputID})
}

func (s *Server) handleSteerQueueInput(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	inputID := chi.URLParam(r, "inputId")
	pi, activeToCancel, events, err := s.store.SteerPendingInput(r.Context(), taskID, inputID, actorFromContext(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_NOT_FOUND", err.Error())
		return
	}
	if errors.Is(err, store.ErrTaskArchived) {
		writeAPIError(w, http.StatusConflict, "ERR_TASK_ARCHIVED", "该会话已归档，请先恢复后再继续执行")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_QUEUE_STEER", err.Error())
		return
	}
	if len(events) > 0 {
		s.broker.Broadcast(events...)
	}

	if activeToCancel != nil {
		_ = s.runners.CancelAttempt(activeToCancel.MachineID, protocol.AttemptCancelFrame{
			AttemptID:  activeToCancel.AttemptID,
			LeaseToken: activeToCancel.LeaseToken,
		})
	} else {
		// Idle task (or undispatched leased attempt cancelled immediately): pop & dispatch the steered input now.
		s.consumeAndDispatchNextPendingInput(r.Context(), taskID, pi.InputID)
	}
	writeJSON(w, http.StatusOK, pi)
}

func (s *Server) handleReorderQueue(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	var req protocol.ReorderPendingInputsRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	items, ev, err := s.store.ReorderPendingInputs(r.Context(), taskID, req.InputIDs, actorFromContext(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err.Error())
		return
	}
	if errors.Is(err, store.ErrTaskArchived) {
		writeAPIError(w, http.StatusConflict, "ERR_TASK_ARCHIVED", "该会话已归档，请先恢复后再继续执行")
		return
	}
	if errors.Is(err, store.ErrInvalidInput) {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_INPUT", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_QUEUE_REORDER", err.Error())
		return
	}
	if ev != nil {
		s.broker.Broadcast(*ev)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleDispatchAnnotations(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	var req protocol.DispatchAnnotationsRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}

	task, err := s.store.GetTask(r.Context(), taskID)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_TASK_LOOKUP", err.Error())
		return
	}
	proj, err := s.store.GetProject(r.Context(), task.ProjectID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PROJECT_LOOKUP", err.Error())
		return
	}
	route, err := s.resolveExecutionRoute(proj, task.RequiredCapability, task.PreferredAdapter)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_ROUTE_FAILED", err.Error())
		return
	}
	if err := enforcePostRouteGuard(actorKindFromContext(r.Context()), route); err != nil {
		writeAPIError(w, http.StatusForbidden, "ERR_DEPLOY_FORBIDDEN", err.Error())
		return
	}
	if st, code, err := s.validateAdapterSupport(route); err != nil {
		writeAPIError(w, st, code, err.Error())
		return
	}

	var anchorPtr *protocol.ProjectAnchorConfig
	if route.HasAnchor {
		a := route.Anchor
		anchorPtr = &a
	}

	res, err := s.store.DispatchAnnotations(r.Context(), store.DispatchAnnotationsParams{
		TaskID:            taskID,
		AnnotationIDs:     req.AnnotationIDs,
		Message:           req.Message,
		MachineID:         route.MachineID,
		Adapter:           route.Adapter,
		Anchor:            anchorPtr,
		AnchorCapability:  route.Capability,
		ForceNewSession:   req.Mode == protocol.FollowupModeForkNewSession,
		VerifiedSite:      req.VerifiedSite,
		VerifiedAttemptID: strings.TrimSpace(req.VerifiedAttemptID),
		LeaseTTLMs:        s.leaseTTL.Milliseconds(),
		ActorID:           actorFromContext(r.Context()),
	})
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err.Error())
		return
	}
	if errors.Is(err, store.ErrTaskArchived) {
		writeAPIError(w, http.StatusConflict, "ERR_TASK_ARCHIVED", "该会话已归档，请先恢复后再继续执行")
		return
	}
	if errors.Is(err, store.ErrInvalidInput) {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_INPUT", err.Error())
		return
	}
	if errors.Is(err, store.ErrVerificationRequired) {
		writeAPIError(w, http.StatusConflict, "ERR_VERIFICATION_REQUIRED", err.Error())
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeAPIError(w, http.StatusConflict, "ERR_ANNOTATION_CONFLICT", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_DISPATCH_ANNOTATIONS", err.Error())
		return
	}

	if len(res.Events) > 0 {
		s.broker.Broadcast(res.Events...)
	}

	dispatched := false
	var attView *protocol.AttemptView
	if res.Attempt != nil {
		dispatched = s.dispatchAttemptRecord(r.Context(), *res.Attempt, "", 0)
		v := res.Attempt.AttemptView
		attView = &v
	}

	writeJSON(w, http.StatusOK, protocol.DispatchAnnotationsResponse{
		TaskID:       taskID,
		QueuedCount:  res.QueuedCount,
		Attempt:      attView,
		PendingInput: res.PendingInput,
		Dispatched:   dispatched,
	})
}

func (s *Server) handleGetTaskBoard(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	var beforeSeq int64
	if raw := strings.TrimSpace(r.URL.Query().Get("beforeSeq")); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 0 {
			writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_BEFORE_SEQ", "beforeSeq must be a non-negative integer")
			return
		}
		beforeSeq = v
	}
	limit := 200
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 {
			writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_LIMIT", "limit must be a positive integer")
			return
		}
		limit = v
	}
	board, err := s.store.GetTaskBoardPage(r.Context(), taskID, beforeSeq, limit)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_BOARD_GET", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, board)
}

func (s *Server) handlePostBoardMessage(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	var req protocol.PostBoardMessageRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	author := strings.TrimSpace(req.AuthorName)
	if author == "" {
		author = "human"
	}
	extKey := ""
	if idemp := strings.TrimSpace(r.Header.Get("Idempotency-Key")); idemp != "" {
		extKey = "board:" + idemp
	}
	msg, ev, err := s.store.PostBoardMessage(r.Context(), taskID, author, req.TargetMention, req.Body, extKey)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err.Error())
		return
	}
	if errors.Is(err, store.ErrTaskArchived) {
		writeAPIError(w, http.StatusConflict, "ERR_TASK_ARCHIVED", "该会话已归档，请先恢复后再继续执行")
		return
	}
	if errors.Is(err, store.ErrInvalidInput) {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_INPUT", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_BOARD_POST", err.Error())
		return
	}
	if ev != nil {
		s.broker.Broadcast(*ev)
	}

	// B16: Route @worker / @member board messages onto a real executable delivery path so
	// status transitions posted -> consumed when dispatched/delivered.
	if msg.TargetMention != "" && msg.Status == protocol.BoardMessagePosted {
		target, _ := s.store.ResolveBoardTarget(r.Context(), taskID, msg.TargetMention)
		actorKind := actorKindFromContext(r.Context())
		actorID := actorFromContext(r.Context())
		if target != nil && target.WorkerTaskID != "" {
			// Child-task worker: deliver directly via its followup/queue path.
			fResp, _, _, fErr := s.followupTaskWithExtra(r.Context(), target.WorkerTaskID, protocol.FollowupTaskRequest{
				Message: fmt.Sprintf("[Board @%s] %s", target.MemberName, msg.Body),
				Mode:    protocol.FollowupModeQueue,
			}, actorKind, actorID, map[string]any{
				"boardMessageId":  msg.MessageID,
				"boardTaskId":     taskID,
				"boardMemberName": target.MemberName,
				"boardSeq":        msg.Seq,
			})
			if fErr == nil && fResp.Attempt != nil {
				msg.Status = protocol.BoardMessageConsumed
			}
		} else {
			// Native Swarm worker (or coordinator mention): forward live into active swarm_monitoring attempt,
			// or fall back to parent task's followup/queue path.
			forwardedLive := false
			if parentTask, err := s.store.GetTask(r.Context(), taskID); err == nil && parentTask.ActiveAttemptID != "" {
				if attRec, err := s.store.GetAttemptRecord(r.Context(), parentTask.ActiveAttemptID); err == nil &&
					attRec.State == protocol.AttemptSwarmMonitoring && s.runners.IsConnected(attRec.MachineID) {
					memberName := msg.TargetMention
					if target != nil && target.MemberName != "" {
						memberName = target.MemberName
					}
					if sendErr := s.runners.SendBoardMessage(attRec.MachineID, protocol.BoardMessageDispatchFrame{
						Type:          protocol.RunnerMsgBoardMessage,
						AttemptID:     attRec.AttemptID,
						TaskID:        taskID,
						MessageID:     msg.MessageID,
						Seq:           msg.Seq,
						AuthorName:    msg.AuthorName,
						TargetMention: memberName,
						Body:          msg.Body,
					}); sendErr == nil {
						forwardedLive = true
					}
				}
			}
			if !forwardedLive {
				memberName := msg.TargetMention
				if target != nil && target.MemberName != "" {
					memberName = target.MemberName
				}
				fResp, _, _, fErr := s.followupTaskWithExtra(r.Context(), taskID, protocol.FollowupTaskRequest{
					Message: fmt.Sprintf("[Board @%s] %s", memberName, msg.Body),
					Mode:    protocol.FollowupModeQueue,
				}, actorKind, actorID, map[string]any{
					"boardMessageId":  msg.MessageID,
					"boardTaskId":     taskID,
					"boardMemberName": memberName,
					"boardSeq":        msg.Seq,
				})
				if fErr == nil && fResp.Attempt != nil {
					msg.Status = protocol.BoardMessageConsumed
				}
			}
		}
	}

	writeJSON(w, http.StatusCreated, msg)
}

func (s *Server) handleGetWorkerStream(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	workerName := chi.URLParam(r, "workerName")
	stream, err := s.store.GetWorkerStream(r.Context(), taskID, workerName)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_WORKER_STREAM", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, stream)
}

func (s *Server) handleListTaskSources(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	sources, err := s.store.ListTaskSources(r.Context(), taskID)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_SOURCES_LIST", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": sources})
}

func (s *Server) handleAddTaskSource(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	var req protocol.CreateTaskSourceInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	src, ev, err := s.store.AddTaskSource(r.Context(), taskID, req, actorFromContext(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err.Error())
		return
	}
	if errors.Is(err, store.ErrTaskArchived) {
		writeAPIError(w, http.StatusConflict, "ERR_TASK_ARCHIVED", "该会话已归档，请先恢复后再继续执行")
		return
	}
	if errors.Is(err, store.ErrInvalidInput) {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_INPUT", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_SOURCES_ADD", err.Error())
		return
	}
	if ev != nil {
		s.broker.Broadcast(*ev)
	}
	writeJSON(w, http.StatusCreated, src)
}

func (s *Server) handleDeleteTaskSource(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	sourceID := chi.URLParam(r, "sourceId")
	ev, err := s.store.DeleteTaskSource(r.Context(), taskID, sourceID, actorFromContext(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_SOURCES_DELETE", err.Error())
		return
	}
	if ev != nil {
		s.broker.Broadcast(*ev)
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "sourceId": sourceID})
}

func (s *Server) handleArchiveTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	var req protocol.ArchiveTaskRequest
	if r.Body != nil {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
			return
		}
	}
	actorID := actorFromContext(r.Context())

	cancelRequestedPending := false
	if req.CancelActive {
		// Atomically verify task-tree protection (e.g. Hebi8 main conversation) and cancel/request cancel
		// inside ONE BEGIN IMMEDIATE SQLite write transaction to eliminate TOCTOU races.
		cancelRes, cErr := s.store.CancelActiveAttemptIfArchivable(r.Context(), taskID, actorID, "归档前自动取消")
		if cErr == nil {
			if len(cancelRes.Events) > 0 {
				s.broker.Broadcast(cancelRes.Events...)
			}
			if cancelRes.NeedsRunnerSend && cancelRes.Attempt.MachineID != "" && s.runners.IsConnected(cancelRes.Attempt.MachineID) {
				_ = s.runners.CancelAttempt(cancelRes.Attempt.MachineID, protocol.AttemptCancelFrame{
					AttemptID:  cancelRes.Attempt.AttemptID,
					LeaseToken: cancelRes.Attempt.LeaseToken,
				})
			}
			if !cancelRes.CancelledNow {
				cancelRequestedPending = true
			}
		} else if errors.Is(cErr, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "ERR_TASK_NOT_FOUND", cErr.Error())
			return
		} else if errors.Is(cErr, store.ErrProtectedTask) || errors.Is(cErr, store.ErrForbidden) {
			writeAPIError(w, http.StatusConflict, "ERR_FORBIDDEN", cErr.Error())
			return
		} else if !errors.Is(cErr, store.ErrConflict) {
			writeAPIError(w, http.StatusInternalServerError, "ERR_CANCEL_TASK", cErr.Error())
			return
		}
	}

	task, ev, err := s.store.ArchiveTask(r.Context(), taskID, actorID)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err.Error())
		return
	}
	if errors.Is(err, store.ErrTaskActive) {
		msg := "会话正在运行中，请先停止或勾选取消后再归档"
		if cancelRequestedPending {
			msg = "已发送停止请求，请等待当前运行确认停止后再归档"
		}
		writeAPIError(w, http.StatusConflict, "ERR_TASK_ACTIVE", msg)
		return
	}
	if errors.Is(err, store.ErrForbidden) {
		writeAPIError(w, http.StatusConflict, "ERR_FORBIDDEN", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_ARCHIVE_TASK", err.Error())
		return
	}
	if ev != nil {
		s.broker.Broadcast(*ev)
	}
	writeJSON(w, http.StatusOK, protocol.ArchiveTaskResponse{Task: task})
}

func (s *Server) handleUnarchiveTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	task, ev, err := s.store.UnarchiveTask(r.Context(), taskID, actorFromContext(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_UNARCHIVE_TASK", err.Error())
		return
	}
	if ev != nil {
		s.broker.Broadcast(*ev)
	}
	writeJSON(w, http.StatusOK, protocol.ArchiveTaskResponse{Task: task})
}

func (s *Server) handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	projectID, deliveryIDs, ev, err := s.store.DeleteTask(r.Context(), taskID, actorFromContext(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err.Error())
		return
	}
	if errors.Is(err, store.ErrTaskActive) {
		writeAPIError(w, http.StatusConflict, "ERR_TASK_ACTIVE", "会话正在运行中，请先停止后再永久删除")
		return
	}
	if errors.Is(err, store.ErrForbidden) {
		writeAPIError(w, http.StatusConflict, "ERR_FORBIDDEN", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_DELETE_TASK", err.Error())
		return
	}
	// Safely remove frozen delivery snapshot directories ONLY under this Hub instance's dataDir/deliveries/<deliveryId>.
	// DeleteTask already recorded every deleted deliveryID into SQLite pending_delivery_cleanups inside its write tx;
	// use a detached 30s timeout context so cleanup waits for any in-flight registration lock even if the HTTP client disconnects.
	cleanCtx, cleanCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cleanCancel()
	s.retryPendingDeliveryCleanups(cleanCtx)
	_ = deliveryIDs
	if ev != nil {
		s.broker.Broadcast(*ev)
	}
	writeJSON(w, http.StatusOK, protocol.DeleteTaskResponse{
		TaskID:    taskID,
		ProjectID: projectID,
		Deleted:   true,
	})
}
