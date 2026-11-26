import { useEffect, useMemo, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { ArrowRight, GitBranch, Sparkles, X, Zap } from 'lucide-react';
import type { BootstrapResponse, Hebi8MainResponse } from '../generated/protocol';
import { Hebi8LineDrawer } from '../components/Hebi8LineDrawer';
import { PageHeader } from '../components/PageHeader';
import { StatusMark } from '../components/StatusMark';
import { useAllTasks, useHebi8Main, useTimeline } from '../hooks/queries';
import { api, errMsg } from '../lib/api';
import { PA_PROJECT_ID, indexInboxByTask, isTaskBusy, sessionMark } from '../lib/format';
import { navigate, paths } from '../lib/router';
import { SessionConversation } from '../session/SessionConversation';
import { SessionPage } from '../session/SessionPage';
import { toast } from '../store/useWorkbenchStore';

const HEBI8_STARTERS = [
  '帮我汇总当前工作区各项目的最新进展和待处理事项，按优先级排序',
  '结合我引用的历史对话，梳理关键结论、分歧点和下一步行动清单',
  '帮我拆解今天最值得推进的两件事，并给出可执行的检查步骤',
];

export function PAPage({
  boot,
  threadId,
}: {
  boot: BootstrapResponse;
  threadId?: string;
}) {
  const mainQ = useHebi8Main();
  const mainTaskId = mainQ.data?.task?.taskId;

  // Preserve historical _pa / Hebi8 task links (#/pa/<taskId> or #/hebi8/<taskId>):
  // if a specific threadId is given and does not match the current workspace Hebi8 main task,
  // render that exact historical task via SessionPage instead of dropping the ID.
  if (threadId) {
    if (mainQ.isLoading) {
      return (
        <main className="main">
          <PageHeader>
            <span className="ttl">Hebi8</span>
          </PageHeader>
          <div className="empty">载入对话…</div>
        </main>
      );
    }
    if (mainQ.isError || (mainTaskId && threadId !== mainTaskId)) {
      return <SessionPage key={threadId} boot={boot} taskId={threadId} />;
    }
  }

  return <Hebi8MainView boot={boot} mainQ={mainQ} />;
}

function Hebi8MainView({
  boot,
  mainQ,
}: {
  boot: BootstrapResponse;
  mainQ: ReturnType<typeof useHebi8Main>;
}) {
  const qc = useQueryClient();
  const mainTask = mainQ.data?.task;
  const activeTaskId = mainTask?.taskId;
  const tlq = useTimeline(activeTaskId);
  const recentNonPATasksQ = useAllTasks({ excludeProject: PA_PROJECT_ID, limit: 80, enabled: !!activeTaskId });

  const [drawerOpen, setDrawerOpen] = useState(false);
  const [selectedNodeId, setSelectedNodeId] = useState<string | null>(null);
  const [draftInsert, setDraftInsert] = useState<{ id: number; text: string } | null>(null);
  const [dismissedAtCount, setDismissedAtCount] = useState<number | null>(null);

  // Escape closes the Git-Tree drawer cleanly
  useEffect(() => {
    if (!drawerOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !e.isComposing) {
        setDrawerOpen(false);
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [drawerOpen]);

  const inboxByTask = useMemo(() => indexInboxByTask(boot.openInboxItems), [boot.openInboxItems]);

  const hasReadyAgent = useMemo(() => {
    for (const m of boot.machines) {
      if (m.transportMode === 'ssh_outbound' || m.status !== 'online') continue;
      if ((m.agents ?? []).some((a) => a.state === 'ready' && a.enabled) || (m.adapters ?? []).length > 0) {
        return true;
      }
    }
    return false;
  }, [boot.machines]);

  const quickSend = useMutation({
    mutationFn: async (message: string) => {
      if (!activeTaskId) throw new Error('Hebi8 主对话尚未就绪');
      return api.followup(activeTaskId, {
        message,
        mode: 'queue',
      });
    },
    onSuccess: () => {
      if (activeTaskId) {
        void qc.invalidateQueries({ queryKey: ['task-timeline', activeTaskId] });
      }
      void qc.invalidateQueries({ queryKey: ['hebi8-main'] });
      void qc.invalidateQueries({ queryKey: ['project-tasks', PA_PROJECT_ID] });
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const foldMut = useMutation({
    mutationFn: () => api.foldHebi8({}),
    onSuccess: (res) => {
      const foldSuffix = res.fold ? `，并合并 ${res.fold.covers?.length ?? 0} 段` : '';
      toast(`已折叠当前段${foldSuffix}`);
      // Update cached Hebi8 main state immediately so HEAD turn count drops to 0 with zero lag.
      qc.setQueryData<Hebi8MainResponse>(['hebi8-main'], (prev) =>
        prev
          ? {
              ...prev,
              nodes: res.nodes,
              pins: res.pins,
              lineSummary: res.lineSummary,
              pendingTurnsSinceLastFold: res.pendingTurnsSinceLastFold,
              pendingCharsSinceLastFold: res.pendingCharsSinceLastFold,
            }
          : prev,
      );
      if (activeTaskId) {
        void qc.invalidateQueries({ queryKey: ['task-timeline', activeTaskId] });
      }
      void qc.invalidateQueries({ queryKey: ['hebi8-main'] });
      void qc.invalidateQueries({ queryKey: ['hebi8-line'] });
      setSelectedNodeId(res.fold ? res.fold.id : res.segment.id);
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const liveTask = tlq.data?.task ?? mainTask;
  const currentMark = liveTask ? sessionMark(liveTask, inboxByTask.get(liveTask.taskId) ?? []) : null;
  const busy = liveTask ? isTaskBusy(liveTask) : false;
  const nodes = mainQ.data?.nodes ?? [];
  const lineSummary = mainQ.data?.lineSummary ?? {
    totalNodes: 0,
    segmentCount: 0,
    foldCount: 0,
    pinnedCount: 0,
    maxLevel: 0,
  };
  const pendingTurns = mainQ.data?.pendingTurnsSinceLastFold ?? lineSummary.pendingTurns ?? 0;
  const updatedTopLevelCount = mainQ.data?.updatedTopLevelTaskCount ?? 0;

  // Reset dismissal when updated count drops to 0 so future updates can surface naturally
  useEffect(() => {
    if (updatedTopLevelCount === 0 && dismissedAtCount !== null) {
      setDismissedAtCount(null);
    }
  }, [updatedTopLevelCount, dismissedAtCount]);

  const showQuickBrief = updatedTopLevelCount > 0 && dismissedAtCount !== updatedTopLevelCount;
  const quickBriefPrompt = `请帮我快速汇总当前 ${updatedTopLevelCount} 个有更新的顶层会话进展、待确认审批与阻塞项，并按优先级给出接下来的建议。`;

  const openFoldNode = (nodeId: string) => {
    setSelectedNodeId(nodeId);
    setDrawerOpen(true);
  };

  const quickBriefSlot = showQuickBrief ? (
    <div className="h8-prompt-wrap">
      <div className="h8-prompt-bar" role="region" aria-label="快速汇总建议">
        <button
          type="button"
          className="h8-prompt-chip"
          title="点击填充汇总提示词到输入框"
          onClick={() => {
            setDraftInsert((prev) => ({ id: (prev?.id ?? 0) + 1, text: quickBriefPrompt }));
          }}
        >
          <span className="h8-prompt-ico" aria-hidden="true">
            <Zap size={12.5} strokeWidth={1.8} />
          </span>
          <span>汇总 {updatedTopLevelCount} 个未读会话的新进展与阻塞项</span>
        </button>
        <button
          type="button"
          className="h8-prompt-send"
          title="立即发送汇总请求"
          disabled={!hasReadyAgent || busy || quickSend.isPending || !activeTaskId}
          onClick={() => {
            setDismissedAtCount(updatedTopLevelCount);
            quickSend.mutate(quickBriefPrompt);
          }}
        >
          {quickSend.isPending ? '发送中…' : '直接发送'}
        </button>
        <button
          type="button"
          className="ib h8-prompt-dismiss"
          title="暂不需要"
          aria-label="关闭快速汇总建议"
          onClick={() => setDismissedAtCount(updatedTopLevelCount)}
        >
          <X size={12.5} strokeWidth={1.8} />
        </button>
      </div>
    </div>
  ) : null;

  const emptyWelcome = (
    <div className="h8-empty-stage">
      <div className="pa-badge">
        <Sparkles size={13} strokeWidth={1.7} />
        <span>Hebi8</span>
      </div>
      <h1 className="nt-heading" style={{ marginBottom: 6 }}>
        工作区唯一主对话
      </h1>
      <p className="nt-hint" style={{ marginBottom: 18 }}>
        跨目录个人协调主线，直连本机真实 Agent，支持引用任意项目或对话上下文，并通过纵向 Git-Tree 滚动折叠保持清晰记忆。
      </p>
      {!hasReadyAgent && (
        <div className="nt-no-agent" style={{ marginBottom: 16 }}>
          <span>还没有可用的本地 Agent，无法启动 Hebi8 对话</span>
          <button type="button" className="nt-no-agent-link" onClick={() => navigate(paths.onboarding())}>
            去接入 Agent
          </button>
        </div>
      )}
      <div className="nt-starters">
        <div className="nt-starters-lbl">直接开始</div>
        <div className="starter-list">
          {HEBI8_STARTERS.map((s) => (
            <button
              type="button"
              key={s}
              className="starter-btn"
              disabled={!hasReadyAgent || quickSend.isPending}
              onClick={() => quickSend.mutate(s)}
            >
              <span className="ell">{s}</span>
              <ArrowRight size={13} strokeWidth={1.6} className="arr" />
            </button>
          ))}
        </div>
      </div>
    </div>
  );

  return (
    <main className="main">
      <PageHeader
        right={
          <button
            type="button"
            className={`ib ${drawerOpen ? 'on' : ''}`}
            title="上下文节点树"
            aria-label="上下文节点树"
            aria-pressed={drawerOpen}
            onClick={() => setDrawerOpen((v) => !v)}
          >
            <GitBranch size={15} strokeWidth={1.7} />
          </button>
        }
      >
        <div className="h8-head-title">
          <span className="ttl">Hebi8</span>
          {currentMark && <StatusMark mark={currentMark} />}
        </div>
      </PageHeader>

      {mainQ.isError ? (
        <div className="empty">载入 Hebi8 主对话失败：{errMsg(mainQ.error)}</div>
      ) : !activeTaskId || tlq.isLoading ? (
        <div className="empty">载入 Hebi8 主对话…</div>
      ) : tlq.isError || !tlq.data ? (
        <div className="empty">载入对话时间线失败：{errMsg(tlq.error)}</div>
      ) : (
        <div className="sess">
          <SessionConversation
            taskId={activeTaskId}
            tl={tlq.data}
            boot={boot}
            extraTasks={recentNonPATasksQ.data}
            emptySlot={emptyWelcome}
            composerTopSlot={quickBriefSlot}
            composerDraftInsert={draftInsert}
            onOpenFoldNode={openFoldNode}
          />
          {drawerOpen && (
            <div
              className="dock has-card h8-dock"
              onClick={(e) => {
                if (e.target === e.currentTarget) setDrawerOpen(false);
              }}
            >
              <Hebi8LineDrawer
                nodes={nodes}
                summary={lineSummary}
                pendingTurns={pendingTurns}
                busy={busy}
                folding={foldMut.isPending}
                selectedNodeId={selectedNodeId}
                onSelectNode={setSelectedNodeId}
                onFold={() => foldMut.mutate()}
                onClose={() => setDrawerOpen(false)}
              />
            </div>
          )}
        </div>
      )}
    </main>
  );
}
