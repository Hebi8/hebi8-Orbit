import type {
  DeliveryView,
  IDEScheme,
  MachineView,
  ProjectView,
} from '../generated/protocol';

export interface IDEJumpResult {
  uri: string;
  mode: 'local' | 'ssh';
  targetMachine: string;
  sshHost?: string;
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

export function ideLabel(scheme: IDEScheme): string {
  return IDE_OPTIONS.find((x) => x.id === scheme)?.label ?? scheme;
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

export function resolveMachineSshAlias(machine?: MachineView | null, fallbackId = ''): string {
  if (!machine) return fallbackId;
  return machine.sshAlias || machine.machineId || machine.tailscaleHost || fallbackId;
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
}): IDEJumpResult | null {
  const { scheme, localMachineId, delivery, project, machines, file, line, preferSnapshot } = params;

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
  const sshHost = machineObj?.sshAlias || machineObj?.tailscaleHost || targetMachine;

  if (proto === 'zed') {
    return {
      uri: `zed://ssh/${encodeURIComponent(sshHost)}${encodedPath}${lineSuffix}`,
      mode: 'ssh',
      targetMachine,
      sshHost,
      targetLabel,
      resolvedPath: fullPath,
      line: file && line && line > 0 ? line : undefined,
    };
  }

  return {
    uri: `${proto}://vscode-remote/ssh-remote+${encodeURIComponent(sshHost)}${encodedPath}${lineSuffix}`,
    mode: 'ssh',
    targetMachine,
    sshHost,
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
}): IDEJumpResult | null {
  const { project, machines, localMachineId, scheme, worktreePath, worktreeMachineId } = params;
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
  const sshHost = machineObj?.sshAlias || machineObj?.tailscaleHost || targetMachine;
  const uri =
    proto === 'zed'
      ? `zed://ssh/${encodeURIComponent(sshHost)}${encodedPath}`
      : `${proto}://vscode-remote/ssh-remote+${encodeURIComponent(sshHost)}${encodedPath}`;
  return { uri, mode: 'ssh', targetMachine, sshHost, targetLabel: '工作目录', resolvedPath: fullPath };
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
