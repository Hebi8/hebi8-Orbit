package hub_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/hub"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

func TestHub_BoardIdempotencyIsolatedByRealBoardID(t *testing.T) {
	ctx := context.Background()
	_, st, ts, runnerToken := setupSubsessionHub(t)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/runner"
	conn := dialTestRunnerWS(t, ctx, wsURL, "hub-node", "run_idemp_1", runnerToken)
	defer conn.CloseNow()

	doRunnerReqWithIdemp := func(path, idempKey string, body any) (*http.Response, []byte) {
		hdr := map[string]string{
			"Authorization":     "Bearer " + runnerToken,
			"X-Orbit-Machine-Id": "hub-node",
		}
		if idempKey != "" {
			hdr["Idempotency-Key"] = idempKey
		}
		return doJSONReq(t, http.MethodPost, ts.URL+path, body, hdr)
	}

	createParentAndChild := func(prefix string) (string, string, string) {
		rp, bp := doJSONReq(t, http.MethodPost, ts.URL+"/api/projects/demo-app/tasks", protocol.CreateTaskRequest{
			Title: prefix + " Parent",
			Goal:  "coordinate",
		}, nil)
		if rp.StatusCode != http.StatusCreated {
			t.Fatalf("create parent %s: %d %s", prefix, rp.StatusCode, bp)
		}
		var parent protocol.CreateTaskResponse
		_ = json.Unmarshal(bp, &parent)
		_, _ = st.TryBindAttemptDispatched(ctx, parent.Attempt.AttemptID, "run_idemp_1")

		rc, bc := doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+parent.Task.TaskID+"/children", protocol.CreateChildTaskRequest{
			Title: prefix + " Child",
			Goal:  "work",
		}, nil)
		if rc.StatusCode != http.StatusCreated {
			t.Fatalf("create child %s: %d %s", prefix, rc.StatusCode, bc)
		}
		var child protocol.CreateTaskResponse
		_ = json.Unmarshal(bc, &child)
		_, _ = st.TryBindAttemptDispatched(ctx, child.Attempt.AttemptID, "run_idemp_1")
		return parent.Task.TaskID, child.Task.TaskID, child.Attempt.AttemptID
	}

	parentA, childA, attChildA := createParentAndChild("GroupA")
	parentB, childB, attChildB := createParentAndChild("GroupB")

	// 1. Two independent task groups post via Runner delegate using the SAME Idempotency-Key.
	rA, bA := doRunnerReqWithIdemp("/api/runner/delegate/board-post", "biz-sync-1", map[string]any{
		"parentTaskId":    childA,
		"parentAttemptId": attChildA,
		"body":            "Group A update",
	})
	if rA.StatusCode != http.StatusCreated {
		t.Fatalf("group A runner board-post status=%d body=%s", rA.StatusCode, bA)
	}
	var msgA protocol.BoardMessage
	_ = json.Unmarshal(bA, &msgA)

	rB, bB := doRunnerReqWithIdemp("/api/runner/delegate/board-post", "biz-sync-1", map[string]any{
		"parentTaskId":    childB,
		"parentAttemptId": attChildB,
		"body":            "Group B update",
	})
	if rB.StatusCode != http.StatusCreated {
		t.Fatalf("group B runner board-post status=%d body=%s", rB.StatusCode, bB)
	}
	var msgB protocol.BoardMessage
	_ = json.Unmarshal(bB, &msgB)

	if msgA.TaskID != parentA || msgB.TaskID != parentB || msgA.MessageID == msgB.MessageID {
		t.Fatalf("idempotency key leaked across boards: msgA=%+v msgB=%+v", msgA, msgB)
	}
	if msgB.Body != "Group B update" {
		t.Fatalf("expected Group B message body, got %+v", msgB)
	}

	// 2. Replaying the same Idempotency-Key on the SAME board (Group A) returns the existing message.
	rAReplay, bAReplay := doRunnerReqWithIdemp("/api/runner/delegate/board-post", "biz-sync-1", map[string]any{
		"parentTaskId":    childA,
		"parentAttemptId": attChildA,
		"body":            "Group A duplicate retry",
	})
	if rAReplay.StatusCode != http.StatusCreated {
		t.Fatalf("group A replay status=%d body=%s", rAReplay.StatusCode, bAReplay)
	}
	var msgAReplay protocol.BoardMessage
	_ = json.Unmarshal(bAReplay, &msgAReplay)
	if msgAReplay.MessageID != msgA.MessageID || msgAReplay.Body != "Group A update" {
		t.Fatalf("expected idempotent replay of msgA (%s), got %+v", msgA.MessageID, msgAReplay)
	}

	// 3. Two independent boards posting via Web REST API with identical Idempotency-Key also stay isolated.
	rWA, bWA := doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+parentA+"/board/messages", protocol.PostBoardMessageRequest{
		Body: "Web A",
	}, map[string]string{"Idempotency-Key": "web-same-1"})
	rWB, bWB := doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+parentB+"/board/messages", protocol.PostBoardMessageRequest{
		Body: "Web B",
	}, map[string]string{"Idempotency-Key": "web-same-1"})
	if rWA.StatusCode != http.StatusCreated || rWB.StatusCode != http.StatusCreated {
		t.Fatalf("web board posts failed: A=%d(%s) B=%d(%s)", rWA.StatusCode, bWA, rWB.StatusCode, bWB)
	}
	var wMsgA, wMsgB protocol.BoardMessage
	_ = json.Unmarshal(bWA, &wMsgA)
	_ = json.Unmarshal(bWB, &wMsgB)
	if wMsgA.MessageID == wMsgB.MessageID || wMsgA.TaskID != parentA || wMsgB.TaskID != parentB {
		t.Fatalf("web board idempotency leaked across boards: A=%+v B=%+v", wMsgA, wMsgB)
	}

	// 4. Raw keys "sync" vs "board:sync" on the SAME board MUST NOT collide.
	rK1, bK1 := doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+parentA+"/board/messages", protocol.PostBoardMessageRequest{
		Body: "key sync",
	}, map[string]string{"Idempotency-Key": "sync"})
	rK2, bK2 := doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+parentA+"/board/messages", protocol.PostBoardMessageRequest{
		Body: "key board:sync",
	}, map[string]string{"Idempotency-Key": "board:sync"})
	if rK1.StatusCode != http.StatusCreated || rK2.StatusCode != http.StatusCreated {
		t.Fatalf("board key distinction failed: k1=%d(%s) k2=%d(%s)", rK1.StatusCode, bK1, rK2.StatusCode, bK2)
	}
	var mK1, mK2 protocol.BoardMessage
	_ = json.Unmarshal(bK1, &mK1)
	_ = json.Unmarshal(bK2, &mK2)
	if mK1.MessageID == mK2.MessageID || mK1.Body == mK2.Body {
		t.Fatalf("raw key 'sync' collided with 'board:sync': m1=%+v m2=%+v", mK1, mK2)
	}
}

func TestHub_BoardMentionsUnifiedDeliveryChildParentAllAndTimelineCount(t *testing.T) {
	ctx := context.Background()
	_, st, ts, runnerToken := setupSubsessionHub(t)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/runner"
	conn := dialTestRunnerWS(t, ctx, wsURL, "hub-node", "run_men_1", runnerToken)
	defer conn.CloseNow()

	doRunnerReq := func(path string, body any) (*http.Response, []byte) {
		return doJSONReq(t, http.MethodPost, ts.URL+path, body, map[string]string{
			"Authorization":     "Bearer " + runnerToken,
			"X-Orbit-Machine-Id": "hub-node",
		})
	}

	// Create top-level task with NO children yet, post a board message, and verify timeline exposes BoardMessageCount=1.
	rSolo, bSolo := doJSONReq(t, http.MethodPost, ts.URL+"/api/projects/demo-app/tasks", protocol.CreateTaskRequest{
		Title: "Solo With Board",
		Goal:  "solo",
	}, nil)
	if rSolo.StatusCode != http.StatusCreated {
		t.Fatalf("create solo task: %d %s", rSolo.StatusCode, bSolo)
	}
	var solo protocol.CreateTaskResponse
	_ = json.Unmarshal(bSolo, &solo)
	rSoloPost, _ := doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+solo.Task.TaskID+"/board/messages", protocol.PostBoardMessageRequest{
		Body: "note before any child exists",
	}, nil)
	if rSoloPost.StatusCode != http.StatusCreated {
		t.Fatalf("solo board post failed: %d", rSoloPost.StatusCode)
	}
	_, soloTLRaw := doJSONReq(t, http.MethodGet, ts.URL+"/api/tasks/"+solo.Task.TaskID+"/timeline", nil, nil)
	var soloTL protocol.TaskTimelineResponse
	_ = json.Unmarshal(soloTLRaw, &soloTL)
	if soloTL.BoardMessageCount != 1 {
		t.Fatalf("expected timeline BoardMessageCount=1 for cold-start top-level session, got %d", soloTL.BoardMessageCount)
	}

	// Create parent + 3 children (one of which will be archived).
	rP, bP := doJSONReq(t, http.MethodPost, ts.URL+"/api/projects/demo-app/tasks", protocol.CreateTaskRequest{
		Title: "Main Coordinator",
		Goal:  "coordinate",
	}, nil)
	if rP.StatusCode != http.StatusCreated {
		t.Fatalf("create parent: %d %s", rP.StatusCode, bP)
	}
	var parent protocol.CreateTaskResponse
	_ = json.Unmarshal(bP, &parent)
	parentID, attParent := parent.Task.TaskID, parent.Attempt.AttemptID
	_, _ = st.TryBindAttemptDispatched(ctx, attParent, "run_men_1")

	spawnChild := func(title string) (string, string) {
		r, b := doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+parentID+"/children", protocol.CreateChildTaskRequest{
			Title: title,
			Goal:  "work",
		}, nil)
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("spawn child %s: %d %s", title, r.StatusCode, b)
		}
		var out protocol.CreateTaskResponse
		_ = json.Unmarshal(b, &out)
		_, _ = st.TryBindAttemptDispatched(ctx, out.Attempt.AttemptID, "run_men_1")
		return out.Task.TaskID, out.Attempt.AttemptID
	}

	child1, attC1 := spawnChild("Backend API")
	child2, attC2 := spawnChild("Frontend UI")
	childArch, attArch := spawnChild("Archived Worker")
	archAttRec, err := st.GetAttemptRecord(ctx, attArch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.IngestOutboxBatch(ctx, "hub-node", "run_men_1", protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  archAttRec.AttemptID,
		LeaseToken: archAttRec.LeaseToken,
		Items: []protocol.OutboxItem{
			{
				OutboxSeq: 1,
				EventID:   "evt_arch_done",
				FrameType: protocol.OutboxFrameState,
				Payload: map[string]any{
					"state":            string(protocol.AttemptSucceeded),
					"completionSource": string(protocol.CompletionPromptEndTurn),
				},
				TS: time.Now().UnixMilli(),
			},
		},
	}); err != nil {
		t.Fatalf("finish childArch attempt failed: %v", err)
	}
	rArch, bArch := doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+childArch+"/archive", protocol.ArchiveTaskRequest{}, nil)
	if rArch.StatusCode != http.StatusOK {
		t.Fatalf("archive child failed: %d %s", rArch.StatusCode, bArch)
	}

	pendingBodies := func(taskID string) []string {
		list, err := st.ListPendingInputs(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(list))
		for _, pi := range list {
			out = append(out, pi.Body)
		}
		return out
	}

	// 1. Runner delegate: parent posts @child2 (by taskId) -> wakes/queues child2 only.
	r1, b1 := doRunnerReq("/api/runner/delegate/board-post", map[string]any{
		"parentTaskId":    parentID,
		"parentAttemptId": attParent,
		"targetMention":   child2,
		"body":            "runner-to-child2",
	})
	if r1.StatusCode != http.StatusCreated {
		t.Fatalf("parent->child2 board-post: %d %s", r1.StatusCode, b1)
	}
	if bodies := pendingBodies(child2); len(bodies) != 1 || !strings.Contains(bodies[0], "runner-to-child2") {
		t.Fatalf("expected child2 queued with runner-to-child2, got %+v", bodies)
	}
	if bodies := pendingBodies(parentID); len(bodies) != 0 {
		t.Fatalf("parent should not be queued by parent->child2, got %+v", bodies)
	}

	// 2. Runner delegate: child1 posts @parent -> wakes/queues parentID, NOT child1, and no bogus @parent native member.
	r2, b2 := doRunnerReq("/api/runner/delegate/board-post", map[string]any{
		"parentTaskId":    child1,
		"parentAttemptId": attC1,
		"targetMention":   "@parent",
		"body":            "child1-to-parent",
	})
	if r2.StatusCode != http.StatusCreated {
		t.Fatalf("child1->parent board-post: %d %s", r2.StatusCode, b2)
	}
	if bodies := pendingBodies(parentID); len(bodies) != 1 || !strings.Contains(bodies[0], "child1-to-parent") {
		t.Fatalf("expected parent queued with child1-to-parent, got %+v", bodies)
	}
	if bodies := pendingBodies(child1); len(bodies) != 0 {
		t.Fatalf("child1 should not self-queue, got %+v", bodies)
	}

	// 3. Runner delegate: parent posts @parent -> MUST NOT self-loop!
	rSelf, _ := doRunnerReq("/api/runner/delegate/board-post", map[string]any{
		"parentTaskId":    parentID,
		"parentAttemptId": attParent,
		"targetMention":   "@parent",
		"body":            "parent-self-ping",
	})
	if rSelf.StatusCode != http.StatusCreated {
		t.Fatalf("parent->parent status=%d", rSelf.StatusCode)
	}
	if bodies := pendingBodies(parentID); len(bodies) != 1 {
		t.Fatalf("parent @parent self-mention must not queue parent again, got %+v", bodies)
	}

	// 4. Runner delegate: child1 posts @all -> wakes parentID + child2 (excludes sender child1 and archived childArch).
	rAllRunner, bAllRunner := doRunnerReq("/api/runner/delegate/board-post", map[string]any{
		"parentTaskId":    child1,
		"parentAttemptId": attC1,
		"targetMention":   "@all",
		"body":            "broadcast-from-child1",
	})
	if rAllRunner.StatusCode != http.StatusCreated {
		t.Fatalf("child1 @all failed: %d %s", rAllRunner.StatusCode, bAllRunner)
	}
	if bodies := pendingBodies(parentID); len(bodies) != 2 || !strings.Contains(bodies[1], "broadcast-from-child1") {
		t.Fatalf("expected parent to receive @all from child1, got %+v", bodies)
	}
	if bodies := pendingBodies(child2); len(bodies) != 2 || !strings.Contains(bodies[1], "broadcast-from-child1") {
		t.Fatalf("expected child2 to receive @all from child1, got %+v", bodies)
	}
	if bodies := pendingBodies(child1); len(bodies) != 0 {
		t.Fatalf("sender child1 must be excluded from @all, got %+v", bodies)
	}
	if bodies := pendingBodies(childArch); len(bodies) != 0 {
		t.Fatalf("archived child must be excluded from @all, got %+v", bodies)
	}

	// 5. Web human posts @all -> wakes parentID, child1, AND child2 (each once).
	rAllWeb, bAllWeb := doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+parentID+"/board/messages", protocol.PostBoardMessageRequest{
		AuthorName:    "human",
		TargetMention: "all",
		Body:          "human-broadcast-all",
	}, nil)
	if rAllWeb.StatusCode != http.StatusCreated {
		t.Fatalf("web @all failed: %d %s", rAllWeb.StatusCode, bAllWeb)
	}
	if bodies := pendingBodies(parentID); len(bodies) != 3 || !strings.Contains(bodies[2], "human-broadcast-all") {
		t.Fatalf("expected parent to receive web @all, got %+v", bodies)
	}
	if bodies := pendingBodies(child1); len(bodies) != 1 || !strings.Contains(bodies[0], "human-broadcast-all") {
		t.Fatalf("expected child1 to receive web @all once, got %+v", bodies)
	}
	if bodies := pendingBodies(child2); len(bodies) != 3 || !strings.Contains(bodies[2], "human-broadcast-all") {
		t.Fatalf("expected child2 to receive web @all once, got %+v", bodies)
	}

	// 6. Web human posts @unknown-ghost -> MUST NOT accidentally queue parentID!
	rGhost, _ := doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+parentID+"/board/messages", protocol.PostBoardMessageRequest{
		AuthorName:    "human",
		TargetMention: "unknown-ghost-target",
		Body:          "should not wake parent",
	}, nil)
	if rGhost.StatusCode != http.StatusCreated {
		t.Fatalf("web @unknown status=%d", rGhost.StatusCode)
	}
	if bodies := pendingBodies(parentID); len(bodies) != 3 {
		t.Fatalf("unknown @mention must not wake parent session, got %+v", bodies)
	}

	// 7. Two same-titled idle children: mentioning dup2 by taskId records consumption cursor on dup2, NOT dup1.
	finishAttempt := func(attID, evID string) {
		rec, err := st.GetAttemptRecord(ctx, attID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.IngestOutboxBatch(ctx, "hub-node", "run_men_1", protocol.AttemptOutboxBatchFrame{
			Type:       protocol.RunnerMsgOutboxBatch,
			AttemptID:  rec.AttemptID,
			LeaseToken: rec.LeaseToken,
			Items: []protocol.OutboxItem{
				{
					OutboxSeq: 1,
					EventID:   evID,
					FrameType: protocol.OutboxFrameState,
					Payload: map[string]any{
						"state":            string(protocol.AttemptSucceeded),
						"completionSource": string(protocol.CompletionPromptEndTurn),
					},
					TS: time.Now().UnixMilli(),
				},
			},
		}); err != nil {
			t.Fatalf("finish attempt %s failed: %v", attID, err)
		}
	}
	dup1, attDup1 := spawnChild("Duplicate Worker")
	dup2, attDup2 := spawnChild("Duplicate Worker")
	finishAttempt(attDup1, "evt_dup1_done")
	finishAttempt(attDup2, "evt_dup2_done")

	rDup, bDup := doJSONReq(t, http.MethodPost, ts.URL+"/api/tasks/"+parentID+"/board/messages", protocol.PostBoardMessageRequest{
		AuthorName:    "human",
		TargetMention: dup2,
		Body:          "exact target dup2",
	}, nil)
	if rDup.StatusCode != http.StatusCreated {
		t.Fatalf("mention dup2 failed: %d %s", rDup.StatusCode, bDup)
	}
	var dupMsg protocol.BoardMessage
	_ = json.Unmarshal(bDup, &dupMsg)
	boardAfterDup, err := st.GetTaskBoard(ctx, parentID)
	if err != nil {
		t.Fatal(err)
	}
	var dup1Cursor, dup2Cursor int64
	for _, m := range boardAfterDup.Members {
		if m.WorkerTaskID == dup1 {
			dup1Cursor = m.LastConsumedSeq
		}
		if m.WorkerTaskID == dup2 {
			dup2Cursor = m.LastConsumedSeq
		}
	}
	if dup2Cursor != dupMsg.Seq || dup1Cursor != 0 {
		t.Fatalf("expected dup2 cursor=%d and dup1 cursor=0, got dup2=%d dup1=%d (members=%+v)", dupMsg.Seq, dup2Cursor, dup1Cursor, boardAfterDup.Members)
	}
	_ = attC2
}

func TestHub_PendingQueueUsesItemPersistedRouteAndRecordsBlock(t *testing.T) {
	ctx := context.Background()
	_, st, ts, runnerToken := setupSubsessionHub(t)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/runner"
	conn := dialTestRunnerWS(t, ctx, wsURL, "hub-node", "run_qroute_1", runnerToken)
	defer conn.CloseNow()

	// Create a dynamic project with 2 locations (LocA default, LocB secondary) on the same part.
	rootDir := t.TempDir()
	dirA := filepath.Join(rootDir, "loc-a")
	dirB := filepath.Join(rootDir, "loc-b")
	_ = os.MkdirAll(dirA, 0o755)
	_ = os.MkdirAll(dirB, 0o755)
	rAllow, bAllow := doJSONReq(t, http.MethodPatch, ts.URL+"/api/machines/hub-node", protocol.UpdateMachineRequest{
		AllowedRoots: []string{rootDir},
	}, nil)
	if rAllow.StatusCode != http.StatusOK {
		t.Fatalf("update allowedRoots: %d %s", rAllow.StatusCode, bAllow)
	}
	rProj, bProj := doJSONReq(t, http.MethodPost, ts.URL+"/api/projects", protocol.CreateProjectRequest{
		DisplayName: "MultiLoc Queue App",
		MachineID:   "hub-node",
		RepoPath:    dirA,
		AllowFolder: true,
	}, nil)
	if rProj.StatusCode != http.StatusCreated {
		t.Fatalf("create project: %d %s", rProj.StatusCode, bProj)
	}
	var projResp protocol.CreateProjectResponse
	_ = json.Unmarshal(bProj, &projResp)
	proj := projResp.Project
	partID := proj.Parts[0].PartID
	locA := proj.Parts[0].Locations[0].LocationID

	_, locBView, _, err := st.CreateProjectLocation(ctx, proj.ProjectID, partID, protocol.CreateProjectLocationRequest{
		MachineID: "hub-node",
		AbsPath:   dirB,
		Isolation: protocol.IsolationInPlace,
	}, nil, "", "human")
	if err != nil {
		t.Fatalf("add location B failed: %v", err)
	}
	locB := locBView.LocationID

	// Create task pinned to Location A.
	rTask, bTask := doJSONReq(t, http.MethodPost, ts.URL+"/api/projects/"+proj.ProjectID+"/tasks", protocol.CreateTaskRequest{
		Title:      "Queue Route Task",
		Goal:       "initial work on loc A",
		PartID:     partID,
		LocationID: locA,
	}, nil)
	if rTask.StatusCode != http.StatusCreated {
		t.Fatalf("create task: %d %s", rTask.StatusCode, bTask)
	}
	var created protocol.CreateTaskResponse
	_ = json.Unmarshal(bTask, &created)
	att1, err := st.GetAttemptRecord(ctx, created.Attempt.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.TryBindAttemptDispatched(ctx, att1.AttemptID, "run_qroute_1")

	// Queue a followup whose persisted route_json explicitly points to Location B.
	if _, err := st.SubmitFollowupAtomic(ctx, store.SubmitFollowupParams{
		TaskID:     created.Task.TaskID,
		Body:       "continue on loc B",
		Mode:       protocol.FollowupModeQueue,
		MachineID:  "hub-node",
		Adapter:    protocol.AdapterAgy,
		PartID:     partID,
		LocationID: locB,
		Isolation:  protocol.IsolationInPlace,
		Anchor: &protocol.ProjectAnchorConfig{
			Machine:  "hub-node",
			RepoPath: dirB,
		},
		ActorKind:  protocol.ActorHuman,
		ActorID:    "human",
		LeaseTTLMs: 45000,
	}); err != nil {
		t.Fatalf("queue followup with route B failed: %v", err)
	}

	// Delete Location A while task's stored location_id still points to Location A!
	if _, _, err := st.DeleteProjectLocation(ctx, proj.ProjectID, partID, locA, "human"); err != nil {
		t.Fatalf("delete locA failed: %v", err)
	}

	// Finish attempt 1 via WebSocket outbox -> Hub auto-consumes queued item using its persisted route B!
	now := time.Now().UnixMilli()
	outboxFrame, _ := json.Marshal(protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  att1.AttemptID,
		LeaseToken: att1.LeaseToken,
		Items: []protocol.OutboxItem{
			{
				OutboxSeq: 1,
				EventID:   "evt_qr_done_1",
				FrameType: protocol.OutboxFrameState,
				Payload: map[string]any{
					"state":            string(protocol.AttemptSucceeded),
					"completionSource": string(protocol.CompletionPromptEndTurn),
				},
				TS: now,
			},
		},
	})
	if err := conn.Write(ctx, 1, outboxFrame); err != nil {
		t.Fatalf("send outbox finish: %v", err)
	}

	deadline := time.Now().Add(4 * time.Second)
	var att2 *protocol.AttemptView
	for time.Now().Before(deadline) {
		tl, err := st.GetTaskTimeline(ctx, created.Task.TaskID, 0, false)
		if err == nil && len(tl.Attempts) >= 2 {
			a := tl.Attempts[1]
			att2 = &a
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if att2 == nil {
		t.Fatal("queued input pointing to Location B did NOT dispatch after deleting Location A")
	}
	if att2.LocationID != locB || att2.PartID != partID {
		t.Fatalf("expected attempt 2 dispatched to locB=%s part=%s, got loc=%s part=%s", locB, partID, att2.LocationID, att2.PartID)
	}

	// Now queue a 3rd input pinned to Location B, DELETE Location B (leaving zero usable locations),
	// and finish attempt 2 -> task MUST record a clear visible dispatch_blocked reason instead of silently spinning!
	att2Rec, err := st.GetAttemptRecord(ctx, att2.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.TryBindAttemptDispatched(ctx, att2Rec.AttemptID, "run_qroute_1")
	if _, err := st.SubmitFollowupAtomic(ctx, store.SubmitFollowupParams{
		TaskID:     created.Task.TaskID,
		Body:       "needs deleted loc B",
		Mode:       protocol.FollowupModeQueue,
		MachineID:  "hub-node",
		Adapter:    protocol.AdapterAgy,
		PartID:     partID,
		LocationID: locB,
		Isolation:  protocol.IsolationInPlace,
		Anchor: &protocol.ProjectAnchorConfig{
			Machine:  "hub-node",
			RepoPath: dirB,
		},
		ActorKind:  protocol.ActorHuman,
		ActorID:    "human",
		LeaseTTLMs: 45000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.DeleteProjectLocation(ctx, proj.ProjectID, partID, locB, "human"); err != nil {
		t.Fatalf("delete locB failed: %v", err)
	}

	outboxFrame2, _ := json.Marshal(protocol.AttemptOutboxBatchFrame{
		Type:       protocol.RunnerMsgOutboxBatch,
		AttemptID:  att2Rec.AttemptID,
		LeaseToken: att2Rec.LeaseToken,
		Items: []protocol.OutboxItem{
			{
				OutboxSeq: 1,
				EventID:   "evt_qr_done_2",
				FrameType: protocol.OutboxFrameState,
				Payload: map[string]any{
					"state":            string(protocol.AttemptSucceeded),
					"completionSource": string(protocol.CompletionPromptEndTurn),
				},
				TS: now + 100,
			},
		},
	})
	if err := conn.Write(ctx, 1, outboxFrame2); err != nil {
		t.Fatal(err)
	}

	deadline = time.Now().Add(4 * time.Second)
	sawBlocked := false
	for time.Now().Before(deadline) {
		tl, err := st.GetTaskTimeline(ctx, created.Task.TaskID, 0, false)
		if err == nil {
			for _, ev := range tl.Events {
				if ev.Kind == protocol.EventQueueUpdated && ev.Payload["action"] == "dispatch_blocked" {
					if reason, _ := ev.Payload["reason"].(string); strings.TrimSpace(reason) != "" {
						sawBlocked = true
						break
					}
				}
			}
			if sawBlocked && tl.Task.State == protocol.TaskStateWaitingResource {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !sawBlocked {
		t.Fatal("expected visible queue.updated(dispatch_blocked) reason when no usable location exists")
	}
	notices, err := st.ListTasksPendingQueueDrain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range notices {
		if n.TaskID == created.Task.TaskID {
			t.Fatalf("pending_queue_drain should be cleared after recording dispatch_blocked to prevent silent loop")
		}
	}

	// Verify waiting_reason is set while blocked, and deleting the blocked head item restores idle task to in_review & clears waiting_reason!
	blockedTask, err := st.GetTask(ctx, created.Task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if blockedTask.State != protocol.TaskStateWaitingResource || strings.TrimSpace(blockedTask.WaitingReason) == "" {
		t.Fatalf("expected waiting_resource with non-empty waitingReason, got state=%s reason=%q", blockedTask.State, blockedTask.WaitingReason)
	}
	pendingList, err := st.ListPendingInputs(ctx, created.Task.TaskID)
	if err != nil || len(pendingList) != 1 {
		t.Fatalf("expected 1 blocked pending input, got %+v (err=%v)", pendingList, err)
	}
	rDelQ, bDelQ := doJSONReq(t, http.MethodDelete, ts.URL+"/api/tasks/"+created.Task.TaskID+"/queue/"+pendingList[0].InputID, nil, nil)
	if rDelQ.StatusCode != http.StatusOK {
		t.Fatalf("delete blocked queue head failed: %d %s", rDelQ.StatusCode, bDelQ)
	}
	recoveredTask, err := st.GetTask(ctx, created.Task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredTask.State != protocol.TaskStateInReview || recoveredTask.WaitingReason != "" {
		t.Fatalf("expected idle task recovered to in_review with empty waitingReason after deleting blocked head, got state=%s reason=%q", recoveredTask.State, recoveredTask.WaitingReason)
	}

	// Verify 0-Part project still rejects an explicit deleted locationId rather than falling back!
	if _, _, err := st.DeleteProjectPart(ctx, proj.ProjectID, partID, "human"); err != nil {
		t.Fatalf("delete last part failed: %v", err)
	}
	rBadZero, bBadZero := doJSONReq(t, http.MethodPost, ts.URL+"/api/projects/"+proj.ProjectID+"/tasks", protocol.CreateTaskRequest{
		Title:      "Zero Part Explicit Loc",
		Goal:       "should reject deleted loc",
		LocationID: locB,
	}, nil)
	if rBadZero.StatusCode == http.StatusCreated {
		t.Fatalf("expected 0-part project with explicit deleted locationId to fail, got 201 %s", bBadZero)
	}
}

func TestHub_NetworkSettingsPatchAtomicWithConcurrentMachineDelete(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "config")
	dataDir := filepath.Join(tmp, "data")
	_ = os.MkdirAll(cfgDir, 0o755)
	_ = os.MkdirAll(dataDir, 0o755)

	machinesYAML := `schemaVersion: 1
machines:
  hub-node:
    displayName: "Local Hub"
    transport: local_ws
    capabilities: ["primary-dev"]
  aux-node:
    displayName: "Aux Worker"
    transport: remote_ws
    capabilities: ["linux-dev"]
`
	if err := os.WriteFile(filepath.Join(cfgDir, "machines.yaml"), []byte(machinesYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(dataDir, "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := protocol.HubConfig{
		SchemaVersion: 1,
		ListenAddr:    "127.0.0.1:0",
		DataDir:       dataDir,
		ConfigDir:     cfgDir,
		DevOpen:       true,
	}
	srv, err := hub.NewServer(ctx, hub.ServerOptions{
		HubConfig:   cfg,
		Store:       st,
		RyugiRunner: &fakeRyugiRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Concurrently fire PATCH /api/network/settings (including aux-node in dutyPriority)
	// alongside DELETE /api/machines/aux-node.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			wsName := "Concurrent LAN"
			r, _ := doJSONReq(t, http.MethodPatch, ts.URL+"/api/network/settings", protocol.UpdateNetworkSettingsRequest{
				WorkspaceName: &wsName,
				DutyPriority:  []string{"hub-node", "aux-node"},
			}, nil)
			if r.StatusCode != http.StatusOK {
				t.Errorf("PATCH /api/network/settings status=%d", r.StatusCode)
			}
		}
	}()
	go func() {
		defer wg.Done()
		time.Sleep(5 * time.Millisecond)
		r, b := doJSONReq(t, http.MethodDelete, ts.URL+"/api/machines/aux-node", nil, nil)
		if r.StatusCode != http.StatusOK {
			t.Errorf("DELETE /api/machines/aux-node status=%d body=%s", r.StatusCode, b)
		}
	}()
	wg.Wait()

	// Verify tombstone survived, dutyPriority excluded aux-node, and workspaceName persisted.
	dynNet, err := st.GetDynamicNetworkSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(dynNet.RemovedMachines, "aux-node") {
		t.Fatalf("concurrent PATCH clobbered removedMachines tombstone! got %+v", dynNet)
	}
	if slices.Contains(dynNet.DutyPriority, "aux-node") {
		t.Fatalf("removed machine aux-node leaked into dutyPriority: %+v", dynNet.DutyPriority)
	}
	if dynNet.WorkspaceName != "Concurrent LAN" {
		t.Fatalf("expected WorkspaceName='Concurrent LAN', got %q", dynNet.WorkspaceName)
	}
}

func TestHub_RepairSameIDPreservesRemoteWSOverStaleYAMLOnReloadAndBootstrap(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "config")
	dataDir := filepath.Join(tmp, "data")
	_ = os.MkdirAll(cfgDir, 0o755)
	_ = os.MkdirAll(dataDir, 0o755)

	machinesYAML := `schemaVersion: 1
machines:
  hub-node:
    displayName: "Local Hub"
    transport: local_ws
    capabilities: ["primary-dev"]
  deploy-vps:
    displayName: "Old Deploy VPS"
    transport: ssh_outbound
    sshAlias: old-vps
    capabilities: ["deploy-node"]
`
	machPath := filepath.Join(cfgDir, "machines.yaml")
	if err := os.WriteFile(machPath, []byte(machinesYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(dataDir, "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := protocol.HubConfig{
		SchemaVersion: 1,
		ListenAddr:    "127.0.0.1:0",
		DataDir:       dataDir,
		ConfigDir:     cfgDir,
		DevOpen:       true,
	}
	srv, err := hub.NewServer(ctx, hub.ServerOptions{
		HubConfig:   cfg,
		Store:       st,
		RyugiRunner: &fakeRyugiRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. Delete YAML ssh_outbound machine deploy-vps.
	rDel, bDel := doJSONReq(t, http.MethodDelete, ts.URL+"/api/machines/deploy-vps", nil, nil)
	if rDel.StatusCode != http.StatusOK {
		t.Fatalf("delete deploy-vps: %d %s", rDel.StatusCode, bDel)
	}

	// 2. Re-pair with the exact same ID "deploy-vps" as a real remote_ws runner.
	rCode, bCode := doJSONReq(t, http.MethodPost, ts.URL+"/api/pairing/codes", protocol.CreatePairingCodeRequest{
		Scope: protocol.PairingScopeRunner,
	}, nil)
	if rCode.StatusCode != http.StatusCreated {
		t.Fatalf("create pairing code: %d %s", rCode.StatusCode, bCode)
	}
	var pCode protocol.CreatePairingCodeResponse
	_ = json.Unmarshal(bCode, &pCode)

	rClaim, bClaim := doJSONReq(t, http.MethodPost, ts.URL+"/api/pairing/claim", protocol.ClaimPairingRequest{
		Code:       pCode.Code,
		WantRunner: true,
		Machine:    &protocol.ClaimMachineInfo{ProposedID: "deploy-vps", DisplayName: "Paired Remote Runner"},
	}, nil)
	if rClaim.StatusCode != http.StatusOK {
		t.Fatalf("claim pairing code: %d %s", rClaim.StatusCode, bClaim)
	}
	var claimed protocol.ClaimPairingResponse
	_ = json.Unmarshal(bClaim, &claimed)
	if claimed.MachineID != "deploy-vps" {
		t.Fatalf("expected reclaimed ID deploy-vps, got %q", claimed.MachineID)
	}

	// 3. Old machines.yaml (still containing deploy-vps: ssh_outbound) stays on disk; run ReloadConfig!
	if err := os.WriteFile(machPath, []byte(machinesYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ReloadConfig(ctx); err != nil {
		t.Fatalf("ReloadConfig failed: %v", err)
	}

	verifyPairedRemoteWS := func(label, baseURL string) {
		t.Helper()
		_, bBoot := doJSONReq(t, http.MethodGet, baseURL+"/api/bootstrap", nil, nil)
		var boot protocol.BootstrapResponse
		_ = json.Unmarshal(bBoot, &boot)
		found := false
		for _, m := range boot.Machines {
			if m.MachineID == "deploy-vps" {
				found = true
				if m.TransportMode != protocol.TransportRemoteWS {
					t.Fatalf("[%s] expected deploy-vps transportMode=remote_ws after ReloadConfig, got %s", label, m.TransportMode)
				}
				if m.Source != protocol.MachineSourcePaired {
					t.Fatalf("[%s] expected deploy-vps source=paired, got %s", label, m.Source)
				}
				if m.DisplayName != "Paired Remote Runner" {
					t.Fatalf("[%s] expected paired displayName preserved, got %q", label, m.DisplayName)
				}
			}
		}
		if !found {
			t.Fatalf("[%s] deploy-vps missing from bootstrap", label)
		}
	}

	verifyPairedRemoteWS("after-reload", ts.URL)

	// 4. Fresh server restart against the same SQLite + old machines.yaml MUST stay remote_ws & paired!
	srv2, err := hub.NewServer(ctx, hub.ServerOptions{
		HubConfig:   cfg,
		Store:       st,
		RyugiRunner: &fakeRyugiRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv2.Shutdown()
	ts2 := httptest.NewServer(srv2.Handler())
	defer ts2.Close()

	verifyPairedRemoteWS("after-restart", ts2.URL)
}

func TestHub_PABriefExcludesRemovedMachinesAndConcurrentYAMLDeleteSafe(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "config")
	dataDir := filepath.Join(tmp, "data")
	_ = os.MkdirAll(cfgDir, 0o755)
	_ = os.MkdirAll(dataDir, 0o755)

	machinesYAML := `schemaVersion: 1
machines:
  hub-node:
    displayName: "Local Hub"
    transport: local_ws
    capabilities: ["primary-dev"]
  aux-node:
    displayName: "Aux Node"
    transport: remote_ws
    capabilities: ["linux-dev"]
  mac-node:
    displayName: "Mac Node"
    transport: remote_ws
    capabilities: ["macos-env"]
  deploy-vps:
    displayName: "Deploy VPS"
    transport: ssh_outbound
    capabilities: ["deploy-node"]
`
	machPath := filepath.Join(cfgDir, "machines.yaml")
	if err := os.WriteFile(machPath, []byte(machinesYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(dataDir, "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := protocol.HubConfig{
		SchemaVersion: 1,
		ListenAddr:    "127.0.0.1:0",
		DataDir:       dataDir,
		ConfigDir:     cfgDir,
		DevOpen:       true,
	}
	srv, err := hub.NewServer(ctx, hub.ServerOptions{
		HubConfig:   cfg,
		Store:       st,
		RyugiRunner: &fakeRyugiRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Concurrently delete 3 YAML machines -> serialized YAML write-back with unique temp files must stay intact.
	targets := []string{"aux-node", "mac-node", "deploy-vps"}
	var wg sync.WaitGroup
	for _, id := range targets {
		wg.Add(1)
		go func(mid string) {
			defer wg.Done()
			r, b := doJSONReq(t, http.MethodDelete, ts.URL+"/api/machines/"+mid, nil, nil)
			if r.StatusCode != http.StatusOK {
				t.Errorf("concurrent DELETE %s failed: %d %s", mid, r.StatusCode, b)
			}
		}(id)
	}
	wg.Wait()

	// Verify machines.yaml has ONLY hub-node left and no stray temp files remain.
	loaded, err := protocol.LoadMachinesConfig(machPath)
	if err != nil {
		t.Fatalf("machines.yaml corrupted after concurrent deletes: %v", err)
	}
	if len(loaded.Machines) != 1 {
		t.Fatalf("expected only hub-node in machines.yaml, got %+v", loaded.Machines)
	}
	if _, ok := loaded.Machines["hub-node"]; !ok {
		t.Fatalf("hub-node unexpectedly removed from machines.yaml")
	}
	entries, err := os.ReadDir(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("leftover temp file in configDir: %s", e.Name())
		}
	}

	// Verify PA brief excludes all removed machines from both Machines slice and summary denominator.
	brief, err := st.BuildPABrief(ctx, 24, "", loaded.Machines)
	if err != nil {
		t.Fatalf("BuildPABrief failed: %v", err)
	}
	if len(brief.Machines) != 1 || brief.Machines[0].MachineID != "hub-node" {
		t.Fatalf("expected PA brief to include only hub-node, got %+v", brief.Machines)
	}
	if !strings.Contains(brief.Summary, "在线节点 1/1") {
		t.Fatalf("expected PA brief summary denominator 1/1 (excluding removed machines), got %q", brief.Summary)
	}
}
