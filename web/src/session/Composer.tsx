import { useEffect, useMemo, useRef, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Check, MessageSquare, Pencil } from 'lucide-react';
import type {
  AnnotationView,
  BoardMemberCursor,
  FollowupMode,
  MachineView,
  PendingInput,
  SessionAdapter,
  TaskSummary,
} from '../generated/protocol';
import { SessionAdapterValues } from '../generated/protocol';
import { QueueSendIcon, SendUpIcon, StopSquareIcon, SwarmIcon } from '../components/Icons';
import { api, errMsg } from '../lib/api';
import { isTaskBusy } from '../lib/format';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';

/** Drafts survive session switches (per-task, in-memory). */
const drafts = new Map<string, string>();

const ADAPTER_LABELS: Record<string, string> = {
  claude: 'Claude Code',
  codex: 'Codex',
  agy: 'Antigravity',
};

const MODEL_PRESETS: Record<string, string[]> = {
  claude: ['opus', 'sonnet', 'haiku'],
  codex: ['gpt-6.2-sol', 'gpt-6.2-astra', 'gpt-6.1-sol'],
  agy: ['gemini-4-neon', 'gemini-4-carbon', 'gemini-4-argon'],
};

const EFFORT_OPTIONS = [
  { value: 'low', label: 'Low' },
  { value: 'medium', label: 'Medium' },
  { value: 'high', label: 'High' },
];

const MODE_OPTIONS = [
  { value: 'ask', label: 'Ask' },
  { value: 'auto', label: 'Auto' },
  { value: 'full', label: 'Full' },
];

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
  needsVerifyAttemptId,
  onEditDone,
  openAnnotations = [],
  runtimeConfig,
  machines = [],
  compact = false,
  onOpenReview,
}: {
  task: TaskSummary;
  worker?: string;
  members: BoardMemberCursor[];
  editing: PendingInput | null;
  needsVerifyAttemptId?: string;
  onEditDone: () => void;
  openAnnotations?: AnnotationView[];
  runtimeConfig?: Record<string, string>;
  machines?: MachineView[];
  compact?: boolean;
  onOpenReview?: () => void;
}) {
  const qc = useQueryClient();
  const remember = useWorkbenchStore((s) => s.rememberPrompt);
  const clearDeselected = useWorkbenchStore((s) => s.clearDeselectedAnnotations);
  const [text, setText] = useState(() => drafts.get(task.taskId) ?? '');
  const [siteVerified, setSiteVerified] = useState(false);
  const [adapterOverride, setAdapterOverride] = useState<string>('');
  const [modelOverride, setModelOverride] = useState<string>('');
  const [effortOverride, setEffortOverride] = useState<string>('');
  const [modeOverride, setModeOverride] = useState<string>('');
  const stash = useRef<string | null>(null);
  const taRef = useRef<HTMLTextAreaElement | null>(null);
  const busy = isTaskBusy(task);

  const taskAdapter = (task.preferredAdapter || 'agy') as SessionAdapter;
  const availableAdapters = useMemo(() => {
    const list: SessionAdapter[] = [];
    const seen = new Set<string>();
    for (const mach of machines) {
      for (const adp of mach.adapters ?? []) {
        if ((SessionAdapterValues as readonly string[]).includes(adp) && !seen.has(adp)) {
          seen.add(adp);
          list.push(adp as SessionAdapter);
        }
      }
    }
    if ((SessionAdapterValues as readonly string[]).includes(taskAdapter) && !seen.has(taskAdapter)) {
      list.unshift(taskAdapter);
    }
    if (list.length === 0) list.push('agy');
    return list;
  }, [machines, taskAdapter]);

  const effectiveAdapter = (adapterOverride || taskAdapter) as SessionAdapter;
  const effectiveModel = modelOverride || runtimeConfig?.model || '';
  const effectiveEffort = effortOverride || runtimeConfig?.thought_level || runtimeConfig?.effort || 'high';
  const effectiveMode = modeOverride || runtimeConfig?.mode || 'auto';

  const modelChoices = useMemo(() => {
    const base = MODEL_PRESETS[effectiveAdapter] ?? [];
    if (effectiveModel && !base.includes(effectiveModel)) return [effectiveModel, ...base];
    return base;
  }, [effectiveAdapter, effectiveModel]);

  useEffect(() => {
    setText(drafts.get(task.taskId) ?? '');
    setSiteVerified(false);
    setAdapterOverride('');
    setModelOverride('');
    setEffortOverride('');
    setModeOverride('');
  }, [task.taskId, needsVerifyAttemptId]);

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
    el.style.height = `${Math.min(Math.max(el.scrollHeight, 34), 180)}px`;
  };
  useEffect(grow, [text]);

  const setDraft = (v: string) => {
    setText(v);
    if (!editing) drafts.set(task.taskId, v);
  };

  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ['task-timeline', task.taskId] });
    void qc.invalidateQueries({ queryKey: ['project-tasks', task.projectId] });
    void qc.invalidateQueries({ queryKey: ['delivery'] });
    void qc.invalidateQueries({ queryKey: ['global-artifacts'] });
    void qc.invalidateQueries({ queryKey: ['bootstrap'] });
  };

  const finishEdit = () => {
    const restored = stash.current ?? '';
    stash.current = null;
    setText(restored);
    drafts.set(task.taskId, restored);
    onEditDone();
  };

  const buildRuntimePayload = () => {
    const cfg: Record<string, string> = {};
    if (modelOverride) cfg.model = modelOverride;
    if (effortOverride) cfg.thought_level = effortOverride;
    if (modeOverride) cfg.mode = modeOverride;
    return {
      adapter: adapterOverride ? (adapterOverride as SessionAdapter) : undefined,
      config: Object.keys(cfg).length > 0 ? cfg : undefined,
    };
  };

  const saveEdit = useMutation({
    mutationFn: (body: string) => api.updateQueueInput(task.taskId, editing!.inputId, body),
    onSuccess: (pi) => {
      remember(`inp:${pi.inputId}`, pi.body);
      toast('已更新');
      refresh();
      finishEdit();
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const followup = useMutation({
    mutationFn: (v: { message: string; mode: FollowupMode; verifiedSite?: boolean; verifiedAttemptId?: string }) => {
      const rt = buildRuntimePayload();
      return api.followup(task.taskId, {
        message: v.message,
        mode: v.mode,
        adapter: rt.adapter,
        config: rt.config,
        verifiedSite: v.verifiedSite,
        verifiedAttemptId: v.verifiedAttemptId,
      });
    },
    onSuccess: (res, v) => {
      if (res.attempt) remember(`att:${res.attempt.attemptId}`, v.message);
      if (res.pendingInput) remember(`inp:${res.pendingInput.inputId}`, v.message);
      if (res.pendingInput && v.mode === 'queue') toast('将在下一轮发出');
      if (v.mode === 'steer') toast('已打断当前轮，优先执行');
      setDraft('');
      setSiteVerified(false);
      refresh();
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const dispatchAnns = useMutation({
    mutationFn: (v: { ids: string[]; message: string; mode: FollowupMode }) =>
      api.dispatchAnnotations(task.taskId, {
        annotationIds: v.ids,
        message: v.message || undefined,
        mode: v.mode,
      }),
    onSuccess: (res, v) => {
      clearDeselected(task.taskId);
      setDraft('');
      setSiteVerified(false);
      if (res.attempt) toast(`已连同 ${v.ids.length} 条批注发出`);
      else if (res.pendingInput) toast(`已入队 · ${v.ids.length} 条批注`);
      refresh();
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const boardPost = useMutation({
    mutationFn: (v: { target: string; body: string }) =>
      api.postBoardMessage(task.taskId, { authorName: 'human', targetMention: v.target, body: v.body }),
    onSuccess: (msg) => {
      toast(`已发给 @${msg.targetMention ?? ''}`);
      setDraft('');
      void qc.invalidateQueries({ queryKey: ['task-board', task.taskId] });
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const cancel = useMutation({
    mutationFn: () => api.cancelTask(task.taskId, 'stopped from composer'),
    onSuccess: (r) => {
      toast(r.cancelSent || r.immediateState ? '已请求停止' : '停止请求已记录');
      refresh();
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const pending = followup.isPending || dispatchAnns.isPending || boardPost.isPending || saveEdit.isPending;
  const needsVerifyGate = !editing && !worker && !busy && !!needsVerifyAttemptId;
  const hasAttachments = !editing && !worker && openAnnotations.length > 0;
  const hasPayload = !!text.trim() || hasAttachments;

  const submit = (steer: boolean) => {
    const msg = text.trim();
    if (pending) return;
    if (editing) {
      if (!msg) return;
      saveEdit.mutate(msg);
      return;
    }
    if (worker) {
      if (!msg) return;
      boardPost.mutate({ target: worker, body: msg });
      return;
    }
    if (msg) {
      const mention = parseMention(msg, members);
      if (mention) {
        boardPost.mutate(mention);
        return;
      }
    }
    if (!hasPayload) return;
    if (needsVerifyGate && !siteVerified) {
      toast('先看一眼工作区并勾选「看过了」再继续', 'err');
      return;
    }
    const mode: FollowupMode = steer && busy ? 'steer' : 'queue';
    if (hasAttachments) {
      dispatchAnns.mutate({
        ids: openAnnotations.map((a) => a.annotationId),
        message: msg,
        mode,
      });
      return;
    }
    followup.mutate({
      message: msg,
      mode,
      verifiedSite: needsVerifyGate ? true : undefined,
      verifiedAttemptId: needsVerifyGate ? needsVerifyAttemptId : undefined,
    });
  };

  // Locked v2 rule: completely blank placeholder when idle; only show concise shortcut when busy.
  let placeholder = '';
  if (editing) placeholder = '编辑这条…';
  else if (worker) placeholder = `发给 @${worker}`;
  else if (busy) placeholder = 'Enter 入队 · ⌘↵ 立刻插入';

  // Pure icon-only state action button
  const renderActionButton = () => {
    if (editing) {
      return (
        <div className="cfoot-acts">
          <button type="button" className="btn ghost" onClick={finishEdit}>
            取消
          </button>
          <button
            type="button"
            className={`send-icon-btn ${text.trim() && !pending ? 'is-ready' : 'is-idle'}`}
            disabled={!text.trim() || pending}
            title="保存 (Enter)"
            aria-label="保存"
            onClick={() => submit(false)}
          >
            {saveEdit.isPending ? <span className="spin" /> : <Check size={14} strokeWidth={2} />}
          </button>
        </div>
      );
    }

    if (busy && !hasPayload && !worker) {
      return (
        <button
          type="button"
          className="send-icon-btn is-stop"
          disabled={cancel.isPending}
          title="停止当前轮"
          aria-label="停止当前轮"
          onClick={() => cancel.mutate()}
        >
          {cancel.isPending ? <span className="spin" /> : <StopSquareIcon size={12} />}
        </button>
      );
    }

    if (busy && hasPayload && !worker) {
      return (
        <button
          type="button"
          className="send-icon-btn is-queue"
          disabled={pending}
          title="入队等待当前轮结束 (⌘↵ 立即插入)"
          aria-label="入队发送"
          onClick={() => submit(false)}
        >
          {pending ? <span className="spin" /> : <QueueSendIcon size={14} />}
        </button>
      );
    }

    const canSend = hasPayload && !pending && !(needsVerifyGate && !siteVerified);
    return (
      <button
        type="button"
        className={`send-icon-btn ${canSend ? 'is-ready' : 'is-idle'}`}
        disabled={!canSend}
        title={hasAttachments ? '连同 Review 批注一起发送 (Enter)' : '发送 (Enter)'}
        aria-label="发送"
        onClick={() => submit(false)}
      >
        {pending ? <span className="spin" /> : <SendUpIcon size={14} />}
      </button>
    );
  };

  return (
    <div className="cwrap">
      <div className={`composer ${editing ? 'editing' : ''}`}>
        {editing && (
          <div className="cbanner">
            <Pencil size={12} strokeWidth={1.6} /> 编辑中
            <span className="r">Esc 取消</span>
          </div>
        )}
        {worker && !editing && (
          <div className="cbanner" style={{ color: 'var(--muted)' }}>
            <SwarmIcon size={12} /> 发给 <b style={{ color: 'var(--fg)' }}>@{worker}</b>
          </div>
        )}
        {needsVerifyGate && (
          <div className="cbanner">
            <span>上一轮中途断了，先看一眼工作区</span>
            <label className="chk" style={{ marginLeft: 'auto', cursor: 'pointer' }}>
              <input type="checkbox" checked={siteVerified} onChange={(e) => setSiteVerified(e.target.checked)} />
              看过了
            </label>
          </div>
        )}

        {/* Pending Review comments attachment pill at top of composer */}
        {hasAttachments && (
          <div className="composer-attach-row">
            <button
              type="button"
              className="attach-chip"
              title="发送将一并附带这些 Review 批注"
              onClick={() => onOpenReview?.()}
            >
              <MessageSquare size={12} strokeWidth={1.8} />
              <span>{openAnnotations.length} 条 Review 批注</span>
            </button>
          </div>
        )}

        <textarea
          ref={taRef}
          value={text}
          rows={1}
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

        <div className="composer-meta-bar">
          {!worker ? (
            <div className="runtime-controls" aria-label="会话运行配置">
              <label className="rt-pill" title="Agent">
                <select
                  value={effectiveAdapter}
                  onChange={(e) => {
                    setAdapterOverride(e.target.value);
                    setModelOverride('');
                  }}
                  aria-label="Agent"
                >
                  {availableAdapters.map((a) => (
                    <option key={a} value={a}>
                      {ADAPTER_LABELS[a] ?? a}
                    </option>
                  ))}
                </select>
              </label>
              <span className="rt-sep">·</span>
              <label className="rt-pill" title="模型">
                <select
                  value={effectiveModel}
                  onChange={(e) => setModelOverride(e.target.value)}
                  aria-label="模型"
                >
                  <option value="">默认模型</option>
                  {modelChoices.map((mc) => (
                    <option key={mc} value={mc}>
                      {mc}
                    </option>
                  ))}
                </select>
              </label>
              {!compact && (
                <>
                  <span className="rt-sep">·</span>
                  <label className="rt-pill" title="思考强度">
                    <select
                      value={effectiveEffort}
                      onChange={(e) => setEffortOverride(e.target.value)}
                      aria-label="Effort"
                    >
                      {EFFORT_OPTIONS.map((opt) => (
                        <option key={opt.value} value={opt.value}>
                          {opt.label}
                        </option>
                      ))}
                    </select>
                  </label>
                  <span className="rt-sep">·</span>
                  <label className="rt-pill" title="权限模式">
                    <select
                      value={effectiveMode}
                      onChange={(e) => setModeOverride(e.target.value)}
                      aria-label="权限"
                    >
                      {MODE_OPTIONS.map((opt) => (
                        <option key={opt.value} value={opt.value}>
                          {opt.label}
                        </option>
                      ))}
                    </select>
                  </label>
                </>
              )}
            </div>
          ) : (
            <span className="rt-pill">@{worker}</span>
          )}
          {renderActionButton()}
        </div>
      </div>
    </div>
  );
}
