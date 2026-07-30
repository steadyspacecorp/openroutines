package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/creds"
)

const credentialsAgentYAML = `name: test-agent
description: Tests credentials
owner:
  name: CI
  email: ci@example.invalid
timezone: UTC
defaults:
  model: fake/model
credentials:
  github_app_private_key:
    type: github_app
    app_id: "1"
`

// credentialsAgent builds an agent directory with a master key and the given
// openroutines.yml, ready for the credentials commands to open its store.
func credentialsAgent(t *testing.T, agentYAML string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openroutines.yml"), []byte(agentYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, creds.KeyFileName), []byte(creds.GenerateKey()), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// withStdin feeds input to os.Stdin for the duration of run, restoring it
// afterward -- credentialsSet reads a piped value as a single line.
func withStdin(t *testing.T, input string, run func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(input); err != nil {
		t.Fatal(err)
	}
	w.Close()
	stdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = stdin }()
	run()
}

// A credential declared type: github_app never puts its stored value in a
// run's environment -- the run gets a minted installation token instead. The
// confirmation printed by `credentials set` has to say that, not the raw
// "receive it as GITHUB_APP_PRIVATE_KEY" message, which sends anyone who
// trusts it to reference an env var that is always empty (#66).
func TestCredentialsSetDescribesTypedCredential(t *testing.T) {
	dir := credentialsAgent(t, credentialsAgentYAML)
	var out string
	withStdin(t, "a-private-key\n", func() {
		out = capture(t, dir, func() { credentialsSet([]string{"github_app_private_key"}) })
	})
	if strings.Contains(out, "receive it as GITHUB_APP_PRIVATE_KEY") {
		t.Fatalf("a github_app credential's stored value is never injected under its own name:\n%s", out)
	}
	want := "Added github_app_private_key (type: github_app) -- routines that declare it receive GITHUB_TOKEN, GH_TOKEN, GITHUB_APP_SLUG, and the App's git identity. The stored key is never injected."
	if !strings.Contains(out, want) {
		t.Fatalf("missing %q:\n%s", want, out)
	}
}

// An oauth2_client credential's injected name is the entry's inject_as, not
// the credential's own name.
func TestCredentialsSetDescribesOAuth2ClientCredential(t *testing.T) {
	config := credentialsAgentYAML + "  support_desk_secret:\n    type: oauth2_client\n    token_url: https://example.invalid/token\n    client_id: abc\n    inject_as: support_desk_token\n"
	dir := credentialsAgent(t, config)
	var out string
	withStdin(t, "a-client-secret\n", func() {
		out = capture(t, dir, func() { credentialsSet([]string{"support_desk_secret"}) })
	})
	want := "Added support_desk_secret (type: oauth2_client) -- routines that declare it receive the minted bearer as SUPPORT_DESK_TOKEN. The stored client secret is never injected."
	if !strings.Contains(out, want) {
		t.Fatalf("missing %q:\n%s", want, out)
	}
}

// A credential with no metadata entry is raw: the existing message is still
// correct there and must not regress.
func TestCredentialsSetDescribesRawCredential(t *testing.T) {
	dir := credentialsAgent(t, credentialsAgentYAML)
	var out string
	withStdin(t, "a-webhook-url\n", func() {
		out = capture(t, dir, func() { credentialsSet([]string{"slack_webhook"}) })
	})
	want := "Added slack_webhook -- routines that declare it receive it as SLACK_WEBHOOK"
	if !strings.Contains(out, want) {
		t.Fatalf("missing %q:\n%s", want, out)
	}
}
