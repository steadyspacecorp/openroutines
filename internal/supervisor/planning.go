package supervisor

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/run"
	"github.com/steadyspacecorp/openroutines/internal/schedule"
)

type dueRun struct {
	routine *routine.Routine
	state   *schedule.State
}

// The tick's bookkeeping critical section: reconcile knowledge with
// origin, trim, reconcile every routine's scheduling state, and commit the
// intent -- all under the knowledge lock, serialized against in-flight
// reservations and settlements. Returns the runnable dispatches, or ok=false
// when nothing may launch (lost lease, blocked sync, failed intent commit).
func (s *Supervisor) plan(now time.Time) ([]dueRun, bool) {
	s.knowledgeMu.Lock()
	defer s.knowledgeMu.Unlock()

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
		s.abandon(r, st, fmt.Sprintf("%d attempts started, none settled -- the supervisor did not survive them", st.Pending.Attempts), now)
		if err := st.Save(s.stateDir()); err != nil {
			log.Error("saving scheduling state failed", "error", err)
		}
		return nil
	}
	if next := schedule.NextRetryAt(st.Pending); now.Before(next) {
		log.Debug("skipped", "reason", "backing off", "next_attempt_at", next)
		return nil
	}
	return &dueRun{routine: r, state: st}
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

func (s *Supervisor) isRunning(name string) bool {
	s.pool.inFlightMu.Lock()
	defer s.pool.inFlightMu.Unlock()
	return s.pool.inFlight[name]
}

func (s *Supervisor) setRunning(name string, running bool) {
	s.pool.inFlightMu.Lock()
	defer s.pool.inFlightMu.Unlock()
	if running {
		s.pool.inFlight[name] = true
	} else {
		delete(s.pool.inFlight, name)
	}
}

// Makes the knowledge worktree durable before anything acts on
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
	if s.blockers.syncBlocked || sha == "" {
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

// Claims the attempt a routine is about to run. Returns the give-back
// for an attempt that never becomes a run: a shutdown, a failed intent commit.
func reserve(pending *schedule.Pending, now time.Time) (giveBack func()) {
	prior := pending.LastAttemptAt
	pending.Attempts++
	pending.LastAttemptAt = now
	return func() {
		pending.Attempts--
		pending.LastAttemptAt = prior
	}
}

// Gives up on a pending run: the work becomes a human-owned task, the
// watermark advances, and the breaker counts the abandonment. The caller
// saves and commits the state.
func (s *Supervisor) abandon(r *routine.Routine, st *schedule.State, detail string, now time.Time) {
	pending := st.Pending
	date := now.UTC().Format("2006-01-02")
	taskID := "task-" + pending.RunID
	if err := s.store.AppendHumanTask(taskID,
		fmt.Sprintf("Investigate routine %s: run %s abandoned after %d attempts (last failure: %s) -- watermark advanced, this work will not retry on its own (source: supervisor; added %s)", r.Name, pending.RunID, pending.Attempts, detail, date)); err != nil {
		r.Log().Warn("could not record the abandonment task in knowledge -- this log line is the only copy",
			"run_id", pending.RunID, "task_id", taskID, "error", err)
	}
	st.Watermark = pending.CoveredThrough
	st.Pending = nil
	if cooldown := st.RecordAbandonment(now); cooldown > 0 {
		if err := s.store.AppendEvent(fmt.Sprintf("%s supervisor: routine %s circuit breaker tripped after %d consecutive abandonments -- cooling down for %s, next success resets", date, r.Name, st.ConsecutiveAbandons, cooldown)); err != nil {
			r.Log().Warn("could not record the circuit breaker event in knowledge -- this log line is the only copy",
				"run_id", pending.RunID, "error", err)
		}
		r.Log().Error("circuit breaker tripped", "cooldown", cooldown, "run_id", pending.RunID)
	}
	r.Log().Error("run abandoned", "run_id", pending.RunID, "attempts", pending.Attempts)
}
