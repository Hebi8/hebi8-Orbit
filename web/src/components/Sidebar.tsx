import { useEffect, useMemo, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Archive, CornerDownRight, Layers, LayoutGrid, MoreHorizontal, PanelLeftClose, Plus, Search, Settings, Trash2 } from 'lucide-react';
import type { BootstrapResponse, ChildTaskView, MachineView, ProjectView, TaskSummary } from '../generated/protocol';
import { useManyProjectTasks, useTaskChildren, useTimeline } from '../hooks/queries';
import { useHubLatency } from '../hooks/useHubLatency';
import { api, errMsg } from '../lib/api';
import {
  PA_PROJECT_ID,
  SCRATCH_PROJECT_ID,
  childStateDot,
  childStateLabel,
  cleanMachineName,
  indexInboxByTask,
  isChildStateLive,
  isVirtualProject,
  rootSessionMark,
  sessionMark,
  sessionMarkHint,
  taskTitle,
  type SessionMark,
} from '../lib/format';
import { navigate, paths, type Route } from '../lib/router';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';
import { CaretIcon, MoonIcon, PAIcon, SunIcon } from './Icons';
import { StatusMark } from './StatusMark';

const MAX_THREADS = 8;

function mainThreads(tasks: TaskSummary[]): TaskSummary[] {
  return tasks.filter((t) => !t.parentTaskId).sort((a, b) => b.updatedAt - a.updatedAt);
}

function SidebarChildTree({
  parentTaskId,
  localChildren,
  machines,
  activeTaskId,
  activeChildDrawerId,
}: {
  parentTaskId: string;
  localChildren: TaskSummary[];
  machines: MachineView[];
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
        const dot = childStateDot(child.state, child.openApprovalCount);
        const isCurrent = activeTaskId === child.taskId || activeChildDrawerId === child.taskId;
        const mach = machines.find((m) => m.machineId === child.machineId);
        const machName = mach ? mach.displayAlias?.trim() || cleanMachineName(mach) : child.machineId || '';
        const tooltip = [
          child.delegatedRole ? `[${child.delegatedRole}]` : '',
          taskTitle(child),
          childStateLabel(child.state, child.openApprovalCount),
          machName,
          child.projectName,
        ]
          .filter(Boolean)
          .join(' · ');

        return (
          <button
            type="button"
            key={child.taskId}
            className={`th-child ${isCurrent ? 'on' : ''}`}
            onClick={() => navigate(paths.session(child.taskId))}
            title={tooltip}
          >
            <span className={`sub-dot is-${dot}`} aria-hidden="true" />
            {child.delegatedRole && <span className="th-child-role">{child.delegatedRole}</span>}
            <span className="tt">{taskTitle(child)}</span>
            {child.openApprovalCount > 0 && <span className="th-child-ap" title="有待确认审批" />}
          </button>
        );
      })}
    </div>
  );
}

function SidebarThreadItem({
  task,
  mark,
  active,
  flat,
  childCount,
  activeChildCount,
  pendingChildApprovals,
  childrenExpanded,
  onToggleChildren,
  localChildren,
  machines,
  activeTaskId,
  activeChildDrawerId,
  menuOpen,
  onToggleMenu,
  onCloseMenu,
  onArchive,
  onDelete,
}: {
  task: TaskSummary;
  mark: SessionMark;
  active: boolean;
  flat?: boolean;
  childCount: number;
  activeChildCount: number;
  pendingChildApprovals: number;
  childrenExpanded: boolean;
  onToggleChildren: () => void;
  localChildren: TaskSummary[];
  machines: MachineView[];
  activeTaskId: string;
  activeChildDrawerId: string;
  menuOpen: boolean;
  onToggleMenu: () => void;
  onCloseMenu: () => void;
  onArchive: () => void;
  onDelete: () => void;
}) {
  const [confirmDelete, setConfirmDelete] = useState(false);
  const hint = sessionMarkHint(mark);
  const hasChildren = childCount > 0;

  return (
    <div className="th-node">
      <div className={`th-wrap ${menuOpen ? 'menu-open' : ''}`}>
        <div
          role="button"
          tabIndex={0}
          className={`th ${flat ? 'flat' : ''} ${active ? 'on' : ''} ${!mark ? 'resting' : ''}`}
          style={{ cursor: 'pointer' }}
          onClick={() => navigate(paths.session(task.taskId))}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ' ') {
              e.preventDefault();
              navigate(paths.session(task.taskId));
            }
          }}
          title={hint ? `${taskTitle(task)} · ${hint}` : taskTitle(task)}
        >
          <span className="tt">{taskTitle(task)}</span>
          {hasChildren && (
            <button
              type="button"
              className={`th-child-toggle ${childrenExpanded ? 'open' : ''} ${activeChildCount > 0 ? 'has-live' : ''} ${pendingChildApprovals > 0 ? 'has-ap' : ''}`}
              title={`${childrenExpanded ? '收起' : '展开'} ${childCount} 个子会话${activeChildCount > 0 ? ` · ${activeChildCount} 运行中` : ''}${pendingChildApprovals > 0 ? ` · ${pendingChildApprovals} 待审批` : ''}`}
              aria-expanded={childrenExpanded}
              onClick={(e) => {
                e.stopPropagation();
                onToggleChildren();
              }}
            >
              <CornerDownRight size={10} strokeWidth={2} />
              <span className="mono">{childCount}</span>
              {pendingChildApprovals > 0 && <span className="th-child-ap" aria-hidden="true" />}
            </button>
          )}
          <StatusMark mark={mark} />
        </div>
        <button
          type="button"
          className="th-more-btn"
          title="会话操作"
          aria-label="会话操作"
          aria-expanded={menuOpen}
          onClick={(e) => {
            e.stopPropagation();
            setConfirmDelete(false);
            onToggleMenu();
          }}
        >
          <MoreHorizontal size={13} strokeWidth={1.7} />
        </button>
        {menuOpen && (
          <>
            <div
              className="scrim"
              style={{ background: 'transparent', backdropFilter: 'none', zIndex: 49 }}
              onClick={() => {
                setConfirmDelete(false);
                onCloseMenu();
              }}
            />
            <div className="th-menu" role="menu">
              <button
                type="button"
                role="menuitem"
                className="th-menu-item"
                onClick={() => {
                  setConfirmDelete(false);
                  onCloseMenu();
                  onArchive();
                }}
              >
                <Archive size={12.5} strokeWidth={1.6} />
                <span>归档会话</span>
              </button>
              {confirmDelete ? (
                <button
                  type="button"
                  role="menuitem"
                  className="th-menu-item danger"
                  onClick={() => {
                    setConfirmDelete(false);
                    onCloseMenu();
                    onDelete();
                  }}
                >
                  <Trash2 size={12.5} strokeWidth={1.7} />
                  <span>确认永久删除</span>
                </button>
              ) : (
                <button
                  type="button"
                  role="menuitem"
                  className="th-menu-item danger"
                  onClick={() => setConfirmDelete(true)}
                >
                  <Trash2 size={12.5} strokeWidth={1.6} />
                  <span>永久删除</span>
                </button>
              )}
            </div>
          </>
        )}
      </div>
      {hasChildren && childrenExpanded && (
        <SidebarChildTree
          parentTaskId={task.taskId}
          localChildren={localChildren}
          machines={machines}
          activeTaskId={activeTaskId}
          activeChildDrawerId={activeChildDrawerId}
        />
      )}
    </div>
  );
}

function WorkspaceFooterButton({
  workspaceName,
  machines,
  open,
  onClick,
}: {
  workspaceName: string;
  machines: MachineView[];
  open: boolean;
  onClick: () => void;
}) {
  const wsStatus = useWorkbenchStore((s) => s.wsStatus);
  const latency = useHubLatency();

  const anyOnline = machines.length === 0 || machines.some((m) => m.status === 'online');
  const unreachable = latency === null || !anyOnline;
  const degraded = !unreachable && (wsStatus === 'reconnecting' || (typeof latency === 'number' && latency >= 80));

  const statusText = unreachable
    ? '离线'
    : wsStatus === 'reconnecting'
      ? '重连中'
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
      {statusText && <span className={`hl mono ${latClass}`}>{statusText}</span>}
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
  const [openMenuTaskId, setOpenMenuTaskId] = useState<string | null>(null);
  const [expandedParents, setExpandedParents] = useState<Record<string, boolean>>({});
  const expanded = useWorkbenchStore((s) => s.expandedProjects);
  const setExpanded = useWorkbenchStore((s) => s.setProjectExpanded);
  const toggleSidebar = useWorkbenchStore((s) => s.toggleSidebar);
  const theme = useWorkbenchStore((s) => s.theme);
  const toggleTheme = useWorkbenchStore((s) => s.toggleTheme);
  const machinesOpen = useWorkbenchStore((s) => s.machinesOpen);
  const setMachinesOpen = useWorkbenchStore((s) => s.setMachinesOpen);
  const setCmdkOpen = useWorkbenchStore((s) => s.setCmdkOpen);
  const workspaceNameOverride = useWorkbenchStore((s) => s.workspaceNameOverride);

  const workspaceName = workspaceNameOverride || boot.workspaceName || 'Home LAN';

  const projects = boot.projects.filter((p) => !isVirtualProject(p.projectId) && !p.archived);
  const scratch = boot.projects.find((p) => p.projectId === SCRATCH_PROJECT_ID);

  const isOpen = (p: ProjectView) =>
    expanded[p.projectId] ?? (p.activeTaskCount + p.blockedTaskCount > 0 || (route.name === 'project' && route.projectId === p.projectId));

  const fetchList = [...projects.filter(isOpen), ...(scratch ? [scratch] : [])];
  const tasksByProject = useManyProjectTasks(fetchList);
  const scratchThreads = scratch ? mainThreads(tasksByProject[scratch.projectId] ?? []) : [];
  const inboxByTask = useMemo(() => indexInboxByTask(boot.openInboxItems), [boot.openInboxItems]);
  const activeTaskId = route.name === 'session' ? route.taskId : '';
  const activeChildDrawerId = route.name === 'session' || route.name === 'pa' ? route.childId ?? '' : '';

  // Deduplicated local children map across boot.activeTasks + loaded project lists.
  const localChildrenByParent = useMemo(() => {
    const map = new Map<string, Map<string, TaskSummary>>();
    const ingest = (list: readonly TaskSummary[]) => {
      for (const t of list) {
        if (!t.parentTaskId) continue;
        let bucket = map.get(t.parentTaskId);
        if (!bucket) {
          bucket = new Map<string, TaskSummary>();
          map.set(t.parentTaskId, bucket);
        }
        bucket.set(t.taskId, t);
      }
    };
    ingest(boot.activeTasks);
    for (const list of Object.values(tasksByProject)) ingest(list);
    const out = new Map<string, TaskSummary[]>();
    for (const [pid, bucket] of map) {
      out.set(pid, [...bucket.values()].sort((a, b) => b.updatedAt - a.updatedAt));
    }
    return out;
  }, [boot.activeTasks, tasksByProject]);

  // Auto-expand parent thread when currently viewing a child session (reuses SessionPage's timeline query).
  const activeSessionTl = useTimeline(route.name === 'session' ? route.taskId : undefined);
  const viewingParentId = activeSessionTl.data?.task?.parentTaskId || activeSessionTl.data?.parentSummary?.taskId || '';
  useEffect(() => {
    if (!viewingParentId) return;
    setExpandedParents((prev) => (prev[viewingParentId] ? prev : { ...prev, [viewingParentId]: true }));
  }, [viewingParentId]);

  const toggleParentExpanded = (parentTaskId: string) => {
    setExpandedParents((prev) => ({ ...prev, [parentTaskId]: !prev[parentTaskId] }));
  };

  // Hebi8 main task child status badge for top nav button
  const hebi8ChildStats = useMemo(() => {
    const paMains = boot.activeTasks.filter((t) => t.projectId === PA_PROJECT_ID && !t.parentTaskId);
    let active = 0;
    let approvals = 0;
    for (const t of paMains) {
      const localKids = localChildrenByParent.get(t.taskId) ?? [];
      const localLive = localKids.filter((c) => isChildStateLive(c.state)).length;
      const localAp = localKids.filter((c) => c.state === 'waiting_approval').length;
      active += Math.max(t.activeChildCount ?? 0, localLive);
      approvals += Math.max(t.pendingChildApprovals ?? 0, localAp);
    }
    return { active, approvals };
  }, [boot.activeTasks, localChildrenByParent]);

  const childStatsFor = (t: TaskSummary) => {
    const localKids = localChildrenByParent.get(t.taskId) ?? [];
    const childCount = Math.max(t.childCount ?? 0, localKids.length);
    const activeChildCount = Math.max(
      t.activeChildCount ?? 0,
      localKids.filter((c) => isChildStateLive(c.state)).length,
    );
    const pendingChildApprovals = Math.max(
      t.pendingChildApprovals ?? 0,
      localKids.filter((c) => c.state === 'waiting_approval').length,
    );
    return { localKids, childCount, activeChildCount, pendingChildApprovals };
  };

  const invalidateTasks = (projectId: string) => {
    void qc.invalidateQueries({ queryKey: ['project-tasks', projectId] });
    void qc.invalidateQueries({ queryKey: ['all-tasks'] });
    void qc.invalidateQueries({ queryKey: ['archived-tasks'] });
    void qc.invalidateQueries({ queryKey: ['global-artifacts'] });
    void qc.invalidateQueries({ queryKey: ['bootstrap'] });
  };

  const handleArchiveTask = async (t: TaskSummary) => {
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

  const handleDeleteTask = async (t: TaskSummary) => {
    try {
      await api.deleteTask(t.taskId);
      invalidateTasks(t.projectId);
      if (activeTaskId === t.taskId) {
        navigate(isVirtualProject(t.projectId) ? paths.home() : paths.project(t.projectId));
      }
      toast(`已永久删除「${taskTitle(t)}」`);
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
        <div className="quick">
          <button type="button" className={`qi ${route.name === 'home' ? 'on' : ''}`} onClick={() => navigate(paths.home())}>
            <LayoutGrid size={14} strokeWidth={1.6} />
            总览
          </button>
          <button type="button" className={`qi ${route.name === 'pa' ? 'on' : ''}`} onClick={() => navigate(paths.hebi8())}>
            <PAIcon />
            <span>Hebi8</span>
            {(hebi8ChildStats.approvals > 0 || hebi8ChildStats.active > 0) && (
              <span className="qi-sub-badges">
                {hebi8ChildStats.approvals > 0 && (
                  <span className="qi-sub-badge is-ap mono" title={`${hebi8ChildStats.approvals} 个子会话待审批`}>
                    {hebi8ChildStats.approvals}
                  </span>
                )}
                {hebi8ChildStats.active > 0 && (
                  <span className="qi-sub-badge is-run mono" title={`${hebi8ChildStats.active} 个子会话运行中`}>
                    {hebi8ChildStats.active}
                  </span>
                )}
              </span>
            )}
          </button>
          <button type="button" className={`qi ${route.name === 'artifacts' ? 'on' : ''}`} onClick={() => navigate(paths.artifacts())}>
            <Layers size={14} strokeWidth={1.6} />
            Artifacts
          </button>
          <button type="button" className={`qi ${route.name === 'archived' ? 'on' : ''}`} onClick={() => navigate(paths.archived())}>
            <Archive size={14} strokeWidth={1.6} />
            已归档
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
                    onClick={() => setExpanded(p.projectId, !open)}
                  >
                    <CaretIcon />
                  </button>
                  <button type="button" className="pname" onClick={() => navigate(paths.project(p.projectId))}>
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
                    onClick={() => navigate(paths.newThread('project', p.projectId))}
                  >
                    <Plus size={13} strokeWidth={1.6} />
                  </button>
                </div>
                {open &&
                  threads.slice(0, MAX_THREADS).map((t) => {
                    const mark = rootSessionMark(t, allProjTasks, inboxByTask);
                    const cs = childStatsFor(t);
                    return (
                      <SidebarThreadItem
                        key={t.taskId}
                        task={t}
                        mark={mark}
                        active={activeTaskId === t.taskId}
                        childCount={cs.childCount}
                        activeChildCount={cs.activeChildCount}
                        pendingChildApprovals={cs.pendingChildApprovals}
                        childrenExpanded={!!expandedParents[t.taskId]}
                        onToggleChildren={() => toggleParentExpanded(t.taskId)}
                        localChildren={cs.localKids}
                        machines={boot.machines}
                        activeTaskId={activeTaskId}
                        activeChildDrawerId={activeChildDrawerId}
                        menuOpen={openMenuTaskId === t.taskId}
                        onToggleMenu={() => setOpenMenuTaskId((cur) => (cur === t.taskId ? null : t.taskId))}
                        onCloseMenu={() => setOpenMenuTaskId(null)}
                        onArchive={() => void handleArchiveTask(t)}
                        onDelete={() => void handleDeleteTask(t)}
                      />
                    );
                  })}
                {open && threads.length > MAX_THREADS && (
                  <button type="button" className="th-more" onClick={() => navigate(paths.project(p.projectId))}>
                    全部线程 →
                  </button>
                )}
              </div>
            );
          })}
          {scratchThreads.length > 0 && (
            <>
              <div className="slabel" style={{ marginTop: 10 }}>
                自由对话
              </div>
              {scratchThreads.slice(0, MAX_THREADS).map((t) => {
                const mark = sessionMark(t, inboxByTask.get(t.taskId));
                const cs = childStatsFor(t);
                return (
                  <SidebarThreadItem
                    key={t.taskId}
                    task={t}
                    mark={mark}
                    active={activeTaskId === t.taskId}
                    flat
                    childCount={cs.childCount}
                    activeChildCount={cs.activeChildCount}
                    pendingChildApprovals={cs.pendingChildApprovals}
                    childrenExpanded={!!expandedParents[t.taskId]}
                    onToggleChildren={() => toggleParentExpanded(t.taskId)}
                    localChildren={cs.localKids}
                    machines={boot.machines}
                    activeTaskId={activeTaskId}
                    activeChildDrawerId={activeChildDrawerId}
                    menuOpen={openMenuTaskId === t.taskId}
                    onToggleMenu={() => setOpenMenuTaskId((cur) => (cur === t.taskId ? null : t.taskId))}
                    onCloseMenu={() => setOpenMenuTaskId(null)}
                    onArchive={() => void handleArchiveTask(t)}
                    onDelete={() => void handleDeleteTask(t)}
                  />
                );
              })}
            </>
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
            machines={boot.machines}
            open={machinesOpen}
            onClick={() => setMachinesOpen(!machinesOpen)}
          />
          <button type="button" className="ib" title="切换深 / 浅色" aria-label="切换深浅色" onClick={toggleTheme}>
            {theme === 'dark' ? <SunIcon /> : <MoonIcon />}
          </button>
        </div>
      </div>
    </aside>
  );
}
