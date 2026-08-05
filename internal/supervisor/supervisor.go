// Package supervisor is the long-running scheduler: the container entrypoint.
//
// Every tick it re-reads routine frontmatter, reconciles memory with origin,
// and dispatches due routines into a bounded pool of concurrent runs --
// implementing the durable two-phase run model: a logical run
// exists durably (committed, pushed) before it is allowed to act; failed
// attempts retry under the same run id with backoff; abandonment after a
// bounded number of attempts records a human-owned task and advances the
// watermark. Runs execute in parallel; every memory-worktree operation --
// the tick's bookkeeping, each attempt's reservation and staging, each
// settlement -- takes its turn behind one lock.
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
	"github.com/steadyspacecorp/openroutines/internal/memory"
	"github.com/steadyspacecorp/openroutines/internal/routine"
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
// because reporting has to agree with dispatch: the tick skips these before
// it reads their state, so whatever a skipped routine's state still owes,
// nothing is coming to advance it -- and a surface that says otherwise is
// promising a retry that cannot happen.
func Schedulable(r *routine.Routine) bool {
	return r.FM.IsActive() && (r.FM.Schedule != "" || r.FM.Trigger != nil)
}

// Supervisor is the tick loop: it re-reads routines, mints and dispatches
// runs, and syncs memory with origin.
type Supervisor struct {
	Dir        string
	InstanceID string

	mem       *memory.Memory
	noOrigin  bool
	loc       *time.Location
	retention time.Duration
	lastTrim  time.Time

	// memMu serializes every memory-worktree critical section: the tick's
	// bookkeeping (sync, trim, scheduling state, intent commit), each
	// attempt's reserve-and-stage, and each attempt's settlement. Runs
	// execute in parallel; the ledger they check out from and settle into
	// takes one writer at a time. The lock is kernel-backed, so a manual
	// `routines run` beside this process serializes through it too instead
	// of becoming a second uncoordinated writer. Everything below through
	// pollFailed is touched only under memMu or only by the tick goroutine.
	memMu *runner.MemoryLock

	// leaseMu guards the lease heartbeat state: in-flight runs heartbeat
	// concurrently with each other and with the tick. Lease git operations
	// deliberately do not take memMu -- they touch only the lease ref and
	// the object store, never the worktree.
	leaseMu      sync.Mutex
	leaseSHA     string        // CAS token: the lease blob we last wrote
	leaseTTL     time.Duration // how long a lease survives without a heartbeat
	leaseRenewed time.Time     // wall clock of the last accepted heartbeat
	leaseWarned  bool          // dispatch pause already announced for the current lease problem

	// Bounded parallelism: slots caps concurrent attempts, inFlight keeps a
	// routine's next dispatch off state its executing attempt still owns,
	// runs lets shutdown wait for every settlement.
	slots          chan uint32
	runs           sync.WaitGroup
	inFlightMu     sync.Mutex
	inFlight       map[string]bool
	waitLogged     map[string]bool // pool-full wait already announced (tick only)
	cooldownWarned map[string]bool // circuit-breaker cool-down already announced (tick only)
	fatal          chan error      // fail-closed production invariant violation
	reap           func(uint32) error

	syncBlocked   bool // rewritten-history or conflict: stop adopting/pushing
	syncWarned    bool // blocker already raised for the current sync problem
	unreachWarned bool
	originWarned  bool // origin unreachable: blocker already raised for this outage
	// blockedTip is the memory tip this instance stranded on the blocked ref
	// while sync was refused: what tells a later successful push that the
	// stranded copy is redundant, and what keeps a repeat push idle. Only what
	// this instance stranded: a ref left by a previous container is the only
	// copy of its blocker and must outlive it.
	blockedTip   string
	commitWarned bool              // intent commit failing: dispatch is halted, someone must look
	loadFailed   map[string]string // routine name -> the load failure already recorded

	// Trigger bookkeeping that is deliberately not durable: last-poll times
	// (persisting them would dirty the memory worktree every tick) and
	// poll-error dedup (log on transition, not per tick).
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
	retention, err := memory.ParseRetention(agent.Retention())
	if err != nil {
		return nil, err
	}
	mem := memory.At(dir)
	memMu, err := runner.OpenMemoryLock(dir)
	if err != nil {
		return nil, err
	}
	slots := make(chan uint32, agent.RunSlots())
	for i := range agent.RunSlots() {
		slots <- uint32(attemptUIDBase + i)
	}
	return &Supervisor{
		Dir:            dir,
		InstanceID:     memory.InstanceID(),
		mem:            mem,
		memMu:          memMu,
		leaseTTL:       memory.LeaseTTL,
		loc:            loc,
		retention:      retention,
		noOrigin:       !mem.HasOrigin(),
		slots:          slots,
		inFlight:       map[string]bool{},
		waitLogged:     map[string]bool{},
		cooldownWarned: map[string]bool{},
		fatal:          make(chan error, 1),
		reap:           sandbox.ReapIdentity,
		lastPolled:     map[string]time.Time{},
		pollFailed:     map[string]bool{},
	}, nil
}

func (s *Supervisor) stateDir() string { return s.mem.StateDir() }

// Run is the supervise loop: startup, then one Tick per minute until ctx is
// canceled, then shutdown (final commit and push, lease release).
func (s *Supervisor) Run(ctx context.Context) error {
	// The supervisor's environment may carry the master and deploy keys.
	// Non-dumpable closes the /proc/<pid>/environ and ptrace paths from
	// same-UID model processes -- set before any child ever exists.
	if err := sandbox.ProtectProcess(); err != nil {
		slog.Warn("could not mark the supervisor non-dumpable", "error", err)
	}
	s.warnKeyDelivery()
	if configured, err := memory.ConfigureDeployKey(); err != nil {
		return fmt.Errorf("deploy key: %w", err)
	} else if configured {
		if memory.ConfigureOriginRewrite(s.Dir) {
			slog.Info("routing the https origin through the deploy key")
		}
		slog.Info("deploy key configured for memory sync")
	}
	// Under memMu like every other worktree operation: first-boot
	// materialization racing a manual run's own locked Ensure would have
	// two processes creating the branch, worktree, and seed commit at once.
	if err := func() error {
		s.memMu.Lock()
		defer s.memMu.Unlock()
		return s.mem.Ensure()
	}(); err != nil {
		return err
	}
	if s.noOrigin {
		slog.Warn("no git origin -- memory is not durable and the single-instance lease is disabled (local mode)")
	} else {
		if err := s.acquireLease(ctx); err != nil {
			return err
		}
		defer func() { s.mem.ReleaseLease(s.leaseSHA) }()
	}
	slog.Info("supervising", "dir", s.Dir, "instance", s.InstanceID, "tick", TickInterval)
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
			// Cancellation has reached every in-flight run; wait for their
			// settlements (Canceled, attempts handed back) before the final
			// commit, or the shutdown push races the very records it exists
			// to carry.
			s.runs.Wait()
			s.shutdown()
			return nil
		case err := <-s.fatal:
			cancelRuns()
			s.runs.Wait()
			s.shutdown()
			return err
		case <-ticker.C:
		}
	}
}

// Tick performs one scheduling pass at the given time. Exported so tests can
// drive the supervisor with synthetic clocks. The pass has two halves: plan
// holds the memory lock and reconciles state; dispatch launches the due
// attempts into the bounded pool and returns without waiting for them.
func (s *Supervisor) Tick(ctx context.Context, now time.Time) {
	now = now.In(s.loc)
	due, ok := s.plan(now)
	if !ok {
		return
	}

	// Dispatch in due order. Each worker reserves its own attempt just
	// before it starts (see execute), so a lost container costs a retry only
	// for the runs that were actually running. A full pool skips, never
	// queues: the pending record is the queue, and the next tick offers the
	// run again -- the same shape as every other deferred dispatch.
	sort.Slice(due, func(i, j int) bool {
		return due[i].st.Pending.ScheduledFor.Before(due[j].st.Pending.ScheduledFor)
	})
	for _, d := range due {
		if ctx.Err() != nil {
			slog.Debug("skipped", "reason", "shutting down")
			return // shutting down: stop launching, nothing is reserved yet
		}
		release, lockErr := runner.LockRoutine(s.Dir, d.r.Name)
		if lockErr != nil {
			if errors.Is(lockErr, runner.ErrRoutineLocked) {
				d.r.Log().Warn("attempt already in flight elsewhere (lock held) -- skipping this tick")
			} else {
				d.r.Log().Error("routine lock failed", "error", lockErr)
			}
			continue
		}
		var attemptUID uint32
		select {
		case attemptUID = <-s.slots:
		default:
			release()
			// warn, not info: an agent whose due work is parked behind a
			// full pool looks idle from outside, and an operator running at
			// warn level deserves to know why nothing is happening.
			if !s.waitLogged[d.r.Name] {
				s.waitLogged[d.r.Name] = true
				d.r.Log().Warn("all run slots busy -- waiting for a free one",
					"slots", cap(s.slots), "run_id", d.st.Pending.RunID)
			}
			continue
		}
		delete(s.waitLogged, d.r.Name)
		s.setRunning(d.r.Name, true)
		s.runs.Add(1)
		go func(d dispatch, uid uint32, release func()) {
			defer s.runs.Done()
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
	if os.Getenv("OPENROUTINES_IN_CONTAINER") == "1" {
		err = errors.Join(err, s.reap(uid))
	}
	if err != nil {
		fatal := fmt.Errorf("attempt uid %d cleanup failed -- refusing to reuse identity: %w", uid, err)
		slog.Error("attempt uid cleanup failed -- refusing to reuse identity", "uid", uid, "error", err)
		select {
		case s.fatal <- fatal:
		default:
		}
		return false // poisoned: never return this identity to the pool
	}
	s.slots <- uid
	return true
}

// dispatch is one due routine and the scheduling state its attempt owns
// until it settles.
type dispatch struct {
	r  *routine.Routine
	st *schedule.State
}

// plan is the tick's bookkeeping critical section: reconcile memory with
// origin, trim, reconcile every routine's scheduling state, and commit the
// intent -- all under the memory lock, serialized against in-flight
// reservations and settlements. Returns the runnable dispatches, or ok=false
// when nothing may launch (lost lease, blocked sync, failed intent commit).
func (s *Supervisor) plan(now time.Time) ([]dispatch, bool) {
	s.memMu.Lock()
	defer s.memMu.Unlock()

	// Reconcile memory with origin, defensively; renew the single-instance
	// lease and pause dispatch entirely if we no longer hold it.
	if !s.noOrigin {
		s.syncOnce()
		// Heartbeat before the sync verdict, not after: an instance that is
		// blocked is still alive, and a lease that lapses while its holder
		// runs would invite a replacement to start writing beside it.
		if !s.renewLease() {
			return nil, false
		}
		if s.syncBlocked {
			// Rewritten history or a conflict needs a human. Dispatching
			// anyway would take external actions under identities that exist
			// only in this container -- lost on replacement, then re-run as
			// duplicates. Same rule as an unreachable origin: hold.
			return nil, false
		}
	}

	// Once a day, trim the record streams to the retention window. Git
	// history keeps everything; the working files stay lean.
	if now.Sub(s.lastTrim) >= 24*time.Hour {
		s.lastTrim = now
		if changed, err := s.mem.Trim(s.retention, now); err != nil {
			slog.Warn("retention trim failed", "error", err)
		} else if changed {
			if _, err := s.mem.CommitTrim(s.retention); err != nil {
				slog.Warn("retention trim commit failed", "error", err)
			}
			s.pushBestEffort()
			slog.Info("memory: trimmed record streams to the retention window", "retention", s.retention)
		}
	}

	routines, parseErrs := routine.LoadAgent(s.Dir)
	s.reportLoadFailures(parseErrs, now)

	// Reconcile scheduling state; collect runnable pending runs.
	var due []dispatch
	for _, r := range routines {
		log := r.Log()
		if !Schedulable(r) {
			if !r.FM.IsActive() {
				log.Debug("skipped", "reason", "inactive")
			} else {
				log.Debug("skipped", "reason", "no schedule or trigger")
			}
			continue
		}
		if s.isRunning(r.Name) {
			// An attempt from an earlier tick is still executing. Its
			// settlement owns this routine's state -- reading it here could
			// only mis-mint, and abandoning it mid-flight would fight the
			// settlement over the same record.
			log.Debug("skipped", "reason", "in flight")
			continue
		}
		var spec *schedule.Spec
		if r.FM.Schedule != "" {
			var err error
			spec, err = schedule.Parse(r.FM.Schedule, s.loc)
			if err != nil {
				log.Warn("bad schedule", "schedule", r.FM.Schedule, "error", err)
				continue
			}
		}
		if r.FM.Trigger != nil {
			if err := r.FM.Trigger.Validate(); err != nil {
				log.Warn("invalid trigger", "error", err)
				continue
			}
		}
		st, err := schedule.Load(s.stateDir(), r.Name)
		if err != nil {
			log.Error("loading scheduling state failed", "error", err)
			continue
		}
		if st == nil {
			// First sight of this routine: it owes nothing from before it existed.
			st = &schedule.State{Routine: r.Name, Watermark: now}
			if err := st.Save(s.stateDir()); err != nil {
				log.Error("saving scheduling state failed", "error", err)
				continue
			}
			log.Info("registered", "watermark", now)
			continue
		}
		if st.Pending == nil {
			if st.CoolingDown(now) {
				// An agent whose breaker has tripped looks idle from outside
				// for up to 24h, the same blind spot waitLogged solves for a
				// full run pool -- announce once, not every tick.
				if !s.cooldownWarned[r.Name] {
					s.cooldownWarned[r.Name] = true
					log.Warn("circuit breaker cooling down -- no new runs",
						"until", st.CooldownUntil, "consecutive_abandons", st.ConsecutiveAbandons)
				}
				continue // circuit breaker: no new runs until the cool-down ends
			}
			delete(s.cooldownWarned, r.Name)
			minted := false
			if spec != nil {
				first, last, n := schedule.Occurrences(spec, st.Watermark, now)
				if n > 0 {
					st.Pending = &schedule.Pending{
						RunID:          schedule.NewRunID(),
						ScheduledFor:   first,
						CoveredThrough: last,
						CreatedAt:      now,
					}
					minted = true
					if n > 1 {
						log.Info("missed firings collapse into one run", "firings", n, "run_id", st.Pending.RunID)
					}
					// The scheduled run will pull whatever the trigger would
					// have announced; refresh the baseline so the same news
					// doesn't double-fire right after it.
					if r.FM.Trigger != nil {
						s.refreshTriggerBaseline(r, now)
					}
				}
			}
			if !minted && r.FM.Trigger != nil {
				if s.evaluateTrigger(r, now) {
					st.Pending = &schedule.Pending{
						RunID:          schedule.NewRunID(),
						ScheduledFor:   now,
						CoveredThrough: now,
						CreatedAt:      now,
					}
					minted = true
					log.Info("trigger fired", "run_id", st.Pending.RunID)
				}
			}
			if !minted {
				log.Debug("skipped", "reason", "nothing due")
				continue
			}
			if err := st.Save(s.stateDir()); err != nil {
				log.Error("saving scheduling state failed", "error", err)
				continue
			}
		}
		if st.Pending.Attempts >= MaxAttempts {
			// Every attempt in the budget was started and none of them
			// settled: the supervisor did not survive them. Settlement is
			// where a run is normally abandoned, but a run that kills its
			// container never gets there.
			s.abandon(r, st, fmt.Sprintf("%d attempts started, none settled -- the supervisor did not survive them", st.Pending.Attempts), "", now)
			if err := st.Save(s.stateDir()); err != nil {
				log.Error("saving scheduling state failed", "error", err)
			}
			continue
		}
		if next := schedule.NextRetryAt(st.Pending); now.Before(next) {
			log.Debug("skipped", "reason", "backing off", "next_attempt_at", next)
			continue // backing off after a failed attempt
		}
		due = append(due, dispatch{r, st})
	}

	slog.Debug("tick", "due", len(due), "routines", len(routines), "slots_free", len(s.slots))

	// This tick's own bookkeeping -- minted pending records, refreshed trigger
	// baselines, abandonments -- has to be durable before anything acts on it.
	if !s.commitIntent("Record scheduling state") {
		return nil, false
	}
	return due, true
}

// isRunning reports whether a routine has an attempt executing right now.
func (s *Supervisor) isRunning(name string) bool {
	s.inFlightMu.Lock()
	defer s.inFlightMu.Unlock()
	return s.inFlight[name]
}

func (s *Supervisor) setRunning(name string, v bool) {
	s.inFlightMu.Lock()
	defer s.inFlightMu.Unlock()
	if v {
		s.inFlight[name] = true
	} else {
		delete(s.inFlight, name)
	}
}

// commitIntent makes the memory worktree durable before anything acts on it,
// and reports whether it got there. Persist-before-act rests on the data, not
// on control flow: a tick that wrote state and then failed to commit it leaves
// the record on disk and nowhere else, and no later tick would mint anything
// to notice. So whatever the worktree carries is the intent -- Commit no-ops
// on a clean tree, and the normal path costs nothing.
func (s *Supervisor) commitIntent(message string) bool {
	sha, err := s.mem.Commit(message)
	if err != nil {
		// Dispatch halts until this clears, and only a person can clear it:
		// a supervisor that cannot record what it is about to do must not do it.
		s.blockOnce("commit", "intent commit failed -- runs held", err, &s.commitWarned)
		return false
	}
	s.recover("commit", "intent commit recovered -- runs resumed", &s.commitWarned)
	if s.noOrigin || s.syncBlocked || sha == "" {
		return true
	}
	if err := s.mem.Push(); err != nil {
		// An identity that isn't durable is how duplicates happen: without
		// a pushed intent, no new logical run starts.
		s.blockOnce("push", "intent push failed -- runs held until origin is reachable", err, &s.unreachWarned)
		return false
	}
	s.recover("push", "push to origin recovered -- runs resumed", &s.unreachWarned)
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

// abandon gives up on a pending run: the work becomes a human-owned task (a
// run that falls over never gets to explain itself, and only a person can
// act), the watermark advances so the schedule moves on, and the breaker
// counts the abandonment. The caller saves the state and commits it, and
// passes the last attempt's exported sessions when there are any -- the
// attempt that gives up on a run is the one an operator reads first.
func (s *Supervisor) abandon(r *routine.Routine, st *schedule.State, detail, sessionsDir string, now time.Time) {
	p := st.Pending
	date := now.UTC().Format("2006-01-02")
	taskID := "task-" + p.RunID
	if err := s.mem.AppendHumanTask(taskID,
		fmt.Sprintf("Investigate routine %s: run %s abandoned after %d attempts (last failure: %s) -- watermark advanced, this work will not retry on its own (source: supervisor; added %s)", r.Name, p.RunID, p.Attempts, detail, date)); err != nil {
		r.Log().Warn("could not record the abandonment task in memory -- this log line is the only copy",
			"run_id", p.RunID, "task_id", taskID, "error", err)
	}
	st.Watermark = p.CoveredThrough
	st.Pending = nil
	if cooldown := st.RecordAbandonment(now); cooldown > 0 {
		if err := s.mem.AppendEvent(fmt.Sprintf("%s supervisor: routine %s circuit breaker tripped after %d consecutive abandonments -- cooling down for %s, next success resets", date, r.Name, st.ConsecutiveAbandons, cooldown)); err != nil {
			r.Log().Warn("could not record the circuit breaker event in memory -- this log line is the only copy",
				"run_id", p.RunID, "error", err)
		}
		r.Log().Error("circuit breaker tripped", "cooldown", cooldown, "run_id", p.RunID)
	}
	r.Log().Error("run abandoned", withSessions(sessionsDir, "run_id", p.RunID, "attempts", p.Attempts)...)
}

// execute runs one attempt of a pending logical run and settles the outcome.
func (s *Supervisor) execute(ctx context.Context, r *routine.Routine, st *schedule.State, now time.Time, attemptUID uint32) (cleanupErr error) {
	// Every line this attempt emits carries the routine and the logical run
	// it belongs to: attempts execute concurrently and interleave on one
	// stdout, so the identity has to travel with the logger rather than be
	// repeated by each call site.
	log := r.Log().With("run_id", st.Pending.RunID)

	agent, err := config.Load(s.Dir)
	if err != nil {
		log.Error("loading the agent configuration failed", "error", err)
		return
	}

	// Reserve the attempt before spawning anything, and make the reservation
	// durable in its own right: no model process starts unless the attempt
	// that spawned it is committed and pushed. A container lost mid-attempt is
	// replaced by one that reads this record, so the budget drains as it
	// should instead of retrying forever at attempts: 0.
	p := st.Pending
	s.memMu.Lock()
	giveBack := reserve(p, now)
	if err := st.Save(s.stateDir()); err != nil {
		s.memMu.Unlock()
		log.Error("saving scheduling state failed", "error", err)
		return
	}
	if !s.commitIntent(fmt.Sprintf("Reserve %s attempt %d (%s)", r.Name, p.Attempts, p.RunID)) {
		giveBack()
		if err := st.Save(s.stateDir()); err != nil {
			log.Error("saving scheduling state failed", "error", err)
		}
		s.memMu.Unlock()
		return
	}
	s.memMu.Unlock()

	// Stage takes the memory lock itself, only around its worktree reads:
	// credential resolution can spend seconds on the network, and holding
	// memMu through it would park every other attempt's settlement behind
	// this one's HTTPS round trips.
	meta := runner.Meta{
		RunID:          p.RunID,
		AttemptID:      fmt.Sprintf("attempt_%02d", p.Attempts),
		ScheduledFor:   p.ScheduledFor.Format(time.RFC3339),
		CoveredThrough: p.CoveredThrough.Format(time.RFC3339),
		AttemptUID:     attemptUID,
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	if !s.noOrigin {
		// Ownership proof begins once the reservation is durable and remains
		// live through staging, execution, settlement, and push.
		stopHeartbeat := s.keepLeaseAlive(runCtx, cancelRun, log)
		defer stopHeartbeat()
	}
	staged, err := runner.Stage(s.Dir, agent, r, meta, s.memMu)
	if errors.Is(err, runner.ErrAttemptCleanup) {
		cleanupErr = err
	}
	if err == nil && !s.noOrigin && !s.renewLease() {
		cleanupErr = staged.Discard()
		s.memMu.Lock()
		giveBack()
		if err := st.Save(s.stateDir()); err != nil {
			log.Error("saving scheduling state failed", "error", err)
		}
		s.memMu.Unlock()
		log.Warn("not started -- lease lost after staging; the current holder will retry it")
		return
	}

	log.Info("attempt starting", "attempt_id", meta.AttemptID, "scheduled_for", meta.ScheduledFor,
		"timeout", runner.EffectiveTimeout(agent, r))

	var res *runner.ExecResult
	var staging *runner.Staging
	if err == nil {
		res, staging, err = staged.Run(runCtx)
		if errors.Is(err, runner.ErrAttemptCleanup) {
			cleanupErr = err
		}
	}
	detail := ""
	fatal := false
	if err != nil {
		// The runner classifies; the supervisor only asks. A start failure
		// nothing can repeat past is abandoned on the spot rather than
		// retried to the budget's end for the same error five times.
		fatal = errors.Is(err, runner.ErrFatal)
		log.Error("attempt failed to start", "error", err)
		res = &runner.ExecResult{Outcome: runner.Crashed, ExitCode: -1}
		detail = err.Error()
	} else {
		defer func() { cleanupErr = errors.Join(cleanupErr, staging.Cleanup()) }()
	}

	// Settlement is the other memory critical section: import, run record,
	// scheduling consequences, push -- serialized against other attempts'
	// reservations and settlements and the tick's own bookkeeping.
	s.memMu.Lock()
	defer s.memMu.Unlock()
	if !s.noOrigin && !s.renewLease() {
		giveBack()
		if err := st.Save(s.stateDir()); err != nil {
			log.Error("saving scheduling state failed", "error", err)
		}
		log.Warn("settlement discarded -- lease lost before import; the current holder will retry it")
		return
	}

	// The settlement commit carries this attempt's scheduling consequences:
	// success clears pending and advances the watermark; the final failed
	// attempt abandons the run -- a human-owned task (someone must act)
	// alongside the advanced watermark; shutdown returns the reserved attempt
	// so an interrupted attempt doesn't count toward abandonment and the same
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
		taskID := fmt.Sprintf("task-%s-memory-conflict-%d", p.RunID, i+1)
		if err := s.mem.AppendHumanTask(taskID,
			fmt.Sprintf("Resolve concurrent memory edit from routine %s run %s: canonical %s was left unchanged; competing version saved at %s", r.Name, p.RunID, conflict.Path, conflict.Quarantine)); err != nil {
			log.Warn("could not record the memory conflict task in memory -- this log line is the only copy",
				"path", conflict.Path, "task_id", taskID, "error", err)
		}
		log.Warn("concurrent memory edit quarantined -- canonical memory left unchanged",
			"path", conflict.Path, "quarantine", conflict.Quarantine)
	}
	conflictCommitOK := true
	if len(settlement.Conflicted) > 0 {
		if _, err := s.mem.Commit(fmt.Sprintf("Record %s memory conflicts", p.RunID)); err != nil {
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

// withSessions completes an outcome record's attributes, naming the
// attempt's exported sessions only when there are some.
func withSessions(sessionsDir string, args ...any) []any {
	if sessionsDir != "" {
		args = append(args, "sessions", sessionsDir)
	}
	return args
}

func (s *Supervisor) syncOnce() {
	rep := s.mem.Sync()
	switch {
	case rep.Rewritten:
		s.syncBlocked = true
		s.blockOnce("sync", "memory branch history rewritten on origin -- sync stopped, running on local state", errors.New(rep.Detail), &s.syncWarned)
		s.strandBlocked()
	case rep.Conflict:
		s.syncBlocked = true
		s.blockOnce("sync", "memory sync conflict -- sync stopped, running on local state", errors.New(rep.Detail), &s.syncWarned)
		s.strandBlocked()
	case rep.Unreachable:
		// Recorded locally, published when origin returns. The tick gives up
		// a few lines below this one -- the lease heartbeat needs the same
		// origin -- so nothing downstream will record it, and an outage whose
		// only trace is a log line in a container that gets replaced is no
		// trace at all.
		s.blockOnce("origin", "origin unreachable -- memory is not durable and no new runs start until it returns", errors.New(rep.Detail), &s.originWarned)
	case rep.Detail != "":
		// A flagless report with a Detail means Sync could not even read the
		// local worktree -- it never proved memory is healthy, so an open
		// blocker must not be resolved on the strength of it.
		slog.Warn("memory sync did not run", "detail", rep.Detail)
	default:
		s.syncBlocked = false
		s.recover("sync", "memory sync with origin recovered", &s.syncWarned)
		s.recover("origin", "origin reachable again -- memory sync resumed", &s.originWarned)
		if rep.Adopted {
			slog.Info("memory: adopted remote commits")
		}
		if rep.RemoteMissing {
			slog.Debug("memory: origin has no memory branch yet -- the next push creates it")
		}
	}
}

// reportLoadFailures records in memory that a routine has stopped loading --
// and, later, that it loads again. The tick schedules around a broken file
// (design decision "A broken routine is one broken routine"), so without this
// the only trace of a routine that quietly stopped running is a log line
// nobody is tailing. The transitions are events rather than human-owned tasks:
// a broken file heals by being edited and the next tick notices, so there is
// nothing for a person to close. Unattributed failures -- a directory that
// would not read -- are left out: they fail every attempt, and abandonment
// already files a task for each.
func (s *Supervisor) reportLoadFailures(errs []error, now time.Time) {
	failing := map[string]string{}
	for _, e := range errs {
		var re *routine.Error
		if !errors.As(e, &re) {
			// Not about one routine -- a directory that would not read. There
			// is no stable per-routine identity to dedupe this by, so unlike
			// the attributed case below it logs on every tick it persists.
			slog.Warn("routine load error", "error", e)
			continue
		}
		// The path is absolute in the container; the event is read in the
		// repository, where the file is routines/<name>.md.
		failing[re.Name] = strings.TrimPrefix(e.Error(), s.Dir+string(filepath.Separator))
	}

	var news []string
	for _, name := range slices.Sorted(maps.Keys(failing)) {
		if s.loadFailed[name] != failing[name] {
			slog.Warn("routine load error", "routine", name, "error", failing[name])
			news = append(news, fmt.Sprintf("routine %s does not load (%s) -- it will not run until the file is fixed", name, failing[name]))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(s.loadFailed)) {
		if _, still := failing[name]; !still {
			news = append(news, fmt.Sprintf("routine %s loads again", name))
		}
	}
	s.loadFailed = failing
	if len(news) == 0 {
		return
	}

	date := now.UTC().Format("2006-01-02")
	for _, line := range news {
		if err := s.mem.AppendEvent(fmt.Sprintf("%s supervisor: %s", date, line)); err != nil {
			slog.Error("recording routine load status failed", "error", err)
			return
		}
		slog.Warn("routine load status changed", "status", line)
	}
	if _, err := s.mem.Commit("Record routine load status"); err != nil {
		slog.Error("routine load status commit failed", "error", err)
		return
	}
	s.pushBestEffort()
}

// blockOnce records a blocking condition when it first appears -- as a
// human-owned task only, because only a person can clear it and the task's
// creation is itself an observable change: a companion event would
// double-record the same fact. The task id is date-scoped so a supervisor
// restart doesn't re-record it -- AppendHumanTask skips ids already present.
// The BLOCKED line is gated on the same warned flag: a blocker that lasts
// many ticks announces its onset once, like every sibling "persisting
// condition" mechanism in this file, instead of repeating the same line
// every minute for its whole duration.
func (s *Supervisor) blockOnce(kind, reason string, err error, warned *bool) {
	if *warned {
		return
	}
	*warned = true
	// err can wrap a raw git error carrying a tokened origin URL; the log
	// writer and the memory-append seam both redact from the scrub registry.
	// BLOCKED and RECOVERED stay literal, greppable markers: the level says
	// how bad it is, but only these say that dispatch itself is held.
	slog.Error("BLOCKED", "kind", kind, "reason", reason, "error", err)
	msg := reason
	if err != nil {
		msg = reason + ": " + err.Error()
	}
	date := time.Now().UTC().Format("2006-01-02")
	taskID := "task-" + kind + "-" + time.Now().UTC().Format("20060102")
	if aerr := s.mem.AppendHumanTask(taskID, fmt.Sprintf("%s (source: supervisor; added %s)", msg, date)); aerr != nil {
		slog.Warn("could not record the supervisor blocker in memory -- this log line is the only copy",
			"kind", kind, "task_id", taskID, "error", aerr)
		*warned = false // retry on the next tick
		return
	}
	if _, cerr := s.mem.Commit("Record supervisor blocker"); cerr != nil {
		slog.Warn("could not record the supervisor blocker in memory -- this log line is the only copy",
			"kind", kind, "task_id", taskID, "error", cerr)
		*warned = false // retry on the next tick
		return
	}
	s.pushBestEffort()
}

// recover clears a blocker whose condition has healed: any open task-<kind>-*
// the supervisor previously recorded is completed in place -- the transition
// is the record, no companion event. It runs on every healthy tick and
// matches by id prefix, so it also heals blockers raised before a restart --
// a blocker that outlives its outage is noise a person has to chase.
func (s *Supervisor) recover(kind, msg string, warned *bool) {
	*warned = false
	changed, err := s.mem.ResolveHumanTasks("task-"+kind+"-",
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
	_, _ = s.mem.Commit("Resolve supervisor blocker")
	s.pushBestEffort()
}

// pushBestEffort publishes what the memory worktree carries. While sync is
// blocked the memory branch is the thing being refused, so the record goes to
// the supervisor-owned blocked ref instead -- otherwise the blocker that
// reports a broken datastore would live only on a container that is about to
// be replaced. Once the branch carries the same state, the stranded copy is
// dropped.
func (s *Supervisor) pushBestEffort() {
	if s.noOrigin {
		return
	}
	if s.syncBlocked {
		s.strandBlocked()
		return
	}
	if err := s.mem.Push(); err != nil {
		slog.Warn("memory push failed (will retry)", "error", err)
		return
	}
	if s.blockedTip != "" {
		s.blockedTip = ""
		s.mem.ClearBlocked()
	}
}

// strandBlocked publishes memory to the blocked ref, and is called on every
// blocked tick rather than only when the blocker is first raised: the record
// is the whole point of stranding it, so an attempt that fails has to be
// retried by the next tick instead of dying with the log line that announced
// it. Keyed on the memory tip, so a tick that changed nothing pushes nothing.
func (s *Supervisor) strandBlocked() {
	tip, err := s.mem.Head()
	if err != nil {
		slog.Error("could not read the memory tip -- blocked memory not stranded to origin", "error", err)
		return
	}
	if tip == s.blockedTip {
		return
	}
	if err := s.mem.PublishBlocked(); err != nil {
		slog.Error("publishing blocked memory to origin failed (will retry)", "error", err)
		return
	}
	s.blockedTip = tip
	slog.Error("memory: stranded until sync is repaired", "ref", memory.BlockedRef)
}

// warnKeyDelivery says out loud, once at boot, that the master key value is
// sitting in this process's environment -- the weaker of the two production
// deliveries. Both work; only the file keeps the value out of the
// environment, and a deployment that picked the env var years ago has no
// other moment where anyone is told. It fires on a leftover variable too: a
// deployment that moved to file delivery without unsetting the old one still
// publishes the value. Log-only: the platform that forced env delivery cannot
// be argued with at boot.
func (s *Supervisor) warnKeyDelivery() {
	if os.Getenv("OPENROUTINES_IN_CONTAINER") != "1" || !creds.KeyValueInEnv() {
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

// verifySandbox enforces the fail-closed policy at boot, not mid-run. Only
// production (inside the agent image) spawns model processes natively behind
// the Landlock shim; everywhere else they run in the per-run container, or
// the operator has explicitly opted into unconfined dev mode.
func (s *Supervisor) verifySandbox() error {
	switch {
	case os.Getenv("OPENROUTINES_IN_CONTAINER") == "1":
		// Attempt tree access is granted by group: staging chgrps each run's
		// trees to the attempt's group, unprivileged only because the agent
		// user belongs to every attempt group. Join the groups first --
		// whether the image's membership reached this process depends on the
		// init that booted the container -- then verify: an image without the
		// identities at all would fail every attempt at staging, so refuse
		// at boot instead.
		if err := sandbox.EnsureAttemptGroups(config.MaxConcurrency + 1); err != nil {
			return err
		}
		groups, err := os.Getgroups()
		if err != nil {
			return fmt.Errorf("attempt group check: %w", err)
		}
		if err := verifyAttemptGroups(groups, cap(s.slots)); err != nil {
			return err
		}
		// Constructed, like every other child: an inherited environment
		// would republish the supervisor's keys in the probe's own
		// /proc/<pid>/environ. TMPDIR is the scratch scope it confines.
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
			if err := s.reap(attemptUIDBase); err != nil {
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
		// identity transition itself is broken -- the gating guarantee, which
		// no override may waive (design: "The required boundary is a
		// per-attempt UID"). The unsafe override disables only Landlock, in
		// the shim.
		var exitErr *exec.ExitError
		if errors.As(probeErr, &exitErr) && len(exitErr.Stderr) > 0 {
			return fmt.Errorf("attempt identity probe: %w: %s", probeErr, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return fmt.Errorf("attempt identity probe: %w -- the binary needs cap_setuid and cap_setgid; rebuild the deploy image from the current template Dockerfile", probeErr)
	case os.Getenv("OPENROUTINES_NATIVE") == "1":
		slog.Warn("OPENROUTINES_NATIVE=1 -- model processes run unconfined (dev mode)")
	default:
		slog.Info("model processes run in the per-run container")
	}
	return nil
}

func (s *Supervisor) shutdown() {
	s.memMu.Lock()
	defer s.memMu.Unlock()
	slog.Info("shutting down: final memory sync")
	if _, err := s.mem.Commit("Shutdown"); err != nil {
		slog.Error("shutdown commit failed", "error", err)
	}
	s.pushBestEffort()
}

// acquireLease enforces "exactly one instance": a fresh foreign lease means
// another supervisor is alive (rolling deploy overlap, accidental replica),
// so this one waits rather than corrupting memory. The write is atomic
// (compare-and-swap on the lease ref): two instances racing cannot both win.
func (s *Supervisor) acquireLease(ctx context.Context) error {
	for {
		lease, err := s.mem.ReadLease()
		if err != nil {
			return fmt.Errorf("lease: %w", err)
		}
		expected := ""
		eligible := lease == nil
		if lease != nil {
			expected = lease.SHA
			eligible = lease.Holder == s.InstanceID || time.Since(lease.At) > s.leaseTTL
		}
		if eligible {
			now := time.Now()
			sha, werr := s.mem.WriteLease(s.InstanceID, now, expected)
			if werr == nil {
				s.leaseMu.Lock()
				s.holdLease(sha, now)
				s.leaseMu.Unlock()
				return nil
			}
			// CAS lost: someone else moved first. Loop and re-evaluate.
			slog.Info("lease race lost -- re-evaluating")
			continue
		}
		slog.Warn("another instance holds the lease -- waiting", "holder", lease.Holder, "heartbeat", lease.At)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
		}
	}
}

// renewLease heartbeats atomically against the lease we last wrote. Returns
// false -- pause all dispatch -- when another live instance holds it. The
// heartbeat carries wall-clock time, not the tick's time: liveness is about
// this process still breathing, and staleness is judged against a real clock.
func (s *Supervisor) renewLease() bool {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	return s.renewLeaseLocked()
}

// tryRenewLease renews unless the last heartbeat is younger than a quarter
// TTL. Every in-flight run heartbeats independently; the freshness gate
// collapses them into one push per cadence, and it is what keeps worst-case
// staleness at half a TTL: a quarter of age tolerated here plus a quarter
// until the next heartbeat lands.
func (s *Supervisor) tryRenewLease() bool {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if time.Since(s.leaseRenewed) < s.leaseTTL/4 {
		return true
	}
	return s.renewLeaseLocked()
}

// leaseExpired reports whether the last accepted heartbeat is older than the
// TTL -- past it, this instance can no longer prove it is the only writer.
func (s *Supervisor) leaseExpired() bool {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	return time.Since(s.leaseRenewed) > s.leaseTTL
}

func (s *Supervisor) renewLeaseLocked() bool {
	now := time.Now()
	if sha, err := s.mem.WriteLease(s.InstanceID, now, s.leaseSHA); err == nil {
		s.holdLease(sha, now)
		return true
	}
	lease, err := s.mem.ReadLease()
	if err == nil && lease != nil && lease.Holder != s.InstanceID && time.Since(lease.At) <= s.leaseTTL {
		return s.leaseLost(fmt.Sprintf("lease held by %s (last heartbeat %s ago, expires in %s) -- pausing dispatch",
			lease.Holder, time.Since(lease.At).Round(time.Second), (s.leaseTTL - time.Since(lease.At)).Round(time.Second)))
	}
	expected := ""
	if lease != nil {
		expected = lease.SHA
	}
	if sha, werr := s.mem.WriteLease(s.InstanceID, now, expected); werr == nil {
		s.holdLease(sha, now)
		return true
	}
	return s.leaseLost("lease renewal failed -- pausing dispatch until origin accepts a heartbeat")
}

// keepLeaseAlive heartbeats the lease every quarter TTL while an attempt
// executes, from a goroutine that runs only for as long as the attempt does:
// the returned stop joins it before the attempt settles. Lease reads and
// writes touch only the lease ref and the object store, never the worktree,
// so heartbeats run without the memory lock and concurrent attempts'
// heartbeats coexist -- the freshness gate in tryRenewLease means whoever
// ticks first renews for everyone. A renewal that fails inside the TTL of
// the last accepted heartbeat is tolerated -- origin blips pass, and until
// the TTL expires this instance is still provably the only writer. Past the
// TTL, or the moment a live foreign lease appears, the run is canceled: an
// instance that cannot prove it is the only writer must not let a model
// process keep acting under identities that a replacement is about to
// re-run.
func (s *Supervisor) keepLeaseAlive(ctx context.Context, cancelRun context.CancelFunc, log *slog.Logger) (stop func()) {
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(s.leaseTTL / 4)
		defer ticker.Stop()
		for {
			select {
			case <-quit:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if s.tryRenewLease() {
					continue
				}
				if s.foreignLeaseLive() || s.leaseExpired() {
					log.Error("lease lost mid-run -- canceling the attempt")
					cancelRun()
					return
				}
			}
		}
	}()
	return func() {
		close(quit)
		<-done
	}
}

// foreignLeaseLive reports whether origin currently carries someone else's
// unexpired lease -- the one condition that means another instance may
// already be dispatching.
func (s *Supervisor) foreignLeaseLive() bool {
	lease, err := s.mem.ReadLease()
	return err == nil && lease != nil && lease.Holder != s.InstanceID && time.Since(lease.At) <= s.leaseTTL
}

// holdLease records a successful heartbeat, announcing the recovery when the
// previous one failed through the same RECOVERED/reason shape as recover(),
// so grepping msg="RECOVERED" finds every healed blocker, lease included.
func (s *Supervisor) holdLease(sha string, at time.Time) {
	if s.leaseWarned {
		s.leaseWarned = false
		slog.Error("RECOVERED", "kind", "lease", "reason", "lease heartbeat recovered -- dispatch resumed")
	}
	s.leaseSHA = sha
	s.leaseRenewed = at
}

// leaseLost pauses dispatch and says why the first time. A rolling deploy's
// overlap persists for many ticks; one line each would bury the transition
// that matters. Unlike blockOnce this records nothing in memory -- an
// instance that cannot prove it is the writer must not write.
func (s *Supervisor) leaseLost(msg string) bool {
	if !s.leaseWarned {
		s.leaseWarned = true
		slog.Error("BLOCKED", "kind", "lease", "reason", msg)
	}
	return false
}
