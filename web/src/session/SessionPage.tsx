import { useEffect, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { ArrowLeft, ChevronDown, Link2 } from 'lucide-react';
import type { BootstrapResponse } from '../generated/protocol';
import { CardIcon, DiffIcon } from '../components/Icons';
import { PageHeader } from '../components/PageHeader';
import { useBoard, useTimeline } from '../hooks/queries';
import { api, errMsg } from '../lib/api';
import { SCRATCH_PROJECT_ID, memberStateLabel, taskTitle } from '../lib/format';
import { navigate, paths } from '../lib/router';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';
import { BoardPanel } from './BoardPanel';
import { RightCard } from './RightCard';
import { SessionConversation } from './SessionConversation';

const CODE_KINDS = new Set(['code_diff', 'mixed', 'workspace_snapshot']);

function BindMenu({ boot, taskId }: { boot: BootstrapResponse; taskId: string }) {
  const qc = useQueryClient();
  const [open, setOpen] = useState(false);
  const projects = boot.projects.filter((p) => p.projectId !== SCRATCH_PROJECT_ID && !p.archived);
  const bind = useMutation({
    mutationFn: (pid: string) => api.bindProject(taskId, pid),
    onSuccess: (res) => {
      toast(`已绑定到 ${boot.projects.find((p) => p.projectId === res.toProjectId)?.displayName ?? res.toProjectId}`);
      setOpen(false);
      void qc.invalidateQueries({ queryKey: ['task-timeline', taskId] });
      void qc.invalidateQueries({ queryKey: ['project-tasks'] });
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });
  return (
    <div className="relpos bind-wrap" style={{ flex: 'none' }}>
      <button type="button" className="bind-btn" onClick={() => setOpen(!open)}>
        <Link2 size={12} strokeWidth={1.6} /> 绑定到项目 <ChevronDown size={11} strokeWidth={1.6} />
      </button>
      {open && (
        <>
          <div className="scrim" style={{ background: 'transparent', backdropFilter: 'none', zIndex: 49 }} onClick={() => setOpen(false)} />
          <div className="menu" style={{ top: 28, left: 0 }}>
            <div className="mh">迁移为项目线程</div>
            {projects.map((p) => (
              <button type="button" key={p.projectId} disabled={bind.isPending} onClick={() => bind.mutate(p.projectId)}>
                <span className="ell">{p.displayName}</span>
              </button>
            ))}
            {projects.length === 0 && <div className="mh">还没有可绑定的项目</div>}
          </div>
        </>
      )}
    </div>
  );
}

export function SessionPage({ boot, taskId, worker }: { boot: BootstrapResponse; taskId: string; worker?: string }) {
  const tlq = useTimeline(taskId);
  const tl = tlq.data;
  const task = tl?.task;
  const project = boot.projects.find((p) => p.projectId === task?.projectId);
  const isScratch = task?.projectId === SCRATCH_PROJECT_ID;

  const boardOpen = useWorkbenchStore((s) => s.boardOpen);
  const setBoardOpen = useWorkbenchStore((s) => s.setBoardOpen);
  const cardOpen = useWorkbenchStore((s) => s.rightCardOpen);
  const setCardOpen = useWorkbenchStore((s) => s.setRightCardOpen);
  const mobileDock = useWorkbenchStore((s) => s.mobileDockOpen);
  const setMobileDock = useWorkbenchStore((s) => s.setMobileDockOpen);
  const expandedProjects = useWorkbenchStore((s) => s.expandedProjects);
  const setProjectExpanded = useWorkbenchStore((s) => s.setProjectExpanded);
  const openReview = useWorkbenchStore((s) => s.openReview);
  const [manualCardOpen, setManualCardOpen] = useState(false);

  const members = tl?.boardMembers ?? [];
  const hasSwarm = members.length > 0 || (task?.workerCount ?? 0) > 0;
  const boardQ = useBoard(taskId, hasSwarm || boardOpen || !!worker);

  useEffect(() => {
    setMobileDock(false);
    setManualCardOpen(false);
  }, [taskId, setMobileDock]);

  useEffect(() => {
    if (worker) setBoardOpen(true);
  }, [worker, setBoardOpen]);

  useEffect(() => {
    if (task && !isScratch && expandedProjects[task.projectId] === undefined) setProjectExpanded(task.projectId, true);
  }, [task, isScratch, expandedProjects, setProjectExpanded]);

  if (tlq.isError) {
    return (
      <main className="main">
        <PageHeader>
          <span className="ttl">会话</span>
        </PageHeader>
        <div className="empty">载入会话失败：{errMsg(tlq.error)}</div>
      </main>
    );
  }
  if (!tl || !task) {
    return (
      <main className="main">
        <PageHeader>
          <span className="ttl faint">载入会话…</span>
        </PageHeader>
      </main>
    );
  }

  const formalDeliveries = tl.deliveries.filter((d) => !d.lateArrival);
  const codeDeliveries = tl.deliveries.filter((d) => CODE_KINDS.has(d.kind));
  const formalCodeDeliveries = formalDeliveries.filter((d) => CODE_KINDS.has(d.kind) && d.manifest.filesChanged > 0);
  const latestCode =
    formalCodeDeliveries.find((d) => d.deliveryId === task.latestDeliveryId) ??
    formalCodeDeliveries[formalCodeDeliveries.length - 1] ??
    codeDeliveries[codeDeliveries.length - 1];
  const artDelivery = [...tl.deliveries].reverse().find((d) => (d.ryugiBindings?.length ?? 0) > 0);
  const primaryReview = latestCode
    ? { tab: 'code' as const, deliveryId: latestCode.deliveryId }
    : artDelivery
      ? { tab: 'art' as const, deliveryId: artDelivery.deliveryId }
      : null;

  const hasCardContent = formalCodeDeliveries.length > 0 || !!artDelivery || hasSwarm || tl.sources.length > 0;
  const cardVisible = hasCardContent ? cardOpen : manualCardOpen;
  const drilledMember = worker ? (boardQ.data?.members ?? members).find((m) => m.memberName === worker) : undefined;
  const dockMode: 'board' | 'card' | null = boardOpen ? 'board' : cardVisible ? 'card' : null;

  return (
    <main className="main">
      <PageHeader
        right={
          <>
            {primaryReview && !worker && (
              <button
                type="button"
                className="hb"
                title="审阅产出"
                onClick={() => openReview(primaryReview.tab, taskId, primaryReview.deliveryId)}
              >
                <DiffIcon size={13} />
                <span className="no-mobile">审阅</span>
              </button>
            )}
            <button
              type="button"
              className={`ib ${cardVisible && !boardOpen ? 'on' : ''} no-mobile`}
              title="右侧浮卡"
              onClick={() => {
                if (boardOpen) {
                  setBoardOpen(false);
                  if (hasCardContent) setCardOpen(true);
                  else setManualCardOpen(true);
                } else if (hasCardContent) {
                  setCardOpen(!cardOpen);
                } else {
                  setManualCardOpen(!manualCardOpen);
                }
              }}
            >
              <CardIcon />
            </button>
            <button
              type="button"
              className="ib only-mobile"
              title="会话上下文"
              onClick={() => {
                setBoardOpen(false);
                setMobileDock(true);
              }}
            >
              <CardIcon />
            </button>
          </>
        }
      >
        {worker ? (
          <>
            <button type="button" className="backbtn" onClick={() => navigate(paths.session(taskId))}>
              <ArrowLeft size={13} strokeWidth={1.6} /> 返回主会话
            </button>
            <div className="crumb">
              <span className="sep">/</span>
              <span className="c1">@{worker}</span>
              {drilledMember && <span className="faint" style={{ fontSize: 11.5 }}>{memberStateLabel(drilledMember.state)}</span>}
            </div>
          </>
        ) : (
          <>
            <div className="crumb">
              {isScratch ? (
                <span className="c0">自由 Chat</span>
              ) : (
                <button type="button" className="c0" onClick={() => project && navigate(paths.project(project.projectId))}>
                  {project?.displayName ?? task.projectId}
                </button>
              )}
              <span className="sep">/</span>
              <span className="c1" title={task.goal}>
                {taskTitle(task)}
              </span>
            </div>
            {isScratch && <BindMenu boot={boot} taskId={taskId} />}
          </>
        )}
      </PageHeader>

      <div className="sess">
        <SessionConversation
          taskId={taskId}
          tl={tl}
          worker={worker}
          drilledMember={drilledMember}
        />
        {(dockMode || mobileDock) && (
          <div
            className={`dock ${dockMode === 'card' || (!dockMode && mobileDock) ? 'has-card' : ''} ${mobileDock ? 'mobile-open' : ''}`}
            onClick={(e) => {
              if (e.target === e.currentTarget) setMobileDock(false);
            }}
          >
            {boardOpen ? (
              <BoardPanel
                taskId={taskId}
                board={boardQ.data}
                fallbackMembers={members}
                worker={worker}
                onClose={() => {
                  setBoardOpen(false);
                  setMobileDock(false);
                }}
              />
            ) : (
              <RightCard
                tl={tl}
                project={project}
                machines={boot.machines}
                board={boardQ.data}
                onOpenBoard={() => setBoardOpen(true)}
              />
            )}
          </div>
        )}
      </div>
    </main>
  );
}
