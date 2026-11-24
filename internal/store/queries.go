package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

// SyncRegistry synchronizes static projects.yaml and machines.yaml into SQLite
// inside a single BEGIN IMMEDIATE write transaction.
func (s *Store) SyncRegistry(ctx context.Context, dataDir string, projects protocol.ProjectsConfig, machines protocol.MachinesConfig) error {
	now := time.Now().UnixMilli()
	return s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		for machineID, m := range machines.Machines {
			caps := m.Capabilities
			if caps == nil {
				caps = []string{}
			}
			capsJSON, err := json.Marshal(caps)
			if err != nil {
				return err
			}
			initialStatus := protocol.MachineOffline
			if m.Transport == protocol.TransportLocalWS || m.Transport == protocol.TransportSSHOutbound {
				initialStatus = protocol.MachineOnline
			}
			_, err = tx.ExecContext(ctx, `
				INSERT INTO machines (machine_id, transport_mode, status, capabilities_json, resources_json, last_seen_at, updated_at)
				VALUES (?, ?, ?, ?, '{}', ?, ?)
				ON CONFLICT(machine_id) DO UPDATE SET
					transport_mode = excluded.transport_mode,
					capabilities_json = excluded.capabilities_json,
					updated_at = excluded.updated_at
			`, machineID, string(m.Transport), string(initialStatus), string(capsJSON), now, now)
			if err != nil {
				return fmt.Errorf("upsert machine %s: %w", machineID, err)
			}
		}

		for projectID, p := range projects.Projects {
			if projectID == protocol.ScratchProjectID {
				continue
			}
			branch := p.DefaultBranch
			if branch == "" {
				branch = "main"
			}
			mirrorPath := filepath.Join(dataDir, "git", projectID+".git")
			cfgJSON, err := json.Marshal(p)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `
				INSERT INTO projects (project_id, display_name, repo_identity, default_branch, mirror_bare_path, config_json, archived, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, 0, ?)
				ON CONFLICT(project_id) DO UPDATE SET
					display_name = excluded.display_name,
					repo_identity = excluded.repo_identity,
					default_branch = excluded.default_branch,
					mirror_bare_path = excluded.mirror_bare_path,
					config_json = excluded.config_json,
					archived = 0,
					updated_at = excluded.updated_at
			`, projectID, p.DisplayName, p.RepoIdentity, branch, mirrorPath, string(cfgJSON), now)
			if err != nil {
				return fmt.Errorf("upsert project %s: %w", projectID, err)
			}
		}
		if err := ensureScratchProjectTx(ctx, tx, now); err != nil {
			return err
		}
		return nil
	})
}

// Bootstrap returns projects, machines, open inbox items, active tasks, PA threads, PA state,
// and snapshotSeq from a single consistent read-only SQLite transaction.
func (s *Store) Bootstrap(ctx context.Context, machineMeta map[string]protocol.MachineConfig) (protocol.BootstrapResponse, error) {
	resp := protocol.BootstrapResponse{
		Projects:       []protocol.ProjectView{},
		Machines:       []protocol.MachineView{},
		OpenInboxItems: []protocol.InboxItemView{},
		ActiveTasks:    []protocol.TaskSummary{},
		PAThreads:      []protocol.PAThread{},
		PAState: protocol.PAStateView{
			PAID:          "default",
			FocusPointers: []protocol.PAFocusPointer{},
		},
	}

	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM events`).Scan(&resp.SnapshotSeq); err != nil {
			return fmt.Errorf("query snapshotSeq: %w", err)
		}

		// Projects with live active & blocked counts (real projects first, _scratch last)
		rows, err := tx.QueryContext(ctx, `
			SELECT p.project_id, p.display_name, p.repo_identity, p.default_branch,
			       p.mirror_bare_path, p.config_json, p.archived, p.updated_at,
			       COALESCE(SUM(CASE WHEN t.state IN ('queued','running','waiting_approval','in_review') THEN 1 ELSE 0 END), 0) AS active_count,
			       COALESCE(SUM(CASE WHEN t.state IN ('waiting_resource','failed') THEN 1 ELSE 0 END), 0) AS blocked_count
			FROM projects p
			LEFT JOIN tasks t ON t.project_id = p.project_id AND t.archived_at IS NULL
			WHERE p.archived = 0
			GROUP BY p.project_id
			ORDER BY CASE WHEN p.project_id = '_scratch' THEN 1 ELSE 0 END, p.display_name ASC
		`)
		if err != nil {
			return fmt.Errorf("query projects: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var pv protocol.ProjectView
			var cfgJSON string
			var archived int
			if err := rows.Scan(
				&pv.ProjectID, &pv.DisplayName, &pv.RepoIdentity, &pv.DefaultBranch,
				&pv.MirrorBarePath, &cfgJSON, &archived, &pv.UpdatedAt,
				&pv.ActiveTaskCount, &pv.BlockedTaskCount,
			); err != nil {
				return err
			}
			pv.Archived = archived != 0
			var cfg protocol.ProjectConfig
			if err := json.Unmarshal([]byte(cfgJSON), &cfg); err == nil {
				pv.DefaultCapability = cfg.DefaultCapability
				pv.DefaultAdapter = cfg.DefaultAdapter
				pv.IDEScheme = cfg.IDEScheme
				pv.Anchors = cfg.Anchors
			}
			if pv.Anchors == nil {
				pv.Anchors = map[string]protocol.ProjectAnchorConfig{}
			}
			if pv.IDEScheme == "" {
				pv.IDEScheme = protocol.IDECursor
			}
			resp.Projects = append(resp.Projects, pv)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		// Machines
		mRows, err := tx.QueryContext(ctx, `
			SELECT machine_id, transport_mode, status, capabilities_json, resources_json,
			       runner_version, last_seen_at, updated_at
			FROM machines
			ORDER BY machine_id ASC
		`)
		if err != nil {
			return fmt.Errorf("query machines: %w", err)
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
			if cfg, ok := machineMeta[mv.MachineID]; ok {
				if cfg.DisplayName != "" {
					mv.DisplayName = cfg.DisplayName
				}
				mv.TailscaleHost = cfg.TailscaleHost
				mv.SSHAlias = cfg.SSHAlias
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

		// Open Inbox Items
		iRows, err := tx.QueryContext(ctx, `
			SELECT inbox_id, project_id, task_id, category, ref_id, title, summary,
			       priority, state, created_at, updated_at, resolved_at
			FROM inbox_items
			WHERE state = 'open'
			ORDER BY priority ASC, updated_at DESC
		`)
		if err != nil {
			return fmt.Errorf("query inbox_items: %w", err)
		}
		defer iRows.Close()

		for iRows.Next() {
			item, err := scanInboxItem(iRows)
			if err != nil {
				return err
			}
			resp.OpenInboxItems = append(resp.OpenInboxItems, item)
		}
		if err := iRows.Err(); err != nil {
			return err
		}

		// Active Tasks across all projects (for Home overview)
		tRows, err := tx.QueryContext(ctx, taskSummarySelectSQL+`
			WHERE t.archived_at IS NULL AND t.state NOT IN ('done', 'cancelled')
			ORDER BY t.updated_at DESC
			LIMIT 100
		`)
		if err != nil {
			return fmt.Errorf("query active tasks: %w", err)
		}
		defer tRows.Close()
		for tRows.Next() {
			ts, err := scanTaskSummary(tRows)
			if err != nil {
				return err
			}
			resp.ActiveTasks = append(resp.ActiveTasks, ts)
		}
		if err := tRows.Err(); err != nil {
			return err
		}

		// PA Threads
		ptRows, err := tx.QueryContext(ctx, `
			SELECT pa_thread_id, title, wb_session_id, status, created_at, updated_at
			FROM pa_threads
			WHERE status = 'active'
			ORDER BY updated_at DESC
			LIMIT 50
		`)
		if err != nil {
			return fmt.Errorf("query pa_threads: %w", err)
		}
		defer ptRows.Close()
		for ptRows.Next() {
			pt, err := scanPAThread(ptRows)
			if err != nil {
				return err
			}
			resp.PAThreads = append(resp.PAThreads, pt)
		}
		if err := ptRows.Err(); err != nil {
			return err
		}

		// PA State
		paState, err := queryPAStateTx(ctx, tx, "default")
		if err == nil {
			resp.PAState = paState
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("query pa_state: %w", err)
		}
		return nil
	})
	return resp, err
}

func scanInboxItem(scanner interface{ Scan(...any) error }) (protocol.InboxItemView, error) {
	var item protocol.InboxItemView
	var taskID sql.NullString
	var cat, state string
	var resolvedAt sql.NullInt64
	if err := scanner.Scan(
		&item.InboxID, &item.ProjectID, &taskID, &cat, &item.RefID,
		&item.Title, &item.Summary, &item.Priority, &state,
		&item.CreatedAt, &item.UpdatedAt, &resolvedAt,
	); err != nil {
		return item, err
	}
	if taskID.Valid {
		item.TaskID = taskID.String
	}
	item.Category = protocol.InboxCategory(cat)
	item.State = protocol.InboxState(state)
	if resolvedAt.Valid {
		v := resolvedAt.Int64
		item.ResolvedAt = &v
	}
	return item, nil
}

// GetProject returns a single project view by ID.
func (s *Store) GetProject(ctx context.Context, projectID string) (protocol.ProjectView, error) {
	var pv protocol.ProjectView
	var cfgJSON string
	var archived int
	err := s.ro.QueryRowContext(ctx, `
		SELECT project_id, display_name, repo_identity, default_branch,
		       mirror_bare_path, config_json, archived, updated_at
		FROM projects WHERE project_id = ?
	`, projectID).Scan(
		&pv.ProjectID, &pv.DisplayName, &pv.RepoIdentity, &pv.DefaultBranch,
		&pv.MirrorBarePath, &cfgJSON, &archived, &pv.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return pv, ErrNotFound
	}
	if err != nil {
		return pv, err
	}
	pv.Archived = archived != 0
	var cfg protocol.ProjectConfig
	if err := json.Unmarshal([]byte(cfgJSON), &cfg); err == nil {
		pv.DefaultCapability = cfg.DefaultCapability
		pv.DefaultAdapter = cfg.DefaultAdapter
		pv.IDEScheme = cfg.IDEScheme
		pv.Anchors = cfg.Anchors
	}
	if pv.Anchors == nil {
		pv.Anchors = map[string]protocol.ProjectAnchorConfig{}
	}
	if pv.IDEScheme == "" {
		pv.IDEScheme = protocol.IDECursor
	}
	return pv, nil
}

const taskSummarySelectSQL = `
		SELECT t.task_id, t.project_id, t.parent_task_id, t.title, t.goal,
		       t.required_capability, t.preferred_adapter, t.state, t.waiting_reason,
		       t.active_attempt_id, a.state, t.latest_delivery_id, d.head_sha,
		       (SELECT COUNT(*) FROM annotations an WHERE an.task_id = t.task_id AND an.status = 'open') AS open_ann,
		       (SELECT COUNT(*) FROM deliveries dl WHERE dl.task_id = t.task_id) AS dlv_count,
		       (SELECT COUNT(*) FROM task_pending_inputs pi WHERE pi.task_id = t.task_id AND pi.state = 'queued') AS queued_inputs,
		       (SELECT COUNT(*) FROM board_members bm WHERE bm.task_id = t.task_id AND bm.state IN ('working', 'waiting')) AS worker_count,
		       t.created_by, t.created_at, t.updated_at
		FROM tasks t
		LEFT JOIN attempts a ON a.attempt_id = t.active_attempt_id
		LEFT JOIN deliveries d ON d.delivery_id = t.latest_delivery_id
`

// ListProjectTasks returns tasks for a project ordered by recent updates.
func (s *Store) ListProjectTasks(ctx context.Context, projectID string) ([]protocol.TaskSummary, error) {
	if _, err := s.GetProject(ctx, projectID); err != nil {
		return nil, err
	}
	tasks := []protocol.TaskSummary{}
	rows, err := s.ro.QueryContext(ctx, taskSummarySelectSQL+`
		WHERE t.project_id = ? AND t.archived_at IS NULL
		ORDER BY t.updated_at DESC
	`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		ts, err := scanTaskSummary(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, ts)
	}
	return tasks, rows.Err()
}

func scanTaskSummary(scanner interface{ Scan(...any) error }) (protocol.TaskSummary, error) {
	var ts protocol.TaskSummary
	var parentID, waitingReason, activeAttID, activeAttState, latestDlvID, latestHead sql.NullString
	var adapter, state, createdBy string
	if err := scanner.Scan(
		&ts.TaskID, &ts.ProjectID, &parentID, &ts.Title, &ts.Goal,
		&ts.RequiredCapability, &adapter, &state, &waitingReason,
		&activeAttID, &activeAttState, &latestDlvID, &latestHead,
		&ts.OpenAnnotationCount, &ts.DeliveryCount,
		&ts.QueuedInputCount, &ts.WorkerCount,
		&createdBy, &ts.CreatedAt, &ts.UpdatedAt,
	); err != nil {
		return ts, err
	}
	ts.PreferredAdapter = protocol.Adapter(adapter)
	ts.State = protocol.TaskState(state)
	ts.CreatedBy = protocol.TaskCreatedBy(createdBy)
	if parentID.Valid {
		ts.ParentTaskID = parentID.String
	}
	if waitingReason.Valid {
		ts.WaitingReason = waitingReason.String
	}
	if activeAttID.Valid {
		ts.ActiveAttemptID = activeAttID.String
	}
	if activeAttState.Valid {
		st := protocol.AttemptState(activeAttState.String)
		ts.ActiveAttemptState = &st
	}
	if latestDlvID.Valid {
		ts.LatestDeliveryID = latestDlvID.String
	}
	if latestHead.Valid {
		ts.LatestDeliveryHead = latestHead.String
	}
	return ts, nil
}

// GetTaskTimeline loads a task with its attempts, immutable deliveries,
// Ryugi bindings, annotations, pending inputs, approvals, sources, board members,
// and timeline events inside a read-only transaction.
func (s *Store) GetTaskTimeline(ctx context.Context, taskID string, beforeSeq int64, includeRaw bool) (protocol.TaskTimelineResponse, error) {
	resp := protocol.TaskTimelineResponse{
		Attempts:      []protocol.AttemptView{},
		Deliveries:    []protocol.DeliveryView{},
		Annotations:   []protocol.AnnotationView{},
		PendingInputs: []protocol.PendingInput{},
		Approvals:     []protocol.ApprovalView{},
		Sources:       []protocol.TaskSource{},
		BoardMembers:  []protocol.BoardMemberCursor{},
		Events:        []protocol.EventRecord{},
	}

	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, taskSummarySelectSQL+`
			WHERE t.task_id = ?
		`, taskID)
		task, err := scanTaskSummary(row)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		resp.Task = task

		// Attempts
		aRows, err := tx.QueryContext(ctx, `
			SELECT attempt_id, task_id, project_id, attempt_no, machine_id, adapter,
			       trigger_kind, worktree_path, base_sha, output_delivery_id,
			       state, completion_source, error_code, error_detail, started_at, finished_at
			FROM attempts
			WHERE task_id = ?
			ORDER BY attempt_no ASC
		`, taskID)
		if err != nil {
			return err
		}
		defer aRows.Close()
		for aRows.Next() {
			att, err := scanAttemptView(aRows)
			if err != nil {
				return err
			}
			resp.Attempts = append(resp.Attempts, att)
		}
		if err := aRows.Err(); err != nil {
			return err
		}

		// Ryugi bindings grouped by delivery_id
		bindingsByDelivery := map[string][]protocol.RyugiBindingView{}
		bRows, err := tx.QueryContext(ctx, `
			SELECT binding_id, delivery_id, project_id, task_id, frozen_project_dir,
			       artifact_locator, derived_artifact_id, open_url,
			       last_thread_receipt, last_changes_receipt, active, created_at, updated_at
			FROM ryugi_bindings
			WHERE task_id = ?
			ORDER BY artifact_locator ASC
		`, taskID)
		if err != nil {
			return err
		}
		defer bRows.Close()
		for bRows.Next() {
			b, err := scanRyugiBinding(bRows)
			if err != nil {
				return err
			}
			bindingsByDelivery[b.DeliveryID] = append(bindingsByDelivery[b.DeliveryID], b)
		}
		if err := bRows.Err(); err != nil {
			return err
		}

		// Deliveries
		dRows, err := tx.QueryContext(ctx, `
			SELECT d.delivery_id, d.project_id, d.task_id, d.attempt_id, d.source_machine_id,
			       d.repo_identity, d.base_sha, d.head_sha, d.git_ref, d.kind, d.summary,
			       d.manifest_json, d.frozen_snapshot_dir, d.ryugi_sync_state,
			       d.late_arrival, d.pinned, d.created_at,
			       (SELECT COUNT(*) FROM annotations an WHERE an.delivery_id = d.delivery_id AND an.status = 'open') AS open_ann
			FROM deliveries d
			WHERE d.task_id = ?
			ORDER BY d.created_at ASC
		`, taskID)
		if err != nil {
			return err
		}
		defer dRows.Close()
		for dRows.Next() {
			dlv, err := scanDeliveryView(dRows)
			if err != nil {
				return err
			}
			dlv.RyugiBindings = bindingsByDelivery[dlv.DeliveryID]
			resp.Deliveries = append(resp.Deliveries, dlv)
		}
		if err := dRows.Err(); err != nil {
			return err
		}

		// Annotations
		anRows, err := tx.QueryContext(ctx, `
			SELECT annotation_id, project_id, task_id, delivery_id, source, external_key,
			       anchor_json, body, status, triggered_attempt_id, resolution_delivery_id,
			       resolution_note, created_at, resolved_at
			FROM annotations
			WHERE task_id = ?
			ORDER BY created_at ASC
		`, taskID)
		if err != nil {
			return err
		}
		defer anRows.Close()
		for anRows.Next() {
			an, err := scanAnnotationView(anRows)
			if err != nil {
				return err
			}
			resp.Annotations = append(resp.Annotations, an)
		}
		if err := anRows.Err(); err != nil {
			return err
		}

		// Pending Inputs (queued)
		piRows, err := tx.QueryContext(ctx, `
			SELECT input_id, task_id, project_id, kind, source, body, payload_json, position, state, created_at, updated_at, dispatched_attempt_id
			FROM task_pending_inputs
			WHERE task_id = ? AND state = 'queued'
			ORDER BY position ASC, created_at ASC
		`, taskID)
		if err != nil {
			return err
		}
		defer piRows.Close()
		for piRows.Next() {
			pi, err := scanPendingInput(piRows)
			if err != nil {
				return err
			}
			resp.PendingInputs = append(resp.PendingInputs, pi)
		}
		if err := piRows.Err(); err != nil {
			return err
		}

		// Approvals
		apRows, err := tx.QueryContext(ctx, approvalSelectSQL+`
			WHERE task_id = ?
			ORDER BY created_at DESC
		`, taskID)
		if err != nil {
			return err
		}
		defer apRows.Close()
		for apRows.Next() {
			ap, err := scanApprovalView(apRows)
			if err != nil {
				return err
			}
			resp.Approvals = append(resp.Approvals, ap)
		}
		if err := apRows.Err(); err != nil {
			return err
		}

		// Task Sources
		srcRows, err := tx.QueryContext(ctx, `
			SELECT source_id, task_id, project_id, kind, label, uri_or_ref, created_at
			FROM task_sources
			WHERE task_id = ?
			ORDER BY created_at ASC
		`, taskID)
		if err != nil {
			return err
		}
		defer srcRows.Close()
		for srcRows.Next() {
			src, err := scanTaskSource(srcRows)
			if err != nil {
				return err
			}
			resp.Sources = append(resp.Sources, src)
		}
		if err := srcRows.Err(); err != nil {
			return err
		}

		// Board Members
		bmRows, err := tx.QueryContext(ctx, `
			SELECT task_id, project_id, member_name, machine_id, adapter, state, last_consumed_seq, current_step, worker_task_id, updated_at
			FROM board_members
			WHERE task_id = ?
			ORDER BY member_name ASC
		`, taskID)
		if err != nil {
			return err
		}
		defer bmRows.Close()
		for bmRows.Next() {
			bm, err := scanBoardMember(bmRows)
			if err != nil {
				return err
			}
			resp.BoardMembers = append(resp.BoardMembers, bm)
		}
		if err := bmRows.Err(); err != nil {
			return err
		}

		// Events: fetch newest page via ORDER BY seq DESC LIMIT 500, then reverse to ascending order
		var evQuery string
		args := []any{taskID}
		if includeRaw {
			evQuery = `SELECT seq, event_id, stream_id, project_id, task_id, attempt_id,
			                  actor_kind, actor_id, kind, visibility, payload_json, created_at
			           FROM events WHERE task_id = ?`
		} else {
			evQuery = `SELECT seq, event_id, stream_id, project_id, task_id, attempt_id,
			                  actor_kind, actor_id, kind, visibility, payload_json, created_at
			           FROM events WHERE task_id = ? AND visibility != 'raw_acp'`
		}
		if beforeSeq > 0 {
			evQuery += ` AND seq < ?`
			args = append(args, beforeSeq)
		}
		evQuery += ` ORDER BY seq DESC LIMIT 500`

		eRows, err := tx.QueryContext(ctx, evQuery, args...)
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
		if err := eRows.Err(); err != nil {
			return err
		}
		// Reverse descending page back into ascending chronological order
		for i, j := 0, len(resp.Events)-1; i < j; i, j = i+1, j-1 {
			resp.Events[i], resp.Events[j] = resp.Events[j], resp.Events[i]
		}
		return nil
	})
	return resp, err
}

func scanAttemptView(scanner interface{ Scan(...any) error }) (protocol.AttemptView, error) {
	var a protocol.AttemptView
	var adapter, trigger, state string
	var worktree, baseSHA, outDlv, compSrc, errCode, errDetail sql.NullString
	var finishedAt sql.NullInt64
	if err := scanner.Scan(
		&a.AttemptID, &a.TaskID, &a.ProjectID, &a.AttemptNo, &a.MachineID, &adapter,
		&trigger, &worktree, &baseSHA, &outDlv,
		&state, &compSrc, &errCode, &errDetail, &a.StartedAt, &finishedAt,
	); err != nil {
		return a, err
	}
	a.Adapter = protocol.Adapter(adapter)
	a.TriggerKind = protocol.TriggerKind(trigger)
	a.State = protocol.AttemptState(state)
	if worktree.Valid {
		a.WorktreePath = worktree.String
	}
	if baseSHA.Valid {
		a.BaseSHA = baseSHA.String
	}
	if outDlv.Valid {
		a.OutputDeliveryID = outDlv.String
	}
	if compSrc.Valid {
		cs := protocol.CompletionSource(compSrc.String)
		a.CompletionSource = &cs
	}
	if errCode.Valid {
		a.ErrorCode = errCode.String
	}
	if errDetail.Valid {
		a.ErrorDetail = errDetail.String
	}
	if finishedAt.Valid {
		v := finishedAt.Int64
		a.FinishedAt = &v
	}
	return a, nil
}

func scanDeliveryView(scanner interface{ Scan(...any) error }) (protocol.DeliveryView, error) {
	var d protocol.DeliveryView
	var kind, manifestJSON, ryugiState string
	var frozenDir sql.NullString
	var lateArrival, pinned int
	if err := scanner.Scan(
		&d.DeliveryID, &d.ProjectID, &d.TaskID, &d.AttemptID, &d.SourceMachineID,
		&d.RepoIdentity, &d.BaseSHA, &d.HeadSHA, &d.GitRef, &kind, &d.Summary,
		&manifestJSON, &frozenDir, &ryugiState,
		&lateArrival, &pinned, &d.CreatedAt, &d.OpenAnnotations,
	); err != nil {
		return d, err
	}
	d.Kind = protocol.DeliveryKind(kind)
	d.RyugiSyncState = protocol.RyugiSyncState(ryugiState)
	d.LateArrival = lateArrival != 0
	d.Pinned = pinned != 0
	if frozenDir.Valid {
		d.FrozenSnapshotDir = frozenDir.String
	}
	_ = json.Unmarshal([]byte(manifestJSON), &d.Manifest)
	return d, nil
}

func scanRyugiBinding(scanner interface{ Scan(...any) error }) (protocol.RyugiBindingView, error) {
	var b protocol.RyugiBindingView
	var lastThread, lastChanges sql.NullString
	var active int
	if err := scanner.Scan(
		&b.BindingID, &b.DeliveryID, &b.ProjectID, &b.TaskID, &b.FrozenProjectDir,
		&b.ArtifactLocator, &b.DerivedArtifactID, &b.OpenURL,
		&lastThread, &lastChanges, &active, &b.CreatedAt, &b.UpdatedAt,
	); err != nil {
		return b, err
	}
	if lastThread.Valid {
		b.LastThreadReceipt = lastThread.String
	}
	if lastChanges.Valid {
		b.LastChangesReceipt = lastChanges.String
	}
	b.Active = active != 0
	return b, nil
}

func scanAnnotationView(scanner interface{ Scan(...any) error }) (protocol.AnnotationView, error) {
	var an protocol.AnnotationView
	var source, anchorJSON, status string
	var extKey, trigAtt, resDlv, resNote sql.NullString
	var resolvedAt sql.NullInt64
	if err := scanner.Scan(
		&an.AnnotationID, &an.ProjectID, &an.TaskID, &an.DeliveryID,
		&source, &extKey, &anchorJSON, &an.Body, &status,
		&trigAtt, &resDlv, &resNote, &an.CreatedAt, &resolvedAt,
	); err != nil {
		return an, err
	}
	an.Source = protocol.AnnotationSource(source)
	an.Status = protocol.AnnotationStatus(status)
	if extKey.Valid {
		an.ExternalKey = extKey.String
	}
	if trigAtt.Valid {
		an.TriggeredAttemptID = trigAtt.String
	}
	if resDlv.Valid {
		an.ResolutionDeliveryID = resDlv.String
	}
	if resNote.Valid {
		an.ResolutionNote = resNote.String
	}
	if resolvedAt.Valid {
		v := resolvedAt.Int64
		an.ResolvedAt = &v
	}
	_ = json.Unmarshal([]byte(anchorJSON), &an.Anchor)
	return an, nil
}

func scanEventRecord(scanner interface{ Scan(...any) error }) (protocol.EventRecord, error) {
	var ev protocol.EventRecord
	var projectID, taskID, attemptID sql.NullString
	var actorKind, kind, vis, payloadJSON string
	if err := scanner.Scan(
		&ev.Seq, &ev.EventID, &ev.StreamID, &projectID, &taskID, &attemptID,
		&actorKind, &ev.ActorID, &kind, &vis, &payloadJSON, &ev.CreatedAt,
	); err != nil {
		return ev, err
	}
	ev.ActorKind = protocol.ActorKind(actorKind)
	ev.Kind = protocol.EventKind(kind)
	ev.Visibility = protocol.EventVisibility(vis)
	if projectID.Valid {
		ev.ProjectID = projectID.String
	}
	if taskID.Valid {
		ev.TaskID = taskID.String
	}
	if attemptID.Valid {
		ev.AttemptID = attemptID.String
	}
	ev.Payload = map[string]any{}
	_ = json.Unmarshal([]byte(payloadJSON), &ev.Payload)
	return ev, nil
}

// ManualImportParams contains validated inputs for atomic Phase 0 manual delivery import.
type ManualImportParams struct {
	DeliveryID      string
	ProjectID       string
	TaskID          string
	Title           string
	Summary         string
	SourceMachineID string
	RepoIdentity    string
	BaseSHA         string
	HeadSHA         string
	GitRef          string
	Kind            protocol.DeliveryKind
	Manifest        protocol.DeliveryManifest
	WorktreePath    string
	IdempotencyKey  string
	ActorID         string
}

// ManualImportResult holds all created rows and emitted events for WS fan-out.
type ManualImportResult struct {
	Task      protocol.TaskSummary
	Attempt   protocol.AttemptView
	Delivery  protocol.DeliveryView
	InboxItem protocol.InboxItemView
	Created   bool
	Events    []protocol.EventRecord
}

// CreateManualImportDelivery atomically creates/updates a manual Task, a manual Attempt,
// an immutable Delivery, unified timeline/inbox events, and a delivery_review inbox card.
// Re-submitting the same deliveryId with identical immutable fields is idempotent (Created=false);
// any mismatch in projectId/taskId/baseSha/headSha/gitRef returns ErrConflict.
func (s *Store) CreateManualImportDelivery(ctx context.Context, p ManualImportParams) (ManualImportResult, error) {
	var res ManualImportResult
	now := time.Now().UnixMilli()

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		resolvedKind := resolveDeliveryKind(p.Kind, p.Manifest)

		// Check if delivery already exists (strict immutable metadata + canonical manifest validation)
		var existingProjectID, existingTaskID, existingAttemptID, existingBaseSHA, existingHeadSHA, existingGitRef, existingKind, existingSummary, existingManifestJSON string
		err := tx.QueryRowContext(ctx, `
			SELECT project_id, task_id, attempt_id, base_sha, head_sha, git_ref, kind, summary, manifest_json
			FROM deliveries WHERE delivery_id = ?
		`, p.DeliveryID).Scan(
			&existingProjectID, &existingTaskID, &existingAttemptID,
			&existingBaseSHA, &existingHeadSHA, &existingGitRef,
			&existingKind, &existingSummary, &existingManifestJSON,
		)
		if err == nil {
			var existingManifest protocol.DeliveryManifest
			_ = json.Unmarshal([]byte(existingManifestJSON), &existingManifest)
			if existingProjectID != p.ProjectID ||
				(p.TaskID != "" && existingTaskID != p.TaskID) ||
				existingBaseSHA != p.BaseSHA ||
				existingHeadSHA != p.HeadSHA ||
				existingGitRef != p.GitRef ||
				existingKind != string(resolvedKind) ||
				existingSummary != p.Summary ||
				canonicalManifestJSON(existingManifest) != canonicalManifestJSON(p.Manifest) {
				return fmt.Errorf("%w: delivery %s already exists with different immutable metadata", ErrConflict, p.DeliveryID)
			}
			res.Created = false
			return loadManualImportExisting(ctx, tx, existingTaskID, existingAttemptID, p.DeliveryID, &res)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		// Verify and increment ref_count once per distinct blob sha256 referenced by this new delivery
		distinctBlobs := map[string]int64{}
		for _, b := range p.Manifest.Blobs {
			if prevSize, dup := distinctBlobs[b.SHA256]; dup && prevSize != b.Size {
				return fmt.Errorf("%w: conflicting sizes for blob %s", ErrBlobMismatch, b.SHA256)
			}
			distinctBlobs[b.SHA256] = b.Size
		}
		for sha, expectedSize := range distinctBlobs {
			var actualSize int64
			err := tx.QueryRowContext(ctx, `SELECT size_bytes FROM delivery_blobs WHERE sha256 = ?`, sha).Scan(&actualSize)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: blob %s not uploaded", ErrBlobMismatch, sha)
			}
			if err != nil {
				return err
			}
			if actualSize != expectedSize {
				return fmt.Errorf("%w: blob %s size mismatch (stored=%d manifest=%d)", ErrBlobMismatch, sha, actualSize, expectedSize)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE delivery_blobs SET ref_count = ref_count + 1 WHERE sha256 = ?`, sha); err != nil {
				return err
			}
		}

		// Ensure source machine exists; if omitted, pick first local_ws machine or any machine.
		machineID := p.SourceMachineID
		if machineID == "" {
			err := tx.QueryRowContext(ctx, `
				SELECT machine_id FROM machines
				ORDER BY CASE WHEN transport_mode = 'local_ws' THEN 0 ELSE 1 END, machine_id ASC
				LIMIT 1
			`).Scan(&machineID)
			if errors.Is(err, sql.ErrNoRows) {
				machineID = "hub-node"
				_, err = tx.ExecContext(ctx, `
					INSERT INTO machines (machine_id, transport_mode, status, capabilities_json, resources_json, last_seen_at, updated_at)
					VALUES (?, 'local_ws', 'online', '["primary-dev"]', '{}', ?, ?)
				`, machineID, now, now)
				if err != nil {
					return fmt.Errorf("auto-register default machine: %w", err)
				}
			} else if err != nil {
				return err
			}
		} else {
			var exists string
			err := tx.QueryRowContext(ctx, `SELECT machine_id FROM machines WHERE machine_id = ?`, machineID).Scan(&exists)
			if errors.Is(err, sql.ErrNoRows) {
				_, err = tx.ExecContext(ctx, `
					INSERT INTO machines (machine_id, transport_mode, status, capabilities_json, resources_json, last_seen_at, updated_at)
					VALUES (?, 'local_ws', 'online', '["primary-dev"]', '{}', ?, ?)
				`, machineID, now, now)
				if err != nil {
					return fmt.Errorf("register source machine %s: %w", machineID, err)
				}
			} else if err != nil {
				return err
			}
		}

		taskID := p.TaskID
		createdNewTask := false
		if taskID != "" {
			var ownerProject string
			err := tx.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE task_id = ?`, taskID).Scan(&ownerProject)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: task %s", ErrNotFound, taskID)
			}
			if err != nil {
				return err
			}
			if ownerProject != p.ProjectID {
				return fmt.Errorf("%w: task %s belongs to project %s, not %s", ErrOwnershipMismatch, taskID, ownerProject, p.ProjectID)
			}
		} else {
			taskID = newID("tsk")
			createdNewTask = true
			title := strings.TrimSpace(p.Title)
			if title == "" {
				title = strings.TrimSpace(p.Summary)
			}
			if title == "" {
				title = "Manual Delivery " + p.DeliveryID
			}
			if len(title) > 120 {
				title = title[:120]
			}
			goal := strings.TrimSpace(p.Summary)
			if goal == "" {
				goal = title
			}
			var idemp any
			if p.IdempotencyKey != "" {
				idemp = p.IdempotencyKey
			}
			_, err := tx.ExecContext(ctx, `
				INSERT INTO tasks (
					task_id, project_id, title, goal, required_capability, required_resources,
					preferred_adapter, desired_config_json, state, created_by, idempotency_key,
					version, created_at, updated_at
				) VALUES (?, ?, ?, ?, 'primary-dev', '[]', 'manual', '{}', 'in_review', 'cli', ?, 1, ?, ?)
			`, taskID, p.ProjectID, title, goal, idemp, now, now)
			if err != nil {
				return fmt.Errorf("insert manual task: %w", err)
			}
		}

		var nextAttemptNo int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(attempt_no), 0) + 1 FROM attempts WHERE task_id = ?`, taskID).Scan(&nextAttemptNo); err != nil {
			return err
		}

		attemptID := newID("att")
		leaseToken := randomHex(16)
		var worktreeVal any
		if p.WorktreePath != "" {
			worktreeVal = p.WorktreePath
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO attempts (
				attempt_id, task_id, project_id, attempt_no, machine_id, adapter,
				trigger_kind, trigger_payload_json, worktree_path, base_sha, output_delivery_id,
				state, completion_source, lease_token, lease_expires_at, started_at, finished_at
			) VALUES (?, ?, ?, ?, ?, 'manual', 'manual_import', '{}', ?, ?, ?, 'succeeded', 'manual_import', ?, ?, ?, ?)
		`, attemptID, taskID, p.ProjectID, nextAttemptNo, machineID, worktreeVal, p.BaseSHA, p.DeliveryID, leaseToken, now, now, now)
		if err != nil {
			return fmt.Errorf("insert manual attempt: %w", err)
		}

		ryugiSyncState := protocol.RyugiSyncNone
		if len(p.Manifest.RyugiArtifacts) > 0 {
			ryugiSyncState = protocol.RyugiSyncPending
		}

		kind := resolvedKind

		manifestBytes, err := json.Marshal(normalizeManifest(p.Manifest))
		if err != nil {
			return err
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO deliveries (
				delivery_id, project_id, task_id, attempt_id, source_machine_id,
				repo_identity, base_sha, head_sha, git_ref, kind, summary,
				manifest_json, verified_machines_json, ryugi_sync_state, late_arrival, pinned, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '{}', ?, 0, 1, ?)
		`, p.DeliveryID, p.ProjectID, taskID, attemptID, machineID,
			p.RepoIdentity, p.BaseSHA, p.HeadSHA, p.GitRef, string(kind), p.Summary,
			string(manifestBytes), string(ryugiSyncState), now)
		if err != nil {
			return fmt.Errorf("insert delivery: %w", err)
		}

		// Update task pointers & state
		_, err = tx.ExecContext(ctx, `
			UPDATE tasks
			SET state = 'in_review', latest_delivery_id = ?, version = version + 1, updated_at = ?
			WHERE task_id = ?
		`, p.DeliveryID, now, taskID)
		if err != nil {
			return fmt.Errorf("update task latest_delivery_id: %w", err)
		}

		actorID := p.ActorID
		if actorID == "" {
			actorID = "orbit-cli"
		}

		if createdNewTask {
			ev, err := insertEventTx(ctx, tx, eventInsert{
				StreamID:   "task:" + taskID,
				ProjectID:  p.ProjectID,
				TaskID:     taskID,
				ActorKind:  protocol.ActorHuman,
				ActorID:    actorID,
				Kind:       protocol.EventTaskCreated,
				Visibility: protocol.VisibilityTimeline,
				Payload: map[string]any{
					"title":   p.Title,
					"summary": p.Summary,
					"adapter": protocol.AdapterManual,
				},
				CreatedAt: now,
			})
			if err != nil {
				return err
			}
			res.Events = append(res.Events, ev)
		}

		evPub, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + taskID,
			ProjectID:  p.ProjectID,
			TaskID:     taskID,
			AttemptID:  attemptID,
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventDeliveryPublished,
			Visibility: protocol.VisibilityInbox,
			Payload: map[string]any{
				"deliveryId":     p.DeliveryID,
				"baseSha":        p.BaseSHA,
				"headSha":        p.HeadSHA,
				"filesChanged":   p.Manifest.FilesChanged,
				"insertions":     p.Manifest.Insertions,
				"deletions":      p.Manifest.Deletions,
				"ryugiArtifacts": p.Manifest.RyugiArtifacts,
				"summary":        p.Summary,
			},
			CreatedAt: now,
		})
		if err != nil {
			return err
		}
		res.Events = append(res.Events, evPub)

		// Upsert deterministic delivery_review inbox item in the SAME transaction
		inboxID := "inb:dlv:" + p.DeliveryID
		shortHead := p.HeadSHA
		if len(shortHead) > 7 {
			shortHead = shortHead[:7]
		}
		inboxTitle := fmt.Sprintf("待审交付 · %s (%s)", p.ProjectID, shortHead)
		inboxSummary := p.Summary
		if inboxSummary == "" {
			inboxSummary = fmt.Sprintf("+%d -%d · %d files", p.Manifest.Insertions, p.Manifest.Deletions, p.Manifest.FilesChanged)
		}
		_, err = tx.ExecContext(ctx, `
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
		`, inboxID, p.ProjectID, taskID, p.DeliveryID, inboxTitle, inboxSummary, now, now)
		if err != nil {
			return fmt.Errorf("upsert inbox_items: %w", err)
		}

		res.Created = true
		return loadManualImportExisting(ctx, tx, taskID, attemptID, p.DeliveryID, &res)
	})
	return res, err
}

func loadManualImportExisting(ctx context.Context, tx *sql.Tx, taskID, attemptID, deliveryID string, res *ManualImportResult) error {
	row := tx.QueryRowContext(ctx, taskSummarySelectSQL+`
		WHERE t.task_id = ?
	`, taskID)
	task, err := scanTaskSummary(row)
	if err != nil {
		return err
	}
	res.Task = task

	aRow := tx.QueryRowContext(ctx, `
		SELECT attempt_id, task_id, project_id, attempt_no, machine_id, adapter,
		       trigger_kind, worktree_path, base_sha, output_delivery_id,
		       state, completion_source, error_code, error_detail, started_at, finished_at
		FROM attempts WHERE attempt_id = ?
	`, attemptID)
	att, err := scanAttemptView(aRow)
	if err != nil {
		return err
	}
	res.Attempt = att

	dRow := tx.QueryRowContext(ctx, `
		SELECT d.delivery_id, d.project_id, d.task_id, d.attempt_id, d.source_machine_id,
		       d.repo_identity, d.base_sha, d.head_sha, d.git_ref, d.kind, d.summary,
		       d.manifest_json, d.frozen_snapshot_dir, d.ryugi_sync_state,
		       d.late_arrival, d.pinned, d.created_at,
		       (SELECT COUNT(*) FROM annotations an WHERE an.delivery_id = d.delivery_id AND an.status = 'open') AS open_ann
		FROM deliveries d WHERE d.delivery_id = ?
	`, deliveryID)
	dlv, err := scanDeliveryView(dRow)
	if err != nil {
		return err
	}
	res.Delivery = dlv

	iRow := tx.QueryRowContext(ctx, `
		SELECT inbox_id, project_id, task_id, category, ref_id, title, summary,
		       priority, state, created_at, updated_at, resolved_at
		FROM inbox_items WHERE inbox_id = ?
	`, "inb:dlv:"+deliveryID)
	inbox, err := scanInboxItem(iRow)
	if err == nil {
		res.InboxItem = inbox
	}
	return nil
}

// GetDeliveryDetail loads a delivery, its Ryugi bindings, annotations, and owning project.
func (s *Store) GetDeliveryDetail(ctx context.Context, deliveryID string) (protocol.DeliveryView, []protocol.RyugiBindingView, []protocol.AnnotationView, protocol.ProjectView, error) {
	var dlv protocol.DeliveryView
	bindings := []protocol.RyugiBindingView{}
	annotations := []protocol.AnnotationView{}
	var proj protocol.ProjectView

	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		dRow := tx.QueryRowContext(ctx, `
			SELECT d.delivery_id, d.project_id, d.task_id, d.attempt_id, d.source_machine_id,
			       d.repo_identity, d.base_sha, d.head_sha, d.git_ref, d.kind, d.summary,
			       d.manifest_json, d.frozen_snapshot_dir, d.ryugi_sync_state,
			       d.late_arrival, d.pinned, d.created_at,
			       (SELECT COUNT(*) FROM annotations an WHERE an.delivery_id = d.delivery_id AND an.status = 'open') AS open_ann
			FROM deliveries d WHERE d.delivery_id = ?
		`, deliveryID)
		var err error
		dlv, err = scanDeliveryView(dRow)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}

		bRows, err := tx.QueryContext(ctx, `
			SELECT binding_id, delivery_id, project_id, task_id, frozen_project_dir,
			       artifact_locator, derived_artifact_id, open_url,
			       last_thread_receipt, last_changes_receipt, active, created_at, updated_at
			FROM ryugi_bindings
			WHERE delivery_id = ?
			ORDER BY artifact_locator ASC
		`, deliveryID)
		if err != nil {
			return err
		}
		defer bRows.Close()
		for bRows.Next() {
			b, err := scanRyugiBinding(bRows)
			if err != nil {
				return err
			}
			bindings = append(bindings, b)
		}
		if err := bRows.Err(); err != nil {
			return err
		}
		dlv.RyugiBindings = bindings

		anRows, err := tx.QueryContext(ctx, `
			SELECT annotation_id, project_id, task_id, delivery_id, source, external_key,
			       anchor_json, body, status, triggered_attempt_id, resolution_delivery_id,
			       resolution_note, created_at, resolved_at
			FROM annotations
			WHERE delivery_id = ?
			ORDER BY created_at ASC
		`, deliveryID)
		if err != nil {
			return err
		}
		defer anRows.Close()
		for anRows.Next() {
			an, err := scanAnnotationView(anRows)
			if err != nil {
				return err
			}
			annotations = append(annotations, an)
		}
		if err := anRows.Err(); err != nil {
			return err
		}

		var cfgJSON string
		var archived int
		err = tx.QueryRowContext(ctx, `
			SELECT project_id, display_name, repo_identity, default_branch,
			       mirror_bare_path, config_json, archived, updated_at
			FROM projects WHERE project_id = ?
		`, dlv.ProjectID).Scan(
			&proj.ProjectID, &proj.DisplayName, &proj.RepoIdentity, &proj.DefaultBranch,
			&proj.MirrorBarePath, &cfgJSON, &archived, &proj.UpdatedAt,
		)
		if err != nil {
			return err
		}
		proj.Archived = archived != 0
		var cfg protocol.ProjectConfig
		if err := json.Unmarshal([]byte(cfgJSON), &cfg); err == nil {
			proj.DefaultCapability = cfg.DefaultCapability
			proj.DefaultAdapter = cfg.DefaultAdapter
			proj.IDEScheme = cfg.IDEScheme
			proj.Anchors = cfg.Anchors
		}
		if proj.Anchors == nil {
			proj.Anchors = map[string]protocol.ProjectAnchorConfig{}
		}
		return nil
	})
	return dlv, bindings, annotations, proj, err
}

// CreateAnnotationParams defines inputs to create a delivery-anchored annotation.
type CreateAnnotationParams struct {
	DeliveryID     string
	ClaimedProject string
	ClaimedTask    string
	Source         protocol.AnnotationSource
	ExternalKey    string
	Anchor         protocol.AnnotationAnchor
	Body           string
	ActorID        string
}

// CreateAnnotation derives authoritative project_id & task_id from delivery_id,
// rejects any contradictory claimed project/task, deduplicates by ExternalKey,
// and atomically emits annotation.created when newly created.
func (s *Store) CreateAnnotation(ctx context.Context, p CreateAnnotationParams) (protocol.AnnotationView, bool, *protocol.EventRecord, error) {
	var out protocol.AnnotationView
	var created bool
	var emitted *protocol.EventRecord
	now := time.Now().UnixMilli()

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var projectID, taskID string
		err := tx.QueryRowContext(ctx, `
			SELECT project_id, task_id FROM deliveries WHERE delivery_id = ?
		`, p.DeliveryID).Scan(&projectID, &taskID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}

		if p.ClaimedProject != "" && p.ClaimedProject != projectID {
			return fmt.Errorf("%w: delivery %s belongs to project %s, not %s", ErrOwnershipMismatch, p.DeliveryID, projectID, p.ClaimedProject)
		}
		if p.ClaimedTask != "" && p.ClaimedTask != taskID {
			return fmt.Errorf("%w: delivery %s belongs to task %s, not %s", ErrOwnershipMismatch, p.DeliveryID, taskID, p.ClaimedTask)
		}

		if p.ExternalKey != "" {
			row := tx.QueryRowContext(ctx, `
				SELECT annotation_id, project_id, task_id, delivery_id, source, external_key,
				       anchor_json, body, status, triggered_attempt_id, resolution_delivery_id,
				       resolution_note, created_at, resolved_at
				FROM annotations WHERE external_key = ?
			`, p.ExternalKey)
			existing, err := scanAnnotationView(row)
			if err == nil {
				if existing.DeliveryID != p.DeliveryID {
					return fmt.Errorf("%w: idempotency key already bound to another delivery", ErrConflict)
				}
				out = existing
				created = false
				return nil
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}

		source := p.Source
		if source == "" {
			source = protocol.AnnotationSourceDiffDrawer
		}
		if !source.IsValid() {
			return fmt.Errorf("invalid annotation source %q", source)
		}

		anchorBytes, err := json.Marshal(p.Anchor)
		if err != nil {
			return err
		}
		annID := newID("ann")
		var extKey any
		if p.ExternalKey != "" {
			extKey = p.ExternalKey
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO annotations (
				annotation_id, project_id, task_id, delivery_id, source, external_key,
				anchor_json, body, status, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'open', ?)
		`, annID, projectID, taskID, p.DeliveryID, string(source), extKey, string(anchorBytes), p.Body, now)
		if err != nil {
			return err
		}

		actorKind := protocol.ActorHuman
		actorID := p.ActorID
		if source == protocol.AnnotationSourceRyugi {
			actorKind = protocol.ActorRyugi
			if actorID == "" {
				actorID = "ryugi-bridge"
			}
		} else if actorID == "" {
			actorID = "human"
		}

		ev, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + taskID,
			ProjectID:  projectID,
			TaskID:     taskID,
			ActorKind:  actorKind,
			ActorID:    actorID,
			Kind:       protocol.EventAnnotationCreated,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"annotationId": annID,
				"deliveryId":   p.DeliveryID,
				"source":       source,
				"anchor":       p.Anchor,
				"body":         p.Body,
			},
			CreatedAt: now,
		})
		if err != nil {
			return err
		}

		out = protocol.AnnotationView{
			AnnotationID: annID,
			ProjectID:    projectID,
			TaskID:       taskID,
			DeliveryID:   p.DeliveryID,
			Source:       source,
			ExternalKey:  p.ExternalKey,
			Anchor:       p.Anchor,
			Body:         p.Body,
			Status:       protocol.AnnotationOpen,
			CreatedAt:    now,
		}
		created = true
		emitted = &ev
		return nil
	})
	return out, created, emitted, err
}

// StateHashEntry tracks both the latest content hash and monotonic revision number
// for unreceipted Ryugi state items (asks/groups/rounds) so A -> B -> A transitions
// emit distinct annotations with external_key suffix :v<rev>.
type StateHashEntry struct {
	Hash string `json:"hash"`
	Rev  int    `json:"rev"`
}

func (e *StateHashEntry) UnmarshalJSON(data []byte) error {
	var legacy string
	if err := json.Unmarshal(data, &legacy); err == nil {
		e.Hash = legacy
		e.Rev = 1
		return nil
	}
	type alias StateHashEntry
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*e = StateHashEntry(a)
	return nil
}

// RyugiBindingRecord extends RyugiBindingView with last_state_hashes for internal bridge syncing.
type RyugiBindingRecord struct {
	protocol.RyugiBindingView
	LastStateHashes map[string]StateHashEntry
}

// SaveRyugiBindings updates frozen_snapshot_dir, ryugi_sync_state, upserts ryugi_bindings,
// and atomically emits a delivery.ryugi_updated event in the same write transaction.
func (s *Store) SaveRyugiBindings(ctx context.Context, deliveryID, frozenDir string, syncState protocol.RyugiSyncState, bindings []protocol.RyugiBindingView) ([]protocol.RyugiBindingView, *protocol.EventRecord, error) {
	now := time.Now().UnixMilli()
	var saved []protocol.RyugiBindingView
	var emitted *protocol.EventRecord

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var projectID, taskID string
		err := tx.QueryRowContext(ctx, `SELECT project_id, task_id FROM deliveries WHERE delivery_id = ?`, deliveryID).Scan(&projectID, &taskID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}

		var frozenVal any
		if frozenDir != "" {
			frozenVal = frozenDir
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE deliveries SET frozen_snapshot_dir = COALESCE(?, frozen_snapshot_dir), ryugi_sync_state = ?
			WHERE delivery_id = ?
		`, frozenVal, string(syncState), deliveryID)
		if err != nil {
			return err
		}

		for _, b := range bindings {
			bid := b.BindingID
			if bid == "" {
				bid = newID("ryg")
			}
			_, err := tx.ExecContext(ctx, `
				INSERT INTO ryugi_bindings (
					binding_id, delivery_id, project_id, task_id, frozen_project_dir,
					artifact_locator, derived_artifact_id, open_url, active, created_at, updated_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
				ON CONFLICT(delivery_id, artifact_locator) DO UPDATE SET
					frozen_project_dir = excluded.frozen_project_dir,
					derived_artifact_id = excluded.derived_artifact_id,
					open_url = excluded.open_url,
					active = 1,
					updated_at = excluded.updated_at
			`, bid, deliveryID, projectID, taskID, b.FrozenProjectDir,
				b.ArtifactLocator, b.DerivedArtifactID, b.OpenURL, now, now)
			if err != nil {
				return fmt.Errorf("upsert ryugi binding %s: %w", b.ArtifactLocator, err)
			}
		}

		rows, err := tx.QueryContext(ctx, `
			SELECT binding_id, delivery_id, project_id, task_id, frozen_project_dir,
			       artifact_locator, derived_artifact_id, open_url,
			       last_thread_receipt, last_changes_receipt, active, created_at, updated_at
			FROM ryugi_bindings
			WHERE delivery_id = ?
			ORDER BY artifact_locator ASC
		`, deliveryID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			rb, err := scanRyugiBinding(rows)
			if err != nil {
				return err
			}
			saved = append(saved, rb)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		ev, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + taskID,
			ProjectID:  projectID,
			TaskID:     taskID,
			ActorKind:  protocol.ActorRyugi,
			ActorID:    "ryugi-bridge",
			Kind:       protocol.EventDeliveryRyugiUpdated,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"deliveryId":     deliveryID,
				"ryugiSyncState": syncState,
				"bindingCount":   len(saved),
			},
			CreatedAt: now,
		})
		if err != nil {
			return err
		}
		emitted = &ev
		return nil
	})
	return saved, emitted, err
}

// ListRyugiBindings lists either all active bindings (deliveryID == "") or bindings for one delivery.
func (s *Store) ListRyugiBindings(ctx context.Context, deliveryID string, activeOnly bool) ([]RyugiBindingRecord, error) {
	query := `
		SELECT binding_id, delivery_id, project_id, task_id, frozen_project_dir,
		       artifact_locator, derived_artifact_id, open_url,
		       last_thread_receipt, last_changes_receipt, last_state_hashes,
		       active, created_at, updated_at
		FROM ryugi_bindings
	`
	var clauses []string
	var args []any
	if deliveryID != "" {
		clauses = append(clauses, "delivery_id = ?")
		args = append(args, deliveryID)
	}
	if activeOnly {
		clauses = append(clauses, "active = 1")
	}
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY updated_at DESC"

	rows, err := s.ro.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RyugiBindingRecord
	for rows.Next() {
		var rec RyugiBindingRecord
		var lastThread, lastChanges sql.NullString
		var hashesJSON string
		var active int
		if err := rows.Scan(
			&rec.BindingID, &rec.DeliveryID, &rec.ProjectID, &rec.TaskID, &rec.FrozenProjectDir,
			&rec.ArtifactLocator, &rec.DerivedArtifactID, &rec.OpenURL,
			&lastThread, &lastChanges, &hashesJSON,
			&active, &rec.CreatedAt, &rec.UpdatedAt,
		); err != nil {
			return nil, err
		}
		if lastThread.Valid {
			rec.LastThreadReceipt = lastThread.String
		}
		if lastChanges.Valid {
			rec.LastChangesReceipt = lastChanges.String
		}
		rec.Active = active != 0
		rec.LastStateHashes = map[string]StateHashEntry{}
		_ = json.Unmarshal([]byte(hashesJSON), &rec.LastStateHashes)
		out = append(out, rec)
	}
	return out, rows.Err()
}

// IncomingRyugiAnnotation is a candidate annotation extracted by RyugiBridge.
type IncomingRyugiAnnotation struct {
	ExternalKey string
	Anchor      protocol.AnnotationAnchor
	Body        string
}

// ApplyRyugiSyncBatch atomically persists new Ryugi annotations (INSERT OR IGNORE by external_key),
// emits annotation.created ONLY for actually inserted rows, and updates binding receipts/state hashes.
// RyugiBridge MUST call this and observe COMMIT success BEFORE acking any Ryugi receipt!
func (s *Store) ApplyRyugiSyncBatch(
	ctx context.Context,
	binding RyugiBindingRecord,
	incoming []IncomingRyugiAnnotation,
	newStateHashes map[string]StateHashEntry,
	threadReceipt string,
	changesReceipt string,
) (int, []protocol.EventRecord, error) {
	now := time.Now().UnixMilli()
	insertedCount := 0
	var emitted []protocol.EventRecord

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		for _, item := range incoming {
			annID := newID("ann")
			anchorBytes, err := json.Marshal(item.Anchor)
			if err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO annotations (
					annotation_id, project_id, task_id, delivery_id, source, external_key,
					anchor_json, body, status, created_at
				) VALUES (?, ?, ?, ?, 'ryugi', ?, ?, ?, 'open', ?)
			`, annID, binding.ProjectID, binding.TaskID, binding.DeliveryID,
				item.ExternalKey, string(anchorBytes), item.Body, now)
			if err != nil {
				return fmt.Errorf("insert ryugi annotation %s: %w", item.ExternalKey, err)
			}
			n, _ := res.RowsAffected()
			if n > 0 {
				insertedCount++
				ev, err := insertEventTx(ctx, tx, eventInsert{
					StreamID:   "task:" + binding.TaskID,
					ProjectID:  binding.ProjectID,
					TaskID:     binding.TaskID,
					ActorKind:  protocol.ActorRyugi,
					ActorID:    "ryugi-bridge",
					Kind:       protocol.EventAnnotationCreated,
					Visibility: protocol.VisibilityTimeline,
					Payload: map[string]any{
						"annotationId": annID,
						"deliveryId":   binding.DeliveryID,
						"source":       protocol.AnnotationSourceRyugi,
						"anchor":       item.Anchor,
						"body":         item.Body,
					},
					CreatedAt: now,
				})
				if err != nil {
					return err
				}
				emitted = append(emitted, ev)
			}
		}

		if newStateHashes == nil {
			newStateHashes = binding.LastStateHashes
		}
		if newStateHashes == nil {
			newStateHashes = map[string]StateHashEntry{}
		}
		keys := make([]string, 0, len(newStateHashes))
		for k := range newStateHashes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		hashesBytes, err := json.Marshal(newStateHashes)
		if err != nil {
			return err
		}

		finalThreadReceipt := binding.LastThreadReceipt
		if threadReceipt != "" {
			finalThreadReceipt = threadReceipt
		}
		finalChangesReceipt := binding.LastChangesReceipt
		if changesReceipt != "" {
			finalChangesReceipt = changesReceipt
		}
		var trVal, crVal any
		if finalThreadReceipt != "" {
			trVal = finalThreadReceipt
		}
		if finalChangesReceipt != "" {
			crVal = finalChangesReceipt
		}

		_, err = tx.ExecContext(ctx, `
			UPDATE ryugi_bindings
			SET last_thread_receipt = ?, last_changes_receipt = ?, last_state_hashes = ?, updated_at = ?
			WHERE binding_id = ?
		`, trVal, crVal, string(hashesBytes), now, binding.BindingID)
		return err
	})
	return insertedCount, emitted, err
}

// CompleteDeliveryReview atomically resolves the delivery_review inbox card for deliveryID
// and emits a delivery.review_completed inbox event so connected clients clear badges immediately.
func (s *Store) CompleteDeliveryReview(ctx context.Context, deliveryID, actorID string) (protocol.CompleteReviewResponse, *protocol.EventRecord, error) {
	var resp protocol.CompleteReviewResponse
	var emitted *protocol.EventRecord
	now := time.Now().UnixMilli()

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var projectID, taskID string
		err := tx.QueryRowContext(ctx, `SELECT project_id, task_id FROM deliveries WHERE delivery_id = ?`, deliveryID).Scan(&projectID, &taskID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}

		inboxID := "inb:dlv:" + deliveryID
		res, err := tx.ExecContext(ctx, `
			UPDATE inbox_items
			SET state = 'resolved', resolved_at = ?, updated_at = ?
			WHERE inbox_id = ? AND state = 'open'
		`, now, now, inboxID)
		if err != nil {
			return err
		}
		changed, _ := res.RowsAffected()

		if changed > 0 {
			if actorID == "" {
				actorID = "human"
			}
			ev, err := insertEventTx(ctx, tx, eventInsert{
				StreamID:   "task:" + taskID,
				ProjectID:  projectID,
				TaskID:     taskID,
				ActorKind:  protocol.ActorHuman,
				ActorID:    actorID,
				Kind:       protocol.EventDeliveryReviewCompleted,
				Visibility: protocol.VisibilityInbox,
				Payload: map[string]any{
					"deliveryId": deliveryID,
					"inboxId":    inboxID,
				},
				CreatedAt: now,
			})
			if err != nil {
				return err
			}
			emitted = &ev
		}

		resp = protocol.CompleteReviewResponse{
			DeliveryID: deliveryID,
			InboxID:    inboxID,
			InboxState: protocol.InboxResolved,
			ResolvedAt: now,
		}
		return nil
	})
	return resp, emitted, err
}

// PutBlobRecord registers a content-addressed blob in delivery_blobs with ref_count = 0.
// Delivery references increment ref_count once per distinct delivery sha256 inside CreateManualImportDelivery.
func (s *Store) PutBlobRecord(ctx context.Context, sha256Hex string, sizeBytes int64, storagePath string) error {
	now := time.Now().UnixMilli()
	return s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO delivery_blobs (sha256, size_bytes, storage_path, ref_count, created_at)
			VALUES (?, ?, ?, 0, ?)
			ON CONFLICT(sha256) DO NOTHING
		`, sha256Hex, sizeBytes, storagePath, now)
		return err
	})
}

// BlobRecord holds a row from delivery_blobs.
type BlobRecord struct {
	SHA256      string
	SizeBytes   int64
	StoragePath string
	RefCount    int
	CreatedAt   int64
}

// GetBlobRecord looks up a blob by sha256.
func (s *Store) GetBlobRecord(ctx context.Context, sha256Hex string) (BlobRecord, error) {
	var b BlobRecord
	err := s.ro.QueryRowContext(ctx, `
		SELECT sha256, size_bytes, storage_path, ref_count, created_at
		FROM delivery_blobs WHERE sha256 = ?
	`, sha256Hex).Scan(&b.SHA256, &b.SizeBytes, &b.StoragePath, &b.RefCount, &b.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNotFound
	}
	return b, err
}

// MaxEventSeq returns the current highest committed sequence number in events.
func (s *Store) MaxEventSeq(ctx context.Context) (int64, error) {
	var maxSeq int64
	err := s.ro.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM events`).Scan(&maxSeq)
	return maxSeq, err
}

// ListEventsPage returns one ascending page of committed non-raw events with seq > afterSeq
// (and seq <= upToSeq when upToSeq > 0), enabling unbounded gapless WebSocket replay & tailing.
func (s *Store) ListEventsPage(ctx context.Context, afterSeq, upToSeq int64, limit int) ([]protocol.EventRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var (
		rows *sql.Rows
		err  error
	)
	if upToSeq > 0 {
		rows, err = s.ro.QueryContext(ctx, `
			SELECT seq, event_id, stream_id, project_id, task_id, attempt_id,
			       actor_kind, actor_id, kind, visibility, payload_json, created_at
			FROM events
			WHERE seq > ? AND seq <= ? AND visibility != 'raw_acp'
			ORDER BY seq ASC
			LIMIT ?
		`, afterSeq, upToSeq, limit)
	} else {
		rows, err = s.ro.QueryContext(ctx, `
			SELECT seq, event_id, stream_id, project_id, task_id, attempt_id,
			       actor_kind, actor_id, kind, visibility, payload_json, created_at
			FROM events
			WHERE seq > ? AND visibility != 'raw_acp'
			ORDER BY seq ASC
			LIMIT ?
		`, afterSeq, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []protocol.EventRecord
	for rows.Next() {
		ev, err := scanEventRecord(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	return events, rows.Err()
}

type eventInsert struct {
	EventID    string
	StreamID   string
	ProjectID  string
	TaskID     string
	AttemptID  string
	ActorKind  protocol.ActorKind
	ActorID    string
	Kind       protocol.EventKind
	Visibility protocol.EventVisibility
	Payload    map[string]any
	CreatedAt  int64
}

func insertEventTx(ctx context.Context, tx *sql.Tx, e eventInsert) (protocol.EventRecord, error) {
	if e.EventID == "" {
		e.EventID = newID("evt")
	}
	if e.CreatedAt == 0 {
		e.CreatedAt = time.Now().UnixMilli()
	}
	if e.Payload == nil {
		e.Payload = map[string]any{}
	}
	payloadBytes, err := json.Marshal(e.Payload)
	if err != nil {
		return protocol.EventRecord{}, err
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
		INSERT INTO events (
			event_id, stream_id, project_id, task_id, attempt_id,
			actor_kind, actor_id, kind, visibility, payload_json, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, e.EventID, e.StreamID, pid, tid, aid,
		string(e.ActorKind), e.ActorID, string(e.Kind), string(e.Visibility),
		string(payloadBytes), e.CreatedAt)
	if err != nil {
		return protocol.EventRecord{}, fmt.Errorf("insert event %s: %w", e.Kind, err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return protocol.EventRecord{}, err
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
	}, nil
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func resolveDeliveryKind(kind protocol.DeliveryKind, manifest protocol.DeliveryManifest) protocol.DeliveryKind {
	if kind != "" {
		return kind
	}
	switch {
	case len(manifest.RyugiArtifacts) > 0 && manifest.FilesChanged > 0:
		return protocol.DeliveryMixed
	case len(manifest.RyugiArtifacts) > 0:
		return protocol.DeliveryRyugiArtifact
	default:
		return protocol.DeliveryCodeDiff
	}
}

func normalizeManifest(m protocol.DeliveryManifest) protocol.DeliveryManifest {
	out := protocol.DeliveryManifest{
		FilesChanged: m.FilesChanged,
		Insertions:   m.Insertions,
		Deletions:    m.Deletions,
		WorktreePath: m.WorktreePath,
	}
	if len(m.Blobs) > 0 {
		out.Blobs = append([]protocol.BlobManifestItem(nil), m.Blobs...)
		sort.Slice(out.Blobs, func(i, j int) bool {
			if out.Blobs[i].Path != out.Blobs[j].Path {
				return out.Blobs[i].Path < out.Blobs[j].Path
			}
			if out.Blobs[i].SHA256 != out.Blobs[j].SHA256 {
				return out.Blobs[i].SHA256 < out.Blobs[j].SHA256
			}
			return out.Blobs[i].Size < out.Blobs[j].Size
		})
	}
	if len(m.RyugiArtifacts) > 0 {
		out.RyugiArtifacts = append([]string(nil), m.RyugiArtifacts...)
		sort.Strings(out.RyugiArtifacts)
	}
	return out
}

func canonicalManifestJSON(m protocol.DeliveryManifest) string {
	b, _ := json.Marshal(normalizeManifest(m))
	return string(b)
}
