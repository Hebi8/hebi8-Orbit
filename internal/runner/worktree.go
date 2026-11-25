package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Hebi8/hebi8-Orbit/internal/gitops"
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

	// Plain-folder or explicit in-place location: run directly inside RepoPath (zero worktree, zero shadow Git).
	if dispatch.IsPlainFolder || dispatch.Isolation == protocol.IsolationInPlace {
		repoPath := protocol.ExpandPath(strings.TrimSpace(dispatch.RepoPath))
		if repoPath == "" {
			return "", "", fmt.Errorf("in-place folder execution requires repoPath")
		}
		absDir, err := filepath.Abs(repoPath)
		if err != nil {
			return "", "", fmt.Errorf("resolve in-place dir: %w", err)
		}
		fi, err := os.Stat(absDir)
		if err != nil || !fi.IsDir() {
			return "", "", fmt.Errorf("in-place directory does not exist: %s", absDir)
		}
		return absDir, "", nil
	}

	repoPath := protocol.ExpandPath(strings.TrimSpace(dispatch.RepoPath))
	rawMirror := strings.TrimSpace(dispatch.MirrorBarePath)
	mirrorPath := ""
	if rawMirror != "" && !strings.HasPrefix(rawMirror, "orbit-hub://") {
		mirrorPath = protocol.ExpandPath(rawMirror)
	}

	// If local repoPath is omitted, we can still work directly from mirrorPath if present
	gitSource := repoPath
	if gitSource == "" {
		gitSource = mirrorPath
	}

	// If gitSource defines an origin remote and dispatch declares RepoIdentity, verify they match
	// using the shared credential-stripping identity sanitizer (never log raw remote URLs!).
	if wantID := strings.TrimSpace(dispatch.RepoIdentity); wantID != "" {
		if srcOrigin, err := runGitCmdTrim(ctx, gitSource, "remote", "get-url", "origin"); err == nil && srcOrigin != "" {
			normSrc := canonicalRepoIdentity(srcOrigin)
			normWant := canonicalRepoIdentity(wantID)
			if normSrc != normWant && !strings.HasSuffix(normSrc, "/"+normWant) && !strings.HasSuffix(normWant, "/"+normSrc) {
				return "", "", fmt.Errorf("ERR_REPO_IDENTITY_MISMATCH: source repo identity %q != project repoIdentity %q", normSrc, normWant)
			}
		}
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

	// B14: If absWorktree already exists, strictly verify repository identity, common git dir,
	// HEAD SHA, and cleanliness/tree-equivalence BEFORE reusing. NEVER silently reuse a mismatched directory!
	if _, err := os.Stat(absWorktree); err == nil {
		if err := validateExistingWorktree(ctx, absWorktree, gitSource, baseSHA, dispatch); err != nil {
			return "", "", err
		}
		sameSessionReuse := strings.TrimSpace(dispatch.ProviderSessionID) != "" &&
			strings.TrimSpace(dispatch.ExpectedSessionCWD) != "" &&
			canonicalPath(dispatch.ExpectedSessionCWD) == canonicalPath(absWorktree)
		if sameSessionReuse && dispatch.InputDelivery != nil && dispatch.InputDelivery.HeadSHA != "" {
			matchesTree, err := worktreeMatchesCommitTree(ctx, absWorktree, gitSource, dispatch.InputDelivery.HeadSHA)
			if err != nil {
				return "", "", fmt.Errorf("ERR_WORKTREE_TREE_VERIFY: %w", err)
			}
			if !matchesTree {
				// B14: Worktree drifted after InputDelivery was frozen! Preserve the original directory
				// untouched and create a clean worktree checked out at exact baseSHA (which also forces
				// a fresh ACP session since CWD changed).
				freshPath := filepath.Join(worktreeRoot, dispatch.TaskID, dispatch.AttemptID)
				absFresh, err := filepath.Abs(protocol.ExpandPath(freshPath))
				if err != nil {
					return "", "", fmt.Errorf("resolve fresh worktree path: %w", err)
				}
				if canonicalPath(absFresh) == canonicalPath(absWorktree) {
					absFresh = absFresh + "-clean"
				}
				if err := os.MkdirAll(filepath.Dir(absFresh), 0o755); err != nil {
					return "", "", fmt.Errorf("mkdir fresh worktree parent: %w", err)
				}
				if _, err := runGitCmd(ctx, gitSource, "worktree", "add", "--detach", absFresh, baseSHA); err != nil {
					return "", "", fmt.Errorf("git worktree add fresh %s at %s: %w", absFresh, baseSHA, err)
				}
				return absFresh, baseSHA, nil
			}
		}
		return absWorktree, baseSHA, nil
	} else if !os.IsNotExist(err) {
		return "", "", fmt.Errorf("stat worktree %s: %w", absWorktree, err)
	}

	if err := os.MkdirAll(filepath.Dir(absWorktree), 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir worktree parent: %w", err)
	}
	if _, err := runGitCmd(ctx, gitSource, "worktree", "add", "--detach", absWorktree, baseSHA); err != nil {
		return "", "", fmt.Errorf("git worktree add %s at %s: %w", absWorktree, baseSHA, err)
	}

	return absWorktree, baseSHA, nil
}

func validateExistingWorktree(ctx context.Context, absWorktree, gitSource, baseSHA string, dispatch protocol.AttemptDispatchFrame) error {
	top, err := runGitCmdTrim(ctx, absWorktree, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("ERR_WORKTREE_INVALID: existing path %s is not a valid git worktree: %w", absWorktree, err)
	}
	if canonicalPath(top) != canonicalPath(absWorktree) {
		return fmt.Errorf("ERR_WORKTREE_TOPLEVEL_MISMATCH: path %s belongs to parent repo %s", absWorktree, top)
	}

	wtCommonRaw, err := runGitCmdTrim(ctx, absWorktree, "rev-parse", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("ERR_WORKTREE_COMMON_DIR: %w", err)
	}
	srcCommonRaw, err := runGitCmdTrim(ctx, gitSource, "rev-parse", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("ERR_SOURCE_COMMON_DIR: %w", err)
	}
	wtCommon := resolveGitPath(absWorktree, wtCommonRaw)
	srcCommon := resolveGitPath(gitSource, srcCommonRaw)
	if wtCommon != srcCommon {
		return fmt.Errorf("ERR_WORKTREE_REPO_MISMATCH: worktree %s common dir %s != expected repo %s", absWorktree, wtCommon, srcCommon)
	}

	// If both worktree and dispatch carry an origin URL / repoIdentity, ensure no conflict
	// using the shared credential-stripping identity sanitizer (never log raw remote URLs!).
	if wtOrigin, err := runGitCmdTrim(ctx, absWorktree, "remote", "get-url", "origin"); err == nil && wtOrigin != "" {
		normWT := canonicalRepoIdentity(wtOrigin)
		if srcOrigin, err := runGitCmdTrim(ctx, gitSource, "remote", "get-url", "origin"); err == nil && srcOrigin != "" {
			normSrc := canonicalRepoIdentity(srcOrigin)
			if normWT != normSrc {
				return fmt.Errorf("ERR_WORKTREE_IDENTITY_MISMATCH: origin %q != source %q", normWT, normSrc)
			}
		}
		if wantID := strings.TrimSpace(dispatch.RepoIdentity); wantID != "" {
			normWant := canonicalRepoIdentity(wantID)
			if normWT != normWant && !strings.HasSuffix(normWT, "/"+normWant) && !strings.HasSuffix(normWant, "/"+normWT) {
				return fmt.Errorf("ERR_WORKTREE_IDENTITY_MISMATCH: worktree origin %q != project repoIdentity %q", normWT, normWant)
			}
		}
	}

	wtHEAD, err := runGitCmdTrim(ctx, absWorktree, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return fmt.Errorf("ERR_WORKTREE_HEAD_INVALID: %w", err)
	}

	sameSessionReuse := strings.TrimSpace(dispatch.ProviderSessionID) != "" &&
		strings.TrimSpace(dispatch.ExpectedSessionCWD) != "" &&
		canonicalPath(dispatch.ExpectedSessionCWD) == canonicalPath(absWorktree)

	headMatches := wtHEAD == baseSHA
	if !headMatches && sameSessionReuse && dispatch.InputDelivery != nil && dispatch.InputDelivery.HeadSHA != "" {
		// PackageCheckpoint commits onto a temporary index without moving worktree HEAD,
		// so on same-session Turn 2..N the worktree HEAD remains an ancestor of InputDelivery.HeadSHA.
		if _, err := runGitCmd(ctx, gitSource, "merge-base", "--is-ancestor", wtHEAD, dispatch.InputDelivery.HeadSHA); err == nil {
			headMatches = true
		}
	}
	if !headMatches {
		return fmt.Errorf("ERR_WORKTREE_SHA_MISMATCH: existing worktree %s HEAD=%s does not match expected base=%s", absWorktree, wtHEAD, baseSHA)
	}

	if !sameSessionReuse {
		statusOut, err := runGitCmdTrim(ctx, absWorktree, "status", "--porcelain")
		if err != nil {
			return fmt.Errorf("ERR_WORKTREE_STATUS: %w", err)
		}
		if statusOut != "" {
			return fmt.Errorf("ERR_WORKTREE_DIRTY: existing worktree %s has uncommitted modifications for a fresh attempt", absWorktree)
		}
	}
	return nil
}

func canonicalPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(real)
	}
	return filepath.Clean(abs)
}

func resolveGitPath(baseDir, raw string) string {
	if filepath.IsAbs(raw) {
		return canonicalPath(raw)
	}
	return canonicalPath(filepath.Join(baseDir, raw))
}

func normalizeRepoIdentity(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, ".git")
	s = strings.TrimSuffix(s, "/")
	if strings.HasPrefix(s, "git@") {
		s = strings.TrimPrefix(s, "git@")
		s = strings.Replace(s, ":", "/", 1)
	}
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "ssh://")
	return strings.ToLower(s)
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

func canonicalRepoIdentity(raw string) string {
	if sanitized := gitops.SanitizeGitRemoteIdentity(raw); sanitized != "" {
		return strings.ToLower(sanitized)
	}
	return normalizeRepoIdentity(raw)
}

func runGitCmdTrim(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := runGitCmd(ctx, dir, args...)
	return strings.TrimSpace(out), err
}

// worktreeMatchesCommitTree compares absWorktree's current packable working tree (computed via
// an isolated temporary GIT_INDEX_FILE, zero mutation to real index/files) against commitSHA^{tree} (B14).
func worktreeMatchesCommitTree(ctx context.Context, absWorktree, gitSource, commitSHA string) (bool, error) {
	expectedTree, err := runGitCmdTrim(ctx, gitSource, "rev-parse", "--verify", commitSHA+"^{tree}")
	if err != nil {
		return false, err
	}
	tmpDir, err := os.MkdirTemp("", "orbit-tree-*")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(tmpDir)

	tmpIndex := filepath.Join(tmpDir, "index")
	env := append(os.Environ(), "GIT_INDEX_FILE="+tmpIndex)

	addCmd := exec.CommandContext(ctx, "git", "-c", "core.quotePath=false", "add", "-A")
	addCmd.Dir = absWorktree
	addCmd.Env = env
	var addErr bytes.Buffer
	addCmd.Stderr = &addErr
	if err := addCmd.Run(); err != nil {
		return false, fmt.Errorf("temp index add -A: %w: %s", err, strings.TrimSpace(addErr.String()))
	}

	treeCmd := exec.CommandContext(ctx, "git", "write-tree")
	treeCmd.Dir = absWorktree
	treeCmd.Env = env
	var treeOut, treeErr bytes.Buffer
	treeCmd.Stdout = &treeOut
	treeCmd.Stderr = &treeErr
	if err := treeCmd.Run(); err != nil {
		return false, fmt.Errorf("temp index write-tree: %w: %s", err, strings.TrimSpace(treeErr.String()))
	}
	actualTree := strings.TrimSpace(treeOut.String())
	return actualTree == expectedTree, nil
}
