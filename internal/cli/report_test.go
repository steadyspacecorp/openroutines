package cli

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/memory"
)

func memoryAgent(t *testing.T) string {
	t.Helper()
	dir := statusAgent(t, nil)
	memoryGit(t, dir, "init", "-q")
	memoryGit(t, dir, "config", "user.name", "Test")
	memoryGit(t, dir, "config", "user.email", "test@example.invalid")
	memoryGit(t, dir, "add", "openroutines.yml")
	memoryGit(t, dir, "commit", "-qm", "agent")
	if err := memory.At(dir).Ensure(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestReportDeclineRunsNothing(t *testing.T) {
	dir := memoryAgent(t)
	var code int
	var out string
	withStdin(t, "n\n", func() {
		out = capture(t, dir, func() { code = cmdReport(nil) })
	})
	if code != 0 {
		t.Fatalf("report returned %d", code)
	}
	if !strings.Contains(out, "Memory is local only") {
		t.Fatalf("report did not report sync state:\n%s", out)
	}
	if !strings.Contains(out, "Not running it") {
		t.Fatalf("declining did not stop the run:\n%s", out)
	}
}

// Confirming hands off to the routine runner at log level warn. The agent has
// no check-in routine, so the wiring is observable in the failure -- without
// spending a model run.
func TestReportConfirmationRunsCheckIn(t *testing.T) {
	dir := memoryAgent(t)
	t.Setenv(config.EnvLogLevel, "")
	var code int
	withStdin(t, "y\n", func() {
		capture(t, dir, func() { code = cmdReport(nil) })
	})
	if code == 0 {
		t.Fatal("report reported success with no check-in routine")
	}
	if got := os.Getenv(config.EnvLogLevel); got != "warn" {
		t.Fatalf("check-in run level = %q, want warn", got)
	}
}

// An operator's explicit level override outranks the command's warn default.
func TestReportKeepsOperatorLogLevel(t *testing.T) {
	dir := memoryAgent(t)
	t.Setenv(config.EnvLogLevel, "debug")
	withStdin(t, "y\n", func() {
		capture(t, dir, func() { cmdReport(nil) })
	})
	if got := os.Getenv(config.EnvLogLevel); got != "debug" {
		t.Fatalf("report clobbered the operator's level: %q", got)
	}
}

func TestConfirmedAcceptsOnlyExplicitYes(t *testing.T) {
	for input, want := range map[string]bool{
		"y\n": true, "YES\n": true, "y": true,
		"n\n": false, "\n": false, "": false, "yeah\n": false,
	} {
		if got := confirmed(strings.NewReader(input)); got != want {
			t.Errorf("confirmed(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestMemoryCommandRetired(t *testing.T) {
	if commands["report"] == nil {
		t.Fatal("report command is not registered")
	}
	for _, retired := range []string{"memory", "teamwork"} {
		if commands[retired] != nil {
			t.Fatalf("the %s command was replaced by report and should be gone", retired)
		}
	}
}

func memoryGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
