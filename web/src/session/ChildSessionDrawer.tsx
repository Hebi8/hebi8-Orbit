import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { Check, ChevronDown, Maximize2, X } from 'lucide-react';
import type { BoardMemberCursor, BootstrapResponse, ChildTaskView, InboxItemView } from '../generated/protocol';
import { StatusMark } from '../components/StatusMark';
import { useTimeline } from '../hooks/queries';
import { ADAPTER_LABELS } from '../lib/agentCaps';
import { errMsg } from '../lib/api';
import { collectSessionArtifacts } from '../lib/artifacts';
import {
  childSessionMark,
  cleanMachineName,
  indexInboxByTask,
  taskTitle,
  type SessionMark,
} from '../lib/format';
import { useAnchoredPopover } from '../lib/popover';
import { navigate, paths } from '../lib/router';
import { useWorkbenchStore } from '../store/useWorkbenchStore';
import { SessionConversation } from './SessionConversation';
import { WorkerStream } from './WorkerStream';

const CODE_KINDS = new Set(['code_diff', 'mixed', 'workspace_snapshot']);

function nativeMemberMark(m: BoardMemberCursor): SessionMark {
  switch (m.state) {
    case 'working':
      return 'running';
    case 'waiting':
      return 'needs-you';
    case 'killed':
      return 'failed';
    default:
      return null;
  }
}

/**
 * Custom-drawn sibling sub-session switcher (NEVER native `<select>`).
 * Uses viewport-clamped portal positioning so it never clips inside narrow split drawers.
 */
function SubsessionSwitcher({
  activeTitle,
  activeMark,
  activeChildId,
  activeNativeName,
  childrenList,
  nativeMembers,
  boot,
  inboxByTask,
  onSelectChild,
  onSelectNativeMember,
}: {
  activeTitle: string;
  activeMark: SessionMark;
  activeChildId?: string;
  activeNativeName?: string;
  childrenList: ChildTaskView[];
  nativeMembers: BoardMemberCursor[];
  boot: BootstrapResponse;
  inboxByTask: ReadonlyMap<string, InboxItemView[]>;
  onSelectChild: (childTaskId: string) => void;
  onSelectNativeMember: (member: BoardMemberCursor) => void;
}) {
  const [open, setOpen] = useState(false);
  const btnRef = useRef<HTMLButtonElement | null>(null);
  const popRef = useRef<HTMLDivElement | null>(null);
  const close = useCallback(() => setOpen(false), []);

  const totalTargets = childrenList.length + nativeMembers.length;
  const canSwitch = totalTargets > 1;

  const popStyle = useAnchoredPopover({
    open: open && canSwitch,
    anchorRef: btnRef,
    popRef,
    width: 292,
    align: 'left',
    placement: 'down',
    gap: 6,
    onClose: close,
  });

  useEffect(() => {
    setOpen(false);
  }, [activeChildId, activeNativeName]);

  const machineNameOf = (machineId?: string) => {
    if (!machineId) return '';
    const m = boot.machines.find((x) => x.machineId === machineId);
    return m ? m.displayAlias?.trim() || cleanMachineName(m) : machineId;
  };

  return (
    <div className="sub-switcher-wrap">
      <button
        ref={btnRef}
        type="button"
        className={`sub-switcher-btn ${open ? 'open' : ''} ${!canSwitch ? 'is-single' : ''}`}
        title={canSwitch ? `切换同级子会话 · ${activeTitle}` : activeTitle}
        aria-haspopup={canSwitch ? 'menu' : undefined}
        aria-expanded={canSwitch ? open : undefined}
        onClick={() => canSwitch && setOpen((v) => !v)}
      >
        <StatusMark mark={activeMark} />
        <span className="sub-switcher-ttl">{activeTitle}</span>
        {canSwitch && <ChevronDown size={12} strokeWidth={1.7} className="faint" aria-hidden="true" />}
      </button>

      {open &&
        canSwitch &&
        popStyle &&
        createPortal(
          <div ref={popRef} className="sub-switcher-menu" style={popStyle} role="menu" aria-label="切换同级子会话">
            <div className="ov-menu-sec">切换同级子会话</div>
            {childrenList.map((c) => {
              const isCurrent = !!activeChildId && activeChildId === c.taskId;
              const mark = childSessionMark(c, inboxByTask.get(c.taskId));
              const machName = machineNameOf(c.machineId);
              const adpLabel = c.preferredAdapter ? ADAPTER_LABELS[c.preferredAdapter] ?? c.preferredAdapter : '';
              const meta = [machName, adpLabel].filter(Boolean).join(' · ');
              return (
                <button
                  type="button"
                  role="menuitemradio"
                  aria-checked={isCurrent}
                  key={c.taskId}
                  className={`sub-menu-row ${isCurrent ? 'active' : ''}`}
                  onClick={() => {
                    close();
                    if (!isCurrent) onSelectChild(c.taskId);
                  }}
                >
                  <StatusMark mark={mark} reserveSlot />
                  <div className="sub-menu-info">
                    <div className="sub-menu-ttl">{taskTitle(c)}</div>
                    {meta && <div className="sub-menu-meta">{meta}</div>}
                  </div>
                  {isCurrent && <Check size={12} strokeWidth={2} className="sub-menu-chk" />}
                </button>
              );
            })}

            {nativeMembers.length > 0 && (
              <>
                <div className="ov-menu-sec">底层成员</div>
                {nativeMembers.map((m) => {
                  const isCurrent = !activeChildId && activeNativeName === m.memberName;
                  const mark = nativeMemberMark(m);
                  const machName = machineNameOf(m.machineId);
                  const adpLabel = m.adapter ? ADAPTER_LABELS[m.adapter] ?? m.adapter : '';
                  const meta = [machName, adpLabel, '内置'].filter(Boolean).join(' · ');
                  return (
                    <button
                      type="button"
                      role="menuitemradio"
                      aria-checked={isCurrent}
                      key={`native:${m.memberName}`}
                      className={`sub-menu-row ${isCurrent ? 'active' : ''}`}
                      onClick={() => {
                        close();
                        if (!isCurrent) onSelectNativeMember(m);
                      }}
                    >
                      <StatusMark mark={mark} reserveSlot />
                      <div className="sub-menu-info">
                        <div className="sub-menu-ttl">@{m.memberName}</div>
                        <div className="sub-menu-meta">{meta}</div>
                      </div>
                      {isCurrent && <Check size={12} strokeWidth={2} className="sub-menu-chk" />}
                    </button>
                  );
                })}
              </>
            )}
          </div>,
          document.body,
        )}
    </div>
  );
}

/**
 * Right-side non-blocking resizable split sub-session drawer (Orbit v4):
 * - Single-line 40px header: custom title dropdown switcher (left) + Maximize2 & Close icons (right)
 * - Reuses the exact same `SessionConversation` & `Composer` at equal dock height (`Δtop=0, Δheight=0`)
 * - Preserves state cleanly when switching to/from Source Review overlay
 */
export function ChildSessionDrawer({
  boot,
  parentTaskId,
  childTaskId,
  nativeMember,
  childrenList,
  nativeMembers = [],
  width = 468,
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
  width?: number;
  onSelectChild: (childTaskId: string) => void;
  onSelectNativeMember: (member: BoardMemberCursor) => void;
  onClose: () => void;
}) {
  const openReview = useWorkbenchStore((s) => s.openReview);
  const tlq = useTimeline(childTaskId);
  const childTl = tlq.data;
  const inboxByTask = useMemo(() => indexInboxByTask(boot.openInboxItems), [boot.openInboxItems]);

  // Close native-member drawer via Escape when not shadowed by higher-layer overlays.
  useEffect(() => {
    if (childTaskId || !nativeMember) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || e.isComposing || e.defaultPrevented) return;
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

  // Native subagent / swarm member read-only inspection view
  if (!childTaskId && nativeMember) {
    const mach = boot.machines.find((m) => m.machineId === nativeMember.machineId);
    const machName = mach ? mach.displayAlias?.trim() || cleanMachineName(mach) : nativeMember.machineId || '';
    const adpLabel = nativeMember.adapter ? ADAPTER_LABELS[nativeMember.adapter] ?? nativeMember.adapter : '';

    return (
      <aside className="sub-drawer" style={{ width }} aria-label={`内置成员 @${nativeMember.memberName}`}>
        <div className="sub-hd">
          <SubsessionSwitcher
            activeTitle={`@${nativeMember.memberName}`}
            activeMark={nativeMemberMark(nativeMember)}
            activeNativeName={nativeMember.memberName}
            childrenList={childrenList}
            nativeMembers={nativeMembers}
            boot={boot}
            inboxByTask={inboxByTask}
            onSelectChild={onSelectChild}
            onSelectNativeMember={onSelectNativeMember}
          />
          <div className="sub-hd-acts">
            <button
              type="button"
              className="sub-hd-icon-btn"
              title="关闭分屏 (Esc)"
              aria-label="关闭分屏"
              onClick={onClose}
            >
              <X size={14} strokeWidth={1.7} />
            </button>
          </div>
        </div>

        <div className="csd-body csd-native-body">
          <div className="sub-native-strip">
            底层 {adpLabel || 'Agent'} 内置成员{machName ? `（${machName}）` : ''} · 只读投影记录，如需实时打断或交互接管请使用 Orbit 子会话。
          </div>
          {nativeMember.brief && (
            <div className="it u" style={{ marginBottom: 4 }}>
              <div className="ub" style={{ maxWidth: '92%', fontSize: 12.5 }}>
                {nativeMember.brief}
              </div>
            </div>
          )}
          <div className="csd-native-stream">
            <WorkerStream taskId={parentTaskId} worker={nativeMember.memberName} member={nativeMember} inDrawer />
          </div>
        </div>
      </aside>
    );
  }

  if (!childTaskId) return null;

  const task = childTl?.task;
  const formalDeliveries = (childTl?.deliveries ?? []).filter((d) => !d.lateArrival);
  const codeDeliveries = (childTl?.deliveries ?? []).filter((d) => CODE_KINDS.has(d.kind));
  const formalCodeDeliveries = formalDeliveries.filter((d) => CODE_KINDS.has(d.kind) && d.manifest.filesChanged > 0);
  const latestCode =
    formalCodeDeliveries.find((d) => d.deliveryId === task?.latestDeliveryId) ??
    formalCodeDeliveries[formalCodeDeliveries.length - 1] ??
    codeDeliveries[codeDeliveries.length - 1];

  const openApprovals =
    (childTl?.approvals.filter((a) => a.state === 'pending').length ?? childSummary?.openApprovalCount) || 0;
  const mark = childSessionMark(
    {
      state: task?.state ?? childSummary?.state ?? 'queued',
      openApprovalCount: openApprovals,
    },
    inboxByTask.get(childTaskId),
  );
  const title = task ? taskTitle(task) : childSummary ? taskTitle(childSummary) : childTaskId;
  const isMismatchedParent = !!childTl && childTl.task.parentTaskId !== parentTaskId;

  return (
    <aside className="sub-drawer" style={{ width }} aria-label={`子会话 ${title}`}>
      <div className="sub-hd">
        <SubsessionSwitcher
          activeTitle={title}
          activeMark={mark}
          activeChildId={childTaskId}
          childrenList={childrenList}
          nativeMembers={nativeMembers}
          boot={boot}
          inboxByTask={inboxByTask}
          onSelectChild={onSelectChild}
          onSelectNativeMember={onSelectNativeMember}
        />

        <div className="sub-hd-acts">
          {!isMismatchedParent && (
            <button
              type="button"
              className="sub-hd-icon-btn"
              title="放大进入完整子会话页"
              aria-label="放大进入完整子会话页"
              onClick={() => navigate(paths.session(childTaskId))}
            >
              <Maximize2 size={13.5} strokeWidth={1.7} />
            </button>
          )}
          <button
            type="button"
            className="sub-hd-icon-btn"
            title="关闭分屏 (Esc)"
            aria-label="关闭分屏"
            onClick={onClose}
          >
            <X size={14} strokeWidth={1.7} />
          </button>
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
              <button type="button" className="btn pri" onClick={() => navigate(paths.session(childTaskId))}>
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
            siblingChildren={childrenList}
            onOpenChild={onSelectChild}
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
  );
}
