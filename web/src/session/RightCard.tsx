import { useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Brain, FileText, Link2, Plus, X } from 'lucide-react';
import type {
  MachineView,
  ProjectView,
  TaskBoardResponse,
  TaskSourceKind,
  TaskTimelineResponse,
} from '../generated/protocol';
import { ArtIcon, BranchIcon, CursorIcon, SwarmIcon } from '../components/Icons';
import { api, errMsg } from '../lib/api';
import { basename, memberDot, memberStateLabel, shortSha } from '../lib/format';
import { buildIDEJump, launchIDE } from '../lib/ide';
import { navigate, paths } from '../lib/router';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';

const CODE_KINDS = new Set(['code_diff', 'mixed', 'workspace_snapshot']);

function SourceIcon({ kind }: { kind: TaskSourceKind }) {
  if (kind === 'link') return <Link2 size={13} strokeWidth={1.6} className="faint" />;
  if (kind === 'nmem') return <Brain size={13} strokeWidth={1.6} className="faint" />;
  return <FileText size={13} strokeWidth={1.6} className="faint" />;
}

/** Codex-Desktop-style compact floating card: every block appears only when it has real data. */
export function RightCard({
  tl,
  project,
  machines,
  board,
  onOpenBoard,
}: {
  tl: TaskTimelineResponse;
  project?: ProjectView;
  machines: MachineView[];
  board?: TaskBoardResponse;
  onOpenBoard: () => void;
}) {
  const qc = useQueryClient();
  const openWindow = useWorkbenchStore((s) => s.openWindow);
  const localMachineId = useWorkbenchStore((s) => s.localMachineId);
  const [adding, setAdding] = useState(false);
  const [kind, setKind] = useState<TaskSourceKind>('link');
  const [label, setLabel] = useState('');
  const [uri, setUri] = useState('');
  const taskId = tl.task.taskId;

  const formalDeliveries = tl.deliveries.filter((d) => !d.lateArrival);
  const formalCodeDeliveries = formalDeliveries.filter((d) => CODE_KINDS.has(d.kind) && d.manifest.filesChanged > 0);
  const latestCode =
    formalCodeDeliveries.find((d) => d.deliveryId === tl.task.latestDeliveryId) ??
    formalCodeDeliveries[formalCodeDeliveries.length - 1];
  const artDelivery = [...formalDeliveries].reverse().find((d) => (d.ryugiBindings?.length ?? 0) > 0);
  const members = board?.members ?? tl.boardMembers;
  const showWorkers = members.length > 0 || tl.task.workerCount > 0;

  const addSrc = useMutation({
    mutationFn: () => api.addSource(taskId, { kind, label: label.trim() || uri.trim(), uriOrRef: uri.trim() }),
    onSuccess: () => {
      setAdding(false);
      setLabel('');
      setUri('');
      void qc.invalidateQueries({ queryKey: ['task-timeline', taskId] });
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });
  const delSrc = useMutation({
    mutationFn: (sourceId: string) => api.deleteSource(taskId, sourceId),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['task-timeline', taskId] }),
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const ide =
    latestCode && project
      ? buildIDEJump({ scheme: project.ideScheme, localMachineId, delivery: latestCode, project, machines })
      : null;
  const branchLabel = latestCode ? basename(latestCode.manifest.worktreePath) || latestCode.gitRef.split('/').pop() || 'worktree' : '';

  return (
    <aside className="fcard" aria-label="会话上下文">
      {latestCode && (
        <div className="fsec">
          <div className="fsec-h">
            Changes
            <span className="r mono">
              v{formalCodeDeliveries.length}
              {formalCodeDeliveries.length > 1 ? ` · 共 ${formalCodeDeliveries.length} 版` : ''}
            </span>
          </div>
          <div className="frow" role="button" tabIndex={0} onClick={() => openWindow('cr', taskId, latestCode.deliveryId)} onKeyDown={(e) => e.key === 'Enter' && openWindow('cr', taskId, latestCode.deliveryId)} title="打开 Code Review 浮窗">
            <BranchIcon size={13} />
            <span className="ell mono" style={{ flex: 1 }}>
              {branchLabel} · {shortSha(latestCode.headSha)}
            </span>
            <span className="fx mono hide-on-hover">
              <span className="add">+{latestCode.manifest.insertions}</span> <span className="del">−{latestCode.manifest.deletions}</span>
            </span>
            {ide && (
              <button
                type="button"
                className="ib hov"
                title={`在 Cursor 打开 · ${ide.targetLabel}`}
                onClick={(e) => {
                  e.stopPropagation();
                  launchIDE(ide.uri);
                }}
              >
                <CursorIcon size={13} />
              </button>
            )}
          </div>
          <div className="fsub">
            {latestCode.manifest.filesChanged} 个文件
            {latestCode.openAnnotations > 0 ? ` · ${latestCode.openAnnotations} 条未决行评` : ''}
          </div>
        </div>
      )}

      {artDelivery && (
        <div className="fsec">
          <div className="fsec-h">Artifacts</div>
          {(artDelivery.ryugiBindings ?? []).map((b) => {
            const openNotes = tl.annotations.filter((a) => a.deliveryId === artDelivery.deliveryId && a.source === 'ryugi' && a.status === 'open').length;
            return (
              <button type="button" key={b.bindingId} className="frow" onClick={() => openWindow('art', taskId, artDelivery.deliveryId)} title={b.artifactLocator}>
                <ArtIcon size={13} />
                <span className="ell" style={{ flex: 1 }}>
                  {basename(b.artifactLocator) || b.artifactLocator}
                </span>
                <span className="fx">{openNotes > 0 ? `${openNotes} 未决` : '冻结'}</span>
              </button>
            );
          })}
        </div>
      )}

      {showWorkers && (
        <div className="fsec">
          <div className="fsec-h">
            Queue & Workers
            <span className="r mono">{members.length || tl.task.workerCount}</span>
          </div>
          {members.map((m) => (
            <button
              type="button"
              key={m.memberName}
              className="frow"
              onClick={() => navigate(paths.session(taskId, m.memberName))}
              title={`${memberStateLabel(m.state)}${m.currentStep ? ` · ${m.currentStep}` : ''}`}
            >
              <span className={`sd ${memberDot(m.state)}`} />
              <span className="ell" style={{ flex: 1 }}>
                {m.memberName}
                {m.currentStep && <span className="faint"> · {m.currentStep}</span>}
              </span>
              <span className="fx mono">#{m.lastConsumedSeq}</span>
            </button>
          ))}
          {members.length === 0 && <div className="fsub">{tl.task.workerCount} 个子任务 Worker</div>}
          <button type="button" className="flink" onClick={onOpenBoard}>
            <SwarmIcon size={12} /> 打开消息板{board ? ` (#${board.latestSeq})` : ''} →
          </button>
        </div>
      )}

      <div className="fsec">
        <div className="fsec-h">
          Sources
          <span className="r">
            <button type="button" className="ib" title="添加 Source" onClick={() => setAdding(!adding)}>
              <Plus size={12} strokeWidth={1.6} />
            </button>
          </span>
        </div>
        {tl.sources.length === 0 && !adding && <div className="fsub">挂载参考文件、链接或 nmem 记忆</div>}
        {tl.sources.map((s) => (
          <div className="frow" key={s.sourceId} title={s.uriOrRef}>
            <SourceIcon kind={s.kind} />
            {s.kind === 'link' && /^https?:\/\//.test(s.uriOrRef) ? (
              <a className="ell" style={{ flex: 1 }} href={s.uriOrRef} target="_blank" rel="noopener noreferrer">
                {s.label}
              </a>
            ) : (
              <span className="ell" style={{ flex: 1 }}>
                {s.label}
              </span>
            )}
            <button type="button" className="ib hov" title="移除" disabled={delSrc.isPending} onClick={() => delSrc.mutate(s.sourceId)}>
              <X size={12} strokeWidth={1.6} />
            </button>
          </div>
        ))}
        {adding && (
          <form
            className="srcform"
            onSubmit={(e) => {
              e.preventDefault();
              if (uri.trim()) addSrc.mutate();
            }}
          >
            <select className="field" value={kind} onChange={(e) => setKind(e.target.value as TaskSourceKind)}>
              <option value="link">链接</option>
              <option value="file">文件</option>
              <option value="nmem">nmem 记忆</option>
            </select>
            <input className="field" placeholder="标题（可选）" value={label} onChange={(e) => setLabel(e.target.value)} />
            <input
              className="field"
              placeholder={kind === 'link' ? 'https://…' : kind === 'file' ? '仓库内路径' : 'nmem 引用'}
              value={uri}
              onChange={(e) => setUri(e.target.value)}
              autoFocus
            />
            <div className="row">
              <button type="button" className="btn ghost" onClick={() => setAdding(false)}>
                取消
              </button>
              <button type="submit" className="btn pri" disabled={!uri.trim() || addSrc.isPending}>
                添加
              </button>
            </div>
          </form>
        )}
      </div>
    </aside>
  );
}
