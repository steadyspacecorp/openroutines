package runner

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/knowledge"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/sandbox"
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
	AttemptUID     uint32 // production-only identity, from the supervisor's pool or the manual-run reservation
	Rehearsal      string // fixture path; set only for manual rehearsal runs
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

// Marks a workspace that was not proven discarded. The
// supervisor must poison the attempt identity instead of returning it to the
// pool, or its next assignee could read the leftover tree.
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
	// Set when the workspace was prepared for an attempt
	// identity: Cleanup may then need that identity's help to reclaim
	// paths the model process chmodded away from the group.
	attemptUID uint32

	Delivery DeliveryBoundary
}

// Discards the whole run workspace, staging and base included. A
// model process can chmod its own files away from the group, so removal may
// need the attempt identity's own help (see removeAttemptTree).
func (s *AttemptWorkspace) Cleanup() error {
	if s.attemptUID != 0 {
		// Kill anything still carrying the identity first: an escaped
		// descendant could otherwise race the removal.
		if err := sandbox.ReapIdentity(s.attemptUID); err != nil {
			return fmt.Errorf("%w: reap uid %d before removal: %w", ErrAttemptCleanup, s.attemptUID, err)
		}
	}
	if err := removeAttemptTree(s.attemptUID, s.root); err != nil {
		return fmt.Errorf("%w: remove %s: %w", ErrAttemptCleanup, s.root, err)
	}
	if s.BaseDir != "" {
		if err := os.RemoveAll(s.BaseDir); err != nil {
			slog.Warn("could not remove the attempt's knowledge base snapshot", "path", s.BaseDir, "error", err)
		}
	}
	return nil
}

// Reports whether the routine created the consume marker. The staged
// knowledge directory is canonical (the one sandbox-writable workspace path);
// the workspace root is still accepted for unsandboxed runs.
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
