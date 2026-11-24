import { useMemo } from 'react';
import { LayoutGrid, PanelLeftClose, Plus } from 'lucide-react';
import type { BootstrapResponse, MachineView, ProjectView, TaskSummary } from '../generated/protocol';
import { useManyProjectTasks } from '../hooks/queries';
import { useHubLatency } from '../hooks/useHubLatency';
import { SCRATCH_PROJECT_ID, attentionTaskIds, projectInitial, taskDot, taskStateLabel, taskTitle } from '../lib/format';
import { navigate, paths, type Route } from '../lib/router';
import { useWorkbenchStore } from '../store/useWorkbenchStore';
import { CaretIcon, MoonIcon, PAIcon, SunIcon } from './Icons';

const MAX_THREADS = 8;

function mainThreads(tasks: TaskSummary[]): TaskSummary[] {
  return tasks.filter((t) => !t.parentTaskId).sort((a, b) => b.updatedAt - a.updatedAt);
}

/**
 * Single current-host footer indicator mirroring Lody LAN fork's sidebar footer +
 * `LanConnectionIndicator`: shows only the connected host and its live round-trip
 * latency (or offline/reconnecting), never a fleet strip of every machine on the network.
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
  const dotClass = unreachable ? 'block' : degraded ? 'wait' : wsStatus === 'live' ? 'on' : '';

  const statusText = unreachable
    ? '未连接'
    : wsStatus === 'reconnecting'
      ? '重连中'
      : latency !== undefined
        ? `${latency} ms`
        : '';
  const tooltip = unreachable
    ? `${hostName} · 未连接`
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
      <span className={`sd ${dotClass}`} />
      <span className="hn ell">{hostName}</span>
      {statusText && <span className={`hl ${unreachable ? 'bad-t' : degraded ? 'warn-t' : ''}`}>{statusText}</span>}
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
  const hasAttention = boot.openInboxItems.length > 0;
  const attentionSet = useMemo(() => attentionTaskIds(boot.openInboxItems), [boot.openInboxItems]);
  const activeTaskId = route.name === 'session' ? route.taskId : '';

  const currentHost =
    boot.machines.find((m) => m.transportMode === 'local_ws') ??
    boot.machines.find((m) => m.machineId === localMachineId) ??
    boot.machines[0];

  // Roll child-task attention up to its root thread so each root row draws one mark only.
  const rootHasAttention = (root: TaskSummary, projectTasks: TaskSummary[]): boolean => {
    if (attentionSet.has(root.taskId)) return true;
    return projectTasks.some((c) => c.parentTaskId === root.taskId && attentionSet.has(c.taskId));
  };

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
            {hasAttention && <span className="sd wait" style={{ marginLeft: 'auto' }} title="有需要注意的会话" />}
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
                    <span className="pl">{projectInitial(p)}</span>
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
                    const attn = rootHasAttention(t, allProjTasks);
                    return (
                      <button
                        type="button"
                        key={t.taskId}
                        className={`th ${activeTaskId === t.taskId ? 'on' : ''}`}
                        onClick={() => navigate(paths.session(t.taskId))}
                        title={`${taskTitle(t)} · ${taskStateLabel(t, attn)}`}
                      >
                        <span className={`sd ${taskDot(t.state, t.activeAttemptState, attn)}`} />
                        <span className="tt">{taskTitle(t)}</span>
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
                const attn = attentionSet.has(t.taskId);
                return (
                  <button
                    type="button"
                    key={t.taskId}
                    className={`th flat ${activeTaskId === t.taskId ? 'on' : ''}`}
                    onClick={() => navigate(paths.session(t.taskId))}
                    title={`${taskTitle(t)} · ${taskStateLabel(t, attn)}`}
                  >
                    <span className={`sd ${taskDot(t.state, t.activeAttemptState, attn)}`} />
                    <span className="tt">{taskTitle(t)}</span>
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
