import { useCallback, useEffect, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Check, Copy, RefreshCw, X } from 'lucide-react';
import type { CreatePairingCodeResponse, MachineView, PairingCodeStatusResponse } from '../generated/protocol';
import { api, errMsg } from '../lib/api';
import { toast } from '../store/useWorkbenchStore';
import { AgentList } from './AgentList';

function formatCountdown(ms: number): string {
  const total = Math.max(0, Math.ceil(ms / 1000));
  const m = Math.floor(total / 60);
  const s = total % 60;
  return `${m}:${String(s).padStart(2, '0')}`;
}

export function PairDialog({
  open,
  bindMachineId,
  machines,
  onClose,
}: {
  open: boolean;
  bindMachineId?: string;
  machines: MachineView[];
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const [asRunner, setAsRunner] = useState(true);
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState('');
  const [codeResp, setCodeResp] = useState<CreatePairingCodeResponse | null>(null);
  const [status, setStatus] = useState<PairingCodeStatusResponse | null>(null);
  const [now, setNow] = useState(() => Date.now());

  const createCode = useCallback(
    async (wantRunner: boolean) => {
      setCreating(true);
      setError('');
      setStatus(null);
      try {
        const res = await api.createPairingCode({
          scope: wantRunner || bindMachineId ? 'both' : 'client',
          bindMachineId: bindMachineId || undefined,
          ttlSec: 600,
        });
        setCodeResp(res);
        setNow(Date.now());
      } catch (e) {
        setError(errMsg(e));
      } finally {
        setCreating(false);
      }
    },
    [bindMachineId],
  );

  useEffect(() => {
    if (!open) {
      setCodeResp(null);
      setStatus(null);
      setError('');
      return;
    }
    setAsRunner(true);
    void createCode(true);
  }, [open, createCode]);

  useEffect(() => {
    if (!open || !codeResp) return;
    const tick = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(tick);
  }, [open, codeResp]);

  const remainingMs = codeResp ? codeResp.expiresAt - now : 0;
  const expired = status?.state === 'expired' || (codeResp !== null && remainingMs <= 0 && status?.state !== 'claimed');
  const claimed = status?.state === 'claimed';
  const pairedMachine = status?.machineId
    ? machines.find((m) => m.machineId === status.machineId)
    : undefined;
  const isClientOnlyClaim = claimed && (codeResp?.scope === 'client' || !status?.machineId);
  const isOnline = claimed && (status?.online || pairedMachine?.status === 'online' || isClientOnlyClaim);

  useEffect(() => {
    if (!open || !codeResp || isOnline || expired) return;
    let active = true;
    let refreshedOnce = false;
    const poll = async () => {
      try {
        const st = await api.getPairingCode(codeResp.id);
        if (!active) return;
        setStatus(st);
        if (st.state === 'claimed' && (!refreshedOnce || st.online)) {
          refreshedOnce = true;
          void qc.invalidateQueries({ queryKey: ['bootstrap'] });
          void qc.invalidateQueries({ queryKey: ['machines'] });
        }
      } catch {
        // transient poll error ignored
      }
    };
    const timer = window.setInterval(() => void poll(), 2000);
    void poll();
    return () => {
      active = false;
      window.clearInterval(timer);
    };
  }, [open, codeResp, isOnline, expired, qc]);

  if (!open) return null;

  const copyCommand = async () => {
    if (!codeResp) return;
    try {
      await navigator.clipboard.writeText(codeResp.joinCommand);
      toast('已复制命令');
    } catch {
      toast(codeResp.joinCommand);
    }
  };

  const handleClose = async () => {
    if (codeResp && !claimed && !expired) {
      try {
        await api.revokePairingCode(codeResp.id);
      } catch {
        // ignore revoke failure on close
      }
    }
    onClose();
  };

  return (
    <div className="modal-scrim" onClick={() => void handleClose()}>
      <section
        className="modal-card pair-modal"
        role="dialog"
        aria-label={bindMachineId ? `连接 ${bindMachineId}` : '添加机器'}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="modal-hd">
          <h3>{bindMachineId ? `连接 ${bindMachineId}` : '添加机器'}</h3>
          <button type="button" className="ib" onClick={() => void handleClose()} aria-label="关闭">
            <X size={15} strokeWidth={1.7} />
          </button>
        </div>

        <div className="modal-body">
          {error ? (
            <div className="pair-err">
              <p className="bad-t">{error}</p>
              <button type="button" className="btn pri sm" onClick={() => void createCode(asRunner)}>
                重试
              </button>
            </div>
          ) : creating && !codeResp ? (
            <div className="pair-wait">正在生成邀请命令…</div>
          ) : isOnline ? (
            <div className="pair-done">
              <div className="pair-ok-line">
                <Check size={15} strokeWidth={2.2} className="ok-t" />
                <b>{status?.machineId ? `${status.machineId} 已加入` : '设备已连接'}</b>
              </div>
              {pairedMachine && (
                <div className="pair-agents-box">
                  <AgentList machine={pairedMachine} />
                </div>
              )}
              <div className="modal-foot">
                <button type="button" className="btn pri" onClick={onClose}>
                  完成
                </button>
              </div>
            </div>
          ) : codeResp ? (
            <>
              <div className="pair-label">在要加入的机器上运行：</div>
              <div className="pair-cmd-box">
                <code className="mono ell">{codeResp.joinCommand}</code>
                <button type="button" className="btn ghost sm" onClick={() => void copyCommand()}>
                  <Copy size={12} strokeWidth={1.7} /> 复制
                </button>
              </div>
              <div className="pair-sub">
                或在那台机器的 Orbit 桌面端选「连到已有的」，粘贴上面这一行。
              </div>

              <div className="pair-meta">
                {!bindMachineId ? (
                  <label className="chk">
                    <input
                      type="checkbox"
                      checked={asRunner}
                      disabled={creating || claimed}
                      onChange={(e) => {
                        const next = e.target.checked;
                        setAsRunner(next);
                        if (codeResp) {
                          void api.revokePairingCode(codeResp.id).catch(() => {});
                        }
                        void createCode(next);
                      }}
                    />
                    <span>让它执行任务</span>
                  </label>
                ) : (
                  <span className="muted">将为节点 {bindMachineId} 签发连接凭据</span>
                )}
                <span className="mono muted">
                  {expired ? '已失效' : `${formatCountdown(remainingMs)} 后失效`}
                </span>
              </div>

              <div className="pair-state">
                {expired ? (
                  <button type="button" className="btn ghost sm" onClick={() => void createCode(asRunner)}>
                    <RefreshCw size={12} strokeWidth={1.7} /> 重新生成
                  </button>
                ) : claimed ? (
                  <span className="ok-t">
                    已认领（{status?.machineId}），正在等待节点上线…
                  </span>
                ) : (
                  <span className="muted pair-waiting">
                    <span className="static-dots" aria-hidden="true">
                      <span />
                      <span />
                      <span />
                    </span>
                    等待连接
                  </span>
                )}
              </div>
            </>
          ) : null}
        </div>
      </section>
    </div>
  );
}
