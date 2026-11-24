import { useMemo, useRef } from 'react';
import { Plus } from 'lucide-react';
import type { BootstrapResponse, InboxItemView, ProjectView, TaskSummary } from '../generated/protocol';
import { PAIcon } from '../components/Icons';
import { PageHeader } from '../components/PageHeader';
import { StatusMark } from '../components/StatusMark';
import { useManyProjectTasks } from '../hooks/queries';
import {
  SCRATCH_PROJECT_ID,
  displayMarkRank,
  inboxItemsMark,
  indexInboxByTask,
  relTime,
  rootSessionMark,
  taskTitle,
  type SessionMark,
} from '../lib/format';
import { navigate, paths } from '../lib/router';

export function projectName(projects: ProjectView[], pid: string): string {
  if (pid === SCRATCH_PROJECT_ID) return '对话';
  return projects.find((p) => p.projectId === pid)?.displayName ?? pid;
}

export function useAutoGrow() {
  const ref = useRef<HTMLTextAreaElement | null>(null);
  const grow = () => {
    const el = ref.current;
    if (!el) return;
    el.style.height = 'auto';
    el.style.height = `${Math.min(el.scrollHeight, 200)}px`;
  };
  return { ref, grow };
}

interface HomeRowItem {
  key: string;
  taskId?: string;
  projectId: string;
  title: string;
  mark: SessionMark;
  updatedAt: number;
}

function buildHomeRows(
  boot: BootstrapResponse,
  tasksByProject: Record<string, TaskSummary[]>,
): HomeRowItem[] {
  const inboxByTask = indexInboxByTask(boot.openInboxItems);

  // Merge active tasks + loaded per-project tasks so resting "最近" threads also appear cleanly.
  const allById = new Map<string, TaskSummary>();
  for (const t of boot.activeTasks) allById.set(t.taskId, t);
  for (const list of Object.values(tasksByProject)) {
    for (const t of list) {
      if (!allById.has(t.taskId)) allById.set(t.taskId, t);
    }
  }
  const allTasks = [...allById.values()];
  const seen = new Set<string>();
  const rows: HomeRowItem[] = [];

  for (const t of allTasks) {
    if (t.parentTaskId) continue;
    const inbox = inboxByTask.get(t.taskId) ?? [];
    const mark = rootSessionMark(t, allTasks, inboxByTask);
    seen.add(t.taskId);
    rows.push({
      key: t.taskId,
      taskId: t.taskId,
      projectId: t.projectId,
      title: taskTitle(t),
      mark,
      updatedAt: Math.max(t.updatedAt, ...inbox.map((i) => i.updatedAt)),
    });
  }

  // Include any orphan inbox item whose task is not in allTasks.
  const orphanByKey = new Map<string, InboxItemView[]>();
  for (const it of boot.openInboxItems) {
    const k = it.taskId || it.inboxId;
    if (seen.has(k)) continue;
    const list = orphanByKey.get(k) ?? [];
    list.push(it);
    orphanByKey.set(k, list);
  }
  for (const [key, list] of orphanByKey.entries()) {
    const mark = inboxItemsMark(list);
    if (!mark) continue;
    const primary = list[0];
    rows.push({
      key,
      taskId: primary.taskId,
      projectId: primary.projectId,
      title: primary.title,
      mark,
      updatedAt: Math.max(...list.map((i) => i.updatedAt)),
    });
  }

  rows.sort((a, b) => displayMarkRank(a.mark) - displayMarkRank(b.mark) || b.updatedAt - a.updatedAt);
  return rows;
}

function HomeRow({ row, projects }: { row: HomeRowItem; projects: ProjectView[] }) {
  const toneClass =
    row.mark === 'failed'
      ? 'failed'
      : row.mark === 'needs-you'
        ? 'needs'
        : row.mark === null
          ? 'is-resting'
          : '';
  return (
    <button
      type="button"
      className={`t-row ${toneClass}`}
      disabled={!row.taskId}
      onClick={() => row.taskId && navigate(paths.session(row.taskId))}
    >
      <span className="t-title">{row.title}</span>
      <span className="t-proj">{projectName(projects, row.projectId)}</span>
      <span className="t-right">
        <span className="t-time">{relTime(row.updatedAt)}</span>
        <StatusMark mark={row.mark} />
      </span>
    </button>
  );
}

/**
 * Compact title-only Home overview:
 * Shows status and intervention priority cleanly — no second-line gists and no direct-to-artifact jumps.
 */
export function HomePage({ boot }: { boot: BootstrapResponse }) {
  const tasksByProject = useManyProjectTasks(boot.projects);
  const rows = useMemo(() => buildHomeRows(boot, tasksByProject), [boot, tasksByProject]);

  const urgent = rows.filter((r) => r.mark === 'failed' || r.mark === 'needs-you');
  const ready = rows.filter((r) => r.mark === 'ready');
  const running = rows.filter((r) => r.mark === 'running');
  const recent = rows.filter((r) => r.mark === null).slice(0, 8);

  const sections: { key: string; title: string; items: HomeRowItem[] }[] = [
    { key: 'urgent', title: '等你处理', items: urgent },
    { key: 'ready', title: '刚完成 · 未读结果', items: ready },
    { key: 'running', title: '进行中', items: running },
    { key: 'recent', title: '最近', items: recent },
  ].filter((s) => s.items.length > 0);

  return (
    <main className="main">
      <PageHeader>
        <span className="ttl">总览</span>
      </PageHeader>
      <div className="page-scroll">
        <div className="page home-page">
          <div className="home-hd">
            <h1 className="home-title">总览</h1>
            <div className="home-acts">
              <button type="button" className="btn ghost" onClick={() => navigate(paths.pa())}>
                <PAIcon size={13} /> 问 PA
              </button>
              <button type="button" className="btn pri" onClick={() => navigate(paths.newThread())}>
                <Plus size={13} strokeWidth={1.8} /> 新建线程
              </button>
            </div>
          </div>

          {sections.length === 0 ? (
            <div className="quiet-alone">都安静着</div>
          ) : (
            sections.map((sec) => (
              <section className="sec" key={sec.key} aria-label={sec.title}>
                <h2 className="sec-hd">{sec.title}</h2>
                <div className="t-list">
                  {sec.items.map((r) => (
                    <HomeRow key={r.key} row={r} projects={boot.projects} />
                  ))}
                </div>
              </section>
            ))
          )}
        </div>
      </div>
    </main>
  );
}
