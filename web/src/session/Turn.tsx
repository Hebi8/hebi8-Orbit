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

const COMPLETION_LABEL: Record<string, string> = {
  explicit_deliver: '显式交付',
  prompt_end_turn: '回合结束',
  quiet_timeout: '静默超时',
  ssh_verified: '部署已核验',
  manual_import: '手动导入',
};

function usePromptFor(attempt: AttemptView, tl: TaskTimelineResponse): { text: string; placeholder: boolean; detail: string[] } {
  const cache = useWorkbenchStore((s) => s.promptCache);
  return useMemo(() => {
    const task = tl.task;
    switch (attempt.triggerKind) {
      case 'initial':
      case 'workspace_import':
        return { text: task.goal, placeholder: false, detail: [] };
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
        const direct = cache[`att:${attempt.attemptId}`];
        if (direct) return { text: direct, placeholder: false, detail: [] };
        const ev = tl.events.find(
          (e) => e.kind === 'queue.updated' && e.payload?.action === 'dispatched' && e.payload?.dispatchedAttemptId === attempt.attemptId,
        );
        const inputId = typeof ev?.payload?.inputId === 'string' ? ev.payload.inputId : '';
        const viaQueue = inputId ? cache[`inp:${inputId}`] : '';
        if (viaQueue) return { text: viaQueue, placeholder: false, detail: [] };
        return { text: '追问（原文未随时间线下发，可在 Agent 原生会话中查看）', placeholder: true, detail: [] };
      }
      case 'retry':
        return { text: `重试 · ${task.goal.split('\n')[0]}`, placeholder: true, detail: [] };
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
    mutationFn: () => api.retryTask(taskId, { verifiedSite: needsVerify ? verified : undefined }),
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
        <span className="lbl">{needsVerify ? '回合中断 · 待核对现场' : attemptStateLabel(attempt.state)}</span>
        {attempt.errorCode && <span className="where mono">{attempt.errorCode}</span>}
        <span className="where">{attempt.machineId}</span>
      </div>
      <div className="nt-b">
        {needsVerify
          ? 'Runner 在回合中途失联，worktree 可能残留半截改动或进程。核对现场（未提交改动、运行中的进程）后再重试。'
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
  onOpenBoard: () => void;
}) {
  const live = isAttemptLive(attempt.state);
  const [expanded, setExpanded] = useState(false);
  const loadRaw = expanded || isLast || live;
  const raw = useAttemptRaw(attempt.attemptId, { enabled: loadRaw, live });
  const segs = useMemo(() => buildSegments(raw.data ?? []), [raw.data]);
  const prompt = usePromptFor(attempt, tl);
  const openWindow = useWorkbenchStore((s) => s.openWindow);

  const approvals = tl.approvals.filter((a) => a.attemptId === attempt.attemptId).sort((a, b) => a.createdAt - b.createdAt);
  const deliveryIdx = attempt.outputDeliveryId ? tl.deliveries.findIndex((d) => d.deliveryId === attempt.outputDeliveryId) : -1;
  const delivery = deliveryIdx >= 0 ? tl.deliveries[deliveryIdx] : undefined;
  const taskIdle = !tl.task.activeAttemptId;
  const step = live ? lastLiveStep(segs) : null;
  const showSwarm = attempt.state === 'swarm_monitoring' || (isLast && tl.boardMembers.length > 0);

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
        <div className="umeta">
          第 {attempt.attemptNo} 轮 · {triggerLabel(attempt.triggerKind)} · {attempt.machineId} · {attempt.adapter} · {hhmm(attempt.startedAt)}
        </div>
      </div>

      {attempt.state === 'waiting_resource' && (
        <div className="it nt">
          <div className="nt-h">
            <span className="lbl">等待资源</span>
            <span className="where">{attempt.machineId}</span>
          </div>
          <div className="nt-b">{tl.task.waitingReason || '目标机器的独占资源暂不可用，资源就绪后自动继续。'}</div>
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
            展开第 {attempt.attemptNo} 轮的思考、工具与回复
            <span className="faint" style={{ marginLeft: 'auto' }}>
              {attemptStateLabel(attempt.state)}
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
              <SwarmIcon size={13} /> Swarm 协作
            </span>
            <span className="smem">
              {tl.boardMembers.slice(0, 4).map((m) => (
                <span key={m.memberName}>
                  <span className={`sd ${m.state === 'working' ? 'run' : m.state === 'waiting' ? 'wait' : 'done'}`} />
                  {m.memberName} <span className="mono faint">#{m.lastConsumedSeq}</span>
                </span>
              ))}
            </span>
            <span className="go">打开消息板 →</span>
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
          <span className="sd rev" />
          <span>产出交付 v{deliveryIdx + 1}</span>
          <span className="mono faint">{shortSha(delivery.headSha)}</span>
          <span className="mono">
            <span className="add">+{delivery.manifest.insertions}</span> <span className="del">−{delivery.manifest.deletions}</span>
          </span>
          <button type="button" className="lk" onClick={() => openWindow('cr', delivery.taskId, delivery.deliveryId)}>
            <DiffIcon size={12} /> Code Review
          </button>
          {(delivery.ryugiBindings?.length ?? 0) > 0 && (
            <button type="button" className="lk" onClick={() => openWindow('art', delivery.taskId, delivery.deliveryId)}>
              <ArtIcon size={12} /> Ryugi
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
            ? `第 ${attempt.attemptNo} 轮结束 · ${hhmm(attempt.finishedAt)}${attempt.completionSource ? ` · ${COMPLETION_LABEL[attempt.completionSource] ?? attempt.completionSource}` : ''}`
            : `第 ${attempt.attemptNo} 轮 · ${attemptStateLabel(attempt.state)} · ${hhmm(attempt.finishedAt)}`}
        </div>
      )}
    </div>
  );
}
