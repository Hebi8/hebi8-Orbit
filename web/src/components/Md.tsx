import { Fragment, type ReactNode } from 'react';

/** Renders inline `code` and **bold** without injecting HTML. */
function inline(text: string, keyBase: string): ReactNode[] {
  const out: ReactNode[] = [];
  const re = /(`[^`\n]+`|\*\*[^*\n]+\*\*)/g;
  let last = 0;
  let m: RegExpExecArray | null;
  let i = 0;
  while ((m = re.exec(text)) !== null) {
    if (m.index > last) out.push(<Fragment key={`${keyBase}t${i}`}>{text.slice(last, m.index)}</Fragment>);
    const tok = m[0];
    if (tok.startsWith('`')) out.push(<code key={`${keyBase}c${i}`}>{tok.slice(1, -1)}</code>);
    else out.push(<b key={`${keyBase}b${i}`}>{tok.slice(2, -2)}</b>);
    last = m.index + tok.length;
    i++;
  }
  if (last < text.length) out.push(<Fragment key={`${keyBase}e`}>{text.slice(last)}</Fragment>);
  return out;
}

/** Minimal, safe markdown: fenced code blocks, paragraphs, inline code / bold. */
export function Md({ text, className }: { text: string; className?: string }) {
  const blocks: ReactNode[] = [];
  const parts = text.split(/```/);
  parts.forEach((part, idx) => {
    if (idx % 2 === 1) {
      const body = part.replace(/^[a-zA-Z0-9_-]*\n/, '');
      blocks.push(<pre key={`pre${idx}`}>{body.replace(/\n$/, '')}</pre>);
      return;
    }
    const paras = part.split(/\n{2,}/).filter((p) => p.trim() !== '');
    paras.forEach((p, j) => {
      blocks.push(<p key={`p${idx}-${j}`}>{inline(p.replace(/^\n+|\n+$/g, ''), `${idx}-${j}`)}</p>);
    });
  });
  return <div className={`md ${className ?? ''}`}>{blocks}</div>;
}
