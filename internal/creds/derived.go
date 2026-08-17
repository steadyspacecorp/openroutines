package creds

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/steadyspacecorp/openroutines/internal/scrub"
)

// A typed credential is transformed by the trusted runner at spawn; the run
// receives only the derived surface, never the stored root secret. A missing
// Spec remains raw and is injected verbatim under its uppercase name.
type Spec struct {
	Type  string `yaml:"type"`
	AppID string `yaml:"app_id,omitempty"`

	TokenURL string `yaml:"token_url,omitempty"`
	ClientID string `yaml:"client_id,omitempty"`
	InjectAs string `yaml:"inject_as,omitempty"`
}

type Derived struct {
	Env     map[string]string
	Bearer  string
	Cleanup func()
}

var appIDPattern = regexp.MustCompile(`^[0-9]+$`)

var DerivedTypes = []string{"github_app", "oauth2_client"}

func KnownType(t string) bool {
	return slices.Contains(DerivedTypes, t)
}

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

func ValidateStored(s Spec, stored string) error {
	if s.Type == "github_app" {
		_, err := parseAppKey(stored)
		return err
	}
	return nil
}

// Providers are framework-owned so agent repositories cannot supply derivation
// code on the trusted side; every bearer leaves through the scrub registry.
func Derive(name string, s Spec, stored string) (*Derived, error) {
	var d *Derived
	var err error
	switch s.Type {
	case "github_app":
		d, err = deriveGitHubApp(name, s, stored, githubAPIBase)
	case "oauth2_client":
		d, err = deriveOAuth2Client(s, stored)
	default:
		return nil, fmt.Errorf("credential %q: unknown derived type %q", name, s.Type)
	}
	if err != nil {
		return nil, err
	}
	// Ephemeral, not named: concurrent runs minting the same credential hold
	// distinct bearers, and registering the second must not stop redacting
	// the first. Cleanup revokes before it releases, so anything the
	// revocation logs still redacts.
	release := scrub.RegisterEphemeral(s.Type+" bearer ("+name+")", d.Bearer)
	revoke := d.Cleanup
	d.Cleanup = func() {
		revoke()
		release()
	}
	return d, nil
}
