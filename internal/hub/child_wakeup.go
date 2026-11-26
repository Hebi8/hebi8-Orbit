package hub

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

func isChildUpdateTriggerEvent(kind protocol.EventKind) bool {
	switch kind {
	case protocol.EventAttemptStarted,
		protocol.EventAttemptFinished,
		protocol.EventAttemptStateChanged,
		protocol.EventApprovalRequested,
		protocol.EventApprovalDecided,
		protocol.EventDeliveryPublished,
		protocol.EventTaskCancelled:
		return true
	default:
		return false
	}
}

func isWakeableChildTerminalState(st protocol.AttemptState) bool {
	switch st {
	case protocol.AttemptSucceeded,
		protocol.AttemptFailed,
		protocol.AttemptCancelled,
		protocol.AttemptNeedsVerification,
		protocol.AttemptExpired:
		return true
	default:
		return false
	}
}

func extractEventAttemptState(payload map[string]any) protocol.AttemptState {
	if payload == nil {
		return ""
	}
	switch v := payload["state"].(type) {
	case protocol.AttemptState:
		return v
	case string:
		return protocol.AttemptState(v)
	default:
		return ""
	}
}

// notifyChildTaskProgressAsync inspects committed events for any task and, if a task is a
// delegated child session, asynchronously emits task.child_updated to its parent stream and
// drains the durable child_parent_notifications table in FIFO order.
// Runs in a detached background goroutine tracked by bgWG so it never delays outbox acks or HTTP replies.
func (s *Server) notifyChildTaskProgressAsync(events []protocol.EventRecord) {
	if len(events) == 0 {
		return
	}
	updateTargets := map[string]string{} // childTaskID -> latest attemptID
	triggerDrain := false

	for _, ev := range events {
		if ev.TaskID == "" || !isChildUpdateTriggerEvent(ev.Kind) {
			continue
		}
		if late, ok := ev.Payload["late_arrival"].(bool); ok && late {
			continue
		}
		updateTargets[ev.TaskID] = ev.AttemptID

		if ev.Kind == protocol.EventAttemptFinished && ev.AttemptID != "" {
			if isWakeableChildTerminalState(extractEventAttemptState(ev.Payload)) {
				triggerDrain = true
			}
		} else if ev.Kind == protocol.EventTaskCancelled && ev.AttemptID != "" {
			triggerDrain = true
		}
	}

	if len(updateTargets) == 0 && !triggerDrain {
		return
	}

	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		for childTaskID, attID := range updateTargets {
			if ev, err := s.store.EmitTaskChildUpdated(ctx, childTaskID, attID); err != nil {
				log.Printf("hub: emit task.child_updated for %s failed: %v", childTaskID, err)
			} else if ev != nil {
				s.broker.Broadcast(*ev)
			}
		}

		if triggerDrain {
			s.ProcessPendingChildNotifications(ctx)
		}
	}()
}

// triggerChildNotificationDrainAsync schedules a background drain of durable pending child notifications
// (e.g., immediately after a parent task's needs_verification block is cleared via verifiedSite=true).
func (s *Server) triggerChildNotificationDrainAsync() {
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.ProcessPendingChildNotifications(ctx)
	}()
}

// ProcessPendingChildNotifications serially consumes durable pending rows from child_parent_notifications
// in notif_seq ASC order with cursor pagination and per-parent head-of-line isolation.
// Delivery confirmation happens atomically inside the parent's SubmitFollowupAtomic transaction;
// transient errors or parent needs_verification record exponential backoff on that parent's head row
// without starving notifications destined for other ready parent tasks.
func (s *Server) ProcessPendingChildNotifications(ctx context.Context) {
	s.childNotifMu.Lock()
	defer s.childNotifMu.Unlock()

	const pageSize = 64
	nowMs := time.Now().UnixMilli()
	var afterSeq int64
	blockedParents := map[string]bool{}

	for {
		if ctx.Err() != nil {
			return
		}
		pending, err := s.store.ListPendingChildParentNotifications(ctx, nowMs, afterSeq, pageSize)
		if err != nil {
			log.Printf("hub: list pending child_parent_notifications failed: %v", err)
			return
		}
		if len(pending) == 0 {
			return
		}
		for _, item := range pending {
			if item.NotifSeq > afterSeq {
				afterSeq = item.NotifSeq
			}
			if ctx.Err() != nil {
				return
			}
			if blockedParents[item.ParentTaskID] {
				continue
			}
			if !s.deliverOneChildNotification(ctx, item, nowMs) {
				blockedParents[item.ParentTaskID] = true
			}
		}
		if len(pending) < pageSize {
			return
		}
	}
}

// deliverOneChildNotification attempts to deliver a single notification.
// Returns true if the notification reached a terminal state (delivered or skipped),
// or false if it was deferred for retry with backoff (blocking subsequent rows for the same parent).
func (s *Server) deliverOneChildNotification(ctx context.Context, item store.PendingChildParentNotification, nowMs int64) bool {
	parentTask, err := s.store.GetTask(ctx, item.ParentTaskID)
	if errors.Is(err, store.ErrNotFound) {
		_ = s.store.MarkChildParentNotificationSkipped(ctx, item.ChildAttemptID, "parent task deleted")
		return true
	}
	if err != nil {
		_ = s.store.RecordChildParentNotificationError(ctx, item.ChildAttemptID, err.Error(), nowMs)
		return false
	}
	if parentTask.ArchivedAt != nil || parentTask.State == protocol.TaskStateCancelled {
		reason := "parent task cancelled"
		if parentTask.ArchivedAt != nil {
			reason = "parent task archived"
		}
		_ = s.store.MarkChildParentNotificationSkipped(ctx, item.ChildAttemptID, reason)
		return true
	}

	resCtx, err := s.store.GetAttemptChildResultContext(ctx, item.ChildTaskID, item.ChildAttemptID)
	if errors.Is(err, store.ErrNotFound) {
		_ = s.store.MarkChildParentNotificationSkipped(ctx, item.ChildAttemptID, "child task or attempt deleted")
		return true
	}
	if err != nil {
		_ = s.store.RecordChildParentNotificationError(ctx, item.ChildAttemptID, err.Error(), nowMs)
		return false
	}

	summaryText := strings.TrimSpace(resCtx.Reply)
	if summaryText == "" && resCtx.DeliverySummary != "" {
		summaryText = resCtx.DeliverySummary
	}
	if summaryText == "" && resCtx.ErrorDetail != "" {
		summaryText = resCtx.ErrorDetail
	}

	meta := protocol.ChildResultMeta{
		ChildTaskID:    resCtx.ChildTaskID,
		ChildTitle:     resCtx.ChildTitle,
		ChildRole:      resCtx.ChildRole,
		ChildProjectID: resCtx.ChildProjectID,
		ChildState:     resCtx.AttemptState,
		Summary:        truncateWakeSummary(summaryText, 600),
		ErrorDetail:    resCtx.ErrorDetail,
		DeliveryID:     resCtx.DeliveryID,
		FilesChanged:   resCtx.FilesChanged,
		Insertions:     resCtx.Insertions,
		Deletions:      resCtx.Deletions,
	}
	promptMsg := formatChildResultWakeMessage(resCtx)

	_, _, code, fErr := s.followupTaskWithTrigger(
		ctx,
		item.ParentTaskID,
		protocol.FollowupTaskRequest{
			Message: promptMsg,
			Mode:    protocol.FollowupModeQueue,
		},
		protocol.TriggerChildResult,
		protocol.ActorSystem,
		"child:"+item.ChildTaskID,
		map[string]any{
			"childResult": meta,
		},
		&store.ChildNotificationDelivery{
			ChildAttemptID: item.ChildAttemptID,
		},
	)
	if fErr != nil {
		// Parent archived/deleted races handled as skipped; needs_verification/route/transient errors stay pending to retry.
		if code == "ERR_TASK_NOT_FOUND" || code == "ERR_TASK_ARCHIVED" {
			_ = s.store.MarkChildParentNotificationSkipped(ctx, item.ChildAttemptID, fErr.Error())
			return true
		}
		_ = s.store.RecordChildParentNotificationError(ctx, item.ChildAttemptID, fErr.Error(), nowMs)
		log.Printf("hub: deliver child notification %s -> parent %s deferred for retry (%s): %v", item.ChildAttemptID, item.ParentTaskID, code, fErr)
		return false
	}
	return true
}

func formatChildResultWakeMessage(rc store.ChildResultContext) string {
	var b strings.Builder
	rolePart := ""
	if strings.TrimSpace(rc.ChildRole) != "" {
		rolePart = fmt.Sprintf(" | role=%s", strings.TrimSpace(rc.ChildRole))
	}
	fmt.Fprintf(&b, "[Orbit 子会话回传] %s (%s%s)\n", rc.ChildTitle, rc.ChildTaskID, rolePart)
	fmt.Fprintf(&b, "- 状态: %s\n", rc.AttemptState)
	if rc.DeliveryID != "" || rc.FilesChanged > 0 {
		if rc.DeliveryID != "" {
			fmt.Fprintf(&b, "- 交付: %s (%d files, +%d -%d)\n", rc.DeliveryID, rc.FilesChanged, rc.Insertions, rc.Deletions)
		} else {
			fmt.Fprintf(&b, "- 变更: %d files (+%d -%d)\n", rc.FilesChanged, rc.Insertions, rc.Deletions)
		}
	}
	if rc.ErrorDetail != "" {
		fmt.Fprintf(&b, "- 错误: %s\n", rc.ErrorDetail)
	}
	reply := strings.TrimSpace(rc.Reply)
	if reply == "" {
		reply = strings.TrimSpace(rc.DeliverySummary)
	}
	if reply != "" {
		fmt.Fprintf(&b, "\n结论摘要:\n%s\n", reply)
	}
	fmt.Fprintf(&b, "\n如需查看完整上下文或继续指令，可调用 `orbit_read_child`（child_task_id=%s）或 `orbit_send_child`。", rc.ChildTaskID)
	return b.String()
}

func truncateWakeSummary(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}
