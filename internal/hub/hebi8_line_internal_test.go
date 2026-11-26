package hub

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

func TestHebi8LineStore_WriteImmutableNodeRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	ls := newHebi8LineStore(dir)

	node := protocol.Hebi8LineNode{
		V:              1,
		ID:             "seg_deadbeef",
		Kind:           protocol.Hebi8NodeSegment,
		Seq:            1,
		CoversFrom:     "2026-11-26T00:00:00Z",
		CoversTo:       "2026-11-26T00:10:00Z",
		Level:          0,
		Title:          "不可变测试节点",
		SummaryQuality: protocol.Hebi8SummaryWritten,
		CreatedAt:      "2026-11-26T00:10:01Z",
	}
	if err := ls.WriteImmutableNode(node, "原始摘要正文。"); err != nil {
		t.Fatalf("first WriteImmutableNode failed: %v", err)
	}

	// Second write with same ID MUST fail with errHebi8NodeImmutable and preserve original text
	err := ls.WriteImmutableNode(node, "试图篡改后的正文。")
	if !errors.Is(err, errHebi8NodeImmutable) {
		t.Fatalf("expected errHebi8NodeImmutable on duplicate write, got %v", err)
	}
	md, err := ls.ReadSummary("seg_deadbeef")
	if err != nil {
		t.Fatalf("ReadSummary failed: %v", err)
	}
	if md != "原始摘要正文。\n" {
		t.Fatalf("expected original summary intact, got %q", md)
	}
}

func TestTrimAtSentenceBoundaryAndFoldFullChild(t *testing.T) {
	// P2-11: Even when the sentence terminator is in the first 35% of the window, cut at sentence boundary (never mid-sentence).
	text := "第一句很短。第二句超级无敌长长长长长长长长长长长长长长长长长长长长长长长长长长长长长长长长没有结束。"
	got := trimAtSentenceBoundary(text, 30)
	if got != "第一句很短。" {
		t.Fatalf("expected cut at first sentence boundary, got %q", got)
	}

	// P2-11: When even the first sentence does not fit in budget, return explicit omission marker instead of cutting mid-sentence.
	noPunct := strings.Repeat("字", 80)
	gotOmit := trimAtSentenceBoundary(noPunct, 20)
	if gotOmit != "[已省略]" {
		t.Fatalf("expected [已省略] when no sentence boundary fits, got %q", gotOmit)
	}

	// P2-10: synthesizeFoldSummary must NOT truncate child .md originals at 1400 chars
	longChild := "子摘要头部。" + strings.Repeat("详情内容。", 400) + "UNIQUE_CHILD_END_MARKER。"
	batch := []protocol.Hebi8LineNode{
		{ID: "seg_00000001", Title: "长子段", CoversFrom: "2026-11-26T00:00:00Z", CoversTo: "2026-11-26T01:00:00Z"},
	}
	_, foldMD := synthesizeFoldSummary(batch, []string{longChild}, 1)
	if !strings.Contains(foldMD, "UNIQUE_CHILD_END_MARKER") {
		t.Fatalf("P2-10 REGRESSION: synthesizeFoldSummary truncated child summary (len=%d)", len(foldMD))
	}

	// Item 8: Version numbers (v2.10) and decimals (3.14) must NOT be treated as sentence terminators!
	verText := "升级到 v2.10 并核对 3.14 阈值且没有句末标点继续很长很长很长"
	if gotVer := trimAtSentenceBoundary(verText, 18); gotVer != "[已省略]" {
		t.Fatalf("Item 8 REGRESSION: version/decimal dot was treated as sentence boundary, got %q", gotVer)
	}
	verWithPeriod := "已完成 v2.10 发布。第二句讨论 3.14 参数调整且非常非常长没有结束"
	if gotVer2 := trimAtSentenceBoundary(verWithPeriod, 24); gotVer2 != "已完成 v2.10 发布。" {
		t.Fatalf("Item 8 REGRESSION: expected full first sentence including v2.10, got %q", gotVer2)
	}

	// Item 6 / Final-3: isSafeDeliveryID allows _release and release.1 while blocking traversal/leading dots
	for _, good := range []string{"dlv_01ABC", "release.1", "_release", "-dash_1.2", "v2.0-rc_1"} {
		if !isSafeDeliveryID(good) {
			t.Fatalf("expected isSafeDeliveryID(%q) == true", good)
		}
	}
	for _, bad := range []string{"", ".hidden", "..", "a..b", "a/b", `a\b`, "a b"} {
		if isSafeDeliveryID(bad) {
			t.Fatalf("expected isSafeDeliveryID(%q) == false", bad)
		}
	}
}

func TestFailUndispatchedAttemptDetachedSucceedsWithCanceledRequestContext(t *testing.T) {
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
	srv, err := NewServer(ctx, ServerOptions{HubConfig: hubCfg, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	cr, err := st.CreateTask(ctx, store.CreateTaskParams{
		ProjectID:          protocol.ScratchProjectID,
		Title:              "Canceled ctx cleanup test",
		Goal:               "test detached fail",
		RequiredCapability: "primary-dev",
		Adapter:            protocol.AdapterAgy,
		MachineID:          "hub-node",
		CreateAttempt:      true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Cancel the request context BEFORE invoking failUndispatchedAttemptDetached!
	reqCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := srv.failUndispatchedAttemptDetached(reqCtx, cr.Attempt.AttemptID, "ERR_HEBI8_OPENING", "request context canceled during opening"); err != nil {
		t.Fatalf("expected detached fail to succeed even with canceled request ctx, got %v", err)
	}

	reloadedTask, err := st.GetTask(ctx, cr.Task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if reloadedTask.ActiveAttemptID != "" {
		t.Fatalf("expected activeAttemptId cleared, got %q", reloadedTask.ActiveAttemptID)
	}
	reloadedAtt, err := st.GetAttemptRecord(ctx, cr.Attempt.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if reloadedAtt.State != protocol.AttemptFailed {
		t.Fatalf("expected attempt state=failed, got %s", reloadedAtt.State)
	}
}
