package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

func scanTaskSource(scanner interface{ Scan(...any) error }) (protocol.TaskSource, error) {
	var s protocol.TaskSource
	var kind string
	if err := scanner.Scan(&s.SourceID, &s.TaskID, &s.ProjectID, &kind, &s.Label, &s.URIOrRef, &s.CreatedAt); err != nil {
		return s, err
	}
	s.Kind = protocol.TaskSourceKind(kind)
	return s, nil
}

func listTaskSourcesTx(ctx context.Context, tx *sql.Tx, taskID string) ([]protocol.TaskSource, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT source_id, task_id, project_id, kind, label, uri_or_ref, created_at
		FROM task_sources WHERE task_id = ? ORDER BY created_at ASC
	`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []protocol.TaskSource{}
	for rows.Next() {
		src, err := scanTaskSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// ListTaskSources lists all attached Sources for a task.
func (s *Store) ListTaskSources(ctx context.Context, taskID string) ([]protocol.TaskSource, error) {
	var out []protocol.TaskSource
	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		var exists string
		if err := tx.QueryRowContext(ctx, `SELECT task_id FROM tasks WHERE task_id = ?`, taskID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: task %s", ErrNotFound, taskID)
			}
			return err
		}
		var err error
		out, err = listTaskSourcesTx(ctx, tx, taskID)
		return err
	})
	return out, err
}

// AddTaskSource attaches a file/link/nmem source to a task.
func (s *Store) AddTaskSource(ctx context.Context, taskID string, in protocol.CreateTaskSourceInput, actorID string) (protocol.TaskSource, *protocol.EventRecord, error) {
	var out protocol.TaskSource
	var emitted *protocol.EventRecord
	now := time.Now().UnixMilli()
	if !in.Kind.IsValid() {
		return out, nil, fmt.Errorf("%w: invalid source kind %q", ErrInvalidInput, in.Kind)
	}
	uri := strings.TrimSpace(in.URIOrRef)
	if uri == "" {
		return out, nil, fmt.Errorf("%w: uriOrRef is required", ErrInvalidInput)
	}
	label := strings.TrimSpace(in.Label)
	if label == "" {
		label = uri
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

		srcID := newID("src")
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO task_sources (source_id, task_id, project_id, kind, label, uri_or_ref, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, srcID, taskID, projectID, string(in.Kind), label, uri, now); err != nil {
			return err
		}

		ev, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + taskID,
			ProjectID:  projectID,
			TaskID:     taskID,
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventTaskSourceAdded,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"sourceId": srcID,
				"kind":     in.Kind,
				"label":    label,
				"uriOrRef": uri,
			},
			CreatedAt: now,
		})
		if err != nil {
			return err
		}
		emitted = &ev

		out = protocol.TaskSource{
			SourceID:  srcID,
			TaskID:    taskID,
			ProjectID: projectID,
			Kind:      in.Kind,
			Label:     label,
			URIOrRef:  uri,
			CreatedAt: now,
		}
		return nil
	})
	return out, emitted, err
}

// DeleteTaskSource removes a source from a task.
func (s *Store) DeleteTaskSource(ctx context.Context, taskID, sourceID, actorID string) (*protocol.EventRecord, error) {
	var emitted *protocol.EventRecord
	now := time.Now().UnixMilli()
	if actorID == "" {
		actorID = "human"
	}

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var projectID string
		if err := tx.QueryRowContext(ctx, `SELECT project_id FROM task_sources WHERE source_id = ? AND task_id = ?`, sourceID, taskID).Scan(&projectID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: source %s", ErrNotFound, sourceID)
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM task_sources WHERE source_id = ? AND task_id = ?`, sourceID, taskID); err != nil {
			return err
		}
		ev, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + taskID,
			ProjectID:  projectID,
			TaskID:     taskID,
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventTaskSourceRemoved,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"sourceId": sourceID,
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

func scanBoardMessage(scanner interface{ Scan(...any) error }) (protocol.BoardMessage, error) {
	var m protocol.BoardMessage
	var target, extKey sql.NullString
	var status string
	if err := scanner.Scan(
		&m.Seq, &m.MessageID, &m.TaskID, &m.ProjectID,
		&m.AuthorName, &target, &m.Body, &status, &extKey, &m.CreatedAt,
	); err != nil {
		return m, err
	}
	m.Status = protocol.BoardMessageStatus(status)
	if target.Valid {
		m.TargetMention = target.String
	}
	if extKey.Valid {
		m.ExternalKey = extKey.String
	}
	return m, nil
}

func scanBoardMember(scanner interface{ Scan(...any) error }) (protocol.BoardMemberCursor, error) {
	var bm protocol.BoardMemberCursor
	var machID, adapter, state, step, workerTID sql.NullString
	if err := scanner.Scan(
		&bm.TaskID, &bm.ProjectID, &bm.MemberName, &machID, &adapter,
		&state, &bm.LastConsumedSeq, &step, &workerTID, &bm.UpdatedAt,
	); err != nil {
		return bm, err
	}
	if machID.Valid {
		bm.MachineID = machID.String
	}
	if adapter.Valid {
		bm.Adapter = protocol.Adapter(adapter.String)
	}
	bm.State = protocol.BoardMemberState(state.String)
	if step.Valid {
		bm.CurrentStep = step.String
	}
	if workerTID.Valid {
		bm.WorkerTaskID = workerTID.String
	}
	return bm, nil
}

func insertBoardMessageTx(
	ctx context.Context,
	tx *sql.Tx,
	taskID, projectID, authorName, targetMention, body, externalKey string,
	now int64,
) (protocol.BoardMessage, *protocol.EventRecord, error) {
	var out protocol.BoardMessage
	if externalKey != "" {
		row := tx.QueryRowContext(ctx, `
			SELECT seq, message_id, task_id, project_id, author_name, target_mention, body, status, external_key, created_at
			FROM board_messages WHERE external_key = ?
		`, externalKey)
		if existing, err := scanBoardMessage(row); err == nil {
			return existing, nil, nil
		}
	}

	msgID := newID("bms")
	var targetVal, extVal any
	if targetMention != "" {
		targetVal = targetMention
	}
	if externalKey != "" {
		extVal = externalKey
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO board_messages (
			message_id, task_id, project_id, author_name, target_mention, body, status, external_key, created_at
		) VALUES (?, ?, ?, ?, ?, ?, 'posted', ?, ?)
	`, msgID, taskID, projectID, authorName, targetVal, body, extVal, now)
	if err != nil {
		return out, nil, fmt.Errorf("insert board_message: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return out, nil, err
	}

	out = protocol.BoardMessage{
		Seq:           seq,
		MessageID:     msgID,
		TaskID:        taskID,
		ProjectID:     projectID,
		AuthorName:    authorName,
		TargetMention: targetMention,
		Body:          body,
		Status:        protocol.BoardMessagePosted,
		ExternalKey:   externalKey,
		CreatedAt:     now,
	}

	actorKind := protocol.ActorAgent
	if authorName == "human" || authorName == "user" {
		actorKind = protocol.ActorHuman
	}
	ev, err := insertEventTx(ctx, tx, eventInsert{
		StreamID:   "task:" + taskID,
		ProjectID:  projectID,
		TaskID:     taskID,
		ActorKind:  actorKind,
		ActorID:    authorName,
		Kind:       protocol.EventBoardMessagePosted,
		Visibility: protocol.VisibilityTimeline,
		Payload: map[string]any{
			"seq":           seq,
			"messageId":     msgID,
			"authorName":    authorName,
			"targetMention": targetMention,
			"body":          body,
		},
		CreatedAt: now,
	})
	if err != nil {
		return out, nil, err
	}
	return out, &ev, nil
}

func upsertBoardMemberTx(ctx context.Context, tx *sql.Tx, bm protocol.BoardMemberCursor) (*protocol.EventRecord, error) {
	if bm.UpdatedAt == 0 {
		bm.UpdatedAt = time.Now().UnixMilli()
	}
	var machVal, adpVal, stepVal, workerVal any
	if bm.MachineID != "" {
		machVal = bm.MachineID
	}
	if bm.Adapter != "" {
		adpVal = string(bm.Adapter)
	}
	if bm.CurrentStep != "" {
		stepVal = bm.CurrentStep
	}
	if bm.WorkerTaskID != "" {
		workerVal = bm.WorkerTaskID
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO board_members (
			task_id, project_id, member_name, machine_id, adapter, state,
			last_consumed_seq, current_step, worker_task_id, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(task_id, member_name) DO UPDATE SET
			machine_id = COALESCE(excluded.machine_id, board_members.machine_id),
			adapter = COALESCE(excluded.adapter, board_members.adapter),
			state = excluded.state,
			last_consumed_seq = MAX(board_members.last_consumed_seq, excluded.last_consumed_seq),
			current_step = COALESCE(excluded.current_step, board_members.current_step),
			worker_task_id = COALESCE(excluded.worker_task_id, board_members.worker_task_id),
			updated_at = excluded.updated_at
	`, bm.TaskID, bm.ProjectID, bm.MemberName, machVal, adpVal, string(bm.State),
		bm.LastConsumedSeq, stepVal, workerVal, bm.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("upsert board_member %s: %w", bm.MemberName, err)
	}
	ev, err := insertEventTx(ctx, tx, eventInsert{
		StreamID:   "task:" + bm.TaskID,
		ProjectID:  bm.ProjectID,
		TaskID:     bm.TaskID,
		ActorKind:  protocol.ActorAgent,
		ActorID:    bm.MemberName,
		Kind:       protocol.EventBoardMemberUpdated,
		Visibility: protocol.VisibilityTimeline,
		Payload: map[string]any{
			"memberName":      bm.MemberName,
			"state":           bm.State,
			"lastConsumedSeq": bm.LastConsumedSeq,
			"currentStep":     bm.CurrentStep,
			"workerTaskId":    bm.WorkerTaskID,
		},
		CreatedAt: bm.UpdatedAt,
	})
	if err != nil {
		return nil, err
	}
	return &ev, nil
}

// GetTaskBoard returns the unified #seq message board and member cursors for a task,
// merging both native swarm members and cross-machine child tasks (parent_task_id).
func (s *Store) GetTaskBoard(ctx context.Context, taskID string) (protocol.TaskBoardResponse, error) {
	resp := protocol.TaskBoardResponse{
		TaskID:   taskID,
		Members:  []protocol.BoardMemberCursor{},
		Messages: []protocol.BoardMessage{},
	}

	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		var projectID string
		if err := tx.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE task_id = ?`, taskID).Scan(&projectID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: task %s", ErrNotFound, taskID)
			}
			return err
		}

		seenMembers := map[string]bool{}
		mRows, err := tx.QueryContext(ctx, `
			SELECT task_id, project_id, member_name, machine_id, adapter, state, last_consumed_seq, current_step, worker_task_id, updated_at
			FROM board_members
			WHERE task_id = ?
			ORDER BY member_name ASC
		`, taskID)
		if err != nil {
			return err
		}
		defer mRows.Close()
		for mRows.Next() {
			bm, err := scanBoardMember(mRows)
			if err != nil {
				return err
			}
			seenMembers[bm.MemberName] = true
			resp.Members = append(resp.Members, bm)
		}
		if err := mRows.Err(); err != nil {
			return err
		}

		// Also surface cross-machine child tasks (parent_task_id = taskID) as board members
		cRows, err := tx.QueryContext(ctx, `
			SELECT t.task_id, t.project_id, t.title, t.preferred_adapter, t.state, t.updated_at, a.machine_id
			FROM tasks t
			LEFT JOIN attempts a ON a.attempt_id = t.active_attempt_id
			WHERE t.parent_task_id = ? AND t.archived_at IS NULL
			ORDER BY t.created_at ASC
		`, taskID)
		if err != nil {
			return err
		}
		defer cRows.Close()
		for cRows.Next() {
			var childID, childProj, title, adapter, tstate string
			var updatedAt int64
			var mach sql.NullString
			if err := cRows.Scan(&childID, &childProj, &title, &adapter, &tstate, &updatedAt, &mach); err != nil {
				return err
			}
			if seenMembers[childID] {
				continue
			}
			mState := protocol.BoardMemberWaiting
			switch protocol.TaskState(tstate) {
			case protocol.TaskStateRunning, protocol.TaskStateQueued:
				mState = protocol.BoardMemberWorking
			case protocol.TaskStateCancelled:
				mState = protocol.BoardMemberKilled
			case protocol.TaskStateDone, protocol.TaskStateFailed:
				mState = protocol.BoardMemberStopped
			}
			mid := ""
			if mach.Valid {
				mid = mach.String
			}
			resp.Members = append(resp.Members, protocol.BoardMemberCursor{
				TaskID:       taskID,
				ProjectID:    childProj,
				MemberName:   title,
				MachineID:    mid,
				Adapter:      protocol.Adapter(adapter),
				State:        mState,
				WorkerTaskID: childID,
				UpdatedAt:    updatedAt,
			})
		}
		if err := cRows.Err(); err != nil {
			return err
		}

		msgRows, err := tx.QueryContext(ctx, `
			SELECT seq, message_id, task_id, project_id, author_name, target_mention, body, status, external_key, created_at
			FROM board_messages
			WHERE task_id = ?
			ORDER BY seq ASC
			LIMIT 500
		`, taskID)
		if err != nil {
			return err
		}
		defer msgRows.Close()
		for msgRows.Next() {
			msg, err := scanBoardMessage(msgRows)
			if err != nil {
				return err
			}
			if msg.Seq > resp.LatestSeq {
				resp.LatestSeq = msg.Seq
			}
			resp.Messages = append(resp.Messages, msg)
		}
		return msgRows.Err()
	})
	return resp, err
}

// PostBoardMessage inserts a human or agent message into a task's shared queue message board.
func (s *Store) PostBoardMessage(ctx context.Context, taskID, authorName, targetMention, body, externalKey string) (protocol.BoardMessage, *protocol.EventRecord, error) {
	var msg protocol.BoardMessage
	var emitted *protocol.EventRecord
	now := time.Now().UnixMilli()
	body = strings.TrimSpace(body)
	if body == "" {
		return msg, nil, fmt.Errorf("%w: message body is required", ErrInvalidInput)
	}
	if authorName == "" {
		authorName = "human"
	}

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var projectID string
		if err := tx.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE task_id = ?`, taskID).Scan(&projectID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: task %s", ErrNotFound, taskID)
			}
			return err
		}
		var err error
		msg, emitted, err = insertBoardMessageTx(ctx, tx, taskID, projectID, authorName, strings.TrimSpace(targetMention), body, externalKey, now)
		return err
	})
	return msg, emitted, err
}

// GetWorkerStream returns the drill-down event stream for a specific swarm member or child worker task.
func (s *Store) GetWorkerStream(ctx context.Context, taskID, workerName string) (protocol.WorkerStreamResponse, error) {
	resp := protocol.WorkerStreamResponse{
		TaskID:     taskID,
		WorkerName: workerName,
		Events:     []protocol.EventRecord{},
	}

	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		var exists string
		if err := tx.QueryRowContext(ctx, `SELECT task_id FROM tasks WHERE task_id = ?`, taskID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: task %s", ErrNotFound, taskID)
			}
			return err
		}

		row := tx.QueryRowContext(ctx, `
			SELECT task_id, project_id, member_name, machine_id, adapter, state, last_consumed_seq, current_step, worker_task_id, updated_at
			FROM board_members WHERE task_id = ? AND member_name = ?
		`, taskID, workerName)
		if bm, err := scanBoardMember(row); err == nil {
			resp.Member = &bm
		}

		// If backed by a linked child task, return that child task's events; otherwise filter parent task events for workerName
		if resp.Member != nil && resp.Member.WorkerTaskID != "" {
			eRows, err := tx.QueryContext(ctx, `
				SELECT seq, event_id, stream_id, project_id, task_id, attempt_id,
				       actor_kind, actor_id, kind, visibility, payload_json, created_at
				FROM events WHERE task_id = ?
				ORDER BY seq ASC LIMIT 500
			`, resp.Member.WorkerTaskID)
			if err != nil {
				return err
			}
			defer eRows.Close()
			for eRows.Next() {
				ev, err := scanEventRecord(eRows)
				if err != nil {
					return err
				}
				resp.Events = append(resp.Events, ev)
			}
			return eRows.Err()
		}

		eRows, err := tx.QueryContext(ctx, `
			SELECT seq, event_id, stream_id, project_id, task_id, attempt_id,
			       actor_kind, actor_id, kind, visibility, payload_json, created_at
			FROM events
			WHERE task_id = ?
			ORDER BY seq ASC LIMIT 1000
		`, taskID)
		if err != nil {
			return err
		}
		defer eRows.Close()
		for eRows.Next() {
			ev, err := scanEventRecord(eRows)
			if err != nil {
				return err
			}
			if eventMatchesWorker(ev, workerName) {
				resp.Events = append(resp.Events, ev)
			}
		}
		return eRows.Err()
	})
	return resp, err
}

func eventMatchesWorker(ev protocol.EventRecord, workerName string) bool {
	if ev.ActorID == workerName {
		return true
	}
	for _, k := range []string{"workerName", "memberName", "authorName", "targetMention", "name"} {
		if v, ok := ev.Payload[k].(string); ok && strings.EqualFold(strings.TrimPrefix(v, "@"), strings.TrimPrefix(workerName, "@")) {
			return true
		}
	}
	return false
}
