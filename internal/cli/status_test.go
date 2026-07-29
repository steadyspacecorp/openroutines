package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/memory"
	"github.com/steadyspacecorp/openroutines/internal/schedule"
	"github.com/steadyspacecorp/openroutines/internal/supervisor"
)

const statusAgentYAML = `name: test-agent
description: Tests status
owner:
  name: CI
  email: ci@example.invalid
timezone: UTC
defaults:
  model: fake/model
`

// statusAgent builds an agent directory with the given routines, keyed by
// name -> frontmatter+body.
func statusAgent(t *testing.T, routines map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openroutines.yml"), []byte(statusAgentYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "routines"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range routines {
		if err := os.WriteFile(filepath.Join(dir, "routines", name+".md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// saveState writes what the supervisor would have written for a routine.
func saveState(t *testing.T, dir string, st *schedule.State) {
	t.Helper()
	if err := st.Save(memory.At(dir).StateDir()); err != nil {
		t.Fatal(err)
	}
}

// capture runs a command from dir and returns everything it printed.
func capture(t *testing.T, dir string, run func()) string {
	t.Helper()
	t.Chdir(dir)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	run()
	os.Stdout = stdout
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// A routine mid-retry or sitting out a circuit-breaker cool-down must not
// render like a healthy one -- least of all with a next-fire time it will not
// honour.
func TestStatusShowsSchedulingState(t *testing.T) {
	now := time.Now().UTC()
	dir := statusAgent(t, map[string]string{
		"flaky":  "---\nschedule: \"*/5 * * * *\"\n---\nFail a lot.\n",
		"digest": "---\nschedule: \"0 9 * * *\"\n---\nSummarize.\n",
		"fresh":  "---\nschedule: \"0 9 * * *\"\n---\nNever seen by a supervisor.\n",
	})
	saveState(t, dir, &schedule.State{
		Routine:             "flaky",
		Watermark:           now.Add(-3 * time.Hour),
		ConsecutiveAbandons: 4,
		CooldownUntil:       now.Add(2 * time.Hour),
	})
	saveState(t, dir, &schedule.State{
		Routine:   "digest",
		Watermark: now.Add(-25 * time.Hour),
		Pending: &schedule.Pending{
			RunID:         "run_abcdefghij",
			ScheduledFor:  now.Add(-time.Hour),
			CreatedAt:     now.Add(-time.Hour),
			Attempts:      2,
			LastAttemptAt: now.Add(-time.Minute),
		},
	})

	out := capture(t, dir, func() { cmdStatus(nil) })

	for _, want := range []string{
		"cooling down until " + now.Add(2*time.Hour).Format("Mon 15:04"),
		"circuit breaker: 4 consecutive abandonments",
		"pending run_abcdefghij for " + now.Add(-time.Hour).Format("Mon 15:04"),
		"2/5 attempts, next attempt " + now.Add(time.Minute).Format("Mon 15:04"),
		"watermark " + now.Add(-25*time.Hour).Format("Jan 2 15:04"),
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("status missing %q:\n%s", want, out)
		}
	}
	// The cooling-down routine fires every five minutes; printing a next time
	// it will not honour is the bug this replaces.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "  flaky") && strings.Contains(line, "next") {
			t.Fatalf("a cooling-down routine must not advertise a next firing: %q", line)
		}
	}
	// A routine the supervisor has never seen has nothing to report.
	if n := strings.Count(out, "watermark "); n != 2 {
		t.Fatalf("expected watermarks for the two routines with state, got %d:\n%s", n, out)
	}
}

// A spent attempt budget is settled by the next tick, not retried -- saying
// "next attempt" there would be a time that never arrives.
func TestStatusReportsSpentAttemptBudget(t *testing.T) {
	now := time.Now().UTC()
	dir := statusAgent(t, map[string]string{
		"doomed": "---\nschedule: \"0 9 * * *\"\n---\nTake the container down.\n",
	})
	saveState(t, dir, &schedule.State{
		Routine:   "doomed",
		Watermark: now.Add(-2 * time.Hour),
		Pending: &schedule.Pending{
			RunID:         "run_doomedrun",
			ScheduledFor:  now.Add(-time.Hour),
			CreatedAt:     now.Add(-time.Hour),
			Attempts:      supervisor.MaxAttempts,
			LastAttemptAt: now.Add(-time.Minute),
		},
	})

	out := capture(t, dir, func() { cmdStatus(nil) })
	if !strings.Contains(out, "5/5 attempts, budget spent -- the next tick abandons it") {
		t.Fatalf("status should report the spent budget:\n%s", out)
	}
}

// Web access and MCP servers are authorities like skills and credentials; the
// surfaces that answer "what can this routine reach" have to name them all.
func TestGrantSurfacesNameEveryAuthority(t *testing.T) {
	dir := statusAgent(t, map[string]string{
		"reach": "---\nschedule: \"0 9 * * *\"\nskills: []\ncredentials: [api_token]\nmcp: [feed]\nwebfetch: true\nwebsearch: true\n---\nReach out.\n",
		"quiet": "---\nschedule: \"0 9 * * *\"\n---\nStay in.\n",
	})

	for name, run := range map[string]func(){
		"status": func() { cmdStatus(nil) },
		"list":   func() { routinesList() },
	} {
		out := capture(t, dir, run)
		for _, want := range []string{"creds:1", "mcp:1", "webfetch", "websearch"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s missing grant %q:\n%s", name, want, out)
			}
		}
		for _, line := range strings.Split(out, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "quiet") {
				continue
			}
			for _, grant := range []string{"skills:", "creds:", "mcp:", "webfetch", "websearch"} {
				if strings.Contains(line, grant) {
					t.Fatalf("%s: a routine with no grants should list none: %q", name, line)
				}
			}
		}
	}
}
