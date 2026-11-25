import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Check, ChevronDown, ChevronRight, Copy, Globe, Laptop, Plus, Trash2 } from 'lucide-react';
import type { BootstrapResponse, MachineView, TailscaleEnableResponse } from '../generated/protocol';
import { AddProjectModal } from '../components/AddProjectModal';
import { AgentList, agentDisplayName } from '../components/AgentList';
import { PageHeader } from '../components/PageHeader';
import { PairDialog } from '../components/PairDialog';
import { useNetworkSettings } from '../hooks/queries';
import { api, errMsg } from '../lib/api';
import { SCRATCH_PROJECT_ID, cleanMachineName, relTime } from '../lib/format';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';

export function SettingsPage({ boot }: { boot: BootstrapResponse }) {
  const qc = useQueryClient();
  const netQ = useNetworkSettings(true);
  const setWorkspaceNameOverride = useWorkbenchStore((s) => s.setWorkspaceNameOverride);

  const net = netQ.data;
  const workspaceName = net?.workspaceName || boot.workspaceName || 'Home LAN';

  const [editingName, setEditingName] = useState(false);
  const [nameDraft, setNameDraft] = useState(workspaceName);
  const [savingName, setSavingName] = useState(false);

  const [expandedMachine, setExpandedMachine] = useState<string>(() => {
    const first = boot.machines.find((m) => m.status === 'online' && m.transportMode !== 'ssh_outbound');
    return first?.machineId ?? '';
  });
  const [aliasEdits, setAliasEdits] = useState<Record<string, string>>({});

  const [pairOpen, setPairOpen] = useState(false);
  const [pairBindMachine, setPairBindMachine] = useState<string | undefined>(undefined);
  const [projModalOpen, setProjModalOpen] = useState(false);

  const [tsLoading, setTsLoading] = useState(false);
  const [tsConfirm, setTsConfirm] = useState<TailscaleEnableResponse | null>(null);

  const realProjects = boot.projects.filter((p) => p.projectId !== SCRATCH_PROJECT_ID && !p.archived);

  const refreshAll = () => {
    void qc.invalidateQueries({ queryKey: ['bootstrap'] });
    void qc.invalidateQueries({ queryKey: ['network-settings'] });
  };

  const saveWorkspaceName = async () => {
    const trimmed = nameDraft.trim();
    if (!trimmed) return;
    setSavingName(true);
    try {
      await api.updateNetworkSettings({ workspaceName: trimmed });
      setWorkspaceNameOverride('');
      setEditingName(false);
      refreshAll();
      toast(`已更新名称：${trimmed}`);
    } catch (e) {
      toast(errMsg(e), 'err');
    } finally {
      setSavingName(false);
    }
  };

  const triggerTailscaleShare = async (confirm = false, port?: number) => {
    setTsLoading(true);
    try {
      const res = await api.enableTailscaleShare({ confirm, port });
      if (res.needsConfirm) {
        setTsConfirm(res);
      } else {
        setTsConfirm(null);
        refreshAll();
        toast(res.origin ? `已开启多机共享：${res.origin}` : '已开启多机共享');
      }
    } catch (e) {
      toast(errMsg(e), 'err');
    } finally {
      setTsLoading(false);
    }
  };

  const saveMachineAlias = async (m: MachineView) => {
    const alias = (aliasEdits[m.machineId] ?? m.displayAlias ?? '').trim();
    try {
      await api.updateMachine(m.machineId, { displayAlias: alias });
      refreshAll();
      toast('已更新机器名称');
    } catch (e) {
      toast(errMsg(e), 'err');
    }
  };

  const removeMachine = async (m: MachineView) => {
    try {
      await api.deleteMachine(m.machineId);
      refreshAll();
      toast(`已移除 ${m.displayAlias || cleanMachineName(m)}`);
    } catch (e) {
      toast(errMsg(e), 'err');
    }
  };

  const removeProject = async (projectId: string, displayName: string) => {
    try {
      await api.deleteProject(projectId);
      refreshAll();
      toast(`已移除项目 ${displayName}`);
    } catch (e) {
      toast(errMsg(e), 'err');
    }
  };

  const copyOrigin = async (u: string) => {
    try {
      await navigator.clipboard.writeText(u);
      toast('已复制地址');
    } catch {
      toast(u);
    }
  };

  return (
    <main className="main">
      <PageHeader>
        <span className="ttl">网络与机器</span>
      </PageHeader>
      <div className="page-scroll">
        <div className="set-wrap">
          {/* Section 1: Network */}
          <section className="set-sec">
            <div className="set-hd">
              <h2>网络</h2>
            </div>

            <div className="set-row">
              <div className="set-k">空间名称</div>
              <div className="set-v">
                {editingName ? (
                  <div className="set-inline">
                    <input
                      className="field"
                      value={nameDraft}
                      onChange={(e) => setNameDraft(e.target.value)}
                      onKeyDown={(e) => {
                        if (e.key === 'Enter') void saveWorkspaceName();
                        if (e.key === 'Escape') setEditingName(false);
                      }}
                      autoFocus
                    />
                    <button type="button" className="btn pri sm" disabled={savingName} onClick={() => void saveWorkspaceName()}>
                      保存
                    </button>
                    <button type="button" className="btn ghost sm" onClick={() => setEditingName(false)}>
                      取消
                    </button>
                  </div>
                ) : (
                  <div className="set-inline">
                    <b>{workspaceName}</b>
                    <button
                      type="button"
                      className="btn ghost sm"
                      onClick={() => {
                        setNameDraft(workspaceName);
                        setEditingName(true);
                      }}
                    >
                      改名
                    </button>
                  </div>
                )}
              </div>
            </div>

            <div className="set-row">
              <div className="set-k">多机接入</div>
              <div className="set-v">
                {net?.tailscaleShared ? (
                  <div className="set-inline">
                    <span className="ok-t">已开启</span>
                    <span className="mono muted ell">{net.tailscaleOrigin || net.hubOrigin}</span>
                    <button
                      type="button"
                      className="btn ghost sm"
                      onClick={() => void copyOrigin(net.tailscaleOrigin || net.hubOrigin)}
                    >
                      <Copy size={11.5} strokeWidth={1.7} /> 复制
                    </button>
                  </div>
                ) : net?.tailscaleAvailable ? (
                  <div className="set-col">
                    <div className="set-inline">
                      <span className="muted">仅本机使用</span>
                      <button
                        type="button"
                        className="btn ghost sm"
                        disabled={tsLoading}
                        onClick={() => void triggerTailscaleShare(false)}
                      >
                        <Globe size={12} strokeWidth={1.7} />
                        {tsLoading ? '检查中…' : '让其他机器连进来'}
                      </button>
                    </div>
                    {tsConfirm && (
                      <div className="set-confirm">
                        <div className="muted">
                          {tsConfirm.conflictDetail ||
                            `将通过 Tailscale 开启端口 ${tsConfirm.suggestedPort} 供你的其他设备接入。`}
                        </div>
                        <div className="mono muted" style={{ fontSize: 11 }}>
                          {tsConfirm.command}
                        </div>
                        <div className="set-inline" style={{ marginTop: 6 }}>
                          <button
                            type="button"
                            className="btn pri sm"
                            disabled={tsLoading}
                            onClick={() => void triggerTailscaleShare(true, tsConfirm.suggestedPort)}
                          >
                            确认开启
                          </button>
                          <button type="button" className="btn ghost sm" onClick={() => setTsConfirm(null)}>
                            取消
                          </button>
                        </div>
                      </div>
                    )}
                  </div>
                ) : (
                  <span className="muted">
                    {net?.tailscaleHint || '安装并登录 Tailscale 后即可让其他机器接入'}
                  </span>
                )}
              </div>
            </div>
          </section>

          {/* Section 2: Machines & Agents */}
          <section className="set-sec">
            <div className="set-hd">
              <h2>机器与 Agent</h2>
              <button
                type="button"
                className="btn pri sm"
                onClick={() => {
                  setPairBindMachine(undefined);
                  setPairOpen(true);
                }}
              >
                <Plus size={12.5} strokeWidth={1.8} /> 添加机器
              </button>
            </div>

            <div className="set-mach-list">
              {boot.machines.map((m) => {
                const isExec = m.transportMode !== 'ssh_outbound';
                const expanded = expandedMachine === m.machineId;
                const online = m.status === 'online';
                const readyAgents = (m.agents ?? []).filter((a) => a.state === 'ready' && a.enabled);
                const needsPair = isExec && !m.hasCredentials;
                const label = m.displayAlias || cleanMachineName(m);

                return (
                  <div className={`set-mach-card ${expanded ? 'open' : ''}`} key={m.machineId}>
                    <div
                      className="set-mach-hd"
                      onClick={() => isExec && setExpandedMachine(expanded ? '' : m.machineId)}
                    >
                      <span className={`node-dot ${online ? 'good' : 'off'}`} />
                      <span className="set-mach-name ell">{label}</span>
                      {m.transportMode === 'local_ws' && <span className="node-local-pill">本机</span>}
                      {!isExec && <span className="node-local-pill">部署目标</span>}

                      <div className="set-mach-summary">
                        {isExec && readyAgents.length > 0 && !expanded && (
                          <span className="muted set-ag-chips">
                            {readyAgents.map((a) => (
                              <span key={a.adapter} className="set-ag-chip">
                                {agentDisplayName(a.adapter)}
                                <Check size={11} strokeWidth={2.2} className="ok-t" />
                              </span>
                            ))}
                          </span>
                        )}

                        {needsPair && (
                          <button
                            type="button"
                            className="btn ghost sm"
                            onClick={(e) => {
                              e.stopPropagation();
                              setPairBindMachine(m.machineId);
                              setPairOpen(true);
                            }}
                          >
                            <Laptop size={12} strokeWidth={1.7} /> 连接这台机器
                          </button>
                        )}

                        <span className="muted mono" style={{ fontSize: 11.5 }}>
                          {online ? '在线' : m.lastSeenAt ? `离线 · ${relTime(m.lastSeenAt)}` : '离线'}
                        </span>

                        {isExec && (
                          <span className="muted">
                            {expanded ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
                          </span>
                        )}
                      </div>
                    </div>

                    {isExec && expanded && (
                      <div className="set-mach-body">
                        <div className="set-mach-alias">
                          <span className="muted">显示名称</span>
                          <input
                            className="field"
                            style={{ maxWidth: 220 }}
                            value={aliasEdits[m.machineId] ?? m.displayAlias ?? ''}
                            placeholder={cleanMachineName(m)}
                            onChange={(e) =>
                              setAliasEdits((prev) => ({ ...prev, [m.machineId]: e.target.value }))
                            }
                            onKeyDown={(e) => {
                              if (e.key === 'Enter') void saveMachineAlias(m);
                            }}
                          />
                          <button type="button" className="btn ghost sm" onClick={() => void saveMachineAlias(m)}>
                            保存
                          </button>
                          {m.source === 'paired' && (
                            <button
                              type="button"
                              className="btn ghost sm bad-t"
                              style={{ marginLeft: 'auto' }}
                              onClick={() => void removeMachine(m)}
                            >
                              <Trash2 size={12} strokeWidth={1.7} /> 移除
                            </button>
                          )}
                        </div>

                        <AgentList machine={m} showAdvanced />
                      </div>
                    )}
                  </div>
                );
              })}
            </div>
          </section>

          {/* Section 3: Projects */}
          <section className="set-sec">
            <div className="set-hd">
              <h2>项目</h2>
              <button type="button" className="btn ghost sm" onClick={() => setProjModalOpen(true)}>
                <Plus size={12.5} strokeWidth={1.8} /> 添加项目
              </button>
            </div>
            {realProjects.length === 0 ? (
              <div className="muted" style={{ padding: '6px 0' }}>
                还没有添加本地项目。
              </div>
            ) : (
              <div className="set-proj-list">
                {realProjects.map((p) => {
                  const anchor = p.anchors[p.defaultCapability] ?? Object.values(p.anchors)[0];
                  return (
                    <div className="set-proj-row" key={p.projectId}>
                      <div className="ell grow">
                        <b>{p.displayName}</b>
                        <span className="muted mono" style={{ marginLeft: 10 }}>
                          {anchor?.machine || ''}
                          {anchor?.repoPath ? ` · ${anchor.repoPath}` : ''}
                        </span>
                      </div>
                      {p.source === 'ui' && (
                        <button
                          type="button"
                          className="ib"
                          title="移除项目"
                          aria-label={`移除 ${p.displayName}`}
                          onClick={() => void removeProject(p.projectId, p.displayName)}
                        >
                          <Trash2 size={13} strokeWidth={1.6} />
                        </button>
                      )}
                    </div>
                  );
                })}
              </div>
            )}
          </section>
        </div>
      </div>

      <PairDialog
        open={pairOpen}
        bindMachineId={pairBindMachine}
        machines={boot.machines}
        onClose={() => {
          setPairOpen(false);
          setPairBindMachine(undefined);
        }}
      />
      <AddProjectModal
        open={projModalOpen}
        machines={boot.machines}
        defaultMachineId={boot.setup?.currentMachineId}
        onClose={() => setProjModalOpen(false)}
      />
    </main>
  );
}
