import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
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
const SUB_DRAWER_NARROW_MIN = 260;
const SUB_DRAWER_MAIN_MIN = 280;
const SUB_DRAWER_MAX = 680;
const SUB_DRAWER_DEFAULT = 440;

function measureNaturalDockHeight(cwrap: HTMLElement | null): number {
  if (!cwrap) return 0;
  const cs = getComputedStyle(cwrap);
  const padBorder =
    (parseFloat(cs.paddingTop) || 0) +
    (parseFloat(cs.paddingBottom) || 0) +
    (parseFloat(cs.borderTopWidth) || 0) +
    (parseFloat(cs.borderBottomWidth) || 0);
  let total = padBorder;
  for (const child of Array.from(cwrap.children) as HTMLElement[]) {
    if (child.classList.contains('composer')) {
      const bcs = getComputedStyle(child);
      const boxPadBorder =
        (parseFloat(bcs.paddingTop) || 0) +
        (parseFloat(bcs.paddingBottom) || 0) +
        (parseFloat(bcs.borderTopWidth) || 0) +
        (parseFloat(bcs.borderBottomWidth) || 0);
      const gap = parseFloat(bcs.rowGap) || 0;
      let kidsH = 0;
      let visCount = 0;
      for (const k of Array.from(child.children) as HTMLElement[]) {
        const kh = k.getBoundingClientRect().height;
        if (kh > 0) {
          const kcs = getComputedStyle(k);
          const marginExtra = k.classList.contains('composer-meta-bar')
            ? parseFloat(kcs.marginBottom) || 0
            : (parseFloat(kcs.marginTop) || 0) + (parseFloat(kcs.marginBottom) || 0);
          kidsH += kh + marginExtra;
          visCount += 1;
        }
      }
      total += boxPadBorder + kidsH + Math.max(0, visCount - 1) * gap;
    } else {
      const ch = child.getBoundingClientRect().height;
      if (ch > 0) {
        const ccs = getComputedStyle(child);
        total += ch + (parseFloat(ccs.marginTop) || 0) + (parseFloat(ccs.marginBottom) || 0);
      }
    }
  }
  return Math.ceil(total);
}

function hasSavedDrawerWidth(): boolean {
  try {
    const raw = localStorage.getItem(SUB_DRAWER_WIDTH_KEY);
    if (raw == null || raw.trim() === '') return false;
    const n = Number(raw);
    return Number.isFinite(n) && n >= SUB_DRAWER_NARROW_MIN;
  } catch {
    return false;
  }
}

function readDrawerWidth(): number {
  try {
    const raw = localStorage.getItem(SUB_DRAWER_WIDTH_KEY);
    if (raw == null || raw.trim() === '') return SUB_DRAWER_DEFAULT;
    const n = Number(raw);
    if (Number.isFinite(n) && n >= SUB_DRAWER_NARROW_MIN) {
      return Math.min(SUB_DRAWER_MAX, Math.max(SUB_DRAWER_MIN, Math.round(n)));
    }
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
  const [hasExplicitDrawerWidth, setHasExplicitDrawerWidth] = useState<boolean>(hasSavedDrawerWidth);
  const [stageWidth, setStageWidth] = useState<number>(0);
  const [draggingSash, setDraggingSash] = useState(false);

  const artMenuRef = useRef<HTMLDivElement | null>(null);
  const moreMenuRef = useRef<HTMLDivElement | null>(null);
  const sessRef = useRef<HTMLDivElement | null>(null);
  const [sessEl, setSessEl] = useState<HTMLDivElement | null>(null);
  const setSessNode = useCallback((node: HTMLDivElement | null) => {
    sessRef.current = node;
    setSessEl(node);
  }, []);
  const drawerWidthRef = useRef(drawerWidth);
  drawerWidthRef.current = drawerWidth;

  const parentSummary = tl?.parentSummary;
  const parentTaskId = parentSummary?.taskId || task?.parentTaskId || '';
  const isChildSession = !!parentTaskId;

  const members = tl?.boardMembers ?? [];
  const hasSwarm = !isChildSession && (members.length > 0 || (task?.workerCount ?? 0) > 0);
  const childrenQ = useTaskChildren(taskId, !!tl && !isChildSession);
  const childrenList = isChildSession ? [] : (childrenQ.data?.children ?? tl?.children ?? []);
  const boardQ = useBoard(
    taskId,
    !isChildSession && (hasSwarm || childrenList.length > 0 || (task?.childCount ?? 0) > 0 || boardOpen || !!worker),
  );
  const allMembers = isChildSession ? [] : (boardQ.data?.members ?? members);
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

  // Observe stage width so split layout at narrow desktop viewports (e.g. 920px with 220px sidebar = 700px)
  // stays cleanly side-by-side without the child drawer ever overlapping the main session.
  // Depends on `sessEl` (callback ref state) so cold-start / direct reload of `#/t/parent?child=...` attaches
  // immediately as soon as `tl` loads and `.sess` mounts.
  useLayoutEffect(() => {
    const root = sessEl;
    if (!root) return;
    const update = () => {
      const w = Math.round(root.getBoundingClientRect().width);
      if (w > 0) setStageWidth(w);
    };
    update();
    const ro = new ResizeObserver(update);
    ro.observe(root);
    return () => ro.disconnect();
  }, [sessEl, taskId, childDrawerOpen]);

  // Keep `--composer-dock-h` synchronized to the MAX natural height of BOTH main & child Composer docks
  // (accounting for multiline textarea growth, pending Review annotation rows, banners, and ref chips),
  // guaranteeing Δtop = 0px and Δheight = 0px across every state and smooth snap-back to 113px when cleared.
  useLayoutEffect(() => {
    const root = sessEl;
    if (!root || !childDrawerOpen) return;

    let rafId = 0;
    const observed = new Set<Element>();
    const ro = new ResizeObserver(() => scheduleSync());

    const attachTargets = () => {
      const selectors = [
        '.session-main-col .cwrap',
        '.session-main-col .composer',
        '.session-main-col .composer > *',
        '.sub-drawer .cwrap',
        '.sub-drawer .composer',
        '.sub-drawer .composer > *',
      ];
      for (const el of root.querySelectorAll(selectors.join(', '))) {
        if (!observed.has(el)) {
          observed.add(el);
          ro.observe(el);
        }
      }
    };

    const sync = () => {
      attachTargets();
      const mainDock = root.querySelector('.session-main-col .cwrap') as HTMLElement | null;
      const childDock = root.querySelector('.sub-drawer .cwrap') as HTMLElement | null;
      const mainH = measureNaturalDockHeight(mainDock);
      const childH = measureNaturalDockHeight(childDock);
      const nextVal = `${Math.max(113, mainH, childH)}px`;
      if (root.style.getPropertyValue('--composer-dock-h') !== nextVal) {
        root.style.setProperty('--composer-dock-h', nextVal);
      }
    };

    const scheduleSync = () => {
      if (rafId) cancelAnimationFrame(rafId);
      rafId = requestAnimationFrame(() => {
        rafId = 0;
        sync();
      });
    };

    sync();
    scheduleSync();
    const mo = new MutationObserver(() => {
      attachTargets();
      scheduleSync();
    });
    mo.observe(root, { childList: true, subtree: true, attributes: true, attributeFilter: ['style', 'class'] });

    return () => {
      if (rafId) cancelAnimationFrame(rafId);
      mo.disconnect();
      ro.disconnect();
      root.style.removeProperty('--composer-dock-h');
    };
  }, [sessEl, childDrawerOpen, taskId, effectiveChildId, activeNativeMember?.memberName]);

  useEffect(() => {
    if (!artMenuOpen && !moreMenuOpen) return;
    const closeAll = () => {
      setArtMenuOpen(false);
      setMoreMenuOpen(false);
      setConfirmDeleteMenu(false);
    };
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
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !e.isComposing) {
        if (useWorkbenchStore.getState().cmdkOpen) return;
        e.preventDefault();
        e.stopImmediatePropagation();
        closeAll();
      }
    };
    window.addEventListener('mousedown', onDown);
    window.addEventListener('keydown', onKey, true);
    window.addEventListener('orbit:dismiss-popovers', closeAll);
    return () => {
      window.removeEventListener('mousedown', onDown);
      window.removeEventListener('keydown', onKey, true);
      window.removeEventListener('orbit:dismiss-popovers', closeAll);
    };
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
    (!isChildSession && (childrenList.length > 0 || hasSwarm || (boardQ.data?.messages?.length ?? 0) > 0)) ||
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

  const clampDrawerWidth = (rawW: number, curStageW = stageWidth || (sessRef.current?.getBoundingClientRect().width ?? 0)) => {
    const maxAllowed =
      curStageW > 0
        ? Math.min(SUB_DRAWER_MAX, Math.max(SUB_DRAWER_NARROW_MIN, curStageW - SUB_DRAWER_MAIN_MIN))
        : SUB_DRAWER_MAX;
    const minDrawer = Math.min(SUB_DRAWER_MIN, maxAllowed);
    return Math.round(Math.min(maxAllowed, Math.max(minDrawer, rawW)));
  };

  // Keep v4 340px floor & 440px default on wide desktop; on narrow desktop (e.g. 920px => 700px stage)
  // default balances ~350px/345px before explicit drag, while explicit drag reaches Math.min(680, stageWidth - 280) (420px at 920px).
  const effectiveDrawerWidth =
    !hasExplicitDrawerWidth && stageWidth > 0 && stageWidth < 900
      ? clampDrawerWidth(Math.min(drawerWidth, Math.max(SUB_DRAWER_MIN, Math.floor(stageWidth * 0.5))), stageWidth)
      : clampDrawerWidth(drawerWidth, stageWidth);

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

      <div ref={setSessNode} className={`sess ${childDrawerOpen ? 'has-child-drawer' : ''}`}>
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
              aria-valuenow={effectiveDrawerWidth}
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
                const next = clampDrawerWidth(rect.right - e.clientX, rect.width);
                setHasExplicitDrawerWidth(true);
                setDrawerWidth(next);
                drawerWidthRef.current = next;
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
                const next = clampDrawerWidth(effectiveDrawerWidth + delta);
                setHasExplicitDrawerWidth(true);
                setDrawerWidth(next);
                drawerWidthRef.current = next;
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
              width={effectiveDrawerWidth}
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
                  childrenList={childrenList}
                  machines={boot.machines}
                  inboxByTask={inboxByTask}
                  worker={effectiveWorker}
                  onOpenChild={openChildDrawer}
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
