export const SEEN_TASK_KEY_PREFIX = 'orbit.seen.v2.';
export const SEEN_TASKS_MAX = 2000;

export interface StorageLike {
  readonly length: number;
  key(index: number): string | null;
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
  removeItem(key: string): void;
}

export interface SeenTaskEntry {
  taskId: string;
  watermark: number;
}

export interface SeenTaskRecord {
  /** Hub monotonic readyVersion watermark (NEVER browser wall clock). */
  w: number;
  /** Local last-read timestamp used strictly for LRU eviction order. */
  r: number;
}

export function seenTaskStorageKey(taskId: string): string {
  return `${SEEN_TASK_KEY_PREFIX}${taskId}`;
}

export function parseSeenTaskKey(key: string | null | undefined): string | null {
  if (!key || !key.startsWith(SEEN_TASK_KEY_PREFIX)) return null;
  const taskId = key.slice(SEEN_TASK_KEY_PREFIX.length).trim();
  return taskId || null;
}

export function parseSeenTaskRecord(raw: string | null | undefined): SeenTaskRecord | null {
  if (!raw) return null;
  try {
    const parsed: unknown = JSON.parse(raw);
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return null;
    const obj = parsed as { w?: unknown; r?: unknown };
    if (typeof obj.w !== 'number' || !Number.isFinite(obj.w) || obj.w <= 0) return null;
    const r = typeof obj.r === 'number' && Number.isFinite(obj.r) && obj.r >= 0 ? obj.r : 0;
    return { w: obj.w, r };
  } catch {
    return null;
  }
}

/** Loads all per-task `orbit.seen.v2.<taskId>` records from storage into a `taskId -> watermark` map. */
export function readSeenTasksFromStorage(
  storage: Pick<StorageLike, 'length' | 'key' | 'getItem'> | null | undefined,
): Record<string, number> {
  if (!storage) return {};
  const out: Record<string, number> = {};
  try {
    const len = storage.length;
    for (let i = 0; i < len; i++) {
      const k = storage.key(i);
      const taskId = parseSeenTaskKey(k);
      if (!taskId || !k) continue;
      const rec = parseSeenTaskRecord(storage.getItem(k));
      if (rec && rec.w > (out[taskId] ?? 0)) {
        out[taskId] = rec.w;
      }
    }
  } catch {
    // storage may be unavailable (private mode)
  }
  return out;
}

/** Monotonically merges a single task watermark into `base`; returns null when unchanged (strictly idempotent). */
export function mergeSeenTaskWatermark(
  base: Readonly<Record<string, number>>,
  taskId: string,
  watermark: number,
  max = SEEN_TASKS_MAX,
): Record<string, number> | null {
  const id = taskId.trim();
  if (!id || !Number.isFinite(watermark) || watermark <= 0) return null;
  if (watermark <= (base[id] ?? 0)) return null;
  const next: Record<string, number> = { ...base };
  // Refresh insertion order so in-memory eviction (if needed) also respects recency.
  delete next[id];
  next[id] = watermark;
  pruneMemoryMapByInsertionOrder(next, max);
  return next;
}

function pruneMemoryMapByInsertionOrder(map: Record<string, number>, max: number) {
  const keys = Object.keys(map);
  if (keys.length <= max) return;
  for (const k of keys.slice(0, keys.length - max)) {
    delete map[k];
  }
}

/**
 * Evicts the least-recently-read (`r` ascending) `orbit.seen.v2.*` keys when total persisted keys
 * exceed `max`, so newly-read low-watermark tasks are NEVER evicted behind older high-version tasks.
 */
function evictOverflowSeenTasksInStorage(
  storage: StorageLike,
  max: number,
  protectedIds: ReadonlySet<string>,
): string[] {
  const records: Array<{ key: string; taskId: string; r: number }> = [];
  try {
    const len = storage.length;
    for (let i = 0; i < len; i++) {
      const k = storage.key(i);
      const taskId = parseSeenTaskKey(k);
      if (!taskId || !k) continue;
      const rec = parseSeenTaskRecord(storage.getItem(k));
      records.push({ key: k, taskId, r: rec?.r ?? 0 });
    }
  } catch {
    return [];
  }
  if (records.length <= max) return [];

  records.sort((a, b) => a.r - b.r || a.key.localeCompare(b.key));
  let toRemove = records.length - max;
  const evicted: string[] = [];
  for (const item of records) {
    if (toRemove <= 0) break;
    if (protectedIds.has(item.taskId)) continue;
    try {
      storage.removeItem(item.key);
      evicted.push(item.taskId);
      toRemove -= 1;
    } catch {
      // ignore individual storage removal errors
    }
  }
  return evicted;
}

/**
 * Per-task lock-free multi-tab-safe persistence for seenTasks:
 * 1. Each task occupies its own `orbit.seen.v2.<taskId>` key (`{ w: watermark, r: touchedAt }`),
 *    eliminating cross-tab whole-table clobbering when tabs mark different tasks concurrently.
 * 2. Before writing a task's key, reads that task's latest persisted `w` and writes ONLY when
 *    `Math.max(storedW, memW, candidateW) > storedW` (monotonic & strictly idempotent).
 * 3. When total persisted keys exceed `max`, evicts strictly by oldest `r` (LRU), never by `w`.
 */
export function persistMergedSeenTasks(
  storage: StorageLike | null | undefined,
  currentMem: Readonly<Record<string, number>>,
  entries: ReadonlyArray<SeenTaskEntry>,
  opts?: { max?: number; nowMs?: number },
): { nextMem: Record<string, number> | null; wroteStorage: boolean; evictedTaskIds: string[] } {
  const max = opts?.max ?? SEEN_TASKS_MAX;
  if (!entries || entries.length === 0) {
    return { nextMem: null, wroteStorage: false, evictedTaskIds: [] };
  }

  const delta = new Map<string, number>();
  for (const entry of entries) {
    const id = entry.taskId?.trim();
    if (!id || !Number.isFinite(entry.watermark) || entry.watermark <= 0) continue;
    const prev = delta.get(id) ?? 0;
    if (entry.watermark > prev) {
      delta.set(id, entry.watermark);
    }
  }
  if (delta.size === 0) {
    return { nextMem: null, wroteStorage: false, evictedTaskIds: [] };
  }

  const touchBase = typeof opts?.nowMs === 'number' && Number.isFinite(opts.nowMs) ? opts.nowMs : Date.now();
  let nextMem: Record<string, number> | null = null;
  let wroteStorage = false;
  const protectedIds = new Set<string>(delta.keys());

  for (const [id, candidateW] of delta) {
    const key = seenTaskStorageKey(id);
    let storedRec: SeenTaskRecord | null = null;
    if (storage) {
      try {
        storedRec = parseSeenTaskRecord(storage.getItem(key));
      } catch {
        storedRec = null;
      }
    }
    const storedW = storedRec?.w ?? 0;
    const memW = currentMem[id] ?? 0;
    const finalW = Math.max(storedW, memW, candidateW);

    if (storage && finalW > storedW) {
      const nextTouch = Math.max(touchBase, (storedRec?.r ?? 0) + 1);
      try {
        storage.setItem(key, JSON.stringify({ w: finalW, r: nextTouch } satisfies SeenTaskRecord));
        wroteStorage = true;
      } catch {
        // storage may be unavailable or full
      }
    }

    if (finalW > memW) {
      if (!nextMem) nextMem = { ...currentMem };
      delete nextMem[id];
      nextMem[id] = finalW;
    }
  }

  let evictedTaskIds: string[] = [];
  if (wroteStorage && storage) {
    evictedTaskIds = evictOverflowSeenTasksInStorage(storage, max, protectedIds);
    if (evictedTaskIds.length > 0 && nextMem) {
      for (const evictedId of evictedTaskIds) {
        delete nextMem[evictedId];
      }
    }
  }

  if (nextMem) {
    pruneMemoryMapByInsertionOrder(nextMem, max);
  }

  return { nextMem, wroteStorage, evictedTaskIds };
}
