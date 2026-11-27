import { useEffect, useRef } from 'react';
import { useQueryClient, type QueryClient } from '@tanstack/react-query';
import type {
  BootstrapResponse,
  EventRecord,
  ListChildrenResponse,
  TaskSummary,
  TaskTimelineResponse,
  WSServerMessage,
} from '../generated/protocol';
import { api, getStoredClientToken, getStoredDevToken } from '../lib/api';
import { useWorkbenchStore } from '../store/useWorkbenchStore';

const strField = (payload: Record<string, unknown> | undefined, key: string): string => {
  const v = payload?.[key];
  return typeof v === 'string' ? v : '';
};

const strListField = (payload: Record<string, unknown> | undefined, key: string): string[] => {
  const v = payload?.[key];
  if (!Array.isArray(v)) return [];
  return v.filter((item): item is string => typeof item === 'string' && item.length > 0);
};

function findCachedParentTaskId(queryClient: QueryClient | undefined, taskId: string): string {
  if (!queryClient || !taskId) return '';
  const tl = queryClient.getQueryData<TaskTimelineResponse>(['task-timeline', taskId]);
  if (tl?.task?.parentTaskId) return tl.task.parentTaskId;
  if (tl?.parentSummary?.taskId) return tl.parentSummary.taskId;

  const boot = queryClient.getQueryData<BootstrapResponse>(['bootstrap']);
  const fromBoot = boot?.activeTasks?.find((t) => t.taskId === taskId);
  if (fromBoot?.parentTaskId) return fromBoot.parentTaskId;

  const childQueries = queryClient.getQueriesData<ListChildrenResponse>({ queryKey: ['task-children'] });
  for (const [, data] of childQueries) {
    if (data?.children?.some((c) => c.taskId === taskId)) {
      return data.parentTaskId;
    }
  }

  const allTaskQueries = queryClient.getQueriesData<TaskSummary[]>({ queryKey: ['all-tasks'] });
  for (const [, list] of allTaskQueries) {
    const match = list?.find((t) => t.taskId === taskId);
    if (match?.parentTaskId) return match.parentTaskId;
  }

  const projTaskQueries = queryClient.getQueriesData<TaskSummary[]>({ queryKey: ['project-tasks'] });
  for (const [, list] of projTaskQueries) {
    const match = list?.find((t) => t.taskId === taskId);
    if (match?.parentTaskId) return match.parentTaskId;
  }

  return '';
}

/**
 * Maps a batch of committed events to the precise set of TanStack Query keys that must be refreshed.
 * Exported separately so the mapping stays a pure function of the event list (with optional parent resolver).
 */
export function collectInvalidations(
  events: EventRecord[],
  resolveParentTaskId?: (taskId: string) => string,
): unknown[][] {
  const keys = new Map<string, unknown[]>();
  const add = (...key: unknown[]) => {
    if (key.some((k) => k === '' || k === undefined)) return;
    keys.set(JSON.stringify(key), key);
  };

  for (const ev of events) {
    const pid = ev.projectId ?? '';
    const tid = ev.taskId ?? '';
    const aid = ev.attemptId ?? '';
    const p = ev.payload ?? {};
    const deliveryId = strField(p, 'deliveryId');
    const childTaskId = strField(p, 'childTaskId');
    const parentTaskId = strField(p, 'parentTaskId') || (tid && resolveParentTaskId ? resolveParentTaskId(tid) : '');
    const kind = ev.kind;

    if (ev.visibility === 'inbox') add('bootstrap');

    if (kind === 'task.child_spawned' || kind === 'task.child_updated') {
      add('task-timeline', tid);
      add('task-children', tid);
      if (childTaskId) {
        add('task-timeline', childTaskId);
        add('task-children', childTaskId);
      }
      if (parentTaskId) {
        add('task-timeline', parentTaskId);
        add('task-children', parentTaskId);
      }
      if (pid === '_pa' || strField(p, 'parentProjectId') === '_pa') add('hebi8-main');
      add('all-tasks');
      add('project-tasks');
      add('bootstrap');
      continue;
    }

    if (kind.startsWith('task.')) {
      add('task-timeline', tid);
      add('task-children', tid);
      if (parentTaskId) {
        add('task-timeline', parentTaskId);
        add('task-children', parentTaskId);
      }
      if (kind === 'task.source_added' || kind === 'task.source_removed') continue;
      add('hebi8-main');
      add('bootstrap');
      add('project-tasks', pid);
      add('all-tasks');
      add('archived-tasks');
      add('global-artifacts');
      if (kind === 'task.project_bound') {
        add('project-tasks', strField(p, 'fromProjectId'));
        add('project-tasks', strField(p, 'toProjectId'));
      }
      continue;
    }
    if (kind.startsWith('attempt.')) {
      add('task-timeline', tid);
      add('task-children', tid);
      add('attempt-raw', aid);
      add('project-tasks', pid);
      add('all-tasks');
      if (parentTaskId) {
        add('task-timeline', parentTaskId);
        add('task-children', parentTaskId);
      }
      add('hebi8-main');
      add('bootstrap');
      if (kind === 'attempt.started') {
        add('delivery');
      }
      for (const did of strListField(p, 'deliveryIds')) add('delivery', did);
      for (const did of strListField(p, 'revertedDeliveryIds')) add('delivery', did);
      continue;
    }
    if (kind.startsWith('resource.')) {
      add('bootstrap');
      add('task-timeline', tid);
      add('project-tasks', pid);
      if (parentTaskId) {
        add('task-timeline', parentTaskId);
        add('task-children', parentTaskId);
      }
      continue;
    }
    if (kind === 'delegation.sent') {
      add('task-timeline', tid);
      const targetTaskId = strField(p, 'targetTaskId');
      if (targetTaskId) add('task-timeline', targetTaskId);
      continue;
    }
    if (kind === 'delivery.review_reopened' || kind === 'delivery.review_completed') {
      if (deliveryId) add('delivery', deliveryId);
      else add('delivery');
      add('task-timeline', tid);
      if (parentTaskId) {
        add('task-timeline', parentTaskId);
        add('task-children', parentTaskId);
      }
      add('project-tasks', pid);
      add('all-tasks');
      add('hebi8-main');
      add('bootstrap');
      continue;
    }
    if (kind.startsWith('delivery.')) {
      add('delivery', deliveryId);
      add('delivery-diff', deliveryId);
      add('task-timeline', tid);
      if (parentTaskId) {
        add('task-timeline', parentTaskId);
        add('task-children', parentTaskId);
      }
      add('project-tasks', pid);
      add('all-tasks');
      add('global-artifacts');
      add('hebi8-main');
      add('bootstrap');
      continue;
    }
    if (kind === 'machine.state_changed' || kind === 'machine.agents_updated' || kind === 'pairing.updated') {
      add('bootstrap');
      add('machines');
      continue;
    }
    if (kind === 'project.changed' || kind === 'network.settings_changed') {
      add('bootstrap');
      add('network-settings');
      continue;
    }
    if (kind.startsWith('annotation.')) {
      add('delivery', deliveryId);
      add('delivery', strField(p, 'resolutionDeliveryId'));
      for (const did of strListField(p, 'deliveryIds')) add('delivery', did);
      add('task-timeline', tid);
      add('project-tasks', pid);
      add('global-artifacts');
      add('bootstrap');
      continue;
    }
    if (kind.startsWith('approval.')) {
      add('approvals');
      add('task-timeline', tid);
      add('task-children', tid);
      add('project-tasks', pid);
      add('all-tasks');
      if (parentTaskId) {
        add('task-timeline', parentTaskId);
        add('task-children', parentTaskId);
      }
      add('hebi8-main');
      add('bootstrap');
      continue;
    }
    if (kind.startsWith('deploy.')) {
      add('task-timeline', tid);
      add('bootstrap');
      continue;
    }
    if (kind.startsWith('acp.')) {
      add('attempt-raw', aid);
      add('worker-stream', tid);
      if (kind === 'acp.usage') {
        add('task-timeline', tid);
      }
      continue;
    }
    if (kind === 'queue.updated') {
      add('task-timeline', tid);
      add('project-tasks', pid);
      add('bootstrap');
      const action = strField(p, 'action');
      if (action === 'enqueued' || action === 'steered' || action === 'annotation_batch_queued') {
        add('delivery');
      }
      for (const did of strListField(p, 'deliveryIds')) add('delivery', did);
      const dispatchedAttempt = strField(p, 'dispatchedAttemptId');
      if (dispatchedAttempt) add('attempt-raw', dispatchedAttempt);
      continue;
    }
    if (kind === 'board.message_posted') {
      add('task-board', tid);
      if (parentTaskId) add('task-board', parentTaskId);
      add('worker-stream', tid);
      continue;
    }
    if (kind === 'board.member_updated') {
      add('task-board', tid);
      add('task-timeline', tid);
      add('task-children', tid);
      add('worker-stream', tid);
      add('project-tasks', pid);
      add('bootstrap');
      continue;
    }
    if (kind === 'pa.turn' || kind === 'pa.thread_created' || kind === 'pa.action_executed') {
      add('pa-threads');
      add('pa-thread');
      add('bootstrap');
      continue;
    }
    if (kind === 'hebi8.folded') {
      add('task-timeline', tid);
      add('hebi8-main');
      add('hebi8-line');
      add('bootstrap');
      continue;
    }
  }
  return [...keys.values()];
}

function applyInvalidations(queryClient: QueryClient, keys: unknown[][]) {
  for (const key of keys) {
    void queryClient.invalidateQueries({ queryKey: key });
  }
}

/**
 * Maintains a persistent, gapless WebSocket stream seeded ONCE from the initial bootstrap snapshotSeq:
 * - The effect depends only on `bootstrapReady` (boolean), NOT on `snapshotSeq`, so subsequent
 *   bootstrap invalidations/refetches never disconnect WS or skip events.
 * - Replay frames are buffered until `replay_end`; live frames that race the replay are buffered and
 *   de-duplicated by seq against `lastAppliedSeqRef`.
 * - Reconnects always carry `lastAppliedSeqRef.current`.
 */
export function useRealtimeSync(snapshotSeq: number | undefined) {
  const queryClient = useQueryClient();
  const setWSStatus = useWorkbenchStore((s) => s.setWSStatus);
  const setLastSeq = useWorkbenchStore((s) => s.setLastSeq);

  const initialSeqRef = useRef<number | null>(null);
  if (snapshotSeq !== undefined && initialSeqRef.current === null) {
    initialSeqRef.current = snapshotSeq;
  }

  const bootstrapReady = initialSeqRef.current !== null;
  const lastAppliedSeqRef = useRef<number>(0);

  useEffect(() => {
    if (!bootstrapReady || initialSeqRef.current === null) return;

    if (lastAppliedSeqRef.current < initialSeqRef.current) {
      lastAppliedSeqRef.current = initialSeqRef.current;
      setLastSeq(initialSeqRef.current);
    }

    let cancelled = false;
    let ws: WebSocket | null = null;
    let retryTimer: number | null = null;
    let backoffMs = 1000;

    const applyEvents = (events: EventRecord[]) => {
      if (events.length === 0) return;

      const sorted = [...events].sort((a, b) => a.seq - b.seq);
      const fresh: EventRecord[] = [];
      for (const ev of sorted) {
        if (ev.seq <= lastAppliedSeqRef.current) {
          continue;
        }
        lastAppliedSeqRef.current = ev.seq;
        fresh.push(ev);
      }
      if (fresh.length === 0) return;
      setLastSeq(lastAppliedSeqRef.current);
      applyInvalidations(
        queryClient,
        collectInvalidations(fresh, (taskId) => findCachedParentTaskId(queryClient, taskId)),
      );
    };

    const scheduleReconnect = () => {
      if (cancelled) return;
      setWSStatus('reconnecting');
      const delay = backoffMs;
      backoffMs = Math.min(10000, backoffMs * 2);
      retryTimer = window.setTimeout(() => void connect(), delay);
    };

    const connect = async () => {
      if (cancelled) return;
      setWSStatus(backoffMs === 1000 ? 'connecting' : 'reconnecting');

      const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
      let qs = `sinceSeq=${lastAppliedSeqRef.current}`;
      // Browsers cannot attach Bearer / X-Orbit-Dev-Token headers to WS handshakes;
      // obtain a short-lived single-use ticket via POST /api/ws-ticket first.
      if (getStoredClientToken() || getStoredDevToken()) {
        try {
          const { ticket } = await api.createWSTicket();
          if (cancelled) return;
          if (ticket) qs += `&ticket=${encodeURIComponent(ticket)}`;
        } catch {
          scheduleReconnect();
          return;
        }
      }
      if (cancelled) return;

      const url = `${proto}//${window.location.host}/ws/client?${qs}`;

      let replaying = true;
      const replayBatch: EventRecord[] = [];
      const liveBuffer: EventRecord[] = [];

      ws = new WebSocket(url);

      ws.onopen = () => {
        backoffMs = 1000;
        setWSStatus('live');
      };

      ws.onmessage = (evt) => {
        try {
          const msg = JSON.parse(String(evt.data)) as WSServerMessage;
          if (msg.type === 'replay' && msg.events) {
            replayBatch.push(...msg.events);
          } else if (msg.type === 'replay_end') {
            replaying = false;
            applyEvents(replayBatch);
            if (typeof msg.replayHeadSeq === 'number' && msg.replayHeadSeq > lastAppliedSeqRef.current) {
              lastAppliedSeqRef.current = msg.replayHeadSeq;
              setLastSeq(msg.replayHeadSeq);
            }
            applyEvents(liveBuffer);
            replayBatch.length = 0;
            liveBuffer.length = 0;
          } else if (msg.type === 'event' && msg.event) {
            if (replaying) {
              liveBuffer.push(msg.event);
            } else {
              applyEvents([msg.event]);
            }
          }
        } catch {
          // ignore malformed WS frames
        }
      };

      ws.onclose = () => {
        scheduleReconnect();
      };

      ws.onerror = () => {
        ws?.close();
      };
    };

    void connect();

    return () => {
      cancelled = true;
      if (retryTimer !== null) {
        window.clearTimeout(retryTimer);
      }
      ws?.close();
    };
  }, [bootstrapReady, queryClient, setLastSeq, setWSStatus]);
}
