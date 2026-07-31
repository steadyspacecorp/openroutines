package creds

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Spec declares how a stored credential is materialized into a run (see
// design decision "Credentials have types"). A credential with no Spec is raw --
// injected verbatim under its uppercase name. A typed credential is
// transformed by the trusted runner at spawn: the routine receives the
// derived surface, never the stored root secret.
type Spec struct {
	Type  string `yaml:"type"`
	AppID string `yaml:"app_id,omitempty"` // github_app

	TokenURL string `yaml:"token_url,omitempty"` // oauth2_client
	ClientID string `yaml:"client_id,omitempty"` // oauth2_client
	InjectAs string `yaml:"inject_as,omitempty"` // oauth2_client
}

// Derived is short-lived material minted from a stored root secret: the
// exact environment to inject into a run, the bearer value (when the type
// produces one) available to trusted supervisor callers, values the log
// scrubber must redact, and cleanup that disposes of anything revocable.
// Cleanup is best-effort and safe to call once after the material's use.
type Derived struct {
	Env     map[string]string
	Bearer  string
	Scrub   map[string]string
	Cleanup func()
}

var appIDPattern = regexp.MustCompile(`^[0-9]+$`)

// DerivedTypes are the derived credential types the framework implements,
// in the order they shipped. Every validator that names the set consults
// this list -- two hardcoded copies drifted once already (the plugin
// validator missed oauth2_client).
var DerivedTypes = []string{"github_app", "oauth2_client"}

// KnownType reports whether t is an implemented derived credential type.
func KnownType(t string) bool {
	return slices.Contains(DerivedTypes, t)
}

// SpecProblems returns human-readable validation failures for one credential
// metadata entry, empty when valid. Fields another type owns are rejected,
// not ignored -- silently dead configuration is what strict decoding exists
// to prevent.
func SpecProblems(name string, s Spec) []string {
	var out []string
	problem := func(format string, a ...any) {
		out = append(out, fmt.Sprintf("credential %q: ", name)+fmt.Sprintf(format, a...))
	}
	switch s.Type {
	case "":
		problem("metadata entry has no type -- omit the entry entirely for a raw credential")
	case "github_app":
		if !appIDPattern.MatchString(s.AppID) {
			problem("type github_app requires a numeric app_id")
		}
		for field, v := range map[string]string{"token_url": s.TokenURL, "client_id": s.ClientID, "inject_as": s.InjectAs} {
			if v != "" {
				problem("%s is not part of type github_app", field)
			}
		}
	case "oauth2_client":
		if u, err := url.Parse(s.TokenURL); err != nil || u.Scheme != "https" || u.Host == "" {
			problem("type oauth2_client requires an https token_url")
		}
		if s.ClientID == "" {
			problem("type oauth2_client requires a client_id")
		}
		switch {
		case !NamePattern.MatchString(s.InjectAs):
			problem("type oauth2_client requires inject_as, a lowercase snake_case env name for the minted bearer")
		case strings.HasPrefix(s.InjectAs, ReservedPrefix) || ReservedEnvName(s.InjectAs):
			problem("inject_as %q collides with a reserved environment name", s.InjectAs)
		}
		if s.AppID != "" {
			problem("app_id is not part of type oauth2_client")
		}
	default:
		problem("unknown type %q (supported: %s)", s.Type, strings.Join(DerivedTypes, ", "))
	}
	return out
}

// InjectionDescription explains what a run actually receives when it
// declares this credential -- the credential's own uppercase name for a raw
// credential, or the derived surface for a typed one. Credential CLI output
// must describe the actual injection, not assume every credential is raw
// (issue #66): a github_app or oauth2_client entry never puts its stored
// value in the run environment.
func InjectionDescription(name string, s Spec) string {
	switch s.Type {
	case "github_app":
		return "(type: github_app) -- routines that declare it receive GITHUB_TOKEN, GH_TOKEN, GITHUB_APP_SLUG, and the App's git identity. The stored key is never injected."
	case "oauth2_client":
		return fmt.Sprintf("(type: oauth2_client) -- routines that declare it receive the minted bearer as %s. The stored client secret is never injected.", strings.ToUpper(s.InjectAs))
	default:
		return fmt.Sprintf("-- routines that declare it receive it as %s", strings.ToUpper(name))
	}
}

// ValidateStored reports whether a stored value can serve its declared
// type, judged from the value alone -- no network. A github_app value must
// parse as an RSA private key: a truncated paste otherwise decrypts cleanly,
// passes every name check, and first fails in a production run trying to
// sign with it. Other types' values are opaque bytes with nothing to judge.
func ValidateStored(s Spec, stored string) error {
	if s.Type == "github_app" {
		_, err := parseAppKey(stored)
		return err
	}
	return nil
}

// DerivedEnvNames returns the environment names Derive mints for s -- the
// static name surface of a typed credential, known without deriving anything.
// The run-environment plan validates collisions against these before any
// secret material moves; the derived-type tests pin each list to what Derive
// actually returns. Unknown types have no surface (SpecProblems rejects them).
func DerivedEnvNames(s Spec) []string {
	switch s.Type {
	case "github_app":
		return []string{"GITHUB_TOKEN", "GH_TOKEN", "GITHUB_APP_SLUG", "GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"}
	case "oauth2_client":
		if s.InjectAs == "" {
			return nil
		}
		return []string{strings.ToUpper(s.InjectAs)}
	}
	return nil
}

// Derive materializes one typed credential. Providers are built into the
// framework -- agent repositories cannot supply derivation code, which would
// otherwise be a privileged plugin boundary on the trusted side.
func Derive(name string, s Spec, stored string) (*Derived, error) {
	switch s.Type {
	case "github_app":
		return deriveGitHubApp(s, stored, githubAPIBase)
	case "oauth2_client":
		return deriveOAuth2Client(name, s, stored)
	default:
		return nil, fmt.Errorf("credential %q: unknown derived type %q", name, s.Type)
	}
}
