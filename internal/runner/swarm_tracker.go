package runner

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	acp "github.com/coder/acp-go-sdk"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

// SwarmProjection holds board member/message projections derived strictly from completed tool calls.
type SwarmProjection struct {
	Members  []map[string]any
	Messages []map[string]any
}

type mergedToolCall struct {
	ToolCallID             string
	Name                   string
	Title                  string
	Status                 string
	RawInput               json.RawMessage
	RawOutput              json.RawMessage
	WorkerTracked          bool
	ExitCodeTracked        bool
	BoardMsgEmitted        bool
	ReadBoardTracked       bool
	SubagentStartedEmitted bool
	SubagentFinalEmitted   bool
	SubagentActionTracked  bool
	SubagentName           string
	SubagentRole           string
	SubagentBrief          string
	SubagentExternalID     string
	WorkerName             string
}

// ActiveSwarmTracker merges tool_call and tool_call_update notifications by toolCallId.
// Only status == "completed" calls update activeWorkers or emit board projections,
// preventing failed calls or streamed argument fragments from corrupting worker accounting.
// Separate per-aspect flags ensure late rawInput/rawOutput updates after completed are still observed (S3).
// Native subagent tool calls (invoke_subagent / Claude Agent / Task) are also projected to board_members
// with sourceKind="native_subagent" without affecting activeWorkers or swarm_monitoring invariants.
type ActiveSwarmTracker struct {
	mu                          sync.Mutex
	attemptID                   string
	calls                       map[string]*mergedToolCall
	activeWorkers               map[string]bool
	subagentsByExtID            map[string]string
	usedSwarm                   bool
	rejectedToolCount           int
	unresolvedRejectedToolCount int
	nonZeroExitCount            int
	unresolvedNonZeroExitCount  int
	failedCommands              map[string]int
}

func NewActiveSwarmTracker(attemptID string) *ActiveSwarmTracker {
	return &ActiveSwarmTracker{
		attemptID:        attemptID,
		calls:            map[string]*mergedToolCall{},
		activeWorkers:    map[string]bool{},
		subagentsByExtID: map[string]string{},
		failedCommands:   map[string]int{},
	}
}

// ObserveToolCall processes an initial SessionUpdateToolCall frame.
func (t *ActiveSwarmTracker) ObserveToolCall(tc *acp.SessionUpdateToolCall) SwarmProjection {
	if tc == nil {
		return SwarmProjection{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	id := string(tc.ToolCallId)
	rec := t.getOrCreateLocked(id)
	if tc.Name != nil && *tc.Name != "" {
		rec.Name = *tc.Name
	}
	if tc.Title != "" {
		rec.Title = tc.Title
	}
	if tc.Status != "" {
		rec.Status = string(tc.Status)
	}
	if len(tc.RawInput) > 0 {
		rec.RawInput = append(json.RawMessage(nil), tc.RawInput...)
	}
	if len(tc.RawOutput) > 0 {
		rec.RawOutput = append(json.RawMessage(nil), tc.RawOutput...)
	}
	subProj := t.observeNativeSubagentLocked(rec)
	swarmProj := t.evaluateCompletedLocked(rec)
	return mergeProjections(subProj, swarmProj)
}

// ObserveToolCallUpdate merges a SessionToolCallUpdate patch into the toolCallId ledger.
func (t *ActiveSwarmTracker) ObserveToolCallUpdate(tu *acp.SessionToolCallUpdate) SwarmProjection {
	if tu == nil {
		return SwarmProjection{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	id := string(tu.ToolCallId)
	rec := t.getOrCreateLocked(id)
	if v, ok := tu.Name.Value(); ok && v != "" {
		rec.Name = v
	}
	if v, ok := tu.Title.Value(); ok && v != "" {
		rec.Title = v
	}
	if v, ok := tu.Status.Value(); ok && v != "" {
		rec.Status = string(v)
	}
	if v, ok := tu.RawInput.Value(); ok && len(v) > 0 {
		rec.RawInput = append(json.RawMessage(nil), v...)
	}
	if v, ok := tu.RawOutput.Value(); ok && len(v) > 0 {
		rec.RawOutput = append(json.RawMessage(nil), v...)
	}
	subProj := t.observeNativeSubagentLocked(rec)
	swarmProj := t.evaluateCompletedLocked(rec)
	return mergeProjections(subProj, swarmProj)
}

func (t *ActiveSwarmTracker) RecordToolRejection() {
	t.mu.Lock()
	t.rejectedToolCount++
	t.unresolvedRejectedToolCount++
	t.mu.Unlock()
}

func (t *ActiveSwarmTracker) Stats() (activeWorkers int, usedSwarm bool, rejectedTools int, nonZeroExits int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.activeWorkers), t.usedSwarm, t.rejectedToolCount, t.nonZeroExitCount
}

func (t *ActiveSwarmTracker) UnresolvedCounts() (unresolvedRejected int, unresolvedNonZero int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.unresolvedRejectedToolCount, t.unresolvedNonZeroExitCount
}

// WorkerNameForToolCall returns the verifiable workerName associated with toolCallID, if known.
func (t *ActiveSwarmTracker) WorkerNameForToolCall(toolCallID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if rec, ok := t.calls[toolCallID]; ok {
		return rec.WorkerName
	}
	return ""
}

func (t *ActiveSwarmTracker) HasActiveWorkers() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.activeWorkers) > 0
}

func (t *ActiveSwarmTracker) ActiveWorkerCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.activeWorkers)
}

func (t *ActiveSwarmTracker) ActiveWorkerNames() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.activeWorkers))
	for name := range t.activeWorkers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (t *ActiveSwarmTracker) getOrCreateLocked(id string) *mergedToolCall {
	rec, ok := t.calls[id]
	if !ok {
		rec = &mergedToolCall{ToolCallID: id}
		t.calls[id] = rec
	}
	return rec
}

func (t *ActiveSwarmTracker) evaluateCompletedLocked(rec *mergedToolCall) SwarmProjection {
	var proj SwarmProjection
	// Hard invariant: ONLY status == "completed" transitions worker accounting / board state!
	if rec.Status != string(acp.ToolCallStatusCompleted) {
		return proj
	}

	var inMap, outMap map[string]any
	if len(rec.RawInput) > 0 {
		_ = json.Unmarshal(rec.RawInput, &inMap)
	}
	if len(rec.RawOutput) > 0 {
		_ = json.Unmarshal(rec.RawOutput, &outMap)
	}

	// Boundary #5, B1, S3: track commands returning exit_code (even if rawOutput arrives on a later patch).
	// ONLY a subsequent exit-0 run of the SAME verification/command signature clears that failure —
	// unrelated commands (e.g. `ls`, `echo ok`) exiting 0 MUST NOT clear a failed test run!
	if !rec.ExitCodeTracked && outMap != nil {
		if ec, ok := extractExitCode(outMap); ok {
			rec.ExitCodeTracked = true
			sig := extractCommandSignature(inMap, rec.ToolCallID)
			if ec != 0 {
				t.nonZeroExitCount++
				t.failedCommands[sig]++
				t.recomputeUnresolvedNonZeroLocked()
			} else if t.failedCommands[sig] > 0 {
				delete(t.failedCommands, sig)
				t.recomputeUnresolvedNonZeroLocked()
			}
		}
	}

	toolName := normalizeToolName(rec.Name, rec.Title)

	switch toolName {
	case "swarm":
		if !rec.WorkerTracked && inMap != nil {
			action := strings.ToLower(getMapString(inMap, "action"))
			switch action {
			case "add":
				names := extractSwarmWorkerNames(inMap)
				if len(names) > 0 {
					rec.WorkerTracked = true
					rec.WorkerName = names[0]
					brief := getMapString(inMap, "brief")
					for _, w := range names {
						t.activeWorkers[w] = true
						t.usedSwarm = true
						proj.Members = append(proj.Members, map[string]any{
							"memberName":  w,
							"state":       string(protocol.BoardMemberWorking),
							"currentStep": "assigned",
							"sourceKind":  "native_swarm",
							"brief":       truncateTrackerText(brief, 240),
						})
						if brief != "" {
							proj.Messages = append(proj.Messages, map[string]any{
								"authorName":    "coordinator",
								"targetMention": "@" + w,
								"body":          brief,
								"externalKey":   fmt.Sprintf("acp:%s:%s:%s", t.attemptID, rec.ToolCallID, w),
							})
						}
					}
				}
			case "kill":
				name := getMapString(inMap, "name")
				if name != "" {
					rec.WorkerTracked = true
					rec.WorkerName = name
					delete(t.activeWorkers, name)
					proj.Members = append(proj.Members, map[string]any{
						"memberName":  name,
						"state":       string(protocol.BoardMemberKilled),
						"currentStep": "killed",
					})
				}
			}
		}

	case "post_message", "board_post":
		if !rec.BoardMsgEmitted && inMap != nil {
			body := getMapString(inMap, "body")
			if body == "" {
				body = getMapString(inMap, "message")
			}
			if body == "" {
				body = getMapString(inMap, "content")
			}
			if body != "" {
				rec.BoardMsgEmitted = true
				author := getMapString(inMap, "author")
				if author == "" {
					author = getMapString(inMap, "authorName")
				}
				if author == "" {
					author = "coordinator"
				}
				rec.WorkerName = author
				mention := getMapString(inMap, "mention")
				if mention == "" {
					mention = getMapString(inMap, "targetMention")
				}
				proj.Messages = append(proj.Messages, map[string]any{
					"authorName":    author,
					"targetMention": mention,
					"body":          body,
					"externalKey":   fmt.Sprintf("acp:%s:%s", t.attemptID, rec.ToolCallID),
				})
			}
		}

	case "read_board":
		if !rec.ReadBoardTracked && (inMap != nil || outMap != nil) {
			member := getMapString(inMap, "member")
			if member == "" {
				member = getMapString(inMap, "memberName")
			}
			if member == "" {
				member = "coordinator"
			}
			var latestSeq int64
			hasSeq := false
			if outMap != nil {
				if v, ok := outMap["latestSeq"].(float64); ok {
					latestSeq = int64(v)
					hasSeq = true
				} else if v, ok := outMap["lastSeq"].(float64); ok {
					latestSeq = int64(v)
					hasSeq = true
				}
			}
			if hasSeq || inMap != nil {
				rec.ReadBoardTracked = hasSeq
				rec.WorkerName = member
				proj.Members = append(proj.Members, map[string]any{
					"memberName":      member,
					"state":           string(protocol.BoardMemberWorking),
					"lastConsumedSeq": latestSeq,
					"currentStep":     "read_board",
				})
			}
		}
	}

	return proj
}

func extractExitCode(outMap map[string]any) (int, bool) {
	if outMap == nil {
		return 0, false
	}
	if ec, ok := outMap["exit_code"].(float64); ok {
		return int(ec), true
	}
	if ec, ok := outMap["exitCode"].(float64); ok {
		return int(ec), true
	}
	return 0, false
}

func normalizeToolName(name, title string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if idx := strings.LastIndex(n, ":"); idx >= 0 {
		n = n[idx+1:]
	}
	if n != "" {
		return n
	}
	tl := strings.ToLower(strings.TrimSpace(title))
	switch {
	case strings.HasPrefix(tl, "swarm"):
		return "swarm"
	case strings.Contains(tl, "post_message"):
		return "post_message"
	case strings.Contains(tl, "read_board"):
		return "read_board"
	case strings.Contains(tl, "invoke_subagent"):
		return "invoke_subagent"
	case strings.Contains(tl, "manage_subagents"):
		return "manage_subagents"
	case strings.Contains(tl, "send_message"):
		return "send_message"
	default:
		return tl
	}
}

func mergeProjections(a, b SwarmProjection) SwarmProjection {
	if len(a.Members) == 0 && len(a.Messages) == 0 {
		return b
	}
	if len(b.Members) == 0 && len(b.Messages) == 0 {
		return a
	}
	return SwarmProjection{
		Members:  append(a.Members, b.Members...),
		Messages: append(a.Messages, b.Messages...),
	}
}

func truncateTrackerText(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if s == "" || maxRunes <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}

func getMapStringAny(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v := getMapString(m, k); v != "" {
			return v
		}
	}
	return ""
}

// observeNativeSubagentLocked projects native invoke_subagent / Claude Agent / Task / send_message / manage_subagents
// calls to board_members with sourceKind="native_subagent". It NEVER mutates activeWorkers or usedSwarm.
func (t *ActiveSwarmTracker) observeNativeSubagentLocked(rec *mergedToolCall) SwarmProjection {
	var proj SwarmProjection
	toolName := normalizeToolName(rec.Name, rec.Title)

	var inMap, outMap map[string]any
	if len(rec.RawInput) > 0 {
		_ = json.Unmarshal(rec.RawInput, &inMap)
	}
	if len(rec.RawOutput) > 0 {
		_ = json.Unmarshal(rec.RawOutput, &outMap)
	}

	switch toolName {
	case "invoke_subagent", "agent", "task":
		// Extract role, display name, brief, and externalSessionId incrementally across updates.
		if role := getMapStringAny(inMap, "role", "Role", "subagent_type", "TypeName", "type_name"); role != "" {
			rec.SubagentRole = role
		}
		if brief := getMapStringAny(inMap, "prompt", "Prompt", "description", "Description"); brief != "" {
			rec.SubagentBrief = truncateTrackerText(brief, 240)
		}
		if rec.SubagentName == "" {
			name := getMapStringAny(inMap, "name", "Name", "description", "Description", "role", "Role", "subagent_type", "TypeName")
			if name == "" && rec.Title != "" && !strings.EqualFold(rec.Title, toolName) {
				name = rec.Title
			}
			if name == "" {
				shortID := rec.ToolCallID
				if len(shortID) > 8 {
					shortID = shortID[:8]
				}
				if shortID == "" {
					shortID = "worker"
				}
				name = "subagent-" + shortID
			}
			rec.SubagentName = truncateTrackerText(name, 48)
		}
		if rec.SubagentName != "" {
			rec.WorkerName = rec.SubagentName
		}

		extID := getMapStringAny(outMap, "conversationId", "conversation_id", "agentId", "agent_id", "sessionId", "session_id", "taskId", "task_id")
		if extID == "" && len(rec.RawOutput) > 0 {
			// Check nested result map if present
			if resMap, ok := outMap["result"].(map[string]any); ok {
				extID = getMapStringAny(resMap, "conversationId", "conversation_id", "agentId", "sessionId")
			}
		}
		if extID != "" {
			rec.SubagentExternalID = extID
			if rec.SubagentName != "" {
				t.subagentsByExtID[extID] = rec.SubagentName
			}
		}

		status := strings.ToLower(strings.TrimSpace(rec.Status))
		isDone := status == string(acp.ToolCallStatusCompleted) || status == string(acp.ToolCallStatusFailed)
		if !isDone {
			if !rec.SubagentStartedEmitted && rec.SubagentName != "" {
				rec.SubagentStartedEmitted = true
				m := map[string]any{
					"memberName":  rec.SubagentName,
					"state":       string(protocol.BoardMemberWorking),
					"currentStep": "running",
					"sourceKind":  "native_subagent",
				}
				if rec.SubagentRole != "" {
					m["role"] = rec.SubagentRole
				}
				if rec.SubagentBrief != "" {
					m["brief"] = rec.SubagentBrief
				}
				if rec.SubagentExternalID != "" {
					m["externalSessionId"] = rec.SubagentExternalID
				}
				proj.Members = append(proj.Members, m)
			}
			return proj
		}

		// Completed or failed: emit final stopped state (also re-emit once if externalSessionId arrived late).
		if (!rec.SubagentFinalEmitted || (extID != "" && !rec.SubagentActionTracked)) && rec.SubagentName != "" {
			rec.SubagentStartedEmitted = true
			rec.SubagentFinalEmitted = true
			if extID != "" {
				rec.SubagentActionTracked = true
			}
			step := "completed"
			if status == string(acp.ToolCallStatusFailed) {
				step = "failed"
			}
			m := map[string]any{
				"memberName":  rec.SubagentName,
				"state":       string(protocol.BoardMemberStopped),
				"currentStep": step,
				"sourceKind":  "native_subagent",
			}
			if rec.SubagentRole != "" {
				m["role"] = rec.SubagentRole
			}
			if rec.SubagentBrief != "" {
				m["brief"] = rec.SubagentBrief
			}
			if rec.SubagentExternalID != "" {
				m["externalSessionId"] = rec.SubagentExternalID
			}
			proj.Members = append(proj.Members, m)
		}

	case "send_message":
		if strings.ToLower(strings.TrimSpace(rec.Status)) != string(acp.ToolCallStatusCompleted) || rec.SubagentActionTracked || inMap == nil {
			return proj
		}
		recipient := getMapStringAny(inMap, "recipient", "Recipient")
		if recipient == "" {
			return proj
		}
		if memberName, ok := t.subagentsByExtID[recipient]; ok && memberName != "" {
			rec.SubagentActionTracked = true
			rec.WorkerName = memberName
			proj.Members = append(proj.Members, map[string]any{
				"memberName":        memberName,
				"state":             string(protocol.BoardMemberWorking),
				"currentStep":       "messaged",
				"sourceKind":        "native_subagent",
				"externalSessionId": recipient,
			})
		}

	case "manage_subagents":
		if strings.ToLower(strings.TrimSpace(rec.Status)) != string(acp.ToolCallStatusCompleted) || rec.SubagentActionTracked || inMap == nil {
			return proj
		}
		action := strings.ToLower(getMapStringAny(inMap, "action", "Action"))
		switch action {
		case "kill":
			ids := extractAnyStringSlice(inMap, "conversationIds", "ConversationIds")
			if len(ids) > 0 {
				rec.SubagentActionTracked = true
				for _, cid := range ids {
					if memberName, ok := t.subagentsByExtID[cid]; ok && memberName != "" {
						proj.Members = append(proj.Members, map[string]any{
							"memberName":        memberName,
							"state":             string(protocol.BoardMemberKilled),
							"currentStep":       "killed",
							"sourceKind":        "native_subagent",
							"externalSessionId": cid,
						})
					}
				}
			}
		case "kill_all":
			if len(t.subagentsByExtID) > 0 {
				rec.SubagentActionTracked = true
				for cid, memberName := range t.subagentsByExtID {
					proj.Members = append(proj.Members, map[string]any{
						"memberName":        memberName,
						"state":             string(protocol.BoardMemberKilled),
						"currentStep":       "killed",
						"sourceKind":        "native_subagent",
						"externalSessionId": cid,
					})
				}
			}
		}
	}
	return proj
}

func extractAnyStringSlice(m map[string]any, keys ...string) []string {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok || raw == nil {
			continue
		}
		if arr, ok := raw.([]any); ok {
			out := make([]string, 0, len(arr))
			for _, it := range arr {
				if s, ok := it.(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, strings.TrimSpace(s))
				}
			}
			return out
		}
	}
	return nil
}

func extractSwarmWorkerNames(inMap map[string]any) []string {
	base := getMapString(inMap, "name")
	if base == "" {
		return nil
	}
	if tasks, ok := inMap["tasks"].([]any); ok && len(tasks) > 0 {
		out := make([]string, len(tasks))
		for i := range tasks {
			out[i] = fmt.Sprintf("%s-%d", base, i+1)
		}
		return out
	}
	if nFloat, ok := inMap["n"].(float64); ok && int(nFloat) > 1 {
		n := int(nFloat)
		out := make([]string, n)
		for i := range n {
			out[i] = fmt.Sprintf("%s-%d", base, i+1)
		}
		return out
	}
	return []string{base}
}

func getMapString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func (t *ActiveSwarmTracker) recomputeUnresolvedNonZeroLocked() {
	total := 0
	for _, n := range t.failedCommands {
		total += n
	}
	t.unresolvedNonZeroExitCount = total
}

func extractCommandSignature(inMap map[string]any, fallbackToolCallID string) string {
	for _, k := range []string{"command", "cmd", "CommandLine"} {
		if raw := getMapString(inMap, k); raw != "" {
			fields := strings.Fields(raw)
			if len(fields) == 0 {
				break
			}
			// Normalize executable + subcommand (e.g., "go test", "pnpm build", "pytest")
			if len(fields) >= 2 && !strings.HasPrefix(fields[1], "-") {
				return strings.ToLower(fields[0] + " " + fields[1])
			}
			return strings.ToLower(fields[0])
		}
	}
	return "call:" + fallbackToolCallID
}
