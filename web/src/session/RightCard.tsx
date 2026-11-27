import { useState, type ReactNode } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Brain, Check, ChevronDown, ChevronRight, FileText, Link2, MessageSquare, Plus, X } from 'lucide-react';
import type {
  BoardMemberCursor,
  ChildTaskView,
  InboxItemView,
  MachineView,
  ProjectView,
  TaskBoardResponse,
  TaskSourceKind,
  TaskTimelineResponse,
} from '../generated/protocol';
import { ArtIcon, DiffIcon, SwarmIcon } from '../components/Icons';
import { StatusMark } from '../components/StatusMark';
import { useDelivery } from '../hooks/queries';
import { api, errMsg } from '../lib/api';
import { collectSessionArtifacts } from '../lib/artifacts';
import {
  PA_PROJECT_ID,
  SCRATCH_PROJECT_ID,
  artifactShortLabel,
  childSessionMark,
  cleanMachineName,
  memberStateLabel,
  taskTitle,
  type SessionMark,
} from '../lib/format';
import { navigate, paths } from '../lib/router';
import { useImeGuard } from '../lib/ime';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';

const CODE_KINDS = new Set(['code_diff', 'mixed', 'workspace_snapshot']);
const COLLAPSED_STORAGE_KEY = 'orbit.rightCardCollapsed';

function readCollapsedMap(): Record<string, boolean> {
  try {
    const raw = localStorage.getItem(COLLAPSED_STORAGE_KEY);
    return raw ? (JSON.parse(raw) as Record<string, boolean>) : {};
  } catch {
    return {};
  }
}

function writeCollapsedMap(next: Record<string, boolean>) {
  try {
    localStorage.setItem(COLLAPSED_STORAGE_KEY, JSON.stringify(next));
  } catch {
    // ignore storage errors
  }
}

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

function SourceIcon({ kind }: { kind: TaskSourceKind }) {
  if (kind === 'link') return <Link2 size={12.5} strokeWidth={1.6} className="faint" />;
  if (kind === 'nmem') return <Brain size={12.5} strokeWidth={1.6} className="faint" />;
  return <FileText size={12.5} strokeWidth={1.6} className="faint" />;
}

function RightSection({
  secKey,
  title,
  count,
  collapsed,
  onToggle,
  action,
  children,
}: {
  secKey: string;
  title: string;
  count?: number;
  collapsed: boolean;
  onToggle: (key: string) => void;
  action?: ReactNode;
  children: ReactNode;
}) {
  return (
    <div className={`fsec ${collapsed ? 'is-collapsed' : ''}`}>
      <div className="fsec-hd-row">
        <button
          type="button"
          className="fsec-toggle"
          aria-expanded={!collapsed}
          onClick={() => onToggle(secKey)}
        >
          {collapsed ? (
            <ChevronRight size={12} strokeWidth={1.8} className="fsec-chev" aria-hidden="true" />
          ) : (
            <ChevronDown size={12} strokeWidth={1.8} className="fsec-chev" aria-hidden="true" />
          )}
          <span>{title}</span>
          {count !== undefined && <span className="fsec-cnt">{count}</span>}
        </button>
        {action}
      </div>
      {!collapsed && <div className="fsec-body">{children}</div>}
    </div>
  );
}

/**
 * Compact, collapsible right context panel (Orbit v4):
 * - Top section: Code Changes (`+N −M` / `Checkpoint` opening Source Review)
 * - Artifacts, Child Sessions (top-level only, max depth = 1), Workspace & Sources
 * - Every section header clicks cleanly to collapse/expand with `⌄ / ›`.
 */
export function RightCard({
  tl,
  project,
  machines,
  board,
  childrenList,
  inboxByTask,
  onOpenBoard,
  onOpenChild,
  onSelectMember,
}: {
  tl: TaskTimelineResponse;
  project?: ProjectView;
  machines: MachineView[];
  board?: TaskBoardResponse;
  childrenList?: ChildTaskView[];
  inboxByTask?: ReadonlyMap<string, InboxItemView[]>;
  onOpenBoard: () => void;
  onOpenChild?: (childTaskId: string) => void;
  onSelectMember?: (member: BoardMemberCursor) => void;
}) {
  const qc = useQueryClient();
  const openReview = useWorkbenchStore((s) => s.openReview);
  const [collapsedMap, setCollapsedMap] = useState<Record<string, boolean>>(readCollapsedMap);
  const [adding, setAdding] = useState(false);
  const [kind, setKind] = useState<TaskSourceKind>('link');
  const [label, setLabel] = useState('');
  const [uri, setUri] = useState('');
  const ime = useImeGuard();
  const taskId = tl.task.taskId;
  const isChildSession = !!tl.task.parentTaskId;
  const isScratch = tl.task.projectId === SCRATCH_PROJECT_ID;
  const isPA = tl.task.projectId === PA_PROJECT_ID;

  const toggleSec = (key: string) => {
    setCollapsedMap((prev) => {
      const next = { ...prev, [key]: !prev[key] };
      writeCollapsedMap(next);
      return next;
    });
  };

  const formalDeliveries = tl.deliveries.filter((d) => !d.lateArrival);
  const codeDeliveries = tl.deliveries.filter((d) => CODE_KINDS.has(d.kind));
  const formalCodeDeliveries = formalDeliveries.filter((d) => CODE_KINDS.has(d.kind) && d.manifest.filesChanged > 0);
  const latestCode =
    formalCodeDeliveries.find((d) => d.deliveryId === tl.task.latestDeliveryId) ??
    formalCodeDeliveries[formalCodeDeliveries.length - 1] ??
    codeDeliveries[codeDeliveries.length - 1];

  const deliveryQ = useDelivery(latestCode?.deliveryId);
  const isCheckpointed = deliveryQ.data?.reviewState === 'resolved';
  const hasCodeDiff =
    !!latestCode &&
    (latestCode.manifest.insertions > 0 || latestCode.manifest.deletions > 0 || latestCode.manifest.filesChanged > 0);
  const openDiffComments = tl.annotations.filter((a) => a.status === 'open' && a.source !== 'ryugi').length;

  const sessionArtifacts = collectSessionArtifacts(tl);
  const allMembers = board?.members ?? tl.boardMembers;
  const nativeMembers = allMembers.filter((m) => !m.workerTaskId);
  const effectiveChildren = isChildSession ? [] : (childrenList ?? tl.children ?? []);
  const totalSubs = effectiveChildren.length + nativeMembers.length;
  const boardMsgCount = board?.messages?.length ?? 0;
  const showSubsSection = !isChildSession && (totalSubs > 0 || tl.task.workerCount > 0 || boardMsgCount > 0);

  const lastAttempt = tl.attempts[tl.attempts.length - 1];
  const matchedPart = lastAttempt?.partId ? project?.parts?.find((p) => p.partId === lastAttempt.partId) : project?.parts?.[0];
  const matchedLoc = lastAttempt?.locationId
    ? matchedPart?.locations?.find((l) => l.locationId === lastAttempt.locationId)
    : matchedPart?.locations?.find((l) => l.isDefault) ?? matchedPart?.locations?.[0];
  const anchorRepo = project
    ? project.anchors[tl.task.requiredCapability || project.defaultCapability]?.repoPath
    : '';
  const workspacePath =
    lastAttempt?.worktreePath ||
    latestCode?.manifest?.worktreePath ||
    matchedLoc?.absPath ||
    anchorRepo ||
    (isScratch ? '自由对话' : isPA ? 'Hebi8' : '');

  const hostMachineId = latestCode?.sourceMachineId || lastAttempt?.machineId || matchedLoc?.machineId || '';
  const hostMachine = hostMachineId ? machines.find((m) => m.machineId === hostMachineId) : undefined;
  const hostLabel = hostMachine ? hostMachine.displayAlias?.trim() || cleanMachineName(hostMachine) : hostMachineId;

  const addSrc = useMutation({
    mutationFn: () => api.addSource(taskId, { kind, label: label.trim() || uri.trim(), uriOrRef: uri.trim() }),
    onSuccess: () => {
      setAdding(false);
      setLabel('');
      setUri('');
      void qc.invalidateQueries({ queryKey: ['task-timeline', taskId] });
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });
  const delSrc = useMutation({
    mutationFn: (sourceId: string) => api.deleteSource(taskId, sourceId),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['task-timeline', taskId] }),
    onError: (e) => toast(errMsg(e), 'err'),
  });

  return (
    <aside className="fcard" aria-label={isChildSession ? '子会话上下文浮卡' : '会话上下文'}>
      {latestCode && (
        <RightSection
          secKey="diff"
          title="代码变动"
          collapsed={!!collapsedMap.diff}
          onToggle={toggleSec}
        >
          <button
            type="button"
            className={`frow ${isCheckpointed || !hasCodeDiff ? 'is-clean' : ''}`}
            onClick={() => openReview('code', taskId, latestCode.deliveryId)}
            title={isCheckpointed ? '已标记 Checkpoint · 点击打开 Source Review' : '打开 Source Review'}
          >
            {isCheckpointed || !hasCodeDiff ? (
              <Check size={12.5} strokeWidth={2} className="ok-t" />
            ) : (
              <DiffIcon size={12.5} />
            )}
            <span className="ell grow">
              {isCheckpointed
                ? '已标记 Checkpoint'
                : hasCodeDiff
                  ? latestCode.manifest.filesChanged > 0
                    ? `${latestCode.manifest.filesChanged} 个文件`
                    : 'Source Review'
                  : 'Checkpoint'}
            </span>
            {openDiffComments > 0 && (
              <span className="comment-count-badge" title={`${openDiffComments} 条待发批注`}>
                <MessageSquare size={10} strokeWidth={1.8} />
                {openDiffComments}
              </span>
            )}
            {hasCodeDiff ? (
              <span className="fx mono">
                <span className="add">+{latestCode.manifest.insertions}</span>{' '}
                <span className="del">−{latestCode.manifest.deletions}</span>
              </span>
            ) : isCheckpointed ? (
              <span className="fx mono ok-t">✓</span>
            ) : null}
          </button>
        </RightSection>
      )}

      {sessionArtifacts.length > 0 && (
        <RightSection
          secKey="artifacts"
          title="Artifacts"
          count={sessionArtifacts.length}
          collapsed={!!collapsedMap.artifacts}
          onToggle={toggleSec}
        >
          {sessionArtifacts.map((art) => (
            <button
              type="button"
              key={art.artifactId}
              className="frow"
              onClick={() => openReview('art', taskId, art.deliveryId, art.artifactId)}
              title={art.locator}
            >
              <ArtIcon size={12.5} />
              <span className="ell grow">{art.title}</span>
              <span className="dlv-kind-tag">{artifactShortLabel(art.kind)}</span>
            </button>
          ))}
        </RightSection>
      )}

      {showSubsSection && (
        <RightSection
          secKey="subs"
          title="协作子会话"
          count={totalSubs || undefined}
          collapsed={!!collapsedMap.subs}
          onToggle={toggleSec}
        >
          {effectiveChildren.map((c) => {
            const mark = childSessionMark(c, inboxByTask?.get(c.taskId));
            return (
              <button
                type="button"
                key={c.taskId}
                className="frow"
                onClick={() => {
                  if (onOpenChild) onOpenChild(c.taskId);
                  else navigate(paths.session(taskId, undefined, c.taskId));
                }}
                title={c.goal || taskTitle(c)}
              >
                <span className="ell grow">{taskTitle(c)}</span>
                <StatusMark mark={mark} />
              </button>
            );
          })}
          {nativeMembers.map((m) => (
            <button
              type="button"
              key={`native:${m.memberName}`}
              className="frow"
              onClick={() => {
                if (onSelectMember) onSelectMember(m);
                else navigate(paths.session(taskId, m.memberName));
              }}
              title={`${memberStateLabel(m.state)}${m.currentStep ? ` · ${m.currentStep}` : ''}`}
            >
              <span className="ell grow">@{m.memberName}</span>
              <span className="faint" style={{ fontSize: 10.5, flex: 'none' }}>
                内置
              </span>
              <StatusMark mark={nativeMemberMark(m)} />
            </button>
          ))}
          {totalSubs === 0 && boardMsgCount === 0 && <div className="fsub">有协作子任务</div>}
          {(effectiveChildren.length > 0 || nativeMembers.length > 0 || boardMsgCount > 0) && (
            <button type="button" className="flink" onClick={onOpenBoard}>
              <SwarmIcon size={12} /> 消息板{boardMsgCount > 0 ? ` (${boardMsgCount})` : ''} →
            </button>
          )}
        </RightSection>
      )}

      <RightSection
        secKey="workspace"
        title="工作区与来源"
        count={tl.sources.length > 0 ? tl.sources.length : undefined}
        collapsed={!!collapsedMap.workspace}
        onToggle={toggleSec}
        action={
          <button
            type="button"
            className="ib fsec-add-btn"
            title="添加参考来源"
            aria-label="添加参考来源"
            onClick={() => {
              if (collapsedMap.workspace) toggleSec('workspace');
              setAdding((v) => !v);
            }}
          >
            <Plus size={12} strokeWidth={1.7} />
          </button>
        }
      >
        {workspacePath && (
          <div className="frow is-static" title={workspacePath}>
            <div className="fdir-meta">
              <span className="fdir-lbl">
                执行目录{hostLabel ? ` · ${hostLabel}` : ''}
              </span>
              <span className="fdir-val mono">{workspacePath}</span>
            </div>
          </div>
        )}

        {tl.sources.map((s) => (
          <div className="frow" key={s.sourceId} title={s.uriOrRef}>
            <SourceIcon kind={s.kind} />
            {s.kind === 'link' && /^https?:\/\//.test(s.uriOrRef) ? (
              <a className="ell grow" href={s.uriOrRef} target="_blank" rel="noopener noreferrer">
                {s.label}
              </a>
            ) : (
              <span className="ell grow">{s.label}</span>
            )}
            <button
              type="button"
              className="ib hov"
              title="移除"
              disabled={delSrc.isPending}
              onClick={() => delSrc.mutate(s.sourceId)}
            >
              <X size={12} strokeWidth={1.6} />
            </button>
          </div>
        ))}

        {adding ? (
          <form
            className="srcform"
            onSubmit={(e) => {
              e.preventDefault();
              if (uri.trim()) addSrc.mutate();
            }}
          >
            <select className="field" value={kind} onChange={(e) => setKind(e.target.value as TaskSourceKind)}>
              <option value="link">链接</option>
              <option value="file">文件</option>
              <option value="nmem">nmem 记忆</option>
            </select>
            <input
              className="field"
              placeholder="标题（可选）"
              value={label}
              onChange={(e) => setLabel(e.target.value)}
              {...ime.compositionProps}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && ime.isImeEnter(e)) e.preventDefault();
              }}
            />
            <input
              className="field"
              placeholder={kind === 'link' ? 'https://…' : kind === 'file' ? '仓库内路径' : 'nmem 引用'}
              value={uri}
              onChange={(e) => setUri(e.target.value)}
              {...ime.compositionProps}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && ime.isImeEnter(e)) e.preventDefault();
              }}
              autoFocus
            />
            <div className="row">
              <button type="button" className="btn ghost sm" onClick={() => setAdding(false)}>
                取消
              </button>
              <button type="submit" className="btn pri sm" disabled={!uri.trim() || addSrc.isPending}>
                添加
              </button>
            </div>
          </form>
        ) : (
          tl.sources.length === 0 && (
            <button type="button" className="flink faint" onClick={() => setAdding(true)}>
              <Plus size={11.5} strokeWidth={1.6} /> 添加参考来源
            </button>
          )
        )}
      </RightSection>
    </aside>
  );
}
