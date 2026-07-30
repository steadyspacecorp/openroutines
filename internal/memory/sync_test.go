package memory

import (
	"fmt"
	"net"
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
	if err := At(a).Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := At(a).Push(); err != nil {
		t.Fatal(err)
	}

	b = filepath.Join(base, "b")
	gitT(t, base, "clone", "-q", bare, b)
	if err := At(b).Ensure(); err != nil {
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

// blackhole is an origin that accepts connections and then says nothing --
// a partitioned network drops packets rather than refusing them, so the
// client waits on a reply that never comes. The URL is https so the stall
// happens in a child helper (git-remote-https), where it happens in
// production: a deadline that reached git alone would leave the helper
// holding the output pipe.
func blackhole(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		var held []net.Conn
		defer func() {
			for _, c := range held {
				c.Close()
			}
		}()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, conn)
		}
	}()
	return fmt.Sprintf("https://%s/memory.git", ln.Addr().String())
}

// Every tick makes several network calls. A blackholed origin must cost the
// tick a bounded wait, not park it until the TCP stack gives up -- and the
// wait must end with the transport, not with the pipe drain giving up on a
// helper the kill failed to reach.
func TestGitAbandonsABlackholedRemote(t *testing.T) {
	restore, restoreGrace := gitTimeout, gitKillGrace
	gitTimeout, gitKillGrace = 500*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { gitTimeout, gitKillGrace = restore, restoreGrace })

	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main", ".")

	remote := blackhole(t)
	done := make(chan error, 1)
	go func() {
		_, err := git(dir, "ls-remote", remote, "refs/heads/memory")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected the blackholed ls-remote to fail")
		}
		if !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("expected a timeout error, got %v", err)
		}
	// Comfortably past the deadline and its grace, and comfortably short of
	// the drain deadline: returning only once the drain expires would mean
	// the group kill never reached the helper holding the pipe.
	case <-time.After(gitDrainDeadline - time.Second):
		t.Fatal("git outlasted its deadline: the kill did not reach the stalled transport")
	}
}

func writeMemory(t *testing.T, clone, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(clone, "memory", name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSyncFastForwardsWhenBehind(t *testing.T) {
	a, b := twoClones(t)
	writeMemory(t, a, "events.md", "fact from a\n")
	if _, err := At(a).Commit("a fact"); err != nil {
		t.Fatal(err)
	}
	if err := At(a).Push(); err != nil {
		t.Fatal(err)
	}

	rep := At(b).Sync()
	if !rep.Adopted || rep.Conflict || rep.Rewritten {
		t.Fatalf("expected clean adoption, got %+v", rep)
	}
	if got, _ := os.ReadFile(filepath.Join(b, "memory", "events.md")); !strings.Contains(string(got), "fact from a") {
		t.Fatalf("b did not adopt a's fact: %q", got)
	}
}

func TestSyncRebasesDivergedHistories(t *testing.T) {
	a, b := twoClones(t)
	// Human curation on a, agent commit on b, both from the same tip.
	writeMemory(t, a, "tasks.md", "curated by human\n")
	if _, err := At(a).Commit("human curation"); err != nil {
		t.Fatal(err)
	}
	if err := At(a).Push(); err != nil {
		t.Fatal(err)
	}
	writeMemory(t, b, "events.md", "agent fact\n")
	if _, err := At(b).Commit("agent fact"); err != nil {
		t.Fatal(err)
	}

	rep := At(b).Sync()
	if !rep.Adopted || rep.Conflict {
		t.Fatalf("expected rebase adoption, got %+v", rep)
	}
	if err := At(b).Push(); err != nil {
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
	if rep := At(b).Sync(); rep.Rewritten || rep.Conflict {
		t.Fatalf("baseline sync failed: %+v", rep)
	}
	// a force-rewrites the memory branch (attacker or confused human).
	wtA := filepath.Join(a, "memory")
	root := gitT(t, wtA, "rev-list", "--max-parents=0", "HEAD")
	gitT(t, wtA, "reset", "-q", "--hard", root)
	writeMemory(t, a, "events.md", "history rewritten\n")
	gitT(t, wtA, "add", "-A")
	gitT(t, wtA, "commit", "-qm", "rewritten")
	gitT(t, wtA, "push", "-q", "--force", "origin", "memory")

	rep := At(b).Sync()
	if !rep.Rewritten {
		t.Fatalf("expected rewrite refusal, got %+v", rep)
	}
	if got, _ := os.ReadFile(filepath.Join(b, "memory", "events.md")); strings.Contains(string(got), "rewritten") {
		t.Fatalf("b adopted rewritten content: %q", got)
	}

	// The refusal must be durable, not one-shot: the first refusal's fetch
	// updated the remote-tracking ref, and an implementation keyed on it
	// would adopt the rewrite on the very next call. The accepted-ref
	// baseline keeps refusing.
	for i := 0; i < 3; i++ {
		if rep := At(b).Sync(); !rep.Rewritten {
			t.Fatalf("sync call %d after rewrite: expected continued refusal, got %+v", i+2, rep)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(b, "memory", "events.md")); strings.Contains(string(got), "rewritten") {
		t.Fatalf("b adopted rewritten content on a repeat sync: %q", got)
	}
}

// A container replacement used to launder a rewrite: the fresh clone has no
// local baseline, so adoption took origin's branch wholesale. The accepted
// ref survives the replacement and must block adoption.
func TestEnsureWorktreeRefusesRewrittenHistoryAfterReplacement(t *testing.T) {
	a, b := twoClones(t)
	if rep := At(b).Sync(); rep.Rewritten || rep.Conflict {
		t.Fatalf("baseline sync failed: %+v", rep)
	}
	// Rewrite origin's memory branch while "the container is down".
	wtA := filepath.Join(a, "memory")
	root := gitT(t, wtA, "rev-list", "--max-parents=0", "HEAD")
	gitT(t, wtA, "reset", "-q", "--hard", root)
	writeMemory(t, a, "events.md", "history rewritten\n")
	gitT(t, wtA, "add", "-A")
	gitT(t, wtA, "commit", "-qm", "rewritten")
	gitT(t, wtA, "push", "-q", "--force", "origin", "memory")

	// "Redeploy": a fresh clone with no local memory branch, like a new
	// container generation.
	base := filepath.Dir(a)
	c := filepath.Join(base, "c")
	gitT(t, base, "clone", "-q", filepath.Join(base, "origin.git"), c)
	err := At(c).Ensure()
	if err == nil {
		t.Fatal("fresh clone adopted a rewritten memory branch")
	}
	if !strings.Contains(err.Error(), "does not descend") {
		t.Fatalf("unexpected refusal error: %v", err)
	}
}

func TestSyncReportsConflictAndAborts(t *testing.T) {
	a, b := twoClones(t)
	writeMemory(t, a, "events.md", "line from a\n")
	if _, err := At(a).Commit("a edit"); err != nil {
		t.Fatal(err)
	}
	if err := At(a).Push(); err != nil {
		t.Fatal(err)
	}
	writeMemory(t, b, "events.md", "conflicting line from b\n")
	if _, err := At(b).Commit("b edit"); err != nil {
		t.Fatal(err)
	}

	rep := At(b).Sync()
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
	shaA, err := At(a).WriteLease("instance-a", time.Now(), "")
	if err != nil {
		t.Fatal(err)
	}
	// b races with a stale expectation ("no lease exists"): must lose.
	if _, err := At(b).WriteLease("instance-b", time.Now(), ""); err == nil {
		t.Fatal("CAS should reject a write against a stale expectation")
	}
	// b reads the truth and takes over against the correct token: must win.
	lease, err := At(b).ReadLease()
	if err != nil || lease == nil || lease.Holder != "instance-a" || lease.SHA != shaA {
		t.Fatalf("unexpected lease read: %+v err=%v", lease, err)
	}
	if _, err := At(b).WriteLease("instance-b", time.Now(), lease.SHA); err != nil {
		t.Fatalf("CAS with correct token should succeed: %v", err)
	}
	if lease, _ = At(a).ReadLease(); lease == nil || lease.Holder != "instance-b" {
		t.Fatalf("takeover not visible: %+v", lease)
	}
	// Release is ownership-checked: the stale instance a (whose last-written
	// SHA has been superseded by b's takeover) must NOT delete b's live lease.
	At(a).ReleaseLease(shaA)
	current, _ := At(a).ReadLease()
	if current == nil || current.Holder != "instance-b" {
		t.Fatalf("stale release deleted the live lease: %+v", current)
	}
	// The rightful holder releases with its own SHA: lease gone.
	At(b).ReleaseLease(current.SHA)
	if lease, _ = At(b).ReadLease(); lease != nil {
		t.Fatalf("owned release should remove the lease: %+v", lease)
	}
}
