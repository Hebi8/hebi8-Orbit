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
 * - If input starts with `@`, evaluates reserved targets (`all`, `parent`) and all participants'
 *   full untruncated names / taskIds in a single longest-prefix pass, selecting the longest
 *   complete target first before checking whether a non-empty message body follows.
 * - Unknown or ambiguous duplicate targets fail fast with a clear inline error instead of
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

  interface UnifiedTarget {
    key: string;
    mentionValue: string;
    names: string[];
  }

  const targets: UnifiedTarget[] = [
    { key: '__reserved:all', mentionValue: 'all', names: ['all'] },
    { key: '__reserved:parent', mentionValue: 'parent', names: ['parent'] },
  ];
  for (const p of participants) {
    const names = new Set<string>();
    if (p.taskId && p.taskId.trim()) names.add(p.taskId.trim());
    if (p.fullName.trim()) names.add(p.fullName.trim());
    const strippedDisplay = p.displayLabel.replace(/^@/, '').trim();
    if (strippedDisplay) names.add(strippedDisplay);
    targets.push({ key: p.key, mentionValue: p.mentionValue, names: [...names] });
  }

  let bestLen = 0;
  let bestMatchedText = '';
  const bestMatches = new Map<string, UnifiedTarget>();

  for (const t of targets) {
    for (const cand of t.names) {
      const candLower = cand.toLowerCase();
      if (!afterAtLower.startsWith(candLower)) continue;
      const tail = afterAt.slice(cand.length);
      if (tail.length > 0 && !/^\s/.test(tail)) continue;
      if (cand.length > bestLen) {
        bestLen = cand.length;
        bestMatchedText = cand;
        bestMatches.clear();
        bestMatches.set(t.key, t);
      } else if (cand.length === bestLen) {
        bestMatches.set(t.key, t);
      }
    }
  }

  if (bestLen > 0) {
    if (bestMatches.size > 1) {
      return {
        ok: false,
        error: `存在多个同名参与者「${bestMatchedText}」，请点击上方提及按钮或使用 @taskId 精确指定`,
      };
    }
    const only = [...bestMatches.values()][0];
    const restBody = afterAt.slice(bestLen).trim();
    if (!restBody) {
      return { ok: false, error: `请在 @${bestMatchedText} 后输入消息正文` };
    }
    return { ok: true, target: only.mentionValue, body: restBody };
  }

  const firstToken = afterAt.split(/\s+/)[0] || '';
  return {
    ok: false,
    error: `未找到提及对象 @${firstToken}，请点击上方按钮选择参与者、@all 或 @parent`,
  };
}
