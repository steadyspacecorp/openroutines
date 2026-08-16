package knowledge

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Builds the #4 harness: a bare origin and two independent clones
// (generations / machines) with knowledge materialized in each.
func twoClones(t *testing.T) (a, b string) {
	t.Helper()
	base := t.TempDir()
	bare := filepath.Join(base, "origin.git")
	gitT(t, base, "init", "-q", "-b", "main", "--bare", bare)

	a = filepath.Join(base, "a")
	gitT(t, base, "clone", "-q", bare, a)
	os.WriteFile(filepath.Join(a, "x.txt"), []byte("x"), 0o644)
	gitT(t, a, "add", "-A")
	gitT(t, a, "commit", "-qm", "main")
	gitT(t, a, "push", "-q", "origin", "main")
	if err := NewStore(a).Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := NewStore(a).Push(); err != nil {
		t.Fatal(err)
	}

	b = filepath.Join(base, "b")
	gitT(t, base, "clone", "-q", bare, b)
	if err := NewStore(b).Ensure(); err != nil {
		t.Fatal(err)
	}
	return a, b
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "protocol.file.allow=always"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeKnowledge(t *testing.T, clone, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(clone, "knowledge", name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSyncFastForwardsWhenBehind(t *testing.T) {
	a, b := twoClones(t)
	writeKnowledge(t, a, "events.md", "fact from a\n")
	if _, err := NewStore(a).Commit("a fact"); err != nil {
		t.Fatal(err)
	}
	if err := NewStore(a).Push(); err != nil {
		t.Fatal(err)
	}

	rep := NewStore(b).Sync()
	if !rep.Adopted || rep.Conflict || rep.Rewritten {
		t.Fatalf("expected clean adoption, got %+v", rep)
	}
	if got, _ := os.ReadFile(filepath.Join(b, "knowledge", "events.md")); !strings.Contains(string(got), "fact from a") {
		t.Fatalf("b did not adopt a's fact: %q", got)
	}
}

func TestSyncRebasesDivergedHistories(t *testing.T) {
	a, b := twoClones(t)
	// Human curation on a, agent commit on b, both from the same tip.
	writeKnowledge(t, a, "tasks.md", "curated by human\n")
	if _, err := NewStore(a).Commit("human curation"); err != nil {
		t.Fatal(err)
	}
	if err := NewStore(a).Push(); err != nil {
		t.Fatal(err)
	}
	writeKnowledge(t, b, "events.md", "agent fact\n")
	if _, err := NewStore(b).Commit("agent fact"); err != nil {
		t.Fatal(err)
	}

	rep := NewStore(b).Sync()
	if !rep.Adopted || rep.Conflict {
		t.Fatalf("expected rebase adoption, got %+v", rep)
	}
	if err := NewStore(b).Push(); err != nil {
		t.Fatalf("push after rebase should fast-forward: %v", err)
	}
	log := gitT(t, filepath.Join(b, "knowledge"), "log", "--oneline")
	if !strings.Contains(log, "human curation") || !strings.Contains(log, "agent fact") {
		t.Fatalf("both lines of history should survive: %q", log)
	}
}

func TestSyncRefusesRewrittenHistory(t *testing.T) {
	a, b := twoClones(t)
	// b must have seen the remote once so a rewrite is detectable.
	if rep := NewStore(b).Sync(); rep.Rewritten || rep.Conflict {
		t.Fatalf("baseline sync failed: %+v", rep)
	}
	// a force-rewrites the knowledge branch (attacker or confused human).
	wtA := filepath.Join(a, "knowledge")
	root := gitT(t, wtA, "rev-list", "--max-parents=0", "HEAD")
	gitT(t, wtA, "reset", "-q", "--hard", root)
	writeKnowledge(t, a, "events.md", "history rewritten\n")
	gitT(t, wtA, "add", "-A")
	gitT(t, wtA, "commit", "-qm", "rewritten")
	gitT(t, wtA, "push", "-q", "--force", "origin", "knowledge")

	rep := NewStore(b).Sync()
	if !rep.Rewritten {
		t.Fatalf("expected rewrite refusal, got %+v", rep)
	}
	if got, _ := os.ReadFile(filepath.Join(b, "knowledge", "events.md")); strings.Contains(string(got), "rewritten") {
		t.Fatalf("b adopted rewritten content: %q", got)
	}

	// The refusal must be durable, not one-shot: the first refusal's fetch
	// updated the remote-tracking ref, and an implementation keyed on it
	// would adopt the rewrite on the very next call. The accepted-ref
	// baseline keeps refusing.
	for i := 0; i < 3; i++ {
		if rep := NewStore(b).Sync(); !rep.Rewritten {
			t.Fatalf("sync call %d after rewrite: expected continued refusal, got %+v", i+2, rep)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(b, "knowledge", "events.md")); strings.Contains(string(got), "rewritten") {
		t.Fatalf("b adopted rewritten content on a repeat sync: %q", got)
	}
}

// A container replacement used to launder a rewrite: the fresh clone has no
// local baseline, so adoption took origin's branch wholesale. The accepted
// ref survives the replacement and must block adoption.
func TestEnsureWorktreeRefusesRewrittenHistoryAfterReplacement(t *testing.T) {
	a, b := twoClones(t)
	if rep := NewStore(b).Sync(); rep.Rewritten || rep.Conflict {
		t.Fatalf("baseline sync failed: %+v", rep)
	}
	// Rewrite origin's knowledge branch while "the container is down".
	wtA := filepath.Join(a, "knowledge")
	root := gitT(t, wtA, "rev-list", "--max-parents=0", "HEAD")
	gitT(t, wtA, "reset", "-q", "--hard", root)
	writeKnowledge(t, a, "events.md", "history rewritten\n")
	gitT(t, wtA, "add", "-A")
	gitT(t, wtA, "commit", "-qm", "rewritten")
	gitT(t, wtA, "push", "-q", "--force", "origin", "knowledge")

	// "Redeploy": a fresh clone with no local knowledge branch, like a new
	// container generation.
	base := filepath.Dir(a)
	c := filepath.Join(base, "c")
	gitT(t, base, "clone", "-q", filepath.Join(base, "origin.git"), c)
	err := NewStore(c).Ensure()
	if err == nil {
		t.Fatal("fresh clone adopted a rewritten knowledge branch")
	}
	if !strings.Contains(err.Error(), "does not descend") {
		t.Fatalf("unexpected refusal error: %v", err)
	}
}

func TestSyncReportsConflictAndAborts(t *testing.T) {
	a, b := twoClones(t)
	writeKnowledge(t, a, "events.md", "line from a\n")
	if _, err := NewStore(a).Commit("a edit"); err != nil {
		t.Fatal(err)
	}
	if err := NewStore(a).Push(); err != nil {
		t.Fatal(err)
	}
	writeKnowledge(t, b, "events.md", "conflicting line from b\n")
	if _, err := NewStore(b).Commit("b edit"); err != nil {
		t.Fatal(err)
	}

	rep := NewStore(b).Sync()
	if !rep.Conflict {
		t.Fatalf("expected conflict report, got %+v", rep)
	}
	// The rebase must have been aborted: worktree clean, still functional.
	if status := gitT(t, filepath.Join(b, "knowledge"), "status", "--porcelain"); status != "" {
		t.Fatalf("worktree left dirty after aborted rebase: %q", status)
	}
	if strings.Contains(rep.Detail, "hint:") || !strings.Contains(rep.Detail, "CONFLICT") {
		t.Fatalf("conflict detail should retain the conflict and omit stale Git hints: %q", rep.Detail)
	}
}
