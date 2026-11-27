import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { Check, ChevronDown, Pencil, RotateCcw, X } from 'lucide-react';
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
import { useAnchoredPopover } from '../lib/popover';
import { useImeGuard } from '../lib/ime';
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
 * Clean, low-noise IDE opener (Orbit v4):
 * - Main button shows only `<IdeGlyph />` + `<IDE name>` (never prints `ssh <alias>` inline; full target in title tooltip).
 * - Dropdown renders via viewport-clamped portal:
 *   1. Editor picker (Cursor / VS Code / Antigravity / Zed)
 *   2. (Remote machines only) Single `SSH · <name>` row with an inline pencil icon; clicking the pencil
 *      switches that row in-place into an inline input (Save / Cancel) and collapses immediately on save/cancel.
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
  const [editingSsh, setEditingSsh] = useState(false);
  const ime = useImeGuard();
  const splitRef = useRef<HTMLDivElement | null>(null);
  const menuRef = useRef<HTMLDivElement | null>(null);

  const preferredIDE = useWorkbenchStore((s) => s.preferredIDE);
  const setPreferredIDE = useWorkbenchStore((s) => s.setPreferredIDE);
  const storedLocalId = useWorkbenchStore((s) => s.localMachineId);
  const sshAliasRev = useWorkbenchStore((s) => s.sshAliasRev);
  const bumpSshAliasRev = useWorkbenchStore((s) => s.bumpSshAliasRev);

  const closeMenu = useCallback(() => {
    setOpen(false);
    setEditingSsh(false);
  }, []);

  const scheme: IDEScheme = preferredIDE || project?.ideScheme || 'cursor';
  const effectiveLocalId = resolveEffectiveLocalMachineId(machines, storedLocalId);

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

  const customAlias = useMemo(() => {
    void sshAliasRev;
    return getCustomSshAlias(targetMachineId);
  }, [targetMachineId, sshAliasRev]);

  const [draftAlias, setDraftAlias] = useState('');
  useEffect(() => {
    if (!open) setEditingSsh(false);
  }, [open]);

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

  const isSsh = jump?.mode === 'ssh';

  const popStyle = useAnchoredPopover({
    open,
    anchorRef: splitRef,
    popRef: menuRef,
    width: isSsh ? 264 : 196,
    align: 'right',
    placement: 'down',
    gap: 6,
    onClose: closeMenu,
  });

  const candidates = sshQ.data?.candidates ?? [];
  const allHosts = sshQ.data?.allHosts ?? [];
  const fallbackHosts = useMemo(() => {
    const seen = new Set(candidates.map((c) => c.alias));
    return allHosts.filter((h) => !seen.has(h)).slice(0, 8);
  }, [candidates, allHosts]);

  if (!jump) return null;

  const hostShort = jump.sshHost || jump.targetMachine;
  const label = ideLabel(scheme);
  const tooltip = isSsh
    ? `在 ${label} 打开 · ssh ${hostShort} · ${jump.resolvedPath}`
    : `在 ${label} 打开 · ${jump.resolvedPath}`;

  const handleLaunch = () => {
    closeMenu();
    launchIDE(jump.uri);
  };

  const handlePick = (next: IDEScheme) => {
    setPreferredIDE(next);
    closeMenu();
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
    setEditingSsh(false);
    toast(`SSH 别名已设为 ${alias}`);
  };

  const handleSaveCustom = () => {
    if (!targetMachineId) return;
    const trimmed = draftAlias.trim();
    if (recommendedAlias && trimmed === recommendedAlias) {
      setCustomSshAlias(targetMachineId, '');
      bumpSshAliasRev();
      setEditingSsh(false);
      toast(`SSH 别名已设为 ${trimmed}`);
      return;
    }
    setCustomSshAlias(targetMachineId, trimmed);
    bumpSshAliasRev();
    setEditingSsh(false);
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
    setEditingSsh(false);
    toast('已恢复自动匹配 SSH 别名');
  };

  return (
    <div className="ide-split relpos" ref={splitRef}>
      <button type="button" className="ide-main" onClick={handleLaunch} title={tooltip}>
        <IdeGlyph scheme={scheme} size={13} />
        <span>{label}</span>
      </button>
      <button
        type="button"
        className="ide-drop-btn"
        aria-label="选择 IDE 或 SSH 别名"
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => (open ? closeMenu() : setOpen(true))}
      >
        <ChevronDown size={11} strokeWidth={1.7} />
      </button>
      {open &&
        popStyle &&
        createPortal(
          <div
            ref={menuRef}
            className={`ide-menu ide-menu-portal ${isSsh ? 'has-ssh' : ''}`}
            style={popStyle}
            role="menu"
          >
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
                {editingSsh ? (
                  <div className="ide-ssh-inline-edit">
                    <span className="ide-ssh-prefix">SSH ·</span>
                    <input
                      type="text"
                      autoFocus
                      className="field mono ide-ssh-input"
                      placeholder="SSH 别名"
                      value={draftAlias}
                      onChange={(e) => setDraftAlias(e.target.value)}
                      {...ime.compositionProps}
                      onKeyDown={(e) => {
                        if (ime.isImeEnter(e)) return;
                        if (e.key === 'Enter') {
                          e.preventDefault();
                          handleSaveCustom();
                        } else if (e.key === 'Escape') {
                          e.preventDefault();
                          e.stopPropagation();
                          setEditingSsh(false);
                        }
                      }}
                    />
                    <button type="button" className="btn pri sm ide-ssh-save" onClick={handleSaveCustom}>
                      保存
                    </button>
                    {customAlias && (
                      <button
                        type="button"
                        className="ide-ssh-edit-btn"
                        title="恢复自动匹配 SSH 别名"
                        aria-label="恢复自动匹配"
                        onClick={handleClearCustom}
                      >
                        <RotateCcw size={11.5} strokeWidth={1.7} />
                      </button>
                    )}
                    <button
                      type="button"
                      className="ide-ssh-edit-btn"
                      title="取消"
                      aria-label="取消"
                      onClick={() => setEditingSsh(false)}
                    >
                      <X size={12} strokeWidth={1.7} />
                    </button>
                  </div>
                ) : (
                  <div className="ide-ssh-head">
                    <div className="ide-ssh-head-main">
                      <span className="ide-ssh-prefix">SSH ·</span>
                      <span className="ide-ssh-name" title={`当前 SSH 别名：${hostShort}`}>
                        {hostShort}
                      </span>
                      <button
                        type="button"
                        className="ide-ssh-edit-btn"
                        title="修改 SSH 别名"
                        aria-label="修改 SSH 别名"
                        onClick={() => {
                          setDraftAlias(customAlias || hostShort);
                          setEditingSsh(true);
                        }}
                      >
                        <Pencil size={11.5} strokeWidth={1.7} />
                      </button>
                    </div>
                    <span className="ide-ssh-src">{sshSourceBadge(jump.sshSource)}</span>
                  </div>
                )}

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
              </>
            )}
          </div>,
          document.body,
        )}
    </div>
  );
}
