import { useEffect, useMemo, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import {
  ArrowUpRight,
  ChevronDown,
  ChevronRight,
  CornerDownRight,
  FileSpreadsheet,
  FileText,
  Layers,
  Link2,
  PanelRightOpen,
  Presentation,
  Square,
  Table,
} from 'lucide-react';
import type { AttemptView, ChildTaskView, MachineView, TaskTimelineResponse } from '../generated/protocol';
import { ApprovalNotice } from '../components/ApprovalNotice';
import { FolderChangesCard } from '../components/FolderChangesCard';
import { CaretIcon } from '../components/Icons';
import { useAttemptRaw } from '../hooks/queries';
import { ADAPTER_LABELS } from '../lib/agentCaps';
import { api, errMsg } from '../lib/api';
import { deliveryInlineImages, deliveryReusableArtifacts } from '../lib/artifacts';
import {
  PA_PROJECT_ID,
  SCRATCH_PROJECT_ID,
  artifactShortLabel,
  attemptStateLabel,
  basename,
  childStateDot,
  childStateLabel,
  cleanMachineName,
  hhmm,
  isAttemptLive,
  isChildStateLive,
  shortSha,
  taskTitle,
  triggerLabel,
} from '../lib/format';
import { navigate, paths } from '../lib/router';
import { buildSegments, lastLiveStep } from '../lib/segments';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';
import { Segments } from './Segments';

const CODE_KINDS = new Set(['code_diff', 'mixed', 'workspace_snapshot']);

function AuthedInlineImage({ sha256, name }: { sha256: string; name: string }) {
  const [src, setSrc] = useState<string>('');
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let active = true;
    let objUrl = '';
    setFailed(false);
    void api
      .fetchBlob(sha256)
      .then((blob) => {
        if (!active) return;
        objUrl = URL.createObjectURL(blob);
        setSrc(objUrl);
      })
      .catch(() => {
        if (active) setFailed(true);
      });
    return () => {
      active = false;
      if (objUrl) URL.revokeObjectURL(objUrl);
    };
  }, [sha256]);

  return (
    <figure className="inline-media-box">
      {src ? (
        <img src={src} alt={name} className="inline-media-img" loading="lazy" />
      ) : (
        <div className="muted" style={{ padding: '12px 14px', fontSize: 12 }}>
          {failed ? '图片加载失败' : '载入图片…'}
        </div>
      )}
      <figcaption className="inline-media-cap">
        <span className="mono">{name}</span>
        <button
          type="button"
          className="lk"
          style={{ background: 'none', border: 0, padding: 0, cursor: 'pointer' }}
          onClick={() => {
            if (src) {
              window.open(src, '_blank', 'noopener,noreferrer');
            } else {
              void api.downloadBlob(sha256, name).catch((e) => toast(errMsg(e), 'err'));
            }
          }}
        >
          原图
        </button>
      </figcaption>
    </figure>
  );
}

function ArtifactGlyph({ kind }: { kind: string }) {
  switch (kind) {
    case 'slides':
      return <Presentation size={14} strokeWidth={1.6} />;
    case 'sheet':
      return <FileSpreadsheet size={14} strokeWidth={1.6} />;
    case 'design':
      return <Layers size={14} strokeWidth={1.6} />;
    case 'word':
    case 'pdf':
      return <Table size={14} strokeWidth={1.6} />;
    default:
      return <FileText size={14} strokeWidth={1.6} />;
  }
}

function usePromptFor(attempt: AttemptView, tl: TaskTimelineResponse): { text: string; placeholder: boolean; detail: string[] } {
  const cache = useWorkbenchStore((s) => s.promptCache);
  return useMemo(() => {
    const task = tl.task;
    const serverPrompt = attempt.promptText?.trim() ?? '';
    switch (attempt.triggerKind) {
      case 'initial':
      case 'workspace_import':
        return { text: serverPrompt || task.goal, placeholder: false, detail: [] };
      case 'annotation_batch': {
        const anns = tl.annotations.filter((a) => a.triggeredAttemptId === attempt.attemptId);
        return {
          text: serverPrompt || `Review 批注 · ${anns.length || '若干'} 条`,
          placeholder: false,
          detail: anns.map((a) => {
            const lineRange = a.anchor.startLine
              ? a.anchor.endLine && a.anchor.endLine !== a.anchor.startLine
                ? `:${a.anchor.startLine}-${a.anchor.endLine}`
                : `:${a.anchor.startLine}`
              : '';
            const loc = a.anchor.path ? `${a.anchor.path}${lineRange}` : a.anchor.artifact || a.source;
            return `${loc} — ${a.body}`;
          }),
        };
      }
      case 'followup': {
        if (serverPrompt) return { text: serverPrompt, placeholder: false, detail: [] };
        const direct = cache[`att:${attempt.attemptId}`];
        if (direct) return { text: direct, placeholder: false, detail: [] };
        const ev = tl.events.find(
          (e) => e.kind === 'queue.updated' && e.payload?.action === 'dispatched' && e.payload?.dispatchedAttemptId === attempt.attemptId,
        );
        const inputId = typeof ev?.payload?.inputId === 'string' ? ev.payload.inputId : '';
        const viaQueue = inputId ? cache[`inp:${inputId}`] : '';
        if (viaQueue) return { text: viaQueue, placeholder: false, detail: [] };
        return { text: '追问', placeholder: true, detail: [] };
      }
      case 'retry':
        return { text: serverPrompt || `重试 · ${task.goal.split('\n')[0]}`, placeholder: !serverPrompt, detail: [] };
      case 'manual_import': {
        const d = tl.deliveries.find((x) => x.deliveryId === attempt.outputDeliveryId);
        return { text: `导入交付${d?.summary ? ` · ${d.summary}` : ''}`, placeholder: true, detail: [] };
      }
      default:
        return { text: triggerLabel(attempt.triggerKind), placeholder: true, detail: [] };
    }
  }, [attempt, tl, cache]);
}

function RetryNotice({ attempt, taskId, needsVerify }: { attempt: AttemptView; taskId: string; needsVerify: boolean }) {
  const qc = useQueryClient();
  const [verified, setVerified] = useState(false);
  const retry = useMutation({
    mutationFn: () =>
      api.retryTask(taskId, {
        verifiedSite: needsVerify ? verified : undefined,
        verifiedAttemptId: needsVerify ? attempt.attemptId : undefined,
      }),
    onSuccess: () => {
      toast('已发起重试');
      void qc.invalidateQueries({ queryKey: ['task-timeline', taskId] });
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });
  return (
    <div className={`it nt ${needsVerify ? '' : 'bad'}`}>
      <div className="nt-h">
        <span className="lbl" title={attempt.errorCode || undefined}>
          {needsVerify ? '上一轮中途断了' : attemptStateLabel(attempt.state)}
        </span>
      </div>
      <div className="nt-b">
        {needsVerify
          ? '工作区可能有没提交的改动，看一眼再继续。'
          : attempt.errorDetail || '这一轮没能完成。'}
      </div>
      <div className="nt-a">
        {needsVerify && (
          <label className="chk">
            <input type="checkbox" checked={verified} onChange={(e) => setVerified(e.target.checked)} />
            看过了
          </label>
        )}
        <button type="button" className="btn pri" disabled={retry.isPending || (needsVerify && !verified)} onClick={() => retry.mutate()}>
          重试
        </button>
      </div>
    </div>
  );
}

function ChildResultSystemCard({
  attempt,
  onOpenChild,
}: {
  attempt: AttemptView;
  onOpenChild?: (childTaskId: string) => void;
}) {
  const [expanded, setExpanded] = useState(false);
  const cr = attempt.childResult;
  const rawPrompt = attempt.promptText?.trim() ?? '';
  const summaryText = cr?.summary?.trim() || cr?.errorDetail?.trim() || rawPrompt;
  const hasLongDetails = !!rawPrompt && (rawPrompt.length > 180 || rawPrompt.includes('\n') || rawPrompt !== summaryText);

  const stateTone =
    cr?.childState === 'succeeded'
      ? 'ok'
      : cr?.childState === 'failed' || cr?.childState === 'expired' || cr?.childState === 'cancelled'
        ? 'bad'
        : 'muted';

  const hasDiff =
    (cr?.filesChanged ?? 0) > 0 || (cr?.insertions ?? 0) > 0 || (cr?.deletions ?? 0) > 0;

  const openTarget = () => {
    if (!cr?.childTaskId) return;
    if (onOpenChild) onOpenChild(cr.childTaskId);
    else navigate(paths.session(cr.childTaskId));
  };

  return (
    <div className="it child-res-card">
      <div className="child-res-hd">
        <span className="child-res-k">
          <CornerDownRight size={12} strokeWidth={1.8} />
          <span>子会话回传</span>
        </span>
        {cr?.childRole && <span className="csd-role-pill">{cr.childRole}</span>}
        {cr?.childTaskId ? (
          <button
            type="button"
            className="child-res-title ell"
            title={`分屏查看子会话「${cr.childTitle}」`}
            onClick={openTarget}
          >
            {cr.childTitle || cr.childTaskId}
          </button>
        ) : (
          <span className="child-res-title ell">子会话已完成</span>
        )}
        {cr && <span className={`child-res-state is-${stateTone}`}>{attemptStateLabel(cr.childState)}</span>}
        {hasDiff && (
          <span className="child-res-diff mono">
            <span className="add">+{cr?.insertions ?? 0}</span> <span className="del">−{cr?.deletions ?? 0}</span>
          </span>
        )}
        <span className="child-res-time">{hhmm(attempt.startedAt)}</span>
      </div>

      {summaryText && !expanded && <div className="child-res-preview">{summaryText}</div>}
      {expanded && rawPrompt && <pre className="child-res-raw">{rawPrompt}</pre>}

      <div className="child-res-acts">
        {cr?.childTaskId && (
          <>
            <button type="button" className="child-act-btn" onClick={openTarget}>
              <PanelRightOpen size={11.5} strokeWidth={1.7} />
              <span>分屏查看</span>
            </button>
            <button
              type="button"
              className="child-act-btn"
              onClick={() => navigate(paths.session(cr.childTaskId))}
            >
              <span>完整页</span>
              <ArrowUpRight size={11.5} strokeWidth={1.7} />
            </button>
          </>
        )}
        {hasLongDetails && (
          <button type="button" className="child-act-btn faint" onClick={() => setExpanded((v) => !v)}>
            {expanded ? <ChevronDown size={11.5} strokeWidth={1.8} /> : <ChevronRight size={11.5} strokeWidth={1.8} />}
            <span>{expanded ? '收起回传原文' : '展开完整回传'}</span>
          </button>
        )}
      </div>
    </div>
  );
}

function InlineDispatchedChildren({
  parentTaskId,
  parentProjectId,
  children,
  machines,
  onOpenChild,
}: {
  parentTaskId: string;
  parentProjectId: string;
  children: ChildTaskView[];
  machines?: MachineView[];
  onOpenChild?: (childTaskId: string) => void;
}) {
  const qc = useQueryClient();
  const stopMut = useMutation({
    mutationFn: (childId: string) => api.cancelTask(childId, 'stopped from inline dispatch card'),
    onSuccess: (res, childId) => {
      toast(res.cancelSent || res.immediateState ? '已请求停止子会话' : '停止请求已记录');
      void qc.invalidateQueries({ queryKey: ['task-timeline', parentTaskId] });
      void qc.invalidateQueries({ queryKey: ['task-timeline', childId] });
      void qc.invalidateQueries({ queryKey: ['task-children', parentTaskId] });
      void qc.invalidateQueries({ queryKey: ['all-tasks'] });
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  if (children.length === 0) return null;

  return (
    <div className="it child-dispatch-list" aria-label="派发的子会话">
      {children.map((c) => {
        const live = isChildStateLive(c.state);
        const dot = childStateDot(c.state, c.openApprovalCount);
        const needsApproval = c.openApprovalCount > 0 || c.state === 'waiting_approval';
        const mach = machines?.find((m) => m.machineId === c.machineId);
        const machName = mach ? mach.displayAlias?.trim() || cleanMachineName(mach) : c.machineId || '';
        const adpLabel = c.preferredAdapter ? ADAPTER_LABELS[c.preferredAdapter] ?? c.preferredAdapter : '';
        const hasDiff = (c.filesChanged ?? 0) > 0 || (c.insertions ?? 0) > 0 || (c.deletions ?? 0) > 0;
        const preview =
          (live && c.currentStep?.trim()) ||
          c.errorDetail?.trim() ||
          c.lastMessagePreview?.trim() ||
          '';
        const title = taskTitle(c);
        const isCrossProject = c.projectId && c.projectId !== parentProjectId && c.projectName;

        const openSplit = () => {
          if (onOpenChild) onOpenChild(c.taskId);
          else navigate(paths.session(parentTaskId, undefined, c.taskId));
        };

        return (
          <div key={c.taskId} className={`child-dcard ${needsApproval ? 'is-ap' : ''} ${live ? 'is-live' : ''}`}>
            <div className="child-dcard-hd">
              <span className={`sub-dot is-${dot}`} aria-hidden="true" />
              {c.delegatedRole && <span className="csd-role-pill">{c.delegatedRole}</span>}
              <button type="button" className="child-dcard-ttl ell" onClick={openSplit} title={c.goal || title}>
                {title}
              </button>
              <span className="child-dcard-state">{childStateLabel(c.state, c.openApprovalCount)}</span>
            </div>

            <div className="child-dcard-meta">
              {isCrossProject && <span className="child-dcard-proj">{c.projectName}</span>}
              {machName && <span className="mono">{machName}</span>}
              {adpLabel && (
                <>
                  {machName && <span className="topbar-meta-dot">·</span>}
                  <span>{adpLabel}</span>
                </>
              )}
              {hasDiff && (
                <>
                  <span className="topbar-meta-dot">·</span>
                  <span className="mono">
                    <span className="add">+{c.insertions ?? 0}</span> <span className="del">−{c.deletions ?? 0}</span>
                  </span>
                </>
              )}
              {needsApproval && (
                <span className="sub-chip-ap">
                  待审批{c.openApprovalCount > 1 ? ` ${c.openApprovalCount}` : ''}
                </span>
              )}
            </div>

            {preview && (
              <div className={`child-dcard-preview ${c.state === 'failed' ? 'is-err' : ''}`}>{preview}</div>
            )}

            <div className="child-dcard-acts">
              <button type="button" className="child-act-btn is-pri" onClick={openSplit}>
                <PanelRightOpen size={11.5} strokeWidth={1.7} />
                <span>分屏查看/接管</span>
              </button>
              <button type="button" className="child-act-btn" onClick={() => navigate(paths.session(c.taskId))}>
                <span>完整页</span>
                <ArrowUpRight size={11.5} strokeWidth={1.7} />
              </button>
              {live && (
                <button
                  type="button"
                  className="child-act-btn is-stop"
                  disabled={stopMut.isPending && stopMut.variables === c.taskId}
                  onClick={() => stopMut.mutate(c.taskId)}
                >
                  <Square size={9} fill="currentColor" strokeWidth={0} />
                  <span>停止</span>
                </button>
              )}
            </div>
          </div>
        );
      })}
    </div>
  );
}

export function Turn({
  attempt,
  index,
  isLast,
  tl,
  compact = false,
  childrenList,
  machines,
  onOpenChild,
}: {
  attempt: AttemptView;
  index: number;
  isLast: boolean;
  tl: TaskTimelineResponse;
  compact?: boolean;
  childrenList?: ChildTaskView[];
  machines?: MachineView[];
  onOpenChild?: (childTaskId: string) => void;
}) {
  const live = isAttemptLive(attempt.state);
  const [expanded, setExpanded] = useState(false);
  const loadRaw = expanded || isLast || live;
  const raw = useAttemptRaw(attempt.attemptId, { enabled: loadRaw, live });
  const segs = useMemo(() => buildSegments(raw.data ?? []), [raw.data]);
  const prompt = usePromptFor(attempt, tl);
  const openReview = useWorkbenchStore((s) => s.openReview);

  const approvals = tl.approvals.filter((a) => a.attemptId === attempt.attemptId).sort((a, b) => a.createdAt - b.createdAt);
  const deliveryIdx = attempt.outputDeliveryId ? tl.deliveries.findIndex((d) => d.deliveryId === attempt.outputDeliveryId) : -1;
  const delivery = deliveryIdx >= 0 ? tl.deliveries[deliveryIdx] : undefined;
  const taskIdle = !tl.task.activeAttemptId;
  const step = live ? lastLiveStep(segs) : null;
  const hasActiveToolStep = !!step && (step.status === 'in_progress' || step.status === 'pending');

  const inlineImages = delivery ? deliveryInlineImages(delivery) : [];
  const artifacts = delivery ? deliveryReusableArtifacts(delivery, tl.annotations) : [];
  const hasCodeDiff = !!delivery && CODE_KINDS.has(delivery.kind) && delivery.manifest.filesChanged > 0;
  const isChildResult = attempt.triggerKind === 'child_result' || !!attempt.childResult;
  const dispatchedChildren = useMemo(
    () => (childrenList ?? tl.children ?? []).filter((c) => c.delegatedByAttemptId === attempt.attemptId),
    [childrenList, tl.children, attempt.attemptId],
  );

  return (
    <div className="turn" id={`turn-${attempt.attemptId}`} data-turn={index}>
      {isChildResult ? (
        <ChildResultSystemCard attempt={attempt} onOpenChild={onOpenChild} />
      ) : (
        <div className="it u">
          {attempt.refTasks && attempt.refTasks.length > 0 && (
            <div className="turn-refs" aria-label="引用的对话">
              {attempt.refTasks.map((ref) => (
                <button
                  key={ref.taskId}
                  type="button"
                  className="turn-ref-pill"
                  title={ref.projectName ? `${ref.projectName} · ${ref.title}` : ref.title}
                  onClick={() => navigate(ref.projectId === PA_PROJECT_ID ? paths.pa(ref.taskId) : paths.session(ref.taskId))}
                >
                  <Link2 size={11} strokeWidth={1.7} />
                  <span className="turn-ref-proj">
                    {ref.projectName || (ref.projectId === SCRATCH_PROJECT_ID ? '对话' : ref.projectId === PA_PROJECT_ID ? 'Hebi8' : ref.projectId)}
                  </span>
                  <span className="ell">{ref.title || ref.taskId}</span>
                </button>
              ))}
            </div>
          )}
          <div className={`ub ${prompt.placeholder ? 'phd' : ''}`}>{prompt.text}</div>
          {prompt.detail.length > 0 && (
            <div className="udetail">
              {prompt.detail.slice(0, 8).map((d, i) => (
                <div key={i} className="ell">
                  {d}
                </div>
              ))}
              {prompt.detail.length > 8 && <div className="faint">… 另 {prompt.detail.length - 8} 条</div>}
            </div>
          )}
          <div className="umeta hover-meta">{hhmm(attempt.startedAt)}</div>
        </div>
      )}

      {attempt.state === 'waiting_resource' && (
        <div className="it nt">
          <div className="nt-h">
            <span className="lbl">等待资源</span>
          </div>
          <div className="nt-b">{tl.task.waitingReason || '目标资源暂不可用，就绪后自动继续。'}</div>
        </div>
      )}

      {loadRaw ? (
        raw.isLoading ? (
          <div className="it running">
            <span className="spin" /> 载入过程…
          </div>
        ) : (
          <Segments segs={segs} live={live} compact={compact} />
        )
      ) : (
        <div className="it">
          <button type="button" className="lazy-turn" onClick={() => setExpanded(true)}>
            <CaretIcon className="i" />
            展开历史过程
          </button>
        </div>
      )}

      {approvals.map((ap) => (
        <div className="it" key={ap.approvalId}>
          <ApprovalNotice approval={ap} />
        </div>
      ))}

      {dispatchedChildren.length > 0 && (
        <InlineDispatchedChildren
          parentTaskId={tl.task.taskId}
          parentProjectId={tl.task.projectId}
          children={dispatchedChildren}
          machines={machines}
          onOpenChild={onOpenChild}
        />
      )}

      {live && attempt.state !== 'waiting_approval' && attempt.state !== 'waiting_resource' && !hasActiveToolStep && (
        <div className="it running">
          <span className="spin" />
          <span className="shim ell">{attemptStateLabel(attempt.state)}</span>
        </div>
      )}

      {/* Conversation-inline images/diagrams render directly in flow (NOT forced into Artifacts) */}
      {inlineImages.length > 0 && (
        <div className="it inline-media-stack">
          {inlineImages.map((img) => (
            <AuthedInlineImage key={`${img.sha256}:${img.path}`} sha256={img.sha256} name={basename(img.path)} />
          ))}
        </div>
      )}

      {/* Reusable Artifacts shelf below the reply turn */}
      {!compact && artifacts.length > 0 && (
        <div className="it dlv-section">
          <div className="dlv-section-hd">
            <span className="dlv-section-title">Artifacts</span>
          </div>
          <div className="dlv-grid">
            {artifacts.map((art) => (
              <button
                type="button"
                key={art.artifactId}
                className="dlv-card"
                onClick={() => openReview('art', delivery!.taskId, delivery!.deliveryId, art.artifactId)}
              >
                <span className="dlv-ico">
                  <ArtifactGlyph kind={art.kind} />
                </span>
                <span className="dlv-info">
                  <span className="dlv-title">{art.title}</span>
                  <span className="dlv-meta mono">{art.locator}</span>
                </span>
                <span className="dlv-kind-tag">{artifactShortLabel(art.kind)}</span>
              </button>
            ))}
          </div>
        </div>
      )}

      {/* Plain-folder in-place changes card (no shadow Git) */}
      {!compact && attempt.folderChanges && attempt.folderChanges.files.length > 0 && (
        <FolderChangesCard
          taskId={tl.task.taskId}
          attemptId={attempt.attemptId}
          changes={attempt.folderChanges}
        />
      )}

      {/* Quiet Source Diff line when code changes were produced */}
      {delivery && hasCodeDiff && (
        <div className="it dline" title={shortSha(delivery.headSha) || undefined}>
          <span>代码改动</span>
          <span className="mono">
            <span className="add">+{delivery.manifest.insertions}</span> <span className="del">−{delivery.manifest.deletions}</span>
          </span>
          <button type="button" className="lk" onClick={() => openReview('code', delivery.taskId, delivery.deliveryId)}>
            Source Review
          </button>
        </div>
      )}

      {isLast && taskIdle && (attempt.state === 'needs_verification' || attempt.state === 'failed' || attempt.state === 'expired') && (
        <RetryNotice attempt={attempt} taskId={tl.task.taskId} needsVerify={attempt.state === 'needs_verification'} />
      )}

      {!live && attempt.state !== 'succeeded' && (
        <div className="it done-line">
          {attemptStateLabel(attempt.state)}
          {attempt.finishedAt ? ` · ${hhmm(attempt.finishedAt)}` : ''}
        </div>
      )}
    </div>
  );
}
