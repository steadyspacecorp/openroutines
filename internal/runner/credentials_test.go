package runner

import (
	"errors"
	"fmt"
	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/scrub"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveCredentialsScope(t *testing.T) {
	dir := t.TempDir()
	key := creds.GenerateKey()
	if err := os.WriteFile(filepath.Join(dir, creds.KeyFileName), []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := creds.LoadKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := map[string]string{
		"slack_webhook":     "hook-value",
		"deploy_token":      "token-value",
		"anthropic_api_key": "sk-ant-x",
		"openai_api_key":    "sk-oai-x",
	}
	if err := creds.Write(dir, loaded, store); err != nil {
		t.Fatal(err)
	}

	agent := &config.Agent{}
	r := &routine.Routine{Name: "x", Frontmatter: routine.Frontmatter{Credentials: []string{"slack_webhook"}}}
	got, err := resolveCredentials(dir, agent, r, "anthropic/claude-sonnet-5")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"SLACK_WEBHOOK": "hook-value", "ANTHROPIC_API_KEY": "sk-ant-x"}
	if len(got.env) != len(want) {
		t.Fatalf("resolved %v, want exactly %v -- undeclared secrets must not exist in the run", got.env, want)
	}
	for k, v := range want {
		if got.env[k] != v {
			t.Fatalf("resolved %v, want %v", got.env, want)
		}
	}

	r.Frontmatter.Credentials = []string{"missing_cred"}
	if _, err := resolveCredentials(dir, agent, r, "anthropic/claude-sonnet-5"); err == nil {
		t.Fatal("declaring an absent credential must fail the run, not proceed without it")
	}
}

func TestResolveCredentialsPreservesUnavailableStoreForDiagnostics(t *testing.T) {
	r := &routine.Routine{Name: "x"}
	secrets, err := resolveCredentials(t.TempDir(), &config.Agent{}, r, "anthropic/claude-x")
	if err != nil {
		t.Fatal(err)
	}
	if secrets.credentialErr == nil || !strings.Contains(secrets.credentialErr.Error(), creds.FileName+" is missing") {
		t.Fatalf("credential error = %v", secrets.credentialErr)
	}
}

func TestResolveCredentialsRaw(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, creds.KeyFileName), []byte(creds.GenerateKey()), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := creds.LoadKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := creds.Write(dir, key, map[string]string{
		"steady_token":   "sekrit",
		"openai_api_key": "provider-key",
		"gh_key":         "not a real pem",
	}); err != nil {
		t.Fatal(err)
	}
	agent := &config.Agent{Credentials: map[string]creds.Spec{"gh_key": {Type: "github_app", AppID: "1"}}}

	r := &routine.Routine{Name: "x", Frontmatter: routine.Frontmatter{Credentials: []string{"steady_token"}}}
	s, err := resolveCredentials(dir, agent, r, "openai/gpt")
	if err != nil {
		t.Fatal(err)
	}
	if s.env["STEADY_TOKEN"] != "sekrit" || s.env["OPENAI_API_KEY"] != "provider-key" {
		t.Fatalf("raw injection wrong: %v", s.env)
	}
	if got := scrub.Redacted("carrying sekrit"); strings.Contains(got, "sekrit") {
		t.Fatalf("stored credential not registered for redaction: %q", got)
	}

	typed := &routine.Routine{Name: "x", Frontmatter: routine.Frontmatter{Credentials: []string{"gh_key"}}}
	if _, err = resolveCredentials(dir, agent, typed, "openai/gpt"); err == nil {
		t.Fatal("expected derivation failure for an invalid stored key")
	}
}

func TestResolveCredentialsReleasesDerivedMaterialOnFailure(t *testing.T) {
	const bearer = "bearer-of-an-abandoned-run"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"access_token":%q}`, bearer)
	}))
	defer server.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, creds.KeyFileName), []byte(creds.GenerateKey()), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := creds.LoadKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := creds.Write(dir, key, map[string]string{"desk": "client-secret"}); err != nil {
		t.Fatal(err)
	}
	agent := &config.Agent{Credentials: map[string]creds.Spec{
		"desk": {Type: "oauth2_client", TokenURL: server.URL, ClientID: "c", InjectAs: "desk_token"},
	}}
	r := &routine.Routine{Name: "x", Frontmatter: routine.Frontmatter{Credentials: []string{"desk", "missing_cred"}}}

	if _, err := resolveCredentials(dir, agent, r, "openai/gpt"); err == nil {
		t.Fatal("declaring an absent credential must fail the run")
	}
	if got := scrub.Redacted(bearer); got != bearer {
		t.Fatalf("the abandoned run's bearer is still registered: %q", got)
	}
}

func TestAuthHintNamesProviderEndpointAndCredential(t *testing.T) {
	dir := t.TempDir()
	cfg := `{"provider":{"my_gateway":{"options":{"baseURL":"https://gateway.example.com/v1/compat"}}}}`
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	hint := authHint(dir, "my_gateway/some-model", true)
	for _, want := range []string{"my_gateway at https://gateway.example.com/v1/compat", "rejected the run's my_gateway_api_key credential", "credentials set my_gateway_api_key"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("hint missing %q:\n%s", want, hint)
		}
	}

	hint = authHint(dir, "anthropic/claude-x", false)
	for _, want := range []string{"anthropic rejected the request", "no anthropic_api_key credential is stored"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("hint missing %q:\n%s", want, hint)
		}
	}
}

func TestAuthFailurePatternMatchesBareStatusText(t *testing.T) {
	for _, line := range []string{
		"the model session ended on an error: Unauthorized",
		"error: unauthorized",
		"the model session ended on an error: invalid bearer token",
		"API key is invalid.",
	} {
		if !authFailurePattern.MatchString(line) {
			t.Fatalf("auth pattern should match %q", line)
		}
	}
	if authFailurePattern.MatchString("the reviewer felt unauthorized to approve") {
		t.Fatal("bare 'unauthorized' outside an error line should not classify as auth failure")
	}
}

func TestModelNotFoundHintDistinguishesMissingCredential(t *testing.T) {
	hint := modelNotFoundHint("anthropic/claude-x", false, nil)
	for _, want := range []string{"no anthropic_api_key was available to the run", "run openroutines check"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("missing-credential hint lacks %q: %s", want, hint)
		}
	}
	if strings.Contains(hint, "credentials set") {
		t.Fatalf("runtime hint duplicated check's remediation: %s", hint)
	}
	hint = modelNotFoundHint("anthropic/not-a-model", true, nil)
	if !strings.Contains(hint, "verify the model name and provider configuration") || strings.Contains(hint, "no anthropic_api_key") {
		t.Fatalf("unknown-model hint misclassified the failure: %s", hint)
	}
}

func TestModelNotFoundHintIncludesCredentialLoadFailure(t *testing.T) {
	hint := modelNotFoundHint("anthropic/claude-x", false, errors.New("locked credential store"))
	for _, want := range []string{"credentials could not be loaded: locked credential store", "run openroutines check"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("credential-load hint lacks %q: %s", want, hint)
		}
	}
}

func TestModelNotFoundClassificationIsSpecific(t *testing.T) {
	if !isModelNotFound("ProviderModelNotFoundError: anthropic/claude-x") {
		t.Fatal("ProviderModelNotFoundError was not classified")
	}
	for _, failure := range []string{"unknown model response shape", "model not found in cache"} {
		if isModelNotFound(failure) {
			t.Fatalf("unrelated failure was classified as a provider model error: %q", failure)
		}
	}
}
