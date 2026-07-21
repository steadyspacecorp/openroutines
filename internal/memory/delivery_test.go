package memory

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// deliveryFixture inits an agent repo with a materialized memory worktree.
func deliveryFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "-b", "main")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init: %v: %s", err, out)
	}
	if err := EnsureWorktree(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func appendMemory(t *testing.T, dir, file, line string) {
	t.Helper()
	p := filepath.Join(WorktreePath(dir), file)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

func TestChangesWalksCommitByCommit(t *testing.T) {
	dir := deliveryFixture(t)
	from, err := Head(dir)
	if err != nil {
		t.Fatal(err)
	}

	appendMemory(t, dir, "events.md", "- 2026-07-21 doc-drift: opened PR #1")
	if _, err := Commit(dir, "Run doc-drift (run_a): completed"); err != nil {
		t.Fatal(err)
	}
	appendMemory(t, dir, "events.md", "- 2026-07-21 a11y-sweep NO-OP: all clean")
	appendMemory(t, dir, "tasks.md", "- [ ] `task-20260721-1` decide the thing (source: doc-drift; added 2026-07-21)")
	// Ledger and supervisor-owned writes must never reach a consumer.
	appendMemory(t, dir, "ledgers/doc-drift.md", "- private cursor note")
	appendMemory(t, dir, "runs.jsonl", `{"run_id":"run_b"}`)
	if _, err := Commit(dir, "Run a11y-sweep (run_b): completed"); err != nil {
		t.Fatal(err)
	}

	through, err := Head(dir)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := Changes(dir, from, through)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("expected 2 commits of changes, got %d: %+v", len(changes), changes)
	}
	if changes[0].Subject != "Run doc-drift (run_a): completed" {
		t.Fatalf("changes must be oldest-first: %+v", changes[0])
	}
	all := ""
	for _, c := range changes {
		for _, f := range c.Files {
			all += f.Path + "\n" + strings.Join(f.Added, "\n") + "\n"
		}
	}
	for _, want := range []string{"opened PR #1", "all clean", "task-20260721-1"} {
		if !strings.Contains(all, want) {
			t.Fatalf("missing %q in changes: %s", want, all)
		}
	}
	for _, banned := range []string{"private cursor note", "run_b\"", "ledgers/"} {
		if strings.Contains(all, banned) {
			t.Fatalf("excluded path leaked into changes (%q): %s", banned, all)
		}
	}
}

// A line added and later removed must still appear as an addition to a
// consumer that hasn't seen it -- the feed is commit-by-commit, never a net
// endpoint diff.
func TestChangesSurvivePruning(t *testing.T) {
	dir := deliveryFixture(t)
	from, _ := Head(dir)

	appendMemory(t, dir, "events.md", "- 2026-07-21 doc-drift: ephemeral fact")
	if _, err := Commit(dir, "Run doc-drift (run_a): completed"); err != nil {
		t.Fatal(err)
	}
	// Simulate retention pruning the line.
	p := filepath.Join(WorktreePath(dir), "events.md")
	raw, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.ReplaceAll(string(raw), "- 2026-07-21 doc-drift: ephemeral fact\n", "")), 0o644)
	if _, err := Commit(dir, "Trim memory to retention window"); err != nil {
		t.Fatal(err)
	}

	through, _ := Head(dir)
	changes, err := Changes(dir, from, through)
	if err != nil {
		t.Fatal(err)
	}
	var added []string
	for _, c := range changes {
		for _, f := range c.Files {
			added = append(added, f.Added...)
		}
	}
	if !strings.Contains(strings.Join(added, "\n"), "ephemeral fact") {
		t.Fatalf("pruned event lost from the feed: %+v", changes)
	}
}

func TestCursorRoundTripAndListing(t *testing.T) {
	dir := deliveryFixture(t)
	head, _ := Head(dir)
	if c, err := LoadCursor(dir, "check-in"); err != nil || c != nil {
		t.Fatalf("expected no cursor yet, got %+v, %v", c, err)
	}
	want := Cursor{ConsumedThrough: head, ByRun: "run_x", At: time.Now().UTC().Truncate(time.Second)}
	if err := SaveCursor(dir, "check-in", want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCursor(dir, "check-in")
	if err != nil || got == nil || got.ConsumedThrough != head || got.ByRun != "run_x" {
		t.Fatalf("cursor round trip failed: %+v, %v", got, err)
	}
	all, err := Cursors(dir)
	if err != nil || len(all) != 1 {
		t.Fatalf("cursor listing failed: %+v, %v", all, err)
	}
	// Cursors live under state/: supervisor-owned, never staged into a run.
	staging := t.TempDir()
	if err := Snapshot(dir, staging); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(staging, "state", "cursors", "check-in.json")); !os.IsNotExist(err) {
		t.Fatal("cursor leaked into staged memory")
	}
}

func TestRenderInboxShapes(t *testing.T) {
	empty := RenderInbox("check-in", "", "abc123def456", nil)
	if !strings.Contains(empty, "first run") || !strings.Contains(empty, "No pending changes") {
		t.Fatalf("first-run inbox wrong: %s", empty)
	}
	changes := []CommitChange{{
		SHA: "abc123def4567890", Date: "2026-07-21", Subject: "Run doc-drift (run_a): completed",
		Files: []FileDelta{{Path: "events.md", Added: []string{"- 2026-07-21 doc-drift: opened PR #1"}}},
	}}
	inbox := RenderInbox("check-in", "000111", "abc123", changes)
	for _, want := range []string{"# Pending memory changes", "Consumer: check-in", "events.md", "opened PR #1"} {
		if !strings.Contains(inbox, want) {
			t.Fatalf("inbox missing %q: %s", want, inbox)
		}
	}
}

func TestAppendHumanTaskSectionsAndDedup(t *testing.T) {
	dir := deliveryFixture(t)
	if err := AppendHumanTask(dir, "task-run_a", "Investigate routine x (source: supervisor; added 2026-07-21)"); err != nil {
		t.Fatal(err)
	}
	if err := AppendHumanTask(dir, "task-run_a", "Investigate routine x (source: supervisor; added 2026-07-21)"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(WorktreePath(dir), "tasks.md"))
	text := string(raw)
	if got := strings.Count(text, "task-run_a"); got != 1 {
		t.Fatalf("expected exactly one task-run_a entry, got %d: %s", got, text)
	}
	human := strings.Index(text, "## Human-owned")
	entry := strings.Index(text, "task-run_a")
	if human < 0 || entry < human {
		t.Fatalf("task not under Human-owned: %s", text)
	}
}
