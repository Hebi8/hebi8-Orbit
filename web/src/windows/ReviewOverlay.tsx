import { useEffect, useRef, useState } from 'react';
import { ArrowUpRight, PanelRightClose, PanelRightOpen, X } from 'lucide-react';
import type { BootstrapResponse } from '../generated/protocol';
import { ArtIcon, DiffIcon } from '../components/Icons';
import { useTimeline } from '../hooks/queries';
import { attentionTaskIds, taskDot, taskStateLabel, taskTitle } from '../lib/format';
import { navigate, paths, useRoute } from '../lib/router';
import { SessionConversation } from '../session/SessionConversation';
import { useWorkbenchStore } from '../store/useWorkbenchStore';
import { ArtifactsPane } from './ArtifactsPane';
import { CodeReviewPane } from './CodeReviewPane';

const CODE_KINDS = new Set(['code_diff', 'mixed', 'workspace_snapshot']);

/**
 * Fixed centered split overlay mirroring Lody LAN fork's `BoardArticleDialog` + `WorktreeReviewDialog`:
 *   - Left pane: deliverable review (Code Diff / Artifacts)
 *   - Right pane: the task's live Session conversation (collapsible, resizable divider)
 *   - Fixed position (never draggable); backdrop click or Esc closes.
 */
export function ReviewOverlay({ boot }: { boot: BootstrapResponse }) {
  const review = useWorkbenchStore((s) => s.review);
  const ratio = useWorkbenchStore((s) => s.reviewRatio);
  const setRatio = useWorkbenchStore((s) => s.setReviewRatio);
  const sessionHidden = useWorkbenchStore((s) => s.reviewSessionHidden);
  const setSessionHidden = useWorkbenchStore((s) => s.setReviewSessionHidden);
  const close = useWorkbenchStore((s) => s.closeReview);
  const setTab = useWorkbenchStore((s) => s.setReviewTab);
  const setDelivery = useWorkbenchStore((s) => s.setReviewDelivery);
  const route = useRoute();
  const [mobilePane, setMobilePane] = useState<'artifact' | 'session'>('artifact');
  const bodyRef = useRef<HTMLDivElement | null>(null);

  const tlq = useTimeline(review?.taskId);
  const tl = tlq.data;

  useEffect(() => {
    setMobilePane('artifact');
  }, [review?.taskId, review?.tab]);

  if (!review) return null;

  const task = tl?.task;
  const deliveries = tl?.deliveries ?? [];
  const hasCode = review.tab === 'code' || deliveries.some((d) => CODE_KINDS.has(d.kind));
  const hasArt = review.tab === 'art' || deliveries.some((d) => (d.ryugiBindings?.length ?? 0) > 0);
  const alreadyOnSession = route.name === 'session' && route.taskId === review.taskId && !route.worker;
  const attentionSet = attentionTaskIds(boot.openInboxItems);

  const fromPointer = (clientX: number) => {
    const rect = bodyRef.current?.getBoundingClientRect();
    if (rect && rect.width > 0) setRatio(((clientX - rect.left) / rect.width) * 100);
  };

  return (
    <div className="rev-layer" role="presentation">
      <div className="rev-scrim" onClick={close} />
      <section className="rev-dialog" role="dialog" aria-modal="true" aria-label="产出审阅" data-review-overlay="true">
        <div
          ref={bodyRef}
          className={`rev-grid ${sessionHidden ? 'session-hidden' : ''} mobile-${mobilePane}`}
          style={
            sessionHidden
              ? undefined
              : { gridTemplateColumns: `minmax(0, ${ratio}fr) 1px minmax(0, ${100 - ratio}fr)` }
          }
        >
          {/* Left: Artifact / Code Review */}
          <div className="rev-pane rev-left">
            <div className="rev-h">
              <div className="rev-tabs" role="tablist">
                {hasCode && (
                  <button
                    type="button"
                    role="tab"
                    aria-selected={review.tab === 'code'}
                    className={`rev-tab ${review.tab === 'code' ? 'on' : ''}`}
                    onClick={() => setTab('code')}
                  >
                    <DiffIcon size={13} /> Code
                  </button>
                )}
                {hasArt && (
                  <button
                    type="button"
                    role="tab"
                    aria-selected={review.tab === 'art'}
                    className={`rev-tab ${review.tab === 'art' ? 'on' : ''}`}
                    onClick={() => setTab('art')}
                  >
                    <ArtIcon size={13} /> Artifacts
                  </button>
                )}
              </div>

              <div className="seg only-mobile" style={{ marginLeft: 'auto' }}>
                <button type="button" className={mobilePane === 'artifact' ? 'on' : ''} onClick={() => setMobilePane('artifact')}>
                  产物
                </button>
                <button type="button" className={mobilePane === 'session' ? 'on' : ''} onClick={() => setMobilePane('session')}>
                  会话
                </button>
              </div>

              <div className="rev-h-acts">
                {sessionHidden && (
                  <>
                    <button
                      type="button"
                      className="hb no-mobile"
                      title="显示右侧会话"
                      onClick={() => setSessionHidden(false)}
                    >
                      <PanelRightOpen size={14} strokeWidth={1.6} /> 会话
                    </button>
                    <button type="button" className="ib no-mobile" title="关闭 (Esc)" onClick={close}>
                      <X size={14} strokeWidth={1.6} />
                    </button>
                  </>
                )}
                <button type="button" className="ib only-mobile" title="关闭" onClick={close}>
                  <X size={14} strokeWidth={1.6} />
                </button>
              </div>
            </div>

            {review.tab === 'code' ? (
              <CodeReviewPane
                taskId={review.taskId}
                deliveryId={review.deliveryId}
                onSelectDelivery={setDelivery}
                tl={tl}
                boot={boot}
                onClose={close}
              />
            ) : (
              <ArtifactsPane
                taskId={review.taskId}
                deliveryId={review.deliveryId}
                onSelectDelivery={setDelivery}
                tl={tl}
              />
            )}
          </div>

          {/* Resizable 1px split divider */}
          {!sessionHidden && (
            <div className="rev-div no-mobile">
              <div
                role="separator"
                aria-orientation="vertical"
                aria-label="调整产物与会话比例"
                aria-valuemin={36}
                aria-valuemax={78}
                aria-valuenow={Math.round(ratio)}
                tabIndex={0}
                className="rev-handle"
                onPointerDown={(e) => {
                  if (e.button !== 0) return;
                  e.preventDefault();
                  e.currentTarget.setPointerCapture(e.pointerId);
                }}
                onPointerMove={(e) => {
                  if (e.currentTarget.hasPointerCapture(e.pointerId)) fromPointer(e.clientX);
                }}
                onPointerUp={(e) => {
                  if (e.currentTarget.hasPointerCapture(e.pointerId)) e.currentTarget.releasePointerCapture(e.pointerId);
                }}
                onKeyDown={(e) => {
                  const step = e.key === 'ArrowLeft' ? -4 : e.key === 'ArrowRight' ? 4 : 0;
                  if (step === 0) return;
                  e.preventDefault();
                  setRatio(ratio + step);
                }}
              />
            </div>
          )}

          {/* Right: Live Session */}
          {!sessionHidden && (
            <div className="rev-pane rev-right">
              <div className="rev-h">
                {task ? (
                  <>
                    <span className={`sd ${taskDot(task.state, task.activeAttemptState, attentionSet.has(task.taskId))}`} />
                    <span className="rev-sttl ell" title={task.goal}>
                      {taskTitle(task)}
                    </span>
                    <span className="faint no-mobile" style={{ fontSize: 11.5, whiteSpace: 'nowrap', flex: 'none' }}>
                      {taskStateLabel(task, attentionSet.has(task.taskId))}
                    </span>
                  </>
                ) : (
                  <span className="faint">会话</span>
                )}

                <div className="seg only-mobile" style={{ marginLeft: 'auto' }}>
                  <button type="button" className={mobilePane === 'artifact' ? 'on' : ''} onClick={() => setMobilePane('artifact')}>
                    产物
                  </button>
                  <button type="button" className={mobilePane === 'session' ? 'on' : ''} onClick={() => setMobilePane('session')}>
                    会话
                  </button>
                </div>

                <div className="rev-h-acts">
                  {!alreadyOnSession && (
                    <button
                      type="button"
                      className="ib no-mobile"
                      title="进入完整会话页"
                      onClick={() => {
                        const tid = review.taskId;
                        close();
                        navigate(paths.session(tid));
                      }}
                    >
                      <ArrowUpRight size={14} strokeWidth={1.6} />
                    </button>
                  )}
                  <button
                    type="button"
                    className="ib no-mobile"
                    title="收起会话"
                    onClick={() => setSessionHidden(true)}
                  >
                    <PanelRightClose size={14} strokeWidth={1.6} />
                  </button>
                  <button type="button" className="ib" title="关闭 (Esc)" onClick={close}>
                    <X size={14} strokeWidth={1.6} />
                  </button>
                </div>
              </div>

              {tl ? (
                <SessionConversation taskId={review.taskId} tl={tl} compact />
              ) : (
                <div className="empty">载入会话…</div>
              )}
            </div>
          )}
        </div>
      </section>
    </div>
  );
}
