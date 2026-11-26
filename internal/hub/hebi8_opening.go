package hub

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

const (
	// DefaultHebi8OpeningMaxRunes is the hard character/rune budget (~24k chars) for the
	// deterministic Fold-Rollover opening prompt injected on the first turn of a fresh ACP session.
	DefaultHebi8OpeningMaxRunes = 24000
)

// Hebi8FoldOpeningInput contains all deterministic inputs needed to assemble the
// 6-section Fold-Rollover opening without side effects or I/O.
type Hebi8FoldOpeningInput struct {
	WorkspaceName     string
	Now               time.Time
	Nodes             []protocol.Hebi8LineNode
	Summaries         map[string]string
	Pins              []string
	WorkspaceSnapshot string
	RecentUserPrompts []string
	TailTurns         []protocol.Hebi8RawTurn
	MaxRunes          int
}

// BuildHebi8FoldOpening deterministically assembles the 6-part Hebi8 Fold-Rollover opening
// within a strict character budget without ever cutting mid-sentence.
func BuildHebi8FoldOpening(in Hebi8FoldOpeningInput) string {
	maxRunes := in.MaxRunes
	if maxRunes <= 0 {
		maxRunes = DefaultHebi8OpeningMaxRunes
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	wsName := strings.TrimSpace(in.WorkspaceName)
	if wsName == "" {
		wsName = "Orbit"
	}

	pinnedSet := map[string]bool{}
	for _, id := range in.Pins {
		pinnedSet[id] = true
	}
	coveredSet := map[string]bool{}
	nodeByID := make(map[string]protocol.Hebi8LineNode, len(in.Nodes))
	for _, n := range in.Nodes {
		nodeByID[n.ID] = n
		if n.Kind == protocol.Hebi8NodeFold {
			for _, childID := range n.Covers {
				coveredSet[childID] = true
			}
		}
	}

	// Count total underlying segments covered recursively by a fold node.
	var countCoveredSegments func(id string, seen map[string]bool) int
	countCoveredSegments = func(id string, seen map[string]bool) int {
		if seen[id] {
			return 0
		}
		seen[id] = true
		n, ok := nodeByID[id]
		if !ok {
			return 0
		}
		if n.Kind == protocol.Hebi8NodeSegment {
			return 1
		}
		total := 0
		for _, cid := range n.Covers {
			total += countCoveredSegments(cid, seen)
		}
		if total == 0 {
			return len(n.Covers)
		}
		return total
	}

	// Section 1: Identity Anchor
	sec1 := fmt.Sprintf(
		"## 1. 根与身份锚点\n"+
			"你是 **Hebi8**，用户在工作区「%s」中的唯一主对话助手，负责跨项目全局跟进、决策梳理、任务协调与持续记忆衔接。\n"+
			"当前会话刚刚完成一次上下文折叠换代（Fold-Rollover），以下为按确定性规则拼装的上下文缓存与原话锚点，请自然承接上一轮语境直接回复用户。",
		wsName,
	)

	// Section 6: This Turn (always preserved in full!)
	sec6 := fmt.Sprintf(
		"## 6. 这一次\n"+
			"当前绝对时间：%s\n"+
			"以上摘要是缓存，原文是证据；需要历史细节时可运行 `orbit hebi8 expand <nodeId>` 查看下层节点或原文，重要节点可运行 `orbit hebi8 pin <nodeId>` 钉住。",
		now.Format(time.RFC3339),
	)

	// Classify nodes:
	// - Top-level uncovered folds (highest level first, then chronological)
	// - Pinned nodes (any kind)
	// - Unfolded segments (not covered by any fold), sorted chronologically by seq ASC
	var topFolds []protocol.Hebi8LineNode
	var pinnedNodes []protocol.Hebi8LineNode
	var unfoldedSegments []protocol.Hebi8LineNode

	seenInHistory := map[string]bool{}
	for _, n := range in.Nodes {
		if pinnedSet[n.ID] {
			pinnedNodes = append(pinnedNodes, n)
			seenInHistory[n.ID] = true
		}
	}
	for _, n := range in.Nodes {
		if n.Kind == protocol.Hebi8NodeFold && !coveredSet[n.ID] && !seenInHistory[n.ID] {
			topFolds = append(topFolds, n)
		}
		if n.Kind == protocol.Hebi8NodeSegment && !coveredSet[n.ID] {
			unfoldedSegments = append(unfoldedSegments, n)
		}
	}

	// Allocate budgets proportionally out of remaining budget after fixed anchors + wrapper.
	const wrapperOverhead = 120
	fixedRunes := runeLen(sec1) + runeLen(sec6) + wrapperOverhead
	remaining := maxRunes - fixedRunes
	if remaining < 600 {
		remaining = 600
	}

	// Budget split:
	//   Section 5 (verbatim anti-distortion layer): 32%
	//   Section 2 (folded history + pinned):        28%
	//   Section 3 (recent segment tree):            25%
	//   Section 4 (workspace snapshot):             15%
	b5 := remaining * 32 / 100
	b2 := remaining * 28 / 100
	b3 := remaining * 25 / 100
	b4 := remaining - b5 - b2 - b3

	// Section 2: Folded History (top folds + pinned nodes)
	buildSec2 := func(budget int) string {
		if len(topFolds) == 0 && len(pinnedNodes) == 0 {
			return "## 2. 折叠的历史\n（尚无高层折叠或钉住的历史节点）"
		}
		var sb strings.Builder
		sb.WriteString("## 2. 折叠的历史\n")
		ordered := make([]protocol.Hebi8LineNode, 0, len(pinnedNodes)+len(topFolds))
		ordered = append(ordered, pinnedNodes...)
		ordered = append(ordered, topFolds...)

		perNodeBudget := budget / max(1, len(ordered))
		if perNodeBudget < 260 {
			perNodeBudget = 260
		}
		for _, n := range ordered {
			if runeLen(sb.String()) >= budget-120 {
				break
			}
			dateSpan := formatDateSpan(n.CoversFrom, n.CoversTo, n.CoversFromMs, n.CoversToMs)
			prefix := ""
			if pinnedSet[n.ID] {
				prefix = "[PINNED] "
			}
			var header string
			if n.Kind == protocol.Hebi8NodeFold {
				segCount := countCoveredSegments(n.ID, map[string]bool{})
				header = fmt.Sprintf("%s[%s · %s · 覆盖 %d 段] %s", prefix, n.ID, dateSpan, segCount, strings.TrimSpace(n.Title))
			} else {
				turns := 0
				if n.AttemptRange != nil {
					turns = n.AttemptRange.TurnCount
				}
				header = fmt.Sprintf("%s[%s · %s · %d 轮] %s", prefix, n.ID, dateSpan, turns, strings.TrimSpace(n.Title))
			}
			body := strings.TrimSpace(in.Summaries[n.ID])
			if body == "" {
				body = strings.TrimSpace(n.Summary)
			}
			avail := min(perNodeBudget, budget-runeLen(sb.String())-runeLen(header)-16)
			if avail < 80 {
				sb.WriteString("- " + header + "\n")
				continue
			}
			trimmedBody := trimAtSentenceBoundary(body, avail)
			sb.WriteString("### " + header + "\n")
			if trimmedBody != "" {
				sb.WriteString(trimmedBody + "\n\n")
			}
		}
		return strings.TrimRight(sb.String(), "\n")
	}

	// Section 3: Recent Segment Tree (earlier unfolded segments 1-line index; latest 2 full summary.md)
	buildSec3 := func(budget int) string {
		if len(unfoldedSegments) == 0 {
			return "## 3. 最近的节点树\n（暂无未折叠分段）"
		}
		var sb strings.Builder
		sb.WriteString("## 3. 最近的节点树\n")
		fullStart := len(unfoldedSegments) - 2
		if fullStart < 0 {
			fullStart = 0
		}
		if fullStart > 0 {
			for _, n := range unfoldedSegments[:fullStart] {
				line := formatSegmentIndexLine(n, pinnedSet[n.ID])
				if runeLen(sb.String())+runeLen(line)+2 > budget/2 {
					break
				}
				sb.WriteString("- " + line + "\n")
			}
			sb.WriteString("\n")
		}
		recentCount := len(unfoldedSegments) - fullStart
		remForFull := budget - runeLen(sb.String())
		perFull := remForFull / max(1, recentCount)
		if perFull < 240 {
			perFull = 240
		}
		for _, n := range unfoldedSegments[fullStart:] {
			idxLine := formatSegmentIndexLine(n, pinnedSet[n.ID])
			body := strings.TrimSpace(in.Summaries[n.ID])
			if body == "" {
				body = strings.TrimSpace(n.Summary)
			}
			avail := min(perFull, budget-runeLen(sb.String())-runeLen(idxLine)-16)
			sb.WriteString("### " + idxLine + "\n")
			if body != "" && avail >= 60 {
				sb.WriteString(trimAtSentenceBoundary(body, avail) + "\n\n")
			}
		}
		return strings.TrimRight(sb.String(), "\n")
	}

	// Section 4: Active Workspace Snapshot
	buildSec4 := func(budget int) string {
		snap := strings.TrimSpace(in.WorkspaceSnapshot)
		if snap == "" {
			return "## 4. 当前工作区快照\n（暂无活跃工作区快照）"
		}
		header := "## 4. 当前工作区快照\n"
		avail := budget - runeLen(header)
		if avail < 120 {
			avail = 120
		}
		return header + trimAtSentenceBoundary(snap, avail)
	}

	// Section 5: Verbatim Recent Utterances & Tail Q&A (anti-distortion layer)
	buildSec5 := func(budget int) string {
		var sb strings.Builder
		sb.WriteString("## 5. 最近的原话\n")

		prompts := in.RecentUserPrompts
		if len(prompts) > 4 {
			prompts = prompts[len(prompts)-4:]
		}
		tail := in.TailTurns
		if len(tail) > 2 {
			tail = tail[len(tail)-2:]
		}
		if len(prompts) == 0 && len(tail) == 0 {
			sb.WriteString("（暂无上段原话记录）")
			return sb.String()
		}

		// Reserve ~35% for the 4 raw user prompts, ~65% for the last 2 full Q&A turns.
		promptBudget := budget * 35 / 100
		if len(tail) == 0 {
			promptBudget = budget - 40
		}
		if len(prompts) > 0 {
			sb.WriteString("### 用户最近原话\n")
			perP := (promptBudget - 30) / len(prompts)
			if perP < 120 {
				perP = 120
			}
			for i, p := range prompts {
				clean := strings.TrimSpace(p)
				if clean == "" {
					continue
				}
				sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, trimAtSentenceBoundary(clean, perP)))
			}
			sb.WriteString("\n")
		}
		if len(tail) > 0 {
			sb.WriteString("### 上段结尾最近完整问答\n")
			rem := budget - runeLen(sb.String()) - 20
			perTurn := rem / len(tail)
			if perTurn < 200 {
				perTurn = 200
			}
			for _, t := range tail {
				uBudget := perTurn * 35 / 100
				aBudget := perTurn - uBudget - 40
				if uBudget < 80 {
					uBudget = 80
				}
				if aBudget < 100 {
					aBudget = 100
				}
				sb.WriteString(fmt.Sprintf("#### 第 %d 轮\n", t.AttemptNo))
				if u := strings.TrimSpace(t.UserPrompt); u != "" {
					sb.WriteString("用户：" + trimAtSentenceBoundary(u, uBudget) + "\n")
				}
				if a := strings.TrimSpace(t.AssistantReply); a != "" {
					sb.WriteString("Hebi8：" + trimAtSentenceBoundary(a, aBudget) + "\n")
				}
				sb.WriteString("\n")
			}
		}
		return strings.TrimRight(sb.String(), "\n")
	}

	s2 := buildSec2(b2)
	s3 := buildSec3(b3)
	s4 := buildSec4(b4)
	s5 := buildSec5(b5)

	assemble := func(p1, p2, p3, p4, p5, p6 string) string {
		return "<hebi8_fold_opening>\n" +
			strings.Join([]string{p1, p2, p3, p4, p5, p6}, "\n\n") +
			"\n</hebi8_fold_opening>"
	}

	out := assemble(sec1, s2, s3, s4, s5, sec6)
	if runeLen(out) <= maxRunes {
		return out
	}

	// Enforce hard budget by tightening sections in priority order (s4 -> s2 -> s3 -> s5),
	// always preserving sentence boundaries and keeping sections 1 and 6 intact.
	excess := runeLen(out) - maxRunes
	shrink := func(s *string, minKeep int) {
		if excess <= 0 {
			return
		}
		cur := runeLen(*s)
		if cur <= minKeep {
			return
		}
		cut := min(excess, cur-minKeep)
		*s = trimAtSentenceBoundary(*s, cur-cut)
		excess -= cur - runeLen(*s)
	}
	shrink(&s4, 160)
	shrink(&s2, 240)
	shrink(&s3, 240)
	shrink(&s5, 320)

	out = assemble(sec1, s2, s3, s4, s5, sec6)
	if runeLen(out) > maxRunes {
		// Last-resort tight clamp on s5/s3/s2/s4 while keeping opening XML tags balanced.
		innerBudget := maxRunes - runeLen("<hebi8_fold_opening>\n\n</hebi8_fold_opening>") - runeLen(sec1) - runeLen(sec6) - 24
		if innerBudget < 80 {
			return trimAtSentenceBoundary(out, maxRunes)
		}
		mid := trimAtSentenceBoundary(strings.Join([]string{s2, s3, s4, s5}, "\n\n"), innerBudget)
		out = "<hebi8_fold_opening>\n" + sec1 + "\n\n" + mid + "\n\n" + sec6 + "\n</hebi8_fold_opening>"
	}
	return out
}

func formatDateSpan(fromRFC, toRFC string, fromMs, toMs int64) string {
	f := parseNodeTime(fromRFC, fromMs)
	t := parseNodeTime(toRFC, toMs)
	if f.IsZero() && t.IsZero() {
		return "未知时间"
	}
	if f.IsZero() {
		f = t
	}
	if t.IsZero() {
		t = f
	}
	fd := f.Format("2006-01-02")
	td := t.Format("2006-01-02")
	if fd == td {
		return fd
	}
	return fd + " ~ " + td
}

func formatSegmentIndexLine(n protocol.Hebi8LineNode, pinned bool) string {
	f := parseNodeTime(n.CoversFrom, n.CoversFromMs)
	t := parseNodeTime(n.CoversTo, n.CoversToMs)
	timePart := "00-00 00:00~00:00"
	if !f.IsZero() {
		if t.IsZero() {
			t = f
		}
		if f.Format("01-02") == t.Format("01-02") {
			timePart = fmt.Sprintf("%s~%s", f.Format("01-02 15:04"), t.Format("15:04"))
		} else {
			timePart = fmt.Sprintf("%s~%s", f.Format("01-02 15:04"), t.Format("01-02 15:04"))
		}
	}
	tagsPart := "[]"
	if len(n.Tags) > 0 {
		tagsPart = "[" + strings.Join(n.Tags, ", ") + "]"
	}
	prefix := ""
	if pinned {
		prefix = "[PINNED] "
	}
	return fmt.Sprintf("%s%s | %s | %s | %s", prefix, n.ID, timePart, tagsPart, strings.TrimSpace(n.Title))
}

func parseNodeTime(rfc string, ms int64) time.Time {
	if ms > 0 {
		return time.UnixMilli(ms)
	}
	if strings.TrimSpace(rfc) != "" {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(rfc)); err == nil {
			return t
		}
	}
	return time.Time{}
}

func runeLen(s string) int {
	return len([]rune(s))
}

// trimAtSentenceBoundary trims s to at most maxRunes runes without cutting in the middle of a sentence.
// It searches the ENTIRE budget window for the last complete sentence terminator (。！？.!?).
// If not even a single complete sentence fits in maxRunes, it returns an explicit "[已省略]" marker
// rather than slicing mid-sentence.
func trimAtSentenceBoundary(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}

	// Search the ENTIRE budget window [0, maxRunes-1] backwards for the last complete sentence boundary.
	for i := maxRunes - 1; i >= 0; i-- {
		if isSentenceBoundaryAt(runes, i) {
			candidate := strings.TrimSpace(string(runes[:i+1]))
			if candidate != "" {
				return candidate
			}
		}
	}

	// Fallback: complete newline-terminated lines if the text has no sentence punctuation
	// (e.g. structured workspace snapshot bullet lists).
	const omittedMarker = "[已省略]"
	omittedLen := runeLen(omittedMarker)
	for i := maxRunes - 1; i >= 0; i-- {
		if runes[i] == '\n' {
			candidate := strings.TrimSpace(string(runes[:i]))
			if candidate != "" {
				if runeLen(candidate)+1+omittedLen <= maxRunes {
					return candidate + "\n" + omittedMarker
				}
				return candidate
			}
		}
	}

	// Budget cannot fit even the first complete sentence: mark explicitly instead of cutting mid-sentence.
	if maxRunes >= omittedLen {
		return omittedMarker
	}
	return ""
}

// trimMetadataSnippet trims short metadata strings (such as node titles or keyword snippets)
// cleanly to maxRunes runes, preferring punctuation or whitespace boundaries.
func trimMetadataSnippet(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	const suffix = "…"
	suffixRunes := runeLen(suffix)
	if maxRunes <= suffixRunes+1 {
		return string(runes[:maxRunes])
	}
	limit := maxRunes - suffixRunes
	for i := limit - 1; i >= 0; i-- {
		if isSentenceBoundaryAt(runes, i) {
			if cand := strings.TrimSpace(string(runes[:i+1])); cand != "" {
				return cand
			}
		}
	}
	for i := limit - 1; i >= limit/2; i-- {
		r := runes[i]
		if r == '，' || r == '、' || r == '；' || r == ',' || r == ';' || r == ':' || unicode.IsSpace(r) {
			if cand := strings.TrimSpace(string(runes[:i])); cand != "" {
				return cand + suffix
			}
		}
	}
	return strings.TrimSpace(string(runes[:limit])) + suffix
}

func isSentenceBoundaryAt(runes []rune, idx int) bool {
	if idx < 0 || idx >= len(runes) {
		return false
	}
	r := runes[idx]
	switch r {
	case '。', '！', '？', '!', '?':
		return true
	case '.':
		// Do not treat decimal numbers, version numbers (e.g. v2.10, 3.14), or dotted
		// identifiers/filenames (e.g. hub.yaml, foo.bar) as sentence terminators.
		hasPrev := idx > 0
		hasNext := idx+1 < len(runes)
		if hasNext && unicode.IsDigit(runes[idx+1]) {
			return false
		}
		if hasPrev && unicode.IsDigit(runes[idx-1]) && hasNext && (unicode.IsDigit(runes[idx+1]) || unicode.IsLetter(runes[idx+1])) {
			return false
		}
		if hasPrev && unicode.IsLetter(runes[idx-1]) && hasNext && unicode.IsLetter(runes[idx+1]) {
			return false
		}
		return true
	default:
		return false
	}
}
