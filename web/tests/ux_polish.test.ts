import assert from 'node:assert/strict';
import test from 'node:test';
import { QueryClient } from '@tanstack/react-query';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import Markdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import type { ChildTaskView, EventRecord, InboxItemView, TaskSummary } from '../src/generated/protocol.ts';
import {
  bindAttemptRawSyncToQueryClient,
  createAttemptRawSyncController,
  createAttemptWatchRegistry,
  isStreamableRawACPEvent,
  mergeAttemptRawEvents,
} from '../src/lib/attemptStream.ts';
import {
  childSessionMark,
  collectParentSeenEntries,
  effectiveReadyWatermark,
  inboxItemsMark,
  indexInboxByTask,
  isChildFinishedForParentRead,
  rootSessionMark,
  sessionMark,
  type SeenTaskMap,
} from '../src/lib/format.ts';
import {
  classifyMdHref,
  findFootnoteTargetInContainer,
  isFootnoteHref,
  resolveFootnoteTargetIds,
  safeMdUrlTransform,
} from '../src/lib/markdown.ts';
import {
  SEEN_TASK_KEY_PREFIX,
  mergeSeenTaskWatermark,
  parseSeenTaskKey,
  parseSeenTaskRecord,
  persistMergedSeenTasks,
  readSeenTasksFromStorage,
  seenTaskStorageKey,
  type StorageLike,
} from '../src/lib/seenStorage.ts';
import { buildSegments } from '../src/lib/segments.ts';

function makeTask(overrides: Partial<TaskSummary> & { taskId: string }): TaskSummary {
  return {
    projectId: 'proj-1',
    title: overrides.taskId,
    goal: 'goal',
    requiredCapability: 'primary-dev',
    preferredAdapter: 'agy',
    state: 'done',
    openAnnotationCount: 0,
    deliveryCount: 0,
    queuedInputCount: 0,
    workerCount: 0,
    version: 1,
    readyVersion: 1,
    createdBy: 'human',
    createdAt: 1000,
    updatedAt: 1000,
    ...overrides,
  };
}

function makeChild(overrides: Partial<ChildTaskView> & { taskId: string; parentTaskId: string }): ChildTaskView {
  return {
    projectId: 'proj-1',
    title: overrides.taskId,
    goal: 'child goal',
    state: 'in_review',
    deliveryCount: 0,
    attemptCount: 1,
    openApprovalCount: 0,
    version: 1,
    readyVersion: 1,
    createdAt: 1000,
    updatedAt: 1000,
    ...overrides,
  };
}

function makeInbox(overrides: Partial<InboxItemView> & { inboxId: string; taskId: string }): InboxItemView {
  return {
    projectId: 'proj-1',
    readyVersion: 1,
    category: 'delivery_review',
    refId: 'dlv_1',
    title: 'Review ready',
    summary: '',
    priority: 1,
    state: 'open',
    createdAt: 1500,
    updatedAt: 1500,
    ...overrides,
  };
}

function makeEvent(overrides: Partial<EventRecord> & { seq: number; kind: EventRecord['kind'] }): EventRecord {
  return {
    eventId: `evt_${overrides.seq}`,
    streamId: 'task:tsk_1',
    projectId: 'proj-1',
    taskId: 'tsk_1',
    attemptId: 'att_1',
    actorKind: 'agent',
    actorId: 'agy',
    visibility: 'raw_acp',
    payload: {},
    createdAt: 1000 + overrides.seq,
    ...overrides,
  };
}

function createMemoryStorage(): StorageLike & { store: Map<string, string>; writeCount: number; removeCount: number } {
  const store = new Map<string, string>();
  const obj = {
    store,
    writeCount: 0,
    removeCount: 0,
    get length() {
      return store.size;
    },
    key(index: number) {
      if (index < 0 || index >= store.size) return null;
      return Array.from(store.keys())[index] ?? null;
    },
    getItem(key: string) {
      return store.get(key) ?? null;
    },
    setItem(key: string, value: string) {
      obj.writeCount += 1;
      store.set(key, value);
    },
    removeItem(key: string) {
      if (store.delete(key)) {
        obj.removeCount += 1;
      }
    },
  };
  return obj;
}

test('Hub monotonic readyVersion clears blue dot across clock skew, ignores archive/unarchive, re-lights on promote, and stays strictly idempotent', () => {
  // Even if browser clock is 5 minutes ahead of Hub and updatedAt is skewed backwards, readyVersion watermark is exact.
  const readyTask = makeTask({ taskId: 'tsk_ready', state: 'in_review', version: 4, readyVersion: 4, updatedAt: 1_700_000_000_000 });
  const deliveryInbox = [makeInbox({ inboxId: 'inb_1', taskId: 'tsk_ready', readyVersion: 5, createdAt: 1_699_999_000_000 })];

  // 1. Unseen task shows ready; watermark takes max(task.readyVersion, inbox.readyVersion)
  assert.equal(effectiveReadyWatermark(readyTask, deliveryInbox), 5);
  assert.equal(sessionMark(readyTask, deliveryInbox, {}), 'ready');

  // 2. Seen at lower readyVersion (4 < 5) still shows ready
  assert.equal(sessionMark(readyTask, deliveryInbox, { tsk_ready: 4 }), 'ready');

  // 3. Seen at exact Hub watermark (5) clears blue dot immediately
  assert.equal(sessionMark(readyTask, deliveryInbox, { tsk_ready: 5 }), null);
  assert.equal(inboxItemsMark(deliveryInbox, { tsk_ready: 5 }), null);

  // 4. Re-marking at the exact same Hub watermark is strictly idempotent (zero state mutation, zero storage write -> prevents #185 loop!)
  const storage = createMemoryStorage();
  const r1 = persistMergedSeenTasks(storage, {}, [{ taskId: 'tsk_ready', watermark: 5 }], { nowMs: 1000 });
  assert.deepEqual(r1.nextMem, { tsk_ready: 5 });
  assert.equal(r1.wroteStorage, true);
  assert.equal(storage.writeCount, 1);

  const r2 = persistMergedSeenTasks(storage, r1.nextMem!, [{ taskId: 'tsk_ready', watermark: 5 }], { nowMs: 2000 });
  assert.equal(r2.nextMem, null);
  assert.equal(r2.wroteStorage, false);
  assert.equal(storage.writeCount, 1);

  // 5. Archive -> Unarchive bumps row `version` (4 -> 6) WITHOUT changing `readyVersion` (5) -> MUST NOT re-light blue dot!
  const unarchivedTask = { ...readyTask, version: 6, readyVersion: 5 };
  assert.equal(sessionMark(unarchivedTask, [], r1.nextMem!), null);

  // 6. Plain-folder promote or new delivery bumps `readyVersion` (5 -> 6) -> blue dot re-lights immediately!
  const promotedTask = { ...unarchivedTask, version: 7, readyVersion: 6, updatedAt: 1_699_000_000_000 };
  assert.equal(sessionMark(promotedTask, [], r1.nextMem!), 'ready');

  // 7. failed / needs-you / running are NEVER cleared by seenTasks
  const hugeSeen: SeenTaskMap = {
    tsk_fail: 999_999,
    tsk_wait: 999_999,
    tsk_run: 999_999,
  };
  assert.equal(sessionMark(makeTask({ taskId: 'tsk_fail', state: 'failed', readyVersion: 2 }), [], hugeSeen), 'failed');
  assert.equal(
    sessionMark(makeTask({ taskId: 'tsk_wait', state: 'waiting_approval', readyVersion: 2 }), [], hugeSeen),
    'needs-you',
  );
  assert.equal(sessionMark(makeTask({ taskId: 'tsk_run', state: 'running', readyVersion: 2 }), [], hugeSeen), 'running');
});

test('parent read clears finished children blue dots, preserves active/approval bleed-through, and re-lights on new child readyVersion', () => {
  const parent = makeTask({ taskId: 'tsk_parent', state: 'in_review', readyVersion: 10 });
  const childDoneReview = makeTask({
    taskId: 'tsk_child_done',
    parentTaskId: 'tsk_parent',
    state: 'in_review',
    readyVersion: 7,
  });
  const childRunning = makeTask({
    taskId: 'tsk_child_run',
    parentTaskId: 'tsk_parent',
    state: 'running',
    readyVersion: 3,
  });
  const childApproval = makeTask({
    taskId: 'tsk_child_wait',
    parentTaskId: 'tsk_parent',
    state: 'waiting_approval',
    readyVersion: 4,
  });
  const childFailed = makeTask({
    taskId: 'tsk_child_fail',
    parentTaskId: 'tsk_parent',
    state: 'failed',
    readyVersion: 5,
  });

  const inboxByTask = indexInboxByTask([
    makeInbox({ inboxId: 'inb_p', taskId: 'tsk_parent', readyVersion: 10 }),
    makeInbox({ inboxId: 'inb_c1', taskId: 'tsk_child_done', readyVersion: 8 }),
    makeInbox({ inboxId: 'inb_c3', taskId: 'tsk_child_wait', category: 'approval_pending', readyVersion: 4 }),
  ]);

  assert.equal(isChildFinishedForParentRead(childDoneReview, inboxByTask.get(childDoneReview.taskId)), true);
  assert.equal(isChildFinishedForParentRead(childRunning, inboxByTask.get(childRunning.taskId)), false);
  assert.equal(isChildFinishedForParentRead(childApproval, inboxByTask.get(childApproval.taskId)), false);
  assert.equal(isChildFinishedForParentRead(childFailed, inboxByTask.get(childFailed.taskId)), false);

  const entries = collectParentSeenEntries(
    parent,
    [childDoneReview, childRunning, childApproval, childFailed],
    inboxByTask,
  );
  const seenTasks: Record<string, number> = {};
  for (const e of entries) seenTasks[e.taskId] = e.watermark;

  assert.deepEqual(seenTasks, { tsk_parent: 10, tsk_child_done: 8 });

  // Collapsed parent with ONLY finished children: completely quiet (null) — no lingering blue dot!
  assert.equal(rootSessionMark(parent, [parent, childDoneReview], inboxByTask, seenTasks), null);
  assert.equal(
    childSessionMark(
      makeChild({ taskId: 'tsk_child_done', parentTaskId: 'tsk_parent', state: 'in_review', readyVersion: 7 }),
      inboxByTask.get('tsk_child_done'),
      seenTasks,
    ),
    null,
  );

  // Collapsed parent STILL bleeds through running / needs-you / failed from active or failed children
  assert.equal(rootSessionMark(parent, [parent, childDoneReview, childRunning], inboxByTask, seenTasks), 'running');
  assert.equal(
    rootSessionMark(parent, [parent, childDoneReview, childRunning, childApproval], inboxByTask, seenTasks),
    'needs-you',
  );
  assert.equal(
    rootSessionMark(parent, [parent, childDoneReview, childRunning, childApproval, childFailed], inboxByTask, seenTasks),
    'failed',
  );

  // New child turn advances child.readyVersion from 8 -> 9 -> blue dot re-lights on BOTH child and collapsed parent!
  const childNewOutput = { ...childDoneReview, readyVersion: 9 };
  assert.equal(
    childSessionMark(
      makeChild({ taskId: 'tsk_child_done', parentTaskId: 'tsk_parent', state: 'in_review', readyVersion: 9 }),
      inboxByTask.get('tsk_child_done'),
      seenTasks,
    ),
    'ready',
  );
  assert.equal(rootSessionMark(parent, [parent, childNewOutput], inboxByTask, seenTasks), 'ready');
});

test('per-task seenTasks storage isolates concurrent multi-tab writes and evicts strictly by LRU touchedAt (not watermark)', () => {
  const sharedStorage = createMemoryStorage();
  let tabAMem: Record<string, number> = {};
  let tabBMem: Record<string, number> = {};

  // Tab A writes ONLY orbit.seen.v2.tsk_a
  const resA1 = persistMergedSeenTasks(sharedStorage, tabAMem, [{ taskId: 'tsk_a', watermark: 4 }], { nowMs: 100 });
  assert.ok(resA1.nextMem);
  tabAMem = resA1.nextMem;
  assert.ok(sharedStorage.getItem(seenTaskStorageKey('tsk_a')));

  // Concurrently, Tab B (holding stale empty memory!) writes ONLY orbit.seen.v2.tsk_b -> zero whole-table clobber!
  const resB1 = persistMergedSeenTasks(sharedStorage, tabBMem, [{ taskId: 'tsk_b', watermark: 7 }], { nowMs: 200 });
  assert.ok(resB1.nextMem);
  tabBMem = resB1.nextMem;
  assert.deepEqual(readSeenTasksFromStorage(sharedStorage), { tsk_a: 4, tsk_b: 7 });

  // Tab A advances tsk_a -> 6 while still unaware of tsk_b -> tsk_b remains untouched!
  const resA2 = persistMergedSeenTasks(sharedStorage, tabAMem, [{ taskId: 'tsk_a', watermark: 6 }], { nowMs: 300 });
  assert.ok(resA2.nextMem);
  tabAMem = resA2.nextMem;
  assert.deepEqual(readSeenTasksFromStorage(sharedStorage), { tsk_a: 6, tsk_b: 7 });

  // Tab B handles single-key 'storage' event for orbit.seen.v2.tsk_a
  const storageKeyA = `${SEEN_TASK_KEY_PREFIX}tsk_a`;
  const parsedTaskId = parseSeenTaskKey(storageKeyA);
  assert.equal(parsedTaskId, 'tsk_a');
  const recA = parseSeenTaskRecord(sharedStorage.getItem(storageKeyA));
  assert.equal(recA?.w, 6);
  const syncedB = mergeSeenTaskWatermark(tabBMem, parsedTaskId!, recA!.w);
  assert.deepEqual(syncedB, { tsk_b: 7, tsk_a: 6 });
  assert.equal(mergeSeenTaskWatermark(syncedB!, 'tsk_a', 5), null);
  assert.equal(mergeSeenTaskWatermark(syncedB!, 'tsk_a', 6), null);

  // LRU eviction test at 2,000-entry cap: fill 2,000 old HIGH-version tasks (w=500, r=1000..2999),
  // then persist a brand-new LOW-version task (w=1, r=9000). The new task MUST survive and the oldest-read task MUST be evicted!
  const capStorage = createMemoryStorage();
  for (let i = 0; i < 2000; i++) {
    capStorage.setItem(seenTaskStorageKey(`tsk_old_${i}`), JSON.stringify({ w: 500, r: 1000 + i }));
  }
  assert.equal(capStorage.length, 2000);

  const lruRes = persistMergedSeenTasks(
    capStorage,
    {},
    [{ taskId: 'tsk_fresh_low_version', watermark: 1 }],
    { max: 2000, nowMs: 9000 },
  );
  assert.equal(lruRes.wroteStorage, true);
  assert.equal(capStorage.length, 2000);
  assert.deepEqual(lruRes.evictedTaskIds, ['tsk_old_0']);
  assert.equal(capStorage.getItem(seenTaskStorageKey('tsk_old_0')), null, 'oldest-read task must be evicted');
  assert.ok(capStorage.getItem(seenTaskStorageKey('tsk_old_1')), 'newer old task must remain');
  const freshRec = parseSeenTaskRecord(capStorage.getItem(seenTaskStorageKey('tsk_fresh_low_version')));
  assert.equal(freshRec?.w, 1, 'newly-read low-version task MUST NOT be evicted behind high-version older tasks');
});

test('attemptStream resets httpCursor on React Query cache GC/removal or clear and recovers full history upon reopen', async () => {
  const qc = new QueryClient();
  const bound = bindAttemptRawSyncToQueryClient(qc);

  const serverEvents: EventRecord[] = [];
  for (let s = 1; s <= 101; s++) {
    serverEvents.push(makeEvent({ seq: s, attemptId: 'att_gc', kind: 'acp.message_batch', payload: { text: `m${s}` } }));
  }
  const requestedAfterSeqs: number[] = [];
  const fetchPage = async (_id: string, afterSeq: number, limit: number) => {
    requestedAfterSeqs.push(afterSeq);
    return { events: serverEvents.filter((e) => e.seq > afterSeq).slice(0, limit) };
  };

  // 1. Initial load fetches 1..101 and stores in real QueryClient cache; httpCursor advances to 101
  const initial = await qc.fetchQuery({
    queryKey: ['attempt-raw', 'att_gc'],
    queryFn: async () => (await bound.fetchAndMergePass('att_gc', fetchPage)).merged,
  });
  assert.equal(initial.length, 101);
  assert.equal(bound.controller.getState('att_gc').httpCursor, 101);
  assert.equal(bound.controller.isCaughtUp('att_gc'), true);

  // 2. Simulate 5-minute React Query GC eviction (`removeQueries` emits queryCache `removed` event!)
  qc.removeQueries({ queryKey: ['attempt-raw', 'att_gc'], exact: true });
  assert.equal(qc.getQueryData(['attempt-raw', 'att_gc']), undefined);
  assert.equal(bound.controller.getState('att_gc').httpCursor, 0, 'GC removal MUST reset httpCursor to 0');
  assert.equal(bound.controller.isCaughtUp('att_gc'), false, 'GC removal MUST reset caughtUp to false');

  // 3. Even if a live WS frame seq=102 lands right after reopen BEFORE HTTP runs, httpCursor is already 0!
  const wsEv102 = makeEvent({ seq: 102, attemptId: 'att_gc', kind: 'acp.message_batch', payload: { text: 'live102' } });
  serverEvents.push(wsEv102);
  qc.setQueryData<EventRecord[]>(['attempt-raw', 'att_gc'], (prev) => mergeAttemptRawEvents(prev, [wsEv102]));

  requestedAfterSeqs.length = 0;
  const reopened = await qc.fetchQuery({
    queryKey: ['attempt-raw', 'att_gc'],
    staleTime: 0,
    queryFn: async () => (await bound.fetchAndMergePass('att_gc', fetchPage)).merged,
  });
  assert.deepEqual(requestedAfterSeqs, [0], 'reopen after GC MUST refetch from afterSeq=0');
  assert.equal(reopened.length, 102);
  assert.equal(reopened[0].seq, 1);
  assert.equal(reopened[101].seq, 102);

  // 4. Also verify `resetQueries` (sets cached data to `undefined` WITHOUT emitting `removed`) triggers full re-read from 0!
  await qc.resetQueries({ queryKey: ['attempt-raw', 'att_gc'], exact: true });
  assert.equal(qc.getQueryData(['attempt-raw', 'att_gc']), undefined);
  requestedAfterSeqs.length = 0;
  const afterClear = await qc.fetchQuery({
    queryKey: ['attempt-raw', 'att_gc'],
    staleTime: 0,
    queryFn: async () => (await bound.fetchAndMergePass('att_gc', fetchPage)).merged,
  });
  assert.deepEqual(requestedAfterSeqs, [0], 'undefined cache data MUST reset httpCursor and refetch from 0');
  assert.equal(afterClear.length, 102);

  bound.unsubscribe();
  qc.clear();
});

test('attemptStream closes HTTP=100 / gap=101 / WS=102 window via independent httpCursor and recovers >10,005 frames without drops', async () => {
  // Part A: Gap scenario (HTTP=100, 101 committed in gap, WS pushes 102, watch_ack triggers catchup from httpCursor=100)
  const serverEventsA: EventRecord[] = [
    makeEvent({ seq: 100, kind: 'acp.message_batch', payload: { text: 'a' } }),
  ];
  const ctrlA = createAttemptRawSyncController();
  let cacheA: EventRecord[] | undefined;

  // 1. Initial HTTP pass reads seq=100 -> httpCursor = 100
  const pass1A = await ctrlA.fetchNextPass('att_gap', async (_id, afterSeq, limit) => ({
    events: serverEventsA.filter((e) => e.seq > afterSeq).slice(0, limit),
  }));
  cacheA = mergeAttemptRawEvents(cacheA, pass1A.fetched);
  assert.equal(pass1A.httpCursor, 100);
  assert.equal(pass1A.needsMore, false);
  assert.equal(ctrlA.isCaughtUp('att_gap'), true);

  // 2. Gap event 101 commits before watch took effect; live WS pushes 102 into cache (does NOT advance httpCursor!)
  const ev101 = makeEvent({ seq: 101, kind: 'acp.message_batch', payload: { text: 'b' } });
  const ev102 = makeEvent({ seq: 102, kind: 'acp.message_batch', payload: { text: 'c' } });
  serverEventsA.push(ev101, ev102);
  cacheA = mergeAttemptRawEvents(cacheA, [ev102]);
  assert.deepEqual(
    cacheA.map((e) => e.seq),
    [100, 102],
  );
  assert.equal(ctrlA.getState('att_gap').httpCursor, 100, 'WS push MUST NOT advance httpCursor');

  // 3. watch_ack(seq=101) arrives -> marks catchup needed -> next HTTP pass starts from httpCursor=100 and recovers 101!
  ctrlA.markNeedsCatchup('att_gap', 101);
  assert.equal(ctrlA.isCaughtUp('att_gap'), false);
  const requestedAfterSeqs: number[] = [];
  const pass2A = await ctrlA.fetchNextPass('att_gap', async (_id, afterSeq, limit) => {
    requestedAfterSeqs.push(afterSeq);
    return { events: serverEventsA.filter((e) => e.seq > afterSeq).slice(0, limit) };
  });
  cacheA = mergeAttemptRawEvents(cacheA, pass2A.fetched);
  assert.deepEqual(requestedAfterSeqs, [100]);
  assert.deepEqual(
    cacheA.map((e) => e.seq),
    [100, 101, 102],
  );
  assert.equal(pass2A.httpCursor, 102);
  assert.equal(ctrlA.isCaughtUp('att_gap'), true);

  // Part B: Mid-flight watch_ack arriving while Pass 1 is awaiting HTTP MUST keep needsMore=true
  const ctrlMid = createAttemptRawSyncController();
  const midPass1 = await ctrlMid.fetchNextPass('att_mid', async () => {
    ctrlMid.markNeedsCatchup('att_mid', 250);
    return { events: [makeEvent({ seq: 200, kind: 'acp.message_batch', payload: { text: 'p1' } })] };
  });
  assert.equal(midPass1.needsMore, true);
  assert.equal(ctrlMid.isCaughtUp('att_mid'), false);

  // Part C: Super-long >10,005-frame attempt (>20 pages * 500) + concurrent high-seq WS frame 10006
  const totalHistorical = 10_005;
  const wsTailEvent = makeEvent({ seq: 10_006, kind: 'acp.message_batch', payload: { text: 'tail' } });
  const ctrlLong = createAttemptRawSyncController();
  let cacheLong: EventRecord[] | undefined = mergeAttemptRawEvents(undefined, [wsTailEvent]);

  const fetchLongPage = async (_id: string, afterSeq: number, limit: number) => {
    const start = afterSeq + 1;
    const end = Math.min(totalHistorical, afterSeq + limit);
    const evs: EventRecord[] = [];
    for (let s = start; s <= end; s++) {
      evs.push(makeEvent({ seq: s, kind: 'acp.message_batch', payload: { text: 'x' } }));
    }
    return { events: evs };
  };

  // Pass 1 hits 20-page budget (1..10000) -> MUST NOT mark caughtUp, MUST return needsMore=true!
  const p1 = await ctrlLong.fetchNextPass('att_long', fetchLongPage, { pageSize: 500, maxPages: 20 });
  cacheLong = mergeAttemptRawEvents(cacheLong, p1.fetched);
  assert.equal(p1.fetched.length, 10_000);
  assert.equal(p1.httpCursor, 10_000);
  assert.equal(p1.needsMore, true);
  assert.equal(ctrlLong.isCaughtUp('att_long'), false);

  // Pass 2 continues from httpCursor=10000 (NOT wsTailEvent.seq=10006!) -> reads 10001..10005 (short page) -> caughtUp=true!
  const p2 = await ctrlLong.fetchNextPass('att_long', fetchLongPage, { pageSize: 500, maxPages: 20 });
  cacheLong = mergeAttemptRawEvents(cacheLong, p2.fetched);
  assert.equal(p2.fetched.length, 5);
  assert.equal(p2.httpCursor, 10_005);
  assert.equal(p2.needsMore, false);
  assert.equal(ctrlLong.isCaughtUp('att_long'), true);
  assert.equal(cacheLong.length, 10_006);
  for (let i = 0; i < cacheLong.length; i++) {
    assert.equal(cacheLong[i].seq, i + 1, `missing or out-of-order frame at index ${i}`);
  }
});

test('attemptStream deduplicates events, tracks ref-counted watch sets, and builds segments with tool_call_update', () => {
  const e1 = makeEvent({ seq: 10, kind: 'acp.message_batch', payload: { text: 'Hello ' } });
  const e2 = makeEvent({ seq: 11, kind: 'acp.message_batch', payload: { text: 'world' } });
  const e3 = makeEvent({ seq: 12, kind: 'acp.tool_call', payload: { toolCallId: 'tc1', name: 'read', status: 'in_progress' } });
  const e4 = makeEvent({ seq: 13, kind: 'acp.tool_call_update', payload: { toolCallId: 'tc1', status: 'completed' } });

  assert.equal(isStreamableRawACPEvent(e1), true);
  assert.equal(isStreamableRawACPEvent(e4), true);
  assert.equal(isStreamableRawACPEvent(makeEvent({ seq: 14, kind: 'acp.usage' })), false);

  const m1 = mergeAttemptRawEvents(undefined, [e1, e2]);
  const dup = mergeAttemptRawEvents(m1, [e2]);
  assert.equal(dup, m1);

  const m2 = mergeAttemptRawEvents(m1, [e4, e3, e1]);
  assert.deepEqual(
    m2.map((e) => e.seq),
    [10, 11, 12, 13],
  );

  const segs = buildSegments(m2);
  assert.equal(segs.length, 2);
  if (segs[0].kind === 'msg') assert.equal(segs[0].text, 'Hello world');
  if (segs[1].kind === 'tools') {
    assert.equal(segs[1].steps.length, 1);
    assert.equal(segs[1].steps[0].status, 'completed');
  }

  const reg = createAttemptWatchRegistry();
  const observed: string[][] = [];
  const unsub = reg.subscribe((ids) => observed.push(ids));
  const rel1 = reg.retain('att_a');
  const rel2 = reg.retain('att_a');
  const rel3 = reg.retain('att_b');
  rel1();
  rel2();
  rel3();
  unsub();

  assert.deepEqual(observed, [['att_a'], ['att_a', 'att_b'], ['att_b'], []]);
});

test('GFM Markdown pipeline renders tables, adjacent heading+list, h4, quotes, hr, checkboxes, ASCII & Chinese footnotes, unclosed fences, and blocks XSS', () => {
  assert.equal(classifyMdHref('#/t/tsk_123'), 'internal');
  assert.equal(classifyMdHref('https://example.com/docs'), 'external');
  assert.equal(classifyMdHref('#user-content-fn-1'), 'footnote');
  assert.equal(classifyMdHref('#user-content-fnref-note_1'), 'footnote');
  assert.equal(classifyMdHref('#user-content-fn-%E8%AF%B4%E6%98%8E'), 'footnote');
  assert.equal(classifyMdHref('#user-content-fnref-%E8%AF%B4%E6%98%8E'), 'footnote');
  assert.equal(classifyMdHref('#user-content-fn-说明'), 'footnote');
  assert.equal(isFootnoteHref('#fn-1'), true);
  assert.equal(isFootnoteHref('#user-content-fn-%E8%AF%B4%E6%98%8E'), true);
  assert.equal(classifyMdHref('#user-content-fn-%ZZ'), 'unsafe');
  assert.equal(classifyMdHref('#arbitrary-hash'), 'unsafe');
  assert.equal(classifyMdHref('javascript:alert(1)'), 'unsafe');
  assert.equal(classifyMdHref('data:text/html,<script>alert(1)</script>'), 'unsafe');
  assert.equal(safeMdUrlTransform('javascript:alert(1)', 'href'), '');
  assert.equal(safeMdUrlTransform('#/t/tsk_1', 'href'), '#/t/tsk_1');
  assert.equal(safeMdUrlTransform('#user-content-fn-1', 'href'), '#user-content-fn-1');
  assert.equal(
    safeMdUrlTransform('#user-content-fn-%E8%AF%B4%E6%98%8E', 'href'),
    '#user-content-fn-%E8%AF%B4%E6%98%8E',
  );
  assert.deepEqual(resolveFootnoteTargetIds('#user-content-fn-%E8%AF%B4%E6%98%8E'), [
    'user-content-fn-%E8%AF%B4%E6%98%8E',
    'user-content-fn-说明',
  ]);

  // Verify container target resolution finds both percent-encoded and decoded Chinese footnote IDs
  const encodedTarget = { id: 'user-content-fn-%E8%AF%B4%E6%98%8E' };
  const decodedTarget = { id: 'user-content-fn-说明' };
  assert.equal(
    findFootnoteTargetInContainer({ querySelectorAll: () => [encodedTarget] }, '#user-content-fn-%E8%AF%B4%E6%98%8E'),
    encodedTarget,
  );
  assert.equal(
    findFootnoteTargetInContainer({ querySelectorAll: () => [decodedTarget] }, '#user-content-fn-%E8%AF%B4%E6%98%8E'),
    decodedTarget,
  );
  assert.equal(
    findFootnoteTargetInContainer({ querySelectorAll: () => [encodedTarget] }, '#user-content-fn-%e8%af%b4%e6%98%8e'),
    encodedTarget,
  );

  const mdSample = [
    '#### 四级标题紧跟列表',
    '- 第一项 *斜体* 与 ~~删除线~~[^1] 以及中文脚注[^说明]',
    '  - 嵌套子项 `code`',
    '- [x] 已完成任务',
    '- [ ] 未完成任务',
    '',
    '> 引用块内容',
    '',
    '---',
    '',
    '| 列A | 列B |',
    '| --- | --- |',
    '| 值1 | 值2 |',
    '',
    '[^1]: 脚注说明内容',
    '[^说明]: 中文脚注详情',
    '',
    '<script>alert("xss")</script>',
    '',
    '```ts',
    'const streaming = true;',
  ].join('\n');

  const html = renderToStaticMarkup(
    React.createElement(Markdown, { remarkPlugins: [remarkGfm], urlTransform: safeMdUrlTransform }, mdSample),
  );

  assert.match(html, /<h4>四级标题紧跟列表<\/h4>/);
  assert.match(html, /<ul>/);
  assert.match(html, /<em>斜体<\/em>/);
  assert.match(html, /<del>删除线<\/del>/);
  assert.match(html, /type="checkbox"/);
  assert.match(html, /<blockquote>/);
  assert.match(html, /<hr\s*\/>/);
  assert.match(html, /<table>.*<th>列A<\/th>.*<td>值2<\/td>.*<\/table>/s);
  assert.match(html, /href="#user-content-fn-1"/);
  assert.match(html, /id="user-content-fn-1"/);
  assert.match(html, /href="#user-content-fnref-1"/);
  assert.match(html, /href="#user-content-fn-%E8%AF%B4%E6%98%8E"/i);
  assert.match(html, /href="#user-content-fnref-%E8%AF%B4%E6%98%8E"/i);
  assert.match(html, /<pre><code class="language-ts">const streaming = true;\n?<\/code><\/pre>/);
  assert.doesNotMatch(html, /<script>/i);
  assert.match(html, /&lt;script&gt;alert\(&quot;xss&quot;\)&lt;\/script&gt;/);
});
