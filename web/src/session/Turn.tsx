import { useEffect, useMemo, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import {
  ArrowUpRight,
  ChevronRight,
  CornerDownLeft,
  FileSpreadsheet,
  FileText,
  Layers,
  Link2,
  Presentation,
  Table,
} from 'lucide-react';
import type {
  AttemptView,
  BoardMemberCursor,
  ChildTaskView,
  InboxItemView,
  MachineView,
  ProjectView,
  TaskSummary,
  TaskTimelineResponse,
} from '../generated/protocol';
import { ApprovalNotice } from '../components/ApprovalNotice';
import { FolderChangesCard } from '../components/FolderChangesCard';
import { Md } from '../components/Md';
import { CrossDirSessionCards, type CrossDirSessionItem } from '../components/PAViews';
import { StatusMark } from '../components/StatusMark';
import { useAttemptRaw } from '../hooks/queries';
import { ADAPTER_LABELS } from '../lib/agentCaps';
import { api, errMsg } from '../lib/api';
import { deliveryInlineImages, deliveryReusableArtifacts } from '../lib/artifacts';
import {
  PA_PROJECT_ID,
  SCRATCH_PROJECT_ID,
  artifactShortLabel,
  attemptStateLabel,
  basename,
  childSessionMark,
  cleanMachineName,
  hhmm,
  isAttemptLive,
  sessionMark,
  shortSha,
  taskTitle,
  triggerLabel,
  type SessionMark,
} from '../lib/format';
import { navigate, paths } from '../lib/router';
import { buildSegments, lastLiveStep } from '../lib/segments';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';
import { Segments } from './Segments';

const CODE_KINDS = new Set(['code_diff', 'mixed', 'workspace_snapshot']);

function nativeMemberMark(m: BoardMemberCursor): SessionMark {
  switch (m.state) {
    case 'working':
      return 'running';
    case 'waiting':
      return 'needs-you';
    case 'killed':
      return 'failed';
    default:
      return null;
  }
}

function extractOneLineGist(md: string): string {
  const line = md
    .split('\n')
    .map((l) => l.replace(/^[#>*\-\d.\s]+/, '').replace(/[`*_~]/g, '').trim())
    .find((l) => l.length > 0);
  return line ?? '';
}

function stripSystemWakeupBoilerplate(raw: string): string {
  const trimmed = raw.trim();
  if (!trimmed) return '';
  // Remove leading system wakeup metadata headers (e.g. "[Orbit 子会话回传] ...") if present.
  const lines = trimmed.split('\n');
  let start = 0;
  while (start < lines.length) {
    const l = lines[start].trim();
    if (!l || /^\[Orbit[^\]]*\]/i.test(l) || /^(子会话|任务|状态|Delivery|TaskID|ChildTaskID)[:：]/i.test(l)) {
      start++;
      continue;
    }
    break;
  }
  const body = lines.slice(start).join('\n').trim();
  return body || trimmed;
}

function AuthedInlineImage({ sha256, name }: { sha256: string; name: string }) {
  const [src, setSrc] = useState<string>('');
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let active = true;
    let objUrl = '';
    setFailed(false);
    void api
      .fetchBlob(sha256)
      .then((blob) => {
        if (!active) return;
        objUrl = URL.createObjectURL(blob);
        setSrc(objUrl);
      })
      .catch(() => {
        if (active) setFailed(true);
      });
    return () => {
      active = false;
      if (objUrl) URL.revokeObjectURL(objUrl);
    };
  }, [sha256]);

  return (
    <figure className="inline-media-box">
      {src ? (
        <img src={src} alt={name} className="inline-media-img" loading="lazy" />
      ) : (
        <div className="muted" style={{ padding: '12px 14px', fontSize: 12 }}>
          {failed ? '图片加载失败' : '载入图片…'}
        </div>
      )}
      <figcaption className="inline-media-cap">
        <span className="mono">{name}</span>
        <button
          type="button"
          className="lk"
          style={{ background: 'none', border: 0, padding: 0, cursor: 'pointer' }}
          onClick={() => {
            if (src) {
              window.open(src, '_blank', 'noopener,noreferrer');
            } else {
              void api.downloadBlob(sha256, name).catch((e) => toast(errMsg(e), 'err'));
            }
          }}
        >
          原图
        </button>
      </figcaption>
    </figure>
  );
}

function ArtifactGlyph({ kind }: { kind: string }) {
  switch (kind) {
    case 'slides':
      return <Presentation size={14} strokeWidth={1.6} />;
    case 'sheet':
      return <FileSpreadsheet size={14} strokeWidth={1.6} />;
    case 'design':
      return <Layers size={14} strokeWidth={1.6} />;
    case 'word':
    case 'pdf':
      return <Table size={14} strokeWidth={1.6} />;
    default:
      return <FileText size={14} strokeWidth={1.6} />;
  }
}

function usePromptFor(attempt: AttemptView, tl: TaskTimelineResponse): { text: string; placeholder: boolean; detail: string[] } {
  const cache = useWorkbenchStore((s) => s.promptCache);
  return useMemo(() => {
    const task = tl.task;
    const serverPrompt = attempt.promptText?.trim() ?? '';
    switch (attempt.triggerKind) {
      case 'initial':
      case 'workspace_import':
        return { text: serverPrompt || task.goal, placeholder: false, detail: [] };
      case 'annotation_batch': {
        const anns = tl.annotations.filter((a) => a.triggeredAttemptId === attempt.attemptId);
        return {
          text: serverPrompt || `Review 批注 · ${anns.length || '若干'} 条`,
          placeholder: false,
          detail: anns.map((a) => {
            const lineRange = a.anchor.startLine
              ? a.anchor.endLine && a.anchor.endLine !== a.anchor.startLine
                ? `:${a.anchor.startLine}-${a.anchor.endLine}`
                : `:${a.anchor.startLine}`
              : '';
            const loc = a.anchor.path ? `${a.anchor.path}${lineRange}` : a.anchor.artifact || a.source;
            return `${loc} — ${a.body}`;
          }),
        };
      }
      case 'followup': {
        if (serverPrompt) return { text: serverPrompt, placeholder: false, detail: [] };
        const direct = cache[`att:${attempt.attemptId}`];
        if (direct) return { text: direct, placeholder: false, detail: [] };
        const ev = tl.events.find(
          (e) => e.kind === 'queue.updated' && e.payload?.action === 'dispatched' && e.payload?.dispatchedAttemptId === attempt.attemptId,
        );
        const inputId = typeof ev?.payload?.inputId === 'string' ? ev.payload.inputId : '';
        const viaQueue = inputId ? cache[`inp:${inputId}`] : '';
        if (viaQueue) return { text: viaQueue, placeholder: false, detail: [] };
        return { text: '追问', placeholder: true, detail: [] };
      }
      case 'retry':
        return { text: serverPrompt || `重试 · ${task.goal.split('\n')[0]}`, placeholder: !serverPrompt, detail: [] };
      case 'manual_import': {
        const d = tl.deliveries.find((x) => x.deliveryId === attempt.outputDeliveryId);
        return { text: `导入交付${d?.summary ? ` · ${d.summary}` : ''}`, placeholder: true, detail: [] };
      }
      default:
        return { text: triggerLabel(attempt.triggerKind), placeholder: true, detail: [] };
    }
  }, [attempt, tl, cache]);
}

function RetryNotice({ attempt, taskId, needsVerify }: { attempt: AttemptView; taskId: string; needsVerify: boolean }) {
  const qc = useQueryClient();
  const [verified, setVerified] = useState(false);
  const retry = useMutation({
    mutationFn: () =>
      api.retryTask(taskId, {
        verifiedSite: needsVerify ? verified : undefined,
        verifiedAttemptId: needsVerify ? attempt.attemptId : undefined,
      }),
    onSuccess: () => {
      toast('已发起重试');
      void qc.invalidateQueries({ queryKey: ['task-timeline', taskId] });
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });
  return (
    <div className={`it nt ${needsVerify ? '' : 'bad'}`}>
      <div className="nt-h">
        <span className="lbl" title={attempt.errorCode || undefined}>
          {needsVerify ? '上一轮中途断了' : attemptStateLabel(attempt.state)}
        </span>
      </div>
      <div className="nt-b">
        {needsVerify
          ? '工作区可能有没提交的改动，看一眼再继续。'
          : attempt.errorDetail || '这一轮没能完成。'}
      </div>
      <div className="nt-a">
        {needsVerify && (
          <label className="chk">
            <input type="checkbox" checked={verified} onChange={(e) => setVerified(e.target.checked)} />
            看过了
          </label>
        )}
        <button type="button" className="btn pri" disabled={retry.isPending || (needsVerify && !verified)} onClick={() => retry.mutate()}>
          重试
        </button>
      </div>
    </div>
  );
}

/**
 * Unified minimal inbound communication rail (`↵`, 1.5px muted accent left rail):
 * Default collapsed with 1-line summary; expands clean formatted Markdown via `<Md />` (never raw `<pre>`).
 * Drops verbose boilerplate labels ("来自子会话" / "同级会话通信" / "已回传摘要").
 */
function InboundCommRail({
  title,
  bodyMd,
  time,
  isFailed = false,
  insertions = 0,
  deletions = 0,
  targetTaskId,
  openLinkLabel,
  onOpenTarget,
}: {
  title: string;
  bodyMd: string;
  time?: string;
  isFailed?: boolean;
  insertions?: number;
  deletions?: number;
  targetTaskId?: string;
  openLinkLabel?: string;
  onOpenTarget?: (taskId: string) => void;
}) {
  const gist = extractOneLineGist(bodyMd);
  const hasDiff = insertions > 0 || deletions > 0;

  return (
    <details className={`it comm-in ${isFailed ? 'is-failed' : ''}`}>
      <summary className="comm-row is-in">
        <span className="comm-dir-ico is-in" aria-hidden="true">
          <CornerDownLeft size={13} strokeWidth={1.75} />
        </span>
        <span className="comm-ttl" title={title}>
          {title}
        </span>
        <span className="comm-meta">{gist}</span>
        <span className="comm-trail">
          {hasDiff && (
            <span className="sub-ret-diff mono">
              {insertions > 0 && <span className="add">+{insertions}</span>}
              {deletions > 0 && <span className="del">−{deletions}</span>}
            </span>
          )}
          {isFailed && <StatusMark mark="failed" />}
          <span className="comm-chev" aria-hidden="true">
            <ChevronRight size={12} strokeWidth={1.7} />
          </span>
        </span>
      </summary>
      <div className="comm-in-body">
        <Md text={bodyMd || '已完成'} />
        <div className="comm-in-foot">
          <span>{time || ''}</span>
          {targetTaskId && (
            <button
              type="button"
              className="sub-ret-link"
              onClick={() => {
                if (onOpenTarget) onOpenTarget(targetTaskId);
                else navigate(paths.session(targetTaskId));
              }}
            >
              <span>{openLinkLabel || '分屏查看'}</span>
              <ChevronRight size={11.5} strokeWidth={1.8} />
            </button>
          )}
        </div>
      </div>
    </details>
  );
}

/**
 * Unified minimal outbound communication rail (`↗`, 1.5px neutral gray left rail):
 * - Orbit child sessions & native subagent/swarm members (native members carry faint plain text "内置" at end of meta).
 * - Trailing single-slot StatusMark only.
 * - Naturally folds into a single quiet disclosure when every item in the batch is resting.
 */
function OutboundCommTrack({
  parentTaskId,
  parentProjectId,
  children,
  nativeMembers = [],
  machines,
  inboxByTask,
  activeChildId,
  activeNativeMember,
  onOpenChild,
  onOpenNativeMember,
}: {
  parentTaskId: string;
  parentProjectId: string;
  children: ChildTaskView[];
  nativeMembers?: BoardMemberCursor[];
  machines?: MachineView[];
  inboxByTask?: ReadonlyMap<string, InboxItemView[]>;
  activeChildId?: string;
  activeNativeMember?: string;
  onOpenChild?: (childTaskId: string) => void;
  onOpenNativeMember?: (member: BoardMemberCursor) => void;
}) {
  const totalCount = children.length + nativeMembers.length;
  if (totalCount === 0) return null;

  const allResting =
    children.every((c) => !childSessionMark(c, inboxByTask?.get(c.taskId))) &&
    nativeMembers.every((m) => !nativeMemberMark(m));

  const rows = (
    <div className="comm-track is-out">
      {children.map((c) => {
        const mark = childSessionMark(c, inboxByTask?.get(c.taskId));
        const mach = machines?.find((m) => m.machineId === c.machineId);
        const machName = mach ? mach.displayAlias?.trim() || cleanMachineName(mach) : c.machineId || '';
        const adpLabel = c.preferredAdapter ? ADAPTER_LABELS[c.preferredAdapter] ?? c.preferredAdapter : '';
        const isCrossProject = !!c.projectId && c.projectId !== parentProjectId && !!c.projectName;
        const metaParts = [
          isCrossProject ? c.projectName : '',
          machName,
          adpLabel,
          mark && c.currentStep?.trim() ? c.currentStep.trim() : '',
        ].filter(Boolean);
        const title = taskTitle(c);
        const isOpen = !!activeChildId && activeChildId === c.taskId;

        return (
          <button
            type="button"
            key={c.taskId}
            className={`comm-row is-out ${isOpen ? 'is-open' : ''} ${!mark ? 'is-resting' : ''}`}
            title={c.goal || title}
            onClick={() => {
              if (onOpenChild) onOpenChild(c.taskId);
              else navigate(paths.session(parentTaskId, undefined, c.taskId));
            }}
          >
            <span className="comm-dir-ico is-out" aria-hidden="true">
              <ArrowUpRight size={13} strokeWidth={1.75} />
            </span>
            <span className="comm-ttl">{title}</span>
            <span className="comm-meta">{metaParts.join(' · ')}</span>
            <span className="comm-trail">
              <StatusMark mark={mark} />
              <span className="comm-chev" aria-hidden="true">
                <ChevronRight size={12} strokeWidth={1.7} />
              </span>
            </span>
          </button>
        );
      })}

      {nativeMembers.map((m) => {
        const mark = nativeMemberMark(m);
        const mach = machines?.find((x) => x.machineId === m.machineId);
        const machName = mach ? mach.displayAlias?.trim() || cleanMachineName(mach) : m.machineId || '';
        const adpLabel = m.adapter ? ADAPTER_LABELS[m.adapter] ?? m.adapter : '';
        const metaParts = [machName, adpLabel, '内置'].filter(Boolean);
        const isOpen = !activeChildId && activeNativeMember === m.memberName;

        return (
          <button
            type="button"
            key={`native:${m.memberName}`}
            className={`comm-row is-out ${isOpen ? 'is-open' : ''} ${!mark ? 'is-resting' : ''}`}
            title={`@${m.memberName} · ${metaParts.join(' · ')}`}
            onClick={() => {
              if (onOpenNativeMember) onOpenNativeMember(m);
              else navigate(paths.session(parentTaskId, m.memberName));
            }}
          >
            <span className="comm-dir-ico is-out" aria-hidden="true">
              <ArrowUpRight size={13} strokeWidth={1.75} />
            </span>
            <span className="comm-ttl">@{m.memberName}</span>
            <span className="comm-meta">{metaParts.join(' · ')}</span>
            <span className="comm-trail">
              <StatusMark mark={mark} />
              <span className="comm-chev" aria-hidden="true">
                <ChevronRight size={12} strokeWidth={1.7} />
              </span>
            </span>
          </button>
        );
      })}
    </div>
  );

  if (allResting) {
    const previewTitles = [
      ...children.map((c) => taskTitle(c).slice(0, 14)),
      ...nativeMembers.map((m) => `@${m.memberName}`),
    ]
      .slice(0, 3)
      .join('、');

    return (
      <details className="it sub-quiet-details">
        <summary className="proc-summary">
          <ChevronRight size={12} strokeWidth={1.8} className="proc-chev" aria-hidden="true" />
          <span>
            {totalCount} 个子会话{previewTitles ? ` · ${previewTitles}` : ''}
          </span>
        </summary>
        {rows}
      </details>
    );
  }

  return <div className="it">{rows}</div>;
}

export function Turn({
  attempt,
  index,
  isLast,
  tl,
  compact = false,
  childrenList,
  nativeMembers,
  siblingChildren,
  allTasks,
  projects,
  machines,
  inboxByTask,
  activeChildId,
  activeNativeMember,
  onOpenChild,
  onOpenNativeMember,
}: {
  attempt: AttemptView;
  index: number;
  isLast: boolean;
  tl: TaskTimelineResponse;
  compact?: boolean;
  childrenList?: ChildTaskView[];
  nativeMembers?: BoardMemberCursor[];
  siblingChildren?: ChildTaskView[];
  allTasks?: TaskSummary[];
  projects?: ProjectView[];
  machines?: MachineView[];
  inboxByTask?: ReadonlyMap<string, InboxItemView[]>;
  activeChildId?: string;
  activeNativeMember?: string;
  onOpenChild?: (childTaskId: string) => void;
  onOpenNativeMember?: (member: BoardMemberCursor) => void;
}) {
  const live = isAttemptLive(attempt.state);
  // Every turn loads its raw ACP events once so historical turns' final prose is NEVER hidden or folded away.
  const raw = useAttemptRaw(attempt.attemptId, { enabled: true, live });
  const segs = useMemo(() => buildSegments(raw.data ?? []), [raw.data]);
  const prompt = usePromptFor(attempt, tl);
  const openReview = useWorkbenchStore((s) => s.openReview);

  const approvals = tl.approvals
    .filter((a) => a.attemptId === attempt.attemptId)
    .sort((a, b) => a.createdAt - b.createdAt);
  const deliveryIdx = attempt.outputDeliveryId
    ? tl.deliveries.findIndex((d) => d.deliveryId === attempt.outputDeliveryId)
    : -1;
  const delivery = deliveryIdx >= 0 ? tl.deliveries[deliveryIdx] : undefined;
  const taskIdle = !tl.task.activeAttemptId;
  const step = live ? lastLiveStep(segs) : null;
  const hasActiveToolStep = !!step && (step.status === 'in_progress' || step.status === 'pending');

  const inlineImages = delivery ? deliveryInlineImages(delivery) : [];
  const artifacts = delivery ? deliveryReusableArtifacts(delivery, tl.annotations) : [];
  const hasCodeDiff = !!delivery && CODE_KINDS.has(delivery.kind) && delivery.manifest.filesChanged > 0;
  const isChildResult = attempt.triggerKind === 'child_result' || !!attempt.childResult;

  // Detect inbound delegated sibling/parent followup directly from AttemptView projection (with event fallback).
  const delegatedInbound = useMemo(() => {
    if (isChildResult) return null;
    let fromId = attempt.delegatedFromTaskId ?? '';
    let relation = attempt.delegatedRelation ?? '';
    if (!fromId) {
      const matchEv = tl.events.find((e) => {
        if (e.kind === 'attempt.started' && e.attemptId === attempt.attemptId) {
          return typeof e.payload?.delegatedFromTaskId === 'string' && e.payload.delegatedFromTaskId.length > 0;
        }
        if (e.kind === 'queue.updated' && e.payload?.dispatchedAttemptId === attempt.attemptId) {
          return typeof e.payload?.delegatedFromTaskId === 'string' && e.payload.delegatedFromTaskId.length > 0;
        }
        return false;
      });
      fromId = typeof matchEv?.payload?.delegatedFromTaskId === 'string' ? matchEv.payload.delegatedFromTaskId : '';
      relation = typeof matchEv?.payload?.delegatedRelation === 'string' ? matchEv.payload.delegatedRelation : '';
    }
    if (!fromId) return null;
    const sibMatch = (siblingChildren ?? tl.siblingChildren ?? []).find((s) => s.taskId === fromId);
    const parentMatch = tl.parentSummary?.taskId === fromId ? tl.parentSummary : undefined;
    const taskMatch = allTasks?.find((t) => t.taskId === fromId);
    const senderTitle =
      (sibMatch && taskTitle(sibMatch)) ||
      parentMatch?.title ||
      (taskMatch && taskTitle(taskMatch)) ||
      fromId;
    return { fromId, relation, senderTitle };
  }, [
    isChildResult,
    attempt.delegatedFromTaskId,
    attempt.delegatedRelation,
    tl.events,
    tl.siblingChildren,
    tl.parentSummary,
    attempt.attemptId,
    siblingChildren,
    allTasks,
  ]);

  const isPA = tl.task.projectId === PA_PROJECT_ID;
  const isTopLevel = !tl.task.parentTaskId;
  const sentDelegations = useMemo(() => {
    if (isPA) return [];
    const sibPool = siblingChildren ?? tl.siblingChildren ?? [];
    const childPool = childrenList ?? tl.children ?? [];
    return tl.events
      .filter((e) => e.kind === 'delegation.sent' && e.attemptId === attempt.attemptId)
      .map((e) => {
        const p = e.payload ?? {};
        const targetTaskId = typeof p.targetTaskId === 'string' ? p.targetTaskId : '';
        const relation = typeof p.relation === 'string' ? p.relation : '';
        const summary = typeof p.summary === 'string' ? p.summary.trim() : '';
        const targetMatch =
          sibPool.find((x) => x.taskId === targetTaskId) ??
          childPool.find((x) => x.taskId === targetTaskId) ??
          allTasks?.find((x) => x.taskId === targetTaskId);
        const rawTitle = typeof p.targetTitle === 'string' ? p.targetTitle.trim() : '';
        const targetTitle = (targetMatch && taskTitle(targetMatch)) || rawTitle || targetTaskId;
        return {
          key: e.eventId || `ds:${e.seq}`,
          targetTaskId,
          targetTitle,
          relation,
          summary,
          createdAt: e.createdAt,
        };
      })
      .filter((x) => !!x.targetTaskId);
  }, [isPA, tl.events, tl.siblingChildren, tl.children, siblingChildren, childrenList, allTasks, attempt.attemptId]);
  const dispatchedChildren = useMemo(() => {
    if (!isTopLevel) return [];
    const pool = childrenList ?? tl.children ?? [];
    const direct = pool.filter((c) => c.delegatedByAttemptId === attempt.attemptId);
    if (!isLast) return direct;
    // Attach any child without a matching attempt id onto the last turn so none are orphaned.
    const knownAttemptIds = new Set(tl.attempts.map((a) => a.attemptId));
    const orphans = pool.filter((c) => !c.delegatedByAttemptId || !knownAttemptIds.has(c.delegatedByAttemptId));
    const seen = new Set(direct.map((c) => c.taskId));
    return [...direct, ...orphans.filter((c) => !seen.has(c.taskId))];
  }, [isTopLevel, childrenList, tl.children, tl.attempts, attempt.attemptId, isLast]);

  const turnNativeMembers = isTopLevel && isLast && !isPA ? (nativeMembers ?? []) : [];

  // Hebi8 / PA dedicated cross-directory session jump cards (never forced into parent/child ↗/↵ rails).
  const paCrossDirCards = useMemo<CrossDirSessionItem[]>(() => {
    if (!isPA) return [];
    const projectMap = new Map((projects ?? []).map((p) => [p.projectId, p]));
    const machineMap = new Map((machines ?? []).map((m) => [m.machineId, m]));
    const taskMap = new Map<string, TaskSummary>();
    for (const t of allTasks ?? []) {
      if (t.projectId !== PA_PROJECT_ID && t.taskId !== tl.task.taskId) {
        taskMap.set(t.taskId, t);
      }
    }

    const resolveMachineLabel = (mid?: string, proj?: ProjectView): string | undefined => {
      const candidateId =
        mid ||
        proj?.parts?.[0]?.locations?.find((l) => l.isDefault)?.machineId ||
        proj?.parts?.[0]?.locations?.[0]?.machineId ||
        (proj?.defaultCapability ? proj.anchors?.[proj.defaultCapability]?.machine : undefined);
      if (!candidateId) return undefined;
      const m = machineMap.get(candidateId);
      return m ? m.displayAlias?.trim() || cleanMachineName(m) : candidateId;
    };

    const resolveProjectLabel = (pid: string): string => {
      if (pid === SCRATCH_PROJECT_ID) return '自由对话';
      return projectMap.get(pid)?.displayName || pid;
    };

    const cards: CrossDirSessionItem[] = [];
    const seen = new Set<string>();

    // 1. Extract cross-directory sessions referenced in Hebi8's reply prose (links `#/t/<id>` or known task IDs)
    const proseLines = segs
      .filter((s): s is Extract<typeof s, { kind: 'msg' }> => s.kind === 'msg')
      .flatMap((s) => s.text.split('\n'));

    const matchedIds: { taskId: string; line?: string }[] = [];
    const hashLinkRe = /#\/t\/([A-Za-z0-9_-]+)/g;
    for (const rawLine of proseLines) {
      let m: RegExpExecArray | null;
      hashLinkRe.lastIndex = 0;
      while ((m = hashLinkRe.exec(rawLine)) !== null) {
        const tid = m[1];
        if (tid && tid !== tl.task.taskId && !seen.has(tid) && taskMap.has(tid)) {
          seen.add(tid);
          matchedIds.push({ taskId: tid, line: rawLine });
        }
      }
      for (const [tid] of taskMap) {
        if (seen.has(tid)) continue;
        if (rawLine.includes(tid)) {
          seen.add(tid);
          matchedIds.push({ taskId: tid, line: rawLine });
        }
      }
    }

    for (const { taskId: tid, line } of matchedIds) {
      const t = taskMap.get(tid);
      if (!t) continue;
      const proj = projectMap.get(t.projectId);
      const inboxItems = inboxByTask?.get(t.taskId) ?? [];
      const mark = sessionMark(t, inboxItems);
      const cleanedLine = line
        ? line
            .replace(/\[([^\]]+)\]\(#\/t\/[A-Za-z0-9_-]+\)/g, '$1')
            .replace(new RegExp(`\\(?taskId\\s*=\\s*${tid}\\)?`, 'g'), '')
            .replace(tid, '')
            .replace(/^[#>*\-\d.\s]+/, '')
            .replace(/[`*_]/g, '')
            .replace(/\s+/g, ' ')
            .trim()
        : '';
      const hint =
        (inboxItems[0]?.title?.trim()) ||
        t.waitingReason?.trim() ||
        (cleanedLine && cleanedLine !== taskTitle(t) ? cleanedLine : '') ||
        t.goal?.split('\n')[0]?.trim() ||
        undefined;

      cards.push({
        key: `pa-cross:${t.taskId}`,
        taskId: t.taskId,
        projectId: t.projectId,
        title: taskTitle(t),
        projectLabel: resolveProjectLabel(t.projectId),
        machineLabel: resolveMachineLabel(undefined, proj),
        mark,
        hint,
      });
    }

    // 2. Render any historical pre-v4 PA children as cross-directory jump cards (never parent/child rails)
    for (const c of dispatchedChildren) {
      if (seen.has(c.taskId)) continue;
      seen.add(c.taskId);
      const proj = projectMap.get(c.projectId);
      const inboxItems = inboxByTask?.get(c.taskId) ?? [];
      cards.push({
        key: `pa-legacy:${c.taskId}`,
        taskId: c.taskId,
        projectId: c.projectId,
        title: taskTitle(c),
        projectLabel: c.projectName || resolveProjectLabel(c.projectId),
        machineLabel: resolveMachineLabel(c.machineId, proj),
        mark: childSessionMark(c, inboxItems),
        hint: c.currentStep?.trim() || c.goal?.split('\n')[0]?.trim() || undefined,
      });
    }

    return cards;
  }, [isPA, projects, machines, allTasks, tl.task.taskId, segs, inboxByTask, dispatchedChildren]);

  const childResultMd = useMemo(() => {
    if (!isChildResult) return '';
    const cr = attempt.childResult;
    if (cr?.summary?.trim()) return cr.summary.trim();
    if (cr?.errorDetail?.trim()) return cr.errorDetail.trim();
    return stripSystemWakeupBoilerplate(attempt.promptText ?? '');
  }, [isChildResult, attempt.childResult, attempt.promptText]);

  return (
    <div className="turn" id={`turn-${attempt.attemptId}`} data-turn={index}>
      {!isPA && isChildResult ? (
        <InboundCommRail
          title={attempt.childResult?.childTitle || attempt.childResult?.childTaskId || '子会话'}
          bodyMd={childResultMd}
          time={hhmm(attempt.startedAt)}
          isFailed={
            attempt.childResult?.childState === 'failed' ||
            attempt.childResult?.childState === 'expired' ||
            attempt.childResult?.childState === 'cancelled'
          }
          insertions={attempt.childResult?.insertions ?? 0}
          deletions={attempt.childResult?.deletions ?? 0}
          targetTaskId={attempt.childResult?.childTaskId}
          openLinkLabel={onOpenChild ? '分屏查看' : '打开会话'}
          onOpenTarget={onOpenChild}
        />
      ) : !isPA && delegatedInbound ? (
        <InboundCommRail
          title={delegatedInbound.senderTitle}
          bodyMd={stripSystemWakeupBoilerplate(prompt.text)}
          time={hhmm(attempt.startedAt)}
          targetTaskId={delegatedInbound.fromId}
          openLinkLabel={delegatedInbound.relation === 'sibling' && onOpenChild ? '切换查看' : '打开会话'}
          onOpenTarget={delegatedInbound.relation === 'sibling' ? onOpenChild : undefined}
        />
      ) : (
        <div className="it u">
          {attempt.refTasks && attempt.refTasks.length > 0 && (
            <div className="turn-refs" aria-label="引用的对话">
              {attempt.refTasks.map((ref) => (
                <button
                  key={ref.taskId}
                  type="button"
                  className="turn-ref-pill"
                  title={ref.projectName ? `${ref.projectName} · ${ref.title}` : ref.title}
                  onClick={() => navigate(ref.projectId === PA_PROJECT_ID ? paths.pa(ref.taskId) : paths.session(ref.taskId))}
                >
                  <Link2 size={11} strokeWidth={1.7} />
                  <span className="turn-ref-proj">
                    {ref.projectName || (ref.projectId === SCRATCH_PROJECT_ID ? '对话' : ref.projectId === PA_PROJECT_ID ? 'Hebi8' : ref.projectId)}
                  </span>
                  <span className="ell">{ref.title || ref.taskId}</span>
                </button>
              ))}
            </div>
          )}
          <div className={`ub ${prompt.placeholder ? 'phd' : ''}`}>{prompt.text}</div>
          {prompt.detail.length > 0 && (
            <div className="udetail">
              {prompt.detail.slice(0, 8).map((d, i) => (
                <div key={i} className="ell">
                  {d}
                </div>
              ))}
              {prompt.detail.length > 8 && <div className="faint">… 另 {prompt.detail.length - 8} 条</div>}
            </div>
          )}
          <div className="umeta hover-meta">{hhmm(attempt.startedAt)}</div>
        </div>
      )}

      {attempt.state === 'waiting_resource' && (
        <div className="it nt">
          <div className="nt-h">
            <span className="lbl">等待资源</span>
          </div>
          <div className="nt-b">{tl.task.waitingReason || '目标资源暂不可用，就绪后自动继续。'}</div>
        </div>
      )}

      {raw.isLoading && !raw.data ? (
        <div className="it running">
          <span className="spin" /> 载入过程…
        </div>
      ) : (
        <Segments segs={segs} live={live} compact={compact} />
      )}

      {approvals.map((ap) => (
        <div className="it" key={ap.approvalId}>
          <ApprovalNotice approval={ap} />
        </div>
      ))}

      {isPA && !compact && paCrossDirCards.length > 0 && (
        <CrossDirSessionCards items={paCrossDirCards} />
      )}

      {!isPA && (dispatchedChildren.length > 0 || turnNativeMembers.length > 0) && (
        <OutboundCommTrack
          parentTaskId={tl.task.taskId}
          parentProjectId={tl.task.projectId}
          children={dispatchedChildren}
          nativeMembers={turnNativeMembers}
          machines={machines}
          inboxByTask={inboxByTask}
          activeChildId={activeChildId}
          activeNativeMember={activeNativeMember}
          onOpenChild={onOpenChild}
          onOpenNativeMember={onOpenNativeMember}
        />
      )}

      {!isPA &&
        sentDelegations.map((msg) => (
          <details className="it comm-out-msg" key={msg.key}>
            <summary className="comm-row is-out">
              <span className="comm-dir-ico is-out" aria-hidden="true">
                <ArrowUpRight size={13} strokeWidth={1.75} />
              </span>
              <span className="comm-ttl" title={msg.targetTitle}>
                {msg.targetTitle}
              </span>
              <span className="comm-meta">{extractOneLineGist(msg.summary)}</span>
              <span className="comm-trail">
                <span className="comm-chev" aria-hidden="true">
                  <ChevronRight size={12} strokeWidth={1.7} />
                </span>
              </span>
            </summary>
            <div className="comm-in-body">
              <Md text={msg.summary || '已发送消息'} />
              <div className="comm-in-foot">
                <span>{hhmm(msg.createdAt)}</span>
                <button
                  type="button"
                  className="sub-ret-link"
                  onClick={() => {
                    if (onOpenChild) onOpenChild(msg.targetTaskId);
                    else navigate(paths.session(msg.targetTaskId));
                  }}
                >
                  <span>{onOpenChild ? (msg.relation === 'sibling' ? '切换查看' : '分屏查看') : '打开会话'}</span>
                  <ChevronRight size={11.5} strokeWidth={1.8} />
                </button>
              </div>
            </div>
          </details>
        ))}

      {live && attempt.state !== 'waiting_approval' && attempt.state !== 'waiting_resource' && !hasActiveToolStep && (
        <div className="it running">
          <span className="spin" />
          <span className="shim ell">{attemptStateLabel(attempt.state)}</span>
        </div>
      )}

      {/* Conversation-inline images/diagrams render directly in flow (NOT forced into Artifacts) */}
      {inlineImages.length > 0 && (
        <div className="it inline-media-stack">
          {inlineImages.map((img) => (
            <AuthedInlineImage key={`${img.sha256}:${img.path}`} sha256={img.sha256} name={basename(img.path)} />
          ))}
        </div>
      )}

      {/* Reusable Artifacts shelf below the reply turn */}
      {!compact && artifacts.length > 0 && (
        <div className="it dlv-section">
          <div className="dlv-section-hd">
            <span className="dlv-section-title">Artifacts</span>
          </div>
          <div className="dlv-grid">
            {artifacts.map((art) => (
              <button
                type="button"
                key={art.artifactId}
                className="dlv-card"
                onClick={() => openReview('art', delivery!.taskId, delivery!.deliveryId, art.artifactId)}
              >
                <span className="dlv-ico">
                  <ArtifactGlyph kind={art.kind} />
                </span>
                <span className="dlv-info">
                  <span className="dlv-title">{art.title}</span>
                  <span className="dlv-meta mono">{art.locator}</span>
                </span>
                <span className="dlv-kind-tag">{artifactShortLabel(art.kind)}</span>
              </button>
            ))}
          </div>
        </div>
      )}

      {/* Plain-folder in-place changes strip (full prose width, airy border + left rail) */}
      {!compact && attempt.folderChanges && attempt.folderChanges.files.length > 0 && (
        <FolderChangesCard
          taskId={tl.task.taskId}
          attemptId={attempt.attemptId}
          changes={attempt.folderChanges}
        />
      )}

      {/* Quiet Source Diff line when code changes were produced */}
      {delivery && hasCodeDiff && (
        <div className="it dline" title={shortSha(delivery.headSha) || undefined}>
          <span>代码改动</span>
          <span className="mono">
            <span className="add">+{delivery.manifest.insertions}</span> <span className="del">−{delivery.manifest.deletions}</span>
          </span>
          <button type="button" className="lk" onClick={() => openReview('code', delivery.taskId, delivery.deliveryId)}>
            Source Review
          </button>
        </div>
      )}

      {isLast && taskIdle && (attempt.state === 'needs_verification' || attempt.state === 'failed' || attempt.state === 'expired') && (
        <RetryNotice attempt={attempt} taskId={tl.task.taskId} needsVerify={attempt.state === 'needs_verification'} />
      )}

      {!live && attempt.state !== 'succeeded' && (
        <div className="it done-line">
          {attemptStateLabel(attempt.state)}
          {attempt.finishedAt ? ` · ${hhmm(attempt.finishedAt)}` : ''}
        </div>
      )}
    </div>
  );
}
