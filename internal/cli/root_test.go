package cli

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// Running any repo-scoped command from outside an agent repository must
// fail with an obvious "not an agent repository" error before the command
// gets a chance to run -- not surface as whatever the command happens to
// read first, e.g. credentials reporting "no master key" when the wrong
// working directory was the actual problem (#64).
func TestRunRequiresAgentRepoBeforeCommandLogic(t *testing.T) {
	t.Chdir(t.TempDir())

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := os.Stderr
	os.Stderr = w
	code := Run([]string{"credentials", "set", "foo"})
	os.Stderr = stderr
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(string(out), "not an agent repository") {
		t.Fatalf("stderr = %q, want it to mention \"not an agent repository\"", out)
	}
	if strings.Contains(string(out), "master key") {
		t.Fatalf("stderr = %q, leaked the credential-store error instead of the repo check", out)
	}
}

// The dispatch, not the command, installs the process logger: after any
// repo-bound command, slog's default is gated at the agent's configured
// level -- a new subcommand gets configured logging without ever hearing
// of logging.Setup.
func TestRunConfiguresLoggingFromAgentConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	if code := Run([]string{"scaffold", "agent"}); code != 0 {
		t.Fatalf("scaffold exit code = %d, want 0", code)
	}
	t.Chdir("agent")
	f, err := os.OpenFile("openroutines.yml", os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("log_level: debug\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	Run([]string{"routines", "list"})
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("dispatch did not install the agent's configured log level")
	}
}

func TestRunAllowsScaffoldOutsideAnAgentRepo(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if code := Run([]string{"scaffold", "new-agent"}); code != 0 {
		t.Fatalf("scaffold exit code = %d, want 0", code)
	}
	if _, err := os.Stat("new-agent/openroutines.yml"); err != nil {
		t.Fatalf("scaffold did not create an agent repo: %v", err)
	}
}
