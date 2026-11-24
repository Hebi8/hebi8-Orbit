import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { ArrowLeft, ChevronDown, Link2 } from 'lucide-react';
import type { BootstrapResponse, PendingInput } from '../generated/protocol';
import { BranchIcon, CardIcon, CursorIcon, SwarmIcon } from '../components/Icons';
import { PageHeader } from '../components/PageHeader';
import { useBoard, useTimeline } from '../hooks/queries';
import { api, errMsg } from '../lib/api';
import { SCRATCH_PROJECT_ID, basename, memberStateLabel, shortSha, taskStateLabel, taskTitle } from '../lib/format';
import { buildIDEJump, buildProjectIDEJump, launchIDE } from '../lib/ide';
import { navigate, paths } from '../lib/router';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';
import { BoardPanel } from './BoardPanel';
import { Composer } from './Composer';
import { Minimap } from './Minimap';
import { QueueBubbles } from './QueueBubbles';
import { RightCard } from './RightCard';
import { Turn } from './Turn';
import { WorkerStream } from './WorkerStream';

const CODE_KINDS = new Set(['code_diff', 'mixed', 'workspace_snapshot']);

function BindMenu({ boot, taskId }: { boot: BootstrapResponse; taskId: string }) {
  const qc = useQueryClient();
  const [open, setOpen] = useState(false);
  const projects = boot.projects.filter((p) => p.projectId !== SCRATCH_PROJECT_ID && !p.archived);
  const bind = useMutation({
    mutationFn: (pid: string) => api.bindProject(taskId, pid),
    onSuccess: (res) => {
      toast(`已绑定到 ${boot.projects.find((p) => p.projectId === res.toProjectId)?.displayName ?? res.toProjectId}`);
      setOpen(false);
      void qc.invalidateQueries({ queryKey: ['task-timeline', taskId] });
      void qc.invalidateQueries({ queryKey: ['project-tasks'] });
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });
  return (
    <div className="relpos" style={{ flex: 'none' }}>
      <button type="button" className="branch" style={{ cursor: 'pointer' }} onClick={() => setOpen(!open)}>
        <Link2 size={12} strokeWidth={1.6} /> 绑定到项目… <ChevronDown size={11} strokeWidth={1.6} />
      </button>
      {open && (
        <>
          <div className="scrim" style={{ background: 'transparent', backdropFilter: 'none', zIndex: 49 }} onClick={() => setOpen(false)} />
          <div className="menu" style={{ top: 28, left: 0 }}>
            <div className="mh">把这个自由 Chat 迁移为项目线程</div>
            {projects.map((p) => (
              <button type="button" key={p.projectId} disabled={bind.isPending} onClick={() => bind.mutate(p.projectId)}>
                <span className="pl">{p.displayName.slice(0, 1)}</span>
                <span className="ell">{p.displayName}</span>
              </button>
            ))}
            {projects.length === 0 && <div className="mh">还没有可绑定的项目</div>}
          </div>
        </>
      )}
    </div>
  );
}

export function SessionPage({ boot, taskId, worker }: { boot: BootstrapResponse; taskId: string; worker?: string }) {
  const tlq = useTimeline(taskId);
  const tl = tlq.data;
  const task = tl?.task;
  const project = boot.projects.find((p) => p.projectId === task?.projectId);
  const isScratch = task?.projectId === SCRATCH_PROJECT_ID;

  const boardOpen = useWorkbenchStore((s) => s.boardOpen);
  const setBoardOpen = useWorkbenchStore((s) => s.setBoardOpen);
  const cardOpen = useWorkbenchStore((s) => s.rightCardOpen);
  const setCardOpen = useWorkbenchStore((s) => s.setRightCardOpen);
  const mobileDock = useWorkbenchStore((s) => s.mobileDockOpen);
  const setMobileDock = useWorkbenchStore((s) => s.setMobileDockOpen);
  const remember = useWorkbenchStore((s) => s.rememberPrompt);
  const expandedProjects = useWorkbenchStore((s) => s.expandedProjects);
  const setProjectExpanded = useWorkbenchStore((s) => s.setProjectExpanded);
  const localMachineId = useWorkbenchStore((s) => s.localMachineId);

  const members = tl?.boardMembers ?? [];
  const hasSwarm = members.length > 0 || (task?.workerCount ?? 0) > 0;
  const boardQ = useBoard(taskId, hasSwarm || boardOpen || !!worker);
  const [editing, setEditing] = useState<PendingInput | null>(null);
  const stageRef = useRef<HTMLDivElement | null>(null);
  const tlRef = useRef<HTMLDivElement | null>(null);
  const stick = useRef(true);

  // Reset transient per-session UI when switching threads.
  useEffect(() => {
    setEditing(null);
    setMobileDock(false);
    stick.current = true;
  }, [taskId, setMobileDock]);

  // Drilling into a worker keeps the shared board open for side-by-side comparison.
  useEffect(() => {
    if (worker) setBoardOpen(true);
  }, [worker, setBoardOpen]);

  useEffect(() => {
    if (task && !isScratch && expandedProjects[task.projectId] === undefined) setProjectExpanded(task.projectId, true);
  }, [task, isScratch, expandedProjects, setProjectExpanded]);

  // Remember queued bodies so a dispatched follow-up can still show its text after it leaves the queue.
  useEffect(() => {
    for (const pi of tl?.pendingInputs ?? []) remember(`inp:${pi.inputId}`, pi.body);
  }, [tl?.pendingInputs, remember]);

  // Stick-to-bottom while new turns / raw frames stream in.
  useLayoutEffect(() => {
    const stage = stageRef.current;
    const inner = tlRef.current;
    if (!stage || !inner) return;
    const ro = new ResizeObserver(() => {
      if (stick.current) stage.scrollTop = stage.scrollHeight;
    });
    ro.observe(inner);
    return () => ro.disconnect();
  }, [taskId, worker, !!tl]);

  const onScroll = () => {
    const el = stageRef.current;
    if (!el) return;
    stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 140;
  };

  if (tlq.isError) {
    return (
      <main className="main">
        <PageHeader>
          <span className="ttl">会话</span>
        </PageHeader>
        <div className="empty">载入会话失败：{errMsg(tlq.error)}</div>
      </main>
    );
  }
  if (!tl || !task) {
    return (
      <main className="main">
        <PageHeader>
          <span className="ttl faint">载入会话…</span>
        </PageHeader>
      </main>
    );
  }

  const codeDeliveries = tl.deliveries.filter((d) => CODE_KINDS.has(d.kind));
  const latestCode = codeDeliveries[codeDeliveries.length - 1];
  const lastAttempt = tl.attempts[tl.attempts.length - 1];
  const branchText = latestCode
    ? `${basename(latestCode.manifest.worktreePath) || 'worktree'} · ${shortSha(latestCode.headSha)}`
    : lastAttempt?.worktreePath
      ? `${basename(lastAttempt.worktreePath)}${lastAttempt.baseSha ? ` · ${shortSha(lastAttempt.baseSha)}` : ''}`
      : '';
  const ide = project
    ? latestCode
      ? buildIDEJump({ scheme: project.ideScheme, localMachineId, delivery: latestCode, project, machines: boot.machines })
      : buildProjectIDEJump({
          project,
          machines: boot.machines,
          localMachineId,
          worktreePath: lastAttempt?.worktreePath,
          worktreeMachineId: lastAttempt?.machineId,
        })
    : null;
  const drilledMember = worker ? (boardQ.data?.members ?? members).find((m) => m.memberName === worker) : undefined;
  const showBoardBtn = hasSwarm || boardOpen;
  const dockMode: 'board' | 'card' | null = boardOpen ? 'board' : cardOpen ? 'card' : null;
  const pendingInputs = [...tl.pendingInputs].sort((a, b) => a.position - b.position || a.createdAt - b.createdAt);

  return (
    <main className="main">
      <PageHeader
        right={
          <>
            {ide && !isScratch && (
              <button type="button" className="ib no-mobile" title={`在 Cursor 打开 · ${ide.targetLabel}`} onClick={() => launchIDE(ide.uri)}>
                <CursorIcon />
              </button>
            )}
            {showBoardBtn && (
              <button
                type="button"
                className={`hb ${boardOpen ? 'on' : ''}`}
                title="共享消息板"
                onClick={() => {
                  setBoardOpen(!boardOpen);
                  setMobileDock(!boardOpen);
                }}
              >
                <SwarmIcon />
                <span className="mono" style={{ fontSize: 11 }}>
                  {members.length} · #{boardQ.data?.latestSeq ?? 0}
                </span>
              </button>
            )}
            <button
              type="button"
              className={`ib ${cardOpen && !boardOpen ? 'on' : ''} no-mobile`}
              title="右侧浮卡"
              onClick={() => {
                if (boardOpen) {
                  setBoardOpen(false);
                  setCardOpen(true);
                } else setCardOpen(!cardOpen);
              }}
            >
              <CardIcon />
            </button>
            <button
              type="button"
              className="ib only-mobile"
              title="会话上下文"
              onClick={() => {
                setBoardOpen(false);
                setMobileDock(true);
              }}
            >
              <CardIcon />
            </button>
          </>
        }
      >
        {worker ? (
          <>
            <button type="button" className="backbtn" onClick={() => navigate(paths.session(taskId))}>
              <ArrowLeft size={13} strokeWidth={1.6} /> 返回主会话
            </button>
            <div className="crumb">
              <span className="sep">/</span>
              <span className="c1">@{worker}</span>
              {drilledMember && <span className="faint" style={{ fontSize: 11.5 }}>{memberStateLabel(drilledMember.state)}</span>}
            </div>
          </>
        ) : (
          <>
            <div className="crumb">
              {isScratch ? (
                <span className="c0">自由 Chat</span>
              ) : (
                <button type="button" className="c0" onClick={() => project && navigate(paths.project(project.projectId))}>
                  {project?.displayName ?? task.projectId}
                </button>
              )}
              <span className="sep">/</span>
              <span className="c1" title={task.goal}>
                {taskTitle(task)}
              </span>
            </div>
            {isScratch ? (
              <BindMenu boot={boot} taskId={taskId} />
            ) : (
              branchText && (
                <span className="branch mono" title={latestCode?.gitRef ?? lastAttempt?.worktreePath}>
                  <BranchIcon size={12} />
                  {branchText}
                </span>
              )
            )}
            <span className="faint no-mobile" style={{ fontSize: 11.5, whiteSpace: 'nowrap' }}>
              {taskStateLabel(task)}
            </span>
          </>
        )}
      </PageHeader>

      <div className="sess">
        <div className="sess-body">
          <div className="stage" ref={stageRef} onScroll={onScroll}>
            {worker ? (
              <div ref={tlRef}>
                <WorkerStream taskId={taskId} worker={worker} member={drilledMember} />
              </div>
            ) : (
              <>
                <Minimap tl={tl} stageRef={stageRef} />
                <div className="tl" ref={tlRef}>
                  {tl.attempts.length === 0 && (
                    <>
                      <div className="it u">
                        <div className="ub">{task.goal}</div>
                        <div className="umeta">初始目标</div>
                      </div>
                      <div className="it running">
                        <span className="spin" />
                        <span className="shim">{taskStateLabel(task)}</span>
                      </div>
                    </>
                  )}
                  {tl.attempts.map((a, i) => (
                    <Turn
                      key={a.attemptId}
                      attempt={a}
                      index={i}
                      isLast={i === tl.attempts.length - 1}
                      tl={tl}
                      onOpenBoard={() => setBoardOpen(true)}
                    />
                  ))}
                  <QueueBubbles
                    taskId={taskId}
                    items={pendingInputs}
                    busy={!!task.activeAttemptId}
                    editingId={editing?.inputId ?? null}
                    onEdit={setEditing}
                  />
                </div>
              </>
            )}
          </div>
          <Composer task={task} worker={worker} members={members} editing={editing} onEditDone={() => setEditing(null)} />
        </div>
        {(dockMode || mobileDock) && (
          <div
            className={`dock ${dockMode === 'card' || (!dockMode && mobileDock) ? 'has-card' : ''} ${mobileDock ? 'mobile-open' : ''}`}
            onClick={(e) => {
              if (e.target === e.currentTarget) setMobileDock(false);
            }}
          >
            {boardOpen ? (
              <BoardPanel
                taskId={taskId}
                board={boardQ.data}
                fallbackMembers={members}
                worker={worker}
                onClose={() => {
                  setBoardOpen(false);
                  setMobileDock(false);
                }}
              />
            ) : (
              <RightCard
                tl={tl}
                project={project}
                machines={boot.machines}
                board={boardQ.data}
                onOpenBoard={() => {
                  setBoardOpen(true);
                }}
              />
            )}
          </div>
        )}
      </div>
    </main>
  );
}
