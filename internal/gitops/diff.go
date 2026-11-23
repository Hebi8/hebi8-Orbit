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
// Supports non-ASCII/Unicode/space filenames and literal pathspecs.
func ComputeMirrorDiff(ctx context.Context, mirrorPath, baseSHA, headSHA, filePath string) ([]protocol.DiffFileStat, string, error) {
	if err := validateCommitArg(baseSHA); err != nil {
		return nil, "", fmt.Errorf("invalid baseSha: %w", err)
	}
	if err := validateCommitArg(headSHA); err != nil {
		return nil, "", fmt.Errorf("invalid headSha: %w", err)
	}
	cleanFile := ""
	if filePath != "" {
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

	args := []string{"-c", "core.quotePath=false", "--git-dir", absMirror, "diff", "-U3", "--no-color", "-M", baseSHA, headSHA}
	if cleanFile != "" {
		// For renames/copies, include both old and new literal pathspecs so git preserves rename diff headers.
		var oldMatch string
		for _, f := range files {
			if f.Path == cleanFile && f.OldPath != "" {
				oldMatch = f.OldPath
				break
			}
		}
		args = append(args, "--")
		if oldMatch != "" {
			args = append(args, ":(literal)"+oldMatch)
		}
		args = append(args, ":(literal)"+cleanFile)
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
		prefix := []string{"-c", "core.quotePath=false"}
		if isBare {
			args := append(prefix, "--git-dir", repoOrMirror)
			args = append(args, rest...)
			return runGit(ctx, "", nil, args...)
		}
		args := append(prefix, rest...)
		return runGit(ctx, repoOrMirror, nil, args...)
	}

	statusOut, err := baseArgs("diff", "--name-status", "-M", "-z", baseSHA, headSHA)
	if err != nil {
		return nil, err
	}
	numOut, err := baseArgs("diff", "--numstat", "-M", "-z", baseSHA, headSHA)
	if err != nil {
		return nil, err
	}

	byPath := map[string]*protocol.DiffFileStat{}
	order := []string{}

	// Parse NUL-delimited --name-status -M -z:
	// - Rnnn\0oldPath\0newPath\0 or Cnnn\0oldPath\0newPath\0
	// - A/M/D/T/U\0path\0
	statusTokens := strings.Split(statusOut, "\x00")
	for i := 0; i < len(statusTokens); {
		code := statusTokens[i]
		if code == "" {
			i++
			continue
		}
		var stat protocol.DiffFileStat
		if (strings.HasPrefix(code, "R") || strings.HasPrefix(code, "C")) && i+2 < len(statusTokens) {
			if strings.HasPrefix(code, "R") {
				stat.Status = "renamed"
			} else {
				stat.Status = "copied"
			}
			stat.OldPath = statusTokens[i+1]
			stat.Path = statusTokens[i+2]
			i += 3
		} else if i+1 < len(statusTokens) {
			switch {
			case strings.HasPrefix(code, "A"):
				stat.Status = "added"
			case strings.HasPrefix(code, "D"):
				stat.Status = "deleted"
			default:
				stat.Status = "modified"
			}
			stat.Path = statusTokens[i+1]
			i += 2
		} else {
			break
		}
		if stat.Path != "" {
			byPath[stat.Path] = &stat
			order = append(order, stat.Path)
		}
	}

	// Parse NUL-delimited --numstat -M -z:
	// - Non-rename: "<ins>\t<del>\t<path>\0"
	// - Rename/copy: "<ins>\t<del>\t\0<oldPath>\0<newPath>\0"
	numTokens := strings.Split(numOut, "\x00")
	for i := 0; i < len(numTokens); {
		tok := numTokens[i]
		if tok == "" {
			i++
			continue
		}
		parts := strings.SplitN(tok, "\t", 3)
		if len(parts) < 3 {
			i++
			continue
		}
		insStr, delStr, pathPart := parts[0], parts[1], parts[2]
		var targetPath string
		if pathPart == "" && i+2 < len(numTokens) {
			// Rename/copy entry: next two NUL tokens are oldPath, newPath
			targetPath = numTokens[i+2]
			i += 3
		} else {
			targetPath = pathPart
			i++
		}
		if targetPath == "" {
			continue
		}
		st, ok := byPath[targetPath]
		if !ok {
			st = &protocol.DiffFileStat{Path: targetPath, Status: "modified"}
			byPath[targetPath] = st
			order = append(order, targetPath)
		}
		if insStr == "-" || delStr == "-" {
			st.Binary = true
			continue
		}
		if ins, err := strconv.Atoi(insStr); err == nil {
			st.Insertions = ins
		}
		if del, err := strconv.Atoi(delStr); err == nil {
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

func validateCommitArg(sha string) error {
	sha = strings.TrimSpace(sha)
	if sha == "" || strings.HasPrefix(sha, "-") {
		return fmt.Errorf("unsafe commit ref %q", sha)
	}
	return nil
}

func validateRelativeFile(p string) (string, error) {
	if p == "" || strings.ContainsRune(p, '\x00') {
		return "", fmt.Errorf("%w: %s", ErrInvalidPath, p)
	}
	cleaned := filepath.ToSlash(filepath.Clean(p))
	if cleaned == "." || cleaned == "" || strings.HasPrefix(cleaned, "/") || strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		return "", fmt.Errorf("%w: %s", ErrInvalidPath, p)
	}
	return cleaned, nil
}
