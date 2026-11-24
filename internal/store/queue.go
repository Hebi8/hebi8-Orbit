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
	return pi, nil
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

// EnqueuePendingInput appends a follow-up or annotation batch to task_pending_inputs.
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
	if payload == nil {
		payload = map[string]any{}
	}
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
		for k, v := range payload {
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
		if cur.Kind == protocol.PendingInputAnnotationBatch {
			if annIDs := extractStringSlice(cur.Payload, "annotationIds"); len(annIDs) > 0 {
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

		ev, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + taskID,
			ProjectID:  cur.ProjectID,
			TaskID:     taskID,
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventQueueUpdated,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"action":  "cancelled",
				"inputId": inputID,
			},
			CreatedAt: now,
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

// SteerPendingInput promotes inputID to the head of the task's queue (position 1, shifting others down)
// and reports the task's current active_attempt_id so Hub can steer or dispatch immediately.
func (s *Store) SteerPendingInput(ctx context.Context, taskID, inputID, actorID string) (protocol.PendingInput, string, *protocol.EventRecord, error) {
	var promoted protocol.PendingInput
	var activeAttemptID string
	var emitted *protocol.EventRecord
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
		if activeAtt.Valid {
			activeAttemptID = activeAtt.String
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
		emitted = &ev
		return nil
	})
	return promoted, activeAttemptID, emitted, err
}

// DispatchAnnotationsParams configures atomic open -> queued annotation dispatch.
type DispatchAnnotationsParams struct {
	TaskID          string
	AnnotationIDs   []string
	Message         string
	MachineID       string
	Adapter         protocol.Adapter
	WorktreePath    string
	BaseSHA         string
	InputDeliveryID string
	DesiredConfig   map[string]string
	ForceNewSession bool
	LeaseTTLMs      int64
	ActorID         string
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
// an annotation_batch into task_pending_inputs (if an active attempt exists) or starts a new Attempt.
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
		var prefAdapter, desiredCfgJSON string
		err := tx.QueryRowContext(ctx, `
			SELECT project_id, active_attempt_id, latest_delivery_id, preferred_adapter, desired_config_json
			FROM tasks WHERE task_id = ?
		`, p.TaskID).Scan(&projectID, &activeAtt, &latestDlv, &prefAdapter, &desiredCfgJSON)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: task %s", ErrNotFound, p.TaskID)
		}
		if err != nil {
			return err
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

		payload := map[string]any{
			"annotationIds": uniqueIDs,
			"prompt":        prompt,
			"message":       p.Message,
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
					input_id, task_id, project_id, kind, source, body, payload_json, position, state, created_at, updated_at
				) VALUES (?, ?, ?, 'annotation_batch', ?, ?, ?, ?, 'queued', ?, ?)
			`, inputID, p.TaskID, projectID, string(source), bodySummary, string(payloadBytes), nextPos, now, now)
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

		// Task is idle -> start new Attempt immediately and bind triggered_attempt_id
		adapter := p.Adapter
		if adapter == "" {
			adapter = protocol.Adapter(prefAdapter)
		}
		desiredCfg := p.DesiredConfig
		if len(desiredCfg) == 0 && desiredCfgJSON != "" {
			desiredCfg = map[string]string{}
			_ = json.Unmarshal([]byte(desiredCfgJSON), &desiredCfg)
		}
		inputDlv := p.InputDeliveryID
		if inputDlv == "" && latestDlv.Valid {
			inputDlv = latestDlv.String
		}

		att, evAtt, err := createAttemptInTx(ctx, tx, createAttemptTxParams{
			TaskID:          p.TaskID,
			ProjectID:       projectID,
			MachineID:       p.MachineID,
			Adapter:         adapter,
			TriggerKind:     protocol.TriggerAnnotationBatch,
			TriggerPayload:  payload,
			Prompt:          prompt,
			WorktreePath:    p.WorktreePath,
			BaseSHA:         p.BaseSHA,
			InputDeliveryID: inputDlv,
			DesiredConfig:   desiredCfg,
			ForceNewSession: p.ForceNewSession,
			LeaseTTLMs:      p.LeaseTTLMs,
			ActorKind:       protocol.ActorHuman,
			ActorID:         p.ActorID,
			Now:             now,
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

		res.Attempt = &att
		res.Events = append(res.Events, evAtt)
		return nil
	})
	return res, err
}

// ConsumeNextPendingInputParams configures automatic queue pop after an Attempt completes.
type ConsumeNextPendingInputParams struct {
	TaskID          string
	MachineID       string
	Adapter         protocol.Adapter
	WorktreePath    string
	BaseSHA         string
	InputDeliveryID string
	DesiredConfig   map[string]string
	LeaseTTLMs      int64
}

// ConsumeNextPendingInput atomically pops the head queued item from task_pending_inputs,
// creates a new leased Attempt, marks the input 'dispatched', and binds any queued annotations.
// Returns (nil, nil, nil, nil) if no queued input exists or task already has an active attempt.
func (s *Store) ConsumeNextPendingInput(ctx context.Context, p ConsumeNextPendingInputParams) (*protocol.PendingInput, *AttemptRecord, []protocol.EventRecord, error) {
	var consumed *protocol.PendingInput
	var createdAtt *AttemptRecord
	var events []protocol.EventRecord
	now := time.Now().UnixMilli()

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var projectID, prefAdapter, desiredCfgJSON string
		var activeAtt, latestDlv sql.NullString
		err := tx.QueryRowContext(ctx, `
			SELECT project_id, active_attempt_id, latest_delivery_id, preferred_adapter, desired_config_json
			FROM tasks WHERE task_id = ?
		`, p.TaskID).Scan(&projectID, &activeAtt, &latestDlv, &prefAdapter, &desiredCfgJSON)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: task %s", ErrNotFound, p.TaskID)
		}
		if err != nil {
			return err
		}
		if activeAtt.Valid && activeAtt.String != "" {
			return nil
		}

		row := tx.QueryRowContext(ctx, `
			SELECT input_id, task_id, project_id, kind, source, body, payload_json, position, state, created_at, updated_at, dispatched_attempt_id
			FROM task_pending_inputs
			WHERE task_id = ? AND state = 'queued'
			ORDER BY position ASC, created_at ASC
			LIMIT 1
		`, p.TaskID)
		pi, err := scanPendingInput(row)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}

		triggerKind := protocol.TriggerFollowup
		if pi.Kind == protocol.PendingInputAnnotationBatch {
			triggerKind = protocol.TriggerAnnotationBatch
		}
		prompt := pi.Body
		if rawPrompt, ok := pi.Payload["prompt"].(string); ok && strings.TrimSpace(rawPrompt) != "" {
			prompt = rawPrompt
		}

		adapter := p.Adapter
		if adapter == "" {
			adapter = protocol.Adapter(prefAdapter)
		}
		desiredCfg := p.DesiredConfig
		if len(desiredCfg) == 0 && desiredCfgJSON != "" {
			desiredCfg = map[string]string{}
			_ = json.Unmarshal([]byte(desiredCfgJSON), &desiredCfg)
		}
		inputDlv := p.InputDeliveryID
		if inputDlv == "" && latestDlv.Valid {
			inputDlv = latestDlv.String
		}

		att, evAtt, err := createAttemptInTx(ctx, tx, createAttemptTxParams{
			TaskID:          p.TaskID,
			ProjectID:       projectID,
			MachineID:       p.MachineID,
			Adapter:         adapter,
			TriggerKind:     triggerKind,
			TriggerPayload:  pi.Payload,
			Prompt:          prompt,
			WorktreePath:    p.WorktreePath,
			BaseSHA:         p.BaseSHA,
			InputDeliveryID: inputDlv,
			DesiredConfig:   desiredCfg,
			ForceNewSession: false,
			LeaseTTLMs:      p.LeaseTTLMs,
			ActorKind:       protocol.ActorSystem,
			ActorID:         "queue-dispatcher",
			Now:             now,
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
		if pi.Kind == protocol.PendingInputAnnotationBatch {
			if annIDs := extractStringSlice(pi.Payload, "annotationIds"); len(annIDs) > 0 {
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

		evQ, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + p.TaskID,
			ProjectID:  projectID,
			TaskID:     p.TaskID,
			AttemptID:  att.AttemptID,
			ActorKind:  protocol.ActorSystem,
			ActorID:    "queue-dispatcher",
			Kind:       protocol.EventQueueUpdated,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"action":              "dispatched",
				"inputId":             pi.InputID,
				"dispatchedAttemptId": att.AttemptID,
			},
			CreatedAt: now,
		})
		if err != nil {
			return err
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
