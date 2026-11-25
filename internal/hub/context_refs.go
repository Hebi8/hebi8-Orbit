package hub

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

const (
	maxReferencedTasks     = 8
	maxTotalRefContextRune = 24000
	maxPerTaskRefRune      = 6000
)

func (s *Server) resolveReferencedTaskMetas(ctx context.Context, taskIDs []string, excludeTaskID string) []protocol.ReferencedTaskMeta {
	if len(taskIDs) == 0 {
		return nil
	}
	seen := map[string]bool{}
	if excludeTaskID != "" {
		seen[excludeTaskID] = true
	}
	projNameCache := map[string]string{
		protocol.ScratchProjectID: "Chat",
		protocol.PAProjectID:      "PA",
	}
	var out []protocol.ReferencedTaskMeta
	for _, raw := range taskIDs {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		task, err := s.store.GetTask(ctx, id)
		if err != nil {
			continue
		}
		projName, ok := projNameCache[task.ProjectID]
		if !ok {
			if proj, pErr := s.store.GetProject(ctx, task.ProjectID); pErr == nil && strings.TrimSpace(proj.DisplayName) != "" {
				projName = strings.TrimSpace(proj.DisplayName)
			} else {
				projName = task.ProjectID
			}
			projNameCache[task.ProjectID] = projName
		}
		out = append(out, protocol.ReferencedTaskMeta{
			TaskID:      task.TaskID,
			Title:       task.Title,
			ProjectID:   task.ProjectID,
			ProjectName: projName,
		})
		if len(out) >= maxReferencedTasks {
			break
		}
	}
	return out
}

func (s *Server) decorateAttemptPrompt(
	ctx context.Context,
	projectID string,
	rawPrompt string,
	refs []protocol.ReferencedTaskMeta,
	includeWorkspace bool,
) string {
	var parts []string
	if includeWorkspace && projectID == protocol.PAProjectID {
		if ws := s.buildPAWorkspaceSnapshot(ctx); ws != "" {
			parts = append(parts, ws)
		}
	}
	if len(refs) > 0 {
		if refBlock := s.buildReferencedConversationsBlock(ctx, refs); refBlock != "" {
			parts = append(parts, refBlock)
		}
	}
	if len(parts) == 0 {
		return rawPrompt
	}
	parts = append(parts, rawPrompt)
	return strings.Join(parts, "\n\n")
}

func (s *Server) buildPAWorkspaceSnapshot(ctx context.Context) string {
	s.mu.RLock()
	yamlMach := s.yamlMachines.Machines
	s.mu.RUnlock()
	boot, err := s.store.Bootstrap(ctx, yamlMach)
	if err != nil {
		return ""
	}
	recent, _ := s.store.ListRecentTasks(ctx, 20)

	var b strings.Builder
	b.WriteString("<orbit_workspace>\n")
	b.WriteString(fmt.Sprintf("Workspace: %s\n", s.effectiveWorkspaceName()))

	var realProjects []protocol.ProjectView
	for _, p := range boot.Projects {
		if !protocol.IsVirtualProjectID(p.ProjectID) && !p.Archived {
			realProjects = append(realProjects, p)
		}
	}
	if len(realProjects) > 0 {
		b.WriteString("Projects:\n")
		for _, p := range realProjects {
			b.WriteString(fmt.Sprintf("- %s (id=%s, active=%d)\n", p.DisplayName, p.ProjectID, p.ActiveTaskCount))
		}
	}
	if len(boot.OpenInboxItems) > 0 {
		b.WriteString(fmt.Sprintf("Open inbox items: %d\n", len(boot.OpenInboxItems)))
		for i, item := range boot.OpenInboxItems {
			if i >= 6 {
				break
			}
			b.WriteString(fmt.Sprintf("- [%s] %s\n", item.Category, item.Title))
		}
	}
	projNames := map[string]string{
		protocol.ScratchProjectID: "Chat",
		protocol.PAProjectID:      "PA",
	}
	for _, p := range boot.Projects {
		projNames[p.ProjectID] = p.DisplayName
	}
	var nonPA []protocol.TaskSummary
	for _, t := range recent {
		if t.ProjectID == protocol.PAProjectID {
			continue
		}
		nonPA = append(nonPA, t)
		if len(nonPA) >= 12 {
			break
		}
	}
	if len(nonPA) > 0 {
		b.WriteString("Recent conversations:\n")
		for _, t := range nonPA {
			pLabel := projNames[t.ProjectID]
			if pLabel == "" {
				pLabel = t.ProjectID
			}
			b.WriteString(fmt.Sprintf("- [%s] %s (taskId=%s, state=%s)\n", pLabel, t.Title, t.TaskID, t.State))
		}
	}
	b.WriteString("</orbit_workspace>")
	return b.String()
}

func (s *Server) buildReferencedConversationsBlock(ctx context.Context, refs []protocol.ReferencedTaskMeta) string {
	if len(refs) == 0 {
		return ""
	}
	perBudget := maxTotalRefContextRune / len(refs)
	if perBudget > maxPerTaskRefRune {
		perBudget = maxPerTaskRefRune
	}
	if perBudget < 1200 {
		perBudget = 1200
	}

	var b strings.Builder
	b.WriteString("<referenced_conversations>\n")
	b.WriteString("The user explicitly referenced the following Orbit conversations. Use their real contents below to answer accurately.\n")

	totalRunes := 0
	for _, ref := range refs {
		if totalRunes >= maxTotalRefContextRune {
			break
		}
		section := s.renderOneReferencedConversation(ctx, ref, perBudget)
		if section == "" {
			continue
		}
		b.WriteString("\n")
		b.WriteString(section)
		b.WriteString("\n")
		totalRunes += len([]rune(section))
	}
	b.WriteString("</referenced_conversations>")
	return b.String()
}

func (s *Server) renderOneReferencedConversation(ctx context.Context, ref protocol.ReferencedTaskMeta, maxRunes int) string {
	tl, err := s.store.GetTaskTimeline(ctx, ref.TaskID, 0, false)
	if err != nil {
		return ""
	}

	dlvByID := map[string]protocol.DeliveryView{}
	for _, d := range tl.Deliveries {
		dlvByID[d.DeliveryID] = d
	}

	var header strings.Builder
	projLabel := ref.ProjectName
	if projLabel == "" {
		projLabel = ref.ProjectID
	}
	header.WriteString(fmt.Sprintf(`<conversation taskId="%s" project="%s" title="%s" state="%s">`+"\n",
		tl.Task.TaskID, projLabel, strings.ReplaceAll(tl.Task.Title, `"`, `'`), tl.Task.State))

	// Load assistant message batches filtered by kind so thousands of non-message raw events
	// (tool calls, thought chunks, usage frames) can NEVER crowd out assistant replies.
	const (
		maxRefRecentAttempts = 12
		rawPageSize          = 500
		maxMsgEventsPerAtt   = 5000
	)
	startIdx := 0
	if len(tl.Attempts) > maxRefRecentAttempts {
		startIdx = len(tl.Attempts) - maxRefRecentAttempts
	}

	var turnBlocks []string
	for _, att := range tl.Attempts[startIdx:] {
		var msgSB strings.Builder
		var tools []string
		rawTruncated := false
		var afterSeq int64
		fetched := 0
		for fetched < maxMsgEventsPerAtt {
			page, pErr := s.store.ListAttemptRawEventsByKinds(ctx, att.AttemptID, []protocol.EventKind{protocol.EventACPMessageBatch}, afterSeq, rawPageSize)
			if pErr != nil || len(page) == 0 {
				break
			}
			for _, ev := range page {
				if ev.Seq > afterSeq {
					afterSeq = ev.Seq
				}
				if txt, ok := ev.Payload["text"].(string); ok && txt != "" {
					msgSB.WriteString(txt)
				}
			}
			fetched += len(page)
			if len(page) < rawPageSize {
				break
			}
			if fetched >= maxMsgEventsPerAtt {
				if nextPage, nErr := s.store.ListAttemptRawEventsByKinds(ctx, att.AttemptID, []protocol.EventKind{protocol.EventACPMessageBatch}, afterSeq, 1); nErr == nil && len(nextPage) > 0 {
					rawTruncated = true
				}
			}
		}

		if toolEvents, tErr := s.store.ListAttemptRawEventsByKinds(ctx, att.AttemptID, []protocol.EventKind{protocol.EventACPToolCall}, 0, 50); tErr == nil {
			for _, ev := range toolEvents {
				name, _ := ev.Payload["title"].(string)
				if name == "" {
					name, _ = ev.Payload["name"].(string)
				}
				name = strings.TrimSpace(name)
				if name != "" && !slices.Contains(tools, name) && len(tools) < 12 {
					tools = append(tools, name)
				}
			}
		}

		var tb strings.Builder
		userPrompt := strings.TrimSpace(att.PromptText)
		if userPrompt == "" && att.AttemptNo == 1 {
			userPrompt = strings.TrimSpace(tl.Task.Goal)
		}
		if userPrompt != "" {
			tb.WriteString(fmt.Sprintf("Turn %d User: %s\n", att.AttemptNo, trimRunesMiddle(userPrompt, 800)))
		}
		if len(tools) > 0 {
			tb.WriteString(fmt.Sprintf("Turn %d Tools: %s\n", att.AttemptNo, strings.Join(tools, ", ")))
		}
		if assistantText := strings.TrimSpace(msgSB.String()); assistantText != "" {
			rendered := trimRunesMiddle(assistantText, 2200)
			if rawTruncated {
				rendered += "\n[该轮事件过多已截断，回复未完整载入]"
			}
			tb.WriteString(fmt.Sprintf("Turn %d Assistant:\n%s\n", att.AttemptNo, rendered))
		} else if rawTruncated {
			tb.WriteString(fmt.Sprintf("Turn %d Assistant:\n[该轮事件过多已截断，回复未完整载入]\n", att.AttemptNo))
		} else if att.ErrorDetail != "" {
			tb.WriteString(fmt.Sprintf("Turn %d Status: %s (%s)\n", att.AttemptNo, att.State, att.ErrorDetail))
		}
		if att.OutputDeliveryID != "" {
			if dlv, ok := dlvByID[att.OutputDeliveryID]; ok {
				tb.WriteString(fmt.Sprintf("Turn %d Delivery: %s (%d files, +%d/-%d)\n",
					att.AttemptNo, dlv.Summary, dlv.Manifest.FilesChanged, dlv.Manifest.Insertions, dlv.Manifest.Deletions))
			}
		} else if att.FolderChanges != nil && len(att.FolderChanges.Files) > 0 {
			var paths []string
			for i, f := range att.FolderChanges.Files {
				if i >= 12 {
					paths = append(paths, fmt.Sprintf("...+%d more", len(att.FolderChanges.Files)-12))
					break
				}
				paths = append(paths, f.Kind+":"+f.Path)
			}
			tb.WriteString(fmt.Sprintf("Turn %d Folder Changes: %s\n", att.AttemptNo, strings.Join(paths, ", ")))
		}
		if tb.Len() > 0 {
			turnBlocks = append(turnBlocks, tb.String())
		}
	}

	if len(turnBlocks) == 0 && strings.TrimSpace(tl.Task.Goal) != "" {
		turnBlocks = append(turnBlocks, "Goal: "+trimRunesMiddle(tl.Task.Goal, 1000)+"\n")
	}

	// Keep the most recent complete turn pairs within maxRunes budget
	var selected []string
	used := len([]rune(header.String())) + 40
	for i := len(turnBlocks) - 1; i >= 0; i-- {
		rLen := len([]rune(turnBlocks[i]))
		if len(selected) > 0 && used+rLen > maxRunes {
			break
		}
		selected = append(selected, turnBlocks[i])
		used += rLen
	}
	slices.Reverse(selected)

	omittedCount := startIdx + (len(turnBlocks) - len(selected))
	var out strings.Builder
	out.WriteString(header.String())
	if omittedCount > 0 {
		out.WriteString(fmt.Sprintf("(... %d earlier turns omitted ...)\n", omittedCount))
	}
	for _, s := range selected {
		out.WriteString(s)
	}
	out.WriteString("</conversation>")
	return trimRunesMiddle(out.String(), maxRunes)
}

func trimRunesMiddle(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit || limit < 40 {
		return s
	}
	head := limit * 2 / 3
	tail := limit - head - 15
	if tail < 10 {
		return string(runes[:limit-1]) + "…"
	}
	return string(runes[:head]) + "\n...[omitted]...\n" + string(runes[len(runes)-tail:])
}
