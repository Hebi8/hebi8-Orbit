import { useMemo } from 'react';
import { ArrowRight, Plus } from 'lucide-react';
import type { BootstrapResponse } from '../generated/protocol';
import { PageHeader } from '../components/PageHeader';
import { useProjectTasks } from '../hooks/queries';
import { SCRATCH_PROJECT_ID, attentionTaskIds, projectInitial, taskDot, taskStateLabel, taskTitle } from '../lib/format';
import { navigate, paths } from '../lib/router';

/** Project index (mobile "项目" tab); threads never expand into workers here either. */
export function ProjectsListPage({ boot }: { boot: BootstrapResponse }) {
  const projects = boot.projects.filter((p) => p.projectId !== SCRATCH_PROJECT_ID && !p.archived);
  const hasScratch = boot.projects.some((p) => p.projectId === SCRATCH_PROJECT_ID);
  const scratchQ = useProjectTasks(SCRATCH_PROJECT_ID, hasScratch);
  const scratch = (scratchQ.data ?? []).filter((t) => !t.parentTaskId).sort((a, b) => b.updatedAt - a.updatedAt);
  const attentionSet = useMemo(() => attentionTaskIds(boot.openInboxItems), [boot.openInboxItems]);

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
          {projects.map((p) => (
            <button type="button" key={p.projectId} className="srow" onClick={() => navigate(paths.project(p.projectId))}>
              <span className="pl">{projectInitial(p)}</span>
              <span className="t">{p.displayName}</span>
              <span className="st">{p.blockedTaskCount > 0 ? '需要注意' : p.activeTaskCount > 0 ? '运行中' : ''}</span>
              <span className="go">
                <ArrowRight size={12} strokeWidth={1.6} />
              </span>
            </button>
          ))}
          {scratch.length > 0 && (
            <section className="sec">
              <div className="sec-h">
                <span className="t">自由 Chat</span>
              </div>
              {scratch.map((t) => {
                const attn = attentionSet.has(t.taskId);
                return (
                  <button type="button" key={t.taskId} className="srow" onClick={() => navigate(paths.session(t.taskId))}>
                    <span className={`sd ${taskDot(t.state, t.activeAttemptState, attn)}`} />
                    <span className="t">{taskTitle(t)}</span>
                    <span className="st">{taskStateLabel(t, attn)}</span>
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
