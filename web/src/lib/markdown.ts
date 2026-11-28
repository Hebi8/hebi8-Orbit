export type MdHrefKind = 'internal' | 'external' | 'footnote' | 'file' | 'unsafe';

export interface ParsedMdFileHref {
  path: string;
  line?: number;
  endLine?: number;
  column?: number;
  rawHref: string;
  isRelative: boolean;
}

/**
 * Matches GFM footnote fragments emitted by remark-gfm/mdast-util-to-hast, including ASCII identifiers,
 * percent-encoded UTF-8 sequences (e.g. `#user-content-fn-%E8%AF%B4%E6%98%8E`), and safe Unicode labels,
 * while rejecting malformed percent escapes, whitespace, quotes, angle brackets, or query/hash injections.
 */
const FOOTNOTE_FRAGMENT_RE =
  /^#(?:user-content-)?fn(?:ref)?-(?:[A-Za-z0-9_.:-]|%[0-9A-Fa-f]{2}|[^\x00-\x20\x7f"'<>\\`#?%])+$/;

/** Matches ASCII control chars (including tabs/newlines/NUL) that can obfuscate schemes or inject headers. */
const CONTROL_CHAR_RE = /[\x00-\x1f\x7f]/;

/** Matches dangerous characters that should never appear raw inside Markdown file hrefs. */
const UNSAFE_FILE_HREF_CHAR_RE = /[<>\\"`]/;

/**
 * Matches a bare filename with an extension followed strictly by a positive `:line`, `:line:col`, or `:line-endLine`
 * suffix (e.g. `now.md:105`, `ux_polish.test.ts:105`, `Segments.tsx:10:12`, `.env.local:5`), preventing URI schemes
 * like `javascript:123` or `tel:12345` from ever matching.
 */
const BARE_FILE_WITH_LINE_RE =
  /^\.?[^\s/:?#]*[^\s/:?#.]\.[A-Za-z0-9_-]{1,16}:[1-9]\d*(?::[1-9]\d*|-[1-9]\d*)?$/;

const MAILTO_RECIPIENT_RE = /^[^\s@/?#"'<>\\`]+@[^\s@/?#"'<>\\`]+$/;
const LINE_FRAGMENT_RE = /^L([1-9]\d*)(?:C([1-9]\d*))?(?:-L?([1-9]\d*)(?:C[1-9]\d*)?)?$/i;
const TRAILING_COLON_LINE_RE = /^(.*?):([1-9]\d*)(?::([1-9]\d*)|-([1-9]\d*))?$/;

function parsePositiveInt(raw?: string): number | undefined {
  if (!raw) return undefined;
  const n = Number.parseInt(raw, 10);
  return Number.isSafeInteger(n) && n > 0 ? n : undefined;
}

function decodePercentSafe(raw: string): string {
  return raw.replace(/(?:%[0-9A-Fa-f]{2})+/g, (seq) => {
    try {
      return decodeURIComponent(seq);
    } catch {
      return seq;
    }
  });
}

function normalizeAbsolutePosix(p: string): string {
  const stack: string[] = [];
  for (const seg of p.split('/')) {
    if (!seg || seg === '.') continue;
    if (seg === '..') {
      if (stack.length > 0) stack.pop();
      continue;
    }
    stack.push(seg);
  }
  return `/${stack.join('/')}`;
}

function normalizeHomePosix(p: string): string {
  const rest = p.slice(2);
  const stack: string[] = [];
  for (const seg of rest.split('/')) {
    if (!seg || seg === '.') continue;
    if (seg === '..') {
      if (stack.length > 0 && stack[stack.length - 1] !== '..') stack.pop();
      else stack.push('..');
      continue;
    }
    stack.push(seg);
  }
  return stack.length > 0 ? `~/${stack.join('/')}` : '~';
}

function isSafeMailtoHref(s: string): boolean {
  if (!/^mailto:/i.test(s)) return false;
  const body = s.slice(7);
  if (!body || /[\x00-\x20\x7f"'<>\\`]/.test(body)) return false;
  const qIdx = body.indexOf('?');
  const addrPart = qIdx >= 0 ? body.slice(0, qIdx) : body;
  const queryPart = qIdx >= 0 ? body.slice(qIdx + 1) : '';
  if (!addrPart) {
    return queryPart.length > 0;
  }
  const recipients = addrPart.split(',');
  return recipients.length > 0 && recipients.every((r) => MAILTO_RECIPIENT_RE.test(r));
}

/**
 * Parses a Markdown file link href (`file:///...`, `/abs/path`, `~/path`, or relative path) into a normalized
 * filesystem path plus optional line/endLine/column metadata. When a relative path is given alongside an absolute
 * `baseDir` (e.g. a session worktree path), resolves and normalizes it into a complete path without duplicate slashes.
 * Returns `null` for non-file URLs, dangerous schemes, or malformed paths.
 */
export function parseMdFileHref(href?: string | null, baseDir?: string | null): ParsedMdFileHref | null {
  const rawHref = typeof href === 'string' ? href.trim() : '';
  if (!rawHref || CONTROL_CHAR_RE.test(rawHref) || UNSAFE_FILE_HREF_CHAR_RE.test(rawHref)) {
    return null;
  }
  if (rawHref.startsWith('#') || rawHref.startsWith('?') || rawHref.startsWith('//') || rawHref.startsWith(':')) {
    return null;
  }

  let isFileUri = false;
  let working = rawHref;

  if (/^file:/i.test(working)) {
    const fileMatch = /^file:\/\/(?:localhost)?(\/.*)$/i.exec(working);
    if (!fileMatch) return null;
    working = fileMatch[1];
    isFileUri = true;
  } else {
    const beforeHash = working.split('#')[0];
    if (beforeHash.includes('?')) return null;
    const colonIdx = beforeHash.indexOf(':');
    const slashIdx = beforeHash.indexOf('/');
    if (colonIdx >= 0 && (slashIdx < 0 || colonIdx < slashIdx)) {
      if (!BARE_FILE_WITH_LINE_RE.test(beforeHash)) {
        return null;
      }
    }
  }

  const hashIdx = working.indexOf('#');
  let pathPart = hashIdx >= 0 ? working.slice(0, hashIdx) : working;
  const fragment = hashIdx >= 0 ? working.slice(hashIdx + 1) : '';

  if (isFileUri) {
    const qIdx = pathPart.indexOf('?');
    if (qIdx >= 0) pathPart = pathPart.slice(0, qIdx);
  }

  let line: number | undefined;
  let endLine: number | undefined;
  let column: number | undefined;

  if (fragment) {
    const fragMatch = LINE_FRAGMENT_RE.exec(fragment);
    if (fragMatch) {
      line = parsePositiveInt(fragMatch[1]);
      column = parsePositiveInt(fragMatch[2]);
      endLine = parsePositiveInt(fragMatch[3]);
    }
  }

  if (line === undefined) {
    const colonMatch = TRAILING_COLON_LINE_RE.exec(pathPart);
    if (colonMatch) {
      const prefix = colonMatch[1];
      if (prefix && prefix !== '/' && prefix !== '~' && prefix !== '~/') {
        const parsedLine = parsePositiveInt(colonMatch[2]);
        if (parsedLine !== undefined) {
          pathPart = prefix;
          line = parsedLine;
          column = parsePositiveInt(colonMatch[3]);
          endLine = parsePositiveInt(colonMatch[4]);
        }
      }
    }
  }

  const decoded = decodePercentSafe(pathPart);
  if (!decoded || CONTROL_CHAR_RE.test(decoded) || UNSAFE_FILE_HREF_CHAR_RE.test(decoded)) {
    return null;
  }
  if (decoded === '/' || decoded === '.' || decoded === '..' || decoded === '~' || decoded === '~/') {
    return null;
  }

  let finalPath = decoded;
  let isRelative = false;

  if (decoded.startsWith('/')) {
    const norm = normalizeAbsolutePosix(decoded);
    if (norm === '/') return null;
    finalPath = norm;
  } else if (decoded.startsWith('~/')) {
    const norm = normalizeHomePosix(decoded);
    if (norm === '~') return null;
    finalPath = norm;
  } else {
    const cleanBase = typeof baseDir === 'string' ? baseDir.trim() : '';
    if (cleanBase.startsWith('/')) {
      const joined = normalizeAbsolutePosix(`${cleanBase.replace(/\/+$/, '')}/${decoded}`);
      if (joined === '/') return null;
      finalPath = joined;
      isRelative = false;
    } else if (cleanBase.startsWith('~/')) {
      const joined = normalizeHomePosix(`${cleanBase.replace(/\/+$/, '')}/${decoded}`);
      if (joined === '~') return null;
      finalPath = joined;
      isRelative = false;
    } else {
      finalPath = decoded;
      isRelative = true;
    }
  }

  const result: ParsedMdFileHref = {
    path: finalPath,
    rawHref,
    isRelative,
  };
  if (line !== undefined) result.line = line;
  if (endLine !== undefined) result.endLine = endLine;
  if (column !== undefined) result.column = column;
  return result;
}

/** Reports whether `href` is a safe GFM footnote anchor fragment (scrolled in-container, never routed). */
export function isFootnoteHref(href?: string | null): boolean {
  const s = typeof href === 'string' ? href.trim() : '';
  return FOOTNOTE_FRAGMENT_RE.test(s);
}

/**
 * Classifies a Markdown link href into Orbit internal hash navigation, in-container footnote anchor,
 * external http(s)/mailto, local/remote file reference, or unsafe (strictly blocking javascript:/vbscript:/data: etc.).
 */
export function classifyMdHref(href?: string | null): MdHrefKind {
  const s = typeof href === 'string' ? href.trim() : '';
  if (!s || CONTROL_CHAR_RE.test(s)) return 'unsafe';
  if (s.startsWith('#/')) return 'internal';
  if (FOOTNOTE_FRAGMENT_RE.test(s)) return 'footnote';
  if (s.startsWith('#')) return 'unsafe';
  if (/^https?:\/\//i.test(s)) return 'external';
  if (isSafeMailtoHref(s)) return 'external';
  if (parseMdFileHref(s) !== null) return 'file';
  return 'unsafe';
}

/** URL sanitizer passed to react-markdown: permits all non-unsafe hrefs, and `https?://` only for non-href attrs (e.g. `src`). */
export function safeMdUrlTransform(url: string, key: string): string {
  const trimmed = url.trim();
  if (key === 'href') {
    return classifyMdHref(trimmed) === 'unsafe' ? '' : trimmed;
  }
  if (!CONTROL_CHAR_RE.test(trimmed) && /^https?:\/\//i.test(trimmed)) return trimmed;
  return '';
}

/**
 * Resolves candidate element IDs for a GFM footnote anchor href (both raw fragment and percent-decoded form,
 * e.g. `user-content-fn-%E8%AF%B4%E6%98%8E` and `user-content-fn-说明`).
 */
export function resolveFootnoteTargetIds(href?: string | null): string[] {
  const s = typeof href === 'string' ? href.trim() : '';
  if (!FOOTNOTE_FRAGMENT_RE.test(s)) return [];
  const rawId = s.slice(1);
  if (!rawId) return [];
  const out = [rawId];
  try {
    const decoded = decodeURIComponent(rawId);
    if (decoded && decoded !== rawId) {
      out.push(decoded);
    }
  } catch {
    // Keep rawId only if percent-decoding fails
  }
  return out;
}

/** Finds the footnote target element within a `.md` container matching either raw or decoded ID (case-insensitive fallback). */
export function findFootnoteTargetInContainer<T extends { id: string }>(
  scope: { querySelectorAll: (selector: string) => ArrayLike<T> } | null | undefined,
  href?: string | null,
): T | undefined {
  if (!scope) return undefined;
  const candidates = resolveFootnoteTargetIds(href);
  if (candidates.length === 0) return undefined;
  const nodes = Array.from(scope.querySelectorAll('[id]'));
  for (const cid of candidates) {
    const exact = nodes.find((el) => el.id === cid);
    if (exact) return exact;
  }
  const lowerCandidates = candidates.map((c) => c.toLowerCase());
  return nodes.find((el) => lowerCandidates.includes(el.id.toLowerCase()));
}
