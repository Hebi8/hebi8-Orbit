import { useEffect, useMemo, useRef, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { ArrowRight, Plus, Settings2, X } from 'lucide-react';
import type {
  BootstrapResponse,
  CreateTaskRequest,
  CreateTaskSourceInput,
  SessionAdapter,
  TaskSourceKind,
} from '../generated/protocol';
import { SessionAdapterValues } from '../generated/protocol';
import { agentDisplayName } from '../components/AgentList';
import { SendUpIcon } from '../components/Icons';
import { PageHeader } from '../components/PageHeader';
import { api, errMsg } from '../lib/api';
import { SCRATCH_PROJECT_ID, cleanMachineName } from '../lib/format';
import { navigate, paths, type NewMode } from '../lib/router';
import { toast } from '../store/useWorkbenchStore';

interface SourceDraft extends CreateTaskSourceInput {
  key: number;
}

let srcKey = 0;

interface ModeCopy {
  tab: string;
  heading: string;
  hint: string;
  placeholder: string;
  starters: string[];
}

const MODE_COPY: Record<NewMode, ModeCopy> = {
  project: {
    tab: '任务',
    heading: '给项目派一个任务',
    hint: '绑定代码仓库并在隔离工作区里执行，完成后可在 Source Review 中检视代码变更与 Artifacts。',
    placeholder: '描述你想让 Agent 完成的目标、范围或验收标准…',
    starters: [
      '排查最近的接口超时问题，补齐回归测试并整理改动说明',
      '给核心接口加上边界校验与单元测试，保持向后兼容',
      '梳理当前模块的架构方案文档，并列出分阶段改造清单',
    ],
  },
  pa: {
    tab: '问 PA',
    heading: '问 PA 私人助手',
    hint: '跨项目串联记忆、总结各线程进展，并按当前问题给出行动建议或直接帮你拉起新线程。',
    placeholder: '问问今天的优先级、卡住的线程，或让 PA 帮你拆解计划…',
    starters: [
      '帮我看看现在哪些线程需要我处理，按紧急程度排一下',
      '汇总本周各个项目的关键交付和还没合入的改动',
      '帮我列一下今天最值得先推进的两件事',
    ],
  },
  chat: {
    tab: '对话',
    heading: '自由对话',
    hint: '不绑定代码仓库，适合构思方案、讨论技术选型或起草文档，需要时随时可以绑定到项目。',
    placeholder: '直接开始聊，或贴一段背景材料…',
    starters: [
      '帮我对比一下这两种数据同步方案的利弊和风险',
      '起草一份面向团队的架构评审提纲（Markdown 格式）',
      '把下面这段零散笔记整理成清晰的需求与里程碑清单',
    ],
  },
};

export function NewThreadPage({
  boot,
  mode,
  projectId,
  initialMachineId,
  initialAdapter,
}: {
  boot: BootstrapResponse;
  mode?: NewMode;
  projectId?: string;
  initialMachineId?: string;
  initialAdapter?: string;
}) {
  const qc = useQueryClient();
  const projects = useMemo(() => boot.projects.filter((p) => p.projectId !== SCRATCH_PROJECT_ID && !p.archived), [boot.projects]);
  const execMachines = useMemo(() => boot.machines.filter((mach) => mach.transportMode !== 'ssh_outbound'), [boot.machines]);

  const [m, setM] = useState<NewMode>(mode ?? (projects.length ? 'project' : 'chat'));
  const [pid, setPid] = useState<string>(projectId ?? projects[0]?.projectId ?? '');
  const [text, setText] = useState('');
  const [machineId, setMachineId] = useState<string>(initialMachineId ?? '');
  const [adapter, setAdapter] = useState<string>(initialAdapter ?? '');
  const [model, setModel] = useState('');
  const [advOpen, setAdvOpen] = useState(false);
  const [capability, setCapability] = useState('');
  const [importDirty, setImportDirty] = useState(false);
  const [sources, setSources] = useState<SourceDraft[]>([]);
  const taRef = useRef<HTMLTextAreaElement | null>(null);

  useEffect(() => {
    if (mode) setM(mode);
  }, [mode]);
  useEffect(() => {
    if (projectId) {
      setPid(projectId);
      setM('project');
    }
  }, [projectId]);

  const project = projects.find((p) => p.projectId === pid);

  // Smart target machine pre-selection: explicit query > project anchor > lastPick > client machine > first online machine with ready agent.
  useEffect(() => {
    if (initialMachineId && execMachines.some((x) => x.machineId === initialMachineId)) {
      setMachineId(initialMachineId);
      return;
    }
    if (m === 'project' && project) {
      const anchorMach = project.anchors[project.defaultCapability]?.machine;
      if (anchorMach && execMachines.some((x) => x.machineId === anchorMach)) {
        setMachineId(anchorMach);
        return;
      }
    }
    try {
      const saved = JSON.parse(localStorage.getItem('orbit.lastPick') || '{}') as Record<
        string,
        { machineId?: string; adapter?: string }
      >;
      const rec = saved[m === 'project' && pid ? pid : SCRATCH_PROJECT_ID];
      if (rec?.machineId && execMachines.some((x) => x.machineId === rec.machineId && x.status === 'online')) {
        setMachineId(rec.machineId);
        if (rec.adapter && !initialAdapter) setAdapter(rec.adapter);
        return;
      }
    } catch {
      // ignore storage errors
    }
    const clientMach = boot.setup?.currentMachineId;
    if (clientMach && execMachines.some((x) => x.machineId === clientMach && x.status === 'online')) {
      setMachineId(clientMach);
      return;
    }
    const firstReady = execMachines.find(
      (x) => x.status === 'online' && (x.agents ?? []).some((a) => a.state === 'ready' && a.enabled),
    );
    if (firstReady) {
      setMachineId(firstReady.machineId);
    } else if (execMachines[0]) {
      setMachineId(execMachines[0].machineId);
    }
  }, [m, pid, project, execMachines, boot.setup?.currentMachineId, initialMachineId, initialAdapter]);

  const selectedMachine = execMachines.find((x) => x.machineId === machineId) ?? execMachines[0];
  const machineProbes = selectedMachine?.agents ?? [];

  const totalReadyAgents = useMemo(() => {
    let n = 0;
    for (const mach of execMachines) {
      if (mach.status !== 'online') continue;
      for (const ag of mach.agents ?? []) {
        if (ag.state === 'ready' && ag.enabled) n++;
      }
    }
    return n;
  }, [execMachines]);

  const readyAdaptersOnMachine = useMemo(() => {
    const ready: SessionAdapter[] = [];
    for (const adp of SessionAdapterValues) {
      const probe = machineProbes.find((p) => p.adapter === adp);
      if (probe ? probe.state === 'ready' && probe.enabled : (selectedMachine?.adapters ?? []).includes(adp)) {
        ready.push(adp);
      }
    }
    return ready;
  }, [machineProbes, selectedMachine]);

  const machineHasReadyAgent = readyAdaptersOnMachine.length > 0;
  const isRemoteSelectedMachine =
    !!selectedMachine?.transportMode && selectedMachine.transportMode !== 'local_ws';

  const defaultDisplayedAdapter: SessionAdapter | '' = useMemo(() => {
    if (!machineHasReadyAgent) return '';
    const def = project?.defaultAdapter;
    if (m === 'project' && def && (readyAdaptersOnMachine as readonly string[]).includes(def)) {
      return def as SessionAdapter;
    }
    return readyAdaptersOnMachine[0];
  }, [m, project, machineHasReadyAgent, readyAdaptersOnMachine]);

  // Clear explicit adapter whenever it is not ready on the currently selected machine (including 0-ready state) (#17).
  useEffect(() => {
    if (adapter && !(readyAdaptersOnMachine as readonly string[]).includes(adapter)) {
      setAdapter('');
    }
  }, [adapter, readyAdaptersOnMachine]);

  // Disable dirty workspace import automatically when switching to a remote machine (#6).
  useEffect(() => {
    if (isRemoteSelectedMachine && importDirty) {
      setImportDirty(false);
    }
  }, [isRemoteSelectedMachine, importDirty]);

  useEffect(() => {
    setCapability('');
    if (!initialAdapter) {
      setAdapter('');
    }
  }, [pid, initialAdapter]);

  const submit = useMutation({
    mutationFn: async () => {
      const goal = text.trim();
      if (m === 'pa') {
        const created = await api.createPAThread();
        const res = await api.paChat({ threadId: created.thread.paThreadId, message: goal });
        return { kind: 'pa' as const, id: res.thread.paThreadId };
      }
      if (!machineHasReadyAgent) {
        throw new Error('当前机器没有可用的 Agent，请先到设置页启用或安装');
      }
      const effectiveAdapter = (adapter || defaultDisplayedAdapter) as SessionAdapter;
      const req: CreateTaskRequest = { goal };
      if (machineId) req.machineId = machineId;
      if (effectiveAdapter) req.adapter = effectiveAdapter;
      if (model.trim()) req.config = { model: model.trim() };

      try {
        const key = m === 'project' && pid ? pid : SCRATCH_PROJECT_ID;
        const prev = JSON.parse(localStorage.getItem('orbit.lastPick') || '{}') as Record<string, unknown>;
        prev[key] = { machineId, adapter: effectiveAdapter };
        localStorage.setItem('orbit.lastPick', JSON.stringify(prev));
      } catch {
        // ignore
      }

      if (m === 'chat') {
        const res = await api.createTask(SCRATCH_PROJECT_ID, req);
        return { kind: 'task' as const, id: res.task.taskId, pid: SCRATCH_PROJECT_ID };
      }
      if (!pid) throw new Error('请选择项目');
      if (capability) req.requiredCapability = capability;
      if (importDirty) req.importDirtyWorkspace = true;
      const srcs = sources
        .filter((s) => s.uriOrRef.trim())
        .map(({ kind, label, uriOrRef }) => ({ kind, label: label.trim() || uriOrRef.trim(), uriOrRef: uriOrRef.trim() }));
      if (srcs.length) req.sources = srcs;
      const res = await api.createTask(pid, req);
      return { kind: 'task' as const, id: res.task.taskId, pid };
    },
    onSuccess: (r) => {
      setText('');
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
      if (r.kind === 'pa') {
        void qc.invalidateQueries({ queryKey: ['pa-threads'] });
        navigate(paths.pa(r.id));
      } else {
        void qc.invalidateQueries({ queryKey: ['project-tasks', r.pid] });
        navigate(paths.session(r.id));
      }
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const canSend =
    !!text.trim() &&
    !submit.isPending &&
    (m !== 'project' || !!pid) &&
    (m === 'pa' || machineHasReadyAgent);
  const send = () => canSend && submit.mutate();
  const copy = MODE_COPY[m];

  return (
    <main className="main">
      <PageHeader>
        <span className="ttl">新建线程</span>
      </PageHeader>
      <div className="page-scroll">
        <div className="nt-wrap">
          {/* Fixed-height header guarantees 0.0px vertical shift across tab switches */}
          <div className="nt-head">
            <div className="nt-tabs" role="tablist" aria-label="线程类型">
              <button
                type="button"
                role="tab"
                aria-selected={m === 'project'}
                className={`nt-tab ${m === 'project' ? 'active' : ''}`}
                disabled={projects.length === 0}
                onClick={() => setM('project')}
              >
                {MODE_COPY.project.tab}
              </button>
              <button
                type="button"
                role="tab"
                aria-selected={m === 'pa'}
                className={`nt-tab ${m === 'pa' ? 'active' : ''}`}
                onClick={() => setM('pa')}
              >
                {MODE_COPY.pa.tab}
              </button>
              <button
                type="button"
                role="tab"
                aria-selected={m === 'chat'}
                className={`nt-tab ${m === 'chat' ? 'active' : ''}`}
                onClick={() => setM('chat')}
              >
                {MODE_COPY.chat.tab}
              </button>
            </div>
            <h1 className="nt-heading">{copy.heading}</h1>
            <p className="nt-hint">{copy.hint}</p>
          </div>

          {m !== 'pa' && (totalReadyAgents === 0 || !machineHasReadyAgent) && (
            <div className="nt-no-agent">
              <span>{totalReadyAgents === 0 ? '还没有可用的 Agent' : '当前机器还没有可用的 Agent'}</span>
              <button type="button" className="nt-no-agent-link" onClick={() => navigate(paths.settings())}>
                去设置
              </button>
            </div>
          )}

          <div className="composer-card">
            <textarea
              ref={taRef}
              className="composer-ta"
              value={text}
              autoFocus
              placeholder={copy.placeholder}
              onChange={(e) => setText(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
                  e.preventDefault();
                  send();
                }
              }}
              aria-label="线程目标"
            />

            <div className="composer-bar">
              <div className="composer-opts">
                {m === 'project' && (
                  <label className="pill-select">
                    <select value={pid} onChange={(e) => setPid(e.target.value)} aria-label="项目">
                      {projects.map((p) => (
                        <option key={p.projectId} value={p.projectId}>
                          {p.displayName}
                        </option>
                      ))}
                    </select>
                  </label>
                )}
                {m !== 'pa' && execMachines.length > 1 && (
                  <label className="pill-select">
                    <select value={machineId} onChange={(e) => setMachineId(e.target.value)} aria-label="机器">
                      {execMachines.map((mach) => (
                        <option key={mach.machineId} value={mach.machineId}>
                          {mach.displayAlias || cleanMachineName(mach)}
                          {mach.status !== 'online' ? ' (离线)' : ''}
                        </option>
                      ))}
                    </select>
                  </label>
                )}
                {m !== 'pa' && (
                  <label className="pill-select">
                    <select
                      value={adapter}
                      disabled={!machineHasReadyAgent}
                      onChange={(e) => setAdapter(e.target.value)}
                      aria-label="Agent"
                    >
                      <option value="">
                        {defaultDisplayedAdapter
                          ? `默认 · ${agentDisplayName(defaultDisplayedAdapter)}`
                          : '无可用 Agent'}
                      </option>
                      {SessionAdapterValues.map((adp) => {
                        const probe = machineProbes.find((p) => p.adapter === adp);
                        const ready = readyAdaptersOnMachine.includes(adp);
                        return (
                          <option
                            key={adp}
                            value={adp}
                            disabled={!ready}
                            title={!ready ? probe?.detail || '该机器尚未就绪' : undefined}
                          >
                            {agentDisplayName(adp)}
                            {!ready ? '（未就绪）' : ''}
                          </option>
                        );
                      })}
                    </select>
                  </label>
                )}
                {m !== 'pa' && (
                  <button
                    type="button"
                    className={`pill-select ${advOpen ? 'on' : ''}`}
                    onClick={() => setAdvOpen(!advOpen)}
                  >
                    <Settings2 size={12} strokeWidth={1.7} /> 高级
                  </button>
                )}
                {m === 'pa' && <span className="pill-static">PA 助手</span>}
              </div>

              <button
                type="button"
                className={`send-icon-btn ${canSend ? 'is-ready' : 'is-idle'}`}
                disabled={!canSend}
                title="发送 (Enter)"
                aria-label="发送"
                onClick={send}
              >
                {submit.isPending ? <span className="spin" /> : <SendUpIcon size={14} />}
              </button>
            </div>

            {m !== 'pa' && advOpen && (
              <div className="adv">
                <span>模型</span>
                <input className="field" value={model} onChange={(e) => setModel(e.target.value)} placeholder="默认" aria-label="模型" />
                {m === 'project' && project && (
                  <>
                    <span>运行环境</span>
                    <select className="field" value={capability} onChange={(e) => setCapability(e.target.value)}>
                      <option value="">默认 · {project.defaultCapability}</option>
                      {Object.entries(project.anchors).map(([cap, a]) => (
                        <option key={cap} value={cap}>
                          {cap} · {a.machine}
                        </option>
                      ))}
                    </select>
                    <span>工作区</span>
                    <label className="chk" title={isRemoteSelectedMachine ? '远程机器项目暂不支持带入未提交改动' : undefined}>
                      <input
                        type="checkbox"
                        checked={importDirty}
                        disabled={isRemoteSelectedMachine}
                        onChange={(e) => setImportDirty(e.target.checked)}
                      />
                      带入当前未提交改动{isRemoteSelectedMachine ? '（仅本机项目支持）' : ''}
                    </label>
                    <span style={{ alignSelf: 'start', paddingTop: 6 }}>参考</span>
                    <div className="srcs">
                      {sources.map((s) => (
                        <div className="r" key={s.key}>
                          <select
                            className="field"
                            value={s.kind}
                            onChange={(e) =>
                              setSources((xs) => xs.map((x) => (x.key === s.key ? { ...x, kind: e.target.value as TaskSourceKind } : x)))
                            }
                          >
                            <option value="link">链接</option>
                            <option value="file">文件</option>
                            <option value="nmem">nmem</option>
                          </select>
                          <input
                            className="field lbl-in"
                            placeholder="标题"
                            value={s.label}
                            onChange={(e) => setSources((xs) => xs.map((x) => (x.key === s.key ? { ...x, label: e.target.value } : x)))}
                          />
                          <input
                            className="field"
                            placeholder={s.kind === 'link' ? 'https://…' : s.kind === 'file' ? '仓库内路径' : 'nmem 引用'}
                            value={s.uriOrRef}
                            onChange={(e) => setSources((xs) => xs.map((x) => (x.key === s.key ? { ...x, uriOrRef: e.target.value } : x)))}
                          />
                          <button type="button" className="ib" title="移除" onClick={() => setSources((xs) => xs.filter((x) => x.key !== s.key))}>
                            <X size={13} strokeWidth={1.6} />
                          </button>
                        </div>
                      ))}
                      <button
                        type="button"
                        className="btn ghost"
                        style={{ alignSelf: 'flex-start' }}
                        onClick={() => setSources((xs) => [...xs, { key: ++srcKey, kind: 'link', label: '', uriOrRef: '' }])}
                      >
                        <Plus size={13} strokeWidth={1.6} /> 添加参考
                      </button>
                    </div>
                  </>
                )}
              </div>
            )}
          </div>

          <div className="nt-starters">
            <div className="nt-starters-lbl">可以这样开场</div>
            <div className="starter-list">
              {copy.starters.map((s) => (
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
    </main>
  );
}
