package store_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

func TestB1_StopReasonDeliveryRequirementAndAnnotations(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mirrorPath, workRepo, baseSHA := setupRealMirror(t, s, "proj-a")
	head1 := pushMirrorCommit(t, mirrorPath, workRepo, "dlv_b1_1", "package main\n// b1-1\nfunc main() {}\n")

	// 1. Abnormal stopReason (refusal / max_tokens) on _scratch MUST fail, NEVER succeed.
	for _, sr := range []string{"refusal", "max_tokens", "max_turn_requests"} {
		cr, err := s.CreateTask(ctx, store.CreateTaskParams{
			ProjectID:     protocol.ScratchProjectID,
			Goal:          "test " + sr,
			MachineID:     "hub-node",
			Adapter:       protocol.AdapterAgy,
			CreateAttempt: true,
			LeaseTTLMs:    45000,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.IngestOutboxBatch(ctx, "hub-node", "run_b1", protocol.AttemptOutboxBatchFrame{
			AttemptID:  cr.Attempt.AttemptID,
			LeaseToken: cr.Attempt.LeaseToken,
			Items: []protocol.OutboxItem{
				{OutboxSeq: 1, EventID: "b1_sr_" + sr, FrameType: protocol.OutboxFrameState, Payload: map[string]any{
					"state":      "succeeded",
					"stopReason": sr,
				}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		att, _ := s.GetAttemptRecord(ctx, cr.Attempt.AttemptID)
		if att.State != protocol.AttemptFailed || att.ErrorCode != "ABNORMAL_STOP_REASON" {
			t.Fatalf("stopReason=%s expected failed(ABNORMAL_STOP_REASON), got state=%s err=%s", sr, att.State, att.ErrorCode)
		}
	}

	// 2. Pure _scratch chat turn with stopReason=end_turn and NO delivery succeeds cleanly.
	crScratch, err := s.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     protocol.ScratchProjectID,
		Goal:          "chat question",
		MachineID:     "hub-node",
		Adapter:       protocol.AdapterAgy,
		CreateAttempt: true,
		LeaseTTLMs:    45000,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.IngestOutboxBatch(ctx, "hub-node", "run_b1", protocol.AttemptOutboxBatchFrame{
		AttemptID:  crScratch.Attempt.AttemptID,
		LeaseToken: crScratch.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 1, EventID: "b1_scratch_ok", FrameType: protocol.OutboxFrameState, Payload: map[string]any{
				"state":            "succeeded",
				"stopReason":       "end_turn",
				"completionSource": "prompt_end_turn",
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	attScratch, _ := s.GetAttemptRecord(ctx, crScratch.Attempt.AttemptID)
	if attScratch.State != protocol.AttemptSucceeded {
		t.Fatalf("expected _scratch pure chat to succeed, got %s", attScratch.State)
	}

	// 3. Project annotation turn with NO verified delivery MUST NOT succeed and MUST release queued annotations back to open!
	crProj, err := s.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "proj-a",
		Goal:          "initial",
		MachineID:     "hub-node",
		Adapter:       protocol.AdapterAgy,
		CreateAttempt: true,
		LeaseTTLMs:    45000,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.IngestOutboxBatch(ctx, "hub-node", "run_b1", protocol.AttemptOutboxBatchFrame{
		AttemptID:  crProj.Attempt.AttemptID,
		LeaseToken: crProj.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 1, EventID: "b1_dlv1", FrameType: protocol.OutboxFrameDeliveryPublished, Payload: map[string]any{
				"deliveryId": "dlv_b1_1", "baseSha": baseSHA, "headSha": head1, "summary": "v1",
			}},
			{OutboxSeq: 2, EventID: "b1_done1", FrameType: protocol.OutboxFrameState, Payload: map[string]any{
				"state": "succeeded", "completionSource": "explicit_deliver",
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ann, _, _, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		DeliveryID: "dlv_b1_1",
		Source:     protocol.AnnotationSourceDiffDrawer,
		Anchor:     protocol.AnnotationAnchor{Path: "main.go", StartLine: 1},
		Body:       "Fix nil check",
	})
	if err != nil {
		t.Fatal(err)
	}
	disp, err := s.DispatchAnnotations(ctx, store.DispatchAnnotationsParams{
		TaskID:        crProj.Task.TaskID,
		AnnotationIDs: []string{ann.AnnotationID},
		MachineID:     "hub-node",
		Adapter:       protocol.AdapterAgy,
	})
	if err != nil || disp.Attempt == nil {
		t.Fatalf("DispatchAnnotations failed: %+v err=%v", disp, err)
	}
	// Attempt 2 claims succeeded WITHOUT publishing any verified delivery
	_, err = s.IngestOutboxBatch(ctx, "hub-node", "run_b1", protocol.AttemptOutboxBatchFrame{
		AttemptID:  disp.Attempt.AttemptID,
		LeaseToken: disp.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 1, EventID: "b1_att2_nodlv", FrameType: protocol.OutboxFrameState, Payload: map[string]any{
				"state": "succeeded", "stopReason": "end_turn", "completionSource": "prompt_end_turn",
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	att2, _ := s.GetAttemptRecord(ctx, disp.Attempt.AttemptID)
	if att2.State != protocol.AttemptFailed || att2.ErrorCode != "NO_VERIFIED_DELIVERY" {
		t.Fatalf("expected NO_VERIFIED_DELIVERY failure, got state=%s err=%s", att2.State, att2.ErrorCode)
	}
	var annStatus string
	_ = s.ReadDB().QueryRowContext(ctx, `SELECT status FROM annotations WHERE annotation_id = ?`, ann.AnnotationID).Scan(&annStatus)
	if annStatus != string(protocol.AnnotationOpen) {
		t.Fatalf("expected annotation released back to open when no delivery produced, got %s", annStatus)
	}

	// 4. Unresolved tool failure with delivery -> needs_verification, NOT succeeded, annotation NOT addressed!
	head2 := pushMirrorCommit(t, mirrorPath, workRepo, "dlv_b1_2", "package main\n// b1-2\nfunc main() {}\n")
	disp3, err := s.DispatchAnnotations(ctx, store.DispatchAnnotationsParams{
		TaskID:        crProj.Task.TaskID,
		AnnotationIDs: []string{ann.AnnotationID},
		MachineID:     "hub-node",
		Adapter:       protocol.AdapterAgy,
	})
	if err != nil || disp3.Attempt == nil {
		t.Fatal(err)
	}
	_, err = s.IngestOutboxBatch(ctx, "hub-node", "run_b1", protocol.AttemptOutboxBatchFrame{
		AttemptID:  disp3.Attempt.AttemptID,
		LeaseToken: disp3.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 1, EventID: "b1_dlv2", FrameType: protocol.OutboxFrameDeliveryPublished, Payload: map[string]any{
				"deliveryId": "dlv_b1_2", "baseSha": baseSHA, "headSha": head2, "summary": "v2",
			}},
			{OutboxSeq: 2, EventID: "b1_att3_unres", FrameType: protocol.OutboxFrameState, Payload: map[string]any{
				"state":                       "succeeded",
				"stopReason":                  "end_turn",
				"unresolvedRejectedToolCount": 1,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	att3, _ := s.GetAttemptRecord(ctx, disp3.Attempt.AttemptID)
	if att3.State != protocol.AttemptNeedsVerification {
		t.Fatalf("expected needs_verification with unresolved tool rejection, got %s", att3.State)
	}
	_ = s.ReadDB().QueryRowContext(ctx, `SELECT status FROM annotations WHERE annotation_id = ?`, ann.AnnotationID).Scan(&annStatus)
	if annStatus == string(protocol.AnnotationAddressed) {
		t.Fatalf("annotation must NOT be marked addressed on needs_verification")
	}
}

func TestB4_IngestOutboxInTxLeaseExpiryAndPerFrameProjection(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mirrorPath, workRepo, baseSHA := setupRealMirror(t, s, "proj-a")
	headExp := pushMirrorCommit(t, mirrorPath, workRepo, "dlv_b4_exp", "package main\n// exp\nfunc main() {}\n")
	headFormal := pushMirrorCommit(t, mirrorPath, workRepo, "dlv_b4_formal", "package main\n// formal\nfunc main() {}\n")
	headPostTerm := pushMirrorCommit(t, mirrorPath, workRepo, "dlv_b4_post", "package main\n// post\nfunc main() {}\n")

	// Part A: lease already expired, scanner has NOT run yet -> IngestOutboxBatch MUST expire in-tx and route to late_history!
	crA, err := s.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "proj-a",
		Goal:          "expired before scanner",
		MachineID:     "mac-node",
		Adapter:       protocol.AdapterAgy,
		CreateAttempt: true,
		LeaseTTLMs:    45000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAttemptLeaseExpiry(ctx, crA.Attempt.AttemptID, time.Now().UnixMilli()-5000); err != nil {
		t.Fatal(err)
	}
	resA, err := s.IngestOutboxBatch(ctx, "mac-node", "run_b4", protocol.AttemptOutboxBatchFrame{
		AttemptID:  crA.Attempt.AttemptID,
		LeaseToken: crA.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 1, EventID: "b4_exp_dlv", FrameType: protocol.OutboxFrameDeliveryPublished, Payload: map[string]any{
				"deliveryId": "dlv_b4_exp", "baseSha": baseSHA, "headSha": headExp, "summary": "should be late",
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resA.AcceptedMode != protocol.OutboxModeLateHistory {
		t.Fatalf("expected late_history for expired lease before scanner, got %s", resA.AcceptedMode)
	}
	attA, _ := s.GetAttemptRecord(ctx, crA.Attempt.AttemptID)
	if attA.State != protocol.AttemptExpired {
		t.Fatalf("expected attempt transitioned to expired in-tx, got %s", attA.State)
	}
	dlvExp, _, _, _, err := s.GetDeliveryDetail(ctx, "dlv_b4_exp")
	if err != nil || !dlvExp.LateArrival {
		t.Fatalf("expected dlv_b4_exp recorded as late_arrival=true, got %+v err=%v", dlvExp, err)
	}
	taskA, _ := s.GetTask(ctx, crA.Task.TaskID)
	if taskA.LatestDeliveryID != "" {
		t.Fatalf("expired lease delivery must NOT set latest_delivery_id, got %s", taskA.LatestDeliveryID)
	}

	// Part B: Single batch contains [delivery_published(dlv_b4_formal), state=succeeded, delivery_published(dlv_b4_post)].
	// Per-frame projection check MUST route seq=3 to late_history without overwriting latest_delivery_id!
	crB, err := s.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:     "proj-a",
		Goal:          "per-frame terminal check",
		MachineID:     "mac-node",
		Adapter:       protocol.AdapterAgy,
		CreateAttempt: true,
		LeaseTTLMs:    45000,
	})
	if err != nil {
		t.Fatal(err)
	}
	resB, err := s.IngestOutboxBatch(ctx, "mac-node", "run_b4", protocol.AttemptOutboxBatchFrame{
		AttemptID:  crB.Attempt.AttemptID,
		LeaseToken: crB.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 1, EventID: "b4_f1", FrameType: protocol.OutboxFrameDeliveryPublished, Payload: map[string]any{
				"deliveryId": "dlv_b4_formal", "baseSha": baseSHA, "headSha": headFormal, "summary": "formal",
			}},
			{OutboxSeq: 2, EventID: "b4_f2", FrameType: protocol.OutboxFrameState, Payload: map[string]any{
				"state": "succeeded", "completionSource": "explicit_deliver",
			}},
			{OutboxSeq: 3, EventID: "b4_f3", FrameType: protocol.OutboxFrameDeliveryPublished, Payload: map[string]any{
				"deliveryId": "dlv_b4_post", "baseSha": baseSHA, "headSha": headPostTerm, "summary": "post-terminal same batch",
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resB.AckedOutboxSeq != 3 {
		t.Fatalf("expected acked=3, got %+v", resB)
	}
	taskB, _ := s.GetTask(ctx, crB.Task.TaskID)
	if taskB.LatestDeliveryID != "dlv_b4_formal" {
		t.Fatalf("post-terminal frame in same batch overwrote latest_delivery_id: got %s", taskB.LatestDeliveryID)
	}
	dlvPost, _, _, _, err := s.GetDeliveryDetail(ctx, "dlv_b4_post")
	if err != nil || !dlvPost.LateArrival {
		t.Fatalf("expected post-terminal delivery in same batch archived as late_arrival=true, got %+v err=%v", dlvPost, err)
	}
}

func TestB6_DeliveryAttemptedDistinguishesExpiredVsUncertain(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Case 1: approved_pending_delivery with delivery_attempted=0 (never written to WS) -> restart marks EXPIRED (not uncertain)!
	cr1, err := s.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: "proj-a", Goal: "never sent", MachineID: "hub-node", Adapter: protocol.AdapterAgy,
		CreateAttempt: true, LeaseTTLMs: 45000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BindAttemptRunnerInstance(ctx, cr1.Attempt.AttemptID, "run_old_1"); err != nil {
		t.Fatal(err)
	}
	_, err = s.IngestOutboxBatch(ctx, "hub-node", "run_old_1", protocol.AttemptOutboxBatchFrame{
		AttemptID: cr1.Attempt.AttemptID, LeaseToken: cr1.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 1, EventID: "b6_p1", FrameType: protocol.OutboxFrameApprovalRequest, Payload: map[string]any{
				"approvalId": "apr_unsent", "payloadSha256": "sha_unsent", "title": "Run tool",
				"options": []map[string]any{{"id": "allow_once", "name": "Allow", "kind": "allow_once"}},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideApproval(ctx, store.DecideApprovalParams{
		ApprovalID: "apr_unsent", OptionID: "allow_once", ExpectedAttemptID: cr1.Attempt.AttemptID,
		ExpectedPayloadSHA256: "sha_unsent", ActorID: "human", DecidedBy: protocol.ApprovalDecidedByHuman,
	}); err != nil {
		t.Fatal(err)
	}
	// Notice: MarkApprovalsDeliveryAttempted is NOT called! Runner restarts with new instance ID:
	if _, err := s.ReconcileRunnerHello(ctx, protocol.RunnerHelloFrame{
		MachineID: "hub-node", RunnerInstanceID: "run_new_1", RunnerVersion: "0.2.0",
	}, 45000); err != nil {
		t.Fatal(err)
	}
	ap1, _ := s.GetApproval(ctx, "apr_unsent")
	att1, _ := s.GetAttemptRecord(ctx, cr1.Attempt.AttemptID)
	if ap1.State != protocol.ApprovalExpired || att1.State != protocol.AttemptExpired {
		t.Fatalf("never-sent approval on restart must become expired (not uncertain), got ap=%s att=%s", ap1.State, att1.State)
	}

	// Case 2: approved_pending_delivery with delivery_attempted=1 -> restart marks UNCERTAIN + needs_verification!
	cr2, err := s.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: "proj-a", Goal: "sent to ws", MachineID: "mac-node", Adapter: protocol.AdapterAgy,
		CreateAttempt: true, LeaseTTLMs: 45000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BindAttemptRunnerInstance(ctx, cr2.Attempt.AttemptID, "run_old_2"); err != nil {
		t.Fatal(err)
	}
	_, err = s.IngestOutboxBatch(ctx, "mac-node", "run_old_2", protocol.AttemptOutboxBatchFrame{
		AttemptID: cr2.Attempt.AttemptID, LeaseToken: cr2.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 1, EventID: "b6_p2", FrameType: protocol.OutboxFrameApprovalRequest, Payload: map[string]any{
				"approvalId": "apr_sent", "payloadSha256": "sha_sent", "title": "Run tool",
				"options": []map[string]any{{"id": "allow_once", "name": "Allow", "kind": "allow_once"}},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideApproval(ctx, store.DecideApprovalParams{
		ApprovalID: "apr_sent", OptionID: "allow_once", ExpectedAttemptID: cr2.Attempt.AttemptID,
		ExpectedPayloadSHA256: "sha_sent", ActorID: "human", DecidedBy: protocol.ApprovalDecidedByHuman,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkApprovalsDeliveryAttempted(ctx, []string{"apr_sent"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileRunnerHello(ctx, protocol.RunnerHelloFrame{
		MachineID: "mac-node", RunnerInstanceID: "run_new_2", RunnerVersion: "0.2.0",
	}, 45000); err != nil {
		t.Fatal(err)
	}
	ap2, _ := s.GetApproval(ctx, "apr_sent")
	att2, _ := s.GetAttemptRecord(ctx, cr2.Attempt.AttemptID)
	if ap2.State != protocol.ApprovalUncertain || att2.State != protocol.AttemptNeedsVerification {
		t.Fatalf("attempted-delivery approval on restart must become uncertain+needs_verification, got ap=%s att=%s", ap2.State, att2.State)
	}
}

func TestB7_DecideApprovalExpiredCommitsBeforeReturningConflict(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	cr, err := s.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: "proj-a", Goal: "expired approval", MachineID: "hub-node", Adapter: protocol.AdapterAgy,
		CreateAttempt: true, LeaseTTLMs: 45000,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.IngestOutboxBatch(ctx, "hub-node", "run_b7", protocol.AttemptOutboxBatchFrame{
		AttemptID: cr.Attempt.AttemptID, LeaseToken: cr.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 1, EventID: "b7_p1", FrameType: protocol.OutboxFrameApprovalRequest, Payload: map[string]any{
				"approvalId": "apr_exp", "payloadSha256": "sha_exp", "title": "Late tool",
				"expiresAt": time.Now().UnixMilli() - 5000,
				"options":   []map[string]any{{"id": "allow_once", "name": "Allow", "kind": "allow_once"}},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.DecideApproval(ctx, store.DecideApprovalParams{
		ApprovalID: "apr_exp", OptionID: "allow_once", ExpectedAttemptID: cr.Attempt.AttemptID,
		ExpectedPayloadSHA256: "sha_exp", ActorID: "human", DecidedBy: protocol.ApprovalDecidedByHuman,
	})
	if !errors.Is(err, store.ErrConflict) || !res.Expired || len(res.Events) == 0 {
		t.Fatalf("expected ErrConflict + Expired=true + committed events, got res=%+v err=%v", res, err)
	}
	// Re-read DB to prove expired transition was COMMITTED and NOT rolled back!
	ap, err := s.GetApproval(ctx, "apr_exp")
	if err != nil || ap.State != protocol.ApprovalExpired {
		t.Fatalf("expired approval state was rolled back! got %+v err=%v", ap, err)
	}
	var inboxState string
	_ = s.ReadDB().QueryRowContext(ctx, `SELECT state FROM inbox_items WHERE inbox_id = 'inb:apr:apr_exp'`).Scan(&inboxState)
	if inboxState != string(protocol.InboxResolved) {
		t.Fatalf("expected inbox card resolved on expiry commit, got %s", inboxState)
	}
}

func TestB8_ApprovalAckStrictBinding(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	cr1, _ := s.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: "proj-a", Goal: "t1", MachineID: "hub-node", Adapter: protocol.AdapterAgy, CreateAttempt: true, LeaseTTLMs: 45000,
	})
	cr2, _ := s.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: "proj-a", Goal: "t2", MachineID: "hub-node", Adapter: protocol.AdapterAgy, CreateAttempt: true, LeaseTTLMs: 45000,
	})
	_ = s.BindAttemptRunnerInstance(ctx, cr1.Attempt.AttemptID, "run_b8")
	_ = s.BindAttemptRunnerInstance(ctx, cr2.Attempt.AttemptID, "run_b8")

	_, err := s.IngestOutboxBatch(ctx, "hub-node", "run_b8", protocol.AttemptOutboxBatchFrame{
		AttemptID: cr1.Attempt.AttemptID, LeaseToken: cr1.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 1, EventID: "b8_p1", FrameType: protocol.OutboxFrameApprovalRequest, Payload: map[string]any{
				"approvalId": "apr_b8", "payloadSha256": "sha_b8", "title": "Tool",
				"options": []map[string]any{{"id": "allow_once", "name": "Allow", "kind": "allow_once"}},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	dec, err := s.DecideApproval(ctx, store.DecideApprovalParams{
		ApprovalID: "apr_b8", OptionID: "allow_once", ExpectedAttemptID: cr1.Attempt.AttemptID,
		ExpectedPayloadSHA256: "sha_b8", ActorID: "human", DecidedBy: protocol.ApprovalDecidedByHuman,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Cross-attempt approval_ack from att2 MUST be ignored!
	_, _ = s.IngestOutboxBatch(ctx, "hub-node", "run_b8", protocol.AttemptOutboxBatchFrame{
		AttemptID: cr2.Attempt.AttemptID, LeaseToken: cr2.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 1, EventID: "b8_cross", FrameType: protocol.OutboxFrameApprovalAck, Payload: map[string]any{
				"approvalId": "apr_b8", "decisionId": dec.Approval.DecisionID, "optionId": "allow_once",
			}},
		},
	})
	ap, _ := s.GetApproval(ctx, "apr_b8")
	if ap.State != protocol.ApprovalApprovedPendingDelivery {
		t.Fatalf("cross-attempt ACK illegally consumed approval: %s", ap.State)
	}

	// 2. Empty decisionId via outbox MUST be ignored!
	_, _ = s.IngestOutboxBatch(ctx, "hub-node", "run_b8", protocol.AttemptOutboxBatchFrame{
		AttemptID: cr1.Attempt.AttemptID, LeaseToken: cr1.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 2, EventID: "b8_empty", FrameType: protocol.OutboxFrameApprovalAck, Payload: map[string]any{
				"approvalId": "apr_b8", "decisionId": "", "optionId": "allow_once",
			}},
		},
	})
	ap, _ = s.GetApproval(ctx, "apr_b8")
	if ap.State != protocol.ApprovalApprovedPendingDelivery {
		t.Fatalf("empty decisionId ACK illegally consumed approval: %s", ap.State)
	}

	// 3. Wrong machine / wrong runnerInstanceId via ReconcileRunnerHello MUST NOT consume!
	_, _ = s.ReconcileRunnerHello(ctx, protocol.RunnerHelloFrame{
		MachineID: "mac-node", RunnerInstanceID: "run_b8",
		HandledApprovalDecisions: []protocol.HandledApprovalDecision{
			{ApprovalID: "apr_b8", DecisionID: dec.Approval.DecisionID, AttemptID: cr1.Attempt.AttemptID, Outcome: protocol.ApprovalConsumedAllowed},
		},
	}, 45000)
	ap, _ = s.GetApproval(ctx, "apr_b8")
	if ap.State != protocol.ApprovalApprovedPendingDelivery {
		t.Fatalf("wrong-machine runner.hello illegally consumed approval: %s", ap.State)
	}

	// 4. Exact matching (machineID, runnerInstanceID, attemptID, decisionId) succeeds!
	_, err = s.IngestOutboxBatch(ctx, "hub-node", "run_b8", protocol.AttemptOutboxBatchFrame{
		AttemptID: cr1.Attempt.AttemptID, LeaseToken: cr1.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 3, EventID: "b8_ok", FrameType: protocol.OutboxFrameApprovalAck, Payload: map[string]any{
				"approvalId": "apr_b8", "decisionId": dec.Approval.DecisionID, "optionId": "allow_once",
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ap, _ = s.GetApproval(ctx, "apr_b8")
	if ap.State != protocol.ApprovalConsumedAllowed {
		t.Fatalf("exact matching ACK failed to consume approval: %s", ap.State)
	}
}

func TestB9_B10_SteerVsCancelAndLeasedCancelPersistence(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Part A: Steer intent triggers AutoConsumeQueue + SteerInputID on cancelled; normal cancel keeps queue paused.
	cr, _ := s.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: "proj-a", Goal: "steer test", MachineID: "hub-node", Adapter: protocol.AdapterAgy,
		CreateAttempt: true, LeaseTTLMs: 45000,
	})
	tid := cr.Task.TaskID
	att1 := cr.Attempt
	if err := s.BindAttemptRunnerInstance(ctx, att1.AttemptID, "run_b9"); err != nil {
		t.Fatal(err)
	}

	// Atomic followup while busy -> enqueues
	f1, err := s.SubmitFollowupAtomic(ctx, store.SubmitFollowupParams{
		TaskID: tid, Body: "msg1", Mode: protocol.FollowupModeQueue,
		MachineID: "hub-node", Adapter: protocol.AdapterAgy, ActorID: "human", LeaseTTLMs: 45000,
	})
	if err != nil || f1.PendingInput == nil || f1.Attempt != nil {
		t.Fatalf("expected queued while busy, got %+v err=%v", f1, err)
	}
	f2, err := s.SubmitFollowupAtomic(ctx, store.SubmitFollowupParams{
		TaskID: tid, Body: "msg2-steer", Mode: protocol.FollowupModeQueue,
		MachineID: "hub-node", Adapter: protocol.AdapterAgy, ActorID: "human", LeaseTTLMs: 45000,
	})
	if err != nil || f2.PendingInput == nil {
		t.Fatal(err)
	}
	// Steer f2 -> persists cancel_reason_kind='steer' and steer_input_id=f2
	_, cancelAtt, _, err := s.SteerPendingInput(ctx, tid, f2.PendingInput.InputID, "human")
	if err != nil || cancelAtt == nil || cancelAtt.AttemptID != att1.AttemptID {
		t.Fatalf("SteerPendingInput failed: att=%+v err=%v", cancelAtt, err)
	}
	// When att1 reaches cancelled -> AutoConsumeQueue=true and SteerInputID=f2!
	resSteer, err := s.IngestOutboxBatch(ctx, "hub-node", "run_b9", protocol.AttemptOutboxBatchFrame{
		AttemptID: att1.AttemptID, LeaseToken: att1.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 1, EventID: "b9_steer_cancel", FrameType: protocol.OutboxFrameState, Payload: map[string]any{"state": "cancelled"}},
		},
	})
	if err != nil || !resSteer.AutoConsumeQueue || resSteer.SteerInputID != f2.PendingInput.InputID {
		t.Fatalf("expected steer cancelled to auto-consume %s, got %+v err=%v", f2.PendingInput.InputID, resSteer, err)
	}
	popped, att2, _, err := s.ConsumeNextPendingInput(ctx, store.ConsumeNextPendingInputParams{
		TaskID: tid, PreferredInputID: resSteer.SteerInputID, MachineID: "hub-node", Adapter: protocol.AdapterAgy, LeaseTTLMs: 45000,
	})
	if err != nil || popped == nil || popped.InputID != f2.PendingInput.InputID || att2 == nil {
		t.Fatalf("expected steer consume to pop f2, got popped=%+v att=%+v err=%v", popped, att2, err)
	}

	// Part B (B10): Cancel dispatched leased att2 with forceImmediate=false -> persists cancel_requested=1,
	// returned in RenewMachineLeases & ReconcileRunnerHello AbortAttemptIDs, and when cancelled does NOT auto-consume f1!
	ok, err := s.TryBindAttemptDispatched(ctx, att2.AttemptID, "run_b9")
	if err != nil || !ok {
		t.Fatalf("TryBindAttemptDispatched failed: ok=%v err=%v", ok, err)
	}
	cancelRes, err := s.CancelTaskActiveAttempt(ctx, tid, "human", "human stop", false)
	if err != nil || cancelRes.Attempt.AttemptID == "" || cancelRes.Attempt.State != protocol.AttemptLeased {
		t.Fatalf("expected leased cancel to stay leased with cancel_requested=1 until runner confirms, got %+v err=%v", cancelRes, err)
	}
	if !cancelRes.Attempt.CancelRequested {
		t.Fatalf("expected CancelRequested=true persisted on leased attempt")
	}
	cancelIDs, err := s.RenewMachineLeases(ctx, "hub-node", "run_b9", []protocol.LiveAttemptLease{
		{AttemptID: att2.AttemptID, LeaseToken: att2.LeaseToken},
	}, nil, nil, time.Now().UnixMilli(), 45000)
	if err != nil || len(cancelIDs) != 1 || cancelIDs[0] != att2.AttemptID {
		t.Fatalf("expected RenewMachineLeases to return cancelAttemptIDs=[%s], got %v err=%v", att2.AttemptID, cancelIDs, err)
	}
	recHello, err := s.ReconcileRunnerHello(ctx, protocol.RunnerHelloFrame{
		MachineID: "hub-node", RunnerInstanceID: "run_b9",
		LiveAttempts: []protocol.LiveAttemptInfo{{AttemptID: att2.AttemptID, LeaseToken: att2.LeaseToken, State: protocol.AttemptLeased}},
	}, 45000)
	if err != nil || len(recHello.Reconcile.AbortAttemptIDs) != 1 || recHello.Reconcile.AbortAttemptIDs[0] != att2.AttemptID {
		t.Fatalf("expected ReconcileRunnerHello AbortAttemptIDs=[%s], got %+v err=%v", att2.AttemptID, recHello.Reconcile.AbortAttemptIDs, err)
	}
	resHumanCancel, err := s.IngestOutboxBatch(ctx, "hub-node", "run_b9", protocol.AttemptOutboxBatchFrame{
		AttemptID: att2.AttemptID, LeaseToken: att2.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 1, EventID: "b9_human_cancel", FrameType: protocol.OutboxFrameState, Payload: map[string]any{"state": "cancelled"}},
		},
	})
	if err != nil || resHumanCancel.AutoConsumeQueue {
		t.Fatalf("human cancel MUST NOT auto-consume queued f1, got %+v err=%v", resHumanCancel, err)
	}
}

func TestB11_NeedsVerificationGateAndAnnotationTransfer(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mirrorPath, workRepo, baseSHA := setupRealMirror(t, s, "proj-a")
	head1 := pushMirrorCommit(t, mirrorPath, workRepo, "dlv_b11_1", "package main\n// b11\nfunc main() {}\n")

	cr, _ := s.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: "proj-a", Goal: "original goal", MachineID: "hub-node", Adapter: protocol.AdapterAgy,
		CreateAttempt: true, LeaseTTLMs: 45000,
	})
	tid := cr.Task.TaskID
	_, _ = s.IngestOutboxBatch(ctx, "hub-node", "run_b11", protocol.AttemptOutboxBatchFrame{
		AttemptID: cr.Attempt.AttemptID, LeaseToken: cr.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 1, EventID: "b11_d1", FrameType: protocol.OutboxFrameDeliveryPublished, Payload: map[string]any{
				"deliveryId": "dlv_b11_1", "baseSha": baseSHA, "headSha": head1, "summary": "v1",
			}},
			{OutboxSeq: 2, EventID: "b11_s1", FrameType: protocol.OutboxFrameState, Payload: map[string]any{
				"state": "succeeded", "completionSource": "explicit_deliver",
			}},
		},
	})
	ann, _, _, _ := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		DeliveryID: "dlv_b11_1", Source: protocol.AnnotationSourceDiffDrawer,
		Anchor: protocol.AnnotationAnchor{Path: "main.go", StartLine: 1}, Body: "Check error handling",
	})
	disp, err := s.DispatchAnnotations(ctx, store.DispatchAnnotationsParams{
		TaskID: tid, AnnotationIDs: []string{ann.AnnotationID}, MachineID: "hub-node", Adapter: protocol.AdapterAgy,
	})
	if err != nil || disp.Attempt == nil {
		t.Fatal(err)
	}
	attNV := disp.Attempt
	// Transition attNV to needs_verification (leaving ann stranded in queued on attNV!)
	_, err = s.IngestOutboxBatch(ctx, "hub-node", "run_b11", protocol.AttemptOutboxBatchFrame{
		AttemptID: attNV.AttemptID, LeaseToken: attNV.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 1, EventID: "b11_nv", FrameType: protocol.OutboxFrameState, Payload: map[string]any{
				"state": "needs_verification", "completionSource": "quiet_timeout",
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Enqueue a pending input while idle -> ConsumeNextPendingInput MUST refuse to pop while needs_verification!
	_, _, err = s.EnqueuePendingInput(ctx, tid, protocol.PendingInputFollowup, protocol.PendingSourceHuman, "queued msg", nil, "human")
	if err != nil {
		t.Fatal(err)
	}
	poppedNV, attNVPop, _, err := s.ConsumeNextPendingInput(ctx, store.ConsumeNextPendingInputParams{
		TaskID: tid, MachineID: "hub-node", Adapter: protocol.AdapterAgy,
	})
	if err != nil || poppedNV != nil || attNVPop != nil {
		t.Fatalf("expected ConsumeNextPendingInput blocked by needs_verification (nil, nil, nil), got popped=%+v att=%+v err=%v", poppedNV, attNVPop, err)
	}
	// Idle SubmitFollowupAtomic without verifiedSite MUST return ErrVerificationRequired!
	if _, err := s.SubmitFollowupAtomic(ctx, store.SubmitFollowupParams{
		TaskID: tid, Body: "bypass", Mode: protocol.FollowupModeQueue, MachineID: "hub-node", Adapter: protocol.AdapterAgy, ActorID: "human",
	}); !errors.Is(err, store.ErrVerificationRequired) {
		t.Fatalf("expected idle SubmitFollowupAtomic blocked by needs_verification, got %v", err)
	}
	// DispatchAnnotations with wrong VerifiedAttemptID MUST return ErrVerificationRequired!
	ann2, _, _, _ := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		DeliveryID: "dlv_b11_1", Source: protocol.AnnotationSourceDiffDrawer,
		Anchor: protocol.AnnotationAnchor{Path: "main.go", StartLine: 2}, Body: "Second note",
	})
	if _, err := s.DispatchAnnotations(ctx, store.DispatchAnnotationsParams{
		TaskID: tid, AnnotationIDs: []string{ann2.AnnotationID}, MachineID: "hub-node", Adapter: protocol.AdapterAgy,
		VerifiedSite: true, VerifiedAttemptID: "att_wrong",
	}); !errors.Is(err, store.ErrVerificationRequired) {
		t.Fatalf("expected DispatchAnnotations with mismatched verifiedAttemptId blocked, got %v", err)
	}

	// Verified retry targeting attNV MUST succeed, transfer stranded queued annotation, and preserve original prompt!
	_, retryAtt, _, err := s.CreateNextAttempt(ctx, store.CreateNextAttemptParams{
		TaskID: tid, MachineID: "hub-node", Adapter: protocol.AdapterAgy,
		TriggerKind: protocol.TriggerRetry, VerifiedSite: true, VerifiedAttemptID: attNV.AttemptID, ActorID: "human",
	})
	if err != nil {
		t.Fatalf("verified retry failed: %v", err)
	}
	var trigAtt, status string
	_ = s.ReadDB().QueryRowContext(ctx, `SELECT status, triggered_attempt_id FROM annotations WHERE annotation_id = ?`, ann.AnnotationID).Scan(&status, &trigAtt)
	if status != string(protocol.AnnotationQueued) || trigAtt != retryAtt.AttemptID {
		t.Fatalf("expected stranded annotation transferred to retryAtt %s, got status=%s trig=%s", retryAtt.AttemptID, status, trigAtt)
	}
	if retryAtt.TriggerPayloadJSON == "" || retryAtt.TriggerPayloadJSON == "{}" {
		t.Fatalf("expected retry attempt to preserve original trigger payload, got %q", retryAtt.TriggerPayloadJSON)
	}
}

func TestB12_B13_MirrorAndBlobVerificationAndDuplicateIdempotency(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mirrorPath, workRepo, baseSHA := setupRealMirror(t, s, "proj-a")
	headOK := pushMirrorCommit(t, mirrorPath, workRepo, "dlv_b12_ok", "package main\n// b12\nfunc main() {}\n")

	cr, _ := s.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: "proj-a", Goal: "verify delivery", MachineID: "hub-node", Adapter: protocol.AdapterAgy,
		CreateAttempt: true, LeaseTTLMs: 45000,
	})

	// 1. Missing ref / wrong SHA MUST fail with ErrDeliveryVerifyFailed
	_, err := s.IngestOutboxBatch(ctx, "hub-node", "run_b12", protocol.AttemptOutboxBatchFrame{
		AttemptID: cr.Attempt.AttemptID, LeaseToken: cr.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 1, EventID: "b12_badref", FrameType: protocol.OutboxFrameDeliveryPublished, Payload: map[string]any{
				"deliveryId": "dlv_nonexistent", "baseSha": baseSHA, "headSha": headOK, "summary": "bad",
			}},
		},
	})
	if !errors.Is(err, store.ErrDeliveryVerifyFailed) {
		t.Fatalf("expected ErrDeliveryVerifyFailed for missing mirror ref, got %v", err)
	}

	// 2. Missing blob MUST fail with ErrDeliveryVerifyFailed
	blobBytes := []byte("binary-attachment-content")
	sum := sha256.Sum256(blobBytes)
	blobSHA := hex.EncodeToString(sum[:])
	_, err = s.IngestOutboxBatch(ctx, "hub-node", "run_b12", protocol.AttemptOutboxBatchFrame{
		AttemptID: cr.Attempt.AttemptID, LeaseToken: cr.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 1, EventID: "b12_badblob", FrameType: protocol.OutboxFrameDeliveryPublished, Payload: map[string]any{
				"deliveryId": "dlv_b12_ok", "baseSha": baseSHA, "headSha": headOK, "summary": "with missing blob",
				"blobs": []map[string]any{{"path": "report.bin", "sha256": blobSHA, "size": len(blobBytes)}},
			}},
		},
	})
	if !errors.Is(err, store.ErrBlobMismatch) {
		t.Fatalf("expected ErrBlobMismatch for missing blob, got %v", err)
	}

	// 3. Persist blob on disk + DB -> delivery_published succeeds and increments ref_count!
	blobPath := filepath.Join(t.TempDir(), blobSHA)
	if err := os.WriteFile(blobPath, blobBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.PutBlobRecord(ctx, blobSHA, int64(len(blobBytes)), blobPath); err != nil {
		t.Fatal(err)
	}
	resOK, err := s.IngestOutboxBatch(ctx, "hub-node", "run_b12", protocol.AttemptOutboxBatchFrame{
		AttemptID: cr.Attempt.AttemptID, LeaseToken: cr.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 1, EventID: "b12_good", FrameType: protocol.OutboxFrameDeliveryPublished, Payload: map[string]any{
				"deliveryId": "dlv_b12_ok", "baseSha": baseSHA, "headSha": headOK, "summary": "verified",
				"blobs": []map[string]any{{"path": "report.bin", "sha256": blobSHA, "size": len(blobBytes)}},
			}},
		},
	})
	if err != nil || resOK.AckedOutboxSeq != 1 {
		t.Fatalf("expected verified delivery_published to succeed, got %+v err=%v", resOK, err)
	}
	blobRec, err := s.GetBlobRecord(ctx, blobSHA)
	if err != nil || blobRec.RefCount != 1 {
		t.Fatalf("expected blob ref_count=1, got %+v err=%v", blobRec, err)
	}

	// 4. B13: Resolve inbox card, then send DUPLICATE delivery_published with same (delivery_id, head_sha) ->
	// MUST be idempotent: NO duplicate event, inbox stays resolved, ref_count stays 1!
	if _, _, err := s.CompleteDeliveryReview(ctx, "dlv_b12_ok", "human"); err != nil {
		t.Fatal(err)
	}
	resDup, err := s.IngestOutboxBatch(ctx, "hub-node", "run_b12", protocol.AttemptOutboxBatchFrame{
		AttemptID: cr.Attempt.AttemptID, LeaseToken: cr.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 2, EventID: "b13_dup", FrameType: protocol.OutboxFrameDeliveryPublished, Payload: map[string]any{
				"deliveryId": "dlv_b12_ok", "baseSha": baseSHA, "headSha": headOK, "summary": "verified retry",
				"blobs": []map[string]any{{"path": "report.bin", "sha256": blobSHA, "size": len(blobBytes)}},
			}},
		},
	})
	if err != nil || resDup.AckedOutboxSeq != 2 {
		t.Fatalf("duplicate delivery_published should be idempotent, got %+v err=%v", resDup, err)
	}
	var inboxState string
	_ = s.ReadDB().QueryRowContext(ctx, `SELECT state FROM inbox_items WHERE inbox_id = 'inb:dlv:dlv_b12_ok'`).Scan(&inboxState)
	if inboxState != string(protocol.InboxResolved) {
		t.Fatalf("duplicate delivery_published illegally reopened resolved inbox card: %s", inboxState)
	}
	blobRec, _ = s.GetBlobRecord(ctx, blobSHA)
	if blobRec.RefCount != 1 {
		t.Fatalf("duplicate delivery_published incremented blob ref_count to %d", blobRec.RefCount)
	}
}

func TestB14_S2_S4_S7_S8_StoreInvariants(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// B14: BindScratchTaskToProject locks old agent_sessions and sets required_capability
	crScr, err := s.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: protocol.ScratchProjectID, Goal: "scratch idea", MachineID: "hub-node",
		Adapter: protocol.AdapterAgy, CreateAttempt: true, LeaseTTLMs: 45000,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Finish scratch attempt so bind is allowed
	_, _ = s.IngestOutboxBatch(ctx, "hub-node", "run_scr", protocol.AttemptOutboxBatchFrame{
		AttemptID: crScr.Attempt.AttemptID, LeaseToken: crScr.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 1, EventID: "scr_done", FrameType: protocol.OutboxFrameState, Payload: map[string]any{
				"state": "succeeded", "stopReason": "end_turn", "providerSessionId": "sid_scratch_1", "worktreePath": "/tmp/scratch",
			}},
		},
	})
	if _, _, err := s.BindScratchTaskToProject(ctx, crScr.Task.TaskID, "proj-a", "human"); err != nil {
		t.Fatalf("BindScratchTaskToProject failed: %v", err)
	}
	var sessLocked int
	var sessState string
	_ = s.ReadDB().QueryRowContext(ctx, `SELECT locked, runtime_state FROM agent_sessions WHERE wb_session_id = ?`, crScr.Attempt.WBSessionID).Scan(&sessLocked, &sessState)
	if sessLocked != 1 || sessState != string(protocol.SessionDead) {
		t.Fatalf("expected old scratch agent_session locked & dead after bind, got locked=%d state=%s", sessLocked, sessState)
	}

	// S2: Attempt freezes DesiredConfig, RequiredResources, and Anchor independently of later project edits
	crFrozen, err := s.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: "proj-a", Goal: "frozen config", MachineID: "hub-node", Adapter: protocol.AdapterAgy,
		DesiredConfig:     map[string]string{"model": "gemini-4-neon", "thought_level": "low"},
		RequiredResources: []string{"xcode_sim"},
		Anchor:            &protocol.ProjectAnchorConfig{Machine: "hub-node", RepoPath: "/orig/repo", WorktreeRoot: "/orig/wt"},
		CreateAttempt:     true, LeaseTTLMs: 45000,
	})
	if err != nil {
		t.Fatal(err)
	}
	attF, err := s.GetAttemptRecord(ctx, crFrozen.Attempt.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attF.DesiredConfig["thought_level"] != "low" || len(attF.RequiredResources) != 1 || attF.Anchor == nil || attF.Anchor.RepoPath != "/orig/repo" {
		t.Fatalf("attempt failed to persist frozen config/resources/anchor: %+v", attF)
	}

	// S7: Board pagination with independent latestSeq and hasMore
	for i := 1; i <= 5; i++ {
		if _, _, err := s.PostBoardMessage(ctx, crFrozen.Task.TaskID, "human", "", "msg", ""); err != nil {
			t.Fatal(err)
		}
	}
	p1, err := s.GetTaskBoardPage(ctx, crFrozen.Task.TaskID, 0, 2)
	if err != nil || len(p1.Messages) != 2 || p1.LatestSeq != 5 || !p1.HasMore || p1.Messages[0].Seq != 4 || p1.Messages[1].Seq != 5 {
		t.Fatalf("unexpected board page 1: %+v err=%v", p1, err)
	}
	p2, err := s.GetTaskBoardPage(ctx, crFrozen.Task.TaskID, p1.Messages[0].Seq, 2)
	if err != nil || len(p2.Messages) != 2 || p2.LatestSeq != 5 || !p2.HasMore || p2.Messages[0].Seq != 2 || p2.Messages[1].Seq != 3 {
		t.Fatalf("unexpected board page 2: %+v err=%v", p2, err)
	}
	p3, err := s.GetTaskBoardPage(ctx, crFrozen.Task.TaskID, p2.Messages[0].Seq, 2)
	if err != nil || len(p3.Messages) != 1 || p3.LatestSeq != 5 || p3.HasMore || p3.Messages[0].Seq != 1 {
		t.Fatalf("unexpected board page 3: %+v err=%v", p3, err)
	}

	// S8: AttemptView.PromptText populated in timeline
	tl, err := s.GetTaskTimeline(ctx, crFrozen.Task.TaskID, 0, false)
	if err != nil || len(tl.Attempts) != 1 || tl.Attempts[0].PromptText != "frozen config" {
		t.Fatalf("expected AttemptView.PromptText='frozen config', got %+v err=%v", tl.Attempts, err)
	}

	// S4: BuildPABrief exact totals + ActiveTasks vs BlockedTasks separation (needs_verification in blocked, not active)
	_, _ = s.IngestOutboxBatch(ctx, "hub-node", "run_nv", protocol.AttemptOutboxBatchFrame{
		AttemptID: crFrozen.Attempt.AttemptID, LeaseToken: crFrozen.Attempt.LeaseToken,
		Items: []protocol.OutboxItem{
			{OutboxSeq: 1, EventID: "s4_nv", FrameType: protocol.OutboxFrameState, Payload: map[string]any{"state": "needs_verification"}},
		},
	})
	// Also seed one running active task
	_, _ = s.CreateTask(ctx, store.CreateTaskParams{
		ProjectID: "proj-a", Goal: "active work", MachineID: "mac-node", Adapter: protocol.AdapterAgy,
		CreateAttempt: true, LeaseTTLMs: 45000,
	})
	// Set adapters_json on hub-node to verify S4 exposes Machines[].Adapters
	_ = s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE machines SET adapters_json = '["agy"]' WHERE machine_id = 'hub-node'`)
		return err
	})
	brief, err := s.BuildPABrief(ctx, 24, "proj-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if brief.TotalBlockedTaskCount < 1 || brief.TotalActiveTaskCount < 1 {
		t.Fatalf("unexpected brief totals: active=%d blocked=%d", brief.TotalActiveTaskCount, brief.TotalBlockedTaskCount)
	}
	foundBlocked := false
	for _, bt := range brief.BlockedTasks {
		if bt.TaskID == crFrozen.Task.TaskID {
			foundBlocked = true
		}
	}
	for _, at := range brief.ActiveTasks {
		if at.TaskID == crFrozen.Task.TaskID {
			t.Fatalf("needs_verification task %s must NOT appear in ActiveTasks", crFrozen.Task.TaskID)
		}
	}
	if !foundBlocked {
		t.Fatalf("needs_verification task %s missing from BlockedTasks: %+v", crFrozen.Task.TaskID, brief.BlockedTasks)
	}
	foundAdapters := false
	for _, m := range brief.Machines {
		if m.MachineID == "hub-node" && len(m.Adapters) == 1 && m.Adapters[0] == protocol.AdapterAgy {
			foundAdapters = true
		}
	}
	if !foundAdapters {
		t.Fatalf("expected hub-node adapters=[agy] in PA brief, got %+v", brief.Machines)
	}
}
