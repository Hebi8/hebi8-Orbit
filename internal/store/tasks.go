package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	Anchor             *protocol.ProjectAnchorConfig
	AnchorCapability   string
	LeaseToken         string
	LeaseExpiresAt     int64
	LastAckedOutbox    int64
	RunnerInstanceID   string
	ResumeState        string
	TargetEnv          string
	DeployPhase        string
	CancelRequested    bool
	CancelReasonKind   string
	SteerInputID       string
}

type storedAttemptAnchor struct {
	Capability string                        `json:"capability,omitempty"`
	Anchor     *protocol.ProjectAnchorConfig `json:"anchor,omitempty"`
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
	ProjectID            string
	ParentTaskID         string
	DelegatedByAttemptID string
	DelegatedRole        string
	NotifyParentOnFinish *bool
	Title                string
	Goal                 string
	RequiredCapability   string
	RequiredResources    []string
	Adapter              protocol.Adapter
	DesiredConfig        map[string]string
	CreatedBy            protocol.TaskCreatedBy
	IdempotencyKey       string
	Sources              []protocol.CreateTaskSourceInput
	MachineID            string
	PartID               string
	LocationID           string
	Isolation            protocol.LocationIsolation
	RouteExplicit        bool
	ProjectGitIsolation  protocol.LocationIsolation
	Anchor               *protocol.ProjectAnchorConfig
	AnchorCapability     string
	WorktreePath         string
	BaseSHA              string
	InputDeliveryID      string
	Prompt               string
	TriggerKind          protocol.TriggerKind
	TriggerPayload       map[string]any
	CreateAttempt        bool
	LeaseTTLMs           int64
	ActorID              string
	ActorKind            protocol.ActorKind
	PAIdempotency        *PAActionIdempotencySpec
	RunnerParentGuard    *RunnerParentControlGuard
}

// CreateTaskResult returns created (or idempotently found) entities and events.
type CreateTaskResult struct {
	Task            protocol.TaskSummary
	Attempt         *AttemptRecord
	Sources         []protocol.TaskSource
	Created         bool
	ReplayedReceipt *protocol.PAActionResult
	Events          []protocol.EventRecord
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
		// P1-1: Atomically verify runner's live control of parent attempt inside the write tx before any lookup/write.
		if p.RunnerParentGuard != nil {
			if err := verifyRunnerParentControlTx(ctx, tx, p.RunnerParentGuard); err != nil {
				return err
			}
		}

		// NEW-3: Atomic PA idempotency check inside the SAME write transaction.
		if prevReceipt, found, err := lookupPAActionReceiptTx(ctx, tx, p.PAIdempotency); err != nil {
			return err
		} else if found {
			res.ReplayedReceipt = prevReceipt
			res.Created = false
			return nil
		}

		// Verify project exists
		var exists string
		if err := tx.QueryRowContext(ctx, `SELECT project_id FROM projects WHERE project_id = ? AND archived = 0`, p.ProjectID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: project %s", ErrNotFound, p.ProjectID)
			}
			return err
		}

		// Idempotency key check: MUST match BOTH project_id AND parent_task_id so one parent can NEVER
		// observe or replay another parent's task summary/attempt/sources under the same project!
		if p.IdempotencyKey != "" {
			var existingTaskID string
			err := tx.QueryRowContext(ctx, `SELECT task_id FROM tasks WHERE idempotency_key = ?`, p.IdempotencyKey).Scan(&existingTaskID)
			if err == nil {
				task, err := loadTaskSummaryTx(ctx, tx, existingTaskID)
				if err != nil {
					return err
				}
				if task.ProjectID != p.ProjectID || task.ParentTaskID != p.ParentTaskID {
					return fmt.Errorf("%w: idempotency key already bound to another task scope", ErrConflict)
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

		// Persist project-wide Git isolation preference atomically inside CreateTask ONLY after
		// PA receipt and idempotency-key dedup checks confirm this is a genuinely new task.
		if p.ProjectGitIsolation != "" && !protocol.IsVirtualProjectID(p.ProjectID) {
			if evProj, changed, isoErr := setProjectGitIsolationTx(ctx, tx, p.ProjectID, p.ProjectGitIsolation, p.ActorID, now); isoErr != nil {
				return isoErr
			} else if changed {
				res.Events = append(res.Events, evProj)
			}
		}

		// Verify parent task exists, is NOT archived, is NOT PA/Hebi8, and satisfies single-layer depth & active-child caps.
		var parentVal any
		var parentProjectID string
		delegationDepth := 0
		if p.ParentTaskID != "" {
			var parentOfParent sql.NullString
			var parentArchived sql.NullInt64
			if err := tx.QueryRowContext(ctx, `
				SELECT project_id, parent_task_id, archived_at
				FROM tasks WHERE task_id = ?
			`, p.ParentTaskID).Scan(&parentProjectID, &parentOfParent, &parentArchived); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("%w: parent task %s", ErrNotFound, p.ParentTaskID)
				}
				return err
			}
			if parentArchived.Valid {
				return fmt.Errorf("%w: parent task %s is archived", ErrTaskArchived, p.ParentTaskID)
			}
			if parentProjectID == protocol.PAProjectID {
				return fmt.Errorf("%w: parent task %s belongs to %s", ErrPANoSubsessions, p.ParentTaskID, protocol.PAProjectID)
			}
			if parentOfParent.Valid && strings.TrimSpace(parentOfParent.String) != "" {
				return fmt.Errorf("%w: child session %s cannot spawn grandchild sessions (max depth %d)", ErrMaxSubsessionDepth, p.ParentTaskID, maxDelegationDepth)
			}
			parentDepth, err := computeTaskDepthTx(ctx, tx, p.ParentTaskID)
			if err != nil {
				return err
			}
			delegationDepth = parentDepth + 1
			if delegationDepth > maxDelegationDepth {
				return fmt.Errorf("%w: delegation depth %d exceeds maximum %d", ErrMaxSubsessionDepth, delegationDepth, maxDelegationDepth)
			}
			if err := checkActiveChildLimitTx(ctx, tx, p.ParentTaskID, ""); err != nil {
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

		var partVal, locVal any
		if strings.TrimSpace(p.PartID) != "" {
			partVal = strings.TrimSpace(p.PartID)
		}
		if strings.TrimSpace(p.LocationID) != "" {
			locVal = strings.TrimSpace(p.LocationID)
		}
		var delegatedByVal, delegatedRoleVal any
		if v := strings.TrimSpace(p.DelegatedByAttemptID); v != "" {
			delegatedByVal = v
		}
		if v := strings.TrimSpace(p.DelegatedRole); v != "" {
			delegatedRoleVal = v
		}
		notifyParent := 1
		if p.NotifyParentOnFinish != nil && !*p.NotifyParentOnFinish {
			notifyParent = 0
		}

		taskID := newID("tsk")
		_, err = tx.ExecContext(ctx, `
			INSERT INTO tasks (
				task_id, project_id, parent_task_id, delegated_by_attempt_id, delegated_role,
				notify_parent_on_finish, delegation_depth,
				title, goal,
				required_capability, required_resources, preferred_adapter, desired_config_json,
				part_id, location_id,
				state, created_by, idempotency_key, version, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?, 1, ?, ?)
		`, taskID, p.ProjectID, parentVal, delegatedByVal, delegatedRoleVal,
			notifyParent, delegationDepth,
			title, p.Goal,
			p.RequiredCapability, string(reqResJSON), string(p.Adapter), string(cfgJSON),
			partVal, locVal,
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
				TaskID:            taskID,
				ProjectID:         p.ProjectID,
				MachineID:         p.MachineID,
				Adapter:           p.Adapter,
				PartID:            p.PartID,
				LocationID:        p.LocationID,
				Isolation:         p.Isolation,
				RouteExplicit:     p.RouteExplicit,
				TriggerKind:       p.TriggerKind,
				TriggerPayload:    p.TriggerPayload,
				Prompt:            p.Prompt,
				WorktreePath:      p.WorktreePath,
				BaseSHA:           p.BaseSHA,
				InputDeliveryID:   p.InputDeliveryID,
				DesiredConfig:     p.DesiredConfig,
				RequiredResources: p.RequiredResources,
				Anchor:            p.Anchor,
				AnchorCapability:  p.AnchorCapability,
				ForceNewSession:   true,
				LeaseTTLMs:        p.LeaseTTLMs,
				ActorKind:         p.ActorKind,
				ActorID:           p.ActorID,
				Now:               now,
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
		if p.ParentTaskID != "" {
			childMachineID := strings.TrimSpace(p.MachineID)
			if res.Attempt != nil && res.Attempt.MachineID != "" {
				childMachineID = res.Attempt.MachineID
			}
			spawnPayload := map[string]any{
				"childTaskId":          taskID,
				"projectId":            p.ProjectID,
				"title":                title,
				"role":                 strings.TrimSpace(p.DelegatedRole),
				"delegatedByAttemptId": strings.TrimSpace(p.DelegatedByAttemptID),
				"state":                task.State,
				"adapter":              p.Adapter,
				"machineId":            childMachineID,
			}
			evSpawn, err := insertEventTx(ctx, tx, eventInsert{
				StreamID:   "task:" + p.ParentTaskID,
				ProjectID:  parentProjectID,
				TaskID:     p.ParentTaskID,
				AttemptID:  strings.TrimSpace(p.DelegatedByAttemptID),
				ActorKind:  p.ActorKind,
				ActorID:    p.ActorID,
				Kind:       protocol.EventTaskChildSpawned,
				Visibility: protocol.VisibilityTimeline,
				Payload:    spawnPayload,
				CreatedAt:  now,
			})
			if err != nil {
				return err
			}
			res.Events = append(res.Events, evSpawn)
		}
		res.Task = task
		res.Created = true
		if p.PAIdempotency != nil && strings.TrimSpace(p.PAIdempotency.Key) != "" {
			receipt := protocol.PAActionResult{
				Action:  p.PAIdempotency.Action,
				Status:  "ok",
				Summary: fmt.Sprintf("已为 %s 创建任务 %s", task.ProjectID, task.TaskID),
				Data: map[string]any{
					"taskId":    task.TaskID,
					"projectId": task.ProjectID,
				},
			}
			if res.Attempt != nil {
				receipt.Data["attemptId"] = res.Attempt.AttemptID
			}
			if err := savePAActionReceiptTx(ctx, tx, p.PAIdempotency, receipt, now); err != nil {
				return err
			}
		}
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
		var curProject, curCapability string
		var activeAtt sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT project_id, required_capability, active_attempt_id FROM tasks WHERE task_id = ?`, taskID).Scan(&curProject, &curCapability, &activeAtt)
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

		var exists, targetCfgJSON string
		if err := tx.QueryRowContext(ctx, `SELECT project_id, config_json FROM projects WHERE project_id = ? AND archived = 0`, targetProjectID).Scan(&exists, &targetCfgJSON); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: project %s", ErrNotFound, targetProjectID)
			}
			return err
		}

		nextCapability := curCapability
		var targetCfg protocol.ProjectConfig
		if err := json.Unmarshal([]byte(targetCfgJSON), &targetCfg); err == nil {
			if targetCfg.DefaultCapability != "" {
				if nextCapability == "" || (len(targetCfg.Anchors) > 0 && targetCfg.Anchors[nextCapability].Machine == "") {
					nextCapability = targetCfg.DefaultCapability
				}
			}
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
		// B14: Lock & terminate all prior _scratch ACP sessions so the next project turn MUST open a fresh session in the project's worktree.
		if _, err := tx.ExecContext(ctx, `
			UPDATE agent_sessions
			SET locked = 1, runtime_state = 'dead', provider_session_id = NULL,
			    closed_at = COALESCE(closed_at, ?), last_activity_at = ?
			WHERE task_id = ?
		`, now, now, taskID); err != nil {
			return fmt.Errorf("lock prior scratch sessions: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks SET required_capability = ?, version = version + 1, updated_at = ? WHERE task_id = ?
		`, nextCapability, now, taskID); err != nil {
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
	TaskID            string
	MachineID         string
	Adapter           protocol.Adapter
	PartID            string
	LocationID        string
	Isolation         protocol.LocationIsolation
	RouteExplicit     bool
	TriggerKind       protocol.TriggerKind
	TriggerPayload    map[string]any
	Prompt            string
	WorktreePath      string
	BaseSHA           string
	InputDeliveryID   string
	DesiredConfig     map[string]string
	RequiredResources []string
	Anchor            *protocol.ProjectAnchorConfig
	AnchorCapability  string
	ForceNewSession   bool
	VerifiedSite      bool
	VerifiedAttemptID string
	LeaseTTLMs        int64
	ActorKind         protocol.ActorKind
	ActorID           string
}

// checkNeedsVerificationTx enforces B11 across ALL continuation entry points.
// If the task's most recent attempt ended in needs_verification, callers MUST provide
// verifiedSite=true (and, if verifiedAttemptID is non-empty, it must match that attempt).
func checkNeedsVerificationTx(ctx context.Context, tx *sql.Tx, taskID string, verifiedSite bool, verifiedAttemptID string) (*AttemptRecord, error) {
	var lastAttemptID string
	err := tx.QueryRowContext(ctx, `
		SELECT attempt_id FROM attempts WHERE task_id = ?
		ORDER BY attempt_no DESC LIMIT 1
	`, taskID).Scan(&lastAttemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	lastAtt, err := loadAttemptRecordTx(ctx, tx, lastAttemptID)
	if err != nil {
		return nil, err
	}
	if lastAtt.State == protocol.AttemptNeedsVerification {
		if !verifiedSite || strings.TrimSpace(verifiedAttemptID) == "" {
			return &lastAtt, fmt.Errorf("%w: previous attempt %s is in needs_verification; pass verifiedSite=true and verifiedAttemptId=%s after inspecting state", ErrVerificationRequired, lastAtt.AttemptID, lastAtt.AttemptID)
		}
		if strings.TrimSpace(verifiedAttemptID) != lastAtt.AttemptID {
			return &lastAtt, fmt.Errorf("%w: verifiedAttemptId %q does not match needs_verification attempt %q", ErrVerificationRequired, verifiedAttemptID, lastAtt.AttemptID)
		}
	}
	return &lastAtt, nil
}

// CreateNextAttempt atomically starts a new Attempt for an idle Task.
// Rejects with ErrConflict if an active attempt already exists, or ErrVerificationRequired
// when continuing after a needs_verification attempt without verifiedSite=true.
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
		var projectID, taskGoal string
		var activeAttID, latestDlvID, desiredCfgJSON, reqResJSON sql.NullString
		var archivedAt sql.NullInt64
		var prefAdapter string
		err := tx.QueryRowContext(ctx, `
			SELECT project_id, goal, active_attempt_id, latest_delivery_id, preferred_adapter, desired_config_json, required_resources, archived_at
			FROM tasks WHERE task_id = ?
		`, p.TaskID).Scan(&projectID, &taskGoal, &activeAttID, &latestDlvID, &prefAdapter, &desiredCfgJSON, &reqResJSON, &archivedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: task %s", ErrNotFound, p.TaskID)
		}
		if err != nil {
			return err
		}
		if archivedAt.Valid {
			return fmt.Errorf("%w: task %s", ErrTaskArchived, p.TaskID)
		}
		if activeAttID.Valid && activeAttID.String != "" {
			return fmt.Errorf("%w: task %s already has active attempt %s", ErrConflict, p.TaskID, activeAttID.String)
		}

		// B11: Unified needs_verification gate across ALL continuation entry points
		lastAtt, err := checkNeedsVerificationTx(ctx, tx, p.TaskID, p.VerifiedSite, p.VerifiedAttemptID)
		if err != nil {
			return err
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
		reqResources := p.RequiredResources
		if reqResources == nil {
			reqResources = []string{}
			if reqResJSON.Valid && reqResJSON.String != "" {
				_ = json.Unmarshal([]byte(reqResJSON.String), &reqResources)
			}
		}
		inputDeliveryID := p.InputDeliveryID
		if inputDeliveryID == "" && latestDlvID.Valid {
			inputDeliveryID = latestDlvID.String
		}

		prompt := p.Prompt
		triggerPayload := cloneMap(p.TriggerPayload)
		if lastAtt != nil && p.TriggerKind == protocol.TriggerRetry && lastAtt.TriggerPayloadJSON != "" {
			var prevPayload map[string]any
			if err := json.Unmarshal([]byte(lastAtt.TriggerPayloadJSON), &prevPayload); err == nil {
				if strings.TrimSpace(prompt) == "" {
					if prevPrompt, ok := prevPayload["prompt"].(string); ok && strings.TrimSpace(prevPrompt) != "" {
						prompt = prevPrompt
					}
				}
				if _, ok := triggerPayload["annotationIds"]; !ok {
					if prevAnns, ok := prevPayload["annotationIds"]; ok {
						triggerPayload["annotationIds"] = prevAnns
					}
				}
			}
		}
		if p.TriggerKind == protocol.TriggerRetry {
			if strings.TrimSpace(prompt) == "" {
				prompt = taskGoal
			}
			if note := stringField(triggerPayload, "note"); note != "" && !strings.Contains(prompt, note) {
				prompt = prompt + "\n\n重试补充说明: " + note
			}
			triggerPayload["prompt"] = prompt
		}
		if lastAtt != nil && lastAtt.State == protocol.AttemptNeedsVerification {
			triggerPayload["verifiedAttemptId"] = lastAtt.AttemptID
		}

		// Resolve any previous blocked inbox card for the last attempt when retrying/continuing
		if lastAtt != nil {
			_, _ = tx.ExecContext(ctx, `
				UPDATE inbox_items SET state = 'resolved', resolved_at = ?, updated_at = ?
				WHERE inbox_id = ? AND state = 'open'
			`, now, now, "inb:blk:"+lastAtt.AttemptID)
		}

		createdAtt, evAtt, err := createAttemptInTx(ctx, tx, createAttemptTxParams{
			TaskID:            p.TaskID,
			ProjectID:         projectID,
			MachineID:         p.MachineID,
			Adapter:           adapter,
			PartID:            p.PartID,
			LocationID:        p.LocationID,
			Isolation:         p.Isolation,
			TriggerKind:       p.TriggerKind,
			TriggerPayload:    triggerPayload,
			Prompt:            prompt,
			WorktreePath:      p.WorktreePath,
			BaseSHA:           p.BaseSHA,
			InputDeliveryID:   inputDeliveryID,
			DesiredConfig:     desiredCfg,
			RequiredResources: reqResources,
			Anchor:            p.Anchor,
			AnchorCapability:  p.AnchorCapability,
			ForceNewSession:   p.ForceNewSession,
			LeaseTTLMs:        p.LeaseTTLMs,
			ActorKind:         p.ActorKind,
			ActorID:           p.ActorID,
			Now:               now,
		})
		if err != nil {
			return err
		}

		// B11: Never leave queued annotations stranded on a terminal needs_verification attempt.
		// On retry, carry them forward onto the new attempt; on other continuations, release them back to open.
		if lastAtt != nil && lastAtt.State == protocol.AttemptNeedsVerification {
			if err := resetParentChildNotificationBackoffTx(ctx, tx, p.TaskID); err != nil {
				return err
			}
			if p.TriggerKind == protocol.TriggerRetry {
				if _, err := tx.ExecContext(ctx, `
					UPDATE annotations SET triggered_attempt_id = ?
					WHERE triggered_attempt_id = ? AND status = 'queued'
				`, createdAtt.AttemptID, lastAtt.AttemptID); err != nil {
					return err
				}
			} else {
				if err := releaseQueuedAnnotationsTx(ctx, tx, lastAtt.AttemptID); err != nil {
					return err
				}
			}
		}

		att = createdAtt
		events = append(events, evAtt)

		task, err = loadTaskSummaryTx(ctx, tx, p.TaskID)
		return err
	})
	return task, att, events, err
}

type createAttemptTxParams struct {
	TaskID            string
	ProjectID         string
	MachineID         string
	Adapter           protocol.Adapter
	PartID            string
	LocationID        string
	Isolation         protocol.LocationIsolation
	RouteExplicit     bool
	TriggerKind       protocol.TriggerKind
	TriggerPayload    map[string]any
	Prompt            string
	WorktreePath      string
	BaseSHA           string
	InputDeliveryID   string
	DesiredConfig     map[string]string
	RequiredResources []string
	Anchor            *protocol.ProjectAnchorConfig
	AnchorCapability  string
	ForceNewSession   bool
	LeaseTTLMs        int64
	ActorKind         protocol.ActorKind
	ActorID           string
	Now               int64
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

	// Enforce active child quota uniformly across ALL attempt creation & reactivation paths
	// (CreateTask, follow-up, retry, queue dispatch, annotation dispatch).
	var taskParentID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT parent_task_id FROM tasks WHERE task_id = ?`, p.TaskID).Scan(&taskParentID); err != nil {
		return AttemptRecord{}, protocol.EventRecord{}, err
	}
	if taskParentID.Valid && strings.TrimSpace(taskParentID.String) != "" {
		if err := checkActiveChildLimitTx(ctx, tx, strings.TrimSpace(taskParentID.String), p.TaskID); err != nil {
			return AttemptRecord{}, protocol.EventRecord{}, err
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

	// Atomically consult Hebi8 Fold-Rollover generation state inside this SAME BEGIN IMMEDIATE tx:
	// ensures no concurrent follow-up/retry/queue-drain can ever reuse a pre-fold session or miss the opening!
	if p.ProjectID == protocol.PAProjectID {
		if isMain, mErr := isHebi8MainTaskTx(ctx, tx, p.TaskID); mErr != nil {
			return AttemptRecord{}, protocol.EventRecord{}, mErr
		} else if isMain {
			st, stErr := loadHebi8RolloverStateTx(ctx, tx)
			if stErr != nil {
				return AttemptRecord{}, protocol.EventRecord{}, stErr
			}
			if st.Generation > 0 && st.PendingOpeningGen == st.Generation && nextAttemptNo > st.LastFoldedAttemptNo {
				var succeededAfter int
				if err := tx.QueryRowContext(ctx, `
					SELECT COUNT(*) FROM attempts
					WHERE task_id = ? AND attempt_no > ? AND state = 'succeeded'
				`, p.TaskID, st.LastFoldedAttemptNo).Scan(&succeededAfter); err != nil {
					return AttemptRecord{}, protocol.EventRecord{}, err
				}
				if succeededAfter > 0 {
					st.PendingOpeningGen = 0
					if err := saveHebi8RolloverStateTx(ctx, tx, st, p.Now); err != nil {
						return AttemptRecord{}, protocol.EventRecord{}, err
					}
				} else {
					p.ForceNewSession = true
					p.TriggerPayload["hebi8OpeningGeneration"] = st.Generation
					p.TriggerPayload["hebi8LastFoldedAttemptNo"] = st.LastFoldedAttemptNo
				}
			}
		}
	}

	cfgBytes, err := json.Marshal(p.DesiredConfig)
	if err != nil {
		return AttemptRecord{}, protocol.EventRecord{}, err
	}
	reqResources := p.RequiredResources
	if reqResources == nil {
		var taskReqJSON sql.NullString
		_ = tx.QueryRowContext(ctx, `SELECT required_resources FROM tasks WHERE task_id = ?`, p.TaskID).Scan(&taskReqJSON)
		reqResources = []string{}
		if taskReqJSON.Valid && taskReqJSON.String != "" {
			_ = json.Unmarshal([]byte(taskReqJSON.String), &reqResources)
		}
	}
	reqResBytes, err := json.Marshal(reqResources)
	if err != nil {
		return AttemptRecord{}, protocol.EventRecord{}, err
	}
	var anchorVal any
	if p.Anchor != nil || p.AnchorCapability != "" {
		ab, err := json.Marshal(storedAttemptAnchor{
			Capability: p.AnchorCapability,
			Anchor:     p.Anchor,
		})
		if err != nil {
			return AttemptRecord{}, protocol.EventRecord{}, err
		}
		anchorVal = string(ab)
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

	if !p.RouteExplicit && (strings.TrimSpace(p.PartID) == "" || strings.TrimSpace(p.LocationID) == "" || p.Isolation == "") {
		var prevMachine, prevPart, prevLoc, prevIso sql.NullString
		_ = tx.QueryRowContext(ctx, `
			SELECT machine_id, part_id, location_id, isolation
			FROM attempts
			WHERE task_id = ?
			ORDER BY attempt_no DESC
			LIMIT 1
		`, p.TaskID).Scan(&prevMachine, &prevPart, &prevLoc, &prevIso)
		// Only inherit prior location/part/isolation when staying on the same machine.
		sameMachine := !prevMachine.Valid || prevMachine.String == "" || prevMachine.String == machineID
		if sameMachine {
			if strings.TrimSpace(p.PartID) == "" && prevPart.Valid {
				p.PartID = prevPart.String
			}
			if strings.TrimSpace(p.LocationID) == "" && prevLoc.Valid {
				p.LocationID = prevLoc.String
			}
			if p.Isolation == "" && prevIso.Valid && prevIso.String != "" {
				p.Isolation = protocol.LocationIsolation(prevIso.String)
			}
		}
	}

	var partVal, locVal, isoVal any
	if strings.TrimSpace(p.PartID) != "" {
		partVal = strings.TrimSpace(p.PartID)
	}
	if strings.TrimSpace(p.LocationID) != "" {
		locVal = strings.TrimSpace(p.LocationID)
	}
	if p.Isolation != "" {
		isoVal = string(p.Isolation)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO attempts (
			attempt_id, task_id, project_id, attempt_no, machine_id, adapter,
			part_id, location_id, isolation,
			wb_session_id, trigger_kind, trigger_payload_json, worktree_path,
			input_delivery_id, base_sha, state, lease_token, lease_expires_at,
			last_acked_outbox, started_at,
			desired_config_json, required_resources_json, anchor_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'leased', ?, ?, 0, ?, ?, ?, ?)
	`, attemptID, p.TaskID, p.ProjectID, nextAttemptNo, machineID, string(p.Adapter),
		partVal, locVal, isoVal,
		wbSessionVal, string(p.TriggerKind), string(payloadBytes), worktreeVal,
		inputDlvVal, baseSHAVal, leaseToken, leaseExpiresAt, p.Now,
		string(cfgBytes), string(reqResBytes), anchorVal)
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

	attStartedPayload := map[string]any{
		"attemptNo":   nextAttemptNo,
		"machineId":   machineID,
		"adapter":     p.Adapter,
		"triggerKind": p.TriggerKind,
		"wbSessionId": wbSessionID,
	}
	if fromTaskID, ok := p.TriggerPayload["delegatedFromTaskId"].(string); ok && strings.TrimSpace(fromTaskID) != "" {
		attStartedPayload["delegatedFromTaskId"] = strings.TrimSpace(fromTaskID)
	}
	if rel, ok := p.TriggerPayload["delegatedRelation"].(string); ok && strings.TrimSpace(rel) != "" {
		attStartedPayload["delegatedRelation"] = strings.TrimSpace(rel)
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
		Payload:    attStartedPayload,
		CreatedAt:  p.Now,
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
	var partID, locationID, isolation, folderChangesJSON sql.NullString
	var wbSessID, worktree, inputDlv, baseSHA, outDlv, compSrc, errCode, errDetail sql.NullString
	var runnerInst, resumeState, targetEnv, deployPhase sql.NullString
	var cancelReasonKind, steerInputID sql.NullString
	var attCfgJSON, attReqResJSON, attAnchorJSON sql.NullString
	var provSID, sessCWD, taskCfgJSON, taskReqResJSON sql.NullString
	var sessLocked sql.NullInt64
	var cancelRequested int
	var finishedAt sql.NullInt64

	err := tx.QueryRowContext(ctx, `
		SELECT a.attempt_id, a.task_id, a.project_id, a.attempt_no, a.machine_id, a.adapter,
		       a.part_id, a.location_id, a.isolation, a.folder_changes_json,
		       a.wb_session_id, a.trigger_kind, a.trigger_payload_json, a.worktree_path,
		       a.input_delivery_id, a.base_sha, a.output_delivery_id, a.target_env, a.deploy_phase,
		       a.state, a.resume_state, a.completion_source, a.lease_token, a.lease_expires_at,
		       a.last_acked_outbox, a.runner_instance_id, a.error_code, a.error_detail,
		       a.started_at, a.finished_at,
		       a.cancel_requested, a.cancel_reason_kind, a.steer_input_id,
		       a.desired_config_json, a.required_resources_json, a.anchor_json,
		       s.provider_session_id, s.cwd, s.locked,
		       t.desired_config_json, t.required_resources
		FROM attempts a
		LEFT JOIN agent_sessions s ON s.wb_session_id = a.wb_session_id
		LEFT JOIN tasks t ON t.task_id = a.task_id
		WHERE a.attempt_id = ?
	`, attemptID).Scan(
		&a.AttemptID, &a.TaskID, &a.ProjectID, &a.AttemptNo, &a.MachineID, &adapter,
		&partID, &locationID, &isolation, &folderChangesJSON,
		&wbSessID, &trigger, &a.TriggerPayloadJSON, &worktree,
		&inputDlv, &baseSHA, &outDlv, &targetEnv, &deployPhase,
		&state, &resumeState, &compSrc, &a.LeaseToken, &a.LeaseExpiresAt,
		&a.LastAckedOutbox, &runnerInst, &errCode, &errDetail,
		&a.StartedAt, &finishedAt,
		&cancelRequested, &cancelReasonKind, &steerInputID,
		&attCfgJSON, &attReqResJSON, &attAnchorJSON,
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
	if partID.Valid {
		a.PartID = partID.String
	}
	if locationID.Valid {
		a.LocationID = locationID.String
	}
	if isolation.Valid {
		a.Isolation = protocol.LocationIsolation(isolation.String)
	}
	if folderChangesJSON.Valid && strings.TrimSpace(folderChangesJSON.String) != "" {
		var fc protocol.FolderChangesSummary
		if json.Unmarshal([]byte(folderChangesJSON.String), &fc) == nil {
			a.FolderChanges = &fc
		}
	}
	a.TriggerKind = protocol.TriggerKind(trigger)
	a.State = protocol.AttemptState(state)
	a.PromptText = extractBoundedPromptText(a.TriggerPayloadJSON)
	a.RefTasks = extractReferencedTasks(a.TriggerPayloadJSON)
	a.CancelRequested = cancelRequested != 0
	if cancelReasonKind.Valid {
		a.CancelReasonKind = cancelReasonKind.String
	}
	if steerInputID.Valid {
		a.SteerInputID = steerInputID.String
	}
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
	if attCfgJSON.Valid && attCfgJSON.String != "" {
		_ = json.Unmarshal([]byte(attCfgJSON.String), &a.DesiredConfig)
	} else if taskCfgJSON.Valid && taskCfgJSON.String != "" {
		_ = json.Unmarshal([]byte(taskCfgJSON.String), &a.DesiredConfig)
	}
	a.RequiredResources = []string{}
	if attReqResJSON.Valid && attReqResJSON.String != "" {
		_ = json.Unmarshal([]byte(attReqResJSON.String), &a.RequiredResources)
	} else if taskReqResJSON.Valid && taskReqResJSON.String != "" {
		_ = json.Unmarshal([]byte(taskReqResJSON.String), &a.RequiredResources)
	}
	if attAnchorJSON.Valid && attAnchorJSON.String != "" {
		var sa storedAttemptAnchor
		if err := json.Unmarshal([]byte(attAnchorJSON.String), &sa); err == nil {
			a.AnchorCapability = sa.Capability
			a.Anchor = sa.Anchor
		}
	}
	return a, nil
}

// GetTaskExecutionDefaults returns the persisted desired_config_json and required_resources
// on a task so fork_new_session inherits parent task execution defaults when not overridden (S2).
func (s *Store) GetTaskExecutionDefaults(ctx context.Context, taskID string) (map[string]string, []string, error) {
	cfg := map[string]string{}
	res := []string{}
	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		var cfgJSON, resJSON sql.NullString
		err := tx.QueryRowContext(ctx, `
			SELECT desired_config_json, required_resources FROM tasks WHERE task_id = ?
		`, taskID).Scan(&cfgJSON, &resJSON)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: task %s", ErrNotFound, taskID)
		}
		if err != nil {
			return err
		}
		if cfgJSON.Valid && cfgJSON.String != "" {
			_ = json.Unmarshal([]byte(cfgJSON.String), &cfg)
		}
		if resJSON.Valid && resJSON.String != "" {
			_ = json.Unmarshal([]byte(resJSON.String), &res)
		}
		return nil
	})
	return cfg, res, err
}

// ListLeasedAttemptsForMachine lists undispatched attempts currently in 'leased' state for machineID.
func (s *Store) ListLeasedAttemptsForMachine(ctx context.Context, machineID string) ([]AttemptRecord, error) {
	var out []AttemptRecord
	err := s.WithReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT attempt_id FROM attempts
			WHERE machine_id = ? AND state = 'leased' AND cancel_requested = 0
			  AND (runner_instance_id IS NULL OR runner_instance_id = '')
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
	return s.ListAttemptRawEventsByKinds(ctx, attemptID, nil, afterSeq, limit)
}

// ListAttemptRawEventsByKinds returns raw_acp events for one attempt, optionally filtered to specific event kinds.
func (s *Store) ListAttemptRawEventsByKinds(ctx context.Context, attemptID string, kinds []protocol.EventKind, afterSeq int64, limit int) ([]protocol.EventRecord, error) {
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
		query := `
			SELECT seq, event_id, stream_id, project_id, task_id, attempt_id,
			       actor_kind, actor_id, kind, visibility, payload_json, created_at
			FROM events
			WHERE attempt_id = ? AND visibility = 'raw_acp' AND seq > ?`
		args := []any{attemptID, afterSeq}
		if len(kinds) > 0 {
			placeholders := make([]string, len(kinds))
			for i, k := range kinds {
				placeholders[i] = "?"
				args = append(args, string(k))
			}
			query += fmt.Sprintf(" AND kind IN (%s)", strings.Join(placeholders, ","))
		}
		query += `
			ORDER BY seq ASC
			LIMIT ?`
		args = append(args, limit)
		rows, err := tx.QueryContext(ctx, query, args...)
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
	Attempt         AttemptRecord
	CancelledNow    bool
	NeedsRunnerSend bool
	ReplayedReceipt *protocol.PAActionResult
	Events          []protocol.EventRecord
}

// CancelTaskActiveAttempt either immediately cancels a never-dispatched leased attempt
// or persists cancel_requested=1 so Hub sends attempt.cancel and retries across reconnects/frames.
func (s *Store) CancelTaskActiveAttempt(ctx context.Context, taskID, actorID, reason string, forceImmediate bool) (CancelTaskResult, error) {
	return s.CancelTaskActiveAttemptWithIdempotency(ctx, taskID, actorID, reason, forceImmediate, nil)
}

// FailUndispatchedAttempt atomically marks a never-dispatched leased attempt as failed and
// clears tasks.active_attempt_id so prompt/opening preparation errors never leave a zombie
// leased attempt blocking subsequent turns.
func (s *Store) FailUndispatchedAttempt(ctx context.Context, attemptID, errorCode, errorDetail string) ([]protocol.EventRecord, error) {
	var events []protocol.EventRecord
	now := time.Now().UnixMilli()
	if strings.TrimSpace(errorCode) == "" {
		errorCode = "ERR_DISPATCH_PREPARE"
	}
	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		att, err := loadAttemptRecordTx(ctx, tx, attemptID)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		// Only transition leased attempts that were never dispatched to a runner.
		if att.State != protocol.AttemptLeased || att.RunnerInstanceID != "" {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE attempts
			SET state = 'failed', error_code = ?, error_detail = ?, finished_at = ?
			WHERE attempt_id = ? AND state = 'leased'
		`, errorCode, errorDetail, now, att.AttemptID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks
			SET state = 'failed', active_attempt_id = NULL, waiting_reason = NULL,
			    pending_queue_drain = 0, pending_steer_input_id = NULL,
			    version = version + 1, updated_at = ?
			WHERE task_id = ? AND active_attempt_id = ?
		`, now, att.TaskID, att.AttemptID); err != nil {
			return err
		}
		revertedDeliveryIDs, _ := lookupQueuedAnnotationDeliveryIDsTx(ctx, tx, att.AttemptID)
		if err := releaseQueuedAnnotationsTx(ctx, tx, att.AttemptID); err != nil {
			return err
		}
		if err := expirePendingApprovalsForAttemptTx(ctx, tx, att.AttemptID, now); err != nil {
			return err
		}
		if err := enqueueChildParentNotificationTx(ctx, tx, att.TaskID, att.AttemptID, protocol.AttemptFailed, now); err != nil {
			return err
		}
		ev1Payload := map[string]any{
			"state":       protocol.AttemptFailed,
			"errorCode":   errorCode,
			"errorDetail": errorDetail,
		}
		if len(revertedDeliveryIDs) > 0 {
			ev1Payload["revertedDeliveryIds"] = revertedDeliveryIDs
			ev1Payload["deliveryIds"] = revertedDeliveryIDs
		}
		ev1, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + att.TaskID,
			ProjectID:  att.ProjectID,
			TaskID:     att.TaskID,
			AttemptID:  att.AttemptID,
			ActorKind:  protocol.ActorSystem,
			ActorID:    "hub",
			Kind:       protocol.EventAttemptFinished,
			Visibility: protocol.VisibilityTimeline,
			Payload:    ev1Payload,
			CreatedAt:  now,
		})
		if err != nil {
			return err
		}
		ev2, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + att.TaskID,
			ProjectID:  att.ProjectID,
			TaskID:     att.TaskID,
			AttemptID:  att.AttemptID,
			ActorKind:  protocol.ActorSystem,
			ActorID:    "hub",
			Kind:       protocol.EventTaskStateChanged,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"state":     protocol.TaskStateFailed,
				"errorCode": errorCode,
			},
			CreatedAt: now,
		})
		if err != nil {
			return err
		}
		events = append(events, ev1, ev2)
		return nil
	})
	return events, err
}

// CancelTaskActiveAttemptWithIdempotency performs cancel + PA idempotency receipt verification/storage
// inside a single BEGIN IMMEDIATE transaction (NEW-3).
func (s *Store) CancelTaskActiveAttemptWithIdempotency(ctx context.Context, taskID, actorID, reason string, forceImmediate bool, idemp *PAActionIdempotencySpec) (CancelTaskResult, error) {
	return s.CancelTaskActiveAttemptWithGuard(ctx, taskID, actorID, reason, forceImmediate, idemp, nil)
}

// CancelTaskActiveAttemptWithGuard cancels a task's active attempt while optionally verifying
// runner parent control + descendant scope atomically inside the same write transaction (P1-1).
func (s *Store) CancelTaskActiveAttemptWithGuard(ctx context.Context, taskID, actorID, reason string, forceImmediate bool, idemp *PAActionIdempotencySpec, guard *RunnerParentControlGuard) (CancelTaskResult, error) {
	var res CancelTaskResult
	now := time.Now().UnixMilli()
	if actorID == "" {
		actorID = "human"
	}

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if guard != nil {
			if err := verifyRunnerParentControlTx(ctx, tx, guard); err != nil {
				return err
			}
			isDirectChild, err := isDirectChildTaskTx(ctx, tx, guard.ParentTaskID, taskID)
			if err != nil {
				return err
			}
			if !isDirectChild {
				return fmt.Errorf("%w: task %s is not a direct child of top-level parent %s", ErrForbidden, taskID, guard.ParentTaskID)
			}
		}
		if prevReceipt, found, err := lookupPAActionReceiptTx(ctx, tx, idemp); err != nil {
			return err
		} else if found {
			res.ReplayedReceipt = prevReceipt
			return nil
		}

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

		// B10: Only cancel immediately without runner confirmation when explicitly forced
		// OR when the leased attempt has NEVER been dispatched to any runner_instance_id.
		neverDispatched := att.State == protocol.AttemptLeased && att.RunnerInstanceID == ""
		if !forceImmediate && !neverDispatched {
			if _, err := tx.ExecContext(ctx, `
				UPDATE attempts
				SET cancel_requested = 1,
				    cancel_reason_kind = 'human',
				    steer_input_id = NULL
				WHERE attempt_id = ?
			`, att.AttemptID); err != nil {
				return err
			}
			res.Attempt.CancelRequested = true
			res.Attempt.CancelReasonKind = "human"
			res.Attempt.SteerInputID = ""
			res.NeedsRunnerSend = true
		} else {
			if err := cancelAttemptTx(ctx, tx, att, actorID, reason, now, &res.Events); err != nil {
				return err
			}
			res.CancelledNow = true
			res.NeedsRunnerSend = !neverDispatched
		}

		if idemp != nil && strings.TrimSpace(idemp.Key) != "" {
			receipt := protocol.PAActionResult{
				Action:  idemp.Action,
				Status:  "ok",
				Summary: fmt.Sprintf("已请求取消任务 %s（attempt=%s）", taskID, att.AttemptID),
				Data: map[string]any{
					"taskId":    taskID,
					"attemptId": att.AttemptID,
				},
			}
			if err := savePAActionReceiptTx(ctx, tx, idemp, receipt, now); err != nil {
				return err
			}
		}
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
		SET state = 'cancelled', active_attempt_id = NULL, waiting_reason = NULL,
		    pending_queue_drain = 0, pending_steer_input_id = NULL,
		    version = version + 1, updated_at = ?
		WHERE task_id = ? AND active_attempt_id = ?
	`, now, att.TaskID, att.AttemptID); err != nil {
		return err
	}
	// Release any queued annotations tied to this attempt back to 'open' and report affected deliveries (S6)
	revertedDeliveryIDs, _ := lookupQueuedAnnotationDeliveryIDsTx(ctx, tx, att.AttemptID)
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
	if err := enqueueChildParentNotificationTx(ctx, tx, att.TaskID, att.AttemptID, protocol.AttemptCancelled, now); err != nil {
		return err
	}

	ev1Payload := map[string]any{
		"state":  protocol.AttemptCancelled,
		"reason": reason,
	}
	if len(revertedDeliveryIDs) > 0 {
		ev1Payload["revertedDeliveryIds"] = revertedDeliveryIDs
		ev1Payload["deliveryIds"] = revertedDeliveryIDs
	}
	ev1, err := insertEventTx(ctx, tx, eventInsert{
		StreamID:   "task:" + att.TaskID,
		ProjectID:  att.ProjectID,
		TaskID:     att.TaskID,
		AttemptID:  att.AttemptID,
		ActorKind:  protocol.ActorHuman,
		ActorID:    actorID,
		Kind:       protocol.EventAttemptFinished,
		Visibility: protocol.VisibilityTimeline,
		Payload:    ev1Payload,
		CreatedAt:  now,
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

// expireAttemptInTx transitions an active attempt whose lease has expired inside an existing write tx.
// Used by ScanExpiredAttempts, IngestOutboxBatch (B4), and ReconcileRunnerHello (B4).
func expireAttemptInTx(ctx context.Context, tx *sql.Tx, att *AttemptRecord, nowMs int64, actorID string, events *[]protocol.EventRecord) error {
	if actorID == "" {
		actorID = "lease-scanner"
	}
	hasUncertain, err := expireOrMarkUncertainApprovalsTx(ctx, tx, att.AttemptID, nowMs, events)
	if err != nil {
		return err
	}

	if att.CancelRequested && !hasUncertain {
		if err := cancelAttemptTx(ctx, tx, *att, actorID, "已请求取消且租约超时，归档为已取消", nowMs, events); err != nil {
			return err
		}
		att.State = protocol.AttemptCancelled
		return nil
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
	att.State = newAttState

	var revertedDeliveryIDs []string
	if !hasUncertain {
		revertedDeliveryIDs, _ = lookupQueuedAnnotationDeliveryIDsTx(ctx, tx, att.AttemptID)
		if err := releaseQueuedAnnotationsTx(ctx, tx, att.AttemptID); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET state = ?, active_attempt_id = NULL, waiting_reason = ?,
		    pending_queue_drain = 0, pending_steer_input_id = NULL,
		    version = version + 1, updated_at = ?
		WHERE task_id = ? AND active_attempt_id = ?
	`, string(newTaskState), reason, nowMs, att.TaskID, att.AttemptID); err != nil {
		return err
	}

	if err := upsertBlockedInboxTx(ctx, tx, att.ProjectID, att.TaskID, att.AttemptID, "任务租约超时", reason, 2, nowMs); err != nil {
		return err
	}
	if err := enqueueChildParentNotificationTx(ctx, tx, att.TaskID, att.AttemptID, newAttState, nowMs); err != nil {
		return err
	}

	evPayload := map[string]any{
		"state":     newAttState,
		"errorCode": errCode,
		"reason":    reason,
	}
	if len(revertedDeliveryIDs) > 0 {
		evPayload["revertedDeliveryIds"] = revertedDeliveryIDs
		evPayload["deliveryIds"] = revertedDeliveryIDs
	}
	ev, err := insertEventTx(ctx, tx, eventInsert{
		StreamID:   "task:" + att.TaskID,
		ProjectID:  att.ProjectID,
		TaskID:     att.TaskID,
		AttemptID:  att.AttemptID,
		ActorKind:  protocol.ActorSystem,
		ActorID:    actorID,
		Kind:       protocol.EventAttemptFinished,
		Visibility: protocol.VisibilityInbox,
		Payload:    evPayload,
		CreatedAt:  nowMs,
	})
	if err != nil {
		return err
	}
	*events = append(*events, ev)
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
			if err := expireAttemptInTx(ctx, tx, &att, nowMs, "lease-scanner", &events); err != nil {
				return err
			}
			expiredIDs = append(expiredIDs, att.AttemptID)
		}
		return nil
	})
	return expiredIDs, events, err
}

// RenewMachineLeases updates machine liveness/resources/adapters, renews lease_expires_at ONLY for
// active, non-expired attempts matching (attemptId, leaseToken, machineId), and returns any
// reported attempt IDs that have cancel_requested = 1 so Hub re-sends attempt.cancel.
func (s *Store) RenewMachineLeases(
	ctx context.Context,
	machineID, runnerInstanceID string,
	leases []protocol.LiveAttemptLease,
	resources map[string]protocol.ResourceState,
	adapters []protocol.Adapter,
	nowMs, leaseTTLMs int64,
) ([]string, error) {
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
		return nil, err
	}
	var adaptersVal any
	if len(adapters) > 0 {
		if b, err := json.Marshal(adapters); err == nil {
			adaptersVal = string(b)
		}
	}
	var cancelIDs []string
	err = s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE machines
			SET status = 'online',
			    resources_json = ?,
			    adapters_json = COALESCE(?, adapters_json),
			    last_seen_at = ?,
			    updated_at = ?
			WHERE machine_id = ?
		`, string(resJSON), adaptersVal, nowMs, nowMs, machineID); err != nil {
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
			var cancelReq int
			if err := tx.QueryRowContext(ctx, `
				SELECT cancel_requested FROM attempts
				WHERE attempt_id = ? AND machine_id = ? AND lease_token = ?
			`, l.AttemptID, machineID, l.LeaseToken).Scan(&cancelReq); err == nil && cancelReq != 0 {
				cancelIDs = append(cancelIDs, l.AttemptID)
			}
		}
		return nil
	})
	return cancelIDs, err
}

// SetMachineStatus updates a machine's connectivity status in SQLite and emits
// machine.state_changed when the status actually transitions.
func (s *Store) SetMachineStatus(ctx context.Context, machineID string, status protocol.MachineStatus) (*protocol.EventRecord, error) {
	now := time.Now().UnixMilli()
	var emitted *protocol.EventRecord
	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var prevStatus string
		err := tx.QueryRowContext(ctx, `SELECT status FROM machines WHERE machine_id = ?`, machineID).Scan(&prevStatus)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE machines SET status = ?, updated_at = ? WHERE machine_id = ?
		`, string(status), now, machineID); err != nil {
			return err
		}
		if prevStatus != string(status) {
			ev, err := insertEventTx(ctx, tx, eventInsert{
				StreamID:   "global",
				ActorKind:  protocol.ActorSystem,
				ActorID:    "runner-gateway",
				Kind:       protocol.EventMachineStateChanged,
				Visibility: protocol.VisibilityTimeline,
				Payload: map[string]any{
					"machineId":  machineID,
					"prevStatus": prevStatus,
					"status":     status,
				},
				CreatedAt: now,
			})
			if err != nil {
				return err
			}
			emitted = &ev
		}
		return nil
	})
	return emitted, err
}

// TryBindAttemptDispatched atomically binds a leased, non-cancel-requested attempt to
// runnerInstanceID right before sending attempt.dispatch. Returns true only if bound.
func (s *Store) TryBindAttemptDispatched(ctx context.Context, attemptID, runnerInstanceID string) (bool, error) {
	var bound bool
	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE attempts
			SET runner_instance_id = ?
			WHERE attempt_id = ? AND state = 'leased' AND cancel_requested = 0
			  AND (runner_instance_id IS NULL OR runner_instance_id = '')
		`, runnerInstanceID, attemptID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		bound = n > 0
		return nil
	})
	return bound, err
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

func collectTaskTreeIDsTx(ctx context.Context, tx *sql.Tx, rootTaskID string) ([]string, error) {
	seen := map[string]bool{rootTaskID: true}
	order := []string{rootTaskID}
	for i := 0; i < len(order); i++ {
		cur := order[i]
		rows, err := tx.QueryContext(ctx, `SELECT task_id FROM tasks WHERE parent_task_id = ?`, cur)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var cid string
			if err := rows.Scan(&cid); err != nil {
				rows.Close()
				return nil, err
			}
			if !seen[cid] {
				seen[cid] = true
				order = append(order, cid)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return order, nil
}

func sqlPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func isHebi8MainTaskInTreeTx(ctx context.Context, tx *sql.Tx, allIDs []string) (bool, error) {
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
	mainID := strings.TrimSpace(doc.TaskID)
	if mainID == "" {
		return false, nil
	}
	for _, id := range allIDs {
		if id == mainID {
			return true, nil
		}
	}
	return false, nil
}

// ErrProtectedTask aliases ErrForbidden for protected task guard checks (e.g. Hebi8 main task).
var ErrProtectedTask = ErrForbidden

func checkTaskTreeProtectedTx(ctx context.Context, tx *sql.Tx, taskID, projectID string) error {
	allIDs, err := collectTaskTreeIDsTx(ctx, tx, taskID)
	if err != nil {
		return err
	}
	if isMain, mErr := isHebi8MainTaskInTreeTx(ctx, tx, allIDs); mErr != nil {
		return mErr
	} else if isMain {
		return fmt.Errorf("%w: cannot archive the Hebi8 main conversation", ErrProtectedTask)
	}
	if projectID == protocol.PAProjectID {
		ph := sqlPlaceholders(len(allIDs))
		idArgs := toAnySlice(allIDs)
		var otherActivePA int
		if err := tx.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT COUNT(*) FROM tasks WHERE project_id = ? AND archived_at IS NULL AND task_id NOT IN (%s)`, ph),
			append([]any{protocol.PAProjectID}, idArgs...)...,
		).Scan(&otherActivePA); err != nil {
			return err
		}
		if otherActivePA == 0 {
			return fmt.Errorf("%w: cannot archive the only active Hebi8 conversation", ErrProtectedTask)
		}
	}
	return nil
}

// CheckTaskTreeProtected verifies whether taskID (or any subtask in its tree) is a protected
// Hebi8 main conversation or the sole active _pa conversation.
func (s *Store) CheckTaskTreeProtected(ctx context.Context, taskID string) error {
	return s.WithReadTx(ctx, func(tx *sql.Tx) error {
		var projectID string
		if err := tx.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE task_id = ?`, taskID).Scan(&projectID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: task %s", ErrNotFound, taskID)
			}
			return err
		}
		return checkTaskTreeProtectedTx(ctx, tx, taskID, projectID)
	})
}

// CancelActiveAttemptIfArchivable atomically checks task-tree protection (e.g. Hebi8 main task)
// and cancels/requests cancel on the task's active attempt inside ONE BEGIN IMMEDIATE write tx,
// eliminating TOCTOU races between main-task selection and archive-with-cancel.
func (s *Store) CancelActiveAttemptIfArchivable(ctx context.Context, taskID, actorID, reason string) (CancelTaskResult, error) {
	var res CancelTaskResult
	now := time.Now().UnixMilli()
	if actorID == "" {
		actorID = "human"
	}

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var projectID string
		var activeAttID sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT project_id, active_attempt_id FROM tasks WHERE task_id = ?`, taskID).Scan(&projectID, &activeAttID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: task %s", ErrNotFound, taskID)
		}
		if err != nil {
			return err
		}
		if err := checkTaskTreeProtectedTx(ctx, tx, taskID, projectID); err != nil {
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

		// Only cancel immediately without runner confirmation when the leased attempt
		// has NEVER been dispatched to any runner_instance_id.
		neverDispatched := att.State == protocol.AttemptLeased && att.RunnerInstanceID == ""
		if !neverDispatched {
			if _, err := tx.ExecContext(ctx, `
				UPDATE attempts
				SET cancel_requested = 1,
				    cancel_reason_kind = 'human',
				    steer_input_id = NULL
				WHERE attempt_id = ?
			`, att.AttemptID); err != nil {
				return err
			}
			res.Attempt.CancelRequested = true
			res.Attempt.CancelReasonKind = "human"
			res.Attempt.SteerInputID = ""
			res.NeedsRunnerSend = true
		} else {
			if err := cancelAttemptTx(ctx, tx, att, actorID, reason, now, &res.Events); err != nil {
				return err
			}
			res.CancelledNow = true
			res.NeedsRunnerSend = false
		}
		return nil
	})
	return res, err
}

// ArchiveTask archives a task and its direct/descendant subtasks in one transaction,
// dismissing open inbox items, deactivating ryugi_bindings, and emitting task.archived.
// Returns ErrTaskActive if the task or any subtask currently has an active attempt.
func (s *Store) ArchiveTask(ctx context.Context, taskID, actorID string) (protocol.TaskSummary, *protocol.EventRecord, error) {
	var out protocol.TaskSummary
	var emitted *protocol.EventRecord
	now := time.Now().UnixMilli()
	if actorID == "" {
		actorID = "human"
	}

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var projectID string
		var activeAtt sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT project_id, active_attempt_id FROM tasks WHERE task_id = ?`, taskID).Scan(&projectID, &activeAtt)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: task %s", ErrNotFound, taskID)
		}
		if err != nil {
			return err
		}

		allIDs, err := collectTaskTreeIDsTx(ctx, tx, taskID)
		if err != nil {
			return err
		}
		ph := sqlPlaceholders(len(allIDs))
		idArgs := toAnySlice(allIDs)

		// Reject if target task or any subtask has an active attempt.
		var activeCount int
		if err := tx.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT COUNT(*) FROM tasks WHERE task_id IN (%s) AND active_attempt_id IS NOT NULL AND active_attempt_id != ''`, ph),
			idArgs...,
		).Scan(&activeCount); err != nil {
			return err
		}
		if activeCount > 0 {
			return fmt.Errorf("%w: task %s has an active attempt", ErrTaskActive, taskID)
		}

		// Protect the unique Hebi8 main conversation (hebi8_main_task_id) and sole active _pa conversation from accidental archive.
		if isMain, mErr := isHebi8MainTaskInTreeTx(ctx, tx, allIDs); mErr != nil {
			return mErr
		} else if isMain {
			return fmt.Errorf("%w: cannot archive the Hebi8 main conversation", ErrForbidden)
		}
		if projectID == protocol.PAProjectID {
			var otherActivePA int
			if err := tx.QueryRowContext(ctx,
				fmt.Sprintf(`SELECT COUNT(*) FROM tasks WHERE project_id = ? AND archived_at IS NULL AND task_id NOT IN (%s)`, ph),
				append([]any{protocol.PAProjectID}, idArgs...)...,
			).Scan(&otherActivePA); err != nil {
				return err
			}
			if otherActivePA == 0 {
				return fmt.Errorf("%w: cannot archive the only active Hebi8 conversation", ErrForbidden)
			}
		}

		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE tasks SET archived_at = COALESCE(archived_at, ?), version = version + 1 WHERE task_id IN (%s)`, ph),
			append([]any{now}, idArgs...)...,
		); err != nil {
			return fmt.Errorf("archive tasks: %w", err)
		}

		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE inbox_items SET state = 'dismissed', resolved_at = ?, updated_at = ? WHERE task_id IN (%s) AND state = 'open'`, ph),
			append([]any{now, now}, idArgs...)...,
		); err != nil {
			return fmt.Errorf("dismiss task inbox_items: %w", err)
		}

		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE ryugi_bindings SET active = 0, updated_at = ? WHERE task_id IN (%s) AND active = 1`, ph),
			append([]any{now}, idArgs...)...,
		); err != nil {
			return fmt.Errorf("deactivate ryugi_bindings: %w", err)
		}

		ev, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + taskID,
			ProjectID:  projectID,
			TaskID:     taskID,
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventTaskArchived,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"taskId":     taskID,
				"projectId":  projectID,
				"archivedAt": now,
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

// UnarchiveTask restores an archived task and its subtasks, reactivating ryugi_bindings
// whose frozen_project_dir still exists on disk, and emits task.unarchived.
func (s *Store) UnarchiveTask(ctx context.Context, taskID, actorID string) (protocol.TaskSummary, *protocol.EventRecord, error) {
	var out protocol.TaskSummary
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

		allIDs, err := collectTaskTreeIDsTx(ctx, tx, taskID)
		if err != nil {
			return err
		}
		ph := sqlPlaceholders(len(allIDs))
		idArgs := toAnySlice(allIDs)

		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE tasks SET archived_at = NULL, version = version + 1 WHERE task_id IN (%s)`, ph),
			idArgs...,
		); err != nil {
			return fmt.Errorf("unarchive tasks: %w", err)
		}

		// Reactivate ryugi_bindings whose frozen directory still exists on disk.
		bRows, err := tx.QueryContext(ctx,
			fmt.Sprintf(`SELECT binding_id, frozen_project_dir FROM ryugi_bindings WHERE task_id IN (%s) AND active = 0`, ph),
			idArgs...,
		)
		if err != nil {
			return fmt.Errorf("query ryugi_bindings for unarchive: %w", err)
		}
		var reactivateIDs []string
		for bRows.Next() {
			var bid, dir string
			if err := bRows.Scan(&bid, &dir); err != nil {
				bRows.Close()
				return err
			}
			if strings.TrimSpace(dir) != "" {
				if info, sErr := os.Stat(dir); sErr == nil && info.IsDir() {
					reactivateIDs = append(reactivateIDs, bid)
				}
			}
		}
		if err := bRows.Err(); err != nil {
			bRows.Close()
			return err
		}
		bRows.Close()

		for _, bid := range reactivateIDs {
			if _, err := tx.ExecContext(ctx, `UPDATE ryugi_bindings SET active = 1, updated_at = ? WHERE binding_id = ?`, now, bid); err != nil {
				return fmt.Errorf("reactivate ryugi_binding %s: %w", bid, err)
			}
		}

		ev, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + taskID,
			ProjectID:  projectID,
			TaskID:     taskID,
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventTaskUnarchived,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"taskId":    taskID,
				"projectId": projectID,
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

// DeleteTask permanently deletes a task and all its subtasks across all 12 dependent child tables
// in strict reverse FK order inside a single write transaction. Returns the owning projectID,
// a deduplicated list of deleted deliveryIDs (so Hub cleans up only under its own dataDir/deliveries/),
// and the emitted task.deleted event. Symmetrically decrements delivery_blobs.ref_count for removed deliveries.
func (s *Store) DeleteTask(ctx context.Context, taskID, actorID string) (string, []string, *protocol.EventRecord, error) {
	var projectID string
	var deliveryIDs []string
	var emitted *protocol.EventRecord
	now := time.Now().UnixMilli()
	if actorID == "" {
		actorID = "human"
	}

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE task_id = ?`, taskID).Scan(&projectID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: task %s", ErrNotFound, taskID)
			}
			return err
		}

		allIDs, err := collectTaskTreeIDsTx(ctx, tx, taskID)
		if err != nil {
			return err
		}
		ph := sqlPlaceholders(len(allIDs))
		idArgs := toAnySlice(allIDs)

		// Reject if any task in the tree has an active attempt.
		var activeCount int
		if err := tx.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT COUNT(*) FROM tasks WHERE task_id IN (%s) AND active_attempt_id IS NOT NULL AND active_attempt_id != ''`, ph),
			idArgs...,
		).Scan(&activeCount); err != nil {
			return err
		}
		if activeCount > 0 {
			return fmt.Errorf("%w: task %s has an active attempt", ErrTaskActive, taskID)
		}
		var activeAttRows int
		if err := tx.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT COUNT(*) FROM attempts WHERE task_id IN (%s) AND state IN (%s)`, ph, protocol.SQLInClause(protocol.ActiveAttemptStates)),
			idArgs...,
		).Scan(&activeAttRows); err != nil {
			return err
		}
		if activeAttRows > 0 {
			return fmt.Errorf("%w: task %s has an active attempt", ErrTaskActive, taskID)
		}

		// Protect the unique Hebi8 main conversation (hebi8_main_task_id) and sole _pa conversation from deletion.
		if isMain, mErr := isHebi8MainTaskInTreeTx(ctx, tx, allIDs); mErr != nil {
			return mErr
		} else if isMain {
			return fmt.Errorf("%w: cannot delete the Hebi8 main conversation", ErrForbidden)
		}
		if projectID == protocol.PAProjectID {
			var otherPA int
			if err := tx.QueryRowContext(ctx,
				fmt.Sprintf(`SELECT COUNT(*) FROM tasks WHERE project_id = ? AND task_id NOT IN (%s)`, ph),
				append([]any{protocol.PAProjectID}, idArgs...)...,
			).Scan(&otherPA); err != nil {
				return err
			}
			if otherPA == 0 {
				return fmt.Errorf("%w: cannot delete the only Hebi8 conversation", ErrForbidden)
			}
		}

		// Collect deleted delivery IDs (for safe Hub-side dataDir/deliveries/<deliveryId> cleanup)
		// and symmetrically decrement delivery_blobs.ref_count once per distinct sha256 per delivery.
		seenDlvs := map[string]bool{}
		blobDecCounts := map[string]int{}
		dlvRows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT delivery_id, manifest_json FROM deliveries WHERE task_id IN (%s)`, ph), idArgs...)
		if err != nil {
			return fmt.Errorf("query deliveries for delete: %w", err)
		}
		for dlvRows.Next() {
			var dlvID, manifestJSON string
			if err := dlvRows.Scan(&dlvID, &manifestJSON); err != nil {
				dlvRows.Close()
				return err
			}
			dlvID = strings.TrimSpace(dlvID)
			if dlvID != "" && !seenDlvs[dlvID] {
				seenDlvs[dlvID] = true
				deliveryIDs = append(deliveryIDs, dlvID)
			}
			if strings.TrimSpace(manifestJSON) != "" {
				var man protocol.DeliveryManifest
				if json.Unmarshal([]byte(manifestJSON), &man) == nil && len(man.Blobs) > 0 {
					seenInDlv := map[string]bool{}
					for _, b := range man.Blobs {
						sha := strings.ToLower(strings.TrimSpace(b.SHA256))
						if sha != "" && !seenInDlv[sha] {
							seenInDlv[sha] = true
							blobDecCounts[sha]++
						}
					}
				}
			}
		}
		if err := dlvRows.Err(); err != nil {
			dlvRows.Close()
			return err
		}
		dlvRows.Close()

		for sha, dec := range blobDecCounts {
			if _, err := tx.ExecContext(ctx, `
				UPDATE delivery_blobs SET ref_count = MAX(0, ref_count - ?) WHERE sha256 = ?
			`, dec, sha); err != nil {
				return fmt.Errorf("decrement delivery_blobs ref_count for %s: %w", sha, err)
			}
		}

		// Reverse FK cascade cleanup across all 12 child tables + subtasks -> target task.
		// 1. annotations (clear any cross-task references to these deliveries/attempts first)
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE annotations SET resolution_delivery_id = NULL
			WHERE resolution_delivery_id IN (SELECT delivery_id FROM deliveries WHERE task_id IN (%s))
		`, ph), idArgs...); err != nil {
			return fmt.Errorf("clear annotation resolution_delivery_id: %w", err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE annotations SET triggered_attempt_id = NULL
			WHERE triggered_attempt_id IN (SELECT attempt_id FROM attempts WHERE task_id IN (%s))
		`, ph), idArgs...); err != nil {
			return fmt.Errorf("clear annotation triggered_attempt_id: %w", err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM annotations WHERE task_id IN (%s)`, ph), idArgs...); err != nil {
			return fmt.Errorf("delete annotations: %w", err)
		}

		// 2. ryugi_bindings
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM ryugi_bindings WHERE task_id IN (%s)`, ph), idArgs...); err != nil {
			return fmt.Errorf("delete ryugi_bindings: %w", err)
		}

		// 3. deliveries + atomically enqueue deleted deliveryIDs into pending_delivery_cleanups in the same tx
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM deliveries WHERE task_id IN (%s)`, ph), idArgs...); err != nil {
			return fmt.Errorf("delete deliveries: %w", err)
		}
		for _, dlvID := range deliveryIDs {
			if _, err := tx.ExecContext(ctx, `
				INSERT OR REPLACE INTO pending_delivery_cleanups (delivery_id, created_at)
				VALUES (?, ?)
			`, dlvID, now); err != nil {
				return fmt.Errorf("record pending_delivery_cleanups for %s: %w", dlvID, err)
			}
		}

		// 4. approvals
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM approvals WHERE task_id IN (%s)`, ph), idArgs...); err != nil {
			return fmt.Errorf("delete approvals: %w", err)
		}

		// 5. task_pending_inputs
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM task_pending_inputs WHERE task_id IN (%s)`, ph), idArgs...); err != nil {
			return fmt.Errorf("delete task_pending_inputs: %w", err)
		}

		// 6. inbox_items
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM inbox_items WHERE task_id IN (%s)`, ph), idArgs...); err != nil {
			return fmt.Errorf("delete inbox_items: %w", err)
		}

		// 7. task_sources
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM task_sources WHERE task_id IN (%s)`, ph), idArgs...); err != nil {
			return fmt.Errorf("delete task_sources: %w", err)
		}

		// 8. board_messages
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM board_messages WHERE task_id IN (%s)`, ph), idArgs...); err != nil {
			return fmt.Errorf("delete board_messages: %w", err)
		}

		// 9. board_members (clear references to worker_task_id from any parent board first)
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE board_members SET worker_task_id = NULL WHERE worker_task_id IN (%s)`, ph), idArgs...); err != nil {
			return fmt.Errorf("clear board_members.worker_task_id: %w", err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM board_members WHERE task_id IN (%s)`, ph), idArgs...); err != nil {
			return fmt.Errorf("delete board_members: %w", err)
		}

		// 9b. child_parent_notifications (must precede attempts & tasks deletion due to FKs)
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			DELETE FROM child_parent_notifications
			WHERE child_task_id IN (%s)
			   OR parent_task_id IN (%s)
			   OR child_attempt_id IN (SELECT attempt_id FROM attempts WHERE task_id IN (%s))
		`, ph, ph, ph), append(append(append([]any(nil), idArgs...), idArgs...), idArgs...)...); err != nil {
			return fmt.Errorf("delete child_parent_notifications: %w", err)
		}

		// 10. attempts
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM attempts WHERE task_id IN (%s)`, ph), idArgs...); err != nil {
			return fmt.Errorf("delete attempts: %w", err)
		}

		// 11. agent_sessions
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM agent_sessions WHERE task_id IN (%s)`, ph), idArgs...); err != nil {
			return fmt.Errorf("delete agent_sessions: %w", err)
		}

		// 12. events
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM events WHERE task_id IN (%s)`, ph), idArgs...); err != nil {
			return fmt.Errorf("delete events: %w", err)
		}

		// 13. subtasks (deepest first) -> target task
		for i := len(allIDs) - 1; i >= 0; i-- {
			if _, err := tx.ExecContext(ctx, `DELETE FROM tasks WHERE task_id = ?`, allIDs[i]); err != nil {
				return fmt.Errorf("delete task %s: %w", allIDs[i], err)
			}
		}

		ev, err := insertEventTx(ctx, tx, eventInsert{
			StreamID:   "project:" + projectID,
			ProjectID:  projectID,
			TaskID:     taskID,
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventTaskDeleted,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"taskId":       taskID,
				"projectId":    projectID,
				"deletedCount": len(allIDs),
			},
			CreatedAt: now,
		})
		if err != nil {
			return err
		}
		emitted = &ev
		return nil
	})
	return projectID, deliveryIDs, emitted, err
}
