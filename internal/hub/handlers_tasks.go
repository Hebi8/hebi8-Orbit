package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/oklog/ulid/v2"

	"github.com/Hebi8/hebi8-Orbit/internal/gitops"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

func (s *Server) createTaskInternal(
	ctx context.Context,
	projectID string,
	req protocol.CreateTaskRequest,
	actorKind protocol.ActorKind,
	actorID string,
	idempHeader string,
) (protocol.CreateTaskResponse, int, string, error) {
	var resp protocol.CreateTaskResponse
	req.Goal = strings.TrimSpace(req.Goal)
	if req.Goal == "" {
		return resp, http.StatusBadRequest, "ERR_EMPTY_GOAL", errors.New("goal is required")
	}
	if req.Adapter != "" && !req.Adapter.IsValid() {
		return resp, http.StatusBadRequest, "ERR_INVALID_ADAPTER", fmt.Errorf("invalid adapter %q", req.Adapter)
	}

	proj, err := s.store.GetProject(ctx, projectID)
	if errors.Is(err, store.ErrNotFound) {
		return resp, http.StatusNotFound, "ERR_PROJECT_NOT_FOUND", fmt.Errorf("project %s not found", projectID)
	}
	if err != nil {
		return resp, http.StatusInternalServerError, "ERR_PROJECT_LOOKUP", err
	}

	route, err := s.resolveExecutionRoute(proj, req.RequiredCapability, req.Adapter)
	if err != nil {
		return resp, http.StatusBadRequest, "ERR_ROUTE_FAILED", err
	}
	if err := enforcePostRouteGuard(actorKind, route); err != nil {
		return resp, http.StatusForbidden, "ERR_DEPLOY_FORBIDDEN", err
	}

	// Merge default project model config with request overrides
	desiredCfg := map[string]string{}
	s.mu.RLock()
	if pCfg, ok := s.projects.Projects[projectID]; ok {
		if pCfg.DefaultModelConfig.Model != "" {
			desiredCfg["model"] = pCfg.DefaultModelConfig.Model
		}
		if pCfg.DefaultModelConfig.ThoughtLevel != "" {
			desiredCfg["thought_level"] = pCfg.DefaultModelConfig.ThoughtLevel
		}
		if pCfg.DefaultModelConfig.Mode != "" {
			desiredCfg["mode"] = pCfg.DefaultModelConfig.Mode
		}
	}
	s.mu.RUnlock()
	for k, v := range req.Config {
		if strings.TrimSpace(v) != "" {
			desiredCfg[k] = v
		}
	}

	triggerKind := protocol.TriggerInitial
	baseSHA := ""
	if req.ImportDirtyWorkspace && projectID != protocol.ScratchProjectID && strings.TrimSpace(route.Anchor.RepoPath) != "" {
		repoPath := protocol.ExpandPath(route.Anchor.RepoPath)
		snapID := "dlv_" + ulid.Make().String()
		pkg, err := gitops.PackageCheckpoint(ctx, gitops.PackageOptions{
			RepoDir:    repoPath,
			DeliveryID: snapID,
		})
		if err != nil {
			return resp, http.StatusBadRequest, "ERR_DIRTY_IMPORT_FAILED", fmt.Errorf("import dirty workspace: %w", err)
		}
		if err := gitops.PushDeliveryRef(ctx, pkg.RepoRoot, proj.MirrorBarePath, snapID, pkg.HeadSHA); err != nil {
			return resp, http.StatusInternalServerError, "ERR_DIRTY_IMPORT_PUSH", fmt.Errorf("push workspace snapshot: %w", err)
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

	triggerPayload := map[string]any{
		"prompt": req.Goal,
	}
	if req.SwarmIdleTimeoutMs > 0 {
		triggerPayload["swarmIdleTimeoutMs"] = req.SwarmIdleTimeoutMs
	}

	createAttempt := route.Adapter != protocol.AdapterSSHDeploy && route.Adapter != protocol.AdapterManual

	res, err := s.store.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:          projectID,
		ParentTaskID:       strings.TrimSpace(req.ParentTaskID),
		Title:              req.Title,
		Goal:               req.Goal,
		RequiredCapability: route.Capability,
		RequiredResources:  req.RequiredResources,
		Adapter:            route.Adapter,
		DesiredConfig:      desiredCfg,
		CreatedBy:          createdBy,
		IdempotencyKey:     idempKey,
		Sources:            req.Sources,
		MachineID:          route.MachineID,
		BaseSHA:            baseSHA,
		Prompt:             req.Goal,
		TriggerKind:        triggerKind,
		TriggerPayload:     triggerPayload,
		CreateAttempt:      createAttempt,
		LeaseTTLMs:         s.leaseTTL.Milliseconds(),
		ActorID:            actorID,
		ActorKind:          actorKind,
	})
	if errors.Is(err, store.ErrNotFound) {
		return resp, http.StatusNotFound, "ERR_NOT_FOUND", err
	}
	if errors.Is(err, store.ErrConflict) {
		return resp, http.StatusConflict, "ERR_CONFLICT", err
	}
	if errors.Is(err, store.ErrInvalidInput) {
		return resp, http.StatusBadRequest, "ERR_INVALID_INPUT", err
	}
	if err != nil {
		return resp, http.StatusInternalServerError, "ERR_CREATE_TASK", err
	}

	if len(res.Events) > 0 {
		s.broker.Broadcast(res.Events...)
	}

	dispatched := false
	if res.Created && res.Attempt != nil {
		dispatched = s.dispatchAttemptRecord(ctx, *res.Attempt, req.Goal, req.SwarmIdleTimeoutMs)
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
	return resp, status, "", nil
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
	var resp protocol.FollowupTaskResponse
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" {
		return resp, http.StatusBadRequest, "ERR_EMPTY_MESSAGE", errors.New("message is required")
	}
	mode := req.Mode
	if mode == "" {
		mode = protocol.FollowupModeQueue
	}
	if !mode.IsValid() {
		return resp, http.StatusBadRequest, "ERR_INVALID_MODE", fmt.Errorf("invalid followup mode %q", mode)
	}
	if req.Adapter != "" && !req.Adapter.IsValid() {
		return resp, http.StatusBadRequest, "ERR_INVALID_ADAPTER", fmt.Errorf("invalid adapter %q", req.Adapter)
	}

	task, err := s.store.GetTask(ctx, taskID)
	if errors.Is(err, store.ErrNotFound) {
		return resp, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err
	}
	if err != nil {
		return resp, http.StatusInternalServerError, "ERR_TASK_LOOKUP", err
	}

	proj, err := s.store.GetProject(ctx, task.ProjectID)
	if err != nil {
		return resp, http.StatusInternalServerError, "ERR_PROJECT_LOOKUP", err
	}

	reqCap := strings.TrimSpace(req.RequiredCapability)
	if reqCap == "" {
		reqCap = task.RequiredCapability
	}
	reqAdapter := req.Adapter
	if reqAdapter == "" {
		reqAdapter = task.PreferredAdapter
	}
	route, err := s.resolveExecutionRoute(proj, reqCap, reqAdapter)
	if err != nil {
		return resp, http.StatusBadRequest, "ERR_ROUTE_FAILED", err
	}
	if err := enforcePostRouteGuard(actorKind, route); err != nil {
		return resp, http.StatusForbidden, "ERR_DEPLOY_FORBIDDEN", err
	}

	// If task currently has an active attempt (e.g. running / swarm_monitoring / waiting_approval):
	// NEVER shove a prompt into the active provider session!
	if task.ActiveAttemptID != "" {
		switch mode {
		case protocol.FollowupModeQueue:
			src := protocol.PendingSourceHuman
			if actorKind == protocol.ActorPA {
				src = protocol.PendingSourcePA
			}
			pi, ev, err := s.store.EnqueuePendingInput(ctx, taskID, protocol.PendingInputFollowup, src, req.Message, map[string]any{
				"prompt":             req.Message,
				"requiredCapability": route.Capability,
				"adapter":            route.Adapter,
				"config":             req.Config,
			}, actorID)
			if err != nil {
				return resp, http.StatusInternalServerError, "ERR_QUEUE_ENQUEUE", err
			}
			if ev != nil {
				s.broker.Broadcast(*ev)
			}
			updatedTask, _ := s.store.GetTask(ctx, taskID)
			resp = protocol.FollowupTaskResponse{
				Mode:         mode,
				Task:         updatedTask,
				PendingInput: &pi,
			}
			return resp, http.StatusAccepted, "", nil

		case protocol.FollowupModeSteer:
			src := protocol.PendingSourceHuman
			if actorKind == protocol.ActorPA {
				src = protocol.PendingSourcePA
			}
			pi, ev1, err := s.store.EnqueuePendingInput(ctx, taskID, protocol.PendingInputFollowup, src, req.Message, map[string]any{
				"prompt":             req.Message,
				"requiredCapability": route.Capability,
				"adapter":            route.Adapter,
				"config":             req.Config,
			}, actorID)
			if err != nil {
				return resp, http.StatusInternalServerError, "ERR_QUEUE_ENQUEUE", err
			}
			promoted, activeAttID, ev2, err := s.store.SteerPendingInput(ctx, taskID, pi.InputID, actorID)
			if err != nil {
				return resp, http.StatusInternalServerError, "ERR_QUEUE_STEER", err
			}
			if ev1 != nil {
				s.broker.Broadcast(*ev1)
			}
			if ev2 != nil {
				s.broker.Broadcast(*ev2)
			}
			if activeAttID != "" {
				if attRec, err := s.store.GetAttemptRecord(ctx, activeAttID); err == nil {
					_ = s.runners.CancelAttempt(attRec.MachineID, protocol.AttemptCancelFrame{
						AttemptID:  attRec.AttemptID,
						LeaseToken: attRec.LeaseToken,
					})
				}
			}
			updatedTask, _ := s.store.GetTask(ctx, taskID)
			resp = protocol.FollowupTaskResponse{
				Mode:         mode,
				Task:         updatedTask,
				PendingInput: &promoted,
			}
			return resp, http.StatusAccepted, "", nil

		case protocol.FollowupModeForkNewSession:
			// Branch immediately onto a linked child task with a fresh wb_session_id & isolated worktree
			// seeded from task.LatestDeliveryID, leaving the active attempt's session completely untouched.
			createdBy := protocol.TaskCreatedByHuman
			if actorKind == protocol.ActorPA {
				createdBy = protocol.TaskCreatedByPA
			}
			forkRes, err := s.store.CreateTask(ctx, store.CreateTaskParams{
				ProjectID:          task.ProjectID,
				ParentTaskID:       task.TaskID,
				Title:              task.Title + " (分支)",
				Goal:               req.Message,
				RequiredCapability: route.Capability,
				RequiredResources:  req.RequiredResources,
				Adapter:            route.Adapter,
				DesiredConfig:      req.Config,
				CreatedBy:          createdBy,
				MachineID:          route.MachineID,
				InputDeliveryID:    task.LatestDeliveryID,
				Prompt:             req.Message,
				TriggerKind:        protocol.TriggerFollowup,
				TriggerPayload:     map[string]any{"prompt": req.Message, "forkedFromTaskId": task.TaskID},
				CreateAttempt:      true,
				LeaseTTLMs:         s.leaseTTL.Milliseconds(),
				ActorID:            actorID,
				ActorKind:          actorKind,
			})
			if err != nil {
				return resp, http.StatusInternalServerError, "ERR_FORK_TASK", err
			}
			if len(forkRes.Events) > 0 {
				s.broker.Broadcast(forkRes.Events...)
			}
			dispatched := false
			var attView *protocol.AttemptView
			if forkRes.Attempt != nil {
				dispatched = s.dispatchAttemptRecord(ctx, *forkRes.Attempt, req.Message, 0)
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
			return resp, http.StatusCreated, "", nil
		}
	}

	// Task is currently idle -> start next attempt directly (fork_new_session forces brand new wb_session_id)
	updatedTask, att, events, err := s.store.CreateNextAttempt(ctx, store.CreateNextAttemptParams{
		TaskID:          taskID,
		MachineID:       route.MachineID,
		Adapter:         route.Adapter,
		TriggerKind:     protocol.TriggerFollowup,
		TriggerPayload:  map[string]any{"prompt": req.Message},
		Prompt:          req.Message,
		DesiredConfig:   req.Config,
		ForceNewSession: mode == protocol.FollowupModeForkNewSession,
		LeaseTTLMs:      s.leaseTTL.Milliseconds(),
		ActorKind:       actorKind,
		ActorID:         actorID,
	})
	if errors.Is(err, store.ErrConflict) {
		return resp, http.StatusConflict, "ERR_TASK_BUSY", err
	}
	if err != nil {
		return resp, http.StatusInternalServerError, "ERR_FOLLOWUP", err
	}
	if len(events) > 0 {
		s.broker.Broadcast(events...)
	}
	dispatched := s.dispatchAttemptRecord(ctx, att, req.Message, 0)
	v := att.AttemptView
	resp = protocol.FollowupTaskResponse{
		Mode:       mode,
		Task:       updatedTask,
		Attempt:    &v,
		Dispatched: dispatched,
	}
	return resp, http.StatusCreated, "", nil
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
	var resp protocol.CancelTaskResponse
	task, err := s.store.GetTask(ctx, taskID)
	if errors.Is(err, store.ErrNotFound) {
		return resp, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err
	}
	if err != nil {
		return resp, http.StatusInternalServerError, "ERR_TASK_LOOKUP", err
	}
	if task.ActiveAttemptID == "" {
		return resp, http.StatusConflict, "ERR_NO_ACTIVE_ATTEMPT", errors.New("task has no active attempt to cancel")
	}
	attRec, err := s.store.GetAttemptRecord(ctx, task.ActiveAttemptID)
	if err != nil {
		return resp, http.StatusInternalServerError, "ERR_ATTEMPT_LOOKUP", err
	}
	runnerConnected := s.runners.IsConnected(attRec.MachineID)
	res, err := s.store.CancelTaskActiveAttempt(ctx, taskID, actorID, reason, !runnerConnected)
	if errors.Is(err, store.ErrNotFound) {
		return resp, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err
	}
	if errors.Is(err, store.ErrConflict) {
		return resp, http.StatusConflict, "ERR_NO_ACTIVE_ATTEMPT", err
	}
	if err != nil {
		return resp, http.StatusInternalServerError, "ERR_CANCEL_TASK", err
	}
	if len(res.Events) > 0 {
		s.broker.Broadcast(res.Events...)
	}

	cancelSent := false
	if res.NeedsRunnerSend && runnerConnected {
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
	return resp, http.StatusOK, "", nil
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

	prompt := task.Goal
	if strings.TrimSpace(req.Note) != "" {
		prompt = prompt + "\n\n重试补充说明: " + strings.TrimSpace(req.Note)
	}

	updatedTask, att, events, err := s.store.CreateNextAttempt(r.Context(), store.CreateNextAttemptParams{
		TaskID:         taskID,
		MachineID:      route.MachineID,
		Adapter:        route.Adapter,
		TriggerKind:    protocol.TriggerRetry,
		TriggerPayload: map[string]any{"prompt": prompt, "note": req.Note, "verifiedSite": req.VerifiedSite},
		Prompt:         prompt,
		VerifiedSite:   req.VerifiedSite,
		LeaseTTLMs:     s.leaseTTL.Milliseconds(),
		ActorKind:      actorKindFromContext(r.Context()),
		ActorID:        actorFromContext(r.Context()),
	})
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
	dispatched := s.dispatchAttemptRecord(r.Context(), att, prompt, 0)
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
	pi, activeAttID, ev, err := s.store.SteerPendingInput(r.Context(), taskID, inputID, actorFromContext(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_QUEUE_STEER", err.Error())
		return
	}
	if ev != nil {
		s.broker.Broadcast(*ev)
	}

	if activeAttID != "" {
		if attRec, err := s.store.GetAttemptRecord(r.Context(), activeAttID); err == nil {
			_ = s.runners.CancelAttempt(attRec.MachineID, protocol.AttemptCancelFrame{
				AttemptID:  attRec.AttemptID,
				LeaseToken: attRec.LeaseToken,
			})
		}
	} else {
		// Idle task: immediately pop & dispatch the steered head input
		s.consumeAndDispatchNextPendingInput(r.Context(), taskID)
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

	res, err := s.store.DispatchAnnotations(r.Context(), store.DispatchAnnotationsParams{
		TaskID:          taskID,
		AnnotationIDs:   req.AnnotationIDs,
		Message:         req.Message,
		MachineID:       route.MachineID,
		Adapter:         route.Adapter,
		ForceNewSession: req.Mode == protocol.FollowupModeForkNewSession,
		LeaseTTLMs:      s.leaseTTL.Milliseconds(),
		ActorID:         actorFromContext(r.Context()),
	})
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err.Error())
		return
	}
	if errors.Is(err, store.ErrInvalidInput) {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_INPUT", err.Error())
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
	board, err := s.store.GetTaskBoard(r.Context(), taskID)
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
