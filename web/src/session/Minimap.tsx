import { useEffect, useState, type RefObject } from 'react';
import type { TaskTimelineResponse } from '../generated/protocol';

/** Thin left-edge rail: one tick per turn, tinted for approvals / failures; click to jump. */
export function Minimap({ tl, stageRef }: { tl: TaskTimelineResponse; stageRef: RefObject<HTMLDivElement | null> }) {
  const attempts = tl.attempts;
  const [cur, setCur] = useState<string>('');

  useEffect(() => {
    const root = stageRef.current;
    if (!root || attempts.length < 2) return;
    const io = new IntersectionObserver(
      (entries) => {
        const visible = entries.filter((e) => e.isIntersecting).sort((a, b) => a.boundingClientRect.top - b.boundingClientRect.top);
        if (visible[0]) setCur(visible[0].target.id);
      },
      { root, threshold: 0.15 },
    );
    for (const a of attempts) {
      const el = document.getElementById(`turn-${a.attemptId}`);
      if (el) io.observe(el);
    }
    return () => io.disconnect();
  }, [attempts, stageRef]);

  if (attempts.length < 2) return null;
  const n = attempts.length;
  return (
    <div className="minimap no-mobile" aria-hidden="true">
      <span className="rail" />
      {attempts.map((a, i) => {
        const pendingAp = tl.approvals.some((ap) => ap.attemptId === a.attemptId && ap.state === 'pending');
        const warn = a.state === 'needs_verification' || a.state === 'failed' || a.state === 'expired';
        const id = `turn-${a.attemptId}`;
        const cls = ['tick', cur === id ? 'cur' : '', pendingAp ? 'ap' : warn ? 'warn' : ''].filter(Boolean).join(' ');
        return (
          <button
            type="button"
            key={a.attemptId}
            className={cls}
            style={{ top: `${(i / Math.max(1, n - 1)) * 100}%` }}
            title={`第 ${a.attemptNo} 轮`}
            tabIndex={-1}
            onClick={() => document.getElementById(id)?.scrollIntoView({ behavior: 'smooth', block: 'start' })}
          />
        );
      })}
    </div>
  );
}
