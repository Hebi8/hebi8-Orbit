import { useEffect, useMemo, useRef, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Check, MessageSquare, Pencil } from 'lucide-react';
import type {
  AnnotationView,
  BoardMemberCursor,
  FollowupMode,
  MachineView,
  PendingInput,
  ProjectView,
  SessionAdapter,
  TaskSummary,
} from '../generated/protocol';
import { SessionAdapterValues } from '../generated/protocol';
import { QueueSendIcon, SendUpIcon, StopSquareIcon, SwarmIcon } from '../components/Icons';
import { TaskRefButton, TaskRefChips } from '../components/TaskRefPicker';
import {
  ADAPTER_LABELS,
  defaultModeDisplayLabel,
  defaultModeFor,
  effortLabel,
  findAgentProbe,
  isFullBypassMode,
  modeLabel,
  nextModeOverrideAfterSend,
  probeEfforts,
  probeModels,
  probeModes,
} from '../lib/agentCaps';
import { api, errMsg } from '../lib/api';
import { PA_PROJECT_ID, isTaskBusy } from '../lib/format';
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
  needsVerifyAttemptId,
  onEditDone,
  openAnnotations = [],
  runtimeConfig,
  machines = [],
  projects = [],
  machineId,
  compact = false,
  inDrawer = false,
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
  projects?: ProjectView[];
  machineId?: string;
  compact?: boolean;
  inDrawer?: boolean;
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
  const [refIds, setRefIds] = useState<string[]>([]);
  const stash = useRef<string | null>(null);
  const taRef = useRef<HTMLTextAreaElement | null>(null);
  const activeTaskIdRef = useRef(task.taskId);
  activeTaskIdRef.current = task.taskId;
  const busy = isTaskBusy(task);
  const isPA = task.projectId === PA_PROJECT_ID;

  const taskAdapter = (task.preferredAdapter || 'agy') as SessionAdapter;
  const availableAdapters = useMemo(() => {
    const list: SessionAdapter[] = [];
    const seen = new Set<string>();
    for (const mach of machines) {
      if (mach.transportMode === 'ssh_outbound') continue;
      for (const ag of mach.agents ?? []) {
        if (ag.state === 'ready' && ag.enabled && (SessionAdapterValues as readonly string[]).includes(ag.adapter) && !seen.has(ag.adapter)) {
          seen.add(ag.adapter);
          list.push(ag.adapter as SessionAdapter);
        }
      }
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
  const activeProbe = useMemo(
    () => findAgentProbe(machines, machineId, effectiveAdapter),
    [machines, machineId, effectiveAdapter],
  );

  const effectiveModel = modelOverride || runtimeConfig?.model || activeProbe?.defaultModel || '';
  const effectiveEffort =
    effortOverride ||
    runtimeConfig?.thought_level ||
    runtimeConfig?.effort ||
    runtimeConfig?.reasoning_effort ||
    activeProbe?.defaultEffort ||
    '';
  const effectiveMode =
    modeOverride ||
    runtimeConfig?.mode ||
    defaultModeFor(activeProbe, effectiveAdapter, isPA);

  const modelChoices = useMemo(
    () => probeModels(activeProbe, effectiveAdapter, effectiveModel),
    [activeProbe, effectiveAdapter, effectiveModel],
  );
  const effortChoices = useMemo(
    () => probeEfforts(activeProbe, effectiveEffort),
    [activeProbe, effectiveEffort],
  );
  const modeChoices = useMemo(
    () => probeModes(activeProbe, effectiveAdapter, effectiveMode),
    [activeProbe, effectiveAdapter, effectiveMode],
  );

  useEffect(() => {
    setText(drafts.get(task.taskId) ?? '');
    setSiteVerified(false);
    setAdapterOverride('');
    setModelOverride('');
    setEffortOverride('');
    setModeOverride('');
    setRefIds([]);
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

  const refresh = (targetTaskId = task.taskId, targetParentId = task.parentTaskId, targetProjectId = task.projectId) => {
    void qc.invalidateQueries({ queryKey: ['task-timeline', targetTaskId] });
    if (targetParentId) {
      void qc.invalidateQueries({ queryKey: ['task-timeline', targetParentId] });
      void qc.invalidateQueries({ queryKey: ['task-children', targetParentId] });
    }
    void qc.invalidateQueries({ queryKey: ['task-children', targetTaskId] });
    void qc.invalidateQueries({ queryKey: ['project-tasks', targetProjectId] });
    void qc.invalidateQueries({ queryKey: ['all-tasks'] });
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
    const explicitFullPermission = modeOverride !== '' && isFullBypassMode(modeOverride) ? true : undefined;
    return {
      adapter: adapterOverride ? (adapterOverride as SessionAdapter) : undefined,
      config: Object.keys(cfg).length > 0 ? cfg : undefined,
      explicitFullPermission,
    };
  };

  const saveEdit = useMutation({
    mutationFn: (v: { submittedTaskId: string; submittedParentId?: string; submittedProjectId: string; inputId: string; body: string }) =>
      api.updateQueueInput(v.submittedTaskId, v.inputId, v.body),
    onSuccess: (pi, v) => {
      remember(`inp:${pi.inputId}`, pi.body);
      toast('已更新');
      refresh(v.submittedTaskId, v.submittedParentId, v.submittedProjectId);
      if (activeTaskIdRef.current === v.submittedTaskId) {
        finishEdit();
      }
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const followup = useMutation({
    mutationFn: (v: {
      submittedTaskId: string;
      submittedParentId?: string;
      submittedProjectId: string;
      message: string;
      mode: FollowupMode;
      referencedTaskIds?: string[];
      verifiedSite?: boolean;
      verifiedAttemptId?: string;
    }) => {
      const rt = buildRuntimePayload();
      return api.followup(v.submittedTaskId, {
        message: v.message,
        mode: v.mode,
        adapter: rt.adapter,
        config: rt.config,
        explicitFullPermission: rt.explicitFullPermission,
        referencedTaskIds: v.referencedTaskIds && v.referencedTaskIds.length > 0 ? v.referencedTaskIds : undefined,
        verifiedSite: v.verifiedSite,
        verifiedAttemptId: v.verifiedAttemptId,
      });
    },
    onSuccess: (res, v) => {
      if (res.attempt) remember(`att:${res.attempt.attemptId}`, v.message);
      if (res.pendingInput) remember(`inp:${res.pendingInput.inputId}`, v.message);
      if (res.pendingInput && v.mode === 'queue') toast('将在下一轮发出');
      if (v.mode === 'steer') toast('已打断当前轮，优先执行');
      drafts.set(v.submittedTaskId, '');
      if (activeTaskIdRef.current === v.submittedTaskId) {
        setText('');
        setRefIds([]);
        setSiteVerified(false);
        setModeOverride((cur) => nextModeOverrideAfterSend(cur));
      }
      refresh(v.submittedTaskId, v.submittedParentId, v.submittedProjectId);
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const dispatchAnns = useMutation({
    mutationFn: (v: {
      submittedTaskId: string;
      submittedParentId?: string;
      submittedProjectId: string;
      ids: string[];
      message: string;
      mode: FollowupMode;
    }) =>
      api.dispatchAnnotations(v.submittedTaskId, {
        annotationIds: v.ids,
        message: v.message || undefined,
        mode: v.mode,
      }),
    onSuccess: (res, v) => {
      clearDeselected(v.submittedTaskId);
      drafts.set(v.submittedTaskId, '');
      if (activeTaskIdRef.current === v.submittedTaskId) {
        setText('');
        setSiteVerified(false);
      }
      if (res.attempt) toast(`已连同 ${v.ids.length} 条批注发出`);
      else if (res.pendingInput) toast(`已入队 · ${v.ids.length} 条批注`);
      refresh(v.submittedTaskId, v.submittedParentId, v.submittedProjectId);
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const boardPost = useMutation({
    mutationFn: (v: { submittedTaskId: string; target: string; body: string }) =>
      api.postBoardMessage(v.submittedTaskId, { authorName: 'human', targetMention: v.target, body: v.body }),
    onSuccess: (msg, v) => {
      toast(`已发给 @${msg.targetMention ?? ''}`);
      drafts.set(v.submittedTaskId, '');
      if (activeTaskIdRef.current === v.submittedTaskId) {
        setText('');
      }
      void qc.invalidateQueries({ queryKey: ['task-board', v.submittedTaskId] });
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
    const submittedTaskId = task.taskId;
    const submittedParentId = task.parentTaskId;
    const submittedProjectId = task.projectId;
    if (editing) {
      if (!msg) return;
      saveEdit.mutate({
        submittedTaskId,
        submittedParentId,
        submittedProjectId,
        inputId: editing.inputId,
        body: msg,
      });
      return;
    }
    if (worker) {
      if (!msg) return;
      boardPost.mutate({ submittedTaskId, target: worker, body: msg });
      return;
    }
    if (msg) {
      const mention = parseMention(msg, members);
      if (mention) {
        boardPost.mutate({ submittedTaskId, ...mention });
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
        submittedTaskId,
        submittedParentId,
        submittedProjectId,
        ids: openAnnotations.map((a) => a.annotationId),
        message: msg,
        mode,
      });
      return;
    }
    followup.mutate({
      submittedTaskId,
      submittedParentId,
      submittedProjectId,
      message: msg,
      mode,
      referencedTaskIds: refIds.length > 0 ? refIds : undefined,
      verifiedSite: needsVerifyGate ? true : undefined,
      verifiedAttemptId: needsVerifyGate ? needsVerifyAttemptId : undefined,
    });
  };

  let placeholder = '';
  if (editing) placeholder = '编辑这条…';
  else if (worker) placeholder = `发给 @${worker}`;
  else if (busy) placeholder = 'Enter 入队 · ⌘↵ 立刻插入';

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
        <div className="cfoot-acts">
          {inDrawer && (
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
          )}
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
        </div>
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
    <div className={`cwrap ${inDrawer ? 'in-drawer' : ''}`}>
      <div className={`composer ${editing ? 'editing' : ''} ${inDrawer ? 'in-drawer' : ''}`}>
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

        {!editing && !worker && refIds.length > 0 && (
          <TaskRefChips
            selectedIds={refIds}
            onRemove={(id) => setRefIds((prev) => prev.filter((x) => x !== id))}
            projects={projects}
          />
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
              {!compact && !editing && (
                <TaskRefButton
                  selectedIds={refIds}
                  onChange={setRefIds}
                  projects={projects}
                  currentTaskId={task.taskId}
                  prominent={isPA}
                />
              )}
              <label className="rt-pill" title="Agent">
                <select
                  value={effectiveAdapter}
                  onChange={(e) => {
                    setAdapterOverride(e.target.value);
                    setModelOverride('');
                    setEffortOverride('');
                    setModeOverride('');
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
                  <option value="">
                    {activeProbe?.defaultModel ? `默认 (${activeProbe.defaultModel})` : '跟随 Agent 默认值'}
                  </option>
                  {modelChoices.map((mc) => (
                    <option key={mc} value={mc}>
                      {mc}
                    </option>
                  ))}
                </select>
              </label>
              {(!compact || inDrawer) && (
                <>
                  {effortChoices.length > 0 && (
                    <>
                      <span className="rt-sep">·</span>
                      <label className="rt-pill" title="思考强度">
                        <select
                          value={effectiveEffort}
                          onChange={(e) => setEffortOverride(e.target.value)}
                          aria-label="思考强度"
                        >
                          <option value="">跟随 Agent 默认值</option>
                          {effortChoices.map((eff) => (
                            <option key={eff} value={eff}>
                              {effortLabel(eff)}
                            </option>
                          ))}
                        </select>
                      </label>
                    </>
                  )}
                  <span className="rt-sep">·</span>
                  <label className="rt-pill" title="Agent 权限模式">
                    <select
                      value={effectiveMode}
                      onChange={(e) => setModeOverride(e.target.value)}
                      aria-label="权限模式"
                    >
                      <option value="">{defaultModeDisplayLabel(activeProbe, modeChoices)}</option>
                      {modeChoices.map((opt) => (
                        <option key={opt.id} value={opt.id} title={opt.description || undefined}>
                          {modeLabel(opt)}
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
