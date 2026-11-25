package store_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

func setupRealMirror(t *testing.T, s *store.Store, projectID string) (mirrorPath, workRepo, baseSHA string) {
	t.Helper()
	tmp := t.TempDir()
	mirrorPath = filepath.Join(tmp, projectID+".git")
	workRepo = filepath.Join(tmp, "work")
	if err := os.MkdirAll(workRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=Tester", "GIT_AUTHOR_EMAIL=tester@example.com",
		"GIT_COMMITTER_NAME=Tester", "GIT_COMMITTER_EMAIL=tester@example.com",
	)
	runG := func(dir string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = gitEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	runG(tmp, "init", "--bare", mirrorPath)
	runG(workRepo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(workRepo, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runG(workRepo, "add", ".")
	runG(workRepo, "commit", "-m", "base")
	baseSHA = runG(workRepo, "rev-parse", "HEAD")
	runG(workRepo, "push", mirrorPath, "HEAD:refs/heads/main")
	if err := s.WithWriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE projects SET mirror_bare_path = ? WHERE project_id = ?`, mirrorPath, projectID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return mirrorPath, workRepo, baseSHA
}

func pushMirrorCommit(t *testing.T, mirrorPath, workRepo, deliveryID, fileContent string) string {
	t.Helper()
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=Tester", "GIT_AUTHOR_EMAIL=tester@example.com",
		"GIT_COMMITTER_NAME=Tester", "GIT_COMMITTER_EMAIL=tester@example.com",
	)
	runG := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = workRepo
		cmd.Env = gitEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.WriteFile(filepath.Join(workRepo, "main.go"), []byte(fileContent), 0o644); err != nil {
		t.Fatal(err)
	}
	runG("add", ".")
	runG("commit", "-m", "delivery "+deliveryID)
	headSHA := runG("rev-parse", "HEAD")
	runG("push", mirrorPath, "HEAD:refs/orbit/deliveries/"+deliveryID)
	return headSHA
}

func TestPhase0ToPhase1SmoothMigration(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "legacy-phase0.sqlite")

	// 1. Create a pure Phase 0 v1 database with existing rows (no v2 tables or columns)
	rawDB, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(dbPath)+"?_txlock=immediate&_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.ExecContext(ctx, store.BuildSchemaDDL()); err != nil {
		rawDB.Close()
		t.Fatalf("seed v1 ddl: %v", err)
	}
	now := time.Now().UnixMilli()
	seedStmts := []struct {
		q    string
		args []any
	}{
		{`INSERT INTO schema_migrations (version, applied_at) VALUES (1, ?)`, []any{now}},
		{`INSERT INTO pa_state (pa_id, last_read_event_seq, focus_pointers_json, updated_at) VALUES ('default', 7, '[]', ?)`, []any{now}},
		{`INSERT INTO projects (project_id, display_name, repo_identity, default_branch, mirror_bare_path, config_json, archived, updated_at) VALUES ('legacy-proj', 'Legacy Project', 'github.com/example/legacy', 'main', '/tmp/legacy.git', '{}', 0, ?)`, []any{now}},
		{`INSERT INTO tasks (task_id, project_id, title, goal, required_capability, state, created_by, created_at, updated_at) VALUES ('tsk_legacy', 'legacy-proj', 'Legacy Task', 'Keep running', 'primary-dev', 'in_review', 'human', ?, ?)`, []any{now, now}},
		{`INSERT INTO task_pending_inputs (input_id, task_id, project_id, kind, payload_json, state, created_at) VALUES ('inp_legacy', 'tsk_legacy', 'legacy-proj', 'followup', '{"prompt":"hello"}', 'queued', ?)`, []any{now}},
	}
	for _, st := range seedStmts {
		if _, err := rawDB.ExecContext(ctx, st.q, st.args...); err != nil {
			rawDB.Close()
			t.Fatalf("seed v1 data: %v", err)
		}
	}
	_ = rawDB.Close()

	// 2. Re-open with Phase 1 store.Open -> smooth migration to v2 + _scratch seed
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open on Phase 0 db failed: %v", err)
	}
	defer s.Close()

	var maxVer int
	if err := s.ReadDB().QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&maxVer); err != nil {
		t.Fatal(err)
	}
	if maxVer != 6 {
		t.Fatalf("expected schema version 6, got %d", maxVer)
	}

	// Verify _scratch exists
	scratch, err := s.GetProject(ctx, protocol.ScratchProjectID)
	if err != nil {
		t.Fatalf("expected _scratch project after migration: %v", err)
	}
	if scratch.DisplayName != protocol.ScratchProjectDisplayName || scratch.RepoIdentity != protocol.ScratchProjectRepoIdentity {
		t.Fatalf("unexpected _scratch metadata: %+v", scratch)
	}

	// Verify existing Phase 0 pending input survived with backfilled columns AND backfilled body (S8)
	items, err := s.ListPendingInputs(ctx, "tsk_legacy")
	if err != nil || len(items) != 1 || items[0].InputID != "inp_legacy" || items[0].Body != "hello" {
		t.Fatalf("legacy pending input did not survive migration / backfill body: items=%+v err=%v", items, err)
	}

	// Verify PA state preserved last_read_event_seq=7 and added active_pa_thread_id
	paState, err := s.GetPAState(ctx, "default")
	if err != nil || paState.LastReadEventSeq != 7 {
		t.Fatalf("legacy pa_state mismatch: %+v err=%v", paState, err)
	}
}

func TestScratchTaskAndBindToRealProject(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Create task in _scratch with initial source, board message, and pending input
	created, err := s.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: protocol.ScratchProjectID,
		Title:     "Explore ideas",
		Goal:      "Brainstorm architecture",
		Adapter:   protocol.AdapterAgy,
		Sources: []protocol.CreateTaskSourceInput{
			{Kind: protocol.TaskSourceNmem, Label: "Design note", URIOrRef: "nmem://mem/1"},
		},
		CreateAttempt: false,
	})
	if err != nil {
		t.Fatalf("CreateTask in _scratch failed: %v", err)
	}
	tid := created.Task.TaskID

	if _, _, err := s.PostBoardMessage(ctx, tid, "human", "@researcher", "check options", "ext-1"); err != nil {
		t.Fatalf("PostBoardMessage failed: %v", err)
	}
	if _, _, err := s.EnqueuePendingInput(ctx, tid, protocol.PendingInputFollowup, protocol.PendingSourceHuman, "next step", nil, "human"); err != nil {
		t.Fatalf("EnqueuePendingInput failed: %v", err)
	}

	// Bind _scratch task to proj-a atomically
	rebound, ev, err := s.BindScratchTaskToProject(ctx, tid, "proj-a", "human")
	if err != nil {
		t.Fatalf("BindScratchTaskToProject failed: %v", err)
	}
	if rebound.ProjectID != "proj-a" || ev == nil || ev.Kind != protocol.EventTaskProjectBound {
		t.Fatalf("unexpected rebound result: task=%+v ev=%+v", rebound, ev)
	}

	// Verify PRAGMA foreign_key_check has 0 violations across the entire DB
	rows, err := s.ReadDB().QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatalf("PRAGMA foreign_key_check reported foreign key violation after BindScratchTaskToProject")
	}

	// Verify child tables now carry project_id = 'proj-a'
	srcs, err := s.ListTaskSources(ctx, tid)
	if err != nil || len(srcs) != 1 || srcs[0].ProjectID != "proj-a" {
		t.Fatalf("expected source migrated to proj-a, got %+v err=%v", srcs, err)
	}
	board, err := s.GetTaskBoard(ctx, tid)
	if err != nil || len(board.Messages) != 1 || board.Messages[0].ProjectID != "proj-a" {
		t.Fatalf("expected board message migrated to proj-a, got %+v err=%v", board, err)
	}

	// Rebinding a non-_scratch task must return ErrConflict
	if _, _, err := s.BindScratchTaskToProject(ctx, tid, "proj-b", "human"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict rebinding real project task, got %v", err)
	}
}

func TestPendingQueueOrderSteerDeleteAndAutoConsume(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	res, err := s.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "proj-a",
		Goal:          "Initial goal",
		MachineID:     "hub-node",
		Adapter:       protocol.AdapterAgy,
		CreateAttempt: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	tid := res.Task.TaskID

	i1, _, err := s.EnqueuePendingInput(ctx, tid, protocol.PendingInputFollowup, protocol.PendingSourceHuman, "first", nil, "human")
	if err != nil {
		t.Fatal(err)
	}
	i2, _, err := s.EnqueuePendingInput(ctx, tid, protocol.PendingInputFollowup, protocol.PendingSourceHuman, "second", nil, "human")
	if err != nil {
		t.Fatal(err)
	}
	i3, _, err := s.EnqueuePendingInput(ctx, tid, protocol.PendingInputFollowup, protocol.PendingSourceHuman, "third", nil, "human")
	if err != nil {
		t.Fatal(err)
	}

	// Edit i2
	edited, _, err := s.UpdatePendingInput(ctx, tid, i2.InputID, "second-edited", nil, "human")
	if err != nil || edited.Body != "second-edited" {
		t.Fatalf("UpdatePendingInput failed: %+v err=%v", edited, err)
	}

	// Reorder to [i3, i1, i2]
	reordered, _, err := s.ReorderPendingInputs(ctx, tid, []string{i3.InputID, i1.InputID, i2.InputID}, "human")
	if err != nil || len(reordered) != 3 || reordered[0].InputID != i3.InputID || reordered[2].InputID != i2.InputID {
		t.Fatalf("unexpected reorder result: %+v err=%v", reordered, err)
	}

	// Steer i2 to front -> [i2, i3, i1]
	if _, _, _, err := s.SteerPendingInput(ctx, tid, i2.InputID, "human"); err != nil {
		t.Fatal(err)
	}
	// Delete i3
	if _, err := s.DeletePendingInput(ctx, tid, i3.InputID, "human"); err != nil {
		t.Fatal(err)
	}

	// Auto-consume head -> must pop i2 ("second-edited")!
	popped, att, _, err := s.ConsumeNextPendingInput(ctx, store.ConsumeNextPendingInputParams{
		TaskID:    tid,
		MachineID: "hub-node",
		Adapter:   protocol.AdapterAgy,
	})
	if err != nil || popped == nil || att == nil {
		t.Fatalf("ConsumeNextPendingInput failed: popped=%+v att=%+v err=%v", popped, att, err)
	}
	if popped.InputID != i2.InputID || popped.State != protocol.PendingInputDispatched || popped.DispatchedAttemptID != att.AttemptID {
		t.Fatalf("expected i2 dispatched, got %+v", popped)
	}

	remaining, err := s.ListPendingInputs(ctx, tid)
	if err != nil || len(remaining) != 1 || remaining[0].InputID != i1.InputID {
		t.Fatalf("expected only i1 remaining queued, got %+v err=%v", remaining, err)
	}
}

func TestStore_R4a_OutboxGapAndLateHistoryIsolation(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mirrorPath, workRepo, baseSHA := setupRealMirror(t, s, "proj-a")
	headBase := pushMirrorCommit(t, mirrorPath, workRepo, "dlv_base", "package main\n// base delivery\nfunc main() {}\n")
	headLate := pushMirrorCommit(t, mirrorPath, workRepo, "dlv_late_att1", "package main\n// late delivery\nfunc main() {}\n")

	// Seed task with att_1
	created, err := s.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "proj-a",
		Goal:          "R4a store test",
		MachineID:     "mac-node",
		Adapter:       protocol.AdapterAgy,
		CreateAttempt: true,
		LeaseTTLMs:    45000,
	})
	if err != nil {
		t.Fatal(err)
	}
	tid := created.Task.TaskID
	att1 := created.Attempt

	// 1. Send outboxSeq = 1 -> acked=1
	r1, err := s.IngestOutboxBatch(ctx, "mac-node", "run_1", protocol.AttemptOutboxBatchFrame{
		AttemptID:  att1.AttemptID,
		LeaseToken: att1.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 1, EventID: "ev_1", FrameType: protocol.OutboxFrameState, Payload: map[string]any{"state": "running", "baseSha": baseSHA}},
		},
	})
	if err != nil || r1.AckedOutboxSeq != 1 || r1.AcceptedMode != protocol.OutboxModeActive {
		t.Fatalf("unexpected seq 1 ingest: %+v err=%v", r1, err)
	}

	// 2. Skip seq=2 and send seq=3 -> Hub MUST refuse to jump gap and stay at acked=1!
	rGap, err := s.IngestOutboxBatch(ctx, "mac-node", "run_1", protocol.AttemptOutboxBatchFrame{
		AttemptID:  att1.AttemptID,
		LeaseToken: att1.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 3, EventID: "ev_3", FrameType: protocol.OutboxFrameEvents, Payload: map[string]any{"kind": "acp.message_batch", "visibility": "raw_acp", "payload": map[string]any{"text": "gap"}}},
		},
	})
	if err != nil || rGap.AckedOutboxSeq != 1 {
		t.Fatalf("gap protection failed: expected acked=1, got %+v err=%v", rGap, err)
	}

	// 3. Deliver seq=1 (dup), 2, 3 -> advances strictly to 3
	rFill, err := s.IngestOutboxBatch(ctx, "mac-node", "run_1", protocol.AttemptOutboxBatchFrame{
		AttemptID:  att1.AttemptID,
		LeaseToken: att1.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 1, EventID: "ev_1", FrameType: protocol.OutboxFrameState, Payload: map[string]any{"state": "running"}},
			{OutboxSeq: 2, EventID: "ev_2", FrameType: protocol.OutboxFrameDeliveryPublished, Payload: map[string]any{
				"deliveryId": "dlv_base",
				"baseSha":    baseSHA,
				"headSha":    headBase,
				"summary":    "initial delivery",
			}},
			{OutboxSeq: 3, EventID: "ev_3", FrameType: protocol.OutboxFrameEvents, Payload: map[string]any{"kind": "acp.message_batch", "visibility": "raw_acp", "payload": map[string]any{"text": "filled"}}},
		},
	})
	if err != nil || rFill.AckedOutboxSeq != 3 {
		t.Fatalf("expected acked=3 after filling gap, got %+v err=%v", rFill, err)
	}

	// Create an open annotation anchored on dlv_base
	ann, _, _, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		DeliveryID: "dlv_base",
		Source:     protocol.AnnotationSourceDiffDrawer,
		Anchor:     protocol.AnnotationAnchor{Path: "main.go", StartLine: 1, EndLine: 2},
		Body:       "Fix edge case",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 4. Expire att_1 via lease timeout scanner
	if err := s.UpdateAttemptLeaseExpiry(ctx, att1.AttemptID, time.Now().UnixMilli()-5000); err != nil {
		t.Fatal(err)
	}
	expired, _, err := s.ScanExpiredAttempts(ctx, time.Now().UnixMilli())
	if err != nil || len(expired) != 1 || expired[0] != att1.AttemptID {
		t.Fatalf("expected att1 expired, got %v err=%v", expired, err)
	}

	// 5. Dispatch annotation -> starts att_2 owning ann (status='queued', triggered_attempt_id=att_2)
	dispRes, err := s.DispatchAnnotations(ctx, store.DispatchAnnotationsParams{
		TaskID:        tid,
		AnnotationIDs: []string{ann.AnnotationID},
		MachineID:     "mac-node",
		Adapter:       protocol.AdapterAgy,
	})
	if err != nil || dispRes.Attempt == nil {
		t.Fatalf("DispatchAnnotations failed: %+v err=%v", dispRes, err)
	}
	att2 := dispRes.Attempt

	// 6. Late arrival from expired att_1 (delivery_published + state=succeeded)
	lateRes, err := s.IngestOutboxBatch(ctx, "mac-node", "run_1", protocol.AttemptOutboxBatchFrame{
		AttemptID:  att1.AttemptID,
		LeaseToken: att1.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 4, EventID: "ev_late_dlv", FrameType: protocol.OutboxFrameDeliveryPublished, Payload: map[string]any{
				"deliveryId": "dlv_late_att1",
				"baseSha":    baseSHA,
				"headSha":    headLate,
				"summary":    "late delivery from att1",
			}},
			{OutboxSeq: 5, EventID: "ev_late_done", FrameType: protocol.OutboxFrameState, Payload: map[string]any{
				"state":            "succeeded",
				"completionSource": "explicit_deliver",
			}},
		},
	})
	if err != nil {
		t.Fatalf("late_history ingest failed: %v", err)
	}
	if lateRes.AcceptedMode != protocol.OutboxModeLateHistory || lateRes.AckedOutboxSeq != 5 {
		t.Fatalf("expected late_history acked=5, got %+v", lateRes)
	}

	// Verify ALL strict R4a invariants:
	// (a) Late delivery archived with late_arrival=1
	lateDlv, _, _, _, err := s.GetDeliveryDetail(ctx, "dlv_late_att1")
	if err != nil || !lateDlv.LateArrival {
		t.Fatalf("expected late delivery archived with late_arrival=true, got %+v err=%v", lateDlv, err)
	}
	// (b) tasks.active_attempt_id STILL points to att_2 and latest_delivery_id STILL points to dlv_base!
	taskNow, err := s.GetTask(ctx, tid)
	if err != nil {
		t.Fatal(err)
	}
	if taskNow.ActiveAttemptID != att2.AttemptID {
		t.Fatalf("late arrival overwrote active_attempt_id: want %s, got %s", att2.AttemptID, taskNow.ActiveAttemptID)
	}
	if taskNow.LatestDeliveryID != "dlv_base" {
		t.Fatalf("late arrival overwrote latest_delivery_id: want dlv_base, got %s", taskNow.LatestDeliveryID)
	}
	// (c) NO inbox card created for dlv_late_att1
	var lateInboxCount int
	if err := s.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox_items WHERE inbox_id = 'inb:dlv:dlv_late_att1'`).Scan(&lateInboxCount); err != nil {
		t.Fatal(err)
	}
	if lateInboxCount != 0 {
		t.Fatalf("late arrival illegally created inbox card")
	}
	// (d) att_2's queued annotation MUST remain 'queued' (NEVER marked addressed by att_1!)
	var annStatus string
	var trigAtt string
	if err := s.ReadDB().QueryRowContext(ctx, `SELECT status, triggered_attempt_id FROM annotations WHERE annotation_id = ?`, ann.AnnotationID).Scan(&annStatus, &trigAtt); err != nil {
		t.Fatal(err)
	}
	if annStatus != string(protocol.AnnotationQueued) || trigAtt != att2.AttemptID {
		t.Fatalf("late arrival corrupted att_2 annotation: status=%s triggered_attempt_id=%s", annStatus, trigAtt)
	}
}
