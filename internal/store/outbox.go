package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/gitops"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

var blobSHA256Regex = regexp.MustCompile(`^[0-9a-f]{64}$`)

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
	AutoConsumeQueue    bool
	SteerInputID        string
	CancelRequested     bool
	PublishedDeliveries []PublishedDeliveryNotice
	Events              []protocol.EventRecord
}

// IngestOutboxBatch processes an ordered attempt.outbox_batch inside a single BEGIN IMMEDIATE transaction.
// Enforces:
// 1. Strict contiguous outboxSeq == last_acked_outbox + 1 (stops immediately on any gap).
// 2. Same-transaction lease expiration check (never extends/projects an expired lease).
// 3. Per-frame active vs late_history projection isolation: once a frame transitions an attempt to
//    terminal (or its lease is already expired), all subsequent frames in the same batch go strictly
//    to late_history and NEVER mutate tasks, inbox_items, or annotations.
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
		var repoIdentity, mirrorBarePath string
		if err := tx.QueryRowContext(ctx, `
			SELECT t.active_attempt_id, p.repo_identity, p.mirror_bare_path
			FROM tasks t JOIN projects p ON p.project_id = t.project_id
			WHERE t.task_id = ?
		`, att.TaskID).Scan(&activeAttemptID, &repoIdentity, &mirrorBarePath); err != nil {
			return err
		}

		// B4: Enforce lease expiration inside the SAME BEGIN IMMEDIATE transaction even if scanner hasn't run yet.
		if protocol.IsActiveAttemptState(att.State) && att.LeaseExpiresAt < now {
			if err := expireAttemptInTx(ctx, tx, &att, now, "outbox-lease-check", &res.Events); err != nil {
				return err
			}
			activeAttemptID.Valid = false
			activeAttemptID.String = ""
		}

		initiallyActive := protocol.IsActiveAttemptState(att.State) && activeAttemptID.Valid && activeAttemptID.String == att.AttemptID
		sawLate := false
		sawActive := false

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

			// B4: Check active projection authority PER FRAME.
			frameActive := protocol.IsActiveAttemptState(att.State) && activeAttemptID.Valid && activeAttemptID.String == att.AttemptID
			if !frameActive {
				sawLate = true
				if err := applyLateHistoryItemTx(ctx, tx, att, repoIdentity, mirrorBarePath, item, ts, &res.Events); err != nil {
					return err
				}
			} else {
				sawActive = true
				if err := applyActiveOutboxItemTx(ctx, tx, &att, repoIdentity, mirrorBarePath, runnerInstanceID, item, ts, &res); err != nil {
					return err
				}
				if !protocol.IsActiveAttemptState(att.State) {
					activeAttemptID.Valid = false
					activeAttemptID.String = ""
				}
			}
			acked = item.OutboxSeq
		}

		if sawLate || (!initiallyActive && !sawActive) {
			res.AcceptedMode = protocol.OutboxModeLateHistory
		} else {
			res.AcceptedMode = protocol.OutboxModeActive
		}
		res.CancelRequested = att.CancelRequested && protocol.IsActiveAttemptState(att.State)

		if acked != att.LastAckedOutbox || (runnerInstanceID != "" && att.RunnerInstanceID == "" && initiallyActive) {
			if initiallyActive && runnerInstanceID != "" {
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
	repoIdentity, mirrorBarePath string,
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
		baseSHA := stringField(item.Payload, "baseSha")
		if baseSHA == "" {
			baseSHA = att.BaseSHA
		}
		headSHA := stringField(item.Payload, "headSha")
		gitRef := stringField(item.Payload, "gitRef")
		if gitRef == "" && deliveryID != "" {
			gitRef = "refs/orbit/deliveries/" + deliveryID
		}
		summary := stringField(item.Payload, "summary")
		manifest := extractDeliveryManifest(item.Payload)

		// Idempotent check for duplicate (delivery_id, head_sha)
		if deliveryID != "" {
			var existingHead string
			err := tx.QueryRowContext(ctx, `SELECT head_sha FROM deliveries WHERE delivery_id = ?`, deliveryID).Scan(&existingHead)
			if err == nil {
				return nil
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			// Reject reusing a delivery_id whose previous frozen snapshot still awaits cleanup.
			if err := checkDeliveryCleanupPendingTx(ctx, tx, deliveryID); err != nil {
				return err
			}
		}

		// Verify ref/mirror/blobs before inserting into deliveries (reject corrupt late records into audit only)
		verifiedManifest, verErr := verifyDeliveryPayloadTx(ctx, tx, att.ProjectID, mirrorBarePath, deliveryID, baseSHA, headSHA, gitRef, manifest, true)
		if verErr != nil {
			ev, inserted, err := insertEventIdempotentTx(ctx, tx, eventInsert{
				EventID:    item.EventID,
				StreamID:   "attempt:" + att.AttemptID,
				ProjectID:  att.ProjectID,
				TaskID:     att.TaskID,
				AttemptID:  att.AttemptID,
				ActorKind:  protocol.ActorSystem,
				ActorID:    "outbox-verifier",
				Kind:       protocol.EventAttemptStateChanged,
				Visibility: protocol.VisibilityAudit,
				Payload: map[string]any{
					"late_arrival":  true,
					"lateFrameType": string(item.FrameType),
					"deliveryId":    deliveryID,
					"rejected":      true,
					"reason":        verErr.Error(),
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
		}

		kind := resolveDeliveryKind(protocol.DeliveryKind(stringField(item.Payload, "kind")), verifiedManifest)
		manifestBytes, _ := json.Marshal(normalizeManifest(verifiedManifest))

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
	repoIdentity, mirrorBarePath, runnerInstanceID string,
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
		return consumeApprovalAckByDecisionTx(ctx, tx, att.MachineID, runnerInstanceID, att.AttemptID, approvalID, decisionID, outcome, ts, &res.Events)

	case protocol.OutboxFrameDeliveryPublished:
		return applyDeliveryPublishedFrameTx(ctx, tx, att, repoIdentity, mirrorBarePath, item, ts, res)

	case protocol.OutboxFrameBoardAck:
		msgID := stringField(item.Payload, "messageId")
		memberName := stringField(item.Payload, "memberName")
		seq := int64Field(item.Payload, "seq")
		ev, err := markBoardMessageConsumedTx(ctx, tx, att.TaskID, msgID, seq, memberName, ts)
		if err != nil {
			return err
		}
		if ev != nil {
			res.Events = append(res.Events, *ev)
		}
		return nil

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
	stopReason := stringField(item.Payload, "stopReason")
	errCode := stringField(item.Payload, "errorCode")
	errDetail := stringField(item.Payload, "errorDetail")
	waitingReason := stringField(item.Payload, "waitingReason")
	resumeState := stringField(item.Payload, "resumeState")
	sessionLocked := boolField(item.Payload, "sessionLocked")
	unresolvedRejected := int64Field(item.Payload, "unresolvedRejectedToolCount")
	unresolvedNonZero := int64Field(item.Payload, "unresolvedNonZeroExitCount")

	// B1: Hub-side authoritative adjudication of succeeded claims.
	// - Abnormal ACP stop_reason (e.g. refusal / max_tokens / max_turn_requests) cannot succeed.
	// - Non-_scratch project / annotation turns MUST have a verified output_delivery_id before succeeding
	//   or marking queued annotations as addressed.
	// - Unresolved tool rejections or non-zero exits require verification rather than clean success.
	if newState == protocol.AttemptSucceeded {
		if stopReason != "" && stopReason != "end_turn" {
			newState = protocol.AttemptFailed
			if errCode == "" {
				errCode = "ABNORMAL_STOP_REASON"
			}
			if errDetail == "" {
				errDetail = fmt.Sprintf("agent stopped with %s", stopReason)
			}
		} else {
			var queuedAnnCount int
			_ = tx.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM annotations
				WHERE triggered_attempt_id = ? AND status = 'queued'
			`, att.AttemptID).Scan(&queuedAnnCount)

			requiresDelivery := queuedAnnCount > 0 ||
				att.TriggerKind == protocol.TriggerAnnotationBatch
			if requiresDelivery && att.OutputDeliveryID == "" {
				newState = protocol.AttemptFailed
				if errCode == "" {
					errCode = "NO_VERIFIED_DELIVERY"
				}
				if errDetail == "" {
					errDetail = "批注修订轮未产出已核验变更，已释放未处理批注"
				}
			} else if unresolvedRejected > 0 || unresolvedNonZero > 0 {
				newState = protocol.AttemptNeedsVerification
				if errCode == "" {
					errCode = "UNRESOLVED_TOOL_FAILURE"
				}
				if errDetail == "" {
					errDetail = "本轮存在未解决的工具拒绝或非零失败，请人工核对后再决定是否续跑"
				}
			}
		}
	}

	var tokenUsageVal any
	if tu, ok := item.Payload["tokenUsage"]; ok && tu != nil {
		if b, err := json.Marshal(tu); err == nil {
			tokenUsageVal = string(b)
		}
	}
	var folderChangesVal any
	if fc, ok := item.Payload["folderChanges"]; ok && fc != nil {
		if b, err := json.Marshal(fc); err == nil {
			folderChangesVal = string(b)
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
		    folder_changes_json = COALESCE(?, folder_changes_json),
		    finished_at = COALESCE(?, finished_at)
		WHERE attempt_id = ?
	`, string(newState), worktree, baseSHA, compSrcVal, errCodeVal, errDetailVal, resumeStateVal, tokenUsageVal, folderChangesVal, finishedAtVal, att.AttemptID); err != nil {
		return fmt.Errorf("update attempt state: %w", err)
	}
	att.State = newState
	if worktree != "" {
		att.WorktreePath = worktree
	}
	if baseSHA != "" {
		att.BaseSHA = baseSHA
	}

	var revertedDeliveryIDs []string
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
		res.AutoConsumeQueue = true
		// B9: Persist pending_queue_drain=1 inside the terminal transaction so Hub restarts
		// between Outbox commit and queue consumption still deterministically drain the queue.
		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks
			SET state = 'in_review', active_attempt_id = NULL, waiting_reason = NULL,
			    pending_queue_drain = 1, pending_steer_input_id = NULL,
			    version = version + 1, updated_at = ?
			WHERE task_id = ? AND active_attempt_id = ?
		`, ts, att.TaskID, att.AttemptID); err != nil {
			return err
		}
		_, _ = tx.ExecContext(ctx, `
			UPDATE inbox_items SET state = 'resolved', resolved_at = ?, updated_at = ?
			WHERE inbox_id = ? AND state = 'open'
		`, ts, ts, "inb:blk:"+att.AttemptID)

		// B1: ONLY mark queued annotations addressed when a verified output delivery exists.
		if att.OutputDeliveryID != "" {
			affectedDeliveryIDs, err := lookupQueuedAnnotationDeliveryIDsTx(ctx, tx, att.AttemptID)
			if err != nil {
				return err
			}
			updRes, err := tx.ExecContext(ctx, `
				UPDATE annotations
				SET status = 'addressed', resolution_delivery_id = ?, resolved_at = ?
				WHERE triggered_attempt_id = ? AND status = 'queued'
			`, att.OutputDeliveryID, ts, att.AttemptID)
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
						"deliveryIds":          affectedDeliveryIDs,
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
			UPDATE tasks
			SET state = 'in_review', active_attempt_id = NULL, waiting_reason = ?,
			    pending_queue_drain = 0, pending_steer_input_id = NULL,
			    version = version + 1, updated_at = ?
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
			UPDATE tasks
			SET state = 'failed', active_attempt_id = NULL, waiting_reason = ?,
			    pending_queue_drain = 0, pending_steer_input_id = NULL,
			    version = version + 1, updated_at = ?
			WHERE task_id = ? AND active_attempt_id = ?
		`, reason, ts, att.TaskID, att.AttemptID); err != nil {
			return err
		}
		if ids, err := lookupQueuedAnnotationDeliveryIDsTx(ctx, tx, att.AttemptID); err == nil && len(ids) > 0 {
			revertedDeliveryIDs = ids
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
		drainFlag := 0
		var steerVal any
		if att.CancelReasonKind == "steer" {
			res.AutoConsumeQueue = true
			res.SteerInputID = att.SteerInputID
			drainFlag = 1
			if att.SteerInputID != "" {
				steerVal = att.SteerInputID
			}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks
			SET state = 'cancelled', active_attempt_id = NULL, waiting_reason = NULL,
			    pending_queue_drain = ?, pending_steer_input_id = ?,
			    version = version + 1, updated_at = ?
			WHERE task_id = ? AND active_attempt_id = ?
		`, drainFlag, steerVal, ts, att.TaskID, att.AttemptID); err != nil {
			return err
		}
		if ids, err := lookupQueuedAnnotationDeliveryIDsTx(ctx, tx, att.AttemptID); err == nil && len(ids) > 0 {
			revertedDeliveryIDs = ids
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
	evPayload := cloneMap(item.Payload)
	evPayload["state"] = newState
	if errCode != "" {
		evPayload["errorCode"] = errCode
	}
	if errDetail != "" {
		evPayload["errorDetail"] = errDetail
	}
	if len(revertedDeliveryIDs) > 0 {
		evPayload["revertedDeliveryIds"] = revertedDeliveryIDs
		evPayload["deliveryIds"] = revertedDeliveryIDs
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
		Payload:    evPayload,
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

func lookupQueuedAnnotationDeliveryIDsTx(ctx context.Context, tx *sql.Tx, attemptID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT delivery_id FROM annotations
		WHERE triggered_attempt_id = ? AND status = 'queued'
	`, attemptID)
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

// verifyDeliveryPayloadTx enforces B12 strong delivery verification (gitRef, headSha, bare mirror ref/base/head,
// authoritative diff stats & Ryugi artifacts, and blob existence/size/on-disk presence + ref_count increment).
func verifyDeliveryPayloadTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID, mirrorBarePath, deliveryID, baseSHA, headSHA, gitRef string,
	manifest protocol.DeliveryManifest,
	incrementBlobRefs bool,
) (protocol.DeliveryManifest, error) {
	if strings.TrimSpace(deliveryID) == "" {
		return manifest, fmt.Errorf("%w: deliveryId is required", ErrDeliveryVerifyFailed)
	}
	if strings.TrimSpace(headSHA) == "" {
		return manifest, fmt.Errorf("%w: headSha is required for delivery %s", ErrDeliveryVerifyFailed, deliveryID)
	}
	expectedRef := "refs/orbit/deliveries/" + deliveryID
	if gitRef == "" {
		gitRef = expectedRef
	}
	if gitRef != expectedRef {
		return manifest, fmt.Errorf("%w: gitRef %q must equal %q", ErrDeliveryVerifyFailed, gitRef, expectedRef)
	}

	outManifest := manifest
	if !protocol.IsVirtualProjectID(projectID) && strings.TrimSpace(mirrorBarePath) != "" {
		if strings.TrimSpace(baseSHA) == "" {
			return outManifest, fmt.Errorf("%w: baseSha is required for project delivery %s", ErrDeliveryVerifyFailed, deliveryID)
		}
		if err := gitops.VerifyMirrorDelivery(ctx, mirrorBarePath, deliveryID, baseSHA, headSHA); err != nil {
			if errors.Is(err, gitops.ErrDeliveryRefConflict) {
				return outManifest, fmt.Errorf("%w: %v", ErrConflict, err)
			}
			return outManifest, fmt.Errorf("%w: %v", ErrDeliveryVerifyFailed, err)
		}
		diffFiles, err := gitops.ComputeMirrorDiffStats(ctx, mirrorBarePath, baseSHA, headSHA)
		if err != nil {
			return outManifest, fmt.Errorf("%w: compute diff stats: %v", ErrDeliveryVerifyFailed, err)
		}
		ins, del := 0, 0
		for _, f := range diffFiles {
			ins += f.Insertions
			del += f.Deletions
		}
		outManifest.FilesChanged = len(diffFiles)
		outManifest.Insertions = ins
		outManifest.Deletions = del

		discovered, err := gitops.DiscoverRyugiArtifactsInMirror(ctx, mirrorBarePath, headSHA)
		if err != nil {
			return outManifest, fmt.Errorf("%w: discover ryugi artifacts: %v", ErrDeliveryVerifyFailed, err)
		}
		outManifest.RyugiArtifacts = discovered
	}

	// Verify all declared blobs exist in delivery_blobs, match size, exist on disk, and increment ref_count once per distinct sha256.
	distinctBlobs := map[string]int64{}
	for _, b := range outManifest.Blobs {
		if !blobSHA256Regex.MatchString(b.SHA256) {
			return outManifest, fmt.Errorf("%w: invalid blob sha256 %q", ErrBlobMismatch, b.SHA256)
		}
		if prev, dup := distinctBlobs[b.SHA256]; dup && prev != b.Size {
			return outManifest, fmt.Errorf("%w: conflicting sizes for blob %s", ErrBlobMismatch, b.SHA256)
		}
		distinctBlobs[b.SHA256] = b.Size
	}
	for sha, expectedSize := range distinctBlobs {
		var actualSize int64
		var storagePath string
		err := tx.QueryRowContext(ctx, `SELECT size_bytes, storage_path FROM delivery_blobs WHERE sha256 = ?`, sha).Scan(&actualSize, &storagePath)
		if errors.Is(err, sql.ErrNoRows) {
			return outManifest, fmt.Errorf("%w: blob %s not uploaded", ErrBlobMismatch, sha)
		}
		if err != nil {
			return outManifest, err
		}
		if actualSize != expectedSize {
			return outManifest, fmt.Errorf("%w: blob %s size mismatch (stored=%d manifest=%d)", ErrBlobMismatch, sha, actualSize, expectedSize)
		}
		if storagePath == "" {
			return outManifest, fmt.Errorf("%w: blob %s has empty storage path", ErrBlobMismatch, sha)
		}
		fi, err := os.Stat(storagePath)
		if err != nil {
			return outManifest, fmt.Errorf("%w: blob %s file missing on disk: %v", ErrBlobMismatch, sha, err)
		}
		if fi.Size() != expectedSize {
			return outManifest, fmt.Errorf("%w: blob %s file size mismatch on disk (%d != %d)", ErrBlobMismatch, sha, fi.Size(), expectedSize)
		}
		if incrementBlobRefs {
			if _, err := tx.ExecContext(ctx, `UPDATE delivery_blobs SET ref_count = ref_count + 1 WHERE sha256 = ?`, sha); err != nil {
				return outManifest, err
			}
		}
	}

	return outManifest, nil
}

func applyDeliveryPublishedFrameTx(
	ctx context.Context,
	tx *sql.Tx,
	att *AttemptRecord,
	repoIdentity, mirrorBarePath string,
	item protocol.OutboxItem,
	ts int64,
	res *IngestOutboxResult,
) error {
	deliveryID := stringField(item.Payload, "deliveryId")
	baseSHA := stringField(item.Payload, "baseSha")
	if baseSHA == "" {
		baseSHA = att.BaseSHA
	}
	headSHA := stringField(item.Payload, "headSha")
	gitRef := stringField(item.Payload, "gitRef")
	if gitRef == "" && deliveryID != "" {
		gitRef = "refs/orbit/deliveries/" + deliveryID
	}
	summary := stringField(item.Payload, "summary")
	manifest := extractDeliveryManifest(item.Payload)

	// B13 & NEW-4: Strictly idempotent handling ONLY for duplicate publication by the SAME attempt
	// with identical immutable ownership (project_id, task_id, attempt_id, base_sha, head_sha, git_ref).
	// A later attempt MUST NEVER bind an older attempt's deliveryId as its output_delivery_id.
	if deliveryID != "" {
		var exProjectID, exTaskID, exBaseSHA, exHeadSHA, exGitRef string
		var exAttemptID sql.NullString
		err := tx.QueryRowContext(ctx, `
			SELECT project_id, task_id, attempt_id, base_sha, head_sha, git_ref FROM deliveries WHERE delivery_id = ?
		`, deliveryID).Scan(&exProjectID, &exTaskID, &exAttemptID, &exBaseSHA, &exHeadSHA, &exGitRef)
		if err == nil {
			if exProjectID != att.ProjectID ||
				exTaskID != att.TaskID ||
				!exAttemptID.Valid || exAttemptID.String != att.AttemptID ||
				(baseSHA != "" && exBaseSHA != baseSHA) ||
				exHeadSHA != headSHA ||
				(gitRef != "" && exGitRef != gitRef) {
				return fmt.Errorf("%w: delivery %s already exists with conflicting attempt/ownership metadata", ErrConflict, deliveryID)
			}
			att.OutputDeliveryID = deliveryID
			if _, err := tx.ExecContext(ctx, `
				UPDATE attempts SET output_delivery_id = ?
				WHERE attempt_id = ? AND (output_delivery_id IS NULL OR output_delivery_id = '')
			`, deliveryID, att.AttemptID); err != nil {
				return err
			}
			// Idempotent no-op: do NOT re-open resolved inbox card, do NOT emit duplicate delivery.published event.
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}

	// Atomically reject reusing a delivery_id whose previous frozen snapshot still awaits cleanup.
	if err := checkDeliveryCleanupPendingTx(ctx, tx, deliveryID); err != nil {
		return err
	}

	// B12: Verify mirror ref / headSha / baseSha / blobs before admitting delivery.
	verifiedManifest, err := verifyDeliveryPayloadTx(ctx, tx, att.ProjectID, mirrorBarePath, deliveryID, baseSHA, headSHA, gitRef, manifest, true)
	if err != nil {
		return err
	}

	kind := resolveDeliveryKind(protocol.DeliveryKind(stringField(item.Payload, "kind")), verifiedManifest)
	ryugiSyncState := protocol.RyugiSyncNone
	if len(verifiedManifest.RyugiArtifacts) > 0 {
		ryugiSyncState = protocol.RyugiSyncPending
	}
	manifestBytes, err := json.Marshal(normalizeManifest(verifiedManifest))
	if err != nil {
		return err
	}

	insRes, err := tx.ExecContext(ctx, `
		INSERT INTO deliveries (
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
	// B4: Strictly guard latest_delivery_id mutation by active_attempt_id = att.AttemptID.
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks SET latest_delivery_id = ?, version = version + 1, updated_at = ?
		WHERE task_id = ? AND active_attempt_id = ?
	`, deliveryID, ts, att.TaskID, att.AttemptID); err != nil {
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
		inboxSummary = fmt.Sprintf("+%d -%d · %d files", verifiedManifest.Insertions, verifiedManifest.Deletions, verifiedManifest.FilesChanged)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO inbox_items (
			inbox_id, project_id, task_id, category, ref_id, title, summary,
			priority, state, created_at, updated_at
		) VALUES (?, ?, ?, 'delivery_review', ?, ?, ?, 2, 'open', ?, ?)
		ON CONFLICT(inbox_id) DO NOTHING
	`, inboxID, att.ProjectID, att.TaskID, deliveryID, inboxTitle, inboxSummary, ts, ts); err != nil {
		return err
	}

	if n, _ := insRes.RowsAffected(); n > 0 {
		res.PublishedDeliveries = append(res.PublishedDeliveries, PublishedDeliveryNotice{
			DeliveryID:     deliveryID,
			RyugiArtifacts: verifiedManifest.RyugiArtifacts,
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
			"filesChanged":   verifiedManifest.FilesChanged,
			"insertions":     verifiedManifest.Insertions,
			"deletions":      verifiedManifest.Deletions,
			"ryugiArtifacts": verifiedManifest.RyugiArtifacts,
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
	if len(m.Blobs) == 0 {
		if rawBlobs, ok := payload["blobs"]; ok && rawBlobs != nil {
			if b, err := json.Marshal(rawBlobs); err == nil {
				_ = json.Unmarshal(b, &m.Blobs)
			}
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
