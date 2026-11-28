import {
  createContext,
  isValidElement,
  memo,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react';
import { AlertCircle, Check, Copy } from 'lucide-react';
import Markdown, { type Components } from 'react-markdown';
import remarkGfm from 'remark-gfm';
import type { IDEScheme, MachineView } from '../generated/protocol';
import {
  buildFileIDEJump,
  getCustomSshAlias,
  ideLabel,
  launchIDE,
  resolveEffectiveLocalMachineId,
} from '../lib/ide';
import {
  classifyMdHref,
  findFootnoteTargetInContainer,
  parseMdFileHref,
  safeMdUrlTransform,
} from '../lib/markdown';
import { navigate } from '../lib/router';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';

export {
  classifyMdHref,
  isFootnoteHref,
  parseMdFileHref,
  type MdHrefKind,
  type ParsedMdFileHref,
} from '../lib/markdown';

export interface MdLinkContextValue {
  machineId?: string;
  worktreePath?: string;
  machines?: MachineView[];
  localMachineId?: string;
  ideScheme?: IDEScheme;
  recommendedSshHost?: string;
}

export const MdLinkContext = createContext<MdLinkContextValue>({});

const EMPTY_MACHINES: MachineView[] = [];

/**
 * Provides execution-machine and working-directory context so `<Md />` file links
 * can resolve relative paths and jump straight into the user's preferred IDE (local or Remote-SSH).
 */
export function MdLinkProvider({
  machineId,
  worktreePath,
  machines,
  localMachineId,
  ideScheme,
  recommendedSshHost,
  children,
}: MdLinkContextValue & { children: ReactNode }) {
  const parent = useContext(MdLinkContext);
  const value = useMemo<MdLinkContextValue>(() => {
    const resolvedMachineId = machineId !== undefined ? machineId : parent.machineId;
    const machineChanged = resolvedMachineId !== parent.machineId;
    return {
      machineId: resolvedMachineId,
      worktreePath:
        worktreePath !== undefined ? worktreePath : machineChanged ? undefined : parent.worktreePath,
      machines: machines !== undefined ? machines : parent.machines,
      localMachineId: localMachineId !== undefined ? localMachineId : parent.localMachineId,
      ideScheme: ideScheme !== undefined ? ideScheme : parent.ideScheme,
      recommendedSshHost:
        recommendedSshHost !== undefined
          ? recommendedSshHost
          : machineChanged
            ? undefined
            : parent.recommendedSshHost,
    };
  }, [
    machineId,
    worktreePath,
    machines,
    localMachineId,
    ideScheme,
    recommendedSshHost,
    parent.machineId,
    parent.worktreePath,
    parent.machines,
    parent.localMachineId,
    parent.ideScheme,
    parent.recommendedSshHost,
  ]);

  return <MdLinkContext.Provider value={value}>{children}</MdLinkContext.Provider>;
}

function fileBaseName(path: string): string {
  const parts = path.split('/').filter(Boolean);
  return parts[parts.length - 1] || path;
}

function formatParsedLineSuffix(line?: number, endLine?: number, column?: number): string {
  if (!line) return '';
  if (column) return `:${line}:${column}`;
  if (endLine && endLine !== line) return `:${line}-${endLine}`;
  return `:${line}`;
}

function MdFileLink({ id, rawHref, children }: { id?: string; rawHref: string; children?: ReactNode }) {
  const ctx = useContext(MdLinkContext);
  const preferredIDE = useWorkbenchStore((s) => s.preferredIDE);
  const storedLocalId = useWorkbenchStore((s) => s.localMachineId);
  const sshAliasRev = useWorkbenchStore((s) => s.sshAliasRev);

  const parsed = useMemo(() => parseMdFileHref(rawHref, ctx.worktreePath), [rawHref, ctx.worktreePath]);
  const machines = ctx.machines ?? EMPTY_MACHINES;
  const scheme: IDEScheme = preferredIDE || ctx.ideScheme || 'cursor';
  const effectiveLocalId = resolveEffectiveLocalMachineId(machines, ctx.localMachineId || storedLocalId);

  const customAlias = useMemo(() => {
    void sshAliasRev;
    return ctx.machineId ? getCustomSshAlias(ctx.machineId) : '';
  }, [ctx.machineId, sshAliasRev]);

  const jump = useMemo(() => {
    if (!parsed || parsed.isRelative) return null;
    return buildFileIDEJump({
      scheme,
      path: parsed.path,
      line: parsed.line,
      column: parsed.column,
      machineId: ctx.machineId,
      localMachineId: effectiveLocalId,
      machines,
      recommendedSshHost: ctx.recommendedSshHost,
      customSshHost: customAlias,
    });
  }, [parsed, scheme, ctx.machineId, effectiveLocalId, machines, ctx.recommendedSshHost, customAlias]);

  if (!parsed) {
    return <>{children}</>;
  }

  const lineSuffix = formatParsedLineSuffix(parsed.line, parsed.endLine, parsed.column);
  const copyTarget = `${parsed.path}${lineSuffix}`;
  const shortName = fileBaseName(parsed.path);
  const ideName = ideLabel(scheme);
  const title = jump
    ? jump.mode === 'ssh'
      ? `在 ${ideName} 打开 ${jump.resolvedPath}${lineSuffix} · SSH · ${jump.sshHost || jump.targetMachine} · Option+点击复制路径`
      : `在 ${ideName} 打开 ${jump.resolvedPath}${lineSuffix} · Option+点击复制路径`
    : `点击复制路径 ${copyTarget}`;

  return (
    <a
      id={id}
      href={jump?.uri ?? rawHref}
      className="md-link md-file-link"
      title={title}
      onClick={(e) => {
        e.preventDefault();
        if (e.altKey || !jump) {
          void copyTextToClipboard(copyTarget).then((ok) => {
            if (ok) toast(`已复制路径：${copyTarget}`);
            else toast('复制路径失败', 'err');
          });
          return;
        }
        launchIDE(jump.uri);
        const shortLoc = parsed.line ? `${shortName}:${parsed.line}` : shortName;
        toast(
          jump.mode === 'ssh'
            ? `已在 ${ideName} 打开 ${shortLoc}（SSH · ${jump.sshHost || jump.targetMachine}）`
            : `已在 ${ideName} 打开 ${shortLoc}`,
        );
      }}
    >
      {children}
    </a>
  );
}

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
            const scope = e.currentTarget.closest('.md');
            const target = findFootnoteTargetInContainer<HTMLElement>(scope, safeHref);
            target?.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
          }}
        >
          {children}
        </a>
      );
    }
    if (kind === 'external') {
      const isMailto = /^mailto:/i.test(safeHref);
      return (
        <a
          id={id}
          href={safeHref}
          className="md-link"
          {...(isMailto ? { rel: 'noopener noreferrer' } : { target: '_blank', rel: 'noopener noreferrer' })}
        >
          {children}
        </a>
      );
    }
    if (kind === 'file') {
      return (
        <MdFileLink id={id} rawHref={safeHref}>
          {children}
        </MdFileLink>
      );
    }
    return <>{children}</>;
  },
  img({ src, alt, title }) {
    if (!src) return alt ? <span>{alt}</span> : null;
    return <img src={src} alt={alt ?? ''} title={title} loading="lazy" />;
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
