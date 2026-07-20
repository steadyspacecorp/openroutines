// Package supervisor is the long-running scheduler: the container entrypoint.
//
// Every tick it re-reads routine frontmatter, reconciles memory with origin,
// and dispatches due routines serially through the shared run pipeline --
// implementing the durable two-phase model from DESIGN.md: a logical run
// exists durably (committed, pushed) before it is allowed to act; failed
// attempts retry under the same run id with backoff; abandonment after a
// bounded number of attempts raises a blocker and advances the watermark.
package supervisor

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/memory"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/runner"
	"github.com/steadyspacecorp/openroutines/internal/schedule"
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
	syncBlocked   bool // rewritten-history or conflict: stop adopting/pushing
	syncWarned    bool // blocker already raised for the current sync problem
	unreachWarned bool
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
	return &Supervisor{
		Dir:        dir,
		InstanceID: memory.InstanceID(),
		Log:        log.New(os.Stdout, "", log.LstdFlags|log.LUTC),
		loc:        loc,
		noOrigin:   !memory.HasOrigin(dir),
	}, nil
}

func (s *Supervisor) stateDir() string {
	return filepath.Join(memory.WorktreePath(s.Dir), schedule.StateDirName)
}

// Run is the supervise loop: startup, then one Tick per minute until ctx is
// cancelled, then shutdown (final commit and push, lease release).
func (s *Supervisor) Run(ctx context.Context) error {
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
		defer memory.ReleaseLease(s.Dir)
	}
	s.Log.Printf("supervising %s (instance %s, tick %s)", s.Dir, s.InstanceID, TickInterval)
	s.Log.Printf("WARNING: filesystem sandbox (Landlock) is not implemented in this build yet -- see DESIGN.md")

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

	// Reconcile memory with origin, defensively.
	if !s.noOrigin {
		s.syncOnce()
		_ = memory.WriteLease(s.Dir, s.InstanceID, now)
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
		if !r.FM.IsActive() || r.FM.ID == "" || r.FM.Schedule == "" {
			continue
		}
		spec, err := cron.ParseStandard(r.FM.Schedule)
		if err != nil {
			s.Log.Printf("%s: bad schedule %q: %v", r.Name, r.FM.Schedule, err)
			continue
		}
		st, err := schedule.Load(s.stateDir(), r.FM.ID)
		if err != nil {
			s.Log.Printf("%s: %v", r.Name, err)
			continue
		}
		if st == nil {
			// First sight of this routine: it owes nothing from before it existed.
			st = &schedule.State{RoutineID: r.FM.ID, Watermark: now}
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
			s.blockOnce("intent push failed -- runs held until origin is reachable: "+err.Error(), &s.unreachWarned)
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
		if err := memory.Import(s.Dir, staging.MemoryDir); err != nil {
			s.settleFailure(r, st, res, meta, now, "memory rejected: "+err.Error())
			return
		}
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
		s.settleFailure(r, st, res, meta, now, fmt.Sprintf("%s after %s (exit %d)", res.Outcome, res.Duration, res.ExitCode))
	}
}

// settleFailure records a failed attempt: blocker, run record, and -- past
// the attempt cap -- abandonment (watermark advances, pending clears).
func (s *Supervisor) settleFailure(r *routine.Routine, st *schedule.State, res *runner.ExecResult, meta runner.Meta, now time.Time, detail string) {
	p := st.Pending
	_ = memory.AppendBlocker(s.Dir, fmt.Sprintf("[%s] routine %s (%s %s): %s", time.Now().UTC().Format(time.RFC3339), r.Name, p.RunID, meta.AttemptID, detail))
	_ = memory.AppendRunRecord(s.Dir, runner.RecordJSON(r, meta, parseAttempt(meta.AttemptID), res, false))
	abandoned := false
	if p.Attempts >= MaxAttempts {
		abandoned = true
		_ = memory.AppendBlocker(s.Dir, fmt.Sprintf("[%s] routine %s (%s): abandoned after %d attempts -- watermark advanced, human attention needed", time.Now().UTC().Format(time.RFC3339), r.Name, p.RunID, p.Attempts))
		st.Watermark = p.CoveredThrough
		st.Pending = nil
		if cooldown := st.RecordAbandonment(now); cooldown > 0 {
			_ = memory.AppendBlocker(s.Dir, fmt.Sprintf("[%s] routine %s: circuit breaker tripped after %d consecutive abandonments -- cooling down for %s, next success resets", time.Now().UTC().Format(time.RFC3339), r.Name, st.ConsecutiveAbandons, cooldown))
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
		s.blockOnce("memory branch history rewritten on origin -- sync stopped, running on local state: "+rep.Detail, &s.syncWarned)
	case rep.Conflict:
		s.syncBlocked = true
		s.blockOnce("memory sync conflict -- sync stopped, running on local state: "+rep.Detail, &s.syncWarned)
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

// blockOnce raises a blocker for a condition only when it first appears.
func (s *Supervisor) blockOnce(msg string, warned *bool) {
	s.Log.Printf("BLOCKED: %s", msg)
	if !*warned {
		*warned = true
		_ = memory.AppendBlocker(s.Dir, fmt.Sprintf("[%s] supervisor: %s", time.Now().UTC().Format(time.RFC3339), msg))
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

func (s *Supervisor) shutdown() {
	s.Log.Printf("shutting down: final memory sync")
	if _, err := memory.Commit(s.Dir, "Shutdown"); err != nil {
		s.Log.Printf("shutdown commit: %v", err)
	}
	s.pushBestEffort()
}

// acquireLease enforces "exactly one instance": a fresh foreign lease means
// another supervisor is alive (rolling deploy overlap, accidental replica),
// so this one waits rather than corrupting memory.
func (s *Supervisor) acquireLease(ctx context.Context) error {
	for {
		holder, at, exists, err := memory.ReadLease(s.Dir)
		if err != nil {
			return fmt.Errorf("lease: %w", err)
		}
		if !exists || holder == s.InstanceID || time.Since(at) > memory.LeaseTTL {
			return memory.WriteLease(s.Dir, s.InstanceID, time.Now())
		}
		s.Log.Printf("another instance holds the lease (%s, heartbeat %s) -- waiting", holder, at.Format(time.RFC3339))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
		}
	}
}

func parseAttempt(attemptID string) int {
	var n int
	fmt.Sscanf(attemptID, "attempt_%d", &n)
	return n
}
