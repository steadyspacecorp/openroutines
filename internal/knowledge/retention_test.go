package knowledge

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseRetention(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
		err  bool
	}{
		{"", DefaultRetention, false},
		{"30d", 30 * 24 * time.Hour, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"720h", 720 * time.Hour, false},
		{"0d", 0, true},
		{"abc", 0, true},
		{"-5d", 0, true},
	} {
		got, err := ParseRetention(tc.in)
		if tc.err != (err != nil) || (!tc.err && got != tc.want) {
			t.Fatalf("ParseRetention(%q) = %v, %v; want %v, err=%v", tc.in, got, err, tc.want, tc.err)
		}
	}
}

// commitAt commits the worktree's current state with a specific timestamp,
// so blame-based aging has something old to find.
func commitAt(t *testing.T, wt string, at time.Time, msg string) {
	t.Helper()
	cmd := exec.Command("git", "-c", "user.name=t", "-c", "user.email=t@t", "add", "-A")
	cmd.Dir = wt
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("add: %v: %s", err, out)
	}
	cmd = exec.Command("git", "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", msg)
	cmd.Dir = wt
	cmd.Env = append(os.Environ(),
		"GIT_COMMITTER_DATE="+at.Format(time.RFC3339),
		"GIT_AUTHOR_DATE="+at.Format(time.RFC3339),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v: %s", err, out)
	}
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

func TestTrimAgesOutOldEntriesOnly(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "-b", "main", ".")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init: %v: %s", err, out)
	}
	if err := At(dir).Ensure(); err != nil {
		t.Fatal(err)
	}
	wt := At(dir).Worktree()
	now := time.Now()
	old := now.Add(-40 * 24 * time.Hour)

	// Old entries in the trimmed streams, plus one in the exempt tasks file.
	// The seeded headers (prose + fenced format examples) are committed at
	// the same old timestamp: documentation must survive trimming at any age.
	appendLine(t, filepath.Join(wt, "events.md"), "- ancient fact")
	appendLine(t, filepath.Join(wt, "context.md"), "- ancient context fact")
	appendLine(t, filepath.Join(wt, "tasks.md"), "- [ ] `task-00000000-1` ancient but still open task")
	os.WriteFile(filepath.Join(wt, "runs.jsonl"),
		[]byte(`{"run_id":"run_old","recorded_at":"`+old.UTC().Format(time.RFC3339)+`"}`+"\n"), 0o644)
	commitAt(t, wt, old, "old entries")

	// Recent entries.
	appendLine(t, filepath.Join(wt, "events.md"), "- recent fact")
	appendLine(t, filepath.Join(wt, "runs.jsonl"), `{"run_id":"run_new","recorded_at":"`+now.UTC().Format(time.RFC3339)+`"}`)
	commitAt(t, wt, now, "recent entries")

	// Uncommitted entry: must always survive.
	appendLine(t, filepath.Join(wt, "events.md"), "- uncommitted fact")

	changed, err := At(dir).Trim(30*24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected trim to report changes")
	}

	events, _ := os.ReadFile(filepath.Join(wt, "events.md"))
	if strings.Contains(string(events), "ancient fact") {
		t.Fatalf("old entry survived: %q", events)
	}
	// Documentation survives at any age: heading, prose, and the fenced
	// format example -- including its "- "-prefixed placeholder lines.
	for _, want := range []string{
		"# Events",
		"append-only outcomes and observations",
		"```markdown",
		"- YYYY-MM-DD <routine>: <what happened, why it matters, links, people>",
		"- recent fact",
		"- uncommitted fact",
	} {
		if !strings.Contains(string(events), want) {
			t.Fatalf("trim removed %q: %q", want, events)
		}
	}
	contextFile, _ := os.ReadFile(filepath.Join(wt, "context.md"))
	if strings.Contains(string(contextFile), "ancient context fact") {
		t.Fatalf("old context entry survived: %q", contextFile)
	}
	if !strings.Contains(string(contextFile), "Shared situational awareness") {
		t.Fatalf("trim removed context documentation: %q", contextFile)
	}
	tasks, _ := os.ReadFile(filepath.Join(wt, "tasks.md"))
	if !strings.Contains(string(tasks), "ancient but still open task") {
		t.Fatalf("tasks must be exempt from retention: %q", tasks)
	}
	runs, _ := os.ReadFile(filepath.Join(wt, "runs.jsonl"))
	if strings.Contains(string(runs), "run_old") || !strings.Contains(string(runs), "run_new") {
		t.Fatalf("run record trim wrong: %q", runs)
	}

	// Idempotent: a second trim changes nothing.
	if changed, err := At(dir).Trim(30*24*time.Hour, now); err != nil || changed {
		t.Fatalf("second trim should be a no-op: changed=%v err=%v", changed, err)
	}
}
