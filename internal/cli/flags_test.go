package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/creds"
)

// An unknown flag must fail the command outright, never run it as if the
// flag had been accepted -- this is the difference between a typo that
// errors and one that silently does nothing (or, for configure, silently
// writes state).
func TestUnknownFlagIsRejected(t *testing.T) {
	dir := statusAgent(t, nil)
	for _, tc := range []struct {
		name string
		run  func([]string) int
	}{
		{"status", cmdStatus},
		{"check", cmdCheck},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(dir)
			if code := tc.run([]string{"--bogus"}); code == 0 {
				t.Fatalf("%s --bogus: expected a nonzero exit, got 0", tc.name)
			}
		})
	}
}

// --help must short-circuit before the command does anything, wherever it
// appears in the arguments -- `check --help` shows usage, it does not run
// the check.
func TestHelpFlagShowsUsageWithoutRunning(t *testing.T) {
	dir := statusAgent(t, nil)
	t.Chdir(dir)

	out := capture(t, dir, func() { cmdCheck([]string{"--help"}) })
	if strings.Contains(out, "check passed") || strings.Contains(out, "check failed") {
		t.Fatalf("check --help must not run the check:\n%s", out)
	}
	if !strings.Contains(out, "usage") {
		t.Fatalf("check --help should print usage:\n%s", out)
	}
}

// configure must refuse to run against a non-interactive stdin unless
// --yes is given explicitly -- accepting EOF-as-default silently generated
// a master key and wrote credentials against an unfamiliar flag like
// --help (issue #67).
func TestConfigureRefusesNonInteractiveWithoutYes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openroutines.yml"), []byte(statusAgentYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	stdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.Close() // EOF immediately, and not a terminal
	os.Stdin = r
	defer func() { os.Stdin = stdin }()

	if code := cmdConfigure(nil); code == 0 {
		t.Fatalf("configure on non-interactive stdin without --yes should fail, got exit 0")
	}
	if _, err := os.Stat(filepath.Join(dir, creds.KeyFileName)); err == nil {
		t.Fatalf("configure must not generate a master key when it refuses to run")
	}
}

func TestConfigureReportsGeneratedMasterKeyWithoutDeploymentInstructions(t *testing.T) {
	dir := statusAgent(t, nil)

	stdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	os.Stdin = r
	defer func() { os.Stdin = stdin }()

	out := capture(t, dir, func() {
		if code := cmdConfigure([]string{"--yes"}); code != 0 {
			t.Fatalf("configure exited %d", code)
		}
	})
	if !strings.Contains(out, "Generated master.key\n") {
		t.Fatalf("configure did not report the generated key plainly:\n%s", out)
	}
	if strings.Contains(out, "mount") || strings.Contains(out, creds.EnvMasterKeyFile) {
		t.Fatalf("configure included deployment instructions in the generated-key message:\n%s", out)
	}
}
