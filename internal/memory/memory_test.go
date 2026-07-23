package memory

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateAcceptsPlainFiles(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "ledgers"), 0o755)
	os.WriteFile(filepath.Join(dir, "events.md"), []byte("fact\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "ledgers", "x.md"), []byte("state\n"), 0o644)
	if err := Validate(dir); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
}

func TestValidateRejectsGitControlFiles(t *testing.T) {
	for _, name := range []string{".gitattributes", ".gitmodules", ".gitignore"} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644)
		if err := Validate(dir); err == nil {
			t.Fatalf("expected rejection for %s", name)
		}
	}
}

func TestValidateRejectsSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.md")
	os.WriteFile(target, []byte("x"), 0o644)
	if err := os.Symlink(target, filepath.Join(dir, "link.md")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if err := Validate(dir); err == nil {
		t.Fatal("expected rejection for symlink")
	}
}

func TestValidateRejectsOversizedFile(t *testing.T) {
	dir := t.TempDir()
	big := make([]byte, maxFile+1)
	os.WriteFile(filepath.Join(dir, "big.md"), big, 0o644)
	if err := Validate(dir); err == nil {
		t.Fatal("expected rejection for oversized file")
	}
}

func TestRestoreFileDiscardsStagedChange(t *testing.T) {
	repo := t.TempDir()
	wt := WorktreePath(repo)
	os.MkdirAll(wt, 0o755)
	os.WriteFile(filepath.Join(wt, "events.md"), []byte("base\n"), 0o644)
	staging := t.TempDir()

	// A staged edit is undone: the worktree copy wins.
	os.WriteFile(filepath.Join(staging, "events.md"), []byte("base\n- sneaky event\n"), 0o644)
	changed, err := RestoreFile(repo, staging, "events.md")
	if err != nil || !changed {
		t.Fatalf("edited file: changed=%v err=%v, want true nil", changed, err)
	}
	if got, _ := os.ReadFile(filepath.Join(staging, "events.md")); string(got) != "base\n" {
		t.Fatalf("staged events.md = %q, want worktree copy restored", got)
	}

	// An untouched file reports no change.
	changed, err = RestoreFile(repo, staging, "events.md")
	if err != nil || changed {
		t.Fatalf("untouched file: changed=%v err=%v, want false nil", changed, err)
	}

	// A staged deletion is undone too -- import would otherwise delete it.
	os.Remove(filepath.Join(staging, "events.md"))
	changed, err = RestoreFile(repo, staging, "events.md")
	if err != nil || !changed {
		t.Fatalf("deleted file: changed=%v err=%v, want true nil", changed, err)
	}
	if _, err := os.Stat(filepath.Join(staging, "events.md")); err != nil {
		t.Fatal("staged events.md not restored after deletion")
	}

	// A file the worktree never had must not be created through staging.
	os.WriteFile(filepath.Join(staging, "novel.md"), []byte("x\n"), 0o644)
	changed, err = RestoreFile(repo, staging, "novel.md")
	if err != nil || !changed {
		t.Fatalf("created file: changed=%v err=%v, want true nil", changed, err)
	}
	if _, err := os.Stat(filepath.Join(staging, "novel.md")); !os.IsNotExist(err) {
		t.Fatal("staged novel.md should have been removed")
	}
}

// A second clone (a new container generation) must adopt the existing memory
// branch from origin instead of minting a fresh root.
func TestEnsureWorktreeAdoptsOriginBranch(t *testing.T) {
	base := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	bare := filepath.Join(base, "origin.git")
	run(base, "git", "init", "-q", "-b", "main", "--bare", bare)

	// Generation 1: create memory, write a fact, push.
	a := filepath.Join(base, "a")
	run(base, "git", "clone", "-q", bare, a)
	os.WriteFile(filepath.Join(a, "x.txt"), []byte("x"), 0o644)
	run(a, "git", "-c", "user.name=t", "-c", "user.email=t@t", "add", "-A")
	run(a, "git", "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "main")
	run(a, "git", "push", "-q", "origin", "main")
	if err := EnsureWorktree(a); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(a, "memory", "events.md"), []byte("generation one fact\n"), 0o644)
	if _, err := Commit(a, "Fact from generation one"); err != nil {
		t.Fatal(err)
	}
	if err := Push(a); err != nil {
		t.Fatal(err)
	}

	// Generation 2: fresh clone (no local memory branch), must adopt.
	b := filepath.Join(base, "b")
	run(base, "git", "clone", "-q", bare, b)
	if err := EnsureWorktree(b); err != nil {
		t.Fatal(err)
	}
	log := run(filepath.Join(b, "memory"), "git", "log", "--oneline")
	if !strings.Contains(log, "Fact from generation one") {
		t.Fatalf("generation two did not adopt origin history: %q", log)
	}
	if got := strings.Count(log, "Memory branch root"); got != 1 {
		t.Fatalf("expected exactly 1 root commit, got %d: %q", got, log)
	}
	events, _ := os.ReadFile(filepath.Join(b, "memory", "events.md"))
	if !strings.Contains(string(events), "generation one fact") {
		t.Fatalf("adopted events missing: %q", events)
	}
}
