package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "orbit.sqlite")
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open failed: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Seed two projects and two machines
	now := time.Now().UnixMilli()
	err = s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		for _, p := range []struct{ id, name string }{
			{"proj-a", "Project A"},
			{"proj-b", "Project B"},
		} {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO projects (project_id, display_name, repo_identity, default_branch, mirror_bare_path, config_json, archived, updated_at)
				VALUES (?, ?, ?, 'main', ?, '{}', 0, ?)
			`, p.id, p.name, "github.com/example/"+p.id, "/tmp/"+p.id+".git", now); err != nil {
				return err
			}
		}
		for _, m := range []struct{ id, tr string }{
			{"hub-node", "local_ws"},
			{"mac-node", "remote_ws"},
			{"deploy-vps", "ssh_outbound"},
		} {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO machines (machine_id, transport_mode, status, capabilities_json, resources_json, updated_at)
				VALUES (?, ?, 'online', '[]', '{}', ?)
			`, m.id, m.tr, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed failed: %v", err)
	}
	return s
}

func seedTask(t *testing.T, s *store.Store, taskID, projectID string) {
	t.Helper()
	now := time.Now().UnixMilli()
	err := s.WithWriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO tasks (task_id, project_id, title, goal, required_capability, state, created_by, created_at, updated_at)
			VALUES (?, ?, 'Task Title', 'Task Goal', 'primary-dev', 'queued', 'human', ?, ?)
		`, taskID, projectID, now, now)
		return err
	})
	if err != nil {
		t.Fatalf("seedTask %s failed: %v", taskID, err)
	}
}

// 1. waiting_resource active uniqueness (idx_attempts_one_active_per_task)
func TestConstraint_WaitingResourceActiveUniquePerTask(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedTask(t, s, "tsk_1", "proj-a")
	now := time.Now().UnixMilli()

	// Insert first attempt in waiting_resource (an active state)
	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO attempts (attempt_id, task_id, project_id, attempt_no, machine_id, adapter, trigger_kind, state, lease_token, lease_expires_at, started_at)
			VALUES ('att_1', 'tsk_1', 'proj-a', 1, 'mac-node', 'agy', 'initial', 'waiting_resource', 'tok1', ?, ?)
		`, now+45000, now)
		return err
	})
	if err != nil {
		t.Fatalf("insert waiting_resource attempt failed: %v", err)
	}

	// Second active attempt for same task MUST be blocked by idx_attempts_one_active_per_task
	err = s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO attempts (attempt_id, task_id, project_id, attempt_no, machine_id, adapter, trigger_kind, state, lease_token, lease_expires_at, started_at)
			VALUES ('att_2', 'tsk_1', 'proj-a', 2, 'mac-node', 'agy', 'retry', 'leased', 'tok2', ?, ?)
		`, now+45000, now)
		return err
	})
	if err == nil {
		t.Fatalf("expected unique index violation for second active attempt while first is waiting_resource")
	}

	// Once att_1 moves to terminal state 'cancelled', att_2 MUST succeed
	err = s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE attempts SET state = 'cancelled', finished_at = ? WHERE attempt_id = 'att_1'`, now); err != nil {
			return err
		}
		_, err := tx.Exec(`
			INSERT INTO attempts (attempt_id, task_id, project_id, attempt_no, machine_id, adapter, trigger_kind, state, lease_token, lease_expires_at, started_at)
			VALUES ('att_2', 'tsk_1', 'proj-a', 2, 'mac-node', 'agy', 'retry', 'leased', 'tok2', ?, ?)
		`, now+45000, now)
		return err
	})
	if err != nil {
		t.Fatalf("expected second attempt to succeed after first became cancelled: %v", err)
	}
}

// 2. Same wb_session_id active exclusivity (idx_attempts_one_active_per_session)
func TestConstraint_OneActiveAttemptPerSession(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedTask(t, s, "tsk_1", "proj-a")
	seedTask(t, s, "tsk_2", "proj-a")
	now := time.Now().UnixMilli()

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`
			INSERT INTO agent_sessions (wb_session_id, task_id, project_id, machine_id, adapter, cwd, desired_config_json, runtime_state, last_activity_at, created_at)
			VALUES ('ses_1', 'tsk_1', 'proj-a', 'hub-node', 'agy', '/tmp/wt1', '{}', 'live', ?, ?)
		`, now, now); err != nil {
			return err
		}
		_, err := tx.Exec(`
			INSERT INTO attempts (attempt_id, task_id, project_id, attempt_no, machine_id, adapter, wb_session_id, trigger_kind, state, lease_token, lease_expires_at, started_at)
			VALUES ('att_1', 'tsk_1', 'proj-a', 1, 'hub-node', 'agy', 'ses_1', 'initial', 'swarm_monitoring', 'tok1', ?, ?)
		`, now+45000, now)
		return err
	})
	if err != nil {
		t.Fatalf("seed session & active attempt failed: %v", err)
	}

	// Second active attempt reusing ses_1 MUST be blocked even on another task
	err = s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO attempts (attempt_id, task_id, project_id, attempt_no, machine_id, adapter, wb_session_id, trigger_kind, state, lease_token, lease_expires_at, started_at)
			VALUES ('att_2', 'tsk_2', 'proj-a', 1, 'hub-node', 'agy', 'ses_1', 'followup', 'running', 'tok2', ?, ?)
		`, now+45000, now)
		return err
	})
	if err == nil {
		t.Fatalf("expected idx_attempts_one_active_per_session to reject concurrent reuse of ses_1")
	}
}

// 3. Same target_env concurrent ssh_deploy block (idx_attempts_one_running_deploy_per_env)
func TestConstraint_OneRunningDeployPerEnv(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedTask(t, s, "tsk_1", "proj-a")
	seedTask(t, s, "tsk_2", "proj-b")
	now := time.Now().UnixMilli()

	// Insert first running ssh_deploy on deploy-vps:prod
	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO attempts (attempt_id, task_id, project_id, attempt_no, machine_id, adapter, trigger_kind, target_env, deploy_phase, state, lease_token, lease_expires_at, started_at)
			VALUES ('att_d1', 'tsk_1', 'proj-a', 1, 'deploy-vps', 'ssh_deploy', 'deploy', 'deploy-vps:prod', 'pushing', 'running', 'tok1', ?, ?)
		`, now+45000, now)
		return err
	})
	if err != nil {
		t.Fatalf("insert first deploy failed: %v", err)
	}

	// Second concurrent running deploy on same target_env MUST fail
	err = s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO attempts (attempt_id, task_id, project_id, attempt_no, machine_id, adapter, trigger_kind, target_env, deploy_phase, state, lease_token, lease_expires_at, started_at)
			VALUES ('att_d2', 'tsk_2', 'proj-b', 1, 'deploy-vps', 'ssh_deploy', 'deploy', 'deploy-vps:prod', 'pushing', 'running', 'tok2', ?, ?)
		`, now+45000, now)
		return err
	})
	if err == nil {
		t.Fatalf("expected idx_attempts_one_running_deploy_per_env to reject concurrent deploy on same target_env")
	}

	// A deploy on same target_env in 'waiting_approval' OR on a different target_env MUST succeed
	err = s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO attempts (attempt_id, task_id, project_id, attempt_no, machine_id, adapter, trigger_kind, target_env, deploy_phase, state, lease_token, lease_expires_at, started_at)
			VALUES ('att_d2', 'tsk_2', 'proj-b', 1, 'deploy-vps', 'ssh_deploy', 'deploy', 'deploy-vps:prod', 'awaiting_approval', 'waiting_approval', 'tok2', ?, ?)
		`, now+45000, now)
		return err
	})
	if err != nil {
		t.Fatalf("expected waiting_approval deploy on same env to be allowed: %v", err)
	}
}

// 4. completion_source='typo' rejected while NULL allowed
func TestConstraint_CompletionSourceCheckAndNullable(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedTask(t, s, "tsk_1", "proj-a")
	now := time.Now().UnixMilli()

	// NULL completion_source MUST succeed
	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO attempts (attempt_id, task_id, project_id, attempt_no, machine_id, adapter, trigger_kind, state, completion_source, lease_token, lease_expires_at, started_at)
			VALUES ('att_null', 'tsk_1', 'proj-a', 1, 'hub-node', 'agy', 'initial', 'running', NULL, 'tok1', ?, ?)
		`, now+45000, now)
		return err
	})
	if err != nil {
		t.Fatalf("expected NULL completion_source to be allowed: %v", err)
	}

	// 'typo' completion_source MUST be rejected by CHECK constraint
	err = s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO attempts (attempt_id, task_id, project_id, attempt_no, machine_id, adapter, trigger_kind, state, completion_source, lease_token, lease_expires_at, started_at)
			VALUES ('att_typo', 'tsk_1', 'proj-a', 2, 'hub-node', 'agy', 'retry', 'succeeded', 'typo', 'tok2', ?, ?)
		`, now+45000, now)
		return err
	})
	if err == nil {
		t.Fatalf("expected completion_source='typo' to be rejected by CHECK constraint")
	}
}

// 5. Cross-task / cross-project deliveries & annotations rejected by composite foreign keys
func TestConstraint_CompositeForeignKeysPreventCrossTaskPollution(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedTask(t, s, "tsk_a", "proj-a")
	seedTask(t, s, "tsk_b", "proj-b")
	now := time.Now().UnixMilli()

	// Create valid attempt on (att_a, tsk_a, proj-a)
	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO attempts (attempt_id, task_id, project_id, attempt_no, machine_id, adapter, trigger_kind, state, completion_source, lease_token, lease_expires_at, started_at, finished_at)
			VALUES ('att_a', 'tsk_a', 'proj-a', 1, 'hub-node', 'manual', 'manual_import', 'succeeded', 'manual_import', 'tok1', ?, ?, ?)
		`, now, now, now)
		return err
	})
	if err != nil {
		t.Fatalf("seed attempt failed: %v", err)
	}

	// Delivery claiming task tsk_b while pointing to attempt att_a (which belongs to tsk_a) MUST fail!
	err = s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO deliveries (delivery_id, project_id, task_id, attempt_id, source_machine_id, repo_identity, base_sha, head_sha, git_ref, kind, summary, manifest_json, created_at)
			VALUES ('dlv_bad', 'proj-b', 'tsk_b', 'att_a', 'hub-node', 'repo', 'aaa', 'bbb', 'refs/orbit/deliveries/dlv_bad', 'code_diff', 'bad', '{}', ?)
		`, now)
		return err
	})
	if err == nil {
		t.Fatalf("expected composite FK to reject delivery with mismatched (attempt_id, task_id, project_id)")
	}

	// Insert valid delivery on (dlv_a, tsk_a, proj-a)
	err = s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO deliveries (delivery_id, project_id, task_id, attempt_id, source_machine_id, repo_identity, base_sha, head_sha, git_ref, kind, summary, manifest_json, created_at)
			VALUES ('dlv_a', 'proj-a', 'tsk_a', 'att_a', 'hub-node', 'repo', 'aaa', 'bbb', 'refs/orbit/deliveries/dlv_a', 'code_diff', 'ok', '{}', ?)
		`, now)
		return err
	})
	if err != nil {
		t.Fatalf("insert valid delivery failed: %v", err)
	}

	// Annotation pointing to dlv_a but claiming tsk_b / proj-b MUST be rejected by composite FK!
	err = s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO annotations (annotation_id, project_id, task_id, delivery_id, source, anchor_json, body, status, created_at)
			VALUES ('ann_bad', 'proj-b', 'tsk_b', 'dlv_a', 'diff_drawer', '{}', 'cross-task', 'open', ?)
		`, now)
		return err
	})
	if err == nil {
		t.Fatalf("expected composite FK to reject annotation with mismatched (delivery_id, task_id, project_id)")
	}

	// Also verify application-level CreateAnnotation rejects mismatched claimed project/task
	_, _, _, err = s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		DeliveryID:     "dlv_a",
		ClaimedProject: "proj-b",
		Source:         protocol.AnnotationSourceDiffDrawer,
		Body:           "mismatch",
	})
	if !errors.Is(err, store.ErrOwnershipMismatch) {
		t.Fatalf("expected ErrOwnershipMismatch from CreateAnnotation, got %v", err)
	}
}

// 6. approvals.decided_by='pa' rejected while 'human' and NULL allowed
func TestConstraint_ApprovalsDecidedByRejectsPA(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedTask(t, s, "tsk_1", "proj-a")
	now := time.Now().UnixMilli()

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO attempts (attempt_id, task_id, project_id, attempt_no, machine_id, adapter, trigger_kind, state, lease_token, lease_expires_at, started_at)
			VALUES ('att_1', 'tsk_1', 'proj-a', 1, 'deploy-vps', 'ssh_deploy', 'deploy', 'waiting_approval', 'tok1', ?, ?)
		`, now+45000, now)
		return err
	})
	if err != nil {
		t.Fatalf("seed attempt failed: %v", err)
	}

	// decided_by='pa' MUST be rejected
	err = s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO approvals (
				approval_id, project_id, task_id, attempt_id, kind, action_name, target_env,
				input_sha, payload_json, payload_sha256, options_json, requested_by, state,
				decided_by, expires_at, created_at
			) VALUES (
				'apr_bad', 'proj-a', 'tsk_1', 'att_1', 'hetz_deploy', 'ssh_deploy', 'deploy-vps:prod',
				'abc123', '{}', 'hash123', '[]', 'pa', 'approved_pending_delivery',
				'pa', ?, ?
			)
		`, now+60000, now)
		return err
	})
	if err == nil {
		t.Fatalf("expected approvals.decided_by='pa' to be rejected by CHECK constraint")
	}

	// decided_by='human' MUST succeed
	err = s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO approvals (
				approval_id, project_id, task_id, attempt_id, kind, action_name, target_env,
				input_sha, payload_json, payload_sha256, options_json, requested_by, state,
				decided_by, expires_at, created_at
			) VALUES (
				'apr_ok', 'proj-a', 'tsk_1', 'att_1', 'hetz_deploy', 'ssh_deploy', 'deploy-vps:prod',
				'abc123', '{}', 'hash123', '[]', 'pa', 'approved_pending_delivery',
				'human', ?, ?
			)
		`, now+60000, now)
		return err
	})
	if err != nil {
		t.Fatalf("expected approvals.decided_by='human' to succeed: %v", err)
	}
}

// 7. Main state change + events + inbox_items atomic single-transaction commit (and full rollback on failure)
func TestAtomicity_StateEventInboxSingleTransaction(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Success case: CreateManualImportDelivery writes task + attempt + delivery + events + inbox_items atomically
	res, err := s.CreateManualImportDelivery(ctx, store.ManualImportParams{
		DeliveryID:      "dlv_01TESTATOMIC000000000001",
		ProjectID:       "proj-a",
		Title:           "Atomic Delivery",
		Summary:         "Initial atomic test",
		SourceMachineID: "hub-node",
		RepoIdentity:    "github.com/example/proj-a",
		BaseSHA:         "1111111111111111111111111111111111111111",
		HeadSHA:         "2222222222222222222222222222222222222222",
		GitRef:          "refs/orbit/deliveries/dlv_01TESTATOMIC000000000001",
		Manifest:        protocol.DeliveryManifest{FilesChanged: 2, Insertions: 10, Deletions: 3},
	})
	if err != nil {
		t.Fatalf("CreateManualImportDelivery failed: %v", err)
	}
	if res.Task.State != protocol.TaskStateInReview || res.InboxItem.Category != protocol.InboxDeliveryReview || len(res.Events) < 2 {
		t.Fatalf("unexpected atomic result: task=%+v inbox=%+v events=%d", res.Task, res.InboxItem, len(res.Events))
	}

	// Bootstrap must reflect all three in one read snapshot
	boot, err := s.Bootstrap(ctx, nil)
	if err != nil {
		t.Fatalf("Bootstrap failed: %v", err)
	}
	if len(boot.OpenInboxItems) != 1 || boot.SnapshotSeq < 2 {
		t.Fatalf("expected 1 open inbox item and snapshotSeq >= 2, got inbox=%d seq=%d", len(boot.OpenInboxItems), boot.SnapshotSeq)
	}

	// Rollback case: simulate failure on inbox_items inside a write tx -> task & events must also roll back
	now := time.Now().UnixMilli()
	err = s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`
			INSERT INTO tasks (task_id, project_id, title, goal, required_capability, state, created_by, created_at, updated_at)
			VALUES ('tsk_rollback', 'proj-a', 'Will Rollback', 'Goal', 'primary-dev', 'in_review', 'human', ?, ?)
		`, now, now); err != nil {
			return err
		}
		if _, err := tx.Exec(`
			INSERT INTO events (event_id, stream_id, project_id, task_id, actor_kind, actor_id, kind, visibility, payload_json, created_at)
			VALUES ('evt_rollback', 'task:tsk_rollback', 'proj-a', 'tsk_rollback', 'human', 'u', 'task.created', 'timeline', '{}', ?)
		`, now); err != nil {
			return err
		}
		// Violate inbox_items category CHECK constraint
		_, err := tx.Exec(`
			INSERT INTO inbox_items (inbox_id, project_id, task_id, category, ref_id, title, summary, priority, state, created_at, updated_at)
			VALUES ('inb_bad', 'proj-a', 'tsk_rollback', 'invalid_cat', 'ref', 't', 's', 1, 'open', ?, ?)
		`, now, now)
		return err
	})
	if err == nil {
		t.Fatalf("expected invalid inbox category to abort transaction")
	}

	var taskCount, eventCount int
	_ = s.ReadDB().QueryRow(`SELECT COUNT(*) FROM tasks WHERE task_id = 'tsk_rollback'`).Scan(&taskCount)
	_ = s.ReadDB().QueryRow(`SELECT COUNT(*) FROM events WHERE event_id = 'evt_rollback'`).Scan(&eventCount)
	if taskCount != 0 || eventCount != 0 {
		t.Fatalf("transaction did not roll back atomically: taskCount=%d eventCount=%d", taskCount, eventCount)
	}
}

// 8. BackupTo hot backup integrity verification under WAL mode
func TestBackupTo_HotBackupIntegrity(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Verify journal_mode is WAL
	var mode string
	if err := s.WriteDB().QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("expected WAL journal_mode, got %q", mode)
	}

	// Write live data into WAL
	_, err := s.CreateManualImportDelivery(ctx, store.ManualImportParams{
		DeliveryID:      "dlv_01BACKUPTEST00000000001",
		ProjectID:       "proj-a",
		Title:           "Backup Verification",
		Summary:         "Verify hot backup captures WAL commits",
		SourceMachineID: "hub-node",
		RepoIdentity:    "github.com/example/proj-a",
		BaseSHA:         "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		HeadSHA:         "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		GitRef:          "refs/orbit/deliveries/dlv_01BACKUPTEST00000000001",
		Manifest:        protocol.DeliveryManifest{FilesChanged: 1, Insertions: 5, Deletions: 1},
	})
	if err != nil {
		t.Fatalf("seed delivery for backup failed: %v", err)
	}

	backupPath := filepath.Join(t.TempDir(), "backups", "orbit-backup.sqlite")
	if err := s.BackupTo(ctx, backupPath); err != nil {
		t.Fatalf("BackupTo failed: %v", err)
	}

	// Open the backup independently and verify PRAGMA integrity_check == ok + data present
	restored, err := store.Open(ctx, backupPath)
	if err != nil {
		t.Fatalf("open restored backup failed: %v", err)
	}
	defer restored.Close()

	var integrity string
	if err := restored.ReadDB().QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatalf("PRAGMA integrity_check failed: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("expected PRAGMA integrity_check == ok, got %q", integrity)
	}

	dlv, _, _, _, err := restored.GetDeliveryDetail(ctx, "dlv_01BACKUPTEST00000000001")
	if err != nil {
		t.Fatalf("expected delivery in backup DB: %v", err)
	}
	if dlv.HeadSHA != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("unexpected headSha in restored backup: %s", dlv.HeadSHA)
	}
}

func TestManualImport_ConflictIdempotencyBlobRefCountAndReviewComplete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	// Repeated PutBlobRecord must keep ref_count = 0
	if err := s.PutBlobRecord(ctx, sha, 42, "/tmp/blob-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutBlobRecord(ctx, sha, 42, "/tmp/blob-a"); err != nil {
		t.Fatal(err)
	}
	rec, err := s.GetBlobRecord(ctx, sha)
	if err != nil || rec.RefCount != 0 {
		t.Fatalf("expected ref_count=0 after PUTs, got %+v err=%v", rec, err)
	}

	// Wrong manifest blob size MUST fail with ErrBlobMismatch
	_, err = s.CreateManualImportDelivery(ctx, store.ManualImportParams{
		DeliveryID:      "dlv_01BADSIZE000000000001",
		ProjectID:       "proj-a",
		Summary:         "bad size",
		SourceMachineID: "hub-node",
		RepoIdentity:    "repo",
		BaseSHA:         "1111111111111111111111111111111111111111",
		HeadSHA:         "2222222222222222222222222222222222222222",
		GitRef:          "refs/orbit/deliveries/dlv_01BADSIZE000000000001",
		Manifest: protocol.DeliveryManifest{
			Blobs: []protocol.BlobManifestItem{{Path: "a.bin", SHA256: sha, Size: 999}},
		},
	})
	if !errors.Is(err, store.ErrBlobMismatch) {
		t.Fatalf("expected ErrBlobMismatch for mismatched manifest blob size, got %v", err)
	}

	// New delivery referencing same blob twice under two paths -> ref_count increments ONCE (+1)
	baseParams := store.ManualImportParams{
		DeliveryID:      "dlv_01IDEMP0000000000001",
		ProjectID:       "proj-a",
		Summary:         "ok",
		SourceMachineID: "hub-node",
		RepoIdentity:    "repo",
		BaseSHA:         "1111111111111111111111111111111111111111",
		HeadSHA:         "2222222222222222222222222222222222222222",
		GitRef:          "refs/orbit/deliveries/dlv_01IDEMP0000000000001",
		Manifest: protocol.DeliveryManifest{
			Blobs: []protocol.BlobManifestItem{
				{Path: "a.bin", SHA256: sha, Size: 42},
				{Path: "copy.bin", SHA256: sha, Size: 42},
			},
		},
	}
	res1, err := s.CreateManualImportDelivery(ctx, baseParams)
	if err != nil || !res1.Created {
		t.Fatalf("first import failed: err=%v created=%v", err, res1.Created)
	}
	rec, _ = s.GetBlobRecord(ctx, sha)
	if rec.RefCount != 1 {
		t.Fatalf("expected ref_count=1 (deduped per delivery), got %d", rec.RefCount)
	}

	// Exact idempotent replay -> Created=false, ref_count stays 1
	res2, err := s.CreateManualImportDelivery(ctx, baseParams)
	if err != nil || res2.Created || res2.Delivery.DeliveryID != res1.Delivery.DeliveryID {
		t.Fatalf("expected idempotent replay Created=false, got err=%v created=%v", err, res2.Created)
	}
	rec, _ = s.GetBlobRecord(ctx, sha)
	if rec.RefCount != 1 {
		t.Fatalf("expected ref_count to remain 1 on replay, got %d", rec.RefCount)
	}

	// Same deliveryId with different headSha -> ErrConflict!
	conflictParams := baseParams
	conflictParams.HeadSHA = "3333333333333333333333333333333333333333"
	if _, err := s.CreateManualImportDelivery(ctx, conflictParams); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict for mismatched headSha on existing deliveryId, got %v", err)
	}

	// Same deliveryId with different projectId -> ErrConflict!
	conflictProj := baseParams
	conflictProj.ProjectID = "proj-b"
	if _, err := s.CreateManualImportDelivery(ctx, conflictProj); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict for mismatched projectId on existing deliveryId, got %v", err)
	}

	// CompleteDeliveryReview resolves inbox item and emits event
	cr, ev, err := s.CompleteDeliveryReview(ctx, baseParams.DeliveryID, "human")
	if err != nil || cr.InboxState != protocol.InboxResolved || ev == nil || ev.Kind != protocol.EventDeliveryReviewCompleted {
		t.Fatalf("unexpected CompleteDeliveryReview result: cr=%+v ev=%+v err=%v", cr, ev, err)
	}
	boot, err := s.Bootstrap(ctx, nil)
	if err != nil || len(boot.OpenInboxItems) != 0 {
		t.Fatalf("expected 0 open inbox items after CompleteDeliveryReview, got %d err=%v", len(boot.OpenInboxItems), err)
	}
}

func TestGetTaskTimeline_ReturnsLatestPageAscending(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedTask(t, s, "tsk_many", "proj-a")
	now := time.Now().UnixMilli()

	// Insert 520 timeline events
	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		for i := 1; i <= 520; i++ {
			_, err := tx.Exec(`
				INSERT INTO events (event_id, stream_id, project_id, task_id, actor_kind, actor_id, kind, visibility, payload_json, created_at)
				VALUES (?, 'task:tsk_many', 'proj-a', 'tsk_many', 'system', 'sys', 'task.state_changed', 'timeline', '{}', ?)
			`, fmt.Sprintf("evt_many_%04d", i), now+int64(i))
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed 520 events failed: %v", err)
	}

	tl, err := s.GetTaskTimeline(ctx, "tsk_many", 0, false)
	if err != nil {
		t.Fatalf("GetTaskTimeline failed: %v", err)
	}
	if len(tl.Events) != 500 {
		t.Fatalf("expected latest 500 events, got %d", len(tl.Events))
	}
	if tl.Events[0].EventID != "evt_many_0021" || tl.Events[499].EventID != "evt_many_0520" {
		t.Fatalf("expected ascending latest page 21..520, got %s..%s", tl.Events[0].EventID, tl.Events[499].EventID)
	}

	// Paginate backwards using beforeSeq = tl.Events[0].Seq -> must return 1..20 ascending
	older, err := s.GetTaskTimeline(ctx, "tsk_many", tl.Events[0].Seq, false)
	if err != nil {
		t.Fatalf("GetTaskTimeline beforeSeq failed: %v", err)
	}
	if len(older.Events) != 20 || older.Events[0].EventID != "evt_many_0001" || older.Events[19].EventID != "evt_many_0020" {
		t.Fatalf("expected older page 1..20, got len=%d", len(older.Events))
	}
}
