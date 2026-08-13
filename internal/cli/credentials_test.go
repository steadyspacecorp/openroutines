package cli

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/creds"
)

const credentialsAgentYAML = `name: test-agent
instructions: Tests credentials
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

func testKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

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

func setPiped(t *testing.T, dir, name, input string) int {
	t.Helper()
	var code int
	withStdin(t, input, func() {
		capture(t, dir, func() { code = credentialsSet([]string{name}) })
	})
	return code
}

func storedValue(t *testing.T, dir, name string) (string, bool) {
	t.Helper()
	key, err := creds.LoadKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := creds.Read(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := store[name]
	return v, ok
}

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

func TestSetStoresMultiLineValueEscaped(t *testing.T) {
	dir := credentialsAgent(t, credentialsAgentYAML)
	pemKey := testKeyPEM(t)
	if code := setPiped(t, dir, "app_key", pemKey); code != 0 {
		t.Fatalf("set exited %d", code)
	}
	v, ok := storedValue(t, dir, "app_key")
	if !ok {
		t.Fatal("nothing stored")
	}
	if strings.Contains(v, "\n") {
		t.Fatalf("stored value must be one line, got %d lines", strings.Count(v, "\n")+1)
	}
	if v != strings.ReplaceAll(strings.TrimSpace(pemKey), "\n", `\n`) {
		t.Fatalf("stored value is not the escaped form of the input:\n%s", v)
	}
	if err := creds.ValidateStored(creds.Spec{Type: "github_app", AppID: "1"}, v); err != nil {
		t.Fatalf("escaped stored key does not validate: %v", err)
	}
}

func TestSetStoresSingleLineValueVerbatim(t *testing.T) {
	dir := credentialsAgent(t, credentialsAgentYAML)
	if code := setPiped(t, dir, "token", "s3cret\n"); code != 0 {
		t.Fatalf("set exited %d", code)
	}
	if v, _ := storedValue(t, dir, "token"); v != "s3cret" {
		t.Fatalf("stored %q, want s3cret", v)
	}
}

func TestSetRefusesTruncatedPEM(t *testing.T) {
	dir := credentialsAgent(t, credentialsAgentYAML)
	if code := setPiped(t, dir, "app_key", "-----BEGIN PRIVATE KEY-----\n"); code == 0 {
		t.Fatal("set accepted the first line of a PEM key")
	}
	if _, ok := storedValue(t, dir, "app_key"); ok {
		t.Fatal("a refused value must not be stored")
	}
}
