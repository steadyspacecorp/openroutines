package runner

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/routine"
)

// A native-mode attempt whose opencode is a shell script. The script's
// descendants must not hold the run's output pipes, or Wait blocks on the
// drain deadline instead of the leader's exit.
func stageFakeRun(t *testing.T, timeout, script string) *PreparedAttempt {
	t.Helper()
	dir := t.TempDir()
	agent := "name: Test\ninstructions: Test agent\nowner:\n  email: test@example.com\ntimezone: UTC\ndefaults:\n  model: test/model\n"
	if err := os.WriteFile(filepath.Join(dir, "openroutines.yml"), []byte(agent), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	fakeBin(t, "opencode", "#!/bin/sh\nif [ \"$1\" = session ]; then printf '[]'; exit 0; fi\n"+script)
	t.Setenv("OPENROUTINES_NATIVE", "1")
	r := &routine.Routine{Name: "stuck", Frontmatter: routine.Frontmatter{Timeout: timeout, Teamwork: routine.TeamworkOff}, Body: "hang"}
	prepared, err := Stage(dir, cfg, r, Attempt{RunID: "run_stuck", Number: 1, SnapshotDir: t.TempDir(), ReadOnly: true}, &sync.Mutex{})
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func waitForPid(t *testing.T, pidfile string) int {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if raw, err := os.ReadFile(pidfile); err == nil && len(raw) > 0 {
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatal("the run never reported its descendant's pid")
		}
	}
}

func TestTimedOutAttemptRecordsAKillAndSweepsAChildThatIgnoredSIGTERM(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	// The leader dies on SIGTERM; its child has TERM ignored and inherits
	// that into sleep, so only the post-kill sweep can end it.
	prepared := stageFakeRun(t, "1s", "sh -c 'trap \"\" TERM; sleep 60' </dev/null >/dev/null 2>&1 &\necho $! > "+pidfile+"\nwait\n")

	result, workspace, err := prepared.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Cleanup()
	if result.Outcome != Timeout || result.ExitCode != -1 {
		t.Fatalf("outcome = %s exit %d, want %s exit -1", result.Outcome, result.ExitCode, Timeout)
	}

	pid := waitForPid(t, pidfile)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("a child that ignored SIGTERM outlived the timed-out attempt")
		}
	}
}

func TestCanceledAttemptRecordsAKill(t *testing.T) {
	prepared := stageFakeRun(t, "1m", "sleep 60\n")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	result, workspace, err := prepared.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Cleanup()
	if result.Outcome != Canceled || result.ExitCode != -1 {
		t.Fatalf("outcome = %s exit %d, want %s exit -1", result.Outcome, result.ExitCode, Canceled)
	}
}
