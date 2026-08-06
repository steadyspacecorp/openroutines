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

func TestTeamworkDeclineLeavesFeedAlone(t *testing.T) {
	dir := memoryAgent(t)
	var code int
	var out string
	withStdin(t, "n\n", func() {
		out = capture(t, dir, func() { code = cmdTeamwork(nil) })
	})
	if code != 0 {
		t.Fatalf("teamwork returned %d", code)
	}
	if !strings.Contains(out, "Memory is local only") {
		t.Fatalf("teamwork did not report sync state:\n%s", out)
	}
	if !strings.Contains(out, "pending changes remain") {
		t.Fatalf("declining did not leave the feed alone:\n%s", out)
	}
}

// Confirming hands off to the routine runner at log level warn. The agent has
// no check-in routine, so the wiring is observable in the failure -- without
// spending a model run.
func TestTeamworkConfirmationRunsCheckIn(t *testing.T) {
	dir := memoryAgent(t)
	t.Setenv(config.EnvLogLevel, "")
	var code int
	withStdin(t, "y\n", func() {
		capture(t, dir, func() { code = cmdTeamwork(nil) })
	})
	if code == 0 {
		t.Fatal("teamwork reported success with no check-in routine")
	}
	if got := os.Getenv(config.EnvLogLevel); got != "warn" {
		t.Fatalf("check-in run level = %q, want warn", got)
	}
}

// An operator's explicit level override outranks the command's warn default.
func TestTeamworkKeepsOperatorLogLevel(t *testing.T) {
	dir := memoryAgent(t)
	t.Setenv(config.EnvLogLevel, "debug")
	withStdin(t, "y\n", func() {
		capture(t, dir, func() { cmdTeamwork(nil) })
	})
	if got := os.Getenv(config.EnvLogLevel); got != "debug" {
		t.Fatalf("teamwork clobbered the operator's level: %q", got)
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
	if commands["teamwork"] == nil {
		t.Fatal("teamwork command is not registered")
	}
	if commands["memory"] != nil {
		t.Fatal("the memory command was replaced by teamwork and should be gone")
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
