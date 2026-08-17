package runner

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/routine"
)

var authFailurePattern = regexp.MustCompile(`(?i)invalid x-api-key|api key is invalid|invalid api key|incorrect api key|401 unauthorized|authentication_error|missing.{0,20}api key|error:\s*unauthorized|invalid bearer token`)

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

func modelNotFoundHint(model string, injected bool, credentialErr error) string {
	provider := strings.SplitN(model, "/", 2)[0]
	keyName := creds.ProviderKeyName(provider)
	if credentialErr != nil {
		return fmt.Sprintf("model %s could not be resolved; credentials could not be loaded: %v (run openroutines check)", model, credentialErr)
	}
	if !injected {
		return fmt.Sprintf("model %s could not be resolved; no %s was available to the run (run openroutines check)", model, keyName)
	}
	return fmt.Sprintf("model %s could not be resolved even though %s was available -- verify the model name and provider configuration", model, keyName)
}

func isModelNotFound(failure string) bool {
	return strings.Contains(failure, "ProviderModelNotFoundError")
}

type runSecrets struct {
	env           map[string]string
	cleanup       []func()
	credentialErr error
}

func (s *runSecrets) setEnv(name, value string) error {
	if _, taken := s.env[name]; taken {
		return fmt.Errorf("credential grants set the %s environment variable twice", name)
	}
	s.env[name] = value
	return nil
}

func (s *runSecrets) release() {
	for _, f := range s.cleanup {
		f()
	}
	s.cleanup = nil
}

func resolveCredentials(dir string, agent *config.Agent, r *routine.Routine, model string) (_ *runSecrets, err error) {
	provider := strings.SplitN(model, "/", 2)[0]
	providerKey := creds.ProviderKeyName(provider)
	out := &runSecrets{env: map[string]string{}}
	defer func() {
		if err != nil {
			out.release()
		}
	}()

	_, store, credentialErr := creds.Load(dir)
	if credentialErr != nil {
		if len(r.Frontmatter.Credentials) > 0 {
			return nil, fmt.Errorf("routine declares credentials but %w", credentialErr)
		}
		out.credentialErr = credentialErr
		return out, nil
	}
	for _, name := range r.Frontmatter.Credentials {
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
