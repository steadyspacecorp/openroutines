// Package supervisor is the long-running scheduler: the container entrypoint.
//
// Every tick it re-reads routine frontmatter, reconciles memory with origin,
// and dispatches due routines serially through the shared run pipeline --
// implementing the durable two-phase model from DESIGN.md: a logical run
// exists durably (committed, pushed) before it is allowed to act; failed
// attempts retry under the same run id with backoff; abandonment after a
// bounded number of attempts records a human-owned task and advances the watermark.
package supervisor

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/memory"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/runner"
	"github.com/steadyspacecorp/openroutines/internal/sandbox"
	"github.com/steadyspacecorp/openroutines/internal/schedule"
	"github.com/steadyspacecorp/openroutines/internal/scrub"
)

const (
	TickInterval = time.Minute
	MaxAttempts  = 5
)

type Supervisor struct {
	Dir        string
	InstanceID string
	Log        *log.Logger

	noOrigin      bool
	loc           *time.Location
	retention     time.Duration
	lastTrim      time.Time
	leaseSHA      string // CAS token: the lease blob we last wrote
	syncBlocked   bool   // rewritten-history or conflict: stop adopting/pushing
	syncWarned    bool   // blocker already raised for the current sync problem
	unreachWarned bool
	secrets       map[string]string // the supervisor's own secrets, for redacting its output
}

func New(dir string) (*Supervisor, error) {
	agent, err := config.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("not an agent repository: %w", err)
	}
	if problems := agent.Problems(); len(problems) > 0 {
		return nil, fmt.Errorf("agent not configured (%s) -- run openroutines configure", problems[0])
	}
	loc, err := time.LoadLocation(agent.Timezone)
	if err != nil {
		return nil, err
	}
	retention, err := memory.ParseRetention(agent.Retention())
	if err != nil {
		return nil, err
	}
	secrets := supervisorSecrets(dir)
	return &Supervisor{
		Dir:        dir,
		InstanceID: memory.InstanceID(),
		Log:        log.New(scrub.NewWriter(os.Stdout, secrets), "", log.LstdFlags|log.LUTC),
		loc:        loc,
		retention:  retention,
		noOrigin:   !memory.HasOrigin(dir),
		secrets:    secrets,
	}, nil
}

// supervisorSecrets collects the secret values the supervisor itself holds
// -- the master key and the deploy key -- so its own log lines and memory
// events can be redacted. (The model's output stream has its own scrubber,
// seeded with the run's credentials.) A git error quoting an origin URL or
// key material would otherwise be logged verbatim and committed to memory.
// The deploy key is multi-line, and logs are scrubbed line by line, so each
// substantial line registers as its own value.
func supervisorSecrets(dir string) map[string]string {
	out := map[string]string{}
	if key, err := creds.LoadKey(dir); err == nil {
		out["master_key"] = hex.EncodeToString(key)
	}
	deployKey := os.Getenv(memory.EnvDeployKey)
	if path := os.Getenv(memory.EnvDeployKeyFile); deployKey == "" && path != "" {
		if raw, err := os.ReadFile(path); err == nil {
			deployKey = string(raw)
		}
	}
	for i, line := range strings.Split(deployKey, "\n") {
		line = strings.TrimSpace(line)
		if len(line) >= 16 && !strings.HasPrefix(line, "-----") {
			out[fmt.Sprintf("deploy_key_%d", i)] = line
		}
	}
	return out
}

func (s *Supervisor) stateDir() string {
	return filepath.Join(memory.WorktreePath(s.Dir), schedule.StateDirName)
}

// Run is the supervise loop: startup, then one Tick per minute until ctx is
// cancelled, then shutdown (final commit and push, lease release).
func (s *Supervisor) Run(ctx context.Context) error {
	// The supervisor's environment may carry the master and deploy keys.
	// Non-dumpable closes the /proc/<pid>/environ and ptrace paths from
	// same-UID model processes -- set before any child ever exists.
	if err := sandbox.ProtectProcess(); err != nil {
		s.Log.Printf("WARNING: could not mark supervisor non-dumpable: %v", err)
	}
	if configured, err := memory.ConfigureDeployKey(); err != nil {
		return fmt.Errorf("deploy key: %w", err)
	} else if configured {
		s.Log.Printf("deploy key configured for memory sync")
	}
	if err := memory.EnsureWorktree(s.Dir); err != nil {
		return err
	}
	if s.noOrigin {
		s.Log.Printf("WARNING: no git origin -- memory is not durable and the single-instance lease is disabled (local mode)")
	} else {
		if err := s.acquireLease(ctx); err != nil {
			return err
		}
		defer func() { memory.ReleaseLease(s.Dir, s.leaseSHA) }()
	}
	s.Log.Printf("supervising %s (instance %s, tick %s)", s.Dir, s.InstanceID, TickInterval)
	if err := s.verifySandbox(); err != nil {
		return err
	}

	ticker := time.NewTicker(TickInterval)
	defer ticker.Stop()
	for {
		s.Tick(ctx, time.Now())
		select {
		case <-ctx.Done():
			s.shutdown()
			return nil
		case <-ticker.C:
		}
	}
}

// Tick performs one scheduling pass at the given time. Exported so tests can
// drive the supervisor with synthetic clocks.
func (s *Supervisor) Tick(ctx context.Context, now time.Time) {
	now = now.In(s.loc)

	// Reconcile memory with origin, defensively; renew the single-instance
	// lease and pause dispatch entirely if we no longer hold it.
	if !s.noOrigin {
		s.syncOnce()
		if s.syncBlocked {
			// Rewritten history or a conflict needs a human. Dispatching
			// anyway would take external actions under identities that exist
			// only in this container -- lost on replacement, then re-run as
			// duplicates. Same rule as an unreachable origin: hold.
			return
		}
		if !s.renewLease(now) {
			return
		}
	}

	// Once a day, trim the record streams to the retention window. Git
	// history keeps everything; the working files stay lean.
	if now.Sub(s.lastTrim) >= 24*time.Hour {
		s.lastTrim = now
		if changed, err := memory.Trim(s.Dir, s.retention, now); err != nil {
			s.Log.Printf("retention trim: %v", err)
		} else if changed {
			if _, err := memory.Commit(s.Dir, fmt.Sprintf("Trim memory to retention window (%s)", s.retention)); err != nil {
				s.Log.Printf("retention trim commit: %v", err)
			}
			s.pushBestEffort()
			s.Log.Printf("memory: trimmed record streams to the %s retention window", s.retention)
		}
	}

	routines, parseErrs := routine.LoadDir(filepath.Join(s.Dir, "routines"))
	for _, e := range parseErrs {
		s.Log.Printf("routine parse error: %v", e)
	}

	// Phase 1: reconcile scheduling state; collect runnable pending runs.
	type dispatch struct {
		r  *routine.Routine
		st *schedule.State
	}
	var due []dispatch
	stateChanged := false
	for _, r := range routines {
		if !r.FM.IsActive() || r.FM.Schedule == "" {
			continue
		}
		spec, err := cron.ParseStandard(r.FM.Schedule)
		if err != nil {
			s.Log.Printf("%s: bad schedule %q: %v", r.Name, r.FM.Schedule, err)
			continue
		}
		st, err := schedule.Load(s.stateDir(), r.Name)
		if err != nil {
			s.Log.Printf("%s: %v", r.Name, err)
			continue
		}
		if st == nil {
			// First sight of this routine: it owes nothing from before it existed.
			st = &schedule.State{Routine: r.Name, Watermark: now}
			if err := st.Save(s.stateDir()); err != nil {
				s.Log.Printf("%s: %v", r.Name, err)
				continue
			}
			s.Log.Printf("%s: registered (watermark %s)", r.Name, now.Format(time.RFC3339))
			stateChanged = true
			continue
		}
		if st.Pending == nil {
			if st.CoolingDown(now) {
				continue // circuit breaker: no new runs until the cool-down ends
			}
			first, last, n := schedule.Occurrences(spec, st.Watermark, now)
			if n == 0 {
				continue
			}
			st.Pending = &schedule.Pending{
				RunID:          schedule.NewRunID(),
				ScheduledFor:   first,
				CoveredThrough: last,
				CreatedAt:      now,
			}
			if err := st.Save(s.stateDir()); err != nil {
				s.Log.Printf("%s: %v", r.Name, err)
				continue
			}
			stateChanged = true
			if n > 1 {
				s.Log.Printf("%s: %d missed firings collapse into run %s", r.Name, n, st.Pending.RunID)
			}
		}
		if now.Before(schedule.NextRetryAt(st.Pending)) {
			continue // backing off after a failed attempt
		}
		due = append(due, dispatch{r, st})
	}

	// Persist-before-act: intent commits must be durable before any run acts.
	if stateChanged {
		if _, err := memory.Commit(s.Dir, "Record pending runs"); err != nil {
			s.Log.Printf("intent commit failed: %v", err)
			return
		}
	}
	if !s.noOrigin && len(due) > 0 && !s.syncBlocked {
		if err := memory.Push(s.Dir); err != nil {
			// An identity that isn't durable is how duplicates happen: without
			// a pushed intent, no new logical run starts.
			s.blockOnce("push", "intent push failed -- runs held until origin is reachable: "+err.Error(), &s.unreachWarned)
			return
		}
		s.unreachWarned = false
	}

	// Phase 2: execute serially, in due order.
	sort.Slice(due, func(i, j int) bool {
		return due[i].st.Pending.ScheduledFor.Before(due[j].st.Pending.ScheduledFor)
	})
	for _, d := range due {
		if ctx.Err() != nil {
			return // shutting down: stop launching
		}
		s.execute(ctx, d.r, d.st, now)
	}
}

// execute runs one attempt of a pending logical run and settles the outcome.
func (s *Supervisor) execute(ctx context.Context, r *routine.Routine, st *schedule.State, now time.Time) {
	// The per-routine kernel lock covers the whole attempt, snapshot through
	// settlement. Held means a manual `routines run` is mid-flight in this
	// checkout: skip and log, per DESIGN.md "Overlap" -- the pending run
	// retries on a later tick.
	release, lockErr := runner.LockRoutine(s.Dir, r.Name)
	if lockErr != nil {
		if errors.Is(lockErr, runner.ErrRoutineLocked) {
			s.Log.Printf("%s: attempt already in flight elsewhere (lock held) -- skipping this tick", r.Name)
		} else {
			s.Log.Printf("%s: routine lock: %v", r.Name, lockErr)
		}
		return
	}
	defer release()

	agent, err := config.Load(s.Dir)
	if err != nil {
		s.Log.Printf("%s: %v", r.Name, err)
		return
	}
	p := st.Pending
	p.Attempts++
	p.LastAttemptAt = now
	if err := st.Save(s.stateDir()); err != nil {
		s.Log.Printf("%s: %v", r.Name, err)
		return
	}
	meta := runner.Meta{
		RunID:          p.RunID,
		AttemptID:      fmt.Sprintf("attempt_%02d", p.Attempts),
		ScheduledFor:   p.ScheduledFor.Format(time.RFC3339),
		CoveredThrough: p.CoveredThrough.Format(time.RFC3339),
	}
	s.Log.Printf("%s: %s %s starting (scheduled %s)", r.Name, p.RunID, meta.AttemptID, meta.ScheduledFor)

	res, staging, err := runner.Execute(ctx, s.Dir, agent, r, meta)
	if err != nil {
		s.Log.Printf("%s: %s failed to start: %v", r.Name, p.RunID, err)
		s.settleFailure(r, st, &runner.ExecResult{Outcome: runner.Crashed, ExitCode: -1}, meta, now, err.Error())
		return
	}
	defer staging.Cleanup()

	switch res.Outcome {
	case runner.Completed:
		discarded, err := runner.ImportMemory(s.Dir, r, staging)
		if err != nil {
			s.settleFailure(r, st, res, meta, now, "memory rejected: "+err.Error())
			return
		}
		if discarded {
			s.Log.Printf("%s: discarded staged events.md change (events: false)", r.Name)
		}
		runner.AdvanceConsumer(s.Dir, r, staging, p.RunID)
		st.Watermark = p.CoveredThrough
		st.Pending = nil
		st.RecordSuccess()
		if err := st.Save(s.stateDir()); err != nil {
			s.Log.Printf("%s: %v", r.Name, err)
		}
		_ = memory.AppendRunRecord(s.Dir, runner.RecordJSON(r, meta, parseAttempt(meta.AttemptID), res, false))
		if _, err := memory.Commit(s.Dir, fmt.Sprintf("Run %s (%s): completed", r.Name, p.RunID)); err != nil {
			s.Log.Printf("%s: commit: %v", r.Name, err)
		}
		s.pushBestEffort()
		s.Log.Printf("%s: %s completed in %s", r.Name, p.RunID, res.Duration)
	case runner.Canceled:
		// Shutdown killed the attempt: pending survives untouched (the same
		// logical run retries on next boot); undo the attempt increment so
		// an interrupted attempt doesn't count toward abandonment.
		p.Attempts--
		_ = st.Save(s.stateDir())
		_ = memory.AppendRunRecord(s.Dir, runner.RecordJSON(r, meta, parseAttempt(meta.AttemptID), res, false))
		s.Log.Printf("%s: %s interrupted by shutdown -- will retry on next boot", r.Name, p.RunID)
	default:
		detail := fmt.Sprintf("%s after %s (exit %d)", res.Outcome, res.Duration, res.ExitCode)
		if res.Hint != "" {
			detail += " -- " + res.Hint
		}
		s.settleFailure(r, st, res, meta, now, detail)
	}
}

// settleFailure records a failed attempt: an event (it happened), a run
// record, and -- past the attempt cap -- abandonment, which is a human-owned
// task (someone must act) alongside the advanced watermark.
func (s *Supervisor) settleFailure(r *routine.Routine, st *schedule.State, res *runner.ExecResult, meta runner.Meta, now time.Time, detail string) {
	// Failure detail often quotes raw errors (git, provider); memory events
	// are committed and pushed, so redact before recording.
	detail = scrub.Redact(detail, s.secrets)
	p := st.Pending
	date := now.UTC().Format("2006-01-02")
	_ = memory.AppendEvent(s.Dir, fmt.Sprintf("%s supervisor: routine %s (%s %s) %s", date, r.Name, p.RunID, meta.AttemptID, detail))
	_ = memory.AppendRunRecord(s.Dir, runner.RecordJSON(r, meta, parseAttempt(meta.AttemptID), res, false))
	abandoned := false
	if p.Attempts >= MaxAttempts {
		abandoned = true
		_ = memory.AppendHumanTask(s.Dir, "task-"+p.RunID,
			fmt.Sprintf("Investigate routine %s: run %s abandoned after %d attempts (last failure: %s) -- watermark advanced, this work will not retry on its own (source: supervisor; added %s)", r.Name, p.RunID, p.Attempts, detail, date))
		st.Watermark = p.CoveredThrough
		st.Pending = nil
		if cooldown := st.RecordAbandonment(now); cooldown > 0 {
			_ = memory.AppendEvent(s.Dir, fmt.Sprintf("%s supervisor: routine %s circuit breaker tripped after %d consecutive abandonments -- cooling down for %s, next success resets", date, r.Name, st.ConsecutiveAbandons, cooldown))
			s.Log.Printf("%s: circuit breaker tripped -- cooling down for %s", r.Name, cooldown)
		}
	}
	if err := st.Save(s.stateDir()); err != nil {
		s.Log.Printf("%s: %v", r.Name, err)
	}
	if _, err := memory.Commit(s.Dir, fmt.Sprintf("Run %s (%s): %s", r.Name, p.RunID, res.Outcome)); err != nil {
		s.Log.Printf("%s: commit: %v", r.Name, err)
	}
	s.pushBestEffort()
	if abandoned {
		s.Log.Printf("%s: %s abandoned after %d attempts", r.Name, p.RunID, MaxAttempts)
	} else {
		s.Log.Printf("%s: %s attempt failed (%s) -- will retry", r.Name, p.RunID, detail)
	}
}

func (s *Supervisor) syncOnce() {
	rep := memory.Sync(s.Dir)
	switch {
	case rep.Rewritten:
		s.syncBlocked = true
		s.blockOnce("sync", "memory branch history rewritten on origin -- sync stopped, running on local state: "+rep.Detail, &s.syncWarned)
	case rep.Conflict:
		s.syncBlocked = true
		s.blockOnce("sync", "memory sync conflict -- sync stopped, running on local state: "+rep.Detail, &s.syncWarned)
	case rep.Unreachable:
		s.Log.Printf("origin unreachable: %s", rep.Detail)
	default:
		s.syncBlocked = false
		s.syncWarned = false
		if rep.Adopted {
			s.Log.Printf("memory: adopted remote commits")
		}
	}
}

// blockOnce records a blocking condition when it first appears: an event for
// the history, and a human-owned task because only a person can clear it. The
// task id is date-scoped so a supervisor restart doesn't re-record it --
// AppendHumanTask skips ids already present.
func (s *Supervisor) blockOnce(kind, msg string, warned *bool) {
	// Sync/push failure text quotes raw git errors, which can carry a
	// tokened origin URL -- and the message below is committed to memory.
	msg = scrub.Redact(msg, s.secrets)
	s.Log.Printf("BLOCKED: %s", msg)
	if !*warned {
		*warned = true
		date := time.Now().UTC().Format("2006-01-02")
		_ = memory.AppendEvent(s.Dir, fmt.Sprintf("%s supervisor: %s", date, msg))
		_ = memory.AppendHumanTask(s.Dir, "task-"+kind+"-"+time.Now().UTC().Format("20060102"),
			fmt.Sprintf("%s (source: supervisor; added %s)", msg, date))
		_, _ = memory.Commit(s.Dir, "Record supervisor blocker")
		s.pushBestEffort()
	}
}

func (s *Supervisor) pushBestEffort() {
	if s.noOrigin || s.syncBlocked {
		return
	}
	if err := memory.Push(s.Dir); err != nil {
		s.Log.Printf("memory push failed (will retry): %v", err)
	}
}

// verifySandbox enforces the fail-closed policy at boot, not mid-run. Only
// production (inside the agent image) spawns model processes natively behind
// the Landlock shim; everywhere else they run in the per-run container, or
// the operator has explicitly opted into unconfined dev mode.
func (s *Supervisor) verifySandbox() error {
	switch {
	case os.Getenv("OPENROUTINES_IN_CONTAINER") == "1":
		self, err := os.Executable()
		if err != nil {
			return err
		}
		out, probeErr := exec.Command(self, "sandbox-probe").Output()
		if probeErr == nil {
			s.Log.Printf("filesystem sandbox: %s active for model processes", strings.TrimSpace(string(out)))
			return nil
		}
		if os.Getenv(sandbox.EnvUnsafeOverride) == "1" {
			s.Log.Printf("WARNING: filesystem sandbox DISABLED by %s -- model processes run unconfined", sandbox.EnvUnsafeOverride)
			return nil
		}
		return fmt.Errorf("filesystem sandbox required but unavailable (host kernel without Landlock?) -- refusing to supervise; set %s=1 to run unconfined deliberately", sandbox.EnvUnsafeOverride)
	case os.Getenv("OPENROUTINES_NATIVE") == "1":
		s.Log.Printf("WARNING: OPENROUTINES_NATIVE=1 -- model processes run unconfined (dev mode)")
	default:
		s.Log.Printf("model processes run in the per-run container")
	}
	return nil
}

func (s *Supervisor) shutdown() {
	s.Log.Printf("shutting down: final memory sync")
	if _, err := memory.Commit(s.Dir, "Shutdown"); err != nil {
		s.Log.Printf("shutdown commit: %v", err)
	}
	s.pushBestEffort()
}

// acquireLease enforces "exactly one instance": a fresh foreign lease means
// another supervisor is alive (rolling deploy overlap, accidental replica),
// so this one waits rather than corrupting memory. The write is atomic
// (compare-and-swap on the lease ref): two instances racing cannot both win.
func (s *Supervisor) acquireLease(ctx context.Context) error {
	for {
		lease, err := memory.ReadLease(s.Dir)
		if err != nil {
			return fmt.Errorf("lease: %w", err)
		}
		expected := ""
		eligible := lease == nil
		if lease != nil {
			expected = lease.SHA
			eligible = lease.Holder == s.InstanceID || time.Since(lease.At) > memory.LeaseTTL
		}
		if eligible {
			sha, werr := memory.WriteLease(s.Dir, s.InstanceID, time.Now(), expected)
			if werr == nil {
				s.leaseSHA = sha
				return nil
			}
			// CAS lost: someone else moved first. Loop and re-evaluate.
			s.Log.Printf("lease race lost -- re-evaluating")
			continue
		}
		s.Log.Printf("another instance holds the lease (%s, heartbeat %s) -- waiting", lease.Holder, lease.At.Format(time.RFC3339))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
		}
	}
}

// renewLease heartbeats atomically against the lease we last wrote. Returns
// false -- pause all dispatch -- when another live instance holds it.
func (s *Supervisor) renewLease(now time.Time) bool {
	if sha, err := memory.WriteLease(s.Dir, s.InstanceID, now, s.leaseSHA); err == nil {
		s.leaseSHA = sha
		return true
	}
	lease, err := memory.ReadLease(s.Dir)
	if err == nil && lease != nil && lease.Holder != s.InstanceID && time.Since(lease.At) <= memory.LeaseTTL {
		s.Log.Printf("BLOCKED: lease held by %s (heartbeat %s) -- pausing dispatch", lease.Holder, lease.At.Format(time.RFC3339))
		return false
	}
	expected := ""
	if lease != nil {
		expected = lease.SHA
	}
	if sha, werr := memory.WriteLease(s.Dir, s.InstanceID, now, expected); werr == nil {
		s.leaseSHA = sha
		return true
	}
	s.Log.Printf("lease renewal failed -- pausing dispatch this tick")
	return false
}

func parseAttempt(attemptID string) int {
	var n int
	fmt.Sscanf(attemptID, "attempt_%d", &n)
	return n
}
