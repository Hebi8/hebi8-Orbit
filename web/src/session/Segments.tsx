import { useMemo, useState } from 'react';
import { ChevronRight } from 'lucide-react';
import { Md } from '../components/Md';
import type { Segment, ToolStep } from '../lib/segments';

function stepClass(s: ToolStep): string {
  if (s.status === 'completed') return 'ok';
  if (s.status === 'failed') return 'fail';
  if (s.status === 'in_progress') return 'live';
  return '';
}

/**
 * Unified single-block process disclosure (Orbit v4, Lody style):
 * Folds ALL reasoning/thinking chunks and ALL tool-call steps within a turn
 * into ONE collapsible `过程 · 思考与 N 步调用` block.
 */
function UnifiedProcessBlock({
  thoughts,
  steps,
  live,
}: {
  thoughts: string[];
  steps: ToolStep[];
  live?: boolean;
}) {
  const [open, setOpen] = useState(false);

  const hasThink = thoughts.length > 0;
  const stepCount = steps.length;
  if (!hasThink && stepCount === 0) return null;

  const baseLabel =
    hasThink && stepCount > 0
      ? `过程 · 思考与 ${stepCount} 步调用`
      : stepCount > 0
        ? `过程 · ${stepCount} 步调用`
        : '过程 · 思考';

  const activeStep = live
    ? [...steps].reverse().find((s) => s.status === 'in_progress' || s.status === 'pending')
    : undefined;
  const hasFailed = steps.some((s) => s.status === 'failed');

  return (
    <div className={`it proc-details ${open ? 'is-open' : ''}`}>
      <button
        type="button"
        className="proc-summary"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <ChevronRight size={12} strokeWidth={1.8} className="proc-chev" aria-hidden="true" />
        {activeStep ? (
          <>
            <span className="spin" />
            <span className="shim ell">
              {baseLabel} · {activeStep.verb} {activeStep.title}
            </span>
          </>
        ) : (
          <>
            <span>{baseLabel}</span>
            {hasFailed && (
              <span className="bad-t" style={{ fontSize: 11 }}>
                有步骤出错
              </span>
            )}
          </>
        )}
      </button>

      {open && (
        <div className="proc-body">
          {hasThink && (
            <div className="proc-think">
              <div className="proc-think-lbl">思考</div>
              <div className="proc-think-txt">{thoughts.join('\n\n')}</div>
            </div>
          )}
          {stepCount > 0 && (
            <ul className="proc-steps">
              {steps.map((s) => (
                <li key={s.id} className={`proc-step ${stepClass(s)}`} title={s.title}>
                  <span className="proc-step-dot" aria-hidden="true" />
                  <span className="proc-step-k">{s.verb}</span>
                  <span className="proc-step-tg ell">{s.title}</span>
                  <span className="proc-step-m">
                    {s.status === 'in_progress' ? (
                      <span className="spin" />
                    ) : s.status === 'failed' ? (
                      <span className="bad-t">出错</span>
                    ) : null}
                  </span>
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </div>
  );
}

/**
 * Renders one turn's raw ACP segments:
 * - All thinking + tool calls fold into ONE single `过程 · 思考与 N 步调用` block.
 * - All final prose messages render permanently visible via `<Md />`.
 */
export function Segments({ segs, live = false }: { segs: Segment[]; live?: boolean; compact?: boolean }) {
  const { thoughts, steps, visible } = useMemo(() => {
    const th: string[] = [];
    const st: ToolStep[] = [];
    const vis: Exclude<Segment, { kind: 'thought' } | { kind: 'tools' }>[] = [];
    for (const s of segs) {
      if (s.kind === 'thought') {
        const trimmed = s.text.trim();
        if (trimmed) th.push(trimmed);
      } else if (s.kind === 'tools') {
        st.push(...s.steps);
      } else {
        vis.push(s);
      }
    }
    return { thoughts: th, steps: st, visible: vis };
  }, [segs]);

  return (
    <>
      {(thoughts.length > 0 || steps.length > 0) && (
        <UnifiedProcessBlock thoughts={thoughts} steps={steps} live={live} />
      )}
      {visible.map((s) => {
        switch (s.kind) {
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
                <div style={{ minWidth: 0 }}>
                  <span className={`who ${s.author === 'human' || s.author === 'user' ? 'you' : ''}`}>
                    {s.author === 'human' ? '你' : s.author}
                  </span>
                  {s.mention && <span className="men">@{s.mention}</span>}
                  <div className="body2 bm-body">
                    <Md text={s.body} />
                  </div>
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
