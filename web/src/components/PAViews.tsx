import { useMutation, useQueryClient } from '@tanstack/react-query';
import { ArrowUpRight, FolderGit2, MessagesSquare } from 'lucide-react';
import type { PAActionResult, PABriefResponse, PAChatResponse, ProjectView } from '../generated/protocol';
import { api, errMsg } from '../lib/api';
import {
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

export function ActionCards({ actions }: { actions?: PAActionResult[] }) {
  if (!actions || actions.length === 0) return null;
  return (
    <div className="acards">
      {actions.map((a, i) => {
        const taskId = typeof a.data?.taskId === 'string' ? a.data.taskId : '';
        const projectId = typeof a.data?.projectId === 'string' ? a.data.projectId : '';
        const target = taskId ? paths.session(taskId) : projectId ? paths.project(projectId) : '';
        return (
          <button
            key={`${a.action}-${i}`}
            type="button"
            className="acard"
            disabled={!target}
            onClick={() => target && navigate(target)}
            title={a.summary}
          >
            {taskId ? <MessagesSquare size={13} strokeWidth={1.6} /> : <FolderGit2 size={13} strokeWidth={1.6} />}
            <span className="ell">{a.summary}</span>
            {target && <ArrowUpRight size={12} strokeWidth={1.6} />}
          </button>
        );
      })}
    </div>
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
    </div>
  );
}
