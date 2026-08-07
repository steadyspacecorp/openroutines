package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/knowledge"
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
	if err := st.Save(knowledge.At(dir).StateDir()); err != nil {
		t.Fatal(err)
	}
}

// writeRunLog writes the settlement records the supervisor would have appended.
func writeRunLog(t *testing.T, dir string, lines ...string) {
	t.Helper()
	path := filepath.Join(knowledge.At(dir).Worktree(), "runs.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
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
// honor.
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
		"circuit breaker: 4 consecutive abandonments -- no new run starts until " + now.Add(2*time.Hour).Format("Mon 15:04"),
		"pending run_abcdefghij for " + now.Add(-time.Hour).Format("Mon 15:04"),
		"2/5 attempts, next attempt " + now.Add(time.Minute).Format("Mon 15:04"),
		"watermark " + now.Add(-25*time.Hour).Format("Mon 15:04"),
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("status missing %q:\n%s", want, out)
		}
	}
	// The cooling-down routine fires every five minutes; printing a next time
	// it will not honor is the bug this replaces.
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

// The tick skips a routine that is inactive or declares neither a schedule nor
// a trigger, before it ever reads the state -- so a pending run left on one is
// going nowhere, and naming a next attempt would be the same lie the
// cool-down case fixes. The cool-down's end has to appear on the breaker line
// itself: the header clause that used to carry it prints only when active.
func TestStatusHoldsPendingTheSupervisorWillNotAdvance(t *testing.T) {
	now := time.Now().UTC()
	dir := statusAgent(t, map[string]string{
		"parked":  "---\nschedule: \"*/5 * * * *\"\nactive: false\n---\nDeactivated mid-retry.\n",
		"nosched": "---\nactive: true\n---\nNo schedule, no trigger.\n",
	})
	pending := func(runID string) *schedule.Pending {
		return &schedule.Pending{
			RunID: runID, ScheduledFor: now.Add(-time.Hour), CreatedAt: now.Add(-time.Hour),
			Attempts: 2, LastAttemptAt: now.Add(-time.Minute),
		}
	}
	saveState(t, dir, &schedule.State{
		Routine: "parked", Watermark: now.Add(-3 * time.Hour), Pending: pending("run_parkedaaa"),
		ConsecutiveAbandons: 4, CooldownUntil: now.Add(2 * time.Hour),
	})
	saveState(t, dir, &schedule.State{
		Routine: "nosched", Watermark: now.Add(-3 * time.Hour), Pending: pending("run_noschedaa"),
	})

	out := capture(t, dir, func() { cmdStatus(nil) })

	if n := strings.Count(out, "held -- the supervisor skips this routine"); n != 2 {
		t.Fatalf("both skipped routines should hold their pending run, got %d:\n%s", n, out)
	}
	if strings.Contains(out, "next attempt") {
		t.Fatalf("a routine the supervisor skips must not promise an attempt:\n%s", out)
	}
	// "until then" with no antecedent: the header says only "inactive" here.
	if !strings.Contains(out, "no new run starts until "+now.Add(2*time.Hour).Format("Mon 15:04")) {
		t.Fatalf("the breaker line must carry the cool-down's end:\n%s", out)
	}
}

// Reserving an attempt and failing one leave identical state on disk, so the
// state file alone cannot say whether a run is executing. The run record,
// written at settlement, is what distinguishes them -- and absent a log to
// read, the retry rendering is the safe answer rather than a false "in flight".
func TestStatusDistinguishesAnAttemptInFlight(t *testing.T) {
	now := time.Now().UTC()
	routines := map[string]string{
		"live":    "---\nschedule: \"*/5 * * * *\"\n---\nExecuting now.\n",
		"backoff": "---\nschedule: \"*/5 * * * *\"\n---\nFailed, waiting.\n",
	}
	states := []*schedule.State{
		{Routine: "live", Watermark: now.Add(-10 * time.Minute), Pending: &schedule.Pending{
			RunID: "run_liveaaaaaa", ScheduledFor: now.Add(-2 * time.Minute),
			CreatedAt: now.Add(-2 * time.Minute), Attempts: 1, LastAttemptAt: now.Add(-10 * time.Second)}},
		{Routine: "backoff", Watermark: now.Add(-10 * time.Minute), Pending: &schedule.Pending{
			RunID: "run_backoffaaa", ScheduledFor: now.Add(-2 * time.Minute),
			CreatedAt: now.Add(-2 * time.Minute), Attempts: 2, LastAttemptAt: now.Add(-30 * time.Second)}},
	}

	t.Run("with run records", func(t *testing.T) {
		dir := statusAgent(t, routines)
		for _, st := range states {
			saveState(t, dir, st)
		}
		// Only backoff's attempt settled.
		writeRunLog(t, dir, `{"run_id":"run_backoffaaa","routine":"backoff","attempt":2,"outcome":"failed"}`)

		out := capture(t, dir, func() { cmdStatus(nil) })
		if !strings.Contains(out, "attempt 1 started "+now.Add(-10*time.Second).Format("Mon 15:04")+", still in flight") {
			t.Fatalf("a reserved, unsettled attempt is running, not backing off:\n%s", out)
		}
		if !strings.Contains(out, "2/5 attempts, next attempt "+now.Add(90*time.Second).Format("Mon 15:04")) {
			t.Fatalf("a settled failed attempt should report its retry:\n%s", out)
		}
	})

	t.Run("without a run log", func(t *testing.T) {
		dir := statusAgent(t, routines)
		for _, st := range states {
			saveState(t, dir, st)
		}

		out := capture(t, dir, func() { cmdStatus(nil) })
		if strings.Contains(out, "in flight") {
			t.Fatalf("no run log is no evidence -- do not claim a run is in flight:\n%s", out)
		}
		if n := strings.Count(out, "next attempt "); n != 2 {
			t.Fatalf("expected both routines to fall back to the retry line, got %d:\n%s", n, out)
		}
	})
}

// A pending run whose retry is already due, or which no attempt has touched,
// must not print a past time under the word "next".
func TestStatusReportsAnOverdueAttemptAsDue(t *testing.T) {
	now := time.Now().UTC()
	dir := statusAgent(t, map[string]string{
		"minted": "---\nschedule: \"*/5 * * * *\"\n---\nMinted, not yet dispatched.\n",
	})
	saveState(t, dir, &schedule.State{
		Routine: "minted", Watermark: now.Add(-10 * time.Minute),
		Pending: &schedule.Pending{
			RunID: "run_mintedaaaa", ScheduledFor: now.Add(-time.Minute), CreatedAt: now.Add(-time.Minute),
		},
	})

	out := capture(t, dir, func() { cmdStatus(nil) })
	if !strings.Contains(out, "0/5 attempts, due now") {
		t.Fatalf("a pending run with no attempts is due, not scheduled:\n%s", out)
	}
}

// A held run is old by definition -- that is why it is on screen -- so the
// stamp has to stay unambiguous past the week that a weekday name covers.
func TestStampWidensAsTimesGetDistant(t *testing.T) {
	now := time.Date(2026, 7, 29, 14, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		name, want string
		t          time.Time
	}{
		{"within the week", "Mon 09:00", time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)},
		{"beyond the week", "Jul 15 09:00", time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)},
		{"another year", "Dec 30 2025 09:00", time.Date(2025, 12, 30, 9, 0, 0, 0, time.UTC)},
	} {
		if got := stamp(c.t, now, time.UTC); got != c.want {
			t.Errorf("%s: stamp = %q, want %q", c.name, got, c.want)
		}
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
