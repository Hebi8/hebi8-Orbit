package gitops

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

// ComputeMirrorDiff reads file stats and unified diff text between baseSHA and headSHA
// directly from the bare mirror repository without checking out a worktree.
func ComputeMirrorDiff(ctx context.Context, mirrorPath, baseSHA, headSHA, filePath string) ([]protocol.DiffFileStat, string, error) {
	if err := validateCommitArg(baseSHA); err != nil {
		return nil, "", fmt.Errorf("invalid baseSha: %w", err)
	}
	if err := validateCommitArg(headSHA); err != nil {
		return nil, "", fmt.Errorf("invalid headSha: %w", err)
	}
	cleanFile := ""
	if strings.TrimSpace(filePath) != "" {
		var err error
		cleanFile, err = validateRelativeFile(filePath)
		if err != nil {
			return nil, "", err
		}
	}

	absMirror, err := filepath.Abs(mirrorPath)
	if err != nil {
		return nil, "", err
	}

	if baseSHA == headSHA {
		return []protocol.DiffFileStat{}, "", nil
	}

	files, err := computeDiffStats(ctx, absMirror, true, baseSHA, headSHA)
	if err != nil {
		return nil, "", err
	}

	args := []string{"--git-dir", absMirror, "diff", "-U3", "--no-color", "-M", baseSHA, headSHA}
	if cleanFile != "" {
		args = append(args, "--", cleanFile)
	}
	diffText, err := runGit(ctx, "", nil, args...)
	if err != nil {
		return nil, "", fmt.Errorf("git diff: %w", err)
	}
	return files, diffText, nil
}

// ComputeMirrorDiffStats computes only changed file stats between baseSHA and headSHA.
func ComputeMirrorDiffStats(ctx context.Context, mirrorPath, baseSHA, headSHA string) ([]protocol.DiffFileStat, error) {
	if err := validateCommitArg(baseSHA); err != nil {
		return nil, err
	}
	if err := validateCommitArg(headSHA); err != nil {
		return nil, err
	}
	if baseSHA == headSHA {
		return []protocol.DiffFileStat{}, nil
	}
	absMirror, err := filepath.Abs(mirrorPath)
	if err != nil {
		return nil, err
	}
	return computeDiffStats(ctx, absMirror, true, baseSHA, headSHA)
}

func computeDiffStats(ctx context.Context, repoOrMirror string, isBare bool, baseSHA, headSHA string) ([]protocol.DiffFileStat, error) {
	if baseSHA == headSHA {
		return []protocol.DiffFileStat{}, nil
	}

	baseArgs := func(rest ...string) (string, error) {
		if isBare {
			args := append([]string{"--git-dir", repoOrMirror}, rest...)
			return runGit(ctx, "", nil, args...)
		}
		return runGit(ctx, repoOrMirror, nil, rest...)
	}

	statusOut, err := baseArgs("diff", "--name-status", "-M", baseSHA, headSHA)
	if err != nil {
		return nil, err
	}
	numOut, err := baseArgs("diff", "--numstat", "-M", baseSHA, headSHA)
	if err != nil {
		return nil, err
	}

	byPath := map[string]*protocol.DiffFileStat{}
	order := []string{}

	for _, line := range strings.Split(statusOut, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}
		code := parts[0]
		var stat protocol.DiffFileStat
		switch {
		case strings.HasPrefix(code, "A"):
			stat.Status = "added"
			stat.Path = parts[1]
		case strings.HasPrefix(code, "D"):
			stat.Status = "deleted"
			stat.Path = parts[1]
		case strings.HasPrefix(code, "R") && len(parts) >= 3:
			stat.Status = "renamed"
			stat.OldPath = parts[1]
			stat.Path = parts[2]
		case strings.HasPrefix(code, "C") && len(parts) >= 3:
			stat.Status = "copied"
			stat.OldPath = parts[1]
			stat.Path = parts[2]
		default:
			stat.Status = "modified"
			stat.Path = parts[1]
		}
		byPath[stat.Path] = &stat
		order = append(order, stat.Path)
	}

	for _, line := range strings.Split(numOut, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			continue
		}
		rawPath := parts[2]
		// Normalize rename numstat syntax "a/{old => new}/b" or "old => new" when looking up
		lookupPath := normalizeNumstatPath(rawPath)
		st, ok := byPath[lookupPath]
		if !ok {
			st = &protocol.DiffFileStat{Path: lookupPath, Status: "modified"}
			byPath[lookupPath] = st
			order = append(order, lookupPath)
		}
		if parts[0] == "-" || parts[1] == "-" {
			st.Binary = true
			continue
		}
		if ins, err := strconv.Atoi(parts[0]); err == nil {
			st.Insertions = ins
		}
		if del, err := strconv.Atoi(parts[1]); err == nil {
			st.Deletions = del
		}
	}

	out := make([]protocol.DiffFileStat, 0, len(order))
	for _, p := range order {
		if st, ok := byPath[p]; ok {
			out = append(out, *st)
		}
	}
	return out, nil
}

func normalizeNumstatPath(raw string) string {
	if !strings.Contains(raw, " => ") {
		return raw
	}
	if start := strings.Index(raw, "{"); start != -1 {
		if end := strings.Index(raw[start:], "}"); end != -1 {
			inner := raw[start+1 : start+end]
			if arrow := strings.Index(inner, " => "); arrow != -1 {
				replacement := inner[arrow+4:]
				combined := raw[:start] + replacement + raw[start+end+1:]
				return strings.ReplaceAll(combined, "//", "/")
			}
		}
	}
	parts := strings.SplitN(raw, " => ", 2)
	return parts[1]
}

func validateCommitArg(sha string) error {
	sha = strings.TrimSpace(sha)
	if sha == "" || strings.HasPrefix(sha, "-") {
		return fmt.Errorf("unsafe commit ref %q", sha)
	}
	return nil
}

func validateRelativeFile(p string) (string, error) {
	cleaned := filepath.ToSlash(filepath.Clean(p))
	if cleaned == "." || cleaned == "" || strings.HasPrefix(cleaned, "/") || strings.HasPrefix(cleaned, "../") || cleaned == ".." || strings.HasPrefix(cleaned, "-") {
		return "", fmt.Errorf("%w: %s", ErrInvalidPath, p)
	}
	return cleaned, nil
}
