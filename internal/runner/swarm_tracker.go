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
	ToolCallID       string
	Name             string
	Title            string
	Status           string
	RawInput         json.RawMessage
	RawOutput        json.RawMessage
	WorkerTracked    bool
	ExitCodeTracked  bool
	BoardMsgEmitted  bool
	ReadBoardTracked bool
	WorkerName       string
}

// ActiveSwarmTracker merges tool_call and tool_call_update notifications by toolCallId.
// Only status == "completed" calls update activeWorkers or emit board projections,
// preventing failed calls or streamed argument fragments from corrupting worker accounting.
// Separate per-aspect flags ensure late rawInput/rawOutput updates after completed are still observed (S3).
type ActiveSwarmTracker struct {
	mu                          sync.Mutex
	attemptID                   string
	calls                       map[string]*mergedToolCall
	activeWorkers               map[string]bool
	usedSwarm                   bool
	rejectedToolCount           int
	unresolvedRejectedToolCount int
	nonZeroExitCount            int
	unresolvedNonZeroExitCount  int
	failedCommands              map[string]int
}

func NewActiveSwarmTracker(attemptID string) *ActiveSwarmTracker {
	return &ActiveSwarmTracker{
		attemptID:      attemptID,
		calls:          map[string]*mergedToolCall{},
		activeWorkers:  map[string]bool{},
		failedCommands: map[string]int{},
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
	return t.evaluateCompletedLocked(rec)
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
	return t.evaluateCompletedLocked(rec)
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
	default:
		return tl
	}
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
