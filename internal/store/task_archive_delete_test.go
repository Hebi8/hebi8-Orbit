package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

func TestTaskArchiveUnarchiveAndDeleteCascade(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	st, err := Open(ctx, filepath.Join(tmpDir, "orbit.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// Seed machine + project
	err = st.SyncRegistry(ctx, tmpDir, protocol.ProjectsConfig{
		SchemaVersion: 1,
		Projects: map[string]protocol.ProjectConfig{
			"demo-proj": {
				DisplayName:       "Demo Project",
				RepoIdentity:      "git:demo",
				DefaultBranch:     "main",
				DefaultCapability: "primary-dev",
				DefaultAdapter:    protocol.AdapterAgy,
				IDEScheme:         protocol.IDECursor,
				Anchors: map[string]protocol.ProjectAnchorConfig{
					"primary-dev": {Machine: "dev-node", RepoPath: "/tmp/demo"},
				},
			},
		},
	}, protocol.MachinesConfig{
		SchemaVersion: 1,
		Machines: map[string]protocol.MachineConfig{
			"dev-node": {
				DisplayName:  "Dev Node",
				Transport:    protocol.TransportLocalWS,
				Capabilities: []string{"primary-dev"},
			},
		},
	})
	if err != nil {
		t.Fatalf("sync registry: %v", err)
	}

	// Create parent task with an active attempt
	created, err := st.CreateTask(ctx, CreateTaskParams{
		ProjectID:          "demo-proj",
		Title:              "Parent Session",
		Goal:               "Implement feature",
		RequiredCapability: "primary-dev",
		Adapter:            protocol.AdapterAgy,
		MachineID:          "dev-node",
		Sources: []protocol.CreateTaskSourceInput{
			{Kind: protocol.TaskSourceFile, Label: "README.md", URIOrRef: "README.md"},
		},
		CreateAttempt: true,
	})
	if err != nil {
		t.Fatalf("create parent task: %v", err)
	}
	parentID := created.Task.TaskID
	parentAttID := created.Attempt.AttemptID

	// Active task cannot be archived or deleted
	if _, _, err := st.ArchiveTask(ctx, parentID, "tester"); !errors.Is(err, ErrTaskActive) {
		t.Fatalf("expected ErrTaskActive archiving active task, got %v", err)
	}
	if _, _, _, err := st.DeleteTask(ctx, parentID, "tester"); !errors.Is(err, ErrTaskActive) {
		t.Fatalf("expected ErrTaskActive deleting active task, got %v", err)
	}

	// Create a child task (inactive) and populate ALL 12 dependent tables
	childRes, err := st.CreateTask(ctx, CreateTaskParams{
		ProjectID:          "demo-proj",
		ParentTaskID:       parentID,
		Title:              "Child Worker",
		Goal:               "Subtask work",
		RequiredCapability: "primary-dev",
		Adapter:            protocol.AdapterAgy,
		MachineID:          "dev-node",
		CreateAttempt:      false,
	})
	if err != nil {
		t.Fatalf("create child task: %v", err)
	}
	childID := childRes.Task.TaskID

	// Cancel parent's active attempt so task becomes idle
	if _, err := st.CancelTaskActiveAttempt(ctx, parentID, "tester", "done", true); err != nil {
		t.Fatalf("cancel parent attempt: %v", err)
	}

	// Create an unrelated task that references childID via board_members.worker_task_id
	otherRes, err := st.CreateTask(ctx, CreateTaskParams{
		ProjectID:          "demo-proj",
		Title:              "Other Task",
		Goal:               "Keep alive",
		RequiredCapability: "primary-dev",
		Adapter:            protocol.AdapterAgy,
		MachineID:          "dev-node",
		CreateAttempt:      false,
	})
	if err != nil {
		t.Fatalf("create other task: %v", err)
	}
	otherID := otherRes.Task.TaskID

	frozenDir := filepath.Join(tmpDir, "deliveries", "dlv_test_cascade")
	if err := os.MkdirAll(frozenDir, 0o755); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UnixMilli()
	err = st.WithWriteTx(ctx, func(tx *sql.Tx) error {
		// delivery
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO deliveries (
				delivery_id, project_id, task_id, attempt_id, source_machine_id,
				repo_identity, base_sha, head_sha, git_ref, kind, summary,
				manifest_json, frozen_snapshot_dir, ryugi_sync_state, created_at
			) VALUES ('dlv_test_cascade', 'demo-proj', ?, ?, 'dev-node', 'git:demo', 'aaa', 'bbb', 'refs/orbit/deliveries/dlv_test_cascade', 'mixed', 'sum', '{}', ?, 'ready', ?)
		`, parentID, parentAttID, frozenDir, now); err != nil {
			return err
		}
		// ryugi_bindings
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ryugi_bindings (
				binding_id, delivery_id, project_id, task_id, frozen_project_dir,
				artifact_locator, derived_artifact_id, open_url, active, created_at, updated_at
			) VALUES ('ryg_1', 'dlv_test_cascade', 'demo-proj', ?, ?, 'html/demo', 'DerivedId123456789012', 'http://127.0.0.1:27329/html/demo?key=k1', 1, ?, ?)
		`, parentID, frozenDir, now, now); err != nil {
			return err
		}
		// annotations
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO annotations (
				annotation_id, project_id, task_id, delivery_id, source, anchor_json, body, status, created_at
			) VALUES ('ann_1', 'demo-proj', ?, 'dlv_test_cascade', 'diff_drawer', '{}', 'fix this', 'open', ?)
		`, parentID, now); err != nil {
			return err
		}
		// approvals
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO approvals (
				approval_id, project_id, task_id, attempt_id, kind, action_name, target_env,
				input_sha, payload_json, payload_sha256, options_json, requested_by, state, expires_at, created_at
			) VALUES ('apr_1', 'demo-proj', ?, ?, 'acp_tool_permission', 'shell', 'local', 'aaa', '{}', 'bbb', '[]', 'agent', 'expired', ?, ?)
		`, parentID, parentAttID, now, now); err != nil {
			return err
		}
		// task_pending_inputs
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO task_pending_inputs (
				input_id, task_id, project_id, kind, payload_json, state, created_at, body, updated_at
			) VALUES ('inp_1', ?, 'demo-proj', 'followup', '{}', 'queued', ?, 'follow', ?)
		`, parentID, now, now); err != nil {
			return err
		}
		// inbox_items
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO inbox_items (
				inbox_id, project_id, task_id, category, ref_id, title, summary, priority, state, created_at, updated_at
			) VALUES ('inb_1', 'demo-proj', ?, 'delivery_review', 'dlv_test_cascade', 'Review', 'Ready', 2, 'open', ?, ?)
		`, parentID, now, now); err != nil {
			return err
		}
		// board_messages
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO board_messages (
				message_id, task_id, project_id, author_name, body, status, created_at
			) VALUES ('bm_1', ?, 'demo-proj', 'coordinator', 'hello', 'posted', ?)
		`, parentID, now); err != nil {
			return err
		}
		// board_members on parent AND cross-task reference to childID on otherID
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO board_members (
				task_id, project_id, member_name, state, worker_task_id, updated_at
			) VALUES (?, 'demo-proj', 'worker-1', 'waiting', ?, ?),
			         (?, 'demo-proj', 'ref-worker', 'waiting', ?, ?)
		`, parentID, childID, now, otherID, childID, now); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed 12 dependent tables: %v", err)
	}

	// 1. ArchiveTask: archives parent + child, dismisses open inbox, deactivates ryugi_bindings
	archivedTask, evArch, err := st.ArchiveTask(ctx, parentID, "tester")
	if err != nil {
		t.Fatalf("archive task: %v", err)
	}
	if archivedTask.ArchivedAt == nil || *archivedTask.ArchivedAt <= 0 {
		t.Fatalf("expected non-nil ArchivedAt")
	}
	if evArch == nil || evArch.Kind != protocol.EventTaskArchived {
		t.Fatalf("expected task.archived event, got %+v", evArch)
	}

	// Verify filters: active vs only vs all
	activeTasks, err := st.ListProjectTasks(ctx, "demo-proj", "active")
	if err != nil {
		t.Fatal(err)
	}
	if len(activeTasks) != 1 || activeTasks[0].TaskID != otherID {
		t.Fatalf("expected only otherID active, got %+v", activeTasks)
	}
	onlyArchived, err := st.ListProjectTasks(ctx, "demo-proj", "only")
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyArchived) != 2 {
		t.Fatalf("expected parent + child in archived=only, got %d", len(onlyArchived))
	}
	recentArchived, err := st.ListRecentTasksFiltered(ctx, 50, "Parent", "", "demo-proj", "only")
	if err != nil || len(recentArchived) != 1 || recentArchived[0].TaskID != parentID {
		t.Fatalf("expected ListRecentTasksFiltered archived=only to find parentID, got %+v err=%v", recentArchived, err)
	}

	// Verify inbox dismissed and ryugi_bindings inactive
	var inboxState string
	var rygActive int
	if err := st.ReadDB().QueryRowContext(ctx, `SELECT state FROM inbox_items WHERE inbox_id = 'inb_1'`).Scan(&inboxState); err != nil || inboxState != "dismissed" {
		t.Fatalf("expected inbox dismissed, got %q err=%v", inboxState, err)
	}
	if err := st.ReadDB().QueryRowContext(ctx, `SELECT active FROM ryugi_bindings WHERE binding_id = 'ryg_1'`).Scan(&rygActive); err != nil || rygActive != 0 {
		t.Fatalf("expected ryugi_binding inactive, got %d err=%v", rygActive, err)
	}

	// 2. UnarchiveTask: restores parent + child and reactivates ryugi_binding because frozenDir exists
	restoredTask, evUnarch, err := st.UnarchiveTask(ctx, parentID, "tester")
	if err != nil {
		t.Fatalf("unarchive task: %v", err)
	}
	if restoredTask.ArchivedAt != nil {
		t.Fatalf("expected nil ArchivedAt after unarchive")
	}
	if evUnarch == nil || evUnarch.Kind != protocol.EventTaskUnarchived {
		t.Fatalf("expected task.unarchived event, got %+v", evUnarch)
	}
	if err := st.ReadDB().QueryRowContext(ctx, `SELECT active FROM ryugi_bindings WHERE binding_id = 'ryg_1'`).Scan(&rygActive); err != nil || rygActive != 1 {
		t.Fatalf("expected ryugi_binding reactivated when frozen dir exists, got %d err=%v", rygActive, err)
	}

	// 3. DeleteTask: cascades all 12 tables cleanly under foreign_keys=ON and clears cross-task worker_task_id
	projID, deliveryIDs, evDel, err := st.DeleteTask(ctx, parentID, "tester")
	if err != nil {
		t.Fatalf("delete task cascade failed: %v", err)
	}
	if projID != "demo-proj" {
		t.Fatalf("expected projectId demo-proj, got %q", projID)
	}
	if len(deliveryIDs) != 1 || deliveryIDs[0] != "dlv_test_cascade" {
		t.Fatalf("expected deliveryIDs=[dlv_test_cascade], got %+v", deliveryIDs)
	}
	if evDel == nil || evDel.Kind != protocol.EventTaskDeleted {
		t.Fatalf("expected task.deleted event, got %+v", evDel)
	}

	// Verify zero rows left for parentID/childID and otherID's board_member worker_task_id was nulled
	var remaining int
	_ = st.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE task_id IN (?, ?)`, parentID, childID).Scan(&remaining)
	if remaining != 0 {
		t.Fatalf("expected 0 remaining tasks for deleted tree, got %d", remaining)
	}
	var otherWorker sql.NullString
	if err := st.ReadDB().QueryRowContext(ctx, `SELECT worker_task_id FROM board_members WHERE task_id = ? AND member_name = 'ref-worker'`, otherID).Scan(&otherWorker); err != nil || otherWorker.Valid {
		t.Fatalf("expected cross-task worker_task_id cleared, got valid=%v err=%v", otherWorker.Valid, err)
	}

	// 4. Protect Hebi8 main conversation (hebi8_main_task_id) even when another legacy _pa task exists
	mainHebi8, _, err := st.EnsureHebi8MainTask(ctx, protocol.AdapterAgy)
	if err != nil {
		t.Fatalf("ensure hebi8 main task: %v", err)
	}
	legacyPA, err := st.CreateTask(ctx, CreateTaskParams{
		ProjectID:          protocol.PAProjectID,
		Title:              "Legacy PA thread",
		Goal:               "old chat",
		RequiredCapability: "primary-dev",
		Adapter:            protocol.AdapterAgy,
		MachineID:          "dev-node",
		CreateAttempt:      false,
	})
	if err != nil {
		t.Fatalf("create legacy pa task: %v", err)
	}
	if _, _, err := st.ArchiveTask(ctx, mainHebi8.TaskID, "tester"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden archiving Hebi8 main task even with legacy _pa task, got %v", err)
	}
	if _, _, _, err := st.DeleteTask(ctx, mainHebi8.TaskID, "tester"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden deleting Hebi8 main task even with legacy _pa task, got %v", err)
	}
	// Legacy non-main _pa task can still be archived and deleted
	if _, _, err := st.ArchiveTask(ctx, legacyPA.Task.TaskID, "tester"); err != nil {
		t.Fatalf("expected legacy _pa task archive to succeed, got %v", err)
	}
	if _, _, _, err := st.DeleteTask(ctx, legacyPA.Task.TaskID, "tester"); err != nil {
		t.Fatalf("expected legacy _pa task delete to succeed, got %v", err)
	}
}

func TestStoreDeleteTaskRefCountsAndArchivedBlocks(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	st, err := Open(ctx, filepath.Join(tmpDir, "orbit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.SyncRegistry(ctx, tmpDir, protocol.ProjectsConfig{
		SchemaVersion: 1,
		Projects: map[string]protocol.ProjectConfig{
			"demo-proj": {
				DisplayName:       "Demo",
				RepoIdentity:      "git:demo",
				DefaultBranch:     "main",
				DefaultCapability: "primary-dev",
				DefaultAdapter:    protocol.AdapterAgy,
				IDEScheme:         protocol.IDECursor,
				Anchors: map[string]protocol.ProjectAnchorConfig{
					"primary-dev": {Machine: "dev-node", RepoPath: "/tmp/demo"},
				},
			},
		},
	}, protocol.MachinesConfig{
		SchemaVersion: 1,
		Machines: map[string]protocol.MachineConfig{
			"dev-node": {
				DisplayName:  "Dev Node",
				Transport:    protocol.TransportLocalWS,
				Capabilities: []string{"primary-dev"},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	t1, err := st.CreateTask(ctx, CreateTaskParams{
		ProjectID:          "demo-proj",
		Title:              "Task 1",
		Goal:               "blobs",
		RequiredCapability: "primary-dev",
		Adapter:            protocol.AdapterAgy,
		MachineID:          "dev-node",
		CreateAttempt:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t2, err := st.CreateTask(ctx, CreateTaskParams{
		ProjectID:          "demo-proj",
		Title:              "Task 2",
		Goal:               "archived guard",
		RequiredCapability: "primary-dev",
		Adapter:            protocol.AdapterAgy,
		MachineID:          "dev-node",
		CreateAttempt:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE attempts SET state = 'succeeded' WHERE attempt_id IN (?, ?)`, t1.Attempt.AttemptID, t2.Attempt.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE tasks SET state = 'done', active_attempt_id = NULL WHERE task_id IN (?, ?)`, t1.Task.TaskID, t2.Task.TaskID); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UnixMilli()
	// Insert shared blob (ref_count=2) and exclusive blob (ref_count=1)
	if _, err := st.WriteDB().ExecContext(ctx, `
		INSERT INTO delivery_blobs (sha256, size_bytes, storage_path, ref_count, created_at)
		VALUES ('sha_shared', 10, 'blobs/sha_shared', 2, ?), ('sha_only', 12, 'blobs/sha_only', 1, ?)
	`, now, now); err != nil {
		t.Fatal(err)
	}
	manifest1 := `{"filesChanged":1,"blobs":[{"path":"a.md","sha256":"sha_shared","size":10},{"path":"dup.md","sha256":"sha_shared","size":10},{"path":"b.md","sha256":"sha_only","size":12}]}`
	manifest2 := `{"filesChanged":1,"blobs":[{"path":"c.md","sha256":"sha_shared","size":10}]}`
	if _, err := st.WriteDB().ExecContext(ctx, `
		INSERT INTO deliveries (
			delivery_id, project_id, task_id, attempt_id, source_machine_id,
			repo_identity, base_sha, head_sha, git_ref, kind, summary, manifest_json, created_at
		) VALUES
			('dlv_blob_1', 'demo-proj', ?, ?, 'dev-node', 'git:demo', 'base1', 'head1', 'refs/orbit/deliveries/dlv_blob_1', 'mixed', 'D1', ?, ?),
			('dlv_blob_2', 'demo-proj', ?, ?, 'dev-node', 'git:demo', 'base2', 'head2', 'refs/orbit/deliveries/dlv_blob_2', 'mixed', 'D2', ?, ?)
	`, t1.Task.TaskID, t1.Attempt.AttemptID, manifest1, now, t2.Task.TaskID, t2.Attempt.AttemptID, manifest2, now); err != nil {
		t.Fatal(err)
	}

	// Delete task 1 -> sha_shared ref_count decrements once (2->1), sha_only decrements (1->0)
	if _, _, _, err := st.DeleteTask(ctx, t1.Task.TaskID, "tester"); err != nil {
		t.Fatalf("delete t1: %v", err)
	}
	var sharedRC, onlyRC int
	if err := st.ReadDB().QueryRowContext(ctx, `SELECT ref_count FROM delivery_blobs WHERE sha256 = 'sha_shared'`).Scan(&sharedRC); err != nil || sharedRC != 1 {
		t.Fatalf("expected sha_shared ref_count=1, got %d err=%v", sharedRC, err)
	}
	if err := st.ReadDB().QueryRowContext(ctx, `SELECT ref_count FROM delivery_blobs WHERE sha256 = 'sha_only'`).Scan(&onlyRC); err != nil || onlyRC != 0 {
		t.Fatalf("expected sha_only ref_count=0, got %d err=%v", onlyRC, err)
	}

	// Archive task 2 -> all execution entry points return ErrTaskArchived
	if _, _, err := st.ArchiveTask(ctx, t2.Task.TaskID, "tester"); err != nil {
		t.Fatalf("archive t2: %v", err)
	}
	if _, err := st.SubmitFollowupAtomic(ctx, SubmitFollowupParams{
		TaskID:    t2.Task.TaskID,
		Body:      "hello archived",
		Mode:      protocol.FollowupModeQueue,
		Source:    protocol.PendingSourceHuman,
		Adapter:   protocol.AdapterAgy,
		MachineID: "dev-node",
	}); !errors.Is(err, ErrTaskArchived) {
		t.Fatalf("expected ErrTaskArchived on SubmitFollowupAtomic, got %v", err)
	}
	if _, _, _, err := st.CreateNextAttempt(ctx, CreateNextAttemptParams{
		TaskID:      t2.Task.TaskID,
		TriggerKind: protocol.TriggerRetry,
		Adapter:     protocol.AdapterAgy,
		MachineID:   "dev-node",
	}); !errors.Is(err, ErrTaskArchived) {
		t.Fatalf("expected ErrTaskArchived on CreateNextAttempt, got %v", err)
	}
	if _, _, err := st.EnqueuePendingInput(ctx, t2.Task.TaskID, protocol.PendingInputFollowup, protocol.PendingSourceHuman, "queued", nil, "user"); !errors.Is(err, ErrTaskArchived) {
		t.Fatalf("expected ErrTaskArchived on EnqueuePendingInput, got %v", err)
	}
	if _, err := st.DispatchAnnotations(ctx, DispatchAnnotationsParams{
		TaskID:        t2.Task.TaskID,
		AnnotationIDs: []string{"ann_1"},
		Message:       "notes",
		Adapter:       protocol.AdapterAgy,
		MachineID:     "dev-node",
		ActorID:       "user",
	}); !errors.Is(err, ErrTaskArchived) {
		t.Fatalf("expected ErrTaskArchived on DispatchAnnotations, got %v", err)
	}
	if pi, att, _, err := st.ConsumeNextPendingInput(ctx, ConsumeNextPendingInputParams{
		TaskID:    t2.Task.TaskID,
		Adapter:   protocol.AdapterAgy,
		MachineID: "dev-node",
	}); err != nil || pi != nil || att != nil {
		t.Fatalf("expected ConsumeNextPendingInput on archived task to return (nil, nil, nil, nil), got (%+v, %+v, %v)", pi, att, err)
	}

	// P2-13: SaveRyugiBindings on archived task's delivery keeps active=0 and ListRyugiBindings(activeOnly=true) omits it
	frozenDir2 := filepath.Join(tmpDir, "deliveries", "dlv_blob_2")
	if _, _, err := st.SaveRyugiBindings(ctx, "dlv_blob_2", frozenDir2, protocol.RyugiSyncReady, []protocol.RyugiBindingView{
		{
			BindingID:        "ryg_arch_1",
			DeliveryID:       "dlv_blob_2",
			ArtifactLocator:  "html/demo",
			FrozenProjectDir: frozenDir2,
			OpenURL:          "http://127.0.0.1:27329/html/demo?key=abc",
			Active:           true,
		},
	}); err != nil {
		t.Fatalf("SaveRyugiBindings: %v", err)
	}
	var activeFlag int
	if err := st.ReadDB().QueryRowContext(ctx, `SELECT active FROM ryugi_bindings WHERE binding_id = 'ryg_arch_1'`).Scan(&activeFlag); err != nil || activeFlag != 0 {
		t.Fatalf("expected archived task binding active=0, got %d err=%v", activeFlag, err)
	}
	activeBindings, err := st.ListRyugiBindings(ctx, "", true)
	if err != nil {
		t.Fatalf("ListRyugiBindings: %v", err)
	}
	for _, b := range activeBindings {
		if b.BindingID == "ryg_arch_1" {
			t.Fatalf("expected archived task binding omitted from active ListRyugiBindings")
		}
	}

	// Item 4: CreateTask with archived ParentTaskID MUST fail with ErrTaskArchived
	if _, err := st.CreateTask(ctx, CreateTaskParams{
		ProjectID:          "demo-proj",
		ParentTaskID:       t2.Task.TaskID,
		Title:              "Subtask under archived parent",
		Goal:               "should be blocked",
		RequiredCapability: "primary-dev",
		Adapter:            protocol.AdapterAgy,
		MachineID:          "dev-node",
		CreateAttempt:      false,
	}); !errors.Is(err, ErrTaskArchived) {
		t.Fatalf("Item 4 REGRESSION: expected ErrTaskArchived creating subtask under archived parent, got %v", err)
	}

	// Item 9: ApplyRyugiSyncBatch on archived task's binding MUST return ErrTaskArchived and write 0 annotations
	allBindings, err := st.ListRyugiBindings(ctx, "dlv_blob_2", false)
	if err != nil || len(allBindings) != 1 {
		t.Fatalf("expected 1 binding for dlv_blob_2, got %d err=%v", len(allBindings), err)
	}
	if _, _, err := st.ApplyRyugiSyncBatch(ctx, allBindings[0], []IncomingRyugiAnnotation{
		{
			ExternalKey: "ryg:ext:archived-1",
			Anchor:      protocol.AnnotationAnchor{Artifact: "html/demo"},
			Body:        "late comment on archived task",
		},
	}, nil, "rcpt_t1", ""); !errors.Is(err, ErrTaskArchived) {
		t.Fatalf("Item 9 REGRESSION: expected ErrTaskArchived on ApplyRyugiSyncBatch for archived task, got %v", err)
	}
	var annCount int
	if err := st.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM annotations WHERE task_id = ?`, t2.Task.TaskID).Scan(&annCount); err != nil || annCount != 0 {
		t.Fatalf("expected 0 annotations written for archived task, got %d err=%v", annCount, err)
	}

	// Item 3: CancelActiveAttemptIfArchivable atomically rejects active Hebi8 main task without cancelling its attempt
	mainHebi8, _, err := st.EnsureHebi8MainTask(ctx, protocol.AdapterAgy)
	if err != nil {
		t.Fatal(err)
	}
	_, mainAtt, _, err := st.CreateNextAttempt(ctx, CreateNextAttemptParams{
		TaskID:      mainHebi8.TaskID,
		TriggerKind: protocol.TriggerFollowup,
		Adapter:     protocol.AdapterAgy,
		MachineID:   "dev-node",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CancelActiveAttemptIfArchivable(ctx, mainHebi8.TaskID, "tester", "archive"); !errors.Is(err, ErrProtectedTask) {
		t.Fatalf("Item 3 REGRESSION: expected ErrProtectedTask on CancelActiveAttemptIfArchivable for Hebi8 main, got %v", err)
	}
	reloadedMain, err := st.GetTask(ctx, mainHebi8.TaskID)
	if err != nil || reloadedMain.ActiveAttemptID != mainAtt.AttemptID {
		t.Fatalf("Item 3 REGRESSION: Hebi8 main active attempt was modified! got %+v err=%v", reloadedMain, err)
	}
}

func TestArchiveUnarchivePreservesUpdatedAtAndDistinctSortOrder(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	st, err := Open(ctx, filepath.Join(tmpDir, "orbit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.SyncRegistry(ctx, tmpDir, protocol.ProjectsConfig{
		SchemaVersion: 1,
		Projects: map[string]protocol.ProjectConfig{
			"demo-proj": {
				DisplayName:       "Demo Project",
				RepoIdentity:      "git:demo",
				DefaultBranch:     "main",
				DefaultCapability: "primary-dev",
				DefaultAdapter:    protocol.AdapterAgy,
			},
		},
	}, protocol.MachinesConfig{
		SchemaVersion: 1,
		Machines: map[string]protocol.MachineConfig{
			"hub-node": {
				DisplayName:  "hub-node",
				Transport:    protocol.TransportLocalWS,
				Capabilities: []string{"primary-dev"},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	olderActive, err := st.CreateTask(ctx, CreateTaskParams{
		ProjectID: "demo-proj", Title: "Older Active", Goal: "older", MachineID: "hub-node",
	})
	if err != nil {
		t.Fatal(err)
	}
	newerActive, err := st.CreateTask(ctx, CreateTaskParams{
		ProjectID: "demo-proj", Title: "Newer Active", Goal: "newer", MachineID: "hub-node",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Pin distinct historical updated_at timestamps
	tsOlder := time.Now().UnixMilli() - 60_000
	tsNewer := time.Now().UnixMilli() - 5_000
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE tasks SET updated_at = ? WHERE task_id = ?`, tsOlder, olderActive.Task.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE tasks SET updated_at = ? WHERE task_id = ?`, tsNewer, newerActive.Task.TaskID); err != nil {
		t.Fatal(err)
	}

	// Archive newerActive FIRST, then sleep briefly and archive olderActive SECOND
	archNewer, _, err := st.ArchiveTask(ctx, newerActive.Task.TaskID, "human")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	archOlder, _, err := st.ArchiveTask(ctx, olderActive.Task.TaskID, "human")
	if err != nil {
		t.Fatal(err)
	}

	// updated_at MUST NOT be overwritten by ArchiveTask!
	if archNewer.UpdatedAt != tsNewer {
		t.Fatalf("ArchiveTask overwrote updated_at on newerActive: got %d, want %d", archNewer.UpdatedAt, tsNewer)
	}
	if archOlder.UpdatedAt != tsOlder {
		t.Fatalf("ArchiveTask overwrote updated_at on olderActive: got %d, want %d", archOlder.UpdatedAt, tsOlder)
	}
	if archOlder.ArchivedAt == nil || archNewer.ArchivedAt == nil || *archOlder.ArchivedAt <= *archNewer.ArchivedAt {
		t.Fatalf("expected olderActive archived AFTER newerActive: older=%v newer=%v", archOlder.ArchivedAt, archNewer.ArchivedAt)
	}

	// Default archived=only listing orders by archived_at DESC (olderActive first, newerActive second)
	// while updatedAt remains strictly distinguishable (newerActive.UpdatedAt > olderActive.UpdatedAt).
	recentOnly, err := st.ListRecentTasksFiltered(ctx, 20, "", "", "demo-proj", "only")
	if err != nil {
		t.Fatal(err)
	}
	if len(recentOnly) != 2 || recentOnly[0].TaskID != olderActive.Task.TaskID || recentOnly[1].TaskID != newerActive.Task.TaskID {
		t.Fatalf("expected ListRecentTasksFiltered archived=only ordered by archived_at DESC [olderActive, newerActive], got %+v", recentOnly)
	}
	if recentOnly[0].UpdatedAt >= recentOnly[1].UpdatedAt {
		t.Fatalf("expected preserved UpdatedAt to distinguish active order: [0]=%d [1]=%d", recentOnly[0].UpdatedAt, recentOnly[1].UpdatedAt)
	}
	projOnly, err := st.ListProjectTasks(ctx, "demo-proj", "only")
	if err != nil {
		t.Fatal(err)
	}
	if len(projOnly) != 2 || projOnly[0].TaskID != olderActive.Task.TaskID || projOnly[1].TaskID != newerActive.Task.TaskID {
		t.Fatalf("expected ListProjectTasks archived=only ordered by archived_at DESC, got %+v", projOnly)
	}

	// UnarchiveTask MUST ALSO preserve original updated_at!
	restored, _, err := st.UnarchiveTask(ctx, olderActive.Task.TaskID, "human")
	if err != nil {
		t.Fatal(err)
	}
	if restored.UpdatedAt != tsOlder || restored.ArchivedAt != nil {
		t.Fatalf("UnarchiveTask must preserve updated_at=%d and clear archivedAt, got updatedAt=%d archivedAt=%v", tsOlder, restored.UpdatedAt, restored.ArchivedAt)
	}
}
