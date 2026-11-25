package gitops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestInspectDirectoryGitAndPlainFolder(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	// 1. Plain folder
	plainDir := filepath.Join(root, "designs")
	if err := os.MkdirAll(plainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	plainRes := InspectDirectory(ctx, plainDir, []string{root})
	if !plainRes.Valid || !plainRes.Exists || plainRes.IsGit {
		t.Fatalf("expected valid plain folder, got %+v", plainRes)
	}
	if plainRes.DisplayName != "designs" || plainRes.RepoIdentity != "folder:designs" {
		t.Fatalf("unexpected plain folder metadata: %+v", plainRes)
	}

	// 2. Git repository
	repoDir := filepath.Join(root, "demo-repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, string(out))
		}
	}
	runGit("init", "-b", "main")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")
	runGit("remote", "add", "origin", "https://github.com/example/demo-repo.git")
	if err := os.WriteFile(filepath.Join(repoDir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "init")

	gitRes := InspectDirectory(ctx, repoDir, []string{root})
	if !gitRes.Valid || !gitRes.Exists || !gitRes.IsGit {
		t.Fatalf("expected valid git repo, got %+v", gitRes)
	}
	if gitRes.DefaultBranch != "main" || gitRes.Dirty || gitRes.RepoIdentity != "github.com/example/demo-repo" || gitRes.RootCommit == "" {
		t.Fatalf("unexpected clean git metadata: %+v", gitRes)
	}

	// Dirty the working tree
	if err := os.WriteFile(filepath.Join(repoDir, "a.txt"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirtyRes := InspectDirectory(ctx, repoDir, []string{root})
	if !dirtyRes.Dirty {
		t.Fatalf("expected dirty=true, got %+v", dirtyRes)
	}

	// Shallow scan finds demo-repo without touching plainDir
	scanned, err := ScanGitSuggestions(ctx, []string{root})
	if err != nil {
		t.Fatalf("scan suggestions: %v", err)
	}
	if len(scanned) != 1 || scanned[0].RepoIdentity != "github.com/example/demo-repo" || scanned[0].Branch != "main" {
		t.Fatalf("unexpected scanned repos: %+v", scanned)
	}
}

func TestCaptureAndDiffFolderSnapshots(t *testing.T) {
	dir := t.TempDir()
	// Create initial files and ignored dirs
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node_modules", "pkg", "index.js"), []byte("ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "spec.md"), []byte("line1\nline2\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old.txt"), []byte("bye1\nbye2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	before, err := CaptureFolderSnapshot(dir)
	if err != nil {
		t.Fatalf("capture before: %v", err)
	}

	// Modify, add, delete, and mutate node_modules (must be skipped)
	if err := os.WriteFile(filepath.Join(dir, "spec.md"), []byte("line1\nline2-edited\nline3\nline4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.md"), []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "old.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node_modules", "pkg", "index.js"), []byte("changed-ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	after, err := CaptureFolderSnapshot(dir)
	if err != nil {
		t.Fatalf("capture after: %v", err)
	}

	// Ensure zero shadow .git created
	if _, err := os.Stat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
		t.Fatal("shadow .git must never be created")
	}

	diff := DiffFolderSnapshots(before, after)
	if len(diff.Files) != 3 {
		t.Fatalf("expected 3 changed files, got %+v", diff.Files)
	}
	// new.md (+3), old.txt (-2), spec.md (+2 -1) => total +5 -3
	if diff.TotalAdds != 5 || diff.TotalDels != 3 {
		t.Fatalf("expected +5 -3, got +%d -%d (%+v)", diff.TotalAdds, diff.TotalDels, diff.Files)
	}

	// ReadFolderFile containment check
	data, name, err := ReadFolderFile(dir, "new.md", []string{dir}, 1024)
	if err != nil || name != "new.md" || string(data) != "a\nb\nc\n" {
		t.Fatalf("read folder file failed: name=%q data=%q err=%v", name, string(data), err)
	}
	if _, _, err := ReadFolderFile(dir, "../escape.txt", []string{dir}, 1024); err == nil {
		t.Fatal("expected traversal rejection")
	}
}
