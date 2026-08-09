package runner

import (
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

// The constructed environment holds exactly the declared credentials plus
// the model's provider key -- the audit's second headline claim.
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

// Raw credentials inject verbatim under their uppercase names.
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
	// Decrypting the store is what registers its values for redaction.
	if got := scrub.Redacted("carrying sekrit"); strings.Contains(got, "sekrit") {
		t.Fatalf("stored credential not registered for redaction: %q", got)
	}

	typed := &routine.Routine{Name: "x", Frontmatter: routine.Frontmatter{Credentials: []string{"gh_key"}}}
	// A run with the typed credential fails at derivation (bad key)
	// rather than injecting the stored root secret.
	if _, err = resolveCredentials(dir, agent, typed, "openai/gpt"); err == nil {
		t.Fatal("expected derivation failure for an invalid stored key")
	}
}

// A resolve that fails partway has already minted whatever came before the
// failure. That material dies with the abandoned run, so its registration
// and its revocation must not outlive it -- the registry is bounded by live
// material, not by history.
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

// An auth failure's hint names what the framework knows and the provider's
// message does not: the resolved provider, the endpoint opencode.json
// declares, and the credential the run injected -- or that none was (#60).
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

	// No provider block: the provider name stands alone. No injected key:
	// the hint says so instead of claiming a credential was rejected.
	hint = authHint(dir, "anthropic/claude-x", false)
	for _, want := range []string{"anthropic rejected the request", "no anthropic_api_key credential is stored"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("hint missing %q:\n%s", want, hint)
		}
	}
}

// Some providers' status text carries no key-shaped phrase -- the session
// record's failure reads "...ended on an error: Unauthorized" -- and
// unmatched it reports as a bare crash (#60).
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
