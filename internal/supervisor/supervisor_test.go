package supervisor

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/memory"
	"github.com/steadyspacecorp/openroutines/internal/schedule"
)

// fakeOpencode is a stand-in for the real binary: it reads fake-mode from
// its own directory (the workspace is allow-list built and carries no test
// scaffolding) to decide whether to succeed (writing memory) or fail. The
// probe mode clones the memory branch from origin at the first spawn --
// exactly what a replacement container would materialize if that attempt
// killed the supervisor. Only the first: the snapshot has to be the moment
// one attempt started, not the moment the last one did.
const fakeOpencode = `#!/bin/sh
d=$(dirname "$0")
mode=$(cat "$d/fake-mode" 2>/dev/null || echo ok)
# Every mode leaves the session storage a real opencode leaves in the
# attempt home -- the surface the runner captures token usage from.
mkdir -p .home/.local/share/opencode/storage/message/ses_fake
printf '{"role":"assistant","modelID":"fake","tokens":{"input":100,"output":20,"reasoning":5,"cache":{"read":0,"write":0}},"cost":0.01}' \
  > .home/.local/share/opencode/storage/message/ses_fake/msg_1.json
case "$mode" in
  fail) echo "boom"; exit 1 ;;
  slow) echo "$OPENROUTINES_RUN_ID" >> "$(dirname "$0")/started"
     sleep 3
     mkdir -p memory/ledgers
     echo "ran $OPENROUTINES_RUN_ID $OPENROUTINES_ATTEMPT_ID" >> memory/ledgers/fake.md
     echo "slept" ;;
  consume) cp inbox.md memory/inbox-copy.md
     : > CONSUMED
     echo "consumed" ;;
  probe) [ -d "$d/replacement" ] || git clone -q -b memory "$(cat "$d/origin")" "$d/replacement" || true
     echo "probed" ;;
  *) mkdir -p memory/ledgers
     echo "ran $OPENROUTINES_RUN_ID $OPENROUTINES_ATTEMPT_ID" >> memory/ledgers/fake.md
     echo "done" ;;
esac
`

// agentYAML is the test agent's config, in the given timezone.
func agentYAML(tz string) string {
	return fmt.Sprintf(`name: test-agent
description: Tests the supervisor
owner:
  name: CI
  email: ci@example.invalid
timezone: %s
defaults:
  model: fake/model
  timeout: 30s
`, tz)
}

// fixture builds a UTC agent whose one routine fires every minute.
func fixture(t *testing.T, mode string) string {
	t.Helper()
	return fixtureIn(t, mode, "UTC", "every-minute", "* * * * *")
}

// fixtureIn builds an agent repo (no origin: local mode) in the given
// timezone with one scheduled routine, and puts a fake opencode on PATH.
func fixtureIn(t *testing.T, mode, tz, name, spec string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	run("git", "init", "-q", "-b", "main", ".")
	os.WriteFile(filepath.Join(dir, "openroutines.yml"), []byte(agentYAML(tz)), 0o644)
	os.MkdirAll(filepath.Join(dir, "routines"), 0o755)
	os.WriteFile(filepath.Join(dir, "routines", name+".md"), []byte(
		fmt.Sprintf("---\nschedule: %q\n---\nDo the fake thing.\n", spec)), 0o644)
	binDir := t.TempDir()
	os.WriteFile(filepath.Join(binDir, "opencode"), []byte(fakeOpencode), 0o755)
	os.WriteFile(filepath.Join(binDir, "fake-mode"), []byte(mode+"\n"), 0o644)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OPENROUTINES_NATIVE", "1") // tests use the fake opencode, not the container
	return dir
}

func newSupervisor(t *testing.T, dir string) *Supervisor {
	t.Helper()
	if err := memory.At(dir).Ensure(); err != nil {
		t.Fatal(err)
	}
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func loadState(t *testing.T, s *Supervisor) *schedule.State {
	t.Helper()
	st, err := schedule.Load(s.stateDir(), "every-minute")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func readFile(_ *testing.T, path string) string {
	raw, _ := os.ReadFile(path)
	return string(raw)
}

func runCmd(t *testing.T, cwd string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = cwd
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v: %s", args, err, out)
	}
}

// fakeBinDir is where fixture put the fake opencode: the first PATH entry.
func fakeBinDir() string {
	return strings.SplitN(os.Getenv("PATH"), string(os.PathListSeparator), 2)[0]
}

// withOrigin gives the agent a bare origin and tells the fake opencode where
// it is, so a probe run can clone it mid-attempt.
func withOrigin(t *testing.T, dir string) {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "origin.git")
	runCmd(t, "", "git", "init", "-q", "-b", "main", "--bare", bare)
	runCmd(t, dir, "git", "remote", "add", "origin", bare)
	if err := os.WriteFile(filepath.Join(fakeBinDir(), "origin"), []byte(bare+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// replacementState is the scheduling state a replacement container would read
// for a routine after materializing memory from origin, as of the moment the
// first probe attempt's model process started.
func replacementState(t *testing.T, name string) *schedule.State {
	t.Helper()
	st, err := schedule.Load(filepath.Join(fakeBinDir(), "replacement", "state"), name)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestRegisterThenRunAdvancesWatermark(t *testing.T) {
	dir := fixture(t, "ok")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	// First sight: registers, does not run.
	s.Tick(ctx, t0)
	st := loadState(t, s)
	if st == nil || st.Pending != nil {
		t.Fatalf("expected registered state with no pending, got %+v", st)
	}
	if ledger := readFile(t, filepath.Join(dir, "memory", "ledgers", "fake.md")); ledger != "" {
		t.Fatalf("nothing should have run yet, ledger: %q", ledger)
	}

	// One minute later: one occurrence due -> runs, imports, advances.
	s.Tick(ctx, t0.Add(61*time.Second))
	st = loadState(t, s)
	if st.Pending != nil {
		t.Fatalf("pending should be cleared after success: %+v", st.Pending)
	}
	if !st.Watermark.After(t0) {
		t.Fatalf("watermark should have advanced past %v, got %v", t0, st.Watermark)
	}
	ledger := readFile(t, filepath.Join(dir, "memory", "ledgers", "fake.md"))
	if !strings.Contains(ledger, "ran run_") || !strings.Contains(ledger, "attempt_01") {
		t.Fatalf("ledger missing run evidence: %q", ledger)
	}
	records := readFile(t, filepath.Join(dir, "memory", "runs.jsonl"))
	if !strings.Contains(records, `"outcome":"completed"`) {
		t.Fatalf("run record missing: %q", records)
	}
}

// A typo in one routine's frontmatter is that routine's problem alone: the
// tick schedules around it and the healthy routine's run assembles a
// workspace without it, rather than failing every attempt at workspace
// assembly and abandoning runs agent-wide.
func TestBrokenRoutineDoesNotFailHealthyRuns(t *testing.T) {
	dir := fixture(t, "ok")
	os.WriteFile(filepath.Join(dir, "routines", "typo.md"), []byte(
		"---\nschedule: \"* * * * *\"\nactve: false\n---\nBroken.\n"), 0o644)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.Tick(ctx, t0) // register
	s.Tick(ctx, t0.Add(61*time.Second))

	st := loadState(t, s)
	if st.Pending != nil {
		t.Fatalf("the healthy routine should have completed, not retried: %+v", st.Pending)
	}
	ledger := readFile(t, filepath.Join(dir, "memory", "ledgers", "fake.md"))
	if !strings.Contains(ledger, "ran run_") {
		t.Fatalf("the healthy routine should have run: %q", ledger)
	}
	records := readFile(t, filepath.Join(dir, "memory", "runs.jsonl"))
	if !strings.Contains(records, `"outcome":"completed"`) {
		t.Fatalf("run record missing: %q", records)
	}

	// Scheduling around the broken routine must not mean saying nothing about
	// it: a routine that stopped running is news, and a log line is not where
	// an unattended agent reports.
	events := readFile(t, filepath.Join(dir, "memory", "events.md"))
	if !strings.Contains(events, "routine typo does not load") {
		t.Errorf("the broken routine should be recorded as an event: %q", events)
	}
	if !strings.Contains(events, "routines/typo.md") || strings.Contains(events, dir) {
		t.Errorf("the event should name the file as the repository spells it: %q", events)
	}
	if strings.Count(events, "does not load") != 1 {
		t.Errorf("the failure is recorded once, not every tick: %q", events)
	}

	os.WriteFile(filepath.Join(dir, "routines", "typo.md"), []byte(
		"---\nschedule: \"0 9 * * *\"\nactive: false\n---\nFixed.\n"), 0o644)
	s.Tick(ctx, t0.Add(122*time.Second))
	if events := readFile(t, filepath.Join(dir, "memory", "events.md")); !strings.Contains(events, "routine typo loads again") {
		t.Errorf("the repair should be recorded too: %q", events)
	}
}

// A broken file claims its name whether or not it parses, so a healthy
// routine of the same name is ambiguous, not runnable: the tick must not mint
// a run the runner will then refuse to assemble a workspace for -- five
// attempts, an abandonment task, and a tripped breaker for a routine whose
// own file is fine.
func TestShadowedRoutineNameIsNotScheduled(t *testing.T) {
	dir := fixture(t, "ok")
	os.MkdirAll(filepath.Join(dir, "plugins", "demo", "routines"), 0o755)
	os.WriteFile(filepath.Join(dir, "plugins", "demo", "routines", "every-minute.md"), []byte(
		"---\nschedule: \"* * * * *\"\nactve: false\n---\nBroken.\n"), 0o644)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	for i := 0; i <= 40; i++ {
		s.Tick(ctx, t0.Add(time.Duration(i)*7*time.Minute))
	}

	if st := loadState(t, s); st != nil {
		t.Errorf("an ambiguous name should not be scheduled at all: %+v", st)
	}
	tasks := readFile(t, filepath.Join(dir, "memory", "tasks.md"))
	if strings.Contains(tasks, "abandoned") {
		t.Errorf("no run should have been minted, let alone abandoned:\n%s", tasks)
	}
	events := readFile(t, filepath.Join(dir, "memory", "events.md"))
	if !strings.Contains(events, "routine every-minute does not load") {
		t.Errorf("the collision should be recorded: %q", events)
	}
}

// A schedule is a wall-clock promise: a 06:00 routine on a New York agent
// fires at 06:00 on both sides of a DST transition. The watermark it scans
// from has round-tripped through the state file by then, so this only holds
// if the supervisor evaluates cron in the agent's timezone rather than in
// whatever zone the persisted timestamp came back carrying.
func TestScheduleHoldsAgentWallClockAcrossDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database")
	}
	// The release container sets no TZ; pin time.Local to match it, so the
	// watermark rehydrates into a fabricated fixed-offset zone here too
	// instead of whatever zone the developer's machine happens to be in.
	defer func(l *time.Location) { time.Local = l }(time.Local)
	time.Local = time.UTC

	// "fail" keeps the pending run on disk after dispatch, where its
	// scheduled_for can be read back.
	dir := fixtureIn(t, "fail", "America/New_York", "daily", "0 6 * * *")
	s := newSupervisor(t, dir)
	ctx := context.Background()

	s.Tick(ctx, time.Date(2026, 10, 31, 12, 0, 0, 0, ny)) // register, EDT
	s.Tick(ctx, time.Date(2026, 11, 2, 12, 0, 0, 0, ny))  // after fall-back, EST

	st, err := schedule.Load(s.stateDir(), "daily")
	if err != nil {
		t.Fatal(err)
	}
	if st == nil || st.Pending == nil {
		t.Fatalf("expected a pending run, got %+v", st)
	}
	if got := st.Pending.ScheduledFor.In(ny); got.Hour() != 6 || got.Day() != 1 {
		t.Fatalf("scheduled_for = %v, want 06:00 New York on Nov 1", got)
	}
	if got := st.Pending.CoveredThrough.In(ny); got.Hour() != 6 || got.Day() != 2 {
		t.Fatalf("covered_through = %v, want 06:00 New York on Nov 2", got)
	}
}

func TestCatchupCollapsesMissedFirings(t *testing.T) {
	dir := fixture(t, "ok")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.Tick(ctx, t0) // register
	// Ten minutes of downtime: ten missed firings must collapse into ONE run.
	s.Tick(ctx, t0.Add(10*time.Minute))
	ledger := readFile(t, filepath.Join(dir, "memory", "ledgers", "fake.md"))
	if got := strings.Count(ledger, "ran run_"); got != 1 {
		t.Fatalf("expected exactly 1 collapsed catch-up run, got %d: %q", got, ledger)
	}
	st := loadState(t, s)
	if st.Pending != nil {
		t.Fatalf("pending should be clear: %+v", st.Pending)
	}
}

func TestRetrySameRunIDThenAbandon(t *testing.T) {
	dir := fixture(t, "fail")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.Tick(ctx, t0) // register
	now := t0.Add(time.Minute)
	s.Tick(ctx, now) // attempt 1 fails
	st := loadState(t, s)
	if st.Pending == nil || st.Pending.Attempts != 1 {
		t.Fatalf("expected pending with 1 attempt, got %+v", st.Pending)
	}
	runID := st.Pending.RunID

	// Drive retries through backoff until abandonment.
	for range MaxAttempts - 1 {
		st = loadState(t, s)
		if st.Pending == nil {
			break
		}
		now = schedule.NextRetryAt(st.Pending).Add(time.Second)
		s.Tick(ctx, now)
	}
	st = loadState(t, s)
	if st.Pending != nil {
		t.Fatalf("expected abandonment after %d attempts, still pending: %+v", MaxAttempts, st.Pending)
	}
	if !st.Watermark.After(t0) {
		t.Fatalf("abandonment should advance the watermark")
	}
	tasks := readFile(t, filepath.Join(dir, "memory", "tasks.md"))
	if !strings.Contains(tasks, "task-"+runID) || !strings.Contains(tasks, "abandoned after 5 attempts") {
		t.Fatalf("tasks missing human-owned abandonment task for %s: %q", runID, tasks)
	}
	events := readFile(t, filepath.Join(dir, "memory", "events.md"))
	if !strings.Contains(events, runID) {
		t.Fatalf("events missing failure entries for %s: %q", runID, events)
	}
	records := readFile(t, filepath.Join(dir, "memory", "runs.jsonl"))
	if got := strings.Count(records, runID); got != MaxAttempts {
		t.Fatalf("expected %d attempt records for %s, got %d", MaxAttempts, runID, got)
	}
}

func TestBackoffHoldsBetweenAttempts(t *testing.T) {
	dir := fixture(t, "fail")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.Tick(ctx, t0)
	s.Tick(ctx, t0.Add(time.Minute)) // attempt 1
	// Immediately after, a tick must NOT retry (backoff).
	s.Tick(ctx, t0.Add(time.Minute).Add(10*time.Second))
	st := loadState(t, s)
	if st.Pending.Attempts != 1 {
		t.Fatalf("backoff violated: %d attempts", st.Pending.Attempts)
	}
}

// driveToAbandonment ticks through a full failed logical run (5 attempts
// with backoff) and returns the time after abandonment.
func driveToAbandonment(t *testing.T, s *Supervisor, from time.Time) time.Time {
	t.Helper()
	ctx := context.Background()
	now := from.Add(time.Minute)
	s.Tick(ctx, now) // mint pending + attempt 1
	for {
		st := loadState(t, s)
		if st.Pending == nil {
			return now
		}
		now = schedule.NextRetryAt(st.Pending).Add(time.Second)
		s.Tick(ctx, now)
	}
}

func TestCircuitBreakerTripsAndRecovers(t *testing.T) {
	dir := fixture(t, "fail")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)
	s.Tick(ctx, t0) // register

	now := t0
	for range 3 {
		now = driveToAbandonment(t, s, now)
	}
	st := loadState(t, s)
	if st.ConsecutiveAbandons != 3 || !st.CoolingDown(now.Add(time.Minute)) {
		t.Fatalf("breaker should be tripped after 3 abandonments: %+v", st)
	}
	// While cooling down: ticks mint no new pending runs.
	s.Tick(ctx, now.Add(2*time.Minute))
	if st = loadState(t, s); st.Pending != nil {
		t.Fatalf("no runs should start during cool-down: %+v", st.Pending)
	}
	events := readFile(t, filepath.Join(dir, "memory", "events.md"))
	if !strings.Contains(events, "circuit breaker tripped") {
		t.Fatalf("breaker event missing: %q", events)
	}

	// After the cool-down, the model recovers: one success resets everything.
	binDir := strings.SplitN(os.Getenv("PATH"), string(os.PathListSeparator), 2)[0]
	os.WriteFile(filepath.Join(binDir, "fake-mode"), []byte("ok\n"), 0o644)
	after := st.CooldownUntil.Add(time.Minute)
	s.Tick(ctx, after)
	if st = loadState(t, s); st.Pending != nil || st.ConsecutiveAbandons != 0 || st.CoolingDown(after) {
		t.Fatalf("success should reset the breaker: %+v", st)
	}
	ledger := readFile(t, filepath.Join(dir, "memory", "ledgers", "fake.md"))
	if !strings.Contains(ledger, "ran run_") {
		t.Fatalf("recovery run should have executed: %q", ledger)
	}
}

// A consumer routine gets an injected inbox, consumes it via the marker, and
// its cursor advances -- the next inbox starts where the last one ended.
func TestConsumerCursorAdvances(t *testing.T) {
	dir := fixture(t, "consume")
	os.WriteFile(filepath.Join(dir, "routines", "every-minute.md"), []byte(
		"---\nschedule: \"* * * * *\"\nevents: false\nconsumes: memory\n---\nReport the fake thing.\n"), 0o644)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.Tick(ctx, t0)                     // register
	s.Tick(ctx, t0.Add(61*time.Second)) // run 1: first-run inbox, consume
	inbox := readFile(t, filepath.Join(dir, "memory", "inbox-copy.md"))
	if !strings.Contains(inbox, "first run") || !strings.Contains(inbox, "No pending changes") {
		t.Fatalf("first inbox should be empty-at-current-state: %q", inbox)
	}
	c1, err := memory.At(dir).LoadCursor("every-minute")
	if err != nil || c1 == nil {
		t.Fatalf("cursor should exist after consume: %+v, %v", c1, err)
	}

	s.Tick(ctx, t0.Add(121*time.Second)) // run 2: feed carries run 1's commit
	inbox = readFile(t, filepath.Join(dir, "memory", "inbox-copy.md"))
	if !strings.Contains(inbox, "Run every-minute") {
		t.Fatalf("second inbox should carry run 1's completion commit: %q", inbox)
	}
	c2, err := memory.At(dir).LoadCursor("every-minute")
	if err != nil || c2 == nil || c2.ConsumedThrough == c1.ConsumedThrough {
		t.Fatalf("cursor should have advanced: %+v -> %+v, %v", c1, c2, err)
	}
}

// A rewritten origin must halt dispatch -- and stay halted on every later
// tick. Runs taken while blocked would act under identities that exist only
// in this container: lost on replacement, duplicated on recovery.
func TestRewrittenOriginHaltsDispatch(t *testing.T) {
	dir := fixture(t, "ok")
	base := t.TempDir()
	bare := filepath.Join(base, "origin.git")
	run := func(cwd string, args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = cwd
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	run(base, "git", "init", "-q", "-b", "main", "--bare", bare)
	run(dir, "git", "remote", "add", "origin", bare)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.Tick(ctx, t0)                     // register
	s.Tick(ctx, t0.Add(61*time.Second)) // one run completes, memory pushed
	ledger := readFile(t, filepath.Join(dir, "memory", "ledgers", "fake.md"))
	if got := strings.Count(ledger, "ran run_"); got != 1 {
		t.Fatalf("expected 1 run before the rewrite, got %d: %q", got, ledger)
	}

	// Rewrite the memory branch on origin out from under the supervisor.
	c := filepath.Join(base, "c")
	run(base, "git", "clone", "-q", "-b", "memory", bare, c)
	run(c, "git", "-c", "user.name=x", "-c", "user.email=x@x", "commit", "--amend", "-q", "--no-edit", "-m", "rewritten")
	run(c, "git", "push", "-q", "--force", "origin", "memory")

	// Every subsequent tick refuses to dispatch.
	for i := 0; i < 3; i++ {
		s.Tick(ctx, t0.Add(time.Duration(2+i)*time.Minute))
	}
	ledger = readFile(t, filepath.Join(dir, "memory", "ledgers", "fake.md"))
	if got := strings.Count(ledger, "ran run_"); got != 1 {
		t.Fatalf("dispatch continued after rewrite: %d runs, %q", got, ledger)
	}
	if !s.syncBlocked {
		t.Fatal("supervisor should be sync-blocked after a rewrite")
	}
}

// Two instances, one origin. A tick has no bounded wall time -- every due
// routine executes serially to completion -- so a lease heartbeated only at
// the top of the tick goes stale while the holder is still working, and a
// second instance booting into that window (a rolling deploy's overlap) reads
// an expired lease and starts dispatching the very runs the first is running.
// The heartbeat has to keep up with the work.
func TestLeaseStaysLiveThroughALongTick(t *testing.T) {
	dir := fixture(t, "slow")
	base := t.TempDir()
	bare := filepath.Join(base, "origin.git")
	run := func(cwd string, args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = cwd
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	run(base, "git", "init", "-q", "-b", "main", "--bare", bare)

	// Three routines due in the same tick: the tick takes several times one
	// run's wall time, which is exactly the gap a per-tick heartbeat leaves.
	writeRoutines := func(dir string) {
		for _, name := range []string{"every-minute", "second", "third"} {
			os.WriteFile(filepath.Join(dir, "routines", name+".md"), []byte(
				"---\nschedule: \"* * * * *\"\n---\nDo the fake thing.\n"), 0o644)
		}
	}
	writeRoutines(dir)
	run(dir, "git", "remote", "add", "origin", bare)

	holder := newSupervisor(t, dir)
	// Scaled down, with room on both sides: runs sleep 3s, so a per-tick
	// heartbeat is 7.5s old at the assertion below (expired) while a per-run
	// one is 1.5s old (live) -- neither margin is close enough that a slow
	// git push flips the verdict.
	holder.leaseTTL = 6 * time.Second
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)
	holder.Tick(ctx, t0) // register

	binDir := strings.SplitN(os.Getenv("PATH"), string(os.PathListSeparator), 2)[0]
	started := filepath.Join(binDir, "started")
	waitForRuns := func(n int) {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			if got := strings.Count(readFile(t, started), "run_"); got >= n {
				return
			} else if time.Now().After(deadline) {
				t.Fatalf("only %d of %d runs started", got, n)
			}
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		holder.Tick(ctx, t0.Add(61*time.Second))
	}()
	waitForRuns(1) // intent is pushed: the second instance can adopt memory from origin

	other := t.TempDir()
	os.MkdirAll(filepath.Join(other, "routines"), 0o755)
	os.WriteFile(filepath.Join(other, "openroutines.yml"), []byte(agentYAML("UTC")), 0o644)
	writeRoutines(other)
	run(other, "git", "init", "-q", "-b", "main", ".")
	run(other, "git", "remote", "add", "origin", bare)
	second := newSupervisor(t, other)
	second.InstanceID = "second-instance"
	second.leaseTTL = holder.leaseTTL

	waitForRuns(3)                      // the holder is deep into its tick
	time.Sleep(1500 * time.Millisecond) // older than a per-tick heartbeat could survive

	acquireCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if err := second.acquireLease(acquireCtx); err == nil {
		t.Fatal("second instance took a live lease while the first was mid-tick")
	}
	// The second instance's memory is the holder's, adopted from origin, so
	// the ledger cannot say who ran what: count launches instead. Three
	// routines are due, and only the lease holder may run them.
	second.Tick(ctx, t0.Add(61*time.Second))
	if got := strings.Count(readFile(t, started), "run_"); got != 3 {
		t.Fatalf("second instance dispatched behind the lease holder: %d runs launched, want 3", got)
	}

	<-done
	if got := strings.Count(readFile(t, filepath.Join(dir, "memory", "ledgers", "fake.md")), "ran run_"); got != 3 {
		t.Fatalf("lease holder should have run all 3 routines, got %d", got)
	}
}

// The attempt that spawns a model process must be committed and pushed before
// it starts. Production recovery is container replacement: an attempt that
// takes the container down with it (OOM, host loss, eviction) never settles,
// and a replacement that materializes memory from origin and reads attempts: 0
// dispatches again -- forever, since the retry budget never drains.
func TestAttemptIsDurableBeforeTheModelStarts(t *testing.T) {
	dir := fixture(t, "probe")
	withOrigin(t, dir)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.Tick(ctx, t0)                     // register
	s.Tick(ctx, t0.Add(61*time.Second)) // mint the run, attempt 1

	seen := replacementState(t, "every-minute")
	if seen == nil || seen.Pending == nil {
		t.Fatalf("a replacement mid-attempt should see the pending run, got %+v", seen)
	}
	if seen.Pending.Attempts != 1 {
		t.Fatalf("a replacement mid-attempt should see attempts=1, got %d", seen.Pending.Attempts)
	}
}

// The reservation belongs to the attempt, not to the tick that scheduled it:
// a container lost mid-attempt must cost a retry only for the run that was
// actually running. Otherwise one routine that reliably kills its container
// drains the budget of everything else that happened to be due alongside it
// -- and catch-up after downtime makes every routine due at once.
func TestOnlyTheRunningAttemptIsReserved(t *testing.T) {
	dir := fixture(t, "probe")
	if err := os.WriteFile(filepath.Join(dir, "routines", "second.md"), []byte(
		"---\nschedule: \"* * * * *\"\n---\nDo the other fake thing.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withOrigin(t, dir)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.Tick(ctx, t0)                     // register both
	s.Tick(ctx, t0.Add(61*time.Second)) // mint both, dispatch serially

	// The snapshot is the first routine's spawn: both runs exist durably, but
	// only the one that started has spent an attempt.
	reserved := 0
	for _, name := range []string{"every-minute", "second"} {
		seen := replacementState(t, name)
		if seen == nil || seen.Pending == nil {
			t.Fatalf("%s should be pending in the replacement, got %+v", name, seen)
		}
		reserved += seen.Pending.Attempts
	}
	if reserved != 1 {
		t.Fatalf("a lost container should cost one attempt, not one per routine due that tick: %d reserved", reserved)
	}
}

// Attempts that never settle still spend the retry budget, and a spent budget
// is abandoned where the tick notices it: settlement is the usual place, but a
// run that kills its container never reaches settlement.
func TestSpentAttemptsAbandonWithoutSettlement(t *testing.T) {
	dir := fixture(t, "ok")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.Tick(ctx, t0) // register
	// What a run that killed the supervisor MaxAttempts times leaves behind.
	scheduled := t0.Add(time.Minute)
	st := loadState(t, s)
	st.Pending = &schedule.Pending{
		RunID: "run_crashloop", ScheduledFor: scheduled, CoveredThrough: scheduled,
		CreatedAt: scheduled, Attempts: MaxAttempts, LastAttemptAt: scheduled,
	}
	if err := st.Save(s.stateDir()); err != nil {
		t.Fatal(err)
	}

	s.Tick(ctx, t0.Add(time.Hour))

	if st = loadState(t, s); st.Pending != nil {
		t.Fatalf("a run with no attempts left must be abandoned, still pending: %+v", st.Pending)
	}
	if !st.Watermark.Equal(scheduled) {
		t.Fatalf("abandonment should advance the watermark to %v, got %v", scheduled, st.Watermark)
	}
	if got := runCount(t, dir); got != 0 {
		t.Fatalf("no attempt should have been dispatched, got %d runs", got)
	}
	tasks := readFile(t, filepath.Join(dir, "memory", "tasks.md"))
	if !strings.Contains(tasks, "task-run_crashloop") {
		t.Fatalf("abandonment should hand run_crashloop to a human: %q", tasks)
	}
}

// A tick that wrote pending state and then failed to commit it leaves the
// record on disk and nowhere else -- and the next tick, seeing a pending run,
// mints nothing and would dispatch under an identity that exists only here.
// Persist-before-act cannot rest on control flow: whatever the worktree
// carries is committed and pushed before anything runs.
func TestUncommittedIntentIsPushedBeforeDispatch(t *testing.T) {
	dir := fixture(t, "probe")
	withOrigin(t, dir)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.Tick(ctx, t0) // register

	// The aftermath of a failed intent commit: pending state on disk only.
	st := loadState(t, s)
	st.Pending = &schedule.Pending{RunID: "run_orphaned", ScheduledFor: t0, CoveredThrough: t0, CreatedAt: t0}
	if err := st.Save(s.stateDir()); err != nil {
		t.Fatal(err)
	}
	if n := memory.At(dir).Status().Uncommitted; n == 0 {
		t.Fatal("precondition: the pending record should be uncommitted")
	}

	s.Tick(ctx, t0.Add(time.Minute)) // dispatches the orphaned pending run

	seen := replacementState(t, "every-minute")
	if seen == nil || seen.Pending == nil || seen.Pending.RunID != "run_orphaned" {
		t.Fatalf("the run's identity should have reached origin before it acted, replacement sees %+v", seen)
	}
}

// A supervisor that cannot record what it is about to do must not do it --
// and has to say so where a person will look. A stale index lock is what a
// killed git leaves behind; on logs alone the agent would just quietly stop
// running anything.
func TestFailedIntentCommitHoldsRunsAndRecordsATask(t *testing.T) {
	dir := fixture(t, "ok")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.Tick(ctx, t0) // register

	cmd := exec.Command("git", "rev-parse", "--absolute-git-dir")
	cmd.Dir = filepath.Join(dir, "memory")
	gitDir, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(strings.TrimSpace(string(gitDir)), "index.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	s.Tick(ctx, t0.Add(61*time.Second))

	if got := runCount(t, dir); got != 0 {
		t.Fatalf("an intent that cannot be committed must not dispatch, got %d runs", got)
	}
	tasks := readFile(t, filepath.Join(dir, "memory", "tasks.md"))
	if !strings.Contains(tasks, "intent commit failed") {
		t.Fatalf("a halted supervisor should hand the blocker to a human: %q", tasks)
	}
}

// A supervised run's record carries the usage the runner captured from the
// attempt home's session storage, plus the resolved model.
func TestRunRecordCarriesUsage(t *testing.T) {
	dir := fixture(t, "ok")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)
	s.Tick(ctx, t0)
	s.Tick(ctx, t0.Add(time.Minute))
	records := readFile(t, filepath.Join(dir, "memory", "runs.jsonl"))
	last := ""
	for _, l := range strings.Split(strings.TrimSpace(records), "\n") {
		if l != "" {
			last = l
		}
	}
	for _, want := range []string{`"model":"fake/model"`, `"input":100`, `"output":20`, `"cost_reported":0.01`} {
		if !strings.Contains(last, want) {
			t.Fatalf("run record missing %s: %s", want, last)
		}
	}
}

// Env delivery of the master key stays supported -- some platforms cannot
// mount a file -- but boot names it as the weaker choice, because a
// deployment that picked it once is never told again.
func TestBootWarnsOnEnvDeliveredMasterKey(t *testing.T) {
	dir := fixture(t, "ok")
	s := newSupervisor(t, dir)
	var out bytes.Buffer
	s.Log = log.New(&out, "", 0)

	t.Setenv(creds.EnvMasterKey, creds.GenerateKey())
	s.warnKeyDelivery()
	if out.Len() > 0 {
		t.Errorf("outside the container there is nothing to warn about: %q", out.String())
	}

	t.Setenv("OPENROUTINES_IN_CONTAINER", "1")
	s.warnKeyDelivery()
	if !strings.Contains(out.String(), creds.EnvMasterKeyFile) {
		t.Errorf("warning should point at the file delivery: %q", out.String())
	}

	keyFile := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(keyFile, []byte(creds.GenerateKey()), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(creds.EnvMasterKeyFile, keyFile)
	out.Reset()
	s.warnKeyDelivery()
	if !strings.Contains(out.String(), creds.EnvMasterKey) {
		t.Errorf("a leftover variable still publishes the value, file delivery or not: %q", out.String())
	}

	t.Setenv(creds.EnvMasterKey, "")
	out.Reset()
	s.warnKeyDelivery()
	if out.Len() > 0 {
		t.Errorf("file delivery with no leftover variable is the recommended path: %q", out.String())
	}
}
