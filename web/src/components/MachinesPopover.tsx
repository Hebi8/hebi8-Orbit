import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { ArrowLeftRight, SlidersHorizontal } from 'lucide-react';
import type { MachineView } from '../generated/protocol';
import { useHubLatency } from '../hooks/useHubLatency';
import { api, errMsg, isDesktopShell, openDesktopSetup } from '../lib/api';
import { cleanMachineName, humanResourceLabel, relTime } from '../lib/format';
import { resolveEffectiveLocalMachineId } from '../lib/ide';
import { navigate, paths } from '../lib/router';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';
import { DeviceRoleIcon } from './DeviceRoleIcon';

const OK_RESOURCE_STATES = new Set(['free', 'available', 'ok', 'idle', 'ready', 'unlocked', 'online']);

function nodeTone(m: MachineView, latency?: number | null): 'good' | 'mid' | 'off' {
  if (m.status === 'offline' || latency === null) return 'off';
  if (m.status === 'degraded' || (typeof latency === 'number' && latency >= 60)) return 'mid';
  return 'good';
}

function NodeRow({
  m,
  latency,
  isActiveDuty,
  isLocal,
  onOpen,
}: {
  m: MachineView;
  latency?: number | null;
  isActiveDuty: boolean;
  isLocal: boolean;
  onOpen: () => void;
}) {
  const res = Object.entries(m.resources ?? {});
  const busyRes = res.filter(([, v]) => !OK_RESOURCE_STATES.has((v.state || '').toLowerCase()));
  const tone = nodeTone(m, latency);

  const rightText =
    tone === 'off'
      ? `离线${m.lastSeenAt ? ` · ${relTime(m.lastSeenAt)}` : ''}`
      : typeof latency === 'number'
        ? `${latency}ms`
        : m.status === 'degraded'
          ? '高延迟'
          : '在线';

  return (
    <div className={`node-row-wrap ${tone === 'off' ? 'is-off' : ''}`}>
      <button
        type="button"
        className={`node-row ${tone === 'off' ? 'is-off' : ''}`}
        onClick={onOpen}
        title={m.runnerVersion ? `${m.machineId} · ${m.runnerVersion} · 点击管理` : `${m.machineId} · 点击管理`}
      >
        <DeviceRoleIcon machine={m} isActiveDuty={isActiveDuty} isCurrentDevice={isLocal} size={13.5} />
        <span className="node-name">{m.displayAlias || cleanMachineName(m)}</span>
        <span className={`node-right mono ${tone}`}>{rightText}</span>
      </button>
      {busyRes.length > 0 && (
        <div className="mbusy" style={{ paddingLeft: 22, paddingBottom: 4 }}>
          {busyRes.map(([k, v]) => (
            <div key={k} className="mbusy-i" title={`${k}: ${v.state}`}>
              {humanResourceLabel(k, v.state, v.reason)}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

export function MachinesPopover({
  machines,
  defaultWorkspaceName,
  dutyPriority,
  activeDutyMachineId,
  currentMachineId,
}: {
  machines: MachineView[];
  defaultWorkspaceName?: string;
  dutyPriority?: string[];
  activeDutyMachineId?: string;
  currentMachineId?: string;
}) {
  const qc = useQueryClient();
  const setOpen = useWorkbenchStore((s) => s.setMachinesOpen);
  const storedLocalId = useWorkbenchStore((s) => s.localMachineId);
  const workspaceNameOverride = useWorkbenchStore((s) => s.workspaceNameOverride);
  const setWorkspaceNameOverride = useWorkbenchStore((s) => s.setWorkspaceNameOverride);
  const latency = useHubLatency();

  const workspaceName = workspaceNameOverride || defaultWorkspaceName || 'Home LAN';
  const [renaming, setRenaming] = useState(false);
  const [draftName, setDraftName] = useState(workspaceName);

  const effectiveLocalId = resolveEffectiveLocalMachineId(machines, currentMachineId || storedLocalId);
  const onlineCount = machines.filter((m) => m.status === 'online').length;
  const anyOnline = machines.length === 0 || onlineCount > 0;

  const primaryDutyId = dutyPriority?.[0] ?? '';
  const primaryMachine = primaryDutyId ? machines.find((m) => m.machineId === primaryDutyId) : undefined;
  const activeDutyMachine = activeDutyMachineId
    ? machines.find((m) => m.machineId === activeDutyMachineId)
    : undefined;
  const isFailoverActive =
    !!primaryDutyId &&
    !!activeDutyMachineId &&
    primaryDutyId !== activeDutyMachineId &&
    (!primaryMachine || primaryMachine.status !== 'online');

  const headerTone =
    latency === null || !anyOnline
      ? 'off'
      : isFailoverActive || (typeof latency === 'number' && latency >= 80)
        ? 'mid'
        : 'good';

  const primaryName = primaryMachine ? primaryMachine.displayAlias || cleanMachineName(primaryMachine) : primaryDutyId;
  const dutyName = activeDutyMachine
    ? activeDutyMachine.displayAlias || cleanMachineName(activeDutyMachine)
    : activeDutyMachineId || '';
  const subText =
    headerTone === 'off'
      ? '未连接'
      : isFailoverActive
        ? `${primaryName} 离线 · ${dutyName} 接管`
        : `已连接 · ${onlineCount}/${machines.length} 在线`;

  const saveName = async () => {
    const trimmed = draftName.trim();
    try {
      await api.updateNetworkSettings({ workspaceName: trimmed });
      setWorkspaceNameOverride('');
      setRenaming(false);
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
      void qc.invalidateQueries({ queryKey: ['network-settings'] });
      toast(`已更新空间名称：${trimmed || defaultWorkspaceName || 'Home LAN'}`);
    } catch (e) {
      toast(errMsg(e), 'err');
    }
  };

  return (
    <>
      <div
        className="scrim"
        style={{ background: 'transparent', backdropFilter: 'none', zIndex: 54 }}
        onClick={() => setOpen(false)}
      />
      <section
        className="pop mach ws-pop"
        style={{ left: 10, bottom: 48, width: 268, maxHeight: 'calc(100vh - 80px)' }}
        role="dialog"
        aria-label="网络与节点状态"
      >
        <div className="pop-b">
          <div className="ws-pop-hd">
            <div>
              <div className="ws-pop-title">
                <span className={`ws-dot ${headerTone}`} aria-hidden="true" />
                <span>{workspaceName}</span>
              </div>
              <div className="ws-pop-sub">{subText}</div>
            </div>
            <button
              type="button"
              className="ws-rename-btn"
              onClick={() => {
                setDraftName(workspaceName);
                setRenaming((v) => !v);
              }}
            >
              {renaming ? '取消' : '改名'}
            </button>
          </div>

          {renaming && (
            <div className="ws-rename-row">
              <input
                className="ws-rename-input"
                value={draftName}
                onChange={(e) => setDraftName(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') void saveName();
                  if (e.key === 'Escape') setRenaming(false);
                }}
                autoFocus
                aria-label="全局空间名称"
              />
              <button type="button" className="btn pri" onClick={() => void saveName()}>
                确定
              </button>
            </div>
          )}

          <div className="ws-section-lbl">节点</div>
          <div className="node-list">
            {machines.length === 0 ? (
              <div className="node-row faint">未检测到节点</div>
            ) : (
              machines.map((m) => {
                const isHubLocal = m.transportMode === 'local_ws';
                return (
                  <NodeRow
                    key={m.machineId}
                    m={m}
                    latency={isHubLocal ? latency : undefined}
                    isActiveDuty={activeDutyMachineId ? m.machineId === activeDutyMachineId : isHubLocal}
                    isLocal={!!effectiveLocalId && m.machineId === effectiveLocalId}
                    onOpen={() => {
                      setOpen(false);
                      navigate(paths.settings(m.machineId));
                    }}
                  />
                );
              })
            )}
          </div>

          <div className="ws-pop-foot">
            {isDesktopShell() && (
              <button
                type="button"
                className="ws-settings-btn"
                onClick={() => {
                  setOpen(false);
                  openDesktopSetup();
                }}
              >
                <ArrowLeftRight size={12} strokeWidth={1.6} />
                <span>切换网络</span>
              </button>
            )}
            <button
              type="button"
              className="ws-settings-btn"
              onClick={() => {
                setOpen(false);
                navigate(paths.settings());
              }}
            >
              <SlidersHorizontal size={12} strokeWidth={1.6} />
              <span>网络与节点设置</span>
            </button>
          </div>
        </div>
      </section>
    </>
  );
}
