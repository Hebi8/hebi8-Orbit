import { useEffect, useMemo, useState } from 'react';
import { Check, ChevronDown, RotateCcw } from 'lucide-react';
import type { DeliveryView, IDEScheme, MachineView, ProjectView } from '../generated/protocol';
import { useMachineSSHCandidates } from '../hooks/queries';
import {
  IDE_OPTIONS,
  buildIDEJump,
  buildProjectIDEJump,
  getCustomSshAlias,
  ideLabel,
  launchIDE,
  resolveEffectiveLocalMachineId,
  setCustomSshAlias,
  type SSHHostSource,
} from '../lib/ide';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';
import { IdeGlyph } from './Icons';

function sshSourceBadge(source?: SSHHostSource): string {
  switch (source) {
    case 'custom':
      return '自定义';
    case 'ssh_config':
      return '自动识别';
    case 'machine':
      return '机器别名';
    case 'tailscale':
      return 'Tailscale';
    default:
      return '默认';
  }
}

/**
 * Clean, low-noise IDE opener:
 * - Main button ALWAYS shows only `<IdeGlyph />` + `在 <IDE> 打开` (full SSH target & path in title tooltip).
 * - Dropdown splits into:
 *   1. Editor picker (Cursor / VS Code / Antigravity / Zed)
 *   2. (Remote machines only) Active SSH alias & provenance, clickable ~/.ssh/config candidates,
 *      and inline custom SSH alias editor saved to localStorage (`orbit.sshAlias.<machineId>`).
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
  const sshAliasRev = useWorkbenchStore((s) => s.sshAliasRev);
  const bumpSshAliasRev = useWorkbenchStore((s) => s.bumpSshAliasRev);

  const scheme: IDEScheme = preferredIDE || project?.ideScheme || 'cursor';
  const effectiveLocalId = resolveEffectiveLocalMachineId(machines, storedLocalId);

  // Determine target machine upfront so we can query local ~/.ssh/config candidates for remote machines.
  const baseJump = useMemo(
    () =>
      delivery
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
          : null,
    [delivery, scheme, effectiveLocalId, project, machines, file, line, worktreePath, worktreeMachineId],
  );

  const targetMachineId = baseJump?.targetMachine ?? '';
  const targetMachine = useMemo(
    () => machines.find((m) => m.machineId === targetMachineId) ?? null,
    [machines, targetMachineId],
  );
  const isRemote = baseJump?.mode === 'ssh';

  const sshQ = useMachineSSHCandidates(targetMachine, isRemote && !!targetMachine);
  const recommendedAlias = sshQ.data?.recommended ?? '';

  // Re-read localStorage custom alias whenever sshAliasRev or targetMachineId changes.
  const customAlias = useMemo(() => {
    void sshAliasRev;
    return getCustomSshAlias(targetMachineId);
  }, [targetMachineId, sshAliasRev]);

  const [draftAlias, setDraftAlias] = useState('');
  useEffect(() => {
    if (open) {
      setDraftAlias(customAlias);
    }
  }, [open, customAlias]);

  const jump = useMemo(
    () =>
      delivery
        ? buildIDEJump({
            scheme,
            localMachineId: effectiveLocalId,
            delivery,
            project,
            machines,
            file,
            line,
            recommendedSshHost: recommendedAlias,
            customSshHost: customAlias,
          })
        : project
          ? buildProjectIDEJump({
              project,
              machines,
              localMachineId: effectiveLocalId,
              scheme,
              worktreePath,
              worktreeMachineId,
              recommendedSshHost: recommendedAlias,
              customSshHost: customAlias,
            })
          : null,
    [
      delivery,
      scheme,
      effectiveLocalId,
      project,
      machines,
      file,
      line,
      worktreePath,
      worktreeMachineId,
      recommendedAlias,
      customAlias,
    ],
  );

  const candidates = sshQ.data?.candidates ?? [];
  const allHosts = sshQ.data?.allHosts ?? [];
  const fallbackHosts = useMemo(() => {
    const seen = new Set(candidates.map((c) => c.alias));
    return allHosts.filter((h) => !seen.has(h)).slice(0, 8);
  }, [candidates, allHosts]);

  if (!jump) return null;

  const isSsh = jump.mode === 'ssh';
  const hostShort = jump.sshHost || jump.targetMachine;
  const label = ideLabel(scheme);
  const tooltip = isSsh
    ? `在 ${label} 打开 · ssh ${hostShort} · ${jump.resolvedPath}`
    : `在 ${label} 打开 · ${jump.resolvedPath}`;

  const handleLaunch = () => {
    setOpen(false);
    launchIDE(jump.uri);
  };

  const handlePick = (next: IDEScheme) => {
    setPreferredIDE(next);
    setOpen(false);
    toast(`默认编辑器已切换为 ${ideLabel(next)}`);
  };

  const handleSelectCandidate = (alias: string) => {
    if (!targetMachineId) return;
    if (recommendedAlias && alias === recommendedAlias) {
      setCustomSshAlias(targetMachineId, '');
    } else {
      setCustomSshAlias(targetMachineId, alias);
    }
    bumpSshAliasRev();
    setDraftAlias(alias === recommendedAlias ? '' : alias);
    toast(`SSH 别名已设为 ${alias}`);
  };

  const handleSaveCustom = () => {
    if (!targetMachineId) return;
    const trimmed = draftAlias.trim();
    setCustomSshAlias(targetMachineId, trimmed);
    bumpSshAliasRev();
    if (trimmed) {
      toast(`已保存自定义 SSH 别名：${trimmed}`);
    } else {
      toast('已恢复自动匹配 SSH 别名');
    }
  };

  const handleClearCustom = () => {
    if (!targetMachineId) return;
    setCustomSshAlias(targetMachineId, '');
    setDraftAlias('');
    bumpSshAliasRev();
    toast('已恢复自动匹配 SSH 别名');
  };

  return (
    <div className="ide-split relpos">
      <button type="button" className="ide-main" onClick={handleLaunch} title={tooltip}>
        <IdeGlyph scheme={scheme} size={13} />
        <span>在 {label} 打开</span>
      </button>
      <button
        type="button"
        className="ide-drop-btn"
        aria-label="选择 IDE 或 SSH 别名"
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
          <div className={`ide-menu ${isSsh ? 'has-ssh' : ''}`} role="menu">
            <div className="ide-menu-sec">编辑器</div>
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

            {isSsh && (
              <>
                <div className="ide-menu-div" />
                <div className="ide-ssh-head">
                  <span className="mono ell" title={hostShort}>
                    SSH: {hostShort}
                  </span>
                  <span className="ide-ssh-src">{sshSourceBadge(jump.sshSource)}</span>
                </div>

                {candidates.length > 0 && (
                  <div className="ide-ssh-list">
                    {candidates.slice(0, 6).map((c) => {
                      const active = hostShort === c.alias;
                      return (
                        <button
                          type="button"
                          key={c.alias}
                          className={`ide-menu-item ide-ssh-cand ${active ? 'active' : ''}`}
                          onClick={() => handleSelectCandidate(c.alias)}
                          title={c.hostName ? `${c.alias} → ${c.hostName}` : c.alias}
                        >
                          <span className="mono ell">{c.alias}</span>
                          {c.hostName && c.hostName !== c.alias && (
                            <span className="ide-ssh-sub mono ell">{c.hostName}</span>
                          )}
                          {active && <Check size={11.5} strokeWidth={1.9} className="chk" />}
                        </button>
                      );
                    })}
                  </div>
                )}

                {candidates.length === 0 && fallbackHosts.length > 0 && (
                  <div className="ide-ssh-list">
                    <div className="ide-menu-sec" style={{ paddingTop: 2 }}>
                      本机 ~/.ssh/config
                    </div>
                    {fallbackHosts.map((alias) => {
                      const active = hostShort === alias;
                      return (
                        <button
                          type="button"
                          key={alias}
                          className={`ide-menu-item ide-ssh-cand ${active ? 'active' : ''}`}
                          onClick={() => handleSelectCandidate(alias)}
                        >
                          <span className="mono ell">{alias}</span>
                          {active && <Check size={11.5} strokeWidth={1.9} className="chk" />}
                        </button>
                      );
                    })}
                  </div>
                )}

                <div className="ide-ssh-custom">
                  <input
                    type="text"
                    className="field mono ide-ssh-input"
                    placeholder="自定义 SSH 别名…"
                    value={draftAlias}
                    onChange={(e) => setDraftAlias(e.target.value)}
                    onKeyDown={(e) => {
                      if (e.key === 'Enter') {
                        e.preventDefault();
                        handleSaveCustom();
                      }
                    }}
                  />
                  <button
                    type="button"
                    className="btn ghost sm"
                    disabled={draftAlias.trim() === customAlias}
                    onClick={handleSaveCustom}
                  >
                    保存
                  </button>
                  {customAlias && (
                    <button
                      type="button"
                      className="ib"
                      title="清空自定义并恢复自动匹配"
                      aria-label="恢复自动匹配"
                      onClick={handleClearCustom}
                    >
                      <RotateCcw size={12} strokeWidth={1.7} />
                    </button>
                  )}
                </div>
              </>
            )}
          </div>
        </>
      )}
    </div>
  );
}
