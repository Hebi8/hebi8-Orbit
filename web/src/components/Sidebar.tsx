import { useEffect, useMemo, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Archive, ChevronRight, Folder, Layers, LayoutGrid, PanelLeftClose, Plus, Search, Settings } from 'lucide-react';
import type {
  BootstrapResponse,
  ChildTaskView,
  InboxItemView,
  MachineView,
  ProjectView,
  TaskSummary,
} from '../generated/protocol';
import { useManyProjectTasks, useTaskChildren, useTimeline } from '../hooks/queries';
import { useHubLatency } from '../hooks/useHubLatency';
import { api, errMsg } from '../lib/api';
import {
  SCRATCH_PROJECT_ID,
  childSessionMark,
  cleanMachineName,
  indexInboxByTask,
  isVirtualProject,
  rootSessionMark,
  sessionMark,
  sessionMarkHint,
  taskTitle,
  type SessionMark,
} from '../lib/format';
import { resolveEffectiveLocalMachineId } from '../lib/ide';
import { navigate, paths, type Route } from '../lib/router';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';
import { CaretIcon, DirectLocalIcon, PAIcon } from './Icons';
import { StatusMark } from './StatusMark';

const MAX_THREADS = 8;

function mainThreads(tasks: TaskSummary[]): TaskSummary[] {
  return tasks.filter((t) => !t.parentTaskId).sort((a, b) => b.updatedAt - a.updatedAt);
}

function SidebarChildTree({
  parentTaskId,
  localChildren,
  machines,
  inboxByTask,
  activeTaskId,
  activeChildDrawerId,
}: {
  parentTaskId: string;
  localChildren: TaskSummary[];
  machines: MachineView[];
  inboxByTask: ReadonlyMap<string, InboxItemView[]>;
  activeTaskId: string;
  activeChildDrawerId: string;
}) {
  const q = useTaskChildren(parentTaskId, true);
  const items = useMemo(() => {
    const fetched = q.data?.children ?? [];
    if (fetched.length > 0) return fetched;
    return localChildren.map(
      (t): ChildTaskView => ({
        taskId: t.taskId,
        projectId: t.projectId,
        parentTaskId,
        delegatedByAttemptId: t.delegatedByAttemptId,
        delegatedRole: t.delegatedRole,
        title: t.title,
        goal: t.goal,
        state: t.state,
        preferredAdapter: t.preferredAdapter,
        activeAttemptId: t.activeAttemptId,
        latestDeliveryId: t.latestDeliveryId,
        deliveryCount: t.deliveryCount,
        attemptCount: 0,
        openApprovalCount: t.state === 'waiting_approval' ? 1 : 0,
        createdAt: t.createdAt,
        updatedAt: t.updatedAt,
      }),
    );
  }, [q.data?.children, localChildren, parentTaskId]);

  if (q.isLoading && items.length === 0) {
    return <div className="th-child-empty">载入子会话…</div>;
  }
  if (items.length === 0) return null;

  return (
    <div className="th-children" role="group" aria-label="子会话列表">
      {items.map((child) => {
        const mark = childSessionMark(child, inboxByTask.get(child.taskId));
        const hint = sessionMarkHint(mark);
        const isCurrent = activeTaskId === child.taskId || activeChildDrawerId === child.taskId;
        const mach = machines.find((m) => m.machineId === child.machineId);
        const machName = mach ? mach.displayAlias?.trim() || cleanMachineName(mach) : child.machineId || '';
        const tooltip = [taskTitle(child), hint, machName, child.projectName].filter(Boolean).join(' · ');

        return (
          <button
            type="button"
            key={child.taskId}
            className={`th-child ${isCurrent ? 'on' : ''} ${!mark ? 'resting' : ''}`}
            onClick={() => navigate(paths.session(child.taskId))}
            title={tooltip}
          >
            <span className="tt">{taskTitle(child)}</span>
            <StatusMark mark={mark} />
          </button>
        );
      })}
    </div>
  );
}

function SidebarThreadItem({
  task,
  ownMark,
  aggregatedMark,
  active,
  childCount,
  childrenExpanded,
  hasActiveChild,
  onToggleChildren,
  localChildren,
  machines,
  inboxByTask,
  activeTaskId,
  activeChildDrawerId,
  archiveArmed,
  onArmArchive,
  onConfirmArchive,
}: {
  task: TaskSummary;
  ownMark: SessionMark;
  aggregatedMark: SessionMark;
  active: boolean;
  childCount: number;
  childrenExpanded: boolean;
  hasActiveChild: boolean;
  onToggleChildren: () => void;
  localChildren: TaskSummary[];
  machines: MachineView[];
  inboxByTask: ReadonlyMap<string, InboxItemView[]>;
  activeTaskId: string;
  activeChildDrawerId: string;
  archiveArmed: boolean;
  onArmArchive: () => void;
  onConfirmArchive: () => void;
}) {
  const hasChildren = childCount > 0;
  // Conditional status bleed-through (v4 invariant):
  // Collapsed -> rollup children onto parent row; Expanded -> parent shows its own status only.
  const displayedMark: SessionMark = hasChildren && childrenExpanded ? ownMark : aggregatedMark;
  const hint = sessionMarkHint(displayedMark);

  return (
    <div
      className={`th-node ${hasChildren && childrenExpanded ? 'is-open' : ''} ${hasActiveChild ? 'has-active-child' : ''}`}
    >
      <div className={`th-wrap ${archiveArmed ? 'is-arm-archive' : ''}`}>
        {hasChildren && (
          <button
            type="button"
            className="th-caret-btn"
            title={childrenExpanded ? '收起子会话' : '展开子会话'}
            aria-label={childrenExpanded ? '收起子会话' : '展开子会话'}
            aria-expanded={childrenExpanded}
            onClick={(e) => {
              e.stopPropagation();
              onToggleChildren();
            }}
          >
            <ChevronRight size={11} strokeWidth={1.8} />
          </button>
        )}
        <button
          type="button"
          className={`th ${active ? 'on' : ''} ${!displayedMark ? 'resting' : ''}`}
          onClick={() => navigate(paths.session(task.taskId))}
          title={hint ? `${taskTitle(task)} · ${hint}` : taskTitle(task)}
        >
          <span className="tt">{taskTitle(task)}</span>
          <StatusMark mark={displayedMark} />
        </button>
        {archiveArmed ? (
          <button
            type="button"
            className="th-arch-btn is-armed"
            data-arm-archive-root="1"
            title="再次点击确认归档"
            aria-label="确认归档会话"
            onClick={(e) => {
              e.stopPropagation();
              onConfirmArchive();
            }}
          >
            <Archive size={11} strokeWidth={1.8} />
            <span>确认</span>
          </button>
        ) : (
          <button
            type="button"
            className="th-arch-btn"
            data-arm-archive-root="1"
            title="归档会话"
            aria-label="归档会话"
            onClick={(e) => {
              e.stopPropagation();
              onArmArchive();
            }}
          >
            <Archive size={11.5} strokeWidth={1.7} />
          </button>
        )}
      </div>
      {hasChildren && childrenExpanded && (
        <SidebarChildTree
          parentTaskId={task.taskId}
          localChildren={localChildren}
          machines={machines}
          inboxByTask={inboxByTask}
          activeTaskId={activeTaskId}
          activeChildDrawerId={activeChildDrawerId}
        />
      )}
    </div>
  );
}

function WorkspaceFooterButton({
  workspaceName,
  boot,
  open,
  onClick,
}: {
  workspaceName: string;
  boot: BootstrapResponse;
  open: boolean;
  onClick: () => void;
}) {
  const wsStatus = useWorkbenchStore((s) => s.wsStatus);
  const storedLocalId = useWorkbenchStore((s) => s.localMachineId);
  const latency = useHubLatency();

  const machines = boot.machines;
  const anyOnline = machines.length === 0 || machines.some((m) => m.status === 'online');
  const unreachable = latency === null || !anyOnline;

  const primaryDutyId = boot.setup?.dutyPriority?.[0] ?? '';
  const activeDutyId = boot.setup?.activeDutyMachineId ?? '';
  const primaryMachine = primaryDutyId ? machines.find((m) => m.machineId === primaryDutyId) : undefined;
  const isFailover =
    !!primaryDutyId &&
    !!activeDutyId &&
    primaryDutyId !== activeDutyId &&
    (!primaryMachine || primaryMachine.status !== 'online');

  const effectiveLocalId = resolveEffectiveLocalMachineId(
    machines,
    boot.setup?.currentMachineId || storedLocalId,
  );
  const hubMachine = machines.find((m) => m.transportMode === 'local_ws');
  const isLoopbackHost =
    typeof window !== 'undefined' &&
    (window.location.hostname === '127.0.0.1' ||
      window.location.hostname === 'localhost' ||
      window.location.hostname === '::1');
  const isDirectLocal =
    !unreachable &&
    wsStatus !== 'reconnecting' &&
    (isLoopbackHost || (!!effectiveLocalId && !!hubMachine && effectiveLocalId === hubMachine.machineId));

  const degraded =
    !unreachable &&
    (isFailover || wsStatus === 'reconnecting' || (typeof latency === 'number' && latency >= 80));

  const statusText = unreachable
    ? '离线'
    : wsStatus === 'reconnecting'
      ? '重连中'
      : isDirectLocal
        ? '直连本机'
        : typeof latency === 'number'
          ? `${latency}ms`
          : '';

  const dotClass = unreachable ? 'off' : degraded ? 'mid' : 'good';
  const latClass = unreachable ? 'off' : degraded ? 'mid' : 'good';

  return (
    <button
      type="button"
      className={`host-row ${open ? 'on' : ''}`}
      title={statusText ? `${workspaceName} · ${statusText}` : workspaceName}
      aria-label={statusText ? `${workspaceName} · ${statusText}` : workspaceName}
      onClick={onClick}
    >
      <span className={`ws-dot ${dotClass}`} aria-hidden="true" />
      <span className="hn ell">{workspaceName}</span>
      {isDirectLocal ? (
        <span
          className={`ws-direct-ico ${isFailover ? 'is-warn' : 'is-local'}`}
          title="直连本机"
          aria-label="直连本机"
        >
          <DirectLocalIcon size={13} />
        </span>
      ) : (
        statusText && <span className={`hl mono ${latClass}`}>{statusText}</span>
      )}
    </button>
  );
}

export function Sidebar({
  boot,
  route,
  onAddProject,
}: {
  boot: BootstrapResponse;
  route: Route;
  onAddProject?: () => void;
}) {
  const qc = useQueryClient();
  const [armArchiveTaskId, setArmArchiveTaskId] = useState<string | null>(null);
  const expanded = useWorkbenchStore((s) => s.expandedProjects);
  const setExpanded = useWorkbenchStore((s) => s.setProjectExpanded);
  const expandedParents = useWorkbenchStore((s) => s.expandedParents);
  const toggleParentExpanded = useWorkbenchStore((s) => s.toggleParentExpanded);
  const ensureParentExpanded = useWorkbenchStore((s) => s.ensureParentExpanded);
  const toggleSidebar = useWorkbenchStore((s) => s.toggleSidebar);
  const machinesOpen = useWorkbenchStore((s) => s.machinesOpen);
  const setMachinesOpen = useWorkbenchStore((s) => s.setMachinesOpen);
  const setCmdkOpen = useWorkbenchStore((s) => s.setCmdkOpen);
  const workspaceNameOverride = useWorkbenchStore((s) => s.workspaceNameOverride);

  // Reset in-place 2-step archive confirmation when clicking outside or pressing Escape.
  useEffect(() => {
    if (!armArchiveTaskId) return;
    const onDown = (e: MouseEvent) => {
      const el = e.target as HTMLElement | null;
      if (el?.closest('[data-arm-archive-root="1"]')) return;
      setArmArchiveTaskId(null);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setArmArchiveTaskId(null);
    };
    document.addEventListener('mousedown', onDown);
    window.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('mousedown', onDown);
      window.removeEventListener('keydown', onKey);
    };
  }, [armArchiveTaskId]);

  const workspaceName = workspaceNameOverride || boot.workspaceName || 'Home LAN';

  const projects = boot.projects.filter((p) => !isVirtualProject(p.projectId) && !p.archived);
  const scratch = boot.projects.find((p) => p.projectId === SCRATCH_PROJECT_ID);

  const isOpen = (p: ProjectView) =>
    expanded[p.projectId] ??
    (p.activeTaskCount + p.blockedTaskCount > 0 || (route.name === 'project' && route.projectId === p.projectId));

  const fetchList = [...projects.filter(isOpen), ...(scratch ? [scratch] : [])];
  const tasksByProject = useManyProjectTasks(fetchList);
  const scratchThreads = scratch ? mainThreads(tasksByProject[scratch.projectId] ?? []) : [];
  const inboxByTask = useMemo(() => indexInboxByTask(boot.openInboxItems), [boot.openInboxItems]);
  const activeTaskId = route.name === 'session' ? route.taskId : '';
  const activeChildDrawerId = route.name === 'session' || route.name === 'pa' ? route.childId ?? '' : '';

  // Deduplicated merged task index + local children map across boot.activeTasks + loaded project lists.
  const { allKnownTasks, localChildrenByParent } = useMemo(() => {
    const byId = new Map<string, TaskSummary>();
    for (const t of boot.activeTasks) byId.set(t.taskId, t);
    for (const list of Object.values(tasksByProject)) {
      for (const t of list) {
        if (!byId.has(t.taskId)) byId.set(t.taskId, t);
      }
    }
    const known = [...byId.values()];
    const map = new Map<string, TaskSummary[]>();
    for (const t of known) {
      if (!t.parentTaskId) continue;
      const list = map.get(t.parentTaskId) ?? [];
      list.push(t);
      map.set(t.parentTaskId, list);
    }
    for (const list of map.values()) {
      list.sort((a, b) => b.updatedAt - a.updatedAt);
    }
    return { allKnownTasks: known, localChildrenByParent: map };
  }, [boot.activeTasks, tasksByProject]);

  // Expand stability invariant (v4):
  // Opening a full child-session page auto-expands its parent branch (persisted in store);
  // switching to any other session NEVER auto-collapses an expanded branch.
  const activeSessionTl = useTimeline(route.name === 'session' ? route.taskId : undefined);
  const localViewingParentId = activeTaskId
    ? allKnownTasks.find((t) => t.taskId === activeTaskId)?.parentTaskId ?? ''
    : '';
  const viewingParentId =
    localViewingParentId ||
    activeSessionTl.data?.task?.parentTaskId ||
    activeSessionTl.data?.parentSummary?.taskId ||
    '';

  useEffect(() => {
    if (!viewingParentId) return;
    ensureParentExpanded(viewingParentId);
  }, [viewingParentId, ensureParentExpanded]);

  const childStatsFor = (t: TaskSummary) => {
    const localKids = localChildrenByParent.get(t.taskId) ?? [];
    const childCount = Math.max(t.childCount ?? 0, localKids.length);
    return { localKids, childCount };
  };

  const invalidateTasks = (projectId: string) => {
    void qc.invalidateQueries({ queryKey: ['project-tasks', projectId] });
    void qc.invalidateQueries({ queryKey: ['all-tasks'] });
    void qc.invalidateQueries({ queryKey: ['archived-tasks'] });
    void qc.invalidateQueries({ queryKey: ['global-artifacts'] });
    void qc.invalidateQueries({ queryKey: ['bootstrap'] });
  };

  const handleArchiveTask = async (t: TaskSummary) => {
    setArmArchiveTaskId(null);
    try {
      await api.archiveTask(t.taskId);
      invalidateTasks(t.projectId);
      if (activeTaskId === t.taskId) {
        navigate(isVirtualProject(t.projectId) ? paths.home() : paths.project(t.projectId));
      }
      toast(`已归档「${taskTitle(t)}」`, 'info', {
        label: '撤销',
        onClick: () => {
          void api
            .unarchiveTask(t.taskId)
            .then(() => {
              invalidateTasks(t.projectId);
              toast(`已恢复「${taskTitle(t)}」`);
            })
            .catch((e) => toast(errMsg(e), 'err'));
        },
      });
    } catch (e) {
      toast(errMsg(e), 'err');
    }
  };

  return (
    <aside className="side">
      <div className="side-in">
        <div className="brand">
          <span className="brand-mark" />
          <b>Orbit</b>
          <div className="brand-acts">
            <button type="button" className="ib" title="快速跳转 (⌘K)" aria-label="搜索" onClick={() => setCmdkOpen(true)}>
              <Search size={13.5} strokeWidth={1.6} />
            </button>
            <button type="button" className="ib" title="收起侧栏 (⌘\)" aria-label="收起侧栏" onClick={toggleSidebar}>
              <PanelLeftClose size={14} strokeWidth={1.6} />
            </button>
          </div>
        </div>
        <div className="quick" role="navigation" aria-label="主导航">
          <button type="button" className={`qi ${route.name === 'home' ? 'on' : ''}`} onClick={() => navigate(paths.home())}>
            <LayoutGrid size={14} strokeWidth={1.6} />
            总览
          </button>
          <button type="button" className={`qi ${route.name === 'pa' ? 'on' : ''}`} onClick={() => navigate(paths.hebi8())}>
            <PAIcon />
            <span>Hebi8</span>
          </button>
          <button type="button" className={`qi ${route.name === 'artifacts' ? 'on' : ''}`} onClick={() => navigate(paths.artifacts())}>
            <Layers size={14} strokeWidth={1.6} />
            Artifacts
          </button>
          <button type="button" className={`qi ${route.name === 'new' ? 'on' : ''}`} onClick={() => navigate(paths.newThread())}>
            <Plus size={14} strokeWidth={1.6} />
            新建线程
            <span className="kbd">⌘N</span>
          </button>
        </div>
        <nav className="projects" aria-label="项目">
          {projects.length === 0 && <div className="quiet" style={{ padding: '4px 8px' }}>还没有项目</div>}
          {projects.map((p) => {
            const open = isOpen(p);
            const allProjTasks = tasksByProject[p.projectId] ?? [];
            const threads = open ? mainThreads(allProjTasks) : [];
            const onProject = route.name === 'project' && route.projectId === p.projectId;
            return (
              <div className="pgroup" key={p.projectId}>
                <div className={`ph ${onProject ? 'on' : ''}`}>
                  <button
                    type="button"
                    className={`caretb ${open ? 'open' : ''}`}
                    title={open ? '收起' : '展开主线程'}
                    aria-label={open ? '收起项目' : '展开项目'}
                    onClick={() => setExpanded(p.projectId, !open)}
                  >
                    <CaretIcon />
                  </button>
                  <button type="button" className="pname" onClick={() => navigate(paths.project(p.projectId))}>
                    <Folder size={12.5} strokeWidth={1.7} className="ph-folder-ico" aria-hidden="true" />
                    <span className="pn ell">{p.displayName}</span>
                  </button>
                  <button
                    type="button"
                    className="padd"
                    title={`${p.displayName} 设置`}
                    aria-label={`${p.displayName} 设置`}
                    onClick={() => navigate(paths.project(p.projectId, 'settings'))}
                  >
                    <Settings size={12} strokeWidth={1.6} />
                  </button>
                  <button
                    type="button"
                    className="padd"
                    title={`在 ${p.displayName} 新建线程`}
                    aria-label={`在 ${p.displayName} 新建线程`}
                    onClick={() => navigate(paths.newThread('project', p.projectId))}
                  >
                    <Plus size={13} strokeWidth={1.6} />
                  </button>
                </div>
                {open && threads.length > 0 && (
                  <div className="proj-threads">
                    {threads.slice(0, MAX_THREADS).map((t) => {
                      const ownMark = sessionMark(t, inboxByTask.get(t.taskId));
                      const aggregatedMark = rootSessionMark(t, allKnownTasks, inboxByTask);
                      const cs = childStatsFor(t);
                      const isParentOfViewingChild =
                        viewingParentId === t.taskId || (activeTaskId === t.taskId && !!activeChildDrawerId);
                      return (
                        <SidebarThreadItem
                          key={t.taskId}
                          task={t}
                          ownMark={ownMark}
                          aggregatedMark={aggregatedMark}
                          active={activeTaskId === t.taskId}
                          childCount={cs.childCount}
                          childrenExpanded={!!expandedParents[t.taskId]}
                          hasActiveChild={isParentOfViewingChild}
                          onToggleChildren={() => toggleParentExpanded(t.taskId)}
                          localChildren={cs.localKids}
                          machines={boot.machines}
                          inboxByTask={inboxByTask}
                          activeTaskId={activeTaskId}
                          activeChildDrawerId={activeChildDrawerId}
                          archiveArmed={armArchiveTaskId === t.taskId}
                          onArmArchive={() => setArmArchiveTaskId(t.taskId)}
                          onConfirmArchive={() => void handleArchiveTask(t)}
                        />
                      );
                    })}
                    {threads.length > MAX_THREADS && (
                      <button type="button" className="th-more" onClick={() => navigate(paths.project(p.projectId))}>
                        全部线程 →
                      </button>
                    )}
                  </div>
                )}
              </div>
            );
          })}
          {scratchThreads.length > 0 && (
            <div className="pgroup">
              <div className="slabel">自由对话</div>
              <div className="proj-threads">
                {scratchThreads.slice(0, MAX_THREADS).map((t) => {
                  const ownMark = sessionMark(t, inboxByTask.get(t.taskId));
                  const aggregatedMark = rootSessionMark(t, allKnownTasks, inboxByTask);
                  const cs = childStatsFor(t);
                  const isParentOfViewingChild =
                    viewingParentId === t.taskId || (activeTaskId === t.taskId && !!activeChildDrawerId);
                  return (
                    <SidebarThreadItem
                      key={t.taskId}
                      task={t}
                      ownMark={ownMark}
                      aggregatedMark={aggregatedMark}
                      active={activeTaskId === t.taskId}
                      childCount={cs.childCount}
                      childrenExpanded={!!expandedParents[t.taskId]}
                      hasActiveChild={isParentOfViewingChild}
                      onToggleChildren={() => toggleParentExpanded(t.taskId)}
                      localChildren={cs.localKids}
                      machines={boot.machines}
                      inboxByTask={inboxByTask}
                      activeTaskId={activeTaskId}
                      activeChildDrawerId={activeChildDrawerId}
                      archiveArmed={armArchiveTaskId === t.taskId}
                      onArmArchive={() => setArmArchiveTaskId(t.taskId)}
                      onConfirmArchive={() => void handleArchiveTask(t)}
                    />
                  );
                })}
              </div>
            </div>
          )}
          {onAddProject && (
            <button type="button" className="side-add-proj" onClick={onAddProject}>
              <Plus size={12.5} strokeWidth={1.7} />
              <span>添加项目</span>
            </button>
          )}
        </nav>
        <div className="sfoot">
          <WorkspaceFooterButton
            workspaceName={workspaceName}
            boot={boot}
            open={machinesOpen}
            onClick={() => setMachinesOpen(!machinesOpen)}
          />
          <button
            type="button"
            className={`ib ${route.name === 'archived' ? 'on' : ''}`}
            title="已归档会话"
            aria-label="已归档会话"
            onClick={() => navigate(paths.archived())}
          >
            <Archive size={14} strokeWidth={1.6} />
          </button>
        </div>
      </div>
    </aside>
  );
}
