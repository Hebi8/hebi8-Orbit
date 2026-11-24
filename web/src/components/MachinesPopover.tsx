import { useState } from 'react';
import { SlidersHorizontal } from 'lucide-react';
import type { MachineView } from '../generated/protocol';
import { useHubLatency } from '../hooks/useHubLatency';
import { cleanMachineName, humanResourceLabel, relTime } from '../lib/format';
import { resolveEffectiveLocalMachineId } from '../lib/ide';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';

const OK_RESOURCE_STATES = new Set(['free', 'available', 'ok', 'idle', 'ready', 'unlocked', 'online']);

function nodeTone(m: MachineView, latency?: number | null): 'good' | 'mid' | 'off' {
  if (m.status === 'offline' || latency === null) return 'off';
  if (m.status === 'degraded' || (typeof latency === 'number' && latency >= 60)) return 'mid';
  return 'good';
}

function NodeRow({ m, latency, isLocal }: { m: MachineView; latency?: number | null; isLocal: boolean }) {
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
      <div className={`node-row ${tone === 'off' ? 'is-off' : ''}`}>
        <span className={`node-dot ${tone}`} aria-hidden="true" />
        <span className="node-name" title={m.runnerVersion ? `${m.machineId} · Runner ${m.runnerVersion}` : m.machineId}>
          {cleanMachineName(m)}
        </span>
        {isLocal && <span className="node-local-pill">本机</span>}
        <span className={`node-right mono ${tone}`}>{rightText}</span>
      </div>
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
}: {
  machines: MachineView[];
  defaultWorkspaceName?: string;
}) {
  const setOpen = useWorkbenchStore((s) => s.setMachinesOpen);
  const storedLocalId = useWorkbenchStore((s) => s.localMachineId);
  const workspaceNameOverride = useWorkbenchStore((s) => s.workspaceNameOverride);
  const setWorkspaceNameOverride = useWorkbenchStore((s) => s.setWorkspaceNameOverride);
  const latency = useHubLatency();

  const workspaceName = workspaceNameOverride || defaultWorkspaceName || 'Home LAN';
  const [renaming, setRenaming] = useState(false);
  const [draftName, setDraftName] = useState(workspaceName);

  const effectiveLocalId = resolveEffectiveLocalMachineId(machines, storedLocalId);
  const onlineCount = machines.filter((m) => m.status === 'online').length;
  const anyOnline = machines.length === 0 || onlineCount > 0;
  const headerTone = latency === null || !anyOnline ? 'off' : typeof latency === 'number' && latency >= 80 ? 'mid' : 'good';

  const saveName = () => {
    setWorkspaceNameOverride(draftName);
    setRenaming(false);
    toast(`已更新空间名称：${draftName.trim() || defaultWorkspaceName || 'Home LAN'}`);
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
              <div className="ws-pop-sub">
                {headerTone === 'off' ? '未连接' : `已连接 · ${onlineCount}/${machines.length} 在线`}
              </div>
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
                  if (e.key === 'Enter') saveName();
                  if (e.key === 'Escape') setRenaming(false);
                }}
                autoFocus
                aria-label="全局空间名称"
              />
              <button type="button" className="btn pri" onClick={saveName}>
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
                    isLocal={effectiveLocalId ? m.machineId === effectiveLocalId : isHubLocal}
                  />
                );
              })
            )}
          </div>

          <div className="ws-pop-foot">
            <button
              type="button"
              className="ws-settings-btn"
              onClick={() => {
                setOpen(false);
                toast('节点增删可在 ~/.config/hebi8-orbit/machines.yaml 配置后热重载');
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
