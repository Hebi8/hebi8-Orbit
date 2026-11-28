import type {
  DeliveryView,
  IDEScheme,
  MachineView,
  ProjectView,
} from '../generated/protocol';

export type SSHHostSource = 'custom' | 'ssh_config' | 'machine' | 'tailscale' | 'fallback';

export interface ResolvedSshHost {
  host: string;
  source: SSHHostSource;
}

export interface IDEJumpResult {
  uri: string;
  mode: 'local' | 'ssh';
  targetMachine: string;
  sshHost?: string;
  sshSource?: SSHHostSource;
  targetLabel: '只读快照' | '工作目录';
  resolvedPath: string;
  line?: number;
}

export const IDE_OPTIONS: { id: IDEScheme; label: string }[] = [
  { id: 'cursor', label: 'Cursor' },
  { id: 'vscode', label: 'VS Code' },
  { id: 'antigravity', label: 'Antigravity' },
  { id: 'zed', label: 'Zed' },
];

const SSH_ALIAS_STORAGE_PREFIX = 'orbit.sshAlias.';

export function ideLabel(scheme: IDEScheme): string {
  return IDE_OPTIONS.find((x) => x.id === scheme)?.label ?? scheme;
}

export function getCustomSshAlias(machineId: string): string {
  if (!machineId) return '';
  try {
    return (localStorage.getItem(`${SSH_ALIAS_STORAGE_PREFIX}${machineId}`) ?? '').trim();
  } catch {
    return '';
  }
}

export function setCustomSshAlias(machineId: string, alias: string) {
  if (!machineId) return;
  try {
    const trimmed = alias.trim();
    const key = `${SSH_ALIAS_STORAGE_PREFIX}${machineId}`;
    if (trimmed) localStorage.setItem(key, trimmed);
    else localStorage.removeItem(key);
  } catch {
    // ignore storage errors
  }
}

/**
 * Formats the VS Code / Cursor / Antigravity Remote-SSH authority segment.
 * Per VS Code Remote-SSH URI protocol, when target contains uppercase letters, colons (ports / IPv6),
 * user@ prefixes, or other non-plain-lowercase characters, hex-encode the JSON object `{"hostName": target}`
 * so VS Code's URI authority lowercasing never corrupts the SSH Host alias.
 */
export function formatVsCodeRemoteAuthority(target: string): string {
  const cleaned = target.trim();
  if (!cleaned) return 'ssh-remote+';
  const needsHex = /[A-Z:@\s]/.test(cleaned) || !/^[a-z0-9._-]+$/.test(cleaned);
  if (!needsHex) {
    return `ssh-remote+${cleaned}`;
  }
  const json = JSON.stringify({ hostName: cleaned });
  const bytes = new TextEncoder().encode(json);
  let hex = '';
  for (let i = 0; i < bytes.length; i++) {
    hex += bytes[i].toString(16).padStart(2, '0');
  }
  return `ssh-remote+${hex}`;
}

/**
 * Best-effort local machine detection: if opened on localhost/127.0.0.1, matches the hub's local_ws machine;
 * otherwise honours an explicit localMachineId override if set.
 */
export function resolveEffectiveLocalMachineId(machines: MachineView[], storedLocalId: string): string {
  if (storedLocalId) return storedLocalId;
  if (typeof window !== 'undefined') {
    const h = window.location.hostname;
    if (h === '127.0.0.1' || h === 'localhost' || h === '::1') {
      const localWs = machines.find((m) => m.transportMode === 'local_ws');
      if (localWs) return localWs.machineId;
    }
  }
  return '';
}

/**
 * Resolves the effective SSH target host and its provenance according to priority:
 * localStorage custom alias > local ~/.ssh/config recommended > machine.sshAlias > machine.tailscaleHost > machine.machineId.
 */
export function resolveEffectiveSshHost(params: {
  machine?: MachineView | null;
  fallbackId?: string;
  recommendedSshHost?: string;
  customSshHost?: string;
}): ResolvedSshHost {
  const { machine, fallbackId = '', recommendedSshHost, customSshHost } = params;
  const machineId = machine?.machineId || fallbackId;
  const custom = (customSshHost !== undefined ? customSshHost : getCustomSshAlias(machineId)).trim();
  if (custom) {
    return { host: custom, source: 'custom' };
  }
  const rec = (recommendedSshHost ?? '').trim();
  if (rec) {
    return { host: rec, source: 'ssh_config' };
  }
  if (machine?.sshAlias?.trim()) {
    return { host: machine.sshAlias.trim(), source: 'machine' };
  }
  if (machine?.tailscaleHost?.trim()) {
    return { host: machine.tailscaleHost.trim(), source: 'tailscale' };
  }
  return { host: machineId || fallbackId, source: 'fallback' };
}

export function resolveMachineSshAlias(machine?: MachineView | null, fallbackId = ''): string {
  if (!machine) return fallbackId;
  return getCustomSshAlias(machine.machineId) || machine.sshAlias || machine.machineId || machine.tailscaleHost || fallbackId;
}

/**
 * Encodes each '/' path segment independently with encodeURIComponent so characters
 * like '#', '?', '%', spaces, or CJK names never truncate IDE URIs while keeping '/'.
 */
export function encodeURIPathSegments(rawPath: string): string {
  const normalized = rawPath.startsWith('/') ? rawPath : `/${rawPath}`;
  return normalized
    .split('/')
    .map((seg, idx) => (idx === 0 ? '' : encodeURIComponent(seg)))
    .join('/');
}

function buildRemoteUri(proto: IDEScheme, sshHost: string, encodedPath: string, lineSuffix: string): string {
  if (proto === 'zed') {
    return `zed://ssh/${encodeURIComponent(sshHost)}${encodedPath}${lineSuffix}`;
  }
  return `${proto}://vscode-remote/${formatVsCodeRemoteAuthority(sshHost)}${encodedPath}${lineSuffix}`;
}

/**
 * Computes a local or SSH-remote IDE URI for opening a delivery's frozen snapshot
 * or worktree path (optionally anchored at a file and line number).
 */
export function buildIDEJump(params: {
  scheme: IDEScheme;
  localMachineId: string;
  delivery: DeliveryView;
  project?: ProjectView;
  machines: MachineView[];
  file?: string;
  line?: number;
  preferSnapshot?: boolean;
  recommendedSshHost?: string;
  customSshHost?: string;
}): IDEJumpResult | null {
  const {
    scheme,
    localMachineId,
    delivery,
    project,
    machines,
    file,
    line,
    preferSnapshot,
    recommendedSshHost,
    customSshHost,
  } = params;

  let basePath = '';
  let targetMachine = delivery.sourceMachineId;
  let targetLabel: '只读快照' | '工作目录' = '工作目录';

  if (preferSnapshot && delivery.frozenSnapshotDir) {
    basePath = delivery.frozenSnapshotDir;
    targetLabel = '只读快照';
    const hubMachine = machines.find((m) => m.transportMode === 'local_ws');
    if (hubMachine) {
      targetMachine = hubMachine.machineId;
    }
  } else if (delivery.manifest.worktreePath) {
    basePath = delivery.manifest.worktreePath;
    targetLabel = '工作目录';
  } else if (delivery.frozenSnapshotDir) {
    basePath = delivery.frozenSnapshotDir;
    targetLabel = '只读快照';
    const hubMachine = machines.find((m) => m.transportMode === 'local_ws');
    if (hubMachine) {
      targetMachine = hubMachine.machineId;
    }
  } else if (project) {
    const anchor =
      project.anchors[project.defaultCapability] ?? Object.values(project.anchors)[0];
    if (anchor?.repoPath) {
      basePath = anchor.repoPath;
      targetMachine = anchor.machine;
      targetLabel = '工作目录';
    }
  }

  if (!basePath) {
    return null;
  }

  const normalizedBase = basePath.replace(/\/+$/, '');
  const fullPath = file ? `${normalizedBase}/${file.replace(/^\/+/, '')}` : normalizedBase;
  const encodedPath = encodeURIPathSegments(fullPath);
  const lineSuffix = file && line && line > 0 ? `:${line}` : '';

  const isLocal = localMachineId !== '' && localMachineId === targetMachine;
  const proto = scheme || 'cursor';

  if (isLocal) {
    return {
      uri: `${proto}://file${encodedPath}${lineSuffix}`,
      mode: 'local',
      targetMachine,
      targetLabel,
      resolvedPath: fullPath,
      line: file && line && line > 0 ? line : undefined,
    };
  }

  const machineObj = machines.find((m) => m.machineId === targetMachine);
  const resolved = resolveEffectiveSshHost({
    machine: machineObj,
    fallbackId: targetMachine,
    recommendedSshHost,
    customSshHost,
  });

  return {
    uri: buildRemoteUri(proto, resolved.host, encodedPath, lineSuffix),
    mode: 'ssh',
    targetMachine,
    sshHost: resolved.host,
    sshSource: resolved.source,
    targetLabel,
    resolvedPath: fullPath,
    line: file && line && line > 0 ? line : undefined,
  };
}

/** IDE jump to a project's main checkout (default-capability anchor), used where no delivery exists yet. */
export function buildProjectIDEJump(params: {
  project: ProjectView;
  machines: MachineView[];
  localMachineId: string;
  scheme?: IDEScheme;
  worktreePath?: string;
  worktreeMachineId?: string;
  recommendedSshHost?: string;
  customSshHost?: string;
}): IDEJumpResult | null {
  const {
    project,
    machines,
    localMachineId,
    scheme,
    worktreePath,
    worktreeMachineId,
    recommendedSshHost,
    customSshHost,
  } = params;
  let basePath = '';
  let targetMachine = '';
  if (worktreePath && worktreeMachineId) {
    basePath = worktreePath;
    targetMachine = worktreeMachineId;
  } else {
    const anchor = project.anchors[project.defaultCapability] ?? Object.values(project.anchors).find((a) => a.repoPath);
    if (!anchor?.repoPath) return null;
    basePath = anchor.repoPath;
    targetMachine = anchor.machine;
  }
  const proto = scheme || project.ideScheme || 'cursor';
  const fullPath = basePath.replace(/\/+$/, '');
  const encodedPath = encodeURIPathSegments(fullPath);
  const effectiveLocal = resolveEffectiveLocalMachineId(machines, localMachineId);
  if (effectiveLocal && effectiveLocal === targetMachine) {
    return { uri: `${proto}://file${encodedPath}`, mode: 'local', targetMachine, targetLabel: '工作目录', resolvedPath: fullPath };
  }
  const machineObj = machines.find((m) => m.machineId === targetMachine);
  const resolved = resolveEffectiveSshHost({
    machine: machineObj,
    fallbackId: targetMachine,
    recommendedSshHost,
    customSshHost,
  });
  return {
    uri: buildRemoteUri(proto, resolved.host, encodedPath, ''),
    mode: 'ssh',
    targetMachine,
    sshHost: resolved.host,
    sshSource: resolved.source,
    targetLabel: '工作目录',
    resolvedPath: fullPath,
  };
}

/**
 * Computes a local or SSH-remote IDE URI for opening a single file (optionally anchored at a line/column),
 * reusing the exact same local-machine detection and SSH alias precedence as `IdeOpenButton`.
 * Returns `null` for empty, relative, or `~/` home-relative paths that cannot be launched via IDE URI directly.
 */
export function buildFileIDEJump(params: {
  scheme?: IDEScheme;
  path: string;
  line?: number;
  column?: number;
  machineId?: string;
  localMachineId?: string;
  machines?: MachineView[];
  recommendedSshHost?: string;
  customSshHost?: string;
}): IDEJumpResult | null {
  const {
    scheme,
    path,
    line,
    column,
    machineId,
    localMachineId = '',
    machines = [],
    recommendedSshHost,
    customSshHost,
  } = params;

  const trimmed = (path ?? '').trim();
  if (!trimmed || !trimmed.startsWith('/')) {
    return null;
  }

  const fullPath = trimmed.length > 1 ? trimmed.replace(/\/+$/, '') : trimmed;
  const encodedPath = encodeURIPathSegments(fullPath);
  const validLine = typeof line === 'number' && Number.isInteger(line) && line > 0 ? line : undefined;
  const validCol =
    validLine && typeof column === 'number' && Number.isInteger(column) && column > 0 ? column : undefined;
  const lineSuffix = validLine ? (validCol ? `:${validLine}:${validCol}` : `:${validLine}`) : '';

  const targetMachine = (machineId ?? '').trim();
  const effectiveLocal = resolveEffectiveLocalMachineId(machines, localMachineId);
  const proto: IDEScheme = scheme || 'cursor';
  const isLocal = !targetMachine || (effectiveLocal !== '' && effectiveLocal === targetMachine);

  if (isLocal) {
    return {
      uri: `${proto}://file${encodedPath}${lineSuffix}`,
      mode: 'local',
      targetMachine,
      targetLabel: '工作目录',
      resolvedPath: fullPath,
      line: validLine,
    };
  }

  const machineObj = machines.find((m) => m.machineId === targetMachine);
  const resolved = resolveEffectiveSshHost({
    machine: machineObj,
    fallbackId: targetMachine,
    recommendedSshHost,
    customSshHost,
  });

  return {
    uri: buildRemoteUri(proto, resolved.host, encodedPath, lineSuffix),
    mode: 'ssh',
    targetMachine,
    sshHost: resolved.host,
    sshSource: resolved.source,
    targetLabel: '工作目录',
    resolvedPath: fullPath,
    line: validLine,
  };
}

/** Resolves the effective execution machine, working directory, and project IDE scheme for a session or turn. */
export function resolveSessionFileContext(params: {
  task?: { requiredCapability?: string; latestDeliveryId?: string };
  attempt?: { machineId?: string; worktreePath?: string; partId?: string; locationId?: string; outputDeliveryId?: string };
  deliveries?: DeliveryView[];
  project?: ProjectView;
  machines?: MachineView[];
  runtimeConfig?: Record<string, string>;
  activeDutyMachineId?: string;
}): { machineId?: string; worktreePath?: string; ideScheme?: IDEScheme } {
  const { task, attempt, deliveries = [], project, machines = [], runtimeConfig, activeDutyMachineId } = params;

  const matchedDelivery =
    (attempt?.outputDeliveryId
      ? deliveries.find((d) => d.deliveryId === attempt.outputDeliveryId)
      : undefined) ??
    (task?.latestDeliveryId ? deliveries.find((d) => d.deliveryId === task.latestDeliveryId) : undefined) ??
    [...deliveries].reverse().find((d) => !!d.manifest?.worktreePath) ??
    deliveries[deliveries.length - 1];

  const matchedPart = attempt?.partId
    ? project?.parts?.find((p) => p.partId === attempt.partId)
    : project?.parts?.[0];
  const matchedLoc = attempt?.locationId
    ? matchedPart?.locations?.find((l) => l.locationId === attempt.locationId)
    : undefined;
  const defaultLoc = matchedPart?.locations?.find((l) => l.isDefault) ?? matchedPart?.locations?.[0];

  const anchor = project
    ? project.anchors[task?.requiredCapability || project.defaultCapability] ??
      Object.values(project.anchors).find((a) => a.repoPath || a.machine)
    : undefined;

  const runtimeMachine = runtimeConfig?.machineId?.trim() || runtimeConfig?.preferredMachine?.trim() || undefined;
  const dutyMachine =
    activeDutyMachineId && machines.some((m) => m.machineId === activeDutyMachineId)
      ? activeDutyMachineId
      : undefined;

  const machineId =
    attempt?.machineId ||
    runtimeMachine ||
    matchedDelivery?.sourceMachineId ||
    matchedLoc?.machineId ||
    defaultLoc?.machineId ||
    anchor?.machine ||
    dutyMachine ||
    undefined;

  const machineLoc = machineId ? matchedPart?.locations?.find((l) => l.machineId === machineId) : undefined;
  const deliveryWorktree =
    matchedDelivery?.manifest?.worktreePath &&
    (!machineId || !matchedDelivery.sourceMachineId || matchedDelivery.sourceMachineId === machineId)
      ? matchedDelivery.manifest.worktreePath
      : '';
  const matchedLocPath =
    matchedLoc?.absPath && (!machineId || !matchedLoc.machineId || matchedLoc.machineId === machineId)
      ? matchedLoc.absPath
      : '';
  const defaultLocPath =
    defaultLoc?.absPath && (!machineId || !defaultLoc.machineId || defaultLoc.machineId === machineId)
      ? defaultLoc.absPath
      : '';
  const anchorRepoPath =
    anchor?.repoPath && (!machineId || !anchor.machine || anchor.machine === machineId) ? anchor.repoPath : '';
  const rawWorktree =
    attempt?.worktreePath || deliveryWorktree || matchedLocPath || machineLoc?.absPath || defaultLocPath || anchorRepoPath || '';

  const trimmedDir = rawWorktree.trim();
  const worktreePath = trimmedDir.startsWith('/') || trimmedDir.startsWith('~/') ? trimmedDir : undefined;

  return {
    machineId,
    worktreePath,
    ideScheme: project?.ideScheme,
  };
}

/** Hands a custom-scheme IDE URI to the OS without leaving the SPA. */
export function launchIDE(uri: string) {
  const a = document.createElement('a');
  a.href = uri;
  a.rel = 'noopener noreferrer';
  a.style.display = 'none';
  document.body.appendChild(a);
  a.click();
  a.remove();
}
