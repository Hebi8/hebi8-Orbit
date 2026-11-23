import { useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import {
  AlertTriangle,
  Check,
  CheckCircle2,
  Command,
  ExternalLink,
  FolderGit2,
  Inbox,
  Layers,
  MessageSquare,
  PackageCheck,
  RefreshCw,
  XCircle,
} from 'lucide-react';
import type {
  DeliveryDetailResponse,
  InboxItemView,
  ProjectView,
  RyugiSyncResponse,
  TaskSummary,
  TaskTimelineResponse,
} from '../generated/protocol';
import { api } from '../lib/api';
import { useWorkbenchStore } from '../store/useWorkbenchStore';

interface MobileShellProps {
  projects: ProjectView[];
  inboxItems: InboxItemView[];
  tasks: TaskSummary[];
  timeline?: TaskTimelineResponse;
  deliveryDetail?: DeliveryDetailResponse;
  onSelectInboxItem: (item: InboxItemView) => void;
}

export function MobileShell({
  projects,
  inboxItems,
  tasks,
  timeline,
  deliveryDetail,
  onSelectInboxItem,
}: MobileShellProps) {
  const queryClient = useQueryClient();
  const {
    mobileTab,
    setMobileTab,
    selectedProjectId,
    setSelectedProject,
    selectedTaskId,
    setSelectedTask,
    setSelectedDelivery,
  } = useWorkbenchStore();

  const [paInput, setPaInput] = useState('');
  const [syncByDelivery, setSyncByDelivery] = useState<Record<string, RyugiSyncResponse>>({});
  const [syncErrByDelivery, setSyncErrByDelivery] = useState<Record<string, string>>({});

  const syncMutation = useMutation({
    mutationFn: (deliveryId: string) => api.syncDeliveryRyugi(deliveryId),
    onSuccess: (res, deliveryId) => {
      setSyncByDelivery((prev) => ({ ...prev, [deliveryId]: res }));
      setSyncErrByDelivery((prev) => ({ ...prev, [deliveryId]: '' }));
      void queryClient.invalidateQueries({ queryKey: ['delivery', deliveryId] });
      if (selectedTaskId) {
        void queryClient.invalidateQueries({ queryKey: ['task-timeline', selectedTaskId] });
      }
      void queryClient.invalidateQueries({ queryKey: ['bootstrap'] });
    },
    onError: (err, deliveryId) => {
      setSyncErrByDelivery((prev) => ({ ...prev, [deliveryId]: (err as Error).message }));
    },
  });

  const completeReviewMutation = useMutation({
    mutationFn: (deliveryId: string) => api.completeDeliveryReview(deliveryId),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['bootstrap'] });
      if (selectedTaskId) {
        void queryClient.invalidateQueries({ queryKey: ['task-timeline', selectedTaskId] });
      }
    },
  });

  return (
    <div className="flex min-h-0 flex-1 flex-col md:hidden">
      <div className="flex-1 overflow-y-auto p-3 pb-20">
        {mobileTab === 'inbox' && (
          <div className="space-y-2.5">
            <div className="flex items-center justify-between">
              <h2 className="text-sm font-semibold">待拍板收件箱</h2>
              <span className="rounded-full bg-sky-500/15 px-2 py-0.5 font-mono text-xs font-semibold text-sky-500">
                {inboxItems.length}
              </span>
            </div>

            {inboxItems.length === 0 ? (
              <div className="rounded-lg border border-dashed border-zinc-200 p-6 text-center text-sm text-zinc-500 dark:border-zinc-800">
                当前无待拍板事项
              </div>
            ) : (
              inboxItems.map((it) => (
                <div
                  key={it.inboxId}
                  className="rounded-lg border border-zinc-200 bg-white p-3 dark:border-zinc-800 dark:bg-zinc-900"
                >
                  <button
                    type="button"
                    onClick={() => {
                      onSelectInboxItem(it);
                      setMobileTab('projects');
                    }}
                    className="flex min-h-[44px] w-full flex-col text-left"
                  >
                    <div className="flex items-center gap-2 text-sm font-medium">
                      <PackageCheck className="h-4 w-4 text-sky-500" />
                      <span>{it.title}</span>
                    </div>
                    <p className="mt-1 text-xs text-zinc-500">{it.summary}</p>
                  </button>

                  {it.category === 'delivery_review' && it.refId && (
                    <div className="mt-2 flex justify-end border-t border-zinc-200/70 pt-2 dark:border-zinc-800">
                      <button
                        type="button"
                        disabled={completeReviewMutation.isPending}
                        onClick={() => completeReviewMutation.mutate(it.refId)}
                        className="flex min-h-[40px] items-center gap-1.5 rounded-md border border-emerald-500/40 bg-emerald-500/10 px-3 text-xs font-medium text-emerald-700 dark:text-emerald-300"
                      >
                        <Check className="h-3.5 w-3.5" />
                        <span>完成审阅</span>
                      </button>
                    </div>
                  )}
                </div>
              ))
            )}
          </div>
        )}

        {mobileTab === 'projects' && (
          <div className="space-y-3">
            <div className="flex gap-1.5 overflow-x-auto pb-1">
              {projects.map((p) => (
                <button
                  key={p.projectId}
                  type="button"
                  onClick={() => setSelectedProject(p.projectId)}
                  className={`min-h-[40px] shrink-0 rounded-md border px-3 text-xs font-medium ${
                    selectedProjectId === p.projectId
                      ? 'border-sky-500 bg-sky-500/10 text-sky-600 dark:text-sky-400'
                      : 'border-zinc-200 dark:border-zinc-800'
                  }`}
                >
                  {p.displayName}
                </button>
              ))}
            </div>

            {tasks.length > 0 && (
              <div className="space-y-2">
                {tasks.map((t) => (
                  <button
                    key={t.taskId}
                    type="button"
                    onClick={() => setSelectedTask(t.taskId)}
                    className={`flex min-h-[44px] w-full items-center justify-between rounded-lg border px-3 py-2 text-left text-xs ${
                      selectedTaskId === t.taskId
                        ? 'border-sky-500/60 bg-sky-500/10'
                        : 'border-zinc-200 dark:border-zinc-800'
                    }`}
                  >
                    <span className="font-medium">{t.title}</span>
                    <span className="font-mono text-[11px] text-zinc-500">{t.state}</span>
                  </button>
                ))}
              </div>
            )}

            {timeline && timeline.deliveries.length > 0 && (
              <div className="space-y-3 pt-1">
                {timeline.deliveries.map((d, i) => {
                  const dlvAnns = timeline.annotations.filter((a) => a.deliveryId === d.deliveryId);
                  const syncRes = syncByDelivery[d.deliveryId];
                  const syncReqErr = syncErrByDelivery[d.deliveryId];
                  const hasOpenInbox = inboxItems.some(
                    (it) =>
                      it.category === 'delivery_review' &&
                      it.refId === d.deliveryId &&
                      it.state === 'open',
                  );

                  return (
                    <div
                      key={d.deliveryId}
                      className="rounded-lg border border-zinc-200 bg-white p-3 dark:border-zinc-800 dark:bg-zinc-900"
                    >
                      <div className="flex items-center justify-between text-xs font-semibold">
                        <span>
                          交付 v{i + 1} ({d.headSha.slice(0, 7)})
                        </span>
                        <span className="font-mono text-[11px] text-zinc-500">
                          +{d.manifest.insertions} -{d.manifest.deletions}
                        </span>
                      </div>
                      <p className="mt-1 text-xs text-zinc-500">{d.summary}</p>

                      <div className="mt-2.5 flex flex-wrap gap-2">
                        {(d.ryugiBindings ?? []).map((b) => (
                          <a
                            key={b.bindingId}
                            href={b.openUrl}
                            target="_blank"
                            rel="noopener noreferrer"
                            className="flex min-h-[40px] items-center gap-1.5 rounded-md bg-sky-600 px-3 text-xs font-medium text-white"
                          >
                            <Layers className="h-3.5 w-3.5" />
                            <span>打开 {b.artifactLocator}</span>
                            <ExternalLink className="h-3 w-3" />
                          </a>
                        ))}

                        {((d.ryugiBindings?.length ?? 0) > 0 || d.ryugiSyncState !== 'none') && (
                          <button
                            type="button"
                            disabled={syncMutation.isPending}
                            onClick={() => syncMutation.mutate(d.deliveryId)}
                            className="flex min-h-[40px] items-center gap-1.5 rounded-md border border-sky-500/40 bg-sky-500/10 px-3 text-xs font-medium text-sky-600 dark:text-sky-400"
                          >
                            <RefreshCw
                              className={`h-3.5 w-3.5 ${syncMutation.isPending ? 'animate-spin' : ''}`}
                            />
                            <span>立即同步批注</span>
                          </button>
                        )}

                        <button
                          type="button"
                          onClick={() => setSelectedDelivery(d.deliveryId, 'diff')}
                          className="min-h-[40px] rounded-md border border-zinc-200 px-3 text-xs dark:border-zinc-700"
                        >
                          查看变更 ({d.manifest.filesChanged} 文件)
                        </button>

                        {hasOpenInbox && (
                          <button
                            type="button"
                            disabled={completeReviewMutation.isPending}
                            onClick={() => completeReviewMutation.mutate(d.deliveryId)}
                            className="flex min-h-[40px] items-center gap-1 rounded-md border border-emerald-500/40 bg-emerald-500/10 px-3 text-xs font-medium text-emerald-700 dark:text-emerald-300"
                          >
                            <Check className="h-3.5 w-3.5" />
                            <span>完成审阅</span>
                          </button>
                        )}
                      </div>

                      {/* Sync Status Banner */}
                      {syncReqErr && (
                        <div className="mt-2 flex items-center gap-1.5 rounded border border-rose-500/30 bg-rose-500/10 px-2.5 py-1.5 text-xs text-rose-600 dark:text-rose-400">
                          <XCircle className="h-3.5 w-3.5 shrink-0" />
                          <span>同步失败：{syncReqErr}</span>
                        </div>
                      )}
                      {syncRes && (
                        <div
                          className={`mt-2 rounded border px-2.5 py-1.5 text-xs ${
                            !syncRes.errors?.length
                              ? 'border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300'
                              : syncRes.bindingsSynced > 0
                                ? 'border-amber-500/40 bg-amber-500/10 text-amber-800 dark:text-amber-300'
                                : 'border-rose-500/40 bg-rose-500/10 text-rose-700 dark:text-rose-300'
                          }`}
                        >
                          <div className="flex items-center gap-1.5">
                            {!syncRes.errors?.length ? (
                              <CheckCircle2 className="h-3.5 w-3.5 shrink-0" />
                            ) : syncRes.bindingsSynced > 0 ? (
                              <AlertTriangle className="h-3.5 w-3.5 shrink-0" />
                            ) : (
                              <XCircle className="h-3.5 w-3.5 shrink-0" />
                            )}
                            <span>
                              新增 {syncRes.newAnnotations} 条批注，已 ack {syncRes.receiptsAcked}
                              {syncRes.errors?.length ? `（${syncRes.errors.length} 项错误）` : ''}
                            </span>
                          </div>
                          {syncRes.errors && syncRes.errors.length > 0 && (
                            <p className="mt-1 font-mono text-[11px]">{syncRes.errors.join('; ')}</p>
                          )}
                        </div>
                      )}

                      {/* Mobile Annotations List (Ryugi + Diff) */}
                      {dlvAnns.length > 0 && (
                        <div className="mt-2.5 space-y-1.5 border-t border-zinc-200/80 pt-2.5 dark:border-zinc-800">
                          <div className="flex items-center gap-1 text-[11px] font-medium text-zinc-500">
                            <MessageSquare className="h-3 w-3" />
                            <span>批注 ({dlvAnns.length})</span>
                          </div>
                          {dlvAnns.map((ann) => (
                            <div
                              key={ann.annotationId}
                              className="rounded border border-zinc-200/80 bg-zinc-50 px-2.5 py-1.5 text-xs dark:border-zinc-800 dark:bg-zinc-950/70"
                            >
                              <div className="font-mono text-[10px] text-zinc-500">
                                {ann.source === 'ryugi'
                                  ? `Ryugi · ${ann.anchor.artifact ?? ''} (${ann.anchor.section ?? ''})`
                                  : `Diff · ${ann.anchor.path ?? ''}:${ann.anchor.startLine ?? ''}`}
                              </div>
                              <p className="mt-0.5 whitespace-pre-wrap text-zinc-800 dark:text-zinc-200">
                                {ann.body}
                              </p>
                            </div>
                          ))}
                        </div>
                      )}
                    </div>
                  );
                })}
              </div>
            )}

            {deliveryDetail && deliveryDetail.diffFiles.length > 0 && (
              <div className="rounded-lg border border-zinc-200 p-3 dark:border-zinc-800">
                <div className="mb-1.5 text-xs font-semibold">变更文件摘要</div>
                <div className="space-y-1 font-mono text-xs">
                  {deliveryDetail.diffFiles.map((f) => (
                    <div key={f.path} className="flex justify-between gap-2">
                      <span className="truncate">{f.path}</span>
                      <span className="shrink-0 text-zinc-500">
                        +{f.insertions} -{f.deletions}
                      </span>
                    </div>
                  ))}
                </div>
              </div>
            )}
          </div>
        )}

        {mobileTab === 'pa' && (
          <div className="space-y-3">
            <div className="rounded-lg border border-zinc-200 bg-white p-4 dark:border-zinc-800 dark:bg-zinc-900">
              <div className="flex items-center gap-2 text-sm font-semibold">
                <Command className="h-4 w-4 text-sky-500" />
                <span>全局 PA 控制口（Phase 3 接入）</span>
              </div>
              <p className="mt-1.5 text-xs leading-relaxed text-zinc-500">
                PA 将以统一事件日志增量水位驱动，支持 Brief Me 简报、跨项目派单与起草受审批部署，不抢占项目审阅主干道。
              </p>
              <textarea
                value={paInput}
                onChange={(e) => setPaInput(e.target.value)}
                rows={3}
                placeholder="Phase 3 上线后可在此一句话调度全局 PA..."
                className="mt-3 w-full rounded-md border border-zinc-200 bg-zinc-50 px-3 py-2 text-base text-zinc-900 dark:border-zinc-700 dark:bg-zinc-950 dark:text-zinc-100"
              />
            </div>
          </div>
        )}
      </div>

      {/* Fixed Bottom 3-Tab Mobile Navigation Bar */}
      <nav className="fixed inset-x-0 bottom-0 z-30 flex border-t border-zinc-200 bg-white/95 pb-[env(safe-area-inset-bottom)] backdrop-blur dark:border-zinc-800 dark:bg-zinc-950/95">
        <button
          type="button"
          onClick={() => setMobileTab('inbox')}
          className={`flex min-h-[52px] flex-1 flex-col items-center justify-center gap-0.5 text-[11px] ${
            mobileTab === 'inbox' ? 'text-sky-500 font-medium' : 'text-zinc-500'
          }`}
        >
          <Inbox className="h-4 w-4" />
          <span>收件箱{inboxItems.length > 0 ? ` (${inboxItems.length})` : ''}</span>
        </button>
        <button
          type="button"
          onClick={() => setMobileTab('projects')}
          className={`flex min-h-[52px] flex-1 flex-col items-center justify-center gap-0.5 text-[11px] ${
            mobileTab === 'projects' ? 'text-sky-500 font-medium' : 'text-zinc-500'
          }`}
        >
          <FolderGit2 className="h-4 w-4" />
          <span>项目</span>
        </button>
        <button
          type="button"
          onClick={() => setMobileTab('pa')}
          className={`flex min-h-[52px] flex-1 flex-col items-center justify-center gap-0.5 text-[11px] ${
            mobileTab === 'pa' ? 'text-sky-500 font-medium' : 'text-zinc-500'
          }`}
        >
          <Command className="h-4 w-4" />
          <span>PA</span>
        </button>
      </nav>
    </div>
  );
}
