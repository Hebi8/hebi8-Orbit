import { useEffect, useMemo } from 'react';
import { ArrowUpRight, Check, Info, Layers, MessageSquare, X } from 'lucide-react';
import type { BoardMemberCursor, BootstrapResponse, ChildTaskView } from '../generated/protocol';
import { DiffIcon } from '../components/Icons';
import { IdeOpenButton } from '../components/IdeOpenButton';
import { useTimeline } from '../hooks/queries';
import { ADAPTER_LABELS } from '../lib/agentCaps';
import { errMsg } from '../lib/api';
import { collectSessionArtifacts } from '../lib/artifacts';
import {
  childStateDot,
  childStateLabel,
  cleanMachineName,
  isVirtualProject,
  memberStateLabel,
  shortSha,
  taskTitle,
} from '../lib/format';
import { navigate, paths } from '../lib/router';
import { useWorkbenchStore } from '../store/useWorkbenchStore';
import { SessionConversation } from './SessionConversation';
import { WorkerStream } from './WorkerStream';

const CODE_KINDS = new Set(['code_diff', 'mixed', 'workspace_snapshot']);

export function ChildSessionDrawer({
  boot,
  parentTaskId,
  parentProjectId,
  childTaskId,
  nativeMember,
  childrenList,
  nativeMembers = [],
  onSelectChild,
  onSelectNativeMember,
  onClose,
}: {
  boot: BootstrapResponse;
  parentTaskId: string;
  parentProjectId?: string;
  childTaskId?: string;
  nativeMember?: BoardMemberCursor;
  childrenList: ChildTaskView[];
  nativeMembers?: BoardMemberCursor[];
  onSelectChild: (childTaskId: string) => void;
  onSelectNativeMember: (member: BoardMemberCursor) => void;
  onClose: () => void;
}) {
  const openReview = useWorkbenchStore((s) => s.openReview);
  const tlq = useTimeline(childTaskId);
  const childTl = tlq.data;

  // Close native-member drawer via Escape when not shadowed by higher-layer overlays.
  useEffect(() => {
    if (childTaskId || !nativeMember) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || e.isComposing) return;
      const s = useWorkbenchStore.getState();
      if (s.cmdkOpen || s.machinesOpen || s.review) return;
      e.preventDefault();
      onClose();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [childTaskId, nativeMember, onClose]);

  const childSummary = useMemo(
    () => (childTaskId ? childrenList.find((c) => c.taskId === childTaskId) : undefined),
    [childTaskId, childrenList],
  );

  const childArtifacts = useMemo(() => collectSessionArtifacts(childTl), [childTl]);

  const totalSwitchTargets = childrenList.length + nativeMembers.length;
  const switcherValue = childTaskId
    ? `child:${childTaskId}`
    : nativeMember
      ? `native:${nativeMember.memberName}`
      : '';

  const handleSwitch = (val: string) => {
    if (val.startsWith('child:')) {
      onSelectChild(val.slice(6));
    } else if (val.startsWith('native:')) {
      const name = val.slice(7);
      const found = nativeMembers.find((m) => m.memberName === name);
      if (found) onSelectNativeMember(found);
    }
  };

  // Native subagent / swarm member inspection view
  if (!childTaskId && nativeMember) {
    const mach = boot.machines.find((m) => m.machineId === nativeMember.machineId);
    const machName = mach ? mach.displayAlias?.trim() || cleanMachineName(mach) : nativeMember.machineId || '';
    const adpLabel = nativeMember.adapter ? ADAPTER_LABELS[nativeMember.adapter] ?? nativeMember.adapter : '';
    const kindLabel = nativeMember.sourceKind === 'native_swarm' ? 'Swarm' : '内置';
    const dotClass =
      nativeMember.state === 'working'
        ? 'run'
        : nativeMember.state === 'waiting'
          ? 'approval'
          : nativeMember.state === 'killed'
            ? 'bad'
            : 'idle';

    return (
      <div
        className="csd-dock"
        onClick={(e) => {
          if (e.target === e.currentTarget) onClose();
        }}
      >
        <aside className="csd-panel" aria-label={`子成员 @${nativeMember.memberName}`}>
          <div className="csd-hd">
            <div className="csd-hd-top">
              <div className="csd-title-wrap">
                <span className={`sub-dot is-${dotClass}`} aria-hidden="true" />
                <span className="sub-chip-native-tag">{kindLabel}</span>
                {nativeMember.role && <span className="csd-role-pill">{nativeMember.role}</span>}
                <span className="csd-title ell">@{nativeMember.memberName}</span>
              </div>
              <div className="csd-acts">
                {totalSwitchTargets > 1 && (
                  <select
                    className="csd-switch"
                    value={switcherValue}
                    aria-label="切换同级子会话"
                    title="切换同级子会话"
                    onChange={(e) => handleSwitch(e.target.value)}
                  >
                    {childrenList.length > 0 && (
                      <optgroup label="Orbit 子会话">
                        {childrenList.map((c) => (
                          <option key={c.taskId} value={`child:${c.taskId}`}>
                            {c.delegatedRole ? `[${c.delegatedRole}] ` : ''}
                            {taskTitle(c)}
                          </option>
                        ))}
                      </optgroup>
                    )}
                    {nativeMembers.length > 0 && (
                      <optgroup label="底层内置成员">
                        {nativeMembers.map((m) => (
                          <option key={m.memberName} value={`native:${m.memberName}`}>
                            @{m.memberName}
                            {m.role ? ` (${m.role})` : ''}
                          </option>
                        ))}
                      </optgroup>
                    )}
                  </select>
                )}
                <button type="button" className="ib" title="关闭抽屉 (Esc)" aria-label="关闭抽屉" onClick={onClose}>
                  <X size={14} strokeWidth={1.7} />
                </button>
              </div>
            </div>
            <div className="csd-meta-row">
              <div className="csd-meta-left">
                <span>{memberStateLabel(nativeMember.state)}</span>
                {machName && (
                  <>
                    <span className="topbar-meta-dot">·</span>
                    <span className="mono">{machName}</span>
                  </>
                )}
                {adpLabel && (
                  <>
                    <span className="topbar-meta-dot">·</span>
                    <span>{adpLabel}</span>
                  </>
                )}
                {nativeMember.externalSessionId && (
                  <>
                    <span className="topbar-meta-dot">·</span>
                    <span className="mono faint" title={nativeMember.externalSessionId}>
                      ext:{nativeMember.externalSessionId.slice(0, 12)}
                    </span>
                  </>
                )}
              </div>
            </div>
          </div>

          <div className="csd-body csd-native-body">
            <div className="csd-native-notice">
              <div className="csd-native-notice-hd">
                <Info size={13} strokeWidth={1.8} />
                <span>底层 Agent 内置 subagent</span>
              </div>
              <p className="csd-native-notice-p">
                内部流由原厂进程托管，当前仅展示投影记录与消息板上下文；若需实时打断或交互接管，请使用 Orbit 子会话。
              </p>
              {nativeMember.brief && (
                <div className="csd-native-brief">
                  <div className="csd-native-brief-lbl">派发指令</div>
                  <div className="csd-native-brief-txt">{nativeMember.brief}</div>
                </div>
              )}
            </div>
            <div className="csd-native-stream">
              <WorkerStream taskId={parentTaskId} worker={nativeMember.memberName} member={nativeMember} inDrawer />
            </div>
          </div>
        </aside>
      </div>
    );
  }

  if (!childTaskId) return null;

  const task = childTl?.task;
  const projectId = task?.projectId ?? childSummary?.projectId ?? '';
  const project = boot.projects.find((p) => p.projectId === projectId);
  const isVirtual = isVirtualProject(projectId);
  const isCrossProject = !!parentProjectId && !!projectId && projectId !== parentProjectId && !isVirtual;

  const formalDeliveries = (childTl?.deliveries ?? []).filter((d) => !d.lateArrival);
  const codeDeliveries = (childTl?.deliveries ?? []).filter((d) => CODE_KINDS.has(d.kind));
  const formalCodeDeliveries = formalDeliveries.filter((d) => CODE_KINDS.has(d.kind) && d.manifest.filesChanged > 0);
  const latestCode =
    formalCodeDeliveries.find((d) => d.deliveryId === task?.latestDeliveryId) ??
    formalCodeDeliveries[formalCodeDeliveries.length - 1] ??
    codeDeliveries[codeDeliveries.length - 1];

  const hasCodeDiff =
    !!latestCode &&
    (latestCode.manifest.insertions > 0 || latestCode.manifest.deletions > 0 || latestCode.manifest.filesChanged > 0);
  const openDiffComments = (childTl?.annotations ?? []).filter((a) => a.status === 'open' && a.source !== 'ryugi').length;

  const lastAttempt = childTl?.attempts[childTl.attempts.length - 1];
  const machineId = latestCode?.sourceMachineId || lastAttempt?.machineId || childSummary?.machineId || '';
  const mach = boot.machines.find((m) => m.machineId === machineId);
  const machName = mach ? mach.displayAlias?.trim() || cleanMachineName(mach) : machineId;
  const adapter = lastAttempt?.adapter || task?.preferredAdapter || childSummary?.preferredAdapter;
  const adapterLabel = adapter ? ADAPTER_LABELS[adapter] ?? adapter : '';
  const modelName = childTl?.runtimeConfig?.model || childSummary?.model || '';

  const isPlainFolderTask =
    lastAttempt?.isolation === 'in_place' ||
    (!!lastAttempt?.partId && project?.parts?.find((p) => p.partId === lastAttempt.partId)?.kind === 'folder') ||
    (!!project?.repoIdentity && project.repoIdentity.startsWith('folder:'));
  const isolationLabel =
    !isVirtual && project
      ? lastAttempt?.isolation === 'worktree'
        ? 'Worktree'
        : !isPlainFolderTask
          ? '目录同步'
          : ''
      : '';
  const gitMeta = !isVirtual && !isPlainFolderTask && latestCode?.headSha ? shortSha(latestCode.headSha) : '';

  const state = task?.state ?? childSummary?.state ?? 'queued';
  const openApprovals =
    (childTl?.approvals.filter((a) => a.state === 'pending').length ?? childSummary?.openApprovalCount) || 0;
  const dot = childStateDot(state, openApprovals);
  const role = task?.delegatedRole || childSummary?.delegatedRole || '';
  const title = task ? taskTitle(task) : childSummary ? taskTitle(childSummary) : childTaskId;
  const isMismatchedParent = !!childTl && childTl.task.parentTaskId !== parentTaskId;

  return (
    <div
      className="csd-dock"
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <aside className="csd-panel" aria-label={`子会话 ${title}`}>
        <div className="csd-hd">
          <div className="csd-hd-top">
            <div className="csd-title-wrap">
              <span className={`sub-dot is-${dot}`} aria-hidden="true" title={childStateLabel(state, openApprovals)} />
              {role && <span className="csd-role-pill">{role}</span>}
              <span className="csd-title ell" title={task?.goal || title}>
                {title}
              </span>
            </div>
            <div className="csd-acts">
              {totalSwitchTargets > 1 && (
                <select
                  className="csd-switch"
                  value={switcherValue}
                  aria-label="切换同级子会话"
                  title="切换同级子会话"
                  onChange={(e) => handleSwitch(e.target.value)}
                >
                  {childrenList.length > 0 && (
                    <optgroup label="Orbit 子会话">
                      {childrenList.map((c) => (
                        <option key={c.taskId} value={`child:${c.taskId}`}>
                          {c.delegatedRole ? `[${c.delegatedRole}] ` : ''}
                          {taskTitle(c)}
                        </option>
                      ))}
                    </optgroup>
                  )}
                  {nativeMembers.length > 0 && (
                    <optgroup label="底层内置成员">
                      {nativeMembers.map((m) => (
                        <option key={m.memberName} value={`native:${m.memberName}`}>
                          @{m.memberName}
                          {m.role ? ` (${m.role})` : ''}
                        </option>
                      ))}
                    </optgroup>
                  )}
                </select>
              )}
              <button
                type="button"
                className="csd-expand-btn"
                title="放大进入完整子会话页"
                onClick={() => navigate(paths.session(childTaskId))}
              >
                <span className="no-mobile">放大</span>
                <ArrowUpRight size={13} strokeWidth={1.8} />
              </button>
              <button type="button" className="ib" title="关闭抽屉 (Esc)" aria-label="关闭抽屉" onClick={onClose}>
                <X size={14} strokeWidth={1.7} />
              </button>
            </div>
          </div>

          <div className="csd-meta-row">
            <div className="csd-meta-left">
              {!isVirtual && (project?.displayName || childSummary?.projectName) && (
                <button
                  type="button"
                  className={`csd-proj-tag ${isCrossProject ? 'is-cross' : ''}`}
                  title={isCrossProject ? '跨项目子会话，点击进入该项目' : '进入所属项目'}
                  onClick={() => projectId && navigate(paths.project(projectId))}
                >
                  {project?.displayName || childSummary?.projectName}
                </button>
              )}
              {machName && <span className="mono">{machName}</span>}
              {adapterLabel && (
                <>
                  {machName && <span className="topbar-meta-dot">·</span>}
                  <span>{adapterLabel}</span>
                </>
              )}
              {modelName && (
                <>
                  <span className="topbar-meta-dot">·</span>
                  <span className="mono ell" style={{ maxWidth: 140 }} title={modelName}>
                    {modelName}
                  </span>
                </>
              )}
              {isolationLabel && (
                <>
                  <span className="topbar-meta-dot">·</span>
                  <span>{isolationLabel}</span>
                </>
              )}
              {gitMeta && (
                <>
                  <span className="topbar-meta-dot">·</span>
                  <span className="mono">{gitMeta}</span>
                </>
              )}
            </div>

            <div className="csd-meta-acts">
              {!isMismatchedParent && project && !isVirtual && latestCode && (
                <div className="no-mobile">
                  <IdeOpenButton project={project} delivery={latestCode} machines={boot.machines} />
                </div>
              )}
              {!isMismatchedParent && childArtifacts.length > 0 && (
                <button
                  type="button"
                  className="csd-mini-btn"
                  title="查看子会话 Artifacts"
                  onClick={() =>
                    openReview('art', childTaskId, childArtifacts[0].deliveryId, childArtifacts[0].artifactId)
                  }
                >
                  <Layers size={12} strokeWidth={1.7} />
                  <span className="mono">{childArtifacts.length}</span>
                </button>
              )}
              {!isMismatchedParent && latestCode && (
                <button
                  type="button"
                  className={`csd-mini-btn ${hasCodeDiff ? '' : 'is-clean'}`}
                  title="打开 Source Review"
                  onClick={() => openReview('code', childTaskId, latestCode.deliveryId)}
                >
                  {hasCodeDiff ? (
                    <>
                      <DiffIcon size={12} />
                      <span className="add mono">+{latestCode.manifest.insertions}</span>
                      <span className="del mono">−{latestCode.manifest.deletions}</span>
                      {openDiffComments > 0 && (
                        <span className="comment-count-badge">
                          <MessageSquare size={10} strokeWidth={1.8} />
                          {openDiffComments}
                        </span>
                      )}
                    </>
                  ) : (
                    <>
                      <Check size={11} strokeWidth={2} />
                      <span>Checkpoint</span>
                    </>
                  )}
                </button>
              )}
            </div>
          </div>
        </div>

        <div className="csd-body">
          {tlq.isError ? (
            <div className="empty">载入子会话失败：{errMsg(tlq.error)}</div>
          ) : !childTl ? (
            <div className="empty">载入子会话…</div>
          ) : isMismatchedParent ? (
            <div className="empty" style={{ display: 'flex', flexDirection: 'column', gap: 12, alignItems: 'center', padding: 28 }}>
              <div style={{ fontWeight: 500 }}>该会话不属于当前父任务的子会话</div>
              <div style={{ fontSize: 12, opacity: 0.72, textAlign: 'center', maxWidth: 320 }}>
                URL 中的会话 <span className="mono">{childTaskId}</span> 与当前父会话不匹配，为避免误操作已禁止在此抽屉中直接操控。
              </div>
              <div style={{ display: 'flex', gap: 8, marginTop: 4 }}>
                <button
                  type="button"
                  className="btn pri"
                  onClick={() => navigate(paths.session(childTaskId))}
                >
                  打开该独立会话 ↗
                </button>
                <button type="button" className="btn" onClick={onClose}>
                  关闭抽屉
                </button>
              </div>
            </div>
          ) : (
            <SessionConversation
              key={childTaskId}
              taskId={childTaskId}
              tl={childTl}
              boot={boot}
              inDrawer
              onOpenChild={(nestedChildId) => navigate(paths.session(childTaskId, undefined, nestedChildId))}
              onOpenReview={() => {
                if (latestCode) openReview('code', childTaskId, latestCode.deliveryId);
                else if (childArtifacts[0]) {
                  openReview('art', childTaskId, childArtifacts[0].deliveryId, childArtifacts[0].artifactId);
                }
              }}
            />
          )}
        </div>
      </aside>
    </div>
  );
}
