// Package runenv plans one run's environment: the single authority on which
// environment variable names a run of one routine will contain and which
// grant mints each of them. `check` fails the repo on a plan's problems
// offline; the runner injects exactly what the plan says and refuses to
// start on the fatal ones. The plan is deliberately pure -- names and
// provenance only, computed from configuration alone: no credential store,
// no network, no secret values. Values stay with the minters.
package runenv

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/routine"
)

// Kind says how a credential grant materializes.
type Kind string

// The grant kinds a plan distinguishes.
const (
	Raw      Kind = "raw"      // stored value injected verbatim under its uppercase name
	Typed    Kind = "typed"    // derived surface minted by the trusted runner at spawn
	Provider Kind = "provider" // the model's provider key, auto-injected, optional in the store
)

// Credential is one credential grant and the environment names it mints.
type Credential struct {
	Name string
	Kind Kind
	Spec creds.Spec // set for Typed
	Env  []string
}

// Variable is one configured variable. A shadowed variable is not injected
// -- the credential wins (design decision "Variables") -- and the standing
// instruction must not advertise it.
type Variable struct {
	Name       string
	Env        string
	ShadowedBy string // the credential grant that owns Env; "" when injected
}

// Plan is the planned environment for one run.
type Plan struct {
	Credentials []Credential // declared credentials plus the provider key, in injection order
	Variables   []Variable   // configured variables, sorted by name
	fatal       []string
	flagged     []string
}

// frameworkOwned reports whether env is a name the framework constructs for
// a run rather than one a grant mints -- run metadata, the harness surface,
// and the process names a child always receives. It is a rule, not a list of
// the runner's current variables: the reserved-name rules already forbid any
// credential or variable from taking these names, so a reference to one is
// answered by the runner or by nothing at all. Enumerating them instead
// would put every new piece of run metadata one forgotten edit away from a
// check that rejects valid configuration.
func frameworkOwned(env string) bool {
	return creds.ReservedEnvName(strings.ToLower(env)) ||
		strings.HasPrefix(env, strings.ToUpper(creds.ReservedPrefix)+"_")
}

// New plans the environment one run of r would contain. model may be ""
// when unresolvable -- the provider grant is simply absent, and whatever
// made the model unresolvable is already someone else's error.
func New(agent *config.Agent, r *routine.Routine, model string) *Plan {
	p := &Plan{}
	owner := map[string]string{} // env name -> credential grant that minted it
	for _, name := range r.FM.Credentials {
		c := Credential{Name: name, Kind: Raw, Env: []string{strings.ToUpper(name)}}
		if spec, typed := agent.Credentials[name]; typed {
			c.Kind = Typed
			c.Spec = spec
			c.Env = creds.DerivedEnvNames(spec)
		}
		for _, env := range c.Env {
			if prev, taken := owner[env]; taken {
				p.fatal = append(p.fatal, fmt.Sprintf("credentials %q and %q both set %s in the run environment", prev, name, env))
				continue
			}
			owner[env] = name
		}
		p.Credentials = append(p.Credentials, c)
	}
	if model != "" {
		providerKey := creds.ProviderKeyName(strings.SplitN(model, "/", 2)[0])
		if spec, typed := agent.Credentials[providerKey]; typed {
			p.fatal = append(p.fatal, fmt.Sprintf("credential %q is the model's provider key but has typed metadata (type %s) -- provider auth needs the raw API key", providerKey, spec.Type))
		}
		env := strings.ToUpper(providerKey)
		switch prev, taken := owner[env]; {
		case taken && prev != providerKey:
			p.fatal = append(p.fatal, fmt.Sprintf("credential %q sets %s, the auto-injected provider key for model %s", prev, env, model))
		case !taken:
			// Declaring the provider key itself is benign: auto-injection is
			// the same grant, so the declared one simply stands in for it.
			owner[env] = providerKey
			p.Credentials = append(p.Credentials, Credential{Name: providerKey, Kind: Provider, Env: []string{env}})
		}
	}
	for _, name := range slices.Sorted(maps.Keys(agent.Variables)) {
		v := Variable{Name: name, Env: strings.ToUpper(name)}
		if prev, taken := owner[v.Env]; taken {
			v.ShadowedBy = prev
			p.flagged = append(p.flagged, fmt.Sprintf("variable %q is shadowed by credential %q, which sets %s in the run environment -- the credential wins; rename one", name, prev, v.Env))
		}
		p.Variables = append(p.Variables, v)
	}
	return p
}

// Problems returns everything check should fail the repo on.
func (p *Plan) Problems() []string {
	return append(slices.Clone(p.fatal), p.flagged...)
}

// Fatal returns the problems a run must refuse to start on: an environment
// name secret material would mint twice. Variable shadowing is not among
// them -- the credential wins and the run proceeds (design decision
// "Variables") -- but a refused start must not be retried either; the next
// attempt would fail identically.
func (p *Plan) Fatal() []string {
	return p.fatal
}

// EnvNames returns the names this plan's grants and unshadowed variables put
// in the run environment, sorted. The auto-injected provider key is not among
// them: the runner skips it when the store lacks it (opencode may carry its
// own auth), so only declaring it -- which makes it a raw grant the run
// refuses to start without -- turns it into a guarantee.
func (p *Plan) EnvNames() []string {
	set := map[string]struct{}{}
	for _, c := range p.Credentials {
		if c.Kind == Provider {
			continue
		}
		for _, env := range c.Env {
			set[env] = struct{}{}
		}
	}
	for _, v := range p.Variables {
		if v.ShadowedBy == "" {
			set[v.Env] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// Satisfies reports whether a run of this routine will contain env -- the
// question check asks of every {env:...} reference in a granted MCP server's
// opencode.json entry. Runs construct their environment from scratch, so a
// reference nothing satisfies can only ever resolve empty.
func (p *Plan) Satisfies(env string) bool {
	return frameworkOwned(env) || slices.Contains(p.EnvNames(), env)
}
