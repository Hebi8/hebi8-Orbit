package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

const (
	maxDelegationDepth         = 1
	maxActiveChildrenPerParent = 12
	childPreviewMaxRunes       = 240
	transcriptReplyMaxRunes    = 4000
)

// ChildResultContext holds all structured data extracted for a finished child attempt
// so Hub can construct an automatic wake-up turn for the parent session.
type ChildResultContext struct {
	ChildTaskID    string
	ParentTaskID   string
	ChildTitle     string
	ChildRole      string
	ChildProjectID string
	AttemptID      string
	AttemptState   protocol.AttemptState
	Reply          string
	ErrorDetail    string
	DeliveryID     string
	DeliverySummary string
	FilesChanged   int
	Insertions     int
	Deletions      int
}

func truncateRunes(s string, max int) string {
	s = strings.TrimSpace(s)
	if s == "" || max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

// ListChildTasks returns delegated child tasks of parentTaskID enriched with latest attempt,
// model, current step, last assistant message preview, open approvals, and delivery stats.
func (s *Store) ListChildTasks(ctx context.Context, parentTaskID string, includeArchived bool) ([]protocol.ChildTaskView, error) {
	var out []protocol.ChildTaskView
	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		var exists string
		if err := tx.QueryRowContext(ctx, `SELECT task_id FROM tasks WHERE task_id = ?`, parentTaskID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: task %s", ErrNotFound, parentTaskID)
			}
			return err
		}
		var err error
		out, err = listChildTasksTx(ctx, tx, parentTaskID, includeArchived)
		return err
	})
	return out, err
}

func listChildTasksTx(ctx context.Context, tx *sql.Tx, parentTaskID string, includeArchived bool) ([]protocol.ChildTaskView, error) {
	archivedClause := "AND t.archived_at IS NULL"
	if includeArchived {
		archivedClause = ""
	}
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT t.task_id, t.project_id, COALESCE(p.display_name, ''), t.parent_task_id,
		       t.delegated_by_attempt_id, t.delegated_role,
		       t.title, t.goal, t.state, t.preferred_adapter,
		       t.desired_config_json, t.active_attempt_id, t.latest_delivery_id,
		       (SELECT COUNT(*) FROM deliveries dl WHERE dl.task_id = t.task_id) AS dlv_count,
		       (SELECT COUNT(*) FROM attempts att WHERE att.task_id = t.task_id) AS att_count,
		       (SELECT COUNT(*) FROM approvals ap WHERE ap.task_id = t.task_id AND ap.state = 'pending') AS open_approvals,
		       t.created_at, t.updated_at, t.archived_at
		FROM tasks t
		LEFT JOIN projects p ON p.project_id = t.project_id
		WHERE t.parent_task_id = ? %s
		ORDER BY t.created_at ASC
	`, archivedClause), parentTaskID)
	if err != nil {
		return nil, err
	}

	children := []protocol.ChildTaskView{}
	taskCfgJSON := map[string]string{}
	for rows.Next() {
		var c protocol.ChildTaskView
		var projName, delBy, delRole, adp, stateStr, cfgJSON, activeAtt, latestDlv sql.NullString
		var archivedAt sql.NullInt64
		if err := rows.Scan(
			&c.TaskID, &c.ProjectID, &projName, &c.ParentTaskID,
			&delBy, &delRole,
			&c.Title, &c.Goal, &stateStr, &adp,
			&cfgJSON, &activeAtt, &latestDlv,
			&c.DeliveryCount, &c.AttemptCount, &c.OpenApprovalCount,
			&c.CreatedAt, &c.UpdatedAt, &archivedAt,
		); err != nil {
			rows.Close()
			return nil, err
		}
		c.State = protocol.TaskState(stateStr.String)
		if projName.Valid {
			c.ProjectName = projName.String
		}
		if delBy.Valid {
			c.DelegatedByAttemptID = delBy.String
		}
		if delRole.Valid {
			c.DelegatedRole = delRole.String
		}
		if adp.Valid {
			c.PreferredAdapter = protocol.Adapter(adp.String)
		}
		if activeAtt.Valid {
			c.ActiveAttemptID = activeAtt.String
		}
		if latestDlv.Valid {
			c.LatestDeliveryID = latestDlv.String
		}
		if archivedAt.Valid {
			v := archivedAt.Int64
			c.ArchivedAt = &v
		}
		if cfgJSON.Valid {
			taskCfgJSON[c.TaskID] = cfgJSON.String
		}
		children = append(children, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	if len(children) == 0 {
		return children, nil
	}

	for i := range children {
		c := &children[i]

		// Latest attempt metadata (machineId, model, errorDetail, folderChanges)
		var latestAttID, machID, attCfgJSON, errDetail, folderChangesJSON sql.NullString
		attErr := tx.QueryRowContext(ctx, `
			SELECT attempt_id, machine_id, desired_config_json, error_detail, folder_changes_json
			FROM attempts
			WHERE task_id = ?
			ORDER BY attempt_no DESC
			LIMIT 1
		`, c.TaskID).Scan(&latestAttID, &machID, &attCfgJSON, &errDetail, &folderChangesJSON)
		if attErr == nil {
			if machID.Valid {
				c.MachineID = machID.String
			}
			if errDetail.Valid {
				c.ErrorDetail = errDetail.String
			}
			if attCfgJSON.Valid && attCfgJSON.String != "" {
				c.Model = extractModelFromConfigJSON(attCfgJSON.String)
			}
		}
		if c.Model == "" {
			if rawCfg, ok := taskCfgJSON[c.TaskID]; ok && rawCfg != "" {
				c.Model = extractModelFromConfigJSON(rawCfg)
			}
		}

		// Latest formal delivery stats (or fallback to plain-folder changes on latest attempt)
		if c.LatestDeliveryID != "" {
			var manifestJSON string
			if err := tx.QueryRowContext(ctx, `SELECT manifest_json FROM deliveries WHERE delivery_id = ?`, c.LatestDeliveryID).Scan(&manifestJSON); err == nil {
				var m protocol.DeliveryManifest
				if json.Unmarshal([]byte(manifestJSON), &m) == nil {
					c.FilesChanged = m.FilesChanged
					c.Insertions = m.Insertions
					c.Deletions = m.Deletions
				}
			}
		} else if folderChangesJSON.Valid && strings.TrimSpace(folderChangesJSON.String) != "" {
			var fc protocol.FolderChangesSummary
			if json.Unmarshal([]byte(folderChangesJSON.String), &fc) == nil && len(fc.Files) > 0 {
				c.FilesChanged = len(fc.Files)
				c.Insertions = fc.TotalAdds
				c.Deletions = fc.TotalDels
			}
		}

		// Current step from most recent tool_call or attempt.state_changed event
		c.CurrentStep = loadChildCurrentStepTx(ctx, tx, c.TaskID)

		// Last assistant message preview from latest attempt
		if latestAttID.Valid && latestAttID.String != "" {
			reply := loadAttemptAssistantReplyTx(ctx, tx, latestAttID.String, childPreviewMaxRunes)
			c.LastMessagePreview = reply
		}
	}

	return children, nil
}

func extractModelFromConfigJSON(raw string) string {
	var cfg map[string]string
	if json.Unmarshal([]byte(raw), &cfg) != nil {
		return ""
	}
	return strings.TrimSpace(cfg["model"])
}

func loadChildCurrentStepTx(ctx context.Context, tx *sql.Tx, taskID string) string {
	var kind, payloadJSON string
	err := tx.QueryRowContext(ctx, `
		SELECT kind, payload_json
		FROM events
		WHERE task_id = ? AND kind IN (?, ?)
		ORDER BY seq DESC
		LIMIT 1
	`, taskID, string(protocol.EventACPToolCall), string(protocol.EventAttemptStateChanged)).Scan(&kind, &payloadJSON)
	if err != nil {
		return ""
	}
	var payload map[string]any
	if json.Unmarshal([]byte(payloadJSON), &payload) != nil {
		return ""
	}
	if protocol.EventKind(kind) == protocol.EventACPToolCall {
		if title := stringField(payload, "title"); title != "" {
			return truncateRunes(title, 80)
		}
		if name := stringField(payload, "name"); name != "" {
			return truncateRunes(name, 80)
		}
		return "running tool"
	}
	if st := stringField(payload, "state"); st != "" {
		return st
	}
	return ""
}

func loadAttemptAssistantReplyTx(ctx context.Context, tx *sql.Tx, attemptID string, maxRunes int) string {
	rows, err := tx.QueryContext(ctx, `
		SELECT payload_json
		FROM events
		WHERE attempt_id = ? AND kind = ?
		ORDER BY seq ASC
	`, attemptID, string(protocol.EventACPMessageBatch))
	if err != nil {
		return ""
	}
	defer rows.Close()

	var sb strings.Builder
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			break
		}
		var payload map[string]any
		if json.Unmarshal([]byte(raw), &payload) != nil {
			continue
		}
		if boolField(payload, "late_arrival") {
			continue
		}
		if text, ok := payload["text"].(string); ok && text != "" {
			sb.WriteString(text)
		}
	}
	return truncateRunes(sb.String(), maxRunes)
}

func loadParentTaskRefTx(ctx context.Context, tx *sql.Tx, parentTaskID string) (*protocol.ParentTaskRef, error) {
	var ref protocol.ParentTaskRef
	var projName sql.NullString
	var stateStr string
	err := tx.QueryRowContext(ctx, `
		SELECT t.task_id, t.project_id, COALESCE(p.display_name, ''), t.title, t.state
		FROM tasks t
		LEFT JOIN projects p ON p.project_id = t.project_id
		WHERE t.task_id = ?
	`, parentTaskID).Scan(&ref.TaskID, &ref.ProjectID, &projName, &ref.Title, &stateStr)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if projName.Valid {
		ref.ProjectName = projName.String
	}
	ref.State = protocol.TaskState(stateStr)
	if ref.ProjectID == protocol.PAProjectID {
		if isMain, mErr := isHebi8MainTaskTx(ctx, tx, ref.TaskID); mErr == nil && isMain {
			ref.IsHebi8Main = true
		}
	}
	return &ref, nil
}

// GetTaskTranscript returns a compact per-attempt transcript of a task suitable for parent agent reading.
func (s *Store) GetTaskTranscript(ctx context.Context, taskID string, limit int) (protocol.TaskTranscriptResponse, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	resp := protocol.TaskTranscriptResponse{
		Turns: []protocol.TaskTranscriptTurn{},
	}

	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		var parentID sql.NullString
		var stateStr, goal string
		err := tx.QueryRowContext(ctx, `
			SELECT task_id, parent_task_id, project_id, title, goal, state,
			       (SELECT COUNT(*) FROM approvals ap WHERE ap.task_id = tasks.task_id AND ap.state = 'pending')
			FROM tasks WHERE task_id = ?
		`, taskID).Scan(&resp.TaskID, &parentID, &resp.ProjectID, &resp.Title, &goal, &stateStr, &resp.OpenApprovalCount)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: task %s", ErrNotFound, taskID)
		}
		if err != nil {
			return err
		}
		if parentID.Valid {
			resp.ParentTaskID = parentID.String
		}
		resp.State = protocol.TaskState(stateStr)

		rows, err := tx.QueryContext(ctx, `
			SELECT attempt_id, attempt_no, trigger_kind, trigger_payload_json, state,
			       error_detail, output_delivery_id, folder_changes_json, started_at, finished_at
			FROM attempts
			WHERE task_id = ?
			ORDER BY attempt_no DESC
			LIMIT ?
		`, taskID, limit)
		if err != nil {
			return err
		}

		type attRow struct {
			turn              protocol.TaskTranscriptTurn
			folderChangesJSON string
		}
		var list []attRow
		for rows.Next() {
			var ar attRow
			var trig, tpJSON, stStr string
			var errDetail, outDlv, fcJSON sql.NullString
			var finAt sql.NullInt64
			if err := rows.Scan(
				&ar.turn.AttemptID, &ar.turn.AttemptNo, &trig, &tpJSON, &stStr,
				&errDetail, &outDlv, &fcJSON, &ar.turn.StartedAt, &finAt,
			); err != nil {
				rows.Close()
				return err
			}
			ar.turn.TriggerKind = protocol.TriggerKind(trig)
			ar.turn.State = protocol.AttemptState(stStr)
			ar.turn.Prompt = extractBoundedPromptText(tpJSON)
			if ar.turn.Prompt == "" && ar.turn.AttemptNo == 1 {
				ar.turn.Prompt = truncatePromptText(goal)
			}
			if errDetail.Valid {
				ar.turn.ErrorDetail = errDetail.String
			}
			if outDlv.Valid {
				ar.turn.DeliveryID = outDlv.String
			}
			if fcJSON.Valid {
				ar.folderChangesJSON = fcJSON.String
			}
			if finAt.Valid {
				v := finAt.Int64
				ar.turn.FinishedAt = &v
			}
			list = append(list, ar)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		// Reverse to ascending chronological attempt order
		for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
			list[i], list[j] = list[j], list[i]
		}

		for _, ar := range list {
			turn := ar.turn
			turn.Reply = loadAttemptAssistantReplyTx(ctx, tx, turn.AttemptID, transcriptReplyMaxRunes)
			turn.ToolCount = countAttemptDistinctToolsTx(ctx, tx, turn.AttemptID)
			if turn.DeliveryID != "" {
				var manifestJSON string
				if err := tx.QueryRowContext(ctx, `SELECT manifest_json FROM deliveries WHERE delivery_id = ?`, turn.DeliveryID).Scan(&manifestJSON); err == nil {
					var m protocol.DeliveryManifest
					if json.Unmarshal([]byte(manifestJSON), &m) == nil {
						turn.FilesChanged = m.FilesChanged
					}
				}
			} else if strings.TrimSpace(ar.folderChangesJSON) != "" {
				var fc protocol.FolderChangesSummary
				if json.Unmarshal([]byte(ar.folderChangesJSON), &fc) == nil {
					turn.FilesChanged = len(fc.Files)
				}
			}
			resp.Turns = append(resp.Turns, turn)
		}
		return nil
	})
	return resp, err
}

func countAttemptDistinctToolsTx(ctx context.Context, tx *sql.Tx, attemptID string) int {
	rows, err := tx.QueryContext(ctx, `
		SELECT payload_json
		FROM events
		WHERE attempt_id = ? AND kind = ?
	`, attemptID, string(protocol.EventACPToolCall))
	if err != nil {
		return 0
	}
	defer rows.Close()
	seen := map[string]struct{}{}
	anon := 0
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			break
		}
		var payload map[string]any
		if json.Unmarshal([]byte(raw), &payload) != nil {
			continue
		}
		if boolField(payload, "late_arrival") {
			continue
		}
		if id := stringField(payload, "toolCallId"); id != "" {
			seen[id] = struct{}{}
		} else {
			anon++
		}
	}
	return len(seen) + anon
}

// GetAttemptChildResultContext gathers the compact result of a child task's finished attempt
// for formatting the automatic parent wake-up turn.
func (s *Store) GetAttemptChildResultContext(ctx context.Context, childTaskID, attemptID string) (ChildResultContext, error) {
	var out ChildResultContext
	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		var parentID, role sql.NullString
		if err := tx.QueryRowContext(ctx, `
			SELECT task_id, parent_task_id, project_id, title, delegated_role
			FROM tasks WHERE task_id = ?
		`, childTaskID).Scan(&out.ChildTaskID, &parentID, &out.ChildProjectID, &out.ChildTitle, &role); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: task %s", ErrNotFound, childTaskID)
			}
			return err
		}
		if parentID.Valid {
			out.ParentTaskID = parentID.String
		}
		if role.Valid {
			out.ChildRole = role.String
		}

		var stateStr string
		var errDetail, outDlv, fcJSON sql.NullString
		if err := tx.QueryRowContext(ctx, `
			SELECT attempt_id, state, error_detail, output_delivery_id, folder_changes_json
			FROM attempts WHERE attempt_id = ? AND task_id = ?
		`, attemptID, childTaskID).Scan(&out.AttemptID, &stateStr, &errDetail, &outDlv, &fcJSON); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: attempt %s", ErrNotFound, attemptID)
			}
			return err
		}
		out.AttemptState = protocol.AttemptState(stateStr)
		if errDetail.Valid {
			out.ErrorDetail = errDetail.String
		}
		if outDlv.Valid {
			out.DeliveryID = outDlv.String
		}

		out.Reply = loadAttemptAssistantReplyTx(ctx, tx, attemptID, transcriptReplyMaxRunes)
		if out.DeliveryID != "" {
			var dlvSummary, manifestJSON string
			if err := tx.QueryRowContext(ctx, `SELECT summary, manifest_json FROM deliveries WHERE delivery_id = ?`, out.DeliveryID).Scan(&dlvSummary, &manifestJSON); err == nil {
				out.DeliverySummary = strings.TrimSpace(dlvSummary)
				var m protocol.DeliveryManifest
				if json.Unmarshal([]byte(manifestJSON), &m) == nil {
					out.FilesChanged = m.FilesChanged
					out.Insertions = m.Insertions
					out.Deletions = m.Deletions
				}
			}
		} else if fcJSON.Valid && strings.TrimSpace(fcJSON.String) != "" {
			var fc protocol.FolderChangesSummary
			if json.Unmarshal([]byte(fcJSON.String), &fc) == nil && len(fc.Files) > 0 {
				out.FilesChanged = len(fc.Files)
				out.Insertions = fc.TotalAdds
				out.Deletions = fc.TotalDels
			}
		}
		return nil
	})
	return out, err
}

// ClaimChildParentNotification atomically claims the right to wake childTaskID's parent
// for attemptID, guaranteeing at-most-once notification per child attempt.
func (s *Store) ClaimChildParentNotification(ctx context.Context, childTaskID, attemptID string) (bool, string, error) {
	if strings.TrimSpace(childTaskID) == "" || strings.TrimSpace(attemptID) == "" {
		return false, "", nil
	}
	var claimed bool
	var parentTaskID string
	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE tasks
			SET parent_notified_attempt_id = ?
			WHERE task_id = ?
			  AND parent_task_id IS NOT NULL AND parent_task_id != ''
			  AND notify_parent_on_finish = 1
			  AND COALESCE(parent_notified_attempt_id, '') != ?
		`, attemptID, childTaskID, attemptID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		var pid sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT parent_task_id FROM tasks WHERE task_id = ?`, childTaskID).Scan(&pid); err != nil {
			return err
		}
		if pid.Valid && pid.String != "" {
			claimed = true
			parentTaskID = pid.String
		}
		return nil
	})
	return claimed, parentTaskID, err
}

// IsDirectChildTask reports whether parentTaskID is a top-level session (empty parent_task_id)
// and childTaskID is its direct child (target.parent_task_id == parentTaskID).
// Enforces the strict depth-1 delegation boundary even on legacy multi-level databases.
func (s *Store) IsDirectChildTask(ctx context.Context, parentTaskID, childTaskID string) (bool, error) {
	var matched bool
	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		matched, err = isDirectChildTaskTx(ctx, tx, parentTaskID, childTaskID)
		return err
	})
	return matched, err
}

func isDirectChildTaskTx(ctx context.Context, tx *sql.Tx, parentTaskID, childTaskID string) (bool, error) {
	parentTaskID = strings.TrimSpace(parentTaskID)
	childTaskID = strings.TrimSpace(childTaskID)
	if parentTaskID == "" || childTaskID == "" || parentTaskID == childTaskID {
		return false, nil
	}
	var callerParent, targetParent sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT parent_task_id FROM tasks WHERE task_id = ?`, parentTaskID).Scan(&callerParent); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	// Only top-level parents (depth 0) can act as parent over direct children; child sessions cannot operate on legacy grandchildren.
	if callerParent.Valid && strings.TrimSpace(callerParent.String) != "" {
		return false, nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT parent_task_id FROM tasks WHERE task_id = ?`, childTaskID).Scan(&targetParent); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return targetParent.Valid && strings.TrimSpace(targetParent.String) == parentTaskID, nil
}

// RecordDelegationSentParams describes a lightweight outbound delegation communication record
// written onto the sender task's timeline when POST /api/runner/delegate/send succeeds.
type RecordDelegationSentParams struct {
	SenderTaskID    string
	SenderAttemptID string
	TargetTaskID    string
	Relation        string // "parent" | "sibling"
	Mode            protocol.FollowupMode
	Summary         string
	ActorID         string
}

// RecordDelegationSent appends a lightweight delegation.sent timeline event onto the sender task.
func (s *Store) RecordDelegationSent(ctx context.Context, p RecordDelegationSentParams) (protocol.EventRecord, error) {
	var ev protocol.EventRecord
	now := time.Now().UnixMilli()
	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		ev, err = insertDelegationSentTx(ctx, tx, p, now)
		return err
	})
	return ev, err
}

func insertDelegationSentTx(ctx context.Context, tx *sql.Tx, p RecordDelegationSentParams, now int64) (protocol.EventRecord, error) {
	var ev protocol.EventRecord
	actorID := strings.TrimSpace(p.ActorID)
	if actorID == "" {
		actorID = "agent"
	}
	var senderProjectID string
	if err := tx.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE task_id = ?`, p.SenderTaskID).Scan(&senderProjectID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ev, fmt.Errorf("%w: sender task %s", ErrNotFound, p.SenderTaskID)
		}
		return ev, err
	}
	var targetProjectID, targetTitle, targetGoal string
	if err := tx.QueryRowContext(ctx, `SELECT project_id, title, goal FROM tasks WHERE task_id = ?`, p.TargetTaskID).Scan(&targetProjectID, &targetTitle, &targetGoal); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return ev, err
		}
	}
	title := strings.TrimSpace(targetTitle)
	if title == "" {
		title = strings.TrimSpace(strings.Split(targetGoal, "\n")[0])
	}
	if title == "" {
		title = p.TargetTaskID
	}
	return insertEventTx(ctx, tx, eventInsert{
		StreamID:   "task:" + p.SenderTaskID,
		ProjectID:  senderProjectID,
		TaskID:     p.SenderTaskID,
		AttemptID:  strings.TrimSpace(p.SenderAttemptID),
		ActorKind:  protocol.ActorAgent,
		ActorID:    actorID,
		Kind:       protocol.EventDelegationSent,
		Visibility: protocol.VisibilityTimeline,
		Payload: map[string]any{
			"targetTaskId":    p.TargetTaskID,
			"targetProjectId": targetProjectID,
			"targetTitle":     title,
			"relation":        strings.TrimSpace(p.Relation),
			"mode":            string(p.Mode),
			"summary":         truncatePromptText(p.Summary),
		},
		CreatedAt: now,
	})
}

// IsDescendantTask reports whether targetTaskID is strictly under ancestorTaskID's delegation subtree.
func (s *Store) IsDescendantTask(ctx context.Context, ancestorTaskID, targetTaskID string) (bool, error) {
	var matched bool
	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		matched, err = isDescendantTaskTx(ctx, tx, ancestorTaskID, targetTaskID)
		return err
	})
	return matched, err
}

func isDescendantTaskTx(ctx context.Context, tx *sql.Tx, ancestorTaskID, targetTaskID string) (bool, error) {
	ancestorTaskID = strings.TrimSpace(ancestorTaskID)
	targetTaskID = strings.TrimSpace(targetTaskID)
	if ancestorTaskID == "" || targetTaskID == "" || ancestorTaskID == targetTaskID {
		return false, nil
	}
	var matched int
	err := tx.QueryRowContext(ctx, `
		WITH RECURSIVE chain(tid, pid, depth) AS (
			SELECT task_id, parent_task_id, 0 FROM tasks WHERE task_id = ?
			UNION ALL
			SELECT t.task_id, t.parent_task_id, c.depth + 1
			FROM tasks t
			JOIN chain c ON t.task_id = c.pid
			WHERE c.pid IS NOT NULL AND c.pid != '' AND c.depth < 16
		)
		SELECT COUNT(*) FROM chain WHERE pid = ?
	`, targetTaskID, ancestorTaskID).Scan(&matched)
	return matched > 0, err
}

// IsSiblingTask reports whether taskA and taskB are distinct child tasks sharing the same non-empty parent_task_id.
func (s *Store) IsSiblingTask(ctx context.Context, taskA, taskB string) (bool, error) {
	var matched bool
	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		matched, err = isSiblingTaskTx(ctx, tx, taskA, taskB)
		return err
	})
	return matched, err
}

func isSiblingTaskTx(ctx context.Context, tx *sql.Tx, taskA, taskB string) (bool, error) {
	taskA = strings.TrimSpace(taskA)
	taskB = strings.TrimSpace(taskB)
	if taskA == "" || taskB == "" || taskA == taskB {
		return false, nil
	}
	var parentA, parentB sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT parent_task_id FROM tasks WHERE task_id = ?`, taskA).Scan(&parentA); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if !parentA.Valid || strings.TrimSpace(parentA.String) == "" {
		return false, nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT parent_task_id FROM tasks WHERE task_id = ?`, taskB).Scan(&parentB); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if !parentB.Valid || strings.TrimSpace(parentB.String) == "" {
		return false, nil
	}
	return parentA.String == parentB.String, nil
}

// RunnerParentControlGuard carries the runner machine & parent attempt identity to be verified
// atomically INSIDE delegate write transactions (spawn, send, cancel).
type RunnerParentControlGuard struct {
	MachineID       string
	ParentTaskID    string
	ParentAttemptID string
	AllowSibling    bool
}

func verifyRunnerParentControlTx(ctx context.Context, tx *sql.Tx, g *RunnerParentControlGuard) error {
	if g == nil {
		return nil
	}
	mach := strings.TrimSpace(g.MachineID)
	parentID := strings.TrimSpace(g.ParentTaskID)
	attID := strings.TrimSpace(g.ParentAttemptID)
	if mach == "" || parentID == "" || attID == "" {
		return fmt.Errorf("%w: parentTaskId, parentAttemptId, and runner machineId are required", ErrForbidden)
	}
	var activeAttID sql.NullString
	var archivedAt sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
		SELECT active_attempt_id, archived_at FROM tasks WHERE task_id = ?
	`, parentID).Scan(&activeAttID, &archivedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: parent task %s not found", ErrForbidden, parentID)
		}
		return err
	}
	if archivedAt.Valid {
		return fmt.Errorf("%w: parent task %s is archived", ErrForbidden, parentID)
	}
	if !activeAttID.Valid || activeAttID.String != attID {
		return fmt.Errorf("%w: parentAttemptId %s is not the active attempt of %s", ErrForbidden, attID, parentID)
	}
	var attTaskID, attMachineID, attStateStr string
	var attLeaseExpiresAt int64
	if err := tx.QueryRowContext(ctx, `
		SELECT task_id, machine_id, state, lease_expires_at FROM attempts WHERE attempt_id = ?
	`, attID).Scan(&attTaskID, &attMachineID, &attStateStr, &attLeaseExpiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: parentAttemptId %s not found", ErrForbidden, attID)
		}
		return err
	}
	if attTaskID != parentID || attMachineID != mach {
		return fmt.Errorf("%w: parentAttemptId does not match parentTaskId and runner machine", ErrForbidden)
	}
	if !slices.Contains(protocol.ActiveAttemptStates, protocol.AttemptState(attStateStr)) {
		return fmt.Errorf("%w: parentAttemptId %s is no longer active (state=%s)", ErrForbidden, attID, attStateStr)
	}
	if attLeaseExpiresAt < time.Now().UnixMilli() {
		return fmt.Errorf("%w: parentAttemptId %s lease has expired", ErrForbidden, attID)
	}
	return nil
}

// EmitTaskChildUpdated appends a task.child_updated event to childTaskID's parent stream
// if childTaskID has a parent_task_id. Returns nil if the task has no parent.
func (s *Store) EmitTaskChildUpdated(ctx context.Context, childTaskID, attemptID string) (*protocol.EventRecord, error) {
	var emitted *protocol.EventRecord
	now := time.Now().UnixMilli()
	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var parentID sql.NullString
		var stateStr string
		var openApprovals int
		err := tx.QueryRowContext(ctx, `
			SELECT parent_task_id, state,
			       (SELECT COUNT(*) FROM approvals ap WHERE ap.task_id = tasks.task_id AND ap.state = 'pending')
			FROM tasks WHERE task_id = ?
		`, childTaskID).Scan(&parentID, &stateStr, &openApprovals)
		if errors.Is(err, sql.ErrNoRows) || !parentID.Valid || parentID.String == "" {
			return nil
		}
		if err != nil {
			return err
		}
		var parentProjectID string
		if err := tx.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE task_id = ?`, parentID.String).Scan(&parentProjectID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		step := loadChildCurrentStepTx(ctx, tx, childTaskID)
		payload := map[string]any{
			"childTaskId":       childTaskID,
			"state":             protocol.TaskState(stateStr),
			"attemptId":         attemptID,
			"openApprovalCount": openApprovals,
		}
		if step != "" {
			payload["currentStep"] = step
		}
		ev, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + parentID.String,
			ProjectID:  parentProjectID,
			TaskID:     parentID.String,
			AttemptID:  attemptID,
			ActorKind:  protocol.ActorSystem,
			ActorID:    "child:" + childTaskID,
			Kind:       protocol.EventTaskChildUpdated,
			Visibility: protocol.VisibilityTimeline,
			Payload:    payload,
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

// PendingChildParentNotification represents a durable child terminal notification waiting
// to be atomically delivered to its parent task, ordered by Hub SQLite AUTOINCREMENT notif_seq.
type PendingChildParentNotification struct {
	NotifSeq       int64
	ChildAttemptID string
	ChildTaskID    string
	ParentTaskID   string
	ChildState     protocol.AttemptState
	Status         string
	RetryCount     int
	NextRetryAt    int64
	LastError      string
	CreatedAt      int64
}

// ChildNotificationDelivery links a parent SubmitFollowupAtomic transaction to a pending
// child_parent_notifications row so state validation and delivery confirmation happen atomically.
type ChildNotificationDelivery struct {
	ChildAttemptID string
}

func isNotifiableChildTerminalState(st protocol.AttemptState) bool {
	switch st {
	case protocol.AttemptSucceeded,
		protocol.AttemptFailed,
		protocol.AttemptCancelled,
		protocol.AttemptNeedsVerification,
		protocol.AttemptExpired:
		return true
	default:
		return false
	}
}

// enqueueChildParentNotificationTx atomically records a pending parent wake-up row inside the
// SAME write transaction that transitions a child attempt into a terminal state.
func enqueueChildParentNotificationTx(ctx context.Context, tx *sql.Tx, childTaskID, childAttemptID string, childState protocol.AttemptState, now int64) error {
	if strings.TrimSpace(childTaskID) == "" || strings.TrimSpace(childAttemptID) == "" || !isNotifiableChildTerminalState(childState) {
		return nil
	}
	if now == 0 {
		now = time.Now().UnixMilli()
	}
	_, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO child_parent_notifications (
			child_attempt_id, child_task_id, parent_task_id, child_state, status, retry_count, next_retry_at, created_at
		)
		SELECT ?, t.task_id, t.parent_task_id, ?, 'pending', 0, 0, ?
		FROM tasks t
		JOIN tasks p ON p.task_id = t.parent_task_id
		WHERE t.task_id = ?
		  AND t.parent_task_id IS NOT NULL AND t.parent_task_id != ''
		  AND t.notify_parent_on_finish = 1
	`, childAttemptID, string(childState), now, childTaskID)
	if err != nil {
		return fmt.Errorf("enqueue child_parent_notification (%s/%s): %w", childTaskID, childAttemptID, err)
	}
	return nil
}

// ListPendingChildParentNotifications returns ready pending notifications after cursor afterSeq
// in strict Hub SQLite notif_seq ASC order, skipping any notification whose parent has an earlier
// pending notification currently in backoff (guaranteeing strict per-parent FIFO without head-of-line
// starvation across distinct parents).
func (s *Store) ListPendingChildParentNotifications(ctx context.Context, nowMs, afterSeq int64, limit int) ([]PendingChildParentNotification, error) {
	if limit <= 0 {
		limit = 64
	}
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}
	var out []PendingChildParentNotification
	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT c.notif_seq, c.child_attempt_id, c.child_task_id, c.parent_task_id,
			       c.child_state, c.status, c.retry_count, c.next_retry_at,
			       COALESCE(c.last_error, ''), c.created_at
			FROM child_parent_notifications c
			WHERE c.status = 'pending'
			  AND c.next_retry_at <= ?
			  AND c.notif_seq > ?
			  AND NOT EXISTS (
				SELECT 1 FROM child_parent_notifications prev
				WHERE prev.parent_task_id = c.parent_task_id
				  AND prev.status = 'pending'
				  AND prev.notif_seq < c.notif_seq
				  AND prev.next_retry_at > ?
			  )
			ORDER BY c.notif_seq ASC
			LIMIT ?
		`, nowMs, afterSeq, nowMs, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n PendingChildParentNotification
			var stStr string
			if err := rows.Scan(
				&n.NotifSeq, &n.ChildAttemptID, &n.ChildTaskID, &n.ParentTaskID,
				&stStr, &n.Status, &n.RetryCount, &n.NextRetryAt,
				&n.LastError, &n.CreatedAt,
			); err != nil {
				return err
			}
			n.ChildState = protocol.AttemptState(stStr)
			out = append(out, n)
		}
		return rows.Err()
	})
	return out, err
}

// MarkChildParentNotificationSkipped marks a pending child notification as skipped (e.g. parent deleted/archived/cancelled).
func (s *Store) MarkChildParentNotificationSkipped(ctx context.Context, childAttemptID, reason string) error {
	now := time.Now().UnixMilli()
	return s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE child_parent_notifications
			SET status = 'skipped', last_error = ?, delivered_at = ?
			WHERE child_attempt_id = ? AND status = 'pending'
		`, reason, now, childAttemptID)
		return err
	})
}

// RecordChildParentNotificationError records the latest error on a still-pending notification
// and applies bounded exponential backoff (2s -> 4s -> 8s -> 16s -> 30s) so blocked parents do not
// spin or starve other parents.
func (s *Store) RecordChildParentNotificationError(ctx context.Context, childAttemptID, errMsg string, nowMs int64) error {
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}
	return s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var retryCount int
		err := tx.QueryRowContext(ctx, `
			SELECT retry_count FROM child_parent_notifications
			WHERE child_attempt_id = ? AND status = 'pending'
		`, childAttemptID).Scan(&retryCount)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		nextRetryCount := retryCount + 1
		delayMs := int64(2000)
		for i := 1; i < nextRetryCount && delayMs < 30000; i++ {
			delayMs *= 2
		}
		if delayMs > 30000 {
			delayMs = 30000
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE child_parent_notifications
			SET retry_count = ?, next_retry_at = ?, last_error = ?
			WHERE child_attempt_id = ? AND status = 'pending'
		`, nextRetryCount, nowMs+delayMs, truncateRunes(errMsg, 500), childAttemptID)
		return err
	})
}

func resetParentChildNotificationBackoffTx(ctx context.Context, tx *sql.Tx, parentTaskID string) error {
	if strings.TrimSpace(parentTaskID) == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE child_parent_notifications
		SET next_retry_at = 0
		WHERE parent_task_id = ? AND status = 'pending' AND next_retry_at > 0
	`, parentTaskID)
	return err
}

// ResetParentChildNotificationBackoff immediately clears backoff on pending notifications for parentTaskID
// (e.g. right after the human verifies a needs_verification attempt on parentTaskID).
func (s *Store) ResetParentChildNotificationBackoff(ctx context.Context, parentTaskID string) error {
	return s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		return resetParentChildNotificationBackoffTx(ctx, tx, parentTaskID)
	})
}

// checkActiveChildLimitTx enforces maxActiveChildrenPerParent across task creation and all
// reactivation paths (follow-up, retry, queue dispatch, annotation dispatch).
func checkActiveChildLimitTx(ctx context.Context, tx *sql.Tx, parentTaskID, excludeTaskID string) error {
	parentTaskID = strings.TrimSpace(parentTaskID)
	if parentTaskID == "" {
		return nil
	}
	var activeChildren int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM tasks
		WHERE parent_task_id = ?
		  AND archived_at IS NULL
		  AND task_id != ?
		  AND state IN ('queued', 'waiting_resource', 'running', 'waiting_approval')
	`, parentTaskID, excludeTaskID).Scan(&activeChildren); err != nil {
		return err
	}
	if activeChildren >= maxActiveChildrenPerParent {
		return fmt.Errorf("%w: parent task %s already has %d active child tasks (limit %d)", ErrConflict, parentTaskID, activeChildren, maxActiveChildrenPerParent)
	}
	return nil
}

// computeTaskDepthTx calculates the true delegation depth of taskID by traversing parent_task_id links.
func computeTaskDepthTx(ctx context.Context, tx *sql.Tx, taskID string) (int, error) {
	var depth int
	err := tx.QueryRowContext(ctx, `
		WITH RECURSIVE anc(tid, pid, d) AS (
			SELECT task_id, parent_task_id, 0 FROM tasks WHERE task_id = ?
			UNION ALL
			SELECT t.task_id, t.parent_task_id, anc.d + 1
			FROM tasks t
			JOIN anc ON t.task_id = anc.pid
			WHERE anc.pid IS NOT NULL AND anc.pid != '' AND anc.d < 16
		)
		SELECT COALESCE(MAX(d), 0) FROM anc
	`, taskID).Scan(&depth)
	return depth, err
}

var (
	v7HookMu                   sync.Mutex
	v7MigrationBeforeCommitFn func(ctx context.Context, tx *sql.Tx) error
)

// SetV7MigrationBeforeCommitHookForTest installs an optional hook executed inside the v7 migration
// transaction right before PRAGMA foreign_key_check and COMMIT (used by unit tests to verify rollback).
func SetV7MigrationBeforeCommitHookForTest(fn func(ctx context.Context, tx *sql.Tx) error) func() {
	v7HookMu.Lock()
	prev := v7MigrationBeforeCommitFn
	v7MigrationBeforeCommitFn = fn
	v7HookMu.Unlock()
	return func() {
		v7HookMu.Lock()
		v7MigrationBeforeCommitFn = prev
		v7HookMu.Unlock()
	}
}

func getV7MigrationBeforeCommitHook() func(ctx context.Context, tx *sql.Tx) error {
	v7HookMu.Lock()
	defer v7HookMu.Unlock()
	return v7MigrationBeforeCommitFn
}

func restoreConnPragmasStrict(conn *sql.Conn) error {
	rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := conn.ExecContext(rctx, `PRAGMA legacy_alter_table = OFF; PRAGMA foreign_keys = ON;`); err != nil {
		return fmt.Errorf("restore sqlite pragmas: %w", err)
	}
	var fkOn int
	if err := conn.QueryRowContext(rctx, `PRAGMA foreign_keys`).Scan(&fkOn); err != nil {
		return fmt.Errorf("verify PRAGMA foreign_keys after restore: %w", err)
	}
	if fkOn != 1 {
		return fmt.Errorf("PRAGMA foreign_keys failed to re-enable (got %d)", fkOn)
	}
	return nil
}

// applyMigrationV7Atomic runs schema v7 (columns, recursive delegation_depth backfill, and optional
// legacy attempts CHECK rebuild preserving ALL indexes/triggers) inside ONE atomic transaction on a
// dedicated *sql.Conn, validating PRAGMA foreign_key_check BEFORE writing schema_migrations(7) and COMMIT.
func applyMigrationV7Atomic(ctx context.Context, db *sql.DB) (retErr error) {
	var hasV7 int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 7`).Scan(&hasV7); err != nil {
		return err
	}
	var createSQL sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'attempts'`).Scan(&createSQL); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	needsAttemptsRebuild := createSQL.Valid && !strings.Contains(createSQL.String, "'"+string(protocol.TriggerChildResult)+"'")

	// Check whether child_parent_notifications exists and whether it needs schema upgrade to notif_seq/next_retry_at.
	var notifDDL sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'child_parent_notifications'`).Scan(&notifDDL); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	needsNotifCreate := !notifDDL.Valid
	needsNotifUpgrade := notifDDL.Valid && (!strings.Contains(notifDDL.String, "notif_seq") || !strings.Contains(notifDDL.String, "next_retry_at"))

	// Also check if any existing child rows still have unbackfilled delegation_depth = 0.
	var unbackfilledDepth int
	if hasV7 > 0 {
		_ = db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM tasks
			WHERE parent_task_id IS NOT NULL AND parent_task_id != '' AND COALESCE(delegation_depth, 0) = 0
		`).Scan(&unbackfilledDepth)
	}

	if hasV7 > 0 && !needsAttemptsRebuild && !needsNotifCreate && !needsNotifUpgrade && unbackfilledDepth == 0 {
		return nil
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire dedicated sqlite conn for v7 migration: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF; PRAGMA legacy_alter_table = ON;`); err != nil {
		return fmt.Errorf("set migration pragmas on dedicated conn: %w", err)
	}
	pragmasRestored := false
	defer func() {
		if !pragmasRestored {
			if rErr := restoreConnPragmasStrict(conn); rErr != nil && retErr == nil {
				retErr = rErr
			}
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UnixMilli()

	// 1. Ensure all v7 additive columns exist on tasks and board_members.
	if err := ensureColumnTx(ctx, tx, "tasks", "delegated_by_attempt_id", "TEXT"); err != nil {
		return err
	}
	if err := ensureColumnTx(ctx, tx, "tasks", "delegated_role", "TEXT"); err != nil {
		return err
	}
	if err := ensureColumnTx(ctx, tx, "tasks", "notify_parent_on_finish", "INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if err := ensureColumnTx(ctx, tx, "tasks", "parent_notified_attempt_id", "TEXT"); err != nil {
		return err
	}
	if err := ensureColumnTx(ctx, tx, "tasks", "delegation_depth", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureColumnTx(ctx, tx, "board_members", "source_kind", "TEXT"); err != nil {
		return err
	}
	if err := ensureColumnTx(ctx, tx, "board_members", "role", "TEXT"); err != nil {
		return err
	}
	if err := ensureColumnTx(ctx, tx, "board_members", "brief", "TEXT"); err != nil {
		return err
	}
	if err := ensureColumnTx(ctx, tx, "board_members", "external_session_id", "TEXT"); err != nil {
		return err
	}

	// 1b. Atomically create or upgrade child_parent_notifications inside this SAME v7 transaction.
	if needsNotifCreate {
		if _, err := tx.ExecContext(ctx, BuildMigrationV7DDL()); err != nil {
			return fmt.Errorf("create child_parent_notifications in v7 migration: %w", err)
		}
	} else if needsNotifUpgrade {
		var missingFinishedEvents int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM child_parent_notifications old
			WHERE NOT EXISTS (
				SELECT 1 FROM events e
				WHERE e.attempt_id = old.child_attempt_id AND e.kind = 'attempt.finished'
			)
		`).Scan(&missingFinishedEvents); err != nil {
			return fmt.Errorf("check finished events for child_parent_notifications upgrade: %w", err)
		}
		orderClause := `ORDER BY old.rowid ASC`
		if missingFinishedEvents == 0 {
			orderClause = `ORDER BY (SELECT MAX(e.seq) FROM events e WHERE e.attempt_id = old.child_attempt_id AND e.kind = 'attempt.finished') ASC, old.rowid ASC`
		}
		upgradeSQL := `
			DROP TABLE IF EXISTS child_parent_notifications_v7_new;
			CREATE TABLE child_parent_notifications_v7_new (
				notif_seq        INTEGER PRIMARY KEY AUTOINCREMENT,
				child_attempt_id TEXT NOT NULL UNIQUE REFERENCES attempts(attempt_id),
				child_task_id    TEXT NOT NULL REFERENCES tasks(task_id),
				parent_task_id   TEXT NOT NULL REFERENCES tasks(task_id),
				child_state      TEXT NOT NULL,
				status           TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'delivered', 'skipped')),
				retry_count      INTEGER NOT NULL DEFAULT 0,
				next_retry_at    INTEGER NOT NULL DEFAULT 0,
				last_error       TEXT,
				created_at       INTEGER NOT NULL,
				delivered_at     INTEGER
			);
			INSERT INTO child_parent_notifications_v7_new (
				child_attempt_id, child_task_id, parent_task_id, child_state, status,
				retry_count, next_retry_at, last_error, created_at, delivered_at
			)
			SELECT old.child_attempt_id, old.child_task_id, old.parent_task_id, old.child_state, old.status,
			       0, 0, old.last_error, old.created_at, old.delivered_at
			FROM child_parent_notifications old
			` + orderClause + `;
			DROP TABLE child_parent_notifications;
			ALTER TABLE child_parent_notifications_v7_new RENAME TO child_parent_notifications;
			CREATE INDEX IF NOT EXISTS idx_child_parent_notif_pending
				ON child_parent_notifications(status, next_retry_at, notif_seq);
		`
		if _, err := tx.ExecContext(ctx, upgradeSQL); err != nil {
			return fmt.Errorf("upgrade child_parent_notifications to notif_seq schema: %w", err)
		}
	}

	// 2. Backfill real delegation_depth across all existing parent_task_id chains via recursive CTE.
	if _, err := tx.ExecContext(ctx, `
		WITH RECURSIVE tree(task_id, depth) AS (
			SELECT task_id, 0 FROM tasks WHERE parent_task_id IS NULL OR parent_task_id = ''
			UNION ALL
			SELECT t.task_id, tree.depth + 1
			FROM tasks t
			JOIN tree ON t.parent_task_id = tree.task_id
			WHERE tree.depth < 16
		)
		UPDATE tasks
		SET delegation_depth = (SELECT tree.depth FROM tree WHERE tree.task_id = tasks.task_id)
		WHERE task_id IN (SELECT task_id FROM tree)
		  AND delegation_depth != (SELECT tree.depth FROM tree WHERE tree.task_id = tasks.task_id)
	`); err != nil {
		return fmt.Errorf("backfill tasks.delegation_depth: %w", err)
	}

	// 3. Rebuild attempts table if its trigger_kind CHECK predates 'child_result', dynamically
	//    preserving ALL explicit indexes and triggers attached to attempts.
	if needsAttemptsRebuild {
		colRows, err := tx.QueryContext(ctx, `PRAGMA table_info(attempts)`)
		if err != nil {
			return err
		}
		var cols []string
		for colRows.Next() {
			var cid, notNull, pk int
			var name, colType string
			var dflt sql.NullString
			if err := colRows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
				colRows.Close()
				return err
			}
			cols = append(cols, quoteSQLiteIdent(name))
		}
		if err := colRows.Err(); err != nil {
			colRows.Close()
			return err
		}
		colRows.Close()

		depRows, err := tx.QueryContext(ctx, `
			SELECT name, sql FROM sqlite_master
			WHERE tbl_name = 'attempts' AND type IN ('index', 'trigger') AND sql IS NOT NULL
			ORDER BY CASE type WHEN 'index' THEN 0 ELSE 1 END, name ASC
		`)
		if err != nil {
			return fmt.Errorf("inspect attempts indexes/triggers: %w", err)
		}
		var existingDepSQLs []string
		existingDepNames := map[string]bool{}
		for depRows.Next() {
			var depName, depSQL string
			if err := depRows.Scan(&depName, &depSQL); err != nil {
				depRows.Close()
				return err
			}
			existingDepNames[strings.ToLower(depName)] = true
			existingDepSQLs = append(existingDepSQLs, depSQL)
		}
		if err := depRows.Err(); err != nil {
			depRows.Close()
			return err
		}
		depRows.Close()

		if len(cols) > 0 {
			rebuiltDDL := fmt.Sprintf(`
CREATE TABLE attempts_v7_rebuild (
  attempt_id          TEXT PRIMARY KEY,
  task_id             TEXT NOT NULL,
  project_id          TEXT NOT NULL,
  attempt_no          INTEGER NOT NULL,
  machine_id          TEXT NOT NULL REFERENCES machines(machine_id),
  adapter             TEXT NOT NULL CHECK (adapter IN (%s)),
  wb_session_id       TEXT REFERENCES agent_sessions(wb_session_id),
  trigger_kind        TEXT NOT NULL CHECK (trigger_kind IN (%s)),
  trigger_payload_json TEXT NOT NULL DEFAULT '{}',
  worktree_path       TEXT,
  input_delivery_id   TEXT,
  base_sha            TEXT,
  output_delivery_id  TEXT,
  target_env          TEXT,
  deploy_phase        TEXT CHECK (deploy_phase IN (%s)),
  state               TEXT NOT NULL CHECK (state IN (%s)),
  resume_state        TEXT CHECK (resume_state IN (%s)),
  completion_source   TEXT CHECK (completion_source IN (%s)),
  lease_token         TEXT NOT NULL,
  lease_expires_at    INTEGER NOT NULL,
  last_acked_outbox   INTEGER NOT NULL DEFAULT 0,
  error_code          TEXT,
  error_detail        TEXT,
  token_usage_json    TEXT,
  started_at          INTEGER NOT NULL,
  finished_at         INTEGER,
  runner_instance_id  TEXT,
  cancel_requested    INTEGER NOT NULL DEFAULT 0,
  cancel_reason_kind  TEXT,
  steer_input_id      TEXT,
  desired_config_json TEXT,
  required_resources_json TEXT,
  anchor_json         TEXT,
  part_id             TEXT,
  location_id         TEXT,
  isolation           TEXT,
  folder_changes_json TEXT,
  FOREIGN KEY (task_id, project_id) REFERENCES tasks(task_id, project_id),
  UNIQUE (task_id, attempt_no),
  UNIQUE (attempt_id, task_id, project_id)
);
`,
				protocol.SQLInClause(protocol.AllAdapters),
				protocol.SQLInClause(protocol.AllTriggerKinds),
				protocol.SQLInClause(protocol.AllDeployPhases),
				protocol.SQLInClause(protocol.AllAttemptStates),
				protocol.SQLInClause(protocol.AllResumeStates),
				protocol.SQLInClause(protocol.AllCompletionSources),
			)
			if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS attempts_v7_rebuild`); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, rebuiltDDL); err != nil {
				return fmt.Errorf("create attempts_v7_rebuild table: %w", err)
			}
			colList := strings.Join(cols, ", ")
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO attempts_v7_rebuild (%s) SELECT %s FROM attempts`, colList, colList)); err != nil {
				return fmt.Errorf("copy attempts rows: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `DROP TABLE attempts`); err != nil {
				return fmt.Errorf("drop legacy attempts table: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `ALTER TABLE attempts_v7_rebuild RENAME TO attempts`); err != nil {
				return fmt.Errorf("rename attempts_v7_rebuild to attempts: %w", err)
			}
			// Restore every dynamically discovered index & trigger first.
			for _, depSQL := range existingDepSQLs {
				if _, err := tx.ExecContext(ctx, depSQL); err != nil {
					return fmt.Errorf("recreate attempts index/trigger: %w", err)
				}
			}
			// Ensure canonical attempts indexes also exist (IF NOT EXISTS).
			canonicalIndexes := fmt.Sprintf(`
CREATE UNIQUE INDEX IF NOT EXISTS idx_attempts_one_active_per_task
  ON attempts(task_id) WHERE state IN (%s);
CREATE UNIQUE INDEX IF NOT EXISTS idx_attempts_one_active_per_session
  ON attempts(wb_session_id) WHERE wb_session_id IS NOT NULL AND state IN (%s);
CREATE UNIQUE INDEX IF NOT EXISTS idx_attempts_one_running_deploy_per_env
  ON attempts(target_env) WHERE adapter = 'ssh_deploy' AND target_env IS NOT NULL AND state IN (%s);
CREATE INDEX IF NOT EXISTS idx_attempts_machine_active
  ON attempts(machine_id, state) WHERE state IN (%s);
`,
				protocol.SQLInClause(protocol.ActiveAttemptStates),
				protocol.SQLInClause(protocol.ActiveAttemptStates),
				protocol.SQLInClause(protocol.ActiveDeployAttemptStates),
				protocol.SQLInClause(protocol.ActiveAttemptStates),
			)
			if _, err := tx.ExecContext(ctx, canonicalIndexes); err != nil {
				return fmt.Errorf("ensure canonical attempts indexes: %w", err)
			}
		}
	}

	if hook := getV7MigrationBeforeCommitHook(); hook != nil {
		if err := hook(ctx, tx); err != nil {
			return err
		}
	}

	// 4. Verify foreign key integrity INSIDE the migration transaction BEFORE committing or recording version 7.
	if err := verifyForeignKeyIntegrityTx(ctx, tx); err != nil {
		return err
	}

	if hasV7 == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO schema_migrations (version, applied_at) VALUES (7, ?)`, now); err != nil {
			return fmt.Errorf("record schema migration v7: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit v7 migration: %w", err)
	}
	if err := restoreConnPragmasStrict(conn); err != nil {
		return err
	}
	pragmasRestored = true
	return nil
}

func quoteSQLiteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// rewriteCreateTableName parses the leading `CREATE [TEMP|TEMPORARY] TABLE [IF NOT EXISTS] <ident>`
// clause using SQLite's identifier quoting rules ("...", `...`, [...], or bare ident) and replaces
// ONLY the table name token cleanly with quoteSQLiteIdent(newName).
func rewriteCreateTableName(ddl, expectedTableName, newTableName string) (string, error) {
	n := len(ddl)
	i := 0
	skipWS := func() {
		for i < n && (ddl[i] == ' ' || ddl[i] == '\t' || ddl[i] == '\r' || ddl[i] == '\n') {
			i++
		}
	}
	matchKeyword := func(kw string) bool {
		skipWS()
		end := i + len(kw)
		if end > n {
			return false
		}
		if !strings.EqualFold(ddl[i:end], kw) {
			return false
		}
		if end < n {
			c := ddl[end]
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
				return false
			}
		}
		i = end
		return true
	}

	if !matchKeyword("CREATE") {
		return "", fmt.Errorf("DDL does not start with CREATE")
	}
	_ = matchKeyword("TEMP") || matchKeyword("TEMPORARY")
	if !matchKeyword("TABLE") {
		return "", fmt.Errorf("DDL missing TABLE keyword")
	}
	if matchKeyword("IF") {
		if !matchKeyword("NOT") || !matchKeyword("EXISTS") {
			return "", fmt.Errorf("malformed IF NOT EXISTS in DDL")
		}
	}
	skipWS()
	if i >= n {
		return "", fmt.Errorf("unexpected end of DDL before table name")
	}

	identStart := i
	var parsed string
	switch ddl[i] {
	case '"', '`':
		quote := ddl[i]
		i++
		var sb strings.Builder
		closed := false
		for i < n {
			if ddl[i] == quote {
				if i+1 < n && ddl[i+1] == quote {
					sb.WriteByte(quote)
					i += 2
					continue
				}
				i++
				closed = true
				break
			}
			sb.WriteByte(ddl[i])
			i++
		}
		if !closed {
			return "", fmt.Errorf("unterminated quoted table identifier in DDL")
		}
		parsed = sb.String()
	case '[':
		i++
		start := i
		for i < n && ddl[i] != ']' {
			i++
		}
		if i >= n {
			return "", fmt.Errorf("unterminated bracketed table identifier in DDL")
		}
		parsed = ddl[start:i]
		i++
	default:
		start := i
		for i < n {
			c := ddl[i]
			if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '(' {
				break
			}
			i++
		}
		parsed = ddl[start:i]
	}
	identEnd := i
	if !strings.EqualFold(parsed, expectedTableName) {
		return "", fmt.Errorf("DDL table identifier %q does not match expected %q", parsed, expectedTableName)
	}
	return ddl[:identStart] + quoteSQLiteIdent(newTableName) + ddl[identEnd:], nil
}

// repairDanglingAttemptForeignKeys idempotently repairs databases whose referencing tables
// (task_pending_inputs, deliveries, annotations, approvals) had their foreign keys rewritten
// to dropped temporary table names ("attempts_legacy_v6" / "attempts_bak") by SQLite 3.26+
// when `ALTER TABLE attempts RENAME TO ...` ran without `PRAGMA legacy_alter_table = ON`.
func repairDanglingAttemptForeignKeys(ctx context.Context, db *sql.DB, absPath string) (retErr error) {
	type corruptedTable struct {
		name            string
		repairCreateSQL string
		cols            []string
		depSQLs         []string
	}

	rows, err := db.QueryContext(ctx, `
		SELECT name, sql FROM sqlite_master
		WHERE type = 'table' AND sql IS NOT NULL
		  AND (sql LIKE '%attempts_legacy_v6%' OR sql LIKE '%attempts_bak%')
		ORDER BY name ASC
	`)
	if err != nil {
		return fmt.Errorf("scan dangling attempt foreign keys: %w", err)
	}
	var rawTables []struct{ name, ddl string }
	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			rows.Close()
			return err
		}
		rawTables = append(rawTables, struct{ name, ddl string }{name: name, ddl: ddl})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(rawTables) == 0 {
		return nil
	}

	// Take a consistent one-time hot backup before modifying live tables.
	if absPath != "" && absPath != ":memory:" {
		bakPath := absPath + ".pre-fk-repair.bak"
		if _, statErr := os.Stat(bakPath); os.IsNotExist(statErr) {
			tmpPath := fmt.Sprintf("%s.tmp.%d", bakPath, time.Now().UnixNano())
			_ = os.Remove(tmpPath)
			escaped := fmt.Sprintf("'%s'", replaceAllSingleQuotes(filepath.ToSlash(tmpPath)))
			if _, err := db.ExecContext(ctx, "VACUUM INTO "+escaped); err != nil {
				_ = os.Remove(tmpPath)
				return fmt.Errorf("vacuum pre-fk-repair backup: %w", err)
			}
			if err := os.Rename(tmpPath, bakPath); err != nil {
				_ = os.Remove(tmpPath)
				return fmt.Errorf("finalize pre-fk-repair backup: %w", err)
			}
		}
	}

	replacer := strings.NewReplacer(
		`"attempts_legacy_v6"`, "attempts",
		`attempts_legacy_v6`, "attempts",
		`"attempts_bak"`, "attempts",
		`attempts_bak`, "attempts",
	)

	tables := make([]corruptedTable, 0, len(rawTables))
	for _, rt := range rawTables {
		colRows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, quoteSQLiteIdent(rt.name)))
		if err != nil {
			return fmt.Errorf("inspect columns for %s: %w", rt.name, err)
		}
		var cols []string
		for colRows.Next() {
			var cid, notNull, pk int
			var colName, colType string
			var dflt sql.NullString
			if err := colRows.Scan(&cid, &colName, &colType, &notNull, &dflt, &pk); err != nil {
				colRows.Close()
				return err
			}
			cols = append(cols, quoteSQLiteIdent(colName))
		}
		if err := colRows.Err(); err != nil {
			colRows.Close()
			return err
		}
		colRows.Close()
		if len(cols) == 0 {
			continue
		}

		depRows, err := db.QueryContext(ctx, `
			SELECT sql FROM sqlite_master
			WHERE tbl_name = ? AND type IN ('index', 'trigger') AND sql IS NOT NULL
			ORDER BY CASE type WHEN 'index' THEN 0 ELSE 1 END, name ASC
		`, rt.name)
		if err != nil {
			return fmt.Errorf("inspect indexes/triggers for %s: %w", rt.name, err)
		}
		var depSQLs []string
		for depRows.Next() {
			var depSQL string
			if err := depRows.Scan(&depSQL); err != nil {
				depRows.Close()
				return err
			}
			depSQLs = append(depSQLs, replacer.Replace(depSQL))
		}
		if err := depRows.Err(); err != nil {
			depRows.Close()
			return err
		}
		depRows.Close()

		fixedDDL := replacer.Replace(rt.ddl)
		tmpName := rt.name + "__fk_repair"
		repairCreateSQL, err := rewriteCreateTableName(fixedDDL, rt.name, tmpName)
		if err != nil {
			return fmt.Errorf("rewrite CREATE TABLE statement for %s: %w", rt.name, err)
		}

		tables = append(tables, corruptedTable{
			name:            rt.name,
			repairCreateSQL: repairCreateSQL,
			cols:            cols,
			depSQLs:         depSQLs,
		})
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire dedicated sqlite conn for fk repair: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF; PRAGMA legacy_alter_table = ON;`); err != nil {
		return fmt.Errorf("disable foreign_keys for fk repair: %w", err)
	}
	pragmasRestored := false
	defer func() {
		if !pragmasRestored {
			if rErr := restoreConnPragmasStrict(conn); rErr != nil && retErr == nil {
				retErr = rErr
			}
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, tbl := range tables {
		tmpName := tbl.name + "__fk_repair"
		tmpIdent := quoteSQLiteIdent(tmpName)
		tblIdent := quoteSQLiteIdent(tbl.name)
		colList := strings.Join(tbl.cols, ", ")
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tmpIdent)); err != nil {
			return fmt.Errorf("cleanup temp repair table %s: %w", tmpName, err)
		}
		if _, err := tx.ExecContext(ctx, tbl.repairCreateSQL); err != nil {
			return fmt.Errorf("create repair table %s: %w", tmpName, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s (%s) SELECT %s FROM %s`, tmpIdent, colList, colList, tblIdent)); err != nil {
			return fmt.Errorf("copy rows during fk repair of %s: %w", tbl.name, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DROP TABLE %s`, tblIdent)); err != nil {
			return fmt.Errorf("drop corrupted table %s: %w", tbl.name, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s RENAME TO %s`, tmpIdent, tblIdent)); err != nil {
			return fmt.Errorf("rename repaired table %s: %w", tbl.name, err)
		}
		for _, depSQL := range tbl.depSQLs {
			if _, err := tx.ExecContext(ctx, depSQL); err != nil {
				return fmt.Errorf("recreate index/trigger on %s: %w", tbl.name, err)
			}
		}
	}

	var remaining int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND sql IS NOT NULL
		  AND (sql LIKE '%attempts_legacy_v6%' OR sql LIKE '%attempts_bak%')
	`).Scan(&remaining); err != nil {
		return err
	}
	if remaining > 0 {
		return fmt.Errorf("fk repair incomplete: %d tables still reference legacy attempts", remaining)
	}
	if err := verifyForeignKeyIntegrityTx(ctx, tx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit fk repair: %w", err)
	}
	if err := restoreConnPragmasStrict(conn); err != nil {
		return err
	}
	pragmasRestored = true
	return nil
}

func verifyForeignKeyIntegrityTx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("foreign_key_check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		var tbl, parent string
		var rowid sql.NullInt64
		var fkid int
		_ = rows.Scan(&tbl, &rowid, &parent, &fkid)
		return fmt.Errorf("foreign key integrity violation on table %s (rowid=%d -> parent=%s)", tbl, rowid.Int64, parent)
	}
	return rows.Err()
}
