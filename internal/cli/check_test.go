package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/version"
)

const checkAgentYAML = `name: test-agent
instructions: Tests check
owner:
  name: CI
  email: ci@example.invalid
timezone: UTC
defaults:
  model: fake/model
`

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

// A run may not outlast the agent's max_timeout ceiling. The runner enforces
// that; check is where an operator learns their setting will be cut down,
// before a routine is quietly killed at the ceiling in production.
// A binary that doesn't match the agent's pin reads the repo through the
// wrong schema; check names that first, before the schema-shaped noise.
func TestCheckNamesBinaryPinMismatchFirst(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "openroutines.yml"), []byte(checkAgentYAML), 0o644)
	os.MkdirAll(filepath.Join(dir, ".openroutines"), 0o755)
	os.WriteFile(filepath.Join(dir, ".openroutines", "version"), []byte("v9.9.9\n"), 0o644)

	was := version.Version
	version.Version = "v1.0.0"
	defer func() { version.Version = was }()

	out := checkOutput(t, dir)
	if !strings.Contains(out, "this binary is v1.0.0 but the agent pins v9.9.9") {
		t.Fatalf("expected the mismatch named:\n%s", out)
	}
	if strings.Index(out, "binary") > strings.Index(out, "openroutines.yml") {
		t.Fatalf("the mismatch must come first:\n%s", out)
	}

	// A source build is exempt: development runs against pinned agents.
	version.Version = "v0.0.0-dev"
	if out := checkOutput(t, dir); strings.Contains(out, "but the agent pins") {
		t.Fatalf("a dev binary must not report a pin mismatch:\n%s", out)
	}
}

// An inactive routine is valid but will never run: it gets a circle, not
// a check mark, so a parked reporter can't read as one that will fire.
func TestCheckMarksInactiveRoutinesWithACircle(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "openroutines.yml"), []byte(checkAgentYAML), 0o644)
	os.MkdirAll(filepath.Join(dir, "routines"), 0o755)
	os.WriteFile(filepath.Join(dir, "routines", "parked.md"), []byte(
		"---\nschedule: \"0 9 * * *\"\nactive: false\n---\nWait.\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "routines", "live.md"), []byte(
		"---\nschedule: \"0 9 * * *\"\n---\nGo.\n"), 0o644)

	out := checkOutput(t, dir)
	if !strings.Contains(out, "○ parked") || strings.Contains(out, "✓ parked") {
		t.Fatalf("inactive routine should get a circle:\n%s", out)
	}
	if !strings.Contains(out, "✓ live") {
		t.Fatalf("active routine should keep the check mark:\n%s", out)
	}
}

// A rehearsal fixture bound to no routine is drift -- the routine was
// renamed or removed out from under it.
func TestCheckWarnsOnOrphanedRehearsalFixtures(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "openroutines.yml"), []byte(checkAgentYAML), 0o644)
	os.MkdirAll(filepath.Join(dir, "routines"), 0o755)
	os.WriteFile(filepath.Join(dir, "routines", "digest.md"), []byte(
		"---\nschedule: \"0 9 * * *\"\n---\nDigest.\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "rehearsals", "gone"), 0o755)
	os.WriteFile(filepath.Join(dir, "rehearsals", "digest.md"), []byte("fixtures"), 0o644)
	os.WriteFile(filepath.Join(dir, "rehearsals", "missing.md"), []byte("fixtures"), 0o644)

	out := checkOutput(t, dir)
	if !strings.Contains(out, "rehearsals/missing.md matches no routine") ||
		!strings.Contains(out, "rehearsals/gone/ matches no routine") {
		t.Fatalf("orphaned fixtures should warn:\n%s", out)
	}
	if strings.Contains(out, "rehearsals/digest.md matches") {
		t.Fatalf("a bound fixture must not warn:\n%s", out)
	}
}

func TestCheckWarnsOnTimeoutsAboveTheCeiling(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "openroutines.yml"), []byte(checkAgentYAML+"max_timeout: 1h\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "routines"), 0o755)
	os.WriteFile(filepath.Join(dir, "routines", "marathon.md"), []byte(
		"---\nschedule: \"0 9 * * *\"\ntimeout: 90m\n---\nTake ages.\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "routines", "sprint.md"), []byte(
		"---\nschedule: \"0 9 * * *\"\ntimeout: 10m\n---\nBe quick.\n"), 0o644)

	out := checkOutput(t, dir)
	if !strings.Contains(out, "marathon: timeout 1h30m0s exceeds the agent's 1h0m0s ceiling") ||
		!strings.Contains(out, "capped at 1h0m0s") {
		t.Fatalf("expected a ceiling warning for marathon:\n%s", out)
	}
	if strings.Contains(out, "sprint: timeout") {
		t.Fatalf("a 10m timeout fits inside the ceiling and must not warn:\n%s", out)
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

func TestCheckReportsAgentOwnedRoutineOverride(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "openroutines.yml"), []byte(checkAgentYAML), 0o644)
	os.WriteFile(filepath.Join(dir, "opencode.json"), []byte("{}\n"), 0o644)
	for path, body := range map[string]string{
		filepath.Join("routines", "digest.md"):                                         "Agent-owned implementation.",
		filepath.Join(".openroutines", "plugins", "reporter", "routines", "digest.md"): "Plugin implementation.",
	} {
		path = filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("---\nschedule: \"0 9 * * *\"\n---\n"+body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	out := checkOutput(t, dir)
	if !strings.Contains(out, "digest overrides .openroutines/plugins/reporter/routines/digest.md") {
		t.Fatalf("check should name the shadowed plugin routine:\n%s", out)
	}
	if strings.Contains(out, "duplicate routine") {
		t.Fatalf("an agent-owned override is not a duplicate error:\n%s", out)
	}
}

func TestCheckAllowsTypedTriggerCredential(t *testing.T) {
	dir := t.TempDir()
	config := checkAgentYAML + "credentials:\n  gh_key:\n    type: github_app\n    app_id: \"1\"\n"
	os.WriteFile(filepath.Join(dir, "openroutines.yml"), []byte(config), 0o644)
	os.WriteFile(filepath.Join(dir, "opencode.json"), []byte("{}\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "routines"), 0o755)
	os.WriteFile(filepath.Join(dir, "routines", "watch-private.md"), []byte(
		"---\nschedule: \"0 9 * * *\"\ntrigger:\n  poll: https://api.github.com/repos/example/private/actions/runs\n  credential: gh_key\ncredentials: [gh_key]\n---\nWatch the private repository.\n"), 0o644)
	os.WriteFile(filepath.Join(dir, creds.KeyFileName), []byte(creds.GenerateKey()), 0o600)
	key, err := creds.LoadKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := creds.Write(dir, key, map[string]string{"gh_key": testKeyPEM(t), "fake_api_key": "provider-key"}); err != nil {
		t.Fatal(err)
	}

	out := checkOutput(t, dir)
	if strings.Contains(out, "use a raw credential") || !strings.Contains(out, "watch-private (") {
		t.Fatalf("typed trigger credential should pass routine validation:\n%s", out)
	}

	// The same agent with a truncated key in the store must fail check:
	// before #69 this passed and first surfaced as a failed production run.
	if err := creds.Write(dir, key, map[string]string{"gh_key": "-----BEGIN PRIVATE KEY-----", "fake_api_key": "provider-key"}); err != nil {
		t.Fatal(err)
	}
	out = checkOutput(t, dir)
	if !strings.Contains(out, "not a PEM private key") || !strings.Contains(out, "credentials set gh_key") {
		t.Fatalf("check should flag a stored github_app value that cannot serve its type:\n%s", out)
	}
}

func TestCheckSurfacesRetiredEventsKeyWithMapping(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "openroutines.yml"), []byte(checkAgentYAML), 0o644)
	os.WriteFile(filepath.Join(dir, "opencode.json"), []byte("{}\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "routines"), 0o755)
	os.WriteFile(filepath.Join(dir, "routines", "check-in.md"), []byte(
		"---\nschedule: \"0 9 * * *\"\nevents: false\n---\nReport.\n"), 0o644)

	out := checkOutput(t, dir)
	if !strings.Contains(out, `"events: false" is now "teamwork: off"`) {
		t.Fatalf("a fleet routine still declaring events: must fail check with the rename, not an unknown-field error:\n%s", out)
	}
}

func TestCheckSurfacesRetiredConsumesKeyWithMapping(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "openroutines.yml"), []byte(checkAgentYAML), 0o644)
	os.WriteFile(filepath.Join(dir, "opencode.json"), []byte("{}\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "routines"), 0o755)
	os.WriteFile(filepath.Join(dir, "routines", "check-in.md"), []byte(
		"---\nschedule: \"0 9 * * *\"\nconsumes: knowledge\n---\nReport.\n"), 0o644)

	out := checkOutput(t, dir)
	if !strings.Contains(out, `"consumes: knowledge" is now "reports: true"`) {
		t.Fatalf("a fleet routine still declaring consumes: must fail check with the rename, not an unknown-field error:\n%s", out)
	}
}
