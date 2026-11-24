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

func scanPAThread(scanner interface{ Scan(...any) error }) (protocol.PAThread, error) {
	var t protocol.PAThread
	var wbSess sql.NullString
	var status string
	if err := scanner.Scan(&t.PAThreadID, &t.Title, &wbSess, &status, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return t, err
	}
	t.Status = protocol.PAThreadStatus(status)
	if wbSess.Valid {
		t.WBSessionID = wbSess.String
	}
	return t, nil
}

func scanPAMessage(scanner interface{ Scan(...any) error }) (protocol.PAMessage, error) {
	var m protocol.PAMessage
	var role, actionsJSON string
	if err := scanner.Scan(
		&m.MessageID, &m.PAThreadID, &role, &m.Content,
		&m.DeltaFromSeq, &m.DeltaToSeq, &actionsJSON, &m.CreatedAt,
	); err != nil {
		return m, err
	}
	m.Role = protocol.PAMessageRole(role)
	if actionsJSON != "" && actionsJSON != "[]" {
		_ = json.Unmarshal([]byte(actionsJSON), &m.Actions)
	}
	return m, nil
}

func queryPAStateTx(ctx context.Context, tx *sql.Tx, paID string) (protocol.PAStateView, error) {
	if paID == "" {
		paID = "default"
	}
	var st protocol.PAStateView
	var activeSess, activeThread sql.NullString
	var focusJSON string
	err := tx.QueryRowContext(ctx, `
		SELECT pa_id, active_pa_thread_id, active_wb_session_id, last_read_event_seq, focus_pointers_json, updated_at
		FROM pa_state WHERE pa_id = ?
	`, paID).Scan(&st.PAID, &activeThread, &activeSess, &st.LastReadEventSeq, &focusJSON, &st.UpdatedAt)
	if err != nil {
		return st, err
	}
	if activeThread.Valid {
		st.ActivePAThreadID = activeThread.String
	}
	if activeSess.Valid {
		st.ActiveWBSessionID = activeSess.String
	}
	st.FocusPointers = []protocol.PAFocusPointer{}
	if focusJSON != "" {
		_ = json.Unmarshal([]byte(focusJSON), &st.FocusPointers)
	}
	return st, nil
}

// GetPAState returns the global PA watermark and focus pointers.
func (s *Store) GetPAState(ctx context.Context, paID string) (protocol.PAStateView, error) {
	var out protocol.PAStateView
	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = queryPAStateTx(ctx, tx, paID)
		return err
	})
	return out, err
}

// ListPAThreads lists PA conversation threads ordered by recent activity.
func (s *Store) ListPAThreads(ctx context.Context) ([]protocol.PAThread, error) {
	out := []protocol.PAThread{}
	rows, err := s.ro.QueryContext(ctx, `
		SELECT pa_thread_id, title, wb_session_id, status, created_at, updated_at
		FROM pa_threads
		ORDER BY updated_at DESC
		LIMIT 100
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		pt, err := scanPAThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, pt)
	}
	return out, rows.Err()
}

// CreatePAThread creates a new PA thread, sets it as active_pa_thread_id, and preserves focus_pointers_json.
func (s *Store) CreatePAThread(ctx context.Context, paID, title string) (protocol.PAThread, protocol.PAStateView, *protocol.EventRecord, error) {
	if paID == "" {
		paID = "default"
	}
	title = strings.TrimSpace(title)
	now := time.Now().UnixMilli()
	if title == "" {
		title = "PA 对话 · " + time.UnixMilli(now).Format("01-02 15:04")
	}

	var thread protocol.PAThread
	var state protocol.PAStateView
	var emitted *protocol.EventRecord

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		tid := newID("pat")
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO pa_threads (pa_thread_id, title, status, created_at, updated_at)
			VALUES (?, ?, 'active', ?, ?)
		`, tid, title, now, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE pa_state SET active_pa_thread_id = ?, updated_at = ? WHERE pa_id = ?
		`, tid, now, paID); err != nil {
			return err
		}

		ev, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "global",
			ActorKind:  protocol.ActorHuman,
			ActorID:    "human",
			Kind:       protocol.EventPAThreadCreated,
			Visibility: protocol.VisibilityAudit,
			Payload: map[string]any{
				"paThreadId": tid,
				"title":      title,
			},
			CreatedAt: now,
		})
		if err != nil {
			return err
		}
		emitted = &ev

		thread = protocol.PAThread{
			PAThreadID: tid,
			Title:      title,
			Status:     protocol.PAThreadStatus("active"),
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		state, err = queryPAStateTx(ctx, tx, paID)
		return err
	})
	return thread, state, emitted, err
}

// GetPAThreadDetail returns one PA thread with all its messages and the current PAState.
func (s *Store) GetPAThreadDetail(ctx context.Context, paID, threadID string) (protocol.PAThreadDetailResponse, error) {
	resp := protocol.PAThreadDetailResponse{
		Messages: []protocol.PAMessage{},
	}
	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `
			SELECT pa_thread_id, title, wb_session_id, status, created_at, updated_at
			FROM pa_threads WHERE pa_thread_id = ?
		`, threadID)
		pt, err := scanPAThread(row)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: pa thread %s", ErrNotFound, threadID)
		}
		if err != nil {
			return err
		}
		resp.Thread = pt

		mRows, err := tx.QueryContext(ctx, `
			SELECT message_id, pa_thread_id, role, content, delta_from_seq, delta_to_seq, actions_json, created_at
			FROM pa_messages
			WHERE pa_thread_id = ?
			ORDER BY created_at ASC
		`, threadID)
		if err != nil {
			return err
		}
		defer mRows.Close()
		for mRows.Next() {
			msg, err := scanPAMessage(mRows)
			if err != nil {
				return err
			}
			resp.Messages = append(resp.Messages, msg)
		}
		if err := mRows.Err(); err != nil {
			return err
		}

		st, err := queryPAStateTx(ctx, tx, paID)
		if err != nil {
			return err
		}
		resp.PAState = st
		return nil
	})
	return resp, err
}

// PADeltaSnapshot captures the locked watermark [lastSeq, highSeq] and deterministic events
// in a single read-only SQLite snapshot so concurrent events at H+1 are NEVER skipped.
type PADeltaSnapshot struct {
	PAState protocol.PAStateView
	FromSeq int64
	HighSeq int64
	Events  []protocol.EventRecord
}

// ReadPADeltaSnapshot reads pa_state.last_read_event_seq, locks H = MAX(seq), and fetches
// deterministic events in (lastSeq, H] with visibility IN ('timeline', 'inbox', 'audit').
func (s *Store) ReadPADeltaSnapshot(ctx context.Context, paID string) (PADeltaSnapshot, error) {
	var snap PADeltaSnapshot
	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		st, err := queryPAStateTx(ctx, tx, paID)
		if err != nil {
			return err
		}
		snap.PAState = st
		snap.FromSeq = st.LastReadEventSeq

		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM events`).Scan(&snap.HighSeq); err != nil {
			return err
		}

		if snap.HighSeq <= snap.FromSeq {
			snap.Events = []protocol.EventRecord{}
			return nil
		}

		rows, err := tx.QueryContext(ctx, `
			SELECT seq, event_id, stream_id, project_id, task_id, attempt_id,
			       actor_kind, actor_id, kind, visibility, payload_json, created_at
			FROM events
			WHERE seq > ? AND seq <= ? AND visibility IN ('timeline', 'inbox', 'audit')
			ORDER BY seq ASC
		`, snap.FromSeq, snap.HighSeq)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			ev, err := scanEventRecord(rows)
			if err != nil {
				return err
			}
			snap.Events = append(snap.Events, ev)
		}
		return rows.Err()
	})
	return snap, err
}

// CommitPATurnParams contains the completed PA turn messages and locked watermark H.
type CommitPATurnParams struct {
	PAID          string
	ThreadID      string
	UserText      string
	AssistantText string
	DeltaFromSeq  int64
	DeltaToSeq    int64
	Actions       []protocol.PAActionResult
	FocusPointers []protocol.PAFocusPointer
}

// CommitPATurnResult holds persisted messages, updated PAState, and emitted audit event.
type CommitPATurnResult struct {
	Thread           protocol.PAThread
	UserMessage      protocol.PAMessage
	AssistantMessage protocol.PAMessage
	PAState          protocol.PAStateView
	Events           []protocol.EventRecord
}

// CommitPATurn atomically persists user + assistant PA messages, advances pa_state.last_read_event_seq
// strictly to DeltaToSeq (H), and emits a pa.turn audit event.
func (s *Store) CommitPATurn(ctx context.Context, p CommitPATurnParams) (CommitPATurnResult, error) {
	var res CommitPATurnResult
	if p.PAID == "" {
		p.PAID = "default"
	}
	now := time.Now().UnixMilli()

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		threadID := strings.TrimSpace(p.ThreadID)
		if threadID == "" {
			st, err := queryPAStateTx(ctx, tx, p.PAID)
			if err != nil {
				return err
			}
			threadID = st.ActivePAThreadID
		}
		if threadID == "" {
			threadID = newID("pat")
			title := strings.TrimSpace(p.UserText)
			if len([]rune(title)) > 48 {
				title = string([]rune(title)[:48])
			}
			if title == "" {
				title = "PA 对话 · " + time.UnixMilli(now).Format("01-02 15:04")
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO pa_threads (pa_thread_id, title, status, created_at, updated_at)
				VALUES (?, ?, 'active', ?, ?)
			`, threadID, title, now, now); err != nil {
				return err
			}
		} else {
			var exists string
			if err := tx.QueryRowContext(ctx, `SELECT pa_thread_id FROM pa_threads WHERE pa_thread_id = ?`, threadID).Scan(&exists); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("%w: pa thread %s", ErrNotFound, threadID)
				}
				return err
			}
		}

		userMsgID := newID("pam")
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO pa_messages (message_id, pa_thread_id, role, content, delta_from_seq, delta_to_seq, actions_json, created_at)
			VALUES (?, ?, 'user', ?, ?, ?, '[]', ?)
		`, userMsgID, threadID, p.UserText, p.DeltaFromSeq, p.DeltaToSeq, now); err != nil {
			return err
		}

		actionsBytes, err := json.Marshal(p.Actions)
		if err != nil {
			return err
		}
		if p.Actions == nil {
			actionsBytes = []byte("[]")
		}
		asstMsgID := newID("pam")
		asstNow := now + 1
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO pa_messages (message_id, pa_thread_id, role, content, delta_from_seq, delta_to_seq, actions_json, created_at)
			VALUES (?, ?, 'assistant', ?, ?, ?, ?, ?)
		`, asstMsgID, threadID, p.AssistantText, p.DeltaFromSeq, p.DeltaToSeq, string(actionsBytes), asstNow); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE pa_threads SET updated_at = ? WHERE pa_thread_id = ?
		`, asstNow, threadID); err != nil {
			return err
		}

		if p.FocusPointers != nil {
			if len(p.FocusPointers) > 20 {
				p.FocusPointers = p.FocusPointers[:20]
			}
			fpBytes, err := json.Marshal(p.FocusPointers)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `
				UPDATE pa_state
				SET active_pa_thread_id = ?, last_read_event_seq = MAX(last_read_event_seq, ?), focus_pointers_json = ?, updated_at = ?
				WHERE pa_id = ?
			`, threadID, p.DeltaToSeq, string(fpBytes), asstNow, p.PAID)
			if err != nil {
				return err
			}
		} else {
			_, err = tx.ExecContext(ctx, `
				UPDATE pa_state
				SET active_pa_thread_id = ?, last_read_event_seq = MAX(last_read_event_seq, ?), updated_at = ?
				WHERE pa_id = ?
			`, threadID, p.DeltaToSeq, asstNow, p.PAID)
			if err != nil {
				return err
			}
		}

		ev, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "global",
			ActorKind:  protocol.ActorPA,
			ActorID:    "pa:" + p.PAID,
			Kind:       protocol.EventPATurn,
			Visibility: protocol.VisibilityAudit,
			Payload: map[string]any{
				"paThreadId":   threadID,
				"deltaFromSeq": p.DeltaFromSeq,
				"deltaToSeq":   p.DeltaToSeq,
				"actionCount":  len(p.Actions),
			},
			CreatedAt: asstNow,
		})
		if err != nil {
			return err
		}
		res.Events = append(res.Events, ev)

		tRow := tx.QueryRowContext(ctx, `
			SELECT pa_thread_id, title, wb_session_id, status, created_at, updated_at
			FROM pa_threads WHERE pa_thread_id = ?
		`, threadID)
		res.Thread, err = scanPAThread(tRow)
		if err != nil {
			return err
		}
		res.UserMessage = protocol.PAMessage{
			MessageID:    userMsgID,
			PAThreadID:   threadID,
			Role:         protocol.PARoleUser,
			Content:      p.UserText,
			DeltaFromSeq: p.DeltaFromSeq,
			DeltaToSeq:   p.DeltaToSeq,
			CreatedAt:    now,
		}
		res.AssistantMessage = protocol.PAMessage{
			MessageID:    asstMsgID,
			PAThreadID:   threadID,
			Role:         protocol.PARoleAssistant,
			Content:      p.AssistantText,
			DeltaFromSeq: p.DeltaFromSeq,
			DeltaToSeq:   p.DeltaToSeq,
			Actions:      p.Actions,
			CreatedAt:    asstNow,
		}
		res.PAState, err = queryPAStateTx(ctx, tx, p.PAID)
		return err
	})
	return res, err
}

// BuildPABrief aggregates inbox_items, active/blocked tasks, recent deliveries, machines,
// and recent non-raw events in a single read-only SQLite snapshot (<10ms, zero mutation).
func (s *Store) BuildPABrief(
	ctx context.Context,
	sinceHours int,
	projectID string,
	machineMeta map[string]protocol.MachineConfig,
) (protocol.PABriefResponse, error) {
	if sinceHours <= 0 {
		sinceHours = 24
	}
	now := time.Now().UnixMilli()
	cutoff := now - int64(sinceHours)*int64(time.Hour/time.Millisecond)

	resp := protocol.PABriefResponse{
		GeneratedAt:      now,
		SinceHours:       sinceHours,
		ProjectID:        projectID,
		OpenInboxItems:   []protocol.InboxItemView{},
		ActiveTasks:      []protocol.TaskSummary{},
		BlockedTasks:     []protocol.TaskSummary{},
		RecentDeliveries: []protocol.DeliveryView{},
		Machines:         []protocol.MachineView{},
		RecentEvents:     []protocol.EventRecord{},
	}

	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM events`).Scan(&resp.SnapshotSeq); err != nil {
			return err
		}

		// Open inbox items
		var iRows *sql.Rows
		var err error
		if projectID != "" {
			iRows, err = tx.QueryContext(ctx, `
				SELECT inbox_id, project_id, task_id, category, ref_id, title, summary,
				       priority, state, created_at, updated_at, resolved_at
				FROM inbox_items WHERE state = 'open' AND project_id = ?
				ORDER BY priority ASC, updated_at DESC LIMIT 50
			`, projectID)
		} else {
			iRows, err = tx.QueryContext(ctx, `
				SELECT inbox_id, project_id, task_id, category, ref_id, title, summary,
				       priority, state, created_at, updated_at, resolved_at
				FROM inbox_items WHERE state = 'open'
				ORDER BY priority ASC, updated_at DESC LIMIT 50
			`)
		}
		if err != nil {
			return err
		}
		defer iRows.Close()
		for iRows.Next() {
			it, err := scanInboxItem(iRows)
			if err != nil {
				return err
			}
			resp.OpenInboxItems = append(resp.OpenInboxItems, it)
		}
		if err := iRows.Err(); err != nil {
			return err
		}
		resp.OpenInboxCount = len(resp.OpenInboxItems)

		// Active & blocked tasks
		var tRows *sql.Rows
		if projectID != "" {
			tRows, err = tx.QueryContext(ctx, taskSummarySelectSQL+`
				WHERE t.project_id = ? AND t.archived_at IS NULL AND t.state NOT IN ('done', 'cancelled')
				ORDER BY t.updated_at DESC LIMIT 50
			`, projectID)
		} else {
			tRows, err = tx.QueryContext(ctx, taskSummarySelectSQL+`
				WHERE t.archived_at IS NULL AND t.state NOT IN ('done', 'cancelled')
				ORDER BY t.updated_at DESC LIMIT 50
			`)
		}
		if err != nil {
			return err
		}
		defer tRows.Close()
		for tRows.Next() {
			ts, err := scanTaskSummary(tRows)
			if err != nil {
				return err
			}
			if ts.State == protocol.TaskStateWaitingResource || ts.State == protocol.TaskStateFailed || ts.State == protocol.TaskStateWaitingApproval {
				resp.BlockedTasks = append(resp.BlockedTasks, ts)
			}
			resp.ActiveTasks = append(resp.ActiveTasks, ts)
		}
		if err := tRows.Err(); err != nil {
			return err
		}

		// Recent deliveries (excluding late_arrival archive rows)
		var dRows *sql.Rows
		if projectID != "" {
			dRows, err = tx.QueryContext(ctx, `
				SELECT d.delivery_id, d.project_id, d.task_id, d.attempt_id, d.source_machine_id,
				       d.repo_identity, d.base_sha, d.head_sha, d.git_ref, d.kind, d.summary,
				       d.manifest_json, d.frozen_snapshot_dir, d.ryugi_sync_state,
				       d.late_arrival, d.pinned, d.created_at,
				       (SELECT COUNT(*) FROM annotations an WHERE an.delivery_id = d.delivery_id AND an.status = 'open') AS open_ann
				FROM deliveries d
				WHERE d.project_id = ? AND d.late_arrival = 0 AND d.created_at >= ?
				ORDER BY d.created_at DESC LIMIT 20
			`, projectID, cutoff)
		} else {
			dRows, err = tx.QueryContext(ctx, `
				SELECT d.delivery_id, d.project_id, d.task_id, d.attempt_id, d.source_machine_id,
				       d.repo_identity, d.base_sha, d.head_sha, d.git_ref, d.kind, d.summary,
				       d.manifest_json, d.frozen_snapshot_dir, d.ryugi_sync_state,
				       d.late_arrival, d.pinned, d.created_at,
				       (SELECT COUNT(*) FROM annotations an WHERE an.delivery_id = d.delivery_id AND an.status = 'open') AS open_ann
				FROM deliveries d
				WHERE d.late_arrival = 0 AND d.created_at >= ?
				ORDER BY d.created_at DESC LIMIT 20
			`, cutoff)
		}
		if err != nil {
			return err
		}
		defer dRows.Close()
		for dRows.Next() {
			dlv, err := scanDeliveryView(dRows)
			if err != nil {
				return err
			}
			resp.RecentDeliveries = append(resp.RecentDeliveries, dlv)
		}
		if err := dRows.Err(); err != nil {
			return err
		}

		// Machines
		mRows, err := tx.QueryContext(ctx, `
			SELECT machine_id, transport_mode, status, capabilities_json, resources_json,
			       runner_version, last_seen_at, updated_at
			FROM machines ORDER BY machine_id ASC
		`)
		if err != nil {
			return err
		}
		defer mRows.Close()
		for mRows.Next() {
			var mv protocol.MachineView
			var transport, status, capsJSON, resJSON string
			var runnerVer sql.NullString
			var lastSeen sql.NullInt64
			if err := mRows.Scan(
				&mv.MachineID, &transport, &status, &capsJSON, &resJSON,
				&runnerVer, &lastSeen, &mv.UpdatedAt,
			); err != nil {
				return err
			}
			mv.TransportMode = protocol.TransportMode(transport)
			mv.Status = protocol.MachineStatus(status)
			mv.DisplayName = mv.MachineID
			if cfg, ok := machineMeta[mv.MachineID]; ok && cfg.DisplayName != "" {
				mv.DisplayName = cfg.DisplayName
			}
			if runnerVer.Valid {
				mv.RunnerVersion = runnerVer.String
			}
			if lastSeen.Valid {
				v := lastSeen.Int64
				mv.LastSeenAt = &v
			}
			_ = json.Unmarshal([]byte(capsJSON), &mv.Capabilities)
			if mv.Capabilities == nil {
				mv.Capabilities = []string{}
			}
			_ = json.Unmarshal([]byte(resJSON), &mv.Resources)
			if mv.Resources == nil {
				mv.Resources = map[string]protocol.ResourceState{}
			}
			resp.Machines = append(resp.Machines, mv)
		}
		if err := mRows.Err(); err != nil {
			return err
		}

		// Recent timeline/inbox events
		var eRows *sql.Rows
		if projectID != "" {
			eRows, err = tx.QueryContext(ctx, `
				SELECT seq, event_id, stream_id, project_id, task_id, attempt_id,
				       actor_kind, actor_id, kind, visibility, payload_json, created_at
				FROM events
				WHERE project_id = ? AND visibility IN ('timeline', 'inbox') AND created_at >= ?
				ORDER BY seq DESC LIMIT 30
			`, projectID, cutoff)
		} else {
			eRows, err = tx.QueryContext(ctx, `
				SELECT seq, event_id, stream_id, project_id, task_id, attempt_id,
				       actor_kind, actor_id, kind, visibility, payload_json, created_at
				FROM events
				WHERE visibility IN ('timeline', 'inbox') AND created_at >= ?
				ORDER BY seq DESC LIMIT 30
			`, cutoff)
		}
		if err != nil {
			return err
		}
		defer eRows.Close()
		for eRows.Next() {
			ev, err := scanEventRecord(eRows)
			if err != nil {
				return err
			}
			resp.RecentEvents = append(resp.RecentEvents, ev)
		}
		return eRows.Err()
	})
	if err != nil {
		return resp, err
	}

	resp.Summary = formatBriefSummary(resp)
	return resp, nil
}

func formatBriefSummary(b protocol.PABriefResponse) string {
	var parts []string
	onlineCount := 0
	for _, m := range b.Machines {
		if m.Status == protocol.MachineOnline {
			onlineCount++
		}
	}
	parts = append(parts, fmt.Sprintf("事件水位 #%d · 在线节点 %d/%d", b.SnapshotSeq, onlineCount, len(b.Machines)))
	parts = append(parts, fmt.Sprintf("待拍板 %d 项 · 活跃线程 %d 个（其中阻塞/待批 %d）", b.OpenInboxCount, len(b.ActiveTasks), len(b.BlockedTasks)))
	if len(b.RecentDeliveries) > 0 {
		parts = append(parts, fmt.Sprintf("近 %dh 产出 %d 个新交付", b.SinceHours, len(b.RecentDeliveries)))
	}
	return strings.Join(parts, "；")
}
