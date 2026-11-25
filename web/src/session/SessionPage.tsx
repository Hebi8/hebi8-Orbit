import { useEffect, useMemo, useRef, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { ArrowLeft, Check, ChevronDown, Layers, Link2, MessageSquare } from 'lucide-react';
import type { BootstrapResponse } from '../generated/protocol';
import { CardIcon, DiffIcon } from '../components/Icons';
import { IdeOpenButton } from '../components/IdeOpenButton';
import { PageHeader } from '../components/PageHeader';
import { StatusMark } from '../components/StatusMark';
import { useBoard, useTimeline } from '../hooks/queries';
import { api, errMsg } from '../lib/api';
import { collectSessionArtifacts } from '../lib/artifacts';
import {
  SCRATCH_PROJECT_ID,
  artifactShortLabel,
  indexInboxByTask,
  memberStateLabel,
  sessionMark,
  shortSha,
  taskTitle,
} from '../lib/format';
import { resolveMachineSshAlias } from '../lib/ide';
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
        <Link2 size={11} strokeWidth={1.6} /> 绑定项目 <ChevronDown size={10} strokeWidth={1.6} />
      </button>
      {open && (
        <>
          <div className="scrim" style={{ background: 'transparent', backdropFilter: 'none', zIndex: 49 }} onClick={() => setOpen(false)} />
          <div className="menu" style={{ top: 24, left: 0 }}>
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
  const [artMenuOpen, setArtMenuOpen] = useState(false);
  const artMenuRef = useRef<HTMLDivElement | null>(null);

  const members = tl?.boardMembers ?? [];
  const hasSwarm = members.length > 0 || (task?.workerCount ?? 0) > 0;
  const boardQ = useBoard(taskId, hasSwarm || boardOpen || !!worker);
  const sessionArtifacts = useMemo(() => collectSessionArtifacts(tl), [tl]);
  const inboxByTask = useMemo(() => indexInboxByTask(boot.openInboxItems), [boot.openInboxItems]);

  useEffect(() => {
    setMobileDock(false);
    setManualCardOpen(false);
    setArtMenuOpen(false);
  }, [taskId, setMobileDock]);

  useEffect(() => {
    if (worker) setBoardOpen(true);
  }, [worker, setBoardOpen]);

  useEffect(() => {
    if (task && !isScratch && expandedProjects[task.projectId] === undefined) setProjectExpanded(task.projectId, true);
  }, [task, isScratch, expandedProjects, setProjectExpanded]);

  useEffect(() => {
    if (!artMenuOpen) return;
    const onDown = (e: MouseEvent) => {
      if (artMenuRef.current && !artMenuRef.current.contains(e.target as Node)) {
        setArtMenuOpen(false);
      }
    };
    window.addEventListener('mousedown', onDown);
    return () => window.removeEventListener('mousedown', onDown);
  }, [artMenuOpen]);

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

  const mark = sessionMark(task, inboxByTask.get(task.taskId) ?? []);
  const lastAttempt = tl.attempts[tl.attempts.length - 1];
  const machineId =
    latestCode?.sourceMachineId ||
    lastAttempt?.machineId ||
    (project ? project.anchors[task.requiredCapability || project.defaultCapability]?.machine : '') ||
    '';
  const hostMachine = boot.machines.find((m) => m.machineId === machineId);
  const hostLabel = machineId ? hostMachine?.displayAlias || resolveMachineSshAlias(hostMachine, machineId) : '';
  const isPlainFolderTask =
    lastAttempt?.isolation === 'in_place' ||
    (!!lastAttempt?.partId && project?.parts?.find((p) => p.partId === lastAttempt.partId)?.kind === 'folder') ||
    (!!project?.repoIdentity && project.repoIdentity.startsWith('folder:'));
  const gitMeta = isPlainFolderTask
    ? ''
    : latestCode?.headSha
      ? shortSha(latestCode.headSha)
      : project?.defaultBranch || '';

  const hasCardContent = formalCodeDeliveries.length > 0 || sessionArtifacts.length > 0 || hasSwarm || tl.sources.length > 0;
  const cardVisible = hasCardContent ? cardOpen : manualCardOpen;
  const drilledMember = worker ? (boardQ.data?.members ?? members).find((m) => m.memberName === worker) : undefined;
  const dockMode: 'board' | 'card' | null = boardOpen ? 'board' : cardVisible ? 'card' : null;
  const hasCodeDiff = !!latestCode && (latestCode.manifest.insertions > 0 || latestCode.manifest.deletions > 0 || latestCode.manifest.filesChanged > 0);
  const openDiffComments = tl.annotations.filter((a) => a.status === 'open' && a.source !== 'ryugi').length;

  return (
    <main className="main">
      <PageHeader
        className={!worker ? 'thead-session' : undefined}
        right={
          <>
            {!worker && project && (
              <div className="no-mobile">
                <IdeOpenButton project={project} delivery={latestCode} machines={boot.machines} />
              </div>
            )}
            {!worker && latestCode && (
              <button
                type="button"
                className={`diff-entry-btn ${hasCodeDiff ? '' : 'is-clean'}`}
                title="打开 Source Review"
                onClick={() => openReview('code', taskId, latestCode.deliveryId)}
              >
                {hasCodeDiff ? (
                  <>
                    <DiffIcon size={13} />
                    <span className="add">+{latestCode.manifest.insertions}</span>
                    <span className="del">−{latestCode.manifest.deletions}</span>
                    {openDiffComments > 0 && (
                      <span className="comment-count-badge">
                        <MessageSquare size={10} strokeWidth={1.8} />
                        {openDiffComments}
                      </span>
                    )}
                  </>
                ) : (
                  <>
                    <Check size={12} strokeWidth={2} />
                    <span>Checkpoint</span>
                  </>
                )}
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
              {drilledMember && (
                <span className="faint" style={{ fontSize: 11.5 }}>
                  {memberStateLabel(drilledMember.state)}
                </span>
              )}
            </div>
          </>
        ) : (
          <div className="topbar-left">
            <div className="topbar-title-row">
              <span className="topbar-title" title={task.goal}>
                {taskTitle(task)}
              </span>
              {sessionArtifacts.length > 0 && (
                <div className="topbar-art-wrap" ref={artMenuRef}>
                  <button
                    type="button"
                    className={`topbar-art-icon ${artMenuOpen ? 'open' : ''}`}
                    title="本会话 Artifacts"
                    aria-label="本会话 Artifacts"
                    aria-expanded={artMenuOpen}
                    onClick={() => setArtMenuOpen((v) => !v)}
                  >
                    <Layers size={13} strokeWidth={1.7} />
                    <span>{sessionArtifacts.length}</span>
                  </button>
                  {artMenuOpen && (
                    <div className="topbar-art-menu" role="menu">
                      <div className="ov-menu-sec">本会话 Artifacts</div>
                      {sessionArtifacts.map((art) => (
                        <button
                          type="button"
                          role="menuitem"
                          key={art.artifactId}
                          className="ov-menu-item"
                          onClick={() => {
                            setArtMenuOpen(false);
                            openReview('art', taskId, art.deliveryId, art.artifactId);
                          }}
                        >
                          <span className="dlv-kind-tag">{artifactShortLabel(art.kind)}</span>
                          <span className="ell" style={{ flex: 1 }}>
                            {art.title}
                          </span>
                          {art.openAnnotations > 0 && (
                            <span className="ov-menu-badge">
                              <MessageSquare size={10} strokeWidth={1.8} />
                              {art.openAnnotations}
                            </span>
                          )}
                        </button>
                      ))}
                    </div>
                  )}
                </div>
              )}
              {mark && <StatusMark mark={mark} />}
            </div>
            <div className="topbar-meta">
              {isScratch ? (
                <>
                  <span>对话</span>
                  <BindMenu boot={boot} taskId={taskId} />
                </>
              ) : (
                <>
                  <button type="button" onClick={() => project && navigate(paths.project(project.projectId))}>
                    {project?.displayName ?? task.projectId}
                  </button>
                  {hostLabel && (
                    <>
                      <span className="topbar-meta-dot">·</span>
                      <span className="mono">{hostLabel}</span>
                    </>
                  )}
                  {gitMeta && (
                    <>
                      <span className="topbar-meta-dot">·</span>
                      <span className="mono">{gitMeta}</span>
                    </>
                  )}
                </>
              )}
            </div>
          </div>
        )}
      </PageHeader>

      <div className="sess">
        <SessionConversation
          taskId={taskId}
          tl={tl}
          boot={boot}
          worker={worker}
          drilledMember={drilledMember}
          onOpenReview={() => {
            if (latestCode) openReview('code', taskId, latestCode.deliveryId);
            else if (sessionArtifacts[0]) openReview('art', taskId, sessionArtifacts[0].deliveryId, sessionArtifacts[0].artifactId);
          }}
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
