import { useEffect, useRef } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import type { EventRecord, WSServerMessage } from '../generated/protocol';
import { useWorkbenchStore } from '../store/useWorkbenchStore';

/**
 * Maintains a gapless WebSocket stream anchored ONCE at the initial bootstrap snapshotSeq:
 * - Subsequent bootstrap refetches NEVER advance lastAppliedSeqRef ahead of WS events
 *   that haven't yet invalidated timeline / delivery caches (Blocking #4).
 * - Buffers live `event` frames received during multi-page `replay` and drains them
 *   in strict ascending seq order upon `replay_end`.
 */
export function useRealtimeSync(snapshotSeq: number | undefined) {
  const queryClient = useQueryClient();
  const setWSStatus = useWorkbenchStore((s) => s.setWSStatus);
  const setLastSeq = useWorkbenchStore((s) => s.setLastSeq);

  const initializedRef = useRef<boolean>(false);
  const lastAppliedSeqRef = useRef<number>(0);

  useEffect(() => {
    if (snapshotSeq === undefined) return;
    // Only seed cursor ONCE from the initial bootstrap snapshot!
    if (initializedRef.current) return;
    initializedRef.current = true;
    lastAppliedSeqRef.current = snapshotSeq;
    setLastSeq(snapshotSeq);

    let cancelled = false;
    let ws: WebSocket | null = null;
    let retryTimer: number | null = null;
    let backoffMs = 1000;

    const applyEvents = (events: EventRecord[]) => {
      if (events.length === 0) return;

      const sorted = [...events].sort((a, b) => a.seq - b.seq);
      const affectedProjects = new Set<string>();
      const affectedTasks = new Set<string>();
      const affectedDeliveries = new Set<string>();
      let needsBootstrap = false;

      for (const ev of sorted) {
        if (ev.seq <= lastAppliedSeqRef.current) {
          continue;
        }
        lastAppliedSeqRef.current = ev.seq;
        setLastSeq(ev.seq);

        if (ev.projectId) affectedProjects.add(ev.projectId);
        if (ev.taskId) affectedTasks.add(ev.taskId);
        const dlvId = typeof ev.payload?.deliveryId === 'string' ? ev.payload.deliveryId : '';
        if (dlvId) affectedDeliveries.add(dlvId);

        if (
          ev.visibility === 'inbox' ||
          ev.kind === 'delivery.published' ||
          ev.kind === 'delivery.review_completed' ||
          ev.kind === 'task.created' ||
          ev.kind === 'task.state_changed'
        ) {
          needsBootstrap = true;
        }
      }

      if (needsBootstrap) {
        void queryClient.invalidateQueries({ queryKey: ['bootstrap'] });
      }
      for (const pid of affectedProjects) {
        void queryClient.invalidateQueries({ queryKey: ['project-tasks', pid] });
      }
      for (const tid of affectedTasks) {
        void queryClient.invalidateQueries({ queryKey: ['task-timeline', tid] });
      }
      for (const did of affectedDeliveries) {
        void queryClient.invalidateQueries({ queryKey: ['delivery', did] });
      }
    };

    const connect = () => {
      if (cancelled) return;
      setWSStatus(backoffMs === 1000 ? 'connecting' : 'reconnecting');

      const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
      const url = `${proto}//${window.location.host}/ws/client?sinceSeq=${lastAppliedSeqRef.current}`;

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
            if (
              typeof msg.replayHeadSeq === 'number' &&
              msg.replayHeadSeq > lastAppliedSeqRef.current
            ) {
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
        if (cancelled) return;
        setWSStatus('reconnecting');
        const delay = backoffMs;
        backoffMs = Math.min(10000, backoffMs * 2);
        retryTimer = window.setTimeout(connect, delay);
      };

      ws.onerror = () => {
        ws?.close();
      };
    };

    connect();

    return () => {
      cancelled = true;
      initializedRef.current = false;
      if (retryTimer !== null) {
        window.clearTimeout(retryTimer);
      }
      ws?.close();
    };
  }, [snapshotSeq, queryClient, setLastSeq, setWSStatus]);
}
