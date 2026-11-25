import { useCallback, useEffect, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { AlertTriangle, Check, Copy, Globe, Laptop, RefreshCw, Server, ShieldCheck, X } from 'lucide-react';
import type {
  CreatePairingCodeResponse,
  MachineRole,
  MachineView,
  PairingCodeStatusResponse,
  TailscaleEnableResponse,
} from '../generated/protocol';
import { useNetworkSettings } from '../hooks/queries';
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
  const netQ = useNetworkSettings(open);
  const net = netQ.data;
  const [role, setRole] = useState<MachineRole>('full');
  const [asRunner, setAsRunner] = useState(true);
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState('');
  const [codeResp, setCodeResp] = useState<CreatePairingCodeResponse | null>(null);
  const [status, setStatus] = useState<PairingCodeStatusResponse | null>(null);
  const [now, setNow] = useState(() => Date.now());
  const [tsEnabling, setTsEnabling] = useState(false);
  const [tsConfirm, setTsConfirm] = useState<TailscaleEnableResponse | null>(null);

  const createCode = useCallback(
    async (wantRunner: boolean, targetRole: MachineRole) => {
      setCreating(true);
      setError('');
      setStatus(null);
      try {
        const res = await api.createPairingCode({
          scope: wantRunner || bindMachineId ? 'both' : 'client',
          role: targetRole,
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
      setTsConfirm(null);
      return;
    }
    setRole('full');
    setAsRunner(true);
    void createCode(true, 'full');
  }, [open, createCode]);

  useEffect(() => {
    if (!open || !codeResp) return;
    const tick = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(tick);
  }, [open, codeResp]);

  const remainingMs = codeResp ? codeResp.expiresAt - now : 0;
  const expired =
    status?.state === 'expired' || (codeResp !== null && remainingMs <= 0 && status?.state !== 'claimed');
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

  const regenerate = (nextRunner: boolean, nextRole: MachineRole) => {
    if (codeResp && !claimed && !expired) {
      void api.revokePairingCode(codeResp.id).catch(() => {});
    }
    void createCode(nextRunner, nextRole);
  };

  const isLoopbackCommand =
    !!codeResp &&
    (codeResp.loopbackOnly ||
      /https?:\/\/(127\.0\.0\.1|localhost|\[::1\])(?::|\/|\s|$)/i.test(codeResp.joinCommand));

  const enableTailscaleAndRefresh = async (confirm = false, port?: number) => {
    setTsEnabling(true);
    try {
      const res = await api.enableTailscaleShare({ confirm, port });
      if (res.needsConfirm) {
        setTsConfirm(res);
        return;
      }
      setTsConfirm(null);
      void qc.invalidateQueries({ queryKey: ['network-settings'] });
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
      toast(res.origin ? `已开启 Tailscale 共享：${res.origin}` : '已开启 Tailscale 共享');
      regenerate(asRunner, role);
    } catch (e) {
      toast(errMsg(e), 'err');
    } finally {
      setTsEnabling(false);
    }
  };

  const copyCommand = async () => {
    if (!codeResp) return;
    try {
      await navigator.clipboard.writeText(codeResp.joinCommand);
      toast('已复制加入命令');
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
        aria-label={bindMachineId ? `连接 ${bindMachineId}` : '添加设备'}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="modal-hd">
          <h3>{bindMachineId ? `连接 ${bindMachineId}` : '添加设备'}</h3>
          <button type="button" className="ib" onClick={() => void handleClose()} aria-label="关闭">
            <X size={15} strokeWidth={1.7} />
          </button>
        </div>

        <div className="modal-body">
          {!bindMachineId && !isOnline && (
            <div className="pair-role-seg" role="radiogroup" aria-label="设备角色">
              <button
                type="button"
                role="radio"
                aria-checked={role === 'full'}
                className={`pair-role-opt ${role === 'full' ? 'on' : ''}`}
                disabled={creating || claimed}
                onClick={() => {
                  if (role === 'full') return;
                  setRole('full');
                  regenerate(asRunner, 'full');
                }}
              >
                <Laptop size={14} strokeWidth={1.75} />
                <div>
                  <div className="pair-role-t">完整设备</div>
                  <div className="pair-role-d">可参与主控接力，同步工作区配置</div>
                </div>
              </button>
              <button
                type="button"
                role="radio"
                aria-checked={role === 'worker'}
                className={`pair-role-opt ${role === 'worker' ? 'on' : ''}`}
                disabled={creating || claimed}
                onClick={() => {
                  if (role === 'worker') return;
                  setRole('worker');
                  setAsRunner(true);
                  regenerate(true, 'worker');
                }}
              >
                <Server size={14} strokeWidth={1.75} />
                <div>
                  <div className="pair-role-t">受限节点</div>
                  <div className="pair-role-d">如云端服务器，仅执行任务，不接管主控</div>
                </div>
              </button>
            </div>
          )}

          {error ? (
            <div className="pair-err">
              <p className="bad-t">{error}</p>
              <button type="button" className="btn pri sm" onClick={() => void createCode(asRunner, role)}>
                重试
              </button>
            </div>
          ) : creating && !codeResp ? (
            <div className="pair-wait muted">正在生成配对命令…</div>
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
              {isLoopbackCommand && (
                <div className="pair-warn-box">
                  <div className="pair-warn-hd">
                    <AlertTriangle size={14} strokeWidth={1.8} />
                    <span>当前地址仅限本机访问（127.0.0.1），其他设备无法直连</span>
                  </div>
                  {net?.tailscaleAvailable ? (
                    <div className="pair-warn-body">
                      {tsConfirm ? (
                        <>
                          <div className="muted" style={{ fontSize: 12 }}>
                            {tsConfirm.conflictDetail ||
                              (tsConfirm.targetOrigin
                                ? `将通过 Tailscale HTTPS 共享到 ${tsConfirm.targetOrigin}，开启后会自动更新上方邀请命令。`
                                : `将通过 Tailscale 端口 ${tsConfirm.suggestedPort} 共享本机 Orbit。`)}
                          </div>
                          <div className="set-inline" style={{ marginTop: 6 }}>
                            <button
                              type="button"
                              className="btn pri sm"
                              disabled={tsEnabling}
                              onClick={() => void enableTailscaleAndRefresh(true, tsConfirm.suggestedPort)}
                            >
                              {tsEnabling ? '开启中…' : '确认开启'}
                            </button>
                            <button
                              type="button"
                              className="btn ghost sm"
                              disabled={tsEnabling}
                              onClick={() => setTsConfirm(null)}
                            >
                              取消
                            </button>
                          </div>
                        </>
                      ) : (
                        <button
                          type="button"
                          className="btn pri sm"
                          disabled={tsEnabling}
                          onClick={() => void enableTailscaleAndRefresh(false)}
                        >
                          <Globe size={12} strokeWidth={1.7} />
                          {tsEnabling ? '检查中…' : '一键开启 Tailscale 共享'}
                        </button>
                      )}
                    </div>
                  ) : (
                    <div className="muted" style={{ fontSize: 11.5 }}>
                      {net?.tailscaleHint || '请先在这台机器安装并登录 Tailscale，开启外部接入后再添加其他机器。'}
                    </div>
                  )}
                </div>
              )}

              <div className="pair-label">在目标设备的终端运行，或粘贴到 Orbit 桌面端：</div>
              <div className="pair-cmd-box">
                <code className="mono ell">{codeResp.joinCommand}</code>
                <button type="button" className="btn ghost sm" onClick={() => void copyCommand()}>
                  <Copy size={12} strokeWidth={1.7} /> 复制
                </button>
              </div>

              {codeResp.fingerprint && (
                <div className="pair-fp-box">
                  <ShieldCheck size={14} strokeWidth={1.8} className="ok-t" />
                  <span className="muted">配对校验指纹</span>
                  <span className="pair-fp-code mono">{codeResp.fingerprint}</span>
                  {codeResp.discoveryId && (
                    <span className="faint mono" style={{ marginLeft: 'auto', fontSize: 11 }}>
                      {codeResp.discoveryId}
                    </span>
                  )}
                </div>
              )}

              <div className="pair-meta">
                {!bindMachineId && role === 'full' ? (
                  <label className="chk">
                    <input
                      type="checkbox"
                      checked={asRunner}
                      disabled={creating || claimed}
                      onChange={(e) => {
                        const next = e.target.checked;
                        setAsRunner(next);
                        regenerate(next, role);
                      }}
                    />
                    <span>同时作为执行节点运行任务</span>
                  </label>
                ) : (
                  <span className="muted">
                    {bindMachineId ? `将为 ${bindMachineId} 签发连接凭据` : '受限执行节点凭据'}
                  </span>
                )}
                <span className="mono muted">
                  {expired ? '已失效' : `${formatCountdown(remainingMs)} 后失效`}
                </span>
              </div>

              <div className="pair-state">
                {expired ? (
                  <button
                    type="button"
                    className="btn ghost sm"
                    onClick={() => void createCode(asRunner, role)}
                  >
                    <RefreshCw size={12} strokeWidth={1.7} /> 重新生成
                  </button>
                ) : claimed ? (
                  <span className="ok-t">已认领（{status?.machineId}），正在等待节点上线…</span>
                ) : (
                  <span className="muted pair-waiting">
                    <span className="static-dots" aria-hidden="true">
                      <span />
                      <span />
                      <span />
                    </span>
                    等待对方确认并加入
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
