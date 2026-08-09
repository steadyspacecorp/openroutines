package knowledge

import (
	"github.com/steadyspacecorp/openroutines/internal/logging/logtest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Removing a routine must remove every per-routine state file, subdirectories
// included: a leftover trigger baseline means a re-created routine with the
// same name never fires on its first genuine change, and a leftover cursor
// replays or skips a change set.
func TestRemoveRoutineStateCoversAllSubtrees(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	sd := store.StateDir()
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

	removed, err := store.RemoveRoutineState("x")
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
	if removed, err := store.RemoveRoutineState("x"); err != nil || len(removed) != 0 {
		t.Fatalf("second removal: %v, %v", removed, err)
	}
	if removed, err := NewStore(t.TempDir()).RemoveRoutineState("x"); err != nil || removed != nil {
		t.Fatalf("no state dir: %v, %v", removed, err)
	}
}

// A second clone (a new container generation) must adopt the existing knowledge
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

	// Generation 1: create knowledge, write a fact, push.
	a := filepath.Join(base, "a")
	run(base, "git", "clone", "-q", bare, a)
	os.WriteFile(filepath.Join(a, "x.txt"), []byte("x"), 0o644)
	run(a, "git", "-c", "user.name=t", "-c", "user.email=t@t", "add", "-A")
	run(a, "git", "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "main")
	run(a, "git", "push", "-q", "origin", "main")
	if err := NewStore(a).Ensure(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(a, "knowledge", "events.md"), []byte("generation one fact\n"), 0o644)
	if _, err := NewStore(a).Commit("Fact from generation one"); err != nil {
		t.Fatal(err)
	}
	if err := NewStore(a).Push(); err != nil {
		t.Fatal(err)
	}

	// Generation 2: fresh clone (no local knowledge branch), must adopt.
	b := filepath.Join(base, "b")
	run(base, "git", "clone", "-q", bare, b)
	if err := NewStore(b).Ensure(); err != nil {
		t.Fatal(err)
	}
	log := run(filepath.Join(b, "knowledge"), "git", "log", "--oneline")
	if !strings.Contains(log, "Fact from generation one") {
		t.Fatalf("generation two did not adopt origin history: %q", log)
	}
	if got := strings.Count(log, "Knowledge branch root"); got != 1 {
		t.Fatalf("expected exactly 1 root commit, got %d: %q", got, log)
	}
	events, _ := os.ReadFile(filepath.Join(b, "knowledge", "events.md"))
	if !strings.Contains(string(events), "generation one fact") {
		t.Fatalf("adopted events missing: %q", events)
	}
}

// A container replaced during a transient origin outage must not mint a
// local root silently -- the resulting lineage would diverge from origin's
// with no trace of why. Ensure still self-heals (it must: origin may never
// come back), but it says so.
func TestEnsureWarnsWhenOriginUnreachable(t *testing.T) {
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main", dir)
	gitT(t, dir, "remote", "add", "origin", filepath.Join(dir, "does-not-exist.git"))

	logs := logtest.Capture(t)

	if err := NewStore(dir).Ensure(); err != nil {
		t.Fatal(err)
	}
	logs.Expect("could not reach origin")
	if _, err := os.Stat(filepath.Join(dir, "knowledge", ".git")); err != nil {
		t.Fatalf("knowledge worktree not created despite the unreachable origin: %v", err)
	}
}
