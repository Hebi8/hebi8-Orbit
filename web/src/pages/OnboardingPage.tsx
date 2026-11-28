import { useEffect, useMemo, useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { ArrowLeftRight, Check, Laptop, Plus, RefreshCw } from 'lucide-react';
import type { BootstrapResponse, ProjectView } from '../generated/protocol';
import { AgentList } from '../components/AgentList';
import { ProjectDirPicker } from '../components/AddProjectModal';
import { BrandMark, BrandWordmark } from '../components/Icons';
import { PairDialog } from '../components/PairDialog';
import { api, errMsg, isDesktopShell, openDesktopSetup } from '../lib/api';
import { SCRATCH_PROJECT_ID, cleanMachineName, isVirtualProject } from '../lib/format';
import { navigate, paths } from '../lib/router';
import { toast } from '../store/useWorkbenchStore';

export function OnboardingPage({ boot }: { boot: BootstrapResponse }) {
  const qc = useQueryClient();
  const [step, setStep] = useState<1 | 2>(1);
  const [pairOpen, setPairOpen] = useState(false);
  const [addedProjects, setAddedProjects] = useState<ProjectView[]>([]);
  const [finishing, setFinishing] = useState(false);
  const [probingAll, setProbingAll] = useState(false);
  const autoProbedRef = useRef(false);
  const inDesktop = isDesktopShell();

  const execMachines = useMemo(
    () => boot.machines.filter((m) => m.transportMode !== 'ssh_outbound'),
    [boot.machines],
  );
  const onlineMachines = useMemo(
    () => execMachines.filter((m) => m.status === 'online'),
    [execMachines],
  );

  // Automatically discover local agents on mount if online machines haven't reported full probes yet.
  useEffect(() => {
    if (autoProbedRef.current || onlineMachines.length === 0) return;
    const needsProbe = onlineMachines.filter(
      (m) =>
        (m.runnerCapabilities ?? []).includes('agents.v1') &&
        ((m.agents ?? []).length === 0 || (m.agents ?? []).every((a) => !a.authChecked && a.state === 'ready' && !a.models?.length)),
    );
    if (needsProbe.length === 0) return;
    autoProbedRef.current = true;
    setProbingAll(true);
    void Promise.allSettled(needsProbe.map((m) => api.probeMachineAgents(m.machineId)))
      .then(() => qc.invalidateQueries({ queryKey: ['bootstrap'] }))
      .finally(() => setProbingAll(false));
  }, [onlineMachines, qc]);

  const rescanAllAgents = async () => {
    const eligible = onlineMachines.filter((m) => (m.runnerCapabilities ?? []).includes('agents.v1'));
    if (eligible.length === 0) {
      toast('当前没有在线且支持扫描的机器', 'err');
      return;
    }
    // Order local machine first so local feedback is prioritized while keeping remote machines visible.
    const targets = [
      ...eligible.filter((m) => m.transportMode === 'local_ws'),
      ...eligible.filter((m) => m.transportMode !== 'local_ws'),
    ];
    setProbingAll(true);
    try {
      const results = await Promise.allSettled(targets.map((m) => api.probeMachineAgents(m.machineId)));
      await qc.invalidateQueries({ queryKey: ['bootstrap'] });
      let okCount = 0;
      let failCount = 0;
      let firstErr = '';
      for (const r of results) {
        if (r.status === 'fulfilled') {
          okCount++;
        } else {
          failCount++;
          if (!firstErr) firstErr = errMsg(r.reason);
        }
      }
      if (okCount === 0) {
        toast(`Agent 扫描失败：${firstErr || '请检查机器连接状态'}`, 'err');
      } else if (failCount > 0) {
        toast(`已完成 ${okCount} 台机器扫描，${failCount} 台失败：${firstErr}`, 'err');
      } else if (targets.length === 1 && targets[0].transportMode === 'local_ws') {
        toast('已完成本机 Agent 扫描');
      } else {
        toast(`已完成 ${okCount} 台机器的 Agent 扫描`);
      }
    } catch (e) {
      toast(errMsg(e), 'err');
    } finally {
      setProbingAll(false);
    }
  };

  const firstReadyPick = useMemo(() => {
    for (const m of onlineMachines) {
      for (const adp of ['agy', 'claude', 'codex'] as const) {
        const probe = (m.agents ?? []).find((a) => a.adapter === adp);
        if (probe && probe.state === 'ready' && probe.enabled) {
          return { machineId: m.machineId, adapter: adp };
        }
      }
    }
    return null;
  }, [onlineMachines]);

  const existingRealProjects = useMemo(
    () => boot.projects.filter((p) => !isVirtualProject(p.projectId) && !p.archived),
    [boot.projects],
  );
  const allProjects = useMemo(() => {
    const byId = new Map<string, ProjectView>();
    for (const p of existingRealProjects) byId.set(p.projectId, p);
    for (const p of addedProjects) byId.set(p.projectId, p);
    return [...byId.values()];
  }, [existingRealProjects, addedProjects]);

  const pickForProject = (projId: string) => {
    const proj = allProjects.find((p) => p.projectId === projId);
    if (!proj) return firstReadyPick;
    const anchorMachineIds = new Set<string>();
    const defAnchor = proj.anchors[proj.defaultCapability]?.machine;
    if (defAnchor) anchorMachineIds.add(defAnchor);
    for (const a of Object.values(proj.anchors)) {
      if (a.machine) anchorMachineIds.add(a.machine);
    }
    const orderedIds = defAnchor
      ? [defAnchor, ...[...anchorMachineIds].filter((id) => id !== defAnchor)]
      : [...anchorMachineIds];
    for (const mid of orderedIds) {
      const m = execMachines.find((x) => x.machineId === mid);
      if (!m) continue;
      for (const adp of [proj.defaultAdapter, 'agy', 'claude', 'codex'] as const) {
        if (!adp) continue;
        const probe = (m.agents ?? []).find((a) => a.adapter === adp);
        if (probe && probe.state === 'ready' && probe.enabled) {
          return { machineId: mid, adapter: adp };
        }
      }
      return { machineId: mid, adapter: proj.defaultAdapter || 'agy' };
    }
    return undefined;
  };

  const finish = async (opts: { skipped?: boolean; mode?: 'chat' | 'project'; projectId?: string }) => {
    setFinishing(true);
    try {
      await api.completeOnboarding({ skipped: opts.skipped });
      const chosenPick = opts.mode === 'project' && opts.projectId ? pickForProject(opts.projectId) : firstReadyPick;
      if (chosenPick) {
        try {
          const key = opts.projectId || SCRATCH_PROJECT_ID;
          const prev = JSON.parse(localStorage.getItem('orbit.lastPick') || '{}') as Record<string, unknown>;
          prev[key] = chosenPick;
          localStorage.setItem('orbit.lastPick', JSON.stringify(prev));
        } catch {
          // ignore storage errors
        }
      }
      await qc.invalidateQueries({ queryKey: ['bootstrap'] });
      if (opts.skipped) {
        navigate(paths.home());
      } else if (opts.mode === 'project' && opts.projectId) {
        navigate(
          paths.newThread('project', opts.projectId, chosenPick ?? undefined),
        );
      } else if (opts.mode === 'chat') {
        navigate(paths.newThread('chat', undefined, firstReadyPick ?? undefined));
      } else {
        navigate(paths.home());
      }
    } catch (e) {
      toast(errMsg(e), 'err');
    } finally {
      setFinishing(false);
    }
  };

  return (
    <main className="main onb-shell">
      <div className="onb-card">
        <div className="onb-top">
          <div className="brand" style={{ margin: 0, padding: 0 }}>
            <BrandMark />
            <BrandWordmark />
          </div>
          <div className="onb-dots" aria-label={`步骤 ${step}`}>
            <span className={`onb-dot ${step === 1 ? 'on' : ''}`} />
            <span className={`onb-dot ${step === 2 ? 'on' : ''}`} />
          </div>
        </div>

        {step === 1 ? (
          <div className="onb-body">
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 8 }}>
              <h1 className="onb-title" style={{ margin: 0 }}>发现并接入本机 Agent</h1>
              {onlineMachines.length > 0 && (
                <button
                  type="button"
                  className="btn ghost sm"
                  disabled={probingAll}
                  onClick={() => void rescanAllAgents()}
                >
                  <RefreshCw size={12} strokeWidth={1.7} className={probingAll ? 'spin-ico' : ''} />
                  {probingAll ? '扫描中…' : '重新扫描'}
                </button>
              )}
            </div>
            <p className="muted" style={{ fontSize: 12.5, margin: '6px 0 14px', lineHeight: 1.55 }}>
              自动探测本机已安装的 Antigravity、Claude Code、Codex，一键补齐缺失的 ACP 适配器并校验登录状态。
            </p>
            {execMachines.length === 0 ? (
              <div className="onb-empty">
                <p className="muted">还没有已连接的执行机器。</p>
                <button type="button" className="btn pri" onClick={() => setPairOpen(true)}>
                  <Plus size={13} strokeWidth={1.7} /> 添加机器
                </button>
              </div>
            ) : (
              <div className="onb-machines">
                {execMachines.map((m) => (
                  <div className="onb-mach-box" key={m.machineId}>
                    <div className="onb-mach-hd">
                      <span className={`node-dot ${m.status === 'online' ? 'good' : 'off'}`} />
                      <b>{m.displayAlias || cleanMachineName(m)}</b>
                      {m.transportMode === 'local_ws' && <span className="node-local-pill">本机</span>}
                      <span className="muted" style={{ marginLeft: 'auto', fontSize: 11.5 }}>
                        {m.status === 'online' ? '在线' : '离线'}
                      </span>
                    </div>
                    <AgentList machine={m} />
                  </div>
                ))}
              </div>
            )}

            <div className="onb-extra" style={{ display: 'flex', gap: 14, flexWrap: 'wrap' }}>
              <button type="button" className="onb-link" onClick={() => setPairOpen(true)}>
                <Laptop size={13} strokeWidth={1.6} /> 接入其他机器到当前网络
              </button>
              {inDesktop && (
                <button type="button" className="onb-link" onClick={() => openDesktopSetup()}>
                  <ArrowLeftRight size={13} strokeWidth={1.6} /> 切换到已有 hebi8/orbit 网络
                </button>
              )}
            </div>

            <div className="onb-foot">
              <button
                type="button"
                className="btn ghost"
                disabled={finishing}
                onClick={() => void finish({ skipped: true })}
              >
                以后再说
              </button>
              <button
                type="button"
                className="btn pri"
                disabled={!firstReadyPick || finishing}
                title={!firstReadyPick ? '至少需要一个已就绪的 Agent' : undefined}
                onClick={() => setStep(2)}
              >
                继续
              </button>
            </div>
          </div>
        ) : (
          <div className="onb-body">
            <h1 className="onb-title">加一个项目</h1>
            <ProjectDirPicker
              machines={boot.machines}
              defaultMachineId={firstReadyPick?.machineId ?? boot.setup?.currentMachineId}
              onAdded={(p) => setAddedProjects((prev) => [...prev, p])}
            />

            {allProjects.length > 0 && (
              <div className="onb-added-list">
                {allProjects.map((p) => (
                  <div className="onb-added-item" key={p.projectId}>
                    <Check size={13} strokeWidth={2} className="ok-t" />
                    <span>{p.displayName}</span>
                    <span className="muted mono">{p.defaultBranch}</span>
                  </div>
                ))}
              </div>
            )}

            <div className="onb-foot">
              <button type="button" className="btn ghost" disabled={finishing} onClick={() => setStep(1)}>
                返回
              </button>
              <div style={{ display: 'flex', gap: 8 }}>
                <button
                  type="button"
                  className="btn ghost"
                  disabled={finishing}
                  onClick={() => void finish({ mode: 'chat' })}
                >
                  先自由对话
                </button>
                <button
                  type="button"
                  className="btn pri"
                  disabled={finishing}
                  onClick={() =>
                    void finish(
                      allProjects.length > 0
                        ? { mode: 'project', projectId: allProjects[allProjects.length - 1].projectId }
                        : { mode: 'chat' },
                    )
                  }
                >
                  开始使用
                </button>
              </div>
            </div>
          </div>
        )}
      </div>

      <PairDialog open={pairOpen} machines={boot.machines} onClose={() => setPairOpen(false)} />
    </main>
  );
}
