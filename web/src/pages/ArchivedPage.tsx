import { useEffect, useMemo, useRef, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import {
  ArchiveRestore,
  ArrowDownWideNarrow,
  Check,
  ChevronDown,
  Folder,
  Search,
  Trash2,
} from 'lucide-react';
import type { BootstrapResponse, TaskSummary } from '../generated/protocol';
import { PageHeader } from '../components/PageHeader';
import { useArchivedTasks } from '../hooks/queries';
import { api, errMsg } from '../lib/api';
import { PA_PROJECT_ID, SCRATCH_PROJECT_ID, isVirtualProject, relTime, taskTitle } from '../lib/format';
import { navigate, paths } from '../lib/router';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';

type ArchivedSortKey = 'archived_desc' | 'active_desc' | 'title_asc';

const SORT_OPTIONS: { id: ArchivedSortKey; serverSort: 'archived_at' | 'updated_at' | 'title'; label: string }[] = [
  { id: 'archived_desc', serverSort: 'archived_at', label: '最近归档' },
  { id: 'active_desc', serverSort: 'updated_at', label: '最近活跃' },
  { id: 'title_asc', serverSort: 'title', label: '按名称' },
];

export function ArchivedPage({ boot }: { boot: BootstrapResponse }) {
  const qc = useQueryClient();
  const [search, setSearch] = useState('');
  const [debouncedSearch, setDebouncedSearch] = useState('');
  const [projectFilter, setProjectFilter] = useState('');
  const [sortBy, setSortBy] = useState<ArchivedSortKey>('archived_desc');
  const [openMenu, setOpenMenu] = useState<null | 'proj' | 'sort'>(null);
  const [confirmDeleteId, setConfirmDeleteId] = useState<string | null>(null);
  const toolbarRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    const t = window.setTimeout(() => setDebouncedSearch(search.trim()), 150);
    return () => window.clearTimeout(t);
  }, [search]);

  const activeSortOpt = SORT_OPTIONS.find((o) => o.id === sortBy) ?? SORT_OPTIONS[0];

  // Server filters by projectId & q and sorts BEFORE LIMIT 200, and returns untruncated top-level projectCounts & totalCount.
  const archivedQ = useArchivedTasks({
    projectId: projectFilter || undefined,
    q: debouncedSearch || undefined,
    sort: activeSortOpt.serverSort,
    limit: 200,
  });

  useEffect(() => {
    if (!openMenu) return;
    const onDown = (e: MouseEvent) => {
      if (toolbarRef.current && !toolbarRef.current.contains(e.target as Node)) {
        setOpenMenu(null);
      }
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !e.isComposing) {
        if (useWorkbenchStore.getState().cmdkOpen) return;
        e.preventDefault();
        e.stopImmediatePropagation();
        setOpenMenu(null);
      }
    };
    const onDismissAll = () => setOpenMenu(null);
    document.addEventListener('mousedown', onDown);
    window.addEventListener('keydown', onKey, true);
    window.addEventListener('orbit:dismiss-popovers', onDismissAll);
    return () => {
      document.removeEventListener('mousedown', onDown);
      window.removeEventListener('keydown', onKey, true);
      window.removeEventListener('orbit:dismiss-popovers', onDismissAll);
    };
  }, [openMenu]);

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

  const tasks = useMemo(
    () => (archivedQ.data?.tasks ?? []).filter((t) => !t.parentTaskId),
    [archivedQ.data?.tasks],
  );
  const serverProjectCounts = archivedQ.data?.projectCounts;
  const totalArchivedCount = archivedQ.data?.totalCount ?? tasks.length;

  const projectOptions = useMemo(() => {
    const counts = new Map<string, number>();
    if (serverProjectCounts) {
      for (const [pid, cnt] of Object.entries(serverProjectCounts)) {
        counts.set(pid, cnt);
      }
    } else {
      for (const t of tasks) {
        counts.set(t.projectId, (counts.get(t.projectId) ?? 0) + 1);
      }
    }
    const opts: { id: string; label: string; count: number }[] = [
      { id: '', label: '全部项目', count: totalArchivedCount },
    ];
    const added = new Set<string>();
    for (const p of boot.projects) {
      if (p.archived || isVirtualProject(p.projectId)) continue;
      added.add(p.projectId);
      opts.push({
        id: p.projectId,
        label: p.displayName,
        count: counts.get(p.projectId) ?? 0,
      });
    }
    if ((counts.get(SCRATCH_PROJECT_ID) ?? 0) > 0) {
      added.add(SCRATCH_PROJECT_ID);
      opts.push({
        id: SCRATCH_PROJECT_ID,
        label: '自由对话',
        count: counts.get(SCRATCH_PROJECT_ID) ?? 0,
      });
    }
    if ((counts.get(PA_PROJECT_ID) ?? 0) > 0) {
      added.add(PA_PROJECT_ID);
      opts.push({
        id: PA_PROJECT_ID,
        label: 'Hebi8',
        count: counts.get(PA_PROJECT_ID) ?? 0,
      });
    }
    for (const [pid, count] of counts.entries()) {
      if (added.has(pid)) continue;
      opts.push({
        id: pid,
        label: projectNames.get(pid) ?? pid,
        count,
      });
    }
    return opts;
  }, [serverProjectCounts, totalArchivedCount, tasks, boot.projects, projectNames]);

  const activeProjOpt = projectOptions.find((o) => o.id === projectFilter) ?? projectOptions[0];

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

  return (
    <main className="main">
      <PageHeader
        right={
          !archivedQ.isLoading ? (
            <span className="faint no-mobile" style={{ fontSize: 11.5, marginRight: 6 }}>
              共 {totalArchivedCount} 条
            </span>
          ) : undefined
        }
      >
        <span className="ttl">已归档</span>
      </PageHeader>
      <div className="page-scroll">
        <div className="page">
          <div className="arch-toolbar" ref={toolbarRef}>
            <div className="arch-search-wrap">
              <Search size={13} strokeWidth={1.6} className="faint" />
              <input
                type="text"
                className="arch-search-in"
                placeholder="按标题搜索已归档会话…"
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                aria-label="搜索已归档会话"
              />
            </div>

            <div className="arch-drop-wrap">
              <button
                type="button"
                className={`arch-drop-btn ${openMenu === 'proj' ? 'open' : ''} ${projectFilter ? 'is-filtered' : ''}`}
                aria-haspopup="menu"
                aria-expanded={openMenu === 'proj'}
                title="按项目筛选"
                onClick={() => setOpenMenu((m) => (m === 'proj' ? null : 'proj'))}
              >
                <Folder size={12.5} strokeWidth={1.6} className="muted" />
                <span className="ell">{activeProjOpt.label}</span>
                <ChevronDown size={11.5} strokeWidth={1.7} className="muted" />
              </button>
              {openMenu === 'proj' && (
                <div className="arch-drop-menu" role="menu" aria-label="按项目筛选">
                  {projectOptions.map((opt) => {
                    const selected = projectFilter === opt.id;
                    return (
                      <button
                        key={opt.id || 'all'}
                        type="button"
                        role="menuitem"
                        className={`arch-drop-item ${selected ? 'on' : ''}`}
                        onClick={() => {
                          setProjectFilter(opt.id);
                          setOpenMenu(null);
                        }}
                      >
                        <span className="ell">{opt.label}</span>
                        <span className="arch-drop-cnt">
                          {opt.count}
                          {selected && <Check size={11} strokeWidth={2.2} className="ok-t" style={{ marginLeft: 4 }} />}
                        </span>
                      </button>
                    );
                  })}
                </div>
              )}
            </div>

            <div className="arch-drop-wrap">
              <button
                type="button"
                className={`arch-drop-btn ${openMenu === 'sort' ? 'open' : ''}`}
                aria-haspopup="menu"
                aria-expanded={openMenu === 'sort'}
                title="排序方式"
                onClick={() => setOpenMenu((m) => (m === 'sort' ? null : 'sort'))}
              >
                <ArrowDownWideNarrow size={12.5} strokeWidth={1.6} className="muted" />
                <span>{activeSortOpt.label}</span>
                <ChevronDown size={11.5} strokeWidth={1.7} className="muted" />
              </button>
              {openMenu === 'sort' && (
                <div className="arch-drop-menu" role="menu" aria-label="排序方式">
                  {SORT_OPTIONS.map((opt) => {
                    const selected = sortBy === opt.id;
                    return (
                      <button
                        key={opt.id}
                        type="button"
                        role="menuitem"
                        className={`arch-drop-item ${selected ? 'on' : ''}`}
                        onClick={() => {
                          setSortBy(opt.id);
                          setOpenMenu(null);
                        }}
                      >
                        <span className="ell">{opt.label}</span>
                        {selected && (
                          <span className="arch-drop-cnt">
                            <Check size={11} strokeWidth={2.2} className="ok-t" />
                          </span>
                        )}
                      </button>
                    );
                  })}
                </div>
              )}
            </div>
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
                const timeText =
                  sortBy === 'active_desc'
                    ? `活跃于 ${relTime(t.updatedAt)}`
                    : `归档于 ${relTime(t.archivedAt ?? t.updatedAt)}`;
                return (
                  <div className="arch-row" key={t.taskId}>
                    <button
                      type="button"
                      className="arch-row-main"
                      onClick={() => navigate(paths.session(t.taskId))}
                    >
                      <span className="arch-row-title ell">{taskTitle(t)}</span>
                      <span className="arch-row-proj">{projLabel}</span>
                      <span className="arch-row-time faint">{timeText}</span>
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
