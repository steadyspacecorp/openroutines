package repository

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoteRefDistinguishesMissingFromUnreachable(t *testing.T) {
	a, _ := leaseRepositories(t)
	repo := Open(a)
	if sha, exists, err := repo.RemoteRef("refs/heads/main"); err != nil || !exists || sha == "" {
		t.Fatalf("main: sha=%q exists=%v err=%v", sha, exists, err)
	}
	if sha, exists, err := repo.RemoteRef("refs/heads/missing"); err != nil || exists || sha != "" {
		t.Fatalf("missing: sha=%q exists=%v err=%v", sha, exists, err)
	}
	gitT(t, a, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
	if _, _, err := repo.RemoteRef("refs/heads/main"); err == nil {
		t.Fatal("unreachable origin reported as a missing ref")
	}
}

func TestCommitRelationsDistinguishNoFromGitFailure(t *testing.T) {
	a, _ := leaseRepositories(t)
	repo := Open(a)
	first := gitT(t, a, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(a, "second.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, a, "add", "second.txt")
	gitT(t, a, "commit", "--quiet", "-m", "second")
	second := gitT(t, a, "rev-parse", "HEAD")

	if sha, exists, err := repo.ResolveCommit(first); err != nil || !exists || sha != first {
		t.Fatalf("first commit: sha=%q exists=%v err=%v", sha, exists, err)
	}
	if _, exists, err := repo.ResolveCommit("missing"); err != nil || exists {
		t.Fatalf("missing commit: exists=%v err=%v", exists, err)
	}
	if yes, err := repo.IsAncestor(first, second); err != nil || !yes {
		t.Fatalf("first ancestor of second: yes=%v err=%v", yes, err)
	}
	if yes, err := repo.IsAncestor(second, first); err != nil || yes {
		t.Fatalf("second ancestor of first: yes=%v err=%v", yes, err)
	}
	if _, _, err := Open(t.TempDir()).ResolveCommit(first); err == nil {
		t.Fatal("invalid repository reported as a missing commit")
	}
}
