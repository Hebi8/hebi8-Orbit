import { useMemo, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { ArrowRight, FoldVertical, Layers, Sparkles, Zap } from 'lucide-react';
import type { BootstrapResponse } from '../generated/protocol';
import { Hebi8LineDrawer } from '../components/Hebi8LineDrawer';
import { PageHeader } from '../components/PageHeader';
import { StatusMark } from '../components/StatusMark';
import { useHebi8Main, useTimeline } from '../hooks/queries';
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

const QUICK_BRIEF_PROMPT =
  '请基于当前工作区概览，简明汇总各个项目的最新进展、待我确认的审批或阻塞项，并按优先级给出接下来的建议。';

export function PAPage({ boot, threadId }: { boot: BootstrapResponse; threadId?: string }) {
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

function Hebi8MainView({ boot, mainQ }: { boot: BootstrapResponse; mainQ: ReturnType<typeof useHebi8Main> }) {
  const qc = useQueryClient();
  const mainTask = mainQ.data?.task;
  const activeTaskId = mainTask?.taskId;
  const tlq = useTimeline(activeTaskId);

  const [drawerOpen, setDrawerOpen] = useState(false);
  const [selectedNodeId, setSelectedNodeId] = useState<string | null>(null);

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
      const foldSuffix = res.fold ? `，并合并 ${res.fold.covers?.length ?? 0} 段 → ${res.fold.id}` : '';
      toast(`已折叠换代 ${res.segment.id}${foldSuffix}`);
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
  const pins = mainQ.data?.pins ?? [];
  const lineSummary = mainQ.data?.lineSummary ?? {
    totalNodes: 0,
    segmentCount: 0,
    foldCount: 0,
    pinnedCount: 0,
    maxLevel: 0,
  };
  const pendingTurns = mainQ.data?.pendingTurnsSinceLastFold ?? 0;

  const openFoldNode = (nodeId: string) => {
    setSelectedNodeId(nodeId);
    setDrawerOpen(true);
  };

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
        一条持续向上延伸的全局对话线，直连本机真实 Agent，支持引用任意项目或对话上下文，并可随时通过 Fold-Rollover 折叠换代保持清爽记忆。
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
          <>
            <button
              type="button"
              className={`h8-hdr-btn ${drawerOpen ? 'on' : ''}`}
              title="上下文折叠树"
              aria-label="上下文折叠树"
              onClick={() => setDrawerOpen((v) => !v)}
            >
              <Layers size={13.5} strokeWidth={1.7} />
              <span className="no-mobile">上下文折叠树</span>
              {lineSummary.totalNodes > 0 && <span className="h8-hdr-badge mono">{lineSummary.totalNodes}</span>}
            </button>
            <button
              type="button"
              className={`h8-hdr-btn ${lineSummary.shouldSuggestFold ? 'suggest' : ''}`}
              title={
                pendingTurns === 0
                  ? '暂无可折叠的新增完成轮次'
                  : busy
                    ? '请等待当前轮回复完成后再折叠换代'
                    : `将本段 ${pendingTurns} 轮封装为摘要节点并开启全新底层会话`
              }
              aria-label="折叠换代"
              disabled={pendingTurns === 0 || busy || foldMut.isPending}
              onClick={() => foldMut.mutate()}
            >
              <FoldVertical size={13.5} strokeWidth={1.7} />
              <span className="no-mobile">{foldMut.isPending ? '折叠中…' : '折叠换代'}</span>
              {pendingTurns > 0 && <span className="h8-hdr-badge mono">{pendingTurns}</span>}
            </button>
            <button
              type="button"
              className="ib"
              title="工作区速览"
              aria-label="工作区速览"
              disabled={quickSend.isPending || !activeTaskId || !hasReadyAgent}
              onClick={() => quickSend.mutate(QUICK_BRIEF_PROMPT)}
            >
              <Zap size={14} strokeWidth={1.6} />
            </button>
          </>
        }
      >
        <div className="h8-head-title">
          <span className="ttl">Hebi8</span>
          {currentMark && <StatusMark mark={currentMark} />}
          {lineSummary.shouldSuggestFold && pendingTurns > 0 && !busy && (
            <button
              type="button"
              className="h8-suggest-pill no-mobile"
              onClick={() => foldMut.mutate()}
              disabled={foldMut.isPending}
            >
              本段已累积 {pendingTurns} 轮，点击折叠换代
            </button>
          )}
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
            emptySlot={emptyWelcome}
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
                pins={pins}
                summary={lineSummary}
                selectedNodeId={selectedNodeId}
                onSelectNode={setSelectedNodeId}
                onClose={() => setDrawerOpen(false)}
              />
            </div>
          )}
        </div>
      )}
    </main>
  );
}
