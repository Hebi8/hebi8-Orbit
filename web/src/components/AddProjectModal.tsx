import { useEffect, useMemo, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { X } from 'lucide-react';
import type { MachineView, ProjectView } from '../generated/protocol';
import { api } from '../lib/api';
import { toast } from '../store/useWorkbenchStore';
import { CodexDirBrowser } from './DirBrowseModal';

export function ProjectDirPicker({
  machines,
  defaultMachineId,
  activeDutyMachineId,
  currentMachineId,
  keyboardActive = true,
  onAdded,
  onCancel,
}: {
  machines: MachineView[];
  defaultMachineId?: string;
  activeDutyMachineId?: string;
  currentMachineId?: string;
  keyboardActive?: boolean;
  onAdded?: (project: ProjectView) => void;
  onCancel?: () => void;
}) {
  const qc = useQueryClient();
  const eligible = useMemo(
    () => machines.filter((m) => m.transportMode !== 'ssh_outbound' && m.status === 'online'),
    [machines],
  );
  const [machineId, setMachineId] = useState(
    defaultMachineId && eligible.some((m) => m.machineId === defaultMachineId)
      ? defaultMachineId
      : (eligible[0]?.machineId ?? ''),
  );
  const [displayName, setDisplayName] = useState('');
  const [adding, setAdding] = useState(false);

  useEffect(() => {
    if (!machineId && eligible[0]) {
      setMachineId(eligible[0].machineId);
    }
  }, [eligible, machineId]);

  if (eligible.length === 0) {
    return <div className="muted">当前没有在线的设备，请先启动或连接一台设备。</div>;
  }

  return (
    <div className="dir-picker">
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

      <CodexDirBrowser
        machines={machines}
        machineId={machineId}
        onMachineChange={setMachineId}
        activeDutyMachineId={activeDutyMachineId}
        currentMachineId={currentMachineId}
        confirmLabel="添加此目录"
        cancelLabel={onCancel ? '取消' : undefined}
        submitting={adding}
        keyboardActive={keyboardActive}
        onCancel={onCancel}
        onConfirm={async ({ machineId: mid, absPath }) => {
          setAdding(true);
          try {
            const res = await api.createProject({
              machineId: mid,
              repoPath: absPath,
              displayName: displayName.trim() || undefined,
              allowFolder: true,
            });
            toast(`已添加项目 ${res.project.displayName}`);
            void qc.invalidateQueries({ queryKey: ['bootstrap'] });
            onAdded?.(res.project);
          } finally {
            setAdding(false);
          }
        }}
      />
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
            keyboardActive={open}
            onCancel={onClose}
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
