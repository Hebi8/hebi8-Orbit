package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

func setupSubsessionStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(tmp, "orbit.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	projects := protocol.ProjectsConfig{
		SchemaVersion: 1,
		Projects: map[string]protocol.ProjectConfig{
			"demo-app": {
				DisplayName:       "Demo App",
				RepoIdentity:      "github.com/example-org/demo-app",
				DefaultBranch:     "main",
				DefaultCapability: "primary-dev",
				DefaultAdapter:    protocol.AdapterAgy,
			},
		},
	}
	machines := protocol.MachinesConfig{
		SchemaVersion: 1,
		Machines: map[string]protocol.MachineConfig{
			"hub-node": {
				DisplayName:  "hub-node",
				Transport:    protocol.TransportLocalWS,
				Capabilities: []string{"primary-dev"},
			},
		},
	}
	if err := st.SyncRegistry(ctx, tmp, projects, machines); err != nil {
		t.Fatalf("sync registry: %v", err)
	}
	return st
}

func TestSubsession_DepthAndActiveChildLimits(t *testing.T) {
	ctx := context.Background()
	st := setupSubsessionStore(t)

	// Create root task (depth 0)
	rootRes, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: "demo-app",
		Title:     "Root",
		Goal:      "Root goal",
	})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}

	// Depth 1 must succeed and emit EventTaskChildSpawned; depth 2 (grandchild) must fail with ErrMaxSubsessionDepth.
	child1Res, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		ParentTaskID:  rootRes.Task.TaskID,
		DelegatedRole: "level-1",
		Title:         "Child L1",
		Goal:          "Goal L1",
	})
	if err != nil {
		t.Fatalf("depth 1 should succeed, got: %v", err)
	}
	foundSpawn := false
	for _, ev := range child1Res.Events {
		if ev.Kind == protocol.EventTaskChildSpawned && ev.TaskID == rootRes.Task.TaskID {
			foundSpawn = true
			if ev.Payload["childTaskId"] != child1Res.Task.TaskID {
				t.Fatalf("unexpected childTaskId in spawn event: %+v", ev.Payload)
			}
			if ev.Payload["role"] != "level-1" {
				t.Fatalf("unexpected role in spawn event: %+v", ev.Payload)
			}
		}
	}
	if !foundSpawn {
		t.Fatalf("expected task.child_spawned event on parent %s", rootRes.Task.TaskID)
	}

	// Depth 2 (grandchild) must be rejected with ErrMaxSubsessionDepth
	if _, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:    "demo-app",
		ParentTaskID: child1Res.Task.TaskID,
		Title:        "Too Deep",
		Goal:         "Should fail",
	}); !errors.Is(err, store.ErrMaxSubsessionDepth) {
		t.Fatalf("expected ErrMaxSubsessionDepth for depth 2, got: %v", err)
	}

	// PA/Hebi8 parent must be rejected with ErrPANoSubsessions
	hebi8Task, _, err := st.EnsureHebi8MainTask(ctx, protocol.AdapterAgy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:    protocol.ScratchProjectID,
		ParentTaskID: hebi8Task.TaskID,
		Title:        "PA Child Blocked",
		Goal:         "Should fail",
	}); !errors.Is(err, store.ErrPANoSubsessions) {
		t.Fatalf("expected ErrPANoSubsessions for _pa parent, got: %v", err)
	}

	// Sibling child under same root succeeds; verify IsDescendantTask & IsSiblingTask
	child2Res, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		ParentTaskID:  rootRes.Task.TaskID,
		DelegatedRole: "peer",
		Title:         "Child Peer",
		Goal:          "Goal Peer",
	})
	if err != nil {
		t.Fatalf("sibling child should succeed: %v", err)
	}
	descOK, err := st.IsDescendantTask(ctx, rootRes.Task.TaskID, child1Res.Task.TaskID)
	if err != nil || !descOK {
		t.Fatalf("expected %s to be descendant of %s (ok=%v err=%v)", child1Res.Task.TaskID, rootRes.Task.TaskID, descOK, err)
	}
	selfOK, err := st.IsDescendantTask(ctx, rootRes.Task.TaskID, rootRes.Task.TaskID)
	if err != nil || selfOK {
		t.Fatalf("task must not be descendant of itself (ok=%v err=%v)", selfOK, err)
	}
	sibOK, err := st.IsSiblingTask(ctx, child1Res.Task.TaskID, child2Res.Task.TaskID)
	if err != nil || !sibOK {
		t.Fatalf("expected child1 and child2 to be siblings (ok=%v err=%v)", sibOK, err)
	}
	sibSelf, err := st.IsSiblingTask(ctx, child1Res.Task.TaskID, child1Res.Task.TaskID)
	if err != nil || sibSelf {
		t.Fatalf("task must not be sibling of itself (ok=%v err=%v)", sibSelf, err)
	}
	sibParent, err := st.IsSiblingTask(ctx, rootRes.Task.TaskID, child1Res.Task.TaskID)
	if err != nil || sibParent {
		t.Fatalf("parent and child must not be siblings (ok=%v err=%v)", sibParent, err)
	}

	// Active child cap: 12 allowed, 13th rejected with ErrConflict
	capRoot, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: "demo-app",
		Title:     "Cap Root",
		Goal:      "Cap test",
	})
	if err != nil {
		t.Fatal(err)
	}
	var firstChildID string
	for i := range 12 {
		c, err := st.CreateTask(ctx, store.CreateTaskParams{
			ProjectID:    "demo-app",
			ParentTaskID: capRoot.Task.TaskID,
			Title:        fmt.Sprintf("Worker %d", i+1),
			Goal:         "work",
		})
		if err != nil {
			t.Fatalf("child %d should succeed: %v", i+1, err)
		}
		if i == 0 {
			firstChildID = c.Task.TaskID
		}
	}
	if _, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:    "demo-app",
		ParentTaskID: capRoot.Task.TaskID,
		Title:        "Worker 13",
		Goal:         "should conflict",
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict for 13th active child, got: %v", err)
	}

	// Mark first child in_review -> frees active slot so 13th succeeds
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE tasks SET state = 'in_review' WHERE task_id = ?`, firstChildID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:    "demo-app",
		ParentTaskID: capRoot.Task.TaskID,
		Title:        "Worker 13 retry",
		Goal:         "now allowed",
	}); err != nil {
		t.Fatalf("expected 13th child to succeed after one finished: %v", err)
	}
}

func TestSubsession_TimelineChildrenTranscriptAndClaimNotification(t *testing.T) {
	ctx := context.Background()
	st := setupSubsessionStore(t)

	// Create top-level project task as parent (PA/Hebi8 cannot attach sub-sessions)
	parentRes, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: "demo-app",
		Title:     "Main Coordinator",
		Goal:      "Coordinate feature work",
		MachineID: "hub-node",
	})
	if err != nil {
		t.Fatal(err)
	}
	parentTask := parentRes.Task

	// Spawn 2 children under parentTask
	c1, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		ParentTaskID:  parentTask.TaskID,
		DelegatedRole: "backend",
		Title:         "Build API",
		Goal:          "Implement backend API",
		MachineID:     "hub-node",
		Adapter:       protocol.AdapterCodex,
		DesiredConfig: map[string]string{"model": "gpt-test"},
		CreateAttempt: true,
		Prompt:        "Implement backend API",
	})
	if err != nil {
		t.Fatal(err)
	}
	c2, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     protocol.ScratchProjectID,
		ParentTaskID:  parentTask.TaskID,
		DelegatedRole: "researcher",
		Title:         "Research",
		Goal:          "Investigate design",
		MachineID:     "hub-node",
		Adapter:       protocol.AdapterAgy,
		CreateAttempt: true,
		Prompt:        "Investigate design",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate outbox events on c1: message batch, tool call, approval request, folder changes, finish
	longReply := strings.Repeat("A", 4500)
	now := time.Now().UnixMilli()
	_, err = st.IngestOutboxBatch(ctx, "hub-node", "run-1", protocol.AttemptOutboxBatchFrame{
		AttemptID:  c1.Attempt.AttemptID,
		LeaseToken: c1.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{
				OutboxSeq: 1,
				EventID:   "evt_msg_1",
				FrameType: protocol.OutboxFrameEvents,
				Payload: map[string]any{
					"kind":       string(protocol.EventACPMessageBatch),
					"visibility": string(protocol.VisibilityRawACP),
					"actorKind":  string(protocol.ActorAgent),
					"actorId":    "codex",
					"payload":    map[string]any{"text": longReply},
				},
				TS: now,
			},
			{
				OutboxSeq: 2,
				EventID:   "evt_tool_1",
				FrameType: protocol.OutboxFrameEvents,
				Payload: map[string]any{
					"kind":       string(protocol.EventACPToolCall),
					"visibility": string(protocol.VisibilityRawACP),
					"actorKind":  string(protocol.ActorAgent),
					"actorId":    "codex",
					"payload":    map[string]any{"toolCallId": "tc_1", "name": "run_tests", "title": "Running unit tests"},
				},
				TS: now + 1,
			},
			{
				OutboxSeq: 3,
				EventID:   "evt_tool_1_upd",
				FrameType: protocol.OutboxFrameEvents,
				Payload: map[string]any{
					"kind":       string(protocol.EventACPToolCall),
					"visibility": string(protocol.VisibilityRawACP),
					"actorKind":  string(protocol.ActorAgent),
					"actorId":    "codex",
					"payload":    map[string]any{"toolCallId": "tc_1", "name": "run_tests", "title": "Running unit tests", "status": "completed"},
				},
				TS: now + 2,
			},
			{
				OutboxSeq: 4,
				EventID:   "evt_apr_1",
				FrameType: protocol.OutboxFrameApprovalRequest,
				Payload: map[string]any{
					"approvalId":    "apr_child_1",
					"kind":          string(protocol.ApprovalACPToolPermission),
					"actionName":    "shell",
					"targetEnv":     "hub-node:worktree",
					"payloadSha256": "abc",
					"options":       []protocol.ApprovalOption{{ID: "allow", Label: "Allow", Kind: "allow_once"}},
				},
				TS: now + 3,
			},
		},
	})
	if err != nil {
		t.Fatalf("ingest child outbox: %v", err)
	}

	// Verify parent TaskSummary counters
	parentSummary, err := st.GetTask(ctx, parentTask.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if parentSummary.ChildCount != 2 || parentSummary.ActiveChildCount != 2 || parentSummary.PendingChildApprovals != 1 {
		t.Fatalf("unexpected parent summary counters: child=%d active=%d approvals=%d",
			parentSummary.ChildCount, parentSummary.ActiveChildCount, parentSummary.PendingChildApprovals)
	}

	// Verify parent GetTaskTimeline populates Children
	parentTL, err := st.GetTaskTimeline(ctx, parentTask.TaskID, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(parentTL.Children) != 2 {
		t.Fatalf("expected 2 children in parent timeline, got %d", len(parentTL.Children))
	}
	child1View := parentTL.Children[0]
	if child1View.TaskID != c1.Task.TaskID || child1View.ProjectName != "Demo App" || child1View.Model != "gpt-test" ||
		child1View.OpenApprovalCount != 1 || child1View.CurrentStep != "Running unit tests" || child1View.AttemptCount != 1 {
		t.Fatalf("unexpected child1 view: %+v", child1View)
	}
	if len([]rune(child1View.LastMessagePreview)) > 241 {
		t.Fatalf("expected preview truncated <= 240 runes + ellipsis, got %d", len([]rune(child1View.LastMessagePreview)))
	}

	// Verify child GetTaskTimeline populates ParentSummary and SiblingChildren
	childTL, err := st.GetTaskTimeline(ctx, c1.Task.TaskID, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if childTL.ParentSummary == nil || childTL.ParentSummary.IsHebi8Main || childTL.ParentSummary.TaskID != parentTask.TaskID {
		t.Fatalf("unexpected ParentSummary: %+v", childTL.ParentSummary)
	}
	if len(childTL.SiblingChildren) != 2 {
		t.Fatalf("expected 2 sibling children, got %d", len(childTL.SiblingChildren))
	}

	// Verify GetTaskTranscript truncates reply <= 4000 runes and deduplicates toolCallId
	tr, err := st.GetTaskTranscript(ctx, c1.Task.TaskID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if tr.OpenApprovalCount != 1 || len(tr.Turns) != 1 {
		t.Fatalf("unexpected transcript header: %+v", tr)
	}
	if tr.Turns[0].ToolCount != 1 {
		t.Fatalf("expected deduplicated ToolCount=1, got %d", tr.Turns[0].ToolCount)
	}
	if len([]rune(tr.Turns[0].Reply)) > 4001 {
		t.Fatalf("expected reply truncated <= 4000 runes + ellipsis, got %d", len([]rune(tr.Turns[0].Reply)))
	}

	// Verify ClaimChildParentNotification concurrency safety (exactly 1 goroutine wins)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, pid, err := st.ClaimChildParentNotification(ctx, c1.Task.TaskID, c1.Attempt.AttemptID)
			if err != nil {
				t.Errorf("claim error: %v", err)
				return
			}
			if claimed {
				if pid != parentTask.TaskID {
					t.Errorf("unexpected parentTaskID %q", pid)
				}
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("expected exactly 1 atomic claim winner, got %d", wins.Load())
	}

	// NotifyParentOnFinish=false must never claim
	f := false
	cSilent, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:            "demo-app",
		ParentTaskID:         parentTask.TaskID,
		Title:                "Silent child",
		Goal:                 "No wakeup",
		NotifyParentOnFinish: &f,
		CreateAttempt:        true,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, _, err := st.ClaimChildParentNotification(ctx, cSilent.Task.TaskID, cSilent.Attempt.AttemptID)
	if err != nil || claimed {
		t.Fatalf("expected silent child not to claim notification (claimed=%v err=%v)", claimed, err)
	}

	// Sibling relationship: c2 is NOT descendant of c1, IS sibling of c1
	ok, err := st.IsDescendantTask(ctx, c1.Task.TaskID, c2.Task.TaskID)
	if err != nil || ok {
		t.Fatalf("sibling must not be descendant (ok=%v err=%v)", ok, err)
	}
	sib, err := st.IsSiblingTask(ctx, c1.Task.TaskID, c2.Task.TaskID)
	if err != nil || !sib {
		t.Fatalf("expected c1 and c2 to be siblings (sib=%v err=%v)", sib, err)
	}
}

func TestSubsession_LegacyAttemptsTriggerKindMigration(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "legacy-v6.sqlite")

	// Open normally first, then downgrade attempts table CHECK constraint to simulate a pre-v7 database
	// (using legacy_alter_table=ON so referencing tables stay clean as in a genuine pre-v7 DB).
	st1, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st1.WriteDB().ExecContext(ctx, `
		PRAGMA foreign_keys = OFF;
		PRAGMA legacy_alter_table = ON;
		ALTER TABLE attempts RENAME TO attempts_bak;
		CREATE TABLE attempts (
			attempt_id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL,
			project_id TEXT NOT NULL,
			attempt_no INTEGER NOT NULL,
			machine_id TEXT NOT NULL,
			adapter TEXT NOT NULL,
			wb_session_id TEXT,
			trigger_kind TEXT NOT NULL CHECK (trigger_kind IN ('initial','followup','annotation_batch','retry','deploy','workspace_import','manual_import')),
			trigger_payload_json TEXT NOT NULL DEFAULT '{}',
			worktree_path TEXT,
			input_delivery_id TEXT,
			base_sha TEXT,
			output_delivery_id TEXT,
			target_env TEXT,
			deploy_phase TEXT,
			state TEXT NOT NULL,
			resume_state TEXT,
			completion_source TEXT,
			lease_token TEXT NOT NULL,
			lease_expires_at INTEGER NOT NULL,
			last_acked_outbox INTEGER NOT NULL DEFAULT 0,
			error_code TEXT,
			error_detail TEXT,
			token_usage_json TEXT,
			started_at INTEGER NOT NULL,
			finished_at INTEGER,
			runner_instance_id TEXT,
			cancel_requested INTEGER NOT NULL DEFAULT 0,
			cancel_reason_kind TEXT,
			steer_input_id TEXT,
			desired_config_json TEXT,
			required_resources_json TEXT,
			anchor_json TEXT,
			part_id TEXT,
			location_id TEXT,
			isolation TEXT,
			folder_changes_json TEXT
		);
		DROP TABLE attempts_bak;
		CREATE UNIQUE INDEX idx_attempts_one_active_per_task ON attempts(task_id) WHERE state IN ('leased', 'preparing', 'waiting_resource', 'running', 'waiting_approval', 'swarm_monitoring');
		CREATE UNIQUE INDEX idx_attempts_one_active_per_session ON attempts(wb_session_id) WHERE wb_session_id IS NOT NULL AND state IN ('leased', 'preparing', 'waiting_resource', 'running', 'waiting_approval', 'swarm_monitoring');
		CREATE UNIQUE INDEX idx_attempts_one_running_deploy_per_env ON attempts(project_id, target_env) WHERE adapter = 'ssh_deploy' AND state IN ('leased', 'preparing', 'waiting_resource', 'running', 'waiting_approval');
		CREATE INDEX idx_attempts_machine_active ON attempts(machine_id, state) WHERE state IN ('leased', 'preparing', 'waiting_resource', 'running', 'waiting_approval', 'swarm_monitoring');
		PRAGMA legacy_alter_table = OFF;
		PRAGMA foreign_keys = ON;
	`)
	if err != nil {
		t.Fatalf("simulate legacy attempts table: %v", err)
	}
	_ = st1.Close()

	// Re-open -> ensureAttemptsTriggerKindCheck should migrate seamlessly WITHOUT corrupting referencing FKs
	st2, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen legacy db: %v", err)
	}
	defer st2.Close()

	var ddl sql.NullString
	if err := st2.ReadDB().QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='attempts'`).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ddl.String, "'child_result'") {
		t.Fatalf("expected migrated attempts DDL to allow 'child_result', got: %s", ddl.String)
	}
	var legacyRefCount int
	if err := st2.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE sql LIKE '%legacy%' OR sql LIKE '%attempts_bak%'`).Scan(&legacyRefCount); err != nil {
		t.Fatal(err)
	}
	if legacyRefCount != 0 {
		t.Fatalf("expected 0 dangling legacy table references after migration, got %d", legacyRefCount)
	}
}

func TestSubsession_RepairDanglingLegacyAttemptForeignKeys(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "corrupted-fk.sqlite")

	st1, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// Seed a real machine, project, task, attempt, and delivery BEFORE corrupting FKs
	if err := st1.SyncRegistry(ctx, tmp,
		protocol.ProjectsConfig{
			SchemaVersion: 1,
			Projects: map[string]protocol.ProjectConfig{
				"demo": {
					DisplayName:       "Demo",
					RepoIdentity:      "demo",
					DefaultBranch:     "main",
					DefaultCapability: "primary-dev",
					DefaultAdapter:    protocol.AdapterAgy,
					Anchors: map[string]protocol.ProjectAnchorConfig{
						"hub-node": {RepoPath: "/tmp/demo"},
					},
				},
			},
		},
		protocol.MachinesConfig{
			SchemaVersion: 1,
			Machines: map[string]protocol.MachineConfig{
				"hub-node": {
					DisplayName:  "hub-node",
					Transport:    protocol.TransportLocalWS,
					Capabilities: []string{"primary-dev"},
				},
			},
		},
	); err != nil {
		t.Fatal(err)
	}
	created, err := st1.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:          "demo",
		Title:              "Existing Task",
		Goal:               "Preserve rows across FK repair",
		RequiredCapability: "primary-dev",
		Adapter:            protocol.AdapterAgy,
		CreatedBy:          protocol.TaskCreatedByHuman,
		MachineID:          "hub-node",
		CreateAttempt:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if _, err := st1.WriteDB().ExecContext(ctx, `
		INSERT INTO deliveries (
			delivery_id, project_id, task_id, attempt_id, source_machine_id,
			repo_identity, base_sha, head_sha, git_ref, kind, summary, manifest_json,
			verified_machines_json, ryugi_sync_state, late_arrival, pinned, created_at
		) VALUES ('dlv_keep_1', 'demo', ?, ?, 'hub-node', 'demo', 'base1', 'head1', 'refs/orbit/keep1', 'code_diff', 'keep', '{}', '{}', 'none', 0, 1, ?)
	`, created.Task.TaskID, created.Attempt.AttemptID, now); err != nil {
		t.Fatalf("seed delivery: %v", err)
	}

	// Reproduce exact SQLite 3.26+ corruption: ALTER TABLE attempts RENAME TO attempts_legacy_v6 without legacy_alter_table=ON
	_, err = st1.WriteDB().ExecContext(ctx, `
		PRAGMA foreign_keys = OFF;
		ALTER TABLE attempts RENAME TO attempts_legacy_v6;
		CREATE TABLE attempts AS SELECT * FROM attempts_legacy_v6 WHERE 0;
		DROP TABLE attempts;
		CREATE TABLE attempts (
			attempt_id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL,
			project_id TEXT NOT NULL,
			attempt_no INTEGER NOT NULL,
			machine_id TEXT NOT NULL REFERENCES machines(machine_id),
			adapter TEXT NOT NULL,
			wb_session_id TEXT REFERENCES agent_sessions(wb_session_id),
			trigger_kind TEXT NOT NULL,
			trigger_payload_json TEXT NOT NULL DEFAULT '{}',
			worktree_path TEXT,
			input_delivery_id TEXT,
			base_sha TEXT,
			output_delivery_id TEXT,
			target_env TEXT,
			deploy_phase TEXT,
			state TEXT NOT NULL,
			resume_state TEXT,
			completion_source TEXT,
			lease_token TEXT NOT NULL,
			lease_expires_at INTEGER NOT NULL,
			last_acked_outbox INTEGER NOT NULL DEFAULT 0,
			error_code TEXT,
			error_detail TEXT,
			token_usage_json TEXT,
			started_at INTEGER NOT NULL,
			finished_at INTEGER,
			runner_instance_id TEXT,
			cancel_requested INTEGER NOT NULL DEFAULT 0,
			cancel_reason_kind TEXT,
			steer_input_id TEXT,
			desired_config_json TEXT,
			required_resources_json TEXT,
			anchor_json TEXT,
			part_id TEXT,
			location_id TEXT,
			isolation TEXT,
			folder_changes_json TEXT,
			FOREIGN KEY (task_id, project_id) REFERENCES tasks(task_id, project_id),
			UNIQUE (task_id, attempt_no),
			UNIQUE (attempt_id, task_id, project_id)
		);
		INSERT INTO attempts SELECT * FROM attempts_legacy_v6;
		DROP TABLE attempts_legacy_v6;
		PRAGMA foreign_keys = ON;
	`)
	if err != nil {
		t.Fatalf("simulate corrupted FKs: %v", err)
	}

	// Confirm the DB is actually broken right now (inserting an approval fails with no such table: main.attempts_legacy_v6)
	_, brokenErr := st1.WriteDB().ExecContext(ctx, `
		INSERT INTO approvals (
			approval_id, project_id, task_id, attempt_id, kind, action_name, target_env,
			input_sha, payload_json, payload_sha256, options_json, requested_by, state,
			expires_at, created_at
		) VALUES ('apr_broken', 'demo', ?, ?, 'acp_tool_permission', 'test', 'dev', '', '{}', 'sha', '[]', 'agent', 'pending', ?, ?)
	`, created.Task.TaskID, created.Attempt.AttemptID, now+60000, now)
	if brokenErr == nil || !strings.Contains(brokenErr.Error(), "attempts_legacy_v6") {
		t.Fatalf("expected corrupted DB insert to fail with attempts_legacy_v6, got: %v", brokenErr)
	}
	_ = st1.Close()

	// Re-open -> repairDanglingAttemptForeignKeys must automatically back up and heal all 4 tables!
	st2, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen corrupted db: %v", err)
	}
	defer st2.Close()

	if _, err := os.Stat(dbPath + ".pre-fk-repair.bak"); err != nil {
		t.Fatalf("expected .pre-fk-repair.bak backup to exist: %v", err)
	}
	var dangling int
	if err := st2.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE sql LIKE '%attempts_legacy_v6%'`).Scan(&dangling); err != nil {
		t.Fatal(err)
	}
	if dangling != 0 {
		t.Fatalf("expected 0 tables referencing attempts_legacy_v6 after repair, got %d", dangling)
	}

	// Existing delivery row + trigger must be preserved!
	var kept int
	if err := st2.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE delivery_id = 'dlv_keep_1'`).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("expected seeded delivery row to survive repair (kept=%d err=%v)", kept, err)
	}
	var triggerCount int
	if err := st2.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = 'trg_deliveries_block_pending_cleanup'`).Scan(&triggerCount); err != nil || triggerCount != 1 {
		t.Fatalf("expected trg_deliveries_block_pending_cleanup trigger to survive repair (count=%d err=%v)", triggerCount, err)
	}

	// Inserts into approvals, task_pending_inputs, annotations, and deliveries MUST now succeed with foreign_keys=ON!
	if _, err := st2.WriteDB().ExecContext(ctx, `
		INSERT INTO approvals (
			approval_id, project_id, task_id, attempt_id, kind, action_name, target_env,
			input_sha, payload_json, payload_sha256, options_json, requested_by, state,
			expires_at, created_at
		) VALUES ('apr_healed', 'demo', ?, ?, 'acp_tool_permission', 'test', 'dev', '', '{}', 'sha', '[]', 'agent', 'pending', ?, ?)
	`, created.Task.TaskID, created.Attempt.AttemptID, now+60000, now); err != nil {
		t.Fatalf("insert into repaired approvals failed: %v", err)
	}
	if _, err := st2.WriteDB().ExecContext(ctx, `
		INSERT INTO task_pending_inputs (
			input_id, task_id, project_id, kind, payload_json, state, created_at, dispatched_attempt_id
		) VALUES ('inp_healed', ?, 'demo', 'followup', '{}', 'dispatched', ?, ?)
	`, created.Task.TaskID, now, created.Attempt.AttemptID); err != nil {
		t.Fatalf("insert into repaired task_pending_inputs failed: %v", err)
	}
	if _, err := st2.WriteDB().ExecContext(ctx, `
		INSERT INTO annotations (
			annotation_id, project_id, task_id, delivery_id, source, anchor_json, body, status, triggered_attempt_id, created_at
		) VALUES ('ann_healed', 'demo', ?, 'dlv_keep_1', 'diff_drawer', '{}', 'ok', 'open', ?, ?)
	`, created.Task.TaskID, created.Attempt.AttemptID, now); err != nil {
		t.Fatalf("insert into repaired annotations failed: %v", err)
	}
}

func TestSubsession_ActiveChildQuotaOnReactivationAndLegacyDepthBackfill(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "quota-depth.sqlite")

	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SyncRegistry(ctx, tmp,
		protocol.ProjectsConfig{
			SchemaVersion: 1,
			Projects: map[string]protocol.ProjectConfig{
				"demo": {
					DisplayName:       "Demo",
					RepoIdentity:      "demo",
					DefaultBranch:     "main",
					DefaultCapability: "primary-dev",
					DefaultAdapter:    protocol.AdapterAgy,
				},
			},
		},
		protocol.MachinesConfig{
			SchemaVersion: 1,
			Machines: map[string]protocol.MachineConfig{
				"hub-node": {DisplayName: "hub-node", Transport: protocol.TransportLocalWS, Capabilities: []string{"primary-dev"}},
			},
		},
	); err != nil {
		t.Fatal(err)
	}

	root, err := st.CreateTask(ctx, store.CreateTaskParams{ProjectID: "demo", Title: "Root", Goal: "root", MachineID: "hub-node"})
	if err != nil {
		t.Fatal(err)
	}

	// Create 12 active children + 1 idle child (13th, finished)
	var childIDs []string
	for i := 0; i < 12; i++ {
		c, err := st.CreateTask(ctx, store.CreateTaskParams{
			ProjectID:     "demo",
			ParentTaskID:  root.Task.TaskID,
			Title:         fmt.Sprintf("Active %d", i),
			Goal:          "work",
			MachineID:     "hub-node",
			CreateAttempt: true,
		})
		if err != nil {
			t.Fatalf("create child %d: %v", i, err)
		}
		childIDs = append(childIDs, c.Task.TaskID)
	}
	// Mark child 0 as finished (in_review, no active attempt), then create a 13th active child so 12 others are active
	now := time.Now().UnixMilli()
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE attempts SET state = 'succeeded', finished_at = ? WHERE task_id = ?`, now, childIDs[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE tasks SET state = 'in_review', active_attempt_id = NULL WHERE task_id = ?`, childIDs[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo",
		ParentTaskID:  root.Task.TaskID,
		Title:         "Active 12th",
		Goal:          "work",
		MachineID:     "hub-node",
		CreateAttempt: true,
	}); err != nil {
		t.Fatalf("create 12th active child: %v", err)
	}

	// Reactivating idle child 0 via SubmitFollowupAtomic MUST fail with ErrConflict (12 active siblings already exist)
	if _, err := st.SubmitFollowupAtomic(ctx, store.SubmitFollowupParams{
		TaskID:    childIDs[0],
		Body:      "try reactivate",
		Mode:      protocol.FollowupModeQueue,
		MachineID: "hub-node",
		Adapter:   protocol.AdapterAgy,
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict when reactivating 13th child via followup, got: %v", err)
	}

	// Reactivating idle child 0 via CreateNextAttempt (retry) MUST also fail with ErrConflict
	if _, _, _, err := st.CreateNextAttempt(ctx, store.CreateNextAttemptParams{
		TaskID:      childIDs[0],
		MachineID:   "hub-node",
		Adapter:     protocol.AdapterAgy,
		TriggerKind: protocol.TriggerRetry,
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict when reactivating 13th child via retry, got: %v", err)
	}

	// Part 2: Legacy 4-level nested tree with delegation_depth=0 & schema version 6 -> v7 upgrade backfills CTE depth & blocks level 5
	prevID := root.Task.TaskID
	var level4ID string
	for lvl := 1; lvl <= 4; lvl++ {
		tid := fmt.Sprintf("tsk_legacy_lvl_%d", lvl)
		if _, err := st.WriteDB().ExecContext(ctx, `
			INSERT INTO tasks (
				task_id, project_id, parent_task_id, title, goal,
				required_capability, required_resources, preferred_adapter, desired_config_json,
				state, created_by, version, created_at, updated_at, delegation_depth
			) VALUES (?, 'demo', ?, ?, 'legacy', 'primary-dev', '[]', 'agy', '{}', 'done', 'human', 1, ?, ?, 0)
		`, tid, prevID, fmt.Sprintf("Legacy L%d", lvl), now, now); err != nil {
			t.Fatalf("seed legacy lvl %d: %v", lvl, err)
		}
		prevID = tid
		level4ID = tid
	}
	// Simulate pre-v7 database state (remove version 7 row)
	if _, err := st.WriteDB().ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = 7`); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	st2, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen for v7 backfill: %v", err)
	}
	defer st2.Close()

	var d4 int
	if err := st2.ReadDB().QueryRowContext(ctx, `SELECT delegation_depth FROM tasks WHERE task_id = ?`, level4ID).Scan(&d4); err != nil {
		t.Fatal(err)
	}
	if d4 != 4 {
		t.Fatalf("expected v7 CTE backfill delegation_depth=4 for level 4 task, got %d", d4)
	}
	if _, err := st2.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:    "demo",
		ParentTaskID: level4ID,
		Title:        "Level 5 Blocked",
		Goal:         "must fail",
		MachineID:    "hub-node",
	}); !errors.Is(err, store.ErrMaxSubsessionDepth) {
		t.Fatalf("expected ErrMaxSubsessionDepth creating child under legacy nested tree, got: %v", err)
	}
}

func TestSubsession_RepairQuotedDeliveriesDDLAndDeleteNotifications(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "quoted-ddl.sqlite")

	st1, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st1.SyncRegistry(ctx, tmp,
		protocol.ProjectsConfig{
			SchemaVersion: 1,
			Projects: map[string]protocol.ProjectConfig{
				"demo": {DisplayName: "Demo", RepoIdentity: "demo", DefaultBranch: "main", DefaultCapability: "primary-dev", DefaultAdapter: protocol.AdapterAgy},
			},
		},
		protocol.MachinesConfig{
			SchemaVersion: 1,
			Machines: map[string]protocol.MachineConfig{
				"hub-node": {DisplayName: "hub-node", Transport: protocol.TransportLocalWS, Capabilities: []string{"primary-dev"}},
			},
		},
	); err != nil {
		t.Fatal(err)
	}
	parent, err := st1.CreateTask(ctx, store.CreateTaskParams{ProjectID: "demo", Title: "P", Goal: "p", MachineID: "hub-node"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := st1.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: "demo", ParentTaskID: parent.Task.TaskID, Title: "C", Goal: "c", MachineID: "hub-node", CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	// Recreate deliveries with an explicitly DOUBLE-QUOTED table name CREATE TABLE "deliveries" (...) AND dangling attempts_legacy_v6 FK
	_, err = st1.WriteDB().ExecContext(ctx, `
		PRAGMA foreign_keys = OFF;
		PRAGMA legacy_alter_table = ON;
		DROP TABLE deliveries;
		CREATE TABLE "deliveries" (
		  delivery_id         TEXT PRIMARY KEY,
		  project_id          TEXT NOT NULL,
		  task_id             TEXT NOT NULL,
		  attempt_id          TEXT NOT NULL,
		  source_machine_id   TEXT NOT NULL REFERENCES machines(machine_id),
		  repo_identity       TEXT NOT NULL,
		  base_sha            TEXT NOT NULL,
		  head_sha            TEXT NOT NULL,
		  git_ref             TEXT NOT NULL UNIQUE,
		  kind                TEXT NOT NULL,
		  summary             TEXT NOT NULL,
		  manifest_json       TEXT NOT NULL,
		  verified_machines_json TEXT NOT NULL DEFAULT '{}',
		  frozen_snapshot_dir TEXT,
		  ryugi_sync_state    TEXT NOT NULL DEFAULT 'none',
		  late_arrival        INTEGER NOT NULL DEFAULT 0,
		  pinned              INTEGER NOT NULL DEFAULT 1,
		  created_at          INTEGER NOT NULL,
		  FOREIGN KEY (attempt_id, task_id, project_id) REFERENCES "attempts_legacy_v6"(attempt_id, task_id, project_id),
		  UNIQUE (delivery_id, task_id, project_id)
		);
		CREATE INDEX idx_deliveries_task ON deliveries(task_id, created_at DESC);
		CREATE TRIGGER trg_deliveries_block_pending_cleanup
		BEFORE INSERT ON deliveries
		WHEN EXISTS (SELECT 1 FROM pending_delivery_cleanups WHERE delivery_id = NEW.delivery_id)
		BEGIN
		  SELECT RAISE(ABORT, 'ERR_DELIVERY_CLEANUP_PENDING');
		END;
	`)
	if err != nil {
		t.Fatalf("seed quoted deliveries DDL: %v", err)
	}
	if _, err := st1.WriteDB().ExecContext(ctx, `
		INSERT INTO deliveries (
			delivery_id, project_id, task_id, attempt_id, source_machine_id,
			repo_identity, base_sha, head_sha, git_ref, kind, summary, manifest_json,
			verified_machines_json, ryugi_sync_state, late_arrival, pinned, created_at
		) VALUES ('dlv_quoted_1', 'demo', ?, ?, 'hub-node', 'demo', 'b1', 'h1', 'refs/orbit/q1', 'code_diff', 'q', '{}', '{}', 'none', 0, 1, ?)
	`, child.Task.TaskID, child.Attempt.AttemptID, now); err != nil {
		t.Fatalf("insert quoted delivery row: %v", err)
	}
	if _, err := st1.WriteDB().ExecContext(ctx, `PRAGMA legacy_alter_table = OFF; PRAGMA foreign_keys = ON;`); err != nil {
		t.Fatal(err)
	}
	// Seed a child_parent_notifications row & finish child attempt so we can verify DeleteTask cascade under foreign_keys=ON
	if _, err := st1.WriteDB().ExecContext(ctx, `
		INSERT INTO child_parent_notifications (child_attempt_id, child_task_id, parent_task_id, child_state, status, created_at)
		VALUES (?, ?, ?, 'succeeded', 'pending', ?)
	`, child.Attempt.AttemptID, child.Task.TaskID, parent.Task.TaskID, now); err != nil {
		t.Fatalf("seed child_parent_notifications: %v", err)
	}
	if _, err := st1.WriteDB().ExecContext(ctx, `UPDATE attempts SET state = 'succeeded', finished_at = ? WHERE attempt_id = ?`, now, child.Attempt.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := st1.WriteDB().ExecContext(ctx, `UPDATE tasks SET state = 'in_review', active_attempt_id = NULL WHERE task_id = ?`, child.Task.TaskID); err != nil {
		t.Fatal(err)
	}
	_ = st1.Close()

	// Re-open -> quoted "deliveries" must repair cleanly without leftover quotes or trigger loss
	st2, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen quoted deliveries db: %v", err)
	}
	defer st2.Close()

	var kept int
	if err := st2.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE delivery_id = 'dlv_quoted_1'`).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("expected dlv_quoted_1 preserved, kept=%d err=%v", kept, err)
	}
	var trig int
	if err := st2.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name='trg_deliveries_block_pending_cleanup'`).Scan(&trig); err != nil || trig != 1 {
		t.Fatalf("expected trigger preserved, trig=%d err=%v", trig, err)
	}
	// DeleteTask on parent MUST cascade-delete child_parent_notifications cleanly under foreign_keys=ON
	if _, _, _, err := st2.DeleteTask(ctx, parent.Task.TaskID, "tester"); err != nil {
		t.Fatalf("DeleteTask with child_parent_notifications failed: %v", err)
	}
}

func TestSubsession_V7MigrationFKViolationRollsBackAtomically(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "v7-rollback.sqlite")

	st1, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// Downgrade to a genuine pre-v7 database (no schema_migrations v7, no child_parent_notifications, legacy attempts DDL)
	_, err = st1.WriteDB().ExecContext(ctx, `
		PRAGMA foreign_keys = OFF;
		PRAGMA legacy_alter_table = ON;
		DELETE FROM schema_migrations WHERE version = 7;
		DROP TABLE IF EXISTS child_parent_notifications;
		ALTER TABLE attempts RENAME TO attempts_bak;
		CREATE TABLE attempts (
			attempt_id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL,
			project_id TEXT NOT NULL,
			attempt_no INTEGER NOT NULL,
			machine_id TEXT NOT NULL,
			adapter TEXT NOT NULL,
			wb_session_id TEXT,
			trigger_kind TEXT NOT NULL CHECK (trigger_kind IN ('initial','followup','annotation_batch','retry','deploy','workspace_import','manual_import')),
			trigger_payload_json TEXT NOT NULL DEFAULT '{}',
			worktree_path TEXT,
			input_delivery_id TEXT,
			base_sha TEXT,
			output_delivery_id TEXT,
			target_env TEXT,
			deploy_phase TEXT,
			state TEXT NOT NULL,
			resume_state TEXT,
			completion_source TEXT,
			lease_token TEXT NOT NULL,
			lease_expires_at INTEGER NOT NULL,
			last_acked_outbox INTEGER NOT NULL DEFAULT 0,
			error_code TEXT,
			error_detail TEXT,
			token_usage_json TEXT,
			started_at INTEGER NOT NULL,
			finished_at INTEGER,
			runner_instance_id TEXT,
			cancel_requested INTEGER NOT NULL DEFAULT 0,
			cancel_reason_kind TEXT,
			steer_input_id TEXT,
			desired_config_json TEXT,
			required_resources_json TEXT,
			anchor_json TEXT,
			part_id TEXT,
			location_id TEXT,
			isolation TEXT,
			folder_changes_json TEXT
		);
		DROP TABLE attempts_bak;
		CREATE UNIQUE INDEX idx_attempts_one_active_per_task ON attempts(task_id) WHERE state IN ('leased', 'preparing', 'waiting_resource', 'running', 'waiting_approval', 'swarm_monitoring');
		CREATE UNIQUE INDEX idx_attempts_one_active_per_session ON attempts(wb_session_id) WHERE wb_session_id IS NOT NULL AND state IN ('leased', 'preparing', 'waiting_resource', 'running', 'waiting_approval', 'swarm_monitoring');
		CREATE UNIQUE INDEX idx_attempts_one_running_deploy_per_env ON attempts(project_id, target_env) WHERE adapter = 'ssh_deploy' AND state IN ('leased', 'preparing', 'waiting_resource', 'running', 'waiting_approval');
		CREATE INDEX idx_attempts_machine_active ON attempts(machine_id, state) WHERE state IN ('leased', 'preparing', 'waiting_resource', 'running', 'waiting_approval', 'swarm_monitoring');
		PRAGMA legacy_alter_table = OFF;
		PRAGMA foreign_keys = ON;
	`)
	if err != nil {
		t.Fatal(err)
	}
	snapshotSchema := func(db *sql.DB) []string {
		rows, err := db.QueryContext(ctx, `
			SELECT type, name, tbl_name, COALESCE(sql, '')
			FROM sqlite_master
			WHERE name NOT LIKE 'sqlite_%'
			ORDER BY type, name
		`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var typ, name, tbl, ddl string
			if err := rows.Scan(&typ, &name, &tbl, &ddl); err != nil {
				t.Fatal(err)
			}
			out = append(out, typ+"|"+name+"|"+tbl+"|"+ddl)
		}
		return out
	}
	beforeSchema := snapshotSchema(st1.ReadDB())
	_ = st1.Close()

	// Inject a foreign key violation inside the v7 migration transaction right before PRAGMA foreign_key_check & COMMIT
	restore := store.SetV7MigrationBeforeCommitHookForTest(func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO attempts (
				attempt_id, task_id, project_id, attempt_no, machine_id, adapter,
				trigger_kind, state, lease_token, lease_expires_at, started_at
			) VALUES ('att_orphan_fk', 'tsk_missing', 'proj_missing', 1, 'mach_missing', 'agy', 'initial', 'leased', 'lt', 1, 1)
		`)
		return err
	})
	defer restore()

	if stBad, openErr := store.Open(ctx, dbPath); openErr == nil {
		_ = stBad.Close()
		t.Fatal("expected store.Open to fail when v7 migration introduces FK violation")
	}
	restore()

	// Verify pre-v7 backup was created AND failed migration completely rolled back (identical sqlite_master snapshot!)
	if _, err := os.Stat(dbPath + ".pre-v7.bak"); err != nil {
		t.Fatalf("expected .pre-v7.bak backup to be created before migration: %v", err)
	}
	rawDB, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer rawDB.Close()
	afterSchema := snapshotSchema(rawDB)
	if len(beforeSchema) != len(afterSchema) {
		t.Fatalf("sqlite_master object count changed after rolled-back v7 migration: before=%d after=%d\nbefore=%v\nafter=%v", len(beforeSchema), len(afterSchema), beforeSchema, afterSchema)
	}
	for i := range beforeSchema {
		if beforeSchema[i] != afterSchema[i] {
			t.Fatalf("sqlite_master diverged at %d after rolled-back v7 migration:\nbefore: %s\nafter:  %s", i, beforeSchema[i], afterSchema[i])
		}
	}
	var hasV7 int
	if err := rawDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 7`).Scan(&hasV7); err != nil {
		t.Fatal(err)
	}
	if hasV7 != 0 {
		t.Fatalf("failed v7 migration must roll back schema_migrations version 7, got count=%d", hasV7)
	}
	var attDDL string
	if err := rawDB.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='attempts'`).Scan(&attDDL); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(attDDL, "'child_result'") {
		t.Fatalf("failed v7 migration must roll back attempts table rebuild, got DDL: %s", attDDL)
	}
}

func TestSubsession_CreateTaskIdempotencyCrossParentConflict(t *testing.T) {
	ctx := context.Background()
	st := setupSubsessionStore(t)

	p1, err := st.CreateTask(ctx, store.CreateTaskParams{ProjectID: "demo-app", Title: "P1", Goal: "p1", MachineID: "hub-node"})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := st.CreateTask(ctx, store.CreateTaskParams{ProjectID: "demo-app", Title: "P2", Goal: "p2", MachineID: "hub-node"})
	if err != nil {
		t.Fatal(err)
	}

	c1, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:      "demo-app",
		ParentTaskID:   p1.Task.TaskID,
		Title:          "Child of P1",
		Goal:           "secret goal",
		IdempotencyKey: "shared-key-1",
		MachineID:      "hub-node",
		CreateAttempt:  true,
	})
	if err != nil || !c1.Created {
		t.Fatalf("first create failed: created=%v err=%v", c1.Created, err)
	}

	// Replay under same project + same parent succeeds idempotently
	replay, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:      "demo-app",
		ParentTaskID:   p1.Task.TaskID,
		Title:          "Child of P1",
		Goal:           "secret goal",
		IdempotencyKey: "shared-key-1",
		MachineID:      "hub-node",
	})
	if err != nil || replay.Created || replay.Task.TaskID != c1.Task.TaskID {
		t.Fatalf("same-parent replay should return existing child, got created=%v id=%s err=%v", replay.Created, replay.Task.TaskID, err)
	}

	// Replay under same project but DIFFERENT parent MUST return ErrConflict without leaking P1's child!
	if leaked, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:      "demo-app",
		ParentTaskID:   p2.Task.TaskID,
		Title:          "Hijack attempt",
		Goal:           "different parent",
		IdempotencyKey: "shared-key-1",
		MachineID:      "hub-node",
	}); !errors.Is(err, store.ErrConflict) || leaked.Task.TaskID != "" {
		t.Fatalf("cross-parent idempotency key MUST return ErrConflict with empty task, got id=%q err=%v", leaked.Task.TaskID, err)
	}
}

func TestSubsession_RunnerParentGuardInsideWriteTransactions(t *testing.T) {
	ctx := context.Background()
	st := setupSubsessionStore(t)

	parent, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		Title:         "Parent",
		Goal:          "parent",
		MachineID:     "hub-node",
		CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	otherParent, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		Title:         "Other Parent",
		Goal:          "other",
		MachineID:     "hub-node",
		CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "demo-app",
		ParentTaskID:  parent.Task.TaskID,
		Title:         "Child",
		Goal:          "child",
		MachineID:     "hub-node",
		CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Cross-tree live runner guard on otherParent MUST be rejected inside SubmitFollowupAtomic and CancelTaskActiveAttemptWithGuard!
	otherGuard := &store.RunnerParentControlGuard{
		MachineID:       "hub-node",
		ParentTaskID:    otherParent.Task.TaskID,
		ParentAttemptID: otherParent.Attempt.AttemptID,
	}
	if _, err := st.SubmitFollowupAtomic(ctx, store.SubmitFollowupParams{
		TaskID:            child.Task.TaskID,
		Body:              "cross tree send",
		Mode:              protocol.FollowupModeQueue,
		MachineID:         "hub-node",
		Adapter:           protocol.AdapterAgy,
		RunnerParentGuard: otherGuard,
	}); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("expected ErrForbidden for cross-tree send guard, got: %v", err)
	}
	if _, err := st.CancelTaskActiveAttemptWithGuard(ctx, child.Task.TaskID, "agent:hub-node", "hijack", false, nil, otherGuard); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("expected ErrForbidden for cross-tree cancel guard, got: %v", err)
	}

	// Expired lease (state still 'running', scanner NOT yet run) MUST be rejected inside ALL 3 write transactions!
	now := time.Now().UnixMilli()
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE attempts SET state = 'running', lease_expires_at = ? WHERE attempt_id = ?`, now-5000, parent.Attempt.AttemptID); err != nil {
		t.Fatal(err)
	}
	expiredGuard := &store.RunnerParentControlGuard{
		MachineID:       "hub-node",
		ParentTaskID:    parent.Task.TaskID,
		ParentAttemptID: parent.Attempt.AttemptID,
	}
	if _, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:         "demo-app",
		ParentTaskID:      parent.Task.TaskID,
		Title:             "Expired Lease Spawn",
		Goal:              "must fail",
		MachineID:         "hub-node",
		CreateAttempt:     true,
		RunnerParentGuard: expiredGuard,
	}); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("expected ErrForbidden for expired parent lease in CreateTask, got: %v", err)
	}
	if _, err := st.SubmitFollowupAtomic(ctx, store.SubmitFollowupParams{
		TaskID:            child.Task.TaskID,
		Body:              "expired lease send",
		Mode:              protocol.FollowupModeQueue,
		MachineID:         "hub-node",
		Adapter:           protocol.AdapterAgy,
		RunnerParentGuard: expiredGuard,
	}); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("expected ErrForbidden for expired parent lease in SubmitFollowupAtomic, got: %v", err)
	}
	if _, err := st.CancelTaskActiveAttemptWithGuard(ctx, child.Task.TaskID, "agent:hub-node", "expired lease cancel", false, nil, expiredGuard); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("expected ErrForbidden for expired parent lease in CancelTaskActiveAttemptWithGuard, got: %v", err)
	}

	// Now finish parent's attempt -> stale parentGuard MUST be rejected inside ALL 3 write transactions with ZERO mutations!
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE attempts SET state = 'succeeded', finished_at = ? WHERE attempt_id = ?`, now, parent.Attempt.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE tasks SET state = 'in_review', active_attempt_id = NULL WHERE task_id = ?`, parent.Task.TaskID); err != nil {
		t.Fatal(err)
	}
	staleGuard := &store.RunnerParentControlGuard{
		MachineID:       "hub-node",
		ParentTaskID:    parent.Task.TaskID,
		ParentAttemptID: parent.Attempt.AttemptID,
	}

	if _, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:         "demo-app",
		ParentTaskID:      parent.Task.TaskID,
		Title:             "Stale Spawn",
		Goal:              "must fail",
		MachineID:         "hub-node",
		CreateAttempt:     true,
		RunnerParentGuard: staleGuard,
	}); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("expected ErrForbidden for stale parent guard in CreateTask, got: %v", err)
	}
	if _, err := st.SubmitFollowupAtomic(ctx, store.SubmitFollowupParams{
		TaskID:            child.Task.TaskID,
		Body:              "stale send",
		Mode:              protocol.FollowupModeQueue,
		MachineID:         "hub-node",
		Adapter:           protocol.AdapterAgy,
		RunnerParentGuard: staleGuard,
	}); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("expected ErrForbidden for stale parent guard in SubmitFollowupAtomic, got: %v", err)
	}
	if _, err := st.CancelTaskActiveAttemptWithGuard(ctx, child.Task.TaskID, "agent:hub-node", "stale cancel", false, nil, staleGuard); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("expected ErrForbidden for stale parent guard in CancelTaskActiveAttemptWithGuard, got: %v", err)
	}

	// Verify ZERO mutations occurred on child or parent children list
	kids, err := st.ListChildTasks(ctx, parent.Task.TaskID, true)
	if err != nil || len(kids) != 1 {
		t.Fatalf("expected still exactly 1 child after rejected spawn, got %d (err=%v)", len(kids), err)
	}
	queued, err := st.ListPendingInputs(ctx, child.Task.TaskID)
	if err != nil || len(queued) != 0 {
		t.Fatalf("expected 0 pending inputs on child after rejected send, got %d (err=%v)", len(queued), err)
	}
	attRec, err := st.GetAttemptRecord(ctx, child.Attempt.AttemptID)
	if err != nil || attRec.State != protocol.AttemptLeased || attRec.CancelRequested {
		t.Fatalf("expected child attempt untouched after rejected cancel, got state=%s cancelReq=%v err=%v", attRec.State, attRec.CancelRequested, err)
	}
}

func TestSubsession_V7NotificationUpgradePreservesHubEventOrderOverClockSkew(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "v7-notif-upgrade.sqlite")

	st1, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st1.SyncRegistry(ctx, tmp,
		protocol.ProjectsConfig{
			SchemaVersion: 1,
			Projects: map[string]protocol.ProjectConfig{
				"demo": {DisplayName: "Demo", RepoIdentity: "demo", DefaultBranch: "main", DefaultCapability: "primary-dev", DefaultAdapter: protocol.AdapterAgy},
			},
		},
		protocol.MachinesConfig{
			SchemaVersion: 1,
			Machines: map[string]protocol.MachineConfig{
				"hub-node": {DisplayName: "hub-node", Transport: protocol.TransportLocalWS, Capabilities: []string{"primary-dev"}},
			},
		},
	); err != nil {
		t.Fatal(err)
	}

	parent, err := st1.CreateTask(ctx, store.CreateTaskParams{ProjectID: "demo", Title: "Parent", Goal: "p", MachineID: "hub-node"})
	if err != nil {
		t.Fatal(err)
	}
	cFirst, err := st1.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: "demo", ParentTaskID: parent.Task.TaskID, Title: "First", Goal: "1", MachineID: "hub-node", CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	cSecond, err := st1.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: "demo", ParentTaskID: parent.Task.TaskID, Title: "Second", Goal: "2", MachineID: "hub-node", CreateAttempt: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Commit Hub terminal events in order: cFirst (lower events.seq) then cSecond (higher events.seq),
	// while Runner item timestamps are skewed in REVERSE (cFirst has a much later created_at than cSecond)!
	now := time.Now().UnixMilli()
	if _, err := st1.IngestOutboxBatch(ctx, "hub-node", "run-1", protocol.AttemptOutboxBatchFrame{
		AttemptID:  cFirst.Attempt.AttemptID,
		LeaseToken: cFirst.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{{
			OutboxSeq: 1, EventID: "evt_first_fin", FrameType: protocol.OutboxFrameState,
			Payload: map[string]any{"state": string(protocol.AttemptSucceeded), "stopReason": "end_turn"},
			TS:      now + 90000,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st1.IngestOutboxBatch(ctx, "hub-node", "run-1", protocol.AttemptOutboxBatchFrame{
		AttemptID:  cSecond.Attempt.AttemptID,
		LeaseToken: cSecond.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{{
			OutboxSeq: 1, EventID: "evt_second_fin", FrameType: protocol.OutboxFrameState,
			Payload: map[string]any{"state": string(protocol.AttemptSucceeded), "stopReason": "end_turn"},
			TS:      now - 90000,
		}},
	}); err != nil {
		t.Fatal(err)
	}

	// Simulate an older v7 DB whose child_parent_notifications lacked notif_seq and stored reverse-skewed created_at
	if _, err := st1.WriteDB().ExecContext(ctx, `
		DROP TABLE child_parent_notifications;
		CREATE TABLE child_parent_notifications (
			child_attempt_id TEXT PRIMARY KEY REFERENCES attempts(attempt_id),
			child_task_id    TEXT NOT NULL REFERENCES tasks(task_id),
			parent_task_id   TEXT NOT NULL REFERENCES tasks(task_id),
			child_state      TEXT NOT NULL,
			status           TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'delivered', 'skipped')),
			last_error       TEXT,
			created_at       INTEGER NOT NULL,
			delivered_at     INTEGER
		);
	`); err != nil {
		t.Fatalf("recreate old v7 child_parent_notifications: %v", err)
	}
	if _, err := st1.WriteDB().ExecContext(ctx, `
		INSERT INTO child_parent_notifications (child_attempt_id, child_task_id, parent_task_id, child_state, status, created_at)
		VALUES (?, ?, ?, 'succeeded', 'pending', ?)
	`, cFirst.Attempt.AttemptID, cFirst.Task.TaskID, parent.Task.TaskID, now+90000); err != nil {
		t.Fatalf("seed first old notification: %v", err)
	}
	if _, err := st1.WriteDB().ExecContext(ctx, `
		INSERT INTO child_parent_notifications (child_attempt_id, child_task_id, parent_task_id, child_state, status, created_at)
		VALUES (?, ?, ?, 'succeeded', 'pending', ?)
	`, cSecond.Attempt.AttemptID, cSecond.Task.TaskID, parent.Task.TaskID, now-90000); err != nil {
		t.Fatalf("seed second old notification: %v", err)
	}
	_ = st1.Close()

	// Re-open -> upgrades child_parent_notifications ordered by Hub events.seq (NOT skewed created_at)!
	st2, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen to upgrade notifications table: %v", err)
	}
	defer st2.Close()

	var seqFirst, seqSecond int64
	if err := st2.ReadDB().QueryRowContext(ctx, `SELECT notif_seq FROM child_parent_notifications WHERE child_attempt_id = ?`, cFirst.Attempt.AttemptID).Scan(&seqFirst); err != nil {
		t.Fatal(err)
	}
	if err := st2.ReadDB().QueryRowContext(ctx, `SELECT notif_seq FROM child_parent_notifications WHERE child_attempt_id = ?`, cSecond.Attempt.AttemptID).Scan(&seqSecond); err != nil {
		t.Fatal(err)
	}
	if !(seqFirst < seqSecond) {
		t.Fatalf("expected Hub events.seq order preserved despite clock-skewed created_at: first=%d second=%d", seqFirst, seqSecond)
	}

	// Subcase: when ANY row lacks an attempt.finished event, migration MUST fall back uniformly
	// to old.rowid ASC instead of mixing events.seq (e.g. 100) with rowid (e.g. 2).
	t.Run("missing_finished_event_falls_back_uniformly_to_rowid", func(t *testing.T) {
		dbPath2 := filepath.Join(t.TempDir(), "v7-notif-mixed.sqlite")
		stM1, err := store.Open(ctx, dbPath2)
		if err != nil {
			t.Fatal(err)
		}
		if err := stM1.SyncRegistry(ctx, t.TempDir(),
			protocol.ProjectsConfig{
				SchemaVersion: 1,
				Projects: map[string]protocol.ProjectConfig{
					"demo": {DisplayName: "Demo", RepoIdentity: "demo", DefaultBranch: "main", DefaultCapability: "primary-dev", DefaultAdapter: protocol.AdapterAgy},
				},
			},
			protocol.MachinesConfig{
				SchemaVersion: 1,
				Machines: map[string]protocol.MachineConfig{
					"hub-node": {DisplayName: "hub-node", Transport: protocol.TransportLocalWS, Capabilities: []string{"primary-dev"}},
				},
			},
		); err != nil {
			t.Fatal(err)
		}
		p, err := stM1.CreateTask(ctx, store.CreateTaskParams{ProjectID: "demo", Title: "P", Goal: "p", MachineID: "hub-node"})
		if err != nil {
			t.Fatal(err)
		}
		first, err := stM1.CreateTask(ctx, store.CreateTaskParams{
			ProjectID: "demo", ParentTaskID: p.Task.TaskID, Title: "First", Goal: "1", MachineID: "hub-node", CreateAttempt: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		second, err := stM1.CreateTask(ctx, store.CreateTaskParams{
			ProjectID: "demo", ParentTaskID: p.Task.TaskID, Title: "Second", Goal: "2", MachineID: "hub-node", CreateAttempt: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		// Give ONLY `first` an attempt.finished event with high seq=100; `second` has NO attempt.finished event!
		if _, err := stM1.WriteDB().ExecContext(ctx, `
			INSERT INTO events (
				seq, event_id, stream_id, project_id, task_id, attempt_id, kind, visibility, actor_kind, actor_id, payload_json, created_at
			) VALUES (100, 'evt_high_seq_first', ?, 'demo', ?, ?, 'attempt.finished', 'timeline', 'runner', 'hub-node', '{"state":"succeeded"}', ?)
		`, "task:"+first.Task.TaskID, first.Task.TaskID, first.Attempt.AttemptID, now); err != nil {
			t.Fatalf("seed high-seq finished event: %v", err)
		}
		// Recreate old pre-notif_seq table and insert first (rowid=1) then second (rowid=2)
		if _, err := stM1.WriteDB().ExecContext(ctx, `
			DROP TABLE child_parent_notifications;
			CREATE TABLE child_parent_notifications (
				child_attempt_id TEXT PRIMARY KEY REFERENCES attempts(attempt_id),
				child_task_id    TEXT NOT NULL REFERENCES tasks(task_id),
				parent_task_id   TEXT NOT NULL REFERENCES tasks(task_id),
				child_state      TEXT NOT NULL,
				status           TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'delivered', 'skipped')),
				last_error       TEXT,
				created_at       INTEGER NOT NULL,
				delivered_at     INTEGER
			);
		`); err != nil {
			t.Fatal(err)
		}
		if _, err := stM1.WriteDB().ExecContext(ctx, `
			INSERT INTO child_parent_notifications (child_attempt_id, child_task_id, parent_task_id, child_state, status, created_at)
			VALUES (?, ?, ?, 'succeeded', 'pending', ?)
		`, first.Attempt.AttemptID, first.Task.TaskID, p.Task.TaskID, now+5000); err != nil {
			t.Fatal(err)
		}
		if _, err := stM1.WriteDB().ExecContext(ctx, `
			INSERT INTO child_parent_notifications (child_attempt_id, child_task_id, parent_task_id, child_state, status, created_at)
			VALUES (?, ?, ?, 'cancelled', 'pending', ?)
		`, second.Attempt.AttemptID, second.Task.TaskID, p.Task.TaskID, now); err != nil {
			t.Fatal(err)
		}
		_ = stM1.Close()

		stM2, err := store.Open(ctx, dbPath2)
		if err != nil {
			t.Fatalf("reopen mixed notifications db: %v", err)
		}
		defer stM2.Close()

		var s1, s2 int64
		if err := stM2.ReadDB().QueryRowContext(ctx, `SELECT notif_seq FROM child_parent_notifications WHERE child_attempt_id = ?`, first.Attempt.AttemptID).Scan(&s1); err != nil {
			t.Fatal(err)
		}
		if err := stM2.ReadDB().QueryRowContext(ctx, `SELECT notif_seq FROM child_parent_notifications WHERE child_attempt_id = ?`, second.Attempt.AttemptID).Scan(&s2); err != nil {
			t.Fatal(err)
		}
		if !(s1 < s2) {
			t.Fatalf("expected uniform rowid fallback preserving first(%d) < second(%d) instead of mixing events.seq=100 with rowid=2", s1, s2)
		}
	})
}
