package runner_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/coder/websocket"

	"github.com/Hebi8/hebi8-Orbit/internal/gitops"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/runner"
)

// ============================================================================
// B1: Runner StopReason adjudication & _scratch vs project delivery enforcement
// ============================================================================
func TestB1_E2E_StopReasonsAndScratchVsProjectDelivery(t *testing.T) {
	env := newTestEnv(t)
	drv := NewFakeAcpDriver()

	var step atomic.Int32
	drv.OnPrompt = func(ctx context.Context, d *FakeAcpDriver, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
		switch step.Add(1) {
		case 1:
			return acp.PromptResponse{StopReason: acp.StopReasonRefusal}, nil
		case 2:
			return acp.PromptResponse{StopReason: acp.StopReasonMaxTokens}, nil
		case 3:
			// _scratch pure chat clean end_turn -> MUST succeed without delivery!
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		case 4:
			// _scratch with human-rejected tool -> MUST land in needs_verification!
			_, _ = d.AskPermission(ctx, acp.RequestPermissionRequest{
				SessionId: sid,
				ToolCall:  acp.ToolCallUpdate{ToolCallId: "tc-rej-scratch", Name: acp.PatchSet("run_command")},
				Options: []acp.PermissionOption{
					{OptionId: "allow_once", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
					{OptionId: "reject_once", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
				},
			})
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		default:
			// demo-app project turn without delivery -> MUST fail with NO_VERIFIED_DELIVERY!
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}
	}

	env.startRunner("hub-node", "", "", drv, nil)

	// 1. Refusal -> failed (ABNORMAL_STOP_REASON)
	_, raw := env.apiReq(http.MethodPost, "/api/projects/_scratch/tasks", protocol.CreateTaskRequest{Goal: "refuse me"}, nil)
	var c1 protocol.CreateTaskResponse
	_ = json.Unmarshal(raw, &c1)
	eventually(t, 3*time.Second, func() bool {
		rec, err := env.st.GetAttemptRecord(context.Background(), c1.Attempt.AttemptID)
		return err == nil && rec.State == protocol.AttemptFailed && rec.ErrorCode == "ABNORMAL_STOP_REASON"
	}, "refusal did not fail with ABNORMAL_STOP_REASON")

	// 2. MaxTokens -> failed (ABNORMAL_STOP_REASON)
	_, raw = env.apiReq(http.MethodPost, "/api/projects/_scratch/tasks", protocol.CreateTaskRequest{Goal: "max tokens"}, nil)
	var c2 protocol.CreateTaskResponse
	_ = json.Unmarshal(raw, &c2)
	eventually(t, 3*time.Second, func() bool {
		rec, err := env.st.GetAttemptRecord(context.Background(), c2.Attempt.AttemptID)
		return err == nil && rec.State == protocol.AttemptFailed && rec.ErrorCode == "ABNORMAL_STOP_REASON"
	}, "max_tokens did not fail with ABNORMAL_STOP_REASON")

	// 3. _scratch clean end_turn -> succeeded without delivery
	_, raw = env.apiReq(http.MethodPost, "/api/projects/_scratch/tasks", protocol.CreateTaskRequest{Goal: "just chat"}, nil)
	var c3 protocol.CreateTaskResponse
	_ = json.Unmarshal(raw, &c3)
	eventually(t, 3*time.Second, func() bool {
		rec, err := env.st.GetAttemptRecord(context.Background(), c3.Attempt.AttemptID)
		return err == nil && rec.State == protocol.AttemptSucceeded
	}, "_scratch clean chat did not succeed")

	// 4. _scratch with rejected permission -> needs_verification
	_, raw = env.apiReq(http.MethodPost, "/api/projects/_scratch/tasks", protocol.CreateTaskRequest{
		Goal: "reject tool", Config: map[string]string{"mode": "default"},
	}, nil)
	var c4 protocol.CreateTaskResponse
	_ = json.Unmarshal(raw, &c4)
	var ap4 protocol.ApprovalView
	eventually(t, 3*time.Second, func() bool {
		list, _ := env.st.ListApprovals(context.Background(), protocol.ApprovalPending)
		if len(list) == 1 {
			ap4 = list[0]
			return true
		}
		return false
	}, "approval for c4 not created")
	env.apiReq(http.MethodPost, "/api/approvals/"+ap4.ApprovalID+"/decide", protocol.DecideApprovalRequest{
		OptionID: "reject_once", ExpectedAttemptID: c4.Attempt.AttemptID, ExpectedPayloadSHA256: ap4.PayloadSHA256,
	}, nil)
	eventually(t, 3*time.Second, func() bool {
		rec, err := env.st.GetAttemptRecord(context.Background(), c4.Attempt.AttemptID)
		return err == nil && rec.State == protocol.AttemptNeedsVerification
	}, "_scratch rejected tool did not land in needs_verification")

	// 5. Real project demo-app without delivery -> failed (NO_VERIFIED_DELIVERY)
	_, raw = env.apiReq(http.MethodPost, "/api/projects/demo-app/tasks", protocol.CreateTaskRequest{Goal: "forgot deliver"}, nil)
	var c5 protocol.CreateTaskResponse
	_ = json.Unmarshal(raw, &c5)
	eventually(t, 3*time.Second, func() bool {
		rec, err := env.st.GetAttemptRecord(context.Background(), c5.Attempt.AttemptID)
		return err == nil && rec.State == protocol.AttemptFailed && rec.ErrorCode == "NO_VERIFIED_DELIVERY"
	}, "demo-app without delivery did not fail with NO_VERIFIED_DELIVERY")
}

// ============================================================================
// B2: `deliver --final` during in-flight Prompt() & active workers isolation
// ============================================================================
func TestB2_E2E_FinalDeliverDuringInFlightPromptAndActiveWorkersIsolation(t *testing.T) {
	t.Run("DeferFinalUntilInFlightPromptReturns", func(t *testing.T) {
		env := newTestEnv(t)
		drv := NewFakeAcpDriver()

		var r *runner.Runner
		var att1ID string
		var mu sync.Mutex
		deliverDone := make(chan runner.LocalDeliverResponse, 1)
		releasePrompt1 := make(chan struct{})
		prompt2Started := make(chan acp.SessionId, 1)
		var promptNum atomic.Int32

		drv.OnPrompt = func(ctx context.Context, d *FakeAcpDriver, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
			if promptNum.Add(1) == 1 {
				// Simulate agent running `orbit deliver --final` INSIDE Prompt() before returning!
				mu.Lock()
				aid := att1ID
				runRef := r
				mu.Unlock()
				eventually(t, 2*time.Second, func() bool {
					rec, err := env.st.GetAttemptRecord(context.Background(), aid)
					return err == nil && rec.WorktreePath != ""
				}, "worktree path not set")
				rec, _ := env.st.GetAttemptRecord(context.Background(), aid)
				_ = os.WriteFile(filepath.Join(rec.WorktreePath, "app.go"), []byte("package main\n// inflight final\nfunc main() {}\n"), 0o644)
				resp, err := runRef.HandleLocalDeliver(context.Background(), runner.LocalDeliverRequest{
					AttemptID: aid, Summary: "delivered inside prompt", Final: true,
				})
				if err != nil {
					t.Errorf("HandleLocalDeliver inside prompt failed: %v", err)
				}
				deliverDone <- resp
				<-releasePrompt1
				return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
			}
			prompt2Started <- sid
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}

		r = env.startRunner("hub-node", "", "", drv, nil)
		_, raw := env.apiReq(http.MethodPost, "/api/projects/demo-app/tasks", protocol.CreateTaskRequest{Goal: "inflight test"}, nil)
		var cr protocol.CreateTaskResponse
		_ = json.Unmarshal(raw, &cr)
		mu.Lock()
		att1ID = cr.Attempt.AttemptID
		mu.Unlock()

		// Queue followup so Hub would try to dispatch immediately if att_1 finalized too early!
		env.apiReq(http.MethodPost, "/api/tasks/"+cr.Task.TaskID+"/followup", protocol.FollowupTaskRequest{
			Message: "turn 2", Mode: protocol.FollowupModeQueue,
		}, nil)

		dlvResp := <-deliverDone
		if dlvResp.Finalized {
			t.Fatalf("deliver --final MUST defer finalization while Prompt() is still executing, got Finalized=true")
		}
		// While prompt 1 is still blocked, prompt 2 MUST NOT have started!
		time.Sleep(80 * time.Millisecond)
		if promptNum.Load() != 1 {
			t.Fatalf("Turn 2 started while Turn 1 Prompt() was still in-flight!")
		}

		// Now let Prompt 1 return -> att_1 finalizes cleanly to succeeded AND Turn 2 starts!
		close(releasePrompt1)
		eventually(t, 3*time.Second, func() bool {
			rec, err := env.st.GetAttemptRecord(context.Background(), cr.Attempt.AttemptID)
			return err == nil && rec.State == protocol.AttemptSucceeded
		}, "att_1 did not finalize to succeeded after Prompt() returned")

		select {
		case <-prompt2Started:
		case <-time.After(3 * time.Second):
			t.Fatal("Turn 2 did not start after att_1 deferred finalization completed")
		}
	})

	t.Run("FinalDeliverOrCancelWithActiveSwarmWorkersIsolatesOldSession", func(t *testing.T) {
		env := newTestEnv(t)
		drv := NewFakeAcpDriver()

		var sids []acp.SessionId
		var sidMu sync.Mutex
		drv.OnPrompt = func(ctx context.Context, d *FakeAcpDriver, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
			sidMu.Lock()
			sids = append(sids, sid)
			n := len(sids)
			sidMu.Unlock()
			if n == 1 {
				_ = d.EmitUpdate(ctx, sid, acp.SessionUpdate{
					ToolCall: &acp.SessionUpdateToolCall{
						ToolCallId: "tc-b2-worker",
						Name:       acp.Ptr("swarm"),
						Status:     acp.ToolCallStatusCompleted,
						RawInput:   json.RawMessage(`{"action":"add","name":"still-busy"}`),
					},
				})
			}
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}

		r := env.startRunner("hub-node", "", "", drv, nil)
		_, raw := env.apiReq(http.MethodPost, "/api/projects/demo-app/tasks", protocol.CreateTaskRequest{Goal: "swarm early final"}, nil)
		var cr protocol.CreateTaskResponse
		_ = json.Unmarshal(raw, &cr)

		eventually(t, 3*time.Second, func() bool {
			rec, err := env.st.GetAttemptRecord(context.Background(), cr.Attempt.AttemptID)
			return err == nil && rec.State == protocol.AttemptSwarmMonitoring
		}, "did not enter swarm_monitoring")

		// Deliver --final WHILE still-busy worker is STILL active -> MUST isolate & close old SID!
		rec, _ := env.st.GetAttemptRecord(context.Background(), cr.Attempt.AttemptID)
		_ = os.WriteFile(filepath.Join(rec.WorktreePath, "app.go"), []byte("package main\n// early final\nfunc main() {}\n"), 0o644)
		dlv, err := r.HandleLocalDeliver(context.Background(), runner.LocalDeliverRequest{
			AttemptID: cr.Attempt.AttemptID, Summary: "early final with active worker", Final: true,
		})
		if err != nil || !dlv.Finalized {
			t.Fatalf("HandleLocalDeliver failed: resp=%+v err=%v", dlv, err)
		}
		eventually(t, 3*time.Second, func() bool {
			r2, err := env.st.GetAttemptRecord(context.Background(), cr.Attempt.AttemptID)
			return err == nil && r2.State == protocol.AttemptSucceeded
		}, "att_1 did not succeed")

		// Verify CloseSession was called on sids[0] and Turn 2 gets a BRAND NEW SID!
		drv.mu.Lock()
		closed := len(drv.closeCalls)
		drv.mu.Unlock()
		if closed == 0 {
			t.Fatal("expected CloseSession called to isolate old SID when finalizing with active swarm workers")
		}
		env.apiReq(http.MethodPost, "/api/tasks/"+cr.Task.TaskID+"/followup", protocol.FollowupTaskRequest{
			Message: "next turn", Mode: protocol.FollowupModeQueue,
		}, nil)
		eventually(t, 3*time.Second, func() bool {
			sidMu.Lock()
			defer sidMu.Unlock()
			return len(sids) == 2 && sids[0] != sids[1]
		}, "Turn 2 illegally reused isolated old provider session!")
	})
}

// ============================================================================
// B3: Short disconnect during which attempt finishes locally in Outbox
// ============================================================================
func TestB3_E2E_ShortDisconnectLocalFinalizeDrainsFormally(t *testing.T) {
	env := newTestEnv(t)
	drv := NewFakeAcpDriver()

	enteredPrompt := make(chan struct{}, 1)
	releasePrompt := make(chan struct{})
	drv.OnPrompt = func(ctx context.Context, d *FakeAcpDriver, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
		enteredPrompt <- struct{}{}
		<-releasePrompt
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	r := env.startRunner("hub-node", "", "", drv, func(c *runner.Config) {
		c.ReconnectInitialDelay = 300 * time.Millisecond
		c.ReconnectMaxDelay = 300 * time.Millisecond
	})

	_, raw := env.apiReq(http.MethodPost, "/api/projects/demo-app/tasks", protocol.CreateTaskRequest{Goal: "offline finish"}, nil)
	var cr protocol.CreateTaskResponse
	_ = json.Unmarshal(raw, &cr)
	<-enteredPrompt

	eventually(t, 2*time.Second, func() bool {
		rec, err := env.st.GetAttemptRecord(context.Background(), cr.Attempt.AttemptID)
		return err == nil && rec.State == protocol.AttemptRunning && rec.WorktreePath != ""
	}, "attempt did not reach running")
	rec, _ := env.st.GetAttemptRecord(context.Background(), cr.Attempt.AttemptID)

	// Drop WS right now, wait for Hub to observe disconnect
	r.SimulateAbruptDisconnect()
	eventually(t, 2*time.Second, func() bool {
		return !env.srv.RunnerGateway().IsConnected("hub-node")
	}, "hub did not see disconnect")

	// While DISCONNECTED, deliver --final and let Prompt() return so attempt finalizes locally into SQLite Outbox!
	_ = os.WriteFile(filepath.Join(rec.WorktreePath, "app.go"), []byte("package main\n// offline completed\nfunc main() {}\n"), 0o644)
	dlv, err := r.HandleLocalDeliver(context.Background(), runner.LocalDeliverRequest{
		AttemptID: cr.Attempt.AttemptID, Summary: "Finished during short disconnect", Final: true,
	})
	if err != nil {
		t.Fatalf("offline HandleLocalDeliver failed: %v", err)
	}
	close(releasePrompt)

	// Runner automatically reconnects -> runner.hello reports PendingFinal=true -> Hub MUST drain formally!
	eventually(t, 5*time.Second, func() bool {
		attNow, err := env.st.GetAttemptRecord(context.Background(), cr.Attempt.AttemptID)
		return err == nil && attNow.State == protocol.AttemptSucceeded
	}, "attempt finalized during short disconnect did not drain to succeeded (likely misclassified as expired)")

	taskNow, _ := env.st.GetTask(context.Background(), cr.Task.TaskID)
	if taskNow.LatestDeliveryID != dlv.DeliveryID {
		t.Fatalf("expected formal latestDeliveryId=%s, got %q", dlv.DeliveryID, taskNow.LatestDeliveryID)
	}
	dlvRec, _, _, _, err := env.st.GetDeliveryDetail(context.Background(), dlv.DeliveryID)
	if err != nil || dlvRec.LateArrival {
		t.Fatalf("expected formal delivery (lateArrival=false), got %+v err=%v", dlvRec, err)
	}
}

// ============================================================================
// B6 & S10: Real ACP JSON-RPC stdio stream write boundary for permissions
// ============================================================================
type gatedWriteCloser struct {
	io.WriteCloser
	mu          sync.Mutex
	gateCh      chan struct{}
	injectErr   error
	intercepted chan struct{}
}

func (g *gatedWriteCloser) Write(p []byte) (int, error) {
	// Gate ONLY permission JSON-RPC responses (contain `"outcome"`).
	if bytes.Contains(p, []byte(`"outcome"`)) {
		g.mu.Lock()
		gate := g.gateCh
		inj := g.injectErr
		inter := g.intercepted
		g.mu.Unlock()
		if inter != nil {
			select {
			case inter <- struct{}{}:
			default:
			}
		}
		if gate != nil {
			<-gate
		}
		if inj != nil {
			return 0, inj
		}
	}
	return g.WriteCloser.Write(p)
}

type realPipeAgent struct {
	mu       sync.Mutex
	conn     *acp.AgentSideConnection
	nextSeq  int
	onPrompt func(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId) (acp.PromptResponse, error)
}

func (a *realPipeAgent) Authenticate(context.Context, acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}
func (a *realPipeAgent) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{ProtocolVersion: acp.ProtocolVersionNumber}, nil
}
func (a *realPipeAgent) Logout(context.Context, acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, nil
}
func (a *realPipeAgent) Cancel(context.Context, acp.CancelNotification) error { return nil }
func (a *realPipeAgent) CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, nil
}
func (a *realPipeAgent) DeleteSession(context.Context, acp.DeleteSessionRequest) (acp.DeleteSessionResponse, error) {
	return acp.DeleteSessionResponse{}, nil
}
func (a *realPipeAgent) ListSessions(context.Context, acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, nil
}
func (a *realPipeAgent) NewSession(context.Context, acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	a.mu.Lock()
	a.nextSeq++
	sid := acp.SessionId(fmt.Sprintf("pipe-sid-%d", a.nextSeq))
	a.mu.Unlock()
	return acp.NewSessionResponse{SessionId: sid}, nil
}
func (a *realPipeAgent) ResumeSession(context.Context, acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, nil
}
func (a *realPipeAgent) SetSessionConfigOption(_ context.Context, req acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	if req.ValueId != nil {
		opts := acp.SessionConfigSelectOptionsUngrouped{
			{Name: string(req.ValueId.Value), Value: req.ValueId.Value},
		}
		return acp.SetSessionConfigOptionResponse{
			ConfigOptions: []acp.SessionConfigOption{
				{Select: &acp.SessionConfigOptionSelect{
					Id:           req.ValueId.ConfigId,
					Name:         string(req.ValueId.ConfigId),
					Type:         "select",
					CurrentValue: req.ValueId.Value,
					Options:      acp.SessionConfigSelectOptions{Ungrouped: &opts},
				}},
			},
		}, nil
	}
	return acp.SetSessionConfigOptionResponse{}, nil
}
func (a *realPipeAgent) SetSessionMode(context.Context, acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, nil
}
func (a *realPipeAgent) Prompt(ctx context.Context, req acp.PromptRequest) (acp.PromptResponse, error) {
	if a.onPrompt != nil {
		return a.onPrompt(ctx, a.conn, req.SessionId)
	}
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func TestB6_S10_RealACPStreamPermissionWriteBoundary(t *testing.T) {
	runCase := func(t *testing.T, failWrite bool) {
		env := newTestEnv(t)

		agentToRunnerR, agentToRunnerW := io.Pipe()
		runnerToAgentR, runnerToAgentW := io.Pipe()

		gate := make(chan struct{})
		intercepted := make(chan struct{}, 1)
		gatedW := &gatedWriteCloser{
			WriteCloser: runnerToAgentW,
			gateCh:      gate,
			intercepted: intercepted,
		}
		if failWrite {
			gatedW.injectErr = errors.New("broken stdio pipe")
		}

		permAnswered := make(chan acp.RequestPermissionResponse, 1)
		agent := &realPipeAgent{
			onPrompt: func(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId) (acp.PromptResponse, error) {
				resp, err := conn.RequestPermission(ctx, acp.RequestPermissionRequest{
					SessionId: sid,
					ToolCall:  acp.ToolCallUpdate{ToolCallId: "tc-real-pipe", Name: acp.PatchSet("run_command")},
					Options: []acp.PermissionOption{
						{OptionId: "allow_once", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
					},
				})
				if err == nil {
					permAnswered <- resp
				}
				return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
			},
		}
		agentConn := acp.NewAgentSideConnection(agent, agentToRunnerW, runnerToAgentR)
		agent.conn = agentConn
		t.Cleanup(func() {
			_ = agentToRunnerW.Close()
			_ = runnerToAgentW.Close()
			_ = agentToRunnerR.Close()
			_ = runnerToAgentR.Close()
		})

		streamDrv := runner.NewStreamAcpDriver(runner.AgyDriverConfig{}, gatedW, agentToRunnerR)
		outboxPath := filepath.Join(env.dataDir, fmt.Sprintf("pipe-outbox-%v.sqlite", failWrite))
		var afterWriteCalled atomic.Bool

		sockDir, _ := os.MkdirTemp("", "orb-pipe-")
		t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
		r, err := runner.New(context.Background(), runner.Config{
			MachineID:    "hub-node",
			HubWSURL:     env.hubWSURL,
			HubHTTPURL:   env.ts.URL,
			BearerToken:  env.hubTok,
			DataDir:      env.dataDir,
			OutboxDBPath: outboxPath,
			SocketPath:   filepath.Join(sockDir, "r.sock"),
			Driver:       streamDrv,
			OnAfterACPApprovalWrite: func(approvalID string) bool {
				afterWriteCalled.Store(true)
				return true
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = r.Close(ctx)
		})
		eventually(t, 3*time.Second, func() bool {
			return env.srv.RunnerGateway().ActiveRunnerInstanceID("hub-node") == r.InstanceID()
		}, "stream runner did not connect")

		_, raw := env.apiReq(http.MethodPost, "/api/projects/_scratch/tasks", protocol.CreateTaskRequest{
			Goal:   "real pipe perm test",
			Config: map[string]string{"mode": "default"},
		}, nil)
		var cr protocol.CreateTaskResponse
		_ = json.Unmarshal(raw, &cr)

		var ap protocol.ApprovalView
		eventually(t, 3*time.Second, func() bool {
			list, _ := env.st.ListApprovals(context.Background(), protocol.ApprovalPending)
			if len(list) == 1 {
				ap = list[0]
				return true
			}
			return false
		}, "real pipe approval did not reach pending")

		// Human approves -> Runner writes JSON-RPC response, which hits our gate BEFORE hitting the wire!
		resp, raw := env.apiReq(http.MethodPost, "/api/approvals/"+ap.ApprovalID+"/decide", protocol.DecideApprovalRequest{
			OptionID: "allow_once", ExpectedAttemptID: cr.Attempt.AttemptID, ExpectedPayloadSHA256: ap.PayloadSHA256,
		}, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("decide failed: %s", string(raw))
		}

		// Wait until JSON-RPC response bytes reach gatedW.Write (blocked at gate!)
		select {
		case <-intercepted:
		case <-time.After(3 * time.Second):
			t.Fatal("permission JSON-RPC response never reached stdio writer")
		}

		// STRICT B6 CHECK: BEFORE underlying stdio Write completes, Runner MUST NOT have fired OnAfterACPApprovalWrite or acked!
		time.Sleep(60 * time.Millisecond)
		if afterWriteCalled.Load() {
			t.Fatal("OnAfterACPApprovalWrite / approval_ack fired BEFORE stdio Write completed!")
		}
		apMid, _ := env.st.GetApproval(context.Background(), ap.ApprovalID)
		if apMid.State == protocol.ApprovalConsumedAllowed {
			t.Fatal("Hub marked approval consumed BEFORE stdio Write completed!")
		}

		// Now unblock stdio Write!
		close(gate)

		if failWrite {
			// When stdio Write fails, Runner MUST NOT record handled_approvals or send approval_ack!
			time.Sleep(150 * time.Millisecond)
			if afterWriteCalled.Load() {
				t.Fatal("OnAfterACPApprovalWrite fired despite stdio Write error!")
			}
			apAfter, _ := env.st.GetApproval(context.Background(), ap.ApprovalID)
			if apAfter.State == protocol.ApprovalConsumedAllowed {
				t.Fatal("Hub marked approval consumed despite stdio Write error!")
			}
		} else {
			// When stdio Write succeeds, real agent receives allow_once AND Hub reaches consumed_allowed!
			select {
			case ans := <-permAnswered:
				if ans.Outcome.Selected == nil || ans.Outcome.Selected.OptionId != "allow_once" {
					t.Fatalf("unexpected real agent permission response: %+v", ans)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("real agent never received permission response")
			}
			eventually(t, 3*time.Second, func() bool {
				cur, _ := env.st.GetApproval(context.Background(), ap.ApprovalID)
				return cur.State == protocol.ApprovalConsumedAllowed && afterWriteCalled.Load()
			}, "approval did not reach consumed_allowed after real stdio write succeeded")
		}
	}

	t.Run("AckStrictlyAfterStdioWriteCompletes", func(t *testing.T) { runCase(t, false) })
	t.Run("NoAckWhenStdioWriteFails", func(t *testing.T) { runCase(t, true) })
}

// ============================================================================
// B7: Swarm idle timer pauses during waiting_approval & re-arms after resume
// ============================================================================
func TestB7_E2E_SwarmIdleTimerRearmsAfterWaitingApproval(t *testing.T) {
	env := newTestEnv(t)
	drv := NewFakeAcpDriver()

	sidCh := make(chan acp.SessionId, 1)
	drv.OnPrompt = func(ctx context.Context, d *FakeAcpDriver, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
		_ = d.EmitUpdate(ctx, sid, acp.SessionUpdate{
			ToolCall: &acp.SessionUpdateToolCall{
				ToolCallId: "tc-b7-swarm",
				Name:       acp.Ptr("swarm"),
				Status:     acp.ToolCallStatusCompleted,
				RawInput:   json.RawMessage(`{"action":"add","name":"w-b7"}`),
			},
		})
		sidCh <- sid
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	env.startRunner("hub-node", "", "", drv, nil)
	_, raw := env.apiReq(http.MethodPost, "/api/projects/demo-app/tasks", protocol.CreateTaskRequest{
		Goal:               "swarm idle rearm",
		SwarmIdleTimeoutMs: 220,
		Config:             map[string]string{"mode": "default"},
	}, nil)
	var cr protocol.CreateTaskResponse
	_ = json.Unmarshal(raw, &cr)
	sid := <-sidCh

	eventually(t, 3*time.Second, func() bool {
		rec, err := env.st.GetAttemptRecord(context.Background(), cr.Attempt.AttemptID)
		return err == nil && rec.State == protocol.AttemptSwarmMonitoring
	}, "did not enter swarm_monitoring")

	// Trigger unsolicited permission request while in swarm_monitoring -> transitions to waiting_approval & PAUSES idle timer!
	go func() {
		_, _ = drv.AskPermission(context.Background(), acp.RequestPermissionRequest{
			SessionId: sid,
			ToolCall:  acp.ToolCallUpdate{ToolCallId: "tc-b7-perm", Name: acp.PatchSet("run_command")},
			Options:   []acp.PermissionOption{{OptionId: "allow_once", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce}},
		})
	}()

	var ap protocol.ApprovalView
	eventually(t, 3*time.Second, func() bool {
		list, _ := env.st.ListApprovals(context.Background(), protocol.ApprovalPending)
		if len(list) == 1 {
			ap = list[0]
			return true
		}
		return false
	}, "approval did not reach pending during swarm_monitoring")

	// Wait LONGER than SwarmIdleTimeoutMs (320ms > 220ms) while waiting_approval -> MUST NOT time out!
	time.Sleep(320 * time.Millisecond)
	recMid, _ := env.st.GetAttemptRecord(context.Background(), cr.Attempt.AttemptID)
	if recMid.State != protocol.AttemptWaitingApproval {
		t.Fatalf("swarm idle timer fired prematurely during waiting_approval! state=%s", recMid.State)
	}

	// Approve -> resumes swarm_monitoring and RE-ARMS swarm_idle_timeout -> fires quiet_timeout cleanly!
	env.apiReq(http.MethodPost, "/api/approvals/"+ap.ApprovalID+"/decide", protocol.DecideApprovalRequest{
		OptionID: "allow_once", ExpectedAttemptID: cr.Attempt.AttemptID, ExpectedPayloadSHA256: ap.PayloadSHA256,
	}, nil)

	eventually(t, 3*time.Second, func() bool {
		rec, err := env.st.GetAttemptRecord(context.Background(), cr.Attempt.AttemptID)
		return err == nil && rec.State == protocol.AttemptNeedsVerification &&
			rec.CompletionSource != nil && *rec.CompletionSource == protocol.CompletionQuietTimeout
	}, "swarm idle timer was NOT re-armed after resuming from waiting_approval!")
}

// ============================================================================
// B9 & B10: E2E Steer auto-consumes vs Human Stop keeps paused + Leased Cancel
// ============================================================================
func TestB9_B10_E2E_SteerVsHumanCancelAndLeasedCancel(t *testing.T) {
	t.Run("SteerAutoConsumesWhileHumanStopKeepsQueuePaused", func(t *testing.T) {
		env := newTestEnv(t)
		drv := NewFakeAcpDriver()

		promptEntered := make(chan string, 4)
		drv.OnPrompt = func(ctx context.Context, d *FakeAcpDriver, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
			promptEntered <- promptText
			<-ctx.Done()
			return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
		}

		env.startRunner("hub-node", "", "", drv, nil)
		_, raw := env.apiReq(http.MethodPost, "/api/projects/_scratch/tasks", protocol.CreateTaskRequest{Goal: "turn 1"}, nil)
		var cr protocol.CreateTaskResponse
		_ = json.Unmarshal(raw, &cr)
		tid := cr.Task.TaskID
		<-promptEntered

		// Queue followup and STEER it -> cancels att_1 and AUTOMATICALLY starts att_2 with steered message!
		_, rawF1 := env.apiReq(http.MethodPost, "/api/tasks/"+tid+"/followup", protocol.FollowupTaskRequest{
			Message: "steered urgent msg", Mode: protocol.FollowupModeSteer,
		}, nil)
		var f1 protocol.FollowupTaskResponse
		_ = json.Unmarshal(rawF1, &f1)
		_ = f1

		select {
		case p2 := <-promptEntered:
			if p2 != "steered urgent msg" {
				t.Fatalf("expected steered urgent msg auto-started, got %q", p2)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Steer did not auto-consume steered input after cancelling att_1")
		}

		// Queue another message while att_2 is running, then human clicks STOP (/cancel) -> queue MUST stay paused!
		env.apiReq(http.MethodPost, "/api/tasks/"+tid+"/followup", protocol.FollowupTaskRequest{
			Message: "must stay queued after stop", Mode: protocol.FollowupModeQueue,
		}, nil)
		resp, _ := env.apiReq(http.MethodPost, "/api/tasks/"+tid+"/cancel", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("cancel failed: %d", resp.StatusCode)
		}

		// Wait until task has no active attempt (att_2 cancelled)
		eventually(t, 3*time.Second, func() bool {
			tk, _ := env.st.GetTask(context.Background(), tid)
			return tk.ActiveAttemptID == "" && tk.State == protocol.TaskStateCancelled
		}, "att_2 did not cancel on human stop")

		time.Sleep(100 * time.Millisecond)
		select {
		case unexpected := <-promptEntered:
			t.Fatalf("human stop illegally auto-consumed queued message %q!", unexpected)
		default:
		}
		q, _ := env.st.ListPendingInputs(context.Background(), tid)
		if len(q) != 1 || q[0].Body != "must stay queued after stop" {
			t.Fatalf("expected queued input preserved after human stop, got %+v", q)
		}
	})

	t.Run("B10_LeasedCancelPersistsAcrossDisconnectAndAbortsOnHello", func(t *testing.T) {
		env := newTestEnv(t)
		ctx := context.Background()

		// Connect raw WS runner for mac-node so we control the exact leased window before preparing!
		hdr := http.Header{
			"X-Orbit-Machine-Id": []string{"mac-node"},
			"Authorization":      []string{"Bearer " + env.remoteTok},
		}
		conn1, _, err := websocket.Dial(ctx, env.hubWSURL, &websocket.DialOptions{HTTPHeader: hdr})
		if err != nil {
			t.Fatal(err)
		}
		writeJSON := func(c *websocket.Conn, v any) {
			b, _ := json.Marshal(v)
			if err := c.Write(ctx, websocket.MessageText, b); err != nil {
				t.Fatal(err)
			}
		}
		readJSON := func(c *websocket.Conn, dst any) {
			rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			_, b, err := c.Read(rctx)
			if err != nil {
				t.Fatal(err)
			}
			_ = json.Unmarshal(b, dst)
		}

		writeJSON(conn1, protocol.RunnerHelloFrame{
			Type: protocol.RunnerMsgHello, ProtocolVersion: 1, MachineID: "mac-node", RunnerInstanceID: "run_b10", RunnerVersion: "0.2.0",
		})
		var w1 protocol.HubWelcomeFrame
		readJSON(conn1, &w1)

		// Create task -> Hub dispatches attempt (state=leased, runner_instance_id=run_b10)
		_, raw := env.apiReq(http.MethodPost, "/api/projects/demo-app/tasks", protocol.CreateTaskRequest{
			Goal: "leased cancel window", RequiredCapability: "macos-env",
		}, nil)
		var cr protocol.CreateTaskResponse
		_ = json.Unmarshal(raw, &cr)
		var disp protocol.AttemptDispatchFrame
		readJSON(conn1, &disp)

		// Disconnect BEFORE runner ever reports preparing -> human cancels -> MUST persist cancel_requested=1!
		conn1.CloseNow()
		eventually(t, 2*time.Second, func() bool {
			return !env.srv.RunnerGateway().IsConnected("mac-node")
		}, "hub did not see mac-node disconnect")

		resp, rawCancel := env.apiReq(http.MethodPost, "/api/tasks/"+cr.Task.TaskID+"/cancel", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("cancel leased failed (%d): %s", resp.StatusCode, string(rawCancel))
		}
		attAfterCancel, _ := env.st.GetAttemptRecord(ctx, cr.Attempt.AttemptID)
		if !attAfterCancel.CancelRequested || attAfterCancel.State != protocol.AttemptLeased {
			t.Fatalf("expected leased attempt with cancel_requested=true until runner confirms, got state=%s cancelReq=%v",
				attAfterCancel.State, attAfterCancel.CancelRequested)
		}

		// Runner reconnects reporting live leased attempt -> hub.welcome MUST include abortAttemptIds=[attID]!
		conn2, _, err := websocket.Dial(ctx, env.hubWSURL, &websocket.DialOptions{HTTPHeader: hdr})
		if err != nil {
			t.Fatal(err)
		}
		defer conn2.CloseNow()
		writeJSON(conn2, protocol.RunnerHelloFrame{
			Type: protocol.RunnerMsgHello, ProtocolVersion: 1, MachineID: "mac-node", RunnerInstanceID: "run_b10", RunnerVersion: "0.2.0",
			LiveAttempts: []protocol.LiveAttemptInfo{{AttemptID: disp.AttemptID, LeaseToken: disp.LeaseToken, State: protocol.AttemptLeased}},
		})
		var w2 protocol.HubWelcomeFrame
		readJSON(conn2, &w2)
		if len(w2.Reconcile.AbortAttemptIDs) != 1 || w2.Reconcile.AbortAttemptIDs[0] != disp.AttemptID {
			t.Fatalf("expected AbortAttemptIDs=[%s] on hello, got %+v", disp.AttemptID, w2.Reconcile)
		}
	})
}

// ============================================================================
// B12 & B13: Attachment blob upload + Push failure retry / --resume atomic emit
// ============================================================================
func TestB12_B13_E2E_AttachBlobUploadAndResumeRetry(t *testing.T) {
	env := newTestEnv(t)
	drv := NewFakeAcpDriver()

	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	drv.OnPrompt = func(ctx context.Context, d *FakeAcpDriver, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		case <-ctx.Done():
			return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
		}
	}

	r := env.startRunner("hub-node", "", "", drv, nil)
	_, raw := env.apiReq(http.MethodPost, "/api/projects/demo-app/tasks", protocol.CreateTaskRequest{Goal: "attach + resume"}, nil)
	var cr protocol.CreateTaskResponse
	_ = json.Unmarshal(raw, &cr)
	<-entered

	eventually(t, 2*time.Second, func() bool {
		rec, err := env.st.GetAttemptRecord(context.Background(), cr.Attempt.AttemptID)
		return err == nil && rec.WorktreePath != ""
	}, "worktree path not set")
	rec, _ := env.st.GetAttemptRecord(context.Background(), cr.Attempt.AttemptID)

	// Create external binary attachment outside worktree
	extFile := filepath.Join(t.TempDir(), "trace.bin")
	_ = os.WriteFile(extFile, []byte{0x00, 0x01, 0x02, 0x03, 0xff}, 0o644)
	_ = os.WriteFile(filepath.Join(rec.WorktreePath, "app.go"), []byte("package main\n// with attach\nfunc main() {}\n"), 0o644)

	// Temporarily replace bare mirror path with a blocking file to simulate push failure on first `orbit deliver --final`!
	proj, _ := env.st.GetProject(context.Background(), "demo-app")
	movedMirror := proj.MirrorBarePath + ".bak"
	if err := os.Rename(proj.MirrorBarePath, movedMirror); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(proj.MirrorBarePath, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(movedMirror); err == nil {
			_ = os.Remove(proj.MirrorBarePath)
			_ = os.Rename(movedMirror, proj.MirrorBarePath)
		}
	})

	failResp, err := r.HandleLocalDeliver(context.Background(), runner.LocalDeliverRequest{
		AttemptID:   cr.Attempt.AttemptID,
		Summary:     "Delivery with blob",
		Attachments: []gitops.AttachmentInput{{TargetPath: "artifacts/trace.bin", LocalPath: extFile}},
		Final:       true,
	})
	if err == nil || failResp.DeliveryID == "" || failResp.HeadSHA == "" {
		t.Fatalf("expected push failure returning staged deliveryId+headSha, resp=%+v err=%v", failResp, err)
	}

	// Restore mirror -> retry with `--resume --delivery-id` MUST reuse EXACT deliveryId & headSha + upload blob!
	if err := os.Remove(proj.MirrorBarePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(movedMirror, proj.MirrorBarePath); err != nil {
		t.Fatal(err)
	}
	okResp, err := r.HandleLocalDeliver(context.Background(), runner.LocalDeliverRequest{
		AttemptID:  cr.Attempt.AttemptID,
		DeliveryID: failResp.DeliveryID,
		Resume:     true,
		Final:      true,
	})
	if err != nil {
		t.Fatalf("resume HandleLocalDeliver failed: %v", err)
	}
	if okResp.DeliveryID != failResp.DeliveryID || okResp.HeadSHA != failResp.HeadSHA {
		t.Fatalf("resume generated new delivery/SHA! want (%s, %s), got (%s, %s)",
			failResp.DeliveryID, failResp.HeadSHA, okResp.DeliveryID, okResp.HeadSHA)
	}
	close(release)

	eventually(t, 3*time.Second, func() bool {
		attNow, err := env.st.GetAttemptRecord(context.Background(), cr.Attempt.AttemptID)
		return err == nil && attNow.State == protocol.AttemptSucceeded
	}, "attempt did not succeed after resumed delivery")

	// Verify Hub persisted delivery with verified blob manifest!
	dlv, _, _, _, err := env.st.GetDeliveryDetail(context.Background(), okResp.DeliveryID)
	if err != nil {
		t.Fatal(err)
	}
	if len(dlv.Manifest.Blobs) != 1 || dlv.Manifest.Blobs[0].Path != "artifacts/trace.bin" || dlv.Manifest.Blobs[0].Size != 5 {
		t.Fatalf("expected verified blob manifest on Hub delivery, got %+v", dlv.Manifest.Blobs)
	}
}

// ============================================================================
// B16, B18, S1: Live Swarm Board Forward + Unsupported Adapter + MaxLiveWorkers
// ============================================================================
func TestB16_B18_S1_BoardForwardUnsupportedAdapterAndMaxWorkers(t *testing.T) {
	t.Run("B16_LiveSwarmBoardMessageForwardedAndAcked", func(t *testing.T) {
		env := newTestEnv(t)
		drv := NewFakeAcpDriver()

		forwardedCh := make(chan string, 1)
		var count atomic.Int32
		drv.OnPrompt = func(ctx context.Context, d *FakeAcpDriver, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
			if count.Add(1) == 1 {
				_ = d.EmitUpdate(ctx, sid, acp.SessionUpdate{
					ToolCall: &acp.SessionUpdateToolCall{
						ToolCallId: "tc-b16-add",
						Name:       acp.Ptr("swarm"),
						Status:     acp.ToolCallStatusCompleted,
						RawInput:   json.RawMessage(`{"action":"add","name":"ui-coder"}`),
					},
				})
				return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
			}
			forwardedCh <- promptText
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}

		env.startRunner("hub-node", "", "", drv, nil)
		_, raw := env.apiReq(http.MethodPost, "/api/projects/demo-app/tasks", protocol.CreateTaskRequest{Goal: "board forward"}, nil)
		var cr protocol.CreateTaskResponse
		_ = json.Unmarshal(raw, &cr)

		eventually(t, 3*time.Second, func() bool {
			rec, err := env.st.GetAttemptRecord(context.Background(), cr.Attempt.AttemptID)
			return err == nil && rec.State == protocol.AttemptSwarmMonitoring
		}, "did not enter swarm_monitoring")

		resp, rawMsg := env.apiReq(http.MethodPost, "/api/tasks/"+cr.Task.TaskID+"/board/messages", protocol.PostBoardMessageRequest{
			AuthorName:    "human",
			TargetMention: "@ui-coder",
			Body:          "Switch button color to blue",
		}, nil)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("post board message failed (%d): %s", resp.StatusCode, string(rawMsg))
		}
		var posted protocol.BoardMessage
		_ = json.Unmarshal(rawMsg, &posted)

		select {
		case gotPrompt := <-forwardedCh:
			if !strings.Contains(gotPrompt, "Switch button color to blue") || !strings.Contains(gotPrompt, "@ui-coder") {
				t.Fatalf("unexpected forwarded board prompt: %q", gotPrompt)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("live swarm coordinator never received forwarded board message")
		}

		// Hub MUST mark board message consumed upon receiving OutboxFrameBoardAck!
		eventually(t, 3*time.Second, func() bool {
			b, err := env.st.GetTaskBoard(context.Background(), cr.Task.TaskID)
			if err != nil {
				return false
			}
			for _, m := range b.Messages {
				if m.MessageID == posted.MessageID && m.Status == protocol.BoardMessageConsumed {
					return true
				}
			}
			return false
		}, "board message was not marked consumed after runner board_message_ack")
	})

	t.Run("B18_RunnerRejectsUnsupportedAdapter", func(t *testing.T) {
		env := newTestEnv(t)
		drv := NewFakeAcpDriver()
		r := env.startRunner("hub-node", "", "", drv, nil)

		r.HandleDispatch(protocol.AttemptDispatchFrame{
			AttemptID:  "att_bad_adapter",
			TaskID:     "tsk_bad",
			ProjectID:  "_scratch",
			LeaseToken: "tok_bad",
			Adapter:    protocol.AdapterCodex,
			Prompt:     "noop",
		})
		if err := r.WaitAttemptDone(context.Background(), "att_bad_adapter"); err != nil {
			t.Fatal(err)
		}
		drv.mu.Lock()
		prompted := len(drv.promptCalls)
		drv.mu.Unlock()
		if prompted != 0 {
			t.Fatalf("unsupported adapter should never invoke driver Prompt, got %d calls", prompted)
		}
	})

	t.Run("S1_MaxLiveWorkersEvictsIdleSession", func(t *testing.T) {
		env := newTestEnv(t)
		drv := NewFakeAcpDriver()
		r := env.startRunner("hub-node", "", "", drv, func(c *runner.Config) {
			c.MaxLiveWorkers = 1
		})

		// Run session 1 on _scratch -> stays live idle
		r.HandleDispatch(protocol.AttemptDispatchFrame{
			AttemptID: "att_s1_1", TaskID: "tsk_s1_1", ProjectID: "_scratch", LeaseToken: "tok1",
			Adapter: protocol.AdapterAgy, WBSessionID: "ses_s1_1", Prompt: "hello 1",
		})
		_ = r.WaitAttemptDone(context.Background(), "att_s1_1")
		_, sid1, _, _ := r.SessionStateSnapshot("ses_s1_1")

		// Run session 2 -> MaxLiveWorkers=1 MUST evict idle ses_s1_1 via CloseSession!
		r.HandleDispatch(protocol.AttemptDispatchFrame{
			AttemptID: "att_s1_2", TaskID: "tsk_s1_2", ProjectID: "_scratch", LeaseToken: "tok2",
			Adapter: protocol.AdapterAgy, WBSessionID: "ses_s1_2", Prompt: "hello 2",
		})
		_ = r.WaitAttemptDone(context.Background(), "att_s1_2")

		drv.mu.Lock()
		closed := append([]acp.SessionId(nil), drv.closeCalls...)
		drv.mu.Unlock()
		foundClosed := false
		for _, c := range closed {
			if c == sid1 {
				foundClosed = true
			}
		}
		if !foundClosed {
			t.Fatalf("expected MaxLiveWorkers=1 to evict idle sid1=%s via CloseSession, closeCalls=%v", sid1, closed)
		}
	})
}

// ============================================================================
// S10: Dual-round combined Diff + Ryugi annotation dispatch -> v2 addressed, v1 preserved
// ============================================================================
func TestS10_E2E_DualRoundCombinedDiffAndRyugiAnnotations(t *testing.T) {
	env := newTestEnv(t)
	drv := NewFakeAcpDriver()

	var r *runner.Runner
	var curAttemptID atomic.Value
	var round2Prompt atomic.Value
	var roundNum atomic.Int32

	drv.OnPrompt = func(ctx context.Context, d *FakeAcpDriver, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
		n := roundNum.Add(1)
		var aid string
		eventually(t, 2*time.Second, func() bool {
			v, _ := curAttemptID.Load().(string)
			if v == "" {
				return false
			}
			rec, err := env.st.GetAttemptRecord(context.Background(), v)
			if err == nil && rec.WorktreePath != "" {
				aid = v
				return true
			}
			return false
		}, "attempt worktree not ready")
		rec, _ := env.st.GetAttemptRecord(context.Background(), aid)

		if n == 1 {
			_ = os.WriteFile(filepath.Join(rec.WorktreePath, "app.go"), []byte("package main\n\nfunc Greet() string { return \"v1\" }\n"), 0o644)
			_ = os.MkdirAll(filepath.Join(rec.WorktreePath, "docs"), 0o755)
			_ = os.WriteFile(filepath.Join(rec.WorktreePath, "docs", "spec.ryugi.md"), []byte("# Spec v1\n"), 0o644)
			_, err := r.HandleLocalDeliver(context.Background(), runner.LocalDeliverRequest{
				AttemptID: aid, Summary: "Round 1 delivery", Final: true,
			})
			if err != nil {
				t.Errorf("round 1 deliver failed: %v", err)
			}
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}

		round2Prompt.Store(promptText)
		_ = os.WriteFile(filepath.Join(rec.WorktreePath, "app.go"), []byte("package main\n\nfunc Greet() string { return \"v2-fixed\" }\n"), 0o644)
		_ = os.WriteFile(filepath.Join(rec.WorktreePath, "docs", "spec.ryugi.md"), []byte("# Spec v2 updated\n"), 0o644)
		_, err := r.HandleLocalDeliver(context.Background(), runner.LocalDeliverRequest{
			AttemptID: aid, Summary: "Round 2 fixes Diff + Ryugi", Final: true,
		})
		if err != nil {
			t.Errorf("round 2 deliver failed: %v", err)
		}
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	r = env.startRunner("hub-node", "", "", drv, nil)

	// Round 1
	_, raw := env.apiReq(http.MethodPost, "/api/projects/demo-app/tasks", protocol.CreateTaskRequest{Goal: "Build v1"}, nil)
	var c1 protocol.CreateTaskResponse
	_ = json.Unmarshal(raw, &c1)
	tid := c1.Task.TaskID
	curAttemptID.Store(c1.Attempt.AttemptID)

	eventually(t, 4*time.Second, func() bool {
		rec, err := env.st.GetAttemptRecord(context.Background(), c1.Attempt.AttemptID)
		return err == nil && rec.State == protocol.AttemptSucceeded && rec.OutputDeliveryID != ""
	}, "round 1 did not succeed with delivery")
	att1Rec, _ := env.st.GetAttemptRecord(context.Background(), c1.Attempt.AttemptID)
	dlv1ID := att1Rec.OutputDeliveryID

	// Create BOTH a Diff annotation AND a Ryugi annotation anchored on dlv1!
	respD, rawD := env.apiReq(http.MethodPost, "/api/deliveries/"+dlv1ID+"/annotations", protocol.CreateAnnotationRequest{
		Source: protocol.AnnotationSourceDiffDrawer,
		Anchor: protocol.AnnotationAnchor{Path: "app.go", StartLine: 3, EndLine: 3},
		Body:   "Return v2-fixed instead of v1",
	}, nil)
	if respD.StatusCode != http.StatusCreated {
		t.Fatalf("create diff ann failed: %s", string(rawD))
	}
	var annDiff protocol.CreateAnnotationResponse
	_ = json.Unmarshal(rawD, &annDiff)

	respR, rawR := env.apiReq(http.MethodPost, "/api/deliveries/"+dlv1ID+"/annotations", protocol.CreateAnnotationRequest{
		Source: protocol.AnnotationSourceRyugi,
		Anchor: protocol.AnnotationAnchor{Artifact: "docs/spec.ryugi.md", Section: "Intro"},
		Body:   "Update spec heading to v2",
	}, nil)
	if respR.StatusCode != http.StatusCreated {
		t.Fatalf("create ryugi ann failed: %s", string(rawR))
	}
	var annRyugi protocol.CreateAnnotationResponse
	_ = json.Unmarshal(rawR, &annRyugi)

	// Dispatch BOTH Diff + Ryugi annotations in ONE batch -> starts Round 2!
	curAttemptID.Store("")
	respDisp, rawDisp := env.apiReq(http.MethodPost, "/api/tasks/"+tid+"/dispatch-annotations", protocol.DispatchAnnotationsRequest{
		AnnotationIDs: []string{annDiff.Annotation.AnnotationID, annRyugi.Annotation.AnnotationID},
		Mode:          protocol.FollowupModeQueue,
	}, nil)
	if respDisp.StatusCode != http.StatusOK {
		t.Fatalf("combined dispatch-annotations failed (%d): %s", respDisp.StatusCode, string(rawDisp))
	}
	var disp protocol.DispatchAnnotationsResponse
	_ = json.Unmarshal(rawDisp, &disp)
	if disp.QueuedCount != 2 || disp.Attempt == nil {
		t.Fatalf("expected 2 annotations dispatched into Round 2 attempt, got %+v", disp)
	}
	curAttemptID.Store(disp.Attempt.AttemptID)

	eventually(t, 4*time.Second, func() bool {
		rec, err := env.st.GetAttemptRecord(context.Background(), disp.Attempt.AttemptID)
		return err == nil && rec.State == protocol.AttemptSucceeded && rec.OutputDeliveryID != ""
	}, "round 2 did not succeed with delivery")
	att2Rec, _ := env.st.GetAttemptRecord(context.Background(), disp.Attempt.AttemptID)
	dlv2ID := att2Rec.OutputDeliveryID

	// Verify Round 2 prompt combined BOTH Diff and Ryugi feedback
	p2, _ := round2Prompt.Load().(string)
	if !strings.Contains(p2, "Return v2-fixed") || !strings.Contains(p2, "Update spec heading") {
		t.Fatalf("round 2 prompt missing combined Diff+Ryugi annotations: %q", p2)
	}

	// Verify BOTH annotations on v1 are now addressed with resolutionDeliveryId == dlv2ID!
	tl, _ := env.st.GetTaskTimeline(context.Background(), tid, 0, false)
	if len(tl.Annotations) != 2 {
		t.Fatalf("expected 2 annotations on timeline, got %+v", tl.Annotations)
	}
	for _, a := range tl.Annotations {
		if a.Status != protocol.AnnotationAddressed || a.ResolutionDeliveryID != dlv2ID {
			t.Fatalf("annotation %s not addressed by dlv2 %s: %+v", a.AnnotationID, dlv2ID, a)
		}
	}

	// Verify v1 frozen delivery & diff remain intact and reopenable!
	respV1, rawV1 := env.apiReq(http.MethodGet, "/api/deliveries/"+dlv1ID+"/diff", nil, nil)
	if respV1.StatusCode != http.StatusOK || !strings.Contains(string(rawV1), `return \"v1\"`) {
		t.Fatalf("v1 delivery diff not preserved/reopenable (%d): %s", respV1.StatusCode, string(rawV1))
	}
}
