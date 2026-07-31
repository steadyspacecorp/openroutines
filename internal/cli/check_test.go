package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/creds"
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

// A run may not outlast the agent's max_timeout ceiling. The runner enforces
// that; check is where an operator learns their setting will be cut down,
// before a routine is quietly killed at the ceiling in production.
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

// A variable shadowed by a credential's derived surface was an error
// nowhere: the run silently dropped the variable while the standing
// instruction kept advertising it. The run-environment plan makes it a
// check failure.
func TestCheckFlagsVariableShadowedByDerivedSurface(t *testing.T) {
	dir := t.TempDir()
	cfg := checkAgentYAML +
		"variables:\n  github_token: ghp-placeholder\n" +
		"credentials:\n  gh_app:\n    type: github_app\n    app_id: \"1\"\n"
	os.WriteFile(filepath.Join(dir, "openroutines.yml"), []byte(cfg), 0o644)
	os.WriteFile(filepath.Join(dir, "opencode.json"), []byte("{}\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "routines"), 0o755)
	os.WriteFile(filepath.Join(dir, "routines", "release.md"), []byte(
		"---\nschedule: \"0 9 * * *\"\ncredentials: [gh_app]\n---\nCut a release.\n"), 0o644)

	out := checkOutput(t, dir)
	if !strings.Contains(out, `variable "github_token" is shadowed by credential "gh_app"`) {
		t.Fatalf("expected a shadowed-variable failure:\n%s", out)
	}
	if !strings.Contains(out, "check failed") {
		t.Fatalf("shadowing must fail check, not warn:\n%s", out)
	}
}

// An {env:...} reference in a granted MCP server's entry must name something
// the routine's run environment will contain -- otherwise the header
// resolves empty and fails as opaque auth at run time.
func TestCheckFlagsUnresolvableMCPEnvRef(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "openroutines.yml"), []byte(checkAgentYAML), 0o644)
	os.WriteFile(filepath.Join(dir, "opencode.json"), []byte(
		`{"mcp":{"steady":{"type":"remote","url":"https://mcp.example.com","headers":{"Authorization":"Bearer {env:STEADY_TOKEN}"}}}}`), 0o644)
	os.MkdirAll(filepath.Join(dir, "routines"), 0o755)
	os.WriteFile(filepath.Join(dir, "routines", "hooked.md"), []byte(
		"---\nschedule: \"0 9 * * *\"\nmcp: [steady]\n---\nUse the server.\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "routines", "wired.md"), []byte(
		"---\nschedule: \"0 9 * * *\"\nmcp: [steady]\ncredentials: [steady_token]\n---\nUse the server.\n"), 0o644)

	out := checkOutput(t, dir)
	if !strings.Contains(out, `hooked: mcp server "steady" references {env:STEADY_TOKEN}`) {
		t.Fatalf("expected an unresolvable env-ref failure for hooked:\n%s", out)
	}
	if strings.Contains(out, `wired: mcp server "steady" references`) {
		t.Fatalf("wired declares the credential, its reference resolves:\n%s", out)
	}
}

func TestCheckFlagsMCPRefOnUndeclaredProviderKey(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "openroutines.yml"), []byte(checkAgentYAML), 0o644)
	os.WriteFile(filepath.Join(dir, "opencode.json"), []byte(
		`{"mcp":{"gateway":{"type":"remote","url":"https://mcp.example.com","headers":{"Authorization":"Bearer {env:FAKE_API_KEY}"}}}}`), 0o644)
	os.MkdirAll(filepath.Join(dir, "routines"), 0o755)
	os.WriteFile(filepath.Join(dir, "routines", "keyed.md"), []byte(
		"---\nschedule: \"0 9 * * *\"\nmcp: [gateway]\n---\nUse the server.\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "routines", "pinned.md"), []byte(
		"---\nschedule: \"0 9 * * *\"\nmcp: [gateway]\ncredentials: [fake_api_key]\n---\nUse the server.\n"), 0o644)

	out := checkOutput(t, dir)
	if !strings.Contains(out, `keyed: mcp server "gateway" references {env:FAKE_API_KEY}`) || !strings.Contains(out, `declare "fake_api_key"`) {
		t.Fatalf("an undeclared provider key must not satisfy an mcp reference, and the failure should say how to fix it:\n%s", out)
	}
	if strings.Contains(out, `pinned: mcp server "gateway" references`) {
		t.Fatalf("pinned declares the provider key, its reference is guaranteed:\n%s", out)
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
