package runner

import (
	"fmt"
	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// authFailurePattern matches provider authentication errors in the session
// record's failure text, so a bad key reads as configuration instead of an
// opaque crash. The `error:` forms cover bare status text passed through
// verbatim ("... ended on an error: Unauthorized").
var authFailurePattern = regexp.MustCompile(`(?i)invalid x-api-key|api key is invalid|invalid api key|incorrect api key|401 unauthorized|authentication_error|missing.{0,20}api key|error:\s*unauthorized|invalid bearer token`)

// authHint adds what the provider's own message does not say: the resolved
// provider, the declared endpoint, and whether a credential was injected.
func authHint(dir, model string, injected bool) string {
	provider := strings.SplitN(model, "/", 2)[0]
	keyName := creds.ProviderKeyName(provider)
	endpoint := provider
	if oc, err := config.LoadOpenCode(dir); err == nil {
		if u := oc.ProviderBaseURL(provider); u != "" {
			endpoint = provider + " at " + u
		}
	}
	if injected {
		return fmt.Sprintf("provider authentication failed -- %s rejected the run's %s credential (openroutines credentials set %s)", endpoint, keyName, keyName)
	}
	return fmt.Sprintf("provider authentication failed -- %s rejected the request and no %s credential is stored (openroutines credentials set %s)", endpoint, keyName, keyName)
}

// runSecrets is a run's resolved secret material: the environment to inject,
// and cleanup for derived credentials. Redaction registers where values
// materialize, not here.
type runSecrets struct {
	env     map[string]string
	cleanup []func()
}

func (s *runSecrets) setEnv(name, value string) error {
	if _, taken := s.env[name]; taken {
		return fmt.Errorf("credential grants set the %s environment variable twice", name)
	}
	s.env[name] = value
	return nil
}

// release disposes of derived material -- best-effort, once, at attempt end.
func (s *runSecrets) release() {
	for _, f := range s.cleanup {
		f()
	}
	s.cleanup = nil
}

// resolveCredentials builds the routine's secret set: declared credentials
// plus the provider key for its model. Raw credentials inject verbatim under
// their uppercase name; typed ones inject their derived surface -- the stored
// root secret never enters the run. A failed resolve releases whatever it
// already derived.
func resolveCredentials(dir string, agent *config.Agent, r *routine.Routine, model string) (_ *runSecrets, err error) {
	provider := strings.SplitN(model, "/", 2)[0]
	providerKey := creds.ProviderKeyName(provider)
	out := &runSecrets{env: map[string]string{}}
	defer func() {
		if err != nil {
			out.release()
		}
	}()

	key, keyErr := creds.LoadKey(dir)
	if keyErr != nil {
		if len(r.FM.Credentials) > 0 {
			return nil, fmt.Errorf("routine declares credentials but %w", keyErr)
		}
		// No store: opencode may still have its own auth for the provider.
		return out, nil
	}
	store, err := creds.Read(dir, key)
	if err != nil {
		return nil, err
	}
	for _, name := range r.FM.Credentials {
		v, present := store[name]
		if !present {
			return nil, fmt.Errorf("routine declares credential %q, not present in %s", name, creds.FileName)
		}
		spec, typed := agent.Credentials[name]
		if !typed {
			if err := out.setEnv(strings.ToUpper(name), v); err != nil {
				return nil, err
			}
			continue
		}
		derived, err := creds.Derive(name, spec, v)
		if err != nil {
			return nil, err
		}
		out.cleanup = append(out.cleanup, derived.Cleanup)
		for _, k := range slices.Sorted(maps.Keys(derived.Env)) {
			if err := out.setEnv(k, derived.Env[k]); err != nil {
				return nil, err
			}
		}
	}
	if v, present := store[providerKey]; present {
		if err := out.setEnv(strings.ToUpper(providerKey), v); err != nil {
			return nil, err
		}
	}
	return out, nil
}
