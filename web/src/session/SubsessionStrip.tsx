import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Square } from 'lucide-react';
import type { BoardMemberCursor, ChildTaskView, MachineView } from '../generated/protocol';
import { ADAPTER_LABELS } from '../lib/agentCaps';
import { api, errMsg } from '../lib/api';
import {
  childStateDot,
  childStateLabel,
  cleanMachineName,
  isChildStateLive,
  memberStateLabel,
  taskTitle,
} from '../lib/format';
import { toast } from '../store/useWorkbenchStore';

function resolveMachineShort(machines: MachineView[], machineId?: string): string {
  if (!machineId) return '';
  const m = machines.find((x) => x.machineId === machineId);
  if (!m) return machineId;
  return m.displayAlias?.trim() || cleanMachineName(m);
}

function nativeMemberDot(state: BoardMemberCursor['state']): 'run' | 'approval' | 'bad' | 'idle' {
  switch (state) {
    case 'working':
      return 'run';
    case 'waiting':
      return 'approval';
    case 'killed':
      return 'bad';
    default:
      return 'idle';
  }
}

export function SubsessionStrip({
  parentTaskId,
  childrenList,
  boardMembers = [],
  machines,
  activeChildId,
  activeNativeMember,
  onSelectChild,
  onSelectNativeMember,
}: {
  parentTaskId: string;
  childrenList: ChildTaskView[];
  boardMembers?: BoardMemberCursor[];
  machines: MachineView[];
  activeChildId?: string;
  activeNativeMember?: string;
  onSelectChild: (childTaskId: string) => void;
  onSelectNativeMember: (member: BoardMemberCursor) => void;
}) {
  const qc = useQueryClient();

  const stopMut = useMutation({
    mutationFn: (childTaskId: string) => api.cancelTask(childTaskId, 'stopped from subsession strip'),
    onSuccess: (res, childTaskId) => {
      toast(res.cancelSent || res.immediateState ? '已请求停止子会话' : '停止请求已记录');
      void qc.invalidateQueries({ queryKey: ['task-timeline', parentTaskId] });
      void qc.invalidateQueries({ queryKey: ['task-timeline', childTaskId] });
      void qc.invalidateQueries({ queryKey: ['task-children', parentTaskId] });
      void qc.invalidateQueries({ queryKey: ['all-tasks'] });
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const nativeMembers = boardMembers.filter((m) => !m.workerTaskId);
  const totalCount = childrenList.length + nativeMembers.length;
  if (totalCount === 0) return null;

  const runningCount =
    childrenList.filter((c) => isChildStateLive(c.state)).length +
    nativeMembers.filter((m) => m.state === 'working').length;

  const pendingApprovals = childrenList.reduce(
    (sum, c) => sum + (c.openApprovalCount > 0 ? c.openApprovalCount : c.state === 'waiting_approval' ? 1 : 0),
    0,
  );

  return (
    <div className="sub-strip" role="region" aria-label="子会话状态条">
      <div className="sub-strip-summary">
        <span className="sub-sum-title">子会话 · {totalCount}</span>
        {runningCount > 0 && <span className="sub-sum-pill is-run">{runningCount} 运行中</span>}
        {pendingApprovals > 0 && <span className="sub-sum-pill is-ap">{pendingApprovals} 待审批</span>}
      </div>

      <div className="sub-strip-list">
        {childrenList.map((child) => {
          const live = isChildStateLive(child.state);
          const dot = childStateDot(child.state, child.openApprovalCount);
          const isActive = activeChildId === child.taskId;
          const needsApproval = child.openApprovalCount > 0 || child.state === 'waiting_approval';
          const mach = resolveMachineShort(machines, child.machineId);
          const adp = child.preferredAdapter ? ADAPTER_LABELS[child.preferredAdapter] ?? child.preferredAdapter : '';
          const metaText =
            (live && child.currentStep?.trim()) || [mach, adp].filter(Boolean).join(' · ') || childStateLabel(child.state);
          const titleText = taskTitle(child);

          return (
            <div
              key={child.taskId}
              className={`sub-chip ${isActive ? 'is-active' : ''} ${needsApproval ? 'is-ap' : ''} ${live ? 'is-live' : ''}`}
            >
              <button
                type="button"
                className="sub-chip-main"
                onClick={() => onSelectChild(child.taskId)}
                title={`${child.delegatedRole ? `[${child.delegatedRole}] ` : ''}${titleText} · ${childStateLabel(child.state, child.openApprovalCount)}${child.currentStep ? ` · ${child.currentStep}` : ''}`}
              >
                <span className={`sub-dot is-${dot}`} aria-hidden="true" />
                {child.delegatedRole && <span className="sub-chip-role">{child.delegatedRole}</span>}
                <span className="sub-chip-ttl ell">{titleText}</span>
                {metaText && <span className="sub-chip-meta ell">{metaText}</span>}
                {needsApproval && (
                  <span className="sub-chip-ap">
                    待审批{child.openApprovalCount > 1 ? ` ${child.openApprovalCount}` : ''}
                  </span>
                )}
              </button>
              {live && (
                <button
                  type="button"
                  className="sub-chip-stop"
                  title="停止该子会话"
                  aria-label={`停止子会话 ${titleText}`}
                  disabled={stopMut.isPending && stopMut.variables === child.taskId}
                  onClick={(e) => {
                    e.stopPropagation();
                    stopMut.mutate(child.taskId);
                  }}
                >
                  <Square size={9} fill="currentColor" strokeWidth={0} />
                </button>
              )}
            </div>
          );
        })}

        {nativeMembers.map((m) => {
          const isActive = !activeChildId && activeNativeMember === m.memberName;
          const dot = nativeMemberDot(m.state);
          const kindLabel = m.sourceKind === 'native_swarm' ? 'Swarm' : '内置';
          const mach = resolveMachineShort(machines, m.machineId);
          const adp = m.adapter ? ADAPTER_LABELS[m.adapter] ?? m.adapter : '';
          const metaText = m.currentStep?.trim() || [mach, adp].filter(Boolean).join(' · ') || memberStateLabel(m.state);

          return (
            <div key={`native:${m.memberName}`} className={`sub-chip is-native ${isActive ? 'is-active' : ''}`}>
              <button
                type="button"
                className="sub-chip-main"
                onClick={() => onSelectNativeMember(m)}
                title={`${kindLabel} · @${m.memberName}${m.role ? ` (${m.role})` : ''} · ${memberStateLabel(m.state)}${m.currentStep ? ` · ${m.currentStep}` : ''}`}
              >
                <span className={`sub-dot is-${dot}`} aria-hidden="true" />
                <span className="sub-chip-native-tag">{kindLabel}</span>
                {m.role && <span className="sub-chip-role">{m.role}</span>}
                <span className="sub-chip-ttl ell">@{m.memberName}</span>
                {metaText && <span className="sub-chip-meta ell">{metaText}</span>}
              </button>
            </div>
          );
        })}
      </div>
    </div>
  );
}
