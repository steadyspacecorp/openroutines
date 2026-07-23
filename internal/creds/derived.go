package creds

import (
	"fmt"
	"regexp"
)

// Spec declares how a stored credential is materialized into a run (see
// DESIGN.md "Credentials have types"). A credential with no Spec is raw --
// injected verbatim under its uppercase name. A typed credential is
// transformed by the trusted runner at spawn: the routine receives the
// derived surface, never the stored root secret.
type Spec struct {
	Type  string `yaml:"type"`
	AppID string `yaml:"app_id,omitempty"`
}

// Derived is run-scoped material minted from a stored root secret: the
// exact environment to inject, values the log scrubber must redact, and
// cleanup that disposes of anything revocable. Cleanup is best-effort and
// safe to call once at attempt end.
type Derived struct {
	Env     map[string]string
	Scrub   map[string]string
	Cleanup func()
}

var appIDPattern = regexp.MustCompile(`^[0-9]+$`)

// SpecProblems returns human-readable validation failures for one credential
// metadata entry, empty when valid.
func SpecProblems(name string, s Spec) []string {
	var out []string
	switch s.Type {
	case "":
		out = append(out, fmt.Sprintf("credential %q: metadata entry has no type -- omit the entry entirely for a raw credential", name))
	case "github_app":
		if !appIDPattern.MatchString(s.AppID) {
			out = append(out, fmt.Sprintf("credential %q: type github_app requires a numeric app_id", name))
		}
	default:
		out = append(out, fmt.Sprintf("credential %q: unknown type %q (supported: github_app)", name, s.Type))
	}
	return out
}

// Derive materializes one typed credential. Providers are built into the
// framework -- agent repositories cannot supply derivation code, which would
// otherwise be a privileged plugin boundary on the trusted side.
func Derive(name string, s Spec, stored string) (*Derived, error) {
	switch s.Type {
	case "github_app":
		return deriveGitHubApp(s, stored, githubAPIBase)
	default:
		return nil, fmt.Errorf("credential %q: unknown derived type %q", name, s.Type)
	}
}
