import { useEffect, useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Folder, GitBranch, ArrowUp, X } from 'lucide-react';
import type { DirEntryView, MachineView, ProjectView } from '../generated/protocol';
import { api, errMsg } from '../lib/api';
import { cleanMachineName } from '../lib/format';
import { toast } from '../store/useWorkbenchStore';

export function ProjectDirPicker({
  machines,
  defaultMachineId,
  onAdded,
}: {
  machines: MachineView[];
  defaultMachineId?: string;
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
  const [entries, setEntries] = useState<DirEntryView[]>([]);
  const [selectedRepo, setSelectedRepo] = useState('');
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
      setSelectedRepo('');
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
    setSelectedRepo('');
    setLoadedMachineId('');
    setError('');
    if (machineId) {
      void loadDir(machineId);
    }
  }, [machineId]);

  const submitAdd = async (repoPath: string) => {
    if (!machineId || loadedMachineId !== machineId || !repoPath.trim()) return;
    setAdding(true);
    setError('');
    try {
      const res = await api.createProject({
        machineId,
        repoPath: repoPath.trim(),
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
    return <div className="muted">当前没有在线的机器，请先启动或连接一台机器。</div>;
  }

  return (
    <div className="dir-picker">
      <div className="dir-bar">
        {eligible.length > 1 && (
          <select
            className="field dir-mach"
            value={machineId}
            onChange={(e) => setMachineId(e.target.value)}
            aria-label="选择机器"
          >
            {eligible.map((m) => (
              <option key={m.machineId} value={m.machineId}>
                {m.displayAlias || cleanMachineName(m)}
              </option>
            ))}
          </select>
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
            placeholder="输入或粘贴 Git 仓库路径"
            onChange={(e) => setPathInput(e.target.value)}
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
      </div>

      {error && <div className="bad-t dir-err">{error}</div>}

      <div className="dir-list" role="listbox" aria-label="目录列表">
        {loading ? (
          <div className="dir-empty muted">正在读取目录…</div>
        ) : entries.length === 0 ? (
          <div className="dir-empty muted">该目录下没有子文件夹</div>
        ) : (
          entries.map((entry) => {
            const selected = selectedRepo === entry.path;
            return (
              <div
                key={entry.path}
                className={`dir-row ${selected ? 'on' : ''}`}
                onClick={() => {
                  if (entry.isGitRepo) {
                    setSelectedRepo(entry.path);
                    setPathInput(entry.path);
                  } else {
                    void loadDir(machineId, entry.path);
                  }
                }}
                onDoubleClick={() => {
                  if (!entry.isGitRepo) {
                    void loadDir(machineId, entry.path);
                  } else {
                    void submitAdd(entry.path);
                  }
                }}
              >
                {entry.isGitRepo ? (
                  <GitBranch size={13.5} strokeWidth={1.7} className="ok-t" />
                ) : (
                  <Folder size={13.5} strokeWidth={1.6} className="muted" />
                )}
                <span className="ell grow">{entry.name}</span>
                {entry.isGitRepo && (
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
                )}
              </div>
            );
          })
        )}
      </div>

      <div className="dir-foot">
        <span className="muted ell mono">{selectedRepo || curPath}</span>
        <button
          type="button"
          className={`btn ${selectedRepo ? 'pri' : 'ghost'} sm`}
          disabled={adding || loading || loadedMachineId !== machineId || !(selectedRepo || pathInput.trim())}
          onClick={() => void submitAdd(selectedRepo || pathInput.trim())}
        >
          {adding ? '添加中…' : '添加当前路径'}
        </button>
      </div>
    </div>
  );
}

export function AddProjectModal({
  open,
  machines,
  defaultMachineId,
  onClose,
  onAdded,
}: {
  open: boolean;
  machines: MachineView[];
  defaultMachineId?: string;
  onClose: () => void;
  onAdded?: (project: ProjectView) => void;
}) {
  if (!open) return null;
  return (
    <div className="modal-scrim" onClick={onClose}>
      <section className="modal-card proj-modal" role="dialog" aria-label="添加项目" onClick={(e) => e.stopPropagation()}>
        <div className="modal-hd">
          <h3>添加本地项目</h3>
          <button type="button" className="ib" onClick={onClose} aria-label="关闭">
            <X size={15} strokeWidth={1.7} />
          </button>
        </div>
        <div className="modal-body">
          <ProjectDirPicker
            machines={machines}
            defaultMachineId={defaultMachineId}
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
