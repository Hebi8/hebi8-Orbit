import { useEffect, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { ArrowUpRight, RefreshCw } from 'lucide-react';
import type { RyugiSyncResponse } from '../generated/protocol';
import { ArtIcon } from '../components/Icons';
import { useDelivery, useTimeline } from '../hooks/queries';
import { api, errMsg } from '../lib/api';
import { basename, relTime, shortSha } from '../lib/format';
import { toast, useWorkbenchStore, type WinState } from '../store/useWorkbenchStore';
import { FloatingWindow } from './FloatingWindow';

function openExternal(url: string) {
  window.open(url, '_blank', 'noopener,noreferrer');
}

function ArtifactsInner({ w }: { w: WinState }) {
  const qc = useQueryClient();
  const [deliveryId, setDeliveryId] = useState(w.deliveryId);
  const [deselected, setDeselected] = useState<Set<string>>(new Set());
  const [lastSync, setLastSync] = useState<RyugiSyncResponse | null>(null);
  useEffect(() => setDeliveryId(w.deliveryId), [w.deliveryId]);
  useEffect(() => {
    setDeselected(new Set());
    setLastSync(null);
  }, [deliveryId]);

  const tl = useTimeline(w.taskId);
  const versions = (tl.data?.deliveries ?? []).filter((d) => (d.ryugiBindings?.length ?? 0) > 0);
  const dq = useDelivery(deliveryId);
  const detail = dq.data;
  const bindings = detail?.ryugiBindings ?? [];
  const notes = (detail?.annotations ?? []).filter((a) => a.source === 'ryugi');
  const openNotes = notes.filter((a) => a.status === 'open');
  const selectedIds = openNotes.filter((a) => !deselected.has(a.annotationId)).map((a) => a.annotationId);

  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ['delivery', deliveryId] });
    void qc.invalidateQueries({ queryKey: ['task-timeline', w.taskId] });
  };

  const sync = useMutation({
    mutationFn: () => api.syncDeliveryRyugi(deliveryId),
    onSuccess: (res) => {
      setLastSync(res);
      const errs = res.errors ?? [];
      toast(
        errs.length
          ? `同步部分失败：${errs[0]}`
          : `已同步 ${res.bindingsSynced} 个画布 · 新批注 ${res.newAnnotations} 条`,
        errs.length && !res.bindingsSynced ? 'err' : 'info',
      );
      refresh();
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });
  const dispatch = useMutation({
    mutationFn: (ids: string[]) => api.dispatchAnnotations(w.taskId, { annotationIds: ids, mode: 'queue' }),
    onSuccess: (res) => {
      toast(res.pendingInput ? `已打包 ${res.queuedCount} 条画布批注 → 待消费队列` : `已打包 ${res.queuedCount} 条画布批注 → 直接开始新一轮`);
      refresh();
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const d = detail?.delivery;
  return (
    <FloatingWindow
      kind="art"
      icon={<ArtIcon />}
      title="Artifacts · Ryugi"
      width="min(820px, calc(100vw - 340px))"
      height="min(560px, calc(100vh - 110px))"
      subtitle={d ? `冻结快照 ${shortSha(d.headSha)} · 同步 ${d.ryugiSyncState}` : '载入中…'}
      actions={
        bindings[0]?.openUrl ? (
          <button type="button" className="btn ghost" onClick={() => openExternal(bindings[0].openUrl)}>
            <ArrowUpRight size={13} strokeWidth={1.6} /> 独立窗口打开
          </button>
        ) : null
      }
      footer={
        <>
          <span className="l">
            {lastSync
              ? `上次同步：${lastSync.bindingsSynced} 画布 · +${lastSync.newAnnotations} 批注${lastSync.errors?.length ? ` · ${lastSync.errors.length} 个错误` : ''}`
              : '批注会作为一条待消费消息挂到会话末尾'}
          </span>
          <span className="r">
            <button type="button" className="btn" disabled={sync.isPending || !d} onClick={() => sync.mutate()}>
              <RefreshCw size={12} strokeWidth={1.6} className={sync.isPending ? 'spinning' : ''} /> 立即同步批注
            </button>
            <button type="button" className="btn pri" disabled={selectedIds.length === 0 || dispatch.isPending} onClick={() => dispatch.mutate(selectedIds)}>
              打包 {selectedIds.length} 条画布批注 → 送入待消费队列
            </button>
          </span>
        </>
      }
    >
      {versions.length > 1 && (
        <div className="vtabs">
          {versions.map((v, i) => (
            <button type="button" key={v.deliveryId} className={v.deliveryId === deliveryId ? 'on' : ''} onClick={() => setDeliveryId(v.deliveryId)}>
              v{i + 1} <span className="mono faint">{shortSha(v.headSha)}</span>
            </button>
          ))}
        </div>
      )}
      <div className="art-body">
        <div className="art-main">
          {dq.isLoading && <div className="empty">载入画布…</div>}
          {!dq.isLoading && bindings.length === 0 && <div className="empty">这次交付没有 Ryugi 产物</div>}
          {bindings.map((b) => {
            const n = notes.filter((a) => a.anchor.artifact === b.artifactLocator || !a.anchor.artifact).length;
            return (
              <div className="bcard" key={b.bindingId}>
                <div className="h">
                  <ArtIcon size={13} />
                  <span className="n ell">{basename(b.artifactLocator) || b.artifactLocator}</span>
                  <span className={`sd ${b.active ? 'on' : 'done'}`} title={b.active ? '活跃绑定 · 自动轮询批注' : '冷历史'} />
                  <span style={{ marginLeft: 'auto' }}>
                    <button type="button" className="btn" disabled={!b.openUrl} onClick={() => openExternal(b.openUrl)}>
                      <ArrowUpRight size={12} strokeWidth={1.6} /> 独立窗口打开
                    </button>
                  </span>
                </div>
                <div className="meta">
                  <span className="k">产物</span>
                  <span className="v mono">{b.artifactLocator}</span>
                  <span className="k">冻结目录</span>
                  <span className="v mono">{basename(b.frozenProjectDir)}</span>
                  <span className="k">派生 ID</span>
                  <span className="v mono">{b.derivedArtifactId || '—'}</span>
                  <span className="k">批注</span>
                  <span className="v">
                    {n} 条{b.lastThreadReceipt ? ' · 已回执' : ''} · 更新于 {relTime(b.updatedAt)}
                  </span>
                </div>
              </div>
            );
          })}
          <div className="fsub" style={{ padding: '2px 2px' }}>
            Ryugi 页面禁止被嵌入（frame-ancestors），设计稿在独立窗口里查看与点选留评；这里只做冻结元信息与批注回流。
          </div>
        </div>
        <div className="notes">
          <div className="notes-h">
            已同步画布批注 {notes.length}
            <span className="r">{openNotes.length ? `${openNotes.length} 未决` : ''}</span>
          </div>
          <div className="notes-l">
            {notes.length === 0 && <div className="fsub" style={{ padding: 6 }}>还没有同步回来的批注。在独立窗口留评后点「立即同步批注」。</div>}
            {notes.map((a) => {
              const open = a.status === 'open';
              const loc = [basename(a.anchor.artifact), a.anchor.section || a.anchor.elementId || a.anchor.page].filter(Boolean).join(' · ');
              return (
                <label className={`note ${open ? '' : 'dim'}`} key={a.annotationId}>
                  <input
                    type="checkbox"
                    disabled={!open}
                    checked={open && !deselected.has(a.annotationId)}
                    onChange={(e) =>
                      setDeselected((s) => {
                        const n = new Set(s);
                        if (e.target.checked) n.delete(a.annotationId);
                        else n.add(a.annotationId);
                        return n;
                      })
                    }
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
    </FloatingWindow>
  );
}

export function ArtifactsWindow() {
  const w = useWorkbenchStore((s) => s.windows.art);
  if (!w) return null;
  return <ArtifactsInner w={w} />;
}
