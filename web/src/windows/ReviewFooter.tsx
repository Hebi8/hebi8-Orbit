import { useMemo, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { ChevronUp } from 'lucide-react';
import type { AnnotationView, TaskTimelineResponse } from '../generated/protocol';
import { api, errMsg } from '../lib/api';
import { basename } from '../lib/format';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';

/**
 * Shared minimal single-row footer for ReviewOverlay:
 * - Left: `{N} 条批注 ▴` popover checklist (hidden when 0 open annotations)
 * - Right: `看完了` + `发给会话` (only when open annotations exist)
 */
export function ReviewFooter({
  taskId,
  deliveryId,
  tl,
  extraAnnotations,
}: {
  taskId: string;
  deliveryId?: string;
  tl?: TaskTimelineResponse;
  extraAnnotations?: AnnotationView[];
}) {
  const qc = useQueryClient();
  const deselectedArr = useWorkbenchStore((s) => s.deselectedAnnotations[taskId]);
  const toggleAnnSelected = useWorkbenchStore((s) => s.toggleAnnotationSelected);
  const clearDeselected = useWorkbenchStore((s) => s.clearDeselectedAnnotations);
  const deselected = useMemo(() => new Set(deselectedArr ?? []), [deselectedArr]);
  const [popOpen, setPopOpen] = useState(false);
  const [verifiedSite, setVerifiedSite] = useState(false);

  const lastAtt = tl?.attempts?.[tl.attempts.length - 1];
  const needsVerifyAttemptId =
    !tl?.task.activeAttemptId && lastAtt?.state === 'needs_verification' ? lastAtt.attemptId : undefined;

  const openAnnotations = useMemo(() => {
    const byId = new Map<string, AnnotationView>();
    for (const a of tl?.annotations ?? []) byId.set(a.annotationId, a);
    for (const a of extraAnnotations ?? []) byId.set(a.annotationId, a);
    return [...byId.values()].filter((a) => a.status === 'open').sort((a, b) => a.createdAt - b.createdAt);
  }, [tl?.annotations, extraAnnotations]);

  const selectedIds = openAnnotations.filter((a) => !deselected.has(a.annotationId)).map((a) => a.annotationId);

  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ['delivery'] });
    void qc.invalidateQueries({ queryKey: ['task-timeline', taskId] });
    void qc.invalidateQueries({ queryKey: ['bootstrap'] });
  };

  const dispatch = useMutation({
    mutationFn: (ids: string[]) =>
      api.dispatchAnnotations(taskId, {
        annotationIds: ids,
        mode: 'queue',
        verifiedSite: needsVerifyAttemptId ? verifiedSite : undefined,
        verifiedAttemptId: needsVerifyAttemptId,
      }),
    onSuccess: (res) => {
      toast(res.pendingInput ? `已送入下一轮（${res.queuedCount} 条）` : `已发给会话（${res.queuedCount} 条）`);
      clearDeselected(taskId);
      setVerifiedSite(false);
      setPopOpen(false);
      refresh();
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const complete = useMutation({
    mutationFn: () => api.completeDeliveryReview(deliveryId!),
    onSuccess: () => {
      toast('已标记看完了');
      refresh();
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  return (
    <div className="wf">
      <div className="wf-l relpos">
        {openAnnotations.length > 0 && (
          <>
            <button
              type="button"
              className={`btn ghost wf-ann-btn ${popOpen ? 'on' : ''}`}
              onClick={() => setPopOpen(!popOpen)}
            >
              <span>{selectedIds.length} 条批注</span>
              <ChevronUp size={12} strokeWidth={1.7} className={popOpen ? 'rot180' : ''} />
            </button>
            {popOpen && (
              <>
                <div
                  className="scrim"
                  style={{ background: 'transparent', backdropFilter: 'none', zIndex: 51 }}
                  onClick={() => setPopOpen(false)}
                />
                <div className="ann-pop" role="dialog" aria-label="待发送批注">
                  {openAnnotations.map((a) => {
                    const loc = a.anchor.path
                      ? `${basename(a.anchor.path)}:${a.anchor.startLine ?? ''}`
                      : [basename(a.anchor.artifact), a.anchor.section || a.anchor.elementId].filter(Boolean).join(' · ') || '画布';
                    return (
                      <label className="ann-pop-i" key={a.annotationId}>
                        <input
                          type="checkbox"
                          checked={!deselected.has(a.annotationId)}
                          onChange={(e) => toggleAnnSelected(taskId, a.annotationId, e.target.checked)}
                        />
                        <span className="ann-pop-b">
                          <span className="loc mono">{loc}</span>
                          <span className="txt">{a.body}</span>
                        </span>
                      </label>
                    );
                  })}
                </div>
              </>
            )}
          </>
        )}
      </div>
      <div className="wf-r">
        {needsVerifyAttemptId && openAnnotations.length > 0 && (
          <label className="chk" style={{ color: 'var(--warn)', marginRight: 4 }}>
            <input type="checkbox" checked={verifiedSite} onChange={(e) => setVerifiedSite(e.target.checked)} />
            看过了
          </label>
        )}
        {deliveryId && (
          <button type="button" className="btn ghost" disabled={complete.isPending} onClick={() => complete.mutate()}>
            看完了
          </button>
        )}
        {openAnnotations.length > 0 && (
          <button
            type="button"
            className="btn pri"
            disabled={selectedIds.length === 0 || dispatch.isPending || (!!needsVerifyAttemptId && !verifiedSite)}
            onClick={() => dispatch.mutate(selectedIds)}
          >
            发给会话
          </button>
        )}
      </div>
    </div>
  );
}
