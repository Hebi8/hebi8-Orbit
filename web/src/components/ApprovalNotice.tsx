import { useMutation, useQueryClient } from '@tanstack/react-query';
import type { ApprovalView } from '../generated/protocol';
import { api, errMsg } from '../lib/api';
import { approvalPreview, approvalStateLabel, hhmm, shortSha } from '../lib/format';
import { toast } from '../store/useWorkbenchStore';

function isAllowKind(kind: string, label: string): boolean {
  const s = `${kind} ${label}`.toLowerCase();
  return /(allow|approve|accept|yes|批准|允许)/.test(s) && !/(reject|deny|拒绝)/.test(s);
}

export function useDecideApproval() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { approval: ApprovalView; optionId: string }) =>
      api.decideApproval(v.approval.approvalId, {
        optionId: v.optionId,
        expectedAttemptId: v.approval.attemptId,
        expectedPayloadSha256: v.approval.payloadSha256,
      }),
    onSuccess: (res, v) => {
      const opt = v.approval.options.find((o) => o.id === v.optionId);
      toast(`已提交决定：${opt?.label ?? v.optionId}${res.sentToRunner ? ' · 已送达 Runner' : ''}`);
      void qc.invalidateQueries({ queryKey: ['approvals'] });
      void qc.invalidateQueries({ queryKey: ['task-timeline', v.approval.taskId] });
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
    },
    onError: (err) => toast(errMsg(err), 'err'),
  });
}

/** The approval option buttons, honoring the human-only decide contract (expected attempt + payload sha). */
export function ApprovalButtons({ approval, big }: { approval: ApprovalView; big?: boolean }) {
  const decide = useDecideApproval();
  const opts = approval.options.length
    ? approval.options
    : [
        { id: 'allow', label: '允许一次', kind: 'allow_once' },
        { id: 'reject', label: '拒绝', kind: 'reject_once' },
      ];
  return (
    <>
      {opts.map((o) => {
        const allow = isAllowKind(o.kind, o.label);
        return (
          <button
            key={o.id}
            type="button"
            className={`btn ${allow ? 'pri' : ''} ${big ? 'big' : ''}`}
            disabled={decide.isPending}
            onClick={() => decide.mutate({ approval, optionId: o.id })}
          >
            {o.label}
          </button>
        );
      })}
    </>
  );
}

export function ApprovalFacts({ approval }: { approval: ApprovalView }) {
  const cmd = approvalPreview(approval.payload);
  return (
    <div className="facts">
      <span className="k">动作</span>
      <span className="v">{approval.actionName}</span>
      {approval.targetEnv && (
        <>
          <span className="k">目标环境</span>
          <span className="v mono">{approval.targetEnv}</span>
        </>
      )}
      {approval.inputSha && (
        <>
          <span className="k">SHA</span>
          <span className="v mono">{shortSha(approval.inputSha)}</span>
        </>
      )}
      {cmd && (
        <>
          <span className="k">命令</span>
          <span className="v mono">{cmd}</span>
        </>
      )}
      <span className="k">有效期至</span>
      <span className="v">{hhmm(approval.expiresAt)}</span>
    </div>
  );
}

/** Left-2px-rule approval notice used inline in the Session timeline. */
export function ApprovalNotice({ approval }: { approval: ApprovalView }) {
  if (approval.state !== 'pending') {
    return (
      <div className="nt resolved">
        <div className="nt-h">
          <span>{approvalStateLabel(approval.state)}</span>
          <span>·</span>
          <span className="ell">{approval.actionName}</span>
          {approval.decidedBy && <span>· {approval.decidedBy === 'human' ? '你' : '策略'}</span>}
          {approval.decidedAt ? <span>· {hhmm(approval.decidedAt)}</span> : null}
        </div>
      </div>
    );
  }
  const cmd = approvalPreview(approval.payload);
  return (
    <div className="nt ap">
      <div className="nt-h">
        <span className="lbl">等待确认</span>
        <span className="ell">{approval.actionName}</span>
        {approval.targetEnv && <span className="where mono">{approval.targetEnv}</span>}
        {approval.inputSha && <span className="where mono">{shortSha(approval.inputSha)}</span>}
      </div>
      {cmd && <code className="nt-cmd mono">{cmd}</code>}
      <div className="nt-a">
        <ApprovalButtons approval={approval} />
        <span className="faint" style={{ fontSize: 11 }}>
          {approval.requestedBy === 'pa' ? 'PA 起草' : 'Agent 请求'} · {hhmm(approval.createdAt)} · 有效至 {hhmm(approval.expiresAt)}
        </span>
      </div>
    </div>
  );
}
