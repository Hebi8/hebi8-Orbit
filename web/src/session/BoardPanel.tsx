import { Fragment, useEffect, useMemo, useRef, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { X } from 'lucide-react';
import type {
  BoardMemberCursor,
  ChildTaskView,
  InboxItemView,
  MachineView,
  TaskBoardResponse,
} from '../generated/protocol';
import { SendUpIcon } from '../components/Icons';
import { StatusMark } from '../components/StatusMark';
import { ADAPTER_LABELS } from '../lib/agentCaps';
import { api, errMsg } from '../lib/api';
import {
  childSessionMark,
  childStateLabel,
  cleanMachineName,
  fullTaskTitle,
  hhmm,
  memberStateLabel,
  taskTitle,
  type SessionMark,
} from '../lib/format';
import { resolveManualBoardMention } from '../lib/boardMention';
import { useImeGuard } from '../lib/ime';
import { navigate, paths } from '../lib/router';
import { toast } from '../store/useWorkbenchStore';

interface BoardParticipant {
  key: string;
  kind: 'child' | 'native';
  label: string;
  fullName: string;
  mentionValue: string;
  taskId?: string;
  meta: string;
  mark: SessionMark;
  lastConsumedSeq: number;
  cursorLabel: string;
  child?: ChildTaskView;
  member?: BoardMemberCursor;
}

function nativeMemberMark(m: BoardMemberCursor): SessionMark {
  switch (m.state) {
    case 'working':
      return 'running';
    case 'waiting':
      return 'needs-you';
    case 'killed':
      return 'failed';
    default:
      return null;
  }
}

/** Expanded right panel: ordered #seq shared board with first-class child sessions, native swarm cursors, and @mention composer. */
export function BoardPanel({
  taskId,
  board,
  fallbackMembers,
  childrenList = [],
  machines = [],
  inboxByTask,
  worker,
  onClose,
  onOpenChild,
  onSelectMember,
}: {
  taskId: string;
  board?: TaskBoardResponse;
  fallbackMembers: BoardMemberCursor[];
  childrenList?: ChildTaskView[];
  machines?: MachineView[];
  inboxByTask?: ReadonlyMap<string, InboxItemView[]>;
  worker?: string;
  onClose: () => void;
  onOpenChild?: (childTaskId: string) => void;
  onSelectMember?: (member: BoardMemberCursor) => void;
}) {
  const qc = useQueryClient();
  const ime = useImeGuard();
  const members = board?.members ?? fallbackMembers;
  const [olderMessages, setOlderMessages] = useState<TaskBoardResponse['messages']>([]);
  const [olderHasMore, setOlderHasMore] = useState<boolean | null>(null);
  const [loadingOlder, setLoadingOlder] = useState(false);

  useEffect(() => {
    setOlderMessages([]);
    setOlderHasMore(null);
  }, [taskId]);

  const msgById = new Map<string, TaskBoardResponse['messages'][number]>();
  for (const m of olderMessages) msgById.set(m.messageId, m);
  for (const m of board?.messages ?? []) msgById.set(m.messageId, m);
  const messages = [...msgById.values()].sort((a, b) => a.seq - b.seq);
  const hasMore = olderHasMore !== null ? olderHasMore : !!board?.hasMore;
  const oldestSeq = messages.length > 0 ? messages[0].seq : 0;

  const machineLabelOf = useMemo(() => {
    const map = new Map<string, string>();
    for (const m of machines) {
      map.set(m.machineId, m.displayAlias?.trim() || cleanMachineName(m));
    }
    return (mid?: string) => (mid ? map.get(mid) || mid : '');
  }, [machines]);

  const participants = useMemo<BoardParticipant[]>(() => {
    const list: BoardParticipant[] = [];
    const memberByChildId = new Map<string, BoardMemberCursor>();
    const consumedNativeKeys = new Set<string>();

    for (const m of members) {
      if (m.workerTaskId) {
        memberByChildId.set(m.workerTaskId, m);
      }
    }

    const seenChildIds = new Set<string>();
    for (const c of childrenList) {
      seenChildIds.add(c.taskId);
      const cursor = memberByChildId.get(c.taskId);
      const label = taskTitle(c);
      const fullName = fullTaskTitle(c);
      const machName = machineLabelOf(c.machineId || cursor?.machineId);
      const adpRaw = c.preferredAdapter || cursor?.adapter;
      const adpName = adpRaw ? ADAPTER_LABELS[adpRaw] ?? adpRaw : '';
      const stateText = childStateLabel(c.state, c.openApprovalCount);
      const meta = [machName, adpName, stateText, c.currentStep || cursor?.currentStep].filter(Boolean).join(' · ');

      list.push({
        key: `child:${c.taskId}`,
        kind: 'child',
        label,
        fullName,
        mentionValue: c.taskId,
        taskId: c.taskId,
        meta,
        mark: childSessionMark(c, inboxByTask?.get(c.taskId)),
        lastConsumedSeq: cursor?.lastConsumedSeq ?? 0,
        cursorLabel: label,
        child: c,
        member: cursor,
      });
    }

    for (const m of members) {
      if (m.workerTaskId && seenChildIds.has(m.workerTaskId)) continue;
      if (consumedNativeKeys.has(m.memberName)) continue;
      consumedNativeKeys.add(m.memberName);

      const isChildCursor = !!m.workerTaskId;
      const machName = machineLabelOf(m.machineId);
      const adpName = m.adapter ? ADAPTER_LABELS[m.adapter] ?? m.adapter : '';
      const meta = [machName, adpName, memberStateLabel(m.state), m.currentStep, !isChildCursor ? '内置' : '']
        .filter(Boolean)
        .join(' · ');

      list.push({
        key: isChildCursor ? `child:${m.workerTaskId}` : `native:${m.memberName}`,
        kind: isChildCursor ? 'child' : 'native',
        label: isChildCursor ? m.memberName : `@${m.memberName}`,
        fullName: m.memberName,
        mentionValue: isChildCursor ? m.workerTaskId! : m.memberName,
        taskId: m.workerTaskId || undefined,
        meta,
        mark: nativeMemberMark(m),
        lastConsumedSeq: m.lastConsumedSeq,
        cursorLabel: m.memberName,
        member: m,
      });
    }

    return list;
  }, [members, childrenList, machineLabelOf, inboxByTask]);

  const childTitleById = useMemo(() => {
    const map = new Map<string, string>();
    for (const c of childrenList) map.set(c.taskId, taskTitle(c));
    for (const m of members) {
      if (m.workerTaskId && !map.has(m.workerTaskId)) map.set(m.workerTaskId, m.memberName);
    }
    return map;
  }, [childrenList, members]);

  const formatActorOrMention = (raw: string) => childTitleById.get(raw) ?? raw;

  const loadOlder = async () => {
    if (loadingOlder || !hasMore || oldestSeq <= 1) return;
    setLoadingOlder(true);
    try {
      const page = await api.getBoard(taskId, { beforeSeq: oldestSeq, limit: 200 });
      setOlderMessages((prev) => {
        const merged = new Map<string, TaskBoardResponse['messages'][number]>();
        for (const m of page.messages) merged.set(m.messageId, m);
        for (const m of prev) merged.set(m.messageId, m);
        return [...merged.values()].sort((a, b) => a.seq - b.seq);
      });
      setOlderHasMore(!!page.hasMore);
    } catch (e) {
      toast(errMsg(e), 'err');
    } finally {
      setLoadingOlder(false);
    }
  };

  const latest = board?.latestSeq ?? 0;
  const [target, setTarget] = useState<string>(worker ?? '');
  const [text, setText] = useState('');
  const [mentionError, setMentionError] = useState('');
  const listRef = useRef<HTMLDivElement | null>(null);
  const prevLatestSeqRef = useRef<number>(0);

  useEffect(() => {
    if (worker) setTarget(worker);
  }, [worker]);
  useEffect(() => {
    const newest = messages.length > 0 ? messages[messages.length - 1].seq : 0;
    if (newest > prevLatestSeqRef.current) {
      prevLatestSeqRef.current = newest;
      const el = listRef.current;
      if (el) el.scrollTop = el.scrollHeight;
    }
  }, [messages]);

  const post = useMutation({
    mutationFn: (v: { target: string; body: string }) =>
      api.postBoardMessage(taskId, { authorName: 'human', targetMention: v.target || undefined, body: v.body }),
    onSuccess: () => {
      setText('');
      setMentionError('');
      void qc.invalidateQueries({ queryKey: ['task-board', taskId] });
      void qc.invalidateQueries({ queryKey: ['task-children', taskId] });
      void qc.invalidateQueries({ queryKey: ['task-timeline', taskId] });
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const send = () => {
    if (!text.trim() || post.isPending) return;
    const resolved = resolveManualBoardMention(
      text,
      target,
      participants.map((p) => ({
        key: p.key,
        mentionValue: p.mentionValue,
        fullName: p.fullName,
        displayLabel: p.label,
        taskId: p.taskId,
      })),
    );
    if (!resolved.ok) {
      setMentionError(resolved.error);
      return;
    }
    setMentionError('');
    post.mutate({ target: resolved.target, body: resolved.body });
  };

  const cursorsAt = (seq: number) =>
    participants.filter((p) => p.lastConsumedSeq > 0 && p.lastConsumedSeq === seq).map((p) => p.cursorLabel);

  const selectedTargetDisplay =
    target === 'all'
      ? 'all'
      : participants.find((p) => p.mentionValue === target)?.label.replace(/^@/, '') || formatActorOrMention(target);

  return (
    <aside className="board" aria-label="共享消息板">
      <div className="bh">
        <span className="t">共享消息板</span>
        <span className="s mono">#{latest}</span>
        <button type="button" className="ib" title="收起 (Esc)" onClick={onClose}>
          <X size={14} strokeWidth={1.6} />
        </button>
      </div>
      <div className="bsec">
        <div className="bsec-h">
          协作参与者与消费游标<span className="r mono">#{latest}</span>
        </div>
        {participants.length === 0 && <div className="fsub">还没有协作子会话或成员加入</div>}
        {participants.map((p) => {
          const pct = latest > 0 && p.lastConsumedSeq > 0 ? Math.min(100, (p.lastConsumedSeq / latest) * 100) : 0;
          const isActiveView = p.kind === 'native' && worker === p.member?.memberName;
          return (
            <button
              type="button"
              key={p.key}
              className={`mem ${isActiveView ? 'cur-view' : ''}`}
              onClick={() => {
                if (p.child) {
                  if (onOpenChild) onOpenChild(p.child.taskId);
                  else navigate(paths.session(taskId, undefined, p.child.taskId));
                } else if (p.member?.workerTaskId) {
                  if (onOpenChild) onOpenChild(p.member.workerTaskId);
                  else navigate(paths.session(taskId, undefined, p.member.workerTaskId));
                } else if (p.member) {
                  if (onSelectMember) onSelectMember(p.member);
                  else navigate(paths.session(taskId, p.member.memberName));
                }
              }}
              title={p.kind === 'child' ? '分屏查看并接管该子会话' : '查看该内置成员的投影记录与消息流'}
            >
              <span className="mem-mark">
                <StatusMark mark={p.mark} reserveSlot />
              </span>
              <span className="nm">
                <span className="ell">{p.label}</span>
                {p.meta && <span className="host">{p.meta}</span>}
              </span>
              <span className={`cur mono ${p.lastConsumedSeq < latest ? '' : 'faint'}`}>
                {p.lastConsumedSeq > 0 ? `#${p.lastConsumedSeq}` : '—'}
              </span>
              {latest > 0 && p.lastConsumedSeq > 0 && (
                <span className="trk">
                  <i style={{ width: `${pct}%` }} />
                </span>
              )}
            </button>
          );
        })}
      </div>
      <div className="blist" ref={listRef}>
        {hasMore && (
          <div style={{ padding: '6px 0 10px', textAlign: 'center' }}>
            <button type="button" className="btn ghost" disabled={loadingOlder} onClick={() => void loadOlder()}>
              {loadingOlder ? '正在加载更早消息…' : `加载 #${oldestSeq} 之前的更早消息`}
            </button>
          </div>
        )}
        {messages.length === 0 && <div className="empty">消息板还是空的</div>}
        {messages.map((msg) => {
          const you = msg.authorName === 'human' || msg.authorName === 'user';
          const marks = cursorsAt(msg.seq);
          return (
            <Fragment key={msg.messageId}>
              <div className="bm">
                <span className="seq mono">#{msg.seq}</span>
                <div style={{ minWidth: 0 }}>
                  <span className={`who ${you ? 'you' : ''}`}>{you ? '你' : formatActorOrMention(msg.authorName)}</span>
                  {msg.targetMention && (
                    <span className="men" title={msg.targetMention}>
                      @{formatActorOrMention(msg.targetMention)}
                    </span>
                  )}
                  <span className="body2">{msg.body}</span>
                  <div className="bell">
                    <span>{hhmm(msg.createdAt)}</span>
                    <span>{msg.status === 'consumed' ? '已消费' : '待消费'}</span>
                  </div>
                </div>
              </div>
              {marks.length > 0 && <div className="cursor-mark">{marks.join(' · ')} 读到这里</div>}
            </Fragment>
          );
        })}
      </div>
      <div className="bcomp">
        {participants.length > 0 && (
          <div className="bmen" role="group" aria-label="提及对象">
            <button
              type="button"
              className={target === 'all' ? 'on' : ''}
              onClick={() => {
                setTarget(target === 'all' ? '' : 'all');
                setMentionError('');
              }}
            >
              @all
            </button>
            {participants.map((p) => (
              <button
                type="button"
                key={p.key}
                className={target === p.mentionValue ? 'on' : ''}
                title={`提及 ${p.fullName}`}
                onClick={() => {
                  setTarget(target === p.mentionValue ? '' : p.mentionValue);
                  setMentionError('');
                }}
              >
                @{p.label.replace(/^@/, '')}
              </button>
            ))}
          </div>
        )}
        {mentionError && (
          <div className="fsub" role="alert" style={{ color: 'var(--warn)', marginBottom: 6 }}>
            {mentionError}
          </div>
        )}
        <div className="bin">
          <input
            value={text}
            placeholder={
              target ? `投给 @${selectedTargetDisplay}，按 Enter 发送…` : '往板上投一条，点击上方 @ 提及子会话或 @all…'
            }
            onChange={(e) => {
              setText(e.target.value);
              if (mentionError) setMentionError('');
            }}
            {...ime.compositionProps}
            onKeyDown={(e) => {
              if (ime.isImeEnter(e)) return;
              if (e.key === 'Enter') {
                e.preventDefault();
                send();
              }
            }}
            aria-label="投递到消息板"
          />
          <button type="button" className="send" title="投递 ↵" disabled={!text.trim() || post.isPending} onClick={send}>
            <SendUpIcon size={12} />
          </button>
        </div>
      </div>
    </aside>
  );
}
