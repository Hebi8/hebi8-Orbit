import { useState } from 'react';
import { Check, ChevronDown } from 'lucide-react';
import type { DeliveryView, IDEScheme, MachineView, ProjectView } from '../generated/protocol';
import {
  IDE_OPTIONS,
  buildIDEJump,
  buildProjectIDEJump,
  ideLabel,
  launchIDE,
  resolveEffectiveLocalMachineId,
} from '../lib/ide';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';
import { IdeGlyph } from './Icons';

/**
 * Compact Lody-LAN-style IDE opener:
 * - Primary click launches the preferred IDE (local direct open or SSH host alias).
 * - Label shows only IDE glyph + `ssh <alias>` (remote) or `<IDE>` (local).
 * - Dropdown lists only the IDE launchers, zero self-explaining copy.
 */
export function IdeOpenButton({
  project,
  machines,
  delivery,
  worktreePath,
  worktreeMachineId,
  file,
  line,
}: {
  project?: ProjectView;
  machines: MachineView[];
  delivery?: DeliveryView;
  worktreePath?: string;
  worktreeMachineId?: string;
  file?: string;
  line?: number;
}) {
  const [open, setOpen] = useState(false);
  const preferredIDE = useWorkbenchStore((s) => s.preferredIDE);
  const setPreferredIDE = useWorkbenchStore((s) => s.setPreferredIDE);
  const storedLocalId = useWorkbenchStore((s) => s.localMachineId);

  const scheme: IDEScheme = preferredIDE || project?.ideScheme || 'cursor';
  const effectiveLocalId = resolveEffectiveLocalMachineId(machines, storedLocalId);

  const jump = delivery
    ? buildIDEJump({
        scheme,
        localMachineId: effectiveLocalId,
        delivery,
        project,
        machines,
        file,
        line,
      })
    : project
      ? buildProjectIDEJump({
          project,
          machines,
          localMachineId: effectiveLocalId,
          scheme,
          worktreePath,
          worktreeMachineId,
        })
      : null;

  if (!jump) return null;

  const isSsh = jump.mode === 'ssh';
  const hostShort = jump.sshHost || jump.targetMachine;

  const handleLaunch = () => {
    setOpen(false);
    launchIDE(jump.uri);
  };

  const handlePick = (next: IDEScheme) => {
    setPreferredIDE(next);
    setOpen(false);
    toast(`默认编辑器已切换为 ${ideLabel(next)}`);
  };

  return (
    <div className="ide-split relpos">
      <button
        type="button"
        className="ide-main"
        onClick={handleLaunch}
        title={isSsh ? `${ideLabel(scheme)} · ssh ${hostShort}` : `${ideLabel(scheme)} · ${jump.resolvedPath}`}
      >
        <IdeGlyph scheme={scheme} size={13} />
        {isSsh ? (
          <span className="ide-host-pill">ssh {hostShort}</span>
        ) : (
          <span>{ideLabel(scheme)}</span>
        )}
      </button>
      <button
        type="button"
        className="ide-drop-btn"
        aria-label="选择 IDE"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <ChevronDown size={11} strokeWidth={1.7} />
      </button>
      {open && (
        <>
          <div
            className="scrim"
            style={{ background: 'transparent', backdropFilter: 'none', zIndex: 49 }}
            onClick={() => setOpen(false)}
          />
          <div className="ide-menu" role="menu">
            {IDE_OPTIONS.map((opt) => (
              <button
                type="button"
                role="menuitemradio"
                aria-checked={scheme === opt.id}
                key={opt.id}
                className="ide-menu-item"
                onClick={() => handlePick(opt.id)}
              >
                <IdeGlyph scheme={opt.id} size={13} />
                <span>{opt.label}</span>
                {scheme === opt.id && <Check size={12} strokeWidth={1.9} className="chk" />}
              </button>
            ))}
          </div>
        </>
      )}
    </div>
  );
}
