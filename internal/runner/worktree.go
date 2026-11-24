package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

// PrepareWorktree creates (or safely reuses) an isolated worktree for an attempt and returns
// (absWorktreePath, fixedReviewBaseSHA, error).
// For virtual _scratch tasks (no git repository), creates an isolated scratch directory.
func PrepareWorktree(ctx context.Context, dispatch protocol.AttemptDispatchFrame, dataDir string) (string, string, error) {
	worktreeRoot := strings.TrimSpace(dispatch.WorktreeRoot)
	if worktreeRoot == "" {
		if dispatch.ProjectID == protocol.ScratchProjectID {
			worktreeRoot = filepath.Join(dataDir, "scratch")
		} else {
			worktreeRoot = filepath.Join(dataDir, "worktrees", dispatch.ProjectID)
		}
	}
	worktreePath := strings.TrimSpace(dispatch.WorktreePath)
	if worktreePath == "" {
		worktreePath = filepath.Join(worktreeRoot, dispatch.TaskID, dispatch.AttemptID)
	}
	absWorktree, err := filepath.Abs(protocol.ExpandPath(worktreePath))
	if err != nil {
		return "", "", fmt.Errorf("resolve worktree path: %w", err)
	}

	// Virtual _scratch project or repo-less task: create clean scratch dir
	if dispatch.ProjectID == protocol.ScratchProjectID || (strings.TrimSpace(dispatch.RepoPath) == "" && strings.TrimSpace(dispatch.MirrorBarePath) == "") {
		if err := os.MkdirAll(absWorktree, 0o755); err != nil {
			return "", "", fmt.Errorf("mkdir scratch dir %s: %w", absWorktree, err)
		}
		return absWorktree, "", nil
	}

	repoPath := protocol.ExpandPath(strings.TrimSpace(dispatch.RepoPath))
	mirrorPath := protocol.ExpandPath(strings.TrimSpace(dispatch.MirrorBarePath))

	// If local repoPath is omitted, we can still work directly from mirrorPath if present
	gitSource := repoPath
	if gitSource == "" {
		gitSource = mirrorPath
	}

	// Determine fixed review base SHA
	var baseSHA string
	switch {
	case dispatch.InputDelivery != nil && dispatch.InputDelivery.HeadSHA != "":
		targetSHA := dispatch.InputDelivery.HeadSHA
		if mirrorPath != "" && gitSource != mirrorPath {
			remoteRef := fmt.Sprintf("+%s:refs/orbit/remote/%s", dispatch.InputDelivery.GitRef, dispatch.InputDelivery.DeliveryID)
			if _, err := runGitCmd(ctx, gitSource, "fetch", "--no-tags", mirrorPath, remoteRef); err != nil {
				return "", "", fmt.Errorf("fetch input delivery %s: %w", dispatch.InputDelivery.DeliveryID, err)
			}
			fetched, err := runGitCmdTrim(ctx, gitSource, "rev-parse", "--verify", "refs/orbit/remote/"+dispatch.InputDelivery.DeliveryID+"^{commit}")
			if err != nil {
				return "", "", fmt.Errorf("verify fetched delivery %s: %w", dispatch.InputDelivery.DeliveryID, err)
			}
			if fetched != targetSHA {
				return "", "", fmt.Errorf("ERR_DELIVERY_SHA_MISMATCH: fetched %s != expected %s", fetched, targetSHA)
			}
		} else {
			if _, err := runGitCmd(ctx, gitSource, "cat-file", "-e", targetSHA+"^{commit}"); err != nil {
				return "", "", fmt.Errorf("input delivery commit %s missing: %w", targetSHA, err)
			}
		}
		baseSHA = targetSHA

	case strings.TrimSpace(dispatch.BaseSHA) != "":
		want := strings.TrimSpace(dispatch.BaseSHA)
		if _, err := runGitCmd(ctx, gitSource, "cat-file", "-e", want+"^{commit}"); err != nil {
			if mirrorPath != "" && gitSource != mirrorPath {
				_, _ = runGitCmd(ctx, gitSource, "fetch", "--no-tags", mirrorPath, "refs/orbit/deliveries/*:refs/orbit/remote/*")
			}
			if _, err2 := runGitCmd(ctx, gitSource, "cat-file", "-e", want+"^{commit}"); err2 != nil {
				return "", "", fmt.Errorf("base commit %s not found in repo: %w", want, err2)
			}
		}
		baseSHA = want

	default:
		sha, err := runGitCmdTrim(ctx, gitSource, "rev-parse", "--verify", "HEAD^{commit}")
		if err != nil {
			return "", "", fmt.Errorf("resolve HEAD in %s: %w", gitSource, err)
		}
		baseSHA = sha
	}

	// If worktree already exists (e.g. same-worktree session reuse or attempt retry), verify & reuse
	if _, err := os.Stat(filepath.Join(absWorktree, ".git")); err == nil {
		if _, err := runGitCmdTrim(ctx, absWorktree, "rev-parse", "--verify", "HEAD^{commit}"); err == nil {
			return absWorktree, baseSHA, nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(absWorktree), 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir worktree parent: %w", err)
	}
	if _, err := runGitCmd(ctx, gitSource, "worktree", "add", "--detach", absWorktree, baseSHA); err != nil {
		return "", "", fmt.Errorf("git worktree add %s at %s: %w", absWorktree, baseSHA, err)
	}

	return absWorktree, baseSHA, nil
}

func runGitCmd(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func runGitCmdTrim(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := runGitCmd(ctx, dir, args...)
	return strings.TrimSpace(out), err
}
