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

const (
	hebi8MainTaskSettingKey = "hebi8_main_task_id"
	hebi8RolloverStateKey   = "hebi8_rollover_state"
)

// Hebi8RolloverState persists the authoritative Fold-Rollover generation, attempt watermark,
// and committed node IDs in SQLite so fold commits, crash recovery, and concurrent attempt
// creations share the same BEGIN IMMEDIATE transaction boundary.
type Hebi8RolloverState struct {
	Generation          int64    `json:"generation"`
	LastFoldedAttemptNo int      `json:"lastFoldedAttemptNo"`
	LastSegmentID       string   `json:"lastSegmentId,omitempty"`
	PendingOpeningGen   int64    `json:"pendingOpeningGen,omitempty"`
	CommittedNodeIDs    []string `json:"committedNodeIds,omitempty"`
	UpdatedAt           int64    `json:"updatedAt,omitempty"`
}

// GetHebi8RolloverState reads the persisted Hebi8 Fold-Rollover generation state.
func (s *Store) GetHebi8RolloverState(ctx context.Context) (Hebi8RolloverState, error) {
	var st Hebi8RolloverState
	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		st, err = loadHebi8RolloverStateTx(ctx, tx)
		return err
	})
	return st, err
}

func loadHebi8RolloverStateTx(ctx context.Context, tx *sql.Tx) (Hebi8RolloverState, error) {
	var st Hebi8RolloverState
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT value_json FROM hub_settings WHERE key = ?`, hebi8RolloverStateKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return Hebi8RolloverState{}, fmt.Errorf("decode %s: %w", hebi8RolloverStateKey, err)
	}
	return st, nil
}

// ListHebi8FoldedEventNodeIDs returns all segmentId and foldedId values recorded in committed
// hebi8.folded timeline events for taskID, used to reconstruct CommittedNodeIDs when upgrading
// legacy workspaces where Generation > 0 had no CommittedNodeIDs manifest yet.
func (s *Store) ListHebi8FoldedEventNodeIDs(ctx context.Context, taskID string) ([]string, error) {
	var ids []string
	seen := map[string]bool{}
	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT payload_json
			FROM events
			WHERE task_id = ? AND kind = ?
			ORDER BY seq ASC
		`, taskID, string(protocol.EventHebi8Folded))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			var p map[string]any
			if err := json.Unmarshal([]byte(raw), &p); err != nil {
				continue
			}
			for _, key := range []string{"segmentId", "foldedId"} {
				if v, ok := p[key].(string); ok {
					v = strings.TrimSpace(v)
					if v != "" && !seen[v] {
						seen[v] = true
						ids = append(ids, v)
					}
				}
			}
		}
		return rows.Err()
	})
	return ids, err
}

func saveHebi8RolloverStateTx(ctx context.Context, tx *sql.Tx, st Hebi8RolloverState, now int64) error {
	st.UpdatedAt = now
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO hub_settings (key, value_json, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json, updated_at = excluded.updated_at
	`, hebi8RolloverStateKey, string(raw), now)
	return err
}

func isHebi8MainTaskTx(ctx context.Context, tx *sql.Tx, taskID string) (bool, error) {
	var rawSetting string
	err := tx.QueryRowContext(ctx, `SELECT value_json FROM hub_settings WHERE key = ?`, hebi8MainTaskSettingKey).Scan(&rawSetting)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var doc struct {
		TaskID string `json:"taskId"`
	}
	if json.Unmarshal([]byte(rawSetting), &doc) != nil {
		return false, nil
	}
	return strings.TrimSpace(doc.TaskID) != "" && strings.TrimSpace(doc.TaskID) == taskID, nil
}

// GetHebi8MainTaskID returns the persisted main Hebi8 task ID if configured.
func (s *Store) GetHebi8MainTaskID(ctx context.Context) (string, error) {
	raw, ok, err := s.GetHubSetting(ctx, hebi8MainTaskSettingKey)
	if err != nil || !ok {
		return "", err
	}
	var doc struct {
		TaskID string `json:"taskId"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return "", nil
	}
	return strings.TrimSpace(doc.TaskID), nil
}

// EnsureHebi8MainTask returns the workspace's single Hebi8 main task (under virtual project _pa).
// Priority:
//  1. Existing non-archived _pa task pinned in hub_settings["hebi8_main_task_id"].
//  2. Most recently updated non-archived _pa task (adopted as main task and saved).
//  3. Newly initialized idle _pa task ready for immediate conversation.
func (s *Store) EnsureHebi8MainTask(ctx context.Context, preferredAdapter protocol.Adapter) (protocol.TaskSummary, *protocol.EventRecord, error) {
	var out protocol.TaskSummary
	var emitted *protocol.EventRecord
	now := time.Now().UnixMilli()
	if preferredAdapter == "" || !protocol.SessionAdapter(preferredAdapter).IsValid() {
		preferredAdapter = protocol.AdapterAgy
	}

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if err := ensureScratchProjectTx(ctx, tx, now); err != nil {
			return err
		}

		// 1. Check persisted hebi8_main_task_id
		var rawSetting string
		if err := tx.QueryRowContext(ctx, `SELECT value_json FROM hub_settings WHERE key = ?`, hebi8MainTaskSettingKey).Scan(&rawSetting); err == nil {
			var doc struct {
				TaskID string `json:"taskId"`
			}
			if json.Unmarshal([]byte(rawSetting), &doc) == nil && strings.TrimSpace(doc.TaskID) != "" {
				var tid string
				err := tx.QueryRowContext(ctx, `
					SELECT task_id FROM tasks
					WHERE task_id = ? AND project_id = ? AND archived_at IS NULL
				`, strings.TrimSpace(doc.TaskID), protocol.PAProjectID).Scan(&tid)
				if err == nil {
					task, loadErr := loadTaskSummaryTx(ctx, tx, tid)
					if loadErr != nil {
						return loadErr
					}
					out = task
					return nil
				}
			}
		}

		// 2. Adopt most recent non-archived _pa task if any exists
		var existingID string
		err := tx.QueryRowContext(ctx, `
			SELECT task_id FROM tasks
			WHERE project_id = ? AND archived_at IS NULL
			ORDER BY updated_at DESC, created_at DESC
			LIMIT 1
		`, protocol.PAProjectID).Scan(&existingID)
		if err == nil && existingID != "" {
			if err := saveHebi8MainTaskIDTx(ctx, tx, existingID, now); err != nil {
				return err
			}
			task, loadErr := loadTaskSummaryTx(ctx, tx, existingID)
			if loadErr != nil {
				return loadErr
			}
			out = task
			return nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		// 3. Create a clean idle Hebi8 main task (no initial attempt yet; ready to chat immediately)
		taskID := newID("tsk")
		cfgJSON, _ := json.Marshal(map[string]string{"mode": "auto"})
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tasks (
				task_id, project_id, parent_task_id, title, goal,
				required_capability, required_resources, preferred_adapter, desired_config_json,
				state, created_by, version, created_at, updated_at
			) VALUES (?, ?, NULL, ?, '', 'primary-dev', '[]', ?, ?, 'done', 'human', 1, ?, ?)
		`, taskID, protocol.PAProjectID, "Hebi8", string(preferredAdapter), string(cfgJSON), now, now); err != nil {
			return fmt.Errorf("insert hebi8 main task: %w", err)
		}

		if err := saveHebi8MainTaskIDTx(ctx, tx, taskID, now); err != nil {
			return err
		}

		ev, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + taskID,
			ProjectID:  protocol.PAProjectID,
			TaskID:     taskID,
			ActorKind:  protocol.ActorSystem,
			ActorID:    "hebi8",
			Kind:       protocol.EventTaskCreated,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"title":              "Hebi8",
				"goal":               "",
				"requiredCapability": "primary-dev",
				"adapter":            preferredAdapter,
				"hebi8Main":          true,
			},
			CreatedAt: now,
		})
		if err != nil {
			return err
		}
		emitted = &ev

		task, loadErr := loadTaskSummaryTx(ctx, tx, taskID)
		if loadErr != nil {
			return loadErr
		}
		out = task
		return nil
	})
	return out, emitted, err
}

func saveHebi8MainTaskIDTx(ctx context.Context, tx *sql.Tx, taskID string, now int64) error {
	raw, err := json.Marshal(map[string]any{
		"taskId":    taskID,
		"updatedAt": now,
	})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO hub_settings (key, value_json, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json, updated_at = excluded.updated_at
	`, hebi8MainTaskSettingKey, string(raw), now)
	return err
}

// LockTaskAgentSessions invalidates all existing agent_sessions for taskID so the very next
// attempt MUST open a brand-new underlying ACP session (NewSession instead of resume).
func (s *Store) LockTaskAgentSessions(ctx context.Context, taskID string) ([]AgentSessionRecord, error) {
	now := time.Now().UnixMilli()
	var closed []AgentSessionRecord
	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT wb_session_id, task_id, project_id, machine_id, adapter,
			       COALESCE(provider_session_id, ''), cwd, desired_config_json,
			       runtime_state, locked, last_activity_at, created_at
			FROM agent_sessions
			WHERE task_id = ? AND (locked = 0 OR runtime_state != 'dead' OR provider_session_id IS NOT NULL)
		`, taskID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var rec AgentSessionRecord
			var adp, rstate string
			var locked int
			if err := rows.Scan(
				&rec.WBSessionID, &rec.TaskID, &rec.ProjectID, &rec.MachineID, &adp,
				&rec.ProviderSessionID, &rec.CWD, &rec.DesiredConfigJSON,
				&rstate, &locked, &rec.LastActivityAt, &rec.CreatedAt,
			); err != nil {
				rows.Close()
				return err
			}
			rec.Adapter = protocol.SessionAdapter(adp)
			rec.RuntimeState = protocol.SessionRuntimeState(rstate)
			rec.Locked = locked != 0
			closed = append(closed, rec)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		if _, err := tx.ExecContext(ctx, `
			UPDATE agent_sessions
			SET locked = 1, runtime_state = 'dead', provider_session_id = NULL,
			    closed_at = COALESCE(closed_at, ?), last_activity_at = ?
			WHERE task_id = ?
		`, now, now, taskID); err != nil {
			return fmt.Errorf("lock agent_sessions for task %s: %w", taskID, err)
		}
		return nil
	})
	return closed, err
}

// EmitHebi8FoldedEvent appends a timeline divider event (hebi8.folded) to the Hebi8 main task stream.
func (s *Store) EmitHebi8FoldedEvent(ctx context.Context, taskID, endAttemptID string, payload map[string]any) (protocol.EventRecord, error) {
	var ev protocol.EventRecord
	now := time.Now().UnixMilli()
	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		ev, err = insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + taskID,
			ProjectID:  protocol.PAProjectID,
			TaskID:     taskID,
			AttemptID:  endAttemptID,
			ActorKind:  protocol.ActorSystem,
			ActorID:    "hebi8",
			Kind:       protocol.EventHebi8Folded,
			Visibility: protocol.VisibilityTimeline,
			Payload:    payload,
			CreatedAt:  now,
		})
		return err
	})
	return ev, err
}

// CommitHebi8FoldParams specifies the parameters for atomically committing one Fold-Rollover boundary.
type CommitHebi8FoldParams struct {
	TaskID           string
	SegmentID        string
	CommittedNodeIDs []string
	EndAttemptID     string
	EndAttemptNo     int
	ExpectedPrevNo   int
	EventPayload     map[string]any
	BeforeCommitHook func() error
}

// CommitHebi8FoldResult returns the committed rollover state, locked sessions to close on runners, and emitted event.
type CommitHebi8FoldResult struct {
	State          Hebi8RolloverState
	ClosedSessions []AgentSessionRecord
	Event          protocol.EventRecord
}

// CommitHebi8Fold atomically verifies task idleness & watermark, advances hebi8_rollover_state,
// locks/kills all prior agent_sessions, and appends hebi8.folded inside ONE BEGIN IMMEDIATE tx.
func (s *Store) CommitHebi8Fold(ctx context.Context, p CommitHebi8FoldParams) (CommitHebi8FoldResult, error) {
	var res CommitHebi8FoldResult
	now := time.Now().UnixMilli()
	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var projectID string
		var activeAtt sql.NullString
		var archivedAt sql.NullInt64
		err := tx.QueryRowContext(ctx, `
			SELECT project_id, active_attempt_id, archived_at
			FROM tasks WHERE task_id = ?
		`, p.TaskID).Scan(&projectID, &activeAtt, &archivedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: task %s", ErrNotFound, p.TaskID)
		}
		if err != nil {
			return err
		}
		if archivedAt.Valid {
			return fmt.Errorf("%w: task %s", ErrTaskArchived, p.TaskID)
		}
		if activeAtt.Valid && activeAtt.String != "" {
			return fmt.Errorf("%w: task %s has active attempt %s", ErrTaskActive, p.TaskID, activeAtt.String)
		}

		// Ensure no newer attempt was created concurrently after we snapshotted turns.
		var maxAttNo int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(attempt_no), 0) FROM attempts WHERE task_id = ?`, p.TaskID).Scan(&maxAttNo); err != nil {
			return err
		}
		if maxAttNo != p.EndAttemptNo {
			return fmt.Errorf("%w: concurrent attempt detected (expected maxAttemptNo=%d, got %d)", ErrConflict, p.EndAttemptNo, maxAttNo)
		}

		st, err := loadHebi8RolloverStateTx(ctx, tx)
		if err != nil {
			return err
		}
		if st.LastFoldedAttemptNo > p.ExpectedPrevNo || p.EndAttemptNo <= st.LastFoldedAttemptNo {
			return fmt.Errorf("%w: attempts up to #%d already folded", ErrConflict, st.LastFoldedAttemptNo)
		}

		// Lock and collect all live/unlocked agent_sessions in this SAME transaction.
		rows, err := tx.QueryContext(ctx, `
			SELECT wb_session_id, task_id, project_id, machine_id, adapter,
			       COALESCE(provider_session_id, ''), cwd, desired_config_json,
			       runtime_state, locked, last_activity_at, created_at
			FROM agent_sessions
			WHERE task_id = ? AND (locked = 0 OR runtime_state != 'dead' OR provider_session_id IS NOT NULL)
		`, p.TaskID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var rec AgentSessionRecord
			var adp, rstate string
			var locked int
			if err := rows.Scan(
				&rec.WBSessionID, &rec.TaskID, &rec.ProjectID, &rec.MachineID, &adp,
				&rec.ProviderSessionID, &rec.CWD, &rec.DesiredConfigJSON,
				&rstate, &locked, &rec.LastActivityAt, &rec.CreatedAt,
			); err != nil {
				rows.Close()
				return err
			}
			rec.Adapter = protocol.SessionAdapter(adp)
			rec.RuntimeState = protocol.SessionRuntimeState(rstate)
			rec.Locked = locked != 0
			res.ClosedSessions = append(res.ClosedSessions, rec)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		if _, err := tx.ExecContext(ctx, `
			UPDATE agent_sessions
			SET locked = 1, runtime_state = 'dead', provider_session_id = NULL,
			    closed_at = COALESCE(closed_at, ?), last_activity_at = ?
			WHERE task_id = ?
		`, now, now, p.TaskID); err != nil {
			return fmt.Errorf("lock agent_sessions for task %s: %w", p.TaskID, err)
		}

		st.Generation++
		st.LastFoldedAttemptNo = p.EndAttemptNo
		st.LastSegmentID = p.SegmentID
		st.PendingOpeningGen = st.Generation
		committedSet := make(map[string]bool, len(st.CommittedNodeIDs)+len(p.CommittedNodeIDs)+1)
		for _, id := range st.CommittedNodeIDs {
			if id != "" {
				committedSet[id] = true
			}
		}
		for _, id := range p.CommittedNodeIDs {
			id = strings.TrimSpace(id)
			if id != "" && !committedSet[id] {
				committedSet[id] = true
				st.CommittedNodeIDs = append(st.CommittedNodeIDs, id)
			}
		}
		if segID := strings.TrimSpace(p.SegmentID); segID != "" && !committedSet[segID] {
			st.CommittedNodeIDs = append(st.CommittedNodeIDs, segID)
		}
		if err := saveHebi8RolloverStateTx(ctx, tx, st, now); err != nil {
			return err
		}

		payload := cloneMap(p.EventPayload)
		payload["generation"] = st.Generation
		ev, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + p.TaskID,
			ProjectID:  projectID,
			TaskID:     p.TaskID,
			AttemptID:  p.EndAttemptID,
			ActorKind:  protocol.ActorSystem,
			ActorID:    "hebi8",
			Kind:       protocol.EventHebi8Folded,
			Visibility: protocol.VisibilityTimeline,
			Payload:    payload,
			CreatedAt:  now,
		})
		if err != nil {
			return err
		}

		if p.BeforeCommitHook != nil {
			if hookErr := p.BeforeCommitHook(); hookErr != nil {
				return hookErr
			}
		}

		res.State = st
		res.Event = ev
		return nil
	})
	return res, err
}

// RawAttemptPromptRow holds the full, untruncated user prompt extracted from attempts.trigger_payload_json.
type RawAttemptPromptRow struct {
	AttemptID   string
	AttemptNo   int
	TriggerKind protocol.TriggerKind
	State       protocol.AttemptState
	UserPrompt  string
	StartedAt   int64
	FinishedAt  *int64
}

// ListTaskAttemptsFullPrompts returns terminal attempts in [minAttemptNo, maxAttemptNo] with FULL
// untruncated user prompts from attempts.trigger_payload_json (and tasks.goal fallback for attempt #1).
func (s *Store) ListTaskAttemptsFullPrompts(ctx context.Context, taskID string, minAttemptNo, maxAttemptNo int) ([]RawAttemptPromptRow, error) {
	var out []RawAttemptPromptRow
	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		var taskGoal string
		if err := tx.QueryRowContext(ctx, `SELECT goal FROM tasks WHERE task_id = ?`, taskID).Scan(&taskGoal); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: task %s", ErrNotFound, taskID)
			}
			return err
		}
		var sb strings.Builder
		sb.WriteString(`
			SELECT attempt_id, attempt_no, trigger_kind, state, trigger_payload_json, started_at, finished_at
			FROM attempts
			WHERE task_id = ?
		`)
		args := []any{taskID}
		if minAttemptNo > 0 {
			sb.WriteString(` AND attempt_no >= ?`)
			args = append(args, minAttemptNo)
		}
		if maxAttemptNo > 0 {
			sb.WriteString(` AND attempt_no <= ?`)
			args = append(args, maxAttemptNo)
		}
		sb.WriteString(` ORDER BY attempt_no ASC`)

		rows, err := tx.QueryContext(ctx, sb.String(), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r RawAttemptPromptRow
			var trigStr, stStr, tpJSON string
			var finAt sql.NullInt64
			if err := rows.Scan(&r.AttemptID, &r.AttemptNo, &trigStr, &stStr, &tpJSON, &r.StartedAt, &finAt); err != nil {
				return err
			}
			r.TriggerKind = protocol.TriggerKind(trigStr)
			r.State = protocol.AttemptState(stStr)
			if finAt.Valid {
				v := finAt.Int64
				r.FinishedAt = &v
			}
			prompt := extractFullPromptText(tpJSON)
			if prompt == "" && r.AttemptNo == 1 {
				prompt = strings.TrimSpace(taskGoal)
			}
			r.UserPrompt = prompt
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

func extractFullPromptText(triggerPayloadJSON string) string {
	if strings.TrimSpace(triggerPayloadJSON) == "" {
		return ""
	}
	var tp map[string]any
	if err := json.Unmarshal([]byte(triggerPayloadJSON), &tp); err != nil {
		return ""
	}
	if p, ok := tp["prompt"].(string); ok && strings.TrimSpace(p) != "" {
		return strings.TrimSpace(p)
	}
	if g, ok := tp["goal"].(string); ok && strings.TrimSpace(g) != "" {
		return strings.TrimSpace(g)
	}
	return ""
}
