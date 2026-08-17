package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/repository"
)

func TestScenarioRehearsalLimitsOutsideAccessButKeepsModelRunning(t *testing.T) {
	dir := t.TempDir()
	agent := `name: Test
timezone: UTC
defaults:
  model: fake/model
`
	if err := os.WriteFile(filepath.Join(dir, "openroutines.yml"), []byte(agent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "routines"), 0o755); err != nil {
		t.Fatal(err)
	}
	routine := `---
schedule: "0 9 * * *"
credentials: [routine_secret]
webfetch: true
websearch: true
---
Use the rehearsal data.
`
	if err := os.WriteFile(filepath.Join(dir, "routines", "digest.md"), []byte(routine), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(dir, "fixture.md")
	if err := os.WriteFile(fixture, []byte("A quiet day."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := creds.Initialize(dir); err != nil {
		t.Fatal(err)
	}
	key, store, err := creds.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	store["fake_api_key"] = "provider-secret-value"
	store["routine_secret"] = "routine-secret-value"
	if err := creds.Write(dir, key, store); err != nil {
		t.Fatal(err)
	}
	if err := repository.Initialize(dir); err != nil {
		t.Fatal(err)
	}

	captured := filepath.Join(t.TempDir(), "run.txt")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = session ]; then
  printf '[]'
  exit 0
fi
{
  printf 'FAKE_API_KEY=%%s\n' "${FAKE_API_KEY-unset}"
  printf 'ROUTINE_SECRET=%%s\n' "${ROUTINE_SECRET-unset}"
  printf '%%s\n' "$@"
} > %q
`, captured)
	fakeBin(t, "opencode", script)
	t.Setenv("OPENROUTINES_NATIVE", "1")

	result, err := RunManual(dir, "digest", ManualOptions{DiscardKnowledge: true, Rehearse: true, Fixture: fixture})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != Completed {
		t.Fatalf("outcome = %s, want %s", result.Outcome, Completed)
	}
	raw, err := os.ReadFile(captured)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{
		"FAKE_API_KEY=provider-secret-value",
		"ROUTINE_SECRET=unset",
		"The credentials, integrations, skills, and web access configured for this routine are unavailable.",
		"The model connection itself remains live.",
		"Knowledge changes will not be saved.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("scenario run missing %q:\n%s", want, got)
		}
	}
	for _, internal := range []string{"You have no credentials", "granted", "provider authentication", "stripped", "fixture world"} {
		if strings.Contains(got, internal) {
			t.Fatalf("scenario instructions contain internal term %q:\n%s", internal, got)
		}
	}
}
