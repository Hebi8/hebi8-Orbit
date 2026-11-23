import {
  computeNewLineNumber,
  computeOldLineNumber,
  isDelete,
  isInsert,
  isNormal,
  type ChangeData,
  type HunkData,
  type HunkTokens,
  type TokenNode,
} from 'react-diff-view';
import { createHighlighterCore, type HighlighterCore } from 'shiki/core';
import { createJavaScriptRegexEngine } from 'shiki/engine/javascript';

const LANG_IMPORTS = {
  typescript: () => import('shiki/dist/langs/typescript.mjs'),
  tsx: () => import('shiki/dist/langs/tsx.mjs'),
  javascript: () => import('shiki/dist/langs/javascript.mjs'),
  jsx: () => import('shiki/dist/langs/jsx.mjs'),
  go: () => import('shiki/dist/langs/go.mjs'),
  json: () => import('shiki/dist/langs/json.mjs'),
  yaml: () => import('shiki/dist/langs/yaml.mjs'),
  xml: () => import('shiki/dist/langs/xml.mjs'),
  html: () => import('shiki/dist/langs/html.mjs'),
  css: () => import('shiki/dist/langs/css.mjs'),
  bash: () => import('shiki/dist/langs/bash.mjs'),
  sql: () => import('shiki/dist/langs/sql.mjs'),
  markdown: () => import('shiki/dist/langs/markdown.mjs'),
  python: () => import('shiki/dist/langs/python.mjs'),
  rust: () => import('shiki/dist/langs/rust.mjs'),
} as const;

type SupportedLang = keyof typeof LANG_IMPORTS;

let highlighterPromise: Promise<HighlighterCore> | null = null;
const loadedLangs = new Set<SupportedLang>();

function getHighlighter(): Promise<HighlighterCore> {
  if (!highlighterPromise) {
    highlighterPromise = createHighlighterCore({
      themes: [
        import('shiki/dist/themes/github-dark-default.mjs'),
        import('shiki/dist/themes/github-light-default.mjs'),
      ],
      langs: [],
      engine: createJavaScriptRegexEngine(),
    });
  }
  return highlighterPromise;
}

export function detectLanguage(filePath: string): SupportedLang | 'text' {
  const lower = filePath.toLowerCase();
  const ext = lower.includes('.') ? lower.slice(lower.lastIndexOf('.') + 1) : '';
  switch (ext) {
    case 'ts':
    case 'mts':
    case 'cts':
      return 'typescript';
    case 'tsx':
      return 'tsx';
    case 'js':
    case 'mjs':
    case 'cjs':
      return 'javascript';
    case 'jsx':
      return 'jsx';
    case 'go':
      return 'go';
    case 'json':
      return 'json';
    case 'yaml':
    case 'yml':
      return 'yaml';
    case 'xml':
    case 'svg':
      return 'xml';
    case 'html':
    case 'htm':
      return 'html';
    case 'css':
      return 'css';
    case 'sh':
    case 'bash':
    case 'zsh':
      return 'bash';
    case 'sql':
      return 'sql';
    case 'md':
    case 'markdown':
      return 'markdown';
    case 'py':
      return 'python';
    case 'rs':
      return 'rust';
    default:
      return 'text';
  }
}

function buildSideLines(changes: ChangeData[], side: 'old' | 'new'): string[] {
  if (changes.length === 0) return [];
  const lineFn = side === 'old' ? computeOldLineNumber : computeNewLineNumber;
  let maxLine = 0;
  const byLine = new Map<number, string>();
  for (const ch of changes) {
    const ln = lineFn(ch);
    if (ln > 0) {
      byLine.set(ln, ch.content);
      if (ln > maxLine) maxLine = ln;
    }
  }
  const lines = new Array<string>(maxLine).fill('');
  for (const [ln, content] of byLine.entries()) {
    lines[ln - 1] = content;
  }
  return lines;
}

export async function tokenizeHunksWithShiki(
  hunks: HunkData[],
  filePath: string,
  isDark: boolean,
): Promise<HunkTokens | null> {
  const lang = detectLanguage(filePath);
  if (lang === 'text' || hunks.length === 0) {
    return null;
  }

  const oldChanges: ChangeData[] = [];
  const newChanges: ChangeData[] = [];
  for (const h of hunks) {
    for (const ch of h.changes) {
      if (isNormal(ch)) {
        oldChanges.push(ch);
        newChanges.push(ch);
      } else if (isDelete(ch)) {
        oldChanges.push(ch);
      } else if (isInsert(ch)) {
        newChanges.push(ch);
      }
    }
  }

  const oldLines = buildSideLines(oldChanges, 'old');
  const newLines = buildSideLines(newChanges, 'new');

  try {
    const highlighter = await getHighlighter();
    if (!loadedLangs.has(lang)) {
      await highlighter.loadLanguage(LANG_IMPORTS[lang]());
      loadedLangs.add(lang);
    }
    const theme = isDark ? 'github-dark-default' : 'github-light-default';

    const highlightSide = (lines: string[]): TokenNode[][] => {
      if (lines.length === 0) return [];
      const { tokens } = highlighter.codeToTokens(lines.join('\n'), { lang, theme });
      return tokens.map((lineTokens) => {
        if (lineTokens.length === 0) {
          return [{ type: 'text', value: '' }];
        }
        return lineTokens.map((t) => ({
          type: 'shiki',
          value: t.content,
          color: t.color,
        }));
      });
    };

    return {
      old: highlightSide(oldLines),
      new: highlightSide(newLines),
    };
  } catch {
    return null;
  }
}
