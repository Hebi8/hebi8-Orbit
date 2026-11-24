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
  return (
    <>
      <div className="drill-banner">
        <div className="in">
          <span className={`sd ${live ? 'run' : 'done'}`} />
          正在查看 <b>@{worker}</b> 的完整思考与工具流
          <span className="r">
            {m ? `${memberStateLabel(m.state)} · 消费游标 #${m.lastConsumedSeq}${m.machineId ? ` · ${m.machineId}` : ''}` : ''}
          </span>
        </div>
      </div>
      <div className="tl">
        {q.isLoading && (
          <div className="it running">
            <span className="spin" /> 载入 Worker 流…
          </div>
        )}
        {!q.isLoading && segs.length === 0 && <div className="empty">这个 Worker 还没有可展示的思考或工具记录</div>}
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
