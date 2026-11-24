import { useEffect, useMemo, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { ArrowUpRight, RefreshCw } from 'lucide-react';
import type { AnnotationView, RyugiSyncResponse, TaskTimelineResponse } from '../generated/protocol';
import { ArtIcon } from '../components/Icons';
import { useDelivery } from '../hooks/queries';
import { api, errMsg } from '../lib/api';
import { basename, relTime, shortSha } from '../lib/format';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';

function openExternal(url: string) {
  window.open(url, '_blank', 'noopener,noreferrer');
}

export function ArtifactsPane({
  taskId,
  deliveryId,
  onSelectDelivery,
  tl,
}: {
  taskId: string;
  deliveryId: string;
  onSelectDelivery: (id: string) => void;
  tl?: TaskTimelineResponse;
}) {
  const qc = useQueryClient();
  const deselectedArr = useWorkbenchStore((s) => s.deselectedAnnotations[taskId]);
  const toggleAnnSelected = useWorkbenchStore((s) => s.toggleAnnotationSelected);
  const clearDeselected = useWorkbenchStore((s) => s.clearDeselectedAnnotations);
  const deselected = useMemo(() => new Set(deselectedArr ?? []), [deselectedArr]);
  const [lastSync, setLastSync] = useState<RyugiSyncResponse | null>(null);
  const [verifiedSite, setVerifiedSite] = useState(false);

  const versions = (tl?.deliveries ?? []).filter((d) => (d.ryugiBindings?.length ?? 0) > 0);
  const effectiveDeliveryId =
    versions.some((v) => v.deliveryId === deliveryId)
      ? deliveryId
      : versions[versions.length - 1]?.deliveryId ?? deliveryId;

  useEffect(() => {
    setLastSync(null);
  }, [effectiveDeliveryId]);

  const dq = useDelivery(effectiveDeliveryId);
  const detail = dq.data;
  const bindings = detail?.ryugiBindings ?? [];
  const notes = (detail?.annotations ?? []).filter((a) => a.source === 'ryugi');
  const openNotes = notes.filter((a) => a.status === 'open');
  const localSelectedIds = openNotes.filter((a) => !deselected.has(a.annotationId)).map((a) => a.annotationId);
  const lastAtt = tl?.attempts?.[tl.attempts.length - 1];
  const needsVerifyAttemptId = !tl?.task.activeAttemptId && lastAtt?.state === 'needs_verification' ? lastAtt.attemptId : undefined;
  useEffect(() => setVerifiedSite(false), [needsVerifyAttemptId]);

  const { selDiffIds, selRyugiIds, combinedSelectedIds } = useMemo(() => {
    const byId = new Map<string, AnnotationView>();
    for (const a of tl?.annotations ?? []) byId.set(a.annotationId, a);
    for (const a of detail?.annotations ?? []) byId.set(a.annotationId, a);
    const allOpen = [...byId.values()].filter((a) => a.status === 'open');
    const openDiff = allOpen.filter((a) => a.source !== 'ryugi');
    const openRyugi = allOpen.filter((a) => a.source === 'ryugi');
    const diffIds = openDiff.filter((a) => !deselected.has(a.annotationId)).map((a) => a.annotationId);
    const ryugiIds = openRyugi.filter((a) => !deselected.has(a.annotationId)).map((a) => a.annotationId);
    return {
      selDiffIds: diffIds,
      selRyugiIds: ryugiIds,
      combinedSelectedIds: [...diffIds, ...ryugiIds],
    };
  }, [tl?.annotations, detail?.annotations, deselected]);

  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ['delivery'] });
    void qc.invalidateQueries({ queryKey: ['task-timeline', taskId] });
  };

  const sync = useMutation({
    mutationFn: () => api.syncDeliveryRyugi(effectiveDeliveryId),
    onSuccess: (res) => {
      setLastSync(res);
      const errs = res.errors ?? [];
      toast(
        errs.length
          ? `同步部分失败：${errs[0]}`
          : `已同步 · 新批注 ${res.newAnnotations} 条`,
        errs.length && !res.bindingsSynced ? 'err' : 'info',
      );
      refresh();
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const dispatch = useMutation({
    mutationFn: (ids: string[]) =>
      api.dispatchAnnotations(taskId, {
        annotationIds: ids,
        mode: 'queue',
        verifiedSite: needsVerifyAttemptId ? verifiedSite : undefined,
        verifiedAttemptId: needsVerifyAttemptId,
      }),
    onSuccess: (res) => {
      toast(res.pendingInput ? `已送入队列（${res.queuedCount} 条批注）` : `已发给会话（${res.queuedCount} 条批注）`);
      clearDeselected(taskId);
      setVerifiedSite(false);
      refresh();
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const d = detail?.delivery;

  return (
    <>
      {versions.length > 1 && (
        <div className="vtabs">
          {versions.map((v, i) => (
            <button
              type="button"
              key={v.deliveryId}
              className={v.deliveryId === effectiveDeliveryId ? 'on' : ''}
              onClick={() => onSelectDelivery(v.deliveryId)}
            >
              v{i + 1} <span className="mono faint">{shortSha(v.headSha)}</span>
            </button>
          ))}
        </div>
      )}

      <div className="art-body">
        <div className="art-main">
          {dq.isLoading && <div className="empty">载入设计产物…</div>}
          {!dq.isLoading && bindings.length === 0 && <div className="empty">这次交付没有设计产物</div>}
          {bindings.map((b) => (
            <div className="bcard" key={b.bindingId}>
              <div className="h">
                <ArtIcon size={13} />
                <span className="n ell">{basename(b.artifactLocator) || b.artifactLocator}</span>
                <span style={{ marginLeft: 'auto' }}>
                  <button type="button" className="btn" disabled={!b.openUrl} onClick={() => openExternal(b.openUrl)}>
                    <ArrowUpRight size={12} strokeWidth={1.6} /> 独立窗口打开
                  </button>
                </span>
              </div>
              <div className="meta">
                <span className="k">路径</span>
                <span className="v mono">{b.artifactLocator}</span>
                <span className="k">更新</span>
                <span className="v">{relTime(b.updatedAt)}</span>
              </div>
            </div>
          ))}
        </div>
        <div className="notes">
          <div className="notes-h">画布批注</div>
          <div className="notes-l">
            {notes.length === 0 && <div className="fsub" style={{ padding: 6 }}>在独立窗口留评后点「同步批注」</div>}
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
                    {!open && <span className="loc">{a.status === 'queued' ? '已入队' : a.status}</span>}
                  </span>
                </label>
              );
            })}
          </div>
        </div>
      </div>

      <div className="wf">
        <span className="l">
          {lastSync
            ? `已同步 · +${lastSync.newAnnotations} 条新批注`
            : combinedSelectedIds.length > 0
              ? `已选 ${combinedSelectedIds.length} 条批注`
              : '画布批注可与代码行评一起送进右侧会话'}
        </span>
        <span className="r">
          {needsVerifyAttemptId && (
            <label className="chk" style={{ color: 'var(--warn)', marginRight: 4 }}>
              <input type="checkbox" checked={verifiedSite} onChange={(e) => setVerifiedSite(e.target.checked)} />
              已核对现场
            </label>
          )}
          <button type="button" className="btn" disabled={sync.isPending || !d} onClick={() => sync.mutate()}>
            <RefreshCw size={12} strokeWidth={1.6} className={sync.isPending ? 'spinning' : ''} /> 同步批注
          </button>
          {selDiffIds.length > 0 && localSelectedIds.length > 0 && (
            <button
              type="button"
              className="btn ghost"
              disabled={dispatch.isPending || (!!needsVerifyAttemptId && !verifiedSite)}
              onClick={() => dispatch.mutate(localSelectedIds)}
            >
              仅发画布 ({localSelectedIds.length})
            </button>
          )}
          <button
            type="button"
            className="btn pri"
            disabled={combinedSelectedIds.length === 0 || dispatch.isPending || (!!needsVerifyAttemptId && !verifiedSite)}
            onClick={() => dispatch.mutate(combinedSelectedIds)}
          >
            {selDiffIds.length > 0 && selRyugiIds.length > 0
              ? `合并发送 ${combinedSelectedIds.length} 条批注`
              : `发送 ${combinedSelectedIds.length} 条批注`}
          </button>
        </span>
      </div>
    </>
  );
}
