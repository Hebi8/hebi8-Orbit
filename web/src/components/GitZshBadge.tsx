import { GitBranch } from 'lucide-react';
import type { LocationRuntimeState } from '../generated/protocol';

/**
 * Clean inline Git branch indicator:
 * - Git repo: `⎇ main` (+ `↑2` / `↓1` when ahead/behind)
 * - Non-Git plain folder: renders nothing.
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
  const ahead = runtime?.ahead ?? 0;
  const behind = runtime?.behind ?? 0;

  return (
    <span className="git-zsh mono" title={branch}>
      <GitBranch size={11} strokeWidth={1.8} />
      <span>{branch}</span>
      {ahead > 0 && <span className="git-zsh-up">↑{ahead}</span>}
      {behind > 0 && <span className="git-zsh-dn">↓{behind}</span>}
    </span>
  );
}
