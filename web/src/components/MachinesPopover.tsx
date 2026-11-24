import { CircleAlert } from 'lucide-react';
import type { MachineView } from '../generated/protocol';
import { useHubLatency } from '../hooks/useHubLatency';
import { humanResourceLabel, relTime } from '../lib/format';
import { useWorkbenchStore } from '../store/useWorkbenchStore';

const OK_RESOURCE_STATES = new Set(['free', 'available', 'ok', 'idle', 'ready', 'unlocked', 'online']);

function MachineBlock({ m, latency, isCurrent }: { m: MachineView; latency?: number | null; isCurrent?: boolean }) {
  const res = Object.entries(m.resources ?? {});
  const busyRes = res.filter(([, v]) => !OK_RESOURCE_STATES.has((v.state || '').toLowerCase()));
  const unreachable = (isCurrent && latency === null) || m.status === 'offline';
  const degraded = !unreachable && m.status === 'degraded';

  const statusText = unreachable
    ? `未连接${m.lastSeenAt ? ` · ${relTime(m.lastSeenAt)}见过` : ''}`
    : degraded
      ? `降级${m.lastSeenAt ? ` · ${relTime(m.lastSeenAt)}` : ''}`
      : typeof latency === 'number'
        ? `${latency} ms`
        : '在线';

  return (
    <div className="mrow">
      <div className="h">
        <b className="ell" title={m.runnerVersion ? `Runner ${m.runnerVersion}` : undefined}>
          {m.displayName}
        </b>
        <span className={`r ${unreachable ? 'bad-t' : degraded ? 'warn-t' : ''}`}>{statusText}</span>
      </div>
      {busyRes.length > 0 && (
        <div className="mbusy">
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
  const otherIssue = others.some((m) => m.status !== 'online');

  return (
    <>
      <div
        className="scrim"
        style={{ background: 'transparent', backdropFilter: 'none', zIndex: 54 }}
        onClick={() => setOpen(false)}
      />
      <section
        className="pop mach"
        style={{ left: 10, bottom: 44, width: 292, maxHeight: 'calc(100vh - 80px)' }}
        role="dialog"
        aria-label="主机连接状态"
      >
        <div className="pop-b">
          {currentHost ? (
            <MachineBlock m={currentHost} latency={latency} isCurrent />
          ) : (
            <div className="mrow faint">未检测到主机</div>
          )}
          {others.length > 0 && (
            <details className="m-others">
              <summary>
                <span>其他节点</span>
                {otherIssue && (
                  <CircleAlert
                    size={12}
                    strokeWidth={2}
                    className="warn-t"
                    style={{ marginLeft: 6 }}
                    aria-label="有节点异常"
                  />
                )}
              </summary>
              {others.map((m) => (
                <MachineBlock key={m.machineId} m={m} />
              ))}
            </details>
          )}
          {machines.length > 0 && (
            <div className="mrow m-ide">
              <span className="faint">Cursor 打开方式</span>
              <select
                className="field"
                value={localMachineId}
                onChange={(e) => setLocal(e.target.value)}
                aria-label="Cursor 打开方式"
              >
                <option value="">自动（SSH Remote）</option>
                {machines.map((m) => (
                  <option key={m.machineId} value={m.machineId}>
                    {m.displayName || m.machineId}
                  </option>
                ))}
              </select>
            </div>
          )}
        </div>
      </section>
    </>
  );
}
