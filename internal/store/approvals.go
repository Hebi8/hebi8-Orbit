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

const approvalSelectSQL = `
	SELECT approval_id, project_id, task_id, attempt_id, kind, action_name, target_env,
	       input_sha, payload_json, payload_sha256, acp_rpc_id, runner_instance_id,
	       options_json, requested_by, state, decision_id, selected_option_id,
	       decided_by, decision_note, expires_at, created_at, decided_at, consumed_at
	FROM approvals
`

func scanApprovalView(scanner interface{ Scan(...any) error }) (protocol.ApprovalView, error) {
	var a protocol.ApprovalView
	var kind, payloadJSON, optionsJSON, reqBy, state string
	var acpRPC, runnerInst, decID, selOpt, decBy, decNote sql.NullString
	var decAt, consAt sql.NullInt64
	if err := scanner.Scan(
		&a.ApprovalID, &a.ProjectID, &a.TaskID, &a.AttemptID,
		&kind, &a.ActionName, &a.TargetEnv, &a.InputSHA,
		&payloadJSON, &a.PayloadSHA256, &acpRPC, &runnerInst,
		&optionsJSON, &reqBy, &state, &decID, &selOpt,
		&decBy, &decNote, &a.ExpiresAt, &a.CreatedAt, &decAt, &consAt,
	); err != nil {
		return a, err
	}
	a.Kind = protocol.ApprovalKind(kind)
	a.RequestedBy = protocol.ApprovalRequestedBy(reqBy)
	a.State = protocol.ApprovalState(state)
	if acpRPC.Valid {
		a.ACPRPCID = acpRPC.String
	}
	if runnerInst.Valid {
		a.RunnerInstanceID = runnerInst.String
	}
	if decID.Valid {
		a.DecisionID = decID.String
	}
	if selOpt.Valid {
		a.SelectedOptionID = selOpt.String
	}
	if decBy.Valid {
		v := protocol.ApprovalDecidedBy(decBy.String)
		a.DecidedBy = &v
	}
	if decNote.Valid {
		a.DecisionNote = decNote.String
	}
	if decAt.Valid {
		v := decAt.Int64
		a.DecidedAt = &v
	}
	if consAt.Valid {
		v := consAt.Int64
		a.ConsumedAt = &v
	}
	a.Payload = map[string]any{}
	if payloadJSON != "" {
		_ = json.Unmarshal([]byte(payloadJSON), &a.Payload)
	}
	a.Options = []protocol.ApprovalOption{}
	if optionsJSON != "" {
		_ = json.Unmarshal([]byte(optionsJSON), &a.Options)
	}
	return a, nil
}

// GetApproval returns one approval by ID.
func (s *Store) GetApproval(ctx context.Context, approvalID string) (protocol.ApprovalView, error) {
	row := s.ro.QueryRowContext(ctx, approvalSelectSQL+` WHERE approval_id = ?`, approvalID)
	ap, err := scanApprovalView(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ap, fmt.Errorf("%w: approval %s", ErrNotFound, approvalID)
	}
	return ap, err
}

// ListApprovals returns approvals optionally filtered by state.
func (s *Store) ListApprovals(ctx context.Context, stateFilter protocol.ApprovalState) ([]protocol.ApprovalView, error) {
	out := []protocol.ApprovalView{}
	var (
		rows *sql.Rows
		err  error
	)
	if stateFilter != "" {
		rows, err = s.ro.QueryContext(ctx, approvalSelectSQL+` WHERE state = ? ORDER BY created_at DESC LIMIT 100`, string(stateFilter))
	} else {
		rows, err = s.ro.QueryContext(ctx, approvalSelectSQL+` ORDER BY created_at DESC LIMIT 100`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		ap, err := scanApprovalView(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ap)
	}
	return out, rows.Err()
}

// DecideApprovalParams contains validated inputs for a human approval decision.
type DecideApprovalParams struct {
	ApprovalID            string
	OptionID              string
	ExpectedAttemptID     string
	ExpectedPayloadSHA256 string
	DecidedBy             protocol.ApprovalDecidedBy
	ActorID               string
	Note                  string
}

// DecideApprovalResult returns the updated approval, machine to notify, wire payload, and events.
type DecideApprovalResult struct {
	Approval  protocol.ApprovalView
	MachineID string
	Decision  protocol.ApprovalDecisionPayload
	Events    []protocol.EventRecord
}

// DecideApproval enforces human identity, option membership, attempt & payload SHA binding,
// expiration, and pending state; transitions to approved_pending_delivery or rejected_pending_delivery,
// resolves the approval inbox card in the SAME transaction, and emits approval.decided.
func (s *Store) DecideApproval(ctx context.Context, p DecideApprovalParams) (DecideApprovalResult, error) {
	var res DecideApprovalResult
	now := time.Now().UnixMilli()

	if p.DecidedBy != protocol.ApprovalDecidedByHuman {
		return res, fmt.Errorf("%w: only human identity may decide approvals", ErrForbidden)
	}
	if p.ActorID == "" {
		p.ActorID = "human"
	}

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, approvalSelectSQL+` WHERE approval_id = ?`, p.ApprovalID)
		ap, err := scanApprovalView(row)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: approval %s", ErrNotFound, p.ApprovalID)
		}
		if err != nil {
			return err
		}

		if ap.State != protocol.ApprovalPending {
			return fmt.Errorf("%w: approval %s is in state %s, expected pending", ErrConflict, ap.ApprovalID, ap.State)
		}
		if ap.ExpiresAt <= now {
			// Lazily mark expired in DB so subsequent reads reflect truth
			_, _ = tx.ExecContext(ctx, `UPDATE approvals SET state = 'expired' WHERE approval_id = ? AND state = 'pending'`, ap.ApprovalID)
			_, _ = tx.ExecContext(ctx, `UPDATE inbox_items SET state = 'resolved', resolved_at = ?, updated_at = ? WHERE inbox_id = ? AND state = 'open'`, now, now, "inb:apr:"+ap.ApprovalID)
			return fmt.Errorf("%w: approval %s has expired", ErrConflict, ap.ApprovalID)
		}
		if p.ExpectedAttemptID == "" || ap.AttemptID != p.ExpectedAttemptID {
			return fmt.Errorf("%w: expectedAttemptId %q does not match approval attempt %q", ErrConflict, p.ExpectedAttemptID, ap.AttemptID)
		}
		if p.ExpectedPayloadSHA256 == "" || ap.PayloadSHA256 != p.ExpectedPayloadSHA256 {
			return fmt.Errorf("%w: expectedPayloadSha256 does not match approval payload digest", ErrConflict)
		}

		var matchedOpt *protocol.ApprovalOption
		for i := range ap.Options {
			if ap.Options[i].ID == p.OptionID {
				matchedOpt = &ap.Options[i]
				break
			}
		}
		if matchedOpt == nil {
			return fmt.Errorf("%w: optionId %q is not in approval options", ErrConflict, p.OptionID)
		}

		nextState := protocol.ApprovalApprovedPendingDelivery
		if isRejectOption(*matchedOpt) {
			nextState = protocol.ApprovalRejectedPendingDelivery
		}

		var machineID string
		if err := tx.QueryRowContext(ctx, `SELECT machine_id FROM attempts WHERE attempt_id = ?`, ap.AttemptID).Scan(&machineID); err != nil {
			return err
		}

		decisionID := newID("dec")
		if _, err := tx.ExecContext(ctx, `
			UPDATE approvals
			SET state = ?, decision_id = ?, selected_option_id = ?, decided_by = ?, decision_note = ?, decided_at = ?
			WHERE approval_id = ? AND state = 'pending'
		`, string(nextState), decisionID, matchedOpt.ID, string(p.DecidedBy), p.Note, now, ap.ApprovalID); err != nil {
			return err
		}

		// Atomically resolve the pending approval inbox card in the SAME transaction
		if _, err := tx.ExecContext(ctx, `
			UPDATE inbox_items
			SET state = 'resolved', resolved_at = ?, updated_at = ?
			WHERE inbox_id = ? AND state = 'open'
		`, now, now, "inb:apr:"+ap.ApprovalID); err != nil {
			return err
		}

		ev, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + ap.TaskID,
			ProjectID:  ap.ProjectID,
			TaskID:     ap.TaskID,
			AttemptID:  ap.AttemptID,
			ActorKind:  protocol.ActorHuman,
			ActorID:    p.ActorID,
			Kind:       protocol.EventApprovalDecided,
			Visibility: protocol.VisibilityInbox,
			Payload: map[string]any{
				"approvalId":       ap.ApprovalID,
				"decisionId":       decisionID,
				"selectedOptionId": matchedOpt.ID,
				"optionKind":       matchedOpt.Kind,
				"state":            nextState,
				"note":             p.Note,
			},
			CreatedAt: now,
		})
		if err != nil {
			return err
		}

		ap.State = nextState
		ap.DecisionID = decisionID
		ap.SelectedOptionID = matchedOpt.ID
		decBy := p.DecidedBy
		ap.DecidedBy = &decBy
		ap.DecisionNote = p.Note
		ap.DecidedAt = &now

		res.Approval = ap
		res.MachineID = machineID
		res.Decision = protocol.ApprovalDecisionPayload{
			ApprovalID:    ap.ApprovalID,
			DecisionID:    decisionID,
			AttemptID:     ap.AttemptID,
			PayloadSHA256: ap.PayloadSHA256,
			OptionID:      matchedOpt.ID,
			DecidedBy:     p.DecidedBy,
		}
		res.Events = append(res.Events, ev)
		return nil
	})
	return res, err
}

func isRejectOption(opt protocol.ApprovalOption) bool {
	k := strings.ToLower(strings.TrimSpace(opt.Kind))
	id := strings.ToLower(strings.TrimSpace(opt.ID))
	return strings.HasPrefix(k, "reject") || strings.HasPrefix(k, "deny") || strings.HasPrefix(id, "reject") || strings.HasPrefix(id, "deny")
}

// ReconcileRunnerHelloResult holds the hub.welcome reconcile payload and any events emitted.
type ReconcileRunnerHelloResult struct {
	Reconcile protocol.HubWelcomeReconcile
	Events    []protocol.EventRecord
}

// ReconcileRunnerHello performs liveness reconciliation when orbit-runner connects or reconnects:
//  1. Updates machine liveness & resources.
//  2. Absorbs handledApprovalDecisions (preserving allow vs reject).
//  3. Distinguishes same-instance transient reconnect (renew lease, redeliver pending_delivery approvals)
//     from changed runnerInstanceId process restart (pending/rejected -> expired; approved_pending_delivery -> uncertain + needs_verification).
func (s *Store) ReconcileRunnerHello(ctx context.Context, hello protocol.RunnerHelloFrame, leaseTTLMs int64) (ReconcileRunnerHelloResult, error) {
	var res ReconcileRunnerHelloResult
	res.Reconcile = protocol.HubWelcomeReconcile{
		AckedOutboxByAttempt: map[string]int64{},
		AbortAttemptIDs:      []string{},
		RedeliverApprovals:   []protocol.ApprovalDecisionPayload{},
	}
	now := time.Now().UnixMilli()
	if leaseTTLMs <= 0 {
		leaseTTLMs = 45000
	}

	resources := hello.Resources
	if resources == nil {
		resources = map[string]protocol.ResourceState{}
	}
	resJSON, err := json.Marshal(resources)
	if err != nil {
		return res, err
	}

	err = s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		// 1. Update machine status -> online
		var exists string
		if err := tx.QueryRowContext(ctx, `SELECT machine_id FROM machines WHERE machine_id = ?`, hello.MachineID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: machine %s", ErrNotFound, hello.MachineID)
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE machines
			SET status = 'online', runner_version = ?, resources_json = ?, last_seen_at = ?, updated_at = ?
			WHERE machine_id = ?
		`, hello.RunnerVersion, string(resJSON), now, now, hello.MachineID); err != nil {
			return err
		}

		// 2. Absorb handledApprovalDecisions BEFORE evaluating pending_delivery state
		for _, hd := range hello.HandledApprovalDecisions {
			if hd.ApprovalID == "" || hd.DecisionID == "" {
				continue
			}
			if err := consumeApprovalAckByDecisionTx(ctx, tx, hd.ApprovalID, hd.DecisionID, hd.Outcome, now, &res.Events); err != nil {
				return err
			}
		}

		liveAttemptMap := map[string]protocol.LiveAttemptInfo{}
		for _, la := range hello.LiveAttempts {
			if la.AttemptID != "" {
				liveAttemptMap[la.AttemptID] = la
			}
		}
		liveApprovalSet := map[string]bool{}
		for _, id := range hello.LivePendingApprovalIDs {
			if id != "" {
				liveApprovalSet[id] = true
			}
		}

		// 3. Reconcile all active attempts belonging to this machine
		activeClause := protocol.SQLInClause(protocol.ActiveAttemptStates)
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
			SELECT attempt_id FROM attempts
			WHERE machine_id = ? AND state IN (%s)
		`, activeClause), hello.MachineID)
		if err != nil {
			return err
		}
		var activeIDs []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			activeIDs = append(activeIDs, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		knownActive := map[string]bool{}
		for _, aid := range activeIDs {
			att, err := loadAttemptRecordTx(ctx, tx, aid)
			if err != nil {
				return err
			}

			la, runnerHasLive := liveAttemptMap[aid]
			sameInstance := (att.RunnerInstanceID == "" && runnerHasLive && la.LeaseToken == att.LeaseToken) ||
				(att.RunnerInstanceID != "" && att.RunnerInstanceID == hello.RunnerInstanceID)

			if att.State == protocol.AttemptLeased && att.RunnerInstanceID == "" && !runnerHasLive {
				// Not dispatched yet — Hub will dispatch it after welcome; keep leased and bind nothing yet
				knownActive[aid] = true
				continue
			}

			if sameInstance && runnerHasLive && la.LeaseToken == att.LeaseToken {
				// Transient disconnect & reconnect of the SAME runner process!
				knownActive[aid] = true
				if _, err := tx.ExecContext(ctx, `
					UPDATE attempts
					SET runner_instance_id = ?, lease_expires_at = ?
					WHERE attempt_id = ?
				`, hello.RunnerInstanceID, now+leaseTTLMs, aid); err != nil {
					return err
				}
				res.Reconcile.AckedOutboxByAttempt[aid] = att.LastAckedOutbox

				// Redeliver any *_pending_delivery approvals whose ACP handle is still live in memory
				apRows, err := tx.QueryContext(ctx, approvalSelectSQL+`
					WHERE attempt_id = ? AND state IN ('approved_pending_delivery', 'rejected_pending_delivery')
					ORDER BY created_at ASC
				`, aid)
				if err != nil {
					return err
				}
				for apRows.Next() {
					ap, err := scanApprovalView(apRows)
					if err != nil {
						apRows.Close()
						return err
					}
					if liveApprovalSet[ap.ApprovalID] && ap.DecisionID != "" && ap.SelectedOptionID != "" {
						decBy := protocol.ApprovalDecidedByHuman
						if ap.DecidedBy != nil {
							decBy = *ap.DecidedBy
						}
						res.Reconcile.RedeliverApprovals = append(res.Reconcile.RedeliverApprovals, protocol.ApprovalDecisionPayload{
							ApprovalID:    ap.ApprovalID,
							DecisionID:    ap.DecisionID,
							AttemptID:     ap.AttemptID,
							PayloadSHA256: ap.PayloadSHA256,
							OptionID:      ap.SelectedOptionID,
							DecidedBy:     decBy,
						})
					}
				}
				apRows.Close()
				continue
			}

			// Different runnerInstanceId (Runner process restarted!) OR live attempt vanished
			hasUncertain, err := expireOrMarkUncertainApprovalsTx(ctx, tx, aid, now, &res.Events)
			if err != nil {
				return err
			}

			if hasUncertain {
				reason := "批准期间 Runner 重启，工具可能已执行，请核对现场后再决定是否重跑"
				if _, err := tx.ExecContext(ctx, `
					UPDATE attempts
					SET state = 'needs_verification', error_code = 'APPROVAL_CRASH_WINDOW', error_detail = ?, finished_at = ?
					WHERE attempt_id = ?
				`, reason, now, aid); err != nil {
					return err
				}
				if att.WBSessionID != "" {
					_, _ = tx.ExecContext(ctx, `UPDATE agent_sessions SET locked = 1, runtime_state = 'dead' WHERE wb_session_id = ?`, att.WBSessionID)
				}
				if _, err := tx.ExecContext(ctx, `
					UPDATE tasks
					SET state = 'in_review', active_attempt_id = NULL, waiting_reason = ?, version = version + 1, updated_at = ?
					WHERE task_id = ? AND active_attempt_id = ?
				`, reason, now, att.TaskID, aid); err != nil {
					return err
				}
				if err := upsertBlockedInboxTx(ctx, tx, att.ProjectID, att.TaskID, aid, "待核对现场 · 批准窗口崩溃", reason, 1, now); err != nil {
					return err
				}
				ev, err := insertEventTx(ctx, tx, eventInsert{
					StreamID:   "task:" + att.TaskID,
					ProjectID:  att.ProjectID,
					TaskID:     att.TaskID,
					AttemptID:  aid,
					ActorKind:  protocol.ActorSystem,
					ActorID:    "runner-reconcile",
					Kind:       protocol.EventAttemptFinished,
					Visibility: protocol.VisibilityInbox,
					Payload: map[string]any{
						"state":     protocol.AttemptNeedsVerification,
						"errorCode": "APPROVAL_CRASH_WINDOW",
						"reason":    reason,
					},
					CreatedAt: now,
				})
				if err != nil {
					return err
				}
				res.Events = append(res.Events, ev)
			} else {
				reason := "Runner 进程重启，挂起尝试已确定过期，可安全重跑"
				if _, err := tx.ExecContext(ctx, `
					UPDATE attempts
					SET state = 'expired', error_code = 'RUNNER_RESTARTED', error_detail = ?, finished_at = ?
					WHERE attempt_id = ?
				`, reason, now, aid); err != nil {
					return err
				}
				if err := releaseQueuedAnnotationsTx(ctx, tx, aid); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `
					UPDATE tasks
					SET state = 'failed', active_attempt_id = NULL, waiting_reason = ?, version = version + 1, updated_at = ?
					WHERE task_id = ? AND active_attempt_id = ?
				`, reason, now, att.TaskID, aid); err != nil {
					return err
				}
				if err := upsertBlockedInboxTx(ctx, tx, att.ProjectID, att.TaskID, aid, "Runner 重启 · 可安全重跑", reason, 2, now); err != nil {
					return err
				}
				ev, err := insertEventTx(ctx, tx, eventInsert{
					StreamID:   "task:" + att.TaskID,
					ProjectID:  att.ProjectID,
					TaskID:     att.TaskID,
					AttemptID:  aid,
					ActorKind:  protocol.ActorSystem,
					ActorID:    "runner-reconcile",
					Kind:       protocol.EventAttemptFinished,
					Visibility: protocol.VisibilityInbox,
					Payload: map[string]any{
						"state":     protocol.AttemptExpired,
						"errorCode": "RUNNER_RESTARTED",
						"reason":    reason,
					},
					CreatedAt: now,
				})
				if err != nil {
					return err
				}
				res.Events = append(res.Events, ev)
			}
		}

		// 4. For any attempt claimed live by runner that Hub no longer considers active, tell runner to abort
		//    and also report its ackedOutbox watermark so outbox can drain cleanly into late_history.
		for aid, la := range liveAttemptMap {
			if knownActive[aid] {
				continue
			}
			res.Reconcile.AbortAttemptIDs = append(res.Reconcile.AbortAttemptIDs, aid)
			var lastAck int64
			var dbToken string
			if err := tx.QueryRowContext(ctx, `SELECT last_acked_outbox, lease_token FROM attempts WHERE attempt_id = ?`, aid).Scan(&lastAck, &dbToken); err == nil {
				if dbToken == la.LeaseToken {
					res.Reconcile.AckedOutboxByAttempt[aid] = lastAck
				}
			}
		}

		return nil
	})
	return res, err
}

// createApprovalTx inserts a pending approval from Runner outbox and updates attempt/task/inbox atomically.
func createApprovalTx(ctx context.Context, tx *sql.Tx, att AttemptRecord, runnerInstanceID string, payload map[string]any, now int64, events *[]protocol.EventRecord) error {
	approvalID := stringField(payload, "approvalId")
	if approvalID == "" {
		approvalID = newID("apr")
	}

	// Idempotent check
	var existing string
	if err := tx.QueryRowContext(ctx, `SELECT approval_id FROM approvals WHERE approval_id = ?`, approvalID).Scan(&existing); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	kind := protocol.ApprovalKind(stringField(payload, "kind"))
	if kind == "" {
		kind = protocol.ApprovalACPToolPermission
	}
	actionName := stringField(payload, "actionName")
	if actionName == "" {
		actionName = "tool_permission"
	}
	targetEnv := stringField(payload, "targetEnv")
	if targetEnv == "" {
		targetEnv = att.MachineID + ":worktree"
	}
	inputSHA := stringField(payload, "inputSha")
	if inputSHA == "" {
		inputSHA = att.BaseSHA
	}
	payloadSHA := stringField(payload, "payloadSha256")
	acpRPCID := stringField(payload, "acpRpcId")
	if ri := stringField(payload, "runnerInstanceId"); ri != "" {
		runnerInstanceID = ri
	}
	reqBy := protocol.ApprovalRequestedBy(stringField(payload, "requestedBy"))
	if reqBy == "" {
		reqBy = protocol.ApprovalRequestedByAgent
	}

	expiresAt := int64Field(payload, "expiresAt")
	if expiresAt <= 0 {
		expiresAt = now + int64((30 * time.Minute).Milliseconds())
	}

	rawToolPayload, ok := payload["payload"]
	if !ok || rawToolPayload == nil {
		rawToolPayload = map[string]any{}
	}
	toolPayloadBytes, err := json.Marshal(rawToolPayload)
	if err != nil {
		return err
	}

	rawOptions, ok := payload["options"]
	if !ok || rawOptions == nil {
		rawOptions = []protocol.ApprovalOption{}
	}
	optionsBytes, err := json.Marshal(rawOptions)
	if err != nil {
		return err
	}

	var rpcVal, runInstVal any
	if acpRPCID != "" {
		rpcVal = acpRPCID
	}
	if runnerInstanceID != "" {
		runInstVal = runnerInstanceID
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO approvals (
			approval_id, project_id, task_id, attempt_id, kind, action_name, target_env,
			input_sha, payload_json, payload_sha256, acp_rpc_id, runner_instance_id,
			options_json, requested_by, state, expires_at, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)
	`, approvalID, att.ProjectID, att.TaskID, att.AttemptID, string(kind), actionName, targetEnv,
		inputSHA, string(toolPayloadBytes), payloadSHA, rpcVal, runInstVal,
		string(optionsBytes), string(reqBy), expiresAt, now); err != nil {
		return fmt.Errorf("insert approval: %w", err)
	}

	// Preserve resume_state (running vs swarm_monitoring) on first pending approval
	resumeState := stringField(payload, "resumeState")
	if resumeState != string(protocol.ResumeStateRunning) && resumeState != string(protocol.ResumeStateSwarmMonitoring) {
		if att.State == protocol.AttemptSwarmMonitoring {
			resumeState = string(protocol.ResumeStateSwarmMonitoring)
		} else {
			resumeState = string(protocol.ResumeStateRunning)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE attempts
		SET state = 'waiting_approval',
		    resume_state = COALESCE(resume_state, ?),
		    runner_instance_id = COALESCE(NULLIF(runner_instance_id, ''), ?)
		WHERE attempt_id = ?
	`, resumeState, runnerInstanceID, att.AttemptID); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET state = 'waiting_approval', waiting_reason = ?, version = version + 1, updated_at = ?
		WHERE task_id = ? AND active_attempt_id = ?
	`, "等待审批: "+actionName, now, att.TaskID, att.AttemptID); err != nil {
		return err
	}

	// Upsert priority-1 approval_pending inbox item in the SAME transaction
	inboxID := "inb:apr:" + approvalID
	title := fmt.Sprintf("等待确认 · %s (%s)", actionName, targetEnv)
	summary := stringField(payload, "summary")
	if summary == "" {
		summary = fmt.Sprintf("Attempt %s 请求执行 %s", att.AttemptID, actionName)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO inbox_items (
			inbox_id, project_id, task_id, category, ref_id, title, summary,
			priority, state, created_at, updated_at
		) VALUES (?, ?, ?, 'approval_pending', ?, ?, ?, 1, 'open', ?, ?)
		ON CONFLICT(inbox_id) DO UPDATE SET
			title = excluded.title,
			summary = excluded.summary,
			state = 'open',
			updated_at = excluded.updated_at,
			resolved_at = NULL
	`, inboxID, att.ProjectID, att.TaskID, approvalID, title, summary, now, now); err != nil {
		return err
	}

	ev, err := insertEventTx(ctx, tx, eventInsert{
		StreamID:   "task:" + att.TaskID,
		ProjectID:  att.ProjectID,
		TaskID:     att.TaskID,
		AttemptID:  att.AttemptID,
		ActorKind:  protocol.ActorRunner,
		ActorID:    att.MachineID,
		Kind:       protocol.EventApprovalRequested,
		Visibility: protocol.VisibilityInbox,
		Payload: map[string]any{
			"approvalId":    approvalID,
			"actionName":    actionName,
			"targetEnv":     targetEnv,
			"payloadSha256": payloadSHA,
			"resumeState":   resumeState,
		},
		CreatedAt: now,
	})
	if err != nil {
		return err
	}
	*events = append(*events, ev)
	return nil
}

// consumeApprovalAckByDecisionTx records runner ACK for a decided approval while strictly
// preserving reject vs allow semantics and restoring attempt.resume_state once all approvals settle.
func consumeApprovalAckByDecisionTx(
	ctx context.Context,
	tx *sql.Tx,
	approvalID, decisionID string,
	reportedOutcome protocol.ApprovalState,
	now int64,
	events *[]protocol.EventRecord,
) error {
	row := tx.QueryRowContext(ctx, approvalSelectSQL+` WHERE approval_id = ?`, approvalID)
	ap, err := scanApprovalView(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if decisionID != "" && ap.DecisionID != "" && ap.DecisionID != decisionID {
		return nil
	}

	// Already in a terminal consumed state -> idempotent
	if ap.State == protocol.ApprovalConsumedAllowed || ap.State == protocol.ApprovalConsumedRejected {
		return nil
	}

	// Strictly derive final state from Hub's persisted decision state (NEVER flip rejection to allowed!)
	var finalState protocol.ApprovalState
	switch ap.State {
	case protocol.ApprovalApprovedPendingDelivery:
		if reportedOutcome == protocol.ApprovalExpired {
			finalState = protocol.ApprovalExpired
		} else {
			finalState = protocol.ApprovalConsumedAllowed
		}
	case protocol.ApprovalRejectedPendingDelivery:
		if reportedOutcome == protocol.ApprovalExpired {
			finalState = protocol.ApprovalExpired
		} else {
			finalState = protocol.ApprovalConsumedRejected
		}
	default:
		return nil
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE approvals SET state = ?, consumed_at = ?
		WHERE approval_id = ?
	`, string(finalState), now, ap.ApprovalID); err != nil {
		return err
	}

	ev, err := insertEventTx(ctx, tx, eventInsert{
		StreamID:   "task:" + ap.TaskID,
		ProjectID:  ap.ProjectID,
		TaskID:     ap.TaskID,
		AttemptID:  ap.AttemptID,
		ActorKind:  protocol.ActorRunner,
		ActorID:    "runner",
		Kind:       protocol.EventApprovalConsumed,
		Visibility: protocol.VisibilityAudit,
		Payload: map[string]any{
			"approvalId": ap.ApprovalID,
			"decisionId": ap.DecisionID,
			"state":      finalState,
		},
		CreatedAt: now,
	})
	if err != nil {
		return err
	}
	*events = append(*events, ev)

	// Check if any unresolved approvals remain for this attempt; if none, restore attempt resume_state!
	var remaining int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM approvals
		WHERE attempt_id = ? AND state IN ('pending', 'approved_pending_delivery', 'rejected_pending_delivery')
	`, ap.AttemptID).Scan(&remaining); err != nil {
		return err
	}
	if remaining == 0 {
		var curState string
		var resumeState sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT state, resume_state FROM attempts WHERE attempt_id = ?`, ap.AttemptID).Scan(&curState, &resumeState); err == nil {
			if curState == string(protocol.AttemptWaitingApproval) {
				target := string(protocol.AttemptRunning)
				if resumeState.Valid && resumeState.String == string(protocol.ResumeStateSwarmMonitoring) {
					target = string(protocol.AttemptSwarmMonitoring)
				}
				if _, err := tx.ExecContext(ctx, `
					UPDATE attempts SET state = ?, resume_state = NULL WHERE attempt_id = ?
				`, target, ap.AttemptID); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `
					UPDATE tasks SET state = 'running', waiting_reason = NULL, version = version + 1, updated_at = ?
					WHERE task_id = ? AND active_attempt_id = ?
				`, now, ap.TaskID, ap.AttemptID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func expirePendingApprovalsForAttemptTx(ctx context.Context, tx *sql.Tx, attemptID string, now int64) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT approval_id FROM approvals
		WHERE attempt_id = ? AND state IN ('pending', 'approved_pending_delivery', 'rejected_pending_delivery')
	`, attemptID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE approvals SET state = 'expired' WHERE approval_id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE inbox_items SET state = 'resolved', resolved_at = ?, updated_at = ? WHERE inbox_id = ? AND state = 'open'`, now, now, "inb:apr:"+id); err != nil {
			return err
		}
	}
	return nil
}

// expireOrMarkUncertainApprovalsTx marks pending/rejected_pending_delivery approvals as 'expired'
// and approved_pending_delivery approvals as 'uncertain', resolving old inbox cards and returning
// true if ANY approval entered 'uncertain'.
func expireOrMarkUncertainApprovalsTx(ctx context.Context, tx *sql.Tx, attemptID string, now int64, events *[]protocol.EventRecord) (bool, error) {
	rows, err := tx.QueryContext(ctx, approvalSelectSQL+`
		WHERE attempt_id = ? AND state IN ('pending', 'approved_pending_delivery', 'rejected_pending_delivery')
	`, attemptID)
	if err != nil {
		return false, err
	}
	var list []protocol.ApprovalView
	for rows.Next() {
		ap, err := scanApprovalView(rows)
		if err != nil {
			rows.Close()
			return false, err
		}
		list = append(list, ap)
	}
	rows.Close()

	hasUncertain := false
	for _, ap := range list {
		nextState := protocol.ApprovalExpired
		if ap.State == protocol.ApprovalApprovedPendingDelivery {
			nextState = protocol.ApprovalUncertain
			hasUncertain = true
		}
		if _, err := tx.ExecContext(ctx, `UPDATE approvals SET state = ? WHERE approval_id = ?`, string(nextState), ap.ApprovalID); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE inbox_items SET state = 'resolved', resolved_at = ?, updated_at = ?
			WHERE inbox_id = ? AND state = 'open'
		`, now, now, "inb:apr:"+ap.ApprovalID); err != nil {
			return false, err
		}
		ev, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + ap.TaskID,
			ProjectID:  ap.ProjectID,
			TaskID:     ap.TaskID,
			AttemptID:  ap.AttemptID,
			ActorKind:  protocol.ActorSystem,
			ActorID:    "approval-reconcile",
			Kind:       protocol.EventApprovalExpired,
			Visibility: protocol.VisibilityAudit,
			Payload: map[string]any{
				"approvalId": ap.ApprovalID,
				"prevState":  ap.State,
				"newState":   nextState,
			},
			CreatedAt: now,
		})
		if err != nil {
			return false, err
		}
		*events = append(*events, ev)
	}
	return hasUncertain, nil
}
