export const SEEN_TASKS_STORAGE_KEY = 'orbit.seenTasks.v2';
export const SEEN_TASKS_MAX = 2000;

export interface StorageLike {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
}

export interface SeenTaskEntry {
  taskId: string;
  watermark: number;
}

export function parseSeenTasksJSON(raw: string | null | undefined): Record<string, number> {
  if (!raw) return {};
  try {
    const parsed: unknown = JSON.parse(raw);
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return {};
    const out: Record<string, number> = {};
    for (const [k, v] of Object.entries(parsed as Record<string, unknown>)) {
      const id = k.trim();
      if (id && typeof v === 'number' && Number.isFinite(v) && v > 0) {
        out[id] = v;
      }
    }
    return out;
  } catch {
    return {};
  }
}

export function readSeenTasksFromStorage(
  storage: Pick<StorageLike, 'getItem'> | null | undefined,
  key = SEEN_TASKS_STORAGE_KEY,
): Record<string, number> {
  if (!storage) return {};
  try {
    return parseSeenTasksJSON(storage.getItem(key));
  } catch {
    return {};
  }
}

/** Monotonically merges `incoming` into `base` taking Math.max per taskId; returns null when unchanged. */
export function mergeSeenTaskMaps(
  base: Readonly<Record<string, number>>,
  incoming: Readonly<Record<string, number>>,
  max = SEEN_TASKS_MAX,
): Record<string, number> | null {
  let next: Record<string, number> | null = null;
  for (const [rawId, val] of Object.entries(incoming)) {
    const id = rawId.trim();
    if (!id || typeof val !== 'number' || !Number.isFinite(val) || val <= 0) continue;
    const prev = (next ?? base)[id] ?? 0;
    if (val > prev) {
      if (!next) next = { ...base };
      next[id] = val;
    }
  }
  if (!next) return null;
  return pruneSeenTaskMap(next, max);
}

export function pruneSeenTaskMap(map: Record<string, number>, max = SEEN_TASKS_MAX): Record<string, number> {
  const keys = Object.keys(map);
  if (keys.length <= max) return map;
  keys.sort((a, b) => map[a] - map[b]);
  for (const k of keys.slice(0, keys.length - max)) {
    delete map[k];
  }
  return map;
}

/**
 * Multi-tab-safe read-modify-write for seenTasks:
 * 1. Reads the latest serialized map from `storage` first so concurrent tabs never clobber each other.
 * 2. Monotonically merges `currentMem`, latest `storage`, and new `entries` via Math.max.
 * 3. Writes to `storage` ONLY when the merged map advances beyond what `storage` already holds.
 * 4. Returns `nextMem` ONLY when memory strictly advances (strictly idempotent — prevents render loops).
 */
export function persistMergedSeenTasks(
  storage: StorageLike | null | undefined,
  currentMem: Readonly<Record<string, number>>,
  entries: ReadonlyArray<SeenTaskEntry>,
  key = SEEN_TASKS_STORAGE_KEY,
  max = SEEN_TASKS_MAX,
): { nextMem: Record<string, number> | null; wroteStorage: boolean } {
  if (!entries || entries.length === 0) {
    return { nextMem: null, wroteStorage: false };
  }

  const delta: Record<string, number> = {};
  let hasDelta = false;
  for (const entry of entries) {
    const id = entry.taskId?.trim();
    if (!id || !Number.isFinite(entry.watermark) || entry.watermark <= 0) continue;
    const prev = delta[id] ?? 0;
    if (entry.watermark > prev) {
      delta[id] = entry.watermark;
      hasDelta = true;
    }
  }
  if (!hasDelta) {
    return { nextMem: null, wroteStorage: false };
  }

  const stored = readSeenTasksFromStorage(storage, key);
  const combined: Record<string, number> = { ...stored };
  for (const [id, v] of Object.entries(currentMem)) {
    if (v > (combined[id] ?? 0)) combined[id] = v;
  }
  for (const [id, v] of Object.entries(delta)) {
    if (v > (combined[id] ?? 0)) combined[id] = v;
  }
  pruneSeenTaskMap(combined, max);

  let storageChanged = false;
  for (const [id, v] of Object.entries(combined)) {
    if (v > (stored[id] ?? 0)) {
      storageChanged = true;
      break;
    }
  }

  let wroteStorage = false;
  if (storageChanged && storage) {
    try {
      storage.setItem(key, JSON.stringify(combined));
      wroteStorage = true;
    } catch {
      // storage may be unavailable (private mode)
    }
  }

  let memChanged = false;
  for (const [id, v] of Object.entries(combined)) {
    if (v > (currentMem[id] ?? 0)) {
      memChanged = true;
      break;
    }
  }

  return {
    nextMem: memChanged ? combined : null,
    wroteStorage,
  };
}
