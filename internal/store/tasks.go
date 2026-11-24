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

// AttemptRecord extends AttemptView with internal scheduling & lease fields.
type AttemptRecord struct {
	protocol.AttemptView
	WBSessionID        string
	ProviderSessionID  string
	SessionCWD         string
	SessionLocked      bool
	InputDeliveryID    string
	TriggerPayloadJSON string
	DesiredConfig      map[string]string
	RequiredResources  []string
	LeaseToken         string
	LeaseExpiresAt     int64
	LastAckedOutbox    int64
	RunnerInstanceID   string
	ResumeState        string
	TargetEnv          string
	DeployPhase        string
}

// AgentSessionRecord stores an underlying ACP session row.
type AgentSessionRecord struct {
	WBSessionID       string
	TaskID            string
	ProjectID         string
	MachineID         string
	Adapter           protocol.SessionAdapter
	ProviderSessionID string
	CWD               string
	DesiredConfigJSON string
	RuntimeState      protocol.SessionRuntimeState
	Locked            bool
	LastActivityAt    int64
	CreatedAt         int64
}

// CreateTaskParams specifies validated parameters for atomic task (+ optional attempt) creation.
type CreateTaskParams struct {
	ProjectID          string
	ParentTaskID       string
	Title              string
	Goal               string
	RequiredCapability string
	RequiredResources  []string
	Adapter            protocol.Adapter
	DesiredConfig      map[string]string
	CreatedBy          protocol.TaskCreatedBy
	IdempotencyKey     string
	Sources            []protocol.CreateTaskSourceInput
	MachineID          string
	WorktreePath       string
	BaseSHA            string
	InputDeliveryID    string
	Prompt             string
	TriggerKind        protocol.TriggerKind
	TriggerPayload     map[string]any
	CreateAttempt      bool
	LeaseTTLMs         int64
	ActorID            string
	ActorKind          protocol.ActorKind
}

// CreateTaskResult returns created (or idempotently found) entities and events.
type CreateTaskResult struct {
	Task      protocol.TaskSummary
	Attempt   *AttemptRecord
	Sources   []protocol.TaskSource
	Created   bool
	Events    []protocol.EventRecord
}

// CreateTask atomically inserts a new Task (including _scratch), optional TaskSources,
// optional initial leased Attempt + AgentSession, and emits timeline events.
func (s *Store) CreateTask(ctx context.Context, p CreateTaskParams) (CreateTaskResult, error) {
	var res CreateTaskResult
	now := time.Now().UnixMilli()
	if p.LeaseTTLMs <= 0 {
		p.LeaseTTLMs = 45000
	}
	if p.CreatedBy == "" {
		p.CreatedBy = protocol.TaskCreatedByHuman
	}
	if p.ActorKind == "" {
		p.ActorKind = protocol.ActorHuman
	}
	if p.ActorID == "" {
		p.ActorID = string(p.ActorKind)
	}
	if p.TriggerKind == "" {
		p.TriggerKind = protocol.TriggerInitial
	}
	if p.RequiredCapability == "" {
		p.RequiredCapability = "primary-dev"
	}
	if p.Adapter == "" {
		p.Adapter = protocol.AdapterAgy
	}
	if p.DesiredConfig == nil {
		p.DesiredConfig = map[string]string{}
	}
	if p.RequiredResources == nil {
		p.RequiredResources = []string{}
	}

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		// Verify project exists
		var exists string
		if err := tx.QueryRowContext(ctx, `SELECT project_id FROM projects WHERE project_id = ? AND archived = 0`, p.ProjectID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: project %s", ErrNotFound, p.ProjectID)
			}
			return err
		}

		// Idempotency key check
		if p.IdempotencyKey != "" {
			var existingTaskID string
			err := tx.QueryRowContext(ctx, `SELECT task_id FROM tasks WHERE idempotency_key = ?`, p.IdempotencyKey).Scan(&existingTaskID)
			if err == nil {
				task, err := loadTaskSummaryTx(ctx, tx, existingTaskID)
				if err != nil {
					return err
				}
				if task.ProjectID != p.ProjectID {
					return fmt.Errorf("%w: idempotency key already bound to project %s", ErrConflict, task.ProjectID)
				}
				res.Task = task
				res.Created = false
				if task.ActiveAttemptID != "" {
					if att, err := loadAttemptRecordTx(ctx, tx, task.ActiveAttemptID); err == nil {
						res.Attempt = &att
					}
				}
				srcs, _ := listTaskSourcesTx(ctx, tx, existingTaskID)
				res.Sources = srcs
				return nil
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}

		// Verify parent task exists if provided
		var parentVal any
		if p.ParentTaskID != "" {
			var pid string
			if err := tx.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE task_id = ?`, p.ParentTaskID).Scan(&pid); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("%w: parent task %s", ErrNotFound, p.ParentTaskID)
				}
				return err
			}
			parentVal = p.ParentTaskID
		}

		title := strings.TrimSpace(p.Title)
		if title == "" {
			title = strings.TrimSpace(p.Goal)
			if nl := strings.IndexByte(title, '\n'); nl > 0 {
				title = title[:nl]
			}
			if len([]rune(title)) > 80 {
				title = string([]rune(title)[:80])
			}
		}
		if title == "" {
			title = "Untitled Task"
		}

		reqResJSON, err := json.Marshal(p.RequiredResources)
		if err != nil {
			return err
		}
		cfgJSON, err := json.Marshal(p.DesiredConfig)
		if err != nil {
			return err
		}
		var idempVal any
		if p.IdempotencyKey != "" {
			idempVal = p.IdempotencyKey
		}

		taskID := newID("tsk")
		_, err = tx.ExecContext(ctx, `
			INSERT INTO tasks (
				task_id, project_id, parent_task_id, title, goal,
				required_capability, required_resources, preferred_adapter, desired_config_json,
				state, created_by, idempotency_key, version, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?, 1, ?, ?)
		`, taskID, p.ProjectID, parentVal, title, p.Goal,
			p.RequiredCapability, string(reqResJSON), string(p.Adapter), string(cfgJSON),
			string(p.CreatedBy), idempVal, now, now)
		if err != nil {
			return fmt.Errorf("insert task: %w", err)
		}

		// Insert optional initial task_sources
		res.Sources = []protocol.TaskSource{}
		for _, src := range p.Sources {
			if !src.Kind.IsValid() || strings.TrimSpace(src.URIOrRef) == "" {
				return fmt.Errorf("%w: invalid source %+v", ErrInvalidInput, src)
			}
			label := strings.TrimSpace(src.Label)
			if label == "" {
				label = src.URIOrRef
			}
			srcID := newID("src")
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO task_sources (source_id, task_id, project_id, kind, label, uri_or_ref, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)
			`, srcID, taskID, p.ProjectID, string(src.Kind), label, src.URIOrRef, now); err != nil {
				return fmt.Errorf("insert task source: %w", err)
			}
			res.Sources = append(res.Sources, protocol.TaskSource{
				SourceID:  srcID,
				TaskID:    taskID,
				ProjectID: p.ProjectID,
				Kind:      src.Kind,
				Label:     label,
				URIOrRef:  src.URIOrRef,
				CreatedAt: now,
			})
		}

		evTask, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + taskID,
			ProjectID:  p.ProjectID,
			TaskID:     taskID,
			ActorKind:  p.ActorKind,
			ActorID:    p.ActorID,
			Kind:       protocol.EventTaskCreated,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"title":              title,
				"goal":               p.Goal,
				"requiredCapability": p.RequiredCapability,
				"adapter":            p.Adapter,
				"parentTaskId":       p.ParentTaskID,
			},
			CreatedAt: now,
		})
		if err != nil {
			return err
		}
		res.Events = append(res.Events, evTask)

		if p.CreateAttempt {
			att, evAtt, err := createAttemptInTx(ctx, tx, createAttemptTxParams{
				TaskID:          taskID,
				ProjectID:       p.ProjectID,
				MachineID:       p.MachineID,
				Adapter:         p.Adapter,
				TriggerKind:     p.TriggerKind,
				TriggerPayload:  p.TriggerPayload,
				Prompt:          p.Prompt,
				WorktreePath:    p.WorktreePath,
				BaseSHA:         p.BaseSHA,
				InputDeliveryID: p.InputDeliveryID,
				DesiredConfig:   p.DesiredConfig,
				ForceNewSession: true,
				LeaseTTLMs:      p.LeaseTTLMs,
				ActorKind:       p.ActorKind,
				ActorID:         p.ActorID,
				Now:             now,
			})
			if err != nil {
				return err
			}
			res.Attempt = &att
			res.Events = append(res.Events, evAtt)
		}

		task, err := loadTaskSummaryTx(ctx, tx, taskID)
		if err != nil {
			return err
		}
		res.Task = task
		res.Created = true
		return nil
	})
	return res, err
}

// BindScratchTaskToProject atomically migrates a task from the built-in _scratch virtual
// project to a real logical project while enforcing all composite foreign keys at COMMIT.
func (s *Store) BindScratchTaskToProject(ctx context.Context, taskID, targetProjectID, actorID string) (protocol.TaskSummary, *protocol.EventRecord, error) {
	var out protocol.TaskSummary
	var emitted *protocol.EventRecord
	now := time.Now().UnixMilli()
	if actorID == "" {
		actorID = "human"
	}

	if targetProjectID == "" || targetProjectID == protocol.ScratchProjectID {
		return out, nil, fmt.Errorf("%w: target projectId must be a real project", ErrInvalidInput)
	}

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var curProject string
		var activeAtt sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT project_id, active_attempt_id FROM tasks WHERE task_id = ?`, taskID).Scan(&curProject, &activeAtt)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: task %s", ErrNotFound, taskID)
		}
		if err != nil {
			return err
		}
		if curProject != protocol.ScratchProjectID {
			return fmt.Errorf("%w: task %s is already bound to project %s", ErrConflict, taskID, curProject)
		}
		if activeAtt.Valid && activeAtt.String != "" {
			return fmt.Errorf("%w: cannot rebind task %s while attempt %s is active", ErrConflict, taskID, activeAtt.String)
		}

		var exists string
		if err := tx.QueryRowContext(ctx, `SELECT project_id FROM projects WHERE project_id = ? AND archived = 0`, targetProjectID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: project %s", ErrNotFound, targetProjectID)
			}
			return err
		}

		// Defer composite FK verification to COMMIT so parent + all child tables update atomically.
		if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys = ON`); err != nil {
			return fmt.Errorf("defer foreign keys: %w", err)
		}

		tables := []string{
			"tasks",
			"agent_sessions",
			"attempts",
			"task_pending_inputs",
			"deliveries",
			"ryugi_bindings",
			"annotations",
			"approvals",
			"inbox_items",
			"task_sources",
			"board_messages",
			"board_members",
			"events",
		}
		for _, tbl := range tables {
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET project_id = ? WHERE task_id = ?`, tbl), targetProjectID, taskID); err != nil {
				return fmt.Errorf("rebind %s: %w", tbl, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET version = version + 1, updated_at = ? WHERE task_id = ?`, now, taskID); err != nil {
			return err
		}

		ev, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + taskID,
			ProjectID:  targetProjectID,
			TaskID:     taskID,
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventTaskProjectBound,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"fromProjectId": protocol.ScratchProjectID,
				"toProjectId":   targetProjectID,
			},
			CreatedAt: now,
		})
		if err != nil {
			return err
		}
		emitted = &ev

		task, err := loadTaskSummaryTx(ctx, tx, taskID)
		if err != nil {
			return err
		}
		out = task
		return nil
	})
	return out, emitted, err
}

// CreateNextAttemptParams configures creating a subsequent Attempt for an existing Task.
type CreateNextAttemptParams struct {
	TaskID          string
	MachineID       string
	Adapter         protocol.Adapter
	TriggerKind     protocol.TriggerKind
	TriggerPayload  map[string]any
	Prompt          string
	WorktreePath    string
	BaseSHA         string
	InputDeliveryID string
	DesiredConfig   map[string]string
	ForceNewSession bool
	VerifiedSite    bool
	LeaseTTLMs      int64
	ActorKind       protocol.ActorKind
	ActorID         string
}

// CreateNextAttempt atomically starts a new Attempt for an idle Task.
// Rejects with ErrConflict if an active attempt already exists, or ErrVerificationRequired
// when retrying a needs_verification attempt without verifiedSite=true.
func (s *Store) CreateNextAttempt(ctx context.Context, p CreateNextAttemptParams) (protocol.TaskSummary, AttemptRecord, []protocol.EventRecord, error) {
	var task protocol.TaskSummary
	var att AttemptRecord
	var events []protocol.EventRecord
	now := time.Now().UnixMilli()
	if p.LeaseTTLMs <= 0 {
		p.LeaseTTLMs = 45000
	}
	if p.ActorKind == "" {
		p.ActorKind = protocol.ActorHuman
	}
	if p.ActorID == "" {
		p.ActorID = string(p.ActorKind)
	}

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var projectID string
		var activeAttID, latestDlvID, desiredCfgJSON sql.NullString
		var prefAdapter string
		err := tx.QueryRowContext(ctx, `
			SELECT project_id, active_attempt_id, latest_delivery_id, preferred_adapter, desired_config_json
			FROM tasks WHERE task_id = ?
		`, p.TaskID).Scan(&projectID, &activeAttID, &latestDlvID, &prefAdapter, &desiredCfgJSON)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: task %s", ErrNotFound, p.TaskID)
		}
		if err != nil {
			return err
		}
		if activeAttID.Valid && activeAttID.String != "" {
			return fmt.Errorf("%w: task %s already has active attempt %s", ErrConflict, p.TaskID, activeAttID.String)
		}

		// Inspect latest attempt for retry verification guard and base delivery continuity
		var lastAttemptID, lastAttemptState string
		var lastWorktree, lastBaseSHA sql.NullString
		err = tx.QueryRowContext(ctx, `
			SELECT attempt_id, state, worktree_path, base_sha
			FROM attempts WHERE task_id = ?
			ORDER BY attempt_no DESC LIMIT 1
		`, p.TaskID).Scan(&lastAttemptID, &lastAttemptState, &lastWorktree, &lastBaseSHA)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		if p.TriggerKind == protocol.TriggerRetry && lastAttemptState == string(protocol.AttemptNeedsVerification) && !p.VerifiedSite {
			return fmt.Errorf("%w: previous attempt %s is in needs_verification; pass verifiedSite=true after inspecting state", ErrVerificationRequired, lastAttemptID)
		}

		adapter := p.Adapter
		if adapter == "" {
			adapter = protocol.Adapter(prefAdapter)
		}
		desiredCfg := p.DesiredConfig
		if len(desiredCfg) == 0 && desiredCfgJSON.Valid {
			desiredCfg = map[string]string{}
			_ = json.Unmarshal([]byte(desiredCfgJSON.String), &desiredCfg)
		}
		inputDeliveryID := p.InputDeliveryID
		if inputDeliveryID == "" && latestDlvID.Valid {
			inputDeliveryID = latestDlvID.String
		}

		// Resolve any previous blocked inbox card for the last attempt when retrying/continuing
		if lastAttemptID != "" {
			_, _ = tx.ExecContext(ctx, `
				UPDATE inbox_items SET state = 'resolved', resolved_at = ?, updated_at = ?
				WHERE inbox_id = ? AND state = 'open'
			`, now, now, "inb:blk:"+lastAttemptID)
		}

		createdAtt, evAtt, err := createAttemptInTx(ctx, tx, createAttemptTxParams{
			TaskID:          p.TaskID,
			ProjectID:       projectID,
			MachineID:       p.MachineID,
			Adapter:         adapter,
			TriggerKind:     p.TriggerKind,
			TriggerPayload:  p.TriggerPayload,
			Prompt:          p.Prompt,
			WorktreePath:    p.WorktreePath,
			BaseSHA:         p.BaseSHA,
			InputDeliveryID: inputDeliveryID,
			DesiredConfig:   desiredCfg,
			ForceNewSession: p.ForceNewSession,
			LeaseTTLMs:      p.LeaseTTLMs,
			ActorKind:       p.ActorKind,
			ActorID:         p.ActorID,
			Now:             now,
		})
		if err != nil {
			return err
		}
		att = createdAtt
		events = append(events, evAtt)

		task, err = loadTaskSummaryTx(ctx, tx, p.TaskID)
		return err
	})
	return task, att, events, err
}

type createAttemptTxParams struct {
	TaskID          string
	ProjectID       string
	MachineID       string
	Adapter         protocol.Adapter
	TriggerKind     protocol.TriggerKind
	TriggerPayload  map[string]any
	Prompt          string
	WorktreePath    string
	BaseSHA         string
	InputDeliveryID string
	DesiredConfig   map[string]string
	ForceNewSession bool
	LeaseTTLMs      int64
	ActorKind       protocol.ActorKind
	ActorID         string
	Now             int64
}

func createAttemptInTx(ctx context.Context, tx *sql.Tx, p createAttemptTxParams) (AttemptRecord, protocol.EventRecord, error) {
	if p.Now == 0 {
		p.Now = time.Now().UnixMilli()
	}
	if p.LeaseTTLMs <= 0 {
		p.LeaseTTLMs = 45000
	}
	if p.DesiredConfig == nil {
		p.DesiredConfig = map[string]string{}
	}
	if p.TriggerPayload == nil {
		p.TriggerPayload = map[string]any{}
	}
	if p.Prompt != "" {
		if _, ok := p.TriggerPayload["prompt"]; !ok {
			p.TriggerPayload["prompt"] = p.Prompt
		}
	}

	machineID := p.MachineID
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
			`, machineID, p.Now, p.Now)
			if err != nil {
				return AttemptRecord{}, protocol.EventRecord{}, err
			}
		} else if err != nil {
			return AttemptRecord{}, protocol.EventRecord{}, err
		}
	}

	var nextAttemptNo int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(attempt_no), 0) + 1 FROM attempts WHERE task_id = ?`, p.TaskID).Scan(&nextAttemptNo); err != nil {
		return AttemptRecord{}, protocol.EventRecord{}, err
	}

	cfgBytes, err := json.Marshal(p.DesiredConfig)
	if err != nil {
		return AttemptRecord{}, protocol.EventRecord{}, err
	}
	payloadBytes, err := json.Marshal(p.TriggerPayload)
	if err != nil {
		return AttemptRecord{}, protocol.EventRecord{}, err
	}

	// Resolve or create wb_session_id for ACP adapters
	var wbSessionVal any
	var wbSessionID string
	var providerSessionID string
	var sessionCWD string
	if protocol.SessionAdapter(p.Adapter).IsValid() {
		reused := false
		if !p.ForceNewSession {
			var sid, cwd, rstate string
			var provSID sql.NullString
			var locked int
			err := tx.QueryRowContext(ctx, `
				SELECT wb_session_id, provider_session_id, cwd, runtime_state, locked
				FROM agent_sessions
				WHERE task_id = ? AND machine_id = ? AND adapter = ?
				ORDER BY created_at DESC LIMIT 1
			`, p.TaskID, machineID, string(p.Adapter)).Scan(&sid, &provSID, &cwd, &rstate, &locked)
			if err == nil && locked == 0 && rstate != string(protocol.SessionDead) {
				if p.WorktreePath == "" || cwd == p.WorktreePath {
					wbSessionID = sid
					if provSID.Valid {
						providerSessionID = provSID.String
					}
					sessionCWD = cwd
					reused = true
				}
			}
		}
		if !reused {
			wbSessionID = newID("ses")
			sessionCWD = p.WorktreePath
			_, err := tx.ExecContext(ctx, `
				INSERT INTO agent_sessions (
					wb_session_id, task_id, project_id, machine_id, adapter,
					cwd, desired_config_json, runtime_state, locked, last_activity_at, created_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, 'starting', 0, ?, ?)
			`, wbSessionID, p.TaskID, p.ProjectID, machineID, string(p.Adapter),
				sessionCWD, string(cfgBytes), p.Now, p.Now)
			if err != nil {
				return AttemptRecord{}, protocol.EventRecord{}, fmt.Errorf("insert agent_session: %w", err)
			}
		}
		wbSessionVal = wbSessionID
	}

	attemptID := newID("att")
	leaseToken := randomHex(16)
	leaseExpiresAt := p.Now + p.LeaseTTLMs

	var worktreeVal, baseSHAVal, inputDlvVal any
	if p.WorktreePath != "" {
		worktreeVal = p.WorktreePath
	} else if sessionCWD != "" {
		worktreeVal = sessionCWD
	}
	if p.BaseSHA != "" {
		baseSHAVal = p.BaseSHA
	}
	if p.InputDeliveryID != "" {
		inputDlvVal = p.InputDeliveryID
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO attempts (
			attempt_id, task_id, project_id, attempt_no, machine_id, adapter,
			wb_session_id, trigger_kind, trigger_payload_json, worktree_path,
			input_delivery_id, base_sha, state, lease_token, lease_expires_at,
			last_acked_outbox, started_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'leased', ?, ?, 0, ?)
	`, attemptID, p.TaskID, p.ProjectID, nextAttemptNo, machineID, string(p.Adapter),
		wbSessionVal, string(p.TriggerKind), string(payloadBytes), worktreeVal,
		inputDlvVal, baseSHAVal, leaseToken, leaseExpiresAt, p.Now)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return AttemptRecord{}, protocol.EventRecord{}, fmt.Errorf("%w: %v", ErrConflict, err)
		}
		return AttemptRecord{}, protocol.EventRecord{}, fmt.Errorf("insert attempt: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE tasks
		SET state = 'queued', active_attempt_id = ?, waiting_reason = NULL, version = version + 1, updated_at = ?
		WHERE task_id = ?
	`, attemptID, p.Now, p.TaskID)
	if err != nil {
		return AttemptRecord{}, protocol.EventRecord{}, fmt.Errorf("update task active_attempt_id: %w", err)
	}

	ev, err := insertEventTx(ctx, tx, eventInsert{
		StreamID:   "task:" + p.TaskID,
		ProjectID:  p.ProjectID,
		TaskID:     p.TaskID,
		AttemptID:  attemptID,
		ActorKind:  p.ActorKind,
		ActorID:    p.ActorID,
		Kind:       protocol.EventAttemptStarted,
		Visibility: protocol.VisibilityTimeline,
		Payload: map[string]any{
			"attemptNo":   nextAttemptNo,
			"machineId":   machineID,
			"adapter":     p.Adapter,
			"triggerKind": p.TriggerKind,
			"wbSessionId": wbSessionID,
		},
		CreatedAt: p.Now,
	})
	if err != nil {
		return AttemptRecord{}, protocol.EventRecord{}, err
	}

	att, err := loadAttemptRecordTx(ctx, tx, attemptID)
	if err != nil {
		return AttemptRecord{}, protocol.EventRecord{}, err
	}
	att.ProviderSessionID = providerSessionID
	att.SessionCWD = sessionCWD
	att.DesiredConfig = p.DesiredConfig
	return att, ev, nil
}

// GetTask loads a single TaskSummary by taskID.
func (s *Store) GetTask(ctx context.Context, taskID string) (protocol.TaskSummary, error) {
	var out protocol.TaskSummary
	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = loadTaskSummaryTx(ctx, tx, taskID)
		return err
	})
	return out, err
}

func loadTaskSummaryTx(ctx context.Context, tx *sql.Tx, taskID string) (protocol.TaskSummary, error) {
	row := tx.QueryRowContext(ctx, taskSummarySelectSQL+` WHERE t.task_id = ?`, taskID)
	ts, err := scanTaskSummary(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ts, fmt.Errorf("%w: task %s", ErrNotFound, taskID)
	}
	return ts, err
}

// GetAttemptRecord loads full attempt + session metadata for dispatching or inspection.
func (s *Store) GetAttemptRecord(ctx context.Context, attemptID string) (AttemptRecord, error) {
	var out AttemptRecord
	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = loadAttemptRecordTx(ctx, tx, attemptID)
		return err
	})
	return out, err
}

func loadAttemptRecordTx(ctx context.Context, tx *sql.Tx, attemptID string) (AttemptRecord, error) {
	var a AttemptRecord
	var adapter, trigger, state string
	var wbSessID, worktree, inputDlv, baseSHA, outDlv, compSrc, errCode, errDetail sql.NullString
	var runnerInst, resumeState, targetEnv, deployPhase sql.NullString
	var provSID, sessCWD, taskCfgJSON, taskReqResJSON sql.NullString
	var sessLocked sql.NullInt64
	var finishedAt sql.NullInt64

	err := tx.QueryRowContext(ctx, `
		SELECT a.attempt_id, a.task_id, a.project_id, a.attempt_no, a.machine_id, a.adapter,
		       a.wb_session_id, a.trigger_kind, a.trigger_payload_json, a.worktree_path,
		       a.input_delivery_id, a.base_sha, a.output_delivery_id, a.target_env, a.deploy_phase,
		       a.state, a.resume_state, a.completion_source, a.lease_token, a.lease_expires_at,
		       a.last_acked_outbox, a.runner_instance_id, a.error_code, a.error_detail,
		       a.started_at, a.finished_at,
		       s.provider_session_id, s.cwd, s.locked,
		       t.desired_config_json, t.required_resources
		FROM attempts a
		LEFT JOIN agent_sessions s ON s.wb_session_id = a.wb_session_id
		LEFT JOIN tasks t ON t.task_id = a.task_id
		WHERE a.attempt_id = ?
	`, attemptID).Scan(
		&a.AttemptID, &a.TaskID, &a.ProjectID, &a.AttemptNo, &a.MachineID, &adapter,
		&wbSessID, &trigger, &a.TriggerPayloadJSON, &worktree,
		&inputDlv, &baseSHA, &outDlv, &targetEnv, &deployPhase,
		&state, &resumeState, &compSrc, &a.LeaseToken, &a.LeaseExpiresAt,
		&a.LastAckedOutbox, &runnerInst, &errCode, &errDetail,
		&a.StartedAt, &finishedAt,
		&provSID, &sessCWD, &sessLocked,
		&taskCfgJSON, &taskReqResJSON,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return a, fmt.Errorf("%w: attempt %s", ErrNotFound, attemptID)
	}
	if err != nil {
		return a, err
	}
	a.Adapter = protocol.Adapter(adapter)
	a.TriggerKind = protocol.TriggerKind(trigger)
	a.State = protocol.AttemptState(state)
	if wbSessID.Valid {
		a.WBSessionID = wbSessID.String
	}
	if worktree.Valid {
		a.WorktreePath = worktree.String
	}
	if inputDlv.Valid {
		a.InputDeliveryID = inputDlv.String
	}
	if baseSHA.Valid {
		a.BaseSHA = baseSHA.String
	}
	if outDlv.Valid {
		a.OutputDeliveryID = outDlv.String
	}
	if targetEnv.Valid {
		a.TargetEnv = targetEnv.String
	}
	if deployPhase.Valid {
		a.DeployPhase = deployPhase.String
	}
	if resumeState.Valid {
		a.ResumeState = resumeState.String
	}
	if compSrc.Valid {
		cs := protocol.CompletionSource(compSrc.String)
		a.CompletionSource = &cs
	}
	if runnerInst.Valid {
		a.RunnerInstanceID = runnerInst.String
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
	if provSID.Valid {
		a.ProviderSessionID = provSID.String
	}
	if sessCWD.Valid {
		a.SessionCWD = sessCWD.String
	}
	if sessLocked.Valid {
		a.SessionLocked = sessLocked.Int64 != 0
	}
	a.DesiredConfig = map[string]string{}
	if taskCfgJSON.Valid && taskCfgJSON.String != "" {
		_ = json.Unmarshal([]byte(taskCfgJSON.String), &a.DesiredConfig)
	}
	a.RequiredResources = []string{}
	if taskReqResJSON.Valid && taskReqResJSON.String != "" {
		_ = json.Unmarshal([]byte(taskReqResJSON.String), &a.RequiredResources)
	}
	return a, nil
}

// ListLeasedAttemptsForMachine lists attempts currently in 'leased' state for machineID.
func (s *Store) ListLeasedAttemptsForMachine(ctx context.Context, machineID string) ([]AttemptRecord, error) {
	var out []AttemptRecord
	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT attempt_id FROM attempts
			WHERE machine_id = ? AND state = 'leased'
			ORDER BY started_at ASC
		`, machineID)
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
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		for _, id := range ids {
			rec, err := loadAttemptRecordTx(ctx, tx, id)
			if err != nil {
				return err
			}
			out = append(out, rec)
		}
		return nil
	})
	return out, err
}

// ListAttemptRawEvents returns raw_acp (and attempt-scoped) events lazily for one attempt.
func (s *Store) ListAttemptRawEvents(ctx context.Context, attemptID string, afterSeq int64, limit int) ([]protocol.EventRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	out := []protocol.EventRecord{}
	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		var exists string
		if err := tx.QueryRowContext(ctx, `SELECT attempt_id FROM attempts WHERE attempt_id = ?`, attemptID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: attempt %s", ErrNotFound, attemptID)
			}
			return err
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT seq, event_id, stream_id, project_id, task_id, attempt_id,
			       actor_kind, actor_id, kind, visibility, payload_json, created_at
			FROM events
			WHERE attempt_id = ? AND visibility = 'raw_acp' AND seq > ?
			ORDER BY seq ASC
			LIMIT ?
		`, attemptID, afterSeq, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			ev, err := scanEventRecord(rows)
			if err != nil {
				return err
			}
			out = append(out, ev)
		}
		return rows.Err()
	})
	return out, err
}

// CancelTaskResult reports how a cancel request was handled.
type CancelTaskResult struct {
	Attempt          AttemptRecord
	CancelledNow     bool
	NeedsRunnerSend  bool
	Events           []protocol.EventRecord
}

// CancelTaskActiveAttempt either immediately cancels an unstarted/disconnected attempt
// or returns the active attempt so Hub can send attempt.cancel to the connected Runner.
func (s *Store) CancelTaskActiveAttempt(ctx context.Context, taskID, actorID, reason string, forceImmediate bool) (CancelTaskResult, error) {
	var res CancelTaskResult
	now := time.Now().UnixMilli()
	if actorID == "" {
		actorID = "human"
	}

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var activeAttID sql.NullString
		var projectID string
		err := tx.QueryRowContext(ctx, `SELECT project_id, active_attempt_id FROM tasks WHERE task_id = ?`, taskID).Scan(&projectID, &activeAttID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: task %s", ErrNotFound, taskID)
		}
		if err != nil {
			return err
		}
		if !activeAttID.Valid || activeAttID.String == "" {
			return fmt.Errorf("%w: task %s has no active attempt", ErrConflict, taskID)
		}

		att, err := loadAttemptRecordTx(ctx, tx, activeAttID.String)
		if err != nil {
			return err
		}
		res.Attempt = att

		if !forceImmediate && att.State != protocol.AttemptLeased && att.State != protocol.AttemptWaitingResource {
			res.NeedsRunnerSend = true
			return nil
		}

		if err := cancelAttemptTx(ctx, tx, att, actorID, reason, now, &res.Events); err != nil {
			return err
		}
		res.CancelledNow = true
		res.NeedsRunnerSend = att.State != protocol.AttemptLeased
		return nil
	})
	return res, err
}

func cancelAttemptTx(ctx context.Context, tx *sql.Tx, att AttemptRecord, actorID, reason string, now int64, events *[]protocol.EventRecord) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE attempts
		SET state = 'cancelled', error_code = 'CANCELLED', error_detail = ?, finished_at = ?
		WHERE attempt_id = ?
	`, reason, now, att.AttemptID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET state = 'cancelled', active_attempt_id = NULL, waiting_reason = NULL, version = version + 1, updated_at = ?
		WHERE task_id = ? AND active_attempt_id = ?
	`, now, att.TaskID, att.AttemptID); err != nil {
		return err
	}
	// Release any queued annotations tied to this attempt back to 'open'
	if err := releaseQueuedAnnotationsTx(ctx, tx, att.AttemptID); err != nil {
		return err
	}
	// Expire any pending approvals on this attempt & resolve their inbox cards
	if err := expirePendingApprovalsForAttemptTx(ctx, tx, att.AttemptID, now); err != nil {
		return err
	}
	_, _ = tx.ExecContext(ctx, `
		UPDATE inbox_items SET state = 'resolved', resolved_at = ?, updated_at = ?
		WHERE inbox_id = ? AND state = 'open'
	`, now, now, "inb:blk:"+att.AttemptID)

	ev1, err := insertEventTx(ctx, tx, eventInsert{
		StreamID:   "task:" + att.TaskID,
		ProjectID:  att.ProjectID,
		TaskID:     att.TaskID,
		AttemptID:  att.AttemptID,
		ActorKind:  protocol.ActorHuman,
		ActorID:    actorID,
		Kind:       protocol.EventAttemptFinished,
		Visibility: protocol.VisibilityTimeline,
		Payload: map[string]any{
			"state":  protocol.AttemptCancelled,
			"reason": reason,
		},
		CreatedAt: now,
	})
	if err != nil {
		return err
	}
	ev2, err := insertEventTx(ctx, tx, eventInsert{
		StreamID:   "task:" + att.TaskID,
		ProjectID:  att.ProjectID,
		TaskID:     att.TaskID,
		AttemptID:  att.AttemptID,
		ActorKind:  protocol.ActorHuman,
		ActorID:    actorID,
		Kind:       protocol.EventTaskCancelled,
		Visibility: protocol.VisibilityTimeline,
		Payload: map[string]any{
			"reason": reason,
		},
		CreatedAt: now,
	})
	if err != nil {
		return err
	}
	*events = append(*events, ev1, ev2)
	return nil
}

// ScanExpiredAttempts marks all active attempts whose lease_expires_at < nowMs as expired,
// releases queued annotations back to open, clears active_attempt_id, and records task_blocked inbox cards.
func (s *Store) ScanExpiredAttempts(ctx context.Context, nowMs int64) ([]string, []protocol.EventRecord, error) {
	if nowMs == 0 {
		nowMs = time.Now().UnixMilli()
	}
	var expiredIDs []string
	var events []protocol.EventRecord

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
			SELECT attempt_id FROM attempts
			WHERE state IN (%s) AND lease_expires_at < ?
		`, protocol.SQLInClause(protocol.ActiveAttemptStates)), nowMs)
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
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		for _, id := range ids {
			att, err := loadAttemptRecordTx(ctx, tx, id)
			if err != nil {
				return err
			}
			hasUncertain, err := expireOrMarkUncertainApprovalsTx(ctx, tx, att.AttemptID, nowMs, &events)
			if err != nil {
				return err
			}

			newAttState := protocol.AttemptExpired
			newTaskState := protocol.TaskStateFailed
			errCode := "LEASE_EXPIRED"
			reason := "Runner 租约超时失联，可安全重跑"
			if hasUncertain {
				newAttState = protocol.AttemptNeedsVerification
				newTaskState = protocol.TaskStateInReview
				errCode = "APPROVAL_CRASH_WINDOW"
				reason = "批准发出后租约过期，工具可能已执行，请核对现场后再决定是否重跑"
				if att.WBSessionID != "" {
					_, _ = tx.ExecContext(ctx, `UPDATE agent_sessions SET locked = 1, runtime_state = 'dead' WHERE wb_session_id = ?`, att.WBSessionID)
				}
			}

			if _, err := tx.ExecContext(ctx, `
				UPDATE attempts
				SET state = ?, error_code = ?, error_detail = ?, finished_at = ?
				WHERE attempt_id = ?
			`, string(newAttState), errCode, reason, nowMs, att.AttemptID); err != nil {
				return err
			}

			if !hasUncertain {
				if err := releaseQueuedAnnotationsTx(ctx, tx, att.AttemptID); err != nil {
					return err
				}
			}

			if _, err := tx.ExecContext(ctx, `
				UPDATE tasks
				SET state = ?, active_attempt_id = NULL, waiting_reason = ?, version = version + 1, updated_at = ?
				WHERE task_id = ? AND active_attempt_id = ?
			`, string(newTaskState), reason, nowMs, att.TaskID, att.AttemptID); err != nil {
				return err
			}

			if err := upsertBlockedInboxTx(ctx, tx, att.ProjectID, att.TaskID, att.AttemptID, "任务租约超时", reason, 2, nowMs); err != nil {
				return err
			}

			ev, err := insertEventTx(ctx, tx, eventInsert{
				StreamID:   "task:" + att.TaskID,
				ProjectID:  att.ProjectID,
				TaskID:     att.TaskID,
				AttemptID:  att.AttemptID,
				ActorKind:  protocol.ActorSystem,
				ActorID:    "lease-scanner",
				Kind:       protocol.EventAttemptFinished,
				Visibility: protocol.VisibilityInbox,
				Payload: map[string]any{
					"state":     newAttState,
					"errorCode": errCode,
					"reason":    reason,
				},
				CreatedAt: nowMs,
			})
			if err != nil {
				return err
			}
			events = append(events, ev)
			expiredIDs = append(expiredIDs, att.AttemptID)
		}
		return nil
	})
	return expiredIDs, events, err
}

// RenewMachineLeases updates machine liveness/resources and renews lease_expires_at ONLY for
// active, non-expired attempts matching (attemptId, leaseToken, machineId).
func (s *Store) RenewMachineLeases(
	ctx context.Context,
	machineID, runnerInstanceID string,
	leases []protocol.LiveAttemptLease,
	resources map[string]protocol.ResourceState,
	nowMs, leaseTTLMs int64,
) error {
	if nowMs == 0 {
		nowMs = time.Now().UnixMilli()
	}
	if leaseTTLMs <= 0 {
		leaseTTLMs = 45000
	}
	if resources == nil {
		resources = map[string]protocol.ResourceState{}
	}
	resJSON, err := json.Marshal(resources)
	if err != nil {
		return err
	}
	return s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE machines
			SET status = 'online', resources_json = ?, last_seen_at = ?, updated_at = ?
			WHERE machine_id = ?
		`, string(resJSON), nowMs, nowMs, machineID); err != nil {
			return err
		}
		newExp := nowMs + leaseTTLMs
		activeClause := protocol.SQLInClause(protocol.ActiveAttemptStates)
		for _, l := range leases {
			if l.AttemptID == "" || l.LeaseToken == "" {
				continue
			}
			_, err := tx.ExecContext(ctx, fmt.Sprintf(`
				UPDATE attempts
				SET lease_expires_at = ?, runner_instance_id = COALESCE(NULLIF(runner_instance_id, ''), ?)
				WHERE attempt_id = ? AND machine_id = ? AND lease_token = ?
				  AND state IN (%s) AND lease_expires_at >= ?
			`, activeClause), newExp, runnerInstanceID, l.AttemptID, machineID, l.LeaseToken, nowMs)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// SetMachineStatus updates a machine's connectivity status in SQLite.
func (s *Store) SetMachineStatus(ctx context.Context, machineID string, status protocol.MachineStatus) error {
	now := time.Now().UnixMilli()
	return s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE machines SET status = ?, updated_at = ? WHERE machine_id = ?
		`, string(status), now, machineID)
		return err
	})
}

// BindAttemptRunnerInstance records which runnerInstanceId accepted an attempt dispatch.
func (s *Store) BindAttemptRunnerInstance(ctx context.Context, attemptID, runnerInstanceID string) error {
	return s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE attempts SET runner_instance_id = ? WHERE attempt_id = ?
		`, runnerInstanceID, attemptID)
		return err
	})
}

// UpdateAttemptLeaseExpiry is a helper primarily for tests to advance/expire leases cleanly.
func (s *Store) UpdateAttemptLeaseExpiry(ctx context.Context, attemptID string, expiresAt int64) error {
	return s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE attempts SET lease_expires_at = ? WHERE attempt_id = ?`, expiresAt, attemptID)
		return err
	})
}

func releaseQueuedAnnotationsTx(ctx context.Context, tx *sql.Tx, attemptID string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE annotations
		SET status = 'open', triggered_attempt_id = NULL
		WHERE triggered_attempt_id = ? AND status = 'queued'
	`, attemptID)
	return err
}

func upsertBlockedInboxTx(ctx context.Context, tx *sql.Tx, projectID, taskID, attemptID, title, summary string, priority int, now int64) error {
	inboxID := "inb:blk:" + attemptID
	_, err := tx.ExecContext(ctx, `
		INSERT INTO inbox_items (
			inbox_id, project_id, task_id, category, ref_id, title, summary,
			priority, state, created_at, updated_at
		) VALUES (?, ?, ?, 'task_blocked', ?, ?, ?, ?, 'open', ?, ?)
		ON CONFLICT(inbox_id) DO UPDATE SET
			title = excluded.title,
			summary = excluded.summary,
			priority = excluded.priority,
			state = 'open',
			updated_at = excluded.updated_at,
			resolved_at = NULL
	`, inboxID, projectID, taskID, attemptID, title, summary, priority, now, now)
	return err
}
