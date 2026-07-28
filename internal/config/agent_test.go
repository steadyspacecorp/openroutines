package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/creds"
)

// Variable names must map cleanly onto env vars and never shadow what the
// framework itself sets.
func TestVariableNameValidation(t *testing.T) {
	base := Agent{
		Name:        "a",
		Description: "d",
		Owner:       Owner{Email: "o@example.com"},
		Timezone:    "UTC",
		Defaults:    Defaults{Model: "anthropic/claude-sonnet-5"},
	}
	valid := base
	valid.Variables = map[string]string{"product_repo": "acme/widgets"}
	if p := valid.Problems(); len(p) != 0 {
		t.Fatalf("valid variables flagged: %v", p)
	}
	for name, wantErr := range map[string]string{
		"Bad-Name":       "snake_case",
		"openroutines_x": "reserved",
		"path":           "environment variable",
		"home":           "environment variable",
	} {
		a := base
		a.Variables = map[string]string{name: "v"}
		p := a.Problems()
		if len(p) != 1 || !strings.Contains(p[0], wantErr) {
			t.Fatalf("variable %q: want one problem containing %q, got %v", name, wantErr, p)
		}
	}
}

// The configuration file resolves newest spelling first: .yml, then the
// legacy .yaml, then the original agent.yaml -- all read, so a pinned
// agent renames on its own schedule (#50).
func TestPathResolvesLegacySpellings(t *testing.T) {
	dir := t.TempDir()
	if got := filepath.Base(Path(dir)); got != FileName {
		t.Fatalf("fresh dir should resolve to %s (the name a write creates), got %s", FileName, got)
	}
	// Oldest first: each newer spelling takes precedence once present.
	names := make([]string, 0, len(LegacyFileNames)+1)
	names = append(names, FileName)
	names = append(names, LegacyFileNames...)
	for i := len(names) - 1; i >= 0; i-- {
		if err := os.WriteFile(filepath.Join(dir, names[i]), []byte("name: a\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := filepath.Base(Path(dir)); got != names[i] {
			t.Fatalf("with %v present, Path should resolve %s, got %s", names[i:], names[i], got)
		}
		if _, err := Load(dir); err != nil {
			t.Fatalf("%s should load: %v", names[i], err)
		}
	}
}

// openroutines.yml decodes strictly: a misspelled key is an error, not silently
// ignored configuration.
func TestLoadRejectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, FileName), []byte("name: a\ndescriptoin: typo\n"), 0o644) //nolint:misspell // deliberate: strict decoding must name the typo
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "descriptoin") {     //nolint:misspell // deliberate: asserts the typo is named
		t.Fatalf("expected unknown-field error naming the typo, got %v", err)
	}
}

// Credential metadata entries are validated like the rest of openroutines.yml:
// a typed entry needs a known type and its type's configuration.
func TestCredentialEntryValidation(t *testing.T) {
	base := Agent{
		Name:        "a",
		Description: "d",
		Owner:       Owner{Email: "o@example.com"},
		Timezone:    "UTC",
		Defaults:    Defaults{Model: "anthropic/claude-sonnet-5"},
	}
	valid := base
	valid.Credentials = map[string]creds.Spec{"github_app_private_key": {Type: "github_app", AppID: "4361572"}}
	if p := valid.Problems(); len(p) != 0 {
		t.Fatalf("valid credential entry flagged: %v", p)
	}
	for name, entry := range map[string]struct {
		key     string
		spec    creds.Spec
		wantErr string
	}{
		"bad entry name": {"Bad-Name", creds.Spec{Type: "github_app", AppID: "1"}, "snake_case"},
		"missing type":   {"gh", creds.Spec{}, "no type"},
		"unknown type":   {"gh", creds.Spec{Type: "aws_sts"}, "unknown type"},
		"bad app id":     {"gh", creds.Spec{Type: "github_app", AppID: "abc"}, "numeric app_id"},
	} {
		a := base
		a.Credentials = map[string]creds.Spec{entry.key: entry.spec}
		p := a.Problems()
		if len(p) != 1 || !strings.Contains(p[0], entry.wantErr) {
			t.Fatalf("%s: want one problem containing %q, got %v", name, entry.wantErr, p)
		}
	}
}
