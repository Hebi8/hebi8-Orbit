import { useMemo } from 'react';
import { Layers, LayoutGrid, PanelLeftClose, Plus, Search, Settings } from 'lucide-react';
import type { BootstrapResponse, MachineView, ProjectView, TaskSummary } from '../generated/protocol';
import { useManyProjectTasks } from '../hooks/queries';
import { useHubLatency } from '../hooks/useHubLatency';
import {
  SCRATCH_PROJECT_ID,
  indexInboxByTask,
  rootSessionMark,
  sessionMark,
  sessionMarkHint,
  taskTitle,
} from '../lib/format';
import { navigate, paths, type Route } from '../lib/router';
import { useWorkbenchStore } from '../store/useWorkbenchStore';
import { CaretIcon, MoonIcon, PAIcon, SunIcon } from './Icons';
import { StatusMark } from './StatusMark';

const MAX_THREADS = 8;

function mainThreads(tasks: TaskSummary[]): TaskSummary[] {
  return tasks.filter((t) => !t.parentTaskId).sort((a, b) => b.updatedAt - a.updatedAt);
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

  const projects = boot.projects.filter((p) => p.projectId !== SCRATCH_PROJECT_ID && !p.archived);
  const scratch = boot.projects.find((p) => p.projectId === SCRATCH_PROJECT_ID);

  const isOpen = (p: ProjectView) =>
    expanded[p.projectId] ?? (p.activeTaskCount + p.blockedTaskCount > 0 || (route.name === 'project' && route.projectId === p.projectId));

  const fetchList = [...projects.filter(isOpen), ...(scratch ? [scratch] : [])];
  const tasksByProject = useManyProjectTasks(fetchList);
  const scratchThreads = scratch ? mainThreads(tasksByProject[scratch.projectId] ?? []) : [];
  const inboxByTask = useMemo(() => indexInboxByTask(boot.openInboxItems), [boot.openInboxItems]);
  const activeTaskId = route.name === 'session' ? route.taskId : '';

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
          <button type="button" className={`qi ${route.name === 'pa' ? 'on' : ''}`} onClick={() => navigate(paths.pa())}>
            <PAIcon />
            PA
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
                    const hint = sessionMarkHint(mark);
                    return (
                      <button
                        type="button"
                        key={t.taskId}
                        className={`th ${activeTaskId === t.taskId ? 'on' : ''} ${!mark ? 'resting' : ''}`}
                        onClick={() => navigate(paths.session(t.taskId))}
                        title={hint ? `${taskTitle(t)} · ${hint}` : taskTitle(t)}
                      >
                        <span className="tt">{taskTitle(t)}</span>
                        <StatusMark mark={mark} />
                      </button>
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
                const hint = sessionMarkHint(mark);
                return (
                  <button
                    type="button"
                    key={t.taskId}
                    className={`th flat ${activeTaskId === t.taskId ? 'on' : ''} ${!mark ? 'resting' : ''}`}
                    onClick={() => navigate(paths.session(t.taskId))}
                    title={hint ? `${taskTitle(t)} · ${hint}` : taskTitle(t)}
                  >
                    <span className="tt">{taskTitle(t)}</span>
                    <StatusMark mark={mark} />
                  </button>
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
