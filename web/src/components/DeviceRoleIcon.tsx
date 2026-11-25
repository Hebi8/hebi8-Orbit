import { Crown, Laptop, Server } from 'lucide-react';
import type { MachineView } from '../generated/protocol';

export interface DeviceRoleIconProps {
  machine: Pick<MachineView, 'machineId' | 'role' | 'status' | 'transportMode' | 'os'>;
  isActiveDuty?: boolean;
  isCurrentDevice?: boolean;
  size?: number;
}

/**
 * Semantic colored device role icon (replaces generic dots & stacked pills):
 * - Crown: active primary controller (主控)
 * - Server: restricted worker / deploy target (受限节点)
 * - Laptop: full peer device (完整设备)
 * Color reflects connectivity (green = online, amber = degraded, gray = offline).
 * A subtle 5px accent corner dot indicates the current client device.
 */
export function DeviceRoleIcon({
  machine,
  isActiveDuty = false,
  isCurrentDevice = false,
  size = 14,
}: DeviceRoleIconProps) {
  const isWorker = machine.role === 'worker' || machine.transportMode === 'ssh_outbound';
  const toneClass =
    machine.status === 'offline' ? 'off' : machine.status === 'degraded' ? 'mid' : 'good';

  const roleHint = isActiveDuty ? '当前主控' : isWorker ? '受限节点' : '完整设备';
  const statusHint = machine.status === 'offline' ? '离线' : machine.status === 'degraded' ? '不稳定' : '在线';
  const title = `${roleHint} · ${statusHint}${isCurrentDevice ? ' · 当前设备' : ''}`;

  return (
    <span className={`dev-ico ${toneClass}`} title={title} aria-label={title}>
      {isActiveDuty ? (
        <Crown size={size} strokeWidth={1.85} />
      ) : isWorker ? (
        <Server size={size} strokeWidth={1.75} />
      ) : (
        <Laptop size={size} strokeWidth={1.75} />
      )}
      {isCurrentDevice && <span className="dev-self-dot" aria-hidden="true" />}
    </span>
  );
}
