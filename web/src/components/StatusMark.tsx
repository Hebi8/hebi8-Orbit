import { CircleAlert, Hand } from 'lucide-react';
import { sessionMarkHint, type SessionMark } from '../lib/format';

/**
 * Static 3x3 dot matrix for low-stimulus active work indication without animation.
 */
function StillWorkingGrid() {
  return (
    <svg
      width={10}
      height={10}
      viewBox="0 0 10 10"
      fill="currentColor"
      aria-hidden="true"
      className="smark-grid"
    >
      <circle cx="2" cy="2" r="1" />
      <circle cx="5" cy="2" r="1" />
      <circle cx="8" cy="2" r="1" />
      <circle cx="2" cy="5" r="1" />
      <circle cx="5" cy="5" r="1" />
      <circle cx="8" cy="5" r="1" />
      <circle cx="2" cy="8" r="1" />
      <circle cx="5" cy="8" r="1" />
      <circle cx="8" cy="8" r="1" />
    </svg>
  );
}

/**
 * Single trailing low-stimulus session mark (12x12 fixed slot):
 * - failed    -> CircleAlert (var(--bad))
 * - needs-you -> Hand (var(--accent))
 * - running   -> static 3x3 dot grid (var(--muted))
 * - ready     -> 6px solid dot (var(--accent))
 * - null      -> renders nothing (resting silence)
 */
export function StatusMark({ mark }: { mark: SessionMark }) {
  if (!mark) return null;
  const hint = sessionMarkHint(mark);
  return (
    <span
      className={`smark smark-${mark}`}
      data-status-mark={mark}
      title={hint}
      aria-label={hint}
      role="img"
    >
      {mark === 'failed' && <CircleAlert size={12} strokeWidth={2.25} />}
      {mark === 'needs-you' && <Hand size={12} strokeWidth={1.9} />}
      {mark === 'running' && <StillWorkingGrid />}
      {mark === 'ready' && <span className="smark-dot" />}
    </span>
  );
}
