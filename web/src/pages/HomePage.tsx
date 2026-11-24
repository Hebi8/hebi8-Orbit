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
import { ApprovalButtons, ApprovalFacts } from '../components/ApprovalNotice';
import { ArtIcon, DiffIcon, SendUpIcon } from '../components/Icons';
import { Md } from '../components/Md';
import { PageHeader } from '../components/PageHeader';
import { ActionCards, BriefView, usePABrief, usePAChat } from '../components/PAViews';
import { useDelivery, useManyProjectTasks, useRecentApprovals, useTimeline } from '../hooks/queries';
import { api, errMsg } from '../lib/api';
import {
  SCRATCH_PROJECT_ID,
  approvalStateLabel,
  attemptStateLabel,
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
          placeholder="直接问 PA：今天有什么要处理？哪个任务卡住了？"
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
            <span className="ell">
              {out.kind === 'chat'
                ? `PA · 增量 #${out.res.deltaFromSeq + 1}..#${out.res.deltaToSeq} · ${out.res.deltaEventCount} 条事件`
                : `Brief · 过去 ${out.res.sinceHours}h · 快照 #${out.res.snapshotSeq}`}
            </span>
            <span className="r">
              <button type="button" className="btn ghost" onClick={() => navigate(paths.pa(threadId || undefined))}>
                在 PA 专属页继续 <ArrowUpRight size={12} strokeWidth={1.6} />
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

function ReviewInboxRow({ item, projects }: { item: InboxItemView; projects: ProjectView[] }) {
  const qc = useQueryClient();
  const openWindow = useWorkbenchStore((s) => s.openWindow);
  const dq = useDelivery(item.refId);
  const complete = useMutation({
    mutationFn: () => api.completeDeliveryReview(item.refId),
    onSuccess: () => {
      toast('已标记审阅完成');
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
      void qc.invalidateQueries({ queryKey: ['delivery', item.refId] });
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });
  const d = dq.data?.delivery;
  const bindings = dq.data?.ryugiBindings ?? [];
  const taskId = item.taskId ?? d?.taskId ?? '';
  return (
    <div className="inbox-row k-review">
      <div className="h">
        <span className="kind">待审</span>
        <span className="ell" style={{ color: 'var(--fg2)' }}>
          {item.title}
        </span>
        <span className="pj">{projectName(projects, item.projectId)}</span>
        <span className="tm">{relTime(item.updatedAt)}</span>
      </div>
      <div className="b">
        {item.summary}
        {d && (
          <span className="mono faint">
            {' '}
            · {d.manifest.filesChanged} 文件 <span className="add">+{d.manifest.insertions}</span> <span className="del">−{d.manifest.deletions}</span>
            {bindings.length > 0 ? ` · Ryugi ${bindings.length}` : ''}
          </span>
        )}
      </div>
      <div className="acts">
        <button type="button" className="btn pri" disabled={!taskId} onClick={() => openWindow('cr', taskId, item.refId)}>
          <DiffIcon size={13} /> Code Review
        </button>
        {bindings.length > 0 && (
          <button type="button" className="btn" onClick={() => openWindow('art', taskId, item.refId)}>
            <ArtIcon size={13} /> Ryugi
          </button>
        )}
        <button type="button" className="btn" disabled={complete.isPending} onClick={() => complete.mutate()}>
          完成审阅
        </button>
        {taskId && (
          <button type="button" className="btn ghost" onClick={() => navigate(paths.session(taskId))}>
            进入会话 <ArrowRight size={12} strokeWidth={1.6} />
          </button>
        )}
      </div>
    </div>
  );
}

function ApprovalInboxRow({
  item,
  approval,
  loaded,
  projects,
}: {
  item: InboxItemView;
  approval?: ApprovalView;
  loaded: boolean;
  projects: ProjectView[];
}) {
  return (
    <div className="inbox-row k-approval">
      <div className="h">
        <span className="kind">审批</span>
        <span className="ell" style={{ color: 'var(--fg2)' }}>
          {item.title}
        </span>
        <span className="pj">{projectName(projects, item.projectId)}</span>
        <span className="tm">{relTime(item.updatedAt)}</span>
      </div>
      {item.summary && <div className="b">{item.summary}</div>}
      {approval ? (
        <>
          <ApprovalFacts approval={approval} />
          {approval.state !== 'pending' && <div className="b faint">这条审批已{approvalStateLabel(approval.state)}，等待 Hub 收敛待拍板项</div>}
        </>
      ) : (
        <div className="b faint">{loaded ? '审批详情不在近期列表中，进入会话查看' : '正在读取审批详情…'}</div>
      )}
      <div className="acts">
        {approval && approval.state === 'pending' && <ApprovalButtons approval={approval} big />}
        {item.taskId && (
          <button type="button" className="btn ghost" onClick={() => navigate(paths.session(item.taskId!))}>
            查看会话 <ArrowRight size={12} strokeWidth={1.6} />
          </button>
        )}
      </div>
    </div>
  );
}

function BlockedInboxRow({ item, projects }: { item: InboxItemView; projects: ProjectView[] }) {
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
  const waiting = tl.data?.task.waitingReason;
  return (
    <div className="inbox-row k-blocked">
      <div className="h">
        <span className="kind">{needsVerify ? '待核对' : '阻塞'}</span>
        <span className="ell" style={{ color: 'var(--fg2)' }}>
          {item.title}
        </span>
        <span className="pj">{projectName(projects, item.projectId)}</span>
        <span className="tm">{relTime(item.updatedAt)}</span>
      </div>
      <div className="b">
        {item.summary}
        {waiting && waiting !== item.summary ? ` · ${waiting}` : ''}
      </div>
      {needsVerify && (
        <div className="b warn-t" style={{ fontSize: 12 }}>
          Runner 在回合中途失联，工作区可能留有半截改动。请先核对 worktree 现场，再决定是否重试。
        </div>
      )}
      {latest && !needsVerify && latest.errorCode && (
        <div className="b faint mono">
          {attemptStateLabel(latest.state)} · {latest.errorCode}
          {latest.errorDetail && latest.errorDetail !== item.summary ? ` · ${latest.errorDetail}` : ''}
        </div>
      )}
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

function SessionRow({ t, projects }: { t: TaskSummary; projects: ProjectView[] }) {
  const extras: string[] = [];
  if (t.queuedInputCount > 0) extras.push(`队列 ${t.queuedInputCount}`);
  if (t.workerCount > 0) extras.push(`${t.workerCount}w`);
  return (
    <button type="button" className="srow" onClick={() => navigate(paths.session(t.taskId))}>
      <span className={`sd ${taskDot(t.state, t.activeAttemptState)}`} />
      <span className="pj">{projectName(projects, t.projectId)}</span>
      <span className="sep">/</span>
      <span className="t">{taskTitle(t)}</span>
      <span className="st">{[taskStateLabel(t), ...extras].join(' · ')}</span>
      <span className="go no-mobile">
        进入 <ArrowRight size={12} strokeWidth={1.6} />
      </span>
    </button>
  );
}

export function HomePage({ boot }: { boot: BootstrapResponse }) {
  const [scope, setScope] = useState<'active' | 'all'>('active');
  const items = boot.openInboxItems;
  const hasApprovals = items.some((i) => i.category === 'approval_pending');
  const approvals = useRecentApprovals(hasApprovals);
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
              <span className="t">待拍板</span>
              {items.length > 0 && <span className="mono">{items.length}</span>}
            </div>
            {items.length === 0 ? (
              <div className="quiet">
                <span className="sd" /> 没有待拍板的审批、交付或阻塞
              </div>
            ) : (
              items.map((it) => {
                if (it.category === 'approval_pending')
                  return <ApprovalInboxRow key={it.inboxId} item={it} approval={approvalById.get(it.refId)} loaded={approvals.isSuccess} projects={boot.projects} />;
                if (it.category === 'delivery_review') return <ReviewInboxRow key={it.inboxId} item={it} projects={boot.projects} />;
                if (it.category === 'task_blocked') return <BlockedInboxRow key={it.inboxId} item={it} projects={boot.projects} />;
                return (
                  <div className="inbox-row" key={it.inboxId}>
                    <div className="h">
                      <span className="kind">记忆冲突</span>
                      <span className="ell">{it.title}</span>
                      <span className="tm">{relTime(it.updatedAt)}</span>
                    </div>
                    <div className="b">{it.summary}</div>
                  </div>
                );
              })
            )}
          </section>

          <section className="sec">
            <div className="sec-h">
              <span className="t">活跃 Session</span>
              {scope === 'active' && sessions.length > 0 && <span className="mono">{sessions.length}</span>}
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
                <span className="sd" /> {scope === 'active' ? '当前没有运行、等待或待审的线程' : '还没有任何线程'}
              </div>
            ) : (
              sessions.map((t) => <SessionRow key={t.taskId} t={t} projects={boot.projects} />)
            )}
          </section>
        </div>
      </div>
    </main>
  );
}
