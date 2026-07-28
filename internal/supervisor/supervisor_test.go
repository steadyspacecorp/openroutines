package supervisor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/memory"
	"github.com/steadyspacecorp/openroutines/internal/schedule"
)

// fakeOpencode is a stand-in for the real binary: it reads fake-mode from
// its own directory (the workspace is allow-list built and carries no test
// scaffolding) to decide whether to succeed (writing memory) or fail.
const fakeOpencode = `#!/bin/sh
mode=$(cat "$(dirname "$0")/fake-mode" 2>/dev/null || echo ok)
# Every mode leaves the session storage a real opencode leaves in the
# attempt home -- the surface the runner captures token usage from.
mkdir -p .home/.local/share/opencode/storage/message/ses_fake
printf '{"role":"assistant","modelID":"fake","tokens":{"input":100,"output":20,"reasoning":5,"cache":{"read":0,"write":0}},"cost":0.01}' \
  > .home/.local/share/opencode/storage/message/ses_fake/msg_1.json
case "$mode" in
  fail) echo "boom"; exit 1 ;;
  consume) cp inbox.md memory/inbox-copy.md
     : > CONSUMED
     echo "consumed" ;;
  *) mkdir -p memory/ledgers
     echo "ran $OPENROUTINES_RUN_ID $OPENROUTINES_ATTEMPT_ID" >> memory/ledgers/fake.md
     echo "done" ;;
esac
`

const agentYAML = `name: test-agent
description: Tests the supervisor
owner:
  name: CI
  email: ci@example.invalid
timezone: UTC
defaults:
  model: fake/model
  timeout: 30s
`

// fixture builds an agent repo (no origin: local mode) and puts a fake
// opencode on PATH.
func fixture(t *testing.T, mode string) string {
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
	os.WriteFile(filepath.Join(dir, "openroutines.yml"), []byte(agentYAML), 0o644)
	os.MkdirAll(filepath.Join(dir, "routines"), 0o755)
	os.WriteFile(filepath.Join(dir, "routines", "every-minute.md"), []byte(
		"---\nschedule: \"* * * * *\"\n---\nDo the fake thing.\n"), 0o644)
	binDir := t.TempDir()
	os.WriteFile(filepath.Join(binDir, "opencode"), []byte(fakeOpencode), 0o755)
	os.WriteFile(filepath.Join(binDir, "fake-mode"), []byte(mode+"\n"), 0o644)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OPENROUTINES_NATIVE", "1") // tests use the fake opencode, not the container
	return dir
}

func newSupervisor(t *testing.T, dir string) *Supervisor {
	t.Helper()
	if err := memory.EnsureWorktree(dir); err != nil {
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
	c1, err := memory.LoadCursor(dir, "every-minute")
	if err != nil || c1 == nil {
		t.Fatalf("cursor should exist after consume: %+v, %v", c1, err)
	}

	s.Tick(ctx, t0.Add(121*time.Second)) // run 2: feed carries run 1's commit
	inbox = readFile(t, filepath.Join(dir, "memory", "inbox-copy.md"))
	if !strings.Contains(inbox, "Run every-minute") {
		t.Fatalf("second inbox should carry run 1's completion commit: %q", inbox)
	}
	c2, err := memory.LoadCursor(dir, "every-minute")
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
