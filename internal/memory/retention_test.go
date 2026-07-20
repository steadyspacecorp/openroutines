package memory

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
	if err := EnsureWorktree(dir); err != nil {
		t.Fatal(err)
	}
	wt := WorktreePath(dir)
	now := time.Now()
	old := now.Add(-40 * 24 * time.Hour)

	// Old entries in the trimmed streams, plus one in the exempt intentions.
	appendLine(t, filepath.Join(wt, "worklog.md"), "ancient fact")
	appendLine(t, filepath.Join(wt, "blockers.md"), "- ancient blocker")
	appendLine(t, filepath.Join(wt, "intentions.md"), "- ancient but still open intention")
	os.WriteFile(filepath.Join(wt, "runs.jsonl"),
		[]byte(`{"run_id":"run_old","recorded_at":"`+old.UTC().Format(time.RFC3339)+`"}`+"\n"), 0o644)
	commitAt(t, wt, old, "old entries")

	// Recent entries.
	appendLine(t, filepath.Join(wt, "worklog.md"), "recent fact")
	appendLine(t, filepath.Join(wt, "runs.jsonl"), `{"run_id":"run_new","recorded_at":"`+now.UTC().Format(time.RFC3339)+`"}`)
	commitAt(t, wt, now, "recent entries")

	// Uncommitted entry: must always survive.
	appendLine(t, filepath.Join(wt, "worklog.md"), "uncommitted fact")

	changed, err := Trim(dir, 30*24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected trim to report changes")
	}

	worklog, _ := os.ReadFile(filepath.Join(wt, "worklog.md"))
	if strings.Contains(string(worklog), "ancient fact") {
		t.Fatalf("old entry survived: %q", worklog)
	}
	for _, want := range []string{"# Worklog", "recent fact", "uncommitted fact"} {
		if !strings.Contains(string(worklog), want) {
			t.Fatalf("trim removed %q: %q", want, worklog)
		}
	}
	blockers, _ := os.ReadFile(filepath.Join(wt, "blockers.md"))
	if strings.Contains(string(blockers), "ancient blocker") {
		t.Fatalf("old blocker survived: %q", blockers)
	}
	intentions, _ := os.ReadFile(filepath.Join(wt, "intentions.md"))
	if !strings.Contains(string(intentions), "ancient but still open intention") {
		t.Fatalf("intentions must be exempt from retention: %q", intentions)
	}
	runs, _ := os.ReadFile(filepath.Join(wt, "runs.jsonl"))
	if strings.Contains(string(runs), "run_old") || !strings.Contains(string(runs), "run_new") {
		t.Fatalf("run record trim wrong: %q", runs)
	}

	// Idempotent: a second trim changes nothing.
	if changed, err := Trim(dir, 30*24*time.Hour, now); err != nil || changed {
		t.Fatalf("second trim should be a no-op: changed=%v err=%v", changed, err)
	}
}
