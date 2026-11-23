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

  // Choose target path & host machine
  let basePath = '';
  let targetMachine = delivery.sourceMachineId;
  let targetLabel: '只读快照' | '工作目录' = '工作目录';

  if (preferSnapshot && delivery.frozenSnapshotDir) {
    basePath = delivery.frozenSnapshotDir;
    targetLabel = '只读快照';
    // Frozen snapshots live on the Hub host (local_ws machine)
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
  const lineSuffix = file && line && line > 0 ? `:${line}` : '';

  const isLocal = localMachineId !== '' && localMachineId === targetMachine;
  const proto = scheme || 'cursor';

  if (isLocal) {
    return {
      uri: `${proto}://file${fullPath.startsWith('/') ? '' : '/'}${fullPath}${lineSuffix}`,
      mode: 'local',
      targetMachine,
      targetLabel,
      resolvedPath: fullPath,
    };
  }

  const machineObj = machines.find((m) => m.machineId === targetMachine);
  const sshHost = machineObj?.sshAlias || machineObj?.tailscaleHost || targetMachine;

  if (proto === 'zed') {
    return {
      uri: `zed://ssh/${encodeURIComponent(sshHost)}${fullPath.startsWith('/') ? '' : '/'}${fullPath}${lineSuffix}`,
      mode: 'ssh',
      targetMachine,
      targetLabel,
      resolvedPath: fullPath,
    };
  }

  return {
    uri: `${proto}://vscode-remote/ssh-remote+${encodeURIComponent(sshHost)}${fullPath.startsWith('/') ? '' : '/'}${fullPath}${lineSuffix}`,
    mode: 'ssh',
    targetMachine,
    targetLabel,
    resolvedPath: fullPath,
  };
}
