package cli

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/knowledge"
)

func TestSyncPushCreatesMissingRemoteBranch(t *testing.T) {
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	runGit(t, base, "init", "-q", "--bare", origin)
	dir := statusAgent(t, nil)
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "user.email", "test@example.invalid")
	runGit(t, dir, "add", "openroutines.yml")
	runGit(t, dir, "commit", "-qm", "Agent")
	runGit(t, dir, "remote", "add", "origin", origin)
	t.Chdir(dir)

	out := capture(t, dir, func() {
		if code := cmdSync([]string{"--push"}); code != 0 {
			t.Fatalf("sync --push exited %d", code)
		}
	})
	if !strings.Contains(out, "published knowledge to new origin/knowledge") {
		t.Fatalf("sync did not report branch creation:\n%s", out)
	}
	refs := runGit(t, base, "--git-dir", origin, "show-ref", "--verify", "refs/heads/knowledge")
	if refs == "" {
		t.Fatal("sync --push did not create origin/knowledge")
	}
}

func TestSyncConflictPrintsRepeatableRecovery(t *testing.T) {
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	runGit(t, base, "init", "-q", "--bare", origin)

	a := statusAgent(t, nil)
	runGit(t, a, "init", "-q", "-b", "main")
	runGit(t, a, "config", "user.name", "Test")
	runGit(t, a, "config", "user.email", "test@example.invalid")
	runGit(t, a, "add", "openroutines.yml")
	runGit(t, a, "commit", "-qm", "Agent")
	runGit(t, a, "remote", "add", "origin", origin)
	storeA := knowledge.NewStore(a)
	if err := storeA.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := storeA.Push(); err != nil {
		t.Fatal(err)
	}

	b := filepath.Join(base, "b")
	runGit(t, base, "clone", "-q", origin, b)
	storeB := knowledge.NewStore(b)
	if err := storeB.Ensure(); err != nil {
		t.Fatal(err)
	}
	writeSyncKnowledge(t, storeA, "local\n", "Local edit")
	writeSyncKnowledge(t, storeB, "remote\n", "Remote edit")
	if err := storeB.Push(); err != nil {
		t.Fatal(err)
	}

	localTip := runGit(t, storeA.Worktree(), "rev-parse", "HEAD")
	code, out := captureSyncError(t, a, func() int { return cmdSync([]string{"--push"}) })
	if code == 0 {
		t.Fatalf("sync --push succeeded despite conflict:\n%s", out)
	}
	for _, want := range []string{
		"CONFLICT",
		"git -C knowledge rebase origin/knowledge",
		"git -C knowledge add -A",
		"git -C knowledge rebase --continue",
		"openroutines sync --push",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("conflict recovery missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "hint:") {
		t.Fatalf("conflict recovery retained stale Git hints:\n%s", out)
	}
	if tip := runGit(t, storeA.Worktree(), "rev-parse", "HEAD"); tip != localTip {
		t.Fatalf("aborted sync moved HEAD from %s to %s", localTip, tip)
	}
	if status := runGit(t, storeA.Worktree(), "status", "--porcelain"); status != "" {
		t.Fatalf("aborted sync left the worktree dirty: %q", status)
	}

	cmd := exec.Command("git", "rebase", "origin/knowledge")
	cmd.Dir = storeA.Worktree()
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_EDITOR=true")
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("manual rebase unexpectedly succeeded:\n%s", output)
	}
	if err := os.WriteFile(filepath.Join(storeA.Worktree(), "events.md"), []byte("resolved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, storeA.Worktree(), "add", "-A")
	runGit(t, storeA.Worktree(), "rebase", "--continue")
	if code := cmdSync([]string{"--push"}); code != 0 {
		t.Fatalf("sync --push after recovery exited %d", code)
	}
	localTip = runGit(t, storeA.Worktree(), "rev-parse", "HEAD")
	remoteTip := runGit(t, base, "--git-dir", origin, "rev-parse", "refs/heads/knowledge")
	if remoteTip != localTip {
		t.Fatalf("recovered knowledge was not published: local=%s remote=%s", localTip, remoteTip)
	}
}

func captureSyncError(t *testing.T, dir string, run func() int) (int, string) {
	t.Helper()
	t.Chdir(dir)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := os.Stderr
	os.Stderr = w
	code := run()
	os.Stderr = stderr
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return code, string(out)
}

func writeSyncKnowledge(t *testing.T, store *knowledge.Store, content, message string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(store.Worktree(), "events.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(message); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_EDITOR=true")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
