import { useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import {
  Check,
  ChevronDown,
  ChevronRight,
  Code2,
  ExternalLink,
  FileDiff,
  GitCommitHorizontal,
  Layers,
  MessageSquare,
  Terminal,
} from 'lucide-react';
import type {
  AnnotationView,
  AttemptView,
  DeliveryView,
  InboxItemView,
  MachineView,
  ProjectView,
  TaskSummary,
  TaskTimelineResponse,
} from '../generated/protocol';
import { api } from '../lib/api';
import { buildIDEJump } from '../lib/ide';
import { useWorkbenchStore } from '../store/useWorkbenchStore';

interface TaskTimelinePaneProps {
  project?: ProjectView;
  machines: MachineView[];
  tasks: TaskSummary[];
  inboxItems: InboxItemView[];
  timeline?: TaskTimelineResponse;
  isLoadingTimeline: boolean;
}

export function TaskTimelinePane({
  project,
  machines,
  tasks,
  inboxItems,
  timeline,
  isLoadingTimeline,
}: TaskTimelinePaneProps) {
  const {
    selectedTaskId,
    setSelectedTask,
    selectedDeliveryId,
    setSelectedDelivery,
    localMachineId,
    preferredIDE,
  } = useWorkbenchStore();

  const [expandedAttempts, setExpandedAttempts] = useState<Record<string, boolean>>({});
  const [showEvents, setShowEvents] = useState(false);

  const toggleAttempt = (id: string) => {
    setExpandedAttempts((prev) => ({ ...prev, [id]: !prev[id] }));
  };

  if (!project) {
    return (
      <div className="flex flex-1 items-center justify-center p-8 text-sm text-zinc-500">
        从左侧选择一个逻辑项目开始审阅交付物
      </div>
    );
  }

  return (
    <main className="flex min-w-0 flex-1 flex-col overflow-hidden bg-white dark:bg-zinc-950">
      {/* Compact Task Selector Bar */}
      <div className="border-b border-zinc-200 px-4 py-2.5 dark:border-zinc-800/80">
        <div className="mb-1.5 flex items-center justify-between">
          <span className="text-[11px] font-semibold uppercase tracking-wider text-zinc-500 dark:text-zinc-400">
            任务列表 ({tasks.length})
          </span>
          <span className="font-mono text-[11px] text-zinc-400">{project.repoIdentity}</span>
        </div>

        {tasks.length === 0 ? (
          <div className="rounded-md border border-dashed border-zinc-200 px-3 py-3 text-xs text-zinc-500 dark:border-zinc-800">
            暂无交付任务。在本地仓库执行{' '}
            <code className="rounded bg-zinc-100 px-1.5 py-0.5 font-mono text-[11px] dark:bg-zinc-900">
              orbit deliver --project {project.projectId} --summary &quot;...&quot;
            </code>{' '}
            导入首版快照。
          </div>
        ) : (
          <div className="flex gap-1.5 overflow-x-auto pb-0.5">
            {tasks.map((t) => {
              const active = selectedTaskId === t.taskId;
              return (
                <button
                  key={t.taskId}
                  type="button"
                  onClick={() => setSelectedTask(t.taskId)}
                  className={`flex shrink-0 items-center gap-2 rounded-md border px-2.5 py-1.5 text-left text-xs transition ${
                    active
                      ? 'border-sky-500/60 bg-sky-500/10 text-zinc-950 dark:text-zinc-50'
                      : 'border-zinc-200 bg-zinc-50/70 text-zinc-600 hover:border-zinc-300 dark:border-zinc-800 dark:bg-zinc-900/60 dark:text-zinc-300 dark:hover:border-zinc-700'
                  }`}
                >
                  <span className="max-w-[200px] truncate font-medium">{t.title}</span>
                  <StateBadge state={t.state} />
                  {t.openAnnotationCount > 0 && (
                    <span className="flex items-center gap-0.5 font-mono text-[10px] text-amber-600 dark:text-amber-400">
                      <MessageSquare className="h-3 w-3" />
                      {t.openAnnotationCount}
                    </span>
                  )}
                </button>
              );
            })}
          </div>
        )}
      </div>

      {/* Center Collapsed Timeline */}
      <div className="flex-1 overflow-y-auto px-5 py-4">
        {!selectedTaskId ? (
          <div className="py-12 text-center text-xs text-zinc-500">
            选择一个任务查看时间线与不可变交付卡
          </div>
        ) : isLoadingTimeline || !timeline ? (
          <div className="py-12 text-center text-xs text-zinc-500">加载任务时间线...</div>
        ) : (
          <div className="mx-auto max-w-3xl space-y-4">
            {/* Task Goal Header */}
            <section className="rounded-lg border border-zinc-200 bg-zinc-50/60 p-3.5 dark:border-zinc-800/80 dark:bg-zinc-900/40">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <h1 className="text-sm font-semibold text-zinc-900 dark:text-zinc-100">
                  {timeline.task.title}
                </h1>
                <div className="flex items-center gap-2">
                  <StateBadge state={timeline.task.state} />
                  <span className="font-mono text-[11px] text-zinc-500">
                    {timeline.task.taskId}
                  </span>
                </div>
              </div>
              {timeline.task.goal !== timeline.task.title && (
                <p className="mt-1.5 text-xs leading-relaxed text-zinc-600 dark:text-zinc-400">
                  {timeline.task.goal}
                </p>
              )}
            </section>

            {/* Collapsed Attempt Capsules + Immutable Delivery Cards */}
            <div className="space-y-3">
              {timeline.attempts.map((att, idx) => {
                const delivery = timeline.deliveries.find((d) => d.attemptId === att.attemptId);
                const deliveryAnns = delivery
                  ? timeline.annotations.filter((a) => a.deliveryId === delivery.deliveryId)
                  : [];
                const isExpanded = !!expandedAttempts[att.attemptId];

                return (
                  <div key={att.attemptId} className="space-y-2">
                    <AttemptCapsule
                      attempt={att}
                      expanded={isExpanded}
                      onToggle={() => toggleAttempt(att.attemptId)}
                    />
                    {delivery && (
                      <DeliveryCard
                        versionNo={idx + 1}
                        delivery={delivery}
                        annotations={deliveryAnns}
                        selected={selectedDeliveryId === delivery.deliveryId}
                        hasOpenReviewInbox={inboxItems.some(
                          (it) =>
                            it.category === 'delivery_review' &&
                            it.refId === delivery.deliveryId &&
                            it.state === 'open',
                        )}
                        project={project}
                        machines={machines}
                        localMachineId={localMachineId}
                        preferredIDE={preferredIDE}
                        onOpenRyugi={() => setSelectedDelivery(delivery.deliveryId, 'ryugi')}
                        onOpenDiff={() => setSelectedDelivery(delivery.deliveryId, 'diff')}
                      />
                    )}
                  </div>
                );
              })}
            </div>

            {/* Optional Event Log Inspector */}
            <section className="pt-2">
              <button
                type="button"
                onClick={() => setShowEvents((v) => !v)}
                className="flex items-center gap-1.5 text-[11px] font-medium text-zinc-500 hover:text-zinc-800 dark:text-zinc-400 dark:hover:text-zinc-200"
              >
                {showEvents ? (
                  <ChevronDown className="h-3.5 w-3.5" />
                ) : (
                  <ChevronRight className="h-3.5 w-3.5" />
                )}
                <span>统一事件日志 ({timeline.events.length})</span>
              </button>
              {showEvents && (
                <div className="mt-2 divide-y divide-zinc-200 rounded-md border border-zinc-200 font-mono text-[11px] dark:divide-zinc-800 dark:border-zinc-800">
                  {timeline.events.map((ev) => (
                    <div
                      key={ev.eventId}
                      className="flex items-center justify-between gap-2 px-3 py-1.5 text-zinc-600 dark:text-zinc-400"
                    >
                      <div className="flex items-center gap-2 min-w-0">
                        <span className="text-zinc-400">#{ev.seq}</span>
                        <span className="rounded bg-zinc-100 px-1 py-0.5 text-[10px] dark:bg-zinc-900">
                          {ev.kind}
                        </span>
                        <span className="truncate text-zinc-500">{ev.actorId}</span>
                      </div>
                      <span className="shrink-0 text-[10px] text-zinc-400">
                        {formatTime(ev.createdAt)}
                      </span>
                    </div>
                  ))}
                </div>
              )}
            </section>
          </div>
        )}
      </div>
    </main>
  );
}

function AttemptCapsule({
  attempt,
  expanded,
  onToggle,
}: {
  attempt: AttemptView;
  expanded: boolean;
  onToggle: () => void;
}) {
  return (
    <div className="rounded-md border border-zinc-200/80 bg-zinc-50/50 dark:border-zinc-800/70 dark:bg-zinc-900/30">
      <button
        type="button"
        onClick={onToggle}
        className="flex w-full items-center justify-between gap-2 px-3 py-2 text-left text-xs text-zinc-600 hover:text-zinc-900 dark:text-zinc-400 dark:hover:text-zinc-100"
      >
        <div className="flex items-center gap-2 min-w-0">
          {expanded ? (
            <ChevronDown className="h-3.5 w-3.5 shrink-0" />
          ) : (
            <ChevronRight className="h-3.5 w-3.5 shrink-0" />
          )}
          <Terminal className="h-3.5 w-3.5 shrink-0 text-zinc-400" />
          <span className="font-medium text-zinc-800 dark:text-zinc-200">
            轮次 #{attempt.attemptNo}
          </span>
          <span className="font-mono text-[11px] text-zinc-500">
            {attempt.adapter} · {attempt.machineId}
          </span>
          <span className="rounded bg-zinc-200/70 px-1.5 py-0.5 font-mono text-[10px] text-zinc-600 dark:bg-zinc-800 dark:text-zinc-300">
            {attempt.state}
          </span>
        </div>
        <span className="shrink-0 font-mono text-[11px] text-zinc-400">
          {formatTime(attempt.startedAt)}
        </span>
      </button>

      {expanded && (
        <div className="border-t border-zinc-200/80 px-3 py-2 font-mono text-[11px] text-zinc-500 dark:border-zinc-800/70 dark:text-zinc-400 space-y-1">
          <div>attemptId: {attempt.attemptId}</div>
          <div>trigger: {attempt.triggerKind} · completion: {attempt.completionSource ?? '-'}</div>
          {attempt.baseSha && <div>reviewBaseSha: {attempt.baseSha}</div>}
          {attempt.worktreePath && <div className="truncate">worktree: {attempt.worktreePath}</div>}
        </div>
      )}
    </div>
  );
}

function DeliveryCard({
  versionNo,
  delivery,
  annotations,
  selected,
  hasOpenReviewInbox,
  project,
  machines,
  localMachineId,
  preferredIDE,
  onOpenRyugi,
  onOpenDiff,
}: {
  versionNo: number;
  delivery: DeliveryView;
  annotations: AnnotationView[];
  selected: boolean;
  hasOpenReviewInbox: boolean;
  project: ProjectView;
  machines: MachineView[];
  localMachineId: string;
  preferredIDE: ProjectView['ideScheme'];
  onOpenRyugi: () => void;
  onOpenDiff: () => void;
}) {
  const queryClient = useQueryClient();
  const completeReviewMutation = useMutation({
    mutationFn: () => api.completeDeliveryReview(delivery.deliveryId),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['bootstrap'] });
      void queryClient.invalidateQueries({ queryKey: ['task-timeline', delivery.taskId] });
    },
  });

  const shortHead = delivery.headSha.slice(0, 7);
  const shortBase = delivery.baseSha.slice(0, 7);
  const openAnns = annotations.filter((a) => a.status === 'open');
  const ideJump = buildIDEJump({
    scheme: preferredIDE || project.ideScheme,
    localMachineId,
    delivery,
    project,
    machines,
  });

  return (
    <div
      className={`rounded-lg border p-3.5 transition ${
        selected
          ? 'border-sky-500/60 bg-sky-500/[0.04] dark:border-sky-500/50 dark:bg-sky-500/[0.06]'
          : 'border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-900/50'
      }`}
    >
      <div className="flex flex-wrap items-start justify-between gap-2">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <GitCommitHorizontal className="h-4 w-4 text-sky-500" />
            <span className="text-xs font-semibold text-zinc-900 dark:text-zinc-100">
              交付 v{versionNo}
            </span>
            <span className="rounded bg-zinc-100 px-1.5 py-0.5 font-mono text-[11px] text-zinc-600 dark:bg-zinc-800 dark:text-zinc-300">
              {shortBase}..{shortHead}
            </span>
            <RyugiBadge state={delivery.ryugiSyncState} count={delivery.ryugiBindings?.length ?? 0} />
          </div>
          <p className="mt-1.5 text-xs text-zinc-700 dark:text-zinc-300">{delivery.summary}</p>
        </div>

        <div className="flex items-center gap-2 font-mono text-[11px]">
          <span className="text-emerald-600 dark:text-emerald-400">
            +{delivery.manifest.insertions}
          </span>
          <span className="text-rose-600 dark:text-rose-400">
            -{delivery.manifest.deletions}
          </span>
          <span className="text-zinc-400">{delivery.manifest.filesChanged} 文件</span>
        </div>
      </div>

      {/* Action Strip */}
      <div className="mt-3 flex flex-wrap items-center gap-2">
        <button
          type="button"
          onClick={onOpenDiff}
          className="flex items-center gap-1.5 rounded-md border border-zinc-200 bg-zinc-50 px-2.5 py-1 text-xs font-medium text-zinc-700 transition hover:border-sky-500/50 hover:text-zinc-950 dark:border-zinc-700 dark:bg-zinc-800/80 dark:text-zinc-200 dark:hover:text-white"
        >
          <FileDiff className="h-3.5 w-3.5 text-sky-500" />
          <span>轻量 Diff</span>
        </button>

        <button
          type="button"
          onClick={onOpenRyugi}
          className="flex items-center gap-1.5 rounded-md border border-zinc-200 bg-zinc-50 px-2.5 py-1 text-xs font-medium text-zinc-700 transition hover:border-sky-500/50 hover:text-zinc-950 dark:border-zinc-700 dark:bg-zinc-800/80 dark:text-zinc-200 dark:hover:text-white"
        >
          <Layers className="h-3.5 w-3.5 text-indigo-500" />
          <span>Ryugi 画布</span>
        </button>

        {ideJump && (
          <a
            href={ideJump.uri}
            title={`${ideJump.mode === 'ssh' ? 'SSH Remote' : '本地'} · ${ideJump.targetLabel}: ${ideJump.resolvedPath}`}
            className="flex items-center gap-1.5 rounded-md border border-zinc-200 px-2.5 py-1 text-xs text-zinc-600 transition hover:bg-zinc-100 dark:border-zinc-800 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-100"
          >
            <Code2 className="h-3.5 w-3.5" />
            <span>在 IDE 打开</span>
            <ExternalLink className="h-3 w-3 opacity-60" />
          </a>
        )}

        {hasOpenReviewInbox && (
          <button
            type="button"
            disabled={completeReviewMutation.isPending}
            onClick={() => completeReviewMutation.mutate()}
            className="ml-auto flex items-center gap-1 rounded-md border border-emerald-500/40 bg-emerald-500/10 px-2.5 py-1 text-xs font-medium text-emerald-700 transition hover:bg-emerald-500/20 disabled:opacity-50 dark:text-emerald-300"
          >
            <Check className="h-3.5 w-3.5" />
            <span>{completeReviewMutation.isPending ? '处理中...' : '完成审阅'}</span>
          </button>
        )}
      </div>

      {/* Open Annotations List under Delivery */}
      {openAnns.length > 0 && (
        <div className="mt-3 space-y-1.5 border-t border-zinc-200/80 pt-2.5 dark:border-zinc-800">
          <div className="text-[11px] font-medium text-zinc-500 dark:text-zinc-400">
            未处理批注 ({openAnns.length})
          </div>
          {openAnns.map((ann) => (
            <div
              key={ann.annotationId}
              className="rounded border border-zinc-200/80 bg-zinc-50/70 px-2.5 py-1.5 text-xs dark:border-zinc-800 dark:bg-zinc-950/60"
            >
              <div className="flex items-center justify-between gap-2 font-mono text-[10px] text-zinc-500">
                <span>
                  {ann.source === 'ryugi'
                    ? `Ryugi · ${ann.anchor.artifact ?? ''} (${ann.anchor.section ?? ''})`
                    : `Diff · ${ann.anchor.path ?? ''}:${ann.anchor.startLine ?? ''}`}
                </span>
                <span>{formatTime(ann.createdAt)}</span>
              </div>
              <p className="mt-0.5 text-zinc-800 dark:text-zinc-200">{ann.body}</p>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function StateBadge({ state }: { state: string }) {
  const styles: Record<string, string> = {
    in_review: 'bg-sky-500/15 text-sky-600 dark:text-sky-400',
    running: 'bg-emerald-500/15 text-emerald-600 dark:text-emerald-400',
    waiting_approval: 'bg-amber-500/15 text-amber-600 dark:text-amber-400',
    waiting_resource: 'bg-amber-500/15 text-amber-600 dark:text-amber-400',
    failed: 'bg-rose-500/15 text-rose-600 dark:text-rose-400',
    done: 'bg-zinc-200 text-zinc-600 dark:bg-zinc-800 dark:text-zinc-400',
  };
  return (
    <span
      className={`rounded px-1.5 py-0.5 font-mono text-[10px] font-medium ${
        styles[state] ?? 'bg-zinc-200 text-zinc-600 dark:bg-zinc-800 dark:text-zinc-400'
      }`}
    >
      {state}
    </span>
  );
}

function RyugiBadge({ state, count }: { state: string; count: number }) {
  if (state === 'none' && count === 0) return null;
  const cls =
    state === 'ready'
      ? 'bg-emerald-500/15 text-emerald-600 dark:text-emerald-400'
      : state === 'failed'
        ? 'bg-rose-500/15 text-rose-600 dark:text-rose-400'
        : 'bg-amber-500/15 text-amber-600 dark:text-amber-400';
  return (
    <span className={`rounded px-1.5 py-0.5 font-mono text-[10px] ${cls}`}>
      ryugi:{state} ({count})
    </span>
  );
}

function formatTime(ms: number): string {
  try {
    return new Date(ms).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  } catch {
    return '';
  }
}
