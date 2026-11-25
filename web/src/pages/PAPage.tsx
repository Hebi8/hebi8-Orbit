import { useEffect, useMemo, useRef, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { ArrowRight, ChevronDown, Plus, Sparkles, Zap } from 'lucide-react';
import type { BootstrapResponse, CreateTaskRequest, SessionAdapter } from '../generated/protocol';
import { SessionAdapterValues } from '../generated/protocol';
import { agentDisplayName } from '../components/AgentList';
import { SendUpIcon } from '../components/Icons';
import { PageHeader } from '../components/PageHeader';
import { StatusMark } from '../components/StatusMark';
import { TaskRefButton, TaskRefChips } from '../components/TaskRefPicker';
import { useProjectTasks, useTimeline } from '../hooks/queries';
import {
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
import { PA_PROJECT_ID, indexInboxByTask, relTime, sessionMark, taskTitle } from '../lib/format';
import { resolveEffectiveLocalMachineId } from '../lib/ide';
import { navigate, paths } from '../lib/router';
import { SessionConversation } from '../session/SessionConversation';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';

const PA_STARTERS = [
  '帮我汇总当前工作区各项目的最新进展和待处理事项，按优先级排序',
  '结合我引用的历史对话，梳理关键结论、分歧点和下一步行动清单',
  '帮我拆解今天最值得推进的两件事，并给出可执行的检查步骤',
];

const QUICK_BRIEF_PROMPT =
  '请基于当前工作区概览，简明汇总各个项目的最新进展、待我确认的审批或阻塞项，并按优先级给出接下来的建议。';

export function PAPage({ boot, threadId }: { boot: BootstrapResponse; threadId?: string }) {
  const qc = useQueryClient();
  const storedLocalId = useWorkbenchStore((s) => s.localMachineId);
  const paTasksQ = useProjectTasks(PA_PROJECT_ID);
  const paTasks = useMemo(
    () => [...(paTasksQ.data ?? [])].sort((a, b) => b.updatedAt - a.updatedAt),
    [paTasksQ.data],
  );
  const inboxByTask = useMemo(() => indexInboxByTask(boot.openInboxItems), [boot.openInboxItems]);

  const [creatingNew, setCreatingNew] = useState(false);
  const [menuOpen, setMenuOpen] = useState(false);

  // Sync route changes back out of creatingNew when navigating to a specific PA task
  useEffect(() => {
    if (threadId) setCreatingNew(false);
  }, [threadId]);

  const activeTaskId = creatingNew ? undefined : (threadId || paTasks[0]?.taskId);
  const activeTask = activeTaskId ? paTasks.find((t) => t.taskId === activeTaskId) : undefined;
  const tlq = useTimeline(activeTaskId);

  // New-PA composer state (real ACP session creation)
  const execMachines = useMemo(
    () => boot.machines.filter((m) => m.transportMode !== 'ssh_outbound'),
    [boot.machines],
  );
  const effectiveLocalId = resolveEffectiveLocalMachineId(
    boot.machines,
    boot.setup?.currentMachineId || storedLocalId,
  );
  const defaultMachine = useMemo(() => {
    if (effectiveLocalId) {
      const m = execMachines.find((x) => x.machineId === effectiveLocalId && x.status === 'online');
      if (m) return m;
    }
    return (
      execMachines.find((x) => x.status === 'online' && (x.agents ?? []).some((a) => a.state === 'ready' && a.enabled)) ??
      execMachines[0]
    );
  }, [execMachines, effectiveLocalId]);

  const readyAdapters = useMemo(() => {
    const ready: SessionAdapter[] = [];
    for (const adp of SessionAdapterValues) {
      const probe = defaultMachine?.agents?.find((p) => p.adapter === adp);
      if (probe ? probe.state === 'ready' && probe.enabled : (defaultMachine?.adapters ?? []).includes(adp)) {
        ready.push(adp);
      }
    }
    return ready;
  }, [defaultMachine]);

  const [text, setText] = useState('');
  const [refIds, setRefIds] = useState<string[]>([]);
  const [adapter, setAdapter] = useState<string>('');
  const [model, setModel] = useState('');
  const [effort, setEffort] = useState('');
  const [permMode, setPermMode] = useState('');
  const taRef = useRef<HTMLTextAreaElement | null>(null);

  const effectiveAdapter = (
    adapter && readyAdapters.includes(adapter as SessionAdapter) ? adapter : (readyAdapters[0] ?? '')
  ) as SessionAdapter | '';
  const activeProbe = useMemo(
    () => findAgentProbe(execMachines, defaultMachine?.machineId, effectiveAdapter),
    [execMachines, defaultMachine?.machineId, effectiveAdapter],
  );
  const modelChoices = useMemo(
    () => probeModels(activeProbe, effectiveAdapter || 'agy', model),
    [activeProbe, effectiveAdapter, model],
  );
  const effortChoices = useMemo(
    () => probeEfforts(activeProbe, effort),
    [activeProbe, effort],
  );
  const modeChoices = useMemo(
    () => probeModes(activeProbe, effectiveAdapter || 'agy', permMode),
    [activeProbe, effectiveAdapter, permMode],
  );
  const effectiveMode = permMode || defaultModeFor(activeProbe, effectiveAdapter || 'agy', true);

  const startPATask = useMutation({
    mutationFn: async (v: { goal: string; refs?: string[] }) => {
      if (!effectiveAdapter) {
        throw new Error('当前没有可用的本地 Agent，请先完成 Agent 接入');
      }
      const req: CreateTaskRequest = {
        goal: v.goal,
        adapter: effectiveAdapter,
        machineId: defaultMachine?.machineId,
      };
      const cfg: Record<string, string> = {};
      if (model) cfg.model = model;
      if (effort) cfg.thought_level = effort;
      if (effectiveMode) cfg.mode = effectiveMode;
      if (permMode && isFullBypassMode(permMode)) req.explicitFullPermission = true;
      if (Object.keys(cfg).length > 0) req.config = cfg;
      if (v.refs && v.refs.length > 0) req.referencedTaskIds = v.refs;
      return api.createTask(PA_PROJECT_ID, req);
    },
    onSuccess: (res) => {
      setText('');
      setRefIds([]);
      setPermMode((cur) => nextModeOverrideAfterSend(cur));
      setCreatingNew(false);
      void qc.invalidateQueries({ queryKey: ['project-tasks', PA_PROJECT_ID] });
      void qc.invalidateQueries({ queryKey: ['all-tasks'] });
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
      navigate(paths.pa(res.task.taskId));
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const quickBrief = useMutation({
    mutationFn: async () => {
      if (activeTaskId && tlq.data) {
        await api.followup(activeTaskId, {
          message: QUICK_BRIEF_PROMPT,
          mode: 'queue',
        });
        return activeTaskId;
      }
      const res = await startPATask.mutateAsync({ goal: QUICK_BRIEF_PROMPT, refs: refIds });
      return res.task.taskId;
    },
    onSuccess: (tid) => {
      void qc.invalidateQueries({ queryKey: ['task-timeline', tid] });
      void qc.invalidateQueries({ queryKey: ['project-tasks', PA_PROJECT_ID] });
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const sendNew = () => {
    const goal = text.trim();
    if (!goal || startPATask.isPending) return;
    startPATask.mutate({ goal, refs: refIds });
  };

  const currentMark = activeTask ? sessionMark(activeTask, inboxByTask.get(activeTask.taskId) ?? []) : null;

  return (
    <main className="main">
      <PageHeader
        right={
          <>
            <button
              type="button"
              className="ib"
              title="新建 PA 对话"
              aria-label="新建 PA 对话"
              onClick={() => {
                setMenuOpen(false);
                setCreatingNew(true);
                navigate(paths.pa());
              }}
            >
              <Plus size={14.5} strokeWidth={1.7} />
            </button>
            <button
              type="button"
              className="ib"
              title="工作区速览"
              aria-label="工作区速览"
              disabled={quickBrief.isPending || startPATask.isPending || readyAdapters.length === 0}
              onClick={() => quickBrief.mutate()}
            >
              <Zap size={14} strokeWidth={1.6} />
            </button>
          </>
        }
      >
        <div className="relpos" style={{ minWidth: 0 }}>
          <button type="button" className="crumb" onClick={() => setMenuOpen(!menuOpen)} title="切换 PA 对话">
            <span className="c0">PA</span>
            <span className="sep">/</span>
            <span className="c1">
              {creatingNew ? '新建对话' : activeTask ? taskTitle(activeTask) : tlq.data?.task ? taskTitle(tlq.data.task) : '新对话'}
            </span>
            {currentMark && <StatusMark mark={currentMark} />}
            <ChevronDown size={13} strokeWidth={1.6} className="faint" />
          </button>
          {menuOpen && (
            <>
              <div
                className="scrim"
                style={{ background: 'transparent', backdropFilter: 'none', zIndex: 49 }}
                onClick={() => setMenuOpen(false)}
              />
              <div className="menu" style={{ top: 32, left: 0, minWidth: 260 }}>
                <div className="mh">PA 历史对话</div>
                {paTasks.map((t) => {
                  const m = sessionMark(t, inboxByTask.get(t.taskId) ?? []);
                  return (
                    <button
                      type="button"
                      key={t.taskId}
                      className={!creatingNew && t.taskId === activeTaskId ? 'on' : ''}
                      onClick={() => {
                        setMenuOpen(false);
                        setCreatingNew(false);
                        navigate(paths.pa(t.taskId));
                      }}
                    >
                      {m && <StatusMark mark={m} />}
                      <span className="ell" style={{ flex: 1 }}>
                        {taskTitle(t)}
                      </span>
                      <span className="faint" style={{ fontSize: 11 }}>
                        {relTime(t.updatedAt)}
                      </span>
                    </button>
                  );
                })}
                {paTasks.length === 0 && <div className="mh">还没有 PA 对话</div>}
                <div style={{ borderTop: '1px solid var(--line)', marginTop: 4, paddingTop: 4 }}>
                  <button
                    type="button"
                    onClick={() => {
                      setMenuOpen(false);
                      setCreatingNew(true);
                      navigate(paths.pa());
                    }}
                  >
                    <Plus size={12} strokeWidth={1.6} />
                    <span>新建 PA 对话</span>
                  </button>
                </div>
              </div>
            </>
          )}
        </div>
      </PageHeader>

      {activeTaskId ? (
        tlq.isError ? (
          <div className="empty">载入 PA 对话失败：{errMsg(tlq.error)}</div>
        ) : !tlq.data ? (
          <div className="empty">载入对话…</div>
        ) : (
          <SessionConversation taskId={activeTaskId} tl={tlq.data} boot={boot} />
        )
      ) : (
        <div className="page-scroll">
          <div className="pa-empty-wrap">
            <div className="nt-head">
              <div className="pa-badge">
                <Sparkles size={13} strokeWidth={1.7} />
                <span>PA 私人助手</span>
              </div>
              <h1 className="nt-heading">跨对话梳理与决策助手</h1>
              <p className="nt-hint">
                直接连接本机真实 Agent，支持引用任意 Chat 或项目线程的真实上下文展开讨论与总结。
              </p>
            </div>

            {readyAdapters.length === 0 && (
              <div className="nt-no-agent">
                <span>还没有可用的本地 Agent，无法启动 PA 对话</span>
                <button type="button" className="nt-no-agent-link" onClick={() => navigate(paths.onboarding())}>
                  去接入 Agent
                </button>
              </div>
            )}

            <div className="composer-card nt-lody-card">
              {refIds.length > 0 && (
                <TaskRefChips
                  selectedIds={refIds}
                  onRemove={(id) => setRefIds((prev) => prev.filter((x) => x !== id))}
                  projects={boot.projects}
                />
              )}

              <textarea
                ref={taRef}
                className="composer-ta"
                value={text}
                autoFocus
                placeholder="问问今天的工作优先级，或点击下方「引用对话」带入任意历史会话上下文…"
                onChange={(e) => setText(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
                    e.preventDefault();
                    sendNew();
                  }
                }}
                aria-label="发给 PA"
              />

              <div className="composer-meta-bar" style={{ padding: '4px 10px 8px' }}>
                <div className="runtime-controls">
                  <TaskRefButton
                    selectedIds={refIds}
                    onChange={setRefIds}
                    projects={boot.projects}
                    prominent
                  />
                  {readyAdapters.length > 0 && (
                    <>
                      <label className="rt-pill" title="Agent">
                        <select
                          value={effectiveAdapter}
                          onChange={(e) => {
                            setAdapter(e.target.value);
                            setModel('');
                            setEffort('');
                            setPermMode('');
                          }}
                          aria-label="Agent"
                        >
                          {readyAdapters.map((adp) => (
                            <option key={adp} value={adp}>
                              {agentDisplayName(adp)}
                            </option>
                          ))}
                        </select>
                      </label>
                      <span className="rt-sep">·</span>
                      <label className="rt-pill" title="模型">
                        <select value={model} onChange={(e) => setModel(e.target.value)} aria-label="模型">
                          <option value="">
                            {activeProbe?.defaultModel ? `默认 (${activeProbe.defaultModel})` : '跟随 Agent 默认值'}
                          </option>
                          {modelChoices.map((mc) => (
                            <option key={mc} value={mc}>{mc}</option>
                          ))}
                        </select>
                      </label>
                      {effortChoices.length > 0 && (
                        <>
                          <span className="rt-sep">·</span>
                          <label className="rt-pill" title="思考强度">
                            <select value={effort} onChange={(e) => setEffort(e.target.value)} aria-label="思考强度">
                              <option value="">跟随 Agent 默认值</option>
                              {effortChoices.map((eff) => (
                                <option key={eff} value={eff}>{effortLabel(eff)}</option>
                              ))}
                            </select>
                          </label>
                        </>
                      )}
                      <span className="rt-sep">·</span>
                      <label className="rt-pill" title="权限模式">
                        <select value={effectiveMode} onChange={(e) => setPermMode(e.target.value)} aria-label="权限模式">
                          <option value="">{defaultModeDisplayLabel(activeProbe, modeChoices)}</option>
                          {modeChoices.map((opt) => (
                            <option key={opt.id} value={opt.id}>{modeLabel(opt)}</option>
                          ))}
                        </select>
                      </label>
                    </>
                  )}
                </div>

                <button
                  type="button"
                  className={`send-icon-btn ${text.trim() && !startPATask.isPending && readyAdapters.length > 0 ? 'is-ready' : 'is-idle'}`}
                  disabled={!text.trim() || startPATask.isPending || readyAdapters.length === 0}
                  title="发送 (Enter)"
                  aria-label="发送"
                  onClick={sendNew}
                >
                  {startPATask.isPending ? <span className="spin" /> : <SendUpIcon size={14} />}
                </button>
              </div>
            </div>

            <div className="nt-starters">
              <div className="nt-starters-lbl">试试这样问 PA</div>
              <div className="starter-list">
                {PA_STARTERS.map((s) => (
                  <button
                    type="button"
                    key={s}
                    className="starter-btn"
                    onClick={() => {
                      setText(s);
                      taRef.current?.focus();
                    }}
                  >
                    <span className="ell">{s}</span>
                    <ArrowRight size={13} strokeWidth={1.6} className="arr" />
                  </button>
                ))}
              </div>
            </div>
          </div>
        </div>
      )}
    </main>
  );
}
