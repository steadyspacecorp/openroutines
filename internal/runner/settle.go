package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/knowledge"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/run"
)

// One attempt's settled, durable outcome.
type Settlement struct {
	Outcome         Outcome // downgraded to Crashed when staged knowledge was rejected
	Detail          string  // the failure description recorded; "" for clean completions
	EventsDiscarded bool    // staged events.md change discarded (teamwork: off)
	Commit          string  // settlement commit hash, "" when nothing changed
	// Names files a concurrently settled run also edited; the
	// staged competitor was quarantined for a person to resolve.
	Conflicts []knowledge.Conflict
}

// Makes one attempt's end durable in knowledge -- the single
// settlement path for manual and scheduled runs. A rejected import downgrades
// the outcome to Crashed. beforeCommit, when set, runs before the settlement
// commit so caller bookkeeping rides the same commit. detail overrides the derived
// failure description. A Canceled attempt gets only its run record and no
// commit of its own -- the same logical run retries.
func Settle(dir string, r *routine.Routine, workspace *AttemptWorkspace, result *AttemptResult, attempt Attempt, detail string, beforeCommit func(*Settlement)) (*Settlement, error) {
	store := knowledge.NewStore(dir)
	settlement := &Settlement{Outcome: result.Outcome, Detail: detail}
	if result.Outcome == Completed {
		eventsDiscarded, conflicts, err := importKnowledge(dir, r, workspace)
		if err != nil {
			settlement.Outcome = Crashed
			settlement.Detail = "knowledge rejected: " + err.Error()
		} else {
			settlement.EventsDiscarded = eventsDiscarded
			settlement.Conflicts = conflicts
			advanceConsumer(dir, r, workspace, attempt.RunID)
		}
	} else if settlement.Detail == "" && result.Outcome != Canceled {
		settlement.Detail = fmt.Sprintf("%s after %s (exit %d)", result.Outcome, result.Duration, result.ExitCode)
		if result.Hint != "" {
			settlement.Detail += " -- " + result.Hint
		}
	}
	if settlement.Outcome != Completed && settlement.Outcome != Canceled {
		if err := store.AppendEvent(fmt.Sprintf("%s supervisor: routine %s (%s %s) %s", datestamp(), r.Name, attempt.RunID, attempt.ID(), settlement.Detail)); err != nil {
			r.Log().Warn("could not record the failure event -- this log line is the only copy", "run_id", attempt.RunID, "error", err)
		}
	}
	record := *result
	record.Outcome = settlement.Outcome
	if err := store.AppendRunRecord(recordJSON(r, attempt, &record)); err != nil {
		return settlement, err
	}
	if beforeCommit != nil {
		beforeCommit(settlement)
	}
	if settlement.Outcome == Canceled {
		return settlement, nil
	}
	commit, err := store.Commit(fmt.Sprintf("Run %s (%s): %s", r.Name, attempt.RunID, settlement.Outcome))
	if err != nil {
		return settlement, err
	}
	settlement.Commit = commit
	return settlement, nil
}

// Applies routine-level policy, then imports the staged tree:
// teamwork: off discards a staged events.md change, the rest imports
// normally. Reports whether such a change was discarded.
func importKnowledge(dir string, r *routine.Routine, workspace *AttemptWorkspace) (eventsDiscarded bool, conflicts []knowledge.Conflict, err error) {
	store := knowledge.NewStore(dir)
	if !r.Frontmatter.RecordsEvents() {
		if eventsDiscarded, err = knowledge.RestoreFile(workspace.KnowledgeDir, workspace.BaseDir, "events.md"); err != nil {
			return false, nil, err
		}
	}
	conflicts, err = store.Import(workspace.KnowledgeDir, workspace.BaseDir)
	return eventsDiscarded, conflicts, err
}

// Fixes the delivery boundary at the knowledge branch's
// current commit and renders the change set since the routine's cursor into
// the workspace. No cursor means first run: nothing to replay.
func prepareChanges(dir, workspace, consumer string) (string, bool, error) {
	store := knowledge.NewStore(dir)
	through, err := store.Head()
	if err != nil {
		return "", false, err
	}
	cursor, err := store.LoadCursor(consumer)
	if err != nil {
		return "", false, err
	}
	firstRun := cursor == nil
	from := ""
	var changes []knowledge.CommitChange
	if cursor != nil {
		from = cursor.ConsumedThrough
		if changes, err = store.Changes(from, through); err != nil {
			if errors.Is(err, knowledge.ErrCursorUnreachable) {
				return "", false, fmt.Errorf("%w: %w -- repair or delete %s on the knowledge branch", ErrFatal, err, knowledge.CursorFile(consumer))
			}
			return "", false, err
		}
	}
	rendered := knowledge.RenderChanges(consumer, from, through, changes)
	return through, firstRun, os.WriteFile(filepath.Join(workspace, knowledge.ChangesFileName), []byte(rendered), 0o644)
}

// Moves a reporting routine's cursor after a successful
// import, before the completion commit, so consumption and results land
// together. Exception to the marker rule: a successful first run's change set
// is empty by construction, so completion establishes the starting cursor.
func advanceConsumer(dir string, r *routine.Routine, workspace *AttemptWorkspace, runID string) {
	if !r.Frontmatter.Reports || workspace.Delivery.Through == "" || (!workspace.Delivery.FirstRun && !workspace.Consumed()) {
		return
	}
	if err := knowledge.NewStore(dir).SaveCursor(r.Name, knowledge.Cursor{
		ConsumedThrough: workspace.Delivery.Through,
		ByRun:           runID,
		At:              time.Now().UTC(),
	}); err != nil {
		r.Log().Error("cursor not advanced -- this change set will be delivered again", "run_id", runID, "through", workspace.Delivery.Through, "error", err)
	}
}

// Formats one run record line for runs.jsonl. Usage fields are
// per attempt (spend happens per attempt; retries would double-count a
// run-level figure) and absent means the runtime didn't report, never zero.
func recordJSON(r *routine.Routine, attempt Attempt, result *AttemptResult) string {
	record := run.Record{
		RunID: attempt.RunID, Routine: r.Name, Attempt: attempt.Number, Outcome: string(result.Outcome),
		RecordedAt: timestamp(), DurationMS: result.Duration.Milliseconds(), ExitCode: result.ExitCode,
		ScheduledFor: formatAttemptTime(attempt.ScheduledFor), CoveredThrough: formatAttemptTime(attempt.CoveredThrough), Manual: attempt.Manual(),
		Model: result.Model, Effort: result.Effort, Hint: result.Hint, Tokens: result.Usage,
	}
	if result.Usage != nil {
		record.CostReported = result.Usage.CostReported
	}
	return record.JSON()
}

func timestamp() string { return time.Now().UTC().Format(time.RFC3339) }

func datestamp() string { return time.Now().UTC().Format("2006-01-02") }
