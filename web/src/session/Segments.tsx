import { useState } from 'react';
import { Md } from '../components/Md';
import type { Segment, ToolStep } from '../lib/segments';
import { CaretIcon } from '../components/Icons';

function stepClass(s: ToolStep): string {
  if (s.status === 'completed') return 'ok';
  if (s.status === 'failed') return 'fail';
  if (s.status === 'in_progress') return 'live';
  return '';
}

function Thought({ text, defaultOpen }: { text: string; defaultOpen: boolean }) {
  const [open, setOpen] = useState(defaultOpen);
  const chars = text.length;
  return (
    <div className={`it ${open ? 'open' : ''}`}>
      <button type="button" className="think-h" onClick={() => setOpen(!open)}>
        <CaretIcon className="i caret" />
        思考过程
        <span className="faint" style={{ fontSize: 11 }}>
          · {chars > 1000 ? `${(chars / 1000).toFixed(1)}k` : chars} 字
        </span>
      </button>
      {open && <div className="think-b">{text}</div>}
    </div>
  );
}

function ToolGroup({ steps, defaultOpen }: { steps: ToolStep[]; defaultOpen: boolean }) {
  const [open, setOpen] = useState(defaultOpen);
  const done = steps.filter((s) => s.status === 'completed').length;
  const failed = steps.filter((s) => s.status === 'failed').length;
  const live = steps.some((s) => s.status === 'in_progress' || s.status === 'pending');
  return (
    <div className={`it ${open ? 'open' : ''}`}>
      <button type="button" className="wg-h" onClick={() => setOpen(!open)}>
        <CaretIcon className="i caret" />
        <span className="t">工作组</span>
        <span className="faint">· {steps.length} 步</span>
        {live && <span className="spin" />}
        <span className="r">
          {done > 0 && <span>{done} 完成</span>}
          {failed > 0 && <span className="bad-t">{failed} 失败</span>}
        </span>
      </button>
      {open && (
        <ul className="wg-steps">
          {steps.map((s) => (
            <li key={s.id} className={`st ${stepClass(s)}`}>
              <div className="st-row" title={s.title}>
                <span className="k">{s.verb}</span>
                <span className="tg">{s.title}</span>
                <span className="m">
                  {s.status === 'in_progress' ? <span className="spin" /> : s.status === 'failed' ? <span className="bad-t">失败</span> : null}
                </span>
              </div>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/** Renders folded raw ACP segments in Opus' thin-timeline vocabulary. */
export function Segments({ segs, live }: { segs: Segment[]; live: boolean }) {
  const lastThought = segs.map((s) => s.kind).lastIndexOf('thought');
  const lastTools = segs.map((s) => s.kind).lastIndexOf('tools');
  return (
    <>
      {segs.map((s, i) => {
        switch (s.kind) {
          case 'thought':
            return <Thought key={s.key} text={s.text} defaultOpen={live && i === lastThought && i === segs.length - 1} />;
          case 'tools':
            return <ToolGroup key={s.key} steps={s.steps} defaultOpen={live && i === lastTools} />;
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
                <span className="sd" /> {s.text}
              </div>
            );
          default:
            return null;
        }
      })}
    </>
  );
}
