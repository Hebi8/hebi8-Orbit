export type MdHrefKind = 'internal' | 'external' | 'footnote' | 'unsafe';

const FOOTNOTE_FRAGMENT_RE = /^#(?:user-content-)?fn(?:ref)?-[A-Za-z0-9_.:-]+$/;

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
