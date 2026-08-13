package runner

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/knowledge"
	"github.com/steadyspacecorp/openroutines/internal/routine"
)

// Classifies how an attempt ended.
type Outcome string

// The terminal outcomes an attempt reports.
const (
	Completed Outcome = "completed"
	Timeout   Outcome = "timeout"
	Crashed   Outcome = "crashed"
	Canceled  Outcome = "canceled" // shutdown interrupted the attempt
)

// Identifies one execution of a logical run.
type Attempt struct {
	RunID          string
	Number         int
	ScheduledFor   time.Time
	CoveredThrough time.Time
	Rehearsal      string // fixture path; set only for manual rehearsal runs
	SnapshotDir    string // immutable knowledge tree supplied by a read-only CLI view
	ReadOnly       bool   // deny acting and writing tools; no settlement path exists
}

// Formats the attempt number for logs and environments.
func (a Attempt) ID() string { return fmt.Sprintf("attempt_%02d", a.Number) }

// Reports whether the attempt was dispatched outside the scheduler.
func (a Attempt) Manual() bool { return a.ScheduledFor.IsZero() }

// One attempt's outcome. Hint, when set, classifies a common
// failure (currently: provider authentication) so it surfaces as a
// configuration problem instead of an opaque crash.
type AttemptResult struct {
	Outcome  Outcome
	ExitCode int
	Duration time.Duration
	Hint     string
	Model    string // the resolved model this attempt ran with
	Effort   string // frontmatter reasoning effort, when set
	Usage    *Usage // token consumption; nil when the surface was unavailable
}

// Marks a start failure no retry can fix; a caller spending a retry
// budget should give up now. The runner classifies because it assembled the
// run; the supervisor only asks.
var ErrFatal = errors.New("not retryable")

// Marks a workspace that was not proven discarded.
var ErrAttemptCleanup = errors.New("attempt workspace cleanup failed")

// Fixes the knowledge range prepared for a reporting routine.
type DeliveryBoundary struct {
	Through  string
	FirstRun bool
}

// The attempt's staged knowledge, awaiting import or discard.
type AttemptWorkspace struct {
	KnowledgeDir string
	// The pristine snapshot the run started from, outside the
	// run's reach; the import diffs staged knowledge against it so
	// concurrent settlements compose.
	BaseDir string
	root    string

	Delivery DeliveryBoundary
}

// Discards the whole run workspace, staging and base included. A run keeps the
// supervisor's uid, so this process owns everything the model created and can
// restore any modes it changed.
func (s *AttemptWorkspace) Cleanup() error {
	if err := removeTree(s.root); err != nil {
		return fmt.Errorf("%w: remove %s: %w", ErrAttemptCleanup, s.root, err)
	}
	if s.BaseDir != "" {
		if err := removeTree(s.BaseDir); err != nil {
			slog.Warn("could not remove the attempt's knowledge base snapshot", "path", s.BaseDir, "error", err)
		}
	}
	return nil
}

// Deletes a tree a run may have chmodded shut behind itself. Owning every file
// means the supervisor is *allowed* to restore the modes it needs, not that
// os.RemoveAll does: a 0000 directory stops its walk with EACCES. One retry
// pass is enough because WalkDir visits a directory before reading it.
func removeTree(path string) error {
	if err := os.RemoveAll(path); err == nil {
		return nil
	}
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(path)
}

// Reports whether the routine created the consume marker. The staged
// knowledge directory is canonical -- writable to the run in every isolation
// shape; the workspace root is still accepted for unconfined runs.
func (s *AttemptWorkspace) Consumed() bool {
	if _, err := os.Stat(filepath.Join(s.KnowledgeDir, knowledge.ConsumeMarker)); err == nil {
		return true
	}
	_, err := os.Stat(filepath.Join(s.root, knowledge.ConsumeMarker))
	return err == nil
}

// A completed manual run.
type ManualResult struct {
	RunID     string
	Outcome   Outcome
	ExitCode  int
	Duration  time.Duration
	Commit    string               // knowledge commit hash, when one was made
	Hint      string               // classified failure cause, when one was recognized
	Conflicts []knowledge.Conflict // semantic edits preserved outside the canonical file
}

type ManualOptions struct {
	DiscardKnowledge bool
	Rehearse         bool
	Fixture          string
}

// Resolves frontmatter over agent defaults.
func EffectiveModel(agent *config.Agent, r *routine.Routine) (string, error) {
	model := r.Frontmatter.Model
	if model == "" {
		model = agent.Defaults.Model
	}
	if model == "" || strings.Contains(model, "{{") {
		return "", fmt.Errorf("no model: set model in frontmatter or defaults.model in openroutines.yml (openroutines configure)")
	}
	return model, nil
}

// The declared timeout capped by the agent's max_timeout
// ceiling -- applied here, not in `check`: a spend guard cannot rest on a
// command the operator may never run.
func EffectiveTimeout(agent *config.Agent, r *routine.Routine) time.Duration {
	return min(DeclaredTimeout(agent, r), agent.MaxRunTimeout())
}

// Resolves frontmatter over agent defaults over 5m, before the
// ceiling applies. `check` reports on it; execution uses EffectiveTimeout.
func DeclaredTimeout(agent *config.Agent, r *routine.Routine) time.Duration {
	timeout, _ := declaredTimeout(agent, r)
	return timeout
}

// Also reports the raw value that failed to parse, "" when
// every declared value parsed clean, so Stage can warn about what it dropped.
func declaredTimeout(agent *config.Agent, r *routine.Routine) (timeout time.Duration, badValue string) {
	timeout = 5 * time.Minute
	for _, t := range []string{agent.Defaults.Timeout, r.Frontmatter.Timeout} {
		if t == "" {
			continue
		}
		if d, err := time.ParseDuration(t); err == nil {
			timeout = d
		} else {
			badValue = t
		}
	}
	return timeout, badValue
}
