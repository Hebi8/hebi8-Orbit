import type { EventRecord } from '../generated/protocol';

const STREAMABLE_RAW_ACP_KINDS = new Set<string>([
  'acp.message_batch',
  'acp.thought_batch',
  'acp.tool_call',
  'acp.tool_call_update',
]);

export const ATTEMPT_RAW_PAGE_SIZE = 500;
export const ATTEMPT_RAW_MAX_PAGES_PER_PASS = 20;

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

export interface AttemptHttpSyncState {
  /** Highest seq continuously covered by sequential HTTP GET /api/attempts/:id/raw pages (NEVER advanced by WS frames). */
  httpCursor: number;
  /** True only after an HTTP pass finishes on a short page (< pageSize) with no mid-flight catchup bump. */
  caughtUp: boolean;
  /** Incremented whenever watch_ack or WS reconnect requires an HTTP catchup pass starting from httpCursor. */
  catchupGen: number;
  /** Highest Hub seq observed via watch_ack / reconnect that HTTP must cover through. */
  requiredSeq: number;
}

export interface AttemptRawSyncController {
  getState: (attemptId: string) => AttemptHttpSyncState;
  isCaughtUp: (attemptId: string | undefined) => boolean;
  markNeedsCatchup: (attemptId: string, targetSeq?: number) => void;
  reset: (attemptId: string) => void;
  clear: () => void;
  fetchNextPass: (
    attemptId: string,
    fetchPage: (attemptId: string, afterSeq: number, limit: number) => Promise<{ events?: EventRecord[] }>,
    opts?: { pageSize?: number; maxPages?: number },
  ) => Promise<{ fetched: EventRecord[]; httpCursor: number; needsMore: boolean }>;
}

const MAX_TRACKED_ATTEMPTS = 500;

/**
 * Maintains strictly contiguous HTTP pagination cursors per attemptId, completely isolated from
 * high-seq live WS pushes:
 * - WS events merge directly into the cache WITHOUT advancing `httpCursor`, so gaps between
 *   an initial HTTP fetch and a later `watch_ack` are always filled cleanly from `httpCursor`.
 * - Long attempts exceeding `maxPages * pageSize` frames stay `caughtUp = false` and return
 *   `needsMore = true` so callers schedule continuation passes until a short page is reached.
 * - `reset(attemptId)` drops cursor state when React Query GCs/clears the cached query so reopening
 *   always rehydrates from `afterSeq=0`.
 */
export function createAttemptRawSyncController(): AttemptRawSyncController {
  const states = new Map<string, AttemptHttpSyncState>();

  const getState = (attemptId: string): AttemptHttpSyncState => {
    let st = states.get(attemptId);
    if (!st) {
      st = { httpCursor: 0, caughtUp: false, catchupGen: 0, requiredSeq: 0 };
      states.set(attemptId, st);
      if (states.size > MAX_TRACKED_ATTEMPTS) {
        const oldest = states.keys().next().value;
        if (oldest && oldest !== attemptId) states.delete(oldest);
      }
    }
    return st;
  };

  return {
    getState,
    isCaughtUp(attemptId: string | undefined) {
      if (!attemptId) return false;
      const st = states.get(attemptId);
      return !!st?.caughtUp;
    },
    markNeedsCatchup(attemptId: string, targetSeq = 0) {
      if (!attemptId) return;
      const st = getState(attemptId);
      st.caughtUp = false;
      st.catchupGen += 1;
      if (targetSeq > st.requiredSeq) {
        st.requiredSeq = targetSeq;
      }
    },
    reset(attemptId: string) {
      if (!attemptId) return;
      states.delete(attemptId);
    },
    clear() {
      states.clear();
    },
    async fetchNextPass(attemptId, fetchPage, opts) {
      const pageSize = opts?.pageSize ?? ATTEMPT_RAW_PAGE_SIZE;
      const maxPages = opts?.maxPages ?? ATTEMPT_RAW_MAX_PAGES_PER_PASS;
      const st = getState(attemptId);
      const startGen = st.catchupGen;
      let cursor = st.httpCursor;
      const fetched: EventRecord[] = [];
      let hitPageBudget = false;

      for (let page = 0; page < maxPages; page++) {
        const res = await fetchPage(attemptId, cursor, pageSize);
        if (states.get(attemptId) !== st) {
          // Cache was GC'd or reset while this pass was in flight; discard stale cursor progress.
          return { fetched: [], httpCursor: 0, needsMore: true };
        }
        const evs = res.events ?? [];
        for (const ev of evs) {
          if (ev.seq > cursor) {
            fetched.push(ev);
            cursor = ev.seq;
          }
        }
        if (evs.length < pageSize) {
          hitPageBudget = false;
          break;
        }
        hitPageBudget = true;
      }

      if (states.get(attemptId) !== st) {
        return { fetched: [], httpCursor: 0, needsMore: true };
      }

      if (cursor > st.httpCursor) {
        st.httpCursor = cursor;
      }
      const needsMore = hitPageBudget || st.catchupGen !== startGen;
      st.caughtUp = !needsMore;
      return {
        fetched,
        httpCursor: st.httpCursor,
        needsMore,
      };
    },
  };
}

export interface AttemptRawQueryCacheLike {
  subscribe: (
    listener: (event: { type: string; query: { queryKey: readonly unknown[] } }) => void,
  ) => () => void;
}

export interface AttemptRawQueryClientLike {
  getQueryData: <T = unknown>(queryKey: readonly unknown[]) => T | undefined;
  getQueryCache: () => AttemptRawQueryCacheLike;
}

export interface BoundAttemptRawSync {
  controller: AttemptRawSyncController;
  unsubscribe: () => void;
  fetchAndMergePass: (
    attemptId: string,
    fetchPage: (attemptId: string, afterSeq: number, limit: number) => Promise<{ events?: EventRecord[] }>,
    opts?: { pageSize?: number; maxPages?: number },
  ) => Promise<{ merged: EventRecord[]; fetched: EventRecord[]; httpCursor: number; needsMore: boolean }>;
}

/**
 * Binds an AttemptRawSyncController to a React Query QueryClient so GC eviction / manual removal /
 * cache clearing of `['attempt-raw', attemptId]` ALWAYS resets `httpCursor = 0` and `caughtUp = false`.
 */
export function bindAttemptRawSyncToQueryClient(qc: AttemptRawQueryClientLike): BoundAttemptRawSync {
  const controller = createAttemptRawSyncController();
  const unsubscribe = qc.getQueryCache().subscribe((event) => {
    if (event.type !== 'removed') return;
    const key = event.query?.queryKey;
    if (Array.isArray(key) && key[0] === 'attempt-raw' && typeof key[1] === 'string' && key[1]) {
      controller.reset(key[1]);
    }
  });

  return {
    controller,
    unsubscribe,
    async fetchAndMergePass(attemptId, fetchPage, opts) {
      // Defensive sync check: if query data was cleared (`setQueryData(..., undefined)`) without a
      // `removed` event, reset httpCursor to 0 before paginating so history is never skipped.
      if (qc.getQueryData<EventRecord[]>(['attempt-raw', attemptId]) === undefined) {
        controller.reset(attemptId);
      }
      const res = await controller.fetchNextPass(attemptId, fetchPage, opts);
      // Re-read cache after async fetch so live WS chunks that landed in-flight are never overwritten.
      const latest = qc.getQueryData<EventRecord[]>(['attempt-raw', attemptId]);
      const merged = mergeAttemptRawEvents(latest, res.fetched);
      return { ...res, merged };
    },
  };
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
