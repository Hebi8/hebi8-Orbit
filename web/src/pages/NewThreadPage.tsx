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
import { DeviceRoleIcon } from '../components/DeviceRoleIcon';
import { GitZshBadge } from '../components/GitZshBadge';
import { PageHeader } from '../components/PageHeader';
import {
  defaultModeFor,
  findAgentProbe,
  isFullBypassMode,
  nextModeOverrideAfterSend,
  probeEfforts,
  probeModels,
  probeModes,
} from '../lib/agentCaps';
import { api, errMsg } from '../lib/api';
import { PA_PROJECT_ID, SCRATCH_PROJECT_ID, cleanMachineName, isVirtualProject } from '../lib/format';
import { resolveEffectiveLocalMachineId } from '../lib/ide';
import { navigate, paths, type NewMode } from '../lib/router';
import { Composer } from '../session/Composer';
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
    tab: '项目',
    heading: '给项目派一个任务',
    hint: '选择项目目录开始执行；可按需开启 Worktree 隔离，关闭时直接与当前目录文件保持同步。',
    placeholder: '描述你想让 Agent 完成的目标、范围或验收标准…',
    starters: [
      '排查最近的接口超时问题，补齐回归测试并整理改动说明',
      '给核心接口加上边界校验与单元测试，保持向后兼容',
      '梳理当前模块的架构方案文档，并列出分阶段改造清单',
    ],
  },
  pa: {
    tab: 'Hebi8',
    heading: 'Hebi8 主对话',
    hint: '直连工作区唯一的 Hebi8 持续主对话，支持引用任意历史对话与项目线程上下文展开讨论、总结进展或拆解下一步计划。',
    placeholder: '问问今天的优先级、卡住的线程，或引用几个对话让 Hebi8 帮你梳理…',
    starters: [
      '帮我看看现在哪些线程需要我处理，按紧急程度排一下',
      '结合我引用的对话，帮我总结关键结论和待决事项',
      '帮我列一下今天最值得先推进的两件事',
    ],
  },
  chat: {
    tab: '对话',
    heading: '自由对话',
    hint: '直接开聊，不绑定项目目录，支持自由选择 Agent、模型与权限模式。',
    placeholder: '直接开始聊，或贴一段背景材料…',
    starters: [
      '帮我对比一下这两种数据同步方案的利弊和风险',
      '起草一份面向团队的架构评审提纲（Markdown 格式）',
      '把下面这段零散笔记整理成清晰的需求与里程碑清单',
    ],
  },
};

type OpenMenu = null | 'machine' | 'project' | 'source';

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
    () => boot.projects.filter((p) => !isVirtualProject(p.projectId) && !p.archived),
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
  const [effort, setEffort] = useState('');
  const [permMode, setPermMode] = useState('');
  const [useWorktree, setUseWorktree] = useState(false);
  const [refIds, setRefIds] = useState<string[]>([]);
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
      if (e.key === 'Escape') {
        if (useWorkbenchStore.getState().cmdkOpen) return;
        e.preventDefault();
        e.stopImmediatePropagation();
        e.stopPropagation();
        setOpenMenu(null);
      }
    };
    const onDismissAll = () => setOpenMenu(null);
    document.addEventListener('mousedown', onDown);
    window.addEventListener('keydown', onKey, true);
    window.addEventListener('orbit:dismiss-popovers', onDismissAll);
    return () => {
      document.removeEventListener('mousedown', onDown);
      window.removeEventListener('keydown', onKey, true);
      window.removeEventListener('orbit:dismiss-popovers', onDismissAll);
    };
  }, [openMenu]);

  const project = projects.find((p) => p.projectId === pid);
  const parts: ProjectPartView[] = project?.parts ?? [];
  const activePart: ProjectPartView | undefined =
    parts.find((pt) => pt.partId === partId) ?? parts[0];

  useEffect(() => {
    setPartId(project?.parts?.[0]?.partId ?? '');
    if (!initialAdapter) setAdapter('');
  }, [pid, project?.parts?.[0]?.partId, initialAdapter]);

  // Smart target machine pre-selection (quietly auto-routed behind the scenes).
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
      const key = m === 'project' && pid ? pid : m === 'pa' ? PA_PROJECT_ID : SCRATCH_PROJECT_ID;
      const rec = saved[key];
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

  const matchedLocation: ProjectLocationView | undefined = useMemo(() => {
    if (!activePart || !selectedMachine) return undefined;
    return (
      activePart.locations.find((l) => l.machineId === selectedMachine.machineId) ??
      activePart.locations.find((l) => l.isDefault) ??
      activePart.locations[0]
    );
  }, [activePart, selectedMachine]);

  // Plain folder is determined strictly by repo/folder kind — NEVER by isolation === 'in_place'.
  const isPlainFolder =
    activePart?.kind === 'folder' ||
    !!project?.repoIdentity?.startsWith('folder:') ||
    (matchedLocation?.runtime != null && !matchedLocation.runtime.isGit);

  const projectGitIsolation = useMemo(() => {
    if (matchedLocation && activePart?.kind !== 'folder') {
      return matchedLocation.isolation;
    }
    for (const pt of project?.parts ?? []) {
      if (pt.kind === 'folder') continue;
      const loc = pt.locations.find((l) => l.isDefault) ?? pt.locations[0];
      if (loc) return loc.isolation;
    }
    return undefined;
  }, [matchedLocation, activePart?.kind, project?.parts]);

  useEffect(() => {
    if (isPlainFolder) {
      setUseWorktree(false);
    } else {
      setUseWorktree(projectGitIsolation === 'worktree');
    }
  }, [pid, activePart?.partId, matchedLocation?.locationId, projectGitIsolation, isPlainFolder]);

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

  const activeAdapterId = (adapter || defaultDisplayedAdapter) as SessionAdapter | '';
  const activeProbe = useMemo(
    () => findAgentProbe(execMachines, selectedMachine?.machineId, activeAdapterId),
    [execMachines, selectedMachine?.machineId, activeAdapterId],
  );

  const modelChoices = useMemo(
    () => probeModels(activeProbe, activeAdapterId || 'agy', model),
    [activeProbe, activeAdapterId, model],
  );
  const effortChoices = useMemo(
    () => probeEfforts(activeProbe, effort),
    [activeProbe, effort],
  );
  const modeChoices = useMemo(
    () => probeModes(activeProbe, activeAdapterId || 'agy', permMode),
    [activeProbe, activeAdapterId, permMode],
  );

  useEffect(() => {
    setModel('');
    setEffort('');
    setPermMode('');
  }, [activeAdapterId]);

  useEffect(() => {
    setPermMode('');
  }, [m]);

  const effectivePermMode = permMode || defaultModeFor(activeProbe, activeAdapterId || 'agy', m === 'pa');

  const submit = useMutation({
    mutationFn: async () => {
      const goal = text.trim();
      if (!machineHasReadyAgent) {
        throw new Error('当前设备没有可用的 Agent，请先到设置或引导页接入本地 Agent');
      }
      const effectiveAdapter = (adapter || defaultDisplayedAdapter) as SessionAdapter;
      const req: CreateTaskRequest = { goal };
      if (selectedMachine?.machineId) req.machineId = selectedMachine.machineId;
      if (effectiveAdapter) req.adapter = effectiveAdapter;
      const cfg: Record<string, string> = {};
      if (model.trim()) cfg.model = model.trim();
      if (effort.trim()) cfg.thought_level = effort.trim();
      if (effectivePermMode) cfg.mode = effectivePermMode;
      if (permMode && isFullBypassMode(permMode)) req.explicitFullPermission = true;
      if (Object.keys(cfg).length > 0) req.config = cfg;
      if (refIds.length > 0) req.referencedTaskIds = refIds;

      try {
        const key = m === 'project' && pid ? pid : m === 'pa' ? PA_PROJECT_ID : SCRATCH_PROJECT_ID;
        const prev = JSON.parse(localStorage.getItem('orbit.lastPick') || '{}') as Record<string, unknown>;
        prev[key] = { machineId: selectedMachine?.machineId, adapter: effectiveAdapter };
        localStorage.setItem('orbit.lastPick', JSON.stringify(prev));
      } catch {
        // ignore
      }

      if (m === 'pa') {
        const main = await api.getHebi8Main();
        await api.followup(main.task.taskId, {
          message: goal,
          mode: 'queue',
          machineId: selectedMachine?.machineId,
          adapter: effectiveAdapter,
          config: Object.keys(cfg).length > 0 ? cfg : undefined,
          explicitFullPermission: req.explicitFullPermission,
          referencedTaskIds: refIds.length > 0 ? refIds : undefined,
        });
        return { kind: 'pa' as const, id: main.task.taskId, pid: PA_PROJECT_ID };
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
      if (!isPlainFolder) {
        req.useWorktree = useWorktree;
      }
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
      // Clear draft, referenced conversations, and attached reference chips immediately on submit.
      setText('');
      setRefIds([]);
      setSources([]);
      setSrcUri('');
      setSrcLabel('');
      setOpenMenu(null);
      setPermMode((cur) => nextModeOverrideAfterSend(cur));
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
      void qc.invalidateQueries({ queryKey: ['all-tasks'] });
      void qc.invalidateQueries({ queryKey: ['project-tasks', r.pid] });
      if (r.kind === 'pa') {
        void qc.invalidateQueries({ queryKey: ['hebi8-main'] });
        void qc.invalidateQueries({ queryKey: ['task-timeline', r.id] });
        navigate(paths.hebi8());
      } else {
        navigate(paths.session(r.id));
      }
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const canSend =
    !!text.trim() &&
    !submit.isPending &&
    (m !== 'project' || !!pid) &&
    machineHasReadyAgent;
  const send = () => {
    if (canSend) submit.mutate();
  };
  const copy = MODE_COPY[m];

  const addDraftSource = () => {
    const uri = srcUri.trim();
    if (!uri) return;
    setSources((xs) => [...xs, { key: ++srcKey, kind: srcKind, label: srcLabel.trim() || uri, uriOrRef: uri }]);
    setSrcUri('');
    setSrcLabel('');
    setOpenMenu(null);
  };

  const boundMachineSet = useMemo(() => {
    const set = new Set<string>();
    if (m === 'project' && activePart) {
      for (const l of activePart.locations) set.add(l.machineId);
    }
    return set;
  }, [m, activePart]);

  const attachSourceControl =
    m === 'project' ? (
      <div className="relpos">
        <button
          type="button"
          className={`nt-ico-btn ${openMenu === 'source' ? 'open' : ''}`}
          title="附加参考（链接 / 文件 / 记忆）"
          aria-label="附加参考"
          aria-expanded={openMenu === 'source'}
          onClick={() => setOpenMenu(openMenu === 'source' ? null : 'source')}
        >
          <Plus size={13.5} strokeWidth={1.8} />
        </button>
        {openMenu === 'source' && (
          <div className="nt-pop nt-pop-form" style={{ right: 0, top: 'calc(100% + 6px)' }}>
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
    ) : undefined;

  const sourceChips =
    m === 'project' && sources.length > 0 ? (
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
    ) : undefined;

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

          {(totalReadyAgents === 0 || !machineHasReadyAgent) && (
            <div className="nt-no-agent">
              <span>{totalReadyAgents === 0 ? '还没有可用的本地 Agent' : '当前设备还没有可用的 Agent'}</span>
              <button type="button" className="nt-no-agent-link" onClick={() => navigate(paths.onboarding())}>
                去接入 Agent
              </button>
            </div>
          )}

          <div className="nt-lody-stack" ref={composerWrapRef}>
            {/* Fixed-height context strip kept across all 3 tracks for 0px vertical shift */}
            <div className="nt-ctx-bar">
              <div className="nt-ctx-left">
                {m === 'project' && project && (
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
                )}

                {m === 'chat' && (
                  <span className="muted ell" style={{ fontSize: 12 }}>自由对话 · 不绑定项目</span>
                )}

                {m === 'pa' && (
                  <span className="muted ell" style={{ fontSize: 12 }}>Hebi8 主对话 · 支持跨对话引用</span>
                )}

                {execMachines.length > 1 && selectedMachine && (
                  <div className="relpos">
                    <button
                      type="button"
                      className={`nt-ctx-mach ${openMenu === 'machine' ? 'open' : ''}`}
                      onClick={() => setOpenMenu(openMenu === 'machine' ? null : 'machine')}
                      title="切换执行节点"
                      aria-label="选择执行节点"
                    >
                      <DeviceRoleIcon
                        machine={selectedMachine}
                        isActiveDuty={selectedMachine.machineId === activeDutyId}
                        isCurrentDevice={selectedMachine.machineId === effectiveLocalId}
                        size={11.5}
                      />
                      <span className="ell">{selectedMachine.displayAlias || cleanMachineName(selectedMachine)}</span>
                      <ChevronDown size={10} strokeWidth={1.8} />
                    </button>
                    {openMenu === 'machine' && (
                      <div className="nt-pop" role="menu" style={{ left: 0, top: 'calc(100% + 6px)' }}>
                        <div className="nt-pop-hd">执行节点</div>
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
              </div>

              {/* Right context controls: optional project source attachment + fixed "Worktree" label */}
              <div className="nt-ctx-right">
                {attachSourceControl}
                {m === 'project' && !isPlainFolder && (
                  <label
                    className={`nt-wt-toggle ${useWorktree ? 'on' : ''}`}
                    title={
                      useWorktree
                        ? '已开启 Worktree 隔离；完成后由 Orbit 自动打包交付改动'
                        : '未开启 Worktree；与当前项目目录文件直接保持同步'
                    }
                  >
                    <input
                      type="checkbox"
                      checked={useWorktree}
                      onChange={(e) => setUseWorktree(e.target.checked)}
                    />
                    <span className="nt-wt-track" aria-hidden="true">
                      <span className="nt-wt-thumb" />
                    </span>
                    <span className="nt-wt-lbl">Worktree</span>
                  </label>
                )}
              </div>
            </div>

            {/* Unified site-wide Composer with indicatorMode="quota" */}
            <Composer
              standalone
              indicatorMode="quota"
              value={text}
              onChange={setText}
              onSubmit={send}
              placeholder={copy.placeholder}
              autoFocus
              textareaRef={taRef}
              canSend={canSend}
              submitting={submit.isPending}
              controlsDisabled={!machineHasReadyAgent}
              availableAdapters={readyAdaptersOnMachine}
              effectiveAdapter={activeAdapterId}
              onAdapterChange={(adp) => setAdapter(adp)}
              activeProbe={activeProbe}
              effectiveModel={model}
              modelChoices={modelChoices}
              onModelChange={setModel}
              effectiveEffort={effort}
              effortChoices={effortChoices}
              onEffortChange={setEffort}
              effectiveMode={effectivePermMode}
              modeChoices={modeChoices}
              onModeChange={setPermMode}
              refIds={refIds}
              onRefIdsChange={setRefIds}
              projects={boot.projects}
              refProminent={m === 'pa'}
              chipsSlot={sourceChips}
            />
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
