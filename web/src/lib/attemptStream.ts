import type { EventRecord } from '../generated/protocol';

const STREAMABLE_RAW_ACP_KINDS = new Set<string>([
  'acp.message_batch',
  'acp.thought_batch',
  'acp.tool_call',
  'acp.tool_call_update',
]);

/** Reports whether an EventRecord is a live raw_acp streaming frame merged directly into ['attempt-raw', attemptId]. */
export function isStreamableRawACPEvent(ev: Pick<EventRecord, 'kind' | 'attemptId'>): boolean {
  return !!ev.attemptId && STREAMABLE_RAW_ACP_KINDS.has(ev.kind);
}

/**
 * Merges incoming attempt raw events into an existing seq-sorted cache list:
 * - Deduplicates by monotonic `seq` (and `eventId` when present).
 * - Preserves strict ascending `seq` order.
 * - Returns the exact `prev` reference when no new events were added (avoids redundant React re-renders).
 */
export function mergeAttemptRawEvents(
  prev: readonly EventRecord[] | undefined,
  incoming: readonly EventRecord[],
): EventRecord[] {
  if (incoming.length === 0) {
    return prev ? (prev as EventRecord[]) : [];
  }
  const base = prev ?? [];
  if (base.length === 0) {
    return dedupeAndSortBySeq(incoming);
  }

  // Fast path: incoming is strictly newer and already ascending (normal live streaming).
  const lastPrevSeq = base[base.length - 1].seq;
  let fastAppend = true;
  let cursor = lastPrevSeq;
  for (let i = 0; i < incoming.length; i++) {
    const seq = incoming[i].seq;
    if (seq <= cursor) {
      fastAppend = false;
      break;
    }
    cursor = seq;
  }
  if (fastAppend) {
    return [...base, ...incoming];
  }

  const bySeq = new Map<number, EventRecord>();
  const seenEventIds = new Set<string>();
  for (const ev of base) {
    bySeq.set(ev.seq, ev);
    if (ev.eventId) seenEventIds.add(ev.eventId);
  }

  let changed = false;
  for (const ev of incoming) {
    if (bySeq.has(ev.seq)) continue;
    if (ev.eventId && seenEventIds.has(ev.eventId)) continue;
    bySeq.set(ev.seq, ev);
    if (ev.eventId) seenEventIds.add(ev.eventId);
    changed = true;
  }
  if (!changed) {
    return base as EventRecord[];
  }
  return [...bySeq.values()].sort((a, b) => a.seq - b.seq);
}

function dedupeAndSortBySeq(events: readonly EventRecord[]): EventRecord[] {
  const bySeq = new Map<number, EventRecord>();
  const seenEventIds = new Set<string>();
  for (const ev of events) {
    if (bySeq.has(ev.seq)) continue;
    if (ev.eventId && seenEventIds.has(ev.eventId)) continue;
    bySeq.set(ev.seq, ev);
    if (ev.eventId) seenEventIds.add(ev.eventId);
  }
  return [...bySeq.values()].sort((a, b) => a.seq - b.seq);
}

export type WatchAttemptListener = (attemptIds: string[]) => void;

export interface AttemptWatchRegistry {
  getIds: () => string[];
  retain: (attemptId: string) => () => void;
  subscribe: (listener: WatchAttemptListener) => () => void;
}

/**
 * Reference-counted registry of live attemptIds currently displayed across the UI
 * (main session, split child drawer, Hebi8, ReviewOverlay).
 */
export function createAttemptWatchRegistry(): AttemptWatchRegistry {
  const refCounts = new Map<string, number>();
  const listeners = new Set<WatchAttemptListener>();

  const getIds = () => [...refCounts.keys()].sort();
  const notify = () => {
    const ids = getIds();
    for (const fn of listeners) fn(ids);
  };

  return {
    getIds,
    retain(attemptId: string) {
      const id = attemptId.trim();
      if (!id) return () => {};
      const prev = refCounts.get(id) ?? 0;
      refCounts.set(id, prev + 1);
      if (prev === 0) notify();

      let released = false;
      return () => {
        if (released) return;
        released = true;
        const cur = refCounts.get(id) ?? 0;
        if (cur <= 1) {
          if (refCounts.delete(id)) notify();
        } else {
          refCounts.set(id, cur - 1);
        }
      };
    },
    subscribe(listener: WatchAttemptListener) {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    },
  };
}

export const attemptWatchRegistry = createAttemptWatchRegistry();
