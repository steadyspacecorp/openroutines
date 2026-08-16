package knowledge

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func deliveryFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "-b", "main")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init: %v: %s", err, out)
	}
	if err := NewStore(dir).Ensure(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func appendKnowledge(t *testing.T, dir, file, line string) {
	t.Helper()
	p := filepath.Join(NewStore(dir).Worktree(), file)
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
	from, err := NewStore(dir).Head()
	if err != nil {
		t.Fatal(err)
	}

	appendKnowledge(t, dir, "events.md", "- 2026-07-21 doc-drift: opened PR #1")
	if _, err := NewStore(dir).Commit("Run doc-drift (run_a): completed"); err != nil {
		t.Fatal(err)
	}
	appendKnowledge(t, dir, "events.md", "- 2026-07-21 a11y-sweep NO-OP: all clean")
	appendKnowledge(t, dir, "tasks.md", "- [ ] `task-20260721-1` decide the thing (source: doc-drift; added 2026-07-21)")
	// Ledger and supervisor-owned writes must never reach a consumer.
	appendKnowledge(t, dir, "ledgers/doc-drift.md", "- private cursor note")
	appendKnowledge(t, dir, "runs.jsonl", `{"run_id":"run_b"}`)
	if _, err := NewStore(dir).Commit("Run a11y-sweep (run_b): completed"); err != nil {
		t.Fatal(err)
	}

	through, err := NewStore(dir).Head()
	if err != nil {
		t.Fatal(err)
	}
	changes, err := NewStore(dir).Changes(from, through)
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

// Appends one event, commits it, then trims and commits the trim
// the way the supervisor does. Returns the commit the event landed in.
func trimFixture(t *testing.T, dir string) string {
	t.Helper()
	appendKnowledge(t, dir, "events.md", "- 2026-07-21 doc-drift: ephemeral fact")
	if _, err := NewStore(dir).Commit("Run doc-drift (run_a): completed"); err != nil {
		t.Fatal(err)
	}
	added, _ := NewStore(dir).Head()
	// Age the whole worktree past the window rather than backdating commits.
	if changed, err := NewStore(dir).Trim(30*24*time.Hour, time.Now().Add(60*24*time.Hour)); err != nil || !changed {
		t.Fatalf("trim: changed=%v err=%v", changed, err)
	}
	if _, err := NewStore(dir).CommitTrim(30 * 24 * time.Hour); err != nil {
		t.Fatal(err)
	}
	return added
}

// A line added and later removed must still appear as an addition to a
// consumer that hasn't seen it -- the feed is commit-by-commit, never a net
// endpoint diff.
func TestChangesSurvivePruning(t *testing.T) {
	dir := deliveryFixture(t)
	from, _ := NewStore(dir).Head()
	trimFixture(t, dir)

	through, _ := NewStore(dir).Head()
	changes, err := NewStore(dir).Changes(from, through)
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

// Retention pruning is not a change to report: a consumer whose cursor is
// already past the trimmed entries must see nothing at all from the trim
// commit, not a block of removals for history it consumed long ago.
func TestRetentionTrimIsNotDelivered(t *testing.T) {
	dir := deliveryFixture(t)
	cursor := trimFixture(t, dir)

	through, _ := NewStore(dir).Head()
	changes, err := NewStore(dir).Changes(cursor, through)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("retention trim re-entered the feed: %+v", changes)
	}
}

// The trim commit is the one commit the feed skips wholesale, so it must
// carry nothing but the trim. Curation left uncommitted in the worktree --
// which `status` invites, and a failed intent commit leaves behind -- would
// otherwise ride along into it and never reach a consumer at all.
func TestTrimCommitLeavesUnrelatedWorkDeliverable(t *testing.T) {
	dir := deliveryFixture(t)
	appendKnowledge(t, dir, "events.md", "- 2026-07-21 doc-drift: ephemeral fact")
	if _, err := NewStore(dir).Commit("Run doc-drift (run_a): completed"); err != nil {
		t.Fatal(err)
	}
	cursor, _ := NewStore(dir).Head()

	// Curation sitting in the worktree when the daily trim fires.
	appendKnowledge(t, dir, "tasks.md", "- [ ] `task-20260721-1` hand-curated ask (source: a human; added 2026-07-21)")
	if changed, err := NewStore(dir).Trim(30*24*time.Hour, time.Now().Add(60*24*time.Hour)); err != nil || !changed {
		t.Fatalf("trim: changed=%v err=%v", changed, err)
	}
	if _, err := NewStore(dir).CommitTrim(30 * 24 * time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(dir).Commit("Run doc-drift (run_c): completed"); err != nil {
		t.Fatal(err)
	}

	through, _ := NewStore(dir).Head()
	changes, err := NewStore(dir).Changes(cursor, through)
	if err != nil {
		t.Fatal(err)
	}
	var added []string
	for _, c := range changes {
		for _, f := range c.Files {
			added = append(added, f.Added...)
		}
	}
	if !strings.Contains(strings.Join(added, "\n"), "hand-curated ask") {
		t.Fatalf("the trim commit swallowed uncommitted curation: %+v", changes)
	}
}

// A cursor whose commit is not on the knowledge branch names no change set, and
// no retry will make it name one: the feed reports that as its own error so
// the caller can tell it apart from an attempt worth repeating.
func TestChangesRejectsUnreachableCursor(t *testing.T) {
	dir := deliveryFixture(t)
	through, _ := NewStore(dir).Head()

	if _, err := NewStore(dir).Changes("0123456789abcdef0123456789abcdef01234567", through); !errors.Is(err, ErrCursorUnreachable) {
		t.Fatalf("a missing commit should report ErrCursorUnreachable, got %v", err)
	}

	// Present but off the branch: a repaired history leaves the old commit in
	// the object store, where from..through would deliver the wrong set.
	appendKnowledge(t, dir, "events.md", "- 2026-07-21 doc-drift: a fact")
	if _, err := NewStore(dir).Commit("Run doc-drift (run_a): completed"); err != nil {
		t.Fatal(err)
	}
	orphan, _ := NewStore(dir).Head()
	if _, err := NewStore(dir).worktree.Run("reset", "--hard", "HEAD~1"); err != nil {
		t.Fatal(err)
	}
	appendKnowledge(t, dir, "events.md", "- 2026-07-21 doc-drift: the repaired fact")
	if _, err := NewStore(dir).Commit("Run doc-drift (run_b): completed"); err != nil {
		t.Fatal(err)
	}
	through, _ = NewStore(dir).Head()
	if _, err := NewStore(dir).Changes(orphan, through); !errors.Is(err, ErrCursorUnreachable) {
		t.Fatalf("an off-branch commit should report ErrCursorUnreachable, got %v", err)
	}
}

// Only git's answer "no such commit" means the cursor is unreachable. A git
// that could not answer at all -- a broken repository, a lock, a full disk --
// is an attempt worth repeating, and must not be classified as the permanent
// failure that abandons a run on its first try.
func TestChangesKeepsEnvironmentFailuresRetryable(t *testing.T) {
	dir := deliveryFixture(t)
	head, _ := NewStore(dir).Head()

	_, err := NewStore(t.TempDir()).Changes(head, head)
	if err == nil {
		t.Fatal("a repository that is not there should fail")
	}
	if errors.Is(err, ErrCursorUnreachable) {
		t.Fatalf("an unanswerable git must stay retryable, got %v", err)
	}

}

func TestCursorRoundTripAndListing(t *testing.T) {
	dir := deliveryFixture(t)
	head, _ := NewStore(dir).Head()
	if c, err := NewStore(dir).LoadCursor("check-in"); err != nil || c != nil {
		t.Fatalf("expected no cursor yet, got %+v, %v", c, err)
	}
	want := Cursor{ConsumedThrough: head, ByRun: "run_x", At: time.Now().UTC().Truncate(time.Second)}
	if err := NewStore(dir).SaveCursor("check-in", want); err != nil {
		t.Fatal(err)
	}
	got, err := NewStore(dir).LoadCursor("check-in")
	if err != nil || got == nil || got.ConsumedThrough != head || got.ByRun != "run_x" {
		t.Fatalf("cursor round trip failed: %+v, %v", got, err)
	}
	all, err := NewStore(dir).Cursors()
	if err != nil || len(all) != 1 {
		t.Fatalf("cursor listing failed: %+v, %v", all, err)
	}
	// Cursors live under state/: supervisor-owned, never staged into a run.
	staging := t.TempDir()
	if err := NewStore(dir).Snapshot(staging); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(staging, "state", "cursors", "check-in.json")); !os.IsNotExist(err) {
		t.Fatal("cursor leaked into staged knowledge")
	}
}

func TestRenderChangesShapes(t *testing.T) {
	empty := RenderChanges("check-in", "", "abc123def456", nil)
	if !strings.Contains(empty, "first run") || !strings.Contains(empty, "No pending changes") {
		t.Fatalf("first-run change set wrong: %s", empty)
	}
	changes := []CommitChange{{
		SHA: "abc123def4567890", Date: "2026-07-21", Subject: "Run doc-drift (run_a): completed",
		Files: []FileDelta{{Path: "events.md", Added: []string{"- 2026-07-21 doc-drift: opened PR #1"}}},
	}}
	rendered := RenderChanges("check-in", "000111", "abc123", changes)
	for _, want := range []string{"# Pending knowledge changes", "Routine: check-in", "events.md", "opened PR #1"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("change set missing %q: %s", want, rendered)
		}
	}
}

func TestAppendHumanTaskSectionsAndDedup(t *testing.T) {
	dir := deliveryFixture(t)
	if err := NewStore(dir).AppendHumanTask("task-run_a", "Investigate routine x (source: supervisor; added 2026-07-21)"); err != nil {
		t.Fatal(err)
	}
	if err := NewStore(dir).AppendHumanTask("task-run_a", "Investigate routine x (source: supervisor; added 2026-07-21)"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(NewStore(dir).Worktree(), "tasks.md"))
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

// Cursor values become git rev-range argv, and cursor files ride the
// untrusted knowledge branch: anything but a commit SHA is rejected on load.
func TestLoadCursorRejectsNonSHA(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"--output=/tmp/evil", "HEAD", "refs/heads/main", "abc", ""} {
		path := NewStore(dir).cursorPath("c")
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte(`{"consumed_through":"`+bad+`"}`), 0o644)
		if _, err := NewStore(dir).LoadCursor("c"); err == nil {
			t.Errorf("cursor value %q should be rejected", bad)
		}
	}
	os.WriteFile(NewStore(dir).cursorPath("c"), []byte(`{"consumed_through":"0123456789abcdef0123456789abcdef01234567"}`), 0o644)
	if _, err := NewStore(dir).LoadCursor("c"); err != nil {
		t.Fatalf("full SHA should load: %v", err)
	}
}

func TestSupervisorEntriesFlattenAndResolve(t *testing.T) {
	dir := deliveryFixture(t)
	raw := "intent push failed: exit status 128: Permission denied (publickey).\nfatal: Could not read from remote repository.\n\nPlease make sure you have the correct access rights"
	if err := NewStore(dir).AppendEvent("2026-07-24 supervisor: " + raw); err != nil {
		t.Fatal(err)
	}
	events, _ := os.ReadFile(filepath.Join(NewStore(dir).Worktree(), "events.md"))
	if strings.Contains(string(events), "\nfatal:") {
		t.Fatalf("multi-line error not flattened in events.md: %s", events)
	}
	if err := NewStore(dir).AppendHumanTask("task-push-20260724", raw+" (source: supervisor; added 2026-07-24)"); err != nil {
		t.Fatal(err)
	}
	tasks, _ := os.ReadFile(filepath.Join(NewStore(dir).Worktree(), "tasks.md"))
	if strings.Contains(string(tasks), "\nfatal:") {
		t.Fatalf("multi-line error not flattened in tasks.md: %s", tasks)
	}

	changed, err := NewStore(dir).ResolveHumanTasks("task-push-", "done 2026-07-25 -- push to origin recovered")
	if err != nil || !changed {
		t.Fatalf("resolve: changed=%v err=%v, want true nil", changed, err)
	}
	tasks, _ = os.ReadFile(filepath.Join(NewStore(dir).Worktree(), "tasks.md"))
	text := string(tasks)
	if strings.Contains(text, "- [ ] `task-push-") || !strings.Contains(text, "- [x] `task-push-20260724`") {
		t.Fatalf("task not resolved in place: %s", text)
	}
	if !strings.Contains(text, "; done 2026-07-25 -- push to origin recovered)") {
		t.Fatalf("resolution not folded into the trailing parens: %s", text)
	}
	if changed, err = NewStore(dir).ResolveHumanTasks("task-push-", "done again"); err != nil || changed {
		t.Fatalf("second resolve: changed=%v err=%v, want false nil", changed, err)
	}
}

func TestResolveHumanTasksIgnoresFencedExamples(t *testing.T) {
	dir := deliveryFixture(t)
	appendKnowledge(t, dir, "tasks.md", "- [ ] `task-YYYY-real` do the real work")

	changed, err := NewStore(dir).ResolveHumanTasks("task-YYYY", "done")
	if err != nil || !changed {
		t.Fatalf("resolve: changed=%v err=%v, want true nil", changed, err)
	}
	raw, err := os.ReadFile(filepath.Join(NewStore(dir).Worktree(), "tasks.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "- [ ] `task-YYYYMMDD-<n>`") {
		t.Fatalf("fenced task example was resolved: %s", text)
	}
	if !strings.Contains(text, "- [x] `task-YYYY-real` do the real work (done)") {
		t.Fatalf("real task was not resolved: %s", text)
	}
}
