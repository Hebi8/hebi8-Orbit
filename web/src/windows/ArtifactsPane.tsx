import { useEffect, useMemo, useRef, type ReactNode } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { ArrowUpRight, RefreshCw } from 'lucide-react';
import type { TaskTimelineResponse } from '../generated/protocol';
import { ArtIcon } from '../components/Icons';
import { useDelivery } from '../hooks/queries';
import { api, errMsg } from '../lib/api';
import { basename, shortSha } from '../lib/format';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';
import { ReviewFooter } from './ReviewFooter';

function openExternal(url: string) {
  window.open(url, '_blank', 'noopener,noreferrer');
}

/**
 * Single-column calm Artifacts pane:
 * Ryugi (:8443) enforces CSP `frame-ancestors 'none'` across ports, so canvases open cleanly
 * in their isolated window while synced annotations appear inline below and auto-sync on focus.
 */
export function ArtifactsPane({
  taskId,
  deliveryId,
  onSelectDelivery,
  tl,
  tabSwitch,
  headerEnd,
}: {
  taskId: string;
  deliveryId: string;
  onSelectDelivery: (id: string) => void;
  tl?: TaskTimelineResponse;
  tabSwitch?: ReactNode;
  headerEnd?: ReactNode;
}) {
  const qc = useQueryClient();
  const deselectedArr = useWorkbenchStore((s) => s.deselectedAnnotations[taskId]);
  const toggleAnnSelected = useWorkbenchStore((s) => s.toggleAnnotationSelected);
  const deselected = useMemo(() => new Set(deselectedArr ?? []), [deselectedArr]);
  const lastAutoSyncRef = useRef<number>(0);

  const versions = (tl?.deliveries ?? []).filter((d) => (d.ryugiBindings?.length ?? 0) > 0);
  const effectiveDeliveryId =
    versions.some((v) => v.deliveryId === deliveryId)
      ? deliveryId
      : versions[versions.length - 1]?.deliveryId ?? deliveryId;

  const dq = useDelivery(effectiveDeliveryId);
  const detail = dq.data;
  const bindings = detail?.ryugiBindings ?? [];
  const notes = (detail?.annotations ?? []).filter((a) => a.source === 'ryugi');
  const d = detail?.delivery;

  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ['delivery'] });
    void qc.invalidateQueries({ queryKey: ['task-timeline', taskId] });
  };

  const sync = useMutation({
    mutationFn: (silent: boolean) => api.syncDeliveryRyugi(effectiveDeliveryId).then((res) => ({ res, silent })),
    onSuccess: ({ res, silent }) => {
      refresh();
      if (!silent) {
        const errs = res.errors ?? [];
        toast(
          errs.length ? `同步部分失败：${errs[0]}` : res.newAnnotations > 0 ? `同步了 ${res.newAnnotations} 条新批注` : '已是最新',
          errs.length && !res.bindingsSynced ? 'err' : 'info',
        );
      }
    },
    onError: (e, silent) => {
      if (!silent) toast(errMsg(e), 'err');
    },
  });

  // Auto-sync silently on window focus after returning from Ryugi :8443
  useEffect(() => {
    if (!effectiveDeliveryId || bindings.length === 0) return;
    const onFocus = () => {
      if (Date.now() - lastAutoSyncRef.current < 4000 || sync.isPending) return;
      lastAutoSyncRef.current = Date.now();
      sync.mutate(true);
    };
    window.addEventListener('focus', onFocus);
    return () => window.removeEventListener('focus', onFocus);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [effectiveDeliveryId, bindings.length]);

  return (
    <>
      <div className="rev-h">
        {tabSwitch}
        {versions.length >= 2 && (
          <select
            className="rev-ver mono"
            value={effectiveDeliveryId}
            onChange={(e) => onSelectDelivery(e.target.value)}
            aria-label="版本"
          >
            {versions.map((v, i) => (
              <option key={v.deliveryId} value={v.deliveryId}>
                v{i + 1} · {shortSha(v.headSha)}
              </option>
            ))}
          </select>
        )}
        <div className="rev-h-acts">
          {d && (
            <button
              type="button"
              className="ib"
              title="同步画布批注"
              disabled={sync.isPending}
              onClick={() => sync.mutate(false)}
            >
              <RefreshCw size={13} strokeWidth={1.6} className={sync.isPending ? 'spinning' : ''} />
            </button>
          )}
          {headerEnd}
        </div>
      </div>

      <div className="art-single">
        {dq.isLoading && <div className="empty">载入设计稿…</div>}
        {!dq.isLoading && bindings.length === 0 && <div className="empty">没有设计稿</div>}

        {bindings.length > 0 && (
          <div className="art-list">
            {bindings.map((b) => (
              <div className="art-row" key={b.bindingId} title={b.artifactLocator}>
                <ArtIcon size={14} />
                <span className="n ell">{basename(b.artifactLocator) || b.artifactLocator}</span>
                <button
                  type="button"
                  className="btn"
                  disabled={!b.openUrl}
                  onClick={() => openExternal(b.openUrl)}
                >
                  打开画布 <ArrowUpRight size={12} strokeWidth={1.6} />
                </button>
              </div>
            ))}
          </div>
        )}

        {notes.length > 0 && (
          <div className="art-notes">
            {notes.map((a) => {
              const open = a.status === 'open';
              const loc = [basename(a.anchor.artifact), a.anchor.section || a.anchor.elementId || a.anchor.page].filter(Boolean).join(' · ');
              return (
                <label className={`note ${open ? '' : 'dim'}`} key={a.annotationId}>
                  <input
                    type="checkbox"
                    disabled={!open}
                    checked={open && !deselected.has(a.annotationId)}
                    onChange={(e) => toggleAnnSelected(taskId, a.annotationId, e.target.checked)}
                  />
                  <span style={{ minWidth: 0 }}>
                    {loc && <span className="loc">{loc}</span>}
                    {a.anchor.quote && <span className="loc">“{a.anchor.quote}”</span>}
                    {a.body}
                  </span>
                </label>
              );
            })}
          </div>
        )}
      </div>

      <ReviewFooter
        taskId={taskId}
        deliveryId={d?.deliveryId}
        tl={tl}
        extraAnnotations={detail?.annotations}
      />
    </>
  );
}
