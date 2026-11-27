import { isValidElement, memo, useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import { Check, Copy } from 'lucide-react';
import Markdown, { type Components } from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { classifyMdHref, safeMdUrlTransform } from '../lib/markdown';
import { navigate } from '../lib/router';

export { classifyMdHref, type MdHrefKind } from '../lib/markdown';

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

function MdCodeBlock({ children }: { children?: ReactNode }) {
  const [copied, setCopied] = useState(false);
  const timerRef = useRef<number | null>(null);
  const lang = extractCodeLang(children);

  useEffect(() => {
    return () => {
      if (timerRef.current !== null) window.clearTimeout(timerRef.current);
    };
  }, []);

  const handleCopy = useCallback(() => {
    const raw = extractNodeText(children).replace(/\n$/, '');
    const markCopied = () => {
      setCopied(true);
      if (timerRef.current !== null) window.clearTimeout(timerRef.current);
      timerRef.current = window.setTimeout(() => {
        timerRef.current = null;
        setCopied(false);
      }, 1500);
    };
    if (navigator.clipboard?.writeText) {
      void navigator.clipboard.writeText(raw).then(markCopied).catch(() => {});
    }
  }, [children]);

  return (
    <div className="md-codeblock">
      <div className="md-code-hd">
        <span className="md-code-lang">{lang || 'code'}</span>
        <button
          type="button"
          className={`md-code-copy ${copied ? 'is-copied' : ''}`}
          title={copied ? '已复制' : '复制代码'}
          aria-label={copied ? '已复制代码' : '复制代码'}
          onClick={handleCopy}
        >
          {copied ? <Check size={11.5} strokeWidth={2} /> : <Copy size={11.5} strokeWidth={1.7} />}
          <span>{copied ? '已复制' : '复制'}</span>
        </button>
      </div>
      <pre className="md-code-pre">{children}</pre>
    </div>
  );
}

const REMARK_PLUGINS = [remarkGfm];

const MD_COMPONENTS: Components = {
  a({ href, children }) {
    const kind = classifyMdHref(href);
    const safeHref = typeof href === 'string' ? href.trim() : '';
    if (kind === 'internal') {
      return (
        <a
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
    if (kind === 'external') {
      return (
        <a href={safeHref} className="md-link" target="_blank" rel="noopener noreferrer">
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
