import { useMemo } from 'react';
import { LayoutGrid, PanelLeftClose, Plus } from 'lucide-react';
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

/**
 * Single current-host footer indicator mirroring Lody LAN fork's sidebar footer +
 * `LanConnectionIndicator`: quiet ink latency on healthy connection (no bright dot);
 * color only surfaces when disconnected or reconnecting.
 */
function CurrentHostFooterButton({
  host,
  open,
  onClick,
}: {
  host?: MachineView;
  open: boolean;
  onClick: () => void;
}) {
  const wsStatus = useWorkbenchStore((s) => s.wsStatus);
  const latency = useHubLatency();
  const hostName = host?.displayName || host?.machineId || window.location.hostname || 'Hub';

  const unreachable = latency === null || host?.status === 'offline';
  const degraded = !unreachable && (wsStatus === 'reconnecting' || host?.status === 'degraded');

  const statusText = unreachable
    ? '未连接'
    : degraded
      ? '重连中'
      : latency !== undefined
        ? `${latency} ms`
        : '';
  const tooltip = unreachable
    ? `${hostName} · 未连接`
    : degraded
      ? `${hostName} · 重连中`
      : latency !== undefined
        ? `${hostName} · ${latency} ms`
        : hostName;

  return (
    <button
      type="button"
      className={`host-row ${open ? 'on' : ''}`}
      title={tooltip}
      aria-label={tooltip}
      onClick={onClick}
    >
      <span className="hn ell">{hostName}</span>
      {statusText && (
        <span className={`hl ${unreachable ? 'bad-t' : degraded ? 'warn-t' : ''}`}>{statusText}</span>
      )}
    </button>
  );
}

export function Sidebar({ boot, route }: { boot: BootstrapResponse; route: Route }) {
  const expanded = useWorkbenchStore((s) => s.expandedProjects);
  const setExpanded = useWorkbenchStore((s) => s.setProjectExpanded);
  const toggleSidebar = useWorkbenchStore((s) => s.toggleSidebar);
  const theme = useWorkbenchStore((s) => s.theme);
  const toggleTheme = useWorkbenchStore((s) => s.toggleTheme);
  const machinesOpen = useWorkbenchStore((s) => s.machinesOpen);
  const setMachinesOpen = useWorkbenchStore((s) => s.setMachinesOpen);
  const localMachineId = useWorkbenchStore((s) => s.localMachineId);

  const projects = boot.projects.filter((p) => p.projectId !== SCRATCH_PROJECT_ID && !p.archived);
  const scratch = boot.projects.find((p) => p.projectId === SCRATCH_PROJECT_ID);

  const isOpen = (p: ProjectView) =>
    expanded[p.projectId] ?? (p.activeTaskCount + p.blockedTaskCount > 0 || (route.name === 'project' && route.projectId === p.projectId));

  const fetchList = [...projects.filter(isOpen), ...(scratch ? [scratch] : [])];
  const tasksByProject = useManyProjectTasks(fetchList);
  const scratchThreads = scratch ? mainThreads(tasksByProject[scratch.projectId] ?? []) : [];
  const inboxByTask = useMemo(() => indexInboxByTask(boot.openInboxItems), [boot.openInboxItems]);
  const activeTaskId = route.name === 'session' ? route.taskId : '';

  const currentHost =
    boot.machines.find((m) => m.transportMode === 'local_ws') ??
    boot.machines.find((m) => m.machineId === localMachineId) ??
    boot.machines[0];

  return (
    <aside className="side">
      <div className="side-in">
        <div className="brand">
          <span className="brand-mark" />
          <b>hebi8-Orbit</b>
          <button type="button" className="ib" title="收起侧栏 (⌘\)" onClick={toggleSidebar}>
            <PanelLeftClose size={14} strokeWidth={1.6} />
          </button>
        </div>
        <div className="quick">
          <button type="button" className={`qi ${route.name === 'new' ? 'on' : ''}`} onClick={() => navigate(paths.newThread())}>
            <Plus size={14} strokeWidth={1.6} />
            新建线程
            <span className="kbd">⌘N</span>
          </button>
          <button type="button" className={`qi ${route.name === 'home' ? 'on' : ''}`} onClick={() => navigate(paths.home())}>
            <LayoutGrid size={14} strokeWidth={1.6} />
            总览
          </button>
          <button type="button" className={`qi ${route.name === 'pa' ? 'on' : ''}`} onClick={() => navigate(paths.pa())}>
            <PAIcon />
            PA
          </button>
        </div>
        <nav className="projects" aria-label="项目">
          <div className="slabel">项目</div>
          {projects.length === 0 && <div className="quiet" style={{ padding: '4px 8px' }}>还没有注册项目</div>}
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
                        className={`th ${activeTaskId === t.taskId ? 'on' : ''}`}
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
                自由 Chat
              </div>
              {scratchThreads.slice(0, MAX_THREADS).map((t) => {
                const mark = sessionMark(t, inboxByTask.get(t.taskId));
                const hint = sessionMarkHint(mark);
                return (
                  <button
                    type="button"
                    key={t.taskId}
                    className={`th flat ${activeTaskId === t.taskId ? 'on' : ''}`}
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
        </nav>
        <div className="sfoot">
          <CurrentHostFooterButton host={currentHost} open={machinesOpen} onClick={() => setMachinesOpen(!machinesOpen)} />
          <button type="button" className="ib" title="切换深/浅色" onClick={toggleTheme}>
            {theme === 'dark' ? <MoonIcon /> : <SunIcon />}
          </button>
        </div>
      </div>
    </aside>
  );
}
