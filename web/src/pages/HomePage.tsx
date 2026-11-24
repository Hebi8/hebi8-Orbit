import { useMemo, useRef, useState } from 'react';
import { ArrowUpRight, X, Zap } from 'lucide-react';
import type {
  ApprovalView,
  BootstrapResponse,
  InboxItemView,
  PABriefResponse,
  PAChatResponse,
  ProjectView,
  TaskSummary,
} from '../generated/protocol';
import { ApprovalButtons } from '../components/ApprovalNotice';
import { SendUpIcon } from '../components/Icons';
import { Md } from '../components/Md';
import { PageHeader } from '../components/PageHeader';
import { ActionCards, BriefView, usePABrief, usePAChat } from '../components/PAViews';
import { StatusMark } from '../components/StatusMark';
import { useRecentApprovals } from '../hooks/queries';
import {
  SCRATCH_PROJECT_ID,
  approvalPreview,
  displayMarkRank,
  inboxItemsMark,
  indexInboxByTask,
  sessionMark,
  shortSha,
  taskTitle,
  type SessionMark,
} from '../lib/format';
import { navigate, paths } from '../lib/router';
import { useWorkbenchStore } from '../store/useWorkbenchStore';

export function projectName(projects: ProjectView[], pid: string): string {
  if (pid === SCRATCH_PROJECT_ID) return '自由 Chat';
  return projects.find((p) => p.projectId === pid)?.displayName ?? pid;
}

export function useAutoGrow() {
  const ref = useRef<HTMLTextAreaElement | null>(null);
  const grow = () => {
    const el = ref.current;
    if (!el) return;
    el.style.height = 'auto';
    el.style.height = `${Math.min(el.scrollHeight, 200)}px`;
  };
  return { ref, grow };
}

type QuickOut = { kind: 'chat'; res: PAChatResponse } | { kind: 'brief'; res: PABriefResponse } | null;

function PAQuickBar({ boot }: { boot: BootstrapResponse }) {
  const [text, setText] = useState('');
  const [out, setOut] = useState<QuickOut>(null);
  const { ref, grow } = useAutoGrow();
  const chat = usePAChat((res) => {
    setOut({ kind: 'chat', res });
    setText('');
    requestAnimationFrame(grow);
  });
  const brief = usePABrief((res) => setOut({ kind: 'brief', res }));
  const send = () => {
    const msg = text.trim();
    if (!msg || chat.isPending) return;
    chat.mutate({ message: msg, threadId: boot.paState.activePaThreadId || undefined });
  };
  const threadId = out?.kind === 'chat' ? out.res.thread.paThreadId : boot.paState.activePaThreadId;

  return (
    <>
      <div className="ask">
        <textarea
          ref={ref}
          value={text}
          rows={1}
          placeholder="问 PA…"
          onChange={(e) => {
            setText(e.target.value);
            grow();
          }}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
              e.preventDefault();
              send();
            }
          }}
          aria-label="问 PA"
        />
        <button
          type="button"
          className="ib"
          title="今日摘要"
          aria-label="今日摘要"
          disabled={brief.isPending}
          onClick={() => brief.mutate()}
        >
          {brief.isPending ? <span className="spin" /> : <Zap size={14} strokeWidth={1.6} />}
        </button>
        <button type="button" className="send" title="发送 ↵" aria-label="发送" disabled={!text.trim() || chat.isPending} onClick={send}>
          {chat.isPending ? <span className="spin" /> : <SendUpIcon />}
        </button>
      </div>
      {out && (
        <div className="reply">
          <div className="reply-h">
            <span className="ell">{out.kind === 'chat' ? 'PA' : '今日摘要'}</span>
            <span className="r">
              <button type="button" className="btn ghost" onClick={() => navigate(paths.pa(threadId || undefined))}>
                在 PA 页继续 <ArrowUpRight size={12} strokeWidth={1.6} />
              </button>
              <button type="button" className="ib" title="收起" onClick={() => setOut(null)}>
                <X size={13} strokeWidth={1.6} />
              </button>
            </span>
          </div>
          {out.kind === 'chat' ? (
            <>
              <Md text={out.res.assistantMessage.content} />
              <ActionCards actions={out.res.actions ?? out.res.assistantMessage.actions} />
            </>
          ) : (
            <BriefView brief={out.res} projects={boot.projects} />
          )}
        </div>
      )}
    </>
  );
}

const CAT_RANK: Record<string, number> = {
  approval_pending: 0,
  task_blocked: 1,
  delivery_review: 2,
  memory_conflict: 3,
};

interface HomeEntry {
  key: string;
  taskId?: string;
  projectId: string;
  title: string;
  mark: Exclude<SessionMark, null>;
  updatedAt: number;
  primaryInbox?: InboxItemView;
  inboxItems: InboxItemView[];
}

function buildHomeEntries(boot: BootstrapResponse): HomeEntry[] {
  const inboxByTask = indexInboxByTask(boot.openInboxItems);
  const taskById = new Map<string, TaskSummary>();
  for (const t of boot.activeTasks) {
    if (!t.parentTaskId) taskById.set(t.taskId, t);
  }

  const seenKeys = new Set<string>();
  const entries: HomeEntry[] = [];

  // 1. All root active tasks (merged with any open inbox items for that task)
  for (const t of boot.activeTasks) {
    if (t.parentTaskId) continue;
    const inbox = [...(inboxByTask.get(t.taskId) ?? [])].sort(
      (a, b) => (CAT_RANK[a.category] ?? 9) - (CAT_RANK[b.category] ?? 9) || b.updatedAt - a.updatedAt,
    );
    const mark = sessionMark(t, inbox);
    if (!mark) continue;
    seenKeys.add(t.taskId);
    entries.push({
      key: t.taskId,
      taskId: t.taskId,
      projectId: t.projectId,
      title: taskTitle(t),
      mark,
      updatedAt: Math.max(t.updatedAt, ...inbox.map((i) => i.updatedAt)),
      primaryInbox: inbox[0],
      inboxItems: inbox,
    });
  }

  // 2. Open inbox items whose task wasn't in activeTasks (e.g. standalone or child-only)
  const extraByKey = new Map<string, InboxItemView[]>();
  for (const it of boot.openInboxItems) {
    const key = it.taskId || it.inboxId;
    if (seenKeys.has(key)) continue;
    const list = extraByKey.get(key) ?? [];
    list.push(it);
    extraByKey.set(key, list);
  }
  for (const [key, list] of extraByKey.entries()) {
    list.sort((a, b) => (CAT_RANK[a.category] ?? 9) - (CAT_RANK[b.category] ?? 9) || b.updatedAt - a.updatedAt);
    const primary = list[0];
    const mark = inboxItemsMark(list);
    if (!mark) continue;
    entries.push({
      key,
      taskId: primary.taskId,
      projectId: primary.projectId,
      title: primary.title,
      mark,
      updatedAt: Math.max(...list.map((i) => i.updatedAt)),
      primaryInbox: primary,
      inboxItems: list,
    });
  }

  entries.sort((a, b) => displayMarkRank(a.mark) - displayMarkRank(b.mark) || b.updatedAt - a.updatedAt);
  return entries;
}

function HomeRow({
  entry,
  approvalById,
  projects,
}: {
  entry: HomeEntry;
  approvalById: Map<string, ApprovalView>;
  projects: ProjectView[];
}) {
  const openReview = useWorkbenchStore((s) => s.openReview);
  const openSession = () => {
    if (entry.taskId) navigate(paths.session(entry.taskId));
  };

  // Running background row: single clean clickable row, zero buttons or subtitle
  if (entry.mark === 'running') {
    return (
      <button type="button" className="hrow hrow-slim" onClick={openSession}>
        <span className="t ell">{entry.title}</span>
        <span className="pj ell">{projectName(projects, entry.projectId)}</span>
        <StatusMark mark={entry.mark} />
      </button>
    );
  }

  const primary = entry.primaryInbox;
  const singleApproval =
    entry.inboxItems.length === 1 && primary?.category === 'approval_pending'
      ? approvalById.get(primary.refId)
      : undefined;
  const reviewItem = entry.inboxItems.find((i) => i.category === 'delivery_review');
  const cmd = singleApproval ? approvalPreview(singleApproval.payload) : '';

  let detail = '';
  if (singleApproval && singleApproval.state === 'pending') {
    const bits = [singleApproval.actionName];
    if (singleApproval.targetEnv) bits.push(singleApproval.targetEnv);
    if (singleApproval.inputSha) bits.push(shortSha(singleApproval.inputSha));
    detail = bits.filter(Boolean).join(' · ');
  } else if (primary?.summary) {
    detail = primary.summary;
  }

  const hasActions = (singleApproval && singleApproval.state === 'pending') || (!!reviewItem && !!entry.taskId);

  return (
    <div className="hrow">
      <button type="button" className="hrow-top" onClick={openSession} disabled={!entry.taskId}>
        <span className="t ell">{entry.title}</span>
        <span className="pj ell">{projectName(projects, entry.projectId)}</span>
        <StatusMark mark={entry.mark} />
      </button>
      {(detail || hasActions) && (
        <div className="hrow-sub">
          <div className="hrow-desc">
            {detail && <div className="b">{detail}</div>}
            {cmd && (
              <details className="cmd-fold">
                <summary>查看命令</summary>
                <code className="mono">{cmd}</code>
              </details>
            )}
          </div>
          {hasActions && (
            <div className="hrow-acts">
              {singleApproval && singleApproval.state === 'pending' && <ApprovalButtons approval={singleApproval} />}
              {reviewItem && entry.taskId && (
                <button
                  type="button"
                  className="btn pri"
                  onClick={() => openReview('code', entry.taskId!, reviewItem.refId)}
                >
                  看产出
                </button>
              )}
            </div>
          )}
        </div>
      )}
    </div>
  );
}

export function HomePage({ boot }: { boot: BootstrapResponse }) {
  const entries = useMemo(() => buildHomeEntries(boot), [boot]);
  const hasSingleApproval = entries.some(
    (e) => e.inboxItems.length === 1 && e.primaryInbox?.category === 'approval_pending',
  );
  const approvals = useRecentApprovals(hasSingleApproval);
  const approvalById = useMemo(() => {
    const m = new Map<string, ApprovalView>();
    for (const a of approvals.data ?? []) m.set(a.approvalId, a);
    return m;
  }, [approvals.data]);

  const actionable = entries.filter((e) => e.mark !== 'running');
  const running = entries.filter((e) => e.mark === 'running');

  return (
    <main className="main">
      <PageHeader>
        <span className="ttl">总览</span>
      </PageHeader>
      <div className="page-scroll">
        <div className="page home-page">
          <PAQuickBar boot={boot} />

          <section className="home-list" aria-label="当前会话">
            {entries.length === 0 ? (
              <div className="quiet-alone">都安静着</div>
            ) : (
              <>
                {actionable.map((e) => (
                  <HomeRow key={e.key} entry={e} approvalById={approvalById} projects={boot.projects} />
                ))}
                {actionable.length > 0 && running.length > 0 && <div className="home-sep" role="separator" />}
                {running.map((e) => (
                  <HomeRow key={e.key} entry={e} approvalById={approvalById} projects={boot.projects} />
                ))}
              </>
            )}
          </section>
        </div>
      </div>
    </main>
  );
}
