import { useMemo, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import type { AttemptView, TaskTimelineResponse } from '../generated/protocol';
import { ApprovalNotice } from '../components/ApprovalNotice';
import { ArtIcon, CaretIcon, DiffIcon, SwarmIcon } from '../components/Icons';
import { useAttemptRaw } from '../hooks/queries';
import { api, errMsg } from '../lib/api';
import { attemptStateLabel, hhmm, isAttemptLive, shortSha, triggerLabel } from '../lib/format';
import { buildSegments, lastLiveStep } from '../lib/segments';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';
import { Segments } from './Segments';

function usePromptFor(attempt: AttemptView, tl: TaskTimelineResponse): { text: string; placeholder: boolean; detail: string[] } {
  const cache = useWorkbenchStore((s) => s.promptCache);
  return useMemo(() => {
    const task = tl.task;
    const serverPrompt = attempt.promptText?.trim() ?? '';
    switch (attempt.triggerKind) {
      case 'initial':
      case 'workspace_import':
        return { text: serverPrompt || task.goal, placeholder: false, detail: [] };
      case 'annotation_batch': {
        const anns = tl.annotations.filter((a) => a.triggeredAttemptId === attempt.attemptId);
        return {
          text: `批注包 · ${anns.length || '若干'} 条`,
          placeholder: false,
          detail: anns.map((a) => {
            const loc = a.anchor.path ? `${a.anchor.path}${a.anchor.startLine ? `:${a.anchor.startLine}` : ''}` : a.anchor.artifact || a.source;
            return `${loc} — ${a.body}`;
          }),
        };
      }
      case 'followup': {
        if (serverPrompt) return { text: serverPrompt, placeholder: false, detail: [] };
        const direct = cache[`att:${attempt.attemptId}`];
        if (direct) return { text: direct, placeholder: false, detail: [] };
        const ev = tl.events.find(
          (e) => e.kind === 'queue.updated' && e.payload?.action === 'dispatched' && e.payload?.dispatchedAttemptId === attempt.attemptId,
        );
        const inputId = typeof ev?.payload?.inputId === 'string' ? ev.payload.inputId : '';
        const viaQueue = inputId ? cache[`inp:${inputId}`] : '';
        if (viaQueue) return { text: viaQueue, placeholder: false, detail: [] };
        return { text: '追问', placeholder: true, detail: [] };
      }
      case 'retry':
        return { text: serverPrompt || `重试 · ${task.goal.split('\n')[0]}`, placeholder: !serverPrompt, detail: [] };
      case 'manual_import': {
        const d = tl.deliveries.find((x) => x.deliveryId === attempt.outputDeliveryId);
        return { text: `导入交付${d?.summary ? ` · ${d.summary}` : ''}`, placeholder: true, detail: [] };
      }
      default:
        return { text: triggerLabel(attempt.triggerKind), placeholder: true, detail: [] };
    }
  }, [attempt, tl, cache]);
}

function RetryNotice({ attempt, taskId, needsVerify }: { attempt: AttemptView; taskId: string; needsVerify: boolean }) {
  const qc = useQueryClient();
  const [verified, setVerified] = useState(false);
  const retry = useMutation({
    mutationFn: () =>
      api.retryTask(taskId, {
        verifiedSite: needsVerify ? verified : undefined,
        verifiedAttemptId: needsVerify ? attempt.attemptId : undefined,
      }),
    onSuccess: () => {
      toast('已发起重试');
      void qc.invalidateQueries({ queryKey: ['task-timeline', taskId] });
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });
  return (
    <div className={`it nt ${needsVerify ? '' : 'bad'}`}>
      <div className="nt-h">
        <span className="lbl">{needsVerify ? '中断 · 需核对现场' : attemptStateLabel(attempt.state)}</span>
        {attempt.errorCode && <span className="where mono">{attempt.errorCode}</span>}
      </div>
      <div className="nt-b">
        {needsVerify
          ? 'Runner 中途失联，工作区可能残留未提交改动，核对现场后再重试。'
          : attempt.errorDetail || '本轮未能完成。'}
        {needsVerify && attempt.errorDetail ? ` · ${attempt.errorDetail}` : ''}
      </div>
      <div className="nt-a">
        {needsVerify && (
          <label className="chk">
            <input type="checkbox" checked={verified} onChange={(e) => setVerified(e.target.checked)} />
            我已核对现场
          </label>
        )}
        <button type="button" className="btn pri" disabled={retry.isPending || (needsVerify && !verified)} onClick={() => retry.mutate()}>
          重试
        </button>
      </div>
    </div>
  );
}

export function Turn({
  attempt,
  index,
  isLast,
  tl,
  onOpenBoard,
}: {
  attempt: AttemptView;
  index: number;
  isLast: boolean;
  tl: TaskTimelineResponse;
  onOpenBoard?: () => void;
}) {
  const live = isAttemptLive(attempt.state);
  const [expanded, setExpanded] = useState(false);
  const loadRaw = expanded || isLast || live;
  const raw = useAttemptRaw(attempt.attemptId, { enabled: loadRaw, live });
  const segs = useMemo(() => buildSegments(raw.data ?? []), [raw.data]);
  const prompt = usePromptFor(attempt, tl);
  const openReview = useWorkbenchStore((s) => s.openReview);

  const approvals = tl.approvals.filter((a) => a.attemptId === attempt.attemptId).sort((a, b) => a.createdAt - b.createdAt);
  const deliveryIdx = attempt.outputDeliveryId ? tl.deliveries.findIndex((d) => d.deliveryId === attempt.outputDeliveryId) : -1;
  const delivery = deliveryIdx >= 0 ? tl.deliveries[deliveryIdx] : undefined;
  const taskIdle = !tl.task.activeAttemptId;
  const step = live ? lastLiveStep(segs) : null;
  const showSwarm = !!onOpenBoard && (attempt.state === 'swarm_monitoring' || (isLast && tl.boardMembers.length > 0));

  return (
    <div className="turn" id={`turn-${attempt.attemptId}`} data-turn={index}>
      <div className="it u">
        <div className={`ub ${prompt.placeholder ? 'phd' : ''}`}>{prompt.text}</div>
        {prompt.detail.length > 0 && (
          <div className="udetail">
            {prompt.detail.slice(0, 8).map((d, i) => (
              <div key={i} className="ell">
                {d}
              </div>
            ))}
            {prompt.detail.length > 8 && <div className="faint">… 另 {prompt.detail.length - 8} 条</div>}
          </div>
        )}
        <div className="umeta">{hhmm(attempt.startedAt)}</div>
      </div>

      {attempt.state === 'waiting_resource' && (
        <div className="it nt">
          <div className="nt-h">
            <span className="lbl">等待资源</span>
          </div>
          <div className="nt-b">{tl.task.waitingReason || '目标资源暂不可用，就绪后自动继续。'}</div>
        </div>
      )}

      {loadRaw ? (
        raw.isLoading ? (
          <div className="it running">
            <span className="spin" /> 载入过程…
          </div>
        ) : (
          <Segments segs={segs} live={live} />
        )
      ) : (
        <div className="it">
          <button type="button" className="lazy-turn" onClick={() => setExpanded(true)}>
            <CaretIcon className="i" />
            展开历史过程
            <span className="faint" style={{ marginLeft: 'auto' }}>
              {attempt.state === 'succeeded' ? '已结束' : attemptStateLabel(attempt.state)}
            </span>
          </button>
        </div>
      )}

      {approvals.map((ap) => (
        <div className="it" key={ap.approvalId}>
          <ApprovalNotice approval={ap} />
        </div>
      ))}

      {showSwarm && (
        <div className="it">
          <button type="button" className="swr" onClick={onOpenBoard}>
            <span className="lead">
              <SwarmIcon size={13} /> 协作
            </span>
            <span className="smem">
              {tl.boardMembers.slice(0, 4).map((m) => (
                <span key={m.memberName}>
                  <span className={`sd ${m.state === 'working' ? 'run' : m.state === 'waiting' ? 'wait' : 'done'}`} />
                  {m.memberName}
                </span>
              ))}
            </span>
            <span className="go">消息板 →</span>
          </button>
        </div>
      )}

      {live && attempt.state !== 'waiting_approval' && attempt.state !== 'waiting_resource' && (
        <div className="it running">
          <span className="spin" />
          <span className="shim ell">{step ? `${step.verb} · ${step.title}` : attemptStateLabel(attempt.state)}</span>
        </div>
      )}

      {delivery && (
        <div className="it dline">
          <span className="sd done" />
          <span>产出 v{deliveryIdx + 1}</span>
          <span className="mono faint">{shortSha(delivery.headSha)}</span>
          {delivery.manifest.filesChanged > 0 && (
            <span className="mono">
              <span className="add">+{delivery.manifest.insertions}</span> <span className="del">−{delivery.manifest.deletions}</span>
            </span>
          )}
          <button type="button" className="lk" onClick={() => openReview('code', delivery.taskId, delivery.deliveryId)}>
            <DiffIcon size={12} /> 审阅代码
          </button>
          {(delivery.ryugiBindings?.length ?? 0) > 0 && (
            <button type="button" className="lk" onClick={() => openReview('art', delivery.taskId, delivery.deliveryId)}>
              <ArtIcon size={12} /> 设计稿
            </button>
          )}
        </div>
      )}

      {isLast && taskIdle && (attempt.state === 'needs_verification' || attempt.state === 'failed' || attempt.state === 'expired') && (
        <RetryNotice attempt={attempt} taskId={tl.task.taskId} needsVerify={attempt.state === 'needs_verification'} />
      )}

      {!live && (
        <div className="it done-line">
          {attempt.state === 'succeeded'
            ? `已结束 · ${hhmm(attempt.finishedAt)}`
            : `${attemptStateLabel(attempt.state)} · ${hhmm(attempt.finishedAt)}`}
        </div>
      )}
    </div>
  );
}
