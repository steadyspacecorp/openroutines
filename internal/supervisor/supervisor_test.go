package supervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/knowledge"
	"github.com/steadyspacecorp/openroutines/internal/logging/logtest"
	"github.com/steadyspacecorp/openroutines/internal/repository"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/runner"
	"github.com/steadyspacecorp/openroutines/internal/sandbox"
	"github.com/steadyspacecorp/openroutines/internal/schedule"
)

const fakeOpencode = `#!/bin/sh
d=$(dirname "$0")
mode=$(cat "$d/fake-mode" 2>/dev/null || echo ok)
msg=.home/.local/share/opencode/storage/message/ses_fake/msg_1.json
# The capture and session-export surface, answered before any run side
# effect: after the attempt the runner asks for the session list and the
# export of each session, from the same working directory the run used.
case "$1" in
  session) printf '[{"id":"ses_fake"}]'; exit 0 ;;
  export) printf '{"messages":[{"info":%s}]}' "$(cat "$msg")"; exit 0 ;;
esac
# A run invocation: record the argv the runner spawned it with.
printf '%s\n' "$@" > "$d/argv"
# Every run leaves the message record a real opencode persists -- the
# surface export renders, where the runner reads token usage and how the
# session ended.
mkdir -p .home/.local/share/opencode/storage/message/ses_fake
printf '{"role":"assistant","modelID":"fake","finish":"stop","tokens":{"input":100,"output":20,"reasoning":5,"cache":{"read":0,"write":0}},"cost":0.01}' \
  > "$msg"
case "$mode" in
  fail) echo "boom" >&2; exit 1 ;;
  stalled) # The agent loop died on a rejected tool call: the session never
     # finished its turn, no knowledge was written, and opencode still exits 0.
     printf '{"role":"assistant","modelID":"fake","finish":"tool-calls","tokens":{"input":100,"output":20,"reasoning":5,"cache":{"read":0,"write":0}},"cost":0.01}' \
       > "$msg"
     echo "stalled" ;;
  slow) echo "$OPENROUTINES_RUN_ID" >> "$(dirname "$0")/started"
     sleep 3
     mkdir -p knowledge/ledgers
     echo "ran $OPENROUTINES_RUN_ID $OPENROUTINES_ATTEMPT_ID" >> knowledge/ledgers/fake.md
     echo "slept" ;;
  blocked) # A run that only ends when something kills it. Staged knowledge is
     # written up front, so a test asserting that a killed run's knowledge was
     # not imported is testing an import that had something to import. The
     # sleep outlives the routine's timeout: a kill that never arrives ends
     # the attempt as a timeout, which settles differently from a cancel,
     # rather than as a run that completed on its own.
     mkdir -p knowledge/ledgers
     echo "ran $OPENROUTINES_RUN_ID $OPENROUTINES_ATTEMPT_ID" >> knowledge/ledgers/fake.md
     echo "$OPENROUTINES_RUN_ID" >> "$d/started"
     sleep 60 ;;
  detach) sleep 60 </dev/null >/dev/null 2>&1 &
     echo $! > "$d/detached.pid"
     echo "detached" ;;
  consume) cp changes.md knowledge/changes-copy.md
     : > knowledge/CONSUMED
     echo "consumed" ;;
  probe) [ -d "$d/replacement" ] || git clone -q -b knowledge "$(cat "$d/origin")" "$d/replacement" || true
     echo "probed" ;;
  orphan) # A detached grandchild in its own process group, holding the run's
     # stdout: the group kill cannot reach it and it outlives the attempt.
     if command -v setsid >/dev/null 2>&1; then setsid sleep 120 &
     else (set -m; sleep 120 &) fi
     sleep 120 ;;
  *) mkdir -p knowledge/ledgers
     echo "ran $OPENROUTINES_RUN_ID $OPENROUTINES_ATTEMPT_ID" >> knowledge/ledgers/fake.md
     echo "done" ;;
esac
`

func agentYAML(tz string) string {
	return fmt.Sprintf(`name: test-agent
instructions: Tests the supervisor
owner:
  name: CI
  email: ci@example.invalid
timezone: %s
defaults:
  model: fake/model
  timeout: 30s
`, tz)
}

func fixture(t *testing.T, mode string) string {
	t.Helper()
	return fixtureIn(t, mode, "UTC", "every-minute", "* * * * *")
}

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
	t.Setenv("OPENROUTINES_NATIVE", "1")
	return dir
}

func newSupervisor(t *testing.T, dir string) *Supervisor {
	t.Helper()
	if err := knowledge.NewStore(dir).Ensure(); err != nil {
		t.Fatal(err)
	}
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func usurpLease(t *testing.T, s *Supervisor) (stop func()) {
	t.Helper()
	take := func() error {
		lease, err := s.repo.ReadLease()
		if err != nil {
			return err
		}
		if lease == nil {
			return fmt.Errorf("no lease to usurp")
		}
		_, err = s.repo.WriteLease("usurper", time.Now(), lease.SHA)
		return err
	}
	var err error
	for deadline := time.Now().Add(10 * time.Second); ; {
		if err = take(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("could not usurp the lease: %v", err)
		}
	}
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-quit:
				return
			case <-time.After(s.lease.ttl / 4):
				_ = take()
			}
		}
	}()
	return func() {
		close(quit)
		<-done
	}
}

func loadState(t *testing.T, s *Supervisor) *schedule.State {
	t.Helper()
	st, err := schedule.Load(s.stateDir(), "every-minute")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func optInConcurrency(t *testing.T, dir string, n int) {
	t.Helper()
	path := filepath.Join(dir, "openroutines.yml")
	cfg, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(cfg, fmt.Appendf(nil, "concurrency: %d\n", n)...), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (s *Supervisor) tickWait(ctx context.Context, now time.Time) {
	s.Tick(ctx, now)
	s.pool.runs.Wait()
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

func fakeBinDir() string {
	return strings.SplitN(os.Getenv("PATH"), string(os.PathListSeparator), 2)[0]
}

func withOrigin(t *testing.T, dir string) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "origin.git")
	runCmd(t, "", "git", "init", "-q", "-b", "main", "--bare", bare)
	runCmd(t, dir, "git", "remote", "add", "origin", bare)
	if err := os.WriteFile(filepath.Join(fakeBinDir(), "origin"), []byte(bare+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return bare
}

func gitTry(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitTry(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return out
}

func replacementState(t *testing.T, name string) *schedule.State {
	t.Helper()
	st, err := schedule.Load(filepath.Join(fakeBinDir(), "replacement", "state"), name)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestSupervisedRunAsksForJSONEvents(t *testing.T) {
	dir := fixture(t, "ok")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)
	s.tickWait(ctx, t0)
	s.tickWait(ctx, t0.Add(61*time.Second))
	argv := readFile(t, filepath.Join(fakeBinDir(), "argv"))
	if !strings.Contains(argv, "--format\njson") {
		t.Fatalf("supervised runs must pass --format json, argv:\n%s", argv)
	}
}

func TestRegisterThenRunAdvancesWatermark(t *testing.T) {
	dir := fixture(t, "ok")
	sessionDir := t.TempDir()
	t.Setenv(runner.EnvSessionDir, sessionDir)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)
	st := loadState(t, s)
	if st == nil || st.Pending != nil {
		t.Fatalf("expected registered state with no pending, got %+v", st)
	}
	if ledger := readFile(t, filepath.Join(dir, "knowledge", "ledgers", "fake.md")); ledger != "" {
		t.Fatalf("nothing should have run yet, ledger: %q", ledger)
	}

	s.tickWait(ctx, t0.Add(61*time.Second))
	st = loadState(t, s)
	if st.Pending != nil {
		t.Fatalf("pending should be cleared after success: %+v", st.Pending)
	}
	if !st.Watermark.After(t0) {
		t.Fatalf("watermark should have advanced past %v, got %v", t0, st.Watermark)
	}
	ledger := readFile(t, filepath.Join(dir, "knowledge", "ledgers", "fake.md"))
	if !strings.Contains(ledger, "ran run_") || !strings.Contains(ledger, "attempt_01") {
		t.Fatalf("ledger missing run evidence: %q", ledger)
	}
	records := readFile(t, filepath.Join(dir, "knowledge", "runs.jsonl"))
	if !strings.Contains(records, `"outcome":"completed"`) {
		t.Fatalf("run record missing: %q", records)
	}

	stored, err := filepath.Glob(filepath.Join(sessionDir, "*_every-minute_run_*_ses_fake.json"))
	if err != nil || len(stored) != 1 {
		t.Fatalf("expected one exported session, got %v (%v)", stored, err)
	}
	if got := readFile(t, stored[0]); !strings.Contains(got, `"finish":"stop"`) {
		t.Fatalf("stored session data does not match what the run wrote: %q", got)
	}
}

func TestSessionThatEndedMidTurnIsNotCompleted(t *testing.T) {
	dir := fixture(t, "stalled")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)
	s.tickWait(ctx, t0.Add(61*time.Second))

	records := readFile(t, filepath.Join(dir, "knowledge", "runs.jsonl"))
	if strings.Contains(records, `"outcome":"completed"`) {
		t.Fatalf("an exit code alone must not report a run completed: %q", records)
	}
	if !strings.Contains(records, `"outcome":"crashed"`) {
		t.Fatalf("run record should be crashed: %q", records)
	}
	if !strings.Contains(records, "tool-calls") {
		t.Errorf("the run record should carry why the session failed: %q", records)
	}
	st := loadState(t, s)
	if st.Pending == nil {
		t.Fatal("the run must stay pending so it retries -- not silently skipped")
	}
	if st.Watermark.After(t0) {
		t.Errorf("the watermark must not advance past unfinished work, got %v", st.Watermark)
	}
	if events := readFile(t, filepath.Join(dir, "knowledge", "events.md")); !strings.Contains(events, "crashed") {
		t.Errorf("the failure should be recorded as an event: %q", events)
	}
}

func TestBrokenRoutineDoesNotFailHealthyRuns(t *testing.T) {
	dir := fixture(t, "ok")
	os.WriteFile(filepath.Join(dir, "routines", "typo.md"), []byte(
		"---\nschedule: \"* * * * *\"\nactve: false\n---\nBroken.\n"), 0o644)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)
	s.tickWait(ctx, t0.Add(61*time.Second))

	st := loadState(t, s)
	if st.Pending != nil {
		t.Fatalf("the healthy routine should have completed, not retried: %+v", st.Pending)
	}
	ledger := readFile(t, filepath.Join(dir, "knowledge", "ledgers", "fake.md"))
	if !strings.Contains(ledger, "ran run_") {
		t.Fatalf("the healthy routine should have run: %q", ledger)
	}
	records := readFile(t, filepath.Join(dir, "knowledge", "runs.jsonl"))
	if !strings.Contains(records, `"outcome":"completed"`) {
		t.Fatalf("run record missing: %q", records)
	}

	events := readFile(t, filepath.Join(dir, "knowledge", "events.md"))
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
	s.tickWait(ctx, t0.Add(122*time.Second))
	if events := readFile(t, filepath.Join(dir, "knowledge", "events.md")); !strings.Contains(events, "routine typo loads again") {
		t.Errorf("the repair should be recorded too: %q", events)
	}
}

func TestAgentOwnedRoutineIsScheduledOverBrokenPluginRoutine(t *testing.T) {
	dir := fixture(t, "ok")
	os.MkdirAll(filepath.Join(dir, ".openroutines", "plugins", "demo", "routines"), 0o755)
	os.WriteFile(filepath.Join(dir, ".openroutines", "plugins", "demo", "routines", "every-minute.md"), []byte(
		"---\nschedule: \"* * * * *\"\nactve: false\n---\nBroken.\n"), 0o644)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)

	if st := loadState(t, s); st == nil {
		t.Error("the agent-owned routine should be registered")
	}
	tasks := readFile(t, filepath.Join(dir, "knowledge", "tasks.md"))
	if strings.Contains(tasks, "abandoned") {
		t.Errorf("no run should have been minted, let alone abandoned:\n%s", tasks)
	}
	events := readFile(t, filepath.Join(dir, "knowledge", "events.md"))
	if strings.Contains(events, "routine every-minute does not load") {
		t.Errorf("the shadowed plugin error should not be recorded: %q", events)
	}
}

func TestScheduleHoldsAgentWallClockAcrossDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database")
	}
	defer func(l *time.Location) { time.Local = l }(time.Local)
	time.Local = time.UTC

	dir := fixtureIn(t, "fail", "America/New_York", "daily", "0 6 * * *")
	s := newSupervisor(t, dir)
	ctx := context.Background()

	s.tickWait(ctx, time.Date(2026, 10, 31, 12, 0, 0, 0, ny))
	s.tickWait(ctx, time.Date(2026, 11, 2, 12, 0, 0, 0, ny))

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

func TestDetachedDescendantDoesNotSurviveACleanRun(t *testing.T) {
	dir := fixture(t, "detach")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)
	s.tickWait(ctx, t0.Add(61*time.Second))

	var pid int
	if _, err := fmt.Sscan(readFile(t, filepath.Join(fakeBinDir(), "detached.pid")), &pid); err != nil || pid == 0 {
		t.Fatalf("the fake run did not report a detached child: %v", err)
	}
	for range 40 {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("detached child %d outlived the run", pid)
}

func TestCatchupCollapsesMissedFirings(t *testing.T) {
	dir := fixture(t, "ok")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)
	s.tickWait(ctx, t0.Add(10*time.Minute))
	ledger := readFile(t, filepath.Join(dir, "knowledge", "ledgers", "fake.md"))
	if got := strings.Count(ledger, "ran run_"); got != 1 {
		t.Fatalf("expected exactly 1 collapsed catch-up run, got %d: %q", got, ledger)
	}
	st := loadState(t, s)
	if st.Pending != nil {
		t.Fatalf("pending should be clear: %+v", st.Pending)
	}
}

func TestFailedAttemptDiagnosticsPassThrough(t *testing.T) {
	failOnce := func(t *testing.T) *logtest.Log {
		t.Helper()
		s := newSupervisor(t, fixture(t, "fail"))
		logs := logtest.Capture(t)
		ctx := context.Background()
		t0 := time.Now().Truncate(time.Minute)
		s.tickWait(ctx, t0)
		s.tickWait(ctx, t0.Add(time.Minute))
		return logs
	}

	t.Run("no session storage designated", func(t *testing.T) {
		t.Setenv(runner.EnvSessionDir, "")
		failOnce(t).Expect("boom routine=every-minute run_id=run_")
	})

	t.Run("session storage designated", func(t *testing.T) {
		sessions := t.TempDir()
		t.Setenv(runner.EnvSessionDir, sessions)
		failOnce(t).Expect("boom routine=every-minute run_id=run_")
		stored, err := filepath.Glob(filepath.Join(sessions, "*_every-minute_run_*_ses_fake.json"))
		if err != nil || len(stored) != 1 {
			t.Fatalf("the failed attempt's sessions should have landed, got %v (%v)", stored, err)
		}
	})
}

func TestRetrySameRunIDThenAbandon(t *testing.T) {
	dir := fixture(t, "fail")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)
	now := t0.Add(time.Minute)
	s.tickWait(ctx, now)
	st := loadState(t, s)
	if st.Pending == nil || st.Pending.Attempts != 1 {
		t.Fatalf("expected pending with 1 attempt, got %+v", st.Pending)
	}
	runID := st.Pending.RunID

	for range MaxAttempts - 1 {
		st = loadState(t, s)
		if st.Pending == nil {
			break
		}
		now = schedule.NextRetryAt(st.Pending).Add(time.Second)
		s.tickWait(ctx, now)
	}
	st = loadState(t, s)
	if st.Pending != nil {
		t.Fatalf("expected abandonment after %d attempts, still pending: %+v", MaxAttempts, st.Pending)
	}
	if !st.Watermark.After(t0) {
		t.Fatalf("abandonment should advance the watermark")
	}
	tasks := readFile(t, filepath.Join(dir, "knowledge", "tasks.md"))
	if !strings.Contains(tasks, "task-"+runID) || !strings.Contains(tasks, "abandoned after 5 attempts") {
		t.Fatalf("tasks missing human-owned abandonment task for %s: %q", runID, tasks)
	}
	events := readFile(t, filepath.Join(dir, "knowledge", "events.md"))
	if !strings.Contains(events, runID) {
		t.Fatalf("events missing failure entries for %s: %q", runID, events)
	}
	records := readFile(t, filepath.Join(dir, "knowledge", "runs.jsonl"))
	if got := strings.Count(records, runID); got != MaxAttempts {
		t.Fatalf("expected %d attempt records for %s, got %d", MaxAttempts, runID, got)
	}
}

func TestBackoffHoldsBetweenAttempts(t *testing.T) {
	dir := fixture(t, "fail")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)
	s.tickWait(ctx, t0.Add(time.Minute))
	s.tickWait(ctx, t0.Add(time.Minute).Add(10*time.Second))
	st := loadState(t, s)
	if st.Pending.Attempts != 1 {
		t.Fatalf("backoff violated: %d attempts", st.Pending.Attempts)
	}
}

func driveToAbandonment(t *testing.T, s *Supervisor, from time.Time) time.Time {
	t.Helper()
	ctx := context.Background()
	now := from.Add(time.Minute)
	s.tickWait(ctx, now)
	for {
		st := loadState(t, s)
		if st.Pending == nil {
			return now
		}
		now = schedule.NextRetryAt(st.Pending).Add(time.Second)
		s.tickWait(ctx, now)
	}
}

func TestCircuitBreakerTripsAndRecovers(t *testing.T) {
	dir := fixture(t, "fail")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)
	s.tickWait(ctx, t0)

	now := t0
	for range 3 {
		now = driveToAbandonment(t, s, now)
	}
	st := loadState(t, s)
	if st.ConsecutiveAbandons != 3 || !st.CoolingDown(now.Add(time.Minute)) {
		t.Fatalf("breaker should be tripped after 3 abandonments: %+v", st)
	}
	s.tickWait(ctx, now.Add(2*time.Minute))
	if st = loadState(t, s); st.Pending != nil {
		t.Fatalf("no runs should start during cool-down: %+v", st.Pending)
	}
	events := readFile(t, filepath.Join(dir, "knowledge", "events.md"))
	if !strings.Contains(events, "circuit breaker tripped") {
		t.Fatalf("breaker event missing: %q", events)
	}

	binDir := strings.SplitN(os.Getenv("PATH"), string(os.PathListSeparator), 2)[0]
	os.WriteFile(filepath.Join(binDir, "fake-mode"), []byte("ok\n"), 0o644)
	after := st.CooldownUntil.Add(time.Minute)
	s.tickWait(ctx, after)
	if st = loadState(t, s); st.Pending != nil || st.ConsecutiveAbandons != 0 || st.CoolingDown(after) {
		t.Fatalf("success should reset the breaker: %+v", st)
	}
	ledger := readFile(t, filepath.Join(dir, "knowledge", "ledgers", "fake.md"))
	if !strings.Contains(ledger, "ran run_") {
		t.Fatalf("recovery run should have executed: %q", ledger)
	}
}

func TestConsumerCursorAdvances(t *testing.T) {
	dir := fixture(t, "consume")
	os.WriteFile(filepath.Join(dir, "routines", "every-minute.md"), []byte(
		"---\nschedule: \"* * * * *\"\nreports: true\n---\nReport the fake thing.\n"), 0o644)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)
	s.tickWait(ctx, t0.Add(61*time.Second))
	changes := readFile(t, filepath.Join(dir, "knowledge", "changes-copy.md"))
	if !strings.Contains(changes, "first run") || !strings.Contains(changes, "No pending changes") {
		t.Fatalf("first change set should be empty-at-current-state: %q", changes)
	}
	c1, err := knowledge.NewStore(dir).LoadCursor("every-minute")
	if err != nil || c1 == nil {
		t.Fatalf("cursor should exist after consume: %+v, %v", c1, err)
	}

	s.tickWait(ctx, t0.Add(121*time.Second))
	changes = readFile(t, filepath.Join(dir, "knowledge", "changes-copy.md"))
	if !strings.Contains(changes, "Run every-minute") {
		t.Fatalf("second change set should carry run 1's completion commit: %q", changes)
	}
	c2, err := knowledge.NewStore(dir).LoadCursor("every-minute")
	if err != nil || c2 == nil || c2.ConsumedThrough == c1.ConsumedThrough {
		t.Fatalf("cursor should have advanced: %+v -> %+v, %v", c1, c2, err)
	}
}

func TestUnreachableCursorAbandonsOnTheFirstAttempt(t *testing.T) {
	dir := fixture(t, "consume")
	os.WriteFile(filepath.Join(dir, "routines", "every-minute.md"), []byte(
		"---\nschedule: \"* * * * *\"\nreports: true\n---\nReport the fake thing.\n"), 0o644)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)
	if err := knowledge.NewStore(dir).SaveCursor("every-minute", knowledge.Cursor{
		ConsumedThrough: "0123456789abcdef0123456789abcdef01234567",
		ByRun:           "run_gone",
	}); err != nil {
		t.Fatal(err)
	}
	s.tickWait(ctx, t0.Add(61*time.Second))

	st := loadState(t, s)
	if st.Pending != nil {
		t.Fatalf("an unrepeatable failure should abandon at once, still pending: %+v", st.Pending)
	}
	tasks := readFile(t, filepath.Join(dir, "knowledge", "tasks.md"))
	if !strings.Contains(tasks, "abandoned after 1 attempts") {
		t.Fatalf("expected abandonment on the first attempt: %q", tasks)
	}
	if !strings.Contains(tasks, "cursors/every-minute.json") {
		t.Fatalf("task should name the cursor file to repair: %q", tasks)
	}
}

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

	s.tickWait(ctx, t0)
	s.tickWait(ctx, t0.Add(61*time.Second))
	ledger := readFile(t, filepath.Join(dir, "knowledge", "ledgers", "fake.md"))
	if got := strings.Count(ledger, "ran run_"); got != 1 {
		t.Fatalf("expected 1 run before the rewrite, got %d: %q", got, ledger)
	}

	c := filepath.Join(base, "c")
	run(base, "git", "clone", "-q", "-b", "knowledge", bare, c)
	run(c, "git", "-c", "user.name=x", "-c", "user.email=x@x", "commit", "--amend", "-q", "--no-edit", "-m", "rewritten")
	run(c, "git", "push", "-q", "--force", "origin", "knowledge")

	for i := 0; i < 3; i++ {
		s.tickWait(ctx, t0.Add(time.Duration(2+i)*time.Minute))
	}
	ledger = readFile(t, filepath.Join(dir, "knowledge", "ledgers", "fake.md"))
	if got := strings.Count(ledger, "ran run_"); got != 1 {
		t.Fatalf("dispatch continued after rewrite: %d runs, %q", got, ledger)
	}
	if !s.blockers.syncBlocked {
		t.Fatal("supervisor should be sync-blocked after a rewrite")
	}
}

func TestBlockerReachesOriginWhileSyncIsBlocked(t *testing.T) {
	dir := fixture(t, "ok")
	bare := withOrigin(t, dir)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)
	s.tickWait(ctx, t0.Add(61*time.Second))

	discarded := gitOut(t, bare, "rev-parse", "refs/heads/knowledge")
	c := filepath.Join(t.TempDir(), "clone")
	runCmd(t, "", "git", "clone", "-q", "-b", "knowledge", bare, c)
	runCmd(t, c, "git", "-c", "user.name=x", "-c", "user.email=x@x", "commit", "--amend", "-q", "--no-edit", "-m", "rewritten")
	runCmd(t, c, "git", "push", "-q", "--force", "origin", "knowledge")
	rewritten := gitOut(t, bare, "rev-parse", "refs/heads/knowledge")

	s.tickWait(ctx, t0.Add(2*time.Minute))
	if !s.blockers.syncBlocked {
		t.Fatal("precondition: the supervisor should be sync-blocked after a rewrite")
	}
	stranded := gitOut(t, bare, "cat-file", "-p", "refs/openroutines/blocked:tasks.md")
	if !strings.Contains(stranded, "history rewritten") {
		t.Fatalf("the blocker task should have left the container: %q", stranded)
	}
	if got := gitOut(t, bare, "rev-parse", "refs/heads/knowledge"); got != rewritten {
		t.Fatalf("a blocked supervisor must not write the knowledge branch: %s -> %s", rewritten, got)
	}
	if n := gitOut(t, bare, "rev-list", "--count", "refs/openroutines/blocked"); n != "1" {
		t.Fatalf("the stranded ref should be a parentless snapshot, got %s commits", n)
	}
	if _, err := gitTry(bare, "merge-base", "--is-ancestor", discarded, "refs/openroutines/blocked"); err == nil {
		t.Fatalf("stranding must not republish the history the rewrite discarded (%.8s)", discarded)
	}

	runCmd(t, bare, "git", "update-ref", "refs/openroutines/accepted", rewritten)
	s.tickWait(ctx, t0.Add(3*time.Minute))
	if s.blockers.syncBlocked {
		t.Fatal("sync should have recovered once the new history was accepted")
	}
	onBranch := gitOut(t, bare, "cat-file", "-p", "refs/heads/knowledge:tasks.md")
	if !strings.Contains(onBranch, "history rewritten") {
		t.Fatalf("the blocker should be on the knowledge branch after recovery: %q", onBranch)
	}
	if out, err := gitTry(bare, "rev-parse", "--verify", "--quiet", "refs/openroutines/blocked"); err == nil {
		t.Fatalf("the stranded ref should be cleared once the branch carries it: %s", out)
	}
}

func TestStrandedRefFromAnotherContainerSurvives(t *testing.T) {
	dir := fixture(t, "ok")
	bare := withOrigin(t, dir)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)
	earlier := gitOut(t, bare, "rev-parse", "refs/heads/knowledge")
	runCmd(t, bare, "git", "update-ref", "refs/openroutines/blocked", earlier)

	s.tickWait(ctx, t0.Add(61*time.Second))

	if got := gitOut(t, bare, "rev-parse", "refs/openroutines/blocked"); got != earlier {
		t.Fatalf("a successor's push must leave someone else's stranded ref alone: %.8s -> %s", earlier, got)
	}
}

func TestUnreachableOriginRecordsADurableBlocker(t *testing.T) {
	dir := fixture(t, "ok")
	bare := withOrigin(t, dir)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)

	gone := bare + ".gone"
	if err := os.Rename(bare, gone); err != nil {
		t.Fatal(err)
	}
	s.tickWait(ctx, t0.Add(61*time.Second))
	s.tickWait(ctx, t0.Add(122*time.Second))
	if err := os.Rename(gone, bare); err != nil {
		t.Fatal(err)
	}

	s.tickWait(ctx, t0.Add(183*time.Second))

	tasks := gitOut(t, bare, "cat-file", "-p", "refs/heads/knowledge:tasks.md")
	if !strings.Contains(tasks, "origin unreachable") {
		t.Fatalf("the outage should be recorded where a person looks: %q", tasks)
	}
	if got := strings.Count(tasks, "origin unreachable"); got != 1 {
		t.Fatalf("the outage is recorded once, not once per tick (%d): %q", got, tasks)
	}
	if !strings.Contains(tasks, "[x]") {
		t.Fatalf("the blocker should be resolved in place once origin returned: %q", tasks)
	}
}

func TestLeaseExcludesASecondInstanceWhileRunsExecute(t *testing.T) {
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

	writeRoutines := func(dir string) {
		for _, name := range []string{"every-minute", "second", "third"} {
			os.WriteFile(filepath.Join(dir, "routines", name+".md"), []byte(
				"---\nschedule: \"* * * * *\"\n---\nDo the fake thing.\n"), 0o644)
		}
	}
	writeRoutines(dir)
	run(dir, "git", "remote", "add", "origin", bare)

	optInConcurrency(t, dir, 2)
	holder := newSupervisor(t, dir)
	holder.lease.ttl = 6 * time.Second
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)
	holder.tickWait(ctx, t0)

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
		holder.tickWait(ctx, t0.Add(61*time.Second))
	}()
	waitForRuns(2)

	other := t.TempDir()
	os.MkdirAll(filepath.Join(other, "routines"), 0o755)
	os.WriteFile(filepath.Join(other, "openroutines.yml"), []byte(agentYAML("UTC")), 0o644)
	writeRoutines(other)
	run(other, "git", "init", "-q", "-b", "main", ".")
	run(other, "git", "remote", "add", "origin", bare)
	second := newSupervisor(t, other)
	second.lease.instanceID = "second-instance"
	second.lease.ttl = holder.lease.ttl

	acquireCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if err := second.acquireLease(acquireCtx); err == nil {
		t.Fatal("second instance took a live lease while the first had runs in flight")
	}
	second.tickWait(ctx, t0.Add(61*time.Second))
	if got := strings.Count(readFile(t, started), "run_"); got != 2 {
		t.Fatalf("second instance dispatched behind the lease holder: %d runs launched, want 2", got)
	}

	<-done
	if got := strings.Count(readFile(t, started), "run_"); got != 2 {
		t.Fatalf("a full pool must skip to the next tick, not queue: %d runs launched, want 2", got)
	}
	holder.tickWait(ctx, t0.Add(61*time.Second))
	if got := strings.Count(readFile(t, filepath.Join(dir, "knowledge", "ledgers", "fake.md")), "ran run_"); got != 3 {
		t.Fatalf("lease holder should have run all 3 routines across two ticks, got %d", got)
	}
}

func TestRunsExecuteInParallel(t *testing.T) {
	dir := fixture(t, "slow")
	if err := os.WriteFile(filepath.Join(dir, "routines", "second.md"), []byte(
		"---\nschedule: \"* * * * *\"\n---\nDo the other fake thing.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	optInConcurrency(t, dir, 2)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)
	s.tickWait(ctx, t0)

	start := time.Now()
	s.tickWait(ctx, t0.Add(61*time.Second))
	if elapsed := time.Since(start); elapsed >= 5500*time.Millisecond {
		t.Fatalf("two 3s runs took %s -- they did not overlap", elapsed)
	}
	if got := strings.Count(readFile(t, filepath.Join(dir, "knowledge", "ledgers", "fake.md")), "ran run_"); got != 2 {
		t.Fatalf("concurrent settlements into one ledger file should compose, got %d entries", got)
	}
}

func TestLeaseStaysLiveThroughALongRun(t *testing.T) {
	dir := fixture(t, "slow")
	base := t.TempDir()
	bare := filepath.Join(base, "origin.git")
	runCmd(t, base, "git", "init", "-q", "-b", "main", "--bare", bare)
	runCmd(t, dir, "git", "remote", "add", "origin", bare)

	holder := newSupervisor(t, dir)
	holder.lease.ttl = 1500 * time.Millisecond
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)
	holder.tickWait(ctx, t0)

	binDir := strings.SplitN(os.Getenv("PATH"), string(os.PathListSeparator), 2)[0]
	started := filepath.Join(binDir, "started")
	done := make(chan struct{})
	go func() {
		defer close(done)
		holder.tickWait(ctx, t0.Add(61*time.Second))
	}()
	for deadline := time.Now().Add(30 * time.Second); !strings.Contains(readFile(t, started), "run_"); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the run never started")
		}
	}
	time.Sleep(2200 * time.Millisecond)

	other := t.TempDir()
	os.MkdirAll(filepath.Join(other, "routines"), 0o755)
	os.WriteFile(filepath.Join(other, "openroutines.yml"), []byte(agentYAML("UTC")), 0o644)
	runCmd(t, other, "git", "init", "-q", "-b", "main", ".")
	runCmd(t, other, "git", "remote", "add", "origin", bare)
	second := newSupervisor(t, other)
	second.lease.instanceID = "second-instance"
	second.lease.ttl = holder.lease.ttl

	acquireCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if err := second.acquireLease(acquireCtx); err == nil {
		t.Fatal("second instance took the lease while a long run was executing")
	}
	<-done
	if got := strings.Count(readFile(t, filepath.Join(dir, "knowledge", "ledgers", "fake.md")), "ran run_"); got != 1 {
		t.Fatalf("the long run should have completed under the heartbeat, got %d ledger entries", got)
	}
}

func TestUnsettledRunIsNotReportedAsCompleted(t *testing.T) {
	dir := fixture(t, "ok")
	s := newSupervisor(t, dir)
	logs := logtest.Capture(t)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)
	if err := os.Mkdir(filepath.Join(dir, "knowledge", "runs.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.tickWait(ctx, t0.Add(61*time.Second))

	logs.Expect("settlement failed -- run remains pending and will retry")
	logs.Reject("run completed")
	if st := loadState(t, s); st.Pending == nil {
		t.Fatal("the unsettled run must remain pending for retry")
	}
}

func TestLeaseLostAfterStagingHandsTheAttemptBack(t *testing.T) {
	dir := fixture(t, "ok")
	withOrigin(t, dir)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)
	s.tickWait(ctx, t0)

	<-s.pool.slots
	s.tickWait(ctx, t0.Add(61*time.Second))
	st := loadState(t, s)
	if st.Pending == nil {
		t.Fatal("expected a pending run parked behind the full pool")
	}

	stopUsurper := usurpLease(t, s)
	defer stopUsurper()

	r, err := routine.Find(s.Dir, "every-minute")
	if err != nil {
		t.Fatal(err)
	}
	if cleanupErr := s.execute(ctx, r, st, t0.Add(61*time.Second)); cleanupErr != nil {
		t.Fatal(cleanupErr)
	}

	after := loadState(t, s)
	if after.Pending == nil || after.Pending.Attempts != 0 {
		t.Fatalf("an attempt that never started must be handed back for the lease holder to retry: %+v", after.Pending)
	}
}

func TestLostLeaseCancelsTheRun(t *testing.T) {
	dir := fixture(t, "blocked")
	base := t.TempDir()
	bare := filepath.Join(base, "origin.git")
	runCmd(t, base, "git", "init", "-q", "-b", "main", "--bare", bare)
	runCmd(t, dir, "git", "remote", "add", "origin", bare)

	holder := newSupervisor(t, dir)
	holder.lease.ttl = 1500 * time.Millisecond
	logs := logtest.Capture(t)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)
	holder.tickWait(ctx, t0)

	binDir := strings.SplitN(os.Getenv("PATH"), string(os.PathListSeparator), 2)[0]
	started := filepath.Join(binDir, "started")
	done := make(chan struct{})
	go func() {
		defer close(done)
		holder.tickWait(ctx, t0.Add(61*time.Second))
	}()
	for deadline := time.Now().Add(30 * time.Second); !strings.Contains(readFile(t, started), "run_"); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the run never started")
		}
	}

	usurped := time.Now()
	stopUsurper := usurpLease(t, holder)
	defer stopUsurper()
	<-done

	if took := time.Since(usurped); took > 15*time.Second {
		t.Fatalf("the run was left to reach its own timeout (%s), not canceled", took.Round(time.Second))
	}

	st, err := schedule.Load(holder.stateDir(), "every-minute")
	if err != nil {
		t.Fatal(err)
	}
	if st.Pending == nil || st.Pending.Attempts != 0 {
		t.Fatalf("a canceled attempt should be handed back for the lease holder to retry: %+v", st.Pending)
	}
	if got := readFile(t, filepath.Join(dir, "knowledge", "ledgers", "fake.md")); strings.Contains(got, "ran run_") {
		t.Fatalf("a canceled run's staged knowledge must not be imported: %q", got)
	}
	canceled := false
	for _, line := range strings.Split(logs.String(), "\n") {
		if !strings.Contains(line, "lease lost mid-run") {
			continue
		}
		canceled = true
		if !strings.Contains(line, "routine=every-minute") || !strings.Contains(line, "run_id=run_") {
			t.Fatalf("cancellation must be attributed to its attempt: %s", line)
		}
	}
	if !canceled {
		t.Fatalf("expected a lease-lost cancellation line, got: %s", logs.String())
	}
}

func TestAttemptIsDurableBeforeTheModelStarts(t *testing.T) {
	dir := fixture(t, "probe")
	withOrigin(t, dir)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)
	s.tickWait(ctx, t0.Add(61*time.Second))

	seen := replacementState(t, "every-minute")
	if seen == nil || seen.Pending == nil {
		t.Fatalf("a replacement mid-attempt should see the pending run, got %+v", seen)
	}
	if seen.Pending.Attempts != 1 {
		t.Fatalf("a replacement mid-attempt should see attempts=1, got %d", seen.Pending.Attempts)
	}
}

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

	s.tickWait(ctx, t0)
	s.tickWait(ctx, t0.Add(61*time.Second))

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

func TestSpentAttemptsAbandonWithoutSettlement(t *testing.T) {
	dir := fixture(t, "ok")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)
	scheduled := t0.Add(time.Minute)
	st := loadState(t, s)
	st.Pending = &schedule.Pending{
		RunID: "run_crashloop", ScheduledFor: scheduled, CoveredThrough: scheduled,
		CreatedAt: scheduled, Attempts: MaxAttempts, LastAttemptAt: scheduled,
	}
	if err := st.Save(s.stateDir()); err != nil {
		t.Fatal(err)
	}

	s.tickWait(ctx, t0.Add(time.Hour))

	if st = loadState(t, s); st.Pending != nil {
		t.Fatalf("a run with no attempts left must be abandoned, still pending: %+v", st.Pending)
	}
	if !st.Watermark.Equal(scheduled) {
		t.Fatalf("abandonment should advance the watermark to %v, got %v", scheduled, st.Watermark)
	}
	if got := runCount(t, dir); got != 0 {
		t.Fatalf("no attempt should have been dispatched, got %d runs", got)
	}
	tasks := readFile(t, filepath.Join(dir, "knowledge", "tasks.md"))
	if !strings.Contains(tasks, "task-run_crashloop") {
		t.Fatalf("abandonment should hand run_crashloop to a human: %q", tasks)
	}
}

func TestUncommittedIntentIsPushedBeforeDispatch(t *testing.T) {
	dir := fixture(t, "probe")
	withOrigin(t, dir)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)

	st := loadState(t, s)
	st.Pending = &schedule.Pending{RunID: "run_orphaned", ScheduledFor: t0, CoveredThrough: t0, CreatedAt: t0}
	if err := st.Save(s.stateDir()); err != nil {
		t.Fatal(err)
	}
	if n := knowledge.NewStore(dir).Status().Uncommitted; n == 0 {
		t.Fatal("precondition: the pending record should be uncommitted")
	}

	s.tickWait(ctx, t0.Add(time.Minute))

	seen := replacementState(t, "every-minute")
	if seen == nil || seen.Pending == nil || seen.Pending.RunID != "run_orphaned" {
		t.Fatalf("the run's identity should have reached origin before it acted, replacement sees %+v", seen)
	}
}

func TestFailedIntentCommitHoldsRunsAndRecordsATask(t *testing.T) {
	dir := fixture(t, "ok")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)

	cmd := exec.Command("git", "rev-parse", "--absolute-git-dir")
	cmd.Dir = filepath.Join(dir, "knowledge")
	gitDir, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(strings.TrimSpace(string(gitDir)), "index.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	s.tickWait(ctx, t0.Add(61*time.Second))

	if got := runCount(t, dir); got != 0 {
		t.Fatalf("an intent that cannot be committed must not dispatch, got %d runs", got)
	}
	tasks := readFile(t, filepath.Join(dir, "knowledge", "tasks.md"))
	if !strings.Contains(tasks, "intent commit failed") {
		t.Fatalf("a halted supervisor should hand the blocker to a human: %q", tasks)
	}
}

func TestBlockedLogsOnceAcrossTicks(t *testing.T) {
	dir := fixture(t, "ok")
	bare := withOrigin(t, dir)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)

	gone := bare + ".gone"
	if err := os.Rename(bare, gone); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(gone, bare)

	logs := logtest.Capture(t)
	for i := 1; i <= 3; i++ {
		s.tickWait(ctx, t0.Add(time.Duration(i)*time.Minute))
	}

	if got := strings.Count(logs.String(), "kind=origin"); got != 1 {
		t.Fatalf("BLOCKED should log once across a persisting blocker, got %d: %q", got, logs.String())
	}
}

func TestOrphanHoldingTheOutputPipeDoesNotParkTheTick(t *testing.T) {
	dir := fixture(t, "orphan")
	os.WriteFile(filepath.Join(dir, "routines", "every-minute.md"), []byte(
		"---\nschedule: \"* * * * *\"\ntimeout: 2s\n---\nDo the fake thing.\n"), 0o644)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)
	s.tickWait(ctx, t0)

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		s.tickWait(ctx, t0.Add(61*time.Second))
	}()
	select {
	case <-returned:
	case <-time.After(30 * time.Second):
		t.Fatal("the tick never returned: the kill path is still waiting on the abandoned pipe")
	}

	records := readFile(t, filepath.Join(dir, "knowledge", "runs.jsonl"))
	if !strings.Contains(records, `"outcome":"timeout"`) {
		t.Fatalf("expected the killed attempt to be recorded as a timeout: %q", records)
	}
}

func TestRunRecordCarriesUsage(t *testing.T) {
	dir := fixture(t, "ok")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)
	s.tickWait(ctx, t0)
	s.tickWait(ctx, t0.Add(time.Minute))
	records := readFile(t, filepath.Join(dir, "knowledge", "runs.jsonl"))
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

func TestBootAcceptsEnvDeliveredKeysWithoutLogging(t *testing.T) {
	logs := logtest.Capture(t)
	dir := t.TempDir()

	t.Setenv(creds.EnvMasterKey, creds.GenerateKey())
	t.Setenv(repository.EnvDeployKey, "PRIVATE KEY") // gitleaks:allow -- a placeholder, not a key
	t.Setenv(creds.EnvMasterKeyFile, "/usr/local/etc/master.key")
	t.Setenv(repository.EnvDeployKeyFile, "/etc/ld.so.conf.d/deploy")
	t.Setenv("OPENROUTINES_IN_CONTAINER", "1")
	if err := ValidateKeyFileLocations(dir); err != nil {
		t.Fatal(err)
	}
	if got := logs.String(); got != "" {
		t.Errorf("environment-delivered keys should not produce logs: %q", got)
	}
}

func TestDeployedBootRequiresDecryptableCredentials(t *testing.T) {
	dir := fixture(t, "ok")
	key := []byte(strings.Repeat("a", 32))
	if err := creds.Write(dir, key, map[string]string{"fake_api_key": "secret"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENROUTINES_IN_CONTAINER", "1")
	t.Setenv(creds.EnvMasterKey, strings.Repeat("b", 64))
	t.Setenv(creds.EnvMasterKeyFile, "")

	_, err := New(dir)
	if err == nil || !strings.Contains(err.Error(), "cannot decrypt credentials") {
		t.Fatalf("deployed credential validation error = %v", err)
	}
}

func TestDeployedBootRequiresAMasterKey(t *testing.T) {
	dir := fixture(t, "ok")
	if err := creds.Write(dir, []byte(strings.Repeat("a", 32)), map[string]string{}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENROUTINES_IN_CONTAINER", "1")
	t.Setenv(creds.EnvMasterKey, "")
	t.Setenv(creds.EnvMasterKeyFile, "")

	_, err := New(dir)
	if err == nil || !strings.Contains(err.Error(), creds.FileName+" exists") || !strings.Contains(err.Error(), "restore "+creds.KeyFileName) {
		t.Fatalf("deployed credential validation error = %v", err)
	}
}

func TestDeployedBootRequiresCredentialStore(t *testing.T) {
	dir := fixture(t, "ok")
	t.Setenv("OPENROUTINES_IN_CONTAINER", "1")
	t.Setenv(creds.EnvMasterKey, "")
	t.Setenv(creds.EnvMasterKeyFile, "")

	_, err := New(dir)
	if err == nil || !strings.Contains(err.Error(), creds.FileName+" is missing") {
		t.Fatalf("deployed credential validation error = %v", err)
	}
}

func TestLocalBootWarnsButDoesNotRequireCredentials(t *testing.T) {
	dir := fixture(t, "ok")
	t.Setenv("OPENROUTINES_IN_CONTAINER", "")
	t.Setenv(creds.EnvMasterKey, "")
	t.Setenv(creds.EnvMasterKeyFile, "")
	logs := logtest.Capture(t)

	if _, err := New(dir); err != nil {
		t.Fatal(err)
	}
	logs.Expect("WARN", "routines may lack provider authentication")
}

func TestBootRunsUnconfinedWhenTheOperatorDisablesTheSandbox(t *testing.T) {
	logs := logtest.Capture(t)
	t.Setenv("OPENROUTINES_IN_CONTAINER", "1")
	t.Setenv(sandbox.EnvDisable, "1")

	if err := verifyIsolation(); err != nil {
		t.Fatalf("the hatch should let a supervisor start where nothing can confine a run: %v", err)
	}
	logs.Expect("WARN", sandbox.EnvDisable)

	t.Setenv(creds.EnvMasterKeyFile, "/usr/local/etc/master.key")
	if err := ValidateKeyFileLocations(t.TempDir()); err != nil {
		t.Errorf("with no sandbox there is no grant list to sit outside of: %v", err)
	}
}

func TestBootRefusesAKeyFileRoutinesCanRead(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENROUTINES_IN_CONTAINER", "1")
	t.Setenv(creds.EnvMasterKey, "")
	t.Setenv(repository.EnvDeployKey, "")
	t.Setenv(creds.EnvMasterKeyFile, "/run/secrets/master.key")
	t.Setenv(repository.EnvDeployKeyFile, "")
	if err := ValidateKeyFileLocations(dir); err != nil {
		t.Errorf("a key outside every granted path is the supported deployment: %v", err)
	}

	t.Setenv(creds.EnvMasterKeyFile, "/usr/local/etc/master.key")
	err := ValidateKeyFileLocations(dir)
	if err == nil {
		t.Fatal("a master key inside the granted read-only OS was accepted")
	}
	want := "the master key file at /usr/local/etc/master.key is in a directory routines can read; move it to /run/secrets or another directory routines cannot access, set OPENROUTINES_MASTER_KEY_FILE to its new path, and see https://openroutines.dev/docs/deploying/"
	if err.Error() != want {
		t.Errorf("unexpected refusal:\n got: %s\nwant: %s", err, want)
	}

	t.Setenv(creds.EnvMasterKeyFile, "")
	t.Setenv(repository.EnvDeployKeyFile, "/etc/ld.so.conf.d/deploy")
	err = ValidateKeyFileLocations(dir)
	if err == nil {
		t.Fatal("a deploy key inside a granted /etc entry was accepted")
	}
	want = "the deploy key file at /etc/ld.so.conf.d/deploy is in a directory routines can read; move it to /run/secrets or another directory routines cannot access, set OPENROUTINES_DEPLOY_KEY_FILE to its new path, and see https://openroutines.dev/docs/deploying/"
	if err.Error() != want {
		t.Errorf("unexpected refusal:\n got: %s\nwant: %s", err, want)
	}

	t.Setenv(repository.EnvDeployKeyFile, "/etc/secrets/deploy")
	if err := ValidateKeyFileLocations(dir); err != nil {
		t.Errorf("a platform secrets directory under /etc is a supported location: %v", err)
	}

	t.Setenv("OPENROUTINES_IN_CONTAINER", "")
	if err := ValidateKeyFileLocations(dir); err != nil {
		t.Errorf("outside production there is no sandbox grant list to violate: %v", err)
	}
}

func TestBootRefusesAnExposedConventionalKeyFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENROUTINES_IN_CONTAINER", "1")
	t.Setenv(creds.EnvMasterKey, "")
	t.Setenv(creds.EnvMasterKeyFile, "")
	t.Setenv(repository.EnvDeployKey, "")
	t.Setenv(repository.EnvDeployKeyFile, "")
	path := filepath.Join(dir, creds.KeyFileName)
	if err := os.Symlink("/usr/bin/env", path); err != nil {
		t.Fatal(err)
	}

	err := ValidateKeyFileLocations(dir)
	if err == nil {
		t.Fatal("a conventional key file resolving inside the runtime OS was accepted")
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), creds.EnvMasterKeyFile) {
		t.Errorf("the refusal should name the file to move and the override to set: %v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(dir, repository.DeployKeyFileName)
	if err := os.Symlink("/usr/bin/env", path); err != nil {
		t.Fatal(err)
	}
	err = ValidateKeyFileLocations(dir)
	if err == nil {
		t.Fatal("a conventional deploy key resolving inside the runtime OS was accepted")
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), repository.EnvDeployKeyFile) {
		t.Errorf("the refusal should name the file to move and the override to set: %v", err)
	}
}
