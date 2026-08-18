package cli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/logging"
	"github.com/steadyspacecorp/openroutines/internal/version"
)

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

func TestRunConfiguresLoggingFromEnvironment(t *testing.T) {
	t.Chdir(t.TempDir())
	if code := Run([]string{"new", "agent"}); code != 0 {
		t.Fatalf("new exit code = %d, want 0", code)
	}
	t.Chdir("agent")

	t.Setenv(logging.EnvLevel, "debug")
	t.Cleanup(func() { logging.Level.Set(slog.LevelInfo) })
	Run([]string{"routines", "list"})
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("dispatch did not install the environment's log level")
	}
}

func TestRunDoesNotWarnOnPinMismatchForCommandsThatHandleIt(t *testing.T) {
	wasVersion := version.Version
	version.Version = "v1.2.3"
	t.Cleanup(func() { version.Version = wasVersion })

	for _, command := range []string{"check", "update"} {
		t.Run(command, func(t *testing.T) {
			dir := t.TempDir()
			capture(t, dir, func() {
				if code := Run([]string{"new", "agent"}); code != 0 {
					t.Fatalf("new exit code = %d, want 0", code)
				}
			})
			agentDir := filepath.Join(dir, "agent")
			if err := os.WriteFile(filepath.Join(agentDir, ".openroutines", "version"), []byte("v1.2.2\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			var logs bytes.Buffer
			wasWriter := logging.Writer
			logging.Writer = &logs
			t.Cleanup(func() { logging.Writer = wasWriter })
			capture(t, agentDir, func() { Run([]string{command}) })
			if strings.Contains(logs.String(), "does not match") {
				t.Fatalf("%s logged a pin mismatch warning: %s", command, logs.String())
			}
		})
	}
}

func TestRunAllowsNewOutsideAnAgentRepo(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if code := Run([]string{"new", "new-agent"}); code != 0 {
		t.Fatalf("new exit code = %d, want 0", code)
	}
	if _, err := os.Stat("new-agent/openroutines.yml"); err != nil {
		t.Fatalf("new did not create an agent repo: %v", err)
	}
}

func TestPluginUsageErrorsExitTwo(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		stderr := os.Stderr
		stdout := os.Stdout
		os.Stderr = w
		os.Stdout = w
		code := cmdPlugin(args)
		os.Stderr = stderr
		os.Stdout = stdout
		w.Close()
		r.Close()
		if code != 2 {
			t.Fatalf("cmdPlugin(%q) exit code = %d, want 2", args, code)
		}
	}
}
