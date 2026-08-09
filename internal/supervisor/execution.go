package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/runner"
	"github.com/steadyspacecorp/openroutines/internal/schedule"
)

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
	pending := st.Pending
	s.knowledgeMu.Lock()
	giveBack := reserve(pending, now)
	if err := st.Save(s.stateDir()); err != nil {
		s.knowledgeMu.Unlock()
		log.Error("saving scheduling state failed", "error", err)
		return
	}
	if !s.commitIntent(fmt.Sprintf("Reserve %s attempt %d (%s)", r.Name, pending.Attempts, pending.RunID)) {
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
	attempt := runner.Attempt{
		RunID:          pending.RunID,
		Number:         pending.Attempts,
		ScheduledFor:   pending.ScheduledFor,
		CoveredThrough: pending.CoveredThrough,
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
	prepared, err := runner.Stage(s.Dir, agent, r, attempt, s.knowledgeMu)
	if errors.Is(err, runner.ErrAttemptCleanup) {
		cleanupErr = err
	}
	if err == nil && !s.noOrigin && !s.renewLease() {
		cleanupErr = prepared.Discard()
		s.knowledgeMu.Lock()
		giveBack()
		if err := st.Save(s.stateDir()); err != nil {
			log.Error("saving scheduling state failed", "error", err)
		}
		s.knowledgeMu.Unlock()
		log.Warn("not started -- lease lost after staging; the current holder will retry it")
		return
	}

	log.Info("attempt starting", "attempt_id", attempt.ID(), "scheduled_for", attempt.ScheduledFor,
		"timeout", runner.EffectiveTimeout(agent, r))

	var result *runner.AttemptResult
	var workspace *runner.AttemptWorkspace
	if err == nil {
		result, workspace, err = prepared.Run(runCtx)
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
		result = &runner.AttemptResult{Outcome: runner.Crashed, ExitCode: -1}
		detail = err.Error()
	} else {
		defer func() { cleanupErr = errors.Join(cleanupErr, workspace.Cleanup()) }()
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
	settlement, settlementErr := runner.Settle(s.Dir, r, workspace, result, attempt, detail, func(settled *runner.Settlement) {
		switch {
		case settled.Outcome == runner.Canceled:
			giveBack()
		case settled.Outcome == runner.Completed:
			st.Watermark = pending.CoveredThrough
			st.Pending = nil
			st.RecordSuccess()
		case fatal, pending.Attempts >= MaxAttempts:
			abandoned = true
			s.abandon(r, st, settled.Detail, result.SessionsDir, now)
		}
		if err := st.Save(s.stateDir()); err != nil {
			log.Error("saving scheduling state failed", "error", err)
		}
	})
	if settlementErr != nil {
		log.Error("settlement failed -- run remains pending and will retry", "error", settlementErr)
		return
	}
	if settlement.EventsDiscarded {
		log.Info("discarded staged events.md change (teamwork: off)")
	}
	conflictsCommitted := s.recordKnowledgeConflicts(r, pending, settlement, log)
	shouldPush := reportSettlement(ctx, settlement, result, abandoned, log)
	if !shouldPush || !conflictsCommitted {
		return
	}
	s.pushBestEffort()
	return
}

func (s *Supervisor) recordKnowledgeConflicts(r *routine.Routine, pending *schedule.Pending, settlement *runner.Settlement, log *slog.Logger) bool {
	for i, conflict := range settlement.Conflicts {
		taskID := fmt.Sprintf("task-%s-knowledge-conflict-%d", pending.RunID, i+1)
		if err := s.store.AppendHumanTask(taskID,
			fmt.Sprintf("Resolve concurrent knowledge edit from routine %s run %s: canonical %s was left unchanged; competing version saved at %s", r.Name, pending.RunID, conflict.Path, conflict.Quarantine)); err != nil {
			log.Warn("could not record the knowledge conflict task in knowledge -- this log line is the only copy",
				"path", conflict.Path, "task_id", taskID, "error", err)
		}
		log.Warn("concurrent knowledge edit quarantined -- canonical knowledge left unchanged",
			"path", conflict.Path, "quarantine", conflict.Quarantine)
	}
	if len(settlement.Conflicts) == 0 {
		return true
	}
	if _, err := s.store.Commit(fmt.Sprintf("Record %s knowledge conflicts", pending.RunID)); err != nil {
		log.Error("conflict task commit failed", "error", err)
		return false
	}
	return true
}

func reportSettlement(ctx context.Context, settlement *runner.Settlement, result *runner.AttemptResult, abandoned bool, log *slog.Logger) bool {
	switch {
	case settlement.Outcome == runner.Canceled:
		if ctx.Err() != nil {
			log.Info("interrupted by shutdown -- will retry on next boot")
		} else {
			log.Warn("canceled -- lease lost mid-run; whoever holds the lease retries it")
		}
		return false // shutdown's final commit carries the record, and a lease loser must not push
	case settlement.Outcome == runner.Completed:
		log.Info("run completed", withSessions(result.SessionsDir, "duration", result.Duration)...)
	case abandoned:
		// abandon() already said so.
	default:
		log.Error("attempt failed -- will retry", withSessions(result.SessionsDir, "detail", settlement.Detail)...)
	}
	return true
}

// withSessions names the attempt's exported sessions only when there are some.
func withSessions(sessionsDir string, args ...any) []any {
	if sessionsDir != "" {
		args = append(args, "sessions", sessionsDir)
	}
	return args
}
