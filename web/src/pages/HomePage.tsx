import { useEffect, useMemo, useRef, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { ArrowRight, ArrowUpRight, X, Zap } from 'lucide-react';
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
import { DiffIcon, SendUpIcon } from '../components/Icons';
import { Md } from '../components/Md';
import { PageHeader } from '../components/PageHeader';
import { ActionCards, BriefView, usePABrief, usePAChat } from '../components/PAViews';
import { useManyProjectTasks, useRecentApprovals, useTimeline } from '../hooks/queries';
import { api, errMsg } from '../lib/api';
import {
  SCRATCH_PROJECT_ID,
  approvalPreview,
  attentionTaskIds,
  relTime,
  taskDot,
  taskStateLabel,
  taskTitle,
  todayLabel,
} from '../lib/format';
import { navigate, paths } from '../lib/router';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';

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
      <div className="hello">
        <h1>{todayLabel().greet}</h1>
        <span className="d">{todayLabel().date}</span>
        <div className="r">
          <button type="button" className="btn" disabled={brief.isPending} onClick={() => brief.mutate()}>
            <Zap size={13} strokeWidth={1.6} />
            {brief.isPending ? '汇总中…' : 'Brief Me'}
          </button>
        </div>
      </div>
      <div className="ask">
        <textarea
          ref={ref}
          value={text}
          rows={1}
          placeholder="问 PA：今天有什么要处理？"
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
        <button type="button" className="send" title="发送 ↵" disabled={!text.trim() || chat.isPending} onClick={send}>
          {chat.isPending ? <span className="spin" /> : <SendUpIcon />}
        </button>
      </div>
      {out && (
        <div className="reply">
          <div className="reply-h">
            <span className="ell">{out.kind === 'chat' ? 'PA' : `过去 ${out.res.sinceHours} 小时摘要`}</span>
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

interface AttentionGroup {
  key: string;
  taskId?: string;
  projectId: string;
  primary: InboxItemView;
  items: InboxItemView[];
  updatedAt: number;
}

const CAT_RANK: Record<string, number> = {
  approval_pending: 0,
  task_blocked: 1,
  delivery_review: 2,
  memory_conflict: 3,
};

function groupInboxBySession(items: InboxItemView[]): AttentionGroup[] {
  const byKey = new Map<string, InboxItemView[]>();
  for (const it of items) {
    const key = it.taskId || it.inboxId;
    const list = byKey.get(key) ?? [];
    list.push(it);
    byKey.set(key, list);
  }
  const groups: AttentionGroup[] = [];
  for (const [key, list] of byKey.entries()) {
    list.sort((a, b) => (CAT_RANK[a.category] ?? 9) - (CAT_RANK[b.category] ?? 9) || b.updatedAt - a.updatedAt);
    const primary = list[0];
    groups.push({
      key,
      taskId: primary.taskId,
      projectId: primary.projectId,
      primary,
      items: list,
      updatedAt: Math.max(...list.map((x) => x.updatedAt)),
    });
  }
  groups.sort((a, b) => (CAT_RANK[a.primary.category] ?? 9) - (CAT_RANK[b.primary.category] ?? 9) || b.updatedAt - a.updatedAt);
  return groups;
}

function BlockedSessionRow({ item, projects }: { item: InboxItemView; projects: ProjectView[] }) {
  const qc = useQueryClient();
  const tl = useTimeline(item.taskId);
  const [verified, setVerified] = useState(false);
  const attempts = tl.data?.attempts ?? [];
  const latest = attempts[attempts.length - 1];
  const needsVerify = latest?.state === 'needs_verification';
  const latestAttemptId = latest?.attemptId;
  useEffect(() => {
    setVerified(false);
  }, [latestAttemptId]);
  const canRetry = !!latest && (latest.state === 'needs_verification' || latest.state === 'failed' || latest.state === 'expired');
  const retry = useMutation({
    mutationFn: () =>
      api.retryTask(item.taskId!, {
        verifiedSite: needsVerify ? verified : undefined,
        verifiedAttemptId: needsVerify ? latestAttemptId : undefined,
      }),
    onSuccess: () => {
      toast('已发起重试');
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
      void qc.invalidateQueries({ queryKey: ['task-timeline', item.taskId] });
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });
  return (
    <div className="inbox-row k-blocked">
      <div className="h">
        <span className="sd wait" />
        <span className="kind ell">{item.title}</span>
        <span className="pj">{projectName(projects, item.projectId)}</span>
        <span className="tm">{relTime(item.updatedAt)}</span>
      </div>
      {item.summary && <div className="b">{item.summary}</div>}
      <div className="acts">
        {needsVerify && (
          <label className="chk">
            <input type="checkbox" checked={verified} onChange={(e) => setVerified(e.target.checked)} />
            我已核对现场
          </label>
        )}
        {canRetry && item.taskId && (
          <button type="button" className="btn pri" disabled={retry.isPending || (needsVerify && !verified)} onClick={() => retry.mutate()}>
            重试
          </button>
        )}
        {item.taskId && (
          <button type="button" className="btn ghost" onClick={() => navigate(paths.session(item.taskId!))}>
            进入会话 <ArrowRight size={12} strokeWidth={1.6} />
          </button>
        )}
      </div>
    </div>
  );
}

function AttentionSessionRow({
  group,
  approvalById,
  projects,
}: {
  group: AttentionGroup;
  approvalById: Map<string, ApprovalView>;
  projects: ProjectView[];
}) {
  const qc = useQueryClient();
  const openReview = useWorkbenchStore((s) => s.openReview);
  const reviewItem = group.items.find((i) => i.category === 'delivery_review');
  const complete = useMutation({
    mutationFn: () => api.completeDeliveryReview(reviewItem!.refId),
    onSuccess: () => {
      toast('已标记审阅完成');
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
      if (reviewItem) void qc.invalidateQueries({ queryKey: ['delivery', reviewItem.refId] });
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  // Single blocked item gets dedicated retry / site-verify affordance.
  if (group.items.length === 1 && group.primary.category === 'task_blocked') {
    return <BlockedSessionRow item={group.primary} projects={projects} />;
  }

  const singleApproval =
    group.items.length === 1 && group.primary.category === 'approval_pending'
      ? approvalById.get(group.primary.refId)
      : undefined;
  const cmd = singleApproval ? approvalPreview(singleApproval.payload) : '';
  const summary =
    singleApproval && singleApproval.state === 'pending'
      ? cmd || singleApproval.actionName
      : group.items.length === 1
        ? group.primary.summary
        : '当前会话需要注意';

  return (
    <div className="inbox-row k-approval">
      <div className="h">
        <span className="sd wait" />
        <span className="kind ell">{group.primary.title}</span>
        <span className="pj">{projectName(projects, group.projectId)}</span>
        <span className="tm">{relTime(group.updatedAt)}</span>
      </div>
      {summary && <div className="b">{summary}</div>}
      <div className="acts">
        {singleApproval && singleApproval.state === 'pending' && <ApprovalButtons approval={singleApproval} big />}
        {reviewItem && group.taskId && (
          <button type="button" className="btn pri" onClick={() => openReview('code', group.taskId!, reviewItem.refId)}>
            <DiffIcon size={13} /> 审阅
          </button>
        )}
        {reviewItem && group.items.length === 1 && (
          <button type="button" className="btn" disabled={complete.isPending} onClick={() => complete.mutate()}>
            审阅完成
          </button>
        )}
        {group.taskId && (
          <button type="button" className="btn ghost" onClick={() => navigate(paths.session(group.taskId!))}>
            进入会话 <ArrowRight size={12} strokeWidth={1.6} />
          </button>
        )}
      </div>
    </div>
  );
}

function SessionRow({ t, projects, hasAttention }: { t: TaskSummary; projects: ProjectView[]; hasAttention: boolean }) {
  return (
    <button type="button" className="srow" onClick={() => navigate(paths.session(t.taskId))}>
      <span className={`sd ${taskDot(t.state, t.activeAttemptState, hasAttention)}`} />
      <span className="pj">{projectName(projects, t.projectId)}</span>
      <span className="sep">/</span>
      <span className="t">{taskTitle(t)}</span>
      <span className="st">{taskStateLabel(t, hasAttention)}</span>
      <span className="go no-mobile">
        <ArrowRight size={12} strokeWidth={1.6} />
      </span>
    </button>
  );
}

export function HomePage({ boot }: { boot: BootstrapResponse }) {
  const [scope, setScope] = useState<'active' | 'all'>('active');
  const items = boot.openInboxItems;
  const groups = useMemo(() => groupInboxBySession(items), [items]);
  const attentionSet = useMemo(() => attentionTaskIds(items), [items]);
  const hasSingleApproval = groups.some((g) => g.items.length === 1 && g.primary.category === 'approval_pending');
  const approvals = useRecentApprovals(hasSingleApproval);
  const approvalById = useMemo(() => {
    const m = new Map<string, ApprovalView>();
    for (const a of approvals.data ?? []) m.set(a.approvalId, a);
    return m;
  }, [approvals.data]);
  const allTasks = useManyProjectTasks(boot.projects, scope === 'all');

  const sessions = useMemo(() => {
    if (scope === 'active') {
      return boot.activeTasks.filter((t) => !t.parentTaskId).sort((a, b) => b.updatedAt - a.updatedAt);
    }
    return Object.values(allTasks)
      .flat()
      .filter((t) => !t.parentTaskId)
      .sort((a, b) => b.updatedAt - a.updatedAt)
      .slice(0, 40);
  }, [scope, boot.activeTasks, allTasks]);

  return (
    <main className="main">
      <PageHeader>
        <span className="ttl">总览</span>
      </PageHeader>
      <div className="page-scroll">
        <div className="page">
          <PAQuickBar boot={boot} />

          <section className="sec">
            <div className="sec-h">
              <span className="t">需要注意</span>
            </div>
            {groups.length === 0 ? (
              <div className="quiet">
                <span className="sd done" /> 当前没有需要处理的会话
              </div>
            ) : (
              groups.map((g) => (
                <AttentionSessionRow key={g.key} group={g} approvalById={approvalById} projects={boot.projects} />
              ))
            )}
          </section>

          <section className="sec">
            <div className="sec-h">
              <span className="t">会话</span>
              <div className="r">
                <div className="seg" role="tablist">
                  <button type="button" className={scope === 'active' ? 'on' : ''} onClick={() => setScope('active')}>
                    活跃中
                  </button>
                  <button type="button" className={scope === 'all' ? 'on' : ''} onClick={() => setScope('all')}>
                    全部
                  </button>
                </div>
              </div>
            </div>
            {sessions.length === 0 ? (
              <div className="quiet">
                <span className="sd done" /> {scope === 'active' ? '当前没有活跃会话' : '还没有任何会话'}
              </div>
            ) : (
              sessions.map((t) => (
                <SessionRow key={t.taskId} t={t} projects={boot.projects} hasAttention={attentionSet.has(t.taskId)} />
              ))
            )}
          </section>
        </div>
      </div>
    </main>
  );
}
