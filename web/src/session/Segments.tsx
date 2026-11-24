import { useState } from 'react';
import { CaretIcon } from '../components/Icons';
import { Md } from '../components/Md';
import type { Segment, ToolStep } from '../lib/segments';

function stepClass(s: ToolStep): string {
  if (s.status === 'completed') return 'ok';
  if (s.status === 'failed') return 'fail';
  if (s.status === 'in_progress') return 'live';
  return '';
}

function Thought({ text }: { text: string }) {
  const [open, setOpen] = useState(false);
  return (
    <div className={`it ${open ? 'open' : ''}`}>
      <button type="button" className="think-h" onClick={() => setOpen(!open)}>
        <CaretIcon className="i caret" />
        思考
      </button>
      {open && <div className="think-b">{text}</div>}
    </div>
  );
}

function ToolGroup({ steps }: { steps: ToolStep[] }) {
  const [open, setOpen] = useState(false);
  const activeStep = [...steps].reverse().find((s) => s.status === 'in_progress' || s.status === 'pending');
  const hasFailed = steps.some((s) => s.status === 'failed');

  return (
    <div className={`it ${open ? 'open' : ''}`}>
      <button type="button" className="wg-h" onClick={() => setOpen(!open)}>
        <CaretIcon className="i caret" />
        {activeStep ? (
          <>
            <span className="spin" />
            <span className="shim ell">
              {activeStep.verb} {activeStep.title}
            </span>
          </>
        ) : (
          <>
            <span className="t">过程</span>
            {hasFailed && <span className="bad-t" style={{ fontSize: 11.5 }}>有步骤出错</span>}
          </>
        )}
      </button>
      {open && (
        <ul className="wg-steps">
          {steps.map((s) => (
            <li key={s.id} className={`st ${stepClass(s)}`}>
              <div className="st-row" title={s.title}>
                <span className="k">{s.verb}</span>
                <span className="tg">{s.title}</span>
                <span className="m">
                  {s.status === 'in_progress' ? <span className="spin" /> : s.status === 'failed' ? <span className="bad-t">出错</span> : null}
                </span>
              </div>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/** Renders folded raw ACP segments cleanly; thoughts are hidden in compact split-review panes. */
export function Segments({ segs, compact = false }: { segs: Segment[]; live?: boolean; compact?: boolean }) {
  return (
    <>
      {segs.map((s) => {
        switch (s.kind) {
          case 'thought':
            return compact ? null : <Thought key={s.key} text={s.text} />;
          case 'tools':
            return <ToolGroup key={s.key} steps={s.steps} />;
          case 'msg':
            return (
              <div className="it" key={s.key}>
                <Md text={s.text} />
              </div>
            );
          case 'board':
            return (
              <div className="it bm" key={s.key} style={{ padding: '4px 0' }}>
                <span className="seq mono">#{s.seq}</span>
                <div>
                  <span className={`who ${s.author === 'human' || s.author === 'user' ? 'you' : ''}`}>{s.author === 'human' ? '你' : s.author}</span>
                  {s.mention && <span className="men">@{s.mention}</span>}
                  <span className="body2">{s.body}</span>
                </div>
              </div>
            );
          case 'note':
            return (
              <div className="it dline faint" key={s.key}>
                {s.text}
              </div>
            );
          default:
            return null;
        }
      })}
    </>
  );
}
