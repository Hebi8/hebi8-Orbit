import { useMutation, useQueryClient } from '@tanstack/react-query';
import { ArrowUpRight, FolderGit2, MessagesSquare } from 'lucide-react';
import type { PAActionResult, PABriefResponse, PAChatResponse, ProjectView } from '../generated/protocol';
import { api, errMsg } from '../lib/api';
import { relTime, shortSha, taskDot, taskStateLabel, taskTitle } from '../lib/format';
import { navigate, paths } from '../lib/router';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';
import { Md } from './Md';

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
            <span className="sub">{a.status}</span>
            {target && <ArrowUpRight size={12} strokeWidth={1.6} />}
          </button>
        );
      })}
    </div>
  );
}

export function BriefView({ brief, projects }: { brief: PABriefResponse; projects: ProjectView[] }) {
  const pname = (pid: string) => projects.find((p) => p.projectId === pid)?.displayName ?? pid;
  const openWindow = useWorkbenchStore((s) => s.openWindow);
  return (
    <div className="brief">
      <Md text={brief.summary} />
      {brief.openInboxItems.length > 0 && (
        <>
          <h5>待拍板 · {brief.openInboxCount}</h5>
          <ul>
            {brief.openInboxItems.slice(0, 6).map((it) => (
              <li key={it.inboxId}>
                <span className="sd wait" />
                <button type="button" className="lk" onClick={() => navigate(it.taskId ? paths.session(it.taskId) : paths.home())}>
                  {it.title}
                </button>
                <span className="faint ell">{pname(it.projectId)}</span>
              </li>
            ))}
          </ul>
        </>
      )}
      {brief.activeTasks.length > 0 && (
        <>
          <h5>正在跑 · {brief.activeTasks.length}</h5>
          <ul>
            {brief.activeTasks.slice(0, 6).map((t) => (
              <li key={t.taskId}>
                <span className={`sd ${taskDot(t.state, t.activeAttemptState)}`} />
                <button type="button" className="lk" onClick={() => navigate(paths.session(t.taskId))}>
                  {taskTitle(t)}
                </button>
                <span className="faint ell">{taskStateLabel(t)}</span>
              </li>
            ))}
          </ul>
        </>
      )}
      {brief.blockedTasks.length > 0 && (
        <>
          <h5>阻塞 / 待核对 · {brief.blockedTasks.length}</h5>
          <ul>
            {brief.blockedTasks.slice(0, 6).map((t) => (
              <li key={t.taskId}>
                <span className="sd block" />
                <button type="button" className="lk" onClick={() => navigate(paths.session(t.taskId))}>
                  {taskTitle(t)}
                </button>
                <span className="faint ell">{t.waitingReason || taskStateLabel(t)}</span>
              </li>
            ))}
          </ul>
        </>
      )}
      {brief.recentDeliveries.length > 0 && (
        <>
          <h5>近期交付 · {brief.recentDeliveries.length}</h5>
          <ul>
            {brief.recentDeliveries.slice(0, 5).map((d) => (
              <li key={d.deliveryId}>
                <span className="sd rev" />
                <button type="button" className="lk" onClick={() => openWindow('cr', d.taskId, d.deliveryId)}>
                  {d.summary || shortSha(d.headSha)}
                </button>
                <span className="faint mono">
                  <span className="add">+{d.manifest.insertions}</span> <span className="del">−{d.manifest.deletions}</span>
                </span>
                <span className="faint">{relTime(d.createdAt)}</span>
              </li>
            ))}
          </ul>
        </>
      )}
    </div>
  );
}
