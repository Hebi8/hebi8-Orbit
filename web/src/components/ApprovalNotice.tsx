import { useMutation, useQueryClient } from '@tanstack/react-query';
import type { ApprovalView } from '../generated/protocol';
import { api, errMsg } from '../lib/api';
import { approvalPreview, approvalStateLabel, hhmm } from '../lib/format';
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
    onSuccess: (_res, v) => {
      const opt = v.approval.options.find((o) => o.id === v.optionId);
      toast(`已提交：${opt?.label ?? v.optionId}`);
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

/** Left-2px-rule approval notice used inline in the Session timeline. */
export function ApprovalNotice({ approval }: { approval: ApprovalView }) {
  if (approval.state !== 'pending') {
    return (
      <div className="nt resolved">
        <div className="nt-h">
          <span>{approvalStateLabel(approval.state)}</span>
          <span>·</span>
          <span className="ell">{approval.actionName}</span>
          {approval.decidedAt ? <span>· {hhmm(approval.decidedAt)}</span> : null}
        </div>
      </div>
    );
  }
  const cmd = approvalPreview(approval.payload);
  return (
    <div className="nt ap">
      <div className="nt-h">
        <span className="lbl">需确认</span>
        <span className="ell">{approval.actionName}</span>
        {approval.targetEnv && <span className="where mono">{approval.targetEnv}</span>}
      </div>
      {cmd && <code className="nt-cmd mono">{cmd}</code>}
      <div className="nt-a">
        <ApprovalButtons approval={approval} />
      </div>
    </div>
  );
}
