import { useMemo, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Check, Laptop, Plus } from 'lucide-react';
import type { BootstrapResponse, ProjectView } from '../generated/protocol';
import { AgentList } from '../components/AgentList';
import { ProjectDirPicker } from '../components/AddProjectModal';
import { PairDialog } from '../components/PairDialog';
import { api, errMsg } from '../lib/api';
import { SCRATCH_PROJECT_ID, cleanMachineName } from '../lib/format';
import { navigate, paths } from '../lib/router';
import { toast } from '../store/useWorkbenchStore';

export function OnboardingPage({ boot }: { boot: BootstrapResponse }) {
  const qc = useQueryClient();
  const [step, setStep] = useState<1 | 2>(1);
  const [pairOpen, setPairOpen] = useState(false);
  const [addedProjects, setAddedProjects] = useState<ProjectView[]>([]);
  const [finishing, setFinishing] = useState(false);

  const execMachines = useMemo(
    () => boot.machines.filter((m) => m.transportMode !== 'ssh_outbound'),
    [boot.machines],
  );
  const onlineMachines = useMemo(
    () => execMachines.filter((m) => m.status === 'online'),
    [execMachines],
  );

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
    () => boot.projects.filter((p) => p.projectId !== SCRATCH_PROJECT_ID && !p.archived),
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
            <span className="brand-mark" />
            <b>Orbit</b>
          </div>
          <div className="onb-dots" aria-label={`步骤 ${step}`}>
            <span className={`onb-dot ${step === 1 ? 'on' : ''}`} />
            <span className={`onb-dot ${step === 2 ? 'on' : ''}`} />
          </div>
        </div>

        {step === 1 ? (
          <div className="onb-body">
            <h1 className="onb-title">这台机器上的 Agent</h1>
            {execMachines.length === 0 ? (
              <div className="onb-empty">
                <p className="muted">还没有已连接的机器。</p>
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

            <div className="onb-extra">
              <button type="button" className="onb-link" onClick={() => setPairOpen(true)}>
                <Laptop size={13} strokeWidth={1.6} /> 还有其他机器？添加更多机器
              </button>
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
