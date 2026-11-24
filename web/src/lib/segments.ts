import type { EventRecord } from '../generated/protocol';
import { toolVerb } from './format';

export interface ToolStep {
  id: string;
  name: string;
  title: string;
  status: string;
  verb: string;
  seq: number;
}

export type Segment =
  | { kind: 'thought'; key: string; text: string }
  | { kind: 'msg'; key: string; text: string }
  | { kind: 'tools'; key: string; steps: ToolStep[] }
  | { kind: 'board'; key: string; author: string; mention: string; body: string; seq: number }
  | { kind: 'note'; key: string; text: string };

const str = (v: unknown) => (typeof v === 'string' ? v : '');

/**
 * Folds raw ACP frames (message/thought chunks + tool_call updates) into display segments:
 * consecutive chunks of the same kind are concatenated, tool_call updates merge by toolCallId,
 * and consecutive tool calls form one "work group".
 */
export function buildSegments(events: EventRecord[]): Segment[] {
  const segs: Segment[] = [];
  const stepById = new Map<string, ToolStep>();
  const sorted = [...events].sort((a, b) => a.seq - b.seq);

  for (const ev of sorted) {
    const p = ev.payload ?? {};
    const last = segs[segs.length - 1];
    switch (ev.kind) {
      case 'acp.thought_batch': {
        const t = str(p.text);
        if (!t) break;
        if (last?.kind === 'thought') last.text += t;
        else segs.push({ kind: 'thought', key: `th${ev.seq}`, text: t });
        break;
      }
      case 'acp.message_batch': {
        const t = str(p.text);
        if (!t) break;
        if (last?.kind === 'msg') last.text += t;
        else segs.push({ kind: 'msg', key: `m${ev.seq}`, text: t });
        break;
      }
      case 'acp.tool_call': {
        const id = str(p.toolCallId) || `anon${ev.seq}`;
        const existing = stepById.get(id);
        if (existing) {
          if (str(p.name)) existing.name = str(p.name);
          if (str(p.title)) existing.title = str(p.title);
          if (str(p.status)) existing.status = str(p.status);
          existing.verb = toolVerb(existing.name, existing.title);
          break;
        }
        const step: ToolStep = {
          id,
          name: str(p.name),
          title: str(p.title) || str(p.name) || '工具调用',
          status: str(p.status) || 'pending',
          verb: toolVerb(str(p.name), str(p.title)),
          seq: ev.seq,
        };
        stepById.set(id, step);
        if (last?.kind === 'tools') last.steps.push(step);
        else segs.push({ kind: 'tools', key: `tg${ev.seq}`, steps: [step] });
        break;
      }
      case 'board.message_posted': {
        segs.push({
          kind: 'board',
          key: `b${ev.seq}`,
          author: str(p.authorName) || ev.actorId,
          mention: str(p.targetMention),
          body: str(p.body),
          seq: typeof p.seq === 'number' ? p.seq : ev.seq,
        });
        break;
      }
      case 'board.member_updated': {
        const step = str(p.currentStep);
        const st = str(p.state);
        segs.push({ kind: 'note', key: `n${ev.seq}`, text: `${st || '状态更新'}${step ? ` · ${step}` : ''}` });
        break;
      }
      default:
        break;
    }
  }
  return segs;
}

export function lastLiveStep(segs: Segment[]): ToolStep | null {
  for (let i = segs.length - 1; i >= 0; i--) {
    const s = segs[i];
    if (s.kind === 'tools') {
      for (let j = s.steps.length - 1; j >= 0; j--) {
        const st = s.steps[j];
        if (st.status === 'in_progress' || st.status === 'pending') return st;
      }
      return s.steps[s.steps.length - 1] ?? null;
    }
  }
  return null;
}
