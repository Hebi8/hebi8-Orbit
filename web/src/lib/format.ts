import type {
  ApprovalState,
  AttemptState,
  BoardMemberState,
  InboxItemView,
  PendingInputSource,
  ProjectView,
  TaskState,
  TaskSummary,
  TriggerKind,
} from '../generated/protocol';

export const SCRATCH_PROJECT_ID = '_scratch';

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

export function todayLabel(): { greet: string; date: string } {
  const d = new Date();
  const h = d.getHours();
  const greet = h < 5 ? '夜深了' : h < 11 ? '早上好' : h < 14 ? '中午好' : h < 18 ? '下午好' : '晚上好';
  const date = `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
  return { greet, date };
}

export type SimpleSessionState = 'attention' | 'running' | 'finished';

/**
 * Low-stimulus 3-state session rollup:
 *   attention (需要注意) > running (运行中) > finished (已结束).
 * Mirrors Lody LAN fork's low-stimulus model: one row, one mark, no count bombardment.
 */
export function taskSimpleState(
  t: Pick<TaskSummary, 'state' | 'activeAttemptState'>,
  hasOpenInbox = false,
): SimpleSessionState {
  if (
    hasOpenInbox ||
    t.activeAttemptState === 'needs_verification' ||
    t.state === 'waiting_approval' ||
    t.state === 'waiting_resource' ||
    t.state === 'in_review' ||
    t.state === 'failed'
  ) {
    return 'attention';
  }
  if (t.state === 'running' || t.state === 'queued') {
    return 'running';
  }
  return 'finished';
}

export function taskSimpleLabel(s: SimpleSessionState): string {
  if (s === 'attention') return '需要注意';
  if (s === 'running') return '运行中';
  return '已结束';
}

export function taskSimpleDot(s: SimpleSessionState): string {
  if (s === 'attention') return 'wait';
  if (s === 'running') return 'run';
  return 'done';
}

export function attentionTaskIds(items: InboxItemView[]): Set<string> {
  const set = new Set<string>();
  for (const it of items) {
    if (it.taskId) set.add(it.taskId);
  }
  return set;
}

export function taskDot(state: TaskState, attemptState?: AttemptState | null, hasOpenInbox = false): string {
  return taskSimpleDot(taskSimpleState({ state, activeAttemptState: attemptState }, hasOpenInbox));
}

export function taskStateLabel(
  t: Pick<TaskSummary, 'state' | 'activeAttemptState' | 'waitingReason'>,
  hasOpenInbox = false,
): string {
  return taskSimpleLabel(taskSimpleState(t, hasOpenInbox));
}

export function isTaskActive(t: Pick<TaskSummary, 'state'>): boolean {
  return t.state === 'queued' || t.state === 'running' || t.state === 'waiting_resource' || t.state === 'waiting_approval';
}

export function isTaskBusy(t: Pick<TaskSummary, 'activeAttemptId'>): boolean {
  return !!t.activeAttemptId;
}

export function attemptStateLabel(s: AttemptState): string {
  const m: Record<AttemptState, string> = {
    leased: '已派发',
    preparing: '准备工作区',
    waiting_resource: '等待资源',
    running: '运行中',
    swarm_monitoring: 'Swarm 监控中',
    waiting_approval: '等待审批',
    succeeded: '完成',
    needs_verification: '中断 · 待核对',
    failed: '失败',
    expired: '已过期',
    cancelled: '已取消',
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
    diff_drawer: 'Code Review',
    ryugi: 'Ryugi',
    pa: 'PA',
    system: '系统',
  };
  return m[s] ?? s;
}

export function approvalStateLabel(s: ApprovalState): string {
  const m: Record<ApprovalState, string> = {
    pending: '待确认',
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
      return 'done';
  }
}

export function memberStateLabel(s: BoardMemberState): string {
  const m: Record<BoardMemberState, string> = { working: '工作中', waiting: '等待', stopped: '已停止', killed: '已终止' };
  return m[s] ?? s;
}

export function projectInitial(p?: Pick<ProjectView, 'displayName' | 'projectId'> | null): string {
  const n = (p?.displayName || p?.projectId || '?').replace(/^[^a-zA-Z0-9一-龥]+/, '');
  return n.slice(0, 1) || '?';
}

export function taskTitle(t: Pick<TaskSummary, 'title' | 'goal' | 'taskId'>): string {
  const s = (t.title || '').trim() || (t.goal || '').trim().split('\n')[0] || t.taskId;
  return s.length > 80 ? `${s.slice(0, 80)}…` : s;
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
