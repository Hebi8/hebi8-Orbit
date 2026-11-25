import { GitBranch } from 'lucide-react';
import type { LocationRuntimeState } from '../generated/protocol';

/**
 * Zsh-style inline Git status indicator:
 * - Clean Git: `⎇ main`
 * - Dirty Git: `⎇ main *`
 * - Ahead/Behind: `↑2` / `↓1`
 * - Non-Git plain folder: renders nothing (zero `普通文件夹` or `干净` badge clutter).
 */
export function GitZshBadge({
  runtime,
  fallbackBranch,
  isGit,
}: {
  runtime?: LocationRuntimeState | null;
  fallbackBranch?: string;
  isGit?: boolean;
}) {
  const git = runtime ? runtime.isGit : !!isGit;
  if (!git) return null;
  const branch = runtime?.branch || fallbackBranch || 'main';
  const dirty = !!runtime?.dirty;
  const ahead = runtime?.ahead ?? 0;
  const behind = runtime?.behind ?? 0;

  return (
    <span className="git-zsh mono" title={dirty ? `${branch} · 有未提交改动` : branch}>
      <GitBranch size={11} strokeWidth={1.8} />
      <span>{branch}</span>
      {dirty && <span className="git-zsh-dirty">*</span>}
      {ahead > 0 && <span className="git-zsh-up">↑{ahead}</span>}
      {behind > 0 && <span className="git-zsh-dn">↓{behind}</span>}
    </span>
  );
}
