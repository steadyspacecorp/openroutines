package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const checkAgentYAML = `name: test-agent
description: Tests check
owner:
  name: CI
  email: ci@example.invalid
timezone: UTC
defaults:
  model: fake/model
`

// checkOutput runs check against dir and returns everything it printed.
func checkOutput(t *testing.T, dir string) string {
	t.Helper()
	t.Chdir(dir)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	cmdCheck(nil)
	os.Stdout = stdout
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// A run may not outlast the single-instance lease. The runner enforces that;
// check is where an operator learns their setting will be cut down, before a
// routine is quietly killed at the ceiling in production.
func TestCheckWarnsOnTimeoutsTheLeaseCannotCover(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "openroutines.yml"), []byte(checkAgentYAML), 0o644)
	os.MkdirAll(filepath.Join(dir, "routines"), 0o755)
	os.WriteFile(filepath.Join(dir, "routines", "marathon.md"), []byte(
		"---\nschedule: \"0 9 * * *\"\ntimeout: 90m\n---\nTake ages.\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "routines", "sprint.md"), []byte(
		"---\nschedule: \"0 9 * * *\"\ntimeout: 10m\n---\nBe quick.\n"), 0o644)

	out := checkOutput(t, dir)
	if !strings.Contains(out, "marathon: timeout 1h30m0s exceeds the 15m0s a single run may take") ||
		!strings.Contains(out, "capped at 15m0s") {
		t.Fatalf("expected a lease-ceiling warning for marathon:\n%s", out)
	}
	if strings.Contains(out, "sprint: timeout") {
		t.Fatalf("a 10m timeout fits inside the lease and must not warn:\n%s", out)
	}
}

// The scaffolded opencode.json carries the baseline permission policy; an
// agent repo that lost it still runs, so check is where the loss surfaces.
func TestCheckWarnsOnMissingOpencodeJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "openroutines.yml"), []byte(checkAgentYAML), 0o644)
	os.MkdirAll(filepath.Join(dir, "routines"), 0o755)

	out := checkOutput(t, dir)
	if !strings.Contains(out, "opencode.json is missing") {
		t.Fatalf("expected a missing-opencode.json warning:\n%s", out)
	}

	os.WriteFile(filepath.Join(dir, "opencode.json"), []byte("{}\n"), 0o644)
	out = checkOutput(t, dir)
	if strings.Contains(out, "opencode.json is missing") {
		t.Fatalf("a present opencode.json must not warn:\n%s", out)
	}
}
