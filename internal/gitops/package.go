package gitops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

var (
	ErrNoHeadCommit        = errors.New("repository has no HEAD commit; create an initial commit first")
	ErrDirtySubmodule      = errors.New("ERR_DIRTY_SUBMODULE: dirty or uninitialized submodule detected")
	ErrAttachmentConflict  = errors.New("ERR_ATTACHMENT_CONFLICT: attachment path collides with tracked/packaged git file")
	ErrDeliveryRefConflict = errors.New("ERR_DELIVERY_REF_CONFLICT: delivery ref already exists with a different SHA")
	ErrInvalidPath         = errors.New("invalid relative file path")
)

// AttachmentInput specifies a file to attach as a content-addressed blob.
type AttachmentInput struct {
	LocalPath  string
	TargetPath string // normalized relative path stored in manifest.blobs[].path
}

// PackageOptions configures zero-pollution checkpoint packaging.
type PackageOptions struct {
	RepoDir       string
	DeliveryID    string
	TaskID        string
	AttemptID     string
	ReviewBaseSHA string
	Attachments   []AttachmentInput
}

// PackagedBlob describes an attachment file ready for upload.
type PackagedBlob struct {
	LocalPath string
	Item      protocol.BlobManifestItem
}

// PackageResult holds immutable refs, baselines, and manifest data produced by packaging.
type PackageResult struct {
	DeliveryID      string
	ReviewBaseSHA   string
	CommitParentSHA string
	HeadSHA         string
	ReusedHead      bool
	Manifest        protocol.DeliveryManifest
	Blobs           []PackagedBlob
}

// PackageCheckpoint packages the repository working tree (including unstaged/untracked changes)
// using an isolated temporary GIT_INDEX_FILE so the source repository's index and working tree
// suffer zero pollution.
func PackageCheckpoint(ctx context.Context, opts PackageOptions) (PackageResult, error) {
	var res PackageResult
	if opts.RepoDir == "" {
		return res, fmt.Errorf("repo dir is required")
	}
	if opts.DeliveryID == "" {
		return res, fmt.Errorf("delivery id is required")
	}

	absRepo, err := filepath.Abs(opts.RepoDir)
	if err != nil {
		return res, fmt.Errorf("resolve repo dir: %w", err)
	}

	// 0. Verify HEAD exists -> commitParentSHA
	commitParentSHA, err := runGitTrim(ctx, absRepo, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return res, fmt.Errorf("%w: %v", ErrNoHeadCommit, err)
	}

	reviewBaseSHA := commitParentSHA
	if strings.TrimSpace(opts.ReviewBaseSHA) != "" {
		resolvedBase, err := runGitTrim(ctx, absRepo, nil, "rev-parse", "--verify", opts.ReviewBaseSHA+"^{commit}")
		if err != nil {
			return res, fmt.Errorf("invalid base sha %q: %w", opts.ReviewBaseSHA, err)
		}
		reviewBaseSHA = resolvedBase
	}

	// 1. Detect dirty submodules
	subOut, err := runGit(ctx, absRepo, nil, "submodule", "status", "--recursive")
	if err == nil {
		for _, line := range strings.Split(subOut, "\n") {
			if len(line) > 0 && (line[0] == '+' || line[0] == '-' || line[0] == 'U') {
				return res, fmt.Errorf("%w: %s", ErrDirtySubmodule, strings.TrimSpace(line))
			}
		}
	}

	// 2. Create a fresh temp directory and point GIT_INDEX_FILE to a non-existent file inside it
	tmpDir, err := os.MkdirTemp("", "orbit-index-*")
	if err != nil {
		return res, fmt.Errorf("create temp index dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	indexEnv := append(os.Environ(), "GIT_INDEX_FILE="+filepath.Join(tmpDir, "index"))

	if _, err := runGit(ctx, absRepo, indexEnv, "read-tree", commitParentSHA); err != nil {
		return res, fmt.Errorf("git read-tree: %w", err)
	}

	// 3. Add all changes respecting .gitignore into the isolated temp index
	if _, err := runGit(ctx, absRepo, indexEnv, "add", "-A"); err != nil {
		return res, fmt.Errorf("git add -A: %w", err)
	}

	// 4. Write tree and compare against HEAD^{tree}; reuse HEAD directly if working tree is clean
	treeSHA, err := runGitTrim(ctx, absRepo, indexEnv, "write-tree")
	if err != nil {
		return res, fmt.Errorf("git write-tree: %w", err)
	}
	headTreeSHA, err := runGitTrim(ctx, absRepo, nil, "rev-parse", commitParentSHA+"^{tree}")
	if err != nil {
		return res, fmt.Errorf("git rev-parse HEAD^{tree}: %w", err)
	}

	var headSHA string
	reusedHead := false
	if treeSHA == headTreeSHA {
		headSHA = commitParentSHA
		reusedHead = true
	} else {
		msg := fmt.Sprintf("orbit checkpoint: delivery=%s task=%s attempt=%s", opts.DeliveryID, opts.TaskID, opts.AttemptID)
		commitEnv := withFallbackGitIdentity(os.Environ())
		headSHA, err = runGitTrim(ctx, absRepo, commitEnv, "commit-tree", treeSHA, "-p", commitParentSHA, "-m", msg)
		if err != nil {
			return res, fmt.Errorf("git commit-tree: %w", err)
		}
	}

	// 5. Inspect packaged tree files for attachment collision & .ryugi/artifacts discovery
	treeListOut, err := runGit(ctx, absRepo, nil, "ls-tree", "-r", "--name-only", treeSHA)
	if err != nil {
		return res, fmt.Errorf("git ls-tree: %w", err)
	}
	treeFiles := map[string]struct{}{}
	var treeFileSlice []string
	for _, line := range strings.Split(treeListOut, "\n") {
		p := strings.TrimSpace(line)
		if p != "" {
			treeFiles[p] = struct{}{}
			treeFileSlice = append(treeFileSlice, p)
		}
	}

	ryugiArtifacts := extractRyugiArtifactLocators(treeFileSlice)

	// Validate attachments against Git tree and compute sha256
	seenAttach := map[string]struct{}{}
	var packagedBlobs []PackagedBlob
	var manifestBlobs []protocol.BlobManifestItem
	for _, att := range opts.Attachments {
		localAbs, err := filepath.Abs(att.LocalPath)
		if err != nil {
			return res, fmt.Errorf("resolve attachment %s: %w", att.LocalPath, err)
		}
		target := att.TargetPath
		if target == "" {
			if rel, err := filepath.Rel(absRepo, localAbs); err == nil && !strings.HasPrefix(rel, "..") {
				target = filepath.ToSlash(rel)
			} else {
				target = filepath.Base(localAbs)
			}
		}
		target = filepath.ToSlash(filepath.Clean(target))
		if target == "." || strings.HasPrefix(target, "../") || strings.HasPrefix(target, "/") {
			return res, fmt.Errorf("%w: %s", ErrInvalidPath, target)
		}
		if _, exists := treeFiles[target]; exists {
			return res, fmt.Errorf("%w: %s", ErrAttachmentConflict, target)
		}
		if _, dup := seenAttach[target]; dup {
			return res, fmt.Errorf("%w: duplicate attachment path %s", ErrAttachmentConflict, target)
		}
		seenAttach[target] = struct{}{}

		digest, size, err := hashFileSHA256(localAbs)
		if err != nil {
			return res, fmt.Errorf("hash attachment %s: %w", localAbs, err)
		}
		item := protocol.BlobManifestItem{
			Path:   target,
			SHA256: digest,
			Size:   size,
		}
		packagedBlobs = append(packagedBlobs, PackagedBlob{LocalPath: localAbs, Item: item})
		manifestBlobs = append(manifestBlobs, item)
	}

	// 6. Compute diff summary against reviewBaseSHA (NOT commitParentSHA!)
	diffStats, err := computeDiffStats(ctx, absRepo, false, reviewBaseSHA, headSHA)
	if err != nil {
		return res, fmt.Errorf("compute review diff stats: %w", err)
	}
	ins, del := 0, 0
	for _, f := range diffStats {
		ins += f.Insertions
		del += f.Deletions
	}

	// 7. Persist local keep-alive ref so network retries reuse the exact same HEAD_SHA
	localRef := "refs/orbit/local/" + opts.DeliveryID
	if _, err := runGit(ctx, absRepo, nil, "update-ref", localRef, headSHA); err != nil {
		return res, fmt.Errorf("update local keep-alive ref: %w", err)
	}

	res = PackageResult{
		DeliveryID:      opts.DeliveryID,
		ReviewBaseSHA:   reviewBaseSHA,
		CommitParentSHA: commitParentSHA,
		HeadSHA:         headSHA,
		ReusedHead:      reusedHead,
		Manifest: protocol.DeliveryManifest{
			FilesChanged:   len(diffStats),
			Insertions:     ins,
			Deletions:      del,
			Blobs:          manifestBlobs,
			RyugiArtifacts: ryugiArtifacts,
			WorktreePath:   absRepo,
		},
		Blobs: packagedBlobs,
	}
	return res, nil
}

func extractRyugiArtifactLocators(paths []string) []string {
	const prefix = ".ryugi/artifacts/"
	seen := map[string]struct{}{}
	for _, p := range paths {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := strings.TrimPrefix(p, prefix)
		parts := strings.Split(rest, "/")
		if len(parts) < 3 {
			continue
		}
		artType, slug := parts[0], parts[1]
		if artType == "" || slug == "" || strings.HasPrefix(artType, ".") || strings.HasPrefix(slug, ".") {
			continue
		}
		seen[artType+"/"+slug] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for loc := range seen {
		out = append(out, loc)
	}
	sort.Strings(out)
	return out
}

func hashFileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func withFallbackGitIdentity(base []string) []string {
	hasKey := func(k string) bool {
		prefix := k + "="
		for _, kv := range base {
			if strings.HasPrefix(kv, prefix) {
				return true
			}
		}
		return false
	}
	env := append([]string{}, base...)
	defaults := map[string]string{
		"GIT_AUTHOR_NAME":     "hebi8-orbit",
		"GIT_AUTHOR_EMAIL":    "orbit@example.com",
		"GIT_COMMITTER_NAME":  "hebi8-orbit",
		"GIT_COMMITTER_EMAIL": "orbit@example.com",
	}
	for k, v := range defaults {
		if !hasKey(k) {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func runGit(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if env != nil {
		cmd.Env = env
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func runGitTrim(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	out, err := runGit(ctx, dir, env, args...)
	return strings.TrimSpace(out), err
}
