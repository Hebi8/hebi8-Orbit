import { useMemo } from 'react';
import type { BoardMemberCursor } from '../generated/protocol';
import { useWorkerStream } from '../hooks/queries';
import { memberStateLabel } from '../lib/format';
import { buildSegments, lastLiveStep } from '../lib/segments';
import { Segments } from './Segments';

/** Drill-down: reuses the main thin timeline vocabulary for one worker's full thought & tool stream. */
export function WorkerStream({ taskId, worker, member }: { taskId: string; worker: string; member?: BoardMemberCursor }) {
  const live = member?.state === 'working';
  const q = useWorkerStream(taskId, worker, live);
  const m = q.data?.member ?? member;
  const segs = useMemo(() => buildSegments(q.data?.events ?? []), [q.data?.events]);
  const step = live ? lastLiveStep(segs) : null;
  const hasPrivateStream = q.data ? q.data.privateStreamAvailable : true;
  const streamNotice = q.data?.streamNotice;
  return (
    <>
      <div className="drill-banner">
        <div className="in">
          <span className={`sd ${live ? 'run' : 'done'}`} />
          正在查看 <b>@{worker}</b> {hasPrivateStream ? '的完整思考与工具流' : '的共享消息板与投影记录'}
          <span className="r">
            {m ? `${memberStateLabel(m.state)} · 消费游标 #${m.lastConsumedSeq}${m.machineId ? ` · ${m.machineId}` : ''}` : ''}
          </span>
        </div>
      </div>
      <div className="tl">
        {!q.isLoading && !hasPrivateStream && (
          <div className="it nt">
            <div className="nt-h">
              <span className="lbl">原生 Swarm 成员视图</span>
              <span className="where">无独立私有流</span>
            </div>
            <div className="nt-b">
              {streamNotice || '原生 Swarm worker 的内部私有思考流由 agy 进程内部托管，当前仅展示共享消息板发言与协调者工具调用投影。'}
            </div>
          </div>
        )}
        {q.isLoading && (
          <div className="it running">
            <span className="spin" /> 载入 Worker 流…
          </div>
        )}
        {!q.isLoading && segs.length === 0 && (
          <div className="empty">
            {hasPrivateStream ? '这个 Worker 还没有可展示的思考或工具记录' : '尚未收到该成员在消息板或工具调用中的公开记录'}
          </div>
        )}
        <Segments segs={segs} live={!!live} />
        {live && (
          <div className="it running">
            <span className="spin" />
            <span className="shim ell">{step ? `${step.verb} · ${step.title}` : m?.currentStep || '工作中'}</span>
          </div>
        )}
      </div>
    </>
  );
}
