package gitops

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

const maxDirEntries = 500

// Directory browsing sentinel errors returned by ResolveAllowedPath and ListSubdirs.
var (
	ErrPathOutsideRoot = errors.New("path is outside allowed root directories")
	ErrDirNotFound     = errors.New("directory does not exist")
	ErrNotADirectory   = errors.New("path is not a directory")
)

func isPathWithinRoots(realTarget string, roots []string) bool {
	realTarget = filepath.Clean(realTarget)
	for _, root := range roots {
		root = filepath.Clean(root)
		if realTarget == root {
			return true
		}
		prefix := root
		if !strings.HasSuffix(prefix, string(os.PathSeparator)) {
			prefix += string(os.PathSeparator)
		}
		if strings.HasPrefix(realTarget, prefix) {
			return true
		}
	}
	return false
}

// isCandidateWithinRoots checks whether a possibly non-existent path sits inside roots by resolving
// its deepest existing ancestor (preventing existence probing outside allowedRoots).
func isCandidateWithinRoots(target string, roots []string) bool {
	cur := filepath.Clean(target)
	for {
		if eval, err := filepath.EvalSymlinks(cur); err == nil {
			return isPathWithinRoots(eval, roots)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return isPathWithinRoots(target, roots)
		}
		cur = parent
	}
}

// ResolveAllowedPath resolves rawPath safely within one of allowedRoots (defaulting to user home).
// It cleans the path, expands ~/, resolves symlinks, and enforces strict prefix containment.
func ResolveAllowedPath(rawPath string, allowedRoots []string) (resolvedPath string, homeRoot string, err error) {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "", "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		realHome = filepath.Clean(home)
	}

	roots := make([]string, 0, len(allowedRoots)+1)
	if len(allowedRoots) == 0 {
		roots = append(roots, realHome)
	} else {
		for _, r := range allowedRoots {
			exp := protocol.ExpandPath(strings.TrimSpace(r))
			if exp == "" {
				continue
			}
			if eval, err := filepath.EvalSymlinks(exp); err == nil {
				roots = append(roots, filepath.Clean(eval))
			} else if abs, err := filepath.Abs(exp); err == nil {
				roots = append(roots, filepath.Clean(abs))
			}
		}
		if len(roots) == 0 {
			roots = append(roots, realHome)
		}
	}

	target := strings.TrimSpace(rawPath)
	switch target {
	case "":
		if isPathWithinRoots(realHome, roots) {
			target = realHome
		} else {
			target = roots[0]
			for _, r := range roots {
				if fi, sErr := os.Stat(r); sErr == nil && fi.IsDir() {
					target = r
					break
				}
			}
		}
	case "~":
		target = realHome
	default:
		target = protocol.ExpandPath(target)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(realHome, target)
	}
	target = filepath.Clean(target)

	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		if os.IsNotExist(err) {
			if !isCandidateWithinRoots(target, roots) {
				return "", realHome, fmt.Errorf("%w: %s", ErrPathOutsideRoot, rawPath)
			}
			return "", realHome, fmt.Errorf("%w: %s", ErrDirNotFound, rawPath)
		}
		return "", realHome, err
	}
	realTarget = filepath.Clean(realTarget)

	if !isPathWithinRoots(realTarget, roots) {
		return "", realHome, fmt.Errorf("%w: %s", ErrPathOutsideRoot, rawPath)
	}
	return realTarget, realHome, nil
}

// ClassifyBrowseError maps directory browse errors to a stable API error code and HTTP status.
func ClassifyBrowseError(err error) (code string, status int) {
	switch {
	case errors.Is(err, ErrPathOutsideRoot):
		return "ERR_PATH_OUTSIDE_ROOT", 403
	case errors.Is(err, ErrDirNotFound) || os.IsNotExist(err):
		return "ERR_DIR_NOT_FOUND", 404
	case errors.Is(err, ErrNotADirectory):
		return "ERR_NOT_A_DIRECTORY", 400
	case os.IsPermission(err):
		return "ERR_PERMISSION_DENIED", 403
	default:
		return "ERR_LIST_DIRS", 400
	}
}

// ListSubdirs safely lists child directories under rawPath (never files), capped at 500 entries,
// hiding dotfiles by default and marking immediate Git repositories.
func ListSubdirs(rawPath string, allowedRoots []string, includeHidden bool) (protocol.ListDirsResponse, error) {
	resolved, realHome, err := ResolveAllowedPath(rawPath, allowedRoots)
	if err != nil {
		return protocol.ListDirsResponse{}, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return protocol.ListDirsResponse{}, fmt.Errorf("%w: %s", ErrDirNotFound, resolved)
		}
		return protocol.ListDirsResponse{}, err
	}
	if !info.IsDir() {
		return protocol.ListDirsResponse{}, fmt.Errorf("%w: %s", ErrNotADirectory, resolved)
	}

	entries, err := os.ReadDir(resolved)
	if err != nil {
		return protocol.ListDirsResponse{}, err
	}

	out := make([]protocol.DirEntryView, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !includeHidden && strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(resolved, name)
		isDir := e.IsDir()
		if !isDir && (e.Type()&os.ModeSymlink) != 0 {
			// Follow symlinks ONLY if they stay inside allowedRoots
			if symTarget, _, symErr := ResolveAllowedPath(full, allowedRoots); symErr == nil {
				if fi, statErr := os.Stat(symTarget); statErr == nil && fi.IsDir() {
					isDir = true
				}
			}
		}
		if !isDir {
			continue
		}
		_, gitErr := os.Stat(filepath.Join(full, ".git"))
		isGit := gitErr == nil
		branch := ""
		if isGit {
			branch = QuickReadGitBranch(full)
		}
		out = append(out, protocol.DirEntryView{
			Name:      name,
			Path:      full,
			IsGitRepo: isGit,
			Branch:    branch,
		})
		if len(out) >= maxDirEntries {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsGitRepo != out[j].IsGitRepo {
			return out[i].IsGitRepo
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})

	parent := ""
	candParent := filepath.Dir(resolved)
	if candParent != resolved {
		if _, _, pErr := ResolveAllowedPath(candParent, allowedRoots); pErr == nil {
			parent = candParent
		}
	}

	return protocol.ListDirsResponse{
		Path:       resolved,
		ParentPath: parent,
		HomePath:   realHome,
		Entries:    out,
	}, nil
}

// CheckGitRepo safely verifies whether rawPath (within allowedRoots) is inside a Git repository
// and returns its canonical root, default branch, repo identity, and display name.
func CheckGitRepo(ctx context.Context, rawPath string, allowedRoots []string) protocol.CheckRepoResponse {
	resolved, _, err := ResolveAllowedPath(rawPath, allowedRoots)
	if err != nil {
		return protocol.CheckRepoResponse{Valid: false, Error: err.Error()}
	}
	fi, err := os.Stat(resolved)
	if err != nil || !fi.IsDir() {
		return protocol.CheckRepoResponse{Valid: false, Error: "directory does not exist"}
	}

	topRaw, err := exec.CommandContext(ctx, "git", "-C", resolved, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return protocol.CheckRepoResponse{Valid: false, Error: "not a Git repository"}
	}
	repoRoot := strings.TrimSpace(string(topRaw))
	if repoRoot == "" {
		return protocol.CheckRepoResponse{Valid: false, Error: "not a Git repository"}
	}
	// Ensure resolved git toplevel is also inside allowedRoots
	realRoot, _, err := ResolveAllowedPath(repoRoot, allowedRoots)
	if err != nil {
		return protocol.CheckRepoResponse{Valid: false, Error: err.Error()}
	}

	branch := "main"
	if brRaw, err := exec.CommandContext(ctx, "git", "-C", realRoot, "symbolic-ref", "--short", "HEAD").Output(); err == nil {
		if b := strings.TrimSpace(string(brRaw)); b != "" {
			branch = b
		}
	}

	displayName := filepath.Base(realRoot)
	repoIdentity := "local:" + strings.ToLower(displayName)
	if remRaw, err := exec.CommandContext(ctx, "git", "-C", realRoot, "remote", "get-url", "origin").Output(); err == nil {
		if sanitized := SanitizeGitRemoteIdentity(string(remRaw)); sanitized != "" {
			repoIdentity = sanitized
		}
	}

	return protocol.CheckRepoResponse{
		Valid:         true,
		RepoRoot:      realRoot,
		DefaultBranch: branch,
		RepoIdentity:  repoIdentity,
		DisplayName:   displayName,
	}
}

// SanitizeGitRemoteIdentity extracts host/path from a Git remote URL while strictly stripping
// embedded credentials (userinfo/passwords/tokens), query parameters, and fragments.
// Shared across project registration and runner worktree validation so both sides always match.
func SanitizeGitRemoteIdentity(raw string) string {
	rem := strings.TrimSpace(raw)
	if rem == "" {
		return ""
	}
	// Standard URL forms: https://user:secret@example.com/org/repo.git, ssh://git@host:22/org/repo.git
	if strings.Contains(rem, "://") {
		u, err := url.Parse(rem)
		if err != nil || u.Host == "" {
			return ""
		}
		p := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git")
		p = strings.Trim(p, "/")
		if p == "" {
			return ""
		}
		return strings.ToLower(u.Host) + "/" + p
	}
	// SCP-like SSH syntax: [user@]host:org/repo.git
	if colonIdx := strings.Index(rem, ":"); colonIdx > 0 && !strings.HasPrefix(rem, "/") {
		hostPart := rem[:colonIdx]
		if atIdx := strings.LastIndex(hostPart, "@"); atIdx >= 0 {
			hostPart = hostPart[atIdx+1:]
		}
		pathPart := strings.TrimSuffix(strings.TrimPrefix(rem[colonIdx+1:], "/"), ".git")
		pathPart = strings.Trim(pathPart, "/")
		if hostPart != "" && pathPart != "" {
			return strings.ToLower(hostPart) + "/" + pathPart
		}
	}
	return ""
}
