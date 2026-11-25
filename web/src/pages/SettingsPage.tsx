import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import {
  ArrowUp,
  Check,
  ChevronDown,
  ChevronRight,
  Copy,
  ExternalLink,
  Folder,
  Globe,
  Laptop,
  Plus,
  Radar,
  Trash2,
} from 'lucide-react';
import type {
  BootstrapResponse,
  MachineRole,
  MachineView,
  NetworkDiscoverResponse,
  TailscaleEnableResponse,
} from '../generated/protocol';
import { AgentList, agentDisplayName } from '../components/AgentList';
import { DeviceRoleIcon } from '../components/DeviceRoleIcon';
import { PageHeader } from '../components/PageHeader';
import { PairDialog } from '../components/PairDialog';
import { useNetworkSettings } from '../hooks/queries';
import { api, errMsg } from '../lib/api';
import { cleanMachineName, relTime } from '../lib/format';
import { resolveEffectiveLocalMachineId } from '../lib/ide';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';

export function SettingsPage({ boot }: { boot: BootstrapResponse }) {
  const qc = useQueryClient();
  const netQ = useNetworkSettings(true);
  const storedLocalId = useWorkbenchStore((s) => s.localMachineId);
  const setWorkspaceNameOverride = useWorkbenchStore((s) => s.setWorkspaceNameOverride);

  const net = netQ.data;
  const workspaceName = net?.workspaceName || boot.workspaceName || 'Home LAN';
  const discoveryId = net?.discoveryId || boot.setup?.discoveryId || '';
  const dutyPriority = net?.dutyPriority ?? boot.setup?.dutyPriority ?? [];
  const activeDutyId = net?.activeDutyMachineId || boot.setup?.activeDutyMachineId || '';
  const effectiveLocalId = resolveEffectiveLocalMachineId(
    boot.machines,
    boot.setup?.currentMachineId || storedLocalId,
  );

  const [editingName, setEditingName] = useState(false);
  const [nameDraft, setNameDraft] = useState(workspaceName);
  const [savingName, setSavingName] = useState(false);
  const [savingDuty, setSavingDuty] = useState(false);

  const [expandedMachine, setExpandedMachine] = useState<string>(() => {
    const first = boot.machines.find((m) => m.status === 'online' && m.transportMode !== 'ssh_outbound');
    return first?.machineId ?? '';
  });
  const [aliasEdits, setAliasEdits] = useState<Record<string, string>>({});
  const [rootDrafts, setRootDrafts] = useState<Record<string, string>>({});

  const [pairOpen, setPairOpen] = useState(false);
  const [pairBindMachine, setPairBindMachine] = useState<string | undefined>(undefined);

  const [tsLoading, setTsLoading] = useState(false);
  const [tsConfirm, setTsConfirm] = useState<TailscaleEnableResponse | null>(null);
  const [discovering, setDiscovering] = useState(false);
  const [discoveryRes, setDiscoveryRes] = useState<NetworkDiscoverResponse | null>(null);

  const refreshAll = () => {
    void qc.invalidateQueries({ queryKey: ['bootstrap'] });
    void qc.invalidateQueries({ queryKey: ['network-settings'] });
  };

  const saveWorkspaceName = async () => {
    const trimmed = nameDraft.trim();
    if (!trimmed) return;
    setSavingName(true);
    try {
      await api.updateNetworkSettings({ workspaceName: trimmed });
      setWorkspaceNameOverride('');
      setEditingName(false);
      refreshAll();
      toast(`已更新工作区名称：${trimmed}`);
    } catch (e) {
      toast(errMsg(e), 'err');
    } finally {
      setSavingName(false);
    }
  };

  const pinDutyMachine = async (machineId: string) => {
    const next = [machineId, ...dutyPriority.filter((id) => id !== machineId)];
    setSavingDuty(true);
    try {
      await api.updateNetworkSettings({ dutyPriority: next });
      refreshAll();
      toast('已调整主控顺位');
    } catch (e) {
      toast(errMsg(e), 'err');
    } finally {
      setSavingDuty(false);
    }
  };

  const triggerTailscaleShare = async (confirm = false, port?: number) => {
    setTsLoading(true);
    try {
      const res = await api.enableTailscaleShare({ confirm, port });
      if (res.needsConfirm) {
        setTsConfirm(res);
      } else {
        setTsConfirm(null);
        refreshAll();
        toast(res.origin ? `已开启 Tailscale 共享：${res.origin}` : '已开启外部接入');
      }
    } catch (e) {
      toast(errMsg(e), 'err');
    } finally {
      setTsLoading(false);
    }
  };

  const triggerTailscaleDisable = async () => {
    setTsLoading(true);
    try {
      await api.disableTailscaleShare();
      setTsConfirm(null);
      refreshAll();
      toast('已关闭 Tailscale 外部接入');
    } catch (e) {
      toast(errMsg(e), 'err');
    } finally {
      setTsLoading(false);
    }
  };

  const scanNetworks = async () => {
    setDiscovering(true);
    try {
      const res = await api.discoverNetworks();
      setDiscoveryRes(res);
    } catch (e) {
      toast(errMsg(e), 'err');
    } finally {
      setDiscovering(false);
    }
  };

  const saveMachineAlias = async (m: MachineView) => {
    const alias = (aliasEdits[m.machineId] ?? m.displayAlias ?? '').trim();
    try {
      await api.updateMachine(m.machineId, { displayAlias: alias });
      refreshAll();
      toast('已更新设备名称');
    } catch (e) {
      toast(errMsg(e), 'err');
    }
  };

  const updateMachineRole = async (m: MachineView, nextRole: MachineRole) => {
    try {
      await api.updateMachine(m.machineId, { role: nextRole });
      refreshAll();
      toast(nextRole === 'full' ? '已设为完整设备' : '已设为受限节点');
    } catch (e) {
      toast(errMsg(e), 'err');
    }
  };

  const addAllowedRoot = async (m: MachineView) => {
    const raw = (rootDrafts[m.machineId] ?? '').trim();
    if (!raw) return;
    const next = [...(m.allowedRoots ?? []), raw];
    try {
      await api.updateMachine(m.machineId, { allowedRoots: next });
      setRootDrafts((d) => ({ ...d, [m.machineId]: '' }));
      refreshAll();
      toast('已添加工作目录');
    } catch (e) {
      toast(errMsg(e), 'err');
    }
  };

  const removeAllowedRoot = async (m: MachineView, target: string) => {
    const next = (m.allowedRoots ?? []).filter((r) => r !== target);
    try {
      await api.updateMachine(m.machineId, { allowedRoots: next });
      refreshAll();
    } catch (e) {
      toast(errMsg(e), 'err');
    }
  };

  const removeMachine = async (m: MachineView) => {
    try {
      await api.deleteMachine(m.machineId);
      refreshAll();
      toast(`已移除 ${m.displayAlias || cleanMachineName(m)}`);
    } catch (e) {
      toast(errMsg(e), 'err');
    }
  };

  const copyValue = async (val: string, label: string) => {
    try {
      await navigator.clipboard.writeText(val);
      toast(`已复制${label}`);
    } catch {
      toast(val);
    }
  };

  const machineMap = new Map(boot.machines.map((m) => [m.machineId, m]));
  const dutyMachines = dutyPriority
    .map((id) => machineMap.get(id))
    .filter((m): m is MachineView => !!m);

  return (
    <main className="main">
      <PageHeader>
        <span className="ttl">网络与机器</span>
      </PageHeader>
      <div className="page-scroll">
        <div className="set-wrap">
          {/* Section 1: 网络 */}
          <section className="set-sec">
            <div className="set-hd">
              <h2>网络</h2>
            </div>

            <div className="set-row">
              <div className="set-k">工作区名称</div>
              <div className="set-v">
                {editingName ? (
                  <div className="set-inline">
                    <input
                      className="field"
                      value={nameDraft}
                      onChange={(e) => setNameDraft(e.target.value)}
                      onKeyDown={(e) => {
                        if (e.key === 'Enter') void saveWorkspaceName();
                        if (e.key === 'Escape') setEditingName(false);
                      }}
                      autoFocus
                    />
                    <button
                      type="button"
                      className="btn pri sm"
                      disabled={savingName}
                      onClick={() => void saveWorkspaceName()}
                    >
                      保存
                    </button>
                    <button type="button" className="btn ghost sm" onClick={() => setEditingName(false)}>
                      取消
                    </button>
                  </div>
                ) : (
                  <div className="set-inline">
                    <b>{workspaceName}</b>
                    <button
                      type="button"
                      className="btn ghost sm"
                      onClick={() => {
                        setNameDraft(workspaceName);
                        setEditingName(true);
                      }}
                    >
                      改名
                    </button>
                    {discoveryId && (
                      <button
                        type="button"
                        className="btn ghost sm"
                        title="复制工作区网络发现 ID"
                        onClick={() => void copyValue(discoveryId, '网络 ID')}
                      >
                        <Copy size={11.5} strokeWidth={1.7} /> 复制网络 ID
                      </button>
                    )}
                  </div>
                )}
              </div>
            </div>

            {dutyMachines.length > 0 && (
              <div className="set-row">
                <div className="set-k">主控顺位</div>
                <div className="set-v">
                  <div className="duty-list">
                    {dutyMachines.map((m, idx) => {
                      const isDuty = m.machineId === activeDutyId;
                      const label = m.displayAlias || cleanMachineName(m);
                      return (
                        <div className="duty-row" key={m.machineId}>
                          <span className="duty-rank mono">#{idx + 1}</span>
                          <DeviceRoleIcon
                            machine={m}
                            isActiveDuty={isDuty}
                            isCurrentDevice={m.machineId === effectiveLocalId}
                            size={13.5}
                          />
                          <span className="duty-name ell">{label}</span>
                          {isDuty && <span className="duty-cur">当前主控</span>}
                          <span className="duty-right">
                            {idx > 0 && (
                              <button
                                type="button"
                                className="btn ghost sm"
                                disabled={savingDuty}
                                onClick={() => void pinDutyMachine(m.machineId)}
                              >
                                <ArrowUp size={11.5} strokeWidth={1.8} /> 置顶
                              </button>
                            )}
                          </span>
                        </div>
                      );
                    })}
                  </div>
                </div>
              </div>
            )}

            <div className="set-row">
              <div className="set-k">外部接入</div>
              <div className="set-v">
                {net?.tailscaleShared ? (
                  <div className="set-inline">
                    <span className="ok-t">已开启</span>
                    <span className="mono muted ell">{net.tailscaleOrigin || net.hubOrigin}</span>
                    <button
                      type="button"
                      className="btn ghost sm"
                      onClick={() => void copyValue(net.tailscaleOrigin || net.hubOrigin, '接入地址')}
                    >
                      <Copy size={11.5} strokeWidth={1.7} /> 复制
                    </button>
                    <button
                      type="button"
                      className="btn ghost sm"
                      disabled={tsLoading}
                      onClick={() => void triggerTailscaleDisable()}
                    >
                      关闭
                    </button>
                  </div>
                ) : net?.tailscaleAvailable ? (
                  <div className="set-col">
                    <div className="set-inline">
                      <span className="muted">仅本机</span>
                      <button
                        type="button"
                        className="btn ghost sm"
                        disabled={tsLoading}
                        onClick={() => void triggerTailscaleShare(false)}
                      >
                        <Globe size={12} strokeWidth={1.7} />
                        {tsLoading ? '检查中…' : '开启外部接入'}
                      </button>
                    </div>
                    {tsConfirm && (
                      <div className="set-confirm">
                        <div className="muted">
                          {tsConfirm.conflictDetail ||
                            (tsConfirm.targetOrigin
                              ? `开启后将通过 ${tsConfirm.targetOrigin} 接入。`
                              : `将使用端口 ${tsConfirm.suggestedPort} 开启外部接入。`)}
                        </div>
                        <div className="set-inline" style={{ marginTop: 8 }}>
                          <button
                            type="button"
                            className="btn pri sm"
                            disabled={tsLoading}
                            onClick={() => void triggerTailscaleShare(true, tsConfirm.suggestedPort)}
                          >
                            确认开启
                          </button>
                          <button type="button" className="btn ghost sm" onClick={() => setTsConfirm(null)}>
                            取消
                          </button>
                        </div>
                      </div>
                    )}
                  </div>
                ) : (
                  <span className="muted">{net?.tailscaleHint || '登录 Tailscale 后可开启'}</span>
                )}
              </div>
            </div>

            <div className="set-row">
              <div className="set-k">发现网络</div>
              <div className="set-v">
                <div className="set-col">
                  <div className="set-inline">
                    <button
                      type="button"
                      className="btn ghost sm"
                      disabled={discovering}
                      onClick={() => void scanNetworks()}
                    >
                      <Radar size={12} strokeWidth={1.7} className={discovering ? 'spin-ico' : ''} />
                      {discovering ? '扫描中…' : '扫描网络'}
                    </button>
                    {discoveryRes && discoveryRes.tailscaleAvailable && !discoveryRes.tailscaleOnline && (
                      <span className="faint" style={{ fontSize: 11.5 }}>
                        Tailscale 未连接
                      </span>
                    )}
                  </div>

                  {discoveryRes && (
                    discoveryRes.hubs.length === 0 ? (
                      <div className="faint" style={{ fontSize: 12, marginTop: 4 }}>
                        未发现其他网络
                      </div>
                    ) : (
                      <div className="disc-list">
                        {discoveryRes.hubs.map((h) => (
                          <div className="disc-row" key={`${h.origin}:${h.discoveryId ?? ''}`}>
                            <div className="disc-main">
                              <div className="disc-hd">
                                <span className="disc-host ell">{h.hostName || h.dnsName || h.origin}</span>
                                {h.discoveryId && <span className="disc-id mono">{h.discoveryId}</span>}
                                {h.isSelf && <span className="duty-cur">当前</span>}
                              </div>
                              <div className="disc-url mono ell">{h.origin}</div>
                            </div>
                            <div className="disc-acts">
                              <button
                                type="button"
                                className="btn ghost sm"
                                onClick={() => void copyValue(h.origin, '网络地址')}
                              >
                                <Copy size={11.5} strokeWidth={1.7} /> 复制
                              </button>
                              {!h.isSelf && (
                                <a
                                  href={h.origin}
                                  target="_blank"
                                  rel="noreferrer"
                                  className="btn ghost sm"
                                >
                                  <ExternalLink size={11.5} strokeWidth={1.7} /> 打开
                                </a>
                              )}
                            </div>
                          </div>
                        ))}
                      </div>
                    )
                  )}
                </div>
              </div>
            </div>
          </section>

          {/* Section 2: 机器与 Agent */}
          <section className="set-sec">
            <div className="set-hd">
              <h2>机器与 Agent</h2>
              <button
                type="button"
                className="btn pri sm"
                onClick={() => {
                  setPairBindMachine(undefined);
                  setPairOpen(true);
                }}
              >
                <Plus size={12.5} strokeWidth={1.8} /> 添加机器
              </button>
            </div>

            <div className="set-mach-list">
              {boot.machines.map((m) => {
                const isExec = m.transportMode !== 'ssh_outbound';
                const expanded = expandedMachine === m.machineId;
                const online = m.status === 'online';
                const readyAgents = (m.agents ?? []).filter((a) => a.state === 'ready' && a.enabled);
                const needsPair = isExec && !m.hasCredentials;
                const label = m.displayAlias || cleanMachineName(m);
                const isDuty = m.machineId === activeDutyId;
                const isCurrent = !!effectiveLocalId && m.machineId === effectiveLocalId;
                const curRole: MachineRole =
                  m.role === 'worker' || !isExec ? 'worker' : 'full';

                return (
                  <div className={`set-mach-card ${expanded ? 'open' : ''}`} key={m.machineId}>
                    <div
                      className="set-mach-hd"
                      onClick={() => isExec && setExpandedMachine(expanded ? '' : m.machineId)}
                    >
                      <DeviceRoleIcon
                        machine={m}
                        isActiveDuty={isDuty}
                        isCurrentDevice={isCurrent}
                        size={14}
                      />
                      <span className="set-mach-name ell">{label}</span>

                      <div className="set-mach-summary">
                        {isExec && readyAgents.length > 0 && !expanded && (
                          <span className="muted set-ag-chips">
                            {readyAgents.map((a) => (
                              <span key={a.adapter} className="set-ag-chip">
                                {agentDisplayName(a.adapter)}
                                <Check size={11} strokeWidth={2.2} className="ok-t" />
                              </span>
                            ))}
                          </span>
                        )}

                        {needsPair && (
                          <button
                            type="button"
                            className="btn ghost sm"
                            onClick={(e) => {
                              e.stopPropagation();
                              setPairBindMachine(m.machineId);
                              setPairOpen(true);
                            }}
                          >
                            <Laptop size={12} strokeWidth={1.7} /> 连接这台机器
                          </button>
                        )}

                        <span className="muted mono" style={{ fontSize: 11.5 }}>
                          {online ? '在线' : m.lastSeenAt ? `离线 · ${relTime(m.lastSeenAt)}` : '离线'}
                        </span>

                        {isExec && (
                          <span className="muted">
                            {expanded ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
                          </span>
                        )}
                      </div>
                    </div>

                    {isExec && expanded && (
                      <div className="set-mach-body">
                        <div className="set-mach-alias">
                          <span className="muted">显示名称</span>
                          <input
                            className="field"
                            style={{ maxWidth: 200 }}
                            value={aliasEdits[m.machineId] ?? m.displayAlias ?? ''}
                            placeholder={cleanMachineName(m)}
                            onChange={(e) =>
                              setAliasEdits((prev) => ({ ...prev, [m.machineId]: e.target.value }))
                            }
                            onKeyDown={(e) => {
                              if (e.key === 'Enter') void saveMachineAlias(m);
                            }}
                          />
                          <button type="button" className="btn ghost sm" onClick={() => void saveMachineAlias(m)}>
                            保存
                          </button>

                          <div className="role-seg" style={{ marginLeft: 'auto' }}>
                            <button
                              type="button"
                              className={curRole === 'full' ? 'on' : ''}
                              onClick={() => curRole !== 'full' && void updateMachineRole(m, 'full')}
                            >
                              完整设备
                            </button>
                            <button
                              type="button"
                              className={curRole === 'worker' ? 'on' : ''}
                              onClick={() => curRole !== 'worker' && void updateMachineRole(m, 'worker')}
                            >
                              受限节点
                            </button>
                          </div>
                        </div>

                        <AgentList machine={m} showAdvanced />

                        {/* Allowed working directories */}
                        <div className="roots-block">
                          <div className="ag-block-lbl">工作目录范围</div>
                          <div className="roots-list">
                            {(m.allowedRoots ?? []).length === 0 ? (
                              <span className="faint" style={{ fontSize: 12 }}>
                                默认使用用户主目录（~）
                              </span>
                            ) : (
                              (m.allowedRoots ?? []).map((rt) => (
                                <span className="root-chip mono" key={rt}>
                                  <Folder size={11} strokeWidth={1.6} />
                                  <span>{rt}</span>
                                  <button
                                    type="button"
                                    aria-label={`移除 ${rt}`}
                                    onClick={() => void removeAllowedRoot(m, rt)}
                                  >
                                    ×
                                  </button>
                                </span>
                              ))
                            )}
                          </div>
                          <div className="set-inline" style={{ marginTop: 6 }}>
                            <input
                              className="field mono"
                              style={{ maxWidth: 260 }}
                              placeholder="添加允许浏览/扫描的路径（如 ~/dev）"
                              value={rootDrafts[m.machineId] ?? ''}
                              onChange={(e) =>
                                setRootDrafts((d) => ({ ...d, [m.machineId]: e.target.value }))
                              }
                              onKeyDown={(e) => {
                                if (e.key === 'Enter') {
                                  e.preventDefault();
                                  void addAllowedRoot(m);
                                }
                              }}
                            />
                            <button
                              type="button"
                              className="btn ghost sm"
                              disabled={!(rootDrafts[m.machineId] ?? '').trim()}
                              onClick={() => void addAllowedRoot(m)}
                            >
                              <Plus size={12} strokeWidth={1.8} /> 添加
                            </button>
                            {m.source === 'paired' && (
                              <button
                                type="button"
                                className="btn ghost sm bad-t"
                                style={{ marginLeft: 'auto' }}
                                onClick={() => void removeMachine(m)}
                              >
                                <Trash2 size={12} strokeWidth={1.7} /> 移除设备
                              </button>
                            )}
                          </div>
                        </div>
                      </div>
                    )}
                  </div>
                );
              })}
            </div>
          </section>
        </div>
      </div>

      <PairDialog
        open={pairOpen}
        bindMachineId={pairBindMachine}
        machines={boot.machines}
        onClose={() => {
          setPairOpen(false);
          setPairBindMachine(undefined);
        }}
      />
    </main>
  );
}
