package runner

import (
	"errors"
	"fmt"
	"github.com/steadyspacecorp/openroutines/internal/knowledge"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/run"
	"os"
	"path/filepath"
	"time"
)

// Settlement is one attempt's settled, durable outcome.
type Settlement struct {
	Outcome   Outcome // downgraded to Crashed when staged knowledge was rejected
	Detail    string  // the failure description recorded; "" for clean completions
	Discarded bool    // staged events.md change discarded (teamwork: off)
	Commit    string  // settlement commit hash, "" when nothing changed
	// Conflicted names files a concurrently settled run also edited; the
	// staged competitor was quarantined for a person to resolve.
	Conflicted []knowledge.Conflict
}

// Settle makes one attempt's end durable in knowledge -- the single
// settlement path for manual and scheduled runs. A rejected import downgrades
// the outcome to Crashed. stage, when set, runs before the settlement commit
// so caller bookkeeping rides the same commit. detail overrides the derived
// failure description. A Canceled attempt gets only its run record and no
// commit of its own -- the same logical run retries.
func Settle(dir string, r *routine.Routine, staging *AttemptWorkspace, res *AttemptResult, meta Attempt, detail string, stage func(*Settlement)) (*Settlement, error) {
	store := knowledge.NewStore(dir)
	s := &Settlement{Outcome: res.Outcome, Detail: detail}
	if res.Outcome == Completed {
		discarded, conflicted, err := importKnowledge(dir, r, staging)
		if err != nil {
			s.Outcome = Crashed
			s.Detail = "knowledge rejected: " + err.Error()
		} else {
			s.Discarded = discarded
			s.Conflicted = conflicted
			advanceConsumer(dir, r, staging, meta.RunID)
		}
	} else if s.Detail == "" && res.Outcome != Canceled {
		s.Detail = fmt.Sprintf("%s after %s (exit %d)", res.Outcome, res.Duration, res.ExitCode)
		if res.Hint != "" {
			s.Detail += " -- " + res.Hint
		}
	}
	if s.Outcome != Completed && s.Outcome != Canceled {
		if err := store.AppendEvent(fmt.Sprintf("%s supervisor: routine %s (%s %s) %s", datestamp(), r.Name, meta.RunID, meta.ID(), s.Detail)); err != nil {
			r.Log().Warn("could not record the failure event -- this log line is the only copy", "run_id", meta.RunID, "error", err)
		}
	}
	if stage != nil {
		stage(s)
	}
	rec := *res
	rec.Outcome = s.Outcome
	if err := store.AppendRunRecord(recordJSON(r, meta, &rec)); err != nil {
		return s, err
	}
	if s.Outcome == Canceled {
		return s, nil
	}
	commit, err := store.Commit(fmt.Sprintf("Run %s (%s): %s", r.Name, meta.RunID, s.Outcome))
	if err != nil {
		return s, err
	}
	s.Commit = commit
	return s, nil
}

// importKnowledge applies routine-level policy, then imports the staged tree:
// teamwork: off discards a staged events.md change, the rest imports
// normally. Reports whether such a change was discarded.
func importKnowledge(dir string, r *routine.Routine, staging *AttemptWorkspace) (discarded bool, conflicted []knowledge.Conflict, err error) {
	store := knowledge.NewStore(dir)
	if !r.Frontmatter.RecordsEvents() {
		if discarded, err = knowledge.RestoreFile(staging.KnowledgeDir, staging.BaseDir, "events.md"); err != nil {
			return false, nil, err
		}
	}
	conflicted, err = store.Import(staging.KnowledgeDir, staging.BaseDir)
	return discarded, conflicted, err
}

// prepareChanges fixes the delivery boundary at the knowledge branch's
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

// advanceConsumer moves a reporting routine's cursor after a successful
// import, before the completion commit, so consumption and results land
// together. Exception to the marker rule: a successful first run's change set
// is empty by construction, so completion establishes the starting cursor.
func advanceConsumer(dir string, r *routine.Routine, staging *AttemptWorkspace, runID string) {
	if !r.Frontmatter.Reports || staging.ConsumerThrough == "" || (!staging.ConsumerFirstRun && !staging.Consumed()) {
		return
	}
	if err := knowledge.NewStore(dir).SaveCursor(r.Name, knowledge.Cursor{
		ConsumedThrough: staging.ConsumerThrough,
		ByRun:           runID,
		At:              time.Now().UTC(),
	}); err != nil {
		r.Log().Error("cursor not advanced -- this change set will be delivered again", "run_id", runID, "through", staging.ConsumerThrough, "error", err)
	}
}

// recordJSON formats one run record line for runs.jsonl. Usage fields are
// per attempt (spend happens per attempt; retries would double-count a
// run-level figure) and absent means the runtime didn't report, never zero.
func recordJSON(r *routine.Routine, meta Attempt, res *AttemptResult) string {
	record := run.Record{
		RunID: meta.RunID, Routine: r.Name, Attempt: meta.Number, Outcome: string(res.Outcome),
		RecordedAt: timestamp(), DurationMS: res.Duration.Milliseconds(), ExitCode: res.ExitCode,
		ScheduledFor: formatAttemptTime(meta.ScheduledFor), CoveredThrough: formatAttemptTime(meta.CoveredThrough), Manual: meta.Manual(),
		Model: res.Model, Effort: res.Effort, Hint: res.Hint, Tokens: res.Usage,
	}
	if res.Usage != nil {
		record.CostReported = res.Usage.CostReported
	}
	return record.JSON()
}

func timestamp() string { return time.Now().UTC().Format(time.RFC3339) }

// datestamp is the YYYY-MM-DD prefix event entries carry.
func datestamp() string { return time.Now().UTC().Format("2006-01-02") }
