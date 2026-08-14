package runner

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/routine"
)

func TestCleanupReportsWorkspaceRemovalFailure(t *testing.T) {
	staging := &AttemptWorkspace{root: "\x00"}
	if err := staging.Cleanup(); !errors.Is(err, ErrAttemptCleanup) {
		t.Fatalf("cleanup error = %v, want ErrAttemptCleanup", err)
	}
}

func TestCleanupRemovesWorkspaceWhoseModesTheRunChanged(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	inner := filepath.Join(workspace, "first", "second")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inner, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(inner, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(inner), 0); err != nil {
		t.Fatal(err)
	}

	staging := &AttemptWorkspace{root: workspace}
	if err := staging.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace still exists after cleanup: %v", err)
	}
}

// The ceiling is the agent's own max_timeout, applied where attempts read
// the timeout -- not left to a `check` the operator may never run. The
// declared value stays readable for `check` to warn about.
func TestTimeoutIsCappedAtTheAgentCeiling(t *testing.T) {
	agent := &config.Agent{Name: "a", Instructions: "d"}
	agent.Defaults.Timeout = "90m"
	marathon := &routine.Routine{Name: "marathon"}
	if got := DeclaredTimeout(agent, marathon); got != 90*time.Minute {
		t.Fatalf("declared timeout = %s, want 90m", got)
	}
	if got := EffectiveTimeout(agent, marathon); got != 90*time.Minute {
		t.Fatalf("effective timeout = %s, want 90m under the default ceiling", got)
	}

	agent.MaxTimeout = "1h"
	if got := EffectiveTimeout(agent, marathon); got != time.Hour {
		t.Fatalf("effective timeout = %s, want the 1h max_timeout ceiling", got)
	}

	agent.MaxTimeout = ""
	week := &routine.Routine{Name: "week", Frontmatter: routine.Frontmatter{Timeout: "168h"}}
	if got := EffectiveTimeout(agent, week); got != config.DefaultMaxTimeout {
		t.Fatalf("effective timeout = %s, want the %s default ceiling", got, config.DefaultMaxTimeout)
	}
}
