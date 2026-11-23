package gitops_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Hebi8/hebi8-Orbit/internal/gitops"
)

func runCmd(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cmd %s %v failed: %v\n%s", name, args, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func initTestRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=Tester",
		"GIT_AUTHOR_EMAIL=tester@example.com",
		"GIT_COMMITTER_NAME=Tester",
		"GIT_COMMITTER_EMAIL=tester@example.com",
	)
	runCmd(t, repo, env, "git", "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# Demo\nline 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("ignored.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, repo, env, "git", "add", ".")
	runCmd(t, repo, env, "git", "commit", "-m", "initial commit")
	baseSHA := runCmd(t, repo, env, "git", "rev-parse", "HEAD")
	return repo, baseSHA
}

func TestPackageCheckpoint_ZeroPollutionAndDualBaseline(t *testing.T) {
	ctx := context.Background()
	repo, baseSHA := initTestRepo(t)
	mirror := filepath.Join(t.TempDir(), "demo.git")

	// Create staged edit, unstaged edit, untracked file, and ignored file
	if err := os.WriteFile(filepath.Join(repo, "staged.txt"), []byte("staged content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, repo, nil, "git", "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# Demo\nline 1\nline 2 dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "untracked.go"), []byte("package demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "ignored.log"), []byte("should not enter tree\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	statusBefore := runCmd(t, repo, nil, "git", "status", "--porcelain")

	pkg, err := gitops.PackageCheckpoint(ctx, gitops.PackageOptions{
		RepoDir:    repo,
		DeliveryID: "dlv_01TESTPKG0000000000001",
		TaskID:     "tsk_1",
		AttemptID:  "att_1",
	})
	if err != nil {
		t.Fatalf("PackageCheckpoint failed: %v", err)
	}

	statusAfter := runCmd(t, repo, nil, "git", "status", "--porcelain")
	if statusBefore != statusAfter {
		t.Fatalf("repo git status polluted!\nbefore:\n%s\nafter:\n%s", statusBefore, statusAfter)
	}
	if pkg.ReusedHead {
		t.Fatalf("expected new checkpoint commit for dirty working tree")
	}
	if pkg.ReviewBaseSHA != baseSHA || pkg.CommitParentSHA != baseSHA {
		t.Fatalf("unexpected baselines: review=%s parent=%s expected=%s", pkg.ReviewBaseSHA, pkg.CommitParentSHA, baseSHA)
	}
	if pkg.Manifest.FilesChanged != 3 {
		t.Fatalf("expected 3 changed files (staged, README, untracked; excluding ignored.log), got %d", pkg.Manifest.FilesChanged)
	}

	// Push to bare mirror and check diff
	if err := gitops.PushDeliveryRef(ctx, repo, mirror, pkg.DeliveryID, pkg.HeadSHA); err != nil {
		t.Fatalf("PushDeliveryRef failed: %v", err)
	}
	files, diffText, err := gitops.ComputeMirrorDiff(ctx, mirror, pkg.ReviewBaseSHA, pkg.HeadSHA, "")
	if err != nil {
		t.Fatalf("ComputeMirrorDiff failed: %v", err)
	}
	if len(files) != 3 || !strings.Contains(diffText, "line 2 dirty") || strings.Contains(diffText, "should not enter tree") {
		t.Fatalf("unexpected diff result: files=%+v diff=%s", files, diffText)
	}

	// Now commit all changes normally in repo -> working tree is CLEAN.
	// Passing ReviewBaseSHA = baseSHA MUST reuse HEAD (no empty commit) AND still yield NON-EMPTY diff!
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=Tester",
		"GIT_AUTHOR_EMAIL=tester@example.com",
		"GIT_COMMITTER_NAME=Tester",
		"GIT_COMMITTER_EMAIL=tester@example.com",
	)
	runCmd(t, repo, env, "git", "add", "-A")
	runCmd(t, repo, env, "git", "commit", "-m", "agent committed changes")
	committedHead := runCmd(t, repo, env, "git", "rev-parse", "HEAD")

	pkg2, err := gitops.PackageCheckpoint(ctx, gitops.PackageOptions{
		RepoDir:       repo,
		DeliveryID:    "dlv_01TESTPKG0000000000002",
		ReviewBaseSHA: baseSHA,
	})
	if err != nil {
		t.Fatalf("second PackageCheckpoint failed: %v", err)
	}
	if !pkg2.ReusedHead || pkg2.HeadSHA != committedHead {
		t.Fatalf("expected clean tree to reuse committed HEAD %s, got head=%s reused=%v", committedHead, pkg2.HeadSHA, pkg2.ReusedHead)
	}
	if pkg2.ReviewBaseSHA != baseSHA || pkg2.Manifest.FilesChanged != 3 {
		t.Fatalf("expected dual baseline diff non-empty (3 files), got base=%s files=%d", pkg2.ReviewBaseSHA, pkg2.Manifest.FilesChanged)
	}

	if err := gitops.PushDeliveryRef(ctx, repo, mirror, pkg2.DeliveryID, pkg2.HeadSHA); err != nil {
		t.Fatalf("PushDeliveryRef v2 failed: %v", err)
	}
	files2, diffText2, err := gitops.ComputeMirrorDiff(ctx, mirror, pkg2.ReviewBaseSHA, pkg2.HeadSHA, "README.md")
	if err != nil {
		t.Fatalf("ComputeMirrorDiff single file failed: %v", err)
	}
	if len(files2) != 3 || !strings.Contains(diffText2, "line 2 dirty") || strings.Contains(diffText2, "untracked.go") {
		t.Fatalf("unexpected single-file diff: files=%d diff=%s", len(files2), diffText2)
	}
}

func TestPackageCheckpoint_AttachmentCollision(t *testing.T) {
	ctx := context.Background()
	repo, _ := initTestRepo(t)

	// Colliding with README.md in git tree MUST fail
	_, err := gitops.PackageCheckpoint(ctx, gitops.PackageOptions{
		RepoDir:    repo,
		DeliveryID: "dlv_01ATTACHCONFLICT000001",
		Attachments: []gitops.AttachmentInput{
			{LocalPath: filepath.Join(repo, "README.md"), TargetPath: "README.md"},
		},
	})
	if !errors.Is(err, gitops.ErrAttachmentConflict) {
		t.Fatalf("expected ErrAttachmentConflict, got %v", err)
	}

	// External non-colliding file MUST succeed with sha256
	extFile := filepath.Join(t.TempDir(), "mockup.png")
	if err := os.WriteFile(extFile, []byte("fake-png-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	pkg, err := gitops.PackageCheckpoint(ctx, gitops.PackageOptions{
		RepoDir:    repo,
		DeliveryID: "dlv_01ATTACHOK000000000001",
		Attachments: []gitops.AttachmentInput{
			{LocalPath: extFile, TargetPath: "assets/mockup.png"},
		},
	})
	if err != nil {
		t.Fatalf("expected valid attachment to succeed: %v", err)
	}
	if len(pkg.Blobs) != 1 || len(pkg.Blobs[0].Item.SHA256) != 64 || pkg.Blobs[0].Item.Size != 14 {
		t.Fatalf("unexpected packaged blob metadata: %+v", pkg.Blobs)
	}
}

func TestBareMirror_PreReceiveIdempotencyAndImmutability(t *testing.T) {
	ctx := context.Background()
	repo, sha1 := initTestRepo(t)
	mirror := filepath.Join(t.TempDir(), "mirror.git")

	// Create a second commit sha2
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=Tester",
		"GIT_AUTHOR_EMAIL=tester@example.com",
		"GIT_COMMITTER_NAME=Tester",
		"GIT_COMMITTER_EMAIL=tester@example.com",
	)
	if err := os.WriteFile(filepath.Join(repo, "second.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, repo, env, "git", "add", "second.txt")
	runCmd(t, repo, env, "git", "commit", "-m", "second commit")
	sha2 := runCmd(t, repo, env, "git", "rev-parse", "HEAD")

	dlvID := "dlv_01PRERECEIVE0000000001"

	// 1. First push -> succeeds
	if err := gitops.PushDeliveryRef(ctx, repo, mirror, dlvID, sha1); err != nil {
		t.Fatalf("first push failed: %v", err)
	}

	// 2. Same SHA retry -> idempotent success
	if err := gitops.PushDeliveryRef(ctx, repo, mirror, dlvID, sha1); err != nil {
		t.Fatalf("idempotent same-SHA push failed: %v", err)
	}

	// 3. Different SHA to same delivery ref -> rejected with ErrDeliveryRefConflict
	err := gitops.PushDeliveryRef(ctx, repo, mirror, dlvID, sha2)
	if !errors.Is(err, gitops.ErrDeliveryRefConflict) {
		t.Fatalf("expected ErrDeliveryRefConflict on different SHA overwrite, got %v", err)
	}

	// 4. Deleting the delivery ref -> rejected by pre-receive
	delCmd := exec.Command("git", "push", mirror, ":refs/orbit/deliveries/"+dlvID)
	delCmd.Dir = repo
	if out, err := delCmd.CombinedOutput(); err == nil {
		t.Fatalf("expected delete of delivery ref to be rejected, succeeded: %s", string(out))
	}

	// 5. Pushing to non-delivery ref (refs/heads/main) -> rejected by pre-receive
	branchCmd := exec.Command("git", "push", mirror, "HEAD:refs/heads/main")
	branchCmd.Dir = repo
	if out, err := branchCmd.CombinedOutput(); err == nil {
		t.Fatalf("expected push to refs/heads/main to be rejected, succeeded: %s", string(out))
	}
}

func TestExportFrozenSnapshot_DeterministicIDAndPermissions(t *testing.T) {
	ctx := context.Background()
	repo, _ := initTestRepo(t)
	mirror := filepath.Join(t.TempDir(), "mirror.git")

	// Add a Ryugi document artifact to the repo
	artDir := filepath.Join(repo, ".ryugi", "artifacts", "document", "v1-spec")
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artDir, "doc.xml"), []byte("<doc><h1 id=\"h1\">Spec</h1></doc>\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pkg1, err := gitops.PackageCheckpoint(ctx, gitops.PackageOptions{
		RepoDir:    repo,
		DeliveryID: "dlv_01SNAPSHOT000000000001",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gitops.PushDeliveryRef(ctx, repo, mirror, pkg1.DeliveryID, pkg1.HeadSHA); err != nil {
		t.Fatal(err)
	}

	pkg2, err := gitops.PackageCheckpoint(ctx, gitops.PackageOptions{
		RepoDir:    repo,
		DeliveryID: "dlv_01SNAPSHOT000000000002",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gitops.PushDeliveryRef(ctx, repo, mirror, pkg2.DeliveryID, pkg2.HeadSHA); err != nil {
		t.Fatal(err)
	}

	snapRoot := t.TempDir()
	snap1 := filepath.Join(snapRoot, pkg1.DeliveryID)
	snap2 := filepath.Join(snapRoot, pkg2.DeliveryID)
	t.Cleanup(func() {
		_ = gitops.RemoveFrozenDir(snap1)
		_ = gitops.RemoveFrozenDir(snap2)
	})

	exp1, err := gitops.ExportFrozenSnapshot(ctx, mirror, pkg1.HeadSHA, pkg1.DeliveryID, snap1)
	if err != nil {
		t.Fatalf("ExportFrozenSnapshot v1 failed: %v", err)
	}
	exp2, err := gitops.ExportFrozenSnapshot(ctx, mirror, pkg2.HeadSHA, pkg2.DeliveryID, snap2)
	if err != nil {
		t.Fatalf("ExportFrozenSnapshot v2 failed: %v", err)
	}
	if len(exp1) != 1 || len(exp2) != 1 {
		t.Fatalf("expected 1 artifact in each snapshot, got %d and %d", len(exp1), len(exp2))
	}

	alnum21 := regexp.MustCompile(`^[A-Za-z0-9]{21}$`)
	if !alnum21.MatchString(exp1[0].DerivedArtifactID) || !alnum21.MatchString(exp2[0].DerivedArtifactID) {
		t.Fatalf("derived artifact IDs must be 21 [A-Za-z0-9] chars: %q vs %q", exp1[0].DerivedArtifactID, exp2[0].DerivedArtifactID)
	}
	if exp1[0].DerivedArtifactID == exp2[0].DerivedArtifactID {
		t.Fatalf("two distinct deliveries MUST NOT share the same .artifact-id: %s", exp1[0].DerivedArtifactID)
	}

	// Verify file permissions: source file 0444, artifact dir 0555, .ryugi/artifacts 0555, .state 0755
	docStat, err := os.Stat(filepath.Join(snap1, ".ryugi", "artifacts", "document", "v1-spec", "doc.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if docStat.Mode().Perm() != 0o444 {
		t.Fatalf("expected doc.xml mode 0444, got %04o", docStat.Mode().Perm())
	}
	artDirStat, err := os.Stat(filepath.Join(snap1, ".ryugi", "artifacts", "document", "v1-spec"))
	if err != nil {
		t.Fatal(err)
	}
	if artDirStat.Mode().Perm() != 0o555 {
		t.Fatalf("expected artifact dir mode 0555, got %04o", artDirStat.Mode().Perm())
	}
	rootArtStat, err := os.Stat(filepath.Join(snap1, ".ryugi", "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	if rootArtStat.Mode().Perm() != 0o555 {
		t.Fatalf("expected .ryugi/artifacts mode 0555, got %04o", rootArtStat.Mode().Perm())
	}
	stateStat, err := os.Stat(filepath.Join(snap1, ".ryugi", "artifacts", ".state"))
	if err != nil {
		t.Fatal(err)
	}
	if stateStat.Mode().Perm() != 0o755 {
		t.Fatalf("expected .ryugi/artifacts/.state mode 0755, got %04o", stateStat.Mode().Perm())
	}

	// Verify writing inside .state succeeds while replacing doc.xml fails (non-root)
	if os.Geteuid() != 0 {
		if err := os.WriteFile(filepath.Join(snap1, ".ryugi", "artifacts", "document", "v1-spec", "tamper.txt"), []byte("no"), 0o644); err == nil {
			t.Fatalf("expected writing into frozen 0555 artifact dir to fail")
		}
	}
	if err := os.WriteFile(filepath.Join(snap1, ".ryugi", "artifacts", ".state", "probe.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("expected writing into 0755 .state dir to succeed: %v", err)
	}
}

func TestPackageCheckpoint_DirtySubmoduleRejected(t *testing.T) {
	ctx := context.Background()
	subRepo, _ := initTestRepo(t)
	mainRepo, _ := initTestRepo(t)

	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=Tester",
		"GIT_AUTHOR_EMAIL=tester@example.com",
		"GIT_COMMITTER_NAME=Tester",
		"GIT_COMMITTER_EMAIL=tester@example.com",
	)
	runCmd(t, mainRepo, env, "git", "-c", "protocol.file.allow=always", "submodule", "add", subRepo, "deps/sub")
	runCmd(t, mainRepo, env, "git", "commit", "-m", "add submodule")

	// Advance commit inside submodule so mainRepo sees '+' dirty submodule
	subDir := filepath.Join(mainRepo, "deps", "sub")
	if err := os.WriteFile(filepath.Join(subDir, "extra.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, subDir, env, "git", "add", "extra.txt")
	runCmd(t, subDir, env, "git", "commit", "-m", "advance sub")

	_, err := gitops.PackageCheckpoint(ctx, gitops.PackageOptions{
		RepoDir:    mainRepo,
		DeliveryID: "dlv_01SUBMODULE00000000001",
	})
	if !errors.Is(err, gitops.ErrDirtySubmodule) {
		t.Fatalf("expected ErrDirtySubmodule, got %v", err)
	}
}

