import assert from 'node:assert/strict';
import test from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import Markdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import type { ChildTaskView, EventRecord, InboxItemView, TaskSummary } from '../src/generated/protocol.ts';
import {
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
import { classifyMdHref, isFootnoteHref, safeMdUrlTransform } from '../src/lib/markdown.ts';
import {
  SEEN_TASKS_STORAGE_KEY,
  mergeSeenTaskMaps,
  persistMergedSeenTasks,
  readSeenTasksFromStorage,
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
    createdAt: 1000,
    updatedAt: 1000,
    ...overrides,
  };
}

function makeInbox(overrides: Partial<InboxItemView> & { inboxId: string; taskId: string }): InboxItemView {
  return {
    projectId: 'proj-1',
    taskVersion: 1,
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

function createMemoryStorage(): StorageLike & { store: Map<string, string>; writeCount: number } {
  const store = new Map<string, string>();
  const obj = {
    store,
    writeCount: 0,
    getItem(key: string) {
      return store.get(key) ?? null;
    },
    setItem(key: string, value: string) {
      obj.writeCount += 1;
      store.set(key, value);
    },
  };
  return obj;
}

test('Hub monotonic version watermark clears ready blue dot regardless of browser/Runner clock skew and stays strictly idempotent', () => {
  // Even if browser clock is 5 minutes ahead of Hub and updatedAt is skewed backwards, version watermark is exact.
  const readyTask = makeTask({ taskId: 'tsk_ready', state: 'in_review', version: 4, updatedAt: 1_700_000_000_000 });
  const deliveryInbox = [makeInbox({ inboxId: 'inb_1', taskId: 'tsk_ready', taskVersion: 5, createdAt: 1_699_999_000_000 })];

  // 1. Unseen task shows ready; watermark takes max(task.version, inbox.taskVersion)
  assert.equal(effectiveReadyWatermark(readyTask, deliveryInbox), 5);
  assert.equal(sessionMark(readyTask, deliveryInbox, {}), 'ready');

  // 2. Seen at lower version (4 < 5) still shows ready
  assert.equal(sessionMark(readyTask, deliveryInbox, { tsk_ready: 4 }), 'ready');

  // 3. Seen at exact Hub watermark (5) clears blue dot immediately
  assert.equal(sessionMark(readyTask, deliveryInbox, { tsk_ready: 5 }), null);
  assert.equal(inboxItemsMark(deliveryInbox, { tsk_ready: 5 }), null);

  // 4. Re-marking at the exact same Hub watermark is strictly idempotent (zero state mutation, zero storage write -> prevents #185 loop!)
  const storage = createMemoryStorage();
  const r1 = persistMergedSeenTasks(storage, {}, [{ taskId: 'tsk_ready', watermark: 5 }]);
  assert.deepEqual(r1.nextMem, { tsk_ready: 5 });
  assert.equal(r1.wroteStorage, true);
  assert.equal(storage.writeCount, 1);

  const r2 = persistMergedSeenTasks(storage, r1.nextMem!, [{ taskId: 'tsk_ready', watermark: 5 }]);
  assert.equal(r2.nextMem, null);
  assert.equal(r2.wroteStorage, false);
  assert.equal(storage.writeCount, 1);

  // 5. Subsequent Hub delivery/turn bumps version to 6 (even if updatedAt went backwards due to clock skew!) -> blue dot re-lights immediately!
  const nextTurnTask = { ...readyTask, version: 6, updatedAt: 1_699_000_000_000 };
  assert.equal(sessionMark(nextTurnTask, deliveryInbox, r1.nextMem!), 'ready');

  // 6. failed / needs-you / running are NEVER cleared by seenTasks
  const hugeSeen: SeenTaskMap = {
    tsk_fail: 999_999,
    tsk_wait: 999_999,
    tsk_run: 999_999,
  };
  assert.equal(sessionMark(makeTask({ taskId: 'tsk_fail', state: 'failed', version: 2 }), [], hugeSeen), 'failed');
  assert.equal(
    sessionMark(makeTask({ taskId: 'tsk_wait', state: 'waiting_approval', version: 2 }), [], hugeSeen),
    'needs-you',
  );
  assert.equal(sessionMark(makeTask({ taskId: 'tsk_run', state: 'running', version: 2 }), [], hugeSeen), 'running');
});

test('parent read clears finished children blue dots, preserves active/approval bleed-through, and re-lights on new child version', () => {
  const parent = makeTask({ taskId: 'tsk_parent', state: 'in_review', version: 10 });
  const childDoneReview = makeTask({
    taskId: 'tsk_child_done',
    parentTaskId: 'tsk_parent',
    state: 'in_review',
    version: 7,
  });
  const childRunning = makeTask({
    taskId: 'tsk_child_run',
    parentTaskId: 'tsk_parent',
    state: 'running',
    version: 3,
  });
  const childApproval = makeTask({
    taskId: 'tsk_child_wait',
    parentTaskId: 'tsk_parent',
    state: 'waiting_approval',
    version: 4,
  });
  const childFailed = makeTask({
    taskId: 'tsk_child_fail',
    parentTaskId: 'tsk_parent',
    state: 'failed',
    version: 5,
  });

  const inboxByTask = indexInboxByTask([
    makeInbox({ inboxId: 'inb_p', taskId: 'tsk_parent', taskVersion: 10 }),
    makeInbox({ inboxId: 'inb_c1', taskId: 'tsk_child_done', taskVersion: 8 }),
    makeInbox({ inboxId: 'inb_c3', taskId: 'tsk_child_wait', category: 'approval_pending', taskVersion: 4 }),
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
      makeChild({ taskId: 'tsk_child_done', parentTaskId: 'tsk_parent', state: 'in_review', version: 7 }),
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

  // New child turn advances child.version from 8 -> 9 -> blue dot re-lights on BOTH child and collapsed parent!
  const childNewOutput = { ...childDoneReview, version: 9 };
  assert.equal(
    childSessionMark(
      makeChild({ taskId: 'tsk_child_done', parentTaskId: 'tsk_parent', state: 'in_review', version: 9 }),
      inboxByTask.get('tsk_child_done'),
      seenTasks,
    ),
    'ready',
  );
  assert.equal(rootSessionMark(parent, [parent, childNewOutput], inboxByTask, seenTasks), 'ready');
});

test('multi-tab seenTasks persistence merges via Math.max without clobbering concurrent tabs', () => {
  const sharedStorage = createMemoryStorage();
  let tabAMem: Record<string, number> = {};
  let tabBMem: Record<string, number> = {};

  // Tab A marks tsk_a = 4
  const resA1 = persistMergedSeenTasks(sharedStorage, tabAMem, [{ taskId: 'tsk_a', watermark: 4 }]);
  assert.ok(resA1.nextMem);
  tabAMem = resA1.nextMem;
  assert.deepEqual(readSeenTasksFromStorage(sharedStorage), { tsk_a: 4 });

  // Tab B (still holding stale empty memory!) marks tsk_b = 7 -> MUST preserve tsk_a = 4!
  const resB1 = persistMergedSeenTasks(sharedStorage, tabBMem, [{ taskId: 'tsk_b', watermark: 7 }]);
  assert.ok(resB1.nextMem);
  tabBMem = resB1.nextMem;
  assert.deepEqual(readSeenTasksFromStorage(sharedStorage), { tsk_a: 4, tsk_b: 7 });
  assert.deepEqual(tabBMem, { tsk_a: 4, tsk_b: 7 });

  // Tab A (stale memory without tsk_b!) advances tsk_a = 6 -> MUST preserve tsk_b = 7!
  const resA2 = persistMergedSeenTasks(sharedStorage, tabAMem, [{ taskId: 'tsk_a', watermark: 6 }]);
  assert.ok(resA2.nextMem);
  tabAMem = resA2.nextMem;
  assert.deepEqual(readSeenTasksFromStorage(sharedStorage), { tsk_a: 6, tsk_b: 7 });

  // Tab B receives storage event from Tab A -> monotonic merge brings tsk_a up to 6 without regressing tsk_b
  const syncedB = mergeSeenTaskMaps(tabBMem, readSeenTasksFromStorage(sharedStorage, SEEN_TASKS_STORAGE_KEY));
  assert.deepEqual(syncedB, { tsk_a: 6, tsk_b: 7 });
  assert.equal(mergeSeenTaskMaps(syncedB!, { tsk_a: 5, tsk_b: 7 }), null);
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

test('GFM Markdown pipeline renders tables, adjacent heading+list, h4, quotes, hr, checkboxes, footnotes, unclosed fences, and blocks XSS', () => {
  assert.equal(classifyMdHref('#/t/tsk_123'), 'internal');
  assert.equal(classifyMdHref('https://example.com/docs'), 'external');
  assert.equal(classifyMdHref('#user-content-fn-1'), 'footnote');
  assert.equal(classifyMdHref('#user-content-fnref-note_1'), 'footnote');
  assert.equal(isFootnoteHref('#fn-1'), true);
  assert.equal(classifyMdHref('#arbitrary-hash'), 'unsafe');
  assert.equal(classifyMdHref('javascript:alert(1)'), 'unsafe');
  assert.equal(classifyMdHref('data:text/html,<script>alert(1)</script>'), 'unsafe');
  assert.equal(safeMdUrlTransform('javascript:alert(1)', 'href'), '');
  assert.equal(safeMdUrlTransform('#/t/tsk_1', 'href'), '#/t/tsk_1');
  assert.equal(safeMdUrlTransform('#user-content-fn-1', 'href'), '#user-content-fn-1');

  const mdSample = [
    '#### 四级标题紧跟列表',
    '- 第一项 *斜体* 与 ~~删除线~~[^1]',
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
  assert.match(html, /<pre><code class="language-ts">const streaming = true;\n?<\/code><\/pre>/);
  assert.doesNotMatch(html, /<script>/i);
  assert.match(html, /&lt;script&gt;alert\(&quot;xss&quot;\)&lt;\/script&gt;/);
});
