import { LayoutGrid, PanelLeftClose, Plus } from 'lucide-react';
import type { BootstrapResponse, ProjectView, TaskSummary } from '../generated/protocol';
import { useManyProjectTasks } from '../hooks/queries';
import { SCRATCH_PROJECT_ID, projectInitial, taskDot, taskTitle } from '../lib/format';
import { navigate, paths, type Route } from '../lib/router';
import { useWorkbenchStore } from '../store/useWorkbenchStore';
import { CaretIcon, MoonIcon, PAIcon, SunIcon } from './Icons';

const MAX_THREADS = 8;

function mainThreads(tasks: TaskSummary[]): TaskSummary[] {
  return tasks.filter((t) => !t.parentTaskId).sort((a, b) => b.updatedAt - a.updatedAt);
}

function threadHint(t: TaskSummary): string {
  if (t.queuedInputCount > 0) return `队列 ${t.queuedInputCount}`;
  if (t.state === 'in_review') return '待审';
  if (t.activeAttemptState === 'needs_verification') return '待核对';
  return '';
}

export function Sidebar({ boot, route }: { boot: BootstrapResponse; route: Route }) {
  const expanded = useWorkbenchStore((s) => s.expandedProjects);
  const setExpanded = useWorkbenchStore((s) => s.setProjectExpanded);
  const toggleSidebar = useWorkbenchStore((s) => s.toggleSidebar);
  const theme = useWorkbenchStore((s) => s.theme);
  const toggleTheme = useWorkbenchStore((s) => s.toggleTheme);
  const machinesOpen = useWorkbenchStore((s) => s.machinesOpen);
  const setMachinesOpen = useWorkbenchStore((s) => s.setMachinesOpen);

  const projects = boot.projects.filter((p) => p.projectId !== SCRATCH_PROJECT_ID && !p.archived);
  const scratch = boot.projects.find((p) => p.projectId === SCRATCH_PROJECT_ID);

  const isOpen = (p: ProjectView) =>
    expanded[p.projectId] ?? (p.activeTaskCount + p.blockedTaskCount > 0 || (route.name === 'project' && route.projectId === p.projectId));

  const fetchList = [...projects.filter(isOpen), ...(scratch ? [scratch] : [])];
  const tasksByProject = useManyProjectTasks(fetchList);
  const scratchThreads = scratch ? mainThreads(tasksByProject[scratch.projectId] ?? []) : [];
  const inboxCount = boot.openInboxItems.length;
  const activeTaskId = route.name === 'session' ? route.taskId : '';

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
            {inboxCount > 0 && <span className="badge" title="待拍板">{inboxCount}</span>}
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
            const threads = open ? mainThreads(tasksByProject[p.projectId] ?? []) : [];
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
                  threads.slice(0, MAX_THREADS).map((t) => (
                    <button
                      type="button"
                      key={t.taskId}
                      className={`th ${activeTaskId === t.taskId ? 'on' : ''}`}
                      onClick={() => navigate(paths.session(t.taskId))}
                      title={taskTitle(t)}
                    >
                      <span className={`sd ${taskDot(t.state, t.activeAttemptState)}`} />
                      <span className="tt">{taskTitle(t)}</span>
                      {threadHint(t) && <span className="tm">{threadHint(t)}</span>}
                    </button>
                  ))}
                {open && threads.length > MAX_THREADS && (
                  <button type="button" className="th-more" onClick={() => navigate(paths.project(p.projectId))}>
                    全部 {threads.length} 个线程 →
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
              {scratchThreads.slice(0, MAX_THREADS).map((t) => (
                <button
                  type="button"
                  key={t.taskId}
                  className={`th flat ${activeTaskId === t.taskId ? 'on' : ''}`}
                  onClick={() => navigate(paths.session(t.taskId))}
                  title={taskTitle(t)}
                >
                  <span className={`sd ${taskDot(t.state, t.activeAttemptState)}`} />
                  <span className="tt">{taskTitle(t)}</span>
                  {threadHint(t) && <span className="tm">{threadHint(t)}</span>}
                </button>
              ))}
            </>
          )}
        </nav>
        <div className="sfoot">
          <div className="machines" role="group" aria-label="机器">
            {boot.machines.map((m) => (
              <button
                type="button"
                key={m.machineId}
                className={`mc ${machinesOpen ? 'on' : ''}`}
                title={`${m.displayName} · ${m.status}`}
                onClick={() => setMachinesOpen(!machinesOpen)}
              >
                <span className={`sd ${m.status === 'online' ? 'on' : m.status === 'degraded' ? 'wait' : ''}`} />
                <span className="n">{m.machineId}</span>
              </button>
            ))}
            {boot.machines.length === 0 && <span className="faint" style={{ fontSize: 11, padding: '0 6px' }}>无机器</span>}
          </div>
          <button type="button" className="ib" title="切换深/浅色" onClick={toggleTheme}>
            {theme === 'dark' ? <MoonIcon /> : <SunIcon />}
          </button>
        </div>
      </div>
    </aside>
  );
}
