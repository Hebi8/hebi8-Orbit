import { isValidElement, memo, useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import { AlertCircle, Check, Copy } from 'lucide-react';
import Markdown, { type Components } from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { classifyMdHref, safeMdUrlTransform } from '../lib/markdown';
import { navigate } from '../lib/router';

export { classifyMdHref, isFootnoteHref, type MdHrefKind } from '../lib/markdown';

function extractNodeText(node: ReactNode): string {
  if (typeof node === 'string') return node;
  if (typeof node === 'number') return String(node);
  if (Array.isArray(node)) return node.map(extractNodeText).join('');
  if (isValidElement<{ children?: ReactNode }>(node)) {
    return extractNodeText(node.props.children);
  }
  return '';
}

function extractCodeLang(children: ReactNode): string {
  if (isValidElement<{ className?: string }>(children)) {
    const m = /(?:^|\s)language-([^\s]+)/.exec(children.props.className ?? '');
    if (m?.[1]) return m[1];
  }
  return '';
}

/**
 * Copies text to clipboard using navigator.clipboard when available (secure context),
 * falling back to an off-screen textarea + execCommand('copy') on non-loopback LAN HTTP origins.
 */
async function copyTextToClipboard(text: string): Promise<boolean> {
  if (typeof navigator !== 'undefined' && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // Fall through to execCommand fallback (e.g. insecure LAN HTTP or denied clipboard permission)
    }
  }
  if (typeof document === 'undefined' || !document.body) return false;
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.setAttribute('readonly', '');
  ta.style.position = 'fixed';
  ta.style.top = '-9999px';
  ta.style.left = '-9999px';
  ta.style.opacity = '0';
  document.body.appendChild(ta);
  try {
    ta.focus();
    ta.select();
    ta.setSelectionRange(0, text.length);
    return document.execCommand('copy');
  } catch {
    return false;
  } finally {
    document.body.removeChild(ta);
  }
}

type CopyState = 'idle' | 'copied' | 'failed';

function MdCodeBlock({ children }: { children?: ReactNode }) {
  const [copyState, setCopyState] = useState<CopyState>('idle');
  const timerRef = useRef<number | null>(null);
  const lang = extractCodeLang(children);

  useEffect(() => {
    return () => {
      if (timerRef.current !== null) window.clearTimeout(timerRef.current);
    };
  }, []);

  const handleCopy = useCallback(() => {
    const raw = extractNodeText(children).replace(/\n$/, '');
    void copyTextToClipboard(raw).then((ok) => {
      setCopyState(ok ? 'copied' : 'failed');
      if (timerRef.current !== null) window.clearTimeout(timerRef.current);
      timerRef.current = window.setTimeout(() => {
        timerRef.current = null;
        setCopyState('idle');
      }, ok ? 1500 : 2200);
    });
  }, [children]);

  const label = copyState === 'copied' ? '已复制' : copyState === 'failed' ? '复制失败' : '复制';

  return (
    <div className="md-codeblock">
      <div className="md-code-hd">
        <span className="md-code-lang">{lang || 'code'}</span>
        <button
          type="button"
          className={`md-code-copy ${copyState === 'copied' ? 'is-copied' : copyState === 'failed' ? 'is-failed' : ''}`}
          title={copyState === 'idle' ? '复制代码' : label}
          aria-label={copyState === 'idle' ? '复制代码' : label}
          onClick={handleCopy}
        >
          {copyState === 'copied' ? (
            <Check size={11.5} strokeWidth={2} />
          ) : copyState === 'failed' ? (
            <AlertCircle size={11.5} strokeWidth={1.8} />
          ) : (
            <Copy size={11.5} strokeWidth={1.7} />
          )}
          <span>{label}</span>
        </button>
      </div>
      <pre className="md-code-pre">{children}</pre>
    </div>
  );
}

const REMARK_PLUGINS = [remarkGfm];

const MD_COMPONENTS: Components = {
  a({ href, children, id, ...rest }) {
    const kind = classifyMdHref(href);
    const safeHref = typeof href === 'string' ? href.trim() : '';
    if (kind === 'internal') {
      return (
        <a
          id={id}
          href={safeHref}
          className="md-link"
          onClick={(e) => {
            e.preventDefault();
            navigate(safeHref);
          }}
        >
          {children}
        </a>
      );
    }
    if (kind === 'footnote') {
      const dataProps: Record<string, unknown> = {};
      for (const [k, v] of Object.entries(rest)) {
        if (k.startsWith('data-') || k.startsWith('aria-')) {
          dataProps[k] = v;
        }
      }
      return (
        <a
          id={id}
          href={safeHref}
          className="md-link md-fn-link"
          {...dataProps}
          onClick={(e) => {
            e.preventDefault();
            const targetId = safeHref.slice(1);
            const scope = e.currentTarget.closest('.md');
            if (!scope || !targetId) return;
            const target = Array.from(scope.querySelectorAll<HTMLElement>('[id]')).find((el) => el.id === targetId);
            target?.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
          }}
        >
          {children}
        </a>
      );
    }
    if (kind === 'external') {
      return (
        <a id={id} href={safeHref} className="md-link" target="_blank" rel="noopener noreferrer">
          {children}
        </a>
      );
    }
    return <>{children}</>;
  },
  pre({ children }) {
    return <MdCodeBlock>{children}</MdCodeBlock>;
  },
  table({ children }) {
    return (
      <div className="md-table-wrap">
        <table>{children}</table>
      </div>
    );
  },
  input({ type, checked }) {
    if (type !== 'checkbox') return null;
    return <input type="checkbox" className="md-task-chk" checked={Boolean(checked)} readOnly disabled />;
  },
};

/** Full GFM Markdown renderer with strict XSS safety (no raw HTML parsing) and low-stimulus code/table chrome. */
export const Md = memo(function Md({ text, className }: { text: string; className?: string }) {
  const src = text ?? '';
  return (
    <div className={`md ${className ?? ''}`.trim()}>
      <Markdown remarkPlugins={REMARK_PLUGINS} components={MD_COMPONENTS} urlTransform={safeMdUrlTransform}>
        {src}
      </Markdown>
    </div>
  );
});
