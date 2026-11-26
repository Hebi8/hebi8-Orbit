import type { AgentModeOption, AgentProbe, MachineView, SessionAdapter } from '../generated/protocol';

export const ADAPTER_LABELS: Record<string, string> = {
  claude: 'Claude Code',
  codex: 'Codex',
  agy: 'Antigravity',
};

const SAFE_AUTO_MODE_CANDIDATES = ['auto', 'agent', 'acceptEdits', 'accept-edits', 'workspace-write'] as const;

export type PermissionIconKind = 'auto' | 'ask' | 'plan' | 'yolo';

export function isFullBypassMode(modeId: string | undefined | null): boolean {
  switch ((modeId ?? '').trim().toLowerCase()) {
    case 'full':
    case 'bypass':
    case 'bypasspermissions':
    case 'agent-full-access':
    case 'yolo':
      return true;
    default:
      return false;
  }
}

/** Classifies an ACP permission mode ID into one of the 4 low-stimulus geometric icon families. */
export function classifyPermissionMode(modeId: string | undefined | null): PermissionIconKind {
  const id = (modeId ?? '').trim().toLowerCase();
  if (!id) return 'auto';
  if (isFullBypassMode(id)) return 'yolo';
  if (/(plan|read[-_]?only|architect|explore)/.test(id)) return 'plan';
  if (/(ask|confirm|prompt|suggest|untrusted|default)/.test(id)) return 'ask';
  return 'auto';
}

/**
 * Full-bypass permission overrides are strictly one-shot per user submission:
 * reset to '' after sending so the next plain follow-up never silently re-sends explicitFullPermission.
 * Non-bypass modes (e.g. auto, plan) are preserved for smooth UX.
 */
export function nextModeOverrideAfterSend(currentMode: string): string {
  return isFullBypassMode(currentMode) ? '' : currentMode;
}

export function findAgentProbe(
  machines: readonly MachineView[] | undefined,
  preferredMachineId: string | undefined,
  adapter: SessionAdapter | string,
): AgentProbe | undefined {
  if (!machines || !adapter) return undefined;
  if (preferredMachineId) {
    const mach = machines.find((m) => m.machineId === preferredMachineId);
    // When a specific execution machine is selected, NEVER borrow capabilities from another machine
    // (even if the selected machine is not present in the list).
    return mach?.agents?.find((a) => a.adapter === adapter);
  }
  for (const mach of machines) {
    if (mach.status !== 'online') continue;
    const p = mach.agents?.find((a) => a.adapter === adapter && a.state === 'ready' && a.enabled);
    if (p) return p;
  }
  for (const mach of machines) {
    const p = mach.agents?.find((a) => a.adapter === adapter);
    if (p) return p;
  }
  return undefined;
}

export function probeModels(probe: AgentProbe | undefined, _adapter: string, current?: string): string[] {
  // Never fabricate hardcoded fake model IDs when a real probe exists (or is missing); let UI follow Agent default.
  const base = probe?.models && probe.models.length > 0 ? probe.models : [];
  const out = [...base];
  if (current && !out.includes(current)) {
    out.unshift(current);
  }
  return out;
}

export function probeEfforts(probe: AgentProbe | undefined, current?: string): string[] {
  const base = probe?.efforts && probe.efforts.length > 0 ? probe.efforts : [];
  const out = [...base];
  if (current && !out.includes(current)) {
    out.unshift(current);
  }
  return out;
}

export function probeModes(probe: AgentProbe | undefined, _adapter: string, current?: string): AgentModeOption[] {
  const base = probe?.modes && probe.modes.length > 0 ? probe.modes : [];
  const out = [...base];
  if (current && !out.some((o) => o.id === current)) {
    out.unshift({ id: current, name: current });
  }
  return out;
}

/**
 * Returns an automatic permission mode override ONLY for PA (preferAuto=true), strictly bounded
 * to safe non-bypass modes. For non-PA tracks (preferAuto=false), returns '' so the UI displays
 * the Agent's default without writing probe.defaultMode into config.mode (never auto-authorizing full/bypass).
 */
export function defaultModeFor(probe: AgentProbe | undefined, adapter: string, preferAuto: boolean): string {
  if (!preferAuto) return '';
  const modes = probeModes(probe, adapter);
  for (const candidate of SAFE_AUTO_MODE_CANDIDATES) {
    const hit = modes.find((m) => m.id.toLowerCase() === candidate.toLowerCase());
    if (hit && !isFullBypassMode(hit.id)) return hit.id;
  }
  return '';
}

export function defaultModeDisplayLabel(probe: AgentProbe | undefined, modes: readonly AgentModeOption[]): string {
  const defId = probe?.defaultMode?.trim();
  if (!defId) return '跟随 Agent 默认值';
  const opt = modes.find((m) => m.id === defId);
  return `默认 (${opt ? modeLabel(opt) : defId})`;
}

export function effortLabel(v: string): string {
  switch (v.toLowerCase()) {
    case 'default':
      return '默认';
    case 'low':
      return 'Low';
    case 'medium':
      return 'Medium';
    case 'high':
      return 'High';
    case 'xhigh':
      return 'XHigh';
    case 'max':
      return 'Max';
    case 'ultra':
      return 'Ultra';
    default:
      return v;
  }
}

export function modeLabel(opt: AgentModeOption): string {
  return opt.name?.trim() || opt.id;
}
