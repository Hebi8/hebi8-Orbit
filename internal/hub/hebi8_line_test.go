package hub_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Hebi8/hebi8-Orbit/internal/hub"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

func TestBuildHebi8FoldOpening_SixSectionsDeterminismAndBudget(t *testing.T) {
	now := time.Date(2026, 11, 26, 10, 30, 0, 0, time.UTC)

	nodes := []protocol.Hebi8LineNode{
		{
			V:          1,
			ID:         "seg_00000001",
			Kind:       protocol.Hebi8NodeSegment,
			Seq:        1,
			CoversFrom: "2026-11-20T09:00:00Z",
			CoversTo:   "2026-11-20T10:00:00Z",
			Level:      0,
			Title:      "早期架构定稿",
			Tags:       []string{"arch"},
			Pinned:     true,
			AttemptRange: &protocol.Hebi8AttemptRange{
				StartAttemptID: "att_1", EndAttemptID: "att_2", StartAttemptNo: 1, EndAttemptNo: 2, TurnCount: 2,
			},
		},
		{
			V:          1,
			ID:         "fld_aaaa1111",
			Kind:       protocol.Hebi8NodeFold,
			Seq:        6,
			CoversFrom: "2026-11-21T09:00:00Z",
			CoversTo:   "2026-11-23T18:00:00Z",
			Covers:     []string{"seg_00000002", "seg_00000003", "seg_00000004", "seg_00000005", "seg_00000006"},
			Level:      1,
			Title:      "多机配对与路由重构阶段",
			Tags:       []string{"pairing", "route"},
		},
		{
			V:          1,
			ID:         "seg_00000007",
			Kind:       protocol.Hebi8NodeSegment,
			Seq:        7,
			CoversFrom: "2026-11-24T11:00:00Z",
			CoversTo:   "2026-11-24T12:15:00Z",
			Level:      0,
			Title:      "梳理桌面端菜单快捷键",
			Tags:       []string{"desktop"},
			AttemptRange: &protocol.Hebi8AttemptRange{
				StartAttemptID: "att_10", EndAttemptID: "att_12", StartAttemptNo: 10, EndAttemptNo: 12, TurnCount: 3,
			},
		},
		{
			V:          1,
			ID:         "seg_00000008",
			Kind:       protocol.Hebi8NodeSegment,
			Seq:        8,
			CoversFrom: "2026-11-25T14:00:00Z",
			CoversTo:   "2026-11-25T15:30:00Z",
			Level:      0,
			Title:      "ACP 真实会话验证",
			Tags:       []string{"acp", "test"},
			AttemptRange: &protocol.Hebi8AttemptRange{
				StartAttemptID: "att_13", EndAttemptID: "att_15", StartAttemptNo: 13, EndAttemptNo: 15, TurnCount: 3,
			},
		},
		{
			V:          1,
			ID:         "seg_00000009",
			Kind:       protocol.Hebi8NodeSegment,
			Seq:        9,
			CoversFrom: "2026-11-26T08:00:00Z",
			CoversTo:   "2026-11-26T09:45:00Z",
			Level:      0,
			Title:      "确认 Fold-Rollover 规格",
			Tags:       []string{"hebi8", "fold"},
			AttemptRange: &protocol.Hebi8AttemptRange{
				StartAttemptID: "att_16", EndAttemptID: "att_19", StartAttemptNo: 16, EndAttemptNo: 19, TurnCount: 4,
			},
		},
	}

	summaries := map[string]string{
		"seg_00000001": "我记录了最终确定的对等设备签名账本原则。这是不可动摇的长期基线。",
		"fld_aaaa1111": "我在这一阶段完成了配对码校验与目录同步逻辑收口。所有集成测试全部通过。",
		"seg_00000007": "这部分是较早的未折叠分段，不应全量展开正文，只保留一行索引即可。",
		"seg_00000008": "我核对了真实 ACP 会话流式输出与思考步骤渲染。确认默认权限模式为 auto。",
		"seg_00000009": "我与你敲定了 Hebi8 单一主对话与 Fold-Rollover 六段确定性开场方案。接下来进入测试验收。",
	}

	in := hub.Hebi8FoldOpeningInput{
		WorkspaceName:     "Home LAN",
		Now:               now,
		Nodes:             nodes,
		Summaries:         summaries,
		Pins:              []string{"seg_00000001"},
		WorkspaceSnapshot: "<orbit_workspace>\nWorkspace: Home LAN\nProjects:\n- Demo App (id=demo-app, active=1)\n</orbit_workspace>",
		RecentUserPrompts: []string{
			"第一条最近原话：先看下 ACP 会话复用问题。",
			"第二条最近原话：确认一下 pins 上限是不是 8 个。",
			"第三条最近原话：换代后绝对不能复用旧的 ACP sessionId。",
			"第四条最近原话：现在帮我做一次折叠换代并继续讨论下一步。",
		},
		TailTurns: []protocol.Hebi8RawTurn{
			{
				AttemptID:      "att_18",
				AttemptNo:      18,
				UserPrompt:     "第三条最近原话：换代后绝对不能复用旧的 ACP sessionId。",
				AssistantReply: "明白，我会在折叠时锁定旧 agent_sessions，确保下一轮走 NewSession。",
			},
			{
				AttemptID:      "att_19",
				AttemptNo:      19,
				UserPrompt:     "第四条最近原话：现在帮我做一次折叠换代并继续讨论下一步。",
				AssistantReply: "好的，折叠完成后我会用最新开场无缝接上当前语境。",
			},
		},
		MaxRunes: hub.DefaultHebi8OpeningMaxRunes,
	}

	out1 := hub.BuildHebi8FoldOpening(in)
	out2 := hub.BuildHebi8FoldOpening(in)
	if out1 != out2 {
		t.Fatalf("expected deterministic output on repeated calls")
	}

	// Verify all 6 sections appear in exact order
	markers := []string{
		"## 1. 根与身份锚点",
		"## 2. 折叠的历史",
		"## 3. 最近的节点树",
		"## 4. 当前工作区快照",
		"## 5. 最近的原话",
		"## 6. 这一次",
	}
	prevIdx := -1
	for _, m := range markers {
		idx := strings.Index(out1, m)
		if idx < 0 {
			t.Fatalf("missing section %q in opening:\n%s", m, out1)
		}
		if idx <= prevIdx {
			t.Fatalf("section %q out of order (idx=%d <= prev=%d)", m, idx, prevIdx)
		}
		prevIdx = idx
	}

	// Verify key semantic invariants in each section
	mustContain := []string{
		"[PINNED] [seg_00000001",
		"[fld_aaaa1111 · 2026-11-21 ~ 2026-11-23 · 覆盖 5 段]",
		"seg_00000007 | 11-24 11:00~12:15 | [desktop] | 梳理桌面端菜单快捷键",
		"我核对了真实 ACP 会话流式输出与思考步骤渲染",
		"我与你敲定了 Hebi8 单一主对话与 Fold-Rollover 六段确定性开场方案",
		"Demo App (id=demo-app, active=1)",
		"第一条最近原话：先看下 ACP 会话复用问题。",
		"第四条最近原话：现在帮我做一次折叠换代并继续讨论下一步。",
		"好的，折叠完成后我会用最新开场无缝接上当前语境。",
		"2026-11-26T10:30:00Z",
		"h8o hebi8 expand <nodeId>",
		"h8o hebi8 pin <nodeId>",
	}
	for _, want := range mustContain {
		if !strings.Contains(out1, want) {
			t.Fatalf("expected opening to contain %q, got:\n%s", want, out1)
		}
	}
	// Earlier unfolded segment (seg_00000007) should only appear as 1-line index, NOT full body!
	if strings.Contains(out1, "这部分是较早的未折叠分段，不应全量展开正文") {
		t.Fatalf("expected earlier unfolded segment body to stay index-only, got:\n%s", out1)
	}

	// Verify strict budget truncation NEVER exceeds MaxRunes and NEVER cuts mid-sentence
	longSentence := "这是一句完整的中文验收测试句子，用于确认预算截断只在完整句末断开。"
	longBody := strings.Repeat(longSentence, 120)
	inTight := in
	inTight.Summaries = map[string]string{
		"seg_00000001": longBody,
		"fld_aaaa1111": longBody,
		"seg_00000008": longBody,
		"seg_00000009": longBody,
	}
	inTight.TailTurns = []protocol.Hebi8RawTurn{
		{AttemptID: "att_19", AttemptNo: 19, UserPrompt: longBody, AssistantReply: longBody},
	}
	inTight.MaxRunes = 3200
	tightOut := hub.BuildHebi8FoldOpening(inTight)
	if len([]rune(tightOut)) > 3200 {
		t.Fatalf("expected tight opening <= 3200 runes, got %d", len([]rune(tightOut)))
	}
	if !strings.Contains(tightOut, "## 6. 这一次") || !strings.Contains(tightOut, "h8o hebi8 expand <nodeId>") {
		t.Fatalf("tight opening must preserve section 6 guidance")
	}
	// Every trimmed occurrence of longSentence must end on '。' rather than a half-sentence fragment!
	for _, line := range strings.Split(tightOut, "\n") {
		if strings.Contains(line, "这是一句完整的中文验收测试句子") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasSuffix(trimmed, "。") {
				t.Fatalf("expected sentence-boundary truncation ending in '。', got mid-sentence line: %q", trimmed)
			}
		}
	}
}

func TestHebi8FoldRollover_EndToEndFreshSessionAutoFoldPinsAndExpand(t *testing.T) {
	srv, st, cfgDir, dataDir := setupTestHub(t, func(c *protocol.HubConfig) {
		c.DevToken = "test-dev-token"
	}, &fakeRyugiRunner{})

	rawRunnerTok := "hebi8-runner-tok-123"
	sum := sha256.Sum256([]byte(rawRunnerTok))
	tokHash := "sha256:" + hex.EncodeToString(sum[:])

	machinesYAML := `schemaVersion: 1
machines:
  hub-node:
    displayName: "hub-node"
    transport: local_ws
    tokenHash: "` + tokHash + `"
    tailscaleHost: "orbit.example.ts.net"
    capabilities: [primary-dev]
`
	if err := os.WriteFile(filepath.Join(cfgDir, "machines.yaml"), []byte(machinesYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ReloadConfig(context.Background()); err != nil {
		t.Fatalf("reload config: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/runner?machineId=hub-node"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + rawRunnerTok}},
	})
	if err != nil {
		t.Fatalf("dial runner ws: %v", err)
	}
	defer conn.CloseNow()

	hello, _ := json.Marshal(protocol.RunnerHelloFrame{
		Type:             protocol.RunnerMsgHello,
		ProtocolVersion:  1,
		MachineID:        "hub-node",
		RunnerInstanceID: "ri_hebi8_1",
		RunnerVersion:    "0.3.2-test",
		Capabilities:     []string{"primary-dev"},
		Adapters:         []protocol.Adapter{protocol.AdapterAgy},
	})
	if err := conn.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}

	authedReq := func(method, path string, body any) *http.Response {
		t.Helper()
		var r *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			r = bytes.NewReader(b)
		} else {
			r = bytes.NewReader(nil)
		}
		req, err := http.NewRequest(method, ts.URL+path, r)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Orbit-Dev-Token", "test-dev-token")
		req.Header.Set("X-Orbit-Request", "1")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	readDispatch := func() protocol.AttemptDispatchFrame {
		t.Helper()
		for {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				t.Fatalf("read runner ws: %v", err)
			}
			var env struct {
				Type protocol.RunnerWSMessageType `json:"type"`
			}
			_ = json.Unmarshal(raw, &env)
			if env.Type == protocol.RunnerMsgAttemptDispatch {
				var frame protocol.AttemptDispatchFrame
				if err := json.Unmarshal(raw, &frame); err != nil {
					t.Fatal(err)
				}
				return frame
			}
		}
	}

	finishAttemptWithReply := func(frame protocol.AttemptDispatchFrame, provSID, replyText string) {
		t.Helper()
		_, err := st.IngestOutboxBatch(ctx, "hub-node", "ri_hebi8_1", protocol.AttemptOutboxBatchFrame{
			Type:       protocol.RunnerMsgOutboxBatch,
			AttemptID:  frame.AttemptID,
			LeaseToken: frame.LeaseToken,
			Items: []protocol.OutboxItem{
				{
					OutboxSeq: 1,
					EventID:   "ev_start_" + frame.AttemptID,
					FrameType: protocol.OutboxFrameState,
					TS:        time.Now().UnixMilli(),
					Payload: map[string]any{
						"state":             string(protocol.AttemptRunning),
						"providerSessionId": provSID,
						"sessionCwd":        frame.WorktreePath,
					},
				},
				{
					OutboxSeq: 2,
					EventID:   "ev_thought_" + frame.AttemptID,
					FrameType: protocol.OutboxFrameEvents,
					TS:        time.Now().UnixMilli(),
					Payload: map[string]any{
						"kind":       string(protocol.EventACPThoughtBatch),
						"visibility": string(protocol.VisibilityRawACP),
						"actorKind":  string(protocol.ActorAgent),
						"actorId":    "agy",
						"payload":    map[string]any{"text": "INTERNAL_THOUGHT_NOISE_SHOULD_NOT_LEAK"},
					},
				},
				{
					OutboxSeq: 3,
					EventID:   "ev_msg_" + frame.AttemptID,
					FrameType: protocol.OutboxFrameEvents,
					TS:        time.Now().UnixMilli(),
					Payload: map[string]any{
						"kind":       string(protocol.EventACPMessageBatch),
						"visibility": string(protocol.VisibilityRawACP),
						"actorKind":  string(protocol.ActorAgent),
						"actorId":    "agy",
						"payload":    map[string]any{"text": replyText},
					},
				},
				{
					OutboxSeq: 4,
					EventID:   "ev_done_" + frame.AttemptID,
					FrameType: protocol.OutboxFrameState,
					TS:        time.Now().UnixMilli(),
					Payload: map[string]any{
						"state":            string(protocol.AttemptSucceeded),
						"completionSource": string(protocol.CompletionPromptEndTurn),
					},
				},
			},
		})
		if err != nil {
			t.Fatalf("ingest outbox for %s: %v", frame.AttemptID, err)
		}
	}

	// 1. GET /api/hebi8/main initializes the single workspace Hebi8 main task idempotently
	mainResp1 := authedReq(http.MethodGet, "/api/hebi8/main", nil)
	if mainResp1.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/hebi8/main status=%d", mainResp1.StatusCode)
	}
	var main1 protocol.Hebi8MainResponse
	_ = json.NewDecoder(mainResp1.Body).Decode(&main1)
	mainResp1.Body.Close()
	if main1.Task.TaskID == "" || main1.Task.ProjectID != protocol.PAProjectID {
		t.Fatalf("expected _pa main task, got %+v", main1.Task)
	}

	mainResp2 := authedReq(http.MethodGet, "/api/hebi8/main", nil)
	var main2 protocol.Hebi8MainResponse
	_ = json.NewDecoder(mainResp2.Body).Decode(&main2)
	mainResp2.Body.Close()
	if main2.Task.TaskID != main1.Task.TaskID {
		t.Fatalf("expected stable single main taskId %s, got %s", main1.Task.TaskID, main2.Task.TaskID)
	}
	mainTaskID := main1.Task.TaskID

	sendAndComplete := func(prompt, provSID, reply string) protocol.AttemptDispatchFrame {
		t.Helper()
		resp := authedReq(http.MethodPost, "/api/tasks/"+mainTaskID+"/followup", protocol.FollowupTaskRequest{
			Message: prompt,
			Mode:    protocol.FollowupModeQueue,
			Adapter: protocol.AdapterAgy,
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("followup %q status=%d", prompt, resp.StatusCode)
		}
		resp.Body.Close()
		frame := readDispatch()
		finishAttemptWithReply(frame, provSID, reply)
		return frame
	}

	// 2. Run 2 turns on the same underlying ACP session ("acp_sid_gen1")
	f1 := sendAndComplete("第一轮：请记录方案 A 为最终路线。", "acp_sid_gen1", "好的，已记录方案 A 为最终路线。")
	f2 := sendAndComplete("第二轮：补充说明一下数据库用 SQLite WAL。", "acp_sid_gen1", "已确认数据库采用 SQLite WAL 模式。")
	if f2.ProviderSessionID != "acp_sid_gen1" || f2.WBSessionID != f1.WBSessionID {
		t.Fatalf("expected turn 2 to reuse acp_sid_gen1 / %s, got prov=%q wb=%q", f1.WBSessionID, f2.ProviderSessionID, f2.WBSessionID)
	}

	// 3. Trigger POST /api/hebi8/fold -> seals seg #1, locks old session, emits hebi8.folded
	foldResp := authedReq(http.MethodPost, "/api/hebi8/fold", protocol.Hebi8FoldRequest{})
	if foldResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/hebi8/fold status=%d", foldResp.StatusCode)
	}
	var fold1 protocol.Hebi8FoldResponse
	_ = json.NewDecoder(foldResp.Body).Decode(&fold1)
	foldResp.Body.Close()
	if !strings.HasPrefix(fold1.Segment.ID, "seg_") || fold1.Segment.Level != 0 || fold1.Segment.AttemptRange == nil || fold1.Segment.AttemptRange.TurnCount != 2 {
		t.Fatalf("unexpected segment node: %+v", fold1.Segment)
	}

	// Verify immutable files written on disk under <dataDir>/hebi8-line/nodes/
	segJSONPath := filepath.Join(dataDir, "hebi8-line", "nodes", fold1.Segment.ID+".json")
	segMDPath := filepath.Join(dataDir, "hebi8-line", "nodes", fold1.Segment.ID+".md")
	origJSON, err := os.ReadFile(segJSONPath)
	if err != nil {
		t.Fatalf("expected immutable segment json at %s: %v", segJSONPath, err)
	}
	if _, err := os.ReadFile(segMDPath); err != nil {
		t.Fatalf("expected immutable segment md at %s: %v", segMDPath, err)
	}

	// Verify hebi8.folded divider event exists on the task timeline
	tlResp := authedReq(http.MethodGet, "/api/tasks/"+mainTaskID+"/timeline", nil)
	var tl protocol.TaskTimelineResponse
	_ = json.NewDecoder(tlResp.Body).Decode(&tl)
	tlResp.Body.Close()
	foundFoldEvent := false
	for _, ev := range tl.Events {
		if ev.Kind == protocol.EventHebi8Folded && ev.Payload["segmentId"] == fold1.Segment.ID {
			foundFoldEvent = true
			break
		}
	}
	if !foundFoldEvent {
		t.Fatalf("expected hebi8.folded timeline event for %s", fold1.Segment.ID)
	}

	// 4. Turn 3 after Fold-Rollover MUST open a fresh ACP session (ProviderSessionID == "" & new WBSessionID)
	//    AND inject the deterministic 6-section opening without thought noise!
	f3 := sendAndComplete("第三轮：换代后我们继续拆解上线清单。", "acp_sid_gen2", "收到，无缝承接方案 A 与 SQLite WAL，下面列出上线清单。")
	if f3.ProviderSessionID != "" {
		t.Fatalf("expected empty ProviderSessionID after fold-rollover, got %q", f3.ProviderSessionID)
	}
	if f3.WBSessionID == f1.WBSessionID {
		t.Fatalf("expected brand-new WBSessionID after fold-rollover, still got %q", f3.WBSessionID)
	}
	for _, want := range []string{
		"<hebi8_fold_opening>",
		"## 1. 根与身份锚点",
		"## 2. 折叠的历史",
		"## 3. 最近的节点树",
		"## 4. 当前工作区快照",
		"## 5. 最近的原话",
		"## 6. 这一次",
		"第一轮：请记录方案 A 为最终路线。",
		"已确认数据库采用 SQLite WAL 模式。",
		"第三轮：换代后我们继续拆解上线清单。",
	} {
		if !strings.Contains(f3.Prompt, want) {
			t.Fatalf("expected post-fold turn 3 prompt to contain %q, got:\n%s", want, f3.Prompt)
		}
	}
	if strings.Contains(f3.Prompt, "INTERNAL_THOUGHT_NOISE_SHOULD_NOT_LEAK") {
		t.Fatalf("opening prompt leaked raw thought noise")
	}

	// Turn 4 in the same generation should reuse acp_sid_gen2 and NOT re-inject <hebi8_fold_opening>
	f4 := sendAndComplete("第四轮：确认一下端口配置。", "acp_sid_gen2", "默认端口为 4310。")
	if f4.ProviderSessionID != "acp_sid_gen2" || f4.WBSessionID != f3.WBSessionID {
		t.Fatalf("expected turn 4 to reuse acp_sid_gen2 / %s, got prov=%q wb=%q", f3.WBSessionID, f4.ProviderSessionID, f4.WBSessionID)
	}
	if strings.Contains(f4.Prompt, "<hebi8_fold_opening>") {
		t.Fatalf("turn 4 should not re-inject fold opening")
	}

	// 5. Verify segment expand pagination returns clean raw turns from SQLite (without thought noise)
	expResp := authedReq(http.MethodGet, "/api/hebi8/line/"+fold1.Segment.ID+"/expand?offset=1&limit=1", nil)
	if expResp.StatusCode != http.StatusOK {
		t.Fatalf("expand segment status=%d", expResp.StatusCode)
	}
	var segExp protocol.Hebi8ExpandResponse
	_ = json.NewDecoder(expResp.Body).Decode(&segExp)
	expResp.Body.Close()
	if segExp.TotalTurns != 2 || len(segExp.Turns) != 1 || segExp.Turns[0].AttemptNo != 2 {
		t.Fatalf("unexpected paginated segment expand: total=%d len=%d turns=%+v", segExp.TotalTurns, len(segExp.Turns), segExp.Turns)
	}
	if !strings.Contains(segExp.Turns[0].AssistantReply, "SQLite WAL") || strings.Contains(segExp.Turns[0].AssistantReply, "INTERNAL_THOUGHT_NOISE") {
		t.Fatalf("unexpected raw turn reply: %q", segExp.Turns[0].AssistantReply)
	}

	// 6. Pin segment #1 so it is NEVER folded when >= 8 L0 nodes accumulate!
	pinResp := authedReq(http.MethodPost, "/api/hebi8/line/"+fold1.Segment.ID+"/pin", protocol.Hebi8PinRequest{Pinned: true})
	if pinResp.StatusCode != http.StatusOK {
		t.Fatalf("pin seg1 status=%d", pinResp.StatusCode)
	}
	pinResp.Body.Close()

	// Create 8 more segments (seg #2 .. seg #9):
	// - Through seg #8 we have 1 pinned + 7 unpinned = 7 eligible -> NO fold yet!
	// - On seg #9 we have 1 pinned + 8 unpinned = 8 eligible -> auto-folds oldest 5 UNPINNED (seg #2..#6) into 1 L1 fold!
	var segIDs []string
	segIDs = append(segIDs, fold1.Segment.ID)
	var autoFold *protocol.Hebi8LineNode

	// Fold seg #2 (covers turns 3 & 4 already done above)
	fold2Resp := authedReq(http.MethodPost, "/api/hebi8/fold", protocol.Hebi8FoldRequest{
		Title:   "分段 2 自定义标题",
		Summary: "分段 2 完整原创第一人称摘要内容 UNIQUE_SEG2_BODY。",
		Tags:    []string{"seg2"},
	})
	if fold2Resp.StatusCode != http.StatusCreated {
		t.Fatalf("fold #2 status=%d", fold2Resp.StatusCode)
	}
	var fold2 protocol.Hebi8FoldResponse
	_ = json.NewDecoder(fold2Resp.Body).Decode(&fold2)
	fold2Resp.Body.Close()
	if fold2.Segment.SummaryQuality != protocol.Hebi8SummaryWritten {
		t.Fatalf("expected summaryQuality=written when summary supplied, got %q", fold2.Segment.SummaryQuality)
	}
	segIDs = append(segIDs, fold2.Segment.ID)

	for i := 3; i <= 9; i++ {
		sendAndComplete(
			fmt.Sprintf("第 %d 段问题：推进里程碑 M%d。", i, i),
			fmt.Sprintf("acp_sid_m%d", i),
			fmt.Sprintf("已完成里程碑 M%d 验收。", i),
		)
		r := authedReq(http.MethodPost, "/api/hebi8/fold", protocol.Hebi8FoldRequest{
			Summary: fmt.Sprintf("分段 %d 原文摘要 UNIQUE_SEG%d_BODY。", i, i),
		})
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("fold #%d status=%d", i, r.StatusCode)
		}
		var fr protocol.Hebi8FoldResponse
		_ = json.NewDecoder(r.Body).Decode(&fr)
		r.Body.Close()
		segIDs = append(segIDs, fr.Segment.ID)
		if i < 9 && fr.Fold != nil {
			t.Fatalf("unexpected fold before 8 unpinned L0 nodes at i=%d: %+v", i, fr.Fold)
		}
		if i == 9 {
			autoFold = fr.Fold
		}
	}

	if autoFold == nil {
		t.Fatalf("expected auto-fold to trigger on 8th unpinned L0 segment (i=9)")
	}
	if autoFold.Kind != protocol.Hebi8NodeFold || autoFold.Level != 1 || len(autoFold.Covers) != 5 {
		t.Fatalf("unexpected auto-fold node: %+v", autoFold)
	}
	// Verify pinned segIDs[0] was skipped and segIDs[1..5] (seg #2..#6) were folded!
	for idx, wantID := range segIDs[1:6] {
		if autoFold.Covers[idx] != wantID {
			t.Fatalf("autoFold.Covers[%d] = %s, want %s (pinned %s must stay unfolded)", idx, autoFold.Covers[idx], wantID, segIDs[0])
		}
	}

	// Verify fold expand returns all 5 covered child nodes with their full original summaries
	fExpResp := authedReq(http.MethodGet, "/api/hebi8/line/"+autoFold.ID+"/expand", nil)
	if fExpResp.StatusCode != http.StatusOK {
		t.Fatalf("expand fold status=%d", fExpResp.StatusCode)
	}
	var foldExp protocol.Hebi8ExpandResponse
	_ = json.NewDecoder(fExpResp.Body).Decode(&foldExp)
	fExpResp.Body.Close()
	if len(foldExp.Children) != 5 {
		t.Fatalf("expected 5 children from fold expand, got %d", len(foldExp.Children))
	}
	if !strings.Contains(foldExp.Summary, "UNIQUE_SEG2_BODY") || !strings.Contains(foldExp.Summary, "UNIQUE_SEG6_BODY") {
		t.Fatalf("expected fold summary synthesized from full child .md originals, got:\n%s", foldExp.Summary)
	}
	if !strings.Contains(foldExp.Children[0].Summary, "UNIQUE_SEG2_BODY") {
		t.Fatalf("expected child[0] full summary in fold expand, got %q", foldExp.Children[0].Summary)
	}

	// Verify pin limit of 8 (1 already pinned -> pin 7 more succeeds -> 9th pin returns 400 ERR_HEBI8_PIN_LIMIT)
	for _, id := range segIDs[1:8] {
		pr := authedReq(http.MethodPost, "/api/hebi8/line/"+id+"/pin", protocol.Hebi8PinRequest{Pinned: true})
		if pr.StatusCode != http.StatusOK {
			t.Fatalf("expected pin %s to succeed, status=%d", id, pr.StatusCode)
		}
		pr.Body.Close()
	}
	pr9 := authedReq(http.MethodPost, "/api/hebi8/line/"+segIDs[8]+"/pin", protocol.Hebi8PinRequest{Pinned: true})
	if pr9.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 9th pin to fail with 400, got %d", pr9.StatusCode)
	}
	pr9.Body.Close()

	// Verify original seg #1 JSON file on disk remained completely untouched (immutability)
	afterJSON, err := os.ReadFile(segJSONPath)
	if err != nil || !bytes.Equal(origJSON, afterJSON) {
		t.Fatalf("immutable segment json %s was modified or unreadable", segJSONPath)
	}

	// P2-17: Verify restrictive 0700 dir / 0600 file permissions on hebi8-line storage
	if info, err := os.Stat(filepath.Join(dataDir, "hebi8-line")); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("expected 0700 perms on hebi8-line dir, got %v err=%v", info.Mode().Perm(), err)
	}
	if info, err := os.Stat(segJSONPath); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("expected 0600 perms on segment json, got %v err=%v", info.Mode().Perm(), err)
	}
	if info, err := os.Stat(filepath.Join(dataDir, "hebi8-line", "pins.json")); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("expected 0600 perms on pins.json, got %v err=%v", info.Mode().Perm(), err)
	}

	// P2-10 & P1-3: Long prompt (>2500 runes) expands untruncated; DB commit failure rolls back orphan disk files & retries cleanly
	longPrompt := "长提示词开头：" + strings.Repeat("甲乙丙丁戊己庚辛壬癸", 260) + "UNIQUE_LONG_PROMPT_TAIL_999"
	sendAndComplete(longPrompt, "acp_sid_long", "已完整收到超长提示词。")

	nodesBeforeFail, _ := os.ReadDir(filepath.Join(dataDir, "hebi8-line", "nodes"))
	srv.SetHebi8BeforeCommitHook(func() error {
		return fmt.Errorf("simulated sqlite commit failure")
	})
	failFold := authedReq(http.MethodPost, "/api/hebi8/fold", protocol.Hebi8FoldRequest{})
	if failFold.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 when fold DB commit fails, got %d", failFold.StatusCode)
	}
	failFold.Body.Close()

	nodesAfterFail, _ := os.ReadDir(filepath.Join(dataDir, "hebi8-line", "nodes"))
	if len(nodesAfterFail) != len(nodesBeforeFail) {
		t.Fatalf("P1-3 REGRESSION: orphan node files left on disk after DB commit failure: before=%d after=%d", len(nodesBeforeFail), len(nodesAfterFail))
	}

	// Clear injected hook and retry fold -> MUST succeed!
	srv.SetHebi8BeforeCommitHook(nil)
	retryFold := authedReq(http.MethodPost, "/api/hebi8/fold", protocol.Hebi8FoldRequest{})
	if retryFold.StatusCode != http.StatusCreated {
		t.Fatalf("expected fold retry after DB failure to succeed with 201, got %d", retryFold.StatusCode)
	}
	var retryFoldBody protocol.Hebi8FoldResponse
	_ = json.NewDecoder(retryFold.Body).Decode(&retryFoldBody)
	retryFold.Body.Close()

	// Verify /expand returns full untruncated userPrompt including UNIQUE_LONG_PROMPT_TAIL_999!
	longExpResp := authedReq(http.MethodGet, "/api/hebi8/line/"+retryFoldBody.Segment.ID+"/expand", nil)
	if longExpResp.StatusCode != http.StatusOK {
		t.Fatalf("expand long prompt segment status=%d", longExpResp.StatusCode)
	}
	var longExp protocol.Hebi8ExpandResponse
	_ = json.NewDecoder(longExpResp.Body).Decode(&longExp)
	longExpResp.Body.Close()
	if len(longExp.Turns) != 1 || !strings.Contains(longExp.Turns[0].UserPrompt, "UNIQUE_LONG_PROMPT_TAIL_999") {
		t.Fatalf("P2-10 REGRESSION: expand truncated long prompt! len=%d", len(longExp.Turns))
	}

	// P1-7: Followup with explicit offline machineId MUST be rejected with 400 ERR_MACHINE_OFFLINE
	offResp := authedReq(http.MethodPost, "/api/tasks/"+mainTaskID+"/followup", protocol.FollowupTaskRequest{
		Message:   "route to offline machine",
		Mode:      protocol.FollowupModeQueue,
		MachineID: "nonexistent-offline-box",
		Adapter:   protocol.AdapterAgy,
	})
	if offResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 ERR_MACHINE_OFFLINE for offline machineId, got %d", offResp.StatusCode)
	}
	offResp.Body.Close()

	// Explicit connected machineId ("hub-node") succeeds and dispatches!
	onResp := authedReq(http.MethodPost, "/api/tasks/"+mainTaskID+"/followup", protocol.FollowupTaskRequest{
		Message:   "route to hub-node explicitly",
		Mode:      protocol.FollowupModeQueue,
		MachineID: "hub-node",
		Adapter:   protocol.AdapterAgy,
	})
	if onResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 for explicit connected machineId hub-node, got %d", onResp.StatusCode)
	}
	onResp.Body.Close()
	explicitFrame := readDispatch()
	finishAttemptWithReply(explicitFrame, "acp_sid_explicit", "已按指定设备执行。")

	// Item 7: Missing committed summary file on disk MUST return error (500 ERR_HEBI8_OPENING)
	// rather than silently dispatching a mutilated opening!
	foldBeforeErr := authedReq(http.MethodPost, "/api/hebi8/fold", protocol.Hebi8FoldRequest{})
	if foldBeforeErr.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 on fold before opening error test, got %d", foldBeforeErr.StatusCode)
	}
	var foldBeforeErrBody protocol.Hebi8FoldResponse
	_ = json.NewDecoder(foldBeforeErr.Body).Decode(&foldBeforeErrBody)
	foldBeforeErr.Body.Close()

	missingMDPath := filepath.Join(dataDir, "hebi8-line", "nodes", foldBeforeErrBody.Segment.ID+".md")
	savedMissingMD, err := os.ReadFile(missingMDPath)
	if err != nil {
		t.Fatalf("read committed md before removing: %v", err)
	}
	if err := os.Remove(missingMDPath); err != nil {
		t.Fatalf("remove committed md to simulate disk corruption: %v", err)
	}
	errOpenResp := authedReq(http.MethodPost, "/api/tasks/"+mainTaskID+"/followup", protocol.FollowupTaskRequest{
		Message: "should fail instead of sending partial opening",
		Mode:    protocol.FollowupModeQueue,
		Adapter: protocol.AdapterAgy,
	})
	if errOpenResp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("Item 7 REGRESSION: expected 500 ERR_HEBI8_OPENING when committed summary is missing, got %d", errOpenResp.StatusCode)
	}
	errOpenResp.Body.Close()

	// Final-2: Failed opening MUST atomically fail the undispatched attempt and clear active_attempt_id!
	tlAfterErrResp := authedReq(http.MethodGet, "/api/tasks/"+mainTaskID+"/timeline", nil)
	var tlAfterErr protocol.TaskTimelineResponse
	_ = json.NewDecoder(tlAfterErrResp.Body).Decode(&tlAfterErr)
	tlAfterErrResp.Body.Close()
	if tlAfterErr.Task.ActiveAttemptID != "" {
		t.Fatalf("Final-2 REGRESSION: expected activeAttemptId cleared after 500 ERR_HEBI8_OPENING, got %q", tlAfterErr.Task.ActiveAttemptID)
	}
	if len(tlAfterErr.Attempts) == 0 || tlAfterErr.Attempts[len(tlAfterErr.Attempts)-1].State != protocol.AttemptFailed {
		t.Fatalf("Final-2 REGRESSION: expected latest attempt state=failed after opening error, got %+v", tlAfterErr.Attempts[len(tlAfterErr.Attempts)-1])
	}

	// Restoring the missing .md lets the very next followup dispatch immediately (no stuck queue)!
	if err := os.WriteFile(missingMDPath, savedMissingMD, 0o600); err != nil {
		t.Fatalf("restore committed md: %v", err)
	}
	recoveredFrame := sendAndComplete("恢复摘要后立即继续对话", "acp_sid_recovered", "已恢复并顺利继续。")
	if !strings.Contains(recoveredFrame.Prompt, "<hebi8_fold_opening>") {
		t.Fatalf("expected recovered turn to receive full fold opening")
	}

	// Verify Store.LockTaskAgentSessions helper directly ensures clean state
	_ = store.ErrNotFound
}

func TestHebi8LegacyUpgradePreservesHistoryAndCorruptStateRefusesCleanup(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	cfgDir := filepath.Join(tmpDir, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "machines.yaml"), []byte(`schemaVersion: 1
machines:
  hub-node:
    displayName: Hub Node
    transport: local_ws
    capabilities: [primary-dev]
`), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(dataDir, "orbit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	hubCfg := protocol.DefaultHubConfig()
	hubCfg.DataDir = dataDir
	hubCfg.ConfigDir = cfgDir
	hubCfg.DevOpen = true

	srv, err := hub.NewServer(ctx, hub.ServerOptions{HubConfig: hubCfg, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	mainTask, _, err := st.EnsureHebi8MainTask(ctx, protocol.AdapterAgy)
	if err != nil {
		t.Fatal(err)
	}

	addCompletedAttempt := func(prompt string) string {
		t.Helper()
		_, att, _, err := st.CreateNextAttempt(ctx, store.CreateNextAttemptParams{
			TaskID:         mainTask.TaskID,
			TriggerKind:    protocol.TriggerFollowup,
			TriggerPayload: map[string]any{"prompt": prompt},
			Adapter:        protocol.AdapterAgy,
			MachineID:      "hub-node",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.WriteDB().ExecContext(ctx, `UPDATE attempts SET state = 'succeeded', finished_at = ? WHERE attempt_id = ?`, time.Now().UnixMilli(), att.AttemptID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.WriteDB().ExecContext(ctx, `UPDATE tasks SET state = 'done', active_attempt_id = NULL WHERE task_id = ?`, mainTask.TaskID); err != nil {
			t.Fatal(err)
		}
		return att.AttemptID
	}

	// 1. Seed legacy state: Generation=1, LastFoldedAttemptNo=1, NO committedNodeIds field
	//    - seg_1111aaaa (attempt #1, recorded in hebi8.folded event)
	//    - fld_aaaa1111 (intermediate L1 fold covering seg_1111aaaa, NOT listed in hebi8.folded!)
	//    - fld_bbbb2222 (top-level L2 fold covering fld_aaaa1111, NOT listed in hebi8.folded!)
	//    - seg_9999ffff (uncommitted crash orphan with attempt #99 > LastFoldedAttemptNo, MUST be cleaned up!)
	att1ID := addCompletedAttempt("旧版第一轮")
	nodesDir := filepath.Join(dataDir, "hebi8-line", "nodes")
	oldSegID := "seg_1111aaaa"
	oldL1ID := "fld_aaaa1111"
	oldL2ID := "fld_bbbb2222"
	orphanID := "seg_9999ffff"
	oldJSON := fmt.Sprintf(`{"v":1,"id":%q,"kind":"segment","seq":1,"level":0,"title":"Legacy Segment","attemptRange":{"startAttemptId":%q,"endAttemptId":%q,"startAttemptNo":1,"endAttemptNo":1,"turnCount":1},"createdAt":"2026-11-25T00:00:00Z","createdAtMs":1700000000000}`, oldSegID, att1ID, att1ID)
	l1JSON := fmt.Sprintf(`{"v":1,"id":%q,"kind":"fold","seq":2,"level":1,"title":"Legacy L1 Fold","covers":[%q],"createdAt":"2026-11-25T00:01:00Z","createdAtMs":1700000001000}`, oldL1ID, oldSegID)
	l2JSON := fmt.Sprintf(`{"v":1,"id":%q,"kind":"fold","seq":3,"level":2,"title":"Legacy L2 Fold","covers":[%q],"createdAt":"2026-11-25T00:02:00Z","createdAtMs":1700000002000}`, oldL2ID, oldL1ID)
	orphanJSON := fmt.Sprintf(`{"v":1,"id":%q,"kind":"segment","seq":4,"level":0,"title":"Uncommitted Orphan","attemptRange":{"startAttemptNo":99,"endAttemptNo":99,"turnCount":1},"createdAt":"2026-11-25T00:03:00Z","createdAtMs":1700000003000}`, orphanID)
	for id, body := range map[string]string{
		oldSegID: oldJSON,
		oldL1ID:  l1JSON,
		oldL2ID:  l2JSON,
		orphanID: orphanJSON,
	} {
		if err := os.WriteFile(filepath.Join(nodesDir, id+".json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(nodesDir, id+".md"), []byte("摘要 "+id+"。"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.EmitHebi8FoldedEvent(ctx, mainTask.TaskID, att1ID, map[string]any{"segmentId": oldSegID, "endAttemptNo": 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `
		INSERT INTO hub_settings (key, value_json, updated_at)
		VALUES ('hebi8_rollover_state', '{"generation":1,"lastFoldedAttemptNo":1,"lastSegmentId":"seg_1111aaaa"}', ?)
		ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json, updated_at = excluded.updated_at
	`, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}

	doFold := func() protocol.Hebi8FoldResponse {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4310/api/hebi8/fold", bytes.NewReader([]byte(`{}`)))
		req.RemoteAddr = "127.0.0.1:50200"
		req.Header.Set("X-Orbit-Request", "1")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("expected 201 on fold, got %d: %s", rec.Code, rec.Body.String())
		}
		var out protocol.Hebi8FoldResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// Execute TWO consecutive folds after upgrading from legacy state:
	// seg_1111aaaa + intermediate L1 fld_aaaa1111 + L2 fld_bbbb2222 MUST ALL stay on disk & in Nodes both times,
	// while uncommitted orphan seg_9999ffff (EndAttemptNo 99 > 1) MUST be cleaned up!
	addCompletedAttempt("升级后第二轮")
	f1 := doFold()
	if len(f1.Nodes) != 4 {
		t.Fatalf("Final-1 REGRESSION: expected 4 nodes (seg + L1 + L2 + fold1), got %d (%+v)", len(f1.Nodes), f1.Nodes)
	}

	addCompletedAttempt("升级后第三轮")
	f2 := doFold()
	if len(f2.Nodes) != 5 {
		t.Fatalf("Final-1 REGRESSION: expected 5 nodes (seg + L1 + L2 + fold1 + fold2), got %d (%+v)", len(f2.Nodes), f2.Nodes)
	}
	present := map[string]bool{}
	for _, n := range f2.Nodes {
		present[n.ID] = true
	}
	for _, wantID := range []string{oldSegID, oldL1ID, oldL2ID} {
		if !present[wantID] {
			t.Fatalf("Final-1 REGRESSION: legacy node %s disappeared after 2nd fold! nodes=%+v", wantID, f2.Nodes)
		}
		if _, err := os.Stat(filepath.Join(nodesDir, wantID+".json")); err != nil {
			t.Fatalf("Final-1 REGRESSION: legacy node file %s.json was deleted! err=%v", wantID, err)
		}
	}
	if _, err := os.Stat(filepath.Join(nodesDir, orphanID+".json")); !os.IsNotExist(err) {
		t.Fatalf("expected uncommitted orphan %s.json cleaned up, err=%v", orphanID, err)
	}

	// 2. Corrupt hebi8_rollover_state JSON in SQLite -> MUST return 500 AND delete ZERO files on disk!
	filesBeforeCorrupt, _ := os.ReadDir(nodesDir)
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE hub_settings SET value_json = '{corrupt_json' WHERE key = 'hebi8_rollover_state'`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method string
		path   string
		body   []byte
	}{
		{http.MethodGet, "/api/hebi8/main", nil},
		{http.MethodPost, "/api/hebi8/fold", []byte(`{}`)},
	} {
		req := httptest.NewRequest(tc.method, "http://127.0.0.1:4310"+tc.path, bytes.NewReader(tc.body))
		req.RemoteAddr = "127.0.0.1:50201"
		req.Header.Set("X-Orbit-Request", "1")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("Final-1 REGRESSION: expected 500 on corrupt rollover state for %s %s, got %d: %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
	filesAfterCorrupt, _ := os.ReadDir(nodesDir)
	if len(filesAfterCorrupt) != len(filesBeforeCorrupt) {
		t.Fatalf("Final-1 REGRESSION: corrupt state deleted disk node files! before=%d after=%d", len(filesBeforeCorrupt), len(filesAfterCorrupt))
	}
}

func TestHebi8CrashOrphanNodeIgnoredAndCleanedOnFold(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	cfgDir := filepath.Join(tmpDir, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "machines.yaml"), []byte(`schemaVersion: 1
machines:
  hub-node:
    displayName: Hub Node
    transport: local_ws
    capabilities: [primary-dev]
`), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(dataDir, "orbit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	hubCfg := protocol.DefaultHubConfig()
	hubCfg.DataDir = dataDir
	hubCfg.ConfigDir = cfgDir
	hubCfg.DevOpen = true

	srv, err := hub.NewServer(ctx, hub.ServerOptions{HubConfig: hubCfg, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	mainTask, _, err := st.EnsureHebi8MainTask(ctx, protocol.AdapterAgy)
	if err != nil {
		t.Fatal(err)
	}
	// Seed one completed attempt (#1) while DB rollover state remains at Generation == 0
	_, att, _, err := st.CreateNextAttempt(ctx, store.CreateNextAttemptParams{
		TaskID:         mainTask.TaskID,
		TriggerKind:    protocol.TriggerFollowup,
		TriggerPayload: map[string]any{"prompt": "崩溃前已完成的一轮对话"},
		Adapter:        protocol.AdapterAgy,
		MachineID:      "hub-node",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE attempts SET state = 'succeeded', finished_at = ? WHERE attempt_id = ?`, time.Now().UnixMilli(), att.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE tasks SET state = 'done', active_attempt_id = NULL WHERE task_id = ?`, mainTask.TaskID); err != nil {
		t.Fatal(err)
	}

	// Simulate kill -9 mid-fold: write an uncommitted orphan seg_deadc0de.{json,md} on disk while DB Generation == 0!
	nodesDir := filepath.Join(dataDir, "hebi8-line", "nodes")
	orphanJSON := `{"v":1,"id":"seg_deadc0de","kind":"segment","seq":1,"level":0,"title":"Uncommitted Crash Orphan","attemptRange":{"startAttemptNo":1,"endAttemptNo":1,"turnCount":1}}`
	if err := os.WriteFile(filepath.Join(nodesDir, "seg_deadc0de.json"), []byte(orphanJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nodesDir, "seg_deadc0de.md"), []byte("ORPHAN"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. GET /api/hebi8/main MUST ignore the uncommitted orphan and report 0 nodes & 1 pending turn!
	reqMain := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4310/api/hebi8/main", nil)
	reqMain.RemoteAddr = "127.0.0.1:50100"
	recMain := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recMain, reqMain)
	if recMain.Code != http.StatusOK {
		t.Fatalf("GET /api/hebi8/main status=%d body=%s", recMain.Code, recMain.Body.String())
	}
	var mainResp protocol.Hebi8MainResponse
	if err := json.Unmarshal(recMain.Body.Bytes(), &mainResp); err != nil {
		t.Fatal(err)
	}
	if len(mainResp.Nodes) != 0 || mainResp.PendingTurnsSinceLastFold != 1 {
		t.Fatalf("Item 2 REGRESSION: uncommitted crash orphan leaked into /api/hebi8/main: nodes=%d pending=%d", len(mainResp.Nodes), mainResp.PendingTurnsSinceLastFold)
	}

	// 2. Pinning uncommitted orphan MUST return 404
	reqPin := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4310/api/hebi8/line/seg_deadc0de/pin", bytes.NewReader([]byte(`{"pinned":true}`)))
	reqPin.RemoteAddr = "127.0.0.1:50101"
	reqPin.Header.Set("X-Orbit-Request", "1")
	recPin := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recPin, reqPin)
	if recPin.Code != http.StatusNotFound {
		t.Fatalf("expected 404 pinning uncommitted orphan, got %d: %s", recPin.Code, recPin.Body.String())
	}

	// 3. POST /api/hebi8/fold MUST clean up seg_deadc0de.* and commit the real segment cleanly!
	reqFold := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4310/api/hebi8/fold", bytes.NewReader([]byte(`{}`)))
	reqFold.RemoteAddr = "127.0.0.1:50102"
	reqFold.Header.Set("X-Orbit-Request", "1")
	recFold := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recFold, reqFold)
	if recFold.Code != http.StatusCreated {
		t.Fatalf("Item 2 REGRESSION: expected 201 on fold after crash orphan, got %d: %s", recFold.Code, recFold.Body.String())
	}
	var foldResp protocol.Hebi8FoldResponse
	if err := json.Unmarshal(recFold.Body.Bytes(), &foldResp); err != nil {
		t.Fatal(err)
	}
	if len(foldResp.Nodes) != 1 || foldResp.Nodes[0].ID == "seg_deadc0de" {
		t.Fatalf("expected only real committed segment in fold response, got %+v", foldResp.Nodes)
	}
	if _, err := os.Stat(filepath.Join(nodesDir, "seg_deadc0de.json")); !os.IsNotExist(err) {
		t.Fatalf("expected orphan seg_deadc0de.json cleaned up on fold, err=%v", err)
	}
}

func TestHebi8ManualFoldResetsHeadAndTracksUpdatedTopLevelTasks(t *testing.T) {
	ctx := context.Background()
	srv, st, cfgDir, _ := setupTestHub(t, func(c *protocol.HubConfig) {
		c.DevOpen = true
	}, &fakeRyugiRunner{})

	if err := os.WriteFile(filepath.Join(cfgDir, "machines.yaml"), []byte(`schemaVersion: 1
machines:
  hub-node:
    displayName: "hub-node"
    transport: local_ws
    capabilities: [primary-dev]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "projects.yaml"), []byte(`schemaVersion: 1
projects:
  demo-app:
    displayName: "Demo App"
    repoIdentity: "demo-app"
    defaultBranch: "main"
    defaultCapability: "primary-dev"
    defaultAdapter: "agy"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ReloadConfig(ctx); err != nil {
		t.Fatal(err)
	}

	doReq := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		req := httptest.NewRequest(method, "http://127.0.0.1:4310"+path, &buf)
		req.RemoteAddr = "127.0.0.1:50200"
		req.Header.Set("X-Orbit-Request", "1")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	// Create 2 active top-level tasks, 1 child task, and 1 archived top-level task
	t1, err := st.CreateTask(ctx, store.CreateTaskParams{ProjectID: "demo-app", Title: "Top 1", Goal: "g1", MachineID: "hub-node"})
	if err != nil {
		t.Fatal(err)
	}
	t2, err := st.CreateTask(ctx, store.CreateTaskParams{ProjectID: "demo-app", Title: "Top 2", Goal: "g2", MachineID: "hub-node"})
	if err != nil {
		t.Fatal(err)
	}
	child1, err := st.CreateTask(ctx, store.CreateTaskParams{ProjectID: "demo-app", ParentTaskID: t1.Task.TaskID, Title: "Child 1", Goal: "gc", MachineID: "hub-node"})
	if err != nil {
		t.Fatal(err)
	}
	tArch, err := st.CreateTask(ctx, store.CreateTaskParams{ProjectID: "demo-app", Title: "Archived Top", Goal: "ga", MachineID: "hub-node"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ArchiveTask(ctx, tArch.Task.TaskID, "human"); err != nil {
		t.Fatal(err)
	}

	// Before any Hebi8 turn: updatedTopLevelTaskCount == 2 (excludes child1 & tArch), pendingTurns == 0
	rec := doReq(http.MethodGet, "/api/hebi8/main", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/hebi8/main failed: %d %s", rec.Code, rec.Body.String())
	}
	var main0 protocol.Hebi8MainResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &main0)
	if main0.UpdatedTopLevelTaskCount != 2 || main0.LastHebi8TurnAt != nil || main0.PendingTurnsSinceLastFold != 0 || main0.LineSummary.PendingTurns == nil || *main0.LineSummary.PendingTurns != 0 {
		t.Fatalf("unexpected initial hebi8/main: count=%d lastTurn=%v pending=%d summaryPending=%v",
			main0.UpdatedTopLevelTaskCount, main0.LastHebi8TurnAt, main0.PendingTurnsSinceLastFold, main0.LineSummary.PendingTurns)
	}

	// Empty fold must be rejected with 400 ERR_NOTHING_TO_FOLD
	rec = doReq(http.MethodPost, "/api/hebi8/fold", map[string]any{})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "ERR_NOTHING_TO_FOLD") {
		t.Fatalf("expected 400 ERR_NOTHING_TO_FOLD on empty fold, got %d: %s", rec.Code, rec.Body.String())
	}

	// Seed 1 completed Hebi8 turn with finished_at AFTER t1/t2 updates -> pending=1, updatedTopLevelTaskCount=0
	turnFinish := time.Now().UnixMilli() + 500
	if _, err := st.WriteDB().ExecContext(ctx, `
		INSERT INTO attempts (
			attempt_id, task_id, project_id, attempt_no, machine_id, adapter,
			trigger_kind, trigger_payload_json, state, lease_token, lease_expires_at, started_at, finished_at
		) VALUES ('att_h_1', ?, ?, 1, 'hub-node', 'agy', 'followup', '{"prompt":"汇总一下"}', 'succeeded', 'lt', ?, ?, ?)
	`, main0.Task.TaskID, protocol.PAProjectID, turnFinish+60000, turnFinish-10, turnFinish); err != nil {
		t.Fatal(err)
	}

	rec = doReq(http.MethodGet, "/api/hebi8/main", nil)
	var main1 protocol.Hebi8MainResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &main1)
	if main1.PendingTurnsSinceLastFold != 1 || main1.LineSummary.PendingTurns == nil || *main1.LineSummary.PendingTurns != 1 || main1.LastHebi8TurnAt == nil || *main1.LastHebi8TurnAt != turnFinish || main1.UpdatedTopLevelTaskCount != 0 {
		t.Fatalf("unexpected hebi8/main after 1 turn: %+v", main1)
	}

	// Updating child1 MUST NOT increment updatedTopLevelTaskCount; updating t2 increments it to 1
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE tasks SET updated_at = ? WHERE task_id = ?`, turnFinish+100, child1.Task.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteDB().ExecContext(ctx, `UPDATE tasks SET updated_at = ? WHERE task_id = ?`, turnFinish+200, t2.Task.TaskID); err != nil {
		t.Fatal(err)
	}
	rec = doReq(http.MethodGet, "/api/hebi8/main", nil)
	var mainAfterTopUpdate protocol.Hebi8MainResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &mainAfterTopUpdate)
	if mainAfterTopUpdate.UpdatedTopLevelTaskCount != 1 {
		t.Fatalf("expected updatedTopLevelTaskCount=1 (ignoring child updates), got %d", mainAfterTopUpdate.UpdatedTopLevelTaskCount)
	}

	// Manual fold -> resets HEAD pendingTurns to 0 across /fold, /main, and /line
	rec = doReq(http.MethodPost, "/api/hebi8/fold", map[string]any{})
	if rec.Code != http.StatusCreated {
		t.Fatalf("manual fold failed: %d %s", rec.Code, rec.Body.String())
	}
	var foldResp protocol.Hebi8FoldResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &foldResp)
	if foldResp.PendingTurnsSinceLastFold != 0 || foldResp.LineSummary.PendingTurns == nil || *foldResp.LineSummary.PendingTurns != 0 {
		t.Fatalf("expected pendingTurns=0 after fold, got pending=%d summary=%v", foldResp.PendingTurnsSinceLastFold, foldResp.LineSummary.PendingTurns)
	}

	rec = doReq(http.MethodGet, "/api/hebi8/line", nil)
	var lineResp protocol.Hebi8LineResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &lineResp)
	if lineResp.PendingTurnsSinceLastFold != 0 || lineResp.LineSummary.PendingTurns == nil || *lineResp.LineSummary.PendingTurns != 0 {
		t.Fatalf("expected /hebi8/line pendingTurns=0 after fold, got pending=%d summary=%v", lineResp.PendingTurnsSinceLastFold, lineResp.LineSummary.PendingTurns)
	}

	rec = doReq(http.MethodGet, "/api/hebi8/main", nil)
	var mainAfterFold protocol.Hebi8MainResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &mainAfterFold)
	if mainAfterFold.PendingTurnsSinceLastFold != 0 || mainAfterFold.LineSummary.PendingTurns == nil || *mainAfterFold.LineSummary.PendingTurns != 0 {
		t.Fatalf("expected /hebi8/main pendingTurns=0 after fold, got pending=%d summary=%v", mainAfterFold.PendingTurnsSinceLastFold, mainAfterFold.LineSummary.PendingTurns)
	}

	// Immediate second fold MUST be rejected with 400 ERR_NOTHING_TO_FOLD
	rec = doReq(http.MethodPost, "/api/hebi8/fold", map[string]any{})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "ERR_NOTHING_TO_FOLD") {
		t.Fatalf("expected 400 ERR_NOTHING_TO_FOLD on repeat fold, got %d: %s", rec.Code, rec.Body.String())
	}
}
