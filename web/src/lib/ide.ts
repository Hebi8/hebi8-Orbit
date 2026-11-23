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
  targetLabel: '只读快照' | '工作目录';
  resolvedPath: string;
  line?: number;
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
      targetLabel,
      resolvedPath: fullPath,
      line: file && line && line > 0 ? line : undefined,
    };
  }

  return {
    uri: `${proto}://vscode-remote/ssh-remote+${encodeURIComponent(sshHost)}${encodedPath}${lineSuffix}`,
    mode: 'ssh',
    targetMachine,
    targetLabel,
    resolvedPath: fullPath,
    line: file && line && line > 0 ? line : undefined,
  };
}
