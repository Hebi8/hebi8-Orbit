import { useEffect, useRef, useState } from 'react';
import { ArrowUp, Folder, GitBranch, X } from 'lucide-react';
import type { DirEntryView, MachineView } from '../generated/protocol';
import { api, errMsg } from '../lib/api';
import { cleanMachineName } from '../lib/format';
import { DeviceRoleIcon } from './DeviceRoleIcon';

/**
 * Reusable cross-machine directory browser modal.
 * Supports BOTH Git repositories (showing zsh-style branch) AND plain folders as first-class choices.
 */
export function DirBrowseModal({
  open,
  title = '选择目录',
  confirmLabel = '选择此目录',
  machines,
  defaultMachineId,
  activeDutyMachineId,
  currentMachineId,
  extraFieldLabel,
  extraFieldPlaceholder,
  onConfirm,
  onClose,
}: {
  open: boolean;
  title?: string;
  confirmLabel?: string;
  machines: MachineView[];
  defaultMachineId?: string;
  activeDutyMachineId?: string;
  currentMachineId?: string;
  extraFieldLabel?: string;
  extraFieldPlaceholder?: string;
  onConfirm: (sel: { machineId: string; absPath: string; isGit: boolean; extraValue?: string }) => Promise<void>;
  onClose: () => void;
}) {
  const eligible = machines.filter((m) => m.transportMode !== 'ssh_outbound' && m.status === 'online');
  const [machineId, setMachineId] = useState(
    defaultMachineId && eligible.some((m) => m.machineId === defaultMachineId)
      ? defaultMachineId
      : (eligible[0]?.machineId ?? ''),
  );
  const [curPath, setCurPath] = useState('');
  const [parentPath, setParentPath] = useState('');
  const [pathInput, setPathInput] = useState('');
  const [entries, setEntries] = useState<DirEntryView[]>([]);
  const [selectedEntry, setSelectedEntry] = useState<DirEntryView | null>(null);
  const [extraVal, setExtraVal] = useState('');
  const [loadedMachineId, setLoadedMachineId] = useState('');
  const [loading, setLoading] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState('');
  const reqSeqRef = useRef(0);
  const activeMachineRef = useRef(machineId);
  activeMachineRef.current = machineId;

  useEffect(() => {
    if (!open) return;
    setExtraVal('');
    setError('');
    if (!machineId || !eligible.some((m) => m.machineId === machineId)) {
      const next =
        defaultMachineId && eligible.some((m) => m.machineId === defaultMachineId)
          ? defaultMachineId
          : (eligible[0]?.machineId ?? '');
      setMachineId(next);
    }
  }, [open, defaultMachineId, eligible.length]);

  const loadDir = async (mid: string, targetPath?: string) => {
    if (!mid) return;
    const seq = ++reqSeqRef.current;
    setLoading(true);
    setError('');
    try {
      const res = await api.listMachineDirs(mid, targetPath);
      if (seq !== reqSeqRef.current || activeMachineRef.current !== mid) return;
      setCurPath(res.path);
      setParentPath(res.parentPath ?? '');
      setPathInput(res.path);
      setEntries(res.entries ?? []);
      setSelectedEntry(null);
      setLoadedMachineId(mid);
    } catch (e) {
      if (seq !== reqSeqRef.current || activeMachineRef.current !== mid) return;
      setError(errMsg(e));
    } finally {
      if (seq === reqSeqRef.current && activeMachineRef.current === mid) {
        setLoading(false);
      }
    }
  };

  useEffect(() => {
    if (!open || !machineId) return;
    setCurPath('');
    setParentPath('');
    setPathInput('');
    setEntries([]);
    setSelectedEntry(null);
    setLoadedMachineId('');
    setError('');
    void loadDir(machineId);
  }, [open, machineId]);

  if (!open) return null;

  const targetPath = (selectedEntry?.path || pathInput || curPath).trim();
  const targetIsGit = selectedEntry ? selectedEntry.isGitRepo : false;

  const submit = async (overridePath?: string, overrideGit?: boolean) => {
    const finalPath = (overridePath ?? targetPath).trim();
    if (!machineId || !finalPath || submitting) return;
    setSubmitting(true);
    setError('');
    try {
      await onConfirm({
        machineId,
        absPath: finalPath,
        isGit: overrideGit ?? targetIsGit,
        extraValue: extraVal.trim() || undefined,
      });
      onClose();
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setSubmitting(false);
    }
  };

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
          {eligible.length === 0 ? (
            <div className="muted">当前没有在线的设备，请先启动或连接一台设备。</div>
          ) : (
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
                      onClick={() => setMachineId(m.machineId)}
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

              {extraFieldLabel && (
                <div className="dir-extra-row">
                  <span className="muted" style={{ fontSize: 12, flexShrink: 0 }}>
                    {extraFieldLabel}
                  </span>
                  <input
                    className="field grow"
                    value={extraVal}
                    placeholder={extraFieldPlaceholder}
                    onChange={(e) => setExtraVal(e.target.value)}
                  />
                </div>
              )}

              <div className="dir-path-wrap">
                {parentPath && (
                  <button
                    type="button"
                    className="ib"
                    title="上一级目录"
                    aria-label="上一级目录"
                    onClick={() => void loadDir(machineId, parentPath)}
                  >
                    <ArrowUp size={13} strokeWidth={1.7} />
                  </button>
                )}
                <input
                  className="field mono grow"
                  value={pathInput}
                  placeholder="输入或粘贴目录路径（Git 仓库或文件夹均可）"
                  onChange={(e) => {
                    setPathInput(e.target.value);
                    setSelectedEntry(null);
                  }}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter') {
                      e.preventDefault();
                      void loadDir(machineId, pathInput);
                    }
                  }}
                />
                <button
                  type="button"
                  className="btn ghost sm"
                  disabled={loading}
                  onClick={() => void loadDir(machineId, pathInput)}
                >
                  打开
                </button>
              </div>

              {error && <div className="bad-t dir-err">{error}</div>}

              <div className="dir-list" role="listbox" aria-label="目录列表">
                {loading ? (
                  <div className="dir-empty muted">正在读取目录…</div>
                ) : entries.length === 0 ? (
                  <div className="dir-empty muted">该目录下没有子文件夹，可直接选择当前路径</div>
                ) : (
                  entries.map((entry) => {
                    const isSel = selectedEntry?.path === entry.path;
                    return (
                      <div
                        key={entry.path}
                        className={`dir-row ${isSel ? 'on' : ''}`}
                        onClick={() => {
                          setSelectedEntry(entry);
                          setPathInput(entry.path);
                        }}
                        onDoubleClick={() => {
                          void loadDir(machineId, entry.path);
                        }}
                      >
                        {entry.isGitRepo ? (
                          <GitBranch size={13.5} strokeWidth={1.7} className="ok-t" />
                        ) : (
                          <Folder size={13.5} strokeWidth={1.6} className="muted" />
                        )}
                        <span className="ell grow">{entry.name}</span>
                        {entry.isGitRepo && entry.branch && (
                          <span className="git-zsh mono">{entry.branch}</span>
                        )}
                        <button
                          type="button"
                          className="btn ghost sm"
                          disabled={submitting}
                          onClick={(e) => {
                            e.stopPropagation();
                            void loadDir(machineId, entry.path);
                          }}
                        >
                          进入
                        </button>
                      </div>
                    );
                  })
                )}
              </div>

              <div className="dir-foot">
                <span className="muted ell mono" title={targetPath}>
                  {targetPath || '选择目录'}
                </span>
                <button
                  type="button"
                  className="btn pri sm"
                  disabled={submitting || loading || loadedMachineId !== machineId || !targetPath}
                  onClick={() => void submit()}
                >
                  {submitting ? '保存中…' : confirmLabel}
                </button>
              </div>
            </div>
          )}
        </div>
      </section>
    </div>
  );
}
