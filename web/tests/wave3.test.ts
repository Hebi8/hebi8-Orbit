import assert from 'node:assert/strict';
import test from 'node:test';
import { resolveManualBoardMention, type BoardMentionCandidate } from '../src/lib/boardMention.ts';
import { createImeGuardController, IME_TRAILING_ENTER_WINDOW_MS } from '../src/lib/ime.ts';
import { resolveAgentQuota } from '../src/lib/quota.ts';
import type { MachineView } from '../src/generated/protocol.ts';

test('BoardPanel manual @mention resolves full untruncated name by longest complete target and rejects ambiguous/unknown', () => {
  const longTitle = 'A'.repeat(95) + ' Final Architecture Review Worker';
  const truncatedDisplay = `${longTitle.slice(0, 80)}…`;

  const participants: BoardMentionCandidate[] = [
    {
      key: 'child:tsk_long',
      mentionValue: 'tsk_long',
      fullName: longTitle,
      displayLabel: truncatedDisplay,
      taskId: 'tsk_long',
    },
    {
      key: 'child:tsk_short',
      mentionValue: 'tsk_short',
      fullName: 'API',
      displayLabel: 'API',
      taskId: 'tsk_short',
    },
    {
      key: 'child:tsk_longer',
      mentionValue: 'tsk_longer',
      fullName: 'API Gateway Worker',
      displayLabel: 'API Gateway Worker',
      taskId: 'tsk_longer',
    },
    {
      key: 'child:tsk_parent_rev',
      mentionValue: 'tsk_parent_rev',
      fullName: 'Parent Review',
      displayLabel: 'Parent Review',
      taskId: 'tsk_parent_rev',
    },
    {
      key: 'child:tsk_dup_1',
      mentionValue: 'tsk_dup_1',
      fullName: 'Duplicate Worker',
      displayLabel: 'Duplicate Worker',
      taskId: 'tsk_dup_1',
    },
    {
      key: 'child:tsk_dup_2',
      mentionValue: 'tsk_dup_2',
      fullName: 'Duplicate Worker',
      displayLabel: 'Duplicate Worker',
      taskId: 'tsk_dup_2',
    },
  ];

  // 1. Full untruncated >80 char title maps cleanly to child taskId
  const resLong = resolveManualBoardMention(`@${longTitle} please verify schema`, '', participants);
  assert.deepEqual(resLong, { ok: true, target: 'tsk_long', body: 'please verify schema' });

  // 2. Longest-prefix match picks "API Gateway Worker" over shorter prefix "API"
  const resPrefix = resolveManualBoardMention('@API Gateway Worker check TLS', '', participants);
  assert.deepEqual(resPrefix, { ok: true, target: 'tsk_longer', body: 'check TLS' });

  // 3. Longest complete target without body asks for body instead of falling back to shorter "API"
  const resNoBody = resolveManualBoardMention('@API Gateway Worker', '', participants);
  assert.equal(resNoBody.ok, false);
  assert.match(resNoBody.error, /输入消息正文/);

  // 4. Child named "Parent Review" beats reserved keyword "parent" by longest-prefix match
  const resParentRev = resolveManualBoardMention('@Parent Review check PR #42', '', participants);
  assert.deepEqual(resParentRev, { ok: true, target: 'tsk_parent_rev', body: 'check PR #42' });

  // 5. Exact @taskId works even for duplicate-titled children
  const resById = resolveManualBoardMention('@tsk_dup_2 run unit tests', '', participants);
  assert.deepEqual(resById, { ok: true, target: 'tsk_dup_2', body: 'run unit tests' });

  // 6. Duplicate ambiguous name fails inline before sending (does NOT misroute to parent)
  const resDup = resolveManualBoardMention('@Duplicate Worker hello', '', participants);
  assert.equal(resDup.ok, false);
  assert.match(resDup.error, /同名/);

  // 7. Unknown @mention fails inline before sending (does NOT misroute to parent)
  const resUnknown = resolveManualBoardMention('@NonExistentWorker hello', '', participants);
  assert.equal(resUnknown.ok, false);
  assert.match(resUnknown.error, /未找到提及对象/);

  // 8. @all and @parent resolve directly when no longer target matches
  assert.deepEqual(resolveManualBoardMention('@all sync now', '', participants), {
    ok: true,
    target: 'all',
    body: 'sync now',
  });
  assert.deepEqual(resolveManualBoardMention('@parent ready for review', '', participants), {
    ok: true,
    target: 'parent',
    body: 'ready for review',
  });

  // 9. Plain message preserves selected button target (child taskId directly)
  assert.deepEqual(resolveManualBoardMention('looks good', 'tsk_long', participants), {
    ok: true,
    target: 'tsk_long',
    body: 'looks good',
  });
});

test('Claude clamps shared+model 7d to tightest limit while Codex independent groups stay isolated', () => {
  const reset5h = Date.UTC(2026, 10, 28, 5, 0, 0);
  const resetShared7d = Date.UTC(2026, 10, 30, 0, 0, 0);
  const resetSonnet7d = Date.UTC(2026, 11, 2, 0, 0, 0);

  const makeMachine = (shared7dRem: number, sonnet7dRem: number): MachineView => ({
    machineId: 'hub-node',
    displayName: 'Hub',
    transportMode: 'local_ws',
    status: 'online',
    capabilities: ['primary-dev'],
    adapters: ['claude', 'codex'],
    resources: {},
    agents: [
      {
        adapter: 'claude',
        enabled: true,
        state: 'ready',
        source: 'path',
        quota: {
          adapter: 'claude',
          available: true,
          fetchedAt: Date.UTC(2026, 10, 27, 12, 0, 0),
          windows: [
            { window: '5h', label: '5h', usedPercent: 20, remainingPercent: 80, resetsAt: reset5h },
            { window: '7d', label: '7d', usedPercent: 100 - shared7dRem, remainingPercent: shared7dRem, resetsAt: resetShared7d },
            {
              window: '7d',
              label: '7d Sonnet',
              group: 'Sonnet',
              modelHints: ['sonnet'],
              usedPercent: 100 - sonnet7dRem,
              remainingPercent: sonnet7dRem,
              resetsAt: resetSonnet7d,
            },
          ],
        },
      },
      {
        adapter: 'codex',
        enabled: true,
        state: 'ready',
        source: 'path',
        quota: {
          adapter: 'codex',
          available: true,
          fetchedAt: Date.UTC(2026, 10, 27, 12, 0, 0),
          windows: [
            { window: '5h', label: '5h', usedPercent: 100, remainingPercent: 0, resetsAt: reset5h },
            { window: '7d', label: '7d', usedPercent: 100, remainingPercent: 0, resetsAt: resetShared7d },
            {
              window: '5h',
              label: 'Codex Spark 5h',
              group: 'Codex Spark',
              modelHints: ['gpt-5.3-codex-spark'],
              usedPercent: 15,
              remainingPercent: 85,
              resetsAt: reset5h,
            },
            {
              window: '7d',
              label: 'Codex Spark 7d',
              group: 'Codex Spark',
              modelHints: ['gpt-5.3-codex-spark'],
              usedPercent: 10,
              remainingPercent: 90,
              resetsAt: resetSonnet7d,
            },
          ],
        },
      },
    ],
    updatedAt: Date.UTC(2026, 10, 27, 12, 0, 0),
  });

  // Claude: 5h=80%, shared 7d=0%, Sonnet 7d=75% -> BOTH primary badge and 7d show 0% with shared 7d reset!
  const exhaustedShared = resolveAgentQuota({
    machines: [makeMachine(0, 75)],
    machineId: 'hub-node',
    adapter: 'claude',
    model: 'claude-sonnet-4-5',
  });
  assert.equal(exhaustedShared.available, true);
  assert.equal(exhaustedShared.primaryRemainingPercent, 0);
  assert.equal(exhaustedShared.win5h?.remainingPercent, 80);
  assert.equal(exhaustedShared.win7d?.remainingPercent, 0);
  assert.equal(exhaustedShared.win7d?.resetsAt, resetShared7d);

  // Claude converse: 5h=80%, shared 7d=60%, Sonnet 7d=12% -> Sonnet 7d is tighter -> 12% with Sonnet 7d reset!
  const tighterSonnet = resolveAgentQuota({
    machines: [makeMachine(60, 12)],
    machineId: 'hub-node',
    adapter: 'claude',
    model: 'claude-sonnet-4-5',
  });
  assert.equal(tighterSonnet.primaryRemainingPercent, 12);
  assert.equal(tighterSonnet.win7d?.remainingPercent, 12);
  assert.equal(tighterSonnet.win7d?.resetsAt, resetSonnet7d);

  // Codex independent pool: default codex is 0%, Codex Spark is 85%/90% -> Spark MUST stay 85% (not clamped to 0%)!
  const codexSpark = resolveAgentQuota({
    machines: [makeMachine(0, 75)],
    machineId: 'hub-node',
    adapter: 'codex',
    model: 'gpt-5.3-codex-spark',
  });
  assert.equal(codexSpark.available, true);
  assert.equal(codexSpark.group, 'Codex Spark');
  assert.equal(codexSpark.primaryRemainingPercent, 85);
  assert.equal(codexSpark.win5h?.remainingPercent, 85);
  assert.equal(codexSpark.win7d?.remainingPercent, 90);
});

test('IME guard blocks only synchronous 25ms one-shot trailing Enter and never swallows independent Enter or Arrow/Esc', () => {
  assert.equal(IME_TRAILING_ENTER_WINDOW_MS, 25);
  let now = 1000;
  const ime = createImeGuardController(() => now);

  // 1. Active composition protects Enter, ArrowDown, Escape, Tab, Backspace
  ime.onCompositionStart();
  assert.equal(ime.isImeKeyEvent({ key: 'Enter' }), true);
  assert.equal(ime.isImeKeyEvent({ key: 'ArrowDown' }), true);
  assert.equal(ime.isImeKeyEvent({ key: 'Escape' }), true);
  assert.equal(ime.isImeKeyEvent({ key: 'Tab' }), true);
  assert.equal(ime.isImeKeyEvent({ key: 'Backspace' }), true);

  // 2. WKWebView synchronous trailing Enter after compositionend is blocked (one-shot),
  //    then an immediately following independent Enter sends normally!
  now = 1000;
  ime.onCompositionEnd();
  now = 1008; // 8ms <= 25ms
  assert.equal(ime.isImeKeyEvent({ key: 'Enter' }), true, 'synchronous trailing Enter must be blocked');
  now = 1015; // still within 25ms, but one-shot already consumed!
  assert.equal(ime.isImeKeyEvent({ key: 'Enter' }), false, 'independent Enter immediately after must NOT be swallowed');

  // 3. keyup clears the one-shot flag immediately
  now = 2000;
  ime.onCompositionStart();
  ime.onCompositionEnd();
  now = 2005;
  ime.onKeyUp();
  now = 2012;
  assert.equal(ime.isImeKeyEvent({ key: 'Enter' }), false, 'independent Enter after keyup must send');

  // 4. Subsequent real input event clears the one-shot flag, while composition-commit input preserves it
  now = 3000;
  ime.onCompositionStart();
  ime.onCompositionEnd();
  ime.onInput({ inputType: 'insertFromComposition', isComposing: false });
  now = 3005;
  assert.equal(ime.isImeKeyEvent({ key: 'Enter' }), true, 'insertFromComposition must not disarm trailing Enter');

  now = 3100;
  ime.onCompositionStart();
  ime.onCompositionEnd();
  ime.onInput({ inputType: 'insertText', isComposing: false });
  now = 3110;
  assert.equal(ime.isImeKeyEvent({ key: 'Enter' }), false, 'new text input must disarm pending trailing Enter');

  // 5. ArrowDown / ArrowUp / Escape / Tab / Backspace within 15ms after compositionend MUST NOT be blocked
  for (const navKey of ['ArrowDown', 'ArrowUp', 'Escape', 'Tab', 'Backspace']) {
    now = 4000;
    ime.onCompositionStart();
    ime.onCompositionEnd();
    now = 4015; // 15ms < 25ms
    assert.equal(ime.isImeKeyEvent({ key: navKey }), false, `${navKey} 15ms after compositionend must respond normally`);
    now = 4018;
    assert.equal(ime.isImeKeyEvent({ key: 'Enter' }), false, `Enter after ${navKey} must not be swallowed`);
  }

  // 6. Expired trailing Enter beyond 25ms window is not swallowed
  now = 5000;
  ime.onCompositionStart();
  ime.onCompositionEnd();
  now = 5000 + IME_TRAILING_ENTER_WINDOW_MS + 1;
  assert.equal(ime.isImeKeyEvent({ key: 'Enter' }), false);

  // 7. Native isComposing / keyCode=229 always protected even without compositionstart
  assert.equal(ime.isImeKeyEvent({ key: 'Enter', isComposing: true }), true);
  assert.equal(ime.isImeKeyEvent({ key: 'Escape', keyCode: 229 }), true);
});
