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

type Outcome string

const (
	Completed Outcome = "completed"
	Timeout   Outcome = "timeout"
	Crashed   Outcome = "crashed"
	Canceled  Outcome = "canceled"
)

type Attempt struct {
	RunID          string
	Number         int
	ScheduledFor   time.Time
	CoveredThrough time.Time
	Rehearsal      string
	SnapshotDir    string
	ReadOnly       bool
}

func (a Attempt) ID() string { return fmt.Sprintf("attempt_%02d", a.Number) }

func (a Attempt) Manual() bool { return a.ScheduledFor.IsZero() }

type AttemptResult struct {
	Outcome  Outcome
	ExitCode int
	Duration time.Duration
	Hint     string
	Model    string
	Effort   string
	Usage    *Usage
}

var ErrFatal = errors.New("not retryable")

var ErrAttemptCleanup = errors.New("attempt workspace cleanup failed")

type DeliveryBoundary struct {
	Through  string
	FirstRun bool
}

type AttemptWorkspace struct {
	KnowledgeDir string
	BaseDir      string
	root         string

	Delivery DeliveryBoundary
}

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

func removeTree(path string) error {
	// chmod makes model-created 000 directories traversable when RemoveAll hits EACCES.
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

func (s *AttemptWorkspace) Consumed() bool {
	_, err := os.Stat(filepath.Join(s.KnowledgeDir, knowledge.ConsumeMarker))
	return err == nil
}

type ManualResult struct {
	RunID     string
	Outcome   Outcome
	ExitCode  int
	Duration  time.Duration
	Commit    string
	Hint      string
	Conflicts []knowledge.Conflict
}

type ManualOptions struct {
	DiscardKnowledge bool
	Rehearse         bool
	Fixture          string
}

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

// The declared timeout capped by the agent's max_timeout ceiling -- applied
// here, not in check: a spend guard cannot rest on a command the operator may
// never run.
func EffectiveTimeout(agent *config.Agent, r *routine.Routine) time.Duration {
	return min(DeclaredTimeout(agent, r), agent.MaxRunTimeout())
}

func DeclaredTimeout(agent *config.Agent, r *routine.Routine) time.Duration {
	timeout, _ := declaredTimeout(agent, r)
	return timeout
}

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
