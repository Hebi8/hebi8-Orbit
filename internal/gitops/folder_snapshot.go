package gitops

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

const (
	maxSnapshotFiles      = 5000
	maxSnapshotDepth      = 10
	maxSnapshotFileBytes  = 5 * 1024 * 1024 // 5 MiB
	maxTextDiffFileBytes  = 512 * 1024      // 512 KiB
	maxScanRepos          = 200
	maxScanDepth          = 2
	maxFolderReadBytesDef = 20 * 1024 * 1024 // 20 MiB
)

var snapshotSkipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	".orbit":       true,
	"target":       true,
	"dist":         true,
	".next":        true,
	".venv":        true,
	"__pycache__":  true,
	".cache":       true,
}

// QuickReadGitBranch reads the checked-out Git branch name directly from .git/HEAD without spawning git.
func QuickReadGitBranch(dir string) string {
	gitPath := filepath.Join(dir, ".git")
	fi, err := os.Stat(gitPath)
	if err != nil {
		return ""
	}
	headFile := filepath.Join(gitPath, "HEAD")
	if !fi.IsDir() {
		// Worktree / submodule gitfile: "gitdir: <path>"
		raw, err := os.ReadFile(gitPath)
		if err != nil {
			return ""
		}
		line := strings.TrimSpace(string(raw))
		if !strings.HasPrefix(line, "gitdir:") {
			return ""
		}
		gd := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
		if !filepath.IsAbs(gd) {
			gd = filepath.Join(dir, gd)
		}
		headFile = filepath.Join(gd, "HEAD")
	}
	raw, err := os.ReadFile(headFile)
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(raw))
	const prefix = "ref: refs/heads/"
	if strings.HasPrefix(head, prefix) {
		return strings.TrimSpace(strings.TrimPrefix(head, prefix))
	}
	if len(head) >= 7 {
		return head[:7]
	}
	return ""
}

// InspectDirectory validates any directory within allowedRoots — supporting BOTH Git repos
// (with zsh-style dirty/ahead/behind/branch/headSha/rootCommit) AND plain folders cleanly.
func InspectDirectory(ctx context.Context, rawPath string, allowedRoots []string) protocol.CheckRepoResponse {
	resolved, _, err := ResolveAllowedPath(rawPath, allowedRoots)
	if err != nil {
		return protocol.CheckRepoResponse{Valid: false, Exists: false, Error: err.Error()}
	}
	fi, err := os.Stat(resolved)
	if err != nil || !fi.IsDir() {
		return protocol.CheckRepoResponse{Valid: false, Exists: false, Error: "directory does not exist"}
	}

	displayName := filepath.Base(resolved)
	topRaw, err := exec.CommandContext(ctx, "git", "-C", resolved, "rev-parse", "--show-toplevel").Output()
	if err != nil || strings.TrimSpace(string(topRaw)) == "" {
		return protocol.CheckRepoResponse{
			Valid:        true,
			Exists:       true,
			IsGit:        false,
			RepoRoot:     resolved,
			RepoIdentity: "folder:" + strings.ToLower(displayName),
			DisplayName:  displayName,
		}
	}

	repoRoot := strings.TrimSpace(string(topRaw))
	realRoot, _, err := ResolveAllowedPath(repoRoot, allowedRoots)
	if err != nil {
		// Parent Git repo is outside allowedRoots -> treat the selected subdir as a plain folder.
		return protocol.CheckRepoResponse{
			Valid:        true,
			Exists:       true,
			IsGit:        false,
			RepoRoot:     resolved,
			RepoIdentity: "folder:" + strings.ToLower(displayName),
			DisplayName:  displayName,
		}
	}

	// If the user explicitly pointed at a subdirectory inside a larger repo that isn't the repo root,
	// still report the actual git repo root + status so callers know whether it's a git repo.
	displayName = filepath.Base(realRoot)
	branch := QuickReadGitBranch(realRoot)
	if brRaw, err := exec.CommandContext(ctx, "git", "-C", realRoot, "symbolic-ref", "--short", "HEAD").Output(); err == nil {
		if b := strings.TrimSpace(string(brRaw)); b != "" {
			branch = b
		}
	}
	if branch == "" {
		branch = "main"
	}

	headSHA := ""
	if hRaw, err := exec.CommandContext(ctx, "git", "-C", realRoot, "rev-parse", "HEAD").Output(); err == nil {
		headSHA = strings.TrimSpace(string(hRaw))
	}

	rootCommit := ""
	if rRaw, err := exec.CommandContext(ctx, "git", "-C", realRoot, "rev-list", "--max-parents=0", "--first-parent", "HEAD").Output(); err == nil {
		lines := strings.Split(strings.TrimSpace(string(rRaw)), "\n")
		if len(lines) > 0 {
			rootCommit = strings.TrimSpace(lines[0])
		}
	}

	dirty := false
	if sRaw, err := exec.CommandContext(ctx, "git", "-C", realRoot, "status", "--porcelain").Output(); err == nil {
		dirty = strings.TrimSpace(string(sRaw)) != ""
	}

	ahead, behind := 0, 0
	if abRaw, err := exec.CommandContext(ctx, "git", "-C", realRoot, "rev-list", "--left-right", "--count", "@{u}...HEAD").Output(); err == nil {
		fields := strings.Fields(strings.TrimSpace(string(abRaw)))
		if len(fields) == 2 {
			behind, _ = strconv.Atoi(fields[0])
			ahead, _ = strconv.Atoi(fields[1])
		}
	}

	repoIdentity := "local:" + strings.ToLower(displayName)
	if remRaw, err := exec.CommandContext(ctx, "git", "-C", realRoot, "remote", "get-url", "origin").Output(); err == nil {
		if sanitized := SanitizeGitRemoteIdentity(string(remRaw)); sanitized != "" {
			repoIdentity = sanitized
		}
	}

	return protocol.CheckRepoResponse{
		Valid:         true,
		Exists:        true,
		IsGit:         true,
		RepoRoot:      realRoot,
		DefaultBranch: branch,
		RepoIdentity:  repoIdentity,
		RootCommit:    rootCommit,
		HeadSHA:       headSHA,
		Dirty:         dirty,
		Ahead:         ahead,
		Behind:        behind,
		DisplayName:   displayName,
	}
}

// ScannedGitRepo represents one shallow-discovered local Git repository for optional recommendations.
type ScannedGitRepo struct {
	AbsPath      string `json:"absPath"`
	Name         string `json:"name"`
	Branch       string `json:"branch,omitempty"`
	RepoIdentity string `json:"repoIdentity,omitempty"`
}

// ScanGitSuggestions performs a strictly bounded (depth <= 2) read-only scan of allowedRoots
// looking ONLY for immediate `.git/config` + `HEAD` metadata. Never spawns subprocesses or mutates files.
func ScanGitSuggestions(ctx context.Context, allowedRoots []string) ([]ScannedGitRepo, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	roots := make([]string, 0, len(allowedRoots)+4)
	if len(allowedRoots) > 0 {
		for _, r := range allowedRoots {
			if res, _, err := ResolveAllowedPath(r, allowedRoots); err == nil {
				roots = append(roots, res)
			}
		}
	} else {
		candidates := []string{
			filepath.Join(home, "dev"),
			filepath.Join(home, "code"),
			filepath.Join(home, "projects"),
			filepath.Join(home, "workspace"),
			home,
		}
		for _, c := range candidates {
			if fi, err := os.Stat(c); err == nil && fi.IsDir() {
				if realC, err := filepath.EvalSymlinks(c); err == nil {
					roots = append(roots, filepath.Clean(realC))
				} else {
					roots = append(roots, filepath.Clean(c))
				}
			}
		}
	}

	seen := map[string]bool{}
	out := make([]ScannedGitRepo, 0, 32)
	var scanDir func(dir string, depth int)
	scanDir = func(dir string, depth int) {
		if ctx.Err() != nil || len(out) >= maxScanRepos || depth > maxScanDepth {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if ctx.Err() != nil || len(out) >= maxScanRepos {
				return
			}
			name := e.Name()
			if strings.HasPrefix(name, ".") || snapshotSkipDirs[name] || name == "Library" || name == "Applications" || name == "Downloads" {
				continue
			}
			if !e.IsDir() {
				continue
			}
			full := filepath.Join(dir, name)
			if seen[full] {
				continue
			}
			if _, err := os.Stat(filepath.Join(full, ".git")); err == nil {
				seen[full] = true
				repoID := readGitOriginIdentityFromConfig(full)
				if repoID == "" {
					repoID = "local:" + strings.ToLower(name)
				}
				out = append(out, ScannedGitRepo{
					AbsPath:      full,
					Name:         name,
					Branch:       QuickReadGitBranch(full),
					RepoIdentity: repoID,
				})
				continue
			}
			if depth < maxScanDepth {
				scanDir(full, depth+1)
			}
		}
	}

	for _, root := range roots {
		if seen[root] {
			continue
		}
		scanDir(root, 1)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AbsPath < out[j].AbsPath })
	return out, nil
}

func readGitOriginIdentityFromConfig(repoDir string) string {
	cfgPath := filepath.Join(repoDir, ".git", "config")
	f, err := os.Open(cfgPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	inOrigin := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inOrigin = strings.EqualFold(line, `[remote "origin"]`)
			continue
		}
		if inOrigin && strings.HasPrefix(strings.ToLower(line), "url") {
			if eq := strings.IndexByte(line, '='); eq > 0 {
				rawURL := strings.TrimSpace(line[eq+1:])
				if id := SanitizeGitRemoteIdentity(rawURL); id != "" {
					return id
				}
			}
		}
	}
	return ""
}

// FolderFileRecord stores lightweight metadata and optional text lines for a single file.
type FolderFileRecord struct {
	RelPath  string
	Size     int64
	SHA256   string
	IsText   bool
	Lines    []string
}

// FolderSnapshot captures a lightweight file-tree state before/after an in-place folder attempt.
type FolderSnapshot struct {
	RootDir string
	Files   map[string]FolderFileRecord
}

// CaptureFolderSnapshot walks rootDir skipping heavy/VCS directories and files > 5MB.
// It NEVER creates any `.git` directory or writes anything to disk.
func CaptureFolderSnapshot(rootDir string) (*FolderSnapshot, error) {
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, err
	}
	snap := &FolderSnapshot{
		RootDir: absRoot,
		Files:   map[string]FolderFileRecord{},
	}
	count := 0
	err = filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if path == absRoot {
			return nil
		}
		rel, err := filepath.Rel(absRoot, path)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		name := d.Name()
		if d.IsDir() {
			if snapshotSkipDirs[name] {
				return filepath.SkipDir
			}
			if strings.Count(relSlash, "/") >= maxSnapshotDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if name == ".DS_Store" || (d.Type()&os.ModeSymlink) != 0 {
			return nil
		}
		if count >= maxSnapshotFiles {
			return nil
		}
		fi, err := d.Info()
		if err != nil || !fi.Mode().IsRegular() {
			return nil
		}
		if fi.Size() > maxSnapshotFileBytes {
			return nil
		}
		rec, ok := readFolderFileRecord(path, relSlash, fi.Size())
		if ok {
			snap.Files[relSlash] = rec
			count++
		}
		return nil
	})
	return snap, err
}

func readFolderFileRecord(absPath, relSlash string, size int64) (FolderFileRecord, bool) {
	f, err := os.Open(absPath)
	if err != nil {
		return FolderFileRecord{}, false
	}
	defer f.Close()

	if size <= maxTextDiffFileBytes {
		data, err := io.ReadAll(io.LimitReader(f, maxTextDiffFileBytes+1))
		if err != nil {
			return FolderFileRecord{}, false
		}
		sum := sha256.Sum256(data)
		rec := FolderFileRecord{
			RelPath: relSlash,
			Size:    int64(len(data)),
			SHA256:  hex.EncodeToString(sum[:]),
		}
		if isLikelyUTF8Text(data) {
			rec.IsText = true
			rec.Lines = splitLinesNormalized(string(data))
		}
		return rec, true
	}

	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, maxSnapshotFileBytes)); err != nil {
		return FolderFileRecord{}, false
	}
	return FolderFileRecord{
		RelPath: relSlash,
		Size:    size,
		SHA256:  hex.EncodeToString(h.Sum(nil)),
	}, true
}

func isLikelyUTF8Text(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	sample := data
	if len(sample) > 8192 {
		sample = sample[:8192]
	}
	if bytes.IndexByte(sample, 0) >= 0 {
		return false
	}
	return utf8.Valid(sample)
}

func splitLinesNormalized(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// DiffFolderSnapshots computes file additions, modifications, deletions, and line deltas between two snapshots.
func DiffFolderSnapshots(before, after *FolderSnapshot) protocol.FolderChangesSummary {
	summary := protocol.FolderChangesSummary{
		Files: []protocol.FolderChangeItem{},
	}
	beforeFiles := map[string]FolderFileRecord{}
	afterFiles := map[string]FolderFileRecord{}
	if before != nil {
		beforeFiles = before.Files
	}
	if after != nil {
		afterFiles = after.Files
	}

	for path, aft := range afterFiles {
		bef, existed := beforeFiles[path]
		if !existed {
			adds := len(aft.Lines)
			summary.Files = append(summary.Files, protocol.FolderChangeItem{
				Path:       path,
				Kind:       "add",
				Insertions: adds,
				Deletions:  0,
				SizeBytes:  aft.Size,
			})
			summary.TotalAdds += adds
			continue
		}
		if bef.SHA256 != aft.SHA256 {
			adds, dels := 0, 0
			if bef.IsText && aft.IsText {
				adds, dels = computeLineDelta(bef.Lines, aft.Lines)
			}
			summary.Files = append(summary.Files, protocol.FolderChangeItem{
				Path:       path,
				Kind:       "mod",
				Insertions: adds,
				Deletions:  dels,
				SizeBytes:  aft.Size,
			})
			summary.TotalAdds += adds
			summary.TotalDels += dels
		}
	}

	for path, bef := range beforeFiles {
		if _, exists := afterFiles[path]; !exists {
			dels := len(bef.Lines)
			summary.Files = append(summary.Files, protocol.FolderChangeItem{
				Path:       path,
				Kind:       "del",
				Insertions: 0,
				Deletions:  dels,
				SizeBytes:  bef.Size,
			})
			summary.TotalDels += dels
		}
	}

	sort.Slice(summary.Files, func(i, j int) bool {
		return summary.Files[i].Path < summary.Files[j].Path
	})
	return summary
}

// computeLineDelta computes exact LCS line insertions/deletions for normal files and falls back
// to O(N) multiset delta when files exceed 1200 lines to keep diff latency negligible.
func computeLineDelta(a, b []string) (adds, dels int) {
	// Trim common prefix and suffix first (handles most real edits in O(N)).
	start := 0
	for start < len(a) && start < len(b) && a[start] == b[start] {
		start++
	}
	endA, endB := len(a), len(b)
	for endA > start && endB > start && a[endA-1] == b[endB-1] {
		endA--
		endB--
	}
	aMid := a[start:endA]
	bMid := b[start:endB]
	if len(aMid) == 0 {
		return len(bMid), 0
	}
	if len(bMid) == 0 {
		return 0, len(aMid)
	}
	if len(aMid)*len(bMid) <= 400000 {
		// Exact LCS length via 2-row DP
		prev := make([]int, len(bMid)+1)
		curr := make([]int, len(bMid)+1)
		for i := 1; i <= len(aMid); i++ {
			for j := 1; j <= len(bMid); j++ {
				if aMid[i-1] == bMid[j-1] {
					curr[j] = prev[j-1] + 1
				} else if prev[j] >= curr[j-1] {
					curr[j] = prev[j]
				} else {
					curr[j] = curr[j-1]
				}
			}
			prev, curr = curr, prev
		}
		lcs := prev[len(bMid)]
		return len(bMid) - lcs, len(aMid) - lcs
	}
	// Fast O(N) multiset fallback for huge middle diffs
	counts := make(map[string]int, len(aMid))
	for _, l := range aMid {
		counts[l]++
	}
	common := 0
	for _, l := range bMid {
		if counts[l] > 0 {
			counts[l]--
			common++
		}
	}
	return len(bMid) - common, len(aMid) - common
}

// ReadFolderFile safely reads a single file inside rootDir with strict symlink & traversal containment.
func ReadFolderFile(rootDir, relPath string, allowedRoots []string, maxBytes int64) ([]byte, string, error) {
	if maxBytes <= 0 {
		maxBytes = maxFolderReadBytesDef
	}
	realRoot, _, err := ResolveAllowedPath(rootDir, allowedRoots)
	if err != nil {
		return nil, "", err
	}
	cleanRel := filepath.Clean(filepath.FromSlash(strings.TrimSpace(relPath)))
	if cleanRel == "" || cleanRel == "." || filepath.IsAbs(cleanRel) || strings.HasPrefix(cleanRel, "..") {
		return nil, "", fmt.Errorf("invalid relative file path %q", relPath)
	}
	candidate := filepath.Join(realRoot, cleanRel)
	realFile, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return nil, "", err
	}
	realFile = filepath.Clean(realFile)
	prefix := realRoot
	if !strings.HasSuffix(prefix, string(os.PathSeparator)) {
		prefix += string(os.PathSeparator)
	}
	if !strings.HasPrefix(realFile, prefix) {
		return nil, "", fmt.Errorf("%w: %s", ErrPathOutsideRoot, relPath)
	}
	fi, err := os.Stat(realFile)
	if err != nil {
		return nil, "", err
	}
	if !fi.Mode().IsRegular() {
		return nil, "", fmt.Errorf("not a regular file: %s", relPath)
	}
	if fi.Size() > maxBytes {
		return nil, "", fmt.Errorf("file %s exceeds max download size (%d bytes)", relPath, maxBytes)
	}
	data, err := os.ReadFile(realFile)
	if err != nil {
		return nil, "", err
	}
	return data, filepath.Base(realFile), nil
}
