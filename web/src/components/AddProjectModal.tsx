import { useEffect, useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { ArrowUp, Folder, GitBranch, X } from 'lucide-react';
import type { DirEntryView, MachineView, ProjectView } from '../generated/protocol';
import { api, errMsg } from '../lib/api';
import { cleanMachineName } from '../lib/format';
import { toast } from '../store/useWorkbenchStore';
import { DeviceRoleIcon } from './DeviceRoleIcon';

export function ProjectDirPicker({
  machines,
  defaultMachineId,
  activeDutyMachineId,
  currentMachineId,
  onAdded,
}: {
  machines: MachineView[];
  defaultMachineId?: string;
  activeDutyMachineId?: string;
  currentMachineId?: string;
  onAdded?: (project: ProjectView) => void;
}) {
  const qc = useQueryClient();
  const eligible = machines.filter((m) => m.transportMode !== 'ssh_outbound' && m.status === 'online');
  const [machineId, setMachineId] = useState(
    defaultMachineId && eligible.some((m) => m.machineId === defaultMachineId)
      ? defaultMachineId
      : (eligible[0]?.machineId ?? ''),
  );
  const [curPath, setCurPath] = useState('');
  const [parentPath, setParentPath] = useState('');
  const [pathInput, setPathInput] = useState('');
  const [displayName, setDisplayName] = useState('');
  const [entries, setEntries] = useState<DirEntryView[]>([]);
  const [selectedDir, setSelectedDir] = useState('');
  const [loadedMachineId, setLoadedMachineId] = useState('');
  const [loading, setLoading] = useState(false);
  const [adding, setAdding] = useState(false);
  const [error, setError] = useState('');
  const reqSeqRef = useRef(0);
  const activeMachineRef = useRef(machineId);
  activeMachineRef.current = machineId;

  useEffect(() => {
    if (!machineId && eligible[0]) {
      setMachineId(eligible[0].machineId);
    }
  }, [eligible, machineId]);

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
      setSelectedDir('');
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
    setCurPath('');
    setParentPath('');
    setPathInput('');
    setEntries([]);
    setSelectedDir('');
    setLoadedMachineId('');
    setError('');
    if (machineId) {
      void loadDir(machineId);
    }
  }, [machineId]);

  const submitAdd = async (targetPath: string) => {
    if (!machineId || loadedMachineId !== machineId || !targetPath.trim()) return;
    setAdding(true);
    setError('');
    try {
      const res = await api.createProject({
        machineId,
        repoPath: targetPath.trim(),
        displayName: displayName.trim() || undefined,
        allowFolder: true,
      });
      toast(`已添加项目 ${res.project.displayName}`);
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
      onAdded?.(res.project);
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setAdding(false);
    }
  };

  if (eligible.length === 0) {
    return <div className="muted">当前没有在线的设备，请先启动或连接一台设备。</div>;
  }

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

      <div className="dir-extra-row">
        <span className="muted" style={{ fontSize: 12, flexShrink: 0 }}>
          项目名称
        </span>
        <input
          className="field grow"
          value={displayName}
          placeholder="留空则默认使用目录名"
          onChange={(e) => setDisplayName(e.target.value)}
        />
      </div>

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
          placeholder="输入或粘贴仓库 / 目录路径"
          onChange={(e) => {
            setPathInput(e.target.value);
            setSelectedDir('');
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
          <div className="dir-empty muted">该目录下没有子文件夹，可直接添加当前目录</div>
        ) : (
          entries.map((entry) => {
            const selected = selectedDir === entry.path;
            return (
              <div
                key={entry.path}
                className={`dir-row ${selected ? 'on' : ''}`}
                onClick={() => {
                  setSelectedDir(entry.path);
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
                  disabled={adding}
                  onClick={(e) => {
                    e.stopPropagation();
                    void submitAdd(entry.path);
                  }}
                >
                  添加
                </button>
              </div>
            );
          })
        )}
      </div>

      <div className="dir-foot">
        <span className="muted ell mono">{selectedDir || curPath}</span>
        <button
          type="button"
          className="btn pri sm"
          disabled={adding || loading || loadedMachineId !== machineId || !(selectedDir || pathInput.trim())}
          onClick={() => void submitAdd(selectedDir || pathInput.trim())}
        >
          {adding ? '添加中…' : '添加此目录'}
        </button>
      </div>
    </div>
  );
}

export function AddProjectModal({
  open,
  machines,
  defaultMachineId,
  activeDutyMachineId,
  currentMachineId,
  onClose,
  onAdded,
}: {
  open: boolean;
  machines: MachineView[];
  defaultMachineId?: string;
  activeDutyMachineId?: string;
  currentMachineId?: string;
  onClose: () => void;
  onAdded?: (project: ProjectView) => void;
}) {
  if (!open) return null;
  return (
    <div className="modal-scrim" onClick={onClose}>
      <section className="modal-card proj-modal" role="dialog" aria-label="添加项目" onClick={(e) => e.stopPropagation()}>
        <div className="modal-hd">
          <h3>添加项目</h3>
          <button type="button" className="ib" onClick={onClose} aria-label="关闭">
            <X size={15} strokeWidth={1.7} />
          </button>
        </div>
        <div className="modal-body">
          <ProjectDirPicker
            machines={machines}
            defaultMachineId={defaultMachineId}
            activeDutyMachineId={activeDutyMachineId}
            currentMachineId={currentMachineId}
            onAdded={(p) => {
              onAdded?.(p);
              onClose();
            }}
          />
        </div>
      </section>
    </div>
  );
}
