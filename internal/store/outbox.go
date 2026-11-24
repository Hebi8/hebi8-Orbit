package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

// PublishedDeliveryNotice informs the Hub of a newly persisted delivery that may need Ryugi registration.
type PublishedDeliveryNotice struct {
	DeliveryID     string
	RyugiArtifacts []string
}

// IngestOutboxResult reports the strictly contiguous watermark and side-effects for Hub dispatch.
type IngestOutboxResult struct {
	AckedOutboxSeq      int64
	AcceptedMode        protocol.OutboxAcceptedMode
	TaskID              string
	AttemptSucceeded    bool
	PublishedDeliveries []PublishedDeliveryNotice
	Events              []protocol.EventRecord
}

// IngestOutboxBatch processes an ordered attempt.outbox_batch inside a single BEGIN IMMEDIATE transaction.
// Enforces:
// 1. Strict contiguous outboxSeq == last_acked_outbox + 1 (stops immediately on any gap).
// 2. Active vs late_history isolation: inactive/superseded attempts NEVER mutate tasks, inbox_items, or annotations.
func (s *Store) IngestOutboxBatch(
	ctx context.Context,
	machineID, runnerInstanceID string,
	batch protocol.AttemptOutboxBatchFrame,
) (IngestOutboxResult, error) {
	var res IngestOutboxResult
	now := time.Now().UnixMilli()

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		att, err := loadAttemptRecordTx(ctx, tx, batch.AttemptID)
		if err != nil {
			return err
		}
		if machineID != "" && att.MachineID != machineID {
			return fmt.Errorf("%w: attempt %s belongs to machine %s, not %s", ErrOwnershipMismatch, batch.AttemptID, att.MachineID, machineID)
		}
		if batch.LeaseToken == "" || att.LeaseToken != batch.LeaseToken {
			return fmt.Errorf("%w: invalid leaseToken for attempt %s", ErrForbidden, batch.AttemptID)
		}

		res.TaskID = att.TaskID

		var activeAttemptID sql.NullString
		var repoIdentity string
		if err := tx.QueryRowContext(ctx, `
			SELECT t.active_attempt_id, p.repo_identity
			FROM tasks t JOIN projects p ON p.project_id = t.project_id
			WHERE t.task_id = ?
		`, att.TaskID).Scan(&activeAttemptID, &repoIdentity); err != nil {
			return err
		}

		isActive := protocol.IsActiveAttemptState(att.State) && activeAttemptID.Valid && activeAttemptID.String == att.AttemptID
		mode := protocol.OutboxModeLateHistory
		if isActive {
			mode = protocol.OutboxModeActive
		}
		res.AcceptedMode = mode

		acked := att.LastAckedOutbox

		for _, item := range batch.Items {
			if item.OutboxSeq <= acked {
				// Idempotent duplicate replay
				continue
			}
			if item.OutboxSeq != acked+1 {
				// Gap detected: STOP advancing watermark immediately so Runner retransmits from acked+1
				break
			}

			ts := item.TS
			if ts <= 0 {
				ts = now
			}
			if mode == protocol.OutboxModeLateHistory {
				if err := applyLateHistoryItemTx(ctx, tx, att, repoIdentity, item, ts, &res.Events); err != nil {
					return err
				}
			} else {
				if err := applyActiveOutboxItemTx(ctx, tx, &att, repoIdentity, runnerInstanceID, item, ts, &res); err != nil {
					return err
				}
			}
			acked = item.OutboxSeq
		}

		if acked != att.LastAckedOutbox || (runnerInstanceID != "" && att.RunnerInstanceID == "" && mode == protocol.OutboxModeActive) {
			if mode == protocol.OutboxModeActive && runnerInstanceID != "" {
				if _, err := tx.ExecContext(ctx, `
					UPDATE attempts
					SET last_acked_outbox = ?, runner_instance_id = COALESCE(NULLIF(runner_instance_id, ''), ?)
					WHERE attempt_id = ?
				`, acked, runnerInstanceID, att.AttemptID); err != nil {
					return err
				}
			} else {
				if _, err := tx.ExecContext(ctx, `
					UPDATE attempts SET last_acked_outbox = ? WHERE attempt_id = ?
				`, acked, att.AttemptID); err != nil {
					return err
				}
			}
		}

		res.AckedOutboxSeq = acked
		return nil
	})
	return res, err
}

func applyLateHistoryItemTx(
	ctx context.Context,
	tx *sql.Tx,
	att AttemptRecord,
	repoIdentity string,
	item protocol.OutboxItem,
	ts int64,
	events *[]protocol.EventRecord,
) error {
	payload := cloneMap(item.Payload)
	payload["late_arrival"] = true
	payload["lateFrameType"] = string(item.FrameType)

	switch item.FrameType {
	case protocol.OutboxFrameDeliveryPublished:
		deliveryID := stringField(item.Payload, "deliveryId")
		if deliveryID == "" {
			deliveryID = newID("dlv")
		}
		baseSHA := stringField(item.Payload, "baseSha")
		if baseSHA == "" {
			baseSHA = att.BaseSHA
		}
		headSHA := stringField(item.Payload, "headSha")
		gitRef := stringField(item.Payload, "gitRef")
		if gitRef == "" {
			gitRef = "refs/orbit/deliveries/" + deliveryID
		}
		summary := stringField(item.Payload, "summary")
		manifest := extractDeliveryManifest(item.Payload)
		kind := resolveDeliveryKind(protocol.DeliveryKind(stringField(item.Payload, "kind")), manifest)
		manifestBytes, _ := json.Marshal(normalizeManifest(manifest))

		// Insert strictly as late_arrival = 1 read-only history; NEVER touch tasks, inbox_items, or annotations!
		_, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO deliveries (
				delivery_id, project_id, task_id, attempt_id, source_machine_id,
				repo_identity, base_sha, head_sha, git_ref, kind, summary,
				manifest_json, verified_machines_json, ryugi_sync_state, late_arrival, pinned, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '{}', 'none', 1, 1, ?)
		`, deliveryID, att.ProjectID, att.TaskID, att.AttemptID, att.MachineID,
			repoIdentity, baseSHA, headSHA, gitRef, string(kind), summary,
			string(manifestBytes), ts)
		if err != nil {
			return fmt.Errorf("insert late_history delivery: %w", err)
		}
		ev, inserted, err := insertEventIdempotentTx(ctx, tx, eventInsert{
			EventID:    item.EventID,
			StreamID:   "attempt:" + att.AttemptID,
			ProjectID:  att.ProjectID,
			TaskID:     att.TaskID,
			AttemptID:  att.AttemptID,
			ActorKind:  protocol.ActorRunner,
			ActorID:    att.MachineID,
			Kind:       protocol.EventDeliveryPublished,
			Visibility: protocol.VisibilityAudit,
			Payload: map[string]any{
				"deliveryId":   deliveryID,
				"baseSha":      baseSHA,
				"headSha":      headSHA,
				"summary":      summary,
				"late_arrival": true,
			},
			CreatedAt: ts,
		})
		if err != nil {
			return err
		}
		if inserted {
			*events = append(*events, ev)
		}
		return nil

	case protocol.OutboxFrameEvents:
		rawEvents := extractSubEvents(item)
		for idx, sub := range rawEvents {
			subID := sub.EventID
			if subID == "" {
				subID = fmt.Sprintf("%s:%d", item.EventID, idx)
			}
			vis := sub.Visibility
			if vis != protocol.VisibilityRawACP {
				vis = protocol.VisibilityAudit
			}
			subPayload := cloneMap(sub.Payload)
			subPayload["late_arrival"] = true
			ev, inserted, err := insertEventIdempotentTx(ctx, tx, eventInsert{
				EventID:    subID,
				StreamID:   "attempt:" + att.AttemptID,
				ProjectID:  att.ProjectID,
				TaskID:     att.TaskID,
				AttemptID:  att.AttemptID,
				ActorKind:  sub.ActorKind,
				ActorID:    sub.ActorID,
				Kind:       sub.Kind,
				Visibility: vis,
				Payload:    subPayload,
				CreatedAt:  sub.CreatedAt,
			})
			if err != nil {
				return err
			}
			if inserted {
				*events = append(*events, ev)
			}
		}
		return nil

	default:
		// Any late state/approval/board frame goes ONLY into audit events with late_arrival=true
		ev, inserted, err := insertEventIdempotentTx(ctx, tx, eventInsert{
			EventID:    item.EventID,
			StreamID:   "attempt:" + att.AttemptID,
			ProjectID:  att.ProjectID,
			TaskID:     att.TaskID,
			AttemptID:  att.AttemptID,
			ActorKind:  protocol.ActorRunner,
			ActorID:    att.MachineID,
			Kind:       protocol.EventAttemptStateChanged,
			Visibility: protocol.VisibilityAudit,
			Payload:    payload,
			CreatedAt:  ts,
		})
		if err != nil {
			return err
		}
		if inserted {
			*events = append(*events, ev)
		}
		return nil
	}
}

func applyActiveOutboxItemTx(
	ctx context.Context,
	tx *sql.Tx,
	att *AttemptRecord,
	repoIdentity, runnerInstanceID string,
	item protocol.OutboxItem,
	ts int64,
	res *IngestOutboxResult,
) error {
	switch item.FrameType {
	case protocol.OutboxFrameEvents:
		subEvents := extractSubEvents(item)
		for idx, sub := range subEvents {
			subID := sub.EventID
			if subID == "" {
				if len(subEvents) == 1 && item.EventID != "" {
					subID = item.EventID
				} else {
					subID = fmt.Sprintf("%s:%d", item.EventID, idx)
				}
			}
			streamID := sub.StreamID
			if streamID == "" {
				streamID = "task:" + att.TaskID
			}
			ev, inserted, err := insertEventIdempotentTx(ctx, tx, eventInsert{
				EventID:    subID,
				StreamID:   streamID,
				ProjectID:  att.ProjectID,
				TaskID:     att.TaskID,
				AttemptID:  att.AttemptID,
				ActorKind:  sub.ActorKind,
				ActorID:    sub.ActorID,
				Kind:       sub.Kind,
				Visibility: sub.Visibility,
				Payload:    sub.Payload,
				CreatedAt:  sub.CreatedAt,
			})
			if err != nil {
				return err
			}
			if inserted {
				res.Events = append(res.Events, ev)
			}
		}
		return nil

	case protocol.OutboxFrameState:
		return applyStateFrameTx(ctx, tx, att, item, ts, res)

	case protocol.OutboxFrameApprovalRequest:
		return createApprovalTx(ctx, tx, *att, runnerInstanceID, item.Payload, ts, &res.Events)

	case protocol.OutboxFrameApprovalAck:
		approvalID := stringField(item.Payload, "approvalId")
		decisionID := stringField(item.Payload, "decisionId")
		outcome := protocol.ApprovalState(stringField(item.Payload, "outcome"))
		return consumeApprovalAckByDecisionTx(ctx, tx, approvalID, decisionID, outcome, ts, &res.Events)

	case protocol.OutboxFrameDeliveryPublished:
		return applyDeliveryPublishedFrameTx(ctx, tx, att, repoIdentity, item, ts, res)

	case protocol.OutboxFrameBoardMessage:
		author := stringField(item.Payload, "authorName")
		if author == "" {
			author = "coordinator"
		}
		mention := stringField(item.Payload, "targetMention")
		body := stringField(item.Payload, "body")
		extKey := stringField(item.Payload, "externalKey")
		if extKey == "" {
			extKey = item.EventID
		}
		_, ev, err := insertBoardMessageTx(ctx, tx, att.TaskID, att.ProjectID, author, mention, body, extKey, ts)
		if err != nil {
			return err
		}
		if ev != nil {
			res.Events = append(res.Events, *ev)
		}
		return nil

	case protocol.OutboxFrameBoardMember:
		memberName := stringField(item.Payload, "memberName")
		if memberName == "" {
			return nil
		}
		state := protocol.BoardMemberState(stringField(item.Payload, "state"))
		if !state.IsValid() {
			state = protocol.BoardMemberWorking
		}
		step := stringField(item.Payload, "currentStep")
		consumedSeq := int64Field(item.Payload, "lastConsumedSeq")
		workerTaskID := stringField(item.Payload, "workerTaskId")
		machineID := stringField(item.Payload, "machineId")
		if machineID == "" {
			machineID = att.MachineID
		}
		adapter := protocol.Adapter(stringField(item.Payload, "adapter"))
		if adapter == "" {
			adapter = att.Adapter
		}
		ev, err := upsertBoardMemberTx(ctx, tx, protocol.BoardMemberCursor{
			TaskID:          att.TaskID,
			ProjectID:       att.ProjectID,
			MemberName:      memberName,
			MachineID:       machineID,
			Adapter:         adapter,
			State:           state,
			LastConsumedSeq: consumedSeq,
			CurrentStep:     step,
			WorkerTaskID:    workerTaskID,
			UpdatedAt:       ts,
		})
		if err != nil {
			return err
		}
		if ev != nil {
			res.Events = append(res.Events, *ev)
		}
		return nil

	default:
		return nil
	}
}

func applyStateFrameTx(
	ctx context.Context,
	tx *sql.Tx,
	att *AttemptRecord,
	item protocol.OutboxItem,
	ts int64,
	res *IngestOutboxResult,
) error {
	newState := protocol.AttemptState(stringField(item.Payload, "state"))
	if !newState.IsValid() {
		return nil
	}

	provSID := stringField(item.Payload, "providerSessionId")
	worktree := stringField(item.Payload, "worktreePath")
	baseSHA := stringField(item.Payload, "baseSha")
	compSrc := stringField(item.Payload, "completionSource")
	errCode := stringField(item.Payload, "errorCode")
	errDetail := stringField(item.Payload, "errorDetail")
	waitingReason := stringField(item.Payload, "waitingReason")
	resumeState := stringField(item.Payload, "resumeState")
	sessionLocked := boolField(item.Payload, "sessionLocked")

	var tokenUsageVal any
	if tu, ok := item.Payload["tokenUsage"]; ok && tu != nil {
		if b, err := json.Marshal(tu); err == nil {
			tokenUsageVal = string(b)
		}
	}

	// Update agent_sessions metadata when provided
	if att.WBSessionID != "" {
		sessState := protocol.SessionLive
		if newState == protocol.AttemptNeedsVerification || sessionLocked {
			sessState = protocol.SessionDead
		}
		lockInt := 0
		if sessionLocked || newState == protocol.AttemptNeedsVerification {
			lockInt = 1
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE agent_sessions
			SET provider_session_id = COALESCE(NULLIF(?, ''), provider_session_id),
			    cwd = COALESCE(NULLIF(?, ''), cwd),
			    runtime_state = ?,
			    locked = CASE WHEN ? = 1 THEN 1 ELSE locked END,
			    last_activity_at = ?
			WHERE wb_session_id = ?
		`, provSID, worktree, string(sessState), lockInt, ts, att.WBSessionID)
		if err != nil {
			return fmt.Errorf("update agent_session: %w", err)
		}
	}

	isTerminal := !protocol.IsActiveAttemptState(newState)
	var finishedAtVal any
	if isTerminal {
		finishedAtVal = ts
	}
	var compSrcVal, errCodeVal, errDetailVal, resumeStateVal any
	if compSrc != "" && protocol.CompletionSource(compSrc).IsValid() {
		compSrcVal = compSrc
	}
	if errCode != "" {
		errCodeVal = errCode
	}
	if errDetail != "" {
		errDetailVal = errDetail
	}
	if resumeState != "" && protocol.ResumeState(resumeState).IsValid() {
		resumeStateVal = resumeState
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE attempts
		SET state = ?,
		    worktree_path = COALESCE(NULLIF(?, ''), worktree_path),
		    base_sha = COALESCE(NULLIF(?, ''), base_sha),
		    completion_source = COALESCE(?, completion_source),
		    error_code = COALESCE(?, error_code),
		    error_detail = COALESCE(?, error_detail),
		    resume_state = COALESCE(?, resume_state),
		    token_usage_json = COALESCE(?, token_usage_json),
		    finished_at = COALESCE(?, finished_at)
		WHERE attempt_id = ?
	`, string(newState), worktree, baseSHA, compSrcVal, errCodeVal, errDetailVal, resumeStateVal, tokenUsageVal, finishedAtVal, att.AttemptID); err != nil {
		return fmt.Errorf("update attempt state: %w", err)
	}
	att.State = newState
	if worktree != "" {
		att.WorktreePath = worktree
	}
	if baseSHA != "" {
		att.BaseSHA = baseSHA
	}

	// Project onto tasks, annotations, and inbox_items inside the SAME transaction
	switch newState {
	case protocol.AttemptPreparing, protocol.AttemptRunning, protocol.AttemptSwarmMonitoring:
		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks SET state = 'running', waiting_reason = NULL, version = version + 1, updated_at = ?
			WHERE task_id = ? AND active_attempt_id = ?
		`, ts, att.TaskID, att.AttemptID); err != nil {
			return err
		}
		_, _ = tx.ExecContext(ctx, `
			UPDATE inbox_items SET state = 'resolved', resolved_at = ?, updated_at = ?
			WHERE inbox_id = ? AND state = 'open'
		`, ts, ts, "inb:blk:"+att.AttemptID)

	case protocol.AttemptWaitingResource:
		reason := waitingReason
		if reason == "" {
			reason = "等待执行机独占资源就绪"
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks SET state = 'waiting_resource', waiting_reason = ?, version = version + 1, updated_at = ?
			WHERE task_id = ? AND active_attempt_id = ?
		`, reason, ts, att.TaskID, att.AttemptID); err != nil {
			return err
		}
		if err := upsertBlockedInboxTx(ctx, tx, att.ProjectID, att.TaskID, att.AttemptID, "等待物理资源", reason, 2, ts); err != nil {
			return err
		}

	case protocol.AttemptWaitingApproval:
		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks SET state = 'waiting_approval', version = version + 1, updated_at = ?
			WHERE task_id = ? AND active_attempt_id = ?
		`, ts, att.TaskID, att.AttemptID); err != nil {
			return err
		}

	case protocol.AttemptSucceeded:
		res.AttemptSucceeded = true
		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks SET state = 'in_review', active_attempt_id = NULL, waiting_reason = NULL, version = version + 1, updated_at = ?
			WHERE task_id = ? AND active_attempt_id = ?
		`, ts, att.TaskID, att.AttemptID); err != nil {
			return err
		}
		_, _ = tx.ExecContext(ctx, `
			UPDATE inbox_items SET state = 'resolved', resolved_at = ?, updated_at = ?
			WHERE inbox_id = ? AND state = 'open'
		`, ts, ts, "inb:blk:"+att.AttemptID)

		// Mark queued annotations owned by THIS attempt as addressed
		var resDlvVal any
		if att.OutputDeliveryID != "" {
			resDlvVal = att.OutputDeliveryID
		}
		updRes, err := tx.ExecContext(ctx, `
			UPDATE annotations
			SET status = 'addressed', resolution_delivery_id = ?, resolved_at = ?
			WHERE triggered_attempt_id = ? AND status = 'queued'
		`, resDlvVal, ts, att.AttemptID)
		if err != nil {
			return err
		}
		if n, _ := updRes.RowsAffected(); n > 0 {
			evAnn, inserted, err := insertEventIdempotentTx(ctx, tx, eventInsert{
				EventID:    item.EventID + ":ann",
				StreamID:   "task:" + att.TaskID,
				ProjectID:  att.ProjectID,
				TaskID:     att.TaskID,
				AttemptID:  att.AttemptID,
				ActorKind:  protocol.ActorRunner,
				ActorID:    att.MachineID,
				Kind:       protocol.EventAnnotationAddressed,
				Visibility: protocol.VisibilityTimeline,
				Payload: map[string]any{
					"count":                n,
					"resolutionDeliveryId": att.OutputDeliveryID,
				},
				CreatedAt: ts,
			})
			if err != nil {
				return err
			}
			if inserted {
				res.Events = append(res.Events, evAnn)
			}
		}

	case protocol.AttemptNeedsVerification:
		reason := errDetail
		if reason == "" {
			reason = waitingReason
		}
		if reason == "" {
			reason = "运行静默超时或状态未知，请人工核对时间线与现场后再决定是否重跑"
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks SET state = 'in_review', active_attempt_id = NULL, waiting_reason = ?, version = version + 1, updated_at = ?
			WHERE task_id = ? AND active_attempt_id = ?
		`, reason, ts, att.TaskID, att.AttemptID); err != nil {
			return err
		}
		if err := upsertBlockedInboxTx(ctx, tx, att.ProjectID, att.TaskID, att.AttemptID, "待核对现场 · 需要人工确认", reason, 1, ts); err != nil {
			return err
		}

	case protocol.AttemptFailed, protocol.AttemptExpired:
		reason := errDetail
		if reason == "" {
			reason = errCode
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks SET state = 'failed', active_attempt_id = NULL, waiting_reason = ?, version = version + 1, updated_at = ?
			WHERE task_id = ? AND active_attempt_id = ?
		`, reason, ts, att.TaskID, att.AttemptID); err != nil {
			return err
		}
		if err := releaseQueuedAnnotationsTx(ctx, tx, att.AttemptID); err != nil {
			return err
		}
		if err := expirePendingApprovalsForAttemptTx(ctx, tx, att.AttemptID, ts); err != nil {
			return err
		}
		if err := upsertBlockedInboxTx(ctx, tx, att.ProjectID, att.TaskID, att.AttemptID, "任务执行失败", reason, 2, ts); err != nil {
			return err
		}

	case protocol.AttemptCancelled:
		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks SET state = 'cancelled', active_attempt_id = NULL, waiting_reason = NULL, version = version + 1, updated_at = ?
			WHERE task_id = ? AND active_attempt_id = ?
		`, ts, att.TaskID, att.AttemptID); err != nil {
			return err
		}
		if err := releaseQueuedAnnotationsTx(ctx, tx, att.AttemptID); err != nil {
			return err
		}
		if err := expirePendingApprovalsForAttemptTx(ctx, tx, att.AttemptID, ts); err != nil {
			return err
		}
		_, _ = tx.ExecContext(ctx, `
			UPDATE inbox_items SET state = 'resolved', resolved_at = ?, updated_at = ?
			WHERE inbox_id = ? AND state = 'open'
		`, ts, ts, "inb:blk:"+att.AttemptID)
	}

	evKind := protocol.EventAttemptStateChanged
	evVis := protocol.VisibilityTimeline
	if isTerminal {
		evKind = protocol.EventAttemptFinished
		if newState == protocol.AttemptNeedsVerification || newState == protocol.AttemptFailed || newState == protocol.AttemptExpired {
			evVis = protocol.VisibilityInbox
		}
	}
	ev, inserted, err := insertEventIdempotentTx(ctx, tx, eventInsert{
		EventID:    item.EventID,
		StreamID:   "task:" + att.TaskID,
		ProjectID:  att.ProjectID,
		TaskID:     att.TaskID,
		AttemptID:  att.AttemptID,
		ActorKind:  protocol.ActorRunner,
		ActorID:    att.MachineID,
		Kind:       evKind,
		Visibility: evVis,
		Payload:    cloneMap(item.Payload),
		CreatedAt:  ts,
	})
	if err != nil {
		return err
	}
	if inserted {
		res.Events = append(res.Events, ev)
	}
	return nil
}

func applyDeliveryPublishedFrameTx(
	ctx context.Context,
	tx *sql.Tx,
	att *AttemptRecord,
	repoIdentity string,
	item protocol.OutboxItem,
	ts int64,
	res *IngestOutboxResult,
) error {
	deliveryID := stringField(item.Payload, "deliveryId")
	if deliveryID == "" {
		deliveryID = newID("dlv")
	}
	baseSHA := stringField(item.Payload, "baseSha")
	if baseSHA == "" {
		baseSHA = att.BaseSHA
	}
	headSHA := stringField(item.Payload, "headSha")
	gitRef := stringField(item.Payload, "gitRef")
	if gitRef == "" {
		gitRef = "refs/orbit/deliveries/" + deliveryID
	}
	summary := stringField(item.Payload, "summary")
	manifest := extractDeliveryManifest(item.Payload)
	kind := resolveDeliveryKind(protocol.DeliveryKind(stringField(item.Payload, "kind")), manifest)
	ryugiSyncState := protocol.RyugiSyncNone
	if len(manifest.RyugiArtifacts) > 0 {
		ryugiSyncState = protocol.RyugiSyncPending
	}
	manifestBytes, err := json.Marshal(normalizeManifest(manifest))
	if err != nil {
		return err
	}

	insRes, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO deliveries (
			delivery_id, project_id, task_id, attempt_id, source_machine_id,
			repo_identity, base_sha, head_sha, git_ref, kind, summary,
			manifest_json, verified_machines_json, ryugi_sync_state, late_arrival, pinned, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '{}', ?, 0, 1, ?)
	`, deliveryID, att.ProjectID, att.TaskID, att.AttemptID, att.MachineID,
		repoIdentity, baseSHA, headSHA, gitRef, string(kind), summary,
		string(manifestBytes), string(ryugiSyncState), ts)
	if err != nil {
		return fmt.Errorf("insert delivery from outbox: %w", err)
	}

	att.OutputDeliveryID = deliveryID
	if _, err := tx.ExecContext(ctx, `
		UPDATE attempts SET output_delivery_id = ? WHERE attempt_id = ?
	`, deliveryID, att.AttemptID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks SET latest_delivery_id = ?, version = version + 1, updated_at = ?
		WHERE task_id = ?
	`, deliveryID, ts, att.TaskID); err != nil {
		return err
	}

	inboxID := "inb:dlv:" + deliveryID
	shortHead := headSHA
	if len(shortHead) > 7 {
		shortHead = shortHead[:7]
	}
	inboxTitle := fmt.Sprintf("待审交付 · %s (%s)", att.ProjectID, shortHead)
	inboxSummary := summary
	if inboxSummary == "" {
		inboxSummary = fmt.Sprintf("+%d -%d · %d files", manifest.Insertions, manifest.Deletions, manifest.FilesChanged)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO inbox_items (
			inbox_id, project_id, task_id, category, ref_id, title, summary,
			priority, state, created_at, updated_at
		) VALUES (?, ?, ?, 'delivery_review', ?, ?, ?, 2, 'open', ?, ?)
		ON CONFLICT(inbox_id) DO UPDATE SET
			title = excluded.title,
			summary = excluded.summary,
			state = 'open',
			updated_at = excluded.updated_at,
			resolved_at = NULL
	`, inboxID, att.ProjectID, att.TaskID, deliveryID, inboxTitle, inboxSummary, ts, ts); err != nil {
		return err
	}

	if n, _ := insRes.RowsAffected(); n > 0 {
		res.PublishedDeliveries = append(res.PublishedDeliveries, PublishedDeliveryNotice{
			DeliveryID:     deliveryID,
			RyugiArtifacts: manifest.RyugiArtifacts,
		})
	}

	ev, inserted, err := insertEventIdempotentTx(ctx, tx, eventInsert{
		EventID:    item.EventID,
		StreamID:   "task:" + att.TaskID,
		ProjectID:  att.ProjectID,
		TaskID:     att.TaskID,
		AttemptID:  att.AttemptID,
		ActorKind:  protocol.ActorRunner,
		ActorID:    att.MachineID,
		Kind:       protocol.EventDeliveryPublished,
		Visibility: protocol.VisibilityInbox,
		Payload: map[string]any{
			"deliveryId":     deliveryID,
			"baseSha":        baseSHA,
			"headSha":        headSHA,
			"filesChanged":   manifest.FilesChanged,
			"insertions":     manifest.Insertions,
			"deletions":      manifest.Deletions,
			"ryugiArtifacts": manifest.RyugiArtifacts,
			"summary":        summary,
		},
		CreatedAt: ts,
	})
	if err != nil {
		return err
	}
	if inserted {
		res.Events = append(res.Events, ev)
	}
	return nil
}

func insertEventIdempotentTx(ctx context.Context, tx *sql.Tx, e eventInsert) (protocol.EventRecord, bool, error) {
	if e.EventID == "" {
		e.EventID = newID("evt")
	}
	if e.CreatedAt == 0 {
		e.CreatedAt = time.Now().UnixMilli()
	}
	if e.Payload == nil {
		e.Payload = map[string]any{}
	}
	if e.ActorKind == "" || !e.ActorKind.IsValid() {
		e.ActorKind = protocol.ActorRunner
	}
	if e.ActorID == "" {
		e.ActorID = string(e.ActorKind)
	}
	if e.Visibility == "" || !e.Visibility.IsValid() {
		e.Visibility = protocol.VisibilityRawACP
	}
	payloadBytes, err := json.Marshal(e.Payload)
	if err != nil {
		return protocol.EventRecord{}, false, err
	}
	var pid, tid, aid any
	if e.ProjectID != "" {
		pid = e.ProjectID
	}
	if e.TaskID != "" {
		tid = e.TaskID
	}
	if e.AttemptID != "" {
		aid = e.AttemptID
	}
	res, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO events (
			event_id, stream_id, project_id, task_id, attempt_id,
			actor_kind, actor_id, kind, visibility, payload_json, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, e.EventID, e.StreamID, pid, tid, aid,
		string(e.ActorKind), e.ActorID, string(e.Kind), string(e.Visibility),
		string(payloadBytes), e.CreatedAt)
	if err != nil {
		return protocol.EventRecord{}, false, fmt.Errorf("insert event %s: %w", e.Kind, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return protocol.EventRecord{}, false, nil
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return protocol.EventRecord{}, false, err
	}
	return protocol.EventRecord{
		Seq:        seq,
		EventID:    e.EventID,
		StreamID:   e.StreamID,
		ProjectID:  e.ProjectID,
		TaskID:     e.TaskID,
		AttemptID:  e.AttemptID,
		ActorKind:  e.ActorKind,
		ActorID:    e.ActorID,
		Kind:       e.Kind,
		Visibility: e.Visibility,
		Payload:    e.Payload,
		CreatedAt:  e.CreatedAt,
	}, true, nil
}

func extractSubEvents(item protocol.OutboxItem) []eventInsert {
	if rawList, ok := item.Payload["events"].([]any); ok && len(rawList) > 0 {
		out := make([]eventInsert, 0, len(rawList))
		for _, entry := range rawList {
			m, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			out = append(out, mapToEventInsert(m, item.TS))
		}
		return out
	}
	return []eventInsert{mapToEventInsert(item.Payload, item.TS)}
}

func mapToEventInsert(m map[string]any, fallbackTS int64) eventInsert {
	e := eventInsert{
		EventID:    stringField(m, "eventId"),
		StreamID:   stringField(m, "streamId"),
		ActorKind:  protocol.ActorKind(stringField(m, "actorKind")),
		ActorID:    stringField(m, "actorId"),
		Kind:       protocol.EventKind(stringField(m, "kind")),
		Visibility: protocol.EventVisibility(stringField(m, "visibility")),
		CreatedAt:  int64Field(m, "createdAt"),
	}
	if e.CreatedAt <= 0 {
		e.CreatedAt = fallbackTS
	}
	if e.Kind == "" {
		e.Kind = protocol.EventACPMessageBatch
	}
	if e.ActorKind == "" {
		e.ActorKind = protocol.ActorAgent
	}
	if e.Visibility == "" {
		e.Visibility = protocol.VisibilityRawACP
	}
	if subP, ok := m["payload"].(map[string]any); ok {
		e.Payload = cloneMap(subP)
	} else {
		e.Payload = cloneMap(m)
	}
	return e
}

func extractDeliveryManifest(payload map[string]any) protocol.DeliveryManifest {
	var m protocol.DeliveryManifest
	if raw, ok := payload["manifest"]; ok && raw != nil {
		if b, err := json.Marshal(raw); err == nil {
			_ = json.Unmarshal(b, &m)
		}
	}
	return m
}

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func boolField(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	if v, ok := m[key].(bool); ok {
		return v
	}
	return false
}

func int64Field(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	default:
		return 0
	}
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
