import { useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import {
  CheckCircle2,
  ExternalLink,
  Eye,
  Layers,
  Lock,
  RefreshCw,
} from 'lucide-react';
import type { DeliveryDetailResponse, RyugiSyncResponse } from '../generated/protocol';
import { api } from '../lib/api';

interface RyugiCanvasTabProps {
  detail: DeliveryDetailResponse;
}

export function RyugiCanvasTab({ detail }: RyugiCanvasTabProps) {
  const queryClient = useQueryClient();
  const [experimentalIframe, setExperimentalIframe] = useState(false);
  const [iframeUrl, setIframeUrl] = useState<string>('');
  const [syncResult, setSyncResult] = useState<RyugiSyncResponse | null>(null);

  const syncMutation = useMutation({
    mutationFn: () => api.syncDeliveryRyugi(detail.delivery.deliveryId),
    onSuccess: (res) => {
      setSyncResult(res);
      void queryClient.invalidateQueries({ queryKey: ['delivery', detail.delivery.deliveryId] });
      void queryClient.invalidateQueries({ queryKey: ['task-timeline', detail.delivery.taskId] });
      void queryClient.invalidateQueries({ queryKey: ['bootstrap'] });
    },
  });

  const bindings = detail.ryugiBindings;
  const ryugiAnnotations = detail.annotations.filter((a) => a.source === 'ryugi');

  return (
    <div className="flex flex-1 flex-col overflow-y-auto p-4 space-y-4">
      {/* Header & Manual Sync Action */}
      <div className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-zinc-200 bg-zinc-50/80 p-3 dark:border-zinc-800 dark:bg-zinc-900/50">
        <div>
          <div className="flex items-center gap-1.5 text-xs font-semibold text-zinc-900 dark:text-zinc-100">
            <Lock className="h-3.5 w-3.5 text-emerald-500" />
            <span>冻结交付画布</span>
            <span className="rounded bg-zinc-200 px-1.5 py-0.5 font-mono text-[10px] text-zinc-600 dark:bg-zinc-800 dark:text-zinc-300">
              {detail.delivery.ryugiSyncState}
            </span>
          </div>
          <p className="mt-0.5 font-mono text-[11px] text-zinc-500">
            {detail.delivery.frozenSnapshotDir || '无 .ryugi/artifacts 快照'}
          </p>
        </div>

        <button
          type="button"
          disabled={syncMutation.isPending}
          onClick={() => syncMutation.mutate()}
          className="flex min-h-[36px] items-center gap-1.5 rounded-md border border-sky-500/40 bg-sky-500/10 px-3 py-1.5 text-xs font-medium text-sky-600 transition hover:bg-sky-500/20 disabled:opacity-50 dark:text-sky-400"
        >
          <RefreshCw className={`h-3.5 w-3.5 ${syncMutation.isPending ? 'animate-spin' : ''}`} />
          <span>{syncMutation.isPending ? '同步中...' : '立即同步批注'}</span>
        </button>
      </div>

      {syncResult && (
        <div className="flex items-center justify-between rounded-md border border-emerald-500/30 bg-emerald-500/10 px-3 py-2 text-xs text-emerald-700 dark:text-emerald-300">
          <div className="flex items-center gap-1.5">
            <CheckCircle2 className="h-3.5 w-3.5" />
            <span>
              同步完成：扫描 {syncResult.bindingsSynced} 个绑定，新增 {syncResult.newAnnotations}{' '}
              条批注，已 ack {syncResult.receiptsAcked} 个回执
            </span>
          </div>
        </div>
      )}

      {syncMutation.isError && (
        <div className="rounded-md border border-rose-500/30 bg-rose-500/10 px-3 py-2 text-xs text-rose-600 dark:text-rose-400">
          同步失败：{(syncMutation.error as Error).message}
        </div>
      )}

      {/* Registered Artifact Cards */}
      {bindings.length === 0 ? (
        <div className="rounded-lg border border-dashed border-zinc-200 px-4 py-8 text-center text-xs text-zinc-500 dark:border-zinc-800">
          本次交付未包含 <code>.ryugi/artifacts/</code> 产物。
        </div>
      ) : (
        <div className="space-y-2.5">
          {bindings.map((b) => (
            <div
              key={b.bindingId}
              className="rounded-lg border border-zinc-200 bg-white p-3 dark:border-zinc-800 dark:bg-zinc-900/60"
            >
              <div className="flex items-start justify-between gap-2">
                <div className="min-w-0">
                  <div className="flex items-center gap-2">
                    <Layers className="h-3.5 w-3.5 text-indigo-500" />
                    <span className="font-mono text-xs font-semibold text-zinc-900 dark:text-zinc-100">
                      {b.artifactLocator}
                    </span>
                  </div>
                  <div className="mt-1 flex flex-wrap items-center gap-2 font-mono text-[10px] text-zinc-500">
                    <span>.artifact-id: {b.derivedArtifactId}</span>
                    {b.lastThreadReceipt && <span>ack: {b.lastThreadReceipt.slice(0, 8)}</span>}
                  </div>
                </div>

                <a
                  href={b.openUrl}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="flex shrink-0 items-center gap-1.5 rounded-md bg-zinc-900 px-2.5 py-1.5 text-xs font-medium text-white transition hover:bg-zinc-700 dark:bg-zinc-100 dark:text-zinc-900 dark:hover:bg-zinc-300"
                >
                  <span>新标签打开</span>
                  <ExternalLink className="h-3 w-3" />
                </a>
              </div>

              <div className="mt-2 truncate rounded bg-zinc-100 px-2 py-1 font-mono text-[10px] text-zinc-500 dark:bg-zinc-950 dark:text-zinc-400">
                {b.openUrl}
              </div>

              {experimentalIframe && (
                <div className="mt-2">
                  <button
                    type="button"
                    onClick={() => setIframeUrl(iframeUrl === b.openUrl ? '' : b.openUrl)}
                    className="text-[11px] text-sky-600 hover:underline dark:text-sky-400"
                  >
                    {iframeUrl === b.openUrl ? '收起内嵌预览' : '在此抽屉预览（实验性）'}
                  </button>
                </div>
              )}
            </div>
          ))}
        </div>
      )}

      {/* Experimental iframe preview toggle (OFF by default, safely collapsed) */}
      {bindings.length > 0 && (
        <div className="rounded-md border border-zinc-200/80 bg-zinc-50/50 p-3 dark:border-zinc-800/80 dark:bg-zinc-900/30">
          <label className="flex cursor-pointer items-center justify-between gap-2 text-xs text-zinc-600 dark:text-zinc-400">
            <span className="flex items-center gap-1.5">
              <Eye className="h-3.5 w-3.5" />
              <span>实验性内嵌 iframe 预览（默认关闭，仅三端口验证通过后启用）</span>
            </span>
            <input
              type="checkbox"
              checked={experimentalIframe}
              onChange={(e) => {
                setExperimentalIframe(e.target.checked);
                if (!e.target.checked) setIframeUrl('');
              }}
              className="h-4 w-4 accent-sky-500"
            />
          </label>
          {experimentalIframe && iframeUrl && (
            <iframe
              title="Ryugi Experimental Preview"
              src={iframeUrl}
              referrerPolicy="no-referrer"
              className="mt-3 h-[420px] w-full rounded border border-zinc-200 bg-white dark:border-zinc-700"
            />
          )}
        </div>
      )}

      {/* Synced Ryugi Annotations */}
      <div className="space-y-2">
        <div className="text-[11px] font-semibold uppercase tracking-wider text-zinc-500 dark:text-zinc-400">
          已同步 Ryugi 批注 ({ryugiAnnotations.length})
        </div>
        {ryugiAnnotations.length === 0 ? (
          <div className="text-xs text-zinc-400 dark:text-zinc-500">
            暂无来自 Ryugi 画布的批注。在画布提交评论后会自动轮询或点击「立即同步批注」回流。
          </div>
        ) : (
          ryugiAnnotations.map((a) => (
            <div
              key={a.annotationId}
              className="rounded-md border border-zinc-200 bg-white p-2.5 text-xs dark:border-zinc-800 dark:bg-zinc-900/60"
            >
              <div className="flex items-center justify-between font-mono text-[10px] text-zinc-500">
                <span>
                  {a.anchor.artifact} · {a.anchor.section}
                  {a.anchor.blockId ? ` · #${a.anchor.blockId}` : ''}
                </span>
                <span className="rounded bg-zinc-100 px-1 py-0.5 dark:bg-zinc-800">
                  {a.status}
                </span>
              </div>
              <p className="mt-1 whitespace-pre-wrap text-zinc-800 dark:text-zinc-200">{a.body}</p>
            </div>
          ))
        )}
      </div>
    </div>
  );
}
