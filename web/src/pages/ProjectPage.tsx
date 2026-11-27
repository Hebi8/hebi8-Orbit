import { useEffect, useMemo, useState } from 'react';
import { useQueries, useQueryClient } from '@tanstack/react-query';
import {
  ArchiveRestore,
  ArrowUpRight,
  Eye,
  EyeOff,
  Folder,
  GitBranch,
  Lock,
  Pencil,
  Plus,
  RefreshCw,
  Trash2,
  X,
} from 'lucide-react';
import type {
  BootstrapResponse,
  DeliveryView,
  ProjectEnvVar,
  ProjectLocationView,
  ProjectPartKind,
  ProjectPartView,
  ProjectView,
  TaskSummary,
} from '../generated/protocol';
import { DeviceRoleIcon } from '../components/DeviceRoleIcon';
import { DirBrowseModal } from '../components/DirBrowseModal';
import { GitZshBadge } from '../components/GitZshBadge';
import { ArtIcon, DiffIcon } from '../components/Icons';
import { IdeOpenButton } from '../components/IdeOpenButton';
import { PageHeader } from '../components/PageHeader';
import { StatusMark } from '../components/StatusMark';
import { useProjectSuggestions, useProjectTasks } from '../hooks/queries';
import { api, errMsg } from '../lib/api';
import {
  cleanMachineName,
  displayMarkRank,
  indexInboxByTask,
  relTime,
  rootSessionMark,
  shortSha,
  taskTitle,
  type SessionMark,
} from '../lib/format';
import { buildIDEJump, launchIDE, resolveEffectiveLocalMachineId } from '../lib/ide';
import { useImeGuard } from '../lib/ime';
import { navigate, paths, type ProjectTab } from '../lib/router';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';

function ThreadRow({ t, mark }: { t: TaskSummary; mark: SessionMark }) {
  return (
    <button type="button" className="srow" onClick={() => navigate(paths.session(t.taskId))}>
      <span className="t">{taskTitle(t)}</span>
      {mark ? <StatusMark mark={mark} /> : <span className="st">{relTime(t.updatedAt)}</span>}
    </button>
  );
}

const CODE_KINDS = new Set(['code_diff', 'mixed', 'workspace_snapshot']);

function ProjectSettingsTab({
  boot,
  project,
}: {
  boot: BootstrapResponse;
  project: ProjectView;
}) {
  const qc = useQueryClient();
  const storedLocalId = useWorkbenchStore((s) => s.localMachineId);
  const effectiveLocalId = resolveEffectiveLocalMachineId(
    boot.machines,
    boot.setup?.currentMachineId || storedLocalId,
  );
  const activeDutyId = boot.setup?.activeDutyMachineId ?? '';

  const parts: ProjectPartView[] = project.parts ?? [];

  const suggestionsQ = useProjectSuggestions(project.projectId, true);
  const suggestions = suggestionsQ.data?.suggestions ?? [];

  const [refreshingLocs, setRefreshingLocs] = useState(false);
  const [browseTarget, setBrowseTarget] = useState<
    | null
    | { kind: 'add_location'; partId: string; partName: string }
    | { kind: 'edit_location'; partId: string; partName: string; loc: ProjectLocationView }
    | { kind: 'add_part' }
  >(null);
  const [editingPart, setEditingPart] = useState<ProjectPartView | null>(null);
  const [partNameDraft, setPartNameDraft] = useState('');
  const [partKindDraft, setPartKindDraft] = useState<ProjectPartKind>('folder');
  const [savingPart, setSavingPart] = useState(false);
  const ime = useImeGuard();

  // Shared ENV state
  const [envRows, setEnvRows] = useState<ProjectEnvVar[]>(() => project.sharedEnv ?? []);
  const [revealed, setRevealed] = useState<Record<number, boolean>>({});
  const [savingEnv, setSavingEnv] = useState(false);

  // System Prompt state
  const [sysPrompt, setSysPrompt] = useState(project.systemPrompt ?? '');
  const [savingPrompt, setSavingPrompt] = useState(false);

  useEffect(() => {
    setEnvRows(project.sharedEnv ?? []);
    setSysPrompt(project.systemPrompt ?? '');
  }, [project.projectId, project.updatedAt]);

  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ['bootstrap'] });
    void qc.invalidateQueries({ queryKey: ['project-suggestions', project.projectId] });
  };

  const machineById = (mid: string) => boot.machines.find((m) => m.machineId === mid);

  const handleRefreshLocations = async () => {
    setRefreshingLocs(true);
    try {
      await api.refreshProjectLocations(project.projectId);
      invalidate();
      toast('已刷新各设备目录状态');
    } catch (e) {
      toast(errMsg(e), 'err');
    } finally {
      setRefreshingLocs(false);
    }
  };

  const handleAddSuggestion = async (partId: string, machineId: string, absPath: string) => {
    try {
      await api.createProjectLocation(project.projectId, partId, { machineId, absPath });
      invalidate();
      toast('已添加设备位置');
    } catch (e) {
      toast(errMsg(e), 'err');
    }
  };

  const handleDismissSuggestion = async (partId: string, machineId: string, absPath: string) => {
    try {
      await api.dismissProjectSuggestion(project.projectId, partId, { machineId, absPath });
      void qc.invalidateQueries({ queryKey: ['project-suggestions', project.projectId] });
    } catch (e) {
      toast(errMsg(e), 'err');
    }
  };

  const handleSetDefaultLoc = async (partId: string, loc: ProjectLocationView) => {
    try {
      await api.updateProjectLocation(project.projectId, partId, loc.locationId, { isDefault: true });
      invalidate();
      toast('已设为默认位置');
    } catch (e) {
      toast(errMsg(e), 'err');
    }
  };

  const handleDeleteLocation = async (partId: string, loc: ProjectLocationView) => {
    try {
      await api.deleteProjectLocation(project.projectId, partId, loc.locationId);
      invalidate();
      toast('已移除设备路径');
    } catch (e) {
      toast(errMsg(e), 'err');
    }
  };

  const openEditPart = (part: ProjectPartView) => {
    setEditingPart(part);
    setPartNameDraft(part.name);
    setPartKindDraft(part.kind);
  };

  const handleSavePart = async () => {
    if (!editingPart) return;
    const trimmed = partNameDraft.trim();
    if (!trimmed) {
      toast('组成部分名称不能为空', 'err');
      return;
    }
    setSavingPart(true);
    try {
      await api.updateProjectPart(project.projectId, editingPart.partId, {
        name: trimmed,
        kind: partKindDraft,
      });
      invalidate();
      setEditingPart(null);
      toast('已更新组成部分');
    } catch (e) {
      toast(errMsg(e), 'err');
    } finally {
      setSavingPart(false);
    }
  };

  const handleDeletePart = async (part: ProjectPartView) => {
    try {
      await api.deleteProjectPart(project.projectId, part.partId);
      invalidate();
      toast(`已删除组成部分「${part.name}」`);
    } catch (e) {
      toast(errMsg(e), 'err');
    }
  };

  const saveEnv = async () => {
    setSavingEnv(true);
    try {
      const cleaned = envRows.filter((r) => r.key.trim());
      await api.updateProjectSettings(project.projectId, { sharedEnv: cleaned });
      invalidate();
      toast('已保存公用环境变量');
    } catch (e) {
      toast(errMsg(e), 'err');
    } finally {
      setSavingEnv(false);
    }
  };

  const savePrompt = async () => {
    setSavingPrompt(true);
    try {
      await api.updateProjectSettings(project.projectId, { systemPrompt: sysPrompt });
      invalidate();
      toast('已保存默认提示词');
    } catch (e) {
      toast(errMsg(e), 'err');
    } finally {
      setSavingPrompt(false);
    }
  };

  const removeProject = async () => {
    try {
      await api.deleteProject(project.projectId);
      invalidate();
      toast(`已移除项目 ${project.displayName}`);
      navigate(paths.home());
    } catch (e) {
      toast(errMsg(e), 'err');
    }
  };

  return (
    <div className="pset-wrap">
      {/* Section 1: 目录组成 */}
      <section className="pset-sec">
        <div className="pset-hd">
          <h3>目录组成</h3>
          <div className="pset-hd-acts">
            <button
              type="button"
              className="ib"
              title="刷新目录与分支状态"
              aria-label="刷新目录状态"
              disabled={refreshingLocs}
              onClick={() => void handleRefreshLocations()}
            >
              <RefreshCw size={13} strokeWidth={1.7} className={refreshingLocs ? 'spin-ico' : ''} />
            </button>
            <button
              type="button"
              className="btn ghost sm"
              onClick={() => setBrowseTarget({ kind: 'add_part' })}
            >
              <Plus size={12} strokeWidth={1.8} /> 添加组成部分
            </button>
          </div>
        </div>

        {parts.length === 0 ? (
          <div className="pset-part">
            <div className="pset-loc-empty">
              <span className="muted">还没有组成部分，点击「添加组成部分」绑定 Git 仓库或文件夹</span>
              <button
                type="button"
                className="btn ghost sm"
                onClick={() => setBrowseTarget({ kind: 'add_part' })}
              >
                <Plus size={12} strokeWidth={1.8} /> 添加组成部分
              </button>
            </div>
          </div>
        ) : (
          <div className="pset-parts">
            {parts.map((part) => {
              const partSugs = suggestions.filter((s) => s.partId === part.partId);
              const isGitPart = part.kind === 'git';
              return (
                <div className="pset-part" key={part.partId}>
                  <div className="pset-part-hd">
                    {isGitPart ? (
                      <GitBranch size={13.5} strokeWidth={1.7} className="muted" />
                    ) : (
                      <Folder size={13.5} strokeWidth={1.6} className="muted" />
                    )}
                    <span className="pset-part-name">{part.name}</span>
                    {isGitPart && part.repoIdentity && !part.repoIdentity.startsWith('folder:') && (
                      <span className="pset-part-remote mono ell">{part.repoIdentity}</span>
                    )}
                    <div className="pset-part-acts">
                      <button
                        type="button"
                        className="btn ghost sm"
                        onClick={() =>
                          setBrowseTarget({ kind: 'add_location', partId: part.partId, partName: part.name })
                        }
                      >
                        <Plus size={11.5} strokeWidth={1.8} /> 添加位置
                      </button>
                      <button
                        type="button"
                        className="ib"
                        title={`编辑组成部分「${part.name}」`}
                        aria-label={`编辑组成部分 ${part.name}`}
                        onClick={() => openEditPart(part)}
                      >
                        <Pencil size={12.5} strokeWidth={1.6} />
                      </button>
                      <button
                        type="button"
                        className="ib pset-del-part"
                        title={`删除组成部分「${part.name}」`}
                        aria-label={`删除组成部分 ${part.name}`}
                        onClick={() => void handleDeletePart(part)}
                      >
                        <Trash2 size={12.5} strokeWidth={1.6} />
                      </button>
                    </div>
                  </div>

                  <div className="pset-loc-list">
                    {part.locations.length === 0 ? (
                      <div className="pset-loc-empty">
                        <span className="muted">该组成部分暂无绑定设备路径</span>
                        <button
                          type="button"
                          className="btn ghost sm"
                          onClick={() =>
                            setBrowseTarget({ kind: 'add_location', partId: part.partId, partName: part.name })
                          }
                        >
                          <Plus size={11.5} strokeWidth={1.8} /> 添加设备路径
                        </button>
                      </div>
                    ) : (
                      part.locations.map((loc) => {
                        const m = machineById(loc.machineId);
                        const mLabel = m ? m.displayAlias || cleanMachineName(m) : loc.machineId;
                        return (
                          <div className="pset-loc-row" key={loc.locationId}>
                            {m ? (
                              <DeviceRoleIcon
                                machine={m}
                                isActiveDuty={m.machineId === activeDutyId}
                                isCurrentDevice={m.machineId === effectiveLocalId}
                                size={13.5}
                              />
                            ) : (
                              <Folder size={13} className="muted" />
                            )}
                            <span className="pset-loc-mach">{mLabel}</span>
                            <span className="pset-loc-path mono ell" title={loc.absPath}>
                              {loc.absPath}
                            </span>
                            <GitZshBadge
                              runtime={loc.runtime}
                              fallbackBranch={project.defaultBranch}
                              isGit={isGitPart}
                            />
                            <div className="pset-loc-acts">
                              {loc.isDefault ? (
                                <span className="pset-def-mark">默认</span>
                              ) : (
                                <button
                                  type="button"
                                  className="pset-hover-act"
                                  onClick={() => void handleSetDefaultLoc(part.partId, loc)}
                                >
                                  设为默认
                                </button>
                              )}
                              <button
                                type="button"
                                className="ib pset-loc-ico-act"
                                title="编辑设备路径"
                                aria-label="编辑设备路径"
                                onClick={() =>
                                  setBrowseTarget({
                                    kind: 'edit_location',
                                    partId: part.partId,
                                    partName: part.name,
                                    loc,
                                  })
                                }
                              >
                                <Pencil size={12} strokeWidth={1.6} />
                              </button>
                              <button
                                type="button"
                                className="ib pset-del-loc"
                                title="删除该设备路径"
                                aria-label="删除该设备路径"
                                onClick={() => void handleDeleteLocation(part.partId, loc)}
                              >
                                <Trash2 size={12} strokeWidth={1.6} />
                              </button>
                            </div>
                          </div>
                        );
                      })
                    )}

                    {/* Quiet optional Git suggestions (never auto-bound) */}
                    {partSugs.map((sug) => {
                      const sm = machineById(sug.machineId);
                      const smLabel = sm ? sm.displayAlias || cleanMachineName(sm) : sug.machineId;
                      return (
                        <div className="pset-sug-row" key={`${sug.machineId}:${sug.absPath}`}>
                          <span className="pset-sug-label">发现于 {smLabel}</span>
                          <span className="pset-loc-path mono ell" title={sug.absPath}>
                            {sug.absPath}
                          </span>
                          {sug.branch && <GitZshBadge fallbackBranch={sug.branch} isGit />}
                          <div className="pset-sug-acts">
                            <button
                              type="button"
                              className="btn ghost sm"
                              onClick={() => void handleAddSuggestion(part.partId, sug.machineId, sug.absPath)}
                            >
                              添加
                            </button>
                            <button
                              type="button"
                              className="btn ghost sm muted"
                              onClick={() => void handleDismissSuggestion(part.partId, sug.machineId, sug.absPath)}
                            >
                              忽略
                            </button>
                          </div>
                        </div>
                      );
                    })}
                  </div>
                </div>
              );
            })}
          </div>
        )}
      </section>

      {/* Section 2: 公用环境变量 */}
      <section className="pset-sec">
        <div className="pset-hd">
          <h3>公用环境变量</h3>
          <div className="pset-hd-acts">
            <button
              type="button"
              className="btn ghost sm"
              onClick={() => setEnvRows((rows) => [...rows, { key: '', value: '', secret: false }])}
            >
              <Plus size={12} strokeWidth={1.8} /> 添加变量
            </button>
            <button type="button" className="btn pri sm" disabled={savingEnv} onClick={() => void saveEnv()}>
              {savingEnv ? '保存中…' : '保存'}
            </button>
          </div>
        </div>
        {envRows.length === 0 ? (
          <div className="quiet">暂无项目公用环境变量，添加后将在各设备执行任务时注入。</div>
        ) : (
          <div className="pset-env-list">
            {envRows.map((row, idx) => {
              const isHidden = !!row.secret && !revealed[idx];
              return (
                <div className="pset-env-row" key={idx}>
                  <input
                    className="field mono pset-env-k"
                    placeholder="KEY"
                    value={row.key}
                    onChange={(e) =>
                      setEnvRows((rs) => rs.map((r, i) => (i === idx ? { ...r, key: e.target.value } : r)))
                    }
                  />
                  <div className="pset-env-vwrap">
                    <input
                      type={isHidden ? 'password' : 'text'}
                      className="field mono grow"
                      placeholder="VALUE"
                      value={row.value}
                      onChange={(e) =>
                        setEnvRows((rs) => rs.map((r, i) => (i === idx ? { ...r, value: e.target.value } : r)))
                      }
                    />
                    {row.secret && (
                      <button
                        type="button"
                        className="ib"
                        title={isHidden ? '显示' : '隐藏'}
                        onClick={() => setRevealed((rv) => ({ ...rv, [idx]: !rv[idx] }))}
                      >
                        {isHidden ? <Eye size={13} /> : <EyeOff size={13} />}
                      </button>
                    )}
                  </div>
                  <button
                    type="button"
                    className={`ib ${row.secret ? 'on' : ''}`}
                    title={row.secret ? '已标记为密钥' : '标记为密钥（默认掩码）'}
                    onClick={() =>
                      setEnvRows((rs) => rs.map((r, i) => (i === idx ? { ...r, secret: !r.secret } : r)))
                    }
                  >
                    <Lock size={12.5} strokeWidth={1.7} />
                  </button>
                  <button
                    type="button"
                    className="ib"
                    title="删除变量"
                    onClick={() => setEnvRows((rs) => rs.filter((_, i) => i !== idx))}
                  >
                    <Trash2 size={12.5} strokeWidth={1.6} />
                  </button>
                </div>
              );
            })}
          </div>
        )}
      </section>

      {/* Section 3: 默认提示词 */}
      <section className="pset-sec">
        <div className="pset-hd">
          <h3>默认提示词</h3>
          <button type="button" className="btn pri sm" disabled={savingPrompt} onClick={() => void savePrompt()}>
            {savingPrompt ? '保存中…' : '保存'}
          </button>
        </div>
        <textarea
          className="field pset-prompt-ta"
          rows={5}
          placeholder="填写该项目每次启动新会话时自动附带的背景说明或编码约束…"
          value={sysPrompt}
          onChange={(e) => setSysPrompt(e.target.value)}
        />
      </section>

      {project.source === 'ui' && (
        <div className="pset-danger">
          <button type="button" className="btn ghost sm bad-t" onClick={() => void removeProject()}>
            <Trash2 size={12.5} strokeWidth={1.7} /> 移除项目
          </button>
        </div>
      )}

      <DirBrowseModal
        open={browseTarget !== null}
        title={
          browseTarget?.kind === 'add_location'
            ? `为「${browseTarget.partName}」添加设备目录`
            : browseTarget?.kind === 'edit_location'
              ? `编辑「${browseTarget.partName}」设备目录`
              : '添加组成部分（Git 仓库或文件夹）'
        }
        confirmLabel={
          browseTarget?.kind === 'add_location'
            ? '添加此位置'
            : browseTarget?.kind === 'edit_location'
              ? '保存修改'
              : '添加组成部分'
        }
        machines={boot.machines}
        defaultMachineId={
          browseTarget?.kind === 'edit_location'
            ? browseTarget.loc.machineId
            : effectiveLocalId || boot.setup?.currentMachineId
        }
        initialPath={browseTarget?.kind === 'edit_location' ? browseTarget.loc.absPath : undefined}
        activeDutyMachineId={activeDutyId}
        currentMachineId={effectiveLocalId}
        extraFieldLabel={browseTarget?.kind === 'add_part' ? '组成部分名称' : undefined}
        extraFieldPlaceholder={browseTarget?.kind === 'add_part' ? '留空则默认使用目录名' : undefined}
        showPartKindToggle={browseTarget?.kind === 'add_part'}
        onConfirm={async ({ machineId, absPath, extraValue, partKind }) => {
          if (browseTarget?.kind === 'add_location') {
            await api.createProjectLocation(project.projectId, browseTarget.partId, {
              machineId,
              absPath,
            });
            toast('已添加设备路径');
          } else if (browseTarget?.kind === 'edit_location') {
            await api.updateProjectLocation(project.projectId, browseTarget.partId, browseTarget.loc.locationId, {
              machineId,
              absPath,
            });
            toast('已更新设备路径');
          } else {
            await api.createProjectPart(project.projectId, {
              name: extraValue || absPath.split('/').pop() || '目录',
              kind: partKind,
              machineId,
              absPath,
            });
            toast('已添加组成部分');
          }
          invalidate();
        }}
        onClose={() => setBrowseTarget(null)}
      />

      {editingPart && (
        <div className="modal-scrim" onClick={() => !savingPart && setEditingPart(null)}>
          <section
            className="modal-card pset-edit-part-modal"
            role="dialog"
            aria-label="编辑组成部分"
            onClick={(e) => e.stopPropagation()}
          >
            <div className="modal-hd">
              <h3>编辑组成部分</h3>
              <button
                type="button"
                className="ib"
                disabled={savingPart}
                onClick={() => setEditingPart(null)}
                aria-label="关闭"
              >
                <X size={15} strokeWidth={1.7} />
              </button>
            </div>
            <div className="modal-body">
              <div className="cdx-meta-grid has-kind">
                <div className="cdx-meta-field">
                  <label className="cdx-meta-lbl" htmlFor="pset-edit-part-name">
                    组成部分名称
                  </label>
                  <input
                    id="pset-edit-part-name"
                    className="field"
                    value={partNameDraft}
                    autoFocus
                    onChange={(e) => setPartNameDraft(e.target.value)}
                    {...ime.compositionProps}
                    onKeyDown={(e) => {
                      if (ime.isImeEnter(e)) return;
                      if (e.key === 'Enter') {
                        e.preventDefault();
                        void handleSavePart();
                      } else if (e.key === 'Escape') {
                        e.preventDefault();
                        setEditingPart(null);
                      }
                    }}
                  />
                </div>
                <div className="cdx-meta-field">
                  <span className="cdx-meta-lbl">目录类型</span>
                  <div className="role-seg" role="radiogroup" aria-label="目录类型">
                    <button
                      type="button"
                      role="radio"
                      aria-checked={partKindDraft === 'git'}
                      className={partKindDraft === 'git' ? 'on' : ''}
                      onClick={() => setPartKindDraft('git')}
                    >
                      Git 仓库
                    </button>
                    <button
                      type="button"
                      role="radio"
                      aria-checked={partKindDraft === 'folder'}
                      className={partKindDraft === 'folder' ? 'on' : ''}
                      onClick={() => setPartKindDraft('folder')}
                    >
                      普通目录
                    </button>
                  </div>
                </div>
              </div>
              <div className="cdx-actions">
                <button
                  type="button"
                  className="btn ghost sm"
                  disabled={savingPart}
                  onClick={() => setEditingPart(null)}
                >
                  取消
                </button>
                <button
                  type="button"
                  className="btn pri sm"
                  disabled={savingPart || !partNameDraft.trim()}
                  onClick={() => void handleSavePart()}
                >
                  {savingPart ? '保存中…' : '保存修改'}
                </button>
              </div>
            </div>
          </section>
        </div>
      )}
    </div>
  );
}

export function ProjectPage({ boot, projectId, tab }: { boot: BootstrapResponse; projectId: string; tab: ProjectTab }) {
  const qc = useQueryClient();
  const project = boot.projects.find((p) => p.projectId === projectId);
  const [threadScope, setThreadScope] = useState<'active' | 'archived'>('active');
  const [confirmDeleteId, setConfirmDeleteId] = useState<string | null>(null);
  const tasksQ = useProjectTasks(projectId, true, 'active');
  const archivedTasksQ = useProjectTasks(projectId, tab === 'threads' && threadScope === 'archived', 'only');
  const [q, setQ] = useState('');
  const openReview = useWorkbenchStore((s) => s.openReview);
  const localMachineId = useWorkbenchStore((s) => s.localMachineId);

  const allTasks = useMemo(() => tasksQ.data ?? [], [tasksQ.data]);
  const threads = useMemo(() => allTasks.filter((t) => !t.parentTaskId), [allTasks]);
  const archivedThreads = useMemo(
    () => (archivedTasksQ.data ?? []).filter((t) => !t.parentTaskId),
    [archivedTasksQ.data],
  );
  const inboxByTask = useMemo(() => indexInboxByTask(boot.openInboxItems), [boot.openInboxItems]);

  const ranked = useMemo(() => {
    const k = q.trim().toLowerCase();
    const list = k ? threads.filter((t) => `${t.title} ${t.goal}`.toLowerCase().includes(k)) : threads;
    return list
      .map((t) => ({ t, mark: rootSessionMark(t, allTasks, inboxByTask) }))
      .sort((a, b) => displayMarkRank(a.mark) - displayMarkRank(b.mark) || b.t.updatedAt - a.t.updatedAt);
  }, [threads, allTasks, inboxByTask, q]);

  const filteredArchived = useMemo(() => {
    const k = q.trim().toLowerCase();
    return k
      ? archivedThreads.filter((t) => `${t.title} ${t.goal}`.toLowerCase().includes(k))
      : archivedThreads;
  }, [archivedThreads, q]);

  const invalidateProjectTasks = () => {
    void qc.invalidateQueries({ queryKey: ['project-tasks', projectId] });
    void qc.invalidateQueries({ queryKey: ['all-tasks'] });
    void qc.invalidateQueries({ queryKey: ['archived-tasks'] });
    void qc.invalidateQueries({ queryKey: ['global-artifacts'] });
    void qc.invalidateQueries({ queryKey: ['bootstrap'] });
  };

  const handleRestoreArchived = async (t: TaskSummary) => {
    try {
      await api.unarchiveTask(t.taskId);
      invalidateProjectTasks();
      toast(`已恢复「${taskTitle(t)}」`);
    } catch (e) {
      toast(errMsg(e), 'err');
    }
  };

  const handleDeleteArchived = async (t: TaskSummary) => {
    try {
      await api.deleteTask(t.taskId);
      setConfirmDeleteId(null);
      invalidateProjectTasks();
      toast(`已永久删除「${taskTitle(t)}」`);
    } catch (e) {
      toast(errMsg(e), 'err');
    }
  };

  const markedRows = ranked.filter((r) => r.mark !== null);
  const restingRows = ranked.filter((r) => r.mark === null);

  const withDeliveries = tab === 'deliveries' ? threads.filter((t) => t.deliveryCount > 0) : [];
  const timelines = useQueries({
    queries: withDeliveries.map((t) => ({
      queryKey: ['task-timeline', t.taskId],
      queryFn: () => api.getTaskTimeline(t.taskId),
    })),
  });
  const deliveries: { d: DeliveryView; v: number; task: TaskSummary }[] = [];
  withDeliveries.forEach((t, i) => {
    (timelines[i]?.data?.deliveries ?? []).forEach((d, idx) => deliveries.push({ d, v: idx + 1, task: t }));
  });
  deliveries.sort((a, b) => b.d.createdAt - a.d.createdAt);

  if (!project) {
    return (
      <main className="main">
        <PageHeader>
          <span className="ttl">项目</span>
        </PageHeader>
        <div className="empty">项目不存在或已归档</div>
      </main>
    );
  }

  const isPureFolder =
    (project.parts?.length ?? 0) > 0 && project.parts!.every((pt) => pt.kind === 'folder');
  const firstGitLoc = project.parts?.find((pt) => pt.kind === 'git')?.locations?.[0];

  return (
    <main className="main">
      <PageHeader>
        <div className="crumb">
          <span className="c0">项目</span>
          <span className="sep">/</span>
          <span className="c1">{project.displayName}</span>
        </div>
      </PageHeader>
      <div className="page-scroll">
        <div className="page">
          <div className="phead">
            <div style={{ minWidth: 0 }}>
              <h1>{project.displayName}</h1>
              <div className="meta">
                {!isPureFolder && project.repoIdentity && !project.repoIdentity.startsWith('folder:') && (
                  <span className="mono">{project.repoIdentity}</span>
                )}
                {!isPureFolder && (
                  <GitZshBadge
                    runtime={firstGitLoc?.runtime}
                    fallbackBranch={project.defaultBranch}
                    isGit
                  />
                )}
                {isPureFolder && project.parts?.[0]?.locations?.[0]?.absPath && (
                  <span className="mono">{project.parts[0].locations[0].absPath}</span>
                )}
              </div>
            </div>
            <div className="r">
              <button
                type="button"
                className="btn ghost"
                onClick={() => navigate(paths.newThread('project', project.projectId))}
              >
                <Plus size={13} strokeWidth={1.6} /> 新线程
              </button>
              <IdeOpenButton project={project} machines={boot.machines} />
            </div>
          </div>

          <div className="tabs" role="tablist">
            <button
              type="button"
              role="tab"
              aria-selected={tab === 'threads'}
              className={tab === 'threads' ? 'on' : ''}
              onClick={() => navigate(paths.project(projectId, 'threads'))}
            >
              会话
            </button>
            <button
              type="button"
              role="tab"
              aria-selected={tab === 'deliveries'}
              className={tab === 'deliveries' ? 'on' : ''}
              onClick={() => navigate(paths.project(projectId, 'deliveries'))}
            >
              Artifacts
            </button>
            <button
              type="button"
              role="tab"
              aria-selected={tab === 'settings'}
              className={tab === 'settings' ? 'on' : ''}
              onClick={() => navigate(paths.project(projectId, 'settings'))}
            >
              设置
            </button>
            {tab === 'threads' && threads.length > 10 && (
              <span className="sp">
                <input
                  className="field"
                  placeholder="搜索会话…"
                  value={q}
                  onChange={(e) => setQ(e.target.value)}
                  aria-label="搜索会话"
                />
              </span>
            )}
          </div>

          {tab === 'threads' && (
            <>
              <div className="proj-scope-bar">
                <div className="proj-scope-seg" role="tablist" aria-label="会话状态筛选">
                  <button
                    type="button"
                    role="tab"
                    aria-selected={threadScope === 'active'}
                    className={threadScope === 'active' ? 'on' : ''}
                    onClick={() => setThreadScope('active')}
                  >
                    活跃{threads.length > 0 ? ` (${threads.length})` : ''}
                  </button>
                  <button
                    type="button"
                    role="tab"
                    aria-selected={threadScope === 'archived'}
                    className={threadScope === 'archived' ? 'on' : ''}
                    onClick={() => setThreadScope('archived')}
                  >
                    已归档{archivedThreads.length > 0 ? ` (${archivedThreads.length})` : ''}
                  </button>
                </div>
              </div>

              {threadScope === 'active' ? (
                tasksQ.isLoading ? (
                  <div className="quiet">载入会话…</div>
                ) : ranked.length === 0 ? (
                  <div className="quiet">{q ? '没有匹配的会话' : '这个项目还没有会话'}</div>
                ) : (
                  <>
                    {markedRows.map(({ t, mark }) => (
                      <ThreadRow key={t.taskId} t={t} mark={mark} />
                    ))}
                    {markedRows.length > 0 && restingRows.length > 0 && <div className="home-sep" role="separator" />}
                    {restingRows.map(({ t, mark }) => (
                      <ThreadRow key={t.taskId} t={t} mark={mark} />
                    ))}
                  </>
                )
              ) : archivedTasksQ.isLoading ? (
                <div className="quiet">载入已归档会话…</div>
              ) : filteredArchived.length === 0 ? (
                <div className="quiet">{q ? '没有匹配的已归档会话' : '该项目还没有已归档会话'}</div>
              ) : (
                <div className="arch-list">
                  {filteredArchived.map((t) => {
                    const confirming = confirmDeleteId === t.taskId;
                    return (
                      <div className="arch-row" key={t.taskId}>
                        <button
                          type="button"
                          className="arch-row-main"
                          onClick={() => navigate(paths.session(t.taskId))}
                        >
                          <span className="arch-row-title ell">{taskTitle(t)}</span>
                          <span className="arch-row-time faint">
                            归档于 {relTime(t.archivedAt ?? t.updatedAt)}
                          </span>
                        </button>
                        <div className="arch-row-acts">
                          <button
                            type="button"
                            className="btn ghost sm"
                            onClick={() => void handleRestoreArchived(t)}
                          >
                            <ArchiveRestore size={12.5} strokeWidth={1.6} /> 恢复
                          </button>
                          {confirming ? (
                            <>
                              <button
                                type="button"
                                className="btn ghost sm bad-t"
                                onClick={() => void handleDeleteArchived(t)}
                              >
                                确认删除
                              </button>
                              <button
                                type="button"
                                className="btn ghost sm"
                                onClick={() => setConfirmDeleteId(null)}
                              >
                                取消
                              </button>
                            </>
                          ) : (
                            <button
                              type="button"
                              className="ib bad-t"
                              title="永久删除"
                              aria-label="永久删除"
                              onClick={() => setConfirmDeleteId(t.taskId)}
                            >
                              <Trash2 size={13} strokeWidth={1.6} />
                            </button>
                          )}
                        </div>
                      </div>
                    );
                  })}
                </div>
              )}
            </>
          )}

          {tab === 'deliveries' &&
            (withDeliveries.length === 0 ? (
              <div className="quiet">还没有 Artifacts 或交付</div>
            ) : (
              deliveries.map(({ d, v, task }) => {
                const ide = buildIDEJump({
                  scheme: project.ideScheme,
                  localMachineId,
                  delivery: d,
                  project,
                  machines: boot.machines,
                });
                const hasArt = (d.ryugiBindings?.length ?? 0) > 0 || (d.manifest.blobs?.length ?? 0) > 0;
                return (
                  <div className="drow" key={d.deliveryId}>
                    <span className="v mono">v{v}</span>
                    <span className="t">
                      {d.summary || taskTitle(task)}
                      <span className="faint"> · {taskTitle(task)}</span>
                    </span>
                    {d.headSha && <span className="mono faint no-mobile">{shortSha(d.headSha)}</span>}
                    <span className="mono">
                      <span className="add">+{d.manifest.insertions}</span>{' '}
                      <span className="del">−{d.manifest.deletions}</span>
                    </span>
                    <span className="faint no-mobile">{relTime(d.createdAt)}</span>
                    <span className="acts">
                      {CODE_KINDS.has(d.kind) && d.headSha && (
                        <button
                          type="button"
                          className="ib"
                          title="审阅代码"
                          onClick={() => openReview('code', d.taskId, d.deliveryId)}
                        >
                          <DiffIcon size={13} />
                        </button>
                      )}
                      {hasArt && (
                        <button
                          type="button"
                          className="ib"
                          title="查看 Artifacts"
                          onClick={() => openReview('art', d.taskId, d.deliveryId)}
                        >
                          <ArtIcon size={13} />
                        </button>
                      )}
                      {ide && (
                        <button
                          type="button"
                          className="ib no-mobile"
                          title={`在 Cursor 打开 · ${ide.targetLabel}`}
                          onClick={() => launchIDE(ide.uri)}
                        >
                          <ArrowUpRight size={13} strokeWidth={1.6} />
                        </button>
                      )}
                    </span>
                  </div>
                );
              })
            ))}

          {tab === 'settings' && <ProjectSettingsTab boot={boot} project={project} />}
        </div>
      </div>
    </main>
  );
}
