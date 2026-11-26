package hub

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

func (s *Server) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	stateFilter := protocol.ApprovalState(strings.TrimSpace(r.URL.Query().Get("state")))
	if stateFilter != "" && !stateFilter.IsValid() {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_STATE", "invalid approval state filter")
		return
	}
	list, err := s.store.ListApprovals(r.Context(), stateFilter)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_LIST_APPROVALS", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": list})
}

func (s *Server) handleDecideApproval(w http.ResponseWriter, r *http.Request) {
	// Hard security rule: ONLY human identity may decide approvals; PA receives 403.
	if actorKindFromContext(r.Context()) != protocol.ActorHuman {
		writeAPIError(w, http.StatusForbidden, "ERR_HUMAN_REQUIRED", "only human identity may decide approvals")
		return
	}

	approvalID := chi.URLParam(r, "approvalId")
	var req protocol.DecideApprovalRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}

	res, err := s.store.DecideApproval(r.Context(), store.DecideApprovalParams{
		ApprovalID:            approvalID,
		OptionID:              strings.TrimSpace(req.OptionID),
		ExpectedAttemptID:     strings.TrimSpace(req.ExpectedAttemptID),
		ExpectedPayloadSHA256: strings.TrimSpace(req.ExpectedPayloadSHA256),
		DecidedBy:             protocol.ApprovalDecidedByHuman,
		ActorID:               actorFromContext(r.Context()),
		Note:                  strings.TrimSpace(req.Note),
	})
	// B7: Even when DecideApproval returns ErrConflict due to expiry, its expired state transition,
	// inbox resolution, and events were COMMITTED — broadcast events and unblock the Runner before 409.
	if len(res.Events) > 0 {
		s.broker.Broadcast(res.Events...)
		s.notifyChildTaskProgressAsync(res.Events)
	}
	if res.Expired && res.MachineID != "" {
		_ = s.runners.SendApprovalDecision(res.MachineID, protocol.ApprovalDecisionFrame{
			Type:          protocol.RunnerMsgApprovalDecide,
			ApprovalID:    res.Decision.ApprovalID,
			DecisionID:    res.Decision.DecisionID,
			AttemptID:     res.Decision.AttemptID,
			PayloadSHA256: res.Decision.PayloadSHA256,
			DecidedBy:     res.Decision.DecidedBy,
			Expired:       true,
		})
	}

	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_APPROVAL_NOT_FOUND", err.Error())
		return
	}
	if errors.Is(err, store.ErrForbidden) {
		writeAPIError(w, http.StatusForbidden, "ERR_HUMAN_REQUIRED", err.Error())
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeAPIError(w, http.StatusConflict, "ERR_APPROVAL_CONFLICT", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_APPROVAL_DECIDE", err.Error())
		return
	}

	sentToRunner := false
	if res.Approval.Kind == protocol.ApprovalACPToolPermission && res.MachineID != "" {
		// B6: Mark delivery_attempted=1 ONLY right before writing over the live Runner WS socket.
		// If onStart fails, writePump skips conn.Write so delivery_attempted stays 0.
		// Once conn.Write is entered, NEVER clear delivery_attempted on write error because bytes may have
		// partially reached the Runner/OS buffer — uncertainty MUST be preserved.
		sendErr := s.runners.SendApprovalDecisionWithHooks(
			res.MachineID,
			protocol.ApprovalDecisionFrame{
				Type:          protocol.RunnerMsgApprovalDecide,
				ApprovalID:    res.Decision.ApprovalID,
				DecisionID:    res.Decision.DecisionID,
				AttemptID:     res.Decision.AttemptID,
				PayloadSHA256: res.Decision.PayloadSHA256,
				OptionID:      res.Decision.OptionID,
				DecidedBy:     res.Decision.DecidedBy,
			},
			func() error {
				return s.store.MarkApprovalsDeliveryAttempted(r.Context(), []string{res.Approval.ApprovalID})
			},
			nil,
		)
		if sendErr == nil {
			sentToRunner = true
			res.Approval.DeliveryAttempted = true
		}
	}

	writeJSON(w, http.StatusOK, protocol.DecideApprovalResponse{
		Approval:     res.Approval,
		SentToRunner: sentToRunner,
	})
}
