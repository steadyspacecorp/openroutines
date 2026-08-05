package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/logging"
	"github.com/steadyspacecorp/openroutines/internal/memory"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/runner"
	"github.com/steadyspacecorp/openroutines/internal/schedule"
)

func TestAttemptIdentityIsNotReusedWhenCleanupFails(t *testing.T) {
	t.Setenv("OPENROUTINES_IN_CONTAINER", "1")
	s := &Supervisor{
		slots: make(chan uint32, 1),
		fatal: make(chan error, 1),
		reap: func(uid uint32) error {
			return fmt.Errorf("pid escaped uid %d", uid)
		},
	}
	if s.releaseIdentity(20000, nil) {
		t.Fatal("cleanup failure returned the identity to the pool")
	}
	if len(s.slots) != 0 {
		t.Fatal("poisoned identity is available for reuse")
	}
	select {
	case err := <-s.fatal:
		if !strings.Contains(err.Error(), "refusing to reuse identity") {
			t.Fatalf("fatal error = %v", err)
		}
	default:
		t.Fatal("cleanup failure did not stop supervision")
	}
}

func TestAttemptIdentityIsNotReusedWhenWorkspaceCleanupFails(t *testing.T) {
	t.Setenv("OPENROUTINES_IN_CONTAINER", "1")
	s := &Supervisor{
		slots: make(chan uint32, 1),
		fatal: make(chan error, 1),
		reap:  func(uint32) error { return nil },
	}
	if s.releaseIdentity(20000, errors.New("attempt workspace still exists")) {
		t.Fatal("workspace cleanup failure returned the identity to the pool")
	}
	if len(s.slots) != 0 {
		t.Fatal("poisoned identity is available for reuse")
	}
	select {
	case err := <-s.fatal:
		if !strings.Contains(err.Error(), "attempt workspace still exists") {
			t.Fatalf("fatal error = %v", err)
		}
	default:
		t.Fatal("workspace cleanup failure did not stop supervision")
	}
}

func TestLandlockProfileReturnsSerialSlotWithoutUIDReaping(t *testing.T) {
	t.Setenv("OPENROUTINES_IN_CONTAINER", "1")
	reaped := false
	s := &Supervisor{
		isolationProfile: config.IsolationLandlock,
		slots:            make(chan uint32, 1),
		fatal:            make(chan error, 1),
		reap:             func(uint32) error { reaped = true; return errors.New("must not run") },
	}
	if !s.releaseIdentity(0, nil) {
		t.Fatal("landlock serial slot was not returned")
	}
	if reaped {
		t.Fatal("landlock profile attempted cross-uid reaping")
	}
	if uid := <-s.slots; uid != 0 {
		t.Fatalf("landlock slot identity = %d, want shared identity marker 0", uid)
	}
}

func TestLandlockProfileUsesSharedIdentityMarker(t *testing.T) {
	dir := fixture(t, "success")
	path := filepath.Join(dir, "openroutines.yml")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("concurrency: 1\nisolation_profile: landlock\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	s := newSupervisor(t, dir)
	if s.isolationProfile != config.IsolationLandlock {
		t.Fatalf("profile = %q", s.isolationProfile)
	}
	if uid := <-s.slots; uid != 0 {
		t.Fatalf("slot identity = %d, want shared identity marker 0", uid)
	}
}

func TestVerifyAttemptGroupsChecksEveryRunSlot(t *testing.T) {
	if err := verifyAttemptGroups([]int{20000}, 2); err == nil || !strings.Contains(err.Error(), "20001") {
		t.Fatalf("group check error = %v, want missing second slot group", err)
	}
	if err := verifyAttemptGroups([]int{20000, 20001}, 2); err != nil {
		t.Fatalf("complete group set rejected: %v", err)
	}
}

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
msg=.home/.local/share/opencode/storage/message/ses_fake/msg_1.json
# The capture and session-export surface, answered before any run side
# effect: after the attempt the runner asks for the session list and the
# export of each session, from the same working directory the run used.
case "$1" in
  session) printf '[{"id":"ses_fake"}]'; exit 0 ;;
  export) printf '{"messages":[{"info":%s}]}' "$(cat "$msg")"; exit 0 ;;
esac
# Every run leaves the message record a real opencode persists -- the
# surface export renders, where the runner reads token usage and how the
# session ended.
mkdir -p .home/.local/share/opencode/storage/message/ses_fake
printf '{"role":"assistant","modelID":"fake","finish":"stop","tokens":{"input":100,"output":20,"reasoning":5,"cache":{"read":0,"write":0}},"cost":0.01}' \
  > "$msg"
case "$mode" in
  fail) echo "boom" >&2; exit 1 ;;
  stalled) # The agent loop died on a rejected tool call: the session never
     # finished its turn, no memory was written, and opencode still exits 0.
     printf '{"role":"assistant","modelID":"fake","finish":"tool-calls","tokens":{"input":100,"output":20,"reasoning":5,"cache":{"read":0,"write":0}},"cost":0.01}' \
       > "$msg"
     echo "stalled" ;;
  slow) echo "$OPENROUTINES_RUN_ID" >> "$(dirname "$0")/started"
     sleep 3
     mkdir -p memory/ledgers
     echo "ran $OPENROUTINES_RUN_ID $OPENROUTINES_ATTEMPT_ID" >> memory/ledgers/fake.md
     echo "slept" ;;
  blocked) # A run that only ends when something kills it. Staged memory is
     # written up front, so a test asserting that a killed run's memory was
     # not imported is testing an import that had something to import. The
     # sleep outlives the routine's timeout: a kill that never arrives ends
     # the attempt as a timeout, which settles differently from a cancel,
     # rather than as a run that completed on its own.
     mkdir -p memory/ledgers
     echo "ran $OPENROUTINES_RUN_ID $OPENROUTINES_ATTEMPT_ID" >> memory/ledgers/fake.md
     echo "$OPENROUTINES_RUN_ID" >> "$d/started"
     sleep 60 ;;
  detach) sleep 60 </dev/null >/dev/null 2>&1 &
     echo $! > "$d/detached.pid"
     echo "detached" ;;
  consume) cp inbox.md memory/inbox-copy.md
     : > CONSUMED
     echo "consumed" ;;
  probe) [ -d "$d/replacement" ] || git clone -q -b memory "$(cat "$d/origin")" "$d/replacement" || true
     echo "probed" ;;
  orphan) # A detached grandchild in its own process group, holding the run's
     # stdout: the group kill cannot reach it and it outlives the attempt.
     if command -v setsid >/dev/null 2>&1; then setsid sleep 120 &
     else (set -m; sleep 120 &) fi
     sleep 120 ;;
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

// usurpLease takes the lease at origin under another instance's name and then
// keeps heartbeating it, CAS-looping against the holder's own renewals. The
// heartbeat is the point: a lease written once expires a TTL later, and an
// expired foreign lease is one the holder may correctly reclaim -- which put
// every assertion downstream of it on a TTL-length clock. Holding the lease
// for as long as the test needs it removes that deadline. The returned stop
// joins the heartbeat goroutine.
func usurpLease(t *testing.T, s *Supervisor) (stop func()) {
	t.Helper()
	take := func() error {
		lease, err := s.mem.ReadLease()
		if err != nil {
			return err
		}
		if lease == nil {
			return fmt.Errorf("no lease to usurp")
		}
		_, err = s.mem.WriteLease("usurper", time.Now(), lease.SHA)
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
			case <-time.After(s.leaseTTL / 4):
				_ = take() // a renewal the holder wins is re-taken next tick
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

// optInConcurrency writes concurrency into a fixture's config, the way an
// operator would: parallelism is opt-in, and these tests are the opt-in path.
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

// tickWait runs one scheduling pass and waits for every attempt it launched
// to settle -- the synchronous shape the scheduling tests want, and exactly
// what a serial Tick used to do.
func (s *Supervisor) tickWait(ctx context.Context, now time.Time) {
	s.Tick(ctx, now)
	s.runs.Wait()
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
// it is, so a probe run can clone it mid-attempt. Returns the bare repo path.
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

// gitTry runs git in dir and returns its combined output, error and all.
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
	sessionDir := t.TempDir()
	t.Setenv(runner.EnvSessionDir, sessionDir)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	// First sight: registers, does not run.
	s.tickWait(ctx, t0)
	st := loadState(t, s)
	if st == nil || st.Pending != nil {
		t.Fatalf("expected registered state with no pending, got %+v", st)
	}
	if ledger := readFile(t, filepath.Join(dir, "memory", "ledgers", "fake.md")); ledger != "" {
		t.Fatalf("nothing should have run yet, ledger: %q", ledger)
	}

	// One minute later: one occurrence due -> runs, imports, advances.
	s.tickWait(ctx, t0.Add(61*time.Second))
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

	// The attempt's session was exported into its per-attempt directory
	// under the designated session dir.
	stored, err := filepath.Glob(filepath.Join(sessionDir, "run_*.attempt_01", "ses_fake.json"))
	if err != nil || len(stored) != 1 {
		t.Fatalf("expected one exported session, got %v (%v)", stored, err)
	}
	if got := readFile(t, stored[0]); !strings.Contains(got, `"finish":"stop"`) {
		t.Fatalf("stored session data does not match what the run wrote: %q", got)
	}
}

// A session that died mid-turn -- the agent loop stopped on a rejected tool
// call -- exits 0 with nothing written. Recorded as completed, that advances
// the watermark and clears pending, which is the "silently skipped" outcome
// the scheduler exists to prevent, with a green run record on top of it.
func TestSessionThatEndedMidTurnIsNotCompleted(t *testing.T) {
	dir := fixture(t, "stalled")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0) // register
	s.tickWait(ctx, t0.Add(61*time.Second))

	records := readFile(t, filepath.Join(dir, "memory", "runs.jsonl"))
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
	if events := readFile(t, filepath.Join(dir, "memory", "events.md")); !strings.Contains(events, "crashed") {
		t.Errorf("the failure should be recorded as an event: %q", events)
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

	s.tickWait(ctx, t0) // register
	s.tickWait(ctx, t0.Add(61*time.Second))

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
	s.tickWait(ctx, t0.Add(122*time.Second))
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
		s.tickWait(ctx, t0.Add(time.Duration(i)*7*time.Minute))
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

	s.tickWait(ctx, time.Date(2026, 10, 31, 12, 0, 0, 0, ny)) // register, EDT
	s.tickWait(ctx, time.Date(2026, 11, 2, 12, 0, 0, 0, ny))  // after fall-back, EST

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

// A model process that exits cleanly can leave a descendant behind, and that
// descendant goes on writing to staging while the supervisor validates and
// imports it. Every attempt therefore ends with its process group, not only
// the ones that time out or are canceled.
func TestDetachedDescendantDoesNotSurviveACleanRun(t *testing.T) {
	dir := fixture(t, "detach")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0) // register
	s.tickWait(ctx, t0.Add(61*time.Second))

	var pid int
	if _, err := fmt.Sscan(readFile(t, filepath.Join(fakeBinDir(), "detached.pid")), &pid); err != nil || pid == 0 {
		t.Fatalf("the fake run did not report a detached child: %v", err)
	}
	// The signal is sent before the run settles; the wait is for the kernel
	// to reap what it killed, which a zombie pid still answers.
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

	s.tickWait(ctx, t0) // register
	// Ten minutes of downtime: ten missed firings must collapse into ONE run.
	s.tickWait(ctx, t0.Add(10*time.Minute))
	ledger := readFile(t, filepath.Join(dir, "memory", "ledgers", "fake.md"))
	if got := strings.Count(ledger, "ran run_"); got != 1 {
		t.Fatalf("expected exactly 1 collapsed catch-up run, got %d: %q", got, ledger)
	}
	st := loadState(t, s)
	if st.Pending != nil {
		t.Fatalf("pending should be clear: %+v", st.Pending)
	}
}

// captureStdout collects what fn prints -- the opencode-log passthrough
// writes to the process's stdout directly, not through the logger.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	read := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		read <- string(b)
	}()
	fn()
	w.Close()
	out := <-read
	r.Close()
	return out
}

// A failed attempt must never fail invisibly: opencode's stderr is its
// diagnostic log, and every line of it passes through to the process log
// stream decorated with the attempt's identity -- with or without session
// storage.
func TestFailedAttemptDiagnosticsPassThrough(t *testing.T) {
	failOnce := func(t *testing.T) string {
		t.Helper()
		s := newSupervisor(t, fixture(t, "fail"))
		ctx := context.Background()
		t0 := time.Now().Truncate(time.Minute)
		s.tickWait(ctx, t0) // register
		return captureStdout(t, func() { s.tickWait(ctx, t0.Add(time.Minute)) })
	}

	t.Run("no session storage designated", func(t *testing.T) {
		t.Setenv(runner.EnvSessionDir, "")
		if out := failOnce(t); !strings.Contains(out, "boom routine=every-minute run_id=run_") {
			t.Fatalf("the failing run's diagnostics must land in the log decorated, got %q", out)
		}
	})

	t.Run("session storage designated", func(t *testing.T) {
		sessions := t.TempDir()
		t.Setenv(runner.EnvSessionDir, sessions)
		out := failOnce(t)
		if !strings.Contains(out, "boom routine=every-minute run_id=run_") {
			t.Fatalf("session storage must not swallow the log passthrough, got %q", out)
		}
		stored, err := filepath.Glob(filepath.Join(sessions, "run_*.attempt_01", "ses_fake.json"))
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

	s.tickWait(ctx, t0) // register
	now := t0.Add(time.Minute)
	s.tickWait(ctx, now) // attempt 1 fails
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
		s.tickWait(ctx, now)
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

// The attempt that abandons a run is the one an operator reads first, so
// its record names the sessions it left -- the outcome that most needs the
// directory cannot be the one outcome that never mentions it.
func TestAbandonedRunNamesItsSessions(t *testing.T) {
	t.Setenv(runner.EnvSessionDir, t.TempDir())
	s := newSupervisor(t, fixture(t, "fail"))
	var out bytes.Buffer
	logging.Setup(&out, slog.LevelInfo, time.UTC)

	t0 := time.Now().Truncate(time.Minute)
	s.tickWait(context.Background(), t0) // register
	driveToAbandonment(t, s, t0)

	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.Contains(line, "run abandoned") {
			continue
		}
		if !strings.Contains(line, "sessions=") {
			t.Fatalf("the abandonment record names no sessions: %q", line)
		}
		return
	}
	t.Fatalf("no abandonment record in the log: %q", out.String())
}

func TestBackoffHoldsBetweenAttempts(t *testing.T) {
	dir := fixture(t, "fail")
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)
	s.tickWait(ctx, t0.Add(time.Minute)) // attempt 1
	// Immediately after, a tick must NOT retry (backoff).
	s.tickWait(ctx, t0.Add(time.Minute).Add(10*time.Second))
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
	s.tickWait(ctx, now) // mint pending + attempt 1
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
	s.tickWait(ctx, t0) // register

	now := t0
	for range 3 {
		now = driveToAbandonment(t, s, now)
	}
	st := loadState(t, s)
	if st.ConsecutiveAbandons != 3 || !st.CoolingDown(now.Add(time.Minute)) {
		t.Fatalf("breaker should be tripped after 3 abandonments: %+v", st)
	}
	// While cooling down: ticks mint no new pending runs.
	s.tickWait(ctx, now.Add(2*time.Minute))
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
	s.tickWait(ctx, after)
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

	s.tickWait(ctx, t0)                     // register
	s.tickWait(ctx, t0.Add(61*time.Second)) // run 1: first-run inbox, consume
	inbox := readFile(t, filepath.Join(dir, "memory", "inbox-copy.md"))
	if !strings.Contains(inbox, "first run") || !strings.Contains(inbox, "No pending changes") {
		t.Fatalf("first inbox should be empty-at-current-state: %q", inbox)
	}
	c1, err := memory.At(dir).LoadCursor("every-minute")
	if err != nil || c1 == nil {
		t.Fatalf("cursor should exist after consume: %+v, %v", c1, err)
	}

	s.tickWait(ctx, t0.Add(121*time.Second)) // run 2: feed carries run 1's commit
	inbox = readFile(t, filepath.Join(dir, "memory", "inbox-copy.md"))
	if !strings.Contains(inbox, "Run every-minute") {
		t.Fatalf("second inbox should carry run 1's completion commit: %q", inbox)
	}
	c2, err := memory.At(dir).LoadCursor("every-minute")
	if err != nil || c2 == nil || c2.ConsumedThrough == c1.ConsumedThrough {
		t.Fatalf("cursor should have advanced: %+v -> %+v, %v", c1, c2, err)
	}
}

// A consumer cursor pointing off the memory branch fails at inbox assembly,
// before the model starts, and fails the same way every time. Spending the
// whole retry budget on it buys nothing but delay and noise: the run is
// abandoned on its first attempt, with a task naming the file to repair.
func TestUnreachableCursorAbandonsOnTheFirstAttempt(t *testing.T) {
	dir := fixture(t, "consume")
	os.WriteFile(filepath.Join(dir, "routines", "every-minute.md"), []byte(
		"---\nschedule: \"* * * * *\"\nconsumes: memory\n---\nReport the fake thing.\n"), 0o644)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0) // register
	if err := memory.At(dir).SaveCursor("every-minute", memory.Cursor{
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
	tasks := readFile(t, filepath.Join(dir, "memory", "tasks.md"))
	if !strings.Contains(tasks, "abandoned after 1 attempts") {
		t.Fatalf("expected abandonment on the first attempt: %q", tasks)
	}
	if !strings.Contains(tasks, "cursors/every-minute.json") {
		t.Fatalf("task should name the cursor file to repair: %q", tasks)
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

	s.tickWait(ctx, t0)                     // register
	s.tickWait(ctx, t0.Add(61*time.Second)) // one run completes, memory pushed
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
		s.tickWait(ctx, t0.Add(time.Duration(2+i)*time.Minute))
	}
	ledger = readFile(t, filepath.Join(dir, "memory", "ledgers", "fake.md"))
	if got := strings.Count(ledger, "ran run_"); got != 1 {
		t.Fatalf("dispatch continued after rewrite: %d runs, %q", got, ledger)
	}
	if !s.syncBlocked {
		t.Fatal("supervisor should be sync-blocked after a rewrite")
	}
}

// The datastore is the alerting channel, so the failures that break it are
// the ones where a blocker has to work hardest: the memory branch is exactly
// what a blocked sync refuses to write, and a task committed only locally dies
// with the container. The blocker goes to a supervisor-owned ref instead, and
// moves onto the branch once a human repairs the history.
func TestBlockerReachesOriginWhileSyncIsBlocked(t *testing.T) {
	dir := fixture(t, "ok")
	bare := withOrigin(t, dir)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)                     // register
	s.tickWait(ctx, t0.Add(61*time.Second)) // one run completes, memory pushed

	// Rewrite the memory branch on origin out from under the supervisor.
	discarded := gitOut(t, bare, "rev-parse", "refs/heads/memory")
	c := filepath.Join(t.TempDir(), "clone")
	runCmd(t, "", "git", "clone", "-q", "-b", "memory", bare, c)
	runCmd(t, c, "git", "-c", "user.name=x", "-c", "user.email=x@x", "commit", "--amend", "-q", "--no-edit", "-m", "rewritten")
	runCmd(t, c, "git", "push", "-q", "--force", "origin", "memory")
	rewritten := gitOut(t, bare, "rev-parse", "refs/heads/memory")

	s.tickWait(ctx, t0.Add(2*time.Minute))
	if !s.syncBlocked {
		t.Fatal("precondition: the supervisor should be sync-blocked after a rewrite")
	}
	stranded := gitOut(t, bare, "cat-file", "-p", "refs/openroutines/blocked:tasks.md")
	if !strings.Contains(stranded, "history rewritten") {
		t.Fatalf("the blocker task should have left the container: %q", stranded)
	}
	if got := gitOut(t, bare, "rev-parse", "refs/heads/memory"); got != rewritten {
		t.Fatalf("a blocked supervisor must not write the memory branch: %s -> %s", rewritten, got)
	}
	// A rewrite is how a human repairs memory, up to and including removing
	// something that should never have been there. The supervisor still holds
	// the pre-rewrite lineage locally, so what it strands has to be a snapshot:
	// publishing its own tip would put the discarded history back on origin.
	if n := gitOut(t, bare, "rev-list", "--count", "refs/openroutines/blocked"); n != "1" {
		t.Fatalf("the stranded ref should be a parentless snapshot, got %s commits", n)
	}
	if _, err := gitTry(bare, "merge-base", "--is-ancestor", discarded, "refs/openroutines/blocked"); err == nil {
		t.Fatalf("stranding must not republish the history the rewrite discarded (%.8s)", discarded)
	}

	// The documented repair: a human accepts the new history by moving the
	// accepted ref. Sync recovers, and the stranded blocker lands on the branch.
	runCmd(t, bare, "git", "update-ref", "refs/openroutines/accepted", rewritten)
	s.tickWait(ctx, t0.Add(3*time.Minute))
	if s.syncBlocked {
		t.Fatal("sync should have recovered once the new history was accepted")
	}
	onBranch := gitOut(t, bare, "cat-file", "-p", "refs/heads/memory:tasks.md")
	if !strings.Contains(onBranch, "history rewritten") {
		t.Fatalf("the blocker should be on the memory branch after recovery: %q", onBranch)
	}
	if out, err := gitTry(bare, "rev-parse", "--verify", "--quiet", "refs/openroutines/blocked"); err == nil {
		t.Fatalf("the stranded ref should be cleared once the branch carries it: %s", out)
	}
}

// A stranded snapshot is the only copy of some earlier container's blocker, and
// a healthy successor has no idea what is in it. Clearing the ref is for the
// instance that put its own state there -- nobody else's.
func TestStrandedRefFromAnotherContainerSurvives(t *testing.T) {
	dir := fixture(t, "ok")
	bare := withOrigin(t, dir)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0) // register, memory branch on origin
	earlier := gitOut(t, bare, "rev-parse", "refs/heads/memory")
	runCmd(t, bare, "git", "update-ref", "refs/openroutines/blocked", earlier)

	s.tickWait(ctx, t0.Add(61*time.Second)) // a run completes and pushes memory

	if got := gitOut(t, bare, "rev-parse", "refs/openroutines/blocked"); got != earlier {
		t.Fatalf("a successor's push must leave someone else's stranded ref alone: %.8s -> %s", earlier, got)
	}
}

// An unreachable origin breaks the alerting channel from the other side, and
// it fails early enough in the tick -- the lease heartbeat -- that nothing
// downstream of it runs. The condition still has to leave a record a person
// can find, which means recording it locally while it lasts and publishing it
// when origin comes back.
func TestUnreachableOriginRecordsADurableBlocker(t *testing.T) {
	dir := fixture(t, "ok")
	bare := withOrigin(t, dir)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0) // register, origin healthy

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

	tasks := gitOut(t, bare, "cat-file", "-p", "refs/heads/memory:tasks.md")
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

// Two instances, one origin, three due routines against two run slots. Runs
// execute in parallel while the pool has room, a full pool skips to the next
// tick instead of queueing, and through all of it only the lease holder may
// dispatch -- a second instance booting into the window (a rolling deploy's
// overlap) reads a live lease and launches nothing.
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

	// Three routines due in the same tick against the default two slots: two
	// launch at once, the third waits for a later tick.
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
	holder.leaseTTL = 6 * time.Second
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)
	holder.tickWait(ctx, t0) // register

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
	waitForRuns(2) // both slots filled; intents are pushed, so the second instance can adopt from origin

	other := t.TempDir()
	os.MkdirAll(filepath.Join(other, "routines"), 0o755)
	os.WriteFile(filepath.Join(other, "openroutines.yml"), []byte(agentYAML("UTC")), 0o644)
	writeRoutines(other)
	run(other, "git", "init", "-q", "-b", "main", ".")
	run(other, "git", "remote", "add", "origin", bare)
	second := newSupervisor(t, other)
	second.InstanceID = "second-instance"
	second.leaseTTL = holder.leaseTTL

	acquireCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if err := second.acquireLease(acquireCtx); err == nil {
		t.Fatal("second instance took a live lease while the first had runs in flight")
	}
	// The second instance's memory is the holder's, adopted from origin, so
	// the ledger cannot say who ran what: count launches instead. Only the
	// lease holder may dispatch.
	second.tickWait(ctx, t0.Add(61*time.Second))
	if got := strings.Count(readFile(t, started), "run_"); got != 2 {
		t.Fatalf("second instance dispatched behind the lease holder: %d runs launched, want 2", got)
	}

	<-done // the first wave settled; the third routine's pending run waited, unlaunched
	if got := strings.Count(readFile(t, started), "run_"); got != 2 {
		t.Fatalf("a full pool must skip to the next tick, not queue: %d runs launched, want 2", got)
	}
	// Same tick minute again: nothing new mints, only the waiting pending run
	// dispatches into the now-free pool.
	holder.tickWait(ctx, t0.Add(61*time.Second))
	if got := strings.Count(readFile(t, filepath.Join(dir, "memory", "ledgers", "fake.md")), "ran run_"); got != 3 {
		t.Fatalf("lease holder should have run all 3 routines across two ticks, got %d", got)
	}
}

// Two due routines with two slots run at the same time, not back to back --
// and their settlements into the same shared memory file compose instead of
// the later import clobbering the earlier one's lines.
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
	s.tickWait(ctx, t0) // register

	start := time.Now()
	s.tickWait(ctx, t0.Add(61*time.Second))
	if elapsed := time.Since(start); elapsed >= 5500*time.Millisecond {
		// Each run sleeps 3s: serial is 6s+, parallel is one sleep plus overhead.
		t.Fatalf("two 3s runs took %s -- they did not overlap", elapsed)
	}
	if got := strings.Count(readFile(t, filepath.Join(dir, "memory", "ledgers", "fake.md")), "ran run_"); got != 2 {
		t.Fatalf("concurrent settlements into one ledger file should compose, got %d entries", got)
	}
}

// One run can now outlast the lease TTL: keepLeaseAlive renews on a
// quarter-TTL cadence while the attempt executes, so run length and TTL are
// decoupled -- what lets max_timeout be hours while takeover latency stays
// minutes (design decision "The lease is renewed per run, not per tick").
func TestLeaseStaysLiveThroughALongRun(t *testing.T) {
	dir := fixture(t, "slow")
	base := t.TempDir()
	bare := filepath.Join(base, "origin.git")
	runCmd(t, base, "git", "init", "-q", "-b", "main", "--bare", bare)
	runCmd(t, dir, "git", "remote", "add", "origin", bare)

	holder := newSupervisor(t, dir)
	// The run sleeps 3s against a 1.5s TTL: a lease renewed only at dispatch
	// is 2.2s stale at the assertion below (expired, 0.7s to spare) while the
	// in-run heartbeat keeps it younger than ~0.5s (live, 1s to spare).
	holder.leaseTTL = 1500 * time.Millisecond
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)
	holder.tickWait(ctx, t0) // register

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
	time.Sleep(2200 * time.Millisecond) // deep in the run, past what the dispatch heartbeat could cover

	other := t.TempDir()
	os.MkdirAll(filepath.Join(other, "routines"), 0o755)
	os.WriteFile(filepath.Join(other, "openroutines.yml"), []byte(agentYAML("UTC")), 0o644)
	runCmd(t, other, "git", "init", "-q", "-b", "main", ".")
	runCmd(t, other, "git", "remote", "add", "origin", bare)
	second := newSupervisor(t, other)
	second.InstanceID = "second-instance"
	second.leaseTTL = holder.leaseTTL

	acquireCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if err := second.acquireLease(acquireCtx); err == nil {
		t.Fatal("second instance took the lease while a long run was executing")
	}
	<-done
	if got := strings.Count(readFile(t, filepath.Join(dir, "memory", "ledgers", "fake.md")), "ran run_"); got != 1 {
		t.Fatalf("the long run should have completed under the heartbeat, got %d ledger entries", got)
	}
}

// A lease lost between staging and start hands the reserved attempt back:
// no model process ran, so the budget must not move -- a reservation that
// never becomes a run is given back (docs/design.md), exactly as the
// settlement-side twin of this branch already does.
func TestLeaseLostAfterStagingHandsTheAttemptBack(t *testing.T) {
	dir := fixture(t, "ok")
	withOrigin(t, dir)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)
	s.tickWait(ctx, t0) // register; plan's heartbeat writes the lease

	// Drain the pool so the next tick mints the pending record but cannot
	// dispatch it: the reservation has to happen below, under a lease that
	// is already lost -- a window plan's own heartbeat cannot see.
	uid := <-s.slots
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
	if cleanupErr := s.execute(ctx, r, st, t0.Add(61*time.Second), uid); cleanupErr != nil {
		t.Fatal(cleanupErr)
	}

	after := loadState(t, s)
	if after.Pending == nil || after.Pending.Attempts != 0 {
		t.Fatalf("an attempt that never started must be handed back for the lease holder to retry: %+v", after.Pending)
	}
}

// A lease that is provably gone mid-run cancels the run: the attempt is
// handed back and whoever holds the lease retries it. An instance that
// cannot prove it is the only writer must not let a model process keep
// acting under identities a replacement is about to re-run.
func TestLostLeaseCancelsTheRun(t *testing.T) {
	// The run blocks until it is killed rather than running for a fixed span:
	// cancellation is what has to end this attempt, so the run must not have
	// a finish line of its own to reach first.
	dir := fixture(t, "blocked")
	base := t.TempDir()
	bare := filepath.Join(base, "origin.git")
	runCmd(t, base, "git", "init", "-q", "-b", "main", "--bare", bare)
	runCmd(t, dir, "git", "remote", "add", "origin", bare)

	holder := newSupervisor(t, dir)
	holder.leaseTTL = 1500 * time.Millisecond
	var logs bytes.Buffer
	logging.Setup(&logs, slog.LevelInfo, time.UTC)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)
	holder.tickWait(ctx, t0) // register

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

	// The next heartbeat finds a live foreign lease and must cancel the run.
	usurped := time.Now()
	stopUsurper := usurpLease(t, holder)
	defer stopUsurper()
	<-done

	// Cancellation has to stop the model process, not just mark the attempt:
	// a run left to expire on its own timeout would hand the attempt back
	// too, having spent the 30s still acting under identities the lease
	// holder is about to re-run. Real cancellation lands in well under a
	// second.
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
	if got := readFile(t, filepath.Join(dir, "memory", "ledgers", "fake.md")); strings.Contains(got, "ran run_") {
		t.Fatalf("a canceled run's staged memory must not be imported: %q", got)
	}
	// Concurrent attempts lose the lease together and interleave on one
	// stdout: the cancellation line must say whose attempt it ended.
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

	s.tickWait(ctx, t0)                     // register
	s.tickWait(ctx, t0.Add(61*time.Second)) // mint the run, attempt 1

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
	// Serial (the unset default): with room for both, both reservations
	// would be genuinely concurrent and genuinely owed. The property under
	// test is that the reservation belongs to the executor -- a routine
	// waiting for a slot has spent nothing.
	withOrigin(t, dir)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0)                     // register both
	s.tickWait(ctx, t0.Add(61*time.Second)) // mint both, dispatch serially

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

	s.tickWait(ctx, t0) // register
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

	s.tickWait(ctx, t0) // register

	// The aftermath of a failed intent commit: pending state on disk only.
	st := loadState(t, s)
	st.Pending = &schedule.Pending{RunID: "run_orphaned", ScheduledFor: t0, CoveredThrough: t0, CreatedAt: t0}
	if err := st.Save(s.stateDir()); err != nil {
		t.Fatal(err)
	}
	if n := memory.At(dir).Status().Uncommitted; n == 0 {
		t.Fatal("precondition: the pending record should be uncommitted")
	}

	s.tickWait(ctx, t0.Add(time.Minute)) // dispatches the orphaned pending run

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

	s.tickWait(ctx, t0) // register

	cmd := exec.Command("git", "rev-parse", "--absolute-git-dir")
	cmd.Dir = filepath.Join(dir, "memory")
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
	tasks := readFile(t, filepath.Join(dir, "memory", "tasks.md"))
	if !strings.Contains(tasks, "intent commit failed") {
		t.Fatalf("a halted supervisor should hand the blocker to a human: %q", tasks)
	}
}

// A blocker that persists across many ticks announces its onset once, like
// every sibling "persisting condition" mechanism in this file -- not once
// per minute for the whole outage.
func TestBlockedLogsOnceAcrossTicks(t *testing.T) {
	dir := fixture(t, "ok")
	bare := withOrigin(t, dir)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)

	s.tickWait(ctx, t0) // register, origin healthy

	gone := bare + ".gone"
	if err := os.Rename(bare, gone); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(gone, bare)

	var out bytes.Buffer
	logging.Setup(&out, slog.LevelInfo, time.UTC)
	for i := 1; i <= 3; i++ {
		s.tickWait(ctx, t0.Add(time.Duration(i)*time.Minute))
	}

	if got := strings.Count(out.String(), "kind=origin"); got != 1 {
		t.Fatalf("BLOCKED should log once across a persisting blocker, got %d: %q", got, out.String())
	}
}

// A supervised run's record carries the usage the runner captured from the
// attempt home's session storage, plus the resolved model.
// Killing the process group does not reach a grandchild that left it, and
// that grandchild still holds the run's output pipe. The kill path must stop
// draining the pipe on a deadline instead of parking the supervisor forever.
func TestOrphanHoldingTheOutputPipeDoesNotParkTheTick(t *testing.T) {
	dir := fixture(t, "orphan")
	os.WriteFile(filepath.Join(dir, "routines", "every-minute.md"), []byte(
		"---\nschedule: \"* * * * *\"\ntimeout: 2s\n---\nDo the fake thing.\n"), 0o644)
	s := newSupervisor(t, dir)
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute)
	s.tickWait(ctx, t0) // register

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

	records := readFile(t, filepath.Join(dir, "memory", "runs.jsonl"))
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
	logging.Setup(&out, slog.LevelInfo, time.UTC)

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
