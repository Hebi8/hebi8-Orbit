import { useEffect, useMemo, useRef, useState } from 'react';
import { Check, Link2, Search, X } from 'lucide-react';
import type { ProjectView, TaskSummary } from '../generated/protocol';
import { useAllTasks } from '../hooks/queries';
import { PA_PROJECT_ID, SCRATCH_PROJECT_ID, relTime, taskTitle } from '../lib/format';

export function projectLabelForTask(task: Pick<TaskSummary, 'projectId'>, projects?: readonly ProjectView[]): string {
  if (task.projectId === SCRATCH_PROJECT_ID) return '对话';
  if (task.projectId === PA_PROJECT_ID) return 'PA';
  return projects?.find((p) => p.projectId === task.projectId)?.displayName || task.projectId;
}

export function TaskRefChips({
  selectedIds,
  onRemove,
  projects,
}: {
  selectedIds: string[];
  onRemove: (taskId: string) => void;
  projects?: readonly ProjectView[];
}) {
  const tasksQ = useAllTasks({ limit: 80, enabled: selectedIds.length > 0 });
  const byId = useMemo(() => {
    const map = new Map<string, TaskSummary>();
    for (const t of tasksQ.data ?? []) map.set(t.taskId, t);
    return map;
  }, [tasksQ.data]);

  if (selectedIds.length === 0) return null;

  return (
    <div className="ref-chips" aria-label="已引用的对话">
      {selectedIds.map((id) => {
        const t = byId.get(id);
        const proj = t ? projectLabelForTask(t, projects) : '';
        const label = t ? taskTitle(t) : id;
        return (
          <span className="ref-chip" key={id} title={proj ? `${proj} · ${label}` : label}>
            <Link2 size={11} strokeWidth={1.7} />
            {proj && <span className="ref-chip-proj">{proj}</span>}
            <span className="ell">{label}</span>
            <button type="button" aria-label={`取消引用 ${label}`} onClick={() => onRemove(id)}>
              <X size={11} strokeWidth={1.8} />
            </button>
          </span>
        );
      })}
    </div>
  );
}

export function TaskRefButton({
  selectedIds,
  onChange,
  projects,
  currentTaskId,
  prominent = false,
  placement = 'up',
}: {
  selectedIds: string[];
  onChange: (next: string[]) => void;
  projects?: readonly ProjectView[];
  currentTaskId?: string;
  prominent?: boolean;
  placement?: 'up' | 'down';
}) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState('');
  const [debouncedQuery, setDebouncedQuery] = useState('');
  const wrapRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    const t = window.setTimeout(() => setDebouncedQuery(query.trim()), 180);
    return () => window.clearTimeout(t);
  }, [query]);

  const tasksQ = useAllTasks({
    limit: 80,
    q: debouncedQuery || undefined,
    enabled: open || selectedIds.length > 0,
  });

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) {
        setOpen(false);
      }
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpen(false);
    };
    window.addEventListener('mousedown', onDown);
    window.addEventListener('keydown', onKey);
    return () => {
      window.removeEventListener('mousedown', onDown);
      window.removeEventListener('keydown', onKey);
    };
  }, [open]);

  const candidates = useMemo(() => {
    const q = query.trim().toLowerCase();
    return (tasksQ.data ?? [])
      .filter((t) => t.taskId !== currentTaskId)
      .filter((t) => {
        if (!q) return true;
        const proj = projectLabelForTask(t, projects).toLowerCase();
        return (
          taskTitle(t).toLowerCase().includes(q) ||
          (t.goal || '').toLowerCase().includes(q) ||
          proj.includes(q)
        );
      })
      .slice(0, 40);
  }, [tasksQ.data, currentTaskId, query, projects]);

  const toggle = (taskId: string) => {
    if (selectedIds.includes(taskId)) {
      onChange(selectedIds.filter((id) => id !== taskId));
      return;
    }
    if (selectedIds.length >= 6) return;
    onChange([...selectedIds, taskId]);
  };

  return (
    <div className="relpos" ref={wrapRef}>
      <button
        type="button"
        className={`ref-trigger ${prominent ? 'prominent' : ''} ${open || selectedIds.length > 0 ? 'on' : ''}`}
        title="引用历史对话或项目线程上下文"
        aria-label="引用对话"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <Link2 size={12.5} strokeWidth={1.8} />
        <span>{selectedIds.length > 0 ? `引用 ${selectedIds.length}` : prominent ? '引用对话' : '引用'}</span>
      </button>
      {open && (
        <div
          className="ref-pop"
          style={placement === 'up' ? { left: 0, bottom: 'calc(100% + 8px)' } : { left: 0, top: 'calc(100% + 6px)' }}
          role="dialog"
          aria-label="选择要引用的对话"
        >
          <div className="ref-pop-hd">
            <span>引用对话上下文</span>
            <span className="faint" style={{ fontSize: 11 }}>{selectedIds.length}/6</span>
          </div>
          <div className="ref-search-wrap">
            <Search size={12} strokeWidth={1.7} className="muted" />
            <input
              className="ref-search"
              placeholder="搜索项目名或对话标题…"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              autoFocus
            />
          </div>
          <div className="ref-list">
            {tasksQ.isLoading && <div className="ref-empty">载入对话列表…</div>}
            {!tasksQ.isLoading && candidates.length === 0 && (
              <div className="ref-empty">{query.trim() ? '没有匹配的对话' : '还没有可引用的对话'}</div>
            )}
            {candidates.map((t) => {
              const checked = selectedIds.includes(t.taskId);
              const proj = projectLabelForTask(t, projects);
              return (
                <button
                  key={t.taskId}
                  type="button"
                  className={`ref-item ${checked ? 'on' : ''}`}
                  disabled={!checked && selectedIds.length >= 6}
                  onClick={() => toggle(t.taskId)}
                >
                  <div className="ref-item-main">
                    <div className="ref-item-top">
                      <span className="ref-item-proj">{proj}</span>
                      <span className="ref-item-time">{relTime(t.updatedAt)}</span>
                    </div>
                    <div className="ref-item-ttl ell">{taskTitle(t)}</div>
                  </div>
                  {checked && <Check size={13} strokeWidth={2.2} className="ok-t" />}
                </button>
              );
            })}
          </div>
        </div>
      )}
    </div>
  );
}
