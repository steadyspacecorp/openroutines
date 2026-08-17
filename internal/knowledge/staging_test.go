package knowledge

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

func TestValidateRejectsHardLinks(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	os.WriteFile(outside, []byte("secret"), 0o644)
	if err := os.Link(outside, filepath.Join(dir, "alias.md")); err != nil {
		t.Skip("hard links unavailable")
	}
	if err := Validate(dir); err == nil || !strings.Contains(err.Error(), "hard link") {
		t.Fatalf("expected hard-link rejection, got %v", err)
	}
}

func TestStagedCopyNeverFollowsSymlinks(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(secret, []byte("SECRET"), 0o644)
	staging, wt := t.TempDir(), t.TempDir()
	if err := os.Symlink(secret, filepath.Join(staging, "events.md")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := copyStaged(staging, t.TempDir(), wt); err == nil {
		t.Error("expected the copy to refuse a symlinked staged file")
	}
	if raw, _ := os.ReadFile(filepath.Join(wt, "events.md")); strings.Contains(string(raw), "SECRET") {
		t.Fatalf("the symlink target was copied into the worktree: %q", raw)
	}
}

func TestStagedCopyRefusesHardLinks(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside.txt")
	os.WriteFile(outside, []byte("SECRET"), 0o644)
	staging, wt := t.TempDir(), t.TempDir()
	if err := os.Link(outside, filepath.Join(staging, "events.md")); err != nil {
		t.Skip("hard links unavailable")
	}
	if _, err := copyStaged(staging, t.TempDir(), wt); err == nil || !strings.Contains(err.Error(), "hard link") {
		t.Errorf("expected hard-link rejection, got %v", err)
	}
	if raw, _ := os.ReadFile(filepath.Join(wt, "events.md")); strings.Contains(string(raw), "SECRET") {
		t.Fatalf("the hard link's content was copied into the worktree: %q", raw)
	}
}

func TestStagedCopyRefusesPathsValidateWouldReject(t *testing.T) {
	for _, rel := range []string{".gitattributes", ".gitignore", filepath.Join(stateDirName, "sched.md"), "runs.jsonl"} {
		staging, wt := t.TempDir(), t.TempDir()
		os.MkdirAll(filepath.Dir(filepath.Join(staging, rel)), 0o755)
		os.WriteFile(filepath.Join(staging, rel), []byte("x\n"), 0o644)
		if _, err := copyStaged(staging, t.TempDir(), wt); err == nil {
			t.Errorf("%s: expected the copy to refuse it", rel)
		}
		if _, err := os.Stat(filepath.Join(wt, rel)); !os.IsNotExist(err) {
			t.Errorf("%s: copied into the worktree anyway", rel)
		}
	}
}

func TestStagedCopyRefusesOversizedFile(t *testing.T) {
	staging, wt := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(staging, "events.md"), make([]byte, maxFile+1), 0o644)
	if _, err := copyStaged(staging, t.TempDir(), wt); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected an oversize rejection, got %v", err)
	}
}

func TestStagedCopyRejectionLeavesTheWorktreeUntouched(t *testing.T) {
	staging, wt := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(wt, "events.md"), []byte("committed\n"), 0o644)
	os.WriteFile(filepath.Join(staging, "events.md"), []byte("committed\n- new\n"), 0o644)
	os.WriteFile(filepath.Join(staging, "tasks.md"), []byte("- [ ] new\n"), 0o644)

	if err := os.Symlink(filepath.Join(t.TempDir(), "secret.txt"), filepath.Join(staging, "zz.md")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := copyStaged(staging, t.TempDir(), wt); err == nil {
		t.Fatal("expected the copy to refuse the symlink")
	}
	if got, _ := os.ReadFile(filepath.Join(wt, "events.md")); string(got) != "committed\n" {
		t.Errorf("worktree events.md = %q, want the pre-import content", got)
	}
	if _, err := os.Stat(filepath.Join(wt, "tasks.md")); !os.IsNotExist(err) {
		t.Error("a file from the rejected tree landed in the worktree")
	}
}

func TestRestoreFileNeverWritesOutsideStaging(t *testing.T) {
	base := t.TempDir()
	os.WriteFile(filepath.Join(base, "events.md"), []byte("base events\n"), 0o644)
	staging := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	os.WriteFile(outside, []byte("do not touch\n"), 0o644)
	if err := os.Symlink(outside, filepath.Join(staging, "events.md")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := RestoreFile(staging, base, "events.md"); err == nil {
		t.Error("expected RestoreFile to refuse a symlinked staged path")
	}
	if got, _ := os.ReadFile(outside); string(got) != "do not touch\n" {
		t.Fatalf("wrote through the symlink: %q", got)
	}
}

func TestImportRefusesDirtyWorktree(t *testing.T) {
	repo := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	run("git", "init", "-q", "-b", "main", ".")
	if err := NewStore(repo).Ensure(); err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	os.WriteFile(filepath.Join(staging, "events.md"), []byte("- staged fact\n"), 0o644)

	wt := NewStore(repo).Worktree()
	os.WriteFile(filepath.Join(wt, "tasks.md"), []byte("- [ ] mid-edit\n"), 0o644)
	if _, err := NewStore(repo).Import(staging, t.TempDir()); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("expected dirty-worktree refusal, got %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(wt, "tasks.md")); string(got) != "- [ ] mid-edit\n" {
		t.Fatalf("refused import still modified the worktree: %q", got)
	}

	if _, err := NewStore(repo).Commit("human curation"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(repo).Import(staging, t.TempDir()); err != nil {
		t.Fatalf("clean worktree should import: %v", err)
	}

	if _, err := NewStore(repo).Commit("import"); err != nil {
		t.Fatal(err)
	}

	os.MkdirAll(filepath.Join(wt, "state"), 0o755)
	os.WriteFile(filepath.Join(wt, "state", "r.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(staging, "events.md"), []byte("- staged fact\n- another\n"), 0o644)
	if _, err := NewStore(repo).Import(staging, t.TempDir()); err != nil {
		t.Fatalf("supervisor-owned dirt must not block import: %v", err)
	}
}

func TestRestoreFileDiscardsStagedChange(t *testing.T) {
	base := t.TempDir()
	os.WriteFile(filepath.Join(base, "events.md"), []byte("base\n"), 0o644)
	staging := t.TempDir()

	os.WriteFile(filepath.Join(staging, "events.md"), []byte("base\n- sneaky event\n"), 0o644)
	changed, err := RestoreFile(staging, base, "events.md")
	if err != nil || !changed {
		t.Fatalf("edited file: changed=%v err=%v, want true nil", changed, err)
	}
	if got, _ := os.ReadFile(filepath.Join(staging, "events.md")); string(got) != "base\n" {
		t.Fatalf("staged events.md = %q, want base copy restored", got)
	}

	changed, err = RestoreFile(staging, base, "events.md")
	if err != nil || changed {
		t.Fatalf("untouched file: changed=%v err=%v, want false nil", changed, err)
	}

	os.Remove(filepath.Join(staging, "events.md"))
	changed, err = RestoreFile(staging, base, "events.md")
	if err != nil || !changed {
		t.Fatalf("deleted file: changed=%v err=%v, want true nil", changed, err)
	}
	if _, err := os.Stat(filepath.Join(staging, "events.md")); err != nil {
		t.Fatal("staged events.md not restored after deletion")
	}

	os.WriteFile(filepath.Join(staging, "novel.md"), []byte("x\n"), 0o644)
	changed, err = RestoreFile(staging, base, "novel.md")
	if err != nil || !changed {
		t.Fatalf("created file: changed=%v err=%v, want true nil", changed, err)
	}
	if _, err := os.Stat(filepath.Join(staging, "novel.md")); !os.IsNotExist(err) {
		t.Fatal("staged novel.md should have been removed")
	}
}

func TestImportThreeWayMerge(t *testing.T) {
	repo := t.TempDir()
	wt := NewStore(repo).Worktree()
	os.MkdirAll(wt, 0o755)
	staging, base := t.TempDir(), t.TempDir()
	write := func(dir, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write(base, "stale.md", "v1\n")
	write(staging, "stale.md", "v1\n")
	write(wt, "stale.md", "v1\n- concurrent\n")

	write(base, "mine.md", "a\n")
	write(staging, "mine.md", "a\nb\n")
	write(wt, "mine.md", "a\n")

	write(base, "both.md", "x\n")
	write(staging, "both.md", "x\ntheirs\n")
	write(wt, "both.md", "x\nours\n")

	write(base, "semantic.md", "status: idle\n")
	write(staging, "semantic.md", "status: away\n")
	write(wt, "semantic.md", "status: shipping\n")

	write(base, "gone.md", "old\n")
	write(wt, "gone.md", "old\n")

	write(base, "contested.md", "old\n")
	write(wt, "contested.md", "old\n- news\n")

	conflicted, err := NewStore(repo).Import(staging, base)
	if err != nil {
		t.Fatal(err)
	}
	got := func(name string) string {
		raw, _ := os.ReadFile(filepath.Join(wt, name))
		return string(raw)
	}
	if got("stale.md") != "v1\n- concurrent\n" {
		t.Errorf("stale.md regressed to the staged copy: %q", got("stale.md"))
	}
	if got("mine.md") != "a\nb\n" {
		t.Errorf("mine.md = %q, want the staged change", got("mine.md"))
	}
	if b := got("both.md"); !strings.Contains(b, "ours") || !strings.Contains(b, "theirs") {
		t.Errorf("both.md = %q, want both sides kept", b)
	}
	if len(conflicted) != 1 || conflicted[0].Path != "semantic.md" {
		t.Errorf("conflicted = %v, want semantic.md quarantined", conflicted)
	} else if raw, err := os.ReadFile(filepath.Join(wt, conflicted[0].Quarantine)); err != nil || string(raw) != "status: away\n" {
		t.Errorf("quarantine = %q, %v; want competing semantic edit", raw, err)
	}
	if got("semantic.md") != "status: shipping\n" {
		t.Errorf("semantic.md = %q, want last valid canonical version", got("semantic.md"))
	}
	if _, err := os.Stat(filepath.Join(wt, "gone.md")); !os.IsNotExist(err) {
		t.Error("gone.md should be deleted: the worktree still matched the base")
	}
	if got("contested.md") != "old\n- news\n" {
		t.Errorf("contested.md = %q, want the concurrent write kept over the deletion", got("contested.md"))
	}
}
