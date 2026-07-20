package memory

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// twoClones builds the #4 harness: a bare origin and two independent clones
// (generations / machines) with memory materialized in each.
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
	if err := EnsureWorktree(a); err != nil {
		t.Fatal(err)
	}
	if err := Push(a); err != nil {
		t.Fatal(err)
	}

	b = filepath.Join(base, "b")
	gitT(t, base, "clone", "-q", bare, b)
	if err := EnsureWorktree(b); err != nil {
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

func writeMemory(t *testing.T, clone, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(clone, "memory", name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSyncFastForwardsWhenBehind(t *testing.T) {
	a, b := twoClones(t)
	writeMemory(t, a, "worklog.md", "fact from a\n")
	if _, err := Commit(a, "a fact"); err != nil {
		t.Fatal(err)
	}
	if err := Push(a); err != nil {
		t.Fatal(err)
	}

	rep := Sync(b)
	if !rep.Adopted || rep.Conflict || rep.Rewritten {
		t.Fatalf("expected clean adoption, got %+v", rep)
	}
	if got, _ := os.ReadFile(filepath.Join(b, "memory", "worklog.md")); !strings.Contains(string(got), "fact from a") {
		t.Fatalf("b did not adopt a's fact: %q", got)
	}
}

func TestSyncRebasesDivergedHistories(t *testing.T) {
	a, b := twoClones(t)
	// Human curation on a, agent commit on b, both from the same tip.
	writeMemory(t, a, "intentions.md", "curated by human\n")
	if _, err := Commit(a, "human curation"); err != nil {
		t.Fatal(err)
	}
	if err := Push(a); err != nil {
		t.Fatal(err)
	}
	writeMemory(t, b, "worklog.md", "agent fact\n")
	if _, err := Commit(b, "agent fact"); err != nil {
		t.Fatal(err)
	}

	rep := Sync(b)
	if !rep.Adopted || rep.Conflict {
		t.Fatalf("expected rebase adoption, got %+v", rep)
	}
	if err := Push(b); err != nil {
		t.Fatalf("push after rebase should fast-forward: %v", err)
	}
	log := gitT(t, filepath.Join(b, "memory"), "log", "--oneline")
	if !strings.Contains(log, "human curation") || !strings.Contains(log, "agent fact") {
		t.Fatalf("both lines of history should survive: %q", log)
	}
}

func TestSyncRefusesRewrittenHistory(t *testing.T) {
	a, b := twoClones(t)
	// b must have seen the remote once so a rewrite is detectable.
	if rep := Sync(b); rep.Rewritten || rep.Conflict {
		t.Fatalf("baseline sync failed: %+v", rep)
	}
	// a force-rewrites the memory branch (attacker or confused human).
	wtA := filepath.Join(a, "memory")
	root := gitT(t, wtA, "rev-list", "--max-parents=0", "HEAD")
	gitT(t, wtA, "reset", "-q", "--hard", root)
	writeMemory(t, a, "worklog.md", "history rewritten\n")
	gitT(t, wtA, "add", "-A")
	gitT(t, wtA, "commit", "-qm", "rewritten")
	gitT(t, wtA, "push", "-q", "--force", "origin", "memory")

	rep := Sync(b)
	if !rep.Rewritten {
		t.Fatalf("expected rewrite refusal, got %+v", rep)
	}
	if got, _ := os.ReadFile(filepath.Join(b, "memory", "worklog.md")); strings.Contains(string(got), "rewritten") {
		t.Fatalf("b adopted rewritten content: %q", got)
	}
}

func TestSyncReportsConflictAndAborts(t *testing.T) {
	a, b := twoClones(t)
	writeMemory(t, a, "worklog.md", "line from a\n")
	if _, err := Commit(a, "a edit"); err != nil {
		t.Fatal(err)
	}
	if err := Push(a); err != nil {
		t.Fatal(err)
	}
	writeMemory(t, b, "worklog.md", "conflicting line from b\n")
	if _, err := Commit(b, "b edit"); err != nil {
		t.Fatal(err)
	}

	rep := Sync(b)
	if !rep.Conflict {
		t.Fatalf("expected conflict report, got %+v", rep)
	}
	// The rebase must have been aborted: worktree clean, still functional.
	if status := gitT(t, filepath.Join(b, "memory"), "status", "--porcelain"); status != "" {
		t.Fatalf("worktree left dirty after aborted rebase: %q", status)
	}
}

func TestLeaseCASPreventsRaces(t *testing.T) {
	a, b := twoClones(t)
	shaA, err := WriteLease(a, "instance-a", time.Now(), "")
	if err != nil {
		t.Fatal(err)
	}
	// b races with a stale expectation ("no lease exists"): must lose.
	if _, err := WriteLease(b, "instance-b", time.Now(), ""); err == nil {
		t.Fatal("CAS should reject a write against a stale expectation")
	}
	// b reads the truth and takes over against the correct token: must win.
	lease, err := ReadLease(b)
	if err != nil || lease == nil || lease.Holder != "instance-a" || lease.SHA != shaA {
		t.Fatalf("unexpected lease read: %+v err=%v", lease, err)
	}
	if _, err := WriteLease(b, "instance-b", time.Now(), lease.SHA); err != nil {
		t.Fatalf("CAS with correct token should succeed: %v", err)
	}
	if lease, _ = ReadLease(a); lease == nil || lease.Holder != "instance-b" {
		t.Fatalf("takeover not visible: %+v", lease)
	}
	ReleaseLease(a)
	if lease, _ = ReadLease(b); lease != nil {
		t.Fatalf("release should remove the lease: %+v", lease)
	}
}
