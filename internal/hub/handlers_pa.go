package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

const paDeltaTokenBudgetChars = 16000 // ~4,000 tokens hard budget for <network_delta>

func (s *Server) handlePABrief(w http.ResponseWriter, r *http.Request) {
	var req protocol.PABriefRequest
	if r.Body != nil {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
			return
		}
	}
	s.mu.RLock()
	machineMeta := s.machines.Machines
	s.mu.RUnlock()

	brief, err := s.store.BuildPABrief(r.Context(), req.SinceHours, strings.TrimSpace(req.ProjectID), machineMeta)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PA_BRIEF", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, brief)
}

func (s *Server) handleListPAThreads(w http.ResponseWriter, r *http.Request) {
	threads, err := s.store.ListPAThreads(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PA_THREADS", err.Error())
		return
	}
	st, _ := s.store.GetPAState(r.Context(), "default")
	writeJSON(w, http.StatusOK, map[string]any{
		"threads": threads,
		"paState": st,
	})
}

func (s *Server) handleCreatePAThread(w http.ResponseWriter, r *http.Request) {
	var req protocol.CreatePAThreadRequest
	if r.Body != nil {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
			return
		}
	}
	thread, state, ev, err := s.store.CreatePAThread(r.Context(), "default", req.Title)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PA_THREAD_CREATE", err.Error())
		return
	}
	if ev != nil {
		s.broker.Broadcast(*ev)
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"thread":  thread,
		"paState": state,
	})
}

func (s *Server) handleGetPAThread(w http.ResponseWriter, r *http.Request) {
	threadID := chi.URLParam(r, "threadId")
	detail, err := s.store.GetPAThreadDetail(r.Context(), "default", threadID)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_PA_THREAD_NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PA_THREAD_GET", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handlePAResetSession(w http.ResponseWriter, r *http.Request) {
	s.paMu.Lock()
	defer s.paMu.Unlock()

	thread, state, ev, err := s.store.CreatePAThread(r.Context(), "default", "")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PA_RESET", err.Error())
		return
	}
	if ev != nil {
		s.broker.Broadcast(*ev)
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"thread":  thread,
		"paState": state,
	})
}

func isPAWriteAction(action string) bool {
	switch action {
	case "create_task", "send_task_followup", "cancel_task":
		return true
	default:
		return false
	}
}

// handlePAChat executes one serialized PA turn:
// 1. Per-PA mutex lock prevents interleaved watermark races.
// 2. Validates threadId existence BEFORE executing any side-effecting action (S5).
// 3. Single read snapshot locks H = MAX(seq) and reads deterministic events in (lastSeq, H].
// 4. Formats budgeted <network_delta> (preserving open inbox > blocked/failed > annotations/approvals > deliveries).
// 5. Executes optional deterministic read/dispatch action (with post-route ssh_deploy 403 block and
//    mandatory idempotency keys on all write actions per S5).
// 6. Advances pa_state.last_read_event_seq strictly to H ONLY on success (H+1 events stay for next turn).
func (s *Server) handlePAChat(w http.ResponseWriter, r *http.Request) {
	var req protocol.PAChatRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" && req.Action == nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_EMPTY_MESSAGE", "message or action is required")
		return
	}

	s.paMu.Lock()
	defer s.paMu.Unlock()

	ctx := r.Context()

	// S5: Validate target PA thread exists BEFORE executing any action side effects.
	if err := s.store.CheckPAThreadExists(ctx, req.ThreadID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "ERR_PA_THREAD_NOT_FOUND", err.Error())
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "ERR_PA_THREAD_CHECK", err.Error())
		return
	}

	// Step 1: Lock upper bound H = MAX(seq) and read (lastSeq, H]
	snap, err := s.store.ReadPADeltaSnapshot(ctx, "default")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PA_SNAPSHOT", err.Error())
		return
	}

	// Step 2: Build prioritized budgeted delta summary (<= 4k tokens)
	deltaText, foldedRange := summarizePADelta(snap.Events, paDeltaTokenBudgetChars)

	// Optional test hook invoked while PA turn is "thinking" (before watermark H commit)
	// so tests can inject an H+1 event and verify PA advances strictly to H without skipping H+1.
	if s.paBeforeCommitHook != nil {
		s.paBeforeCommitHook(snap.FromSeq, snap.HighSeq)
	}

	var actions []protocol.PAActionResult
	var replyParts []string

	// Step 3: Execute explicit PA control action (strictly scoped as ActorPA -> ssh_deploy returns 403!)
	if req.Action != nil {
		act := req.Action
		actionName := strings.TrimSpace(act.Action)

		// Pre-check deploy security guard on write actions targeting ssh_deploy so 403 takes precedence.
		if actionName == "create_task" {
			pid := strings.TrimSpace(act.ProjectID)
			if pid == "" {
				pid = protocol.ScratchProjectID
			}
			if proj, err := s.store.GetProject(ctx, pid); err == nil {
				if route, err := s.resolveExecutionRoute(proj, act.RequiredCapability, act.Adapter); err == nil {
					if err := enforcePostRouteGuard(protocol.ActorPA, route); err != nil {
						writeAPIError(w, http.StatusForbidden, "ERR_DEPLOY_FORBIDDEN", err.Error())
						return
					}
				}
			}
		} else if actionName == "send_task_followup" && strings.TrimSpace(act.TaskID) != "" {
			if task, err := s.store.GetTask(ctx, act.TaskID); err == nil {
				if proj, err := s.store.GetProject(ctx, task.ProjectID); err == nil {
					reqCap := strings.TrimSpace(act.RequiredCapability)
					if reqCap == "" {
						reqCap = task.RequiredCapability
					}
					reqAdp := act.Adapter
					if reqAdp == "" {
						reqAdp = task.PreferredAdapter
					}
					if route, err := s.resolveExecutionRoute(proj, reqCap, reqAdp); err == nil {
						if err := enforcePostRouteGuard(protocol.ActorPA, route); err != nil {
							writeAPIError(w, http.StatusForbidden, "ERR_DEPLOY_FORBIDDEN", err.Error())
							return
						}
					}
				}
			}
		}

		// S5: Enforce idempotency key on all PA write actions and replay cached receipts cleanly.
		idempKey := strings.TrimSpace(act.IdempotencyKey)
		if idempKey == "" {
			idempKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		}
		replayed := false
		if isPAWriteAction(actionName) {
			if idempKey == "" {
				writeAPIError(w, http.StatusBadRequest, "ERR_IDEMPOTENCY_KEY_REQUIRED", fmt.Sprintf("PA write action %q requires idempotencyKey or Idempotency-Key header", actionName))
				return
			}
			prevAction, prevRes, found, err := s.store.GetPAActionReceipt(ctx, idempKey)
			if err != nil {
				writeAPIError(w, http.StatusInternalServerError, "ERR_PA_IDEMPOTENCY", err.Error())
				return
			}
			if found {
				if prevAction != actionName {
					writeAPIError(w, http.StatusConflict, "ERR_IDEMPOTENCY_CONFLICT", fmt.Sprintf("idempotencyKey %q already bound to action %q", idempKey, prevAction))
					return
				}
				actions = append(actions, prevRes)
				replyParts = append(replyParts, prevRes.Summary+"（幂等回放）")
				replayed = true
			}
		}

		if !replayed {
			switch actionName {
			case "create_task":
				pid := strings.TrimSpace(act.ProjectID)
				if pid == "" {
					pid = protocol.ScratchProjectID
				}
				createResp, status, code, err := s.createTaskInternal(ctx, pid, protocol.CreateTaskRequest{
					Goal:               act.Goal,
					RequiredCapability: act.RequiredCapability,
					Adapter:            act.Adapter,
					Config:             act.Config,
					IdempotencyKey:     idempKey,
				}, protocol.ActorPA, "pa:default", idempKey)
				if err != nil {
					writeAPIError(w, status, code, err.Error())
					return
				}
				actRes := protocol.PAActionResult{
					Action:  "create_task",
					Status:  "ok",
					Summary: fmt.Sprintf("已为 %s 创建任务 %s（dispatched=%v）", pid, createResp.Task.TaskID, createResp.Dispatched),
					Data: map[string]any{
						"taskId":     createResp.Task.TaskID,
						"projectId":  createResp.Task.ProjectID,
						"dispatched": createResp.Dispatched,
					},
				}
				_ = s.store.SavePAActionReceipt(ctx, idempKey, actionName, actRes)
				actions = append(actions, actRes)
				replyParts = append(replyParts, fmt.Sprintf("已派发任务 `%s` 到项目 `%s`（目标：%s）。", createResp.Task.TaskID, createResp.Task.ProjectID, createResp.Task.Goal))

			case "send_task_followup":
				fResp, status, code, err := s.followupTaskInternal(ctx, act.TaskID, protocol.FollowupTaskRequest{
					Message:            act.Message,
					Mode:               act.Mode,
					RequiredCapability: act.RequiredCapability,
					Adapter:            act.Adapter,
					Config:             act.Config,
				}, protocol.ActorPA, "pa:default")
				if err != nil {
					writeAPIError(w, status, code, err.Error())
					return
				}
				actRes := protocol.PAActionResult{
					Action:  "send_task_followup",
					Status:  "ok",
					Summary: fmt.Sprintf("已向任务 %s 发送追问（mode=%s）", act.TaskID, fResp.Mode),
					Data: map[string]any{
						"taskId": act.TaskID,
						"mode":   fResp.Mode,
					},
				}
				_ = s.store.SavePAActionReceipt(ctx, idempKey, actionName, actRes)
				actions = append(actions, actRes)
				replyParts = append(replyParts, fmt.Sprintf("已向任务 `%s` 发送追问（模式：%s）。", act.TaskID, fResp.Mode))

			case "cancel_task":
				cResp, status, code, err := s.cancelTaskInternal(ctx, act.TaskID, "pa:default", act.Message)
				if err != nil {
					writeAPIError(w, status, code, err.Error())
					return
				}
				actRes := protocol.PAActionResult{
					Action:  "cancel_task",
					Status:  "ok",
					Summary: fmt.Sprintf("已请求取消任务 %s（attempt=%s）", cResp.TaskID, cResp.AttemptID),
					Data: map[string]any{
						"taskId":    cResp.TaskID,
						"attemptId": cResp.AttemptID,
					},
				}
				_ = s.store.SavePAActionReceipt(ctx, idempKey, actionName, actRes)
				actions = append(actions, actRes)
				replyParts = append(replyParts, fmt.Sprintf("已取消任务 `%s` 的活跃尝试 `%s`。", cResp.TaskID, cResp.AttemptID))

			case "list_projects_and_machines":
				s.mu.RLock()
				mm := s.machines.Machines
				s.mu.RUnlock()
				boot, err := s.store.Bootstrap(ctx, mm)
				if err != nil {
					writeAPIError(w, http.StatusInternalServerError, "ERR_PA_LIST", err.Error())
					return
				}
				actions = append(actions, protocol.PAActionResult{
					Action:  "list_projects_and_machines",
					Status:  "ok",
					Summary: fmt.Sprintf("%d 个项目，%d 台机器", len(boot.Projects), len(boot.Machines)),
					Data: map[string]any{
						"projects": len(boot.Projects),
						"machines": len(boot.Machines),
					},
				})
				replyParts = append(replyParts, fmt.Sprintf("当前共有 %d 个项目、%d 台机器、%d 项待拍板。", len(boot.Projects), len(boot.Machines), len(boot.OpenInboxItems)))

			case "read_task_timeline":
				tl, err := s.store.GetTaskTimeline(ctx, act.TaskID, 0, false)
				if errors.Is(err, store.ErrNotFound) {
					writeAPIError(w, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err.Error())
					return
				}
				if err != nil {
					writeAPIError(w, http.StatusInternalServerError, "ERR_PA_TIMELINE", err.Error())
					return
				}
				actions = append(actions, protocol.PAActionResult{
					Action:  "read_task_timeline",
					Status:  "ok",
					Summary: fmt.Sprintf("任务 %s 当前状态 %s，%d 次尝试，%d 个交付", tl.Task.TaskID, tl.Task.State, len(tl.Attempts), len(tl.Deliveries)),
					Data: map[string]any{
						"taskId":     tl.Task.TaskID,
						"state":      tl.Task.State,
						"attempts":   len(tl.Attempts),
						"deliveries": len(tl.Deliveries),
					},
				})
				replyParts = append(replyParts, fmt.Sprintf("任务 `%s`（%s）当前状态为 `%s`，共 %d 次尝试、%d 个交付、%d 条未决批注。",
					tl.Task.TaskID, tl.Task.Title, tl.Task.State, len(tl.Attempts), len(tl.Deliveries), tl.Task.OpenAnnotationCount))

			case "brief", "":
				// Handled below via stateless brief aggregation

			default:
				writeAPIError(w, http.StatusBadRequest, "ERR_UNKNOWN_PA_ACTION", fmt.Sprintf("unsupported PA action %q", act.Action))
				return
			}
		}
	}

	// Always attach fresh read-only network posture & delta awareness
	s.mu.RLock()
	machineMeta := s.machines.Machines
	s.mu.RUnlock()
	brief, err := s.store.BuildPABrief(ctx, 24, "", machineMeta)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PA_BRIEF", err.Error())
		return
	}

	if len(replyParts) == 0 {
		replyParts = append(replyParts, brief.Summary)
	}
	if len(snap.Events) > 0 {
		header := fmt.Sprintf("增量同步 #%d..#%d（%d 条确定事件）：", snap.FromSeq+1, snap.HighSeq, len(snap.Events))
		if foldedRange != "" {
			header += " " + foldedRange
		}
		replyParts = append(replyParts, header+"\n"+deltaText)
	} else {
		replyParts = append(replyParts, fmt.Sprintf("自上次水位 #%d 以来无新增关键事件。", snap.HighSeq))
	}
	if brief.OpenInboxCount > 0 {
		var lines []string
		for i, item := range brief.OpenInboxItems {
			if i >= 5 {
				break
			}
			lines = append(lines, fmt.Sprintf("- [%s] %s：%s", item.Category, item.Title, item.Summary))
		}
		replyParts = append(replyParts, "当前待拍板：\n"+strings.Join(lines, "\n"))
	}

	assistantText := strings.Join(replyParts, "\n\n")
	userText := req.Message
	if userText == "" && req.Action != nil {
		userText = fmt.Sprintf("[action:%s]", req.Action.Action)
	}

	// Step 4: Commit turn and advance cursor STRICTLY to locked H (snap.HighSeq)
	commitRes, err := s.store.CommitPATurn(ctx, store.CommitPATurnParams{
		PAID:          "default",
		ThreadID:      req.ThreadID,
		UserText:      userText,
		AssistantText: assistantText,
		DeltaFromSeq:  snap.FromSeq,
		DeltaToSeq:    snap.HighSeq,
		Actions:       actions,
	})
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_PA_THREAD_NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PA_COMMIT", err.Error())
		return
	}

	if len(commitRes.Events) > 0 {
		s.broker.Broadcast(commitRes.Events...)
	}

	writeJSON(w, http.StatusOK, protocol.PAChatResponse{
		Thread:           commitRes.Thread,
		UserMessage:      commitRes.UserMessage,
		AssistantMessage: commitRes.AssistantMessage,
		DeltaFromSeq:     snap.FromSeq,
		DeltaToSeq:       snap.HighSeq,
		DeltaEventCount:  len(snap.Events),
		FoldedRange:      foldedRange,
		Actions:          actions,
		PAState:          commitRes.PAState,
	})
}

func summarizePADelta(events []protocol.EventRecord, maxChars int) (string, string) {
	if len(events) == 0 {
		return "", ""
	}
	type scoredEvent struct {
		ev       protocol.EventRecord
		priority int
		line     string
	}
	scored := make([]scoredEvent, len(events))
	for i, ev := range events {
		prio := 4
		switch ev.Kind {
		case protocol.EventApprovalRequested, protocol.EventApprovalExpired:
			prio = 1
		case protocol.EventResourceWaiting, protocol.EventDeployFailed, protocol.EventAttemptFinished:
			prio = 2
		case protocol.EventAnnotationCreated, protocol.EventApprovalDecided, protocol.EventAnnotationAddressed:
			prio = 3
		case protocol.EventDeliveryPublished, protocol.EventDeployVerified:
			prio = 4
		default:
			prio = 5
		}
		line := fmt.Sprintf("#%d [%s/%s] project=%s task=%s", ev.Seq, ev.Kind, ev.Visibility, ev.ProjectID, ev.TaskID)
		scored[i] = scoredEvent{ev: ev, priority: prio, line: line}
	}

	// Keep highest-priority recent events within maxChars budget
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].priority != scored[j].priority {
			return scored[i].priority < scored[j].priority
		}
		return scored[i].ev.Seq > scored[j].ev.Seq
	})

	kept := make([]scoredEvent, 0, len(scored))
	used := 0
	var foldedMin, foldedMax int64
	for _, se := range scored {
		need := len(se.line) + 1
		if used+need > maxChars {
			if foldedMin == 0 || se.ev.Seq < foldedMin {
				foldedMin = se.ev.Seq
			}
			if se.ev.Seq > foldedMax {
				foldedMax = se.ev.Seq
			}
			continue
		}
		used += need
		kept = append(kept, se)
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].ev.Seq < kept[j].ev.Seq })

	lines := make([]string, len(kept))
	for i, k := range kept {
		lines[i] = k.line
	}
	folded := ""
	if foldedMin > 0 {
		folded = fmt.Sprintf("[folded seq %d..%d]", foldedMin, foldedMax)
	}
	return strings.Join(lines, "\n"), folded
}
