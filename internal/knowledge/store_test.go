package knowledge

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/logging/logtest"
	"github.com/steadyspacecorp/openroutines/internal/repository"
)

func deployKey(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "deploy-key")
	cmd := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate deploy key: %v: %s", err, out)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

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

	if removed, err := store.RemoveRoutineState("x"); err != nil || len(removed) != 0 {
		t.Fatalf("second removal: %v, %v", removed, err)
	}
	if removed, err := NewStore(t.TempDir()).RemoveRoutineState("x"); err != nil || removed != nil {
		t.Fatalf("no state dir: %v, %v", removed, err)
	}
}

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

func TestEnsureReplacesAWorktreeInvalidatedByFreshRepositoryMetadata(t *testing.T) {
	_, dir := twoClones(t)
	origin := gitT(t, dir, "remote", "get-url", "origin")
	orphaned := filepath.Join(dir, "knowledge", "local-note")
	if err := os.WriteFile(orphaned, []byte("preserve me"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	ssh := "#!/bin/sh\nwhile [ $# -gt 0 ]; do case \"$1\" in -o|-p|-i|-F|-l) shift 2;; -*) shift;; *) shift; break;; esac; done\nexec sh -c \"$1\"\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(ssh), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv(repository.EnvDeployKey, deployKey(t))
	t.Setenv(repository.EnvDeployKeyFile, "")

	if err := repository.Open(dir).Prepare("git@local:"+origin, true); err != nil {
		t.Fatal(err)
	}
	logs := logtest.Capture(t)
	if err := NewStore(dir).Ensure(); err != nil {
		t.Fatal(err)
	}
	logs.Expect("moved an unattached worktree aside")
	raw, err := os.ReadFile(filepath.Join(dir, "knowledge", "events.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "# Events") {
		t.Fatalf("knowledge was not reconstructed from origin: %q", raw)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "knowledge.orphaned-*", "local-note"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("local file was not preserved in one orphaned worktree: %v, %v", matches, err)
	}
	if raw, err := os.ReadFile(matches[0]); err != nil || string(raw) != "preserve me" {
		t.Fatalf("preserved local file = %q, %v", raw, err)
	}
}

func TestEnsureDoesNotRemoveWorktreeOnGitProbeFailure(t *testing.T) {
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main", dir)
	store := NewStore(dir)
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(store.Worktree(), "local-note")
	if err := os.WriteFile(sentinel, []byte("preserve me"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := store.Ensure(); err == nil {
		t.Fatal("Ensure succeeded despite the failed Git probe")
	}
	if raw, err := os.ReadFile(sentinel); err != nil || string(raw) != "preserve me" {
		t.Fatalf("worktree was modified after failed Git probe: %q, %v", raw, err)
	}
}

func TestDeployedRestartPreservesUnpushedKnowledge(t *testing.T) {
	_, dir := twoClones(t)
	origin := gitT(t, dir, "remote", "get-url", "origin")
	bin := t.TempDir()
	ssh := "#!/bin/sh\nwhile [ $# -gt 0 ]; do case \"$1\" in -o|-p|-i|-F|-l) shift 2;; -*) shift;; *) shift; break;; esac; done\nexec sh -c \"$1\"\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(ssh), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv(repository.EnvDeployKey, deployKey(t))
	t.Setenv(repository.EnvDeployKeyFile, "")

	configured := "git@local:" + origin
	if err := repository.Open(dir).Prepare(configured, true); err != nil {
		t.Fatal(err)
	}
	store := NewStore(dir)
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	want := "knowledge that has not reached origin\n"
	if err := os.WriteFile(filepath.Join(store.Worktree(), "events.md"), []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit("Unpushed knowledge"); err != nil {
		t.Fatal(err)
	}
	tip := gitT(t, store.Worktree(), "rev-parse", "HEAD")

	if err := repository.Open(dir).Prepare(configured, true); err != nil {
		t.Fatal(err)
	}
	restarted := NewStore(dir)
	if err := restarted.Ensure(); err != nil {
		t.Fatal(err)
	}
	if got := gitT(t, restarted.Worktree(), "rev-parse", "HEAD"); got != tip {
		t.Fatalf("knowledge tip changed from %s to %s", tip, got)
	}
	if got, err := os.ReadFile(filepath.Join(restarted.Worktree(), "events.md")); err != nil || string(got) != want {
		t.Fatalf("unpushed knowledge was not preserved: %q, %v", got, err)
	}
}

func TestEnsureRefusesWhenOriginUnreachable(t *testing.T) {
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main", dir)
	gitT(t, dir, "remote", "add", "origin", filepath.Join(dir, "does-not-exist.git"))

	logs := logtest.Capture(t)
	err := NewStore(dir).Ensure()
	if err == nil || !strings.Contains(err.Error(), "origin state is unknown") {
		t.Fatalf("Ensure error = %v", err)
	}
	for _, want := range []string{"does-not-exist.git", "Could not read from remote repository"} {
		if strings.Contains(err.Error(), want) {
			t.Fatalf("Ensure error inlined Git detail %q: %v", want, err)
		}
		logs.Expect("detail=", want)
	}
	if _, err := os.Stat(filepath.Join(dir, "knowledge")); !os.IsNotExist(err) {
		t.Fatalf("knowledge was created despite unreachable origin: %v", err)
	}
}
