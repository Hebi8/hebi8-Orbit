import { useEffect, useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import {
  ArrowLeftRight,
  ArrowUp,
  Check,
  ChevronDown,
  ChevronRight,
  Copy,
  Folder,
  Globe,
  GripVertical,
  Laptop,
  LogOut,
  MonitorSmartphone,
  Plus,
  Radar,
  Server,
  Sparkles,
  Trash2,
} from 'lucide-react';
import type {
  BootstrapResponse,
  MachineRole,
  MachineView,
  NetworkDiscoverResponse,
  NetworkSettingsView,
  TailscaleEnableResponse,
} from '../generated/protocol';
import { AgentList, agentDisplayName } from '../components/AgentList';
import { DeviceRoleIcon } from '../components/DeviceRoleIcon';
import { MoonIcon, SunIcon } from '../components/Icons';
import { PageHeader } from '../components/PageHeader';
import { PairDialog } from '../components/PairDialog';
import { useClientDevices, useNetworkSettings } from '../hooks/queries';
import { api, errMsg, isDesktopShell, openDesktopSetup, setStoredClientToken } from '../lib/api';
import { cleanMachineName, relTime } from '../lib/format';
import { resolveEffectiveLocalMachineId } from '../lib/ide';
import { useImeGuard } from '../lib/ime';
import { navigate, paths } from '../lib/router';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';

export function SettingsPage({
  boot,
  initialMachineId,
}: {
  boot: BootstrapResponse;
  initialMachineId?: string;
}) {
  const qc = useQueryClient();
  const netQ = useNetworkSettings(true);
  const clientsQ = useClientDevices(true);
  const storedLocalId = useWorkbenchStore((s) => s.localMachineId);
  const setWorkspaceNameOverride = useWorkbenchStore((s) => s.setWorkspaceNameOverride);
  const theme = useWorkbenchStore((s) => s.theme);
  const toggleTheme = useWorkbenchStore((s) => s.toggleTheme);
  const inDesktop = isDesktopShell();

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
  const [optimisticDutyOrder, setOptimisticDutyOrder] = useState<string[] | null>(null);
  const [draggingDutyId, setDraggingDutyId] = useState<string | null>(null);
  const [dropTarget, setDropTarget] = useState<{ id: string; edge: 'before' | 'after' } | null>(null);
  const draggingDutyRef = useRef<string | null>(null);
  const ime = useImeGuard();

  const consumedInitialRef = useRef<string | null>(
    initialMachineId && boot.machines.some((m) => m.machineId === initialMachineId) ? initialMachineId : null,
  );

  const [expandedMachine, setExpandedMachine] = useState<string>(() => {
    if (initialMachineId && boot.machines.some((m) => m.machineId === initialMachineId)) {
      return initialMachineId;
    }
    const first = boot.machines.find((m) => m.status === 'online') ?? boot.machines[0];
    return first?.machineId ?? '';
  });

  useEffect(() => {
    if (!initialMachineId) {
      consumedInitialRef.current = null;
      return;
    }
    if (consumedInitialRef.current === initialMachineId) return;
    if (boot.machines.some((m) => m.machineId === initialMachineId)) {
      consumedInitialRef.current = initialMachineId;
      setExpandedMachine(initialMachineId);
    }
  }, [initialMachineId, boot.machines]);

  const [aliasEdits, setAliasEdits] = useState<Record<string, string>>({});
  const [sshAliasEdits, setSshAliasEdits] = useState<Record<string, string>>({});
  const [rootDrafts, setRootDrafts] = useState<Record<string, string>>({});
  const [confirmRemoveId, setConfirmRemoveId] = useState<string | null>(null);

  useEffect(() => {
    if (!confirmRemoveId) return;
    const onDown = (e: MouseEvent) => {
      const target = e.target as HTMLElement | null;
      if (target?.closest('[data-remove-machine-confirm]')) return;
      setConfirmRemoveId(null);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setConfirmRemoveId(null);
    };
    document.addEventListener('mousedown', onDown);
    window.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('mousedown', onDown);
      window.removeEventListener('keydown', onKey);
    };
  }, [confirmRemoveId]);

  const [pairOpen, setPairOpen] = useState(false);
  const [pairBindMachine, setPairBindMachine] = useState<string | undefined>(undefined);

  const [tsLoading, setTsLoading] = useState(false);
  const [tsConfirm, setTsConfirm] = useState<TailscaleEnableResponse | null>(null);
  const [discovering, setDiscovering] = useState(false);
  const [discoveryRes, setDiscoveryRes] = useState<NetworkDiscoverResponse | null>(null);

  const refreshAll = () => {
    void qc.invalidateQueries({ queryKey: ['bootstrap'] });
    void qc.invalidateQueries({ queryKey: ['machines'] });
    void qc.invalidateQueries({ queryKey: ['network-settings'] });
    void qc.invalidateQueries({ queryKey: ['client-devices'] });
  };

  const revokeClientDevice = async (clientId: string, name: string, isCurrent?: boolean) => {
    try {
      await api.revokeClient(clientId);
      if (isCurrent) {
        setStoredClientToken('');
        window.location.reload();
        return;
      }
      refreshAll();
      toast(`已移除客户端设备：${name}`);
    } catch (e) {
      toast(errMsg(e), 'err');
    }
  };

  const logoutCurrentDevice = async () => {
    try {
      await api.logoutClient();
    } catch {
      // ignore if already revoked
    } finally {
      setStoredClientToken('');
      window.location.reload();
    }
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

  const saveDutyOrder = async (nextOrder: string[]) => {
    setOptimisticDutyOrder(nextOrder);
    setSavingDuty(true);
    try {
      const updated = await api.updateNetworkSettings({ dutyPriority: nextOrder });
      qc.setQueryData<NetworkSettingsView>(['network-settings'], updated);
      qc.setQueryData<BootstrapResponse>(['bootstrap'], (prev) =>
        prev
          ? {
              ...prev,
              setup: prev.setup
                ? {
                    ...prev.setup,
                    dutyPriority: updated.dutyPriority,
                    activeDutyMachineId: updated.activeDutyMachineId,
                  }
                : prev.setup,
            }
          : prev,
      );
      setOptimisticDutyOrder(null);
      refreshAll();
      toast('已调整主控顺位');
    } catch (e) {
      setOptimisticDutyOrder(null);
      toast(errMsg(e), 'err');
    } finally {
      setSavingDuty(false);
    }
  };

  const pinDutyMachine = async (machineId: string) => {
    const base = optimisticDutyOrder ?? dutyPriority;
    const next = [machineId, ...base.filter((id) => id !== machineId)];
    await saveDutyOrder(next);
  };

  const handleDutyDrop = (targetId: string, edge: 'before' | 'after', transferredId?: string) => {
    const dragId = draggingDutyRef.current || transferredId || '';
    draggingDutyRef.current = null;
    setDraggingDutyId(null);
    setDropTarget(null);
    if (!dragId || dragId === targetId || savingDuty) return;
    const currentOrder = dutyMachines.map((m) => m.machineId);
    if (!currentOrder.includes(dragId) || !currentOrder.includes(targetId)) return;
    const without = currentOrder.filter((id) => id !== dragId);
    const targetIdx = without.indexOf(targetId);
    if (targetIdx < 0) return;
    const insertIdx = edge === 'before' ? targetIdx : targetIdx + 1;
    without.splice(insertIdx, 0, dragId);
    if (without.every((id, i) => id === currentOrder[i])) return;
    void saveDutyOrder(without);
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
      setAliasEdits((prev) => ({ ...prev, [m.machineId]: alias }));
      refreshAll();
      toast('已更新设备名称');
    } catch (e) {
      toast(errMsg(e), 'err');
    }
  };

  const saveMachineSshAlias = async (m: MachineView) => {
    const sshAlias = (sshAliasEdits[m.machineId] ?? m.sshAlias ?? '').trim();
    try {
      await api.updateMachine(m.machineId, { sshAlias });
      setSshAliasEdits((prev) => ({ ...prev, [m.machineId]: sshAlias }));
      refreshAll();
      toast(sshAlias ? '已更新 SSH 别名' : '已清除自定义 SSH 别名');
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
    const existing = m.allowedRoots ?? [];
    if (existing.includes(raw)) {
      setRootDrafts((d) => ({ ...d, [m.machineId]: '' }));
      toast('该目录已在白名单中');
      return;
    }
    const next = [...existing, raw];
    try {
      await api.updateMachine(m.machineId, { allowedRoots: next });
      setRootDrafts((d) => ({ ...d, [m.machineId]: '' }));
      refreshAll();
      toast('已添加目录白名单');
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
      if (expandedMachine === m.machineId) {
        setExpandedMachine('');
      }
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
  const effectiveDutyPriority = optimisticDutyOrder ?? dutyPriority;
  const dutyMachines = effectiveDutyPriority
    .map((id) => machineMap.get(id))
    .filter((m): m is MachineView => !!m);
  const primaryDutyMachine = dutyMachines[0];
  const isFailoverActive =
    !!primaryDutyMachine &&
    !!activeDutyId &&
    primaryDutyMachine.machineId !== activeDutyId &&
    primaryDutyMachine.status !== 'online';

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
              <div style={{ display: 'flex', gap: 6 }}>
                <button
                  type="button"
                  className="btn ghost sm"
                  title="切换深 / 浅色外观"
                  onClick={toggleTheme}
                >
                  {theme === 'dark' ? <SunIcon size={12.5} /> : <MoonIcon size={12.5} />}
                  <span>{theme === 'dark' ? '浅色模式' : '深色模式'}</span>
                </button>
                {inDesktop && (
                  <button type="button" className="btn ghost sm" onClick={() => openDesktopSetup()}>
                    <ArrowLeftRight size={12} strokeWidth={1.7} /> 切换网络
                  </button>
                )}
                {boot.setup?.currentClientId && (
                  <button type="button" className="btn ghost sm" onClick={() => void logoutCurrentDevice()}>
                    <LogOut size={12} strokeWidth={1.7} /> 退出登录
                  </button>
                )}
              </div>
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
                      {...ime.compositionProps}
                      onKeyDown={(e) => {
                        if (ime.isImeEnter(e)) return;
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
                      const isTakeover = isFailoverActive && isDuty;
                      const online = m.status === 'online';
                      const label = m.displayAlias || cleanMachineName(m);
                      const canDrag = dutyMachines.length > 1 && !savingDuty;
                      const isDragging = draggingDutyId === m.machineId;
                      const dropEdge = dropTarget?.id === m.machineId ? dropTarget.edge : null;
                      return (
                        <div
                          className={`duty-row ${isTakeover ? 'is-takeover' : isDuty ? 'is-active-duty' : ''} ${canDrag ? 'is-draggable' : ''} ${isDragging ? 'is-dragging' : ''} ${dropEdge === 'before' ? 'is-drop-before' : dropEdge === 'after' ? 'is-drop-after' : ''}`}
                          key={m.machineId}
                          draggable={canDrag}
                          onDragStart={(e) => {
                            if (!canDrag) return;
                            draggingDutyRef.current = m.machineId;
                            setDraggingDutyId(m.machineId);
                            e.dataTransfer.effectAllowed = 'move';
                            e.dataTransfer.setData('text/plain', m.machineId);
                          }}
                          onDragOver={(e) => {
                            const dragId = draggingDutyRef.current;
                            if (!canDrag || !dragId || dragId === m.machineId) return;
                            e.preventDefault();
                            e.dataTransfer.dropEffect = 'move';
                            const rect = e.currentTarget.getBoundingClientRect();
                            const edge: 'before' | 'after' = e.clientY - rect.top < rect.height / 2 ? 'before' : 'after';
                            if (dropTarget?.id !== m.machineId || dropTarget.edge !== edge) {
                              setDropTarget({ id: m.machineId, edge });
                            }
                          }}
                          onDragLeave={(e) => {
                            if (!e.currentTarget.contains(e.relatedTarget as Node | null)) {
                              setDropTarget((cur) => (cur?.id === m.machineId ? null : cur));
                            }
                          }}
                          onDrop={(e) => {
                            e.preventDefault();
                            const rect = e.currentTarget.getBoundingClientRect();
                            const edge: 'before' | 'after' = e.clientY - rect.top < rect.height / 2 ? 'before' : 'after';
                            handleDutyDrop(m.machineId, edge, e.dataTransfer.getData('text/plain'));
                          }}
                          onDragEnd={() => {
                            draggingDutyRef.current = null;
                            setDraggingDutyId(null);
                            setDropTarget(null);
                          }}
                        >
                          {dutyMachines.length > 1 && (
                            <span className="duty-grip" title="拖拽调整主控顺位" aria-hidden="true">
                              <GripVertical size={13} strokeWidth={1.7} />
                            </span>
                          )}
                          <DeviceRoleIcon
                            machine={m}
                            isActiveDuty={isDuty}
                            isCurrentDevice={m.machineId === effectiveLocalId}
                            size={13.5}
                          />
                          <span className={`duty-name ell ${!online ? 'muted' : ''}`}>{label}</span>
                          {isTakeover ? (
                            <span className="duty-takeover">当前接管</span>
                          ) : isDuty ? (
                            <span className="duty-cur">当前主控</span>
                          ) : !online ? (
                            <span className="faint" style={{ fontSize: 11 }}>
                              离线
                            </span>
                          ) : null}
                          <span className="duty-right">
                            {idx > 0 && (
                              <button
                                type="button"
                                className="btn ghost sm"
                                draggable={false}
                                disabled={savingDuty}
                                onMouseDown={(e) => e.stopPropagation()}
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
                                inDesktop ? (
                                  <button
                                    type="button"
                                    className="btn pri sm"
                                    onClick={() =>
                                      openDesktopSetup({
                                        origin: h.origin,
                                        hostName: h.hostName || h.dnsName,
                                        discoveryId: h.discoveryId,
                                      })
                                    }
                                  >
                                    <ArrowLeftRight size={11.5} strokeWidth={1.7} /> 切换 / 配对
                                  </button>
                                ) : (
                                  <button
                                    type="button"
                                    className="btn pri sm"
                                    onClick={() => {
                                      window.open(h.origin, '_blank', 'noopener,noreferrer');
                                    }}
                                  >
                                    <ArrowLeftRight size={11.5} strokeWidth={1.7} /> 配对访问
                                  </button>
                                )
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

          {/* Section 1.5: 已登录客户端设备 */}
          <section className="set-sec">
            <div className="set-hd">
              <h2>已登录客户端设备</h2>
              <span className="faint" style={{ fontSize: 11.5 }}>
                通过配对码登录的浏览器/桌面端（仅访问，不作为执行主机）
              </span>
            </div>
            <div className="client-dev-list">
              {(clientsQ.data ?? []).length === 0 ? (
                <div className="faint" style={{ fontSize: 12, padding: '6px 0' }}>
                  暂无已配对的远程客户端设备
                </div>
              ) : (
                (clientsQ.data ?? []).map((c) => (
                  <div className="client-dev-row" key={c.id}>
                    <MonitorSmartphone size={14} strokeWidth={1.7} className="muted" />
                    <div className="client-dev-main">
                      <div className="client-dev-hd">
                        <span className="ell">{c.name}</span>
                        {c.isCurrent && <span className="duty-cur">当前设备</span>}
                        <span className="faint mono" style={{ fontSize: 11 }}>{c.scope}</span>
                      </div>
                      <div className="faint" style={{ fontSize: 11.5 }}>
                        {c.lastUsedAt ? `最近活跃 · ${relTime(c.lastUsedAt)}` : `登录于 ${relTime(c.createdAt)}`}
                      </div>
                    </div>
                    <button
                      type="button"
                      className="btn ghost sm bad-t"
                      onClick={() => void revokeClientDevice(c.id, c.name, c.isCurrent)}
                    >
                      <LogOut size={11.5} strokeWidth={1.7} /> {c.isCurrent ? '登出' : '移除'}
                    </button>
                  </div>
                ))
              )}
            </div>
          </section>

          {/* Section 2: 机器与 Agent */}
          <section className="set-sec">
            <div className="set-hd">
              <h2>机器与 Agent</h2>
              <div style={{ display: 'flex', gap: 6 }}>
                <button
                  type="button"
                  className="btn ghost sm"
                  onClick={() => navigate(paths.onboarding())}
                >
                  <Sparkles size={12} strokeWidth={1.7} /> Agent 接入引导
                </button>
                <button
                  type="button"
                  className="btn pri sm"
                  onClick={() => {
                    setPairBindMachine(undefined);
                    setPairOpen(true);
                  }}
                >
                  <Plus size={12.5} strokeWidth={1.8} /> 添加机器 / 配对码
                </button>
              </div>
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
                const isWorker = curRole === 'worker';

                const toggleCard = () => {
                  consumedInitialRef.current = initialMachineId ?? '__manual__';
                  setConfirmRemoveId(null);
                  setExpandedMachine((cur) => (cur === m.machineId ? '' : m.machineId));
                };

                return (
                  <div className={`set-mach-card ${expanded ? 'open' : ''}`} key={m.machineId}>
                    <div
                      className="set-mach-hd"
                      role="button"
                      tabIndex={0}
                      aria-expanded={expanded}
                      onClick={toggleCard}
                      onKeyDown={(e) => {
                        if (e.key === 'Enter' || e.key === ' ') {
                          e.preventDefault();
                          toggleCard();
                        }
                      }}
                    >
                      <DeviceRoleIcon
                        machine={m}
                        isActiveDuty={isDuty}
                        isCurrentDevice={isCurrent}
                        size={14}
                      />
                      <span className="set-mach-name ell">{label}</span>
                      {m.os && <span className="set-mach-os faint">{m.os}</span>}

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

                        <span className="muted" aria-hidden="true">
                          {expanded ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
                        </span>
                      </div>
                    </div>

                    {expanded && (
                      <div className="set-mach-body">
                        {isWorker && (
                          <div className="worker-scope-note">
                            <Server size={13.5} strokeWidth={1.75} className="muted" aria-hidden="true" />
                            <span>
                              <strong>受限节点权限</strong> · 仅执行显式派发任务，不保存全局历史，且仅限访问下方白名单目录。
                            </span>
                          </div>
                        )}

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
                            {...ime.compositionProps}
                            onKeyDown={(e) => {
                              if (ime.isImeEnter(e)) return;
                              if (e.key === 'Enter') void saveMachineAlias(m);
                            }}
                          />
                          <button type="button" className="btn ghost sm" onClick={() => void saveMachineAlias(m)}>
                            保存
                          </button>

                          {isExec ? (
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
                          ) : (
                            <span className="faint" style={{ marginLeft: 'auto', fontSize: 11.5 }}>
                              受限节点（SSH 目标）
                            </span>
                          )}
                        </div>

                        {m.transportMode !== 'local_ws' && (
                          <div className="set-mach-alias">
                            <span className="muted">SSH 别名</span>
                            <input
                              className="field mono"
                              style={{ maxWidth: 200 }}
                              value={sshAliasEdits[m.machineId] ?? m.sshAlias ?? ''}
                              placeholder={m.tailscaleHost || '~/.ssh/config 中的 Host'}
                              onChange={(e) =>
                                setSshAliasEdits((prev) => ({ ...prev, [m.machineId]: e.target.value }))
                              }
                              {...ime.compositionProps}
                              onKeyDown={(e) => {
                                if (ime.isImeEnter(e)) return;
                                if (e.key === 'Enter') void saveMachineSshAlias(m);
                              }}
                            />
                            <button type="button" className="btn ghost sm" onClick={() => void saveMachineSshAlias(m)}>
                              保存
                            </button>
                            {m.tailscaleHost && (
                              <span className="faint mono ell" style={{ fontSize: 11 }}>
                                {m.tailscaleHost}
                              </span>
                            )}
                          </div>
                        )}

                        {isExec && <AgentList machine={m} showAdvanced />}

                        {/* Allowed working directories / worker whitelist */}
                        <div className="roots-block">
                          <div className="ag-block-lbl">
                            {isWorker ? '允许访问目录白名单' : '工作目录范围'}
                          </div>
                          <div className="roots-list">
                            {(m.allowedRoots ?? []).length === 0 ? (
                              <span className="faint" style={{ fontSize: 12 }}>
                                {isWorker
                                  ? '未配置白名单时默认限制在用户主目录（~），可添加明确允许访问的目录路径'
                                  : '默认使用用户主目录（~）'}
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
                              placeholder={
                                isWorker ? '添加白名单目录路径（如 /srv/apps）' : '添加允许浏览/扫描的路径（如 ~/dev）'
                              }
                              value={rootDrafts[m.machineId] ?? ''}
                              onChange={(e) =>
                                setRootDrafts((d) => ({ ...d, [m.machineId]: e.target.value }))
                              }
                              {...ime.compositionProps}
                              onKeyDown={(e) => {
                                if (ime.isImeEnter(e)) return;
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
                            {m.transportMode !== 'local_ws' && (
                              confirmRemoveId === m.machineId ? (
                                <button
                                  type="button"
                                  data-remove-machine-confirm="1"
                                  className="btn sm set-mach-rm-confirm"
                                  style={{ marginLeft: 'auto' }}
                                  onClick={() => {
                                    setConfirmRemoveId(null);
                                    void removeMachine(m);
                                  }}
                                >
                                  <Trash2 size={12} strokeWidth={1.8} /> 确认移除
                                </button>
                              ) : (
                                <button
                                  type="button"
                                  data-remove-machine-confirm="1"
                                  className="btn ghost sm bad-t"
                                  style={{ marginLeft: 'auto' }}
                                  onClick={() => setConfirmRemoveId(m.machineId)}
                                >
                                  <Trash2 size={12} strokeWidth={1.7} /> 移除设备
                                </button>
                              )
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
        onPaired={(machineId) => {
          consumedInitialRef.current = initialMachineId ?? '__paired__';
          setExpandedMachine(machineId);
          refreshAll();
        }}
        onClose={() => {
          setPairOpen(false);
          setPairBindMachine(undefined);
        }}
      />
    </main>
  );
}
