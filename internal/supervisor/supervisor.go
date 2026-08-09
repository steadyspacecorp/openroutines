// Package supervisor is the long-running scheduler: the container entrypoint.
// Every tick re-reads frontmatter, reconciles knowledge with origin, and
// dispatches due routines into a bounded pool -- the durable two-phase run
// model: a logical run exists durably before it acts, failed attempts retry
// under the same run id, abandonment records a human-owned task.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/knowledge"
	"github.com/steadyspacecorp/openroutines/internal/lock"
	"github.com/steadyspacecorp/openroutines/internal/mode"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/run"
	"github.com/steadyspacecorp/openroutines/internal/runner"
	"github.com/steadyspacecorp/openroutines/internal/sandbox"
	"github.com/steadyspacecorp/openroutines/internal/schedule"
)

// Scheduling constants: the tick cadence and the per-run attempt cap.
const (
	TickInterval   = time.Minute
	MaxAttempts    = 5
	attemptUIDBase = sandbox.AttemptUIDBase
)

// Schedulable reports whether a tick will act on a routine at all. Exported
// because reporting has to agree with dispatch -- a surface that disagrees
// promises a retry that cannot happen.
func Schedulable(r *routine.Routine) bool {
	return r.Frontmatter.IsActive() && (r.Frontmatter.Schedule != "" || r.Frontmatter.Trigger != nil)
}

// Supervisor is the tick loop: it re-reads routines, mints and dispatches
// runs, and syncs knowledge with origin.
type Supervisor struct {
	Dir string

	store     *knowledge.Store
	noOrigin  bool
	loc       *time.Location
	retention time.Duration
	lastTrim  time.Time

	// knowledgeMu serializes every knowledge-worktree critical section: tick
	// bookkeeping, each attempt's reserve-and-stage, each settlement.
	// Kernel-backed, so a manual `routines run` serializes through it too.
	knowledgeMu sync.Locker

	lease    leaseKeeper
	pool     runPool
	blockers blockerTracker
	triggers triggerTracker
}

type runPool struct {
	slots          chan uint32
	runs           sync.WaitGroup
	inFlightMu     sync.Mutex
	inFlight       map[string]bool
	waitLogged     map[string]bool
	cooldownWarned map[string]bool
	fatal          chan error
	reap           func(uint32) error
}

type blockerTracker struct {
	syncBlocked   bool
	syncWarned    bool
	unreachWarned bool
	originWarned  bool
	blockedTip    string
	commitWarned  bool
	loadFailed    map[string]string
}

type triggerTracker struct {
	lastPolled map[string]time.Time
	pollFailed map[string]bool
}

// New builds a supervisor for the agent repository at dir.
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
	retention, err := config.ParseRetention(agent.Retention())
	if err != nil {
		return nil, err
	}
	store := knowledge.NewStore(dir)
	knowledgeMu, err := lock.Locker(dir, "knowledge")
	if err != nil {
		return nil, err
	}
	slots := make(chan uint32, agent.RunSlots())
	for i := range agent.RunSlots() {
		slots <- uint32(attemptUIDBase + i)
	}
	return &Supervisor{
		Dir:         dir,
		store:       store,
		knowledgeMu: knowledgeMu,
		loc:         loc,
		retention:   retention,
		noOrigin:    !store.HasOrigin(),
		lease: leaseKeeper{
			instanceID: knowledge.InstanceID(),
			ttl:        knowledge.LeaseTTL,
		},
		pool: runPool{
			slots:          slots,
			inFlight:       map[string]bool{},
			waitLogged:     map[string]bool{},
			cooldownWarned: map[string]bool{},
			fatal:          make(chan error, 1),
			reap:           sandbox.ReapIdentity,
		},
		blockers: blockerTracker{loadFailed: map[string]string{}},
		triggers: triggerTracker{
			lastPolled: map[string]time.Time{},
			pollFailed: map[string]bool{},
		},
	}, nil
}

func (s *Supervisor) stateDir() string { return s.store.StateDir() }

// InstanceID returns the identity used to own the distributed lease.
func (s *Supervisor) InstanceID() string { return s.lease.instanceID }

// Run is the supervise loop: startup, then one Tick per minute until ctx is
// canceled, then shutdown (final commit and push, lease release).
func (s *Supervisor) Run(ctx context.Context) error {
	// Non-dumpable closes the /proc/<pid>/environ and ptrace paths from
	// same-UID model processes -- set before any child exists.
	if err := sandbox.ProtectProcess(); err != nil {
		slog.Warn("could not mark the supervisor non-dumpable", "error", err)
	}
	s.warnKeyDelivery()
	if configured, err := knowledge.ConfigureDeployKey(); err != nil {
		return fmt.Errorf("deploy key: %w", err)
	} else if configured {
		if knowledge.ConfigureOriginRewrite(s.Dir) {
			slog.Info("routing the https origin through the deploy key")
		}
		slog.Info("deploy key configured for knowledge sync")
	}
	// Under knowledgeMu: first-boot materialization must not race a manual run's
	// own locked Ensure.
	if err := func() error {
		s.knowledgeMu.Lock()
		defer s.knowledgeMu.Unlock()
		return s.store.Ensure()
	}(); err != nil {
		return err
	}
	if s.noOrigin {
		slog.Warn("no git origin -- knowledge is not durable and the single-instance lease is disabled (local mode)")
	} else {
		if err := s.acquireLease(ctx); err != nil {
			return err
		}
		defer func() { s.store.ReleaseLease(s.lease.sha) }()
	}
	slog.Info("supervising", "dir", s.Dir, "instance", s.lease.instanceID, "tick", TickInterval)
	if err := s.verifySandbox(); err != nil {
		return err
	}

	runCtx, cancelRuns := context.WithCancel(ctx)
	defer cancelRuns()
	ticker := time.NewTicker(TickInterval)
	defer ticker.Stop()
	for {
		s.Tick(runCtx, time.Now())
		select {
		case <-ctx.Done():
			// Wait for in-flight settlements before the final commit, or the
			// shutdown push races the records it exists to carry.
			s.pool.runs.Wait()
			s.shutdown()
			return nil
		case err := <-s.pool.fatal:
			cancelRuns()
			s.pool.runs.Wait()
			s.shutdown()
			return err
		case <-ticker.C:
		}
	}
}

// Tick performs one scheduling pass at the given time. Exported so tests can
// drive the supervisor with synthetic clocks. The pass has two halves: plan
// holds the knowledge lock and reconciles state; dispatch launches the due
// attempts into the bounded pool and returns without waiting for them.
func (s *Supervisor) Tick(ctx context.Context, now time.Time) {
	now = now.In(s.loc)
	due, ok := s.plan(now)
	if !ok {
		return
	}

	// Dispatch in due order. A full pool skips, never queues: the pending
	// record is the queue, and the next tick offers the run again.
	sort.Slice(due, func(i, j int) bool {
		return due[i].st.Pending.ScheduledFor.Before(due[j].st.Pending.ScheduledFor)
	})
	for _, d := range due {
		if ctx.Err() != nil {
			slog.Debug("skipped", "reason", "shutting down")
			return // shutting down: stop launching, nothing is reserved yet
		}
		// One attempt per routine at a time; the holder may be an earlier run
		// still settling, or a manual `routines run`.
		release, lockErr := lock.Take(s.Dir, d.r.Name)
		if lockErr != nil {
			if errors.Is(lockErr, lock.ErrLocked) {
				d.r.Log().Warn("attempt already in flight elsewhere (lock held) -- skipping this tick")
			} else {
				d.r.Log().Error("routine lock failed", "error", lockErr)
			}
			continue
		}
		var attemptUID uint32
		select {
		case attemptUID = <-s.pool.slots:
		default:
			release()
			// warn, not info: due work parked behind a full pool looks idle
			// from outside.
			if !s.pool.waitLogged[d.r.Name] {
				s.pool.waitLogged[d.r.Name] = true
				d.r.Log().Warn("all run slots busy -- waiting for a free one",
					"slots", cap(s.pool.slots), "run_id", d.st.Pending.RunID)
			}
			continue
		}
		delete(s.pool.waitLogged, d.r.Name)
		s.setRunning(d.r.Name, true)
		s.pool.runs.Add(1)
		go func(d dueRun, uid uint32, release func()) {
			defer s.pool.runs.Done()
			defer release()
			defer s.setRunning(d.r.Name, false)
			cleanupErr := s.execute(ctx, d.r, d.st, now, uid)
			if !s.releaseIdentity(uid, cleanupErr) {
				return
			}
		}(d, attemptUID, release)
	}
}

func (s *Supervisor) releaseIdentity(uid uint32, cleanupErr error) bool {
	err := cleanupErr
	if mode.Current().Container {
		err = errors.Join(err, s.pool.reap(uid))
	}
	if err != nil {
		fatal := fmt.Errorf("attempt uid %d cleanup failed -- refusing to reuse identity: %w", uid, err)
		slog.Error("attempt uid cleanup failed -- refusing to reuse identity", "uid", uid, "error", err)
		select {
		case s.pool.fatal <- fatal:
		default:
		}
		return false // poisoned: never return this identity to the pool
	}
	s.pool.slots <- uid
	return true
}

// dueRun is one due routine and the scheduling state its attempt owns
// until it settles.
type dueRun struct {
	r  *routine.Routine
	st *schedule.State
}

// plan is the tick's bookkeeping critical section: reconcile knowledge with
// origin, trim, reconcile every routine's scheduling state, and commit the
// intent -- all under the knowledge lock, serialized against in-flight
// reservations and settlements. Returns the runnable dispatches, or ok=false
// when nothing may launch (lost lease, blocked sync, failed intent commit).
func (s *Supervisor) plan(now time.Time) ([]dueRun, bool) {
	s.knowledgeMu.Lock()
	defer s.knowledgeMu.Unlock()

	if !s.noOrigin {
		s.syncOnce()
		// Heartbeat before the sync verdict: a blocked instance is still
		// alive, and a lapsed lease invites a replacement writer.
		if !s.renewLease() {
			return nil, false
		}
		if s.blockers.syncBlocked {
			// Rewritten history or a conflict needs a human; dispatching
			// anyway would re-run external actions as duplicates after
			// container replacement.
			return nil, false
		}
	}

	// Daily retention trim: git history keeps everything, the working files
	// stay lean.
	if now.Sub(s.lastTrim) >= 24*time.Hour {
		s.lastTrim = now
		if changed, err := s.store.Trim(s.retention, now); err != nil {
			slog.Warn("retention trim failed", "error", err)
		} else if changed {
			if _, err := s.store.CommitTrim(s.retention); err != nil {
				slog.Warn("retention trim commit failed", "error", err)
			}
			s.pushBestEffort()
			slog.Info("knowledge: trimmed record streams to the retention window", "retention", s.retention)
		}
	}

	routines, parseErrs := routine.LoadAgent(s.Dir)
	s.reportLoadFailures(parseErrs, now)

	var due []dueRun
	for _, r := range routines {
		if run := s.reconcile(r, now); run != nil {
			due = append(due, *run)
		}
	}

	slog.Debug("tick", "due", len(due), "routines", len(routines), "slots_free", len(s.pool.slots))

	// This tick's own bookkeeping -- minted pending records, refreshed trigger
	// baselines, abandonments -- has to be durable before anything acts on it.
	if !s.commitIntent("Record scheduling state") {
		return nil, false
	}
	return due, true
}

func (s *Supervisor) reconcile(r *routine.Routine, now time.Time) *dueRun {
	log := r.Log()
	if !Schedulable(r) {
		if !r.Frontmatter.IsActive() {
			log.Debug("skipped", "reason", "inactive")
		} else {
			log.Debug("skipped", "reason", "no schedule or trigger")
		}
		return nil
	}
	if s.isRunning(r.Name) {
		log.Debug("skipped", "reason", "in flight")
		return nil
	}

	var spec *schedule.Spec
	if r.Frontmatter.Schedule != "" {
		var err error
		spec, err = schedule.Parse(r.Frontmatter.Schedule, s.loc)
		if err != nil {
			log.Warn("bad schedule", "schedule", r.Frontmatter.Schedule, "error", err)
			return nil
		}
	}
	if r.Frontmatter.Trigger != nil {
		if err := r.Frontmatter.Trigger.Validate(); err != nil {
			log.Warn("invalid trigger", "error", err)
			return nil
		}
	}

	st, err := schedule.Load(s.stateDir(), r.Name)
	if err != nil {
		log.Error("loading scheduling state failed", "error", err)
		return nil
	}
	if st == nil {
		st = &schedule.State{Routine: r.Name, Watermark: now}
		if err := st.Save(s.stateDir()); err != nil {
			log.Error("saving scheduling state failed", "error", err)
			return nil
		}
		log.Info("registered", "watermark", now)
		return nil
	}

	if st.Pending == nil && !s.mintPending(r, st, spec, now) {
		return nil
	}
	if st.Pending.Attempts >= MaxAttempts {
		s.abandon(r, st, fmt.Sprintf("%d attempts started, none settled -- the supervisor did not survive them", st.Pending.Attempts), "", now)
		if err := st.Save(s.stateDir()); err != nil {
			log.Error("saving scheduling state failed", "error", err)
		}
		return nil
	}
	if next := schedule.NextRetryAt(st.Pending); now.Before(next) {
		log.Debug("skipped", "reason", "backing off", "next_attempt_at", next)
		return nil
	}
	return &dueRun{r: r, st: st}
}

func (s *Supervisor) mintPending(r *routine.Routine, st *schedule.State, spec *schedule.Spec, now time.Time) bool {
	log := r.Log()
	if st.CoolingDown(now) {
		if !s.pool.cooldownWarned[r.Name] {
			s.pool.cooldownWarned[r.Name] = true
			log.Warn("circuit breaker cooling down -- no new runs",
				"until", st.CooldownUntil, "consecutive_abandons", st.ConsecutiveAbandons)
		}
		return false
	}
	delete(s.pool.cooldownWarned, r.Name)

	if spec != nil {
		first, last, n := schedule.Occurrences(spec, st.Watermark, now)
		if n > 0 {
			st.Pending = &schedule.Pending{
				RunID:          run.NewID(),
				ScheduledFor:   first,
				CoveredThrough: last,
				CreatedAt:      now,
			}
			if n > 1 {
				log.Info("missed firings collapse into one run", "firings", n, "run_id", st.Pending.RunID)
			}
			if r.Frontmatter.Trigger != nil {
				s.refreshTriggerBaseline(r, now)
			}
		}
	}
	if st.Pending == nil && r.Frontmatter.Trigger != nil && s.evaluateTrigger(r, now) {
		st.Pending = &schedule.Pending{
			RunID:          run.NewID(),
			ScheduledFor:   now,
			CoveredThrough: now,
			CreatedAt:      now,
		}
		log.Info("trigger fired", "run_id", st.Pending.RunID)
	}
	if st.Pending == nil {
		log.Debug("skipped", "reason", "nothing due")
		return false
	}
	if err := st.Save(s.stateDir()); err != nil {
		log.Error("saving scheduling state failed", "error", err)
		return false
	}
	return true
}

// isRunning reports whether a routine has an attempt executing right now.
func (s *Supervisor) isRunning(name string) bool {
	s.pool.inFlightMu.Lock()
	defer s.pool.inFlightMu.Unlock()
	return s.pool.inFlight[name]
}

func (s *Supervisor) setRunning(name string, v bool) {
	s.pool.inFlightMu.Lock()
	defer s.pool.inFlightMu.Unlock()
	if v {
		s.pool.inFlight[name] = true
	} else {
		delete(s.pool.inFlight, name)
	}
}

// commitIntent makes the knowledge worktree durable before anything acts on
// it. Whatever the worktree carries is the intent: Commit no-ops on a clean
// tree, so the normal path costs nothing.
func (s *Supervisor) commitIntent(message string) bool {
	sha, err := s.store.Commit(message)
	if err != nil {
		// A supervisor that cannot record what it is about to do must not
		// do it.
		s.blockOnce("commit", "intent commit failed -- runs held", err, &s.blockers.commitWarned)
		return false
	}
	s.recover("commit", "intent commit recovered -- runs resumed", &s.blockers.commitWarned)
	if s.noOrigin || s.blockers.syncBlocked || sha == "" {
		return true
	}
	if err := s.store.Push(); err != nil {
		// An identity that isn't durable is how duplicates happen.
		s.blockOnce("push", "intent push failed -- runs held until origin is reachable", err, &s.blockers.unreachWarned)
		return false
	}
	s.recover("push", "push to origin recovered -- runs resumed", &s.blockers.unreachWarned)
	return true
}

// reserve claims the attempt a routine is about to run. Returns the give-back
// for an attempt that never becomes a run: a shutdown, a failed intent commit.
func reserve(p *schedule.Pending, now time.Time) (giveBack func()) {
	prior := p.LastAttemptAt
	p.Attempts++
	p.LastAttemptAt = now
	return func() {
		p.Attempts--
		p.LastAttemptAt = prior
	}
}

// abandon gives up on a pending run: the work becomes a human-owned task, the
// watermark advances, and the breaker counts the abandonment. The caller
// saves and commits the state.
func (s *Supervisor) abandon(r *routine.Routine, st *schedule.State, detail, sessionsDir string, now time.Time) {
	p := st.Pending
	date := now.UTC().Format("2006-01-02")
	taskID := "task-" + p.RunID
	if err := s.store.AppendHumanTask(taskID,
		fmt.Sprintf("Investigate routine %s: run %s abandoned after %d attempts (last failure: %s) -- watermark advanced, this work will not retry on its own (source: supervisor; added %s)", r.Name, p.RunID, p.Attempts, detail, date)); err != nil {
		r.Log().Warn("could not record the abandonment task in knowledge -- this log line is the only copy",
			"run_id", p.RunID, "task_id", taskID, "error", err)
	}
	st.Watermark = p.CoveredThrough
	st.Pending = nil
	if cooldown := st.RecordAbandonment(now); cooldown > 0 {
		if err := s.store.AppendEvent(fmt.Sprintf("%s supervisor: routine %s circuit breaker tripped after %d consecutive abandonments -- cooling down for %s, next success resets", date, r.Name, st.ConsecutiveAbandons, cooldown)); err != nil {
			r.Log().Warn("could not record the circuit breaker event in knowledge -- this log line is the only copy",
				"run_id", p.RunID, "error", err)
		}
		r.Log().Error("circuit breaker tripped", "cooldown", cooldown, "run_id", p.RunID)
	}
	r.Log().Error("run abandoned", withSessions(sessionsDir, "run_id", p.RunID, "attempts", p.Attempts)...)
}

// execute runs one attempt of a pending logical run and settles the outcome.
func (s *Supervisor) execute(ctx context.Context, r *routine.Routine, st *schedule.State, now time.Time, attemptUID uint32) (cleanupErr error) {
	// Attempts interleave on one stdout, so the identity travels with the
	// logger.
	log := r.Log().With("run_id", st.Pending.RunID)

	agent, err := config.Load(s.Dir)
	if err != nil {
		log.Error("loading the agent configuration failed", "error", err)
		return
	}

	// The reservation is durable before anything spawns: a container lost
	// mid-attempt is replaced by one that reads this record, so the budget
	// drains instead of retrying forever at attempts: 0.
	p := st.Pending
	s.knowledgeMu.Lock()
	giveBack := reserve(p, now)
	if err := st.Save(s.stateDir()); err != nil {
		s.knowledgeMu.Unlock()
		log.Error("saving scheduling state failed", "error", err)
		return
	}
	if !s.commitIntent(fmt.Sprintf("Reserve %s attempt %d (%s)", r.Name, p.Attempts, p.RunID)) {
		giveBack()
		if err := st.Save(s.stateDir()); err != nil {
			log.Error("saving scheduling state failed", "error", err)
		}
		s.knowledgeMu.Unlock()
		return
	}
	s.knowledgeMu.Unlock()

	// Stage takes the knowledge lock itself, only around its worktree reads:
	// holding knowledgeMu through credential resolution would park every other
	// settlement behind this one's HTTPS round trips.
	meta := runner.Attempt{
		RunID:          p.RunID,
		Number:         p.Attempts,
		ScheduledFor:   p.ScheduledFor,
		CoveredThrough: p.CoveredThrough,
		AttemptUID:     attemptUID,
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	if !s.noOrigin {
		// Ownership proof stays live through staging, execution, settlement,
		// and push.
		stopHeartbeat := s.keepLeaseAlive(runCtx, cancelRun, log)
		defer stopHeartbeat()
	}
	staged, err := runner.Stage(s.Dir, agent, r, meta, s.knowledgeMu)
	if errors.Is(err, runner.ErrAttemptCleanup) {
		cleanupErr = err
	}
	if err == nil && !s.noOrigin && !s.renewLease() {
		cleanupErr = staged.Discard()
		s.knowledgeMu.Lock()
		giveBack()
		if err := st.Save(s.stateDir()); err != nil {
			log.Error("saving scheduling state failed", "error", err)
		}
		s.knowledgeMu.Unlock()
		log.Warn("not started -- lease lost after staging; the current holder will retry it")
		return
	}

	log.Info("attempt starting", "attempt_id", meta.ID(), "scheduled_for", meta.ScheduledFor,
		"timeout", runner.EffectiveTimeout(agent, r))

	var res *runner.AttemptResult
	var staging *runner.AttemptWorkspace
	if err == nil {
		res, staging, err = staged.Run(runCtx)
		if errors.Is(err, runner.ErrAttemptCleanup) {
			cleanupErr = err
		}
	}
	detail := ""
	fatal := false
	if err != nil {
		// The runner classifies; the supervisor only asks.
		fatal = errors.Is(err, runner.ErrFatal)
		log.Error("attempt failed to start", "error", err)
		res = &runner.AttemptResult{Outcome: runner.Crashed, ExitCode: -1}
		detail = err.Error()
	} else {
		defer func() { cleanupErr = errors.Join(cleanupErr, staging.Cleanup()) }()
	}

	// Settlement is the other knowledge critical section: import, run
	// record, scheduling consequences, push.
	s.knowledgeMu.Lock()
	defer s.knowledgeMu.Unlock()
	if !s.noOrigin && !s.renewLease() {
		giveBack()
		if err := st.Save(s.stateDir()); err != nil {
			log.Error("saving scheduling state failed", "error", err)
		}
		log.Warn("settlement discarded -- lease lost before import; the current holder will retry it")
		return
	}

	// The settlement commit carries this attempt's scheduling consequences:
	// success clears pending and advances the watermark, the final failure
	// abandons the run, shutdown returns the reserved attempt so the same
	// logical run retries on next boot.
	abandoned := false
	settlement, serr := runner.Settle(s.Dir, r, staging, res, meta, detail, func(fin *runner.Settlement) {
		switch {
		case fin.Outcome == runner.Canceled:
			giveBack()
		case fin.Outcome == runner.Completed:
			st.Watermark = p.CoveredThrough
			st.Pending = nil
			st.RecordSuccess()
		case fatal, p.Attempts >= MaxAttempts:
			abandoned = true
			s.abandon(r, st, fin.Detail, res.SessionsDir, now)
		}
		if err := st.Save(s.stateDir()); err != nil {
			log.Error("saving scheduling state failed", "error", err)
		}
	})
	if serr != nil {
		log.Error("settle failed", "error", serr)
	}
	if settlement.Discarded {
		log.Info("discarded staged events.md change (teamwork: off)")
	}
	for i, conflict := range settlement.Conflicted {
		taskID := fmt.Sprintf("task-%s-knowledge-conflict-%d", p.RunID, i+1)
		if err := s.store.AppendHumanTask(taskID,
			fmt.Sprintf("Resolve concurrent knowledge edit from routine %s run %s: canonical %s was left unchanged; competing version saved at %s", r.Name, p.RunID, conflict.Path, conflict.Quarantine)); err != nil {
			log.Warn("could not record the knowledge conflict task in knowledge -- this log line is the only copy",
				"path", conflict.Path, "task_id", taskID, "error", err)
		}
		log.Warn("concurrent knowledge edit quarantined -- canonical knowledge left unchanged",
			"path", conflict.Path, "quarantine", conflict.Quarantine)
	}
	conflictCommitOK := true
	if len(settlement.Conflicted) > 0 {
		if _, err := s.store.Commit(fmt.Sprintf("Record %s knowledge conflicts", p.RunID)); err != nil {
			conflictCommitOK = false
			log.Error("conflict task commit failed", "error", err)
		}
	}
	switch {
	case settlement.Outcome == runner.Canceled:
		if ctx.Err() != nil {
			log.Info("interrupted by shutdown -- will retry on next boot")
		} else {
			log.Warn("canceled -- lease lost mid-run; whoever holds the lease retries it")
		}
		return // no push: shutdown's final commit carries the record, and a lease loser must not push
	case settlement.Outcome == runner.Completed:
		log.Info("run completed", withSessions(res.SessionsDir, "duration", res.Duration)...)
	case abandoned:
		// abandon() already said so.
	default:
		log.Error("attempt failed -- will retry", withSessions(res.SessionsDir, "detail", settlement.Detail)...)
	}
	if !conflictCommitOK {
		return // keep completion and remediation together on the next successful push
	}
	s.pushBestEffort()
	return
}

// withSessions names the attempt's exported sessions only when there are some.
func withSessions(sessionsDir string, args ...any) []any {
	if sessionsDir != "" {
		args = append(args, "sessions", sessionsDir)
	}
	return args
}

func (s *Supervisor) syncOnce() {
	rep := s.store.Sync()
	switch {
	case rep.Rewritten:
		s.blockers.syncBlocked = true
		s.blockOnce("sync", "knowledge branch history rewritten on origin -- sync stopped, running on local state", errors.New(rep.Detail), &s.blockers.syncWarned)
		s.strandBlocked()
	case rep.Conflict:
		s.blockers.syncBlocked = true
		s.blockOnce("sync", "knowledge sync conflict -- sync stopped, running on local state", errors.New(rep.Detail), &s.blockers.syncWarned)
		s.strandBlocked()
	case rep.Unreachable:
		// Recorded locally, published when origin returns -- an outage whose
		// only trace is a log line in a replaced container is no trace.
		s.blockOnce("origin", "origin unreachable -- knowledge is not durable and no new runs start until it returns", errors.New(rep.Detail), &s.blockers.originWarned)
	case rep.Detail != "":
		// Sync could not even read the local worktree; an open blocker must
		// not be resolved on the strength of it.
		slog.Warn("knowledge sync did not run", "detail", rep.Detail)
	default:
		s.blockers.syncBlocked = false
		s.recover("sync", "knowledge sync with origin recovered", &s.blockers.syncWarned)
		s.recover("origin", "origin reachable again -- knowledge sync resumed", &s.blockers.originWarned)
		if rep.Adopted {
			slog.Info("knowledge: adopted remote commits")
		}
		if rep.RemoteMissing {
			slog.Debug("knowledge: origin has no knowledge branch yet -- the next push creates it")
		}
	}
}

// reportLoadFailures records in knowledge that a routine stopped loading and,
// later, that it loads again. Events rather than tasks: a broken file heals
// by being edited, so there is nothing for a person to close. Unattributed
// failures are left out -- abandonment already files a task for each.
func (s *Supervisor) reportLoadFailures(errs []error, now time.Time) {
	failing := map[string]string{}
	for _, e := range errs {
		var re *routine.Error
		if !errors.As(e, &re) {
			// No per-routine identity to dedupe by, so this logs every tick
			// it persists.
			slog.Warn("routine load error", "error", e)
			continue
		}
		// The event is read in the repository, where the path is relative.
		failing[re.Name] = strings.TrimPrefix(e.Error(), s.Dir+string(filepath.Separator))
	}

	var news []string
	for _, name := range slices.Sorted(maps.Keys(failing)) {
		if s.blockers.loadFailed[name] != failing[name] {
			slog.Warn("routine load error", "routine", name, "error", failing[name])
			news = append(news, fmt.Sprintf("routine %s does not load (%s) -- it will not run until the file is fixed", name, failing[name]))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(s.blockers.loadFailed)) {
		if _, still := failing[name]; !still {
			news = append(news, fmt.Sprintf("routine %s loads again", name))
		}
	}
	s.blockers.loadFailed = failing
	if len(news) == 0 {
		return
	}

	date := now.UTC().Format("2006-01-02")
	for _, line := range news {
		if err := s.store.AppendEvent(fmt.Sprintf("%s supervisor: %s", date, line)); err != nil {
			slog.Error("recording routine load status failed", "error", err)
			return
		}
		slog.Warn("routine load status changed", "status", line)
	}
	if _, err := s.store.Commit("Record routine load status"); err != nil {
		slog.Error("routine load status commit failed", "error", err)
		return
	}
	s.pushBestEffort()
}

// blockOnce records a blocking condition when it first appears, as a
// human-owned task -- only a person can clear it. The task id is date-scoped
// so a restart doesn't re-record it, and the BLOCKED line announces the onset
// once rather than every tick.
func (s *Supervisor) blockOnce(kind, reason string, err error, warned *bool) {
	if *warned {
		return
	}
	*warned = true
	// BLOCKED and RECOVERED are literal, greppable markers: only these say
	// that dispatch itself is held.
	slog.Error("BLOCKED", "kind", kind, "reason", reason, "error", err)
	msg := reason
	if err != nil {
		msg = reason + ": " + err.Error()
	}
	date := time.Now().UTC().Format("2006-01-02")
	taskID := "task-" + kind + "-" + time.Now().UTC().Format("20060102")
	if aerr := s.store.AppendHumanTask(taskID, fmt.Sprintf("%s (source: supervisor; added %s)", msg, date)); aerr != nil {
		slog.Warn("could not record the supervisor blocker in knowledge -- this log line is the only copy",
			"kind", kind, "task_id", taskID, "error", aerr)
		*warned = false // retry on the next tick
		return
	}
	if _, cerr := s.store.Commit("Record supervisor blocker"); cerr != nil {
		slog.Warn("could not record the supervisor blocker in knowledge -- this log line is the only copy",
			"kind", kind, "task_id", taskID, "error", cerr)
		*warned = false // retry on the next tick
		return
	}
	s.pushBestEffort()
}

// recover clears a blocker whose condition has healed: any open task-<kind>-*
// is completed in place. Runs every healthy tick and matches by id prefix, so
// it also heals blockers raised before a restart.
func (s *Supervisor) recover(kind, msg string, warned *bool) {
	*warned = false
	changed, err := s.store.ResolveHumanTasks("task-"+kind+"-",
		"done "+time.Now().UTC().Format("2006-01-02")+" -- "+msg)
	if err != nil {
		slog.Warn("could not resolve the supervisor blocker task -- it will read as open until repaired",
			"kind", kind, "error", err)
		return
	}
	if !changed {
		return
	}
	slog.Error("RECOVERED", "kind", kind, "reason", msg)
	_, _ = s.store.Commit("Resolve supervisor blocker")
	s.pushBestEffort()
}

// pushBestEffort publishes what the knowledge worktree carries. While sync is
// blocked the record goes to the supervisor-owned blocked ref instead; once
// the branch carries the same state, the stranded copy is dropped.
func (s *Supervisor) pushBestEffort() {
	if s.noOrigin {
		return
	}
	if s.blockers.syncBlocked {
		s.strandBlocked()
		return
	}
	if err := s.store.Push(); err != nil {
		slog.Warn("knowledge push failed (will retry)", "error", err)
		return
	}
	if s.blockers.blockedTip != "" {
		s.blockers.blockedTip = ""
		s.store.ClearBlocked()
	}
}

// strandBlocked publishes knowledge to the blocked ref on every blocked tick,
// so a failed attempt is retried rather than dying with the log line that
// announced it. Keyed on the tip: a tick that changed nothing pushes nothing.
func (s *Supervisor) strandBlocked() {
	tip, err := s.store.Head()
	if err != nil {
		slog.Error("could not read the knowledge tip -- blocked knowledge not stranded to origin", "error", err)
		return
	}
	if tip == s.blockers.blockedTip {
		return
	}
	if err := s.store.PublishBlocked(); err != nil {
		slog.Error("publishing blocked knowledge to origin failed (will retry)", "error", err)
		return
	}
	s.blockers.blockedTip = tip
	slog.Error("knowledge: stranded until sync is repaired", "ref", knowledge.BlockedRef)
}

// warnKeyDelivery says once at boot that the master key value is in this
// process's environment -- the weaker delivery, and boot is the only moment
// anyone is told. Fires on a leftover variable too: unset, it still publishes
// the value.
func (s *Supervisor) warnKeyDelivery() {
	if !mode.Current().Container || !creds.KeyValueInEnv() {
		return
	}
	slog.Warn("the master key value is in this process's environment -- readable wherever that environment is; mount the key as a file, point the file variable at the path, and unset the value variable",
		"value_env", creds.EnvMasterKey, "file_env", creds.EnvMasterKeyFile)
}

func verifyAttemptGroups(groups []int, slots int) error {
	for slot := range slots {
		gid := attemptUIDBase + slot
		if !slices.Contains(groups, gid) {
			return fmt.Errorf("the agent user is not in attempt group %d for run slot %d -- rebuild the deploy image from the current template Dockerfile", gid, slot+1)
		}
	}
	return nil
}

// verifySandbox enforces the fail-closed policy at boot, not mid-run.
func (s *Supervisor) verifySandbox() error {
	switch {
	case mode.Current().Container:
		// Join the attempt groups first -- whether the image's membership
		// reached this process depends on the init that booted the container
		// -- then verify, refusing at boot rather than failing every attempt
		// at staging.
		if err := sandbox.EnsureAttemptGroups(config.MaxConcurrency + 1); err != nil {
			return err
		}
		groups, err := os.Getgroups()
		if err != nil {
			return fmt.Errorf("attempt group check: %w", err)
		}
		if err := verifyAttemptGroups(groups, cap(s.pool.slots)); err != nil {
			return err
		}
		// Constructed environment, like every other child; TMPDIR is the
		// scratch scope it confines.
		probe := exec.Command(sandbox.HelperPath, "sandbox-probe")
		probe.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
			Uid: attemptUIDBase,
			Gid: attemptUIDBase,
		}}
		probe.Env = []string{
			"TMPDIR=" + os.Getenv("TMPDIR"),
			sandbox.EnvAttemptUID + "=" + strconv.Itoa(attemptUIDBase),
		}
		out, probeErr := probe.Output()
		if probeErr == nil {
			// Prove the other half of reusable identities: an escaped process
			// in its own session can be found and killed by UID.
			hold := exec.Command(sandbox.HelperPath, "sandbox-hold")
			hold.Env = []string{sandbox.EnvAttemptUID + "=" + strconv.Itoa(attemptUIDBase)}
			hold.SysProcAttr = &syscall.SysProcAttr{
				Setsid: true,
				Credential: &syscall.Credential{
					Uid: attemptUIDBase,
					Gid: attemptUIDBase,
				},
			}
			if err := hold.Start(); err != nil {
				return fmt.Errorf("attempt uid cleanup probe start: %w", err)
			}
			if err := s.pool.reap(attemptUIDBase); err != nil {
				_ = hold.Process.Kill()
				_ = hold.Wait()
				return fmt.Errorf("attempt uid cleanup probe: %w", err)
			}
			if err := hold.Wait(); err == nil {
				return fmt.Errorf("attempt uid cleanup probe: escaped process was not killed")
			}
			slog.Info("filesystem sandbox active for model processes", "mode", strings.TrimSpace(string(out)))
			return nil
		}
		// The probe tolerates Landlock absence, so failure here means the
		// identity transition itself is broken -- the gating guarantee no
		// override may waive.
		var exitErr *exec.ExitError
		if errors.As(probeErr, &exitErr) && len(exitErr.Stderr) > 0 {
			return fmt.Errorf("attempt identity probe: %w: %s", probeErr, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return fmt.Errorf("attempt identity probe: %w -- the binary needs cap_setuid and cap_setgid; rebuild the deploy image from the current template Dockerfile", probeErr)
	case mode.Current().Native:
		slog.Warn("OPENROUTINES_NATIVE=1 -- model processes run unconfined (dev mode)")
	default:
		slog.Info("model processes run in the per-run container")
	}
	return nil
}

func (s *Supervisor) shutdown() {
	s.knowledgeMu.Lock()
	defer s.knowledgeMu.Unlock()
	slog.Info("shutting down: final knowledge sync")
	if _, err := s.store.Commit("Shutdown"); err != nil {
		slog.Error("shutdown commit failed", "error", err)
	}
	s.pushBestEffort()
}
