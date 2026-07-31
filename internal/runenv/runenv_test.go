package runenv

import (
	"slices"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/routine"
)

func planFor(agent *config.Agent, credentials []string, model string) *Plan {
	return New(agent, &routine.Routine{Name: "x", FM: routine.Frontmatter{Credentials: credentials}}, model)
}

// The baseline plan: declared credentials mint their surfaces, the provider
// key auto-injects, variables ride along -- and nothing is a problem.
func TestPlanGrantsAndProviderKey(t *testing.T) {
	agent := &config.Agent{
		Variables:   map[string]string{"docs_url": "https://docs.example.com"},
		Credentials: map[string]creds.Spec{"gh_app": {Type: "github_app", AppID: "1"}},
	}
	p := planFor(agent, []string{"slack_webhook", "gh_app"}, "anthropic/claude-sonnet-5")
	if got := p.Problems(); len(got) != 0 {
		t.Fatalf("clean config should plan without problems, got %v", got)
	}
	names := p.EnvNames()
	for _, want := range []string{"SLACK_WEBHOOK", "GITHUB_TOKEN", "GH_TOKEN", "DOCS_URL"} {
		if !slices.Contains(names, want) {
			t.Fatalf("plan should contain %s: %v", want, names)
		}
	}
	if p.Credentials[len(p.Credentials)-1].Kind != Provider {
		t.Fatalf("provider key should be the last grant: %+v", p.Credentials)
	}
}

// Framework metadata satisfies a reference without the plan enumerating it:
// no grant can take a reserved name, so the runner is the only thing that
// could have set it. A run-metadata variable the runner grows later must not
// need a matching edit here to stop check from rejecting valid config.
func TestSatisfiesFrameworkNamesByRule(t *testing.T) {
	p := planFor(&config.Agent{}, nil, "anthropic/claude-sonnet-5")
	for _, env := range []string{"TZ", "HOME", "OPENROUTINES_RUN_ID", "OPENROUTINES_URL", "OPENCODE_ENABLE_EXA"} {
		if !p.Satisfies(env) {
			t.Errorf("%s is framework-owned and must satisfy a reference", env)
		}
	}
	if p.Satisfies("SOME_SERVICE_TOKEN") {
		t.Error("a name no grant mints and no rule covers must not satisfy a reference")
	}
}

// The auto-injected provider key is optional -- the runner skips it when the
// store lacks it -- so it is never a guaranteed name. Declaring it promotes
// the same grant to a raw credential the run refuses to start without.
func TestEnvNamesOmitOptionalProviderKey(t *testing.T) {
	if p := planFor(&config.Agent{}, nil, "anthropic/claude-sonnet-5"); slices.Contains(p.EnvNames(), "ANTHROPIC_API_KEY") {
		t.Fatalf("an undeclared provider key is not guaranteed to exist at run time: %v", p.EnvNames())
	}
	if p := planFor(&config.Agent{}, []string{"anthropic_api_key"}, "anthropic/claude-sonnet-5"); !slices.Contains(p.EnvNames(), "ANTHROPIC_API_KEY") {
		t.Fatalf("a declared provider key is guaranteed: %v", p.EnvNames())
	}
}

// Declaring the model's own provider key is benign: auto-injection is the
// same grant, so the declared one stands in for it instead of colliding.
func TestPlanDedupesDeclaredProviderKey(t *testing.T) {
	p := planFor(&config.Agent{}, []string{"anthropic_api_key"}, "anthropic/claude-sonnet-5")
	if got := p.Problems(); len(got) != 0 {
		t.Fatalf("declaring the provider key must not be a problem, got %v", got)
	}
	count := 0
	for _, c := range p.Credentials {
		if c.Env[0] == "ANTHROPIC_API_KEY" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("want exactly one ANTHROPIC_API_KEY grant, got %+v", p.Credentials)
	}
}

// Secret material minting one name twice is fatal: two github_apps, a raw
// credential under a derived surface's name, an inject_as collision, or a
// grant taking the provider key's name.
func TestPlanFatalCollisions(t *testing.T) {
	cases := []struct {
		name        string
		agent       *config.Agent
		credentials []string
		model       string
		want        string
	}{
		{
			name: "two typed credentials share a surface",
			agent: &config.Agent{Credentials: map[string]creds.Spec{
				"app_one": {Type: "github_app", AppID: "1"},
				"app_two": {Type: "github_app", AppID: "2"},
			}},
			credentials: []string{"app_one", "app_two"},
			want:        "GITHUB_TOKEN",
		},
		{
			name:        "raw credential under a derived surface's name",
			agent:       &config.Agent{Credentials: map[string]creds.Spec{"gh_app": {Type: "github_app", AppID: "1"}}},
			credentials: []string{"gh_app", "gh_token"},
			want:        "GH_TOKEN",
		},
		{
			name:        "inject_as collides with a raw credential",
			agent:       &config.Agent{Credentials: map[string]creds.Spec{"helpscout": {Type: "oauth2_client", TokenURL: "https://api.example.com/token", ClientID: "id", InjectAs: "steady_token"}}},
			credentials: []string{"steady_token", "helpscout"},
			want:        "STEADY_TOKEN",
		},
		{
			name:        "credential takes the provider key's name",
			agent:       &config.Agent{Credentials: map[string]creds.Spec{"svc": {Type: "oauth2_client", TokenURL: "https://api.example.com/token", ClientID: "id", InjectAs: "anthropic_api_key"}}},
			credentials: []string{"svc"},
			want:        "ANTHROPIC_API_KEY",
		},
		{
			name:        "typed metadata on the provider key",
			agent:       &config.Agent{Credentials: map[string]creds.Spec{"anthropic_api_key": {Type: "github_app", AppID: "1"}}},
			credentials: nil,
			want:        "provider key",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := tc.model
			if model == "" {
				model = "anthropic/claude-sonnet-5"
			}
			p := planFor(tc.agent, tc.credentials, model)
			if len(p.Fatal()) == 0 {
				t.Fatalf("want a fatal problem, got none (problems: %v)", p.Problems())
			}
			if !strings.Contains(strings.Join(p.Fatal(), "; "), tc.want) {
				t.Fatalf("fatal problem should mention %s: %v", tc.want, p.Fatal())
			}
		})
	}
}

// A shadowed variable is flagged, never fatal: the credential wins and the
// run proceeds (design decision "Variables") -- but the plan marks it so the
// instruction stops advertising it and check fails the repo.
func TestPlanFlagsShadowedVariables(t *testing.T) {
	agent := &config.Agent{
		Variables:   map[string]string{"github_token": "ghp-placeholder", "docs_url": "https://docs.example.com"},
		Credentials: map[string]creds.Spec{"gh_app": {Type: "github_app", AppID: "1"}},
	}
	p := planFor(agent, []string{"gh_app"}, "anthropic/claude-sonnet-5")
	if len(p.Fatal()) != 0 {
		t.Fatalf("shadowing must not be fatal, got %v", p.Fatal())
	}
	if got := p.Problems(); len(got) != 1 || !strings.Contains(got[0], "github_token") || !strings.Contains(got[0], "gh_app") {
		t.Fatalf("want one flag naming the variable and its shadower, got %v", got)
	}
	for _, v := range p.Variables {
		switch v.Name {
		case "github_token":
			if v.ShadowedBy != "gh_app" {
				t.Fatalf("github_token should be shadowed by gh_app: %+v", v)
			}
		case "docs_url":
			if v.ShadowedBy != "" {
				t.Fatalf("docs_url is not shadowed: %+v", v)
			}
		}
	}
	if slices.Contains(p.EnvNames(), "GHP-PLACEHOLDER") {
		t.Fatal("variable values must never appear in the plan")
	}
}

// A raw credential shadows a variable of the same name too -- the case the
// store-level check catches agent-wide, planned here per routine.
func TestPlanFlagsVariableShadowedByRawCredential(t *testing.T) {
	agent := &config.Agent{Variables: map[string]string{"steady_token": "public"}}
	p := planFor(agent, []string{"steady_token"}, "")
	if len(p.Fatal()) != 0 || len(p.Problems()) != 1 {
		t.Fatalf("want exactly one flagged problem, got fatal %v problems %v", p.Fatal(), p.Problems())
	}
}
