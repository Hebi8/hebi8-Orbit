import { useEffect, useRef, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Pencil } from 'lucide-react';
import type { BoardMemberCursor, FollowupMode, PendingInput, TaskSummary } from '../generated/protocol';
import { SteerIcon, SwarmIcon } from '../components/Icons';
import { api, errMsg } from '../lib/api';
import { isTaskBusy } from '../lib/format';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';

/** Drafts survive session switches (per-task, in-memory). */
const drafts = new Map<string, string>();

function parseMention(text: string, members: BoardMemberCursor[]): { target: string; body: string } | null {
  const m = /^@([\w.-]+)\s+([\s\S]+)$/.exec(text.trim());
  if (!m) return null;
  if (!members.some((x) => x.memberName === m[1])) return null;
  return { target: m[1], body: m[2].trim() };
}

export function Composer({
  task,
  worker,
  members,
  editing,
  onEditDone,
}: {
  task: TaskSummary;
  worker?: string;
  members: BoardMemberCursor[];
  editing: PendingInput | null;
  onEditDone: () => void;
}) {
  const qc = useQueryClient();
  const remember = useWorkbenchStore((s) => s.rememberPrompt);
  const [text, setText] = useState(() => drafts.get(task.taskId) ?? '');
  const stash = useRef<string | null>(null);
  const taRef = useRef<HTMLTextAreaElement | null>(null);
  const busy = isTaskBusy(task);

  useEffect(() => {
    setText(drafts.get(task.taskId) ?? '');
  }, [task.taskId]);

  // Entering edit mode stashes the in-progress draft; leaving restores it verbatim.
  useEffect(() => {
    if (editing) {
      stash.current = text;
      setText(editing.body);
      requestAnimationFrame(() => taRef.current?.focus());
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [editing?.inputId]);

  const grow = () => {
    const el = taRef.current;
    if (!el) return;
    el.style.height = 'auto';
    el.style.height = `${Math.min(Math.max(el.scrollHeight, 42), 200)}px`;
  };
  useEffect(grow, [text]);

  const setDraft = (v: string) => {
    setText(v);
    if (!editing) drafts.set(task.taskId, v);
  };

  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ['task-timeline', task.taskId] });
    void qc.invalidateQueries({ queryKey: ['project-tasks', task.projectId] });
    void qc.invalidateQueries({ queryKey: ['bootstrap'] });
  };

  const finishEdit = () => {
    const restored = stash.current ?? '';
    stash.current = null;
    setText(restored);
    drafts.set(task.taskId, restored);
    onEditDone();
  };

  const saveEdit = useMutation({
    mutationFn: (body: string) => api.updateQueueInput(task.taskId, editing!.inputId, body),
    onSuccess: (pi) => {
      remember(`inp:${pi.inputId}`, pi.body);
      toast('已更新队列消息');
      refresh();
      finishEdit();
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const followup = useMutation({
    mutationFn: (v: { message: string; mode: FollowupMode }) => api.followup(task.taskId, { message: v.message, mode: v.mode }),
    onSuccess: (res, v) => {
      if (res.attempt) remember(`att:${res.attempt.attemptId}`, v.message);
      if (res.pendingInput) remember(`inp:${res.pendingInput.inputId}`, v.message);
      if (res.pendingInput && v.mode === 'queue') toast('已加入待消费队列');
      if (v.mode === 'steer') toast('已 Steer：中断当前轮并优先执行');
      setDraft('');
      refresh();
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const boardPost = useMutation({
    mutationFn: (v: { target: string; body: string }) =>
      api.postBoardMessage(task.taskId, { authorName: 'human', targetMention: v.target, body: v.body }),
    onSuccess: (msg) => {
      toast(`已投到消息板 #${msg.seq} · @${msg.targetMention ?? ''}`);
      setDraft('');
      void qc.invalidateQueries({ queryKey: ['task-board', task.taskId] });
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const cancel = useMutation({
    mutationFn: () => api.cancelTask(task.taskId, 'stopped from composer'),
    onSuccess: (r) => {
      toast(r.cancelSent || r.immediateState ? '已请求停止当前轮' : '停止请求已记录（Runner 离线）');
      refresh();
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const pending = followup.isPending || boardPost.isPending || saveEdit.isPending;

  const submit = (steer: boolean) => {
    const msg = text.trim();
    if (!msg || pending) return;
    if (editing) {
      saveEdit.mutate(msg);
      return;
    }
    if (worker) {
      boardPost.mutate({ target: worker, body: msg });
      return;
    }
    const mention = parseMention(msg, members);
    if (mention) {
      boardPost.mutate(mention);
      return;
    }
    followup.mutate({ message: msg, mode: steer && busy ? 'steer' : 'queue' });
  };

  let placeholder = '继续追问…';
  if (editing) placeholder = '编辑这条队列消息…';
  else if (worker) placeholder = `经消息板发给 @${worker}…`;
  else if (busy) placeholder = '运行中 · ↵ 挂到对话末尾排队，⌘↵ Steer 打断；@worker 投到消息板';
  else if (members.length) placeholder = '继续追问…  @worker 会投到共享消息板';

  return (
    <div className="cwrap">
      <div className={`composer ${editing ? 'editing' : ''}`}>
        {editing && (
          <div className="cbanner">
            <Pencil size={12} strokeWidth={1.6} /> 编辑队列消息 · 原草稿已暂存，保存或取消后自动恢复
            <span className="r">Esc 取消</span>
          </div>
        )}
        {worker && !editing && (
          <div className="cbanner" style={{ color: 'var(--muted)' }}>
            <SwarmIcon size={12} /> 经共享消息板发给 <b style={{ color: 'var(--fg)' }}>@{worker}</b> · 不绕过队列私聊
          </div>
        )}
        <textarea
          ref={taRef}
          value={text}
          rows={2}
          placeholder={placeholder}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.nativeEvent.isComposing) return;
            if (e.key === 'Escape' && editing) {
              e.preventDefault();
              e.stopPropagation();
              finishEdit();
              return;
            }
            if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
              e.preventDefault();
              submit(true);
              return;
            }
            if (e.key === 'Enter' && !e.shiftKey) {
              e.preventDefault();
              submit(false);
            }
          }}
          aria-label="输入"
        />
        <div className="cfoot">
          <span className="ctl mono" title="本线程的 Agent 与能力锚点">
            {task.preferredAdapter} · {task.requiredCapability}
          </span>
          <span className="hint no-mobile">{editing ? '↵ 保存' : worker ? '↵ 投递' : busy ? '运行中 · 新消息会排队' : '空闲'}</span>
          {editing ? (
            <>
              <button type="button" className="cbtn" onClick={finishEdit}>
                取消
              </button>
              <button type="button" className="cbtn pri" disabled={!text.trim() || pending} onClick={() => submit(false)}>
                保存 <span className="kb">↵</span>
              </button>
            </>
          ) : worker ? (
            <button type="button" className="cbtn pri" disabled={!text.trim() || pending} onClick={() => submit(false)}>
              发到消息板 <span className="kb">↵</span>
            </button>
          ) : busy ? (
            <>
              <button type="button" className="cbtn" disabled={!text.trim() || pending} onClick={() => submit(true)} title="中断当前轮并优先执行">
                <SteerIcon size={12} /> Steer <span className="kb">⌘↵</span>
              </button>
              <button type="button" className="cbtn pri" disabled={!text.trim() || pending} onClick={() => submit(false)}>
                加入队列 <span className="kb">↵</span>
              </button>
              <button type="button" className="stopb" title="停止当前轮" disabled={cancel.isPending} onClick={() => cancel.mutate()}>
                <i />
              </button>
            </>
          ) : (
            <button type="button" className="cbtn pri" disabled={!text.trim() || pending} onClick={() => submit(false)}>
              发送 <span className="kb">↵</span>
            </button>
          )}
        </div>
      </div>
    </div>
  );
}
