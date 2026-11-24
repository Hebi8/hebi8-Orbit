import { useMemo, useState } from 'react';
import { useQueries } from '@tanstack/react-query';
import { ArrowUpRight, Plus } from 'lucide-react';
import type { BootstrapResponse, DeliveryView, TaskSummary } from '../generated/protocol';
import { ArtIcon, CursorIcon, DiffIcon } from '../components/Icons';
import { PageHeader } from '../components/PageHeader';
import { StatusMark } from '../components/StatusMark';
import { useProjectTasks } from '../hooks/queries';
import { api } from '../lib/api';
import {
  displayMarkRank,
  indexInboxByTask,
  relTime,
  rootSessionMark,
  shortSha,
  taskTitle,
  type SessionMark,
} from '../lib/format';
import { buildIDEJump, buildProjectIDEJump, launchIDE } from '../lib/ide';
import { navigate, paths, type ProjectTab } from '../lib/router';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';

function ThreadRow({ t, mark }: { t: TaskSummary; mark: SessionMark }) {
  return (
    <button type="button" className="srow" onClick={() => navigate(paths.session(t.taskId))}>
      <span className="t">{taskTitle(t)}</span>
      {mark ? <StatusMark mark={mark} /> : <span className="st">{relTime(t.updatedAt)}</span>}
    </button>
  );
}

const CODE_KINDS = new Set(['code_diff', 'mixed', 'workspace_snapshot']);

export function ProjectPage({ boot, projectId, tab }: { boot: BootstrapResponse; projectId: string; tab: ProjectTab }) {
  const project = boot.projects.find((p) => p.projectId === projectId);
  const tasksQ = useProjectTasks(projectId);
  const [q, setQ] = useState('');
  const openReview = useWorkbenchStore((s) => s.openReview);
  const localMachineId = useWorkbenchStore((s) => s.localMachineId);

  const allTasks = useMemo(() => tasksQ.data ?? [], [tasksQ.data]);
  const threads = useMemo(() => allTasks.filter((t) => !t.parentTaskId), [allTasks]);
  const inboxByTask = useMemo(() => indexInboxByTask(boot.openInboxItems), [boot.openInboxItems]);

  const ranked = useMemo(() => {
    const k = q.trim().toLowerCase();
    const list = k ? threads.filter((t) => `${t.title} ${t.goal}`.toLowerCase().includes(k)) : threads;
    return list
      .map((t) => ({ t, mark: rootSessionMark(t, allTasks, inboxByTask) }))
      .sort((a, b) => displayMarkRank(a.mark) - displayMarkRank(b.mark) || b.t.updatedAt - a.t.updatedAt);
  }, [threads, allTasks, inboxByTask, q]);

  const markedRows = ranked.filter((r) => r.mark !== null);
  const restingRows = ranked.filter((r) => r.mark === null);

  const withDeliveries = tab === 'deliveries' ? threads.filter((t) => t.deliveryCount > 0) : [];
  const timelines = useQueries({
    queries: withDeliveries.map((t) => ({
      queryKey: ['task-timeline', t.taskId],
      queryFn: () => api.getTaskTimeline(t.taskId),
    })),
  });
  const deliveries: { d: DeliveryView; v: number; task: TaskSummary }[] = [];
  withDeliveries.forEach((t, i) => {
    (timelines[i]?.data?.deliveries ?? []).forEach((d, idx) => deliveries.push({ d, v: idx + 1, task: t }));
  });
  deliveries.sort((a, b) => b.d.createdAt - a.d.createdAt);

  if (!project) {
    return (
      <main className="main">
        <PageHeader>
          <span className="ttl">项目</span>
        </PageHeader>
        <div className="empty">项目不存在或已归档</div>
      </main>
    );
  }

  const openRepo = () => {
    const j = buildProjectIDEJump({ project, machines: boot.machines, localMachineId });
    if (!j) {
      toast('该项目没有可打开的仓库锚点', 'err');
      return;
    }
    launchIDE(j.uri);
  };

  const nonDefaultBranch = project.defaultBranch && project.defaultBranch !== 'main' && project.defaultBranch !== 'master';

  return (
    <main className="main">
      <PageHeader>
        <div className="crumb">
          <span className="c0">项目</span>
          <span className="sep">/</span>
          <span className="c1">{project.displayName}</span>
        </div>
      </PageHeader>
      <div className="page-scroll">
        <div className="page">
          <div className="phead">
            <div style={{ minWidth: 0 }}>
              <h1>{project.displayName}</h1>
              <div className="meta">
                <span className="mono">{project.repoIdentity}</span>
                {nonDefaultBranch && (
                  <span>
                    分支 <span className="mono">{project.defaultBranch}</span>
                  </span>
                )}
              </div>
            </div>
            <div className="r">
              <button type="button" className="btn ghost" onClick={() => navigate(paths.newThread('project', project.projectId))}>
                <Plus size={13} strokeWidth={1.6} /> 新线程
              </button>
              <button type="button" className="ib" title="在 Cursor 打开" onClick={openRepo}>
                <CursorIcon size={14} />
              </button>
            </div>
          </div>

          <div className="tabs" role="tablist">
            <button type="button" className={tab === 'threads' ? 'on' : ''} onClick={() => navigate(paths.project(projectId, 'threads'))}>
              线程
            </button>
            <button type="button" className={tab === 'deliveries' ? 'on' : ''} onClick={() => navigate(paths.project(projectId, 'deliveries'))}>
              交付
            </button>
            {tab === 'threads' && threads.length > 10 && (
              <span className="sp">
                <input className="field" placeholder="搜索线程…" value={q} onChange={(e) => setQ(e.target.value)} aria-label="搜索线程" />
              </span>
            )}
          </div>

          {tab === 'threads' &&
            (tasksQ.isLoading ? (
              <div className="quiet">载入线程…</div>
            ) : ranked.length === 0 ? (
              <div className="quiet">{q ? '没有匹配的线程' : '这个项目还没有线程'}</div>
            ) : (
              <>
                {markedRows.map(({ t, mark }) => (
                  <ThreadRow key={t.taskId} t={t} mark={mark} />
                ))}
                {markedRows.length > 0 && restingRows.length > 0 && <div className="home-sep" role="separator" />}
                {restingRows.map(({ t, mark }) => (
                  <ThreadRow key={t.taskId} t={t} mark={mark} />
                ))}
              </>
            ))}

          {tab === 'deliveries' &&
            (withDeliveries.length === 0 ? (
              <div className="quiet">还没有交付</div>
            ) : (
              deliveries.map(({ d, v, task }) => {
                const ide = buildIDEJump({
                  scheme: project.ideScheme,
                  localMachineId,
                  delivery: d,
                  project,
                  machines: boot.machines,
                });
                return (
                  <div className="drow" key={d.deliveryId}>
                    <span className="v mono">v{v}</span>
                    <span className="t">
                      {d.summary || taskTitle(task)}
                      <span className="faint"> · {taskTitle(task)}</span>
                    </span>
                    <span className="mono faint no-mobile">{shortSha(d.headSha)}</span>
                    <span className="mono">
                      <span className="add">+{d.manifest.insertions}</span> <span className="del">−{d.manifest.deletions}</span>
                    </span>
                    <span className="faint no-mobile">{relTime(d.createdAt)}</span>
                    <span className="acts">
                      {CODE_KINDS.has(d.kind) && (
                        <button type="button" className="ib" title="审阅代码" onClick={() => openReview('code', d.taskId, d.deliveryId)}>
                          <DiffIcon size={13} />
                        </button>
                      )}
                      {(d.ryugiBindings?.length ?? 0) > 0 && (
                        <button type="button" className="ib" title="审阅设计稿" onClick={() => openReview('art', d.taskId, d.deliveryId)}>
                          <ArtIcon size={13} />
                        </button>
                      )}
                      {ide && (
                        <button type="button" className="ib no-mobile" title={`在 Cursor 打开 · ${ide.targetLabel}`} onClick={() => launchIDE(ide.uri)}>
                          <ArrowUpRight size={13} strokeWidth={1.6} />
                        </button>
                      )}
                    </span>
                  </div>
                );
              })
            ))}
        </div>
      </div>
    </main>
  );
}
