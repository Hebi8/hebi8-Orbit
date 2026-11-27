import assert from 'node:assert/strict';
import test from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import Markdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import type { ChildTaskView, EventRecord, InboxItemView, TaskSummary } from '../src/generated/protocol.ts';
import {
  createAttemptWatchRegistry,
  isStreamableRawACPEvent,
  mergeAttemptRawEvents,
} from '../src/lib/attemptStream.ts';
import {
  childSessionMark,
  collectParentSeenEntries,
  effectiveReadyAt,
  inboxItemsMark,
  indexInboxByTask,
  isChildFinishedForParentRead,
  rootSessionMark,
  sessionMark,
  type SeenTaskMap,
} from '../src/lib/format.ts';
import { classifyMdHref, safeMdUrlTransform } from '../src/lib/markdown.ts';
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
    createdAt: 1000,
    updatedAt: 1000,
    ...overrides,
  };
}

function makeInbox(overrides: Partial<InboxItemView> & { inboxId: string; taskId: string }): InboxItemView {
  return {
    projectId: 'proj-1',
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

test('seenTasks clears ready blue dot on single session while preserving failed/needs-you/running', () => {
  const readyTask = makeTask({ taskId: 'tsk_ready', state: 'in_review', updatedAt: 2000 });
  const deliveryInbox = [makeInbox({ inboxId: 'inb_1', taskId: 'tsk_ready', createdAt: 2200 })];

  // 1. Unseen task shows ready
  assert.equal(sessionMark(readyTask, deliveryInbox, {}), 'ready');
  assert.equal(effectiveReadyAt(readyTask, deliveryInbox), 2200);

  // 2. Seen earlier than delivery_review inbox createdAt still shows ready
  assert.equal(sessionMark(readyTask, deliveryInbox, { tsk_ready: 2100 }), 'ready');

  // 3. Seen at or after effectiveReadyAt clears blue dot to null (quiet resting)
  assert.equal(sessionMark(readyTask, deliveryInbox, { tsk_ready: 2200 }), null);
  assert.equal(sessionMark(readyTask, deliveryInbox, { tsk_ready: 3000 }), null);
  assert.equal(inboxItemsMark(deliveryInbox, { tsk_ready: 2200 }), null);

  // 4. Subsequent new output advancing updatedAt re-lights ready blue dot
  const updatedAgain = { ...readyTask, updatedAt: 4000 };
  assert.equal(sessionMark(updatedAgain, deliveryInbox, { tsk_ready: 3000 }), 'ready');

  // 5. failed / needs-you / running are NEVER cleared by seenTasks
  const hugeSeen: SeenTaskMap = {
    tsk_fail: 999_999,
    tsk_wait: 999_999,
    tsk_run: 999_999,
  };
  assert.equal(sessionMark(makeTask({ taskId: 'tsk_fail', state: 'failed', updatedAt: 1000 }), [], hugeSeen), 'failed');
  assert.equal(
    sessionMark(makeTask({ taskId: 'tsk_wait', state: 'waiting_approval', updatedAt: 1000 }), [], hugeSeen),
    'needs-you',
  );
  assert.equal(sessionMark(makeTask({ taskId: 'tsk_run', state: 'running', updatedAt: 1000 }), [], hugeSeen), 'running');
});

test('parent read clears finished children blue dots, preserves active/approval bleed-through, and re-lights on new child output', () => {
  const parent = makeTask({ taskId: 'tsk_parent', state: 'in_review', updatedAt: 2000 });
  const childDoneReview = makeTask({
    taskId: 'tsk_child_done',
    parentTaskId: 'tsk_parent',
    state: 'in_review',
    updatedAt: 1900,
  });
  const childRunning = makeTask({
    taskId: 'tsk_child_run',
    parentTaskId: 'tsk_parent',
    state: 'running',
    updatedAt: 1950,
  });
  const childApproval = makeTask({
    taskId: 'tsk_child_wait',
    parentTaskId: 'tsk_parent',
    state: 'waiting_approval',
    updatedAt: 1960,
  });
  const childFailed = makeTask({
    taskId: 'tsk_child_fail',
    parentTaskId: 'tsk_parent',
    state: 'failed',
    updatedAt: 1970,
  });

  const inboxByTask = indexInboxByTask([
    makeInbox({ inboxId: 'inb_p', taskId: 'tsk_parent', createdAt: 2000 }),
    makeInbox({ inboxId: 'inb_c1', taskId: 'tsk_child_done', createdAt: 2050 }),
    makeInbox({ inboxId: 'inb_c3', taskId: 'tsk_child_wait', category: 'approval_pending', createdAt: 1960 }),
  ]);

  // Parent read collects parent + finished child ONLY (never running / waiting_approval / failed)
  assert.equal(isChildFinishedForParentRead(childDoneReview, inboxByTask.get(childDoneReview.taskId)), true);
  assert.equal(isChildFinishedForParentRead(childRunning, inboxByTask.get(childRunning.taskId)), false);
  assert.equal(isChildFinishedForParentRead(childApproval, inboxByTask.get(childApproval.taskId)), false);
  assert.equal(isChildFinishedForParentRead(childFailed, inboxByTask.get(childFailed.taskId)), false);

  const entries = collectParentSeenEntries(
    parent,
    [childDoneReview, childRunning, childApproval, childFailed],
    inboxByTask,
    2100,
  );
  const seenTasks: Record<string, number> = {};
  for (const e of entries) seenTasks[e.taskId] = e.seenAtMs;

  assert.deepEqual(Object.keys(seenTasks).sort(), ['tsk_child_done', 'tsk_parent']);
  assert.equal(seenTasks.tsk_parent, 2100);
  assert.equal(seenTasks.tsk_child_done, 2100);

  // Collapsed parent with ONLY finished children: completely quiet (null) — no lingering blue dot!
  assert.equal(rootSessionMark(parent, [parent, childDoneReview], inboxByTask, seenTasks), null);
  assert.equal(
    childSessionMark(
      makeChild({ taskId: 'tsk_child_done', parentTaskId: 'tsk_parent', state: 'in_review', updatedAt: 1900 }),
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

  // When a previously seen child produces a new turn later (updatedAt > seenAt), blue dot re-lights on BOTH child & collapsed parent!
  const childNewOutput = { ...childDoneReview, updatedAt: 3500 };
  assert.equal(
    childSessionMark(
      makeChild({ taskId: 'tsk_child_done', parentTaskId: 'tsk_parent', state: 'in_review', updatedAt: 3500 }),
      inboxByTask.get('tsk_child_done'),
      seenTasks,
    ),
    'ready',
  );
  assert.equal(rootSessionMark(parent, [parent, childNewOutput], inboxByTask, seenTasks), 'ready');
});

test('attemptStream merges seq-ordered raw chunks in-place without duplicates and tracks ref-counted watch sets', () => {
  const e1 = makeEvent({ seq: 10, kind: 'acp.message_batch', payload: { text: 'Hello ' } });
  const e2 = makeEvent({ seq: 11, kind: 'acp.message_batch', payload: { text: 'world' } });
  const e3 = makeEvent({ seq: 12, kind: 'acp.tool_call', payload: { toolCallId: 'tc1', name: 'read', status: 'in_progress' } });
  const e4 = makeEvent({ seq: 13, kind: 'acp.tool_call_update', payload: { toolCallId: 'tc1', status: 'completed' } });

  assert.equal(isStreamableRawACPEvent(e1), true);
  assert.equal(isStreamableRawACPEvent(e4), true);
  assert.equal(isStreamableRawACPEvent(makeEvent({ seq: 14, kind: 'acp.usage' })), false);

  const m1 = mergeAttemptRawEvents(undefined, [e1, e2]);
  assert.deepEqual(
    m1.map((e) => e.seq),
    [10, 11],
  );

  // Duplicate returns exact same reference
  const dup = mergeAttemptRawEvents(m1, [e2]);
  assert.equal(dup, m1);

  // Out-of-order + fast-append merge stays strictly ascending and deduplicated
  const m2 = mergeAttemptRawEvents(m1, [e4, e3, e1]);
  assert.deepEqual(
    m2.map((e) => e.seq),
    [10, 11, 12, 13],
  );

  // segments.ts merges acp.tool_call + acp.tool_call_update by toolCallId
  const segs = buildSegments(m2);
  assert.equal(segs.length, 2);
  assert.equal(segs[0].kind, 'msg');
  if (segs[0].kind === 'msg') assert.equal(segs[0].text, 'Hello world');
  assert.equal(segs[1].kind, 'tools');
  if (segs[1].kind === 'tools') {
    assert.equal(segs[1].steps.length, 1);
    assert.equal(segs[1].steps[0].status, 'completed');
  }

  // Reference-counted watch registry notifies only on unique set transitions
  const reg = createAttemptWatchRegistry();
  const observed: string[][] = [];
  const unsub = reg.subscribe((ids) => observed.push(ids));
  const rel1 = reg.retain('att_a');
  const rel2 = reg.retain('att_a'); // second mount of same attempt -> no duplicate notify
  const rel3 = reg.retain('att_b');
  rel1(); // still held by rel2 -> no notify
  rel2(); // drops att_a -> notifies ['att_b']
  rel3(); // drops att_b -> notifies []
  unsub();

  assert.deepEqual(observed, [['att_a'], ['att_a', 'att_b'], ['att_b'], []]);
});

test('GFM Markdown pipeline renders tables, adjacent heading+list, h4, quotes, hr, checkboxes, unclosed fences, and blocks XSS', () => {
  assert.equal(classifyMdHref('#/t/tsk_123'), 'internal');
  assert.equal(classifyMdHref('https://example.com/docs'), 'external');
  assert.equal(classifyMdHref('javascript:alert(1)'), 'unsafe');
  assert.equal(classifyMdHref('data:text/html,<script>alert(1)</script>'), 'unsafe');
  assert.equal(safeMdUrlTransform('javascript:alert(1)', 'href'), '');
  assert.equal(safeMdUrlTransform('#/t/tsk_1', 'href'), '#/t/tsk_1');

  const mdSample = [
    '#### 四级标题紧跟列表',
    '- 第一项 *斜体* 与 ~~删除线~~',
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
  assert.match(html, /<pre><code class="language-ts">const streaming = true;\n?<\/code><\/pre>/);
  // Raw HTML script MUST be escaped as text, NEVER executed as an HTML tag
  assert.doesNotMatch(html, /<script>/i);
  assert.match(html, /&lt;script&gt;alert\(&quot;xss&quot;\)&lt;\/script&gt;/);
});
