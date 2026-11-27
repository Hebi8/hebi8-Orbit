import { useMemo } from 'react';
import { ArrowRight, Plus } from 'lucide-react';
import type { BootstrapResponse } from '../generated/protocol';
import { PageHeader } from '../components/PageHeader';
import { StatusMark } from '../components/StatusMark';
import { useManyProjectTasks } from '../hooks/queries';
import {
  SCRATCH_PROJECT_ID,
  indexInboxByTask,
  isVirtualProject,
  mergeSessionMarks,
  relTime,
  rootSessionMark,
  sessionMark,
  taskTitle,
} from '../lib/format';
import { navigate, paths } from '../lib/router';
import { useWorkbenchStore } from '../store/useWorkbenchStore';

/** Project index (mobile "项目" tab); threads never expand into workers here either. */
export function ProjectsListPage({ boot }: { boot: BootstrapResponse }) {
  const seenTasks = useWorkbenchStore((s) => s.seenTasks);
  const projects = boot.projects.filter((p) => !isVirtualProject(p.projectId) && !p.archived);
  const tasksByProject = useManyProjectTasks(boot.projects);
  const scratch = (tasksByProject[SCRATCH_PROJECT_ID] ?? [])
    .filter((t) => !t.parentTaskId)
    .sort((a, b) => b.updatedAt - a.updatedAt);
  const inboxByTask = useMemo(() => indexInboxByTask(boot.openInboxItems), [boot.openInboxItems]);

  return (
    <main className="main">
      <PageHeader
        right={
          <button type="button" className="hb" onClick={() => navigate(paths.newThread())}>
            <Plus size={14} strokeWidth={1.6} /> 新建
          </button>
        }
      >
        <span className="ttl">项目</span>
      </PageHeader>
      <div className="page-scroll">
        <div className="page">
          {projects.length === 0 && <div className="quiet">还没有注册项目</div>}
          {projects.map((p) => {
            const pTasks = tasksByProject[p.projectId] ?? [];
            const roots = pTasks.filter((t) => !t.parentTaskId);
            const mark = mergeSessionMarks(...roots.map((r) => rootSessionMark(r, pTasks, inboxByTask, seenTasks)));
            return (
              <button type="button" key={p.projectId} className="srow" onClick={() => navigate(paths.project(p.projectId))}>
                <span className="t">{p.displayName}</span>
                <StatusMark mark={mark} />
                <span className="go">
                  <ArrowRight size={12} strokeWidth={1.6} />
                </span>
              </button>
            );
          })}
          {scratch.length > 0 && (
            <section className="sec">
              <div className="sec-h">
                <span className="t">自由 Chat</span>
              </div>
              {scratch.map((t) => {
                const mark = sessionMark(t, inboxByTask.get(t.taskId), seenTasks);
                return (
                  <button type="button" key={t.taskId} className="srow" onClick={() => navigate(paths.session(t.taskId))}>
                    <span className="t">{taskTitle(t)}</span>
                    {mark ? <StatusMark mark={mark} /> : <span className="st">{relTime(t.updatedAt)}</span>}
                  </button>
                );
              })}
            </section>
          )}
        </div>
      </div>
    </main>
  );
}
