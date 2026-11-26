import { useMemo, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { ArchiveRestore, Search, Trash2 } from 'lucide-react';
import type { BootstrapResponse, TaskSummary } from '../generated/protocol';
import { PageHeader } from '../components/PageHeader';
import { useArchivedTasks } from '../hooks/queries';
import { api, errMsg } from '../lib/api';
import { PA_PROJECT_ID, SCRATCH_PROJECT_ID, isVirtualProject, relTime, taskTitle } from '../lib/format';
import { navigate, paths } from '../lib/router';
import { toast } from '../store/useWorkbenchStore';

export function ArchivedPage({ boot }: { boot: BootstrapResponse }) {
  const qc = useQueryClient();
  const [search, setSearch] = useState('');
  const [projectFilter, setProjectFilter] = useState('');
  const [confirmDeleteId, setConfirmDeleteId] = useState<string | null>(null);

  const archivedQ = useArchivedTasks({
    projectId: projectFilter || undefined,
    q: search || undefined,
    limit: 200,
  });

  const projectNames = useMemo(() => {
    const map = new Map<string, string>();
    map.set(SCRATCH_PROJECT_ID, '自由对话');
    map.set(PA_PROJECT_ID, 'Hebi8');
    for (const p of boot.projects) {
      if (p.projectId === SCRATCH_PROJECT_ID) {
        map.set(p.projectId, '自由对话');
      } else if (p.projectId === PA_PROJECT_ID) {
        map.set(p.projectId, 'Hebi8');
      } else {
        map.set(p.projectId, p.displayName);
      }
    }
    return map;
  }, [boot.projects]);

  const filterProjects = useMemo(() => {
    const list: { id: string; label: string }[] = [{ id: '', label: '全部' }];
    for (const p of boot.projects) {
      if (p.archived) continue;
      if (p.projectId === SCRATCH_PROJECT_ID) {
        list.push({ id: SCRATCH_PROJECT_ID, label: '自由对话' });
      } else if (p.projectId === PA_PROJECT_ID) {
        list.push({ id: PA_PROJECT_ID, label: 'Hebi8' });
      } else if (!isVirtualProject(p.projectId)) {
        list.push({ id: p.projectId, label: p.displayName });
      }
    }
    return list;
  }, [boot.projects]);

  const invalidateAll = (projectId?: string) => {
    void qc.invalidateQueries({ queryKey: ['archived-tasks'] });
    void qc.invalidateQueries({ queryKey: ['all-tasks'] });
    void qc.invalidateQueries({ queryKey: ['bootstrap'] });
    void qc.invalidateQueries({ queryKey: ['global-artifacts'] });
    if (projectId) {
      void qc.invalidateQueries({ queryKey: ['project-tasks', projectId] });
    } else {
      void qc.invalidateQueries({ queryKey: ['project-tasks'] });
    }
  };

  const restoreMut = useMutation({
    mutationFn: (task: TaskSummary) => api.unarchiveTask(task.taskId).then(() => task),
    onSuccess: (task) => {
      invalidateAll(task.projectId);
      toast(`已恢复「${taskTitle(task)}」`);
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const deleteMut = useMutation({
    mutationFn: (task: TaskSummary) => api.deleteTask(task.taskId).then(() => task),
    onSuccess: (task) => {
      setConfirmDeleteId(null);
      invalidateAll(task.projectId);
      toast(`已永久删除「${taskTitle(task)}」`);
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const tasks = useMemo(() => (archivedQ.data ?? []).filter((t) => !t.parentTaskId), [archivedQ.data]);

  return (
    <main className="main">
      <PageHeader>
        <span className="ttl">已归档</span>
      </PageHeader>
      <div className="page-scroll">
        <div className="page">
          <div className="home-hd">
            <div>
              <h1 className="home-title">已归档会话</h1>
            </div>
          </div>

          <div className="arch-toolbar">
            <div className="arch-search-wrap">
              <Search size={13} strokeWidth={1.6} className="faint" />
              <input
                type="text"
                className="arch-search-in"
                placeholder="搜索已归档会话标题或内容…"
                value={search}
                onChange={(e) => setSearch(e.target.value)}
              />
            </div>
            {filterProjects.length > 1 && (
              <div className="art-filters" style={{ marginBottom: 0 }} role="tablist" aria-label="按项目筛选">
                {filterProjects.map((fp) => (
                  <button
                    key={fp.id || 'all'}
                    type="button"
                    role="tab"
                    aria-selected={projectFilter === fp.id}
                    className={`art-filter-btn ${projectFilter === fp.id ? 'active' : ''}`}
                    onClick={() => setProjectFilter(fp.id)}
                  >
                    {fp.label}
                  </button>
                ))}
              </div>
            )}
          </div>

          {archivedQ.isLoading ? (
            <div className="quiet">载入已归档会话…</div>
          ) : archivedQ.isError ? (
            <div className="empty bad-t">载入失败：{errMsg(archivedQ.error)}</div>
          ) : tasks.length === 0 ? (
            <div className="quiet-alone">
              {search || projectFilter ? '没有匹配的已归档会话' : '还没有已归档的会话'}
            </div>
          ) : (
            <div className="arch-list">
              {tasks.map((t) => {
                const projLabel = projectNames.get(t.projectId) ?? t.projectId;
                const confirming = confirmDeleteId === t.taskId;
                return (
                  <div className="arch-row" key={t.taskId}>
                    <button
                      type="button"
                      className="arch-row-main"
                      onClick={() => navigate(paths.session(t.taskId))}
                    >
                      <span className="arch-row-title ell">{taskTitle(t)}</span>
                      <span className="arch-row-proj">{projLabel}</span>
                      <span className="arch-row-time faint">
                        归档于 {relTime(t.archivedAt ?? t.updatedAt)}
                      </span>
                    </button>
                    <div className="arch-row-acts">
                      <button
                        type="button"
                        className="btn ghost sm"
                        disabled={restoreMut.isPending}
                        onClick={() => restoreMut.mutate(t)}
                      >
                        <ArchiveRestore size={12.5} strokeWidth={1.6} /> 恢复
                      </button>
                      {confirming ? (
                        <>
                          <button
                            type="button"
                            className="btn ghost sm bad-t"
                            disabled={deleteMut.isPending}
                            onClick={() => deleteMut.mutate(t)}
                          >
                            确认删除
                          </button>
                          <button
                            type="button"
                            className="btn ghost sm"
                            onClick={() => setConfirmDeleteId(null)}
                          >
                            取消
                          </button>
                        </>
                      ) : (
                        <button
                          type="button"
                          className="ib bad-t"
                          title="永久删除"
                          aria-label="永久删除"
                          onClick={() => setConfirmDeleteId(t.taskId)}
                        >
                          <Trash2 size={13} strokeWidth={1.6} />
                        </button>
                      )}
                    </div>
                  </div>
                );
              })}
            </div>
          )}
        </div>
      </div>
    </main>
  );
}
