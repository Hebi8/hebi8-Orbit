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

func testGitEnv() []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME=Tester",
		"GIT_AUTHOR_EMAIL=tester@example.com",
		"GIT_COMMITTER_NAME=Tester",
		"GIT_COMMITTER_EMAIL=tester@example.com",
	)
}

func initTestRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	env := testGitEnv()
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
		t.Fatalf("expected 3 changed files, got %d", pkg.Manifest.FilesChanged)
	}

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

	// Commit normally -> working tree clean -> passing ReviewBaseSHA reuses HEAD AND yields non-empty diff
	env := testGitEnv()
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

func TestPackageCheckpoint_UnicodePathsAndSubdirectoryInvocation(t *testing.T) {
	ctx := context.Background()
	repo, _ := initTestRepo(t)
	mirror := filepath.Join(t.TempDir(), "mirror.git")
	env := testGitEnv()

	// Create Chinese/non-ASCII path with spaces and glob metacharacters, plus a subdirectory
	subDir := filepath.Join(repo, "设计稿", "子目录")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	origFile := filepath.Join(subDir, "首页 方案[v1].md")
	if err := os.WriteFile(origFile, []byte("line one\nline two\nline three\nline four\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, repo, env, "git", "add", "-A")
	runCmd(t, repo, env, "git", "commit", "-m", "add unicode file")
	baseSHA := runCmd(t, repo, env, "git", "rev-parse", "HEAD")

	// Rename unicode file and edit a line, plus add a root-level dirty file
	renamedFile := filepath.Join(subDir, "首页 方案[v2].md")
	if err := os.Rename(origFile, renamedFile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(renamedFile, []byte("line one\nline two updated\nline three\nline four\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "根目录变更.txt"), []byte("from root\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Invoke PackageCheckpoint from nested subdirectory -> must package whole repo cleanly!
	pkg, err := gitops.PackageCheckpoint(ctx, gitops.PackageOptions{
		RepoDir:       subDir,
		DeliveryID:    "dlv_01UNICODE000000000001",
		ReviewBaseSHA: baseSHA,
	})
	if err != nil {
		t.Fatalf("PackageCheckpoint from subdir failed: %v", err)
	}
	if pkg.RepoRoot != repo {
		t.Fatalf("expected RepoRoot=%s, got %s", repo, pkg.RepoRoot)
	}
	if pkg.Manifest.FilesChanged != 2 {
		t.Fatalf("expected 2 changed files (renamed unicode + root file), got %d", pkg.Manifest.FilesChanged)
	}

	if err := gitops.PushDeliveryRef(ctx, subDir, mirror, pkg.DeliveryID, pkg.HeadSHA); err != nil {
		t.Fatalf("PushDeliveryRef from subdir failed: %v", err)
	}

	targetRel := "设计稿/子目录/首页 方案[v2].md"
	files, diffText, err := gitops.ComputeMirrorDiff(ctx, mirror, baseSHA, pkg.HeadSHA, targetRel)
	if err != nil {
		t.Fatalf("ComputeMirrorDiff literal unicode path failed: %v", err)
	}
	foundRename := false
	for _, f := range files {
		if f.Path == targetRel && f.Status == "renamed" && f.OldPath == "设计稿/子目录/首页 方案[v1].md" {
			foundRename = true
		}
	}
	if !foundRename {
		t.Fatalf("expected renamed unicode file stat, got %+v", files)
	}
	if !strings.Contains(diffText, "rename from ") || !strings.Contains(diffText, "rename to ") || !strings.Contains(diffText, "line two updated") || strings.Contains(diffText, "from root") {
		t.Fatalf("literal single-file unicode rename diff unexpected:\n%s", diffText)
	}

	// Verify filenames with leading/trailing spaces survive end-to-end without trimming
	spacedFile := " leading space .txt"
	if err := os.WriteFile(filepath.Join(repo, spacedFile), []byte("spaced content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pkgSp, err := gitops.PackageCheckpoint(ctx, gitops.PackageOptions{
		RepoDir:       repo,
		DeliveryID:    "dlv_01SPACED000000000001",
		ReviewBaseSHA: baseSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gitops.PushDeliveryRef(ctx, repo, mirror, pkgSp.DeliveryID, pkgSp.HeadSHA); err != nil {
		t.Fatal(err)
	}
	_, spDiff, err := gitops.ComputeMirrorDiff(ctx, mirror, baseSHA, pkgSp.HeadSHA, spacedFile)
	if err != nil || !strings.Contains(spDiff, "spaced content") {
		t.Fatalf("expected leading/trailing spaced filename diff, err=%v diff=%s", err, spDiff)
	}
}

func TestPackageCheckpoint_AttachmentCollision(t *testing.T) {
	ctx := context.Background()
	repo, _ := initTestRepo(t)

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
	env := testGitEnv()

	if err := os.WriteFile(filepath.Join(repo, "second.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, repo, env, "git", "add", "second.txt")
	runCmd(t, repo, env, "git", "commit", "-m", "second commit")
	sha2 := runCmd(t, repo, env, "git", "rev-parse", "HEAD")

	dlvID := "dlv_01PRERECEIVE0000000001"

	if err := gitops.PushDeliveryRef(ctx, repo, mirror, dlvID, sha1); err != nil {
		t.Fatalf("first push failed: %v", err)
	}
	if err := gitops.PushDeliveryRef(ctx, repo, mirror, dlvID, sha1); err != nil {
		t.Fatalf("idempotent same-SHA push failed: %v", err)
	}
	err := gitops.PushDeliveryRef(ctx, repo, mirror, dlvID, sha2)
	if !errors.Is(err, gitops.ErrDeliveryRefConflict) {
		t.Fatalf("expected ErrDeliveryRefConflict on different SHA overwrite, got %v", err)
	}
	delCmd := exec.Command("git", "push", mirror, ":refs/orbit/deliveries/"+dlvID)
	delCmd.Dir = repo
	if out, err := delCmd.CombinedOutput(); err == nil {
		t.Fatalf("expected delete of delivery ref to be rejected, succeeded: %s", string(out))
	}
	branchCmd := exec.Command("git", "push", mirror, "HEAD:refs/heads/main")
	branchCmd.Dir = repo
	if out, err := branchCmd.CombinedOutput(); err == nil {
		t.Fatalf("expected push to refs/heads/main to be rejected, succeeded: %s", string(out))
	}
}

func TestExportFrozenSnapshot_DeterministicIDPermissionsAndRecalibration(t *testing.T) {
	ctx := context.Background()
	repo, _ := initTestRepo(t)
	mirror := filepath.Join(t.TempDir(), "mirror.git")

	artDir := filepath.Join(repo, ".ryugi", "artifacts", "document", "v1-spec")
	if err := os.MkdirAll(filepath.Join(artDir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artDir, "doc.xml"), []byte("<doc><h1 id=\"h1\">Spec</h1></doc>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artDir, "assets", "pic.txt"), []byte("asset"), 0o644); err != nil {
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

	// Verify model: source files 0444, <artifactDir> 0755, nested subdirs 0555, <type> 0555, .ryugi/artifacts 0555, .state 0755
	checkPerm := func(p string, want os.FileMode) {
		t.Helper()
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if st.Mode().Perm() != want {
			t.Fatalf("%s perm=%04o want=%04o", p, st.Mode().Perm(), want)
		}
	}
	specDir := filepath.Join(snap1, ".ryugi", "artifacts", "document", "v1-spec")
	checkPerm(filepath.Join(specDir, "doc.xml"), 0o444)
	checkPerm(filepath.Join(specDir, ".artifact-id"), 0o444)
	checkPerm(filepath.Join(specDir, "assets"), 0o555)
	checkPerm(filepath.Join(specDir, "assets", "pic.txt"), 0o444)
	checkPerm(specDir, 0o755)
	checkPerm(filepath.Join(snap1, ".ryugi", "artifacts", "document"), 0o555)
	checkPerm(filepath.Join(snap1, ".ryugi", "artifacts"), 0o555)
	checkPerm(filepath.Join(snap1, ".ryugi", "artifacts", ".state"), 0o755)

	// Simulate an older snapshot with 0555 <artifactDir> and verify fast-path ExportFrozenSnapshot recalibrates it to 0755
	if err := os.Chmod(specDir, 0o555); err != nil {
		t.Fatal(err)
	}
	if _, err := gitops.ExportFrozenSnapshot(ctx, mirror, pkg1.HeadSHA, pkg1.DeliveryID, snap1); err != nil {
		t.Fatalf("fast-path recalibration failed: %v", err)
	}
	checkPerm(specDir, 0o755)
	if err := os.WriteFile(filepath.Join(specDir, "comments.json"), []byte(`{"version":1,"threads":[]}`), 0o644); err != nil {
		t.Fatalf("expected writing comments.json inside 0755 artifactDir to succeed: %v", err)
	}
}

func TestPackageCheckpoint_DirtySubmoduleHeadAdvanceAndWorkingTreeEdit(t *testing.T) {
	ctx := context.Background()
	subRepo, _ := initTestRepo(t)
	mainRepo, _ := initTestRepo(t)
	env := testGitEnv()

	runCmd(t, mainRepo, env, "git", "-c", "protocol.file.allow=always", "submodule", "add", subRepo, "deps/sub")
	runCmd(t, mainRepo, env, "git", "commit", "-m", "add submodule")

	subDir := filepath.Join(mainRepo, "deps/sub")

	// Case 1: Submodule HEAD unchanged, but working tree has uncommitted modification -> MUST be rejected!
	if err := os.WriteFile(filepath.Join(subDir, "README.md"), []byte("dirty working tree in submodule\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := gitops.PackageCheckpoint(ctx, gitops.PackageOptions{
		RepoDir:    mainRepo,
		DeliveryID: "dlv_01SUBDIRTYWT00000001",
	})
	if !errors.Is(err, gitops.ErrDirtySubmodule) {
		t.Fatalf("expected ErrDirtySubmodule for uncommitted submodule working tree edit, got %v", err)
	}

	// Case 1b: Even when submodule has status.showUntrackedFiles=no, an untracked file MUST be caught!
	runCmd(t, subDir, env, "git", "checkout", "--", "README.md")
	runCmd(t, subDir, env, "git", "config", "status.showUntrackedFiles", "no")
	if err := os.WriteFile(filepath.Join(subDir, "sneaky_untracked.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = gitops.PackageCheckpoint(ctx, gitops.PackageOptions{
		RepoDir:    mainRepo,
		DeliveryID: "dlv_01SUBUNTRK00000001",
	})
	if !errors.Is(err, gitops.ErrDirtySubmodule) {
		t.Fatalf("expected ErrDirtySubmodule for untracked file even with status.showUntrackedFiles=no, got %v", err)
	}

	// Case 2: Commit inside submodule so HEAD advances ('+' flag) -> MUST also be rejected!
	runCmd(t, subDir, env, "git", "add", "-A")
	runCmd(t, subDir, env, "git", "commit", "-m", "advance sub head")
	_, err = gitops.PackageCheckpoint(ctx, gitops.PackageOptions{
		RepoDir:    mainRepo,
		DeliveryID: "dlv_01SUBHEADADV00000001",
	})
	if !errors.Is(err, gitops.ErrDirtySubmodule) {
		t.Fatalf("expected ErrDirtySubmodule for advanced submodule HEAD, got %v", err)
	}
}

func TestPackageCheckpoint_UnregisteredNestedGitRepoRejected(t *testing.T) {
	ctx := context.Background()
	mainRepo, _ := initTestRepo(t)
	env := testGitEnv()

	// Create an unregistered nested git repository inside mainRepo (no .gitmodules entry)
	nestedDir := filepath.Join(mainRepo, "vendor", "nested-lib")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runCmd(t, nestedDir, env, "git", "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(nestedDir, "lib.go"), []byte("package lib\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, nestedDir, env, "git", "add", ".")
	runCmd(t, nestedDir, env, "git", "commit", "-m", "nested init")

	_, err := gitops.PackageCheckpoint(ctx, gitops.PackageOptions{
		RepoDir:    mainRepo,
		DeliveryID: "dlv_01NESTEDGIT000000001",
	})
	if !errors.Is(err, gitops.ErrUnregisteredSubmodule) {
		t.Fatalf("expected ErrUnregisteredSubmodule for unregistered nested .git dir, got %v", err)
	}
	if !strings.Contains(err.Error(), "vendor/nested-lib") {
		t.Fatalf("expected error to name offending path vendor/nested-lib, got: %v", err)
	}
}
