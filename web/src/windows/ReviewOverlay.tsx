import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { ChevronDown, Layers, MessageSquare, X } from 'lucide-react';
import type { BootstrapResponse } from '../generated/protocol';
import { DiffIcon } from '../components/Icons';
import { StatusMark } from '../components/StatusMark';
import { useTimeline } from '../hooks/queries';
import { collectSessionArtifacts } from '../lib/artifacts';
import { artifactShortLabel, effectiveReadyWatermark, indexInboxByTask, sessionMark, taskTitle } from '../lib/format';
import { useAnchoredPopover } from '../lib/popover';
import { SessionConversation } from '../session/SessionConversation';
import { useWorkbenchStore } from '../store/useWorkbenchStore';
import { ArtifactsPane } from './ArtifactsPane';
import { CodeReviewPane } from './CodeReviewPane';

const CODE_KINDS = new Set(['code_diff', 'mixed', 'workspace_snapshot']);

/**
 * Fixed centered split overlay (Orbit v4):
 *   - Left pane: Single-title dropdown switcher + Source Review (Unified/Split + revocable Checkpoint) or Artifact viewer
 *   - Right pane: Compact live Session conversation with intuitive `×` close button returning to full-width session
 */
export function ReviewOverlay({ boot }: { boot: BootstrapResponse }) {
  const review = useWorkbenchStore((s) => s.review);
  const reviewRatios = useWorkbenchStore((s) => s.reviewRatios);
  const setRatio = useWorkbenchStore((s) => s.setReviewRatio);
  const close = useWorkbenchStore((s) => s.closeReview);
  const openReview = useWorkbenchStore((s) => s.openReview);
  const setDelivery = useWorkbenchStore((s) => s.setReviewDelivery);
  const seenTasks = useWorkbenchStore((s) => s.seenTasks);
  const markTaskSeen = useWorkbenchStore((s) => s.markTaskSeen);

  const [mobilePane, setMobilePane] = useState<'artifact' | 'session'>('artifact');
  const [switcherOpen, setSwitcherOpen] = useState(false);
  const switcherBtnRef = useRef<HTMLButtonElement | null>(null);
  const switcherMenuRef = useRef<HTMLDivElement | null>(null);
  const bodyRef = useRef<HTMLDivElement | null>(null);
  const closeSwitcher = useCallback(() => setSwitcherOpen(false), []);

  const tlq = useTimeline(review?.taskId);
  const tl = tlq.data;
  const sessionArtifacts = useMemo(() => collectSessionArtifacts(tl), [tl]);
  const inboxByTask = useMemo(() => indexInboxByTask(boot.openInboxItems), [boot.openInboxItems]);

  useEffect(() => {
    setMobilePane('artifact');
    setSwitcherOpen(false);
  }, [review?.taskId, review?.tab, review?.artifactId]);

  const switcherStyle = useAnchoredPopover({
    open: switcherOpen && !!review,
    anchorRef: switcherBtnRef,
    popRef: switcherMenuRef,
    width: 296,
    align: 'left',
    placement: 'down',
    gap: 6,
    onClose: closeSwitcher,
  });

  const reviewTask = tl?.task;
  const reviewTaskId = review && reviewTask ? reviewTask.taskId : '';
  const reviewReadyWatermark =
    review && reviewTask ? effectiveReadyWatermark(reviewTask, inboxByTask.get(reviewTask.taskId)) : 0;

  useEffect(() => {
    if (!reviewTaskId || reviewReadyWatermark <= 0) return;
    const markVisible = () => {
      if (typeof document !== 'undefined' && document.visibilityState !== 'visible') return;
      markTaskSeen(reviewTaskId, reviewReadyWatermark);
    };
    markVisible();
    document.addEventListener('visibilitychange', markVisible);
    return () => document.removeEventListener('visibilitychange', markVisible);
  }, [reviewTaskId, reviewReadyWatermark, markTaskSeen]);

  if (!review) return null;

  const task = tl?.task;
  const taskMark = task ? sessionMark(task, inboxByTask.get(task.taskId) ?? [], seenTasks) : null;
  const deliveries = tl?.deliveries ?? [];
  const formalCode = deliveries.filter((d) => !d.lateArrival && CODE_KINDS.has(d.kind));
  const latestCode =
    formalCode.find((d) => d.deliveryId === task?.latestDeliveryId) ??
    formalCode[formalCode.length - 1] ??
    deliveries.find((d) => CODE_KINDS.has(d.kind));

  const openDiffComments = (tl?.annotations ?? []).filter((a) => a.status === 'open' && a.source !== 'ryugi').length;
  const activeArtifact =
    review.tab === 'art'
      ? sessionArtifacts.find((a) => a.artifactId === review.artifactId) ??
        sessionArtifacts.find((a) => a.deliveryId === review.deliveryId) ??
        sessionArtifacts[0]
      : undefined;

  const ratio = reviewRatios[review.tab];

  const fromPointer = (clientX: number) => {
    const rect = bodyRef.current?.getBoundingClientRect();
    if (rect && rect.width > 0) setRatio(review.tab, ((clientX - rect.left) / rect.width) * 100);
  };

  const currentTitle = review.tab === 'code' ? 'Source Review' : activeArtifact?.title ?? 'Artifacts';
  const canSwitch = (latestCode ? 1 : 0) + sessionArtifacts.length > 1 || (review.tab === 'art' && !!latestCode);

  const titleSwitcher = (
    <div className="ov-switcher-wrap">
      <button
        ref={switcherBtnRef}
        type="button"
        className={`ov-switcher-btn ${switcherOpen ? 'open' : ''}`}
        aria-haspopup={canSwitch ? 'menu' : undefined}
        aria-expanded={canSwitch ? switcherOpen : undefined}
        onClick={() => canSwitch && setSwitcherOpen((v) => !v)}
        style={!canSwitch ? { cursor: 'default' } : undefined}
      >
        {review.tab === 'code' ? <DiffIcon size={14} /> : <Layers size={14} strokeWidth={1.7} />}
        <span className="ov-switcher-title">{currentTitle}</span>
        {canSwitch && <ChevronDown size={13} strokeWidth={1.7} className="faint" />}
      </button>

      {switcherOpen &&
        canSwitch &&
        switcherStyle &&
        createPortal(
          <div ref={switcherMenuRef} className="ov-switcher-menu ov-switcher-portal" style={switcherStyle} role="menu">
            {latestCode && (
              <>
                <div className="ov-menu-sec">代码</div>
                <button
                  type="button"
                  role="menuitem"
                  className={`ov-menu-item ${review.tab === 'code' ? 'active' : ''}`}
                  onClick={() => {
                    closeSwitcher();
                    openReview('code', review.taskId, latestCode.deliveryId);
                  }}
                >
                  <DiffIcon size={13} />
                  <span className="ell grow">Source Review</span>
                  <span className="mono" style={{ fontSize: 11 }}>
                    <span className="add">+{latestCode.manifest.insertions}</span>{' '}
                    <span className="del">−{latestCode.manifest.deletions}</span>
                  </span>
                  {openDiffComments > 0 && (
                    <span className="ov-menu-badge">
                      <MessageSquare size={10} strokeWidth={1.8} />
                      {openDiffComments}
                    </span>
                  )}
                </button>
              </>
            )}

            {sessionArtifacts.length > 0 && (
              <>
                <div className="ov-menu-sec">Artifacts</div>
                {sessionArtifacts.map((art) => {
                  const isOn = review.tab === 'art' && activeArtifact?.artifactId === art.artifactId;
                  return (
                    <button
                      type="button"
                      role="menuitem"
                      key={art.artifactId}
                      className={`ov-menu-item ${isOn ? 'active' : ''}`}
                      onClick={() => {
                        closeSwitcher();
                        openReview('art', review.taskId, art.deliveryId, art.artifactId);
                      }}
                    >
                      <span className="dlv-kind-tag">{artifactShortLabel(art.kind)}</span>
                      <span className="ell grow">{art.title}</span>
                      {art.openAnnotations > 0 && (
                        <span className="ov-menu-badge">
                          <MessageSquare size={10} strokeWidth={1.8} />
                          {art.openAnnotations}
                        </span>
                      )}
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

  const leftHeaderEnd = (
    <>
      <div className="seg only-mobile">
        <button type="button" className={mobilePane === 'artifact' ? 'on' : ''} onClick={() => setMobilePane('artifact')}>
          审阅
        </button>
        <button type="button" className={mobilePane === 'session' ? 'on' : ''} onClick={() => setMobilePane('session')}>
          会话
        </button>
      </div>
      <button type="button" className="ib only-mobile" title="关闭审阅返回会话" aria-label="关闭审阅返回会话" onClick={close}>
        <X size={14} strokeWidth={1.6} />
      </button>
    </>
  );

  return (
    <div className="rev-layer" role="presentation">
      <div className="rev-scrim" onClick={close} />
      <section className="rev-dialog" role="dialog" aria-modal="true" aria-label="审阅浮层" data-review-overlay="true">
        <div
          ref={bodyRef}
          className={`rev-grid mobile-${mobilePane}`}
          style={{ gridTemplateColumns: `minmax(360px, ${ratio}fr) 1px minmax(340px, ${100 - ratio}fr)` }}
        >
          {/* Left: Source Review or Artifacts */}
          <div className="rev-pane rev-left">
            {review.tab === 'code' ? (
              <CodeReviewPane
                taskId={review.taskId}
                deliveryId={review.deliveryId}
                onSelectDelivery={setDelivery}
                tl={tl}
                boot={boot}
                onClose={close}
                titleSwitcher={titleSwitcher}
                headerEnd={leftHeaderEnd}
              />
            ) : (
              <ArtifactsPane
                taskId={review.taskId}
                deliveryId={review.deliveryId}
                artifactId={activeArtifact?.artifactId ?? review.artifactId}
                artifacts={sessionArtifacts}
                tl={tl}
                titleSwitcher={titleSwitcher}
                headerEnd={leftHeaderEnd}
              />
            )}
          </div>

          {/* Resizable 1px split divider */}
          <div className="rev-div no-mobile">
            <div
              role="separator"
              aria-orientation="vertical"
              aria-label="调整审阅与会话比例"
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
                setRatio(review.tab, ratio + step);
              }}
            />
          </div>

          {/* Right: Live Session */}
          <div className="rev-pane rev-right">
            <div className="rev-h">
              <div className="rev-sttl-wrap">
                <span className="rev-sttl ell" title={task?.goal}>
                  {task ? taskTitle(task) : '会话'}
                </span>
                {taskMark && <StatusMark mark={taskMark} />}
              </div>

              <div className="seg only-mobile" style={{ marginLeft: 'auto' }}>
                <button type="button" className={mobilePane === 'artifact' ? 'on' : ''} onClick={() => setMobilePane('artifact')}>
                  审阅
                </button>
                <button type="button" className={mobilePane === 'session' ? 'on' : ''} onClick={() => setMobilePane('session')}>
                  会话
                </button>
              </div>

              <div className="rev-h-acts">
                <button
                  type="button"
                  className="ib"
                  title="关闭审阅返回会话 (Esc)"
                  aria-label="关闭审阅返回会话"
                  onClick={close}
                >
                  <X size={14} strokeWidth={1.7} />
                </button>
              </div>
            </div>

            {tl ? (
              <SessionConversation taskId={review.taskId} tl={tl} boot={boot} compact />
            ) : (
              <div className="empty">载入会话…</div>
            )}
          </div>
        </div>
      </section>
    </div>
  );
}
