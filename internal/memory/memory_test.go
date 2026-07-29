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

// Import must refuse to overwrite uncommitted human curation -- there is no
// reflog for edits that were never committed. Supervisor-owned paths are the
// attempt's own in-flight bookkeeping and do not gate.
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
	if err := At(repo).Ensure(); err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	os.WriteFile(filepath.Join(staging, "events.md"), []byte("- staged fact\n"), 0o644)

	// A human edit, uncommitted: refuse.
	wt := At(repo).Worktree()
	os.WriteFile(filepath.Join(wt, "tasks.md"), []byte("- [ ] mid-edit\n"), 0o644)
	if err := At(repo).Import(staging); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("expected dirty-worktree refusal, got %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(wt, "tasks.md")); string(got) != "- [ ] mid-edit\n" {
		t.Fatalf("refused import still modified the worktree: %q", got)
	}

	// Committed: import proceeds.
	if _, err := At(repo).Commit("human curation"); err != nil {
		t.Fatal(err)
	}
	if err := At(repo).Import(staging); err != nil {
		t.Fatalf("clean worktree should import: %v", err)
	}

	// The pipeline commits right after a successful import; mirror that.
	if _, err := At(repo).Commit("import"); err != nil {
		t.Fatal(err)
	}

	// Supervisor-owned dirt (this attempt's own bookkeeping) does not gate.
	os.MkdirAll(filepath.Join(wt, "state"), 0o755)
	os.WriteFile(filepath.Join(wt, "state", "r.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(staging, "events.md"), []byte("- staged fact\n- another\n"), 0o644)
	if err := At(repo).Import(staging); err != nil {
		t.Fatalf("supervisor-owned dirt must not block import: %v", err)
	}
}

func TestRestoreFileDiscardsStagedChange(t *testing.T) {
	repo := t.TempDir()
	wt := At(repo).Worktree()
	os.MkdirAll(wt, 0o755)
	os.WriteFile(filepath.Join(wt, "events.md"), []byte("base\n"), 0o644)
	staging := t.TempDir()

	// A staged edit is undone: the worktree copy wins.
	os.WriteFile(filepath.Join(staging, "events.md"), []byte("base\n- sneaky event\n"), 0o644)
	changed, err := At(repo).RestoreFile(staging, "events.md")
	if err != nil || !changed {
		t.Fatalf("edited file: changed=%v err=%v, want true nil", changed, err)
	}
	if got, _ := os.ReadFile(filepath.Join(staging, "events.md")); string(got) != "base\n" {
		t.Fatalf("staged events.md = %q, want worktree copy restored", got)
	}

	// An untouched file reports no change.
	changed, err = At(repo).RestoreFile(staging, "events.md")
	if err != nil || changed {
		t.Fatalf("untouched file: changed=%v err=%v, want false nil", changed, err)
	}

	// A staged deletion is undone too -- import would otherwise delete it.
	os.Remove(filepath.Join(staging, "events.md"))
	changed, err = At(repo).RestoreFile(staging, "events.md")
	if err != nil || !changed {
		t.Fatalf("deleted file: changed=%v err=%v, want true nil", changed, err)
	}
	if _, err := os.Stat(filepath.Join(staging, "events.md")); err != nil {
		t.Fatal("staged events.md not restored after deletion")
	}

	// A file the worktree never had must not be created through staging.
	os.WriteFile(filepath.Join(staging, "novel.md"), []byte("x\n"), 0o644)
	changed, err = At(repo).RestoreFile(staging, "novel.md")
	if err != nil || !changed {
		t.Fatalf("created file: changed=%v err=%v, want true nil", changed, err)
	}
	if _, err := os.Stat(filepath.Join(staging, "novel.md")); !os.IsNotExist(err) {
		t.Fatal("staged novel.md should have been removed")
	}
}

// Removing a routine must remove every per-routine state file, subdirectories
// included: a leftover trigger baseline means a re-created routine with the
// same name never fires on its first genuine change, and a leftover cursor
// replays or skips an inbox.
func TestRemoveRoutineStateCoversAllSubtrees(t *testing.T) {
	dir := t.TempDir()
	mem := At(dir)
	sd := mem.StateDir()
	for _, p := range []string{
		filepath.Join(sd, "x.json"),
		filepath.Join(sd, "triggers", "x.json"),
		filepath.Join(sd, "cursors", "x.json"),
		filepath.Join(sd, "y.json"),
		filepath.Join(sd, "triggers", "y.json"),
	} {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("{}"), 0o644)
	}

	removed, err := mem.RemoveRoutineState("x")
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 3 {
		t.Fatalf("expected 3 removed paths, got %v", removed)
	}
	for _, p := range []string{
		filepath.Join(sd, "x.json"),
		filepath.Join(sd, "triggers", "x.json"),
		filepath.Join(sd, "cursors", "x.json"),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s should be gone", p)
		}
	}
	for _, p := range []string{filepath.Join(sd, "y.json"), filepath.Join(sd, "triggers", "y.json")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s should survive: %v", p, err)
		}
	}

	// Idempotent, and quiet when there is no state at all.
	if removed, err := mem.RemoveRoutineState("x"); err != nil || len(removed) != 0 {
		t.Fatalf("second removal: %v, %v", removed, err)
	}
	if removed, err := At(t.TempDir()).RemoveRoutineState("x"); err != nil || removed != nil {
		t.Fatalf("no state dir: %v, %v", removed, err)
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
	if err := At(a).Ensure(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(a, "memory", "events.md"), []byte("generation one fact\n"), 0o644)
	if _, err := At(a).Commit("Fact from generation one"); err != nil {
		t.Fatal(err)
	}
	if err := At(a).Push(); err != nil {
		t.Fatal(err)
	}

	// Generation 2: fresh clone (no local memory branch), must adopt.
	b := filepath.Join(base, "b")
	run(base, "git", "clone", "-q", bare, b)
	if err := At(b).Ensure(); err != nil {
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
