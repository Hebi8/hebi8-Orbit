package runner_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/coder/websocket"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/runner"
)

func TestRunner_ConcurrentInPlaceDoesNotMisattributeToReadOnlyAttempt(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	sharedDir := filepath.Join(tmp, "shared-workspace")
	if err := os.MkdirAll(sharedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sharedDir, "seed.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var entered atomic.Int32
	bothInside := make(chan struct{})
	var bothOnce sync.Once
	wroteFile := make(chan struct{})

	drv := NewFakeAcpDriver()
	drv.OnPrompt = func(pctx context.Context, d *FakeAcpDriver, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
		if entered.Add(1) == 2 {
			bothOnce.Do(func() { close(bothInside) })
		}
		select {
		case <-bothInside:
		case <-time.After(4 * time.Second):
			t.Errorf("concurrent in-place attempts did not overlap (location lock regression)")
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}

		if strings.Contains(promptText, "READ_ONLY") {
			// Emit strictly read-only tool calls and wait until the writer attempt modifies the folder.
			_ = d.EmitUpdate(pctx, sid, acp.SessionUpdate{
				ToolCall: &acp.SessionUpdateToolCall{
					ToolCallId: "tc_ro_1",
					Name:       acp.Ptr("read_file"),
					Title:      "Read seed.txt",
					Kind:       acp.ToolKindRead,
					Status:     acp.ToolCallStatusCompleted,
				},
			})
			_ = d.EmitUpdate(pctx, sid, acp.SessionUpdate{
				ToolCall: &acp.SessionUpdateToolCall{
					ToolCallId: "tc_ro_2",
					Name:       acp.Ptr("grep_search"),
					Title:      "Search codebase",
					Kind:       acp.ToolKindSearch,
					Status:     acp.ToolCallStatusCompleted,
				},
			})
			select {
			case <-wroteFile:
			case <-time.After(4 * time.Second):
				t.Errorf("timed out waiting for writer to modify shared dir")
			}
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}

		// Writer attempt executes a write tool and creates a new file while overlapping.
		_ = d.EmitUpdate(pctx, sid, acp.SessionUpdate{
			ToolCall: &acp.SessionUpdateToolCall{
				ToolCallId: "tc_wr_1",
				Name:       acp.Ptr("write_file"),
				Title:      "Write created_by_writer.txt",
				Kind:       acp.ToolKindEdit,
				Status:     acp.ToolCallStatusCompleted,
			},
		})
		if err := os.WriteFile(filepath.Join(sharedDir, "created_by_writer.txt"), []byte("line1\nline2\n"), 0o644); err != nil {
			t.Errorf("write file failed: %v", err)
		}
		close(wroteFile)
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	r, err := runner.New(ctx, runner.Config{
		MachineID: "hub-node",
		DataDir:   tmp,
		Driver:    drv,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(context.Background()) }()

	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:     "att_ro",
		TaskID:        "tsk_ro",
		ProjectID:     "proj_shared",
		LeaseToken:    "lease_ro",
		Adapter:       protocol.AdapterAgy,
		WBSessionID:   "ses_ro",
		LocationID:    "loc_shared",
		Isolation:     protocol.IsolationInPlace,
		IsPlainFolder: true,
		RepoPath:      sharedDir,
		WorktreePath:  sharedDir,
		Prompt:        "READ_ONLY research task",
	})
	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:     "att_wr",
		TaskID:        "tsk_wr",
		ProjectID:     "proj_shared",
		LeaseToken:    "lease_wr",
		Adapter:       protocol.AdapterAgy,
		WBSessionID:   "ses_wr",
		LocationID:    "loc_shared",
		Isolation:     protocol.IsolationInPlace,
		IsPlainFolder: true,
		RepoPath:      sharedDir,
		WorktreePath:  sharedDir,
		Prompt:        "WRITER modify task",
	})

	if err := r.WaitAttemptDone(ctx, "att_ro"); err != nil {
		t.Fatal(err)
	}
	if err := r.WaitAttemptDone(ctx, "att_wr"); err != nil {
		t.Fatal(err)
	}

	findFolderChanges := func(attemptID string) *protocol.FolderChangesSummary {
		_, items, err := r.Outbox().ReadUnacked(ctx, attemptID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if it.FrameType == protocol.OutboxFrameState && it.Payload["state"] == string(protocol.AttemptSucceeded) {
				if rawFC, ok := it.Payload["folderChanges"]; ok && rawFC != nil {
					b, _ := json.Marshal(rawFC)
					var fc protocol.FolderChangesSummary
					if json.Unmarshal(b, &fc) == nil {
						return &fc
					}
				}
			}
		}
		return nil
	}

	if fcRO := findFolderChanges("att_ro"); fcRO != nil {
		t.Fatalf("pure read-only overlapping attempt MUST NOT receive another attempt's folderChanges: %+v", fcRO)
	}
	fcWR := findFolderChanges("att_wr")
	if fcWR == nil || len(fcWR.Files) == 0 {
		t.Fatalf("writer overlapping attempt expected folderChanges with created_by_writer.txt, got %+v", fcWR)
	}
	if !fcWR.ConcurrentSharedDir || strings.TrimSpace(fcWR.ConcurrentNotice) == "" {
		t.Fatalf("writer overlapping attempt expected ConcurrentSharedDir=true and non-empty notice, got %+v", fcWR)
	}

	// Non-overlapping subsequent attempt writing another file should NOT carry concurrent overlap notice.
	drv.OnPrompt = func(pctx context.Context, d *FakeAcpDriver, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
		_ = d.EmitUpdate(pctx, sid, acp.SessionUpdate{
			ToolCall: &acp.SessionUpdateToolCall{
				ToolCallId: "tc_solo_1",
				Name:       acp.Ptr("write_file"),
				Kind:       acp.ToolKindEdit,
				Status:     acp.ToolCallStatusCompleted,
			},
		})
		_ = os.WriteFile(filepath.Join(sharedDir, "solo.txt"), []byte("solo\n"), 0o644)
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:     "att_solo",
		TaskID:        "tsk_solo",
		ProjectID:     "proj_shared",
		LeaseToken:    "lease_solo",
		Adapter:       protocol.AdapterAgy,
		WBSessionID:   "ses_solo",
		LocationID:    "loc_shared",
		Isolation:     protocol.IsolationInPlace,
		IsPlainFolder: true,
		RepoPath:      sharedDir,
		WorktreePath:  sharedDir,
		Prompt:        "solo write",
	})
	if err := r.WaitAttemptDone(ctx, "att_solo"); err != nil {
		t.Fatal(err)
	}
	fcSolo := findFolderChanges("att_solo")
	if fcSolo == nil || len(fcSolo.Files) != 1 || fcSolo.Files[0].Path != "solo.txt" {
		t.Fatalf("expected solo.txt change on non-overlapping attempt, got %+v", fcSolo)
	}
	if fcSolo.ConcurrentSharedDir || fcSolo.ConcurrentNotice != "" {
		t.Fatalf("non-overlapping attempt should not set ConcurrentSharedDir/ConcurrentNotice, got %+v", fcSolo)
	}

	// Nested in-place directories (parent repo dir vs child subdir) MUST also be treated as overlapping.
	subDir := filepath.Join(sharedDir, "subpkg")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var nestedEntered atomic.Int32
	nestedBoth := make(chan struct{})
	var nestedOnce sync.Once
	nestedWrote := make(chan struct{})
	drv.OnPrompt = func(pctx context.Context, d *FakeAcpDriver, sid acp.SessionId, promptText string) (acp.PromptResponse, error) {
		if nestedEntered.Add(1) == 2 {
			nestedOnce.Do(func() { close(nestedBoth) })
		}
		select {
		case <-nestedBoth:
		case <-time.After(4 * time.Second):
			t.Errorf("nested concurrent attempts did not overlap")
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}
		if strings.Contains(promptText, "PARENT_RO") {
			_ = d.EmitUpdate(pctx, sid, acp.SessionUpdate{
				ToolCall: &acp.SessionUpdateToolCall{
					ToolCallId: "tc_nested_ro",
					Name:       acp.Ptr("read_file"),
					Kind:       acp.ToolKindRead,
					Status:     acp.ToolCallStatusCompleted,
				},
			})
			<-nestedWrote
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}
		_ = d.EmitUpdate(pctx, sid, acp.SessionUpdate{
			ToolCall: &acp.SessionUpdateToolCall{
				ToolCallId: "tc_nested_wr",
				Name:       acp.Ptr("write_file"),
				Kind:       acp.ToolKindEdit,
				Status:     acp.ToolCallStatusCompleted,
			},
		})
		_ = os.WriteFile(filepath.Join(subDir, "nested.txt"), []byte("nested\n"), 0o644)
		close(nestedWrote)
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:     "att_nested_ro",
		TaskID:        "tsk_nested_ro",
		ProjectID:     "proj_shared",
		LeaseToken:    "lease_nested_ro",
		Adapter:       protocol.AdapterAgy,
		WBSessionID:   "ses_nested_ro",
		Isolation:     protocol.IsolationInPlace,
		IsPlainFolder: true,
		RepoPath:      sharedDir,
		WorktreePath:  sharedDir,
		Prompt:        "PARENT_RO",
	})
	r.HandleDispatch(protocol.AttemptDispatchFrame{
		AttemptID:     "att_nested_wr",
		TaskID:        "tsk_nested_wr",
		ProjectID:     "proj_shared",
		LeaseToken:    "lease_nested_wr",
		Adapter:       protocol.AdapterAgy,
		WBSessionID:   "ses_nested_wr",
		Isolation:     protocol.IsolationInPlace,
		IsPlainFolder: true,
		RepoPath:      subDir,
		WorktreePath:  subDir,
		Prompt:        "SUB_WR",
	})
	if err := r.WaitAttemptDone(ctx, "att_nested_ro"); err != nil {
		t.Fatal(err)
	}
	if err := r.WaitAttemptDone(ctx, "att_nested_wr"); err != nil {
		t.Fatal(err)
	}
	if fc := findFolderChanges("att_nested_ro"); fc != nil {
		t.Fatalf("nested parent pure read-only attempt must not receive child subdir writes: %+v", fc)
	}
	if fc := findFolderChanges("att_nested_wr"); fc == nil || !fc.ConcurrentSharedDir {
		t.Fatalf("nested child writer expected ConcurrentSharedDir=true, got %+v", fc)
	}
}

func TestRunner_ProbeQuotasRPCDoesNotSendDuplicateQuotasReport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Fake Claude OAuth server returning deterministic quota.
	claudeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":20,"resets_at":"2099-01-01T00:00:00Z"}}`))
	}))
	defer claudeSrv.Close()

	var quotaReportCount atomic.Int32
	initialReportCh := make(chan struct{}, 1)
	rpcDone := make(chan protocol.RunnerRPCResultFrame, 1)

	hubWS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		// Read runner.hello
		_, _, err = conn.Read(ctx)
		if err != nil {
			return
		}
		// Send hub.welcome
		welcome, _ := json.Marshal(protocol.HubWelcomeFrame{
			Type:                protocol.RunnerMsgHubWelcome,
			ServerTime:          time.Now().UnixMilli(),
			HeartbeatIntervalMs: 30000,
			LeaseTtlMs:          45000,
		})
		_ = conn.Write(ctx, websocket.MessageText, welcome)

		// Wait for the initial connect-time quotas_report to finish, reset counter, then trigger probe_quotas RPC.
		go func() {
			select {
			case <-initialReportCh:
			case <-time.After(3 * time.Second):
			}
			quotaReportCount.Store(0)
			rpcFrame, _ := json.Marshal(protocol.RunnerRPCFrame{
				Type:      protocol.RunnerMsgRPC,
				RequestID: "rpc_q_1",
				Op:        "probe_quotas",
				Args:      map[string]any{"force": true},
			})
			_ = conn.Write(ctx, websocket.MessageText, rpcFrame)
		}()

		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var env struct {
				Type protocol.RunnerWSMessageType `json:"type"`
			}
			if json.Unmarshal(data, &env) != nil {
				continue
			}
			switch env.Type {
			case protocol.RunnerMsgQuotasReport:
				quotaReportCount.Add(1)
				select {
				case initialReportCh <- struct{}{}:
				default:
				}
			case protocol.RunnerMsgRPCResult:
				var res protocol.RunnerRPCResultFrame
				if json.Unmarshal(data, &res) == nil && res.RequestID == "rpc_q_1" {
					rpcDone <- res
				}
			}
		}
	}))
	defer hubWS.Close()

	// Use fast non-existent binary path so local host agy/codex binaries are never invoked.
	missingBin := filepath.Join(t.TempDir(), "nonexistent-cli")
	qc := runner.NewQuotaCollector(runner.QuotaCollectorConfig{
		ResolveMainCLI: func(protocol.Adapter) string { return missingBin },
		ClaudeTokenFn:  func(context.Context) string { return "tok_test" },
		ClaudeUsageURL: claudeSrv.URL,
	})
	drv := NewFakeAcpDriver()
	r, err := runner.New(ctx, runner.Config{
		MachineID:      "hub-node",
		HubWSURL:       "ws" + strings.TrimPrefix(hubWS.URL, "http"),
		DataDir:        t.TempDir(),
		Driver:         drv,
		QuotaCollector: qc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(context.Background()) }()

	select {
	case res := <-rpcDone:
		if !res.OK {
			t.Fatalf("probe_quotas RPC failed: %+v", res)
		}
		if _, ok := res.Data["agentQuotas"]; !ok {
			t.Fatalf("expected agentQuotas in RPC response, got %+v", res.Data)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for probe_quotas RPC response")
	}

	// Give any stray goroutine time to send a duplicate runner.quotas_report if buggy.
	time.Sleep(150 * time.Millisecond)
	if n := quotaReportCount.Load(); n != 0 {
		t.Fatalf("probe_quotas RPC must NOT emit a duplicate runner.quotas_report frame, got %d", n)
	}
}
