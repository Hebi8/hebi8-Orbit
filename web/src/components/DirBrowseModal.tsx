import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { ArrowUp, Check, ChevronRight, Folder, GitBranch, Pencil, X } from 'lucide-react';
import type { DirEntryView, MachineView, ProjectPartKind } from '../generated/protocol';
import { ApiError, api, errMsg } from '../lib/api';
import { cleanMachineName } from '../lib/format';
import { isImeKeyEvent, useImeGuard } from '../lib/ime';
import { DeviceRoleIcon } from './DeviceRoleIcon';

export interface CodexDirSelection {
  machineId: string;
  absPath: string;
  isGit: boolean;
  branch?: string;
  entryName?: string;
}

interface CrumbItem {
  label: string;
  path: string;
}

function formatBrowseError(err: unknown): string {
  if (err instanceof ApiError) {
    switch (err.code) {
      case 'ERR_PATH_OUTSIDE_ROOT':
        return '路径超出该设备允许访问的根目录范围';
      case 'ERR_DIR_NOT_FOUND':
        return '目录不存在，请检查路径是否正确';
      case 'ERR_NOT_A_DIRECTORY':
        return '该路径不是目录，请选择文件夹';
      case 'ERR_PERMISSION_DENIED':
        return '无权访问该目录';
      case 'ERR_MACHINE_OFFLINE':
        return '目标设备当前离线，无法浏览目录';
      default:
        break;
    }
  }
  return errMsg(err);
}

function formatDisplayPath(fullPath: string, homePath: string): string {
  if (!fullPath) return '~';
  if (homePath) {
    const cleanHome = homePath.replace(/\/+$/, '');
    if (fullPath === cleanHome) return '~';
    if (fullPath.startsWith(`${cleanHome}/`)) {
      return `~${fullPath.slice(cleanHome.length)}`;
    }
  }
  return fullPath;
}

function buildBreadcrumbs(curPath: string, homePath: string): CrumbItem[] {
  if (!curPath) return [{ label: '~', path: '~' }];
  const cleanCur = curPath.length > 1 ? curPath.replace(/\/+$/, '') : curPath;
  const cleanHome = homePath ? homePath.replace(/\/+$/, '') : '';

  if (cleanHome && (cleanCur === cleanHome || cleanCur.startsWith(`${cleanHome}/`))) {
    const out: CrumbItem[] = [{ label: '~', path: cleanHome }];
    const rel = cleanCur.slice(cleanHome.length).replace(/^\/+/, '');
    if (rel) {
      let acc = cleanHome;
      for (const seg of rel.split('/').filter(Boolean)) {
        acc = `${acc}/${seg}`;
        out.push({ label: seg, path: acc });
      }
    }
    return out;
  }

  if (cleanCur === '/') return [{ label: '/', path: '/' }];
  if (cleanCur.startsWith('/')) {
    const out: CrumbItem[] = [{ label: '/', path: '/' }];
    let acc = '';
    for (const seg of cleanCur.slice(1).split('/').filter(Boolean)) {
      acc = `${acc}/${seg}`;
      out.push({ label: seg, path: acc });
    }
    return out;
  }
  return [{ label: cleanCur, path: cleanCur }];
}

/**
 * Shared Codex-style cross-machine directory browser:
 * - Defaults to `~` (home directory) on normal machines and seamlessly lands on allowed root for restricted workers.
 * - Single-row clickable breadcrumb bar that switches in-place to an editable path input with structured error feedback.
 * - Single source of truth (`curPath` + `focusIdx`) ensuring 100% consistency between mouse single-click, double-click,
 *   `›` enter button, and keyboard navigation (`↑`/`↓`/`Enter`/`Backspace`).
 * - Never intercepts `Enter` when focus is on Confirm / Cancel / other interactive buttons.
 */
export function CodexDirBrowser({
  machines,
  machineId,
  onMachineChange,
  initialPath,
  activeDutyMachineId,
  currentMachineId,
  confirmLabel,
  cancelLabel,
  submitting = false,
  keyboardActive = true,
  onSelectionChange,
  onConfirm,
  onCancel,
}: {
  machines: MachineView[];
  machineId: string;
  onMachineChange: (mid: string) => void;
  initialPath?: string;
  activeDutyMachineId?: string;
  currentMachineId?: string;
  confirmLabel: string;
  cancelLabel?: string;
  submitting?: boolean;
  keyboardActive?: boolean;
  onSelectionChange?: (sel: CodexDirSelection | null) => void;
  onConfirm: (sel: CodexDirSelection) => Promise<void> | void;
  onCancel?: () => void;
}) {
  const eligible = useMemo(
    () => machines.filter((m) => m.transportMode !== 'ssh_outbound' && m.status === 'online'),
    [machines],
  );

  const [curPath, setCurPath] = useState('');
  const [homePath, setHomePath] = useState('');
  const [parentPath, setParentPath] = useState('');
  const [curIsGit, setCurIsGit] = useState(false);
  const [curBranch, setCurBranch] = useState<string | undefined>(undefined);
  const [entries, setEntries] = useState<DirEntryView[]>([]);
  const [focusIdx, setFocusIdx] = useState(-1);
  const [editingPath, setEditingPath] = useState(false);
  const [pathDraft, setPathDraft] = useState('');
  const [loadedMachineId, setLoadedMachineId] = useState('');
  const [loading, setLoading] = useState(false);
  const [validating, setValidating] = useState(false);
  const [error, setError] = useState('');

  const reqSeqRef = useRef(0);
  const validateSeqRef = useRef(0);
  const activeMachineRef = useRef(machineId);
  activeMachineRef.current = machineId;
  const initialMachineRef = useRef(machineId);
  const listRef = useRef<HTMLDivElement | null>(null);
  const pathInputRef = useRef<HTMLInputElement | null>(null);
  const lastClickRef = useRef<{ path: string; at: number }>({ path: '', at: 0 });
  const lastEnteredRef = useRef<{ path: string; at: number }>({ path: '', at: 0 });
  const appliedInitRef = useRef<string | null>(null);
  const ime = useImeGuard();

  const loadDir = useCallback(
    async (
      mid: string,
      targetPath?: string,
      opts?: { knownIsGit?: boolean; knownBranch?: string; fromManualEdit?: boolean; fallbackToHome?: boolean },
    ): Promise<boolean> => {
      if (!mid) return false;
      const seq = ++reqSeqRef.current;
      validateSeqRef.current += 1;
      setValidating(false);
      setLoading(true);
      setError('');
      try {
        const res = await api.listMachineDirs(mid, targetPath);
        if (seq !== reqSeqRef.current || activeMachineRef.current !== mid) return false;
        setCurPath(res.path);
        setHomePath(res.homePath || '');
        setParentPath(res.parentPath ?? '');
        setCurIsGit(Boolean(opts?.knownIsGit));
        setCurBranch(opts?.knownBranch);
        setEntries(res.entries ?? []);
        setFocusIdx(-1);
        setEditingPath(false);
        setPathDraft(formatDisplayPath(res.path, res.homePath || ''));
        setLoadedMachineId(mid);
        lastClickRef.current = { path: '', at: 0 };
        return true;
      } catch (e) {
        if (seq !== reqSeqRef.current || activeMachineRef.current !== mid) return false;
        const msg = formatBrowseError(e);
        if (opts?.fallbackToHome && targetPath) {
          // If an initialPath no longer exists, load default home/root while keeping the error visible.
          try {
            const fallback = await api.listMachineDirs(mid);
            if (seq !== reqSeqRef.current || activeMachineRef.current !== mid) return false;
            setCurPath(fallback.path);
            setHomePath(fallback.homePath || '');
            setParentPath(fallback.parentPath ?? '');
            setCurIsGit(false);
            setCurBranch(undefined);
            setEntries(fallback.entries ?? []);
            setFocusIdx(-1);
            setPathDraft(formatDisplayPath(fallback.path, fallback.homePath || ''));
            setLoadedMachineId(mid);
            setError(msg);
            return false;
          } catch {
            // fall through to primary error
          }
        }
        setError(msg);
        if (opts?.fromManualEdit) {
          setEditingPath(true);
        }
        return false;
      } finally {
        if (seq === reqSeqRef.current && activeMachineRef.current === mid) {
          setLoading(false);
        }
      }
    },
    [],
  );

  // Load default home (~ / allowed root) or initialPath when machine changes
  useEffect(() => {
    if (!machineId) return;
    reqSeqRef.current += 1;
    validateSeqRef.current += 1;
    setValidating(false);
    setCurPath('');
    setHomePath('');
    setParentPath('');
    setCurIsGit(false);
    setCurBranch(undefined);
    setEntries([]);
    setFocusIdx(-1);
    setEditingPath(false);
    setLoadedMachineId('');
    setError('');
    const initKey = `${machineId}::${initialPath ?? ''}`;
    const shouldUseInitial =
      !!initialPath &&
      machineId === initialMachineRef.current &&
      appliedInitRef.current !== initKey;
    if (shouldUseInitial) {
      appliedInitRef.current = initKey;
      void loadDir(machineId, initialPath, { fallbackToHome: true });
    } else {
      void loadDir(machineId);
    }
  }, [machineId, initialPath, loadDir]);

  // Focus & select path input when entering edit mode
  useEffect(() => {
    if (editingPath) {
      requestAnimationFrame(() => {
        pathInputRef.current?.focus();
        pathInputRef.current?.select();
      });
    }
  }, [editingPath]);

  // Keep keyboard-focused row scrolled into view (and sync focus when navigating within the list)
  useEffect(() => {
    if (focusIdx < 0) return;
    const row = listRef.current?.querySelector<HTMLElement>(`[data-cdx-idx="${focusIdx}"]`);
    if (!row) return;
    row.scrollIntoView({ block: 'nearest' });
    if (document.activeElement instanceof HTMLElement && listRef.current?.contains(document.activeElement)) {
      row.querySelector<HTMLButtonElement>('.cdx-row-main')?.focus({ preventScroll: true });
    }
  }, [focusIdx]);

  const focusedEntry = focusIdx >= 0 && focusIdx < entries.length ? entries[focusIdx] : null;
  const effectivePath = (focusedEntry ? focusedEntry.path : curPath).trim();
  const effectiveIsGit = focusedEntry ? focusedEntry.isGitRepo : curIsGit;
  const effectiveBranch = focusedEntry ? focusedEntry.branch : curBranch;
  const effectiveEntryName = focusedEntry
    ? focusedEntry.name
    : effectivePath.split('/').filter(Boolean).pop() || '';

  // Notify parent whenever canonical selection changes
  useEffect(() => {
    if (!onSelectionChange) return;
    if (!machineId || loadedMachineId !== machineId || !effectivePath) {
      onSelectionChange(null);
      return;
    }
    onSelectionChange({
      machineId,
      absPath: effectivePath,
      isGit: effectiveIsGit,
      branch: effectiveBranch,
      entryName: effectiveEntryName,
    });
  }, [
    onSelectionChange,
    machineId,
    loadedMachineId,
    effectivePath,
    effectiveIsGit,
    effectiveBranch,
    effectiveEntryName,
  ]);

  const enterDirectory = useCallback(
    async (targetPath: string, knownIsGit = false, knownBranch?: string) => {
      if (!machineId || !targetPath) return;
      const now = Date.now();
      if (lastEnteredRef.current.path === targetPath && now - lastEnteredRef.current.at < 450) {
        return;
      }
      lastEnteredRef.current = { path: targetPath, at: now };
      await loadDir(machineId, targetPath, { knownIsGit, knownBranch });
    },
    [machineId, loadDir],
  );

  const startEditPath = () => {
    if (!machineId || loading) return;
    setEditingPath(true);
    setPathDraft(formatDisplayPath(curPath, homePath));
    setError('');
  };

  const applyTypedPath = useCallback(async () => {
    if (!machineId) return;
    const raw = pathDraft.trim();
    if (!raw) {
      setEditingPath(false);
      setError('');
      return;
    }
    await loadDir(machineId, raw, { fromManualEdit: true });
  }, [machineId, pathDraft, loadDir]);

  const handleConfirm = useCallback(async () => {
    if (!machineId || submitting || validating) return;
    const targetMid = machineId;
    setError('');

    // If the user is currently editing the path bar with unapplied text, validate it against the backend first.
    if (editingPath) {
      const raw = pathDraft.trim();
      if (!raw) {
        setError('请输入有效的目录路径');
        return;
      }
      const seq = ++reqSeqRef.current;
      const vSeq = ++validateSeqRef.current;
      setValidating(true);
      try {
        const res = await api.listMachineDirs(targetMid, raw);
        // Discard stale response if user switched machines or triggered a newer directory request while validating.
        if (seq !== reqSeqRef.current || vSeq !== validateSeqRef.current || activeMachineRef.current !== targetMid) return;
        setCurPath(res.path);
        setHomePath(res.homePath || '');
        setParentPath(res.parentPath ?? '');
        setEntries(res.entries ?? []);
        setFocusIdx(-1);
        setEditingPath(false);
        setLoadedMachineId(targetMid);
        await onConfirm({
          machineId: targetMid,
          absPath: res.path,
          isGit: false,
          entryName: res.path.split('/').filter(Boolean).pop() || '',
        });
      } catch (e) {
        if (seq === reqSeqRef.current && vSeq === validateSeqRef.current && activeMachineRef.current === targetMid) {
          setError(formatBrowseError(e));
        }
      } finally {
        if (vSeq === validateSeqRef.current) {
          setValidating(false);
        }
      }
      return;
    }

    if (loadedMachineId !== targetMid || !effectivePath) return;
    try {
      await onConfirm({
        machineId: targetMid,
        absPath: effectivePath,
        isGit: effectiveIsGit,
        branch: effectiveBranch,
        entryName: effectiveEntryName,
      });
    } catch (e) {
      if (activeMachineRef.current === targetMid) {
        setError(formatBrowseError(e));
      }
    }
  }, [
    machineId,
    submitting,
    validating,
    editingPath,
    pathDraft,
    loadedMachineId,
    effectivePath,
    effectiveIsGit,
    effectiveBranch,
    effectiveEntryName,
    onConfirm,
  ]);

  // Keep latest values in refs for the keyboard listener so we never stale-close or re-bind on every keystroke.
  const entriesRef = useRef(entries);
  entriesRef.current = entries;
  const focusIdxRef = useRef(focusIdx);
  focusIdxRef.current = focusIdx;
  const parentPathRef = useRef(parentPath);
  parentPathRef.current = parentPath;
  const editingPathRef = useRef(editingPath);
  editingPathRef.current = editingPath;
  const enterDirRef = useRef(enterDirectory);
  enterDirRef.current = enterDirectory;
  const confirmRef = useRef(handleConfirm);
  confirmRef.current = handleConfirm;
  const cancelRef = useRef(onCancel);
  cancelRef.current = onCancel;

  useEffect(() => {
    if (!keyboardActive) return;
    let winComposing = false;
    let winLastCompositionEndAt = 0;
    const onCompStart = () => {
      winComposing = true;
    };
    const onCompEnd = () => {
      winComposing = false;
      winLastCompositionEndAt = performance.now();
    };
    const onKeyDown = (e: KeyboardEvent) => {
      if (isImeKeyEvent(e, winComposing, winLastCompositionEndAt)) return;
      const activeEl = document.activeElement instanceof HTMLElement ? document.activeElement : null;
      const targetEl = e.target instanceof HTMLElement ? e.target : activeEl;

      const inFormField =
        (activeEl && /^(INPUT|TEXTAREA|SELECT)$/.test(activeEl.tagName)) ||
        (targetEl && /^(INPUT|TEXTAREA|SELECT)$/.test(targetEl.tagName));
      if (inFormField) return;

      if (e.key === 'Escape') {
        if (e.defaultPrevented) return;
        if (editingPathRef.current) {
          e.preventDefault();
          e.stopPropagation();
          setEditingPath(false);
          setError('');
          return;
        }
        if (cancelRef.current) {
          e.preventDefault();
          e.stopPropagation();
          cancelRef.current();
        }
        return;
      }

      // CRITICAL: If focus is on any non-row button (Confirm, Cancel, Up, Crumb, Edit, Machine tab, Close, etc.),
      // NEVER intercept Enter so button activation works natively.
      const focusedBtn = targetEl?.closest('button') || activeEl?.closest('button');
      const isRowMainBtn = !!focusedBtn?.classList.contains('cdx-row-main');
      if (e.key === 'Enter' && focusedBtn && !isRowMainBtn) {
        return;
      }

      if (e.key === 'ArrowDown') {
        const list = entriesRef.current;
        if (list.length === 0) return;
        e.preventDefault();
        setFocusIdx((prev) => (prev < 0 ? 0 : (prev + 1) % list.length));
        return;
      }
      if (e.key === 'ArrowUp') {
        const list = entriesRef.current;
        if (list.length === 0) return;
        e.preventDefault();
        setFocusIdx((prev) => (prev <= 0 ? list.length - 1 : prev - 1));
        return;
      }
      if (e.key === 'ArrowRight') {
        const idx = focusIdxRef.current;
        const item = idx >= 0 ? entriesRef.current[idx] : undefined;
        if (item) {
          e.preventDefault();
          void enterDirRef.current(item.path, item.isGitRepo, item.branch);
        }
        return;
      }
      if (e.key === 'Enter') {
        const idx = focusIdxRef.current;
        const item = idx >= 0 ? entriesRef.current[idx] : undefined;
        if (item) {
          e.preventDefault();
          void enterDirRef.current(item.path, item.isGitRepo, item.branch);
        } else if (!focusedBtn) {
          e.preventDefault();
          void confirmRef.current();
        }
        return;
      }
      if (e.key === 'Backspace' || e.key === 'ArrowLeft') {
        if (parentPathRef.current) {
          e.preventDefault();
          void enterDirRef.current(parentPathRef.current, false, undefined);
        }
      }
    };
    window.addEventListener('compositionstart', onCompStart, true);
    window.addEventListener('compositionend', onCompEnd, true);
    window.addEventListener('keydown', onKeyDown);
    return () => {
      window.removeEventListener('compositionstart', onCompStart, true);
      window.removeEventListener('compositionend', onCompEnd, true);
      window.removeEventListener('keydown', onKeyDown);
    };
  }, [keyboardActive]);

  if (eligible.length === 0) {
    return <div className="muted">当前没有在线的设备，请先启动或连接一台设备。</div>;
  }

  const crumbs = buildBreadcrumbs(curPath, homePath);
  const displayTarget = formatDisplayPath(effectivePath, homePath);
  const canSubmit =
    !submitting &&
    !validating &&
    !loading &&
    !!machineId &&
    (editingPath ? !!pathDraft.trim() : loadedMachineId === machineId && !!effectivePath);

  const handleRowClick = (entry: DirEntryView, idx: number) => {
    const now = Date.now();
    const prev = lastClickRef.current;
    if (prev.path === entry.path && now - prev.at <= 380) {
      lastClickRef.current = { path: '', at: 0 };
      void enterDirectory(entry.path, entry.isGitRepo, entry.branch);
      return;
    }
    lastClickRef.current = { path: entry.path, at: now };
    setFocusIdx(idx);
    setEditingPath(false);
    setError('');
  };

  return (
    <div className="dir-picker">
      {eligible.length > 1 && (
        <div className="dir-mach-tabs" role="tablist" aria-label="选择设备">
          {eligible.map((m) => (
            <button
              key={m.machineId}
              type="button"
              role="tab"
              aria-selected={machineId === m.machineId}
              className={`dir-mach-tab ${machineId === m.machineId ? 'on' : ''}`}
              onClick={() => {
                if (m.machineId !== machineId) {
                  reqSeqRef.current += 1;
                  setValidating(false);
                  onMachineChange(m.machineId);
                }
              }}
            >
              <DeviceRoleIcon
                machine={m}
                isActiveDuty={m.machineId === activeDutyMachineId}
                isCurrentDevice={m.machineId === currentMachineId}
                size={13}
              />
              <span className="ell">{m.displayAlias || cleanMachineName(m)}</span>
            </button>
          ))}
        </div>
      )}

      <div className="cdx-dir-box">
        <div className="cdx-nav">
          <button
            type="button"
            className="cdx-up-btn"
            disabled={!parentPath || loading}
            title="返回上级目录 (Backspace)"
            aria-label="返回上级目录"
            onClick={() => parentPath && void enterDirectory(parentPath)}
          >
            <ArrowUp size={13} strokeWidth={1.8} />
          </button>

          {editingPath ? (
            <>
              <input
                ref={pathInputRef}
                className="cdx-path-in mono"
                value={pathDraft}
                placeholder="输入或粘贴路径后回车，例如 ~/dev"
                aria-label="目录路径"
                onChange={(e) => {
                  setPathDraft(e.target.value);
                  if (error) setError('');
                }}
                {...ime.compositionProps}
                onKeyDown={(e) => {
                  if (ime.isImeEnter(e)) return;
                  if (e.key === 'Enter') {
                    e.preventDefault();
                    e.stopPropagation();
                    void applyTypedPath();
                  } else if (e.key === 'Escape') {
                    e.preventDefault();
                    e.stopPropagation();
                    setEditingPath(false);
                    setPathDraft(formatDisplayPath(curPath, homePath));
                    setError('');
                  }
                }}
              />
              <button
                type="button"
                className="cdx-edit-btn"
                title="前往该路径 (Enter)"
                aria-label="前往该路径"
                disabled={loading}
                onClick={() => void applyTypedPath()}
              >
                <Check size={13} strokeWidth={2} />
              </button>
            </>
          ) : (
            <>
              <div
                className="cdx-crumbs"
                title="点击路径段跳回上级，点击空白或右侧铅笔编辑路径"
                onClick={(e) => {
                  if (e.target === e.currentTarget) startEditPath();
                }}
              >
                {crumbs.map((c, idx) => (
                  <span className="cdx-crumb-seg" key={`${c.path}-${idx}`}>
                    {idx > 0 && <span className="cdx-crumb-sep">/</span>}
                    <button
                      type="button"
                      className={`cdx-crumb ${idx === crumbs.length - 1 ? 'is-cur' : ''}`}
                      onClick={(e) => {
                        e.stopPropagation();
                        if (idx === crumbs.length - 1) {
                          startEditPath();
                        } else {
                          void enterDirectory(c.path);
                        }
                      }}
                    >
                      {c.label}
                    </button>
                  </span>
                ))}
              </div>
              <button
                type="button"
                className="cdx-edit-btn"
                title="手动输入或粘贴路径"
                aria-label="编辑路径"
                onClick={startEditPath}
              >
                <Pencil size={12} strokeWidth={1.7} />
              </button>
            </>
          )}
        </div>

        {error && (
          <div className="cdx-err" role="alert">
            {error}
          </div>
        )}

        <div className="cdx-list" ref={listRef} role="listbox" aria-label="目录列表">
          {loading ? (
            <div className="cdx-empty">正在读取目录…</div>
          ) : entries.length === 0 ? (
            <div className="cdx-empty">
              当前目录无下级子目录 · 将直接采用 <span className="mono">{formatDisplayPath(curPath, homePath)}</span>
            </div>
          ) : (
            entries.map((entry, idx) => {
              const isAct = idx === focusIdx;
              return (
                <div
                  key={entry.path}
                  className={`cdx-row ${isAct ? 'active' : ''}`}
                  role="option"
                  aria-selected={isAct}
                  data-cdx-idx={idx}
                >
                  <button
                    type="button"
                    className="cdx-row-main"
                    onFocus={() => setFocusIdx(idx)}
                    onClick={() => handleRowClick(entry, idx)}
                    onDoubleClick={(e) => {
                      e.stopPropagation();
                      void enterDirectory(entry.path, entry.isGitRepo, entry.branch);
                    }}
                  >
                    <span className="cdx-row-ico" aria-hidden="true">
                      {entry.isGitRepo ? (
                        <GitBranch size={13} strokeWidth={1.75} className="ok-t" />
                      ) : (
                        <Folder size={13} strokeWidth={1.65} />
                      )}
                    </span>
                    <span className="cdx-name">{entry.name}</span>
                    {entry.isGitRepo && entry.branch && (
                      <span className="git-zsh mono">{entry.branch}</span>
                    )}
                  </button>
                  <button
                    type="button"
                    className="cdx-enter-btn"
                    title={`进入 ${entry.name}`}
                    aria-label={`进入 ${entry.name}`}
                    onClick={(e) => {
                      e.stopPropagation();
                      void enterDirectory(entry.path, entry.isGitRepo, entry.branch);
                    }}
                  >
                    <ChevronRight size={13} strokeWidth={1.8} />
                  </button>
                </div>
              );
            })
          )}
        </div>

        <div className="cdx-foot">
          <span className="cdx-foot-path" title={effectivePath || curPath}>
            {editingPath ? pathDraft || '~' : displayTarget || '~'}
          </span>
          <span className="no-mobile">↑↓ 选择 · Enter 进入 · ⌫ 上级</span>
        </div>
      </div>

      <div className="cdx-actions">
        {cancelLabel && onCancel && (
          <button type="button" className="btn ghost sm" onClick={onCancel}>
            {cancelLabel}
          </button>
        )}
        <button
          type="button"
          className="btn pri sm"
          data-cdx-confirm="1"
          disabled={!canSubmit}
          onClick={() => void handleConfirm()}
        >
          {submitting || validating ? '保存中…' : confirmLabel}
        </button>
      </div>
    </div>
  );
}

interface DirBrowseModalProps {
  open: boolean;
  title?: string;
  confirmLabel?: string;
  machines: MachineView[];
  defaultMachineId?: string;
  initialPath?: string;
  activeDutyMachineId?: string;
  currentMachineId?: string;
  extraFieldLabel?: string;
  extraFieldPlaceholder?: string;
  showPartKindToggle?: boolean;
  onConfirm: (sel: {
    machineId: string;
    absPath: string;
    isGit: boolean;
    extraValue?: string;
    partKind?: ProjectPartKind;
  }) => Promise<void>;
  onClose: () => void;
}

/**
 * Reusable Codex-style cross-machine directory browser modal.
 * Shared across "+ 添加组成部分", "+ 添加位置", and "编辑目录位置".
 */
export function DirBrowseModal(props: DirBrowseModalProps) {
  if (!props.open) return null;
  return <DirBrowseModalContent {...props} />;
}

function DirBrowseModalContent({
  title = '选择设备目录',
  confirmLabel = '选择此目录',
  machines,
  defaultMachineId,
  initialPath,
  activeDutyMachineId,
  currentMachineId,
  extraFieldLabel,
  extraFieldPlaceholder,
  showPartKindToggle = false,
  onConfirm,
  onClose,
}: DirBrowseModalProps) {
  const eligible = useMemo(
    () => machines.filter((m) => m.transportMode !== 'ssh_outbound' && m.status === 'online'),
    [machines],
  );
  const [machineId, setMachineId] = useState(() => {
    if (defaultMachineId && eligible.some((m) => m.machineId === defaultMachineId)) {
      return defaultMachineId;
    }
    return eligible[0]?.machineId ?? '';
  });
  const [extraVal, setExtraVal] = useState('');
  const [partKind, setPartKind] = useState<ProjectPartKind>('folder');
  const [partKindPinned, setPartKindPinned] = useState(false);
  const [submitting, setSubmitting] = useState(false);

  const handleSelectionChange = useCallback(
    (sel: CodexDirSelection | null) => {
      if (!showPartKindToggle || !sel || partKindPinned) return;
      setPartKind(sel.isGit ? 'git' : 'folder');
    },
    [showPartKindToggle, partKindPinned],
  );

  return (
    <div className="modal-scrim" onClick={onClose}>
      <section className="modal-card proj-modal" role="dialog" aria-label={title} onClick={(e) => e.stopPropagation()}>
        <div className="modal-hd">
          <h3>{title}</h3>
          <button type="button" className="ib" onClick={onClose} aria-label="关闭">
            <X size={15} strokeWidth={1.7} />
          </button>
        </div>
        <div className="modal-body">
          {(extraFieldLabel || showPartKindToggle) && (
            <div className={`cdx-meta-grid ${showPartKindToggle ? 'has-kind' : ''}`}>
              {extraFieldLabel && (
                <div className="cdx-meta-field">
                  <label className="cdx-meta-lbl">{extraFieldLabel}</label>
                  <input
                    className="field"
                    value={extraVal}
                    placeholder={extraFieldPlaceholder}
                    onChange={(e) => setExtraVal(e.target.value)}
                  />
                </div>
              )}
              {showPartKindToggle && (
                <div className="cdx-meta-field">
                  <span className="cdx-meta-lbl">目录类型</span>
                  <div className="role-seg" role="radiogroup" aria-label="目录类型">
                    <button
                      type="button"
                      role="radio"
                      aria-checked={partKind === 'git'}
                      className={partKind === 'git' ? 'on' : ''}
                      onClick={() => {
                        setPartKind('git');
                        setPartKindPinned(true);
                      }}
                    >
                      Git 仓库
                    </button>
                    <button
                      type="button"
                      role="radio"
                      aria-checked={partKind === 'folder'}
                      className={partKind === 'folder' ? 'on' : ''}
                      onClick={() => {
                        setPartKind('folder');
                        setPartKindPinned(true);
                      }}
                    >
                      普通目录
                    </button>
                  </div>
                </div>
              )}
            </div>
          )}

          <CodexDirBrowser
            machines={machines}
            machineId={machineId}
            onMachineChange={setMachineId}
            initialPath={initialPath}
            activeDutyMachineId={activeDutyMachineId}
            currentMachineId={currentMachineId}
            confirmLabel={confirmLabel}
            cancelLabel="取消"
            submitting={submitting}
            keyboardActive
            onSelectionChange={handleSelectionChange}
            onCancel={onClose}
            onConfirm={async (sel) => {
              setSubmitting(true);
              try {
                await onConfirm({
                  machineId: sel.machineId,
                  absPath: sel.absPath,
                  isGit: sel.isGit,
                  extraValue: extraVal.trim() || undefined,
                  partKind: showPartKindToggle ? (partKindPinned ? partKind : sel.isGit ? 'git' : undefined) : undefined,
                });
                onClose();
              } finally {
                setSubmitting(false);
              }
            }}
          />
        </div>
      </section>
    </div>
  );
}
