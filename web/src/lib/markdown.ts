export type MdHrefKind = 'internal' | 'external' | 'unsafe';

/** Classifies a Markdown link href into Orbit internal hash navigation, external http(s), or unsafe/non-linkable. */
export function classifyMdHref(href?: string | null): MdHrefKind {
  const s = typeof href === 'string' ? href.trim() : '';
  if (!s) return 'unsafe';
  if (s.startsWith('#/')) return 'internal';
  if (/^https?:\/\//i.test(s)) return 'external';
  return 'unsafe';
}

/** URL sanitizer passed to react-markdown: permits Orbit `#/...` routes for href and `https?://` only. */
export function safeMdUrlTransform(url: string, key: string): string {
  const trimmed = url.trim();
  if (key === 'href') {
    return classifyMdHref(trimmed) === 'unsafe' ? '' : trimmed;
  }
  if (/^https?:\/\//i.test(trimmed)) return trimmed;
  return '';
}
