import type { AgentQuotaSnapshot, MachineView, QuotaWindow } from '../generated/protocol';

export interface ResolvedAgentQuota {
  machineId: string;
  machineOnline: boolean;
  canRefresh: boolean;
  adapter: string;
  available: boolean;
  group?: string;
  win5h?: QuotaWindow;
  win7d?: QuotaWindow;
  /** Tightest remaining percentage across matched 5h / 7d windows (0..100), or null if unavailable. */
  primaryRemainingPercent: number | null;
  fetchedAt?: number;
  error?: string;
}

export interface ResolveAgentQuotaParams {
  machines?: readonly MachineView[];
  machineId?: string;
  adapter?: string;
  model?: string;
}

function normToken(s: string): string {
  return s.toLowerCase().replace(/[^a-z0-9]+/g, '');
}

function findSnapshotOnMachine(machine: MachineView | undefined, adapter: string): AgentQuotaSnapshot | undefined {
  if (!machine || !adapter) return undefined;
  const fromAgent = machine.agents?.find((a) => a.adapter === adapter)?.quota;
  if (fromAgent) return fromAgent;
  return machine.agentQuotas?.find((q) => q.adapter === adapter);
}

function resolveTargetMachine(
  machines: readonly MachineView[] | undefined,
  preferredMachineId: string | undefined,
  adapter: string,
): MachineView | undefined {
  if (!machines || machines.length === 0) return undefined;
  if (preferredMachineId) {
    return machines.find((m) => m.machineId === preferredMachineId);
  }

  const execOnline = machines.filter((m) => m.transportMode !== 'ssh_outbound' && m.status === 'online');
  if (adapter) {
    const localWithQuota = execOnline.find(
      (m) => m.transportMode === 'local_ws' && findSnapshotOnMachine(m, adapter)?.available,
    );
    if (localWithQuota) return localWithQuota;

    const anyWithQuota = execOnline.find((m) => findSnapshotOnMachine(m, adapter)?.available);
    if (anyWithQuota) return anyWithQuota;

    const localWithReady = execOnline.find(
      (m) =>
        m.transportMode === 'local_ws' &&
        (m.agents ?? []).some((a) => a.adapter === adapter && a.state === 'ready' && a.enabled),
    );
    if (localWithReady) return localWithReady;

    const anyWithReady = execOnline.find((m) =>
      (m.agents ?? []).some((a) => a.adapter === adapter && a.state === 'ready' && a.enabled),
    );
    if (anyWithReady) return anyWithReady;
  }

  const localOnline = execOnline.find((m) => m.transportMode === 'local_ws');
  if (localOnline) return localOnline;
  if (execOnline[0]) return execOnline[0];
  return machines.find((m) => m.transportMode !== 'ssh_outbound') ?? machines[0];
}

function scoreWindowMatch(w: QuotaWindow, adapter: string, model: string): number {
  const m = model.trim().toLowerCase();
  if (!m) return 0;
  const mNorm = normToken(m);
  const hints = w.modelHints ?? [];

  for (const rawHint of hints) {
    const h = rawHint.trim().toLowerCase();
    if (!h) continue;
    const hNorm = normToken(h);
    if (h === m || (hNorm && hNorm === mNorm)) return 100;
    if (m.includes(h) || h.includes(m)) return 82;
    if (hNorm.length >= 3 && (mNorm.includes(hNorm) || hNorm.includes(mNorm))) return 76;
  }

  const grp = (w.group ?? '').trim().toLowerCase();
  if (!grp) return 0;

  if (adapter === 'agy') {
    if (m.includes('gemini') && grp.includes('gemini')) return 60;
    if (
      (m.includes('claude') ||
        m.includes('gpt') ||
        m.includes('opus') ||
        m.includes('sonnet') ||
        m.includes('haiku') ||
        /\bo[134]\b/.test(m)) &&
      (grp.includes('claude') || grp.includes('gpt'))
    ) {
      return 60;
    }
  } else if (adapter === 'claude') {
    if (m.includes('opus') && grp.includes('opus')) return 65;
    if (m.includes('sonnet') && grp.includes('sonnet')) return 65;
    if (m.includes('haiku') && grp.includes('haiku')) return 65;
  } else if (adapter === 'codex') {
    const grpNorm = normToken(grp);
    if (m.includes(grp) || grp.includes(m) || (grpNorm.length >= 3 && mNorm.includes(grpNorm))) {
      return 60;
    }
  }

  return 0;
}

function isWindow5h(w: QuotaWindow): boolean {
  return w.window.trim().toLowerCase() === '5h';
}

function isWindow7d(w: QuotaWindow): boolean {
  return w.window.trim().toLowerCase() === '7d';
}

function groupKey(w: QuotaWindow): string {
  return (w.group ?? '').trim();
}

/**
 * Resolves real 5h / 7d quota windows for the active machine, adapter, and selected model.
 * Never fabricates numbers when the target machine is offline or quota telemetry is unavailable.
 */
export function resolveAgentQuota({
  machines,
  machineId,
  adapter = '',
  model = '',
}: ResolveAgentQuotaParams): ResolvedAgentQuota {
  const target = resolveTargetMachine(machines, machineId, adapter);
  const resolvedMachineId = target?.machineId ?? machineId ?? '';
  const machineOnline = !!target && target.status === 'online';
  const canRefresh =
    machineOnline &&
    target.transportMode !== 'ssh_outbound' &&
    (!(target.runnerCapabilities && target.runnerCapabilities.length > 0) ||
      target.runnerCapabilities.includes('quotas.v1'));

  if (!target || !machineOnline || !adapter) {
    return {
      machineId: resolvedMachineId,
      machineOnline,
      canRefresh,
      adapter,
      available: false,
      primaryRemainingPercent: null,
    };
  }

  const snap = findSnapshotOnMachine(target, adapter);
  if (!snap || !snap.available || !snap.windows || snap.windows.length === 0) {
    return {
      machineId: resolvedMachineId,
      machineOnline,
      canRefresh,
      adapter,
      available: false,
      primaryRemainingPercent: null,
      fetchedAt: snap?.fetchedAt,
      error: snap?.error,
    };
  }

  const windows = snap.windows;
  const genericWindows = windows.filter((w) => groupKey(w) === '');

  let bestScore = 0;
  let bestGroup = '';
  for (const w of windows) {
    const sc = scoreWindowMatch(w, adapter, model);
    if (sc > bestScore) {
      bestScore = sc;
      bestGroup = groupKey(w);
    }
  }

  let win5h: QuotaWindow | undefined;
  let win7d: QuotaWindow | undefined;
  let resolvedGroup = '';

  if (bestScore > 0 && bestGroup !== '') {
    const groupWins = windows.filter((w) => groupKey(w) === bestGroup);
    // Prefer the matched model group; fall back to generic ungrouped windows (e.g. Claude's shared 5h)
    // without ever crossing over into a different model family group.
    win5h = groupWins.find(isWindow5h) ?? genericWindows.find(isWindow5h);
    win7d = groupWins.find(isWindow7d) ?? genericWindows.find(isWindow7d);
    resolvedGroup = bestGroup;
  } else if (genericWindows.some((w) => isWindow5h(w) || isWindow7d(w))) {
    win5h = genericWindows.find(isWindow5h);
    win7d = genericWindows.find(isWindow7d);
  } else {
    const firstValid = windows.find((w) => isWindow5h(w) || isWindow7d(w));
    const fallbackGroup = firstValid ? groupKey(firstValid) : '';
    const groupWins = windows.filter((w) => groupKey(w) === fallbackGroup);
    win5h = groupWins.find(isWindow5h);
    win7d = groupWins.find(isWindow7d);
    resolvedGroup = fallbackGroup;
  }

  const remValues = [win5h?.remainingPercent, win7d?.remainingPercent].filter(
    (v): v is number => typeof v === 'number' && Number.isFinite(v),
  );
  const primaryRemainingPercent =
    remValues.length > 0 ? Math.min(100, Math.max(0, Math.round(Math.min(...remValues)))) : null;

  return {
    machineId: resolvedMachineId,
    machineOnline,
    canRefresh,
    adapter,
    available: primaryRemainingPercent !== null,
    group: resolvedGroup || undefined,
    win5h,
    win7d,
    primaryRemainingPercent,
    fetchedAt: snap.fetchedAt,
    error: snap.error,
  };
}

/**
 * Formats a Unix-millisecond reset timestamp into concise, human-readable Chinese copy.
 * Returns empty string for missing, zero, or already-elapsed timestamps (never shows 1970).
 */
export function formatQuotaResetTime(resetsAt?: number | null, now = Date.now()): string {
  if (!resetsAt || !Number.isFinite(resetsAt) || resetsAt <= 0) return '';
  const diff = resetsAt - now;
  if (diff <= 0) return '';
  if (diff < 60_000) return '1分钟内重置';
  if (diff < 3_600_000) return `${Math.ceil(diff / 60_000)}分钟后重置`;
  if (diff < 86_400_000) {
    const hours = Math.floor(diff / 3_600_000);
    const mins = Math.floor((diff % 3_600_000) / 60_000);
    return mins > 0 ? `${hours}小时${mins}分后重置` : `${hours}小时后重置`;
  }
  const d = new Date(resetsAt);
  const mm = d.getMonth() + 1;
  const dd = d.getDate();
  const hh = String(d.getHours()).padStart(2, '0');
  const min = String(d.getMinutes()).padStart(2, '0');
  return `${mm}月${dd}日 ${hh}:${min} 重置`;
}
