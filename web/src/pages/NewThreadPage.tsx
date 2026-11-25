import { useEffect, useMemo, useRef, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import {
  ArrowRight,
  Check,
  ChevronDown,
  Folder,
  GitBranch,
  Link2,
  Plus,
  Shield,
  ShieldAlert,
  ShieldCheck,
  Sparkles,
  X,
} from 'lucide-react';
import type {
  BootstrapResponse,
  CreateTaskRequest,
  CreateTaskSourceInput,
  ProjectLocationView,
  ProjectPartView,
  SessionAdapter,
  TaskSourceKind,
} from '../generated/protocol';
import { SessionAdapterValues } from '../generated/protocol';
import { agentDisplayName } from '../components/AgentList';
import { DeviceRoleIcon } from '../components/DeviceRoleIcon';
import { GitZshBadge } from '../components/GitZshBadge';
import { SendUpIcon } from '../components/Icons';
import { PageHeader } from '../components/PageHeader';
import { api, errMsg } from '../lib/api';
import { SCRATCH_PROJECT_ID, cleanMachineName } from '../lib/format';
import { resolveEffectiveLocalMachineId } from '../lib/ide';
import { navigate, paths, type NewMode } from '../lib/router';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';

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
    hint: '选择设备与目录开始执行，代码仓库默认隔离工作区，非 Git 目录直接记录文件变更清单。',
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

type PermLevel = 'default' | 'strict' | 'auto';

const PERM_META: Record<PermLevel, { label: string; desc: string; modeVal: string }> = {
  default: { label: '标准权限', desc: '高风险操作询问确认', modeVal: '' },
  strict: { label: '严格审批', desc: '写文件与执行命令均询问', modeVal: 'ask' },
  auto: { label: '自动放行', desc: '工作区内免确认快速执行', modeVal: 'auto' },
};

type OpenMenu = null | 'machine' | 'project' | 'agent' | 'perm' | 'source';

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
  const storedLocalId = useWorkbenchStore((s) => s.localMachineId);
  const projects = useMemo(
    () => boot.projects.filter((p) => p.projectId !== SCRATCH_PROJECT_ID && !p.archived),
    [boot.projects],
  );
  const execMachines = useMemo(
    () => boot.machines.filter((mach) => mach.transportMode !== 'ssh_outbound'),
    [boot.machines],
  );
  const effectiveLocalId = resolveEffectiveLocalMachineId(
    boot.machines,
    boot.setup?.currentMachineId || storedLocalId,
  );
  const activeDutyId = boot.setup?.activeDutyMachineId ?? '';

  const [m, setM] = useState<NewMode>(mode ?? (projects.length ? 'project' : 'chat'));
  const [pid, setPid] = useState<string>(projectId ?? projects[0]?.projectId ?? '');
  const [partId, setPartId] = useState<string>('');
  const [text, setText] = useState('');
  const [machineId, setMachineId] = useState<string>(initialMachineId ?? '');
  const [adapter, setAdapter] = useState<string>(initialAdapter ?? '');
  const [model, setModel] = useState('');
  const [perm, setPerm] = useState<PermLevel>('default');
  const [importDirty, setImportDirty] = useState(false);
  const [sources, setSources] = useState<SourceDraft[]>([]);
  const [srcKind, setSrcKind] = useState<TaskSourceKind>('link');
  const [srcUri, setSrcUri] = useState('');
  const [srcLabel, setSrcLabel] = useState('');
  const [openMenu, setOpenMenu] = useState<OpenMenu>(null);
  const taRef = useRef<HTMLTextAreaElement | null>(null);
  const composerWrapRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (mode) setM(mode);
  }, [mode]);
  useEffect(() => {
    if (projectId) {
      setPid(projectId);
      setM('project');
    }
  }, [projectId]);

  useEffect(() => {
    if (!openMenu) return;
    const onDown = (e: MouseEvent) => {
      if (composerWrapRef.current && !composerWrapRef.current.contains(e.target as Node)) {
        setOpenMenu(null);
      }
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpenMenu(null);
    };
    window.addEventListener('mousedown', onDown);
    window.addEventListener('keydown', onKey);
    return () => {
      window.removeEventListener('mousedown', onDown);
      window.removeEventListener('keydown', onKey);
    };
  }, [openMenu]);

  const project = projects.find((p) => p.projectId === pid);
  const parts: ProjectPartView[] = project?.parts ?? [];
  const activePart: ProjectPartView | undefined =
    parts.find((pt) => pt.partId === partId) ?? parts[0];

  // Reset part when switching project
  useEffect(() => {
    setPartId(project?.parts?.[0]?.partId ?? '');
    if (!initialAdapter) setAdapter('');
  }, [pid, project?.parts?.[0]?.partId, initialAdapter]);

  // Smart target machine pre-selection: explicit query > part default location > anchor > lastPick > client device > first ready.
  useEffect(() => {
    if (initialMachineId && execMachines.some((x) => x.machineId === initialMachineId)) {
      setMachineId(initialMachineId);
      return;
    }
    if (m === 'project' && activePart && activePart.locations.length > 0) {
      const defLoc = activePart.locations.find((l) => l.isDefault) ?? activePart.locations[0];
      if (defLoc && execMachines.some((x) => x.machineId === defLoc.machineId)) {
        setMachineId(defLoc.machineId);
        return;
      }
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
    if (effectiveLocalId && execMachines.some((x) => x.machineId === effectiveLocalId && x.status === 'online')) {
      setMachineId(effectiveLocalId);
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
  }, [m, pid, project, activePart?.partId, execMachines, effectiveLocalId, initialMachineId, initialAdapter]);

  const selectedMachine = execMachines.find((x) => x.machineId === machineId) ?? execMachines[0];
  const machineProbes = selectedMachine?.agents ?? [];

  // Match bound location on selected machine for activePart (if any)
  const matchedLocation: ProjectLocationView | undefined = useMemo(() => {
    if (!activePart || !selectedMachine) return undefined;
    return (
      activePart.locations.find((l) => l.machineId === selectedMachine.machineId) ??
      activePart.locations.find((l) => l.isDefault) ??
      activePart.locations[0]
    );
  }, [activePart, selectedMachine]);

  const isPlainFolder =
    activePart?.kind === 'folder' ||
    matchedLocation?.isolation === 'in_place' ||
    !!project?.repoIdentity?.startsWith('folder:');

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

  useEffect(() => {
    if (adapter && !(readyAdaptersOnMachine as readonly string[]).includes(adapter)) {
      setAdapter('');
    }
  }, [adapter, readyAdaptersOnMachine]);

  useEffect(() => {
    if ((isRemoteSelectedMachine || isPlainFolder) && importDirty) {
      setImportDirty(false);
    }
  }, [isRemoteSelectedMachine, isPlainFolder, importDirty]);

  const submit = useMutation({
    mutationFn: async () => {
      const goal = text.trim();
      if (m === 'pa') {
        const created = await api.createPAThread();
        const res = await api.paChat({ threadId: created.thread.paThreadId, message: goal });
        return { kind: 'pa' as const, id: res.thread.paThreadId };
      }
      if (!machineHasReadyAgent) {
        throw new Error('当前设备没有可用的 Agent，请先到设置页启用或安装');
      }
      const effectiveAdapter = (adapter || defaultDisplayedAdapter) as SessionAdapter;
      const req: CreateTaskRequest = { goal };
      if (selectedMachine?.machineId) req.machineId = selectedMachine.machineId;
      if (effectiveAdapter) req.adapter = effectiveAdapter;
      const cfg: Record<string, string> = {};
      if (model.trim()) cfg.model = model.trim();
      if (PERM_META[perm].modeVal) cfg.mode = PERM_META[perm].modeVal;
      if (Object.keys(cfg).length > 0) req.config = cfg;

      try {
        const key = m === 'project' && pid ? pid : SCRATCH_PROJECT_ID;
        const prev = JSON.parse(localStorage.getItem('orbit.lastPick') || '{}') as Record<string, unknown>;
        prev[key] = { machineId: selectedMachine?.machineId, adapter: effectiveAdapter };
        localStorage.setItem('orbit.lastPick', JSON.stringify(prev));
      } catch {
        // ignore
      }

      if (m === 'chat') {
        const res = await api.createTask(SCRATCH_PROJECT_ID, req);
        return { kind: 'task' as const, id: res.task.taskId, pid: SCRATCH_PROJECT_ID };
      }
      if (!pid) throw new Error('请选择项目');
      if (activePart?.partId) req.partId = activePart.partId;
      if (matchedLocation && matchedLocation.machineId === selectedMachine?.machineId) {
        req.locationId = matchedLocation.locationId;
      }
      if (importDirty && !isPlainFolder && !isRemoteSelectedMachine) req.importDirtyWorkspace = true;
      const srcs = sources
        .filter((s) => s.uriOrRef.trim())
        .map(({ kind, label, uriOrRef }) => ({
          kind,
          label: label.trim() || uriOrRef.trim(),
          uriOrRef: uriOrRef.trim(),
        }));
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

  const activeAdapterId = (adapter || defaultDisplayedAdapter) as SessionAdapter | '';
  const agentTriggerLabel = activeAdapterId
    ? `${agentDisplayName(activeAdapterId)}${model.trim() ? ` · ${model.trim()}` : ''}`
    : '无可用 Agent';

  const addDraftSource = () => {
    const uri = srcUri.trim();
    if (!uri) return;
    setSources((xs) => [...xs, { key: ++srcKey, kind: srcKind, label: srcLabel.trim() || uri, uriOrRef: uri }]);
    setSrcUri('');
    setSrcLabel('');
    setOpenMenu(null);
  };

  // Eligible machines for a project: if the active part has bound locations, show bound machines first.
  const boundMachineSet = useMemo(() => {
    const set = new Set<string>();
    if (m === 'project' && activePart) {
      for (const l of activePart.locations) set.add(l.machineId);
    }
    return set;
  }, [m, activePart]);

  return (
    <main className="main">
      <PageHeader>
        <span className="ttl">新建线程</span>
      </PageHeader>
      <div className="page-scroll">
        <div className="nt-wrap">
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
              <span>{totalReadyAgents === 0 ? '还没有可用的 Agent' : '当前设备还没有可用的 Agent'}</span>
              <button type="button" className="nt-no-agent-link" onClick={() => navigate(paths.settings())}>
                去设置
              </button>
            </div>
          )}

          {/* Lody-style 2-bar composer: detached top context bar + clean input card */}
          <div className="nt-lody-stack" ref={composerWrapRef}>
            {m !== 'pa' && (
              <div className="nt-ctx-bar">
                <div className="nt-ctx-left">
                  {/* Machine picker trigger */}
                  {selectedMachine && (
                    <div className="relpos">
                      <button
                        type="button"
                        className={`nt-ctx-btn ${openMenu === 'machine' ? 'open' : ''}`}
                        onClick={() => setOpenMenu(openMenu === 'machine' ? null : 'machine')}
                        aria-label="选择设备"
                      >
                        <DeviceRoleIcon
                          machine={selectedMachine}
                          isActiveDuty={selectedMachine.machineId === activeDutyId}
                          isCurrentDevice={selectedMachine.machineId === effectiveLocalId}
                          size={13}
                        />
                        <span className="ell">{selectedMachine.displayAlias || cleanMachineName(selectedMachine)}</span>
                        {execMachines.length > 1 && <ChevronDown size={11} strokeWidth={1.8} className="muted" />}
                      </button>
                      {openMenu === 'machine' && execMachines.length > 1 && (
                        <div className="nt-pop" role="menu" style={{ left: 0, top: 'calc(100% + 6px)' }}>
                          <div className="nt-pop-hd">执行设备</div>
                          {execMachines.map((mach) => {
                            const disabledByPart =
                              m === 'project' && boundMachineSet.size > 0 && !boundMachineSet.has(mach.machineId);
                            return (
                              <button
                                key={mach.machineId}
                                type="button"
                                role="menuitem"
                                className={`nt-pop-item ${mach.machineId === selectedMachine.machineId ? 'on' : ''}`}
                                disabled={disabledByPart}
                                title={disabledByPart ? '该项目未绑定此设备的目录' : undefined}
                                onClick={() => {
                                  setMachineId(mach.machineId);
                                  setOpenMenu(null);
                                }}
                              >
                                <DeviceRoleIcon
                                  machine={mach}
                                  isActiveDuty={mach.machineId === activeDutyId}
                                  isCurrentDevice={mach.machineId === effectiveLocalId}
                                  size={13}
                                />
                                <span className="ell grow">{mach.displayAlias || cleanMachineName(mach)}</span>
                                {mach.status !== 'online' && <span className="faint" style={{ fontSize: 11 }}>离线</span>}
                                {disabledByPart && <span className="faint" style={{ fontSize: 11 }}>未绑定</span>}
                                {mach.machineId === selectedMachine.machineId && (
                                  <Check size={12.5} strokeWidth={2.2} className="ok-t" />
                                )}
                              </button>
                            );
                          })}
                        </div>
                      )}
                    </div>
                  )}

                  {m === 'project' && project && (
                    <>
                      <span className="nt-ctx-sep" aria-hidden="true" />
                      {/* Project / Part picker trigger */}
                      <div className="relpos">
                        <button
                          type="button"
                          className={`nt-ctx-btn ${openMenu === 'project' ? 'open' : ''}`}
                          onClick={() => setOpenMenu(openMenu === 'project' ? null : 'project')}
                          aria-label="选择项目与目录"
                        >
                          {isPlainFolder ? (
                            <Folder size={13} strokeWidth={1.7} className="muted" />
                          ) : (
                            <GitBranch size={13} strokeWidth={1.7} className="muted" />
                          )}
                          <span className="ell">
                            {project.displayName}
                            {parts.length > 1 && activePart && activePart.name !== project.displayName
                              ? ` · ${activePart.name}`
                              : ''}
                          </span>
                          <GitZshBadge
                            runtime={matchedLocation?.runtime}
                            fallbackBranch={project.defaultBranch}
                            isGit={!isPlainFolder}
                          />
                          <ChevronDown size={11} strokeWidth={1.8} className="muted" />
                        </button>
                        {openMenu === 'project' && (
                          <div className="nt-pop nt-pop-wide" role="menu" style={{ left: 0, top: 'calc(100% + 6px)' }}>
                            <div className="nt-pop-hd">项目与目录</div>
                            {projects.map((p) => {
                              const pParts = p.parts ?? [];
                              if (pParts.length > 1) {
                                return (
                                  <div key={p.projectId} className="nt-pop-group">
                                    <div className="nt-pop-sub">{p.displayName}</div>
                                    {pParts.map((pt) => {
                                      const isSel = pid === p.projectId && activePart?.partId === pt.partId;
                                      return (
                                        <button
                                          key={pt.partId}
                                          type="button"
                                          role="menuitem"
                                          className={`nt-pop-item indent ${isSel ? 'on' : ''}`}
                                          onClick={() => {
                                            setPid(p.projectId);
                                            setPartId(pt.partId);
                                            const defLoc = pt.locations.find((l) => l.isDefault) ?? pt.locations[0];
                                            if (defLoc) setMachineId(defLoc.machineId);
                                            setOpenMenu(null);
                                          }}
                                        >
                                          {pt.kind === 'folder' ? (
                                            <Folder size={12.5} strokeWidth={1.6} className="muted" />
                                          ) : (
                                            <GitBranch size={12.5} strokeWidth={1.7} className="muted" />
                                          )}
                                          <span className="ell grow">{pt.name}</span>
                                          {isSel && <Check size={12.5} strokeWidth={2.2} className="ok-t" />}
                                        </button>
                                      );
                                    })}
                                  </div>
                                );
                              }
                              const singlePart = pParts[0];
                              const singleFolder =
                                singlePart?.kind === 'folder' || !!p.repoIdentity?.startsWith('folder:');
                              const isSel = pid === p.projectId;
                              return (
                                <button
                                  key={p.projectId}
                                  type="button"
                                  role="menuitem"
                                  className={`nt-pop-item ${isSel ? 'on' : ''}`}
                                  onClick={() => {
                                    setPid(p.projectId);
                                    setPartId(singlePart?.partId ?? '');
                                    const defLoc =
                                      singlePart?.locations.find((l) => l.isDefault) ?? singlePart?.locations[0];
                                    if (defLoc) setMachineId(defLoc.machineId);
                                    setOpenMenu(null);
                                  }}
                                >
                                  {singleFolder ? (
                                    <Folder size={12.5} strokeWidth={1.6} className="muted" />
                                  ) : (
                                    <GitBranch size={12.5} strokeWidth={1.7} className="muted" />
                                  )}
                                  <span className="ell grow">{p.displayName}</span>
                                  {isSel && <Check size={12.5} strokeWidth={2.2} className="ok-t" />}
                                </button>
                              );
                            })}
                          </div>
                        )}
                      </div>
                    </>
                  )}

                  {m === 'chat' && (
                    <>
                      <span className="nt-ctx-sep" aria-hidden="true" />
                      <span className="muted" style={{ fontSize: 12 }}>自由对话 · 不绑定目录</span>
                    </>
                  )}
                </div>

                {m === 'project' && !isPlainFolder && !isRemoteSelectedMachine && (
                  <label className="nt-dirty-chk" title="带入本机当前未提交改动">
                    <input
                      type="checkbox"
                      checked={importDirty}
                      onChange={(e) => setImportDirty(e.target.checked)}
                    />
                    <span>含未提交改动</span>
                  </label>
                )}
              </div>
            )}

            {/* Main input box */}
            <div className="composer-card nt-lody-card">
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

              {sources.length > 0 && (
                <div className="nt-src-chips">
                  {sources.map((s) => (
                    <span className="nt-src-chip" key={s.key}>
                      <Link2 size={11} strokeWidth={1.7} />
                      <span className="ell">{s.label}</span>
                      <button
                        type="button"
                        aria-label={`移除 ${s.label}`}
                        onClick={() => setSources((xs) => xs.filter((x) => x.key !== s.key))}
                      >
                        <X size={11} strokeWidth={1.8} />
                      </button>
                    </span>
                  ))}
                </div>
              )}

              <div className="nt-lody-bar">
                <div className="nt-lody-left">
                  {/* Attach source (+) */}
                  {m === 'project' && (
                    <div className="relpos">
                      <button
                        type="button"
                        className={`nt-ico-btn ${openMenu === 'source' ? 'open' : ''}`}
                        title="附加参考（链接 / 文件 / 记忆）"
                        aria-label="附加参考"
                        onClick={() => setOpenMenu(openMenu === 'source' ? null : 'source')}
                      >
                        <Plus size={14.5} strokeWidth={1.8} />
                      </button>
                      {openMenu === 'source' && (
                        <div className="nt-pop nt-pop-form" style={{ left: 0, bottom: 'calc(100% + 8px)' }}>
                          <div className="nt-pop-hd">附加参考资料</div>
                          <div className="nt-pop-seg">
                            {(['link', 'file', 'nmem'] as TaskSourceKind[]).map((k) => (
                              <button
                                key={k}
                                type="button"
                                className={srcKind === k ? 'on' : ''}
                                onClick={() => setSrcKind(k)}
                              >
                                {k === 'link' ? '链接' : k === 'file' ? '文件' : 'nmem'}
                              </button>
                            ))}
                          </div>
                          <input
                            className="field"
                            placeholder={srcKind === 'link' ? 'https://…' : srcKind === 'file' ? '目录内相对路径' : 'nmem 记忆引用'}
                            value={srcUri}
                            onChange={(e) => setSrcUri(e.target.value)}
                            onKeyDown={(e) => {
                              if (e.key === 'Enter') {
                                e.preventDefault();
                                addDraftSource();
                              }
                            }}
                            autoFocus
                          />
                          <input
                            className="field"
                            placeholder="标题（可选）"
                            value={srcLabel}
                            onChange={(e) => setSrcLabel(e.target.value)}
                          />
                          <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 6 }}>
                            <button type="button" className="btn ghost sm" onClick={() => setOpenMenu(null)}>
                              取消
                            </button>
                            <button type="button" className="btn pri sm" disabled={!srcUri.trim()} onClick={addDraftSource}>
                              添加
                            </button>
                          </div>
                        </div>
                      )}
                    </div>
                  )}

                  {/* Agent + Model trigger */}
                  {m !== 'pa' ? (
                    <div className="relpos">
                      <button
                        type="button"
                        className={`nt-agent-btn ${openMenu === 'agent' ? 'open' : ''}`}
                        disabled={!machineHasReadyAgent}
                        onClick={() => setOpenMenu(openMenu === 'agent' ? null : 'agent')}
                        aria-label="选择 Agent 与模型"
                      >
                        <Sparkles size={13} strokeWidth={1.7} />
                        <span className="ell">{agentTriggerLabel}</span>
                        <ChevronDown size={11} strokeWidth={1.8} className="muted" />
                      </button>
                      {openMenu === 'agent' && (
                        <div className="nt-pop nt-pop-wide" role="menu" style={{ left: 0, bottom: 'calc(100% + 8px)' }}>
                          <div className="nt-pop-hd">Agent</div>
                          {SessionAdapterValues.map((adp) => {
                            const ready = readyAdaptersOnMachine.includes(adp);
                            const isSel = activeAdapterId === adp;
                            return (
                              <button
                                key={adp}
                                type="button"
                                role="menuitem"
                                className={`nt-pop-item ${isSel ? 'on' : ''}`}
                                disabled={!ready}
                                onClick={() => {
                                  setAdapter(adp);
                                  setOpenMenu(null);
                                }}
                              >
                                <span className="ell grow">{agentDisplayName(adp)}</span>
                                {!ready && <span className="faint" style={{ fontSize: 11 }}>未就绪</span>}
                                {isSel && <Check size={12.5} strokeWidth={2.2} className="ok-t" />}
                              </button>
                            );
                          })}
                          <div className="nt-pop-div" />
                          <div className="nt-pop-hd">指定模型（可选）</div>
                          <div style={{ padding: '4px 8px 6px' }}>
                            <input
                              className="field"
                              value={model}
                              onChange={(e) => setModel(e.target.value)}
                              placeholder="留空则跟随默认"
                              aria-label="模型"
                            />
                          </div>
                        </div>
                      )}
                    </div>
                  ) : (
                    <span className="pill-static">PA 助手</span>
                  )}

                  {/* Permission shield button */}
                  {m !== 'pa' && (
                    <div className="relpos">
                      <button
                        type="button"
                        className={`nt-ico-btn nt-perm-${perm} ${openMenu === 'perm' ? 'open' : ''}`}
                        title={`${PERM_META[perm].label} · ${PERM_META[perm].desc}`}
                        aria-label={`权限模式：${PERM_META[perm].label}`}
                        onClick={() => setOpenMenu(openMenu === 'perm' ? null : 'perm')}
                      >
                        {perm === 'auto' ? (
                          <ShieldAlert size={14} strokeWidth={1.8} />
                        ) : perm === 'strict' ? (
                          <ShieldCheck size={14} strokeWidth={1.8} />
                        ) : (
                          <Shield size={14} strokeWidth={1.8} />
                        )}
                      </button>
                      {openMenu === 'perm' && (
                        <div className="nt-pop nt-pop-wide" role="menu" style={{ left: 0, bottom: 'calc(100% + 8px)' }}>
                          <div className="nt-pop-hd">执行权限</div>
                          {(['default', 'strict', 'auto'] as PermLevel[]).map((lvl) => (
                            <button
                              key={lvl}
                              type="button"
                              role="menuitem"
                              className={`nt-pop-item ${perm === lvl ? 'on' : ''}`}
                              onClick={() => {
                                setPerm(lvl);
                                setOpenMenu(null);
                              }}
                            >
                              <div className="grow" style={{ textAlign: 'left' }}>
                                <div>{PERM_META[lvl].label}</div>
                                <div className="faint" style={{ fontSize: 11 }}>{PERM_META[lvl].desc}</div>
                              </div>
                              {perm === lvl && <Check size={12.5} strokeWidth={2.2} className="ok-t" />}
                            </button>
                          ))}
                        </div>
                      )}
                    </div>
                  )}
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
            </div>
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
