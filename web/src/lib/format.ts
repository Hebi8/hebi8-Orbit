import type {
  ApprovalState,
  AttemptState,
  BoardMemberState,
  InboxItemView,
  PendingInputSource,
  TaskSummary,
  TriggerKind,
} from '../generated/protocol';

export const SCRATCH_PROJECT_ID = '_scratch';
export const PA_PROJECT_ID = '_pa';

export function isVirtualProject(projectId?: string | null): boolean {
  return projectId === SCRATCH_PROJECT_ID || projectId === PA_PROJECT_ID;
}

export function shortSha(sha?: string | null): string {
  return sha ? sha.slice(0, 7) : '';
}

export function basename(p?: string | null): string {
  if (!p) return '';
  const parts = p.replace(/\/+$/, '').split('/');
  return parts[parts.length - 1] ?? '';
}

export function hhmm(ts?: number | null): string {
  if (!ts) return '';
  const d = new Date(ts);
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
}

export function relTime(ts?: number | null): string {
  if (!ts) return '';
  const diff = Date.now() - ts;
  if (diff < 60_000) return '刚刚';
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)} 分钟前`;
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)} 小时前`;
  const d = new Date(ts);
  return `${d.getMonth() + 1}-${String(d.getDate()).padStart(2, '0')}`;
}

/**
 * Low-stimulus 5-state single-channel session mark:
 *   failed > needs-you > running > ready > null (resting/quiet = blank).
 * Never rendered as visible status prose; hint text is strictly for title / aria-label.
 */
export type SessionMark = 'failed' | 'needs-you' | 'running' | 'ready' | null;

const ROLLUP_RANK: Record<Exclude<SessionMark, null>, number> = {
  failed: 0,
  'needs-you': 1,
  running: 2,
  ready: 3,
};

/** Display sort order on Home & Project lists: actionable marks above hairline, running below, resting last. */
export function displayMarkRank(m: SessionMark): number {
  switch (m) {
    case 'failed':
      return 0;
    case 'needs-you':
      return 1;
    case 'ready':
      return 2;
    case 'running':
      return 3;
    default:
      return 4;
  }
}

export function indexInboxByTask(items: readonly InboxItemView[]): Map<string, InboxItemView[]> {
  const map = new Map<string, InboxItemView[]>();
  for (const it of items) {
    if (!it.taskId) continue;
    const list = map.get(it.taskId);
    if (list) list.push(it);
    else map.set(it.taskId, [it]);
  }
  return map;
}

export function inboxItemsMark(items?: readonly InboxItemView[]): SessionMark {
  if (!items || items.length === 0) return null;
  let best: SessionMark = null;
  for (const it of items) {
    let m: SessionMark = null;
    if (it.category === 'approval_pending' || it.category === 'memory_conflict' || it.category === 'task_blocked') {
      m = 'needs-you';
    } else if (it.category === 'delivery_review') {
      m = 'ready';
    }
    best = mergeSessionMarks(best, m);
  }
  return best;
}

export function sessionMark(
  t: Pick<TaskSummary, 'state' | 'activeAttemptState'>,
  inboxItems?: readonly InboxItemView[],
): SessionMark {
  if (
    t.state === 'failed' ||
    t.activeAttemptState === 'failed' ||
    t.activeAttemptState === 'expired'
  ) {
    return 'failed';
  }
  const hasApprovalOrBlock = inboxItems?.some(
    (it) => it.category === 'approval_pending' || it.category === 'task_blocked' || it.category === 'memory_conflict',
  );
  if (
    hasApprovalOrBlock ||
    t.state === 'waiting_approval' ||
    t.activeAttemptState === 'waiting_approval' ||
    t.activeAttemptState === 'needs_verification'
  ) {
    return 'needs-you';
  }
  if (
    t.state === 'running' ||
    t.state === 'queued' ||
    t.state === 'waiting_resource' ||
    t.activeAttemptState === 'leased' ||
    t.activeAttemptState === 'preparing' ||
    t.activeAttemptState === 'waiting_resource' ||
    t.activeAttemptState === 'running' ||
    t.activeAttemptState === 'swarm_monitoring'
  ) {
    return 'running';
  }
  if (t.state === 'in_review' || inboxItems?.some((it) => it.category === 'delivery_review')) {
    return 'ready';
  }
  return null;
}

export function mergeSessionMarks(...marks: SessionMark[]): SessionMark {
  let best: SessionMark = null;
  for (const m of marks) {
    if (!m) continue;
    if (!best || ROLLUP_RANK[m] < ROLLUP_RANK[best]) best = m;
  }
  return best;
}

/** Rolls hidden child-task marks up onto their root thread so each root row draws at most one mark. */
export function rootSessionMark(
  root: TaskSummary,
  allTasks: readonly TaskSummary[],
  inboxByTask: ReadonlyMap<string, InboxItemView[]>,
): SessionMark {
  let best = sessionMark(root, inboxByTask.get(root.taskId));
  for (const child of allTasks) {
    if (child.parentTaskId === root.taskId) {
      best = mergeSessionMarks(best, sessionMark(child, inboxByTask.get(child.taskId)));
    }
  }
  return best;
}

/** Strictly for `title` / `aria-label` tooltips — never rendered inline in visible prose. */
export function sessionMarkHint(m: SessionMark): string {
  switch (m) {
    case 'failed':
      return '出错了';
    case 'needs-you':
      return '等你确认';
    case 'running':
      return '进行中';
    case 'ready':
      return '有新产出';
    default:
      return '';
  }
}

/** Mobile tab dot lights up ONLY for urgent human intervention (`failed` or `needs-you`). */
export function hasUrgentSession(
  inboxItems: readonly InboxItemView[],
  activeTasks: readonly TaskSummary[],
): boolean {
  const byTask = indexInboxByTask(inboxItems);
  for (const t of activeTasks) {
    const m = sessionMark(t, byTask.get(t.taskId));
    if (m === 'failed' || m === 'needs-you') return true;
  }
  for (const it of inboxItems) {
    if (it.category === 'approval_pending' || it.category === 'task_blocked' || it.category === 'memory_conflict') {
      return true;
    }
  }
  return false;
}

export function isTaskBusy(t: Pick<TaskSummary, 'activeAttemptId'>): boolean {
  return !!t.activeAttemptId;
}

export function attemptStateLabel(s: AttemptState): string {
  const m: Record<AttemptState, string> = {
    leased: '准备中',
    preparing: '准备工作区',
    waiting_resource: '等待资源',
    running: '进行中',
    swarm_monitoring: '协作进行中',
    waiting_approval: '等你确认',
    succeeded: '完成',
    needs_verification: '中途中断',
    failed: '出错了',
    expired: '已过期',
    cancelled: '已停止',
  };
  return m[s] ?? s;
}

export function isAttemptLive(s?: AttemptState | null): boolean {
  return (
    s === 'leased' ||
    s === 'preparing' ||
    s === 'waiting_resource' ||
    s === 'running' ||
    s === 'swarm_monitoring' ||
    s === 'waiting_approval'
  );
}

export function triggerLabel(k: TriggerKind): string {
  const m: Record<TriggerKind, string> = {
    initial: '初始目标',
    followup: '追问',
    annotation_batch: '批注包',
    retry: '重试',
    deploy: '部署',
    workspace_import: '导入工作区',
    manual_import: '手动导入',
  };
  return m[k] ?? k;
}

export function sourceLabel(s: PendingInputSource): string {
  const m: Record<PendingInputSource, string> = {
    human: '你',
    diff_drawer: '批注',
    ryugi: '批注',
    pa: 'Hebi8',
    system: '系统',
  };
  return m[s] ?? s;
}

export function approvalStateLabel(s: ApprovalState): string {
  const m: Record<ApprovalState, string> = {
    pending: '等你确认',
    approved_pending_delivery: '已批准 · 送达中',
    rejected_pending_delivery: '已拒绝 · 送达中',
    consumed_allowed: '已批准',
    consumed_rejected: '已拒绝',
    uncertain: '结果未知',
    expired: '已过期',
  };
  return m[s] ?? s;
}

export function memberDot(s: BoardMemberState): string {
  switch (s) {
    case 'working':
      return 'run';
    case 'waiting':
      return 'wait';
    case 'killed':
      return 'block';
    default:
      return 'idle';
  }
}

export function memberStateLabel(s: BoardMemberState): string {
  const m: Record<BoardMemberState, string> = { working: '进行中', waiting: '等待中', stopped: '已停止', killed: '已终止' };
  return m[s] ?? s;
}

export function taskTitle(t: Pick<TaskSummary, 'title' | 'goal' | 'taskId'>): string {
  const s = (t.title || '').trim() || (t.goal || '').trim().split('\n')[0] || t.taskId;
  return s.length > 80 ? `${s.slice(0, 80)}…` : s;
}

/** Human-readable copy for busy exclusive machine resources (no raw mono resource keys). */
export function humanResourceLabel(key: string, state: string, reason?: string): string {
  const k = key.toLowerCase();
  if (k === 'macos_gui') return reason || 'macOS 锁屏中';
  if (k === 'xcode_sim') return reason ? `iOS 模拟器占用 · ${reason}` : 'iOS 模拟器占用';
  if (k === 'chrome_logged_in') return reason ? `浏览器占用 · ${reason}` : '登录态浏览器占用';
  if (reason) return reason;
  return `${key} (${state})`;
}

/** Extracts a human-readable command / target preview from an approval payload. */
export function approvalPreview(payload: Record<string, unknown> | undefined | null): string {
  if (!payload) return '';
  const pick = (obj: Record<string, unknown>, keys: string[]): string => {
    for (const k of keys) {
      const v = obj[k];
      if (typeof v === 'string' && v.trim()) return v;
      if (Array.isArray(v) && v.every((x) => typeof x === 'string') && v.length) return (v as string[]).join(' ');
    }
    return '';
  };
  const direct = pick(payload, ['command', 'cmd', 'commandLine', 'script', 'title', 'path', 'url']);
  if (direct) return direct;
  for (const nestKey of ['rawInput', 'input', 'toolCall', 'params']) {
    const nest = payload[nestKey];
    if (nest && typeof nest === 'object' && !Array.isArray(nest)) {
      const v = pick(nest as Record<string, unknown>, ['command', 'cmd', 'title', 'path', 'url']);
      if (v) return v;
    }
  }
  try {
    const s = JSON.stringify(payload);
    if (s === '{}') return '';
    return s.length > 400 ? `${s.slice(0, 400)}…` : s;
  } catch {
    return '';
  }
}

export function toolVerb(name: string, title: string): string {
  const s = `${name} ${title}`.toLowerCase();
  if (/(edit|write|patch|replace|create_file|apply)/.test(s)) return '编辑';
  if (/(read|view|open|cat )/.test(s)) return '读取';
  if (/(grep|search|find|glob|list)/.test(s)) return '搜索';
  if (/(run|bash|exec|shell|command|terminal|test)/.test(s)) return '运行';
  if (/(fetch|http|browse|web)/.test(s)) return '获取';
  if (/(post_message|board|spawn|swarm|invoke)/.test(s)) return '协作';
  return '工具';
}

/**
 * Strips any parenthetical role/tag description from machine display names
 * (e.g. "dev-node (Primary Hub & Dev)" -> "dev-node") so machine names stand cleanly on their own.
 */
export function cleanMachineName(m?: { displayName?: string; machineId: string } | null): string {
  if (!m) return '';
  const raw = (m.displayName || m.machineId || '').trim();
  const stripped = raw.replace(/\s*[（(][^）)]*[）)]\s*$/g, '').trim();
  return stripped || m.machineId;
}

export function isImagePath(p?: string | null): boolean {
  if (!p) return false;
  return /\.(png|jpe?g|gif|webp|svg|avif|bmp)$/i.test(p.trim());
}

export type ArtifactDisplayKind = 'design' | 'markdown' | 'docx' | 'pptx' | 'spreadsheet' | 'file';

export function classifyArtifactKind(pathOrLocator?: string | null): ArtifactDisplayKind {
  const low = (pathOrLocator || '').trim().toLowerCase();
  if (low.startsWith('artboard/') || low.startsWith('html/') || low.startsWith('whiteboard/')) return 'design';
  if (low.startsWith('slides/') || /\.(pptx?|key)$/.test(low)) return 'pptx';
  if (low.startsWith('document/') || /\.(md|markdown)$/.test(low)) return 'markdown';
  if (/\.(docx?)$/.test(low)) return 'docx';
  if (/\.(xlsx?|csv)$/.test(low)) return 'spreadsheet';
  return 'file';
}

export function artifactShortLabel(kind: string): string {
  switch (kind) {
    case 'design':
      return 'Ryugi';
    case 'markdown':
      return 'MD';
    case 'docx':
      return 'DOCX';
    case 'pptx':
      return 'PPTX';
    case 'spreadsheet':
      return 'XLSX';
    default:
      return 'File';
  }
}

export function artifactFilterLabel(kind: string): string {
  switch (kind) {
    case 'design':
      return '设计画板';
    case 'markdown':
      return 'Markdown';
    case 'pptx':
      return 'PPT';
    case 'docx':
      return 'Word';
    case 'spreadsheet':
      return '表格';
    case 'file':
      return '文件';
    default:
      return '全部';
  }
}
