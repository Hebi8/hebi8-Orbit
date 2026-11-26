package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

func (s *Server) createChildTaskInternal(
	ctx context.Context,
	parentTaskID string,
	req protocol.CreateChildTaskRequest,
	actorKind protocol.ActorKind,
	actorID string,
	idempHeader string,
) (protocol.CreateTaskResponse, int, string, error) {
	return s.createChildTaskInternalWithGuard(ctx, parentTaskID, req, actorKind, actorID, idempHeader, nil)
}

func (s *Server) createChildTaskInternalWithGuard(
	ctx context.Context,
	parentTaskID string,
	req protocol.CreateChildTaskRequest,
	actorKind protocol.ActorKind,
	actorID string,
	idempHeader string,
	runnerGuard *store.RunnerParentControlGuard,
) (protocol.CreateTaskResponse, int, string, error) {
	var resp protocol.CreateTaskResponse
	parentTaskID = strings.TrimSpace(parentTaskID)
	if parentTaskID == "" {
		return resp, http.StatusBadRequest, "ERR_MISSING_PARENT_TASK_ID", errors.New("parentTaskId is required")
	}
	if req.Adapter == protocol.AdapterSSHDeploy {
		return resp, http.StatusForbidden, "ERR_DEPLOY_FORBIDDEN", errors.New("child sessions cannot use ssh_deploy adapter")
	}

	parent, err := s.store.GetTask(ctx, parentTaskID)
	if errors.Is(err, store.ErrNotFound) {
		return resp, http.StatusNotFound, "ERR_TASK_NOT_FOUND", fmt.Errorf("parent task %s not found", parentTaskID)
	}
	if err != nil {
		return resp, http.StatusInternalServerError, "ERR_TASK_LOOKUP", err
	}
	if parent.ArchivedAt != nil {
		return resp, http.StatusConflict, "ERR_TASK_ARCHIVED", fmt.Errorf("parent task %s is archived", parentTaskID)
	}

	targetProjectID := strings.TrimSpace(req.ProjectID)
	if targetProjectID == "" {
		if parent.ProjectID == protocol.PAProjectID {
			targetProjectID = protocol.ScratchProjectID
		} else {
			targetProjectID = parent.ProjectID
		}
	}

	// Unconditionally reject routes resolving to ssh_deploy / ssh_outbound for child sessions,
	// even when requested by a human caller via POST /api/tasks/{taskId}/children.
	if proj, pErr := s.store.GetProject(ctx, targetProjectID); pErr == nil {
		if route, rErr := s.resolveExecutionRouteWithLocation(proj, req.RequiredCapability, req.Adapter, req.MachineID, req.PartID, req.LocationID); rErr == nil {
			if err := enforceChildNoDeployGuard(parentTaskID, route); err != nil {
				return resp, http.StatusForbidden, "ERR_DEPLOY_FORBIDDEN", err
			}
		}
	}

	createReq := protocol.CreateTaskRequest{
		Title:                strings.TrimSpace(req.Title),
		Goal:                 req.Goal,
		ParentTaskID:         parentTaskID,
		DelegatedByAttemptID: strings.TrimSpace(req.DelegatedByAttemptID),
		DelegatedRole:        strings.TrimSpace(req.Role),
		NotifyParent:         req.NotifyParent,
		MachineID:            strings.TrimSpace(req.MachineID),
		PartID:               strings.TrimSpace(req.PartID),
		LocationID:           strings.TrimSpace(req.LocationID),
		UseWorktree:          req.UseWorktree,
		RequiredCapability:   strings.TrimSpace(req.RequiredCapability),
		Adapter:              req.Adapter,
		Config:               req.Config,
		IdempotencyKey:       strings.TrimSpace(req.IdempotencyKey),
	}
	out, _, status, code, err := s.createTaskInternalWithGuards(ctx, targetProjectID, createReq, actorKind, actorID, idempHeader, nil, runnerGuard)
	return out, status, code, err
}

func (s *Server) handleCreateChildTask(w http.ResponseWriter, r *http.Request) {
	parentTaskID := chi.URLParam(r, "taskId")
	var req protocol.CreateChildTaskRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	resp, status, code, err := s.createChildTaskInternal(
		r.Context(),
		parentTaskID,
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

func (s *Server) handleListChildTasks(w http.ResponseWriter, r *http.Request) {
	parentTaskID := chi.URLParam(r, "taskId")
	includeArchived := r.URL.Query().Get("includeArchived") == "1" || strings.EqualFold(r.URL.Query().Get("includeArchived"), "true")
	children, err := s.store.ListChildTasks(r.Context(), parentTaskID, includeArchived)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_LIST_CHILDREN", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, protocol.ListChildrenResponse{
		ParentTaskID: parentTaskID,
		Children:     children,
	})
}

func (s *Server) handleGetTaskTranscript(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	limit := 10
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 {
			writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_LIMIT", "limit must be a positive integer")
			return
		}
		limit = v
	}
	resp, err := s.store.GetTaskTranscript(r.Context(), taskID, limit)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_TASK_TRANSCRIPT", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

type runnerDelegateSpawnRequest struct {
	ParentTaskID    string `json:"parentTaskId"`
	ParentAttemptID string `json:"parentAttemptId,omitempty"`
	protocol.CreateChildTaskRequest
}

type runnerDelegateListRequest struct {
	ParentTaskID    string `json:"parentTaskId"`
	ParentAttemptID string `json:"parentAttemptId"`
	IncludeArchived bool   `json:"includeArchived,omitempty"`
	IncludeTargets  bool   `json:"includeTargets,omitempty"`
}

type delegateMachineTarget struct {
	MachineID    string             `json:"machineId"`
	DisplayName  string             `json:"displayName"`
	Status       string             `json:"status"`
	Adapters     []protocol.Adapter `json:"adapters,omitempty"`
	Capabilities []string           `json:"capabilities,omitempty"`
}

type delegateProjectTarget struct {
	ProjectID         string           `json:"projectId"`
	DisplayName       string           `json:"displayName"`
	DefaultAdapter    protocol.Adapter `json:"defaultAdapter,omitempty"`
	DefaultCapability string           `json:"defaultCapability,omitempty"`
}

type runnerDelegateListResponse struct {
	ParentTaskID string                  `json:"parentTaskId"`
	Children     []protocol.ChildTaskView `json:"children"`
	Machines     []delegateMachineTarget `json:"machines,omitempty"`
	Projects     []delegateProjectTarget `json:"projects,omitempty"`
}

type runnerDelegateSendRequest struct {
	ParentTaskID    string                `json:"parentTaskId"`
	ParentAttemptID string                `json:"parentAttemptId"`
	ChildTaskID     string                `json:"childTaskId"`
	Message         string                `json:"message"`
	Mode            protocol.FollowupMode `json:"mode,omitempty"`
}

type runnerDelegateReadRequest struct {
	ParentTaskID    string `json:"parentTaskId"`
	ParentAttemptID string `json:"parentAttemptId"`
	ChildTaskID     string `json:"childTaskId"`
	Limit           int    `json:"limit,omitempty"`
}

type runnerDelegateCancelRequest struct {
	ParentTaskID    string `json:"parentTaskId"`
	ParentAttemptID string `json:"parentAttemptId"`
	ChildTaskID     string `json:"childTaskId"`
	Reason          string `json:"reason,omitempty"`
}

// verifyRunnerParentControl enforces live ownership across ALL 5 /api/runner/delegate/* endpoints:
// parent task exists & is unarchived, parentAttemptId is provided and currently the parent's active
// non-terminal attempt executing on runnerMachineID.
func (s *Server) verifyRunnerParentControl(ctx context.Context, runnerMachineID, parentTaskID, parentAttemptID string) (int, string, error) {
	runnerMachineID = strings.TrimSpace(runnerMachineID)
	parentTaskID = strings.TrimSpace(parentTaskID)
	parentAttemptID = strings.TrimSpace(parentAttemptID)
	if runnerMachineID == "" || parentTaskID == "" || parentAttemptID == "" {
		return http.StatusForbidden, "ERR_FORBIDDEN", errors.New("parentTaskId, parentAttemptId, and authenticated runner machine are required")
	}

	parentTask, err := s.store.GetTask(ctx, parentTaskID)
	if errors.Is(err, store.ErrNotFound) {
		return http.StatusForbidden, "ERR_FORBIDDEN", fmt.Errorf("parent task %s not found", parentTaskID)
	}
	if err != nil {
		return http.StatusInternalServerError, "ERR_TASK_LOOKUP", err
	}
	if parentTask.ArchivedAt != nil {
		return http.StatusForbidden, "ERR_FORBIDDEN", fmt.Errorf("parent task %s is archived", parentTaskID)
	}
	if parentTask.ActiveAttemptID != parentAttemptID {
		return http.StatusForbidden, "ERR_FORBIDDEN", fmt.Errorf("parentAttemptId %s is not the active attempt of %s", parentAttemptID, parentTaskID)
	}

	att, err := s.store.GetAttemptRecord(ctx, parentAttemptID)
	if errors.Is(err, store.ErrNotFound) {
		return http.StatusForbidden, "ERR_FORBIDDEN", fmt.Errorf("parentAttemptId %s not found", parentAttemptID)
	}
	if err != nil {
		return http.StatusInternalServerError, "ERR_ATTEMPT_LOOKUP", err
	}
	if att.TaskID != parentTaskID || att.MachineID != runnerMachineID {
		return http.StatusForbidden, "ERR_FORBIDDEN", errors.New("parentAttemptId does not match parentTaskId and runner machine")
	}
	if !slices.Contains(protocol.ActiveAttemptStates, att.State) {
		return http.StatusForbidden, "ERR_FORBIDDEN", fmt.Errorf("parentAttemptId %s is no longer active (state=%s)", parentAttemptID, att.State)
	}
	if att.LeaseExpiresAt < time.Now().UnixMilli() {
		return http.StatusForbidden, "ERR_FORBIDDEN", fmt.Errorf("parentAttemptId %s lease has expired", parentAttemptID)
	}
	return http.StatusOK, "", nil
}

func (s *Server) handleRunnerDelegateSpawn(w http.ResponseWriter, r *http.Request) {
	runnerMachineID := strings.TrimSpace(r.Header.Get("X-Orbit-Machine-Id"))
	var req runnerDelegateSpawnRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	req.ParentTaskID = strings.TrimSpace(req.ParentTaskID)
	req.ParentAttemptID = strings.TrimSpace(req.ParentAttemptID)
	if st, code, err := s.verifyRunnerParentControl(r.Context(), runnerMachineID, req.ParentTaskID, req.ParentAttemptID); err != nil {
		writeAPIError(w, st, code, err.Error())
		return
	}
	req.DelegatedByAttemptID = req.ParentAttemptID
	guard := &store.RunnerParentControlGuard{
		MachineID:       runnerMachineID,
		ParentTaskID:    req.ParentTaskID,
		ParentAttemptID: req.ParentAttemptID,
	}

	resp, status, code, err := s.createChildTaskInternalWithGuard(
		r.Context(),
		req.ParentTaskID,
		req.CreateChildTaskRequest,
		protocol.ActorAgent,
		"agent:"+runnerMachineID,
		r.Header.Get("Idempotency-Key"),
		guard,
	)
	if err != nil {
		writeAPIError(w, status, code, err.Error())
		return
	}
	writeJSON(w, status, resp)
}

func (s *Server) handleRunnerDelegateList(w http.ResponseWriter, r *http.Request) {
	runnerMachineID := strings.TrimSpace(r.Header.Get("X-Orbit-Machine-Id"))
	var req runnerDelegateListRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	req.ParentTaskID = strings.TrimSpace(req.ParentTaskID)
	req.ParentAttemptID = strings.TrimSpace(req.ParentAttemptID)
	if st, code, err := s.verifyRunnerParentControl(r.Context(), runnerMachineID, req.ParentTaskID, req.ParentAttemptID); err != nil {
		writeAPIError(w, st, code, err.Error())
		return
	}
	children, err := s.store.ListChildTasks(r.Context(), req.ParentTaskID, req.IncludeArchived)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_LIST_CHILDREN", err.Error())
		return
	}

	out := runnerDelegateListResponse{
		ParentTaskID: req.ParentTaskID,
		Children:     children,
	}
	if req.IncludeTargets {
		s.mu.RLock()
		yamlMachines := s.yamlMachines.Machines
		s.mu.RUnlock()
		if boot, bErr := s.store.Bootstrap(r.Context(), yamlMachines); bErr == nil {
			machines := s.enrichMachineViews(r.Context(), boot.Machines)
			for _, m := range machines {
				if m.TransportMode == protocol.TransportSSHOutbound {
					continue
				}
				dn := m.DisplayAlias
				if dn == "" {
					dn = m.DisplayName
				}
				out.Machines = append(out.Machines, delegateMachineTarget{
					MachineID:    m.MachineID,
					DisplayName:  dn,
					Status:       string(m.Status),
					Adapters:     m.Adapters,
					Capabilities: m.Capabilities,
				})
			}
			for _, p := range boot.Projects {
				if p.ProjectID == protocol.PAProjectID {
					continue
				}
				out.Projects = append(out.Projects, delegateProjectTarget{
					ProjectID:         p.ProjectID,
					DisplayName:       p.DisplayName,
					DefaultAdapter:    p.DefaultAdapter,
					DefaultCapability: p.DefaultCapability,
				})
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) verifyDescendantScope(ctx context.Context, parentTaskID, childTaskID string) (int, string, error) {
	parentTaskID = strings.TrimSpace(parentTaskID)
	childTaskID = strings.TrimSpace(childTaskID)
	if parentTaskID == "" || childTaskID == "" {
		return http.StatusBadRequest, "ERR_INVALID_INPUT", errors.New("parentTaskId and childTaskId are required")
	}
	ok, err := s.store.IsDescendantTask(ctx, parentTaskID, childTaskID)
	if err != nil {
		return http.StatusInternalServerError, "ERR_SCOPE_CHECK", err
	}
	if !ok {
		return http.StatusForbidden, "ERR_FORBIDDEN", fmt.Errorf("task %s is not a descendant of %s", childTaskID, parentTaskID)
	}
	return http.StatusOK, "", nil
}

func (s *Server) handleRunnerDelegateSend(w http.ResponseWriter, r *http.Request) {
	runnerMachineID := strings.TrimSpace(r.Header.Get("X-Orbit-Machine-Id"))
	var req runnerDelegateSendRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	if st, code, err := s.verifyRunnerParentControl(r.Context(), runnerMachineID, req.ParentTaskID, req.ParentAttemptID); err != nil {
		writeAPIError(w, st, code, err.Error())
		return
	}
	if st, code, err := s.verifyDescendantScope(r.Context(), req.ParentTaskID, req.ChildTaskID); err != nil {
		writeAPIError(w, st, code, err.Error())
		return
	}
	mode := req.Mode
	if mode == "" {
		mode = protocol.FollowupModeQueue
	}
	if mode != protocol.FollowupModeQueue && mode != protocol.FollowupModeSteer {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_MODE", "mode must be queue or steer")
		return
	}
	guard := &store.RunnerParentControlGuard{
		MachineID:       runnerMachineID,
		ParentTaskID:    strings.TrimSpace(req.ParentTaskID),
		ParentAttemptID: strings.TrimSpace(req.ParentAttemptID),
	}
	resp, status, code, err := s.followupTaskWithRunnerGuard(
		r.Context(),
		strings.TrimSpace(req.ChildTaskID),
		protocol.FollowupTaskRequest{
			Message: req.Message,
			Mode:    mode,
		},
		protocol.ActorAgent,
		"agent:"+runnerMachineID,
		map[string]any{"delegatedFromTaskId": strings.TrimSpace(req.ParentTaskID)},
		guard,
	)
	if err != nil {
		writeAPIError(w, status, code, err.Error())
		return
	}
	writeJSON(w, status, resp)
}

func (s *Server) handleRunnerDelegateRead(w http.ResponseWriter, r *http.Request) {
	runnerMachineID := strings.TrimSpace(r.Header.Get("X-Orbit-Machine-Id"))
	var req runnerDelegateReadRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	if st, code, err := s.verifyRunnerParentControl(r.Context(), runnerMachineID, req.ParentTaskID, req.ParentAttemptID); err != nil {
		writeAPIError(w, st, code, err.Error())
		return
	}
	if st, code, err := s.verifyDescendantScope(r.Context(), req.ParentTaskID, req.ChildTaskID); err != nil {
		writeAPIError(w, st, code, err.Error())
		return
	}
	resp, err := s.store.GetTaskTranscript(r.Context(), strings.TrimSpace(req.ChildTaskID), req.Limit)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_TASK_TRANSCRIPT", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleRunnerDelegateCancel(w http.ResponseWriter, r *http.Request) {
	runnerMachineID := strings.TrimSpace(r.Header.Get("X-Orbit-Machine-Id"))
	var req runnerDelegateCancelRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	if st, code, err := s.verifyRunnerParentControl(r.Context(), runnerMachineID, req.ParentTaskID, req.ParentAttemptID); err != nil {
		writeAPIError(w, st, code, err.Error())
		return
	}
	if st, code, err := s.verifyDescendantScope(r.Context(), req.ParentTaskID, req.ChildTaskID); err != nil {
		writeAPIError(w, st, code, err.Error())
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		reason = "cancelled by parent session " + strings.TrimSpace(req.ParentTaskID)
	}
	guard := &store.RunnerParentControlGuard{
		MachineID:       runnerMachineID,
		ParentTaskID:    strings.TrimSpace(req.ParentTaskID),
		ParentAttemptID: strings.TrimSpace(req.ParentAttemptID),
	}
	resp, _, status, code, err := s.cancelTaskInternalWithGuards(r.Context(), strings.TrimSpace(req.ChildTaskID), "agent:"+runnerMachineID, reason, nil, guard)
	if err != nil {
		writeAPIError(w, status, code, err.Error())
		return
	}
	writeJSON(w, status, resp)
}
