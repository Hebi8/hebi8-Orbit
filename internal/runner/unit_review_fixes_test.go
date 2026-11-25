package runner

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

func TestB5_DefaultPermissionPolicyAllowlistMatrix(t *testing.T) {
	wt := t.TempDir()
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(wt, "app.go"), []byte("package main\n"), 0o644)
	_ = os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644)
	_ = os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(wt, "escape_link.txt"))
	linkPath := filepath.Join(wt, "escape_dir")
	_ = os.Symlink(outside, linkPath)
	_ = os.MkdirAll(filepath.Join(wt, ".git"), 0o755)
	_ = os.WriteFile(filepath.Join(wt, ".git", "config"), []byte("[core]\n"), 0o644)

	opts := []acp.PermissionOption{
		{OptionId: "allow_once", Name: "Allow Once", Kind: acp.PermissionOptionKindAllowOnce},
		{OptionId: "reject_once", Name: "Reject Once", Kind: acp.PermissionOptionKindRejectOnce},
	}

	makeReq := func(kind *acp.ToolKind, locations []acp.ToolCallLocation, rawInput string) acp.RequestPermissionRequest {
		tc := acp.ToolCallUpdate{
			ToolCallId: "tc_1",
		}
		if kind != nil {
			tc.Kind = acp.PatchSet(*kind)
		}
		if locations != nil {
			tc.Locations = acp.PatchSet(locations)
		}
		if rawInput != "" {
			tc.RawInput = acp.PatchSet(json.RawMessage(rawInput))
		}
		return acp.RequestPermissionRequest{
			SessionId: "sid_1",
			ToolCall:  tc,
			Options:   opts,
		}
	}

	kRead := acp.ToolKindRead
	kEdit := acp.ToolKindEdit
	kThink := acp.ToolKindThink
	kExec := acp.ToolKindExecute
	kOther := acp.ToolKindOther
	kDelete := acp.ToolKindDelete

	cases := []struct {
		name      string
		mode      string
		req       acp.RequestPermissionRequest
		wantAllow bool
	}{
		{
			name:      "default mode forwards permission to human",
			mode:      "default",
			req:       makeReq(&kRead, []acp.ToolCallLocation{{Path: filepath.Join(wt, "app.go")}}, ""),
			wantAllow: false,
		},
		{
			name:      "auto mode forwards ACP permission requests to human (ACP agent decides when to ask)",
			mode:      "auto",
			req:       makeReq(&kEdit, []acp.ToolCallLocation{{Path: "app.go"}}, ""),
			wantAllow: false,
		},
		{
			name:      "accept-edits mode forwards ACP permission requests to human",
			mode:      "accept-edits",
			req:       makeReq(&kExec, nil, `{"command":"git status"}`),
			wantAllow: false,
		},
		{
			name:      "codex agent mode forwards ACP permission requests to human",
			mode:      "agent",
			req:       makeReq(&kExec, nil, `{"command":"rm -rf /tmp/x"}`),
			wantAllow: false,
		},
		{
			name:      "empty mode forwards permission requests to human",
			mode:      "",
			req:       makeReq(&kThink, nil, ""),
			wantAllow: false,
		},
		{
			name:      "explicit full mode auto-allows",
			mode:      "full",
			req:       makeReq(&kExec, nil, `{"command":"go test ./..."}`),
			wantAllow: true,
		},
		{
			name:      "explicit bypassPermissions mode auto-allows",
			mode:      "bypassPermissions",
			req:       makeReq(&kEdit, []acp.ToolCallLocation{{Path: "app.go"}}, ""),
			wantAllow: true,
		},
		{
			name:      "explicit agent-full-access mode auto-allows",
			mode:      "agent-full-access",
			req:       makeReq(&kDelete, []acp.ToolCallLocation{{Path: filepath.Join(wt, "app.go")}}, ""),
			wantAllow: true,
		},
	}
	_ = kOther

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			optID, allowed := defaultPermissionPolicy(wt, tc.mode, tc.req)
			if allowed != tc.wantAllow {
				t.Fatalf("allowed=%v (opt=%q), want %v", allowed, optID, tc.wantAllow)
			}
			if allowed && optID != "allow_once" {
				t.Fatalf("expected allow_once, got %q", optID)
			}
		})
	}
}

func TestB14_PrepareWorktreeStrictValidation(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	repoA := filepath.Join(tmp, "repoA")
	repoB := filepath.Join(tmp, "repoB")
	_ = os.MkdirAll(repoA, 0o755)
	_ = os.MkdirAll(repoB, 0o755)

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
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}

	runG(repoA, "init", "-b", "main")
	_ = os.WriteFile(filepath.Join(repoA, "a.txt"), []byte("a1\n"), 0o644)
	runG(repoA, "add", ".")
	runG(repoA, "commit", "-m", "a1")
	shaA1 := runG(repoA, "rev-parse", "HEAD")
	_ = os.WriteFile(filepath.Join(repoA, "a.txt"), []byte("a2\n"), 0o644)
	runG(repoA, "add", ".")
	runG(repoA, "commit", "-m", "a2")
	shaA2 := runG(repoA, "rev-parse", "HEAD")

	runG(repoB, "init", "-b", "main")
	_ = os.WriteFile(filepath.Join(repoB, "b.txt"), []byte("b1\n"), 0o644)
	runG(repoB, "add", ".")
	runG(repoB, "commit", "-m", "b1")

	// 1. Existing non-git directory -> MUST fail and keep file untouched
	nonGit := filepath.Join(tmp, "nongit")
	_ = os.MkdirAll(nonGit, 0o755)
	_ = os.WriteFile(filepath.Join(nonGit, "keep.txt"), []byte("do-not-touch"), 0o644)
	if _, _, err := PrepareWorktree(ctx, protocol.AttemptDispatchFrame{
		ProjectID: "demo-app", TaskID: "t1", AttemptID: "a1", RepoPath: repoA, WorktreePath: nonGit, BaseSHA: shaA2,
	}, tmp); err == nil {
		t.Fatal("expected error for existing non-git directory")
	}
	if b, _ := os.ReadFile(filepath.Join(nonGit, "keep.txt")); string(b) != "do-not-touch" {
		t.Fatal("PrepareWorktree modified foreign directory!")
	}

	// 2. Existing directory belonging to repoB (different git-common-dir) -> MUST fail
	if _, _, err := PrepareWorktree(ctx, protocol.AttemptDispatchFrame{
		ProjectID: "demo-app", TaskID: "t1", AttemptID: "a2", RepoPath: repoA, WorktreePath: repoB, BaseSHA: shaA2,
	}, tmp); err == nil {
		t.Fatal("expected error for mismatched repo worktree")
	}

	// 3. Valid worktree at shaA1 reused when dispatch asks for shaA2 -> MUST fail with SHA mismatch
	wtA := filepath.Join(tmp, "wtA")
	if _, _, err := PrepareWorktree(ctx, protocol.AttemptDispatchFrame{
		ProjectID: "demo-app", TaskID: "t1", AttemptID: "a3", RepoPath: repoA, WorktreePath: wtA, BaseSHA: shaA1,
	}, tmp); err != nil {
		t.Fatalf("fresh worktree add failed: %v", err)
	}
	if _, _, err := PrepareWorktree(ctx, protocol.AttemptDispatchFrame{
		ProjectID: "demo-app", TaskID: "t1", AttemptID: "a4", RepoPath: repoA, WorktreePath: wtA, BaseSHA: shaA2,
	}, tmp); err == nil || !strings.Contains(err.Error(), "ERR_WORKTREE_SHA_MISMATCH") {
		t.Fatalf("expected ERR_WORKTREE_SHA_MISMATCH, got %v", err)
	}

	// 4. Dirty existing worktree for a fresh attempt (not sameSessionReuse) -> MUST fail with ERR_WORKTREE_DIRTY
	_ = os.WriteFile(filepath.Join(wtA, "a.txt"), []byte("dirty\n"), 0o644)
	if _, _, err := PrepareWorktree(ctx, protocol.AttemptDispatchFrame{
		ProjectID: "demo-app", TaskID: "t1", AttemptID: "a5", RepoPath: repoA, WorktreePath: wtA, BaseSHA: shaA1,
	}, tmp); err == nil || !strings.Contains(err.Error(), "ERR_WORKTREE_DIRTY") {
		t.Fatalf("expected ERR_WORKTREE_DIRTY, got %v", err)
	}
}

func TestB15_ChunkCoalescerDeadlineOrderAndCloseDrain(t *testing.T) {
	type emitted struct {
		kind        protocol.EventKind
		text        string
		unsolicited bool
		at          time.Time
	}
	var mu sync.Mutex
	var got []emitted

	c := NewChunkCoalescer(60*time.Millisecond, func(attemptID, leaseToken string, kind protocol.EventKind, text string, unsolicited bool) {
		mu.Lock()
		got = append(got, emitted{kind: kind, text: text, unsolicited: unsolicited, at: time.Now()})
		mu.Unlock()
	})

	// 1. Continuous streaming faster than 60ms window MUST still flush at fixed 60ms deadline (no timer starvation!)
	start := time.Now()
	for i := 0; i < 6; i++ {
		c.Append("att_1", "tok", protocol.EventACPMessageBatch, "x", false)
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	flushedDuringStream := len(got)
	mu.Unlock()
	if flushedDuringStream == 0 {
		t.Fatalf("coalescer starved during continuous streaming over %v", time.Since(start))
	}

	// 2. Kind switch + unsolicited switch preserves strict chronological order
	c.FlushAttempt("att_1")
	mu.Lock()
	got = nil
	mu.Unlock()

	c.Append("att_1", "tok", protocol.EventACPThoughtBatch, "think-1", false)
	c.Append("att_1", "tok", protocol.EventACPMessageBatch, "msg-1", false)
	c.Append("att_1", "tok", protocol.EventACPMessageBatch, "msg-unsol", true)
	c.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("expected 3 ordered segments after Close drain, got %+v", got)
	}
	if got[0].kind != protocol.EventACPThoughtBatch || got[0].text != "think-1" ||
		got[1].kind != protocol.EventACPMessageBatch || got[1].text != "msg-1" || got[1].unsolicited ||
		got[2].kind != protocol.EventACPMessageBatch || got[2].text != "msg-unsol" || !got[2].unsolicited {
		t.Fatalf("ordering/unsolicited mismatch: %+v", got)
	}
}

func TestB19_OutboxErrorDoesNotCorruptRunnerState(t *testing.T) {
	ctx := context.Background()
	outbox, err := OpenLocalOutbox(ctx, filepath.Join(t.TempDir(), "outbox.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{
		runCtx:        ctx,
		outbox:        outbox,
		coalescer:     NewChunkCoalescer(50*time.Millisecond, func(string, string, protocol.EventKind, string, bool) {}),
		attempts:      map[string]*attemptContext{},
		sessionOwner:  map[acp.SessionId]string{"sid_b19": "att_b19"},
		sessions:      map[string]*sessionSlot{},
		resourceLocks: map[string]string{"gpu": "att_b19"},
		outboxWakeCh:  make(chan struct{}, 1),
	}
	att := &attemptContext{
		dispatch: protocol.AttemptDispatchFrame{
			AttemptID:  "att_b19",
			LeaseToken: "tok_b19",
		},
		providerSessionID: "sid_b19",
		state:             protocol.AttemptRunning,
		heldResources:     []string{"gpu"},
		tracker:           NewActiveSwarmTracker("att_b19"),
		pendingApprovals:  map[string]*pendingApprovalHandle{},
		doneCh:            make(chan struct{}),
	}
	r.attempts["att_b19"] = att

	// Close outbox DB to force enqueueOutbox failure!
	_ = outbox.Close()

	if err := r.finalizeAttempt(att, protocol.AttemptSucceeded, map[string]any{"state": "succeeded"}); err == nil {
		t.Fatal("expected finalizeAttempt to return error when outbox DB is closed")
	}
	att.mu.Lock()
	finalized := att.finalized
	att.mu.Unlock()
	if finalized {
		t.Fatal("finalizeAttempt corrupted att.finalized=true despite outbox failure!")
	}
	if r.HeldResourceOwner("gpu") != "att_b19" {
		t.Fatal("finalizeAttempt released resource despite outbox failure!")
	}
}

func TestS3_ActiveSwarmTrackerLateRawInputAndOutputAfterCompleted(t *testing.T) {
	tr := NewActiveSwarmTracker("att_s3")

	// 1. ToolCall arrives as completed WITHOUT rawInput yet
	_ = tr.ObserveToolCall(&acp.SessionUpdateToolCall{
		ToolCallId: "tc-swarm-late",
		Name:       acp.Ptr("swarm"),
		Status:     acp.ToolCallStatusCompleted,
	})
	if tr.HasActiveWorkers() {
		t.Fatal("should have 0 active workers before rawInput arrives")
	}

	// 2. Late ToolCallUpdate delivers rawInput AFTER status=completed -> MUST still track worker!
	proj := tr.ObserveToolCallUpdate(&acp.SessionToolCallUpdate{
		ToolCallId: "tc-swarm-late",
		RawInput:   acp.PatchSet(json.RawMessage(`{"action":"add","name":"late-worker","brief":"work"}`)),
	})
	if !tr.HasActiveWorkers() || len(proj.Members) != 1 || proj.Members[0]["memberName"] != "late-worker" {
		t.Fatalf("late rawInput after completed was dropped! active=%v proj=%+v", tr.HasActiveWorkers(), proj)
	}

	// 3. Command tool completes first, then late rawOutput delivers non-zero exit_code -> MUST count nonZeroExit!
	_ = tr.ObserveToolCall(&acp.SessionUpdateToolCall{
		ToolCallId: "tc-cmd-late",
		Name:       acp.Ptr("run_command"),
		Status:     acp.ToolCallStatusCompleted,
	})
	_ = tr.ObserveToolCallUpdate(&acp.SessionToolCallUpdate{
		ToolCallId: "tc-cmd-late",
		RawOutput:  acp.PatchSet(json.RawMessage(`{"exit_code":2}`)),
	})
	_, _, _, nonZero := tr.Stats()
	_, unresNonZero := tr.UnresolvedCounts()
	if nonZero != 1 || unresNonZero != 1 {
		t.Fatalf("late rawOutput exit_code after completed was dropped! nonZero=%d unresNonZero=%d", nonZero, unresNonZero)
	}
}
