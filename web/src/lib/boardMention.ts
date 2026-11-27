export interface BoardMentionCandidate {
  key: string;
  mentionValue: string;
  fullName: string;
  displayLabel: string;
  taskId?: string;
}

export type ResolvedBoardMention =
  | { ok: true; target: string; body: string }
  | { ok: false; error: string };

/**
 * Resolves a Board composer input line.
 * - If input does not start with `@`, uses `selectedTarget` directly.
 * - If input starts with `@`, resolves `@all`, `@parent`, `@<taskId>`, or longest-prefix
 *   match against participants' full untruncated names (converting matched children to `taskId`).
 * - Unknown or ambiguous duplicate names fail fast with a clear inline error instead of
 *   misrouting to the parent session.
 */
export function resolveManualBoardMention(
  rawInput: string,
  selectedTarget: string,
  participants: readonly BoardMentionCandidate[],
): ResolvedBoardMention {
  const trimmed = rawInput.trim();
  if (!trimmed) {
    return { ok: false, error: '请输入消息内容' };
  }
  if (!trimmed.startsWith('@')) {
    return { ok: true, target: selectedTarget, body: trimmed };
  }

  const afterAt = trimmed.slice(1);
  const afterAtLower = afterAt.toLowerCase();

  const reservedMatch = /^(all|parent)(?:\s+([\s\S]*))?$/i.exec(afterAt);
  if (reservedMatch) {
    const kw = reservedMatch[1].toLowerCase();
    const rest = (reservedMatch[2] ?? '').trim();
    if (!rest) {
      return { ok: false, error: `请在 @${kw} 后输入消息正文` };
    }
    return { ok: true, target: kw, body: rest };
  }

  let bestLen = 0;
  let bestMatchedText = '';
  const bestMatches = new Map<string, BoardMentionCandidate>();
  let exactOnlyNoBody = false;

  for (const p of participants) {
    const names = new Set<string>();
    if (p.taskId && p.taskId.trim()) names.add(p.taskId.trim());
    if (p.fullName.trim()) names.add(p.fullName.trim());
    const strippedDisplay = p.displayLabel.replace(/^@/, '').trim();
    if (strippedDisplay) names.add(strippedDisplay);

    for (const cand of names) {
      const candLower = cand.toLowerCase();
      if (!afterAtLower.startsWith(candLower)) continue;
      const tail = afterAt.slice(cand.length);
      if (tail.length === 0) {
        exactOnlyNoBody = true;
        continue;
      }
      if (!/^\s/.test(tail)) continue;
      const restBody = tail.trim();
      if (!restBody) {
        exactOnlyNoBody = true;
        continue;
      }
      if (cand.length > bestLen) {
        bestLen = cand.length;
        bestMatchedText = cand;
        bestMatches.clear();
        bestMatches.set(p.key, p);
      } else if (cand.length === bestLen) {
        bestMatches.set(p.key, p);
      }
    }
  }

  if (bestLen > 0) {
    if (bestMatches.size === 1) {
      const only = [...bestMatches.values()][0];
      return {
        ok: true,
        target: only.mentionValue,
        body: afterAt.slice(bestLen).trim(),
      };
    }
    return {
      ok: false,
      error: `存在多个同名参与者「${bestMatchedText}」，请点击上方提及按钮或使用 @taskId 精确指定`,
    };
  }

  if (exactOnlyNoBody) {
    return { ok: false, error: '请在 @提及 后输入消息正文' };
  }

  const firstToken = afterAt.split(/\s+/)[0] || '';
  return {
    ok: false,
    error: `未找到提及对象 @${firstToken}，请点击上方按钮选择参与者、@all 或 @parent`,
  };
}
