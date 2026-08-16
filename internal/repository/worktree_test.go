package repository

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRepositoryAndWorktreeBindCommandsToTheirDirectories(t *testing.T) {
	root := t.TempDir()
	gitT(t, root, "init", "--quiet", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, root, "add", "README.md")
	gitT(t, root, "commit", "--quiet", "-m", "fixture")

	repo := Open(root)
	dir := filepath.Join(t.TempDir(), "knowledge")
	gitT(t, root, "worktree", "add", "--quiet", "-b", "knowledge", dir)
	worktree := repo.Worktree(dir)

	if branch, err := repo.Run("branch", "--show-current"); err != nil || branch != "main" {
		t.Fatalf("repository branch = %q, err=%v", branch, err)
	}
	if branch, err := worktree.Run("branch", "--show-current"); err != nil || branch != "knowledge" {
		t.Fatalf("worktree branch = %q, err=%v", branch, err)
	}
}
