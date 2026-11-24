import { useMemo, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { FileSpreadsheet, FileText, Layers, Presentation, Table } from 'lucide-react';
import type { AttemptView, TaskTimelineResponse } from '../generated/protocol';
import { ApprovalNotice } from '../components/ApprovalNotice';
import { CaretIcon } from '../components/Icons';
import { useAttemptRaw } from '../hooks/queries';
import { api, errMsg } from '../lib/api';
import { deliveryInlineImages, deliveryReusableArtifacts } from '../lib/artifacts';
import { artifactShortLabel, attemptStateLabel, basename, hhmm, isAttemptLive, shortSha, triggerLabel } from '../lib/format';
import { buildSegments, lastLiveStep } from '../lib/segments';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';
import { Segments } from './Segments';

const CODE_KINDS = new Set(['code_diff', 'mixed', 'workspace_snapshot']);

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

export function Turn({
  attempt,
  index,
  isLast,
  tl,
  compact = false,
}: {
  attempt: AttemptView;
  index: number;
  isLast: boolean;
  tl: TaskTimelineResponse;
  compact?: boolean;
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

  return (
    <div className="turn" id={`turn-${attempt.attemptId}`} data-turn={index}>
      <div className="it u">
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
            <figure className="inline-media-box" key={`${img.sha256}:${img.path}`}>
              <img src={api.blobUrl(img.sha256)} alt={basename(img.path)} className="inline-media-img" loading="lazy" />
              <figcaption className="inline-media-cap">
                <span className="mono">{basename(img.path)}</span>
                <a href={api.blobUrl(img.sha256)} target="_blank" rel="noopener noreferrer" className="lk">
                  原图
                </a>
              </figcaption>
            </figure>
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
