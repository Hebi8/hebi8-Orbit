import { useMutation, useQueryClient } from '@tanstack/react-query';
import { ArrowUpRight, ChevronRight, FolderGit2, MessagesSquare } from 'lucide-react';
import type {
  MachineView,
  PAActionResult,
  PABriefResponse,
  PAChatResponse,
  ProjectView,
} from '../generated/protocol';
import { api, errMsg } from '../lib/api';
import {
  cleanMachineName,
  displayMarkRank,
  inboxItemsMark,
  indexInboxByTask,
  relTime,
  sessionMark,
  shortSha,
  taskTitle,
  type SessionMark,
} from '../lib/format';
import { navigate, paths } from '../lib/router';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';
import { Md } from './Md';
import { StatusMark } from './StatusMark';

export interface CrossDirSessionItem {
  key: string;
  taskId?: string;
  projectId?: string;
  title: string;
  projectLabel: string;
  machineLabel?: string;
  mark: SessionMark;
  hint?: string;
}

/**
 * Dedicated Hebi8 / PA cross-directory session jump cards:
 * Displays StatusMark, title, `project · machine` badge, and concise explanation body.
 * Clicking navigates directly to the target directory session (never uses parent/child ↗/↵ rails).
 */
export function CrossDirSessionCards({ items }: { items: CrossDirSessionItem[] }) {
  if (items.length === 0) return null;
  return (
    <div className="it h8-cross-list" aria-label="跨目录会话提示">
      {items.map((item) => {
        const target = item.taskId
          ? paths.session(item.taskId)
          : item.projectId
            ? paths.project(item.projectId)
            : '';
        const dirTag = [item.projectLabel, item.machineLabel].filter(Boolean).join(' · ');
        return (
          <button
            key={item.key}
            type="button"
            className="h8-cross-card"
            disabled={!target}
            onClick={() => target && navigate(target)}
            title={item.hint ? `${item.title} — ${item.hint}` : item.title}
          >
            <div className="h8-cross-top">
              {item.mark && <StatusMark mark={item.mark} />}
              <span className="h8-cross-ttl">{item.title}</span>
              {dirTag && <span className="h8-cross-dir">{dirTag}</span>}
              {target && <ChevronRight size={13} strokeWidth={1.7} className="h8-cross-chev" aria-hidden="true" />}
            </div>
            {item.hint && <div className="h8-cross-hint">{item.hint}</div>}
          </button>
        );
      })}
    </div>
  );
}

export function usePAChat(onDone?: (res: PAChatResponse) => void) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { message: string; threadId?: string }) => api.paChat({ message: v.message, threadId: v.threadId }),
    onSuccess: (res) => {
      void qc.invalidateQueries({ queryKey: ['pa-threads'] });
      void qc.invalidateQueries({ queryKey: ['pa-thread', res.thread.paThreadId] });
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
      onDone?.(res);
    },
    onError: (err) => toast(errMsg(err), 'err'),
  });
}

export function usePABrief(onDone?: (res: PABriefResponse) => void) {
  return useMutation({
    mutationFn: () => api.paBrief({ sinceHours: 24 }),
    onSuccess: (res) => onDone?.(res),
    onError: (err) => toast(errMsg(err), 'err'),
  });
}

export function ActionCards({
  actions,
  projects,
  machines,
}: {
  actions?: PAActionResult[];
  projects?: ProjectView[];
  machines?: MachineView[];
}) {
  if (!actions || actions.length === 0) return null;
  const projectMap = new Map((projects ?? []).map((p) => [p.projectId, p]));
  const machineMap = new Map((machines ?? []).map((m) => [m.machineId, m]));

  const items: CrossDirSessionItem[] = [];
  const plain: PAActionResult[] = [];
  actions.forEach((a, i) => {
    const taskId = typeof a.data?.taskId === 'string' ? a.data.taskId : '';
    const projectId = typeof a.data?.projectId === 'string' ? a.data.projectId : '';
    if (taskId || projectId) {
      const proj = projectId ? projectMap.get(projectId) : undefined;
      const mid = typeof a.data?.machineId === 'string' ? a.data.machineId : '';
      const mach = mid ? machineMap.get(mid) : undefined;
      items.push({
        key: `${a.action}-${i}`,
        taskId: taskId || undefined,
        projectId: projectId || undefined,
        title: (typeof a.data?.title === 'string' && a.data.title) || a.summary,
        projectLabel: proj?.displayName || projectId || '项目会话',
        machineLabel: mach ? mach.displayAlias || cleanMachineName(mach) : mid || undefined,
        mark: a.status === 'error' ? 'failed' : null,
        hint: typeof a.data?.title === 'string' && a.data.title !== a.summary ? a.summary : undefined,
      });
    } else {
      plain.push(a);
    }
  });

  return (
    <>
      {items.length > 0 && <CrossDirSessionCards items={items} />}
      {plain.length > 0 && (
        <div className="acards">
          {plain.map((a, i) => (
            <div key={`${a.action}-${i}`} className="acard" title={a.summary}>
              {a.action.includes('project') ? (
                <FolderGit2 size={13} strokeWidth={1.6} />
              ) : (
                <MessagesSquare size={13} strokeWidth={1.6} />
              )}
              <span className="ell">{a.summary}</span>
            </div>
          ))}
        </div>
      )}
    </>
  );
}

interface BriefRow {
  key: string;
  taskId?: string;
  title: string;
  project: string;
  mark: SessionMark;
}

export function BriefView({ brief, projects }: { brief: PABriefResponse; projects: ProjectView[] }) {
  const pname = (pid: string) => projects.find((p) => p.projectId === pid)?.displayName ?? pid;
  const openReview = useWorkbenchStore((s) => s.openReview);

  const inboxByTask = indexInboxByTask(brief.openInboxItems);
  const seen = new Set<string>();
  const rows: BriefRow[] = [];

  for (const t of [...brief.blockedTasks, ...brief.activeTasks]) {
    if (seen.has(t.taskId)) continue;
    const mark = sessionMark(t, inboxByTask.get(t.taskId));
    if (!mark) continue;
    seen.add(t.taskId);
    rows.push({ key: t.taskId, taskId: t.taskId, title: taskTitle(t), project: pname(t.projectId), mark });
  }
  for (const it of brief.openInboxItems) {
    const key = it.taskId || it.inboxId;
    if (seen.has(key)) continue;
    const mark = inboxItemsMark([it]);
    if (!mark) continue;
    seen.add(key);
    rows.push({ key, taskId: it.taskId, title: it.title, project: pname(it.projectId), mark });
  }
  rows.sort((a, b) => displayMarkRank(a.mark) - displayMarkRank(b.mark));

  return (
    <div className="brief">
      <Md text={brief.summary} />
      {rows.length > 0 && (
        <ul>
          {rows.slice(0, 10).map((r) => (
            <li key={r.key}>
              <button
                type="button"
                className="lk"
                onClick={() => navigate(r.taskId ? paths.session(r.taskId) : paths.home())}
              >
                {r.title}
              </button>
              <span className="faint ell">{r.project}</span>
              <span style={{ marginLeft: 'auto', flex: 'none' }}>
                <StatusMark mark={r.mark} />
              </span>
            </li>
          ))}
        </ul>
      )}
      {brief.recentDeliveries.length > 0 && (
        <>
          <h5>新产出</h5>
          <ul>
            {brief.recentDeliveries.slice(0, 5).map((d) => (
              <li key={d.deliveryId}>
                <button type="button" className="lk" onClick={() => openReview('code', d.taskId, d.deliveryId)}>
                  {d.summary || shortSha(d.headSha)}
                </button>
                <span className="faint" style={{ marginLeft: 'auto', flex: 'none' }}>
                  {relTime(d.createdAt)}
                </span>
              </li>
            ))}
          </ul>
        </>
      )}
      {rows.length === 0 && brief.recentDeliveries.length === 0 && (
        <div className="faint" style={{ fontSize: 12, marginTop: 6 }}>
          暂无待处理事项 <ArrowUpRight size={11} style={{ display: 'inline', verticalAlign: '-1px' }} />
        </div>
      )}
    </div>
  );
}
