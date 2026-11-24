import { X } from 'lucide-react';
import type { MachineView } from '../generated/protocol';
import { useHubLatency } from '../hooks/useHubLatency';
import { relTime } from '../lib/format';
import { useWorkbenchStore } from '../store/useWorkbenchStore';

const OK_RESOURCE_STATES = new Set(['free', 'available', 'ok', 'idle', 'ready', 'unlocked', 'online']);

function MachineBlock({ m, latency }: { m: MachineView; latency?: number | null }) {
  const res = Object.entries(m.resources ?? {});
  const busyRes = res.filter(([, v]) => !OK_RESOURCE_STATES.has((v.state || '').toLowerCase()));
  return (
    <div className="mrow">
      <div className="h">
        <span className={`sd ${m.status === 'online' ? 'on' : m.status === 'degraded' ? 'wait' : ''}`} />
        <b className="ell">{m.displayName}</b>
        <span className="r">
          {m.status === 'online' ? '在线' : m.status === 'degraded' ? '降级' : '离线'}
          {typeof latency === 'number' ? ` · ${latency} ms` : m.lastSeenAt ? ` · ${relTime(m.lastSeenAt)}` : ''}
        </span>
      </div>
      {(m.runnerVersion || busyRes.length > 0) && (
        <div className="locks">
          {m.runnerVersion && (
            <>
              <span className="k">Runner</span>
              <span className="mono">{m.runnerVersion}</span>
            </>
          )}
          {busyRes.map(([k, v]) => (
            <span key={k} style={{ display: 'contents' }}>
              <span className="k mono" title={k}>
                {k}
              </span>
              <span className="w">
                {v.state}
                {v.reason ? ` · ${v.reason}` : ''}
              </span>
            </span>
          ))}
        </div>
      )}
    </div>
  );
}

export function MachinesPopover({ machines }: { machines: MachineView[] }) {
  const setOpen = useWorkbenchStore((s) => s.setMachinesOpen);
  const localMachineId = useWorkbenchStore((s) => s.localMachineId);
  const setLocal = useWorkbenchStore((s) => s.setLocalMachineId);
  const latency = useHubLatency();

  const currentHost =
    machines.find((m) => m.transportMode === 'local_ws') ??
    machines.find((m) => m.machineId === localMachineId) ??
    machines[0];
  const others = currentHost ? machines.filter((m) => m.machineId !== currentHost.machineId) : [];

  return (
    <>
      <div className="scrim" style={{ background: 'transparent', backdropFilter: 'none', zIndex: 54 }} onClick={() => setOpen(false)} />
      <section className="pop mach" style={{ left: 10, bottom: 48, width: 310, maxHeight: 'calc(100vh - 80px)' }} role="dialog" aria-label="当前主机">
        <div className="pop-h">
          <span className="t">当前主机</span>
          <button type="button" className="ib" title="关闭 (Esc)" onClick={() => setOpen(false)}>
            <X size={14} strokeWidth={1.6} />
          </button>
        </div>
        <div className="pop-b">
          {currentHost ? (
            <MachineBlock m={currentHost} latency={latency} />
          ) : (
            <div className="mrow faint">未检测到主机</div>
          )}
          {machines.length > 0 && (
            <div className="mrow">
              <div className="locks" style={{ marginTop: 0, alignItems: 'center' }}>
                <span className="k">本机 IDE</span>
                <select className="field" value={localMachineId} onChange={(e) => setLocal(e.target.value)} style={{ height: 26, padding: '0 22px 0 8px' }}>
                  <option value="">自动（SSH Remote）</option>
                  {machines.map((m) => (
                    <option key={m.machineId} value={m.machineId}>
                      {m.machineId}
                    </option>
                  ))}
                </select>
              </div>
            </div>
          )}
          {others.length > 0 && (
            <details className="m-others">
              <summary>其他节点</summary>
              {others.map((m) => (
                <MachineBlock key={m.machineId} m={m} />
              ))}
            </details>
          )}
        </div>
      </section>
    </>
  );
}
