import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Archive, ArchiveRestore, ArrowLeft, Layers, MessageSquare, MoreHorizontal, Trash2 } from 'lucide-react';
import type { BoardMemberCursor, BootstrapResponse } from '../generated/protocol';
import { CardIcon } from '../components/Icons';
import { IdeOpenButton } from '../components/IdeOpenButton';
import { PageHeader, type PageHeaderCrumb } from '../components/PageHeader';
import { StatusMark } from '../components/StatusMark';
import { useBoard, useTaskChildren, useTimeline } from '../hooks/queries';
import { ApiError, api, errMsg } from '../lib/api';
import { collectSessionArtifacts } from '../lib/artifacts';
import {
  PA_PROJECT_ID,
  SCRATCH_PROJECT_ID,
  artifactShortLabel,
  indexInboxByTask,
  isVirtualProject,
  memberStateLabel,
  relTime,
  sessionMark,
  shortSha,
  taskTitle,
} from '../lib/format';
import { resolveMachineSshAlias } from '../lib/ide';
import { navigate, paths, replaceHash } from '../lib/router';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';
import { BoardPanel } from './BoardPanel';
import { ChildSessionDrawer } from './ChildSessionDrawer';
import { RightCard } from './RightCard';
import { SessionConversation } from './SessionConversation';

const CODE_KINDS = new Set(['code_diff', 'mixed', 'workspace_snapshot']);
const SUB_DRAWER_WIDTH_KEY = 'orbit.subDrawerWidth';
const SUB_DRAWER_MIN = 340;
const SUB_DRAWER_MAX = 680;
const SUB_DRAWER_DEFAULT = 468;

function readDrawerWidth(): number {
  try {
    const n = Number(localStorage.getItem(SUB_DRAWER_WIDTH_KEY));
    if (Number.isFinite(n)) return Math.min(SUB_DRAWER_MAX, Math.max(SUB_DRAWER_MIN, Math.round(n)));
  } catch {
    // ignore storage errors
  }
  return SUB_DRAWER_DEFAULT;
}

function writeDrawerWidth(w: number) {
  try {
    localStorage.setItem(SUB_DRAWER_WIDTH_KEY, String(Math.round(w)));
  } catch {
    // ignore storage errors
  }
}

export function SessionPage({
  boot,
  taskId,
  worker,
  childId,
}: {
  boot: BootstrapResponse;
  taskId: string;
  worker?: string;
  childId?: string;
}) {
  const qc = useQueryClient();
  const tlq = useTimeline(taskId);
  const tl = tlq.data;
  const task = tl?.task;
  const project = boot.projects.find((p) => p.projectId === task?.projectId);
  const isScratch = task?.projectId === SCRATCH_PROJECT_ID;
  const isPA = task?.projectId === PA_PROJECT_ID;
  const isVirtual = isVirtualProject(task?.projectId);

  const boardOpen = useWorkbenchStore((s) => s.boardOpen);
  const setBoardOpen = useWorkbenchStore((s) => s.setBoardOpen);
  const cardOpen = useWorkbenchStore((s) => s.rightCardOpen);
  const setCardOpen = useWorkbenchStore((s) => s.setRightCardOpen);
  const mobileDock = useWorkbenchStore((s) => s.mobileDockOpen);
  const setMobileDock = useWorkbenchStore((s) => s.setMobileDockOpen);
  const expandedProjects = useWorkbenchStore((s) => s.expandedProjects);
  const setProjectExpanded = useWorkbenchStore((s) => s.setProjectExpanded);
  const ensureParentExpanded = useWorkbenchStore((s) => s.ensureParentExpanded);
  const openReview = useWorkbenchStore((s) => s.openReview);

  const [manualCardOpen, setManualCardOpen] = useState(false);
  const [artMenuOpen, setArtMenuOpen] = useState(false);
  const [moreMenuOpen, setMoreMenuOpen] = useState(false);
  const [confirmDeleteMenu, setConfirmDeleteMenu] = useState(false);
  const [confirmDeleteBanner, setConfirmDeleteBanner] = useState(false);
  const [actionBusy, setActionBusy] = useState(false);
  const [activeNativeMemberName, setActiveNativeMemberName] = useState<string | null>(null);
  const [drawerWidth, setDrawerWidth] = useState<number>(readDrawerWidth);
  const [draggingSash, setDraggingSash] = useState(false);

  const artMenuRef = useRef<HTMLDivElement | null>(null);
  const moreMenuRef = useRef<HTMLDivElement | null>(null);
  const sessRef = useRef<HTMLDivElement | null>(null);
  const drawerWidthRef = useRef(drawerWidth);
  drawerWidthRef.current = drawerWidth;

  const parentSummary = tl?.parentSummary;
  const parentTaskId = parentSummary?.taskId || task?.parentTaskId || '';
  const isChildSession = !!parentTaskId;

  const members = tl?.boardMembers ?? [];
  const hasSwarm = !isChildSession && (members.length > 0 || (task?.workerCount ?? 0) > 0);
  const boardQ = useBoard(taskId, !isChildSession && (hasSwarm || boardOpen || !!worker));
  const childrenQ = useTaskChildren(taskId, !!tl && !isChildSession);
  const allMembers = isChildSession ? [] : (boardQ.data?.members ?? members);
  const childrenList = isChildSession ? [] : (childrenQ.data?.children ?? tl?.children ?? []);
  const nativeMembers = useMemo(() => allMembers.filter((m) => !m.workerTaskId), [allMembers]);

  // Max depth = 1: child sessions never open a grandchild drawer.
  const matchedWorkerMember = !isChildSession && worker ? allMembers.find((m) => m.memberName === worker) : undefined;
  const upgradedChildId = matchedWorkerMember?.workerTaskId || undefined;
  const effectiveChildId = isChildSession ? undefined : (childId || upgradedChildId);
  const effectiveWorker = isChildSession || upgradedChildId ? undefined : worker;
  const activeNativeMember =
    !isChildSession && !effectiveChildId && activeNativeMemberName
      ? nativeMembers.find((m) => m.memberName === activeNativeMemberName)
      : undefined;
  const childDrawerOpen = !isChildSession && (!!effectiveChildId || !!activeNativeMember);

  const sessionArtifacts = useMemo(() => collectSessionArtifacts(tl), [tl]);
  const inboxByTask = useMemo(() => indexInboxByTask(boot.openInboxItems), [boot.openInboxItems]);

  // Entering a full child-session page auto-expands its parent branch in the sidebar (never auto-collapses when leaving).
  useEffect(() => {
    if (parentTaskId) ensureParentExpanded(parentTaskId);
  }, [parentTaskId, ensureParentExpanded]);

  useEffect(() => {
    setMobileDock(false);
    setManualCardOpen(false);
    setArtMenuOpen(false);
    setMoreMenuOpen(false);
    setConfirmDeleteMenu(false);
    setConfirmDeleteBanner(false);
    setActiveNativeMemberName(null);
  }, [taskId, setMobileDock]);

  useEffect(() => {
    if (upgradedChildId && worker) {
      replaceHash(paths.session(taskId, undefined, upgradedChildId));
    }
  }, [upgradedChildId, worker, taskId]);

  useEffect(() => {
    if (effectiveWorker) setBoardOpen(true);
  }, [effectiveWorker, setBoardOpen]);

  // Mutual exclusion: opening ChildSessionDrawer collapses BoardPanel / mobileDock.
  useEffect(() => {
    if (childDrawerOpen) {
      setBoardOpen(false);
      setMobileDock(false);
      setManualCardOpen(false);
    }
  }, [childDrawerOpen, setBoardOpen, setMobileDock]);

  useEffect(() => {
    if (childId) setActiveNativeMemberName(null);
  }, [childId]);

  useEffect(() => {
    if (task && !isVirtual && expandedProjects[task.projectId] === undefined) setProjectExpanded(task.projectId, true);
  }, [task, isVirtual, expandedProjects, setProjectExpanded]);

  // Keep `--composer-dock-h` synchronized with the main Composer dock height so the sash's
  // continuous 1px top divider line bridges both Composer borders with 0px vertical drift.
  useLayoutEffect(() => {
    const root = sessRef.current;
    if (!root || !childDrawerOpen) return;
    const dockEl = root.querySelector('.session-main-col .cwrap') as HTMLElement | null;
    if (!dockEl) return;
    const sync = () => {
      const h = Math.round(dockEl.getBoundingClientRect().height);
      if (h > 0) root.style.setProperty('--composer-dock-h', `${h}px`);
    };
    sync();
    const ro = new ResizeObserver(sync);
    ro.observe(dockEl);
    return () => {
      ro.disconnect();
      root.style.removeProperty('--composer-dock-h');
    };
  }, [childDrawerOpen, taskId]);

  useEffect(() => {
    if (!artMenuOpen && !moreMenuOpen) return;
    const onDown = (e: MouseEvent) => {
      const target = e.target as Node;
      if (artMenuOpen && artMenuRef.current && !artMenuRef.current.contains(target)) {
        setArtMenuOpen(false);
      }
      if (moreMenuOpen && moreMenuRef.current && !moreMenuRef.current.contains(target)) {
        setMoreMenuOpen(false);
        setConfirmDeleteMenu(false);
      }
    };
    window.addEventListener('mousedown', onDown);
    return () => window.removeEventListener('mousedown', onDown);
  }, [artMenuOpen, moreMenuOpen]);

  const invalidateAfterTaskMut = (pid?: string) => {
    void qc.invalidateQueries({ queryKey: ['task-timeline', taskId] });
    void qc.invalidateQueries({ queryKey: ['all-tasks'] });
    void qc.invalidateQueries({ queryKey: ['archived-tasks'] });
    void qc.invalidateQueries({ queryKey: ['global-artifacts'] });
    void qc.invalidateQueries({ queryKey: ['bootstrap'] });
    if (pid) {
      void qc.invalidateQueries({ queryKey: ['project-tasks', pid] });
    } else {
      void qc.invalidateQueries({ queryKey: ['project-tasks'] });
    }
  };

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

  const isArchived = !!task.archivedAt;
  const fallbackReturnHash = project && !isVirtual ? paths.project(project.projectId) : paths.home();

  const handleArchive = async (cancelActive = false) => {
    if (actionBusy) return;
    setActionBusy(true);
    try {
      await api.archiveTask(task.taskId, cancelActive);
      setMoreMenuOpen(false);
      invalidateAfterTaskMut(task.projectId);
      const label = taskTitle(task);
      navigate(fallbackReturnHash);
      toast(`已归档「${label}」`, 'info', {
        label: '撤销',
        onClick: () => {
          void api
            .unarchiveTask(task.taskId)
            .then(() => {
              invalidateAfterTaskMut(task.projectId);
              navigate(paths.session(task.taskId));
              toast(`已恢复「${label}」`);
            })
            .catch((e) => toast(errMsg(e), 'err'));
        },
      });
    } catch (e) {
      if (e instanceof ApiError && e.code === 'ERR_TASK_ACTIVE' && !cancelActive) {
        toast('会话正在运行中，可点击重试并自动取消后归档', 'err', {
          label: '取消并归档',
          onClick: () => void handleArchive(true),
        });
      } else {
        toast(errMsg(e), 'err');
      }
    } finally {
      setActionBusy(false);
    }
  };

  const handleUnarchive = async () => {
    if (actionBusy) return;
    setActionBusy(true);
    try {
      await api.unarchiveTask(task.taskId);
      setMoreMenuOpen(false);
      invalidateAfterTaskMut(task.projectId);
      toast(`已恢复「${taskTitle(task)}」`);
    } catch (e) {
      toast(errMsg(e), 'err');
    } finally {
      setActionBusy(false);
    }
  };

  const handleDelete = async () => {
    if (actionBusy) return;
    setActionBusy(true);
    try {
      const label = taskTitle(task);
      await api.deleteTask(task.taskId);
      setMoreMenuOpen(false);
      setConfirmDeleteMenu(false);
      setConfirmDeleteBanner(false);
      invalidateAfterTaskMut(task.projectId);
      navigate(fallbackReturnHash);
      toast(`已永久删除「${label}」`);
    } catch (e) {
      toast(errMsg(e), 'err');
    } finally {
      setActionBusy(false);
    }
  };

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

  const hasCardContent =
    !!latestCode ||
    sessionArtifacts.length > 0 ||
    (!isChildSession && (childrenList.length > 0 || hasSwarm)) ||
    tl.sources.length > 0 ||
    (!isVirtual && !!project);
  const cardVisible = hasCardContent ? cardOpen : manualCardOpen;
  const drilledMember = effectiveWorker ? allMembers.find((m) => m.memberName === effectiveWorker) : undefined;
  const dockMode: 'board' | 'card' | null = childDrawerOpen ? null : boardOpen ? 'board' : cardVisible ? 'card' : null;

  const openChildDrawer = (cid: string) => {
    if (isChildSession) {
      navigate(paths.session(cid));
      return;
    }
    setActiveNativeMemberName(null);
    setBoardOpen(false);
    setMobileDock(false);
    navigate(paths.session(taskId, undefined, cid));
  };

  const openNativeMemberDrawer = (member: BoardMemberCursor) => {
    if (isChildSession) return;
    if (member.workerTaskId) {
      openChildDrawer(member.workerTaskId);
      return;
    }
    if (childId || worker) navigate(paths.session(taskId));
    setBoardOpen(false);
    setMobileDock(false);
    setActiveNativeMemberName(member.memberName);
  };

  const closeChildDrawer = () => {
    setActiveNativeMemberName(null);
    if (childId || worker) navigate(paths.session(taskId));
  };

  const parentTitle = parentSummary?.title || (parentSummary?.isHebi8Main ? 'Hebi8 主对话' : parentTaskId || '');
  const parentReturnHash = parentSummary?.isHebi8Main
    ? paths.hebi8()
    : parentTaskId
      ? paths.session(parentTaskId)
      : fallbackReturnHash;

  const headerBreadcrumb: PageHeaderCrumb[] | undefined = effectiveWorker
    ? undefined
    : parentTaskId
      ? [
          {
            label: parentTitle,
            title: `返回父会话：${parentTitle}`,
            onClick: () => navigate(parentReturnHash),
          },
          {
            label: taskTitle(task),
            title: task.goal,
          },
        ]
      : [
          {
            label: taskTitle(task),
            title: task.goal,
          },
        ];

  const titleTrail = effectiveWorker ? undefined : (
    <>
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
                  <span className="ell grow">{art.title}</span>
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
    </>
  );

  const headerSubtitle = effectiveWorker ? undefined : isScratch ? (
    <span>自由对话</span>
  ) : isPA ? (
    <button type="button" onClick={() => navigate(paths.hebi8())}>
      Hebi8
    </button>
  ) : (
    <>
      <button type="button" onClick={() => project && navigate(paths.project(project.projectId))}>
        {project?.displayName ?? task.projectId}
      </button>
      {lastAttempt?.isolation === 'worktree' ? (
        <>
          <span className="topbar-meta-dot">·</span>
          <span>Worktree</span>
        </>
      ) : !isPlainFolderTask ? (
        <>
          <span className="topbar-meta-dot">·</span>
          <span>目录同步</span>
        </>
      ) : null}
      {gitMeta && (
        <>
          <span className="topbar-meta-dot">·</span>
          <span className="mono">{gitMeta}</span>
        </>
      )}
      {hostLabel && boot.machines.filter((m) => m.transportMode !== 'ssh_outbound').length > 1 && (
        <>
          <span className="topbar-meta-dot">·</span>
          <span className="mono faint">{hostLabel}</span>
        </>
      )}
    </>
  );

  const clampDrawerWidth = (rawW: number) => {
    const stageW = sessRef.current?.getBoundingClientRect().width ?? 0;
    const maxAllowed = stageW > 0 ? Math.min(SUB_DRAWER_MAX, Math.max(SUB_DRAWER_MIN, stageW - 310)) : SUB_DRAWER_MAX;
    return Math.round(Math.min(maxAllowed, Math.max(SUB_DRAWER_MIN, rawW)));
  };

  return (
    <main className="main">
      <PageHeader
        breadcrumb={headerBreadcrumb}
        titleTrail={titleTrail}
        subtitle={headerSubtitle}
        right={
          <>
            {!effectiveWorker && project && !isVirtual && (
              <div className="no-mobile">
                <IdeOpenButton project={project} delivery={latestCode} machines={boot.machines} />
              </div>
            )}
            <button
              type="button"
              className={`ib ${!childDrawerOpen && cardVisible && !boardOpen ? 'on' : ''} no-mobile`}
              title="右侧浮卡"
              aria-label="右侧浮卡"
              onClick={() => {
                if (childDrawerOpen) {
                  closeChildDrawer();
                  if (hasCardContent) setCardOpen(true);
                  else setManualCardOpen(true);
                  return;
                }
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
              aria-label="会话上下文"
              onClick={() => {
                if (childDrawerOpen) closeChildDrawer();
                setBoardOpen(false);
                setMobileDock(true);
              }}
            >
              <CardIcon />
            </button>
            {!effectiveWorker && (
              <div className="relpos" ref={moreMenuRef}>
                <button
                  type="button"
                  className={`ib ${moreMenuOpen ? 'on' : ''}`}
                  title="更多操作"
                  aria-label="更多操作"
                  aria-expanded={moreMenuOpen}
                  onClick={() => {
                    setConfirmDeleteMenu(false);
                    setMoreMenuOpen((v) => !v);
                  }}
                >
                  <MoreHorizontal size={15} strokeWidth={1.7} />
                </button>
                {moreMenuOpen && (
                  <div className="sess-more-menu" role="menu">
                    {isArchived ? (
                      <button
                        type="button"
                        role="menuitem"
                        className="th-menu-item"
                        disabled={actionBusy}
                        onClick={() => void handleUnarchive()}
                      >
                        <ArchiveRestore size={13} strokeWidth={1.6} />
                        <span>取消归档</span>
                      </button>
                    ) : (
                      <button
                        type="button"
                        role="menuitem"
                        className="th-menu-item"
                        disabled={actionBusy}
                        onClick={() => void handleArchive(false)}
                      >
                        <Archive size={13} strokeWidth={1.6} />
                        <span>归档会话</span>
                      </button>
                    )}
                    {confirmDeleteMenu ? (
                      <button
                        type="button"
                        role="menuitem"
                        className="th-menu-item danger"
                        disabled={actionBusy}
                        onClick={() => void handleDelete()}
                      >
                        <Trash2 size={13} strokeWidth={1.7} />
                        <span>确认永久删除</span>
                      </button>
                    ) : (
                      <button
                        type="button"
                        role="menuitem"
                        className="th-menu-item danger"
                        disabled={actionBusy}
                        onClick={() => setConfirmDeleteMenu(true)}
                      >
                        <Trash2 size={13} strokeWidth={1.6} />
                        <span>永久删除</span>
                      </button>
                    )}
                  </div>
                )}
              </div>
            )}
          </>
        }
      >
        {effectiveWorker && (
          <>
            <button type="button" className="backbtn" onClick={() => navigate(paths.session(taskId))}>
              <ArrowLeft size={13} strokeWidth={1.6} /> 返回主会话
            </button>
            <div className="crumb">
              <span className="sep">/</span>
              <span className="c1">@{effectiveWorker}</span>
              {drilledMember && (
                <span className="faint" style={{ fontSize: 11.5 }}>
                  {memberStateLabel(drilledMember.state)}
                </span>
              )}
            </div>
          </>
        )}
      </PageHeader>

      {isArchived && !effectiveWorker && (
        <div className="sess-archived-bar">
          <Archive size={13} strokeWidth={1.6} className="muted" />
          <span>已归档会话{task.archivedAt ? ` · ${relTime(task.archivedAt)}` : ''}</span>
          <div className="sess-archived-acts">
            <button
              type="button"
              className="btn ghost sm"
              disabled={actionBusy}
              onClick={() => void handleUnarchive()}
            >
              <ArchiveRestore size={12} strokeWidth={1.6} /> 恢复
            </button>
            {confirmDeleteBanner ? (
              <>
                <button
                  type="button"
                  className="btn ghost sm bad-t"
                  disabled={actionBusy}
                  onClick={() => void handleDelete()}
                >
                  确认永久删除
                </button>
                <button
                  type="button"
                  className="btn ghost sm"
                  onClick={() => setConfirmDeleteBanner(false)}
                >
                  取消
                </button>
              </>
            ) : (
              <button
                type="button"
                className="btn ghost sm bad-t"
                disabled={actionBusy}
                onClick={() => setConfirmDeleteBanner(true)}
              >
                <Trash2 size={12} strokeWidth={1.6} /> 永久删除
              </button>
            )}
          </div>
        </div>
      )}

      <div ref={sessRef} className={`sess ${childDrawerOpen ? 'has-child-drawer' : ''}`}>
        <div className="session-main-col">
          <SessionConversation
            taskId={taskId}
            tl={tl}
            boot={boot}
            worker={effectiveWorker}
            drilledMember={drilledMember}
            childrenList={childrenList}
            nativeMembers={nativeMembers}
            siblingChildren={tl.siblingChildren}
            activeChildId={effectiveChildId}
            activeNativeMember={activeNativeMember?.memberName}
            onOpenChild={openChildDrawer}
            onOpenNativeMember={isChildSession ? undefined : openNativeMemberDrawer}
            onOpenReview={() => {
              if (latestCode) openReview('code', taskId, latestCode.deliveryId);
              else if (sessionArtifacts[0]) openReview('art', taskId, sessionArtifacts[0].deliveryId, sessionArtifacts[0].artifactId);
            }}
          />
        </div>

        {childDrawerOpen ? (
          <>
            <div
              role="separator"
              aria-orientation="vertical"
              aria-label="拖拽调整子会话分屏宽度"
              aria-valuemin={SUB_DRAWER_MIN}
              aria-valuemax={SUB_DRAWER_MAX}
              aria-valuenow={drawerWidth}
              tabIndex={0}
              className={`sub-resize-handle no-mobile ${draggingSash ? 'is-dragging' : ''}`}
              title="拖拽调整分屏宽度"
              onPointerDown={(e) => {
                if (e.button !== 0) return;
                e.preventDefault();
                e.currentTarget.setPointerCapture(e.pointerId);
                setDraggingSash(true);
              }}
              onPointerMove={(e) => {
                if (!e.currentTarget.hasPointerCapture(e.pointerId)) return;
                const rect = sessRef.current?.getBoundingClientRect();
                if (!rect) return;
                setDrawerWidth(clampDrawerWidth(rect.right - e.clientX));
              }}
              onPointerUp={(e) => {
                if (e.currentTarget.hasPointerCapture(e.pointerId)) {
                  e.currentTarget.releasePointerCapture(e.pointerId);
                }
                setDraggingSash(false);
                writeDrawerWidth(drawerWidthRef.current);
              }}
              onPointerCancel={() => setDraggingSash(false)}
              onKeyDown={(e) => {
                const delta = e.key === 'ArrowLeft' ? 24 : e.key === 'ArrowRight' ? -24 : 0;
                if (delta === 0) return;
                e.preventDefault();
                const next = clampDrawerWidth(drawerWidth + delta);
                setDrawerWidth(next);
                writeDrawerWidth(next);
              }}
            />
            <ChildSessionDrawer
              boot={boot}
              parentTaskId={taskId}
              parentProjectId={task.projectId}
              childTaskId={effectiveChildId}
              nativeMember={activeNativeMember}
              childrenList={childrenList}
              nativeMembers={nativeMembers}
              width={drawerWidth}
              onSelectChild={openChildDrawer}
              onSelectNativeMember={openNativeMemberDrawer}
              onClose={closeChildDrawer}
            />
          </>
        ) : (
          (dockMode || mobileDock) && (
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
                  worker={effectiveWorker}
                  onSelectMember={openNativeMemberDrawer}
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
                  childrenList={childrenList}
                  inboxByTask={inboxByTask}
                  onOpenBoard={() => setBoardOpen(true)}
                  onOpenChild={openChildDrawer}
                  onSelectMember={openNativeMemberDrawer}
                />
              )}
            </div>
          )
        )}
      </div>
    </main>
  );
}
