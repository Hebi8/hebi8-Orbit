import { Fragment, useEffect, useRef, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { X } from 'lucide-react';
import type { BoardMemberCursor, TaskBoardResponse } from '../generated/protocol';
import { SendUpIcon } from '../components/Icons';
import { api, errMsg } from '../lib/api';
import { hhmm, memberDot, memberStateLabel } from '../lib/format';
import { navigate, paths } from '../lib/router';
import { toast } from '../store/useWorkbenchStore';

/** Expanded right panel: ordered #seq shared board with per-member cursors and an @worker composer. */
export function BoardPanel({
  taskId,
  board,
  fallbackMembers,
  worker,
  onClose,
  onSelectMember,
}: {
  taskId: string;
  board?: TaskBoardResponse;
  fallbackMembers: BoardMemberCursor[];
  worker?: string;
  onClose: () => void;
  onSelectMember?: (member: BoardMemberCursor) => void;
}) {
  const qc = useQueryClient();
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
      void qc.invalidateQueries({ queryKey: ['task-board', taskId] });
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const send = () => {
    let body = text.trim();
    if (!body || post.isPending) return;
    let to = target;
    const m = /^@([\w.-]+)\s+([\s\S]+)$/.exec(body);
    if (m) {
      to = m[1];
      body = m[2].trim();
    }
    post.mutate({ target: to, body });
  };

  const cursorsAt = (seq: number) => members.filter((m) => m.lastConsumedSeq === seq).map((m) => m.memberName);

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
          成员与消费游标<span className="r mono">#{latest}</span>
        </div>
        {members.length === 0 && <div className="fsub">还没有成员加入</div>}
        {members.map((m) => {
          const pct = latest > 0 ? Math.min(100, (m.lastConsumedSeq / latest) * 100) : 0;
          return (
            <button
              type="button"
              key={m.memberName}
              className={`mem ${worker === m.memberName ? 'cur-view' : ''}`}
              onClick={() => {
                if (onSelectMember) onSelectMember(m);
                else if (m.workerTaskId) navigate(paths.session(taskId, undefined, m.workerTaskId));
                else navigate(paths.session(taskId, m.memberName));
              }}
              title={m.workerTaskId ? '分屏查看并接管该子会话' : '查看该成员的投影记录与消息流'}
            >
              <span className={`sd ${memberDot(m.state)}`} />
              <span className="nm">
                <span className="ell">{m.memberName}</span>
                <span className="host">
                  {[m.machineId, m.adapter, memberStateLabel(m.state), m.currentStep].filter(Boolean).join(' · ')}
                </span>
              </span>
              <span className={`cur mono ${m.lastConsumedSeq < latest ? '' : 'faint'}`}>#{m.lastConsumedSeq}</span>
              <span className="trk">
                <i style={{ width: `${pct}%` }} />
              </span>
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
                  <span className={`who ${you ? 'you' : ''}`}>{you ? '你' : msg.authorName}</span>
                  {msg.targetMention && <span className="men">@{msg.targetMention}</span>}
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
        {members.length > 0 && (
          <div className="bmen">
            {members.map((m) => (
              <button
                type="button"
                key={m.memberName}
                className={target === m.memberName ? 'on' : ''}
                onClick={() => setTarget(target === m.memberName ? '' : m.memberName)}
              >
                @{m.memberName}
              </button>
            ))}
          </div>
        )}
        <div className="bin">
          <input
            value={text}
            placeholder={target ? `投给 @${target}，@ 即门铃…` : '往板上投一条，@worker 唤醒对应成员…'}
            onChange={(e) => setText(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && !e.nativeEvent.isComposing) {
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
