package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

func scanPendingInput(scanner interface{ Scan(...any) error }) (protocol.PendingInput, error) {
	var pi protocol.PendingInput
	var kind, source, payloadJSON, state string
	var dispAtt sql.NullString
	if err := scanner.Scan(
		&pi.InputID, &pi.TaskID, &pi.ProjectID, &kind, &source,
		&pi.Body, &payloadJSON, &pi.Position, &state,
		&pi.CreatedAt, &pi.UpdatedAt, &dispAtt,
	); err != nil {
		return pi, err
	}
	pi.Kind = protocol.PendingInputKind(kind)
	pi.Source = protocol.PendingInputSource(source)
	pi.State = protocol.PendingInputState(state)
	if dispAtt.Valid {
		pi.DispatchedAttemptID = dispAtt.String
	}
	pi.Payload = map[string]any{}
	if payloadJSON != "" {
		_ = json.Unmarshal([]byte(payloadJSON), &pi.Payload)
	}
	// S8: Backfill empty legacy body from payload.prompt if present.
	if strings.TrimSpace(pi.Body) == "" {
		if prompt, ok := pi.Payload["prompt"].(string); ok && strings.TrimSpace(prompt) != "" {
			pi.Body = strings.TrimSpace(prompt)
		}
	}
	return pi, nil
}

func scanPendingInputWithRoute(scanner interface{ Scan(...any) error }) (protocol.PendingInput, sql.NullString, error) {
	var pi protocol.PendingInput
	var kind, source, payloadJSON, state string
	var routeJSON, dispAtt sql.NullString
	if err := scanner.Scan(
		&pi.InputID, &pi.TaskID, &pi.ProjectID, &kind, &source,
		&pi.Body, &payloadJSON, &routeJSON, &pi.Position, &state,
		&pi.CreatedAt, &pi.UpdatedAt, &dispAtt,
	); err != nil {
		return pi, routeJSON, err
	}
	pi.Kind = protocol.PendingInputKind(kind)
	pi.Source = protocol.PendingInputSource(source)
	pi.State = protocol.PendingInputState(state)
	if dispAtt.Valid {
		pi.DispatchedAttemptID = dispAtt.String
	}
	pi.Payload = map[string]any{}
	if payloadJSON != "" {
		_ = json.Unmarshal([]byte(payloadJSON), &pi.Payload)
	}
	if strings.TrimSpace(pi.Body) == "" {
		if prompt, ok := pi.Payload["prompt"].(string); ok && strings.TrimSpace(prompt) != "" {
			pi.Body = strings.TrimSpace(prompt)
		}
	}
	return pi, routeJSON, nil
}

// ListTasksPendingQueueDrain returns idle tasks with durable pending_queue_drain=1 so Hub startup
// and maintenance loops recover queued/steered consumption across crashes (B9).
func (s *Store) ListTasksPendingQueueDrain(ctx context.Context) ([]SteerConsumeNotice, error) {
	var out []SteerConsumeNotice
	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT task_id, COALESCE(pending_steer_input_id, '')
			FROM tasks
			WHERE pending_queue_drain = 1 AND active_attempt_id IS NULL AND archived_at IS NULL
			ORDER BY updated_at ASC
		`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n SteerConsumeNotice
			if err := rows.Scan(&n.TaskID, &n.SteerInputID); err != nil {
				return err
			}
			out = append(out, n)
		}
		return rows.Err()
	})
	return out, err
}

func lookupAnnotationDeliveryIDsTx(ctx context.Context, tx *sql.Tx, taskID string, annotationIDs []string) ([]string, error) {
	if len(annotationIDs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(annotationIDs))
	args := make([]any, 0, len(annotationIDs)+1)
	args = append(args, taskID)
	for i, id := range annotationIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT DISTINCT delivery_id FROM annotations
		WHERE task_id = ? AND annotation_id IN (%s)
	`, strings.Join(placeholders, ",")), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if id != "" {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

// ListPendingInputs returns all queued pending inputs for taskID in consumption order.
func (s *Store) ListPendingInputs(ctx context.Context, taskID string) ([]protocol.PendingInput, error) {
	out := []protocol.PendingInput{}
	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		var exists string
		if err := tx.QueryRowContext(ctx, `SELECT task_id FROM tasks WHERE task_id = ?`, taskID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: task %s", ErrNotFound, taskID)
			}
			return err
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT input_id, task_id, project_id, kind, source, body, payload_json, position, state, created_at, updated_at, dispatched_attempt_id
			FROM task_pending_inputs
			WHERE task_id = ? AND state = 'queued'
			ORDER BY position ASC, created_at ASC
		`, taskID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			pi, err := scanPendingInput(rows)
			if err != nil {
				return err
			}
			out = append(out, pi)
		}
		return rows.Err()
	})
	return out, err
}

// PendingInputRoute stores server-resolved, server-validated execution routing metadata
// in task_pending_inputs.route_json, strictly isolated from user-editable payload_json (NEW-1).
type PendingInputRoute struct {
	MachineID         string                      `json:"machineId,omitempty"`
	Adapter           protocol.Adapter            `json:"adapter,omitempty"`
	DesiredConfig     map[string]string           `json:"desiredConfig,omitempty"`
	RequiredResources []string                    `json:"requiredResources,omitempty"`
	WorktreePath      string                      `json:"worktreePath,omitempty"`
	BaseSHA           string                      `json:"baseSha,omitempty"`
	InputDeliveryID   string                      `json:"inputDeliveryId,omitempty"`
	AnchorCapability  string                      `json:"anchorCapability,omitempty"`
	Anchor            *protocol.ProjectAnchorConfig `json:"anchor,omitempty"`
	ForceNewSession   bool                        `json:"forceNewSession,omitempty"`
	ActorKind         protocol.ActorKind          `json:"actorKind,omitempty"`
	ActorID           string                      `json:"actorId,omitempty"`
}

var reservedQueuePayloadKeys = map[string]bool{
	"machineId":         true,
	"adapter":           true,
	"desiredConfig":     true,
	"requiredResources": true,
	"worktreePath":      true,
	"baseSha":           true,
	"inputDeliveryId":   true,
	"anchorCapability":  true,
	"anchor":            true,
	"forceNewSession":   true,
	"actorKind":         true,
	"actorId":           true,
}

func sanitizeUserPendingPayload(in map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range in {
		if reservedQueuePayloadKeys[k] {
			continue
		}
		out[k] = v
	}
	return out
}

func marshalPendingInputRoute(r *PendingInputRoute) (any, error) {
	if r == nil {
		return nil, nil
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func parsePendingInputRoute(raw sql.NullString) *PendingInputRoute {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return nil
	}
	var r PendingInputRoute
	if err := json.Unmarshal([]byte(raw.String), &r); err != nil {
		return nil
	}
	return &r
}

// EnqueuePendingInput appends a follow-up or annotation batch to task_pending_inputs.
// Public callers cannot inject server-only routing overrides via payload (NEW-1).
func (s *Store) EnqueuePendingInput(
	ctx context.Context,
	taskID string,
	kind protocol.PendingInputKind,
	source protocol.PendingInputSource,
	body string,
	payload map[string]any,
	actorID string,
) (protocol.PendingInput, *protocol.EventRecord, error) {
	var out protocol.PendingInput
	var emitted *protocol.EventRecord
	now := time.Now().UnixMilli()
	if kind == "" {
		kind = protocol.PendingInputFollowup
	}
	if !kind.IsValid() {
		return out, nil, fmt.Errorf("%w: invalid pending input kind %q", ErrInvalidInput, kind)
	}
	if source == "" {
		source = protocol.PendingSourceHuman
	}
	if !source.IsValid() {
		return out, nil, fmt.Errorf("%w: invalid pending input source %q", ErrInvalidInput, source)
	}
	payload = sanitizeUserPendingPayload(payload)
	if actorID == "" {
		actorID = "human"
	}

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var projectID string
		if err := tx.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE task_id = ?`, taskID).Scan(&projectID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: task %s", ErrNotFound, taskID)
			}
			return err
		}

		var nextPos int
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(MAX(position), 0) + 1 FROM task_pending_inputs WHERE task_id = ? AND state = 'queued'
		`, taskID).Scan(&nextPos); err != nil {
			return err
		}

		if _, ok := payload["prompt"]; !ok && body != "" {
			payload["prompt"] = body
		}
		payloadBytes, err := json.Marshal(payload)
		if err != nil {
			return err
		}

		inputID := newID("inp")
		_, err = tx.ExecContext(ctx, `
			INSERT INTO task_pending_inputs (
				input_id, task_id, project_id, kind, source, body, payload_json, position, state, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?)
		`, inputID, taskID, projectID, string(kind), string(source), body, string(payloadBytes), nextPos, now, now)
		if err != nil {
			return fmt.Errorf("insert pending input: %w", err)
		}

		ev, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + taskID,
			ProjectID:  projectID,
			TaskID:     taskID,
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventQueueUpdated,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"action":  "enqueued",
				"inputId": inputID,
				"kind":    kind,
				"source":  source,
			},
			CreatedAt: now,
		})
		if err != nil {
			return err
		}
		emitted = &ev

		out = protocol.PendingInput{
			InputID:   inputID,
			TaskID:    taskID,
			ProjectID: projectID,
			Kind:      kind,
			Source:    source,
			Body:      body,
			Payload:   payload,
			Position:  nextPos,
			State:     protocol.PendingInputQueued,
			CreatedAt: now,
			UpdatedAt: now,
		}
		return nil
	})
	return out, emitted, err
}

// UpdatePendingInput edits the body/payload of a still-queued pending input.
func (s *Store) UpdatePendingInput(ctx context.Context, taskID, inputID, body string, payload map[string]any, actorID string) (protocol.PendingInput, *protocol.EventRecord, error) {
	var out protocol.PendingInput
	var emitted *protocol.EventRecord
	now := time.Now().UnixMilli()
	if actorID == "" {
		actorID = "human"
	}

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `
			SELECT input_id, task_id, project_id, kind, source, body, payload_json, position, state, created_at, updated_at, dispatched_attempt_id
			FROM task_pending_inputs WHERE input_id = ? AND task_id = ?
		`, inputID, taskID)
		cur, err := scanPendingInput(row)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: pending input %s", ErrNotFound, inputID)
		}
		if err != nil {
			return err
		}
		if cur.State != protocol.PendingInputQueued {
			return fmt.Errorf("%w: pending input %s is already %s", ErrConflict, inputID, cur.State)
		}

		merged := cur.Payload
		if merged == nil {
			merged = map[string]any{}
		}
		// NEW-1: Reject/strip server-only routing keys from user-editable payload updates.
		for k, v := range sanitizeUserPendingPayload(payload) {
			merged[k] = v
		}
		if body != "" {
			cur.Body = body
			merged["prompt"] = body
		}
		payloadBytes, err := json.Marshal(merged)
		if err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE task_pending_inputs SET body = ?, payload_json = ?, updated_at = ?
			WHERE input_id = ?
		`, cur.Body, string(payloadBytes), now, inputID); err != nil {
			return err
		}

		ev, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + taskID,
			ProjectID:  cur.ProjectID,
			TaskID:     taskID,
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventQueueUpdated,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"action":  "updated",
				"inputId": inputID,
			},
			CreatedAt: now,
		})
		if err != nil {
			return err
		}
		emitted = &ev

		cur.Payload = merged
		cur.UpdatedAt = now
		out = cur
		return nil
	})
	return out, emitted, err
}

// DeletePendingInput cancels a queued pending input and releases any queued annotations back to open.
func (s *Store) DeletePendingInput(ctx context.Context, taskID, inputID, actorID string) (*protocol.EventRecord, error) {
	var emitted *protocol.EventRecord
	now := time.Now().UnixMilli()
	if actorID == "" {
		actorID = "human"
	}

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `
			SELECT input_id, task_id, project_id, kind, source, body, payload_json, position, state, created_at, updated_at, dispatched_attempt_id
			FROM task_pending_inputs WHERE input_id = ? AND task_id = ?
		`, inputID, taskID)
		cur, err := scanPendingInput(row)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: pending input %s", ErrNotFound, inputID)
		}
		if err != nil {
			return err
		}
		if cur.State != protocol.PendingInputQueued {
			return fmt.Errorf("%w: pending input %s is already %s", ErrConflict, inputID, cur.State)
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE task_pending_inputs SET state = 'cancelled', updated_at = ?
			WHERE input_id = ?
		`, now, inputID); err != nil {
			return err
		}

		// If this queued item held open->queued annotations, release them back to open
		var affectedDeliveryIDs []string
		if cur.Kind == protocol.PendingInputAnnotationBatch {
			if annIDs := extractStringSlice(cur.Payload, "annotationIds"); len(annIDs) > 0 {
				affectedDeliveryIDs, _ = lookupAnnotationDeliveryIDsTx(ctx, tx, taskID, annIDs)
				placeholders := make([]string, len(annIDs))
				args := make([]any, 0, len(annIDs)+1)
				args = append(args, taskID)
				for i, id := range annIDs {
					placeholders[i] = "?"
					args = append(args, id)
				}
				_, err := tx.ExecContext(ctx, fmt.Sprintf(`
					UPDATE annotations SET status = 'open', triggered_attempt_id = NULL
					WHERE task_id = ? AND status = 'queued' AND triggered_attempt_id IS NULL
					  AND annotation_id IN (%s)
				`, strings.Join(placeholders, ",")), args...)
				if err != nil {
					return err
				}
			}
		}

		evPayload := map[string]any{
			"action":  "cancelled",
			"inputId": inputID,
		}
		if len(affectedDeliveryIDs) > 0 {
			evPayload["deliveryIds"] = affectedDeliveryIDs
		}
		ev, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + taskID,
			ProjectID:  cur.ProjectID,
			TaskID:     taskID,
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventQueueUpdated,
			Visibility: protocol.VisibilityTimeline,
			Payload:    evPayload,
			CreatedAt:  now,
		})
		if err != nil {
			return err
		}
		emitted = &ev
		return nil
	})
	return emitted, err
}

// ReorderPendingInputs updates positions of queued inputs inside a single transaction.
func (s *Store) ReorderPendingInputs(ctx context.Context, taskID string, orderedIDs []string, actorID string) ([]protocol.PendingInput, *protocol.EventRecord, error) {
	var out []protocol.PendingInput
	var emitted *protocol.EventRecord
	now := time.Now().UnixMilli()
	if actorID == "" {
		actorID = "human"
	}

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var projectID string
		if err := tx.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE task_id = ?`, taskID).Scan(&projectID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: task %s", ErrNotFound, taskID)
			}
			return err
		}

		// Load current queued IDs
		rows, err := tx.QueryContext(ctx, `
			SELECT input_id FROM task_pending_inputs
			WHERE task_id = ? AND state = 'queued'
			ORDER BY position ASC, created_at ASC
		`, taskID)
		if err != nil {
			return err
		}
		existing := map[string]bool{}
		existingOrder := []string{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			existing[id] = true
			existingOrder = append(existingOrder, id)
		}
		rows.Close()

		finalOrder := make([]string, 0, len(existingOrder))
		seen := map[string]bool{}
		for _, id := range orderedIDs {
			if !existing[id] {
				return fmt.Errorf("%w: input %s is not queued on task %s", ErrInvalidInput, id, taskID)
			}
			if !seen[id] {
				seen[id] = true
				finalOrder = append(finalOrder, id)
			}
		}
		for _, id := range existingOrder {
			if !seen[id] {
				finalOrder = append(finalOrder, id)
			}
		}

		for idx, id := range finalOrder {
			if _, err := tx.ExecContext(ctx, `
				UPDATE task_pending_inputs SET position = ?, updated_at = ? WHERE input_id = ?
			`, idx+1, now, id); err != nil {
				return err
			}
		}

		ev, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + taskID,
			ProjectID:  projectID,
			TaskID:     taskID,
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventQueueUpdated,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"action":   "reordered",
				"inputIds": finalOrder,
			},
			CreatedAt: now,
		})
		if err != nil {
			return err
		}
		emitted = &ev

		qRows, err := tx.QueryContext(ctx, `
			SELECT input_id, task_id, project_id, kind, source, body, payload_json, position, state, created_at, updated_at, dispatched_attempt_id
			FROM task_pending_inputs
			WHERE task_id = ? AND state = 'queued'
			ORDER BY position ASC, created_at ASC
		`, taskID)
		if err != nil {
			return err
		}
		defer qRows.Close()
		out = []protocol.PendingInput{}
		for qRows.Next() {
			pi, err := scanPendingInput(qRows)
			if err != nil {
				return err
			}
			out = append(out, pi)
		}
		return qRows.Err()
	})
	return out, emitted, err
}

// SubmitFollowupParams defines inputs for atomic busy-vs-idle follow-up handling (B9, B11, S2).
type SubmitFollowupParams struct {
	TaskID            string
	Body              string
	Mode              protocol.FollowupMode
	Source            protocol.PendingInputSource
	MachineID         string
	Adapter           protocol.Adapter
	WorktreePath      string
	BaseSHA           string
	InputDeliveryID   string
	DesiredConfig     map[string]string
	RequiredResources []string
	Anchor            *protocol.ProjectAnchorConfig
	AnchorCapability  string
	VerifiedSite      bool
	VerifiedAttemptID string
	LeaseTTLMs        int64
	ActorKind         protocol.ActorKind
	ActorID           string
	PayloadExtra      map[string]any
	PAIdempotency     *PAActionIdempotencySpec
}

// SubmitFollowupResult reports whether the follow-up started an Attempt directly, queued while busy,
// or persisted a steer cancellation against an active attempt.
type SubmitFollowupResult struct {
	Task            protocol.TaskSummary
	Attempt         *AttemptRecord
	PendingInput    *protocol.PendingInput
	ActiveToCancel  *AttemptRecord
	ReplayedReceipt *protocol.PAActionResult
	Events          []protocol.EventRecord
}

// SubmitFollowupAtomic eliminates the check-vs-enqueue race (B9) by checking active_attempt_id,
// enforcing needs_verification (B11), freezing server-validated route_json (NEW-1/S2),
// deferring unpinned inputDeliveryId until consume time when busy (NEW-2),
// and optionally committing PA idempotency receipts inside ONE BEGIN IMMEDIATE tx (NEW-3).
func (s *Store) SubmitFollowupAtomic(ctx context.Context, p SubmitFollowupParams) (SubmitFollowupResult, error) {
	var res SubmitFollowupResult
	now := time.Now().UnixMilli()
	if p.Mode == "" {
		p.Mode = protocol.FollowupModeQueue
	}
	if !p.Mode.IsValid() {
		return res, fmt.Errorf("%w: invalid followup mode %q", ErrInvalidInput, p.Mode)
	}
	if p.Source == "" {
		p.Source = protocol.PendingSourceHuman
	}
	if p.ActorKind == "" {
		p.ActorKind = protocol.ActorHuman
	}
	if p.ActorID == "" {
		p.ActorID = string(p.ActorKind)
	}
	if p.LeaseTTLMs <= 0 {
		p.LeaseTTLMs = 45000
	}

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		// NEW-3: Atomic PA idempotency check inside the SAME write transaction.
		if prevReceipt, found, err := lookupPAActionReceiptTx(ctx, tx, p.PAIdempotency); err != nil {
			return err
		} else if found {
			res.ReplayedReceipt = prevReceipt
			return nil
		}

		var projectID, prefAdapter, desiredCfgJSON, reqResJSON string
		var activeAtt, latestDlv sql.NullString
		err := tx.QueryRowContext(ctx, `
			SELECT project_id, active_attempt_id, latest_delivery_id, preferred_adapter, desired_config_json, required_resources
			FROM tasks WHERE task_id = ?
		`, p.TaskID).Scan(&projectID, &activeAtt, &latestDlv, &prefAdapter, &desiredCfgJSON, &reqResJSON)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: task %s", ErrNotFound, p.TaskID)
		}
		if err != nil {
			return err
		}

		adapter := p.Adapter
		if adapter == "" {
			adapter = protocol.Adapter(prefAdapter)
		}
		mergedCfg := map[string]string{}
		if desiredCfgJSON != "" {
			_ = json.Unmarshal([]byte(desiredCfgJSON), &mergedCfg)
		}
		for k, v := range p.DesiredConfig {
			mergedCfg[k] = v
		}
		reqResources := p.RequiredResources
		if reqResources == nil {
			reqResources = []string{}
			if reqResJSON != "" {
				_ = json.Unmarshal([]byte(reqResJSON), &reqResources)
			}
		}

		// User-visible payload holds prompt/mode/board metadata only; server-validated routing lives in route_json (NEW-1).
		userPayload := sanitizeUserPendingPayload(p.PayloadExtra)
		userPayload["prompt"] = p.Body
		userPayload["mode"] = p.Mode

		// NEW-2: When queuing behind an active attempt without an explicitly pinned InputDeliveryID,
		// leave route.InputDeliveryID empty so ConsumeNextPendingInput resolves the latest formal delivery
		// AFTER the active attempt completes (avoiding stale D1 freeze when D2 is published!).
		queuedRoute := &PendingInputRoute{
			MachineID:         p.MachineID,
			Adapter:           adapter,
			DesiredConfig:     mergedCfg,
			RequiredResources: reqResources,
			WorktreePath:      p.WorktreePath,
			BaseSHA:           p.BaseSHA,
			InputDeliveryID:   p.InputDeliveryID, // Only set if caller explicitly pinned a delivery
			AnchorCapability:  p.AnchorCapability,
			Anchor:            p.Anchor,
			ForceNewSession:   p.Mode == protocol.FollowupModeForkNewSession,
			ActorKind:         p.ActorKind,
			ActorID:           p.ActorID,
		}
		routeVal, err := marshalPendingInputRoute(queuedRoute)
		if err != nil {
			return err
		}

		if activeAtt.Valid && activeAtt.String != "" {
			actRec, err := loadAttemptRecordTx(ctx, tx, activeAtt.String)
			if err != nil {
				return err
			}

			if p.Mode == protocol.FollowupModeSteer {
				// If the active attempt was never dispatched to any runner instance yet, cancel it immediately
				// and fall through to create the new attempt directly!
				if actRec.State == protocol.AttemptLeased && actRec.RunnerInstanceID == "" {
					if err := cancelAttemptTx(ctx, tx, actRec, p.ActorID, "steered by user before dispatch", now, &res.Events); err != nil {
						return err
					}
					activeAtt.Valid = false
					activeAtt.String = ""
				} else {
					// Shift all existing queued inputs down by +1 and insert steered input at position 1.
					if _, err := tx.ExecContext(ctx, `
						UPDATE task_pending_inputs SET position = position + 1, updated_at = ?
						WHERE task_id = ? AND state = 'queued'
					`, now, p.TaskID); err != nil {
						return err
					}
					payloadBytes, err := json.Marshal(userPayload)
					if err != nil {
						return err
					}
					inputID := newID("inp")
					if _, err := tx.ExecContext(ctx, `
						INSERT INTO task_pending_inputs (
							input_id, task_id, project_id, kind, source, body, payload_json, route_json, position, state, created_at, updated_at
						) VALUES (?, ?, ?, 'followup', ?, ?, ?, ?, 1, 'queued', ?, ?)
					`, inputID, p.TaskID, projectID, string(p.Source), p.Body, string(payloadBytes), routeVal, now, now); err != nil {
						return err
					}
					// B9: Persist explicit steer cancellation intent on the active attempt.
					if _, err := tx.ExecContext(ctx, `
						UPDATE attempts
						SET cancel_requested = 1, cancel_reason_kind = 'steer', steer_input_id = ?
						WHERE attempt_id = ?
					`, inputID, actRec.AttemptID); err != nil {
						return err
					}
					actRec.CancelRequested = true
					actRec.CancelReasonKind = "steer"
					actRec.SteerInputID = inputID

					ev, err := insertEventTx(ctx, tx, eventInsert{
						StreamID:   "task:" + p.TaskID,
						ProjectID:  projectID,
						TaskID:     p.TaskID,
						ActorKind:  p.ActorKind,
						ActorID:    p.ActorID,
						Kind:       protocol.EventQueueUpdated,
						Visibility: protocol.VisibilityTimeline,
						Payload: map[string]any{
							"action":  "steered",
							"inputId": inputID,
							"kind":    protocol.PendingInputFollowup,
							"source":  p.Source,
						},
						CreatedAt: now,
					})
					if err != nil {
						return err
					}
					res.Events = append(res.Events, ev)
					res.PendingInput = &protocol.PendingInput{
						InputID:   inputID,
						TaskID:    p.TaskID,
						ProjectID: projectID,
						Kind:      protocol.PendingInputFollowup,
						Source:    p.Source,
						Body:      p.Body,
						Payload:   userPayload,
						Position:  1,
						State:     protocol.PendingInputQueued,
						CreatedAt: now,
						UpdatedAt: now,
					}
					res.ActiveToCancel = &actRec
					res.Task, err = loadTaskSummaryTx(ctx, tx, p.TaskID)
					if err != nil {
						return err
					}
					if p.PAIdempotency != nil && strings.TrimSpace(p.PAIdempotency.Key) != "" {
						receipt := protocol.PAActionResult{
							Action:  p.PAIdempotency.Action,
							Status:  "ok",
							Summary: fmt.Sprintf("已向任务 %s 发送追问（mode=%s）", p.TaskID, p.Mode),
							Data: map[string]any{
								"taskId":    p.TaskID,
								"mode":      p.Mode,
								"inputId":   inputID,
								"attemptId": actRec.AttemptID,
							},
						}
						if err := savePAActionReceiptTx(ctx, tx, p.PAIdempotency, receipt, now); err != nil {
							return err
						}
					}
					return nil
				}
			} else {
				// Queue or fork_new_session while busy -> enqueue into task_pending_inputs
				var nextPos int
				if err := tx.QueryRowContext(ctx, `
					SELECT COALESCE(MAX(position), 0) + 1 FROM task_pending_inputs WHERE task_id = ? AND state = 'queued'
				`, p.TaskID).Scan(&nextPos); err != nil {
					return err
				}
				payloadBytes, err := json.Marshal(userPayload)
				if err != nil {
					return err
				}
				inputID := newID("inp")
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO task_pending_inputs (
						input_id, task_id, project_id, kind, source, body, payload_json, route_json, position, state, created_at, updated_at
					) VALUES (?, ?, ?, 'followup', ?, ?, ?, ?, ?, 'queued', ?, ?)
				`, inputID, p.TaskID, projectID, string(p.Source), p.Body, string(payloadBytes), routeVal, nextPos, now, now); err != nil {
					return err
				}
				ev, err := insertEventTx(ctx, tx, eventInsert{
					StreamID:   "task:" + p.TaskID,
					ProjectID:  projectID,
					TaskID:     p.TaskID,
					ActorKind:  p.ActorKind,
					ActorID:    p.ActorID,
					Kind:       protocol.EventQueueUpdated,
					Visibility: protocol.VisibilityTimeline,
					Payload: map[string]any{
						"action":  "enqueued",
						"inputId": inputID,
						"kind":    protocol.PendingInputFollowup,
						"source":  p.Source,
					},
					CreatedAt: now,
				})
				if err != nil {
					return err
				}
				res.Events = append(res.Events, ev)
				res.PendingInput = &protocol.PendingInput{
					InputID:   inputID,
					TaskID:    p.TaskID,
					ProjectID: projectID,
					Kind:      protocol.PendingInputFollowup,
					Source:    p.Source,
					Body:      p.Body,
					Payload:   userPayload,
					Position:  nextPos,
					State:     protocol.PendingInputQueued,
					CreatedAt: now,
					UpdatedAt: now,
				}
				res.Task, err = loadTaskSummaryTx(ctx, tx, p.TaskID)
				if err != nil {
					return err
				}
				if p.PAIdempotency != nil && strings.TrimSpace(p.PAIdempotency.Key) != "" {
					receipt := protocol.PAActionResult{
						Action:  p.PAIdempotency.Action,
						Status:  "ok",
						Summary: fmt.Sprintf("已向任务 %s 发送追问（mode=%s）", p.TaskID, p.Mode),
						Data: map[string]any{
							"taskId":  p.TaskID,
							"mode":    p.Mode,
							"inputId": inputID,
						},
					}
					if err := savePAActionReceiptTx(ctx, tx, p.PAIdempotency, receipt, now); err != nil {
						return err
					}
				}
				return nil
			}
		}

		// Task is idle -> resolve latest delivery NOW and enforce B11 needs_verification gate before creating Attempt
		inputDlv := p.InputDeliveryID
		if inputDlv == "" && latestDlv.Valid {
			inputDlv = latestDlv.String
		}

		lastAtt, err := checkNeedsVerificationTx(ctx, tx, p.TaskID, p.VerifiedSite, p.VerifiedAttemptID)
		if err != nil {
			return err
		}
		if lastAtt != nil {
			_, _ = tx.ExecContext(ctx, `
				UPDATE inbox_items SET state = 'resolved', resolved_at = ?, updated_at = ?
				WHERE inbox_id = ? AND state = 'open'
			`, now, now, "inb:blk:"+lastAtt.AttemptID)
			if lastAtt.State == protocol.AttemptNeedsVerification {
				if err := releaseQueuedAnnotationsTx(ctx, tx, lastAtt.AttemptID); err != nil {
					return err
				}
				userPayload["verifiedAttemptId"] = lastAtt.AttemptID
			}
		}

		att, evAtt, err := createAttemptInTx(ctx, tx, createAttemptTxParams{
			TaskID:            p.TaskID,
			ProjectID:         projectID,
			MachineID:         p.MachineID,
			Adapter:           adapter,
			TriggerKind:       protocol.TriggerFollowup,
			TriggerPayload:    userPayload,
			Prompt:            p.Body,
			WorktreePath:      p.WorktreePath,
			BaseSHA:           p.BaseSHA,
			InputDeliveryID:   inputDlv,
			DesiredConfig:     mergedCfg,
			RequiredResources: reqResources,
			Anchor:            p.Anchor,
			AnchorCapability:  p.AnchorCapability,
			ForceNewSession:   p.Mode == protocol.FollowupModeForkNewSession,
			LeaseTTLMs:        p.LeaseTTLMs,
			ActorKind:         p.ActorKind,
			ActorID:           p.ActorID,
			Now:               now,
		})
		if err != nil {
			return err
		}
		res.Attempt = &att
		res.Events = append(res.Events, evAtt)

		// B16: If this followup came from a board @mention, mark that board message consumed now.
		if bmID := stringField(p.PayloadExtra, "boardMessageId"); bmID != "" {
			bmTaskID := stringField(p.PayloadExtra, "boardTaskId")
			if bmTaskID == "" {
				bmTaskID = p.TaskID
			}
			bmMember := stringField(p.PayloadExtra, "boardMemberName")
			bmSeq := int64Field(p.PayloadExtra, "boardSeq")
			if evBM, err := markBoardMessageConsumedTx(ctx, tx, bmTaskID, bmID, bmSeq, bmMember, now); err == nil && evBM != nil {
				res.Events = append(res.Events, *evBM)
			}
		}

		res.Task, err = loadTaskSummaryTx(ctx, tx, p.TaskID)
		if err != nil {
			return err
		}
		if p.PAIdempotency != nil && strings.TrimSpace(p.PAIdempotency.Key) != "" {
			receipt := protocol.PAActionResult{
				Action:  p.PAIdempotency.Action,
				Status:  "ok",
				Summary: fmt.Sprintf("已向任务 %s 发送追问（mode=%s）", p.TaskID, p.Mode),
				Data: map[string]any{
					"taskId":    p.TaskID,
					"mode":      p.Mode,
					"attemptId": att.AttemptID,
				},
			}
			if err := savePAActionReceiptTx(ctx, tx, p.PAIdempotency, receipt, now); err != nil {
				return err
			}
		}
		return nil
	})
	return res, err
}

// SteerPendingInput promotes inputID to the head of the task's queue (position 1, shifting others down),
// and if an active attempt exists, atomically persists steer cancellation intent (or cancels immediately
// if never dispatched) so Hub cancels the active attempt and auto-consumes inputID upon cancellation (B9).
func (s *Store) SteerPendingInput(ctx context.Context, taskID, inputID, actorID string) (protocol.PendingInput, *AttemptRecord, []protocol.EventRecord, error) {
	var promoted protocol.PendingInput
	var activeToCancel *AttemptRecord
	var events []protocol.EventRecord
	now := time.Now().UnixMilli()
	if actorID == "" {
		actorID = "human"
	}

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var projectID string
		var activeAtt sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT project_id, active_attempt_id FROM tasks WHERE task_id = ?`, taskID).Scan(&projectID, &activeAtt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: task %s", ErrNotFound, taskID)
			}
			return err
		}

		rows, err := tx.QueryContext(ctx, `
			SELECT input_id FROM task_pending_inputs
			WHERE task_id = ? AND state = 'queued'
			ORDER BY position ASC, created_at ASC
		`, taskID)
		if err != nil {
			return err
		}
		var others []string
		found := false
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			if id == inputID {
				found = true
			} else {
				others = append(others, id)
			}
		}
		rows.Close()

		if !found {
			return fmt.Errorf("%w: pending input %s not queued on task %s", ErrNotFound, inputID, taskID)
		}

		ordered := append([]string{inputID}, others...)
		for idx, id := range ordered {
			if _, err := tx.ExecContext(ctx, `
				UPDATE task_pending_inputs SET position = ?, updated_at = ? WHERE input_id = ?
			`, idx+1, now, id); err != nil {
				return err
			}
		}

		row := tx.QueryRowContext(ctx, `
			SELECT input_id, task_id, project_id, kind, source, body, payload_json, position, state, created_at, updated_at, dispatched_attempt_id
			FROM task_pending_inputs WHERE input_id = ?
		`, inputID)
		promoted, err = scanPendingInput(row)
		if err != nil {
			return err
		}

		if activeAtt.Valid && activeAtt.String != "" {
			actRec, err := loadAttemptRecordTx(ctx, tx, activeAtt.String)
			if err != nil {
				return err
			}
			if actRec.State == protocol.AttemptLeased && actRec.RunnerInstanceID == "" {
				if err := cancelAttemptTx(ctx, tx, actRec, actorID, "steered before dispatch", now, &events); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `
					UPDATE tasks SET pending_queue_drain = 1, pending_steer_input_id = ?
					WHERE task_id = ?
				`, inputID, taskID); err != nil {
					return err
				}
			} else {
				if _, err := tx.ExecContext(ctx, `
					UPDATE attempts
					SET cancel_requested = 1, cancel_reason_kind = 'steer', steer_input_id = ?
					WHERE attempt_id = ?
				`, inputID, actRec.AttemptID); err != nil {
					return err
				}
				actRec.CancelRequested = true
				actRec.CancelReasonKind = "steer"
				actRec.SteerInputID = inputID
				activeToCancel = &actRec
			}
		}

		ev, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + taskID,
			ProjectID:  projectID,
			TaskID:     taskID,
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventQueueUpdated,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"action":  "steered",
				"inputId": inputID,
			},
			CreatedAt: now,
		})
		if err != nil {
			return err
		}
		events = append(events, ev)
		return nil
	})
	return promoted, activeToCancel, events, err
}

// DispatchAnnotationsParams configures atomic open -> queued annotation dispatch.
type DispatchAnnotationsParams struct {
	TaskID            string
	AnnotationIDs     []string
	Message           string
	MachineID         string
	Adapter           protocol.Adapter
	WorktreePath      string
	BaseSHA           string
	InputDeliveryID   string
	DesiredConfig     map[string]string
	RequiredResources []string
	Anchor            *protocol.ProjectAnchorConfig
	AnchorCapability  string
	ForceNewSession   bool
	VerifiedSite      bool
	VerifiedAttemptID string
	LeaseTTLMs        int64
	ActorID           string
}

// DispatchAnnotationsResult captures whether annotations launched a new Attempt or entered task_pending_inputs.
type DispatchAnnotationsResult struct {
	QueuedCount  int
	Attempt      *AttemptRecord
	PendingInput *protocol.PendingInput
	Events       []protocol.EventRecord
}

// DispatchAnnotations atomically transitions all specified open annotations to 'queued'
// (verifying RowsAffected == len(uniqueIDs) to block duplicate clicks), then either enqueues
// an annotation_batch into task_pending_inputs (if an active attempt exists) or starts a new Attempt
// (enforcing B11 needs_verification gate and freezing effective route/config per S2).
func (s *Store) DispatchAnnotations(ctx context.Context, p DispatchAnnotationsParams) (DispatchAnnotationsResult, error) {
	var res DispatchAnnotationsResult
	now := time.Now().UnixMilli()
	if p.ActorID == "" {
		p.ActorID = "human"
	}

	uniqueIDs := dedupeStrings(p.AnnotationIDs)
	if len(uniqueIDs) == 0 {
		return res, fmt.Errorf("%w: annotationIds is required", ErrInvalidInput)
	}

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var projectID string
		var activeAtt, latestDlv sql.NullString
		var prefAdapter, desiredCfgJSON, reqResJSON string
		err := tx.QueryRowContext(ctx, `
			SELECT project_id, active_attempt_id, latest_delivery_id, preferred_adapter, desired_config_json, required_resources
			FROM tasks WHERE task_id = ?
		`, p.TaskID).Scan(&projectID, &activeAtt, &latestDlv, &prefAdapter, &desiredCfgJSON, &reqResJSON)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: task %s", ErrNotFound, p.TaskID)
		}
		if err != nil {
			return err
		}

		// B11: If task is idle, enforce needs_verification gate BEFORE transitioning annotations!
		var lastAtt *AttemptRecord
		if !activeAtt.Valid || activeAtt.String == "" {
			lastAtt, err = checkNeedsVerificationTx(ctx, tx, p.TaskID, p.VerifiedSite, p.VerifiedAttemptID)
			if err != nil {
				return err
			}
			if lastAtt != nil && lastAtt.State == protocol.AttemptNeedsVerification {
				if err := releaseQueuedAnnotationsTx(ctx, tx, lastAtt.AttemptID); err != nil {
					return err
				}
			}
		}

		placeholders := make([]string, len(uniqueIDs))
		args := make([]any, 0, len(uniqueIDs)+1)
		args = append(args, p.TaskID)
		for i, id := range uniqueIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		inList := strings.Join(placeholders, ",")

		// Atomic open -> queued guard: affected rows MUST strictly equal len(uniqueIDs)
		updRes, err := tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE annotations
			SET status = 'queued'
			WHERE task_id = ? AND status = 'open' AND annotation_id IN (%s)
		`, inList), args...)
		if err != nil {
			return err
		}
		affected, _ := updRes.RowsAffected()
		if int(affected) != len(uniqueIDs) {
			return fmt.Errorf("%w: expected %d open annotations on task %s, matched %d", ErrConflict, len(uniqueIDs), p.TaskID, affected)
		}
		res.QueuedCount = int(affected)

		// Load annotations to build structured prompt
		anRows, err := tx.QueryContext(ctx, fmt.Sprintf(`
			SELECT annotation_id, project_id, task_id, delivery_id, source, external_key,
			       anchor_json, body, status, triggered_attempt_id, resolution_delivery_id,
			       resolution_note, created_at, resolved_at
			FROM annotations
			WHERE task_id = ? AND annotation_id IN (%s)
			ORDER BY created_at ASC
		`, inList), args...)
		if err != nil {
			return err
		}
		var anns []protocol.AnnotationView
		hasRyugi, hasDiff := false, false
		deliveryIDSet := map[string]bool{}
		deliveryIDs := []string{}
		for anRows.Next() {
			an, err := scanAnnotationView(anRows)
			if err != nil {
				anRows.Close()
				return err
			}
			if an.Source == protocol.AnnotationSourceRyugi {
				hasRyugi = true
			} else if an.Source == protocol.AnnotationSourceDiffDrawer {
				hasDiff = true
			}
			if an.DeliveryID != "" && !deliveryIDSet[an.DeliveryID] {
				deliveryIDSet[an.DeliveryID] = true
				deliveryIDs = append(deliveryIDs, an.DeliveryID)
			}
			anns = append(anns, an)
		}
		anRows.Close()

		prompt := buildAnnotationBatchPrompt(anns, p.Message)
		source := protocol.PendingSourceHuman
		if hasRyugi && !hasDiff {
			source = protocol.PendingSourceRyugi
		} else if hasDiff {
			source = protocol.PendingSourceDiffDrawer
		}

		adapter := p.Adapter
		if adapter == "" {
			adapter = protocol.Adapter(prefAdapter)
		}
		desiredCfg := map[string]string{}
		if desiredCfgJSON != "" {
			_ = json.Unmarshal([]byte(desiredCfgJSON), &desiredCfg)
		}
		for k, v := range p.DesiredConfig {
			desiredCfg[k] = v
		}
		reqResources := p.RequiredResources
		if reqResources == nil {
			reqResources = []string{}
			if reqResJSON != "" {
				_ = json.Unmarshal([]byte(reqResJSON), &reqResources)
			}
		}
		inputDlv := p.InputDeliveryID
		if inputDlv == "" && latestDlv.Valid {
			inputDlv = latestDlv.String
		}

		payload := map[string]any{
			"annotationIds": uniqueIDs,
			"deliveryIds":   deliveryIDs,
			"prompt":        prompt,
			"message":       p.Message,
		}
		annRoute := &PendingInputRoute{
			MachineID:         p.MachineID,
			Adapter:           adapter,
			DesiredConfig:     desiredCfg,
			RequiredResources: reqResources,
			WorktreePath:      p.WorktreePath,
			BaseSHA:           p.BaseSHA,
			InputDeliveryID:   inputDlv,
			AnchorCapability:  p.AnchorCapability,
			Anchor:            p.Anchor,
			ForceNewSession:   p.ForceNewSession,
			ActorKind:         protocol.ActorHuman,
			ActorID:           p.ActorID,
		}
		annRouteVal, err := marshalPendingInputRoute(annRoute)
		if err != nil {
			return err
		}

		if activeAtt.Valid && activeAtt.String != "" {
			// Task currently running / swarm_monitoring -> queue into task_pending_inputs
			var nextPos int
			if err := tx.QueryRowContext(ctx, `
				SELECT COALESCE(MAX(position), 0) + 1 FROM task_pending_inputs WHERE task_id = ? AND state = 'queued'
			`, p.TaskID).Scan(&nextPos); err != nil {
				return err
			}
			payloadBytes, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			inputID := newID("inp")
			bodySummary := fmt.Sprintf("批注包 (%d 条)", len(anns))
			if strings.TrimSpace(p.Message) != "" {
				bodySummary += ": " + strings.TrimSpace(p.Message)
			}
			_, err = tx.ExecContext(ctx, `
				INSERT INTO task_pending_inputs (
					input_id, task_id, project_id, kind, source, body, payload_json, route_json, position, state, created_at, updated_at
				) VALUES (?, ?, ?, 'annotation_batch', ?, ?, ?, ?, ?, 'queued', ?, ?)
			`, inputID, p.TaskID, projectID, string(source), bodySummary, string(payloadBytes), annRouteVal, nextPos, now, now)
			if err != nil {
				return err
			}
			ev, err := insertEventTx(ctx, tx, eventInsert{
				StreamID:   "task:" + p.TaskID,
				ProjectID:  projectID,
				TaskID:     p.TaskID,
				ActorKind:  protocol.ActorHuman,
				ActorID:    p.ActorID,
				Kind:       protocol.EventQueueUpdated,
				Visibility: protocol.VisibilityTimeline,
				Payload: map[string]any{
					"action":        "annotation_batch_queued",
					"inputId":       inputID,
					"annotationIds": uniqueIDs,
					"deliveryIds":   deliveryIDs,
				},
				CreatedAt: now,
			})
			if err != nil {
				return err
			}
			res.Events = append(res.Events, ev)
			res.PendingInput = &protocol.PendingInput{
				InputID:   inputID,
				TaskID:    p.TaskID,
				ProjectID: projectID,
				Kind:      protocol.PendingInputAnnotationBatch,
				Source:    source,
				Body:      bodySummary,
				Payload:   payload,
				Position:  nextPos,
				State:     protocol.PendingInputQueued,
				CreatedAt: now,
				UpdatedAt: now,
			}
			return nil
		}

		if lastAtt != nil {
			_, _ = tx.ExecContext(ctx, `
				UPDATE inbox_items SET state = 'resolved', resolved_at = ?, updated_at = ?
				WHERE inbox_id = ? AND state = 'open'
			`, now, now, "inb:blk:"+lastAtt.AttemptID)
			if lastAtt.State == protocol.AttemptNeedsVerification {
				payload["verifiedAttemptId"] = lastAtt.AttemptID
			}
		}

		// Task is idle -> start new Attempt immediately and bind triggered_attempt_id
		att, evAtt, err := createAttemptInTx(ctx, tx, createAttemptTxParams{
			TaskID:            p.TaskID,
			ProjectID:         projectID,
			MachineID:         p.MachineID,
			Adapter:           adapter,
			TriggerKind:       protocol.TriggerAnnotationBatch,
			TriggerPayload:    payload,
			Prompt:            prompt,
			WorktreePath:      p.WorktreePath,
			BaseSHA:           p.BaseSHA,
			InputDeliveryID:   inputDlv,
			DesiredConfig:     desiredCfg,
			RequiredResources: reqResources,
			Anchor:            p.Anchor,
			AnchorCapability:  p.AnchorCapability,
			ForceNewSession:   p.ForceNewSession,
			LeaseTTLMs:        p.LeaseTTLMs,
			ActorKind:         protocol.ActorHuman,
			ActorID:           p.ActorID,
			Now:               now,
		})
		if err != nil {
			return err
		}

		bindArgs := make([]any, 0, len(uniqueIDs)+2)
		bindArgs = append(bindArgs, att.AttemptID, p.TaskID)
		for _, id := range uniqueIDs {
			bindArgs = append(bindArgs, id)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE annotations SET triggered_attempt_id = ?
			WHERE task_id = ? AND annotation_id IN (%s)
		`, inList), bindArgs...); err != nil {
			return err
		}

		// S6: Emit queue.updated with deliveryIds so open delivery drawers refresh queued annotation state immediately.
		evQ, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + p.TaskID,
			ProjectID:  projectID,
			TaskID:     p.TaskID,
			AttemptID:  att.AttemptID,
			ActorKind:  protocol.ActorHuman,
			ActorID:    p.ActorID,
			Kind:       protocol.EventQueueUpdated,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"action":              "annotation_batch_dispatched",
				"dispatchedAttemptId": att.AttemptID,
				"annotationIds":       uniqueIDs,
				"deliveryIds":         deliveryIDs,
			},
			CreatedAt: now,
		})
		if err != nil {
			return err
		}

		res.Attempt = &att
		res.Events = append(res.Events, evQ, evAtt)
		return nil
	})
	return res, err
}

// ConsumeNextPendingInputParams configures automatic queue pop after an Attempt completes.
type ConsumeNextPendingInputParams struct {
	TaskID            string
	PreferredInputID  string
	MachineID         string
	Adapter           protocol.Adapter
	WorktreePath      string
	BaseSHA           string
	InputDeliveryID   string
	DesiredConfig     map[string]string
	RequiredResources []string
	Anchor            *protocol.ProjectAnchorConfig
	AnchorCapability  string
	LeaseTTLMs        int64
	// ValidateRoute runs inside the BEGIN IMMEDIATE transaction against the FINAL resolved route
	// (including any server-stored route_json overrides) before creating the Attempt (NEW-1).
	ValidateRoute func(machineID string, adapter protocol.Adapter, anchor *protocol.ProjectAnchorConfig, actorKind protocol.ActorKind) error
}

// ConsumeNextPendingInput atomically pops the head (or preferred steered) queued item from task_pending_inputs,
// creates a new leased Attempt, marks the input 'dispatched', and binds any queued annotations.
// Returns (nil, nil, nil, nil) if no queued input exists, task already has an active attempt,
// or the latest attempt is in needs_verification (B11).
func (s *Store) ConsumeNextPendingInput(ctx context.Context, p ConsumeNextPendingInputParams) (*protocol.PendingInput, *AttemptRecord, []protocol.EventRecord, error) {
	var consumed *protocol.PendingInput
	var createdAtt *AttemptRecord
	var events []protocol.EventRecord
	now := time.Now().UnixMilli()

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var projectID, prefAdapter, desiredCfgJSON, reqResJSON string
		var activeAtt, latestDlv sql.NullString
		err := tx.QueryRowContext(ctx, `
			SELECT project_id, active_attempt_id, latest_delivery_id, preferred_adapter, desired_config_json, required_resources
			FROM tasks WHERE task_id = ?
		`, p.TaskID).Scan(&projectID, &activeAtt, &latestDlv, &prefAdapter, &desiredCfgJSON, &reqResJSON)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: task %s", ErrNotFound, p.TaskID)
		}
		if err != nil {
			return err
		}
		if activeAtt.Valid && activeAtt.String != "" {
			return nil
		}

		// B11: NEVER auto-consume pending queue when previous attempt requires human site verification!
		if _, err := checkNeedsVerificationTx(ctx, tx, p.TaskID, false, ""); err != nil {
			if errors.Is(err, ErrVerificationRequired) {
				_, _ = tx.ExecContext(ctx, `UPDATE tasks SET pending_queue_drain = 0, pending_steer_input_id = NULL WHERE task_id = ?`, p.TaskID)
				return nil
			}
			return err
		}

		var pi protocol.PendingInput
		var routeRaw sql.NullString
		found := false
		if strings.TrimSpace(p.PreferredInputID) != "" {
			row := tx.QueryRowContext(ctx, `
				SELECT input_id, task_id, project_id, kind, source, body, payload_json, route_json, position, state, created_at, updated_at, dispatched_attempt_id
				FROM task_pending_inputs
				WHERE task_id = ? AND input_id = ? AND state = 'queued'
			`, p.TaskID, p.PreferredInputID)
			cand, rr, err := scanPendingInputWithRoute(row)
			if err == nil {
				pi = cand
				routeRaw = rr
				found = true
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if !found {
			row := tx.QueryRowContext(ctx, `
				SELECT input_id, task_id, project_id, kind, source, body, payload_json, route_json, position, state, created_at, updated_at, dispatched_attempt_id
				FROM task_pending_inputs
				WHERE task_id = ? AND state = 'queued'
				ORDER BY position ASC, created_at ASC
				LIMIT 1
			`, p.TaskID)
			cand, rr, err := scanPendingInputWithRoute(row)
			if errors.Is(err, sql.ErrNoRows) {
				// Queue is empty -> clear durable drain marker (B9)
				_, _ = tx.ExecContext(ctx, `UPDATE tasks SET pending_queue_drain = 0, pending_steer_input_id = NULL WHERE task_id = ?`, p.TaskID)
				return nil
			}
			if err != nil {
				return err
			}
			pi = cand
			routeRaw = rr
		}

		triggerKind := protocol.TriggerFollowup
		if pi.Kind == protocol.PendingInputAnnotationBatch {
			triggerKind = protocol.TriggerAnnotationBatch
		}
		prompt := pi.Body
		if rawPrompt, ok := pi.Payload["prompt"].(string); ok && strings.TrimSpace(rawPrompt) != "" {
			prompt = rawPrompt
		}

		// NEW-1: Read routing overrides STRICTLY from server-written route_json, NEVER from user-editable payload_json!
		rt := parsePendingInputRoute(routeRaw)
		machineID := p.MachineID
		adapter := p.Adapter
		desiredCfg := map[string]string{}
		if desiredCfgJSON != "" {
			_ = json.Unmarshal([]byte(desiredCfgJSON), &desiredCfg)
		}
		for k, v := range p.DesiredConfig {
			desiredCfg[k] = v
		}
		reqResources := p.RequiredResources
		if reqResources == nil {
			reqResources = []string{}
			if reqResJSON != "" {
				_ = json.Unmarshal([]byte(reqResJSON), &reqResources)
			}
		}
		worktreePath := p.WorktreePath
		baseSHA := p.BaseSHA
		inputDlv := p.InputDeliveryID
		anchorCap := p.AnchorCapability
		anchor := p.Anchor
		forceNewSession := false
		actorKind := protocol.ActorHuman
		if pi.Source == protocol.PendingSourcePA {
			actorKind = protocol.ActorPA
		}

		if rt != nil {
			if rt.MachineID != "" {
				machineID = rt.MachineID
			}
			if rt.Adapter != "" {
				adapter = rt.Adapter
			}
			for k, v := range rt.DesiredConfig {
				desiredCfg[k] = v
			}
			if rt.RequiredResources != nil {
				reqResources = rt.RequiredResources
			}
			if rt.WorktreePath != "" {
				worktreePath = rt.WorktreePath
			}
			if rt.BaseSHA != "" {
				baseSHA = rt.BaseSHA
			}
			if rt.InputDeliveryID != "" {
				inputDlv = rt.InputDeliveryID
			}
			if rt.AnchorCapability != "" {
				anchorCap = rt.AnchorCapability
			}
			if rt.Anchor != nil {
				anchor = rt.Anchor
			}
			forceNewSession = rt.ForceNewSession
			if rt.ActorKind != "" {
				actorKind = rt.ActorKind
			}
		}
		if adapter == "" {
			adapter = protocol.Adapter(prefAdapter)
		}
		// NEW-2: Normal queued followups leave rt.InputDeliveryID empty so latest formal delivery (e.g. D2) is resolved HERE!
		if inputDlv == "" && latestDlv.Valid {
			inputDlv = latestDlv.String
		}

		// NEW-1: Re-validate FINAL resolved route (principal guard + implemented adapter + anchor/machine check).
		if p.ValidateRoute != nil {
			if err := p.ValidateRoute(machineID, adapter, anchor, actorKind); err != nil {
				return err
			}
		}

		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET pending_queue_drain = 0, pending_steer_input_id = NULL WHERE task_id = ?`, p.TaskID); err != nil {
			return err
		}

		att, evAtt, err := createAttemptInTx(ctx, tx, createAttemptTxParams{
			TaskID:            p.TaskID,
			ProjectID:         projectID,
			MachineID:         machineID,
			Adapter:           adapter,
			TriggerKind:       triggerKind,
			TriggerPayload:    pi.Payload,
			Prompt:            prompt,
			WorktreePath:      worktreePath,
			BaseSHA:           baseSHA,
			InputDeliveryID:   inputDlv,
			DesiredConfig:     desiredCfg,
			RequiredResources: reqResources,
			Anchor:            anchor,
			AnchorCapability:  anchorCap,
			ForceNewSession:   forceNewSession,
			LeaseTTLMs:        p.LeaseTTLMs,
			ActorKind:         protocol.ActorSystem,
			ActorID:           "queue-dispatcher",
			Now:               now,
		})
		if err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE task_pending_inputs
			SET state = 'dispatched', dispatched_attempt_id = ?, updated_at = ?
			WHERE input_id = ?
		`, att.AttemptID, now, pi.InputID); err != nil {
			return err
		}

		// If annotation_batch, bind triggered_attempt_id on the queued annotations
		var affectedDeliveryIDs []string
		if pi.Kind == protocol.PendingInputAnnotationBatch {
			if annIDs := extractStringSlice(pi.Payload, "annotationIds"); len(annIDs) > 0 {
				affectedDeliveryIDs, _ = lookupAnnotationDeliveryIDsTx(ctx, tx, p.TaskID, annIDs)
				placeholders := make([]string, len(annIDs))
				args := make([]any, 0, len(annIDs)+2)
				args = append(args, att.AttemptID, p.TaskID)
				for i, id := range annIDs {
					placeholders[i] = "?"
					args = append(args, id)
				}
				_, err := tx.ExecContext(ctx, fmt.Sprintf(`
					UPDATE annotations SET triggered_attempt_id = ?
					WHERE task_id = ? AND status = 'queued' AND annotation_id IN (%s)
				`, strings.Join(placeholders, ",")), args...)
				if err != nil {
					return err
				}
			}
		}

		evQPayload := map[string]any{
			"action":              "dispatched",
			"inputId":             pi.InputID,
			"dispatchedAttemptId": att.AttemptID,
		}
		if len(affectedDeliveryIDs) > 0 {
			evQPayload["deliveryIds"] = affectedDeliveryIDs
		}
		evQ, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + p.TaskID,
			ProjectID:  projectID,
			TaskID:     p.TaskID,
			AttemptID:  att.AttemptID,
			ActorKind:  protocol.ActorSystem,
			ActorID:    "queue-dispatcher",
			Kind:       protocol.EventQueueUpdated,
			Visibility: protocol.VisibilityTimeline,
			Payload:    evQPayload,
			CreatedAt:  now,
		})
		if err != nil {
			return err
		}

		// B16: If this queued input originated from a board @mention, mark that board message consumed now.
		if bmID := stringField(pi.Payload, "boardMessageId"); bmID != "" {
			bmTaskID := stringField(pi.Payload, "boardTaskId")
			if bmTaskID == "" {
				bmTaskID = p.TaskID
			}
			bmMember := stringField(pi.Payload, "boardMemberName")
			bmSeq := int64Field(pi.Payload, "boardSeq")
			if evBM, err := markBoardMessageConsumedTx(ctx, tx, bmTaskID, bmID, bmSeq, bmMember, now); err == nil && evBM != nil {
				events = append(events, *evBM)
			}
		}

		pi.State = protocol.PendingInputDispatched
		pi.DispatchedAttemptID = att.AttemptID
		pi.UpdatedAt = now
		consumed = &pi
		createdAtt = &att
		events = append(events, evQ, evAtt)
		return nil
	})
	return consumed, createdAtt, events, err
}

func buildAnnotationBatchPrompt(anns []protocol.AnnotationView, extraMessage string) string {
	var b strings.Builder
	b.WriteString("请逐条处理以下审阅批注，并在完成后运行 `orbit deliver --summary \"...\" --final`：\n")
	if strings.TrimSpace(extraMessage) != "" {
		b.WriteString("\n补充说明: " + strings.TrimSpace(extraMessage) + "\n")
	}
	for i, an := range anns {
		loc := ""
		if an.Anchor.Path != "" {
			loc = fmt.Sprintf("%s:%d-%d", an.Anchor.Path, an.Anchor.StartLine, an.Anchor.EndLine)
		} else if an.Anchor.Artifact != "" {
			loc = fmt.Sprintf("%s (%s)", an.Anchor.Artifact, an.Anchor.Section)
		}
		fmt.Fprintf(&b, "\n%d. [%s] %s\n   批注: %s\n", i+1, an.Source, loc, an.Body)
	}
	return b.String()
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func extractStringSlice(m map[string]any, key string) []string {
	raw, ok := m[key]
	if !ok || raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
