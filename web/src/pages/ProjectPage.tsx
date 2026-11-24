import { useMemo, useState } from 'react';
import { useQueries } from '@tanstack/react-query';
import { ArrowUpRight, Plus } from 'lucide-react';
import type { BootstrapResponse, DeliveryView, TaskSummary } from '../generated/protocol';
import { ArtIcon, CursorIcon, DiffIcon } from '../components/Icons';
import { PageHeader } from '../components/PageHeader';
import { useProjectTasks } from '../hooks/queries';
import { api } from '../lib/api';
import { buildIDEJump, buildProjectIDEJump, launchIDE } from '../lib/ide';
import { projectInitial, relTime, shortSha, taskDot, taskStateLabel, taskTitle } from '../lib/format';
import { navigate, paths, type ProjectTab } from '../lib/router';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';

const GROUPS: { key: string; label: string; match: (t: TaskSummary) => boolean }[] = [
  { key: 'active', label: '活跃中', match: (t) => ['queued', 'running', 'waiting_resource', 'waiting_approval'].includes(t.state) },
  { key: 'review', label: '待审阅', match: (t) => t.state === 'in_review' },
  { key: 'closed', label: '已结束', match: (t) => ['done', 'failed', 'cancelled'].includes(t.state) },
];

function ThreadRow({ t }: { t: TaskSummary }) {
  const extras: string[] = [];
  if (t.latestDeliveryHead) extras.push(shortSha(t.latestDeliveryHead));
  if (t.queuedInputCount > 0) extras.push(`队列 ${t.queuedInputCount}`);
  if (t.workerCount > 0) extras.push(`${t.workerCount}w`);
  return (
    <button type="button" className="srow" onClick={() => navigate(paths.session(t.taskId))}>
      <span className={`sd ${taskDot(t.state, t.activeAttemptState)}`} />
      <span className="t">{taskTitle(t)}</span>
      <span className="st mono">{extras.join(' · ')}</span>
      <span className="st">{taskStateLabel(t)}</span>
      <span className="st">{relTime(t.updatedAt)}</span>
    </button>
  );
}

const CODE_KINDS = new Set(['code_diff', 'mixed', 'workspace_snapshot']);

export function ProjectPage({ boot, projectId, tab }: { boot: BootstrapResponse; projectId: string; tab: ProjectTab }) {
  const project = boot.projects.find((p) => p.projectId === projectId);
  const tasksQ = useProjectTasks(projectId);
  const [q, setQ] = useState('');
  const openWindow = useWorkbenchStore((s) => s.openWindow);
  const localMachineId = useWorkbenchStore((s) => s.localMachineId);

  const threads = useMemo(() => (tasksQ.data ?? []).filter((t) => !t.parentTaskId), [tasksQ.data]);
  const filtered = useMemo(() => {
    const k = q.trim().toLowerCase();
    if (!k) return threads;
    return threads.filter((t) => `${t.title} ${t.goal}`.toLowerCase().includes(k));
  }, [threads, q]);

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
  const machineStatus = (id: string) => boot.machines.find((m) => m.machineId === id)?.status;

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
            <span className="pl">{projectInitial(project)}</span>
            <div style={{ minWidth: 0 }}>
              <h1>{project.displayName}</h1>
              <div className="meta">
                <span className="mono">{project.repoIdentity}</span>
                <span>
                  默认分支 <span className="mono">{project.defaultBranch}</span>
                </span>
              </div>
            </div>
            <div className="r">
              <button type="button" className="btn pri" onClick={() => navigate(paths.newThread('project', project.projectId))}>
                <Plus size={13} strokeWidth={1.6} /> 新建项目线程
              </button>
              <button type="button" className="btn" onClick={openRepo}>
                <CursorIcon size={13} /> 在 Cursor 打开
              </button>
            </div>
          </div>
          <div className="anchors-line">
            {Object.entries(project.anchors).map(([cap, a]) => {
              const st = machineStatus(a.machine);
              return (
                <span key={cap} title={a.repoPath || a.remoteDir || ''}>
                  <span className={`sd ${st === 'online' ? 'on' : st === 'degraded' ? 'wait' : ''}`} />
                  {a.machine} · {cap}
                  {a.deployStrategy ? ' · 部署目标' : ''}
                  {cap === project.defaultCapability ? ' · 默认' : ''}
                </span>
              );
            })}
          </div>

          <div className="tabs" role="tablist">
            <button type="button" className={tab === 'threads' ? 'on' : ''} onClick={() => navigate(paths.project(projectId, 'threads'))}>
              线程 <span className="faint mono">{threads.length}</span>
            </button>
            <button type="button" className={tab === 'deliveries' ? 'on' : ''} onClick={() => navigate(paths.project(projectId, 'deliveries'))}>
              交付与部署
            </button>
            {tab === 'threads' && (
              <span className="sp">
                <input className="field" placeholder="搜索线程…" value={q} onChange={(e) => setQ(e.target.value)} aria-label="搜索线程" />
              </span>
            )}
          </div>

          {tab === 'threads' &&
            (tasksQ.isLoading ? (
              <div className="quiet">载入线程…</div>
            ) : filtered.length === 0 ? (
              <div className="quiet">{q ? '没有匹配的线程' : '这个项目还没有线程'}</div>
            ) : (
              GROUPS.map((g) => {
                const rows = filtered.filter(g.match).sort((a, b) => b.updatedAt - a.updatedAt);
                if (!rows.length) return null;
                return (
                  <div key={g.key}>
                    <div className="grp-h">
                      {g.label} · {rows.length}
                    </div>
                    {rows.map((t) => (
                      <ThreadRow key={t.taskId} t={t} />
                    ))}
                  </div>
                );
              })
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
                    {d.openAnnotations > 0 && <span className="faint">{d.openAnnotations} 条未决</span>}
                    <span className="faint no-mobile">{relTime(d.createdAt)}</span>
                    <span className="acts">
                      {CODE_KINDS.has(d.kind) && (
                        <button type="button" className="ib" title="Code Review" onClick={() => openWindow('cr', d.taskId, d.deliveryId)}>
                          <DiffIcon size={13} />
                        </button>
                      )}
                      {(d.ryugiBindings?.length ?? 0) > 0 && (
                        <button type="button" className="ib" title="Artifacts · Ryugi" onClick={() => openWindow('art', d.taskId, d.deliveryId)}>
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
