export type MdHrefKind = 'internal' | 'external' | 'footnote' | 'unsafe';

/**
 * Matches GFM footnote fragments emitted by remark-gfm/mdast-util-to-hast, including ASCII identifiers,
 * percent-encoded UTF-8 sequences (e.g. `#user-content-fn-%E8%AF%B4%E6%98%8E`), and safe Unicode labels,
 * while rejecting malformed percent escapes, whitespace, quotes, angle brackets, or query/hash injections.
 */
const FOOTNOTE_FRAGMENT_RE =
  /^#(?:user-content-)?fn(?:ref)?-(?:[A-Za-z0-9_.:-]|%[0-9A-Fa-f]{2}|[^\x00-\x20\x7f"'<>\\`#?%])+$/;

/** Reports whether `href` is a safe GFM footnote anchor fragment (scrolled in-container, never routed). */
export function isFootnoteHref(href?: string | null): boolean {
  const s = typeof href === 'string' ? href.trim() : '';
  return FOOTNOTE_FRAGMENT_RE.test(s);
}

/** Classifies a Markdown link href into Orbit internal hash navigation, in-container footnote anchor, external http(s), or unsafe. */
export function classifyMdHref(href?: string | null): MdHrefKind {
  const s = typeof href === 'string' ? href.trim() : '';
  if (!s) return 'unsafe';
  if (s.startsWith('#/')) return 'internal';
  if (FOOTNOTE_FRAGMENT_RE.test(s)) return 'footnote';
  if (/^https?:\/\//i.test(s)) return 'external';
  return 'unsafe';
}

/** URL sanitizer passed to react-markdown: permits Orbit `#/...` routes & GFM footnote anchors for href, and `https?://` only. */
export function safeMdUrlTransform(url: string, key: string): string {
  const trimmed = url.trim();
  if (key === 'href') {
    return classifyMdHref(trimmed) === 'unsafe' ? '' : trimmed;
  }
  if (/^https?:\/\//i.test(trimmed)) return trimmed;
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
