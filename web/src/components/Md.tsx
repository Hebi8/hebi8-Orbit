import { Fragment, type ReactNode } from 'react';
import { navigate } from '../lib/router';

/** Renders inline `code`, **bold**, and safe Markdown links `[label](href)` without injecting HTML. */
function inline(text: string, keyBase: string): ReactNode[] {
  const out: ReactNode[] = [];
  const re = /(`[^`\n]+`|\*\*[^*\n]+\*\*|\[[^\]\n]+\]\([^)\s]+\))/g;
  let last = 0;
  let m: RegExpExecArray | null;
  let i = 0;
  while ((m = re.exec(text)) !== null) {
    if (m.index > last) out.push(<Fragment key={`${keyBase}t${i}`}>{text.slice(last, m.index)}</Fragment>);
    const tok = m[0];
    if (tok.startsWith('`')) {
      out.push(<code key={`${keyBase}c${i}`}>{tok.slice(1, -1)}</code>);
    } else if (tok.startsWith('**')) {
      out.push(<b key={`${keyBase}b${i}`}>{tok.slice(2, -2)}</b>);
    } else {
      const lm = /^\[([^\]\n]+)\]\(([^)\s]+)\)$/.exec(tok);
      if (lm) {
        const label = lm[1];
        const href = lm[2];
        if (href.startsWith('#/')) {
          out.push(
            <a
              key={`${keyBase}a${i}`}
              href={href}
              className="md-link"
              onClick={(e) => {
                e.preventDefault();
                navigate(href);
              }}
            >
              {label}
            </a>,
          );
        } else if (/^https?:\/\//i.test(href)) {
          out.push(
            <a key={`${keyBase}a${i}`} href={href} className="md-link" target="_blank" rel="noopener noreferrer">
              {label}
            </a>,
          );
        } else {
          out.push(<Fragment key={`${keyBase}l${i}`}>{tok}</Fragment>);
        }
      } else {
        out.push(<Fragment key={`${keyBase}l${i}`}>{tok}</Fragment>);
      }
    }
    last = m.index + tok.length;
    i++;
  }
  if (last < text.length) out.push(<Fragment key={`${keyBase}e`}>{text.slice(last)}</Fragment>);
  return out;
}

/** Minimal, safe markdown: headings, bullet/ordered lists, fenced code blocks, paragraphs, inline code & bold. */
export function Md({ text, className }: { text: string; className?: string }) {
  const blocks: ReactNode[] = [];
  const parts = text.split(/```/);
  parts.forEach((part, idx) => {
    if (idx % 2 === 1) {
      const body = part.replace(/^[a-zA-Z0-9_-]*\n/, '');
      blocks.push(<pre key={`pre${idx}`}>{body.replace(/\n$/, '')}</pre>);
      return;
    }
    const chunks = part.split(/\n{2,}/).filter((p) => p.trim() !== '');
    chunks.forEach((chunk, j) => {
      const lines = chunk
        .replace(/^\n+|\n+$/g, '')
        .split('\n')
        .map((l) => l.trimEnd());
      const k = `${idx}-${j}`;

      if (lines.length === 1) {
        const hm = /^(#{1,3})\s+(.+)$/.exec(lines[0]);
        if (hm) {
          const level = hm[1].length;
          if (level === 1) blocks.push(<h1 key={`h${k}`}>{inline(hm[2], k)}</h1>);
          else if (level === 2) blocks.push(<h2 key={`h${k}`}>{inline(hm[2], k)}</h2>);
          else blocks.push(<h3 key={`h${k}`}>{inline(hm[2], k)}</h3>);
          return;
        }
      }

      if (lines.every((l) => /^[-*]\s+/.test(l))) {
        blocks.push(
          <ul key={`ul${k}`}>
            {lines.map((l, li) => (
              <li key={li}>{inline(l.replace(/^[-*]\s+/, ''), `${k}-u${li}`)}</li>
            ))}
          </ul>,
        );
        return;
      }

      if (lines.every((l) => /^\d+\.\s+/.test(l))) {
        blocks.push(
          <ol key={`ol${k}`}>
            {lines.map((l, li) => (
              <li key={li}>{inline(l.replace(/^\d+\.\s+/, ''), `${k}-o${li}`)}</li>
            ))}
          </ol>,
        );
        return;
      }

      blocks.push(<p key={`p${k}`}>{inline(lines.join('\n'), k)}</p>);
    });
  });
  return <div className={`md ${className ?? ''}`}>{blocks}</div>;
}
