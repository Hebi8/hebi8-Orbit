import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import type { BoardMemberCursor, BootstrapResponse, PendingInput, TaskTimelineResponse } from '../generated/protocol';
import { useWorkbenchStore } from '../store/useWorkbenchStore';
import { Composer } from './Composer';
import { Minimap } from './Minimap';
import { QueueBubbles } from './QueueBubbles';
import { Turn } from './Turn';
import { WorkerStream } from './WorkerStream';

/**
 * Reusable live conversation stream + composer shared between the full SessionPage
 * and the right-hand session pane of the fixed split ReviewOverlay.
 */
export function SessionConversation({
  taskId,
  tl,
  boot,
  worker,
  drilledMember,
  compact = false,
  onOpenReview,
}: {
  taskId: string;
  tl: TaskTimelineResponse;
  boot?: BootstrapResponse;
  worker?: string;
  drilledMember?: BoardMemberCursor;
  compact?: boolean;
  onOpenReview?: () => void;
}) {
  const remember = useWorkbenchStore((s) => s.rememberPrompt);
  const [editing, setEditing] = useState<PendingInput | null>(null);
  const stageRef = useRef<HTMLDivElement | null>(null);
  const tlRef = useRef<HTMLDivElement | null>(null);
  const stick = useRef(true);

  useEffect(() => {
    setEditing(null);
    stick.current = true;
  }, [taskId]);

  useEffect(() => {
    for (const pi of tl.pendingInputs) remember(`inp:${pi.inputId}`, pi.body);
  }, [tl.pendingInputs, remember]);

  useLayoutEffect(() => {
    const stage = stageRef.current;
    const inner = tlRef.current;
    if (!stage || !inner) return;
    const ro = new ResizeObserver(() => {
      if (stick.current) stage.scrollTop = stage.scrollHeight;
    });
    ro.observe(inner);
    return () => ro.disconnect();
  }, [taskId, worker]);

  const onScroll = () => {
    const el = stageRef.current;
    if (!el) return;
    stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 140;
  };

  const task = tl.task;
  const members = tl.boardMembers ?? [];
  const lastAttempt = tl.attempts[tl.attempts.length - 1];
  const needsVerifyAttemptId = !task.activeAttemptId && lastAttempt?.state === 'needs_verification' ? lastAttempt.attemptId : undefined;
  const pendingInputs = [...tl.pendingInputs].sort((a, b) => a.position - b.position || a.createdAt - b.createdAt);
  const openAnnotations = useMemo(() => tl.annotations.filter((a) => a.status === 'open'), [tl.annotations]);

  return (
    <div className={`sess-body ${compact ? 'compact' : ''}`}>
      <div className="stage" ref={stageRef} onScroll={onScroll}>
        {worker ? (
          <div ref={tlRef}>
            <WorkerStream taskId={taskId} worker={worker} member={drilledMember} />
          </div>
        ) : (
          <>
            {!compact && <Minimap tl={tl} stageRef={stageRef} />}
            <div className="tl" ref={tlRef}>
              {tl.attempts.length === 0 && (
                <>
                  <div className="it u">
                    <div className="ub">{task.goal}</div>
                  </div>
                  <div className="it running">
                    <span className="spin" />
                    <span className="shim">准备中…</span>
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
                  compact={compact}
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
      <Composer
        task={task}
        worker={worker}
        members={members}
        editing={editing}
        needsVerifyAttemptId={needsVerifyAttemptId}
        onEditDone={() => setEditing(null)}
        openAnnotations={openAnnotations}
        runtimeConfig={tl.runtimeConfig}
        machines={boot?.machines}
        compact={compact}
        onOpenReview={onOpenReview}
      />
    </div>
  );
}
