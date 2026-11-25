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

// ErrPathOutsideRoot is returned when a requested directory escapes allowed root directories.
var ErrPathOutsideRoot = errors.New("path is outside allowed root directories")

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
	if target == "" || target == "~" {
		target = realHome
	} else {
		target = protocol.ExpandPath(target)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(realHome, target)
	}
	target = filepath.Clean(target)

	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", realHome, err
	}
	realTarget = filepath.Clean(realTarget)

	allowed := false
	for _, root := range roots {
		if realTarget == root {
			allowed = true
			break
		}
		prefix := root
		if !strings.HasSuffix(prefix, string(os.PathSeparator)) {
			prefix += string(os.PathSeparator)
		}
		if strings.HasPrefix(realTarget, prefix) {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", realHome, fmt.Errorf("%w: %s", ErrPathOutsideRoot, rawPath)
	}
	return realTarget, realHome, nil
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
		return protocol.ListDirsResponse{}, err
	}
	if !info.IsDir() {
		return protocol.ListDirsResponse{}, fmt.Errorf("not a directory: %s", resolved)
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
		out = append(out, protocol.DirEntryView{
			Name:      name,
			Path:      full,
			IsGitRepo: gitErr == nil,
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
	if resolved != realHome {
		candParent := filepath.Dir(resolved)
		if _, _, pErr := ResolveAllowedPath(candParent, allowedRoots); pErr == nil && candParent != resolved {
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
