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
	"sort"
	"sync"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/knowledge"
	"github.com/steadyspacecorp/openroutines/internal/lock"
	"github.com/steadyspacecorp/openroutines/internal/mode"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/sandbox"
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
		return due[i].state.Pending.ScheduledFor.Before(due[j].state.Pending.ScheduledFor)
	})
	for _, planned := range due {
		if ctx.Err() != nil {
			slog.Debug("skipped", "reason", "shutting down")
			return // shutting down: stop launching, nothing is reserved yet
		}
		// One attempt per routine at a time; the holder may be an earlier run
		// still settling, or a manual `routines run`.
		release, lockErr := lock.Take(s.Dir, planned.routine.Name)
		if lockErr != nil {
			if errors.Is(lockErr, lock.ErrLocked) {
				planned.routine.Log().Warn("attempt already in flight elsewhere (lock held) -- skipping this tick")
			} else {
				planned.routine.Log().Error("routine lock failed", "error", lockErr)
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
			if !s.pool.waitLogged[planned.routine.Name] {
				s.pool.waitLogged[planned.routine.Name] = true
				planned.routine.Log().Warn("all run slots busy -- waiting for a free one",
					"slots", cap(s.pool.slots), "run_id", planned.state.Pending.RunID)
			}
			continue
		}
		delete(s.pool.waitLogged, planned.routine.Name)
		s.setRunning(planned.routine.Name, true)
		s.pool.runs.Add(1)
		go func(run dueRun, uid uint32, release func()) {
			defer s.pool.runs.Done()
			defer release()
			defer s.setRunning(run.routine.Name, false)
			cleanupErr := s.execute(ctx, run.routine, run.state, now, uid)
			if !s.releaseIdentity(uid, cleanupErr) {
				return
			}
		}(planned, attemptUID, release)
	}
}

func (s *Supervisor) releaseIdentity(uid uint32, cleanupErr error) bool {
	err := cleanupErr
	if mode.Current() == mode.DeployedContainer {
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

func (s *Supervisor) shutdown() {
	s.knowledgeMu.Lock()
	defer s.knowledgeMu.Unlock()
	slog.Info("shutting down: final knowledge sync")
	if _, err := s.store.Commit("Shutdown"); err != nil {
		slog.Error("shutdown commit failed", "error", err)
	}
	s.pushBestEffort()
}
