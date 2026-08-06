package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/memory"
)

func memoryAgent(t *testing.T) string {
	t.Helper()
	dir := statusAgent(t, nil)
	memoryGit(t, dir, "init", "-q")
	memoryGit(t, dir, "config", "user.name", "Test")
	memoryGit(t, dir, "config", "user.email", "test@example.invalid")
	memoryGit(t, dir, "add", "openroutines.yml")
	memoryGit(t, dir, "commit", "-qm", "agent")
	if err := memory.At(dir).Ensure(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestMemoryRendersPrimitivesWithoutFormatExamples(t *testing.T) {
	dir := memoryAgent(t)
	wt := memory.At(dir).Worktree()
	appendFile(t, filepath.Join(wt, "tasks.md"), "\n## Human-owned\n\n- [ ] `task-20260803-1` Grant access\n")
	appendFile(t, filepath.Join(wt, "events.md"), "\n- 2026-08-03 review: checked PR #42\n")
	appendFile(t, filepath.Join(wt, "context.md"), "\n- 2026-08-03 review: releases happen on Tuesdays\n")
	if err := os.WriteFile(filepath.Join(wt, "ledgers", "review.md"), []byte("# Review state\n\n- PR #42 is open\n\n```text\nkeep this fenced record\n```\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var code int
	out := capture(t, dir, func() { code = cmdMemory([]string{"--no-sync"}) })
	if code != 0 {
		t.Fatalf("memory returned %d", code)
	}
	for _, want := range []string{"Tasks", "Grant access", "Recent events", "checked PR #42", "Context", "releases happen", "Routine state", "## review", "PR #42 is open", "keep this fenced record"} {
		if !strings.Contains(out, want) {
			t.Fatalf("memory output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "task-YYYYMMDD") || strings.Contains(out, "Format") {
		t.Fatalf("memory output included seeded format example:\n%s", out)
	}
}

func TestMemorySectionsAndJSON(t *testing.T) {
	dir := memoryAgent(t)
	wt := memory.At(dir).Worktree()
	appendFile(t, filepath.Join(wt, "tasks.md"), "\n- [ ] `task-20260803-1` Grant access\n")
	appendFile(t, filepath.Join(wt, "events.md"), "\n- event that should be hidden\n")

	var code int
	out := capture(t, dir, func() { code = cmdMemory([]string{"--no-sync", "--tasks"}) })
	if code != 0 {
		t.Fatalf("memory returned %d", code)
	}
	if !strings.Contains(out, "Grant access") || strings.Contains(out, "Recent events") {
		t.Fatalf("section selection was not honored:\n%s", out)
	}

	out = capture(t, dir, func() { code = cmdMemory([]string{"--no-sync", "--tasks", "--json"}) })
	if code != 0 {
		t.Fatalf("memory --json returned %d", code)
	}
	var view map[string]any
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatalf("JSON output is invalid: %v\n%s", err, out)
	}
	if _, ok := view["tasks"]; !ok {
		t.Fatalf("JSON output has no tasks: %s", out)
	}
	if _, ok := view["events"]; ok {
		t.Fatalf("JSON output included unselected events: %s", out)
	}
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func memoryGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
