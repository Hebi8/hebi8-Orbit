import { Fragment, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { ArchiveRestore } from 'lucide-react';
import type {
  BoardMemberCursor,
  BootstrapResponse,
  ChildTaskView,
  EventRecord,
  PendingInput,
  TaskSummary,
  TaskTimelineResponse,
} from '../generated/protocol';
import { MdLinkProvider } from '../components/Md';
import { useMdLinkRecommendedSshHost } from '../hooks/queries';
import { api, errMsg } from '../lib/api';
import { indexInboxByTask, taskTitle } from '../lib/format';
import { resolveSessionFileContext } from '../lib/ide';
import { navigate, paths } from '../lib/router';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';
import { Composer } from './Composer';
import { Minimap } from './Minimap';
import { QueueBubbles } from './QueueBubbles';
import { Turn } from './Turn';
import { WorkerStream } from './WorkerStream';

interface FoldDividerInfo {
  eventId: string;
  segmentId: string;
  title: string;
  turnCount: number;
  endAttemptId: string;
  endAttemptNo: number;
  foldedId: string;
  foldedCount: number;
}

function parseFoldDivider(ev: EventRecord): FoldDividerInfo | null {
  if (ev.kind !== 'hebi8.folded') return null;
  const p = ev.payload ?? {};
  const segmentId = typeof p.segmentId === 'string' ? p.segmentId : '';
  if (!segmentId) return null;
  return {
    eventId: ev.eventId,
    segmentId,
    title: typeof p.title === 'string' ? p.title : '',
    turnCount: typeof p.turnCount === 'number' ? p.turnCount : 0,
    endAttemptId: (typeof p.endAttemptId === 'string' && p.endAttemptId) || ev.attemptId || '',
    endAttemptNo: typeof p.endAttemptNo === 'number' ? p.endAttemptNo : 0,
    foldedId: typeof p.foldedId === 'string' ? p.foldedId : '',
    foldedCount: typeof p.foldedCount === 'number' ? p.foldedCount : 0,
  };
}

function FoldDividerBar({
  info,
  onOpenFoldNode,
}: {
  info: FoldDividerInfo;
  onOpenFoldNode?: (nodeId: string) => void;
}) {
  return (
    <div className="h8-fold-divider" role="separator" aria-label={`已折叠换代 ${info.segmentId}`}>
      <span className="h8-fold-line" aria-hidden="true" />
      <div className="h8-fold-pill">
        <button
          type="button"
          className="h8-fold-btn"
          title={info.title ? `${info.segmentId} · ${info.title}` : `查看 ${info.segmentId} 摘要`}
          onClick={() => onOpenFoldNode?.(info.segmentId)}
        >
          已折叠换代 · <span className="mono">{info.segmentId}</span>
          {info.turnCount > 0 ? ` · ${info.turnCount} 轮` : ''} · 点击查看摘要
        </button>
        {info.foldedId && (
          <button
            type="button"
            className="h8-fold-btn h8-fold-hi"
            title={`查看高层折叠节点 ${info.foldedId}`}
            onClick={() => onOpenFoldNode?.(info.foldedId)}
          >
            · 已合并 {info.foldedCount || 5} 段 → <span className="mono">{info.foldedId}</span>
          </button>
        )}
      </div>
      <span className="h8-fold-line" aria-hidden="true" />
    </div>
  );
}

/**
 * Reusable live conversation stream + composer shared between the full SessionPage,
 * the split ChildSessionDrawer, the infinite Hebi8 conversation, and the split ReviewOverlay.
 */
export function SessionConversation({
  taskId,
  tl,
  boot,
  worker,
  drilledMember,
  compact = false,
  inDrawer = false,
  childrenList,
  nativeMembers,
  siblingChildren,
  extraTasks,
  activeChildId,
  activeNativeMember,
  emptySlot,
  composerTopSlot,
  composerDraftInsert,
  onOpenReview,
  onOpenFoldNode,
  onOpenChild,
  onOpenNativeMember,
}: {
  taskId: string;
  tl: TaskTimelineResponse;
  boot?: BootstrapResponse;
  worker?: string;
  drilledMember?: BoardMemberCursor;
  compact?: boolean;
  inDrawer?: boolean;
  childrenList?: ChildTaskView[];
  nativeMembers?: BoardMemberCursor[];
  siblingChildren?: ChildTaskView[];
  extraTasks?: TaskSummary[];
  activeChildId?: string;
  activeNativeMember?: string;
  emptySlot?: ReactNode;
  composerTopSlot?: ReactNode;
  composerDraftInsert?: { id: number; text: string } | null;
  onOpenReview?: () => void;
  onOpenFoldNode?: (nodeId: string) => void;
  onOpenChild?: (childTaskId: string) => void;
  onOpenNativeMember?: (member: BoardMemberCursor) => void;
}) {
  const qc = useQueryClient();
  const remember = useWorkbenchStore((s) => s.rememberPrompt);
  const [editing, setEditing] = useState<PendingInput | null>(null);
  const stageRef = useRef<HTMLDivElement | null>(null);
  const tlRef = useRef<HTMLDivElement | null>(null);
  const stick = useRef(true);

  const restoreMut = useMutation({
    mutationFn: () => api.unarchiveTask(taskId),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['task-timeline', taskId] });
      void qc.invalidateQueries({ queryKey: ['archived-tasks'] });
      void qc.invalidateQueries({ queryKey: ['all-tasks'] });
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
      void qc.invalidateQueries({ queryKey: ['global-artifacts'] });
      if (tl.task.projectId) {
        void qc.invalidateQueries({ queryKey: ['project-tasks', tl.task.projectId] });
      }
      toast('会话已恢复，现在可以继续对话');
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  useEffect(() => {
    setEditing(null);
    stick.current = true;
  }, [taskId]);

  useEffect(() => {
    for (const pi of tl.pendingInputs) remember(`inp:${pi.inputId}`, pi.body);
  }, [tl.pendingInputs, remember]);

  useLayoutEffect(() => {
    const stage = stageRef.current;
    const inner = tlRef.current;
    if (!stage || !inner) return;
    const ro = new ResizeObserver(() => {
      if (stick.current) stage.scrollTop = stage.scrollHeight;
    });
    ro.observe(inner);
    return () => ro.disconnect();
  }, [taskId, worker]);

  const onScroll = () => {
    const el = stageRef.current;
    if (!el) return;
    stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 140;
  };

  const task = tl.task;
  const members = tl.boardMembers ?? [];
  const lastAttempt = tl.attempts[tl.attempts.length - 1];
  const project = boot?.projects.find((p) => p.projectId === task.projectId);
  const anchorMachineId = project
    ? project.anchors[task.requiredCapability || project.defaultCapability]?.machine
    : undefined;
  const composerMachineId =
    lastAttempt?.machineId ||
    (anchorMachineId && boot?.machines?.some((m) => m.machineId === anchorMachineId) ? anchorMachineId : undefined) ||
    (boot?.setup?.activeDutyMachineId && boot?.machines?.some((m) => m.machineId === boot.setup?.activeDutyMachineId)
      ? boot.setup.activeDutyMachineId
      : undefined);

  const sessionFileCtx = useMemo(() => {
    const resolved = resolveSessionFileContext({
      task,
      attempt: lastAttempt,
      deliveries: tl.deliveries,
      project,
      machines: boot?.machines,
      runtimeConfig: tl.runtimeConfig,
      activeDutyMachineId: boot?.setup?.activeDutyMachineId,
    });
    const effectiveMachineId = (worker && drilledMember?.machineId) || resolved.machineId;
    return {
      machineId: effectiveMachineId,
      worktreePath: worker && drilledMember?.machineId && drilledMember.machineId !== resolved.machineId ? undefined : resolved.worktreePath,
      ideScheme: resolved.ideScheme,
    };
  }, [
    task,
    lastAttempt,
    tl.deliveries,
    tl.runtimeConfig,
    project,
    boot?.machines,
    boot?.setup?.activeDutyMachineId,
    worker,
    drilledMember?.machineId,
  ]);
  const recommendedSshHost = useMdLinkRecommendedSshHost(
    sessionFileCtx.machineId,
    boot?.machines,
  );
  const needsVerifyAttemptId = !task.activeAttemptId && lastAttempt?.state === 'needs_verification' ? lastAttempt.attemptId : undefined;
  const pendingInputs = [...tl.pendingInputs].sort((a, b) => a.position - b.position || a.createdAt - b.createdAt);
  const openAnnotations = useMemo(() => tl.annotations.filter((a) => a.status === 'open'), [tl.annotations]);
  const inboxByTask = useMemo(() => indexInboxByTask(boot?.openInboxItems ?? []), [boot?.openInboxItems]);

  const mergedAllTasks = useMemo(() => {
    const map = new Map<string, TaskSummary>();
    for (const t of extraTasks ?? []) map.set(t.taskId, t);
    for (const t of boot?.activeTasks ?? []) map.set(t.taskId, t);
    return [...map.values()];
  }, [extraTasks, boot?.activeTasks]);

  const effectiveSiblings = siblingChildren ?? tl.siblingChildren ?? [];
  const resolveTaskTitle = useCallback(
    (id: string) => {
      const sib = effectiveSiblings.find((c) => c.taskId === id);
      if (sib) return taskTitle(sib);
      if (tl.parentSummary?.taskId === id) return tl.parentSummary.title;
      const ch = (childrenList ?? tl.children ?? []).find((c) => c.taskId === id);
      if (ch) return taskTitle(ch);
      const act = boot?.activeTasks?.find((t) => t.taskId === id);
      if (act) return taskTitle(act);
      return id;
    },
    [effectiveSiblings, tl.parentSummary, tl.children, childrenList, boot?.activeTasks],
  );

  const handleOpenDelegatedTask = useCallback(
    (targetId: string) => {
      if (inDrawer && onOpenChild && effectiveSiblings.some((c) => c.taskId === targetId)) {
        onOpenChild(targetId);
        return;
      }
      navigate(paths.session(targetId));
    },
    [inDrawer, onOpenChild, effectiveSiblings],
  );

  const { foldDividersByAttemptId, orphanFoldDividers } = useMemo(() => {
    const byId = new Map<string, FoldDividerInfo[]>();
    const attemptNoToId = new Map<number, string>();
    for (const a of tl.attempts) {
      attemptNoToId.set(a.attemptNo, a.attemptId);
    }
    const orphans: FoldDividerInfo[] = [];
    for (const ev of tl.events) {
      const info = parseFoldDivider(ev);
      if (!info) continue;
      let targetId = info.endAttemptId;
      if ((!targetId || !tl.attempts.some((a) => a.attemptId === targetId)) && info.endAttemptNo > 0) {
        targetId = attemptNoToId.get(info.endAttemptNo) ?? '';
      }
      if (targetId) {
        const list = byId.get(targetId) ?? [];
        list.push(info);
        byId.set(targetId, list);
      } else {
        orphans.push(info);
      }
    }
    return { foldDividersByAttemptId: byId, orphanFoldDividers: orphans };
  }, [tl.events, tl.attempts]);

  return (
    <MdLinkProvider
      machineId={sessionFileCtx.machineId}
      worktreePath={sessionFileCtx.worktreePath}
      machines={boot?.machines}
      ideScheme={sessionFileCtx.ideScheme}
      recommendedSshHost={recommendedSshHost}
    >
    <div className={`sess-body ${compact ? 'compact' : ''} ${inDrawer ? 'in-drawer' : ''}`}>
      <div className="stage" ref={stageRef} onScroll={onScroll}>
        {worker ? (
          <div ref={tlRef}>
            <WorkerStream taskId={taskId} worker={worker} member={drilledMember} />
          </div>
        ) : (
          <>
            {!compact && !inDrawer && tl.attempts.length > 0 && <Minimap tl={tl} stageRef={stageRef} />}
            <div className="tl" ref={tlRef}>
              {tl.attempts.length === 0 && (
                emptySlot ? (
                  emptySlot
                ) : task.goal ? (
                  <>
                    <div className="it u">
                      <div className="ub">{task.goal}</div>
                    </div>
                    <div className="it running">
                      <span className="spin" />
                      <span className="shim">准备中…</span>
                    </div>
                  </>
                ) : null
              )}
              {tl.attempts.map((a, i) => {
                const dividers = foldDividersByAttemptId.get(a.attemptId) ?? [];
                return (
                  <Fragment key={a.attemptId}>
                    <Turn
                      attempt={a}
                      index={i}
                      isLast={i === tl.attempts.length - 1}
                      tl={tl}
                      compact={compact}
                      childrenList={childrenList}
                      nativeMembers={nativeMembers}
                      siblingChildren={effectiveSiblings}
                      allTasks={mergedAllTasks}
                      projects={boot?.projects}
                      machines={boot?.machines}
                      inboxByTask={inboxByTask}
                      activeChildId={activeChildId}
                      activeNativeMember={activeNativeMember}
                      onOpenChild={onOpenChild}
                      onOpenNativeMember={onOpenNativeMember}
                    />
                    {dividers.map((div) => (
                      <FoldDividerBar key={div.eventId} info={div} onOpenFoldNode={onOpenFoldNode} />
                    ))}
                  </Fragment>
                );
              })}
              {orphanFoldDividers.map((div) => (
                <FoldDividerBar key={div.eventId} info={div} onOpenFoldNode={onOpenFoldNode} />
              ))}
              <QueueBubbles
                taskId={taskId}
                items={pendingInputs}
                busy={!!task.activeAttemptId}
                editingId={editing?.inputId ?? null}
                onEdit={setEditing}
                resolveTaskTitle={resolveTaskTitle}
                onOpenTask={handleOpenDelegatedTask}
              />
            </div>
          </>
        )}
      </div>
      {task.archivedAt ? (
        <div className="arch-readonly-bar" role="status" aria-label="已归档会话只读提示">
          <span className="arch-readonly-msg">该会话已归档，恢复后可继续对话</span>
          <button
            type="button"
            className="btn pri sm"
            disabled={restoreMut.isPending}
            onClick={() => restoreMut.mutate()}
          >
            <ArchiveRestore size={13} strokeWidth={1.7} />
            <span>{restoreMut.isPending ? '恢复中…' : '恢复会话'}</span>
          </button>
        </div>
      ) : (
        <Composer
          task={task}
          worker={worker}
          members={members}
          editing={editing}
          needsVerifyAttemptId={needsVerifyAttemptId}
          onEditDone={() => setEditing(null)}
          openAnnotations={openAnnotations}
          runtimeConfig={tl.runtimeConfig}
          contextUsage={tl.contextUsage}
          indicatorMode="context"
          machines={boot?.machines}
          projects={boot?.projects}
          machineId={composerMachineId}
          compact={compact}
          inDrawer={inDrawer}
          topSlot={composerTopSlot}
          draftInsert={composerDraftInsert}
          onOpenReview={onOpenReview}
        />
      )}
    </div>
    </MdLinkProvider>
  );
}
