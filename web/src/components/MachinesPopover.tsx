import { X } from 'lucide-react';
import type { MachineView } from '../generated/protocol';
import { relTime } from '../lib/format';
import { useWorkbenchStore } from '../store/useWorkbenchStore';

const OK_RESOURCE_STATES = new Set(['free', 'available', 'ok', 'idle', 'ready', 'unlocked', 'online']);

function transportLabel(t: MachineView['transportMode']): string {
  return t === 'local_ws' ? 'Hub 本机' : t === 'remote_ws' ? '远端 Runner' : 'SSH 出站';
}

export function MachinesPopover({ machines }: { machines: MachineView[] }) {
  const setOpen = useWorkbenchStore((s) => s.setMachinesOpen);
  const localMachineId = useWorkbenchStore((s) => s.localMachineId);
  const setLocal = useWorkbenchStore((s) => s.setLocalMachineId);
  return (
    <>
      <div className="scrim" style={{ background: 'transparent', backdropFilter: 'none', zIndex: 54 }} onClick={() => setOpen(false)} />
      <section className="pop mach" style={{ left: 10, bottom: 50, width: 340, maxHeight: 'calc(100vh - 80px)' }} role="dialog" aria-label="机器">
        <div className="pop-h">
          <span className="t">机器</span>
          <span className="s">状态 · 能力 · 独占资源</span>
          <button type="button" className="ib" title="关闭 (Esc)" onClick={() => setOpen(false)}>
            <X size={14} strokeWidth={1.6} />
          </button>
        </div>
        <div className="pop-b">
          {machines.map((m) => {
            const res = Object.entries(m.resources ?? {});
            return (
              <div className="mrow" key={m.machineId}>
                <div className="h">
                  <span className={`sd ${m.status === 'online' ? 'on' : m.status === 'degraded' ? 'wait' : ''}`} />
                  <b className="ell">{m.displayName}</b>
                  <span className="r">
                    {m.status === 'online' ? '在线' : m.status === 'degraded' ? '降级' : '离线'}
                    {m.lastSeenAt ? ` · ${relTime(m.lastSeenAt)}` : ''}
                  </span>
                </div>
                <div className="locks">
                  <span className="k">接入</span>
                  <span>
                    {transportLabel(m.transportMode)}
                    {m.runnerVersion ? ` · runner ${m.runnerVersion}` : ''}
                  </span>
                  {m.capabilities.length > 0 && (
                    <>
                      <span className="k">能力</span>
                      <span style={{ wordBreak: 'break-word' }}>{m.capabilities.join(' · ')}</span>
                    </>
                  )}
                  {res.map(([k, v]) => {
                    const busy = !OK_RESOURCE_STATES.has((v.state || '').toLowerCase());
                    return (
                      <span key={k} style={{ display: 'contents' }}>
                        <span className="k mono" title={k}>
                          {k}
                        </span>
                        <span className={busy ? 'w' : ''}>
                          {v.state}
                          {v.reason ? ` · ${v.reason}` : ''}
                        </span>
                      </span>
                    );
                  })}
                </div>
              </div>
            );
          })}
          <div className="mrow">
            <div className="locks" style={{ marginTop: 0, alignItems: 'center' }}>
              <span className="k">本机</span>
              <select className="field" value={localMachineId} onChange={(e) => setLocal(e.target.value)} style={{ height: 26, padding: '0 22px 0 8px' }}>
                <option value="">（未指定 · IDE 走 SSH Remote）</option>
                {machines.map((m) => (
                  <option key={m.machineId} value={m.machineId}>
                    {m.machineId}
                  </option>
                ))}
              </select>
            </div>
          </div>
        </div>
      </section>
    </>
  );
}
