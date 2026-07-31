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

// max_timeout is the agent-wide run ceiling: parsed when set, the default
// when absent, a reported problem (never fail-open to unlimited) when junk.
func TestMaxTimeoutCeiling(t *testing.T) {
	a := Agent{
		Name:        "a",
		Description: "d",
		Owner:       Owner{Email: "o@example.com"},
		Timezone:    "UTC",
		Defaults:    Defaults{Model: "anthropic/claude-sonnet-5"},
	}
	if got := a.MaxRunTimeout(); got != DefaultMaxTimeout {
		t.Fatalf("unset max_timeout = %s, want the %s default", got, DefaultMaxTimeout)
	}
	a.MaxTimeout = "12h"
	if p := a.Problems(); len(p) != 0 {
		t.Fatalf("valid max_timeout flagged: %v", p)
	}
	if got := a.MaxRunTimeout(); got.Hours() != 12 {
		t.Fatalf("max_timeout 12h parsed as %s", got)
	}
	for _, bad := range []string{"six hours", "-1h", "0"} {
		a.MaxTimeout = bad
		if p := a.Problems(); len(p) != 1 || !strings.Contains(p[0], "max_timeout") {
			t.Fatalf("max_timeout %q: want one problem, got %v", bad, p)
		}
		if got := a.MaxRunTimeout(); got != DefaultMaxTimeout {
			t.Fatalf("junk max_timeout %q must fall back to the default, got %s", bad, got)
		}
	}
}

// concurrency is the run-slot count: unset and 0 both mean serial (an
// existing agent must not gain parallelism on upgrade -- the scaffold
// template is what opts new agents in), and a negative value is a problem.
func TestConcurrencyConfig(t *testing.T) {
	a := Agent{
		Name:        "a",
		Description: "d",
		Owner:       Owner{Email: "o@example.com"},
		Timezone:    "UTC",
		Defaults:    Defaults{Model: "anthropic/claude-sonnet-5"},
	}
	if got := a.RunSlots(); got != 1 {
		t.Fatalf("unset concurrency = %d, want serial", got)
	}
	a.Concurrency = 0
	if got := a.RunSlots(); got != 1 {
		t.Fatalf("concurrency 0 = %d, want serial", got)
	}
	if p := a.Problems(); len(p) != 0 {
		t.Fatalf("zero concurrency flagged: %v", p)
	}
	a.Concurrency = 4
	if p := a.Problems(); len(p) != 0 {
		t.Fatalf("valid concurrency flagged: %v", p)
	}
	if got := a.RunSlots(); got != 4 {
		t.Fatalf("concurrency 4 read as %d", got)
	}
	a.Concurrency = -1
	if p := a.Problems(); len(p) != 1 || !strings.Contains(p[0], "concurrency") {
		t.Fatalf("negative concurrency: want one problem, got %v", p)
	}
	if got := a.RunSlots(); got != 1 {
		t.Fatalf("negative concurrency must fall back to serial, got %d", got)
	}
	a.Concurrency = MaxConcurrency + 1
	if p := a.Problems(); len(p) != 1 || !strings.Contains(p[0], "maximum") {
		t.Fatalf("excessive concurrency: want one maximum problem, got %v", p)
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

// Save keeps the scaffold's 2-space indentation: this file is hand-edited,
// reviewed config, and configure must not reformat it (#65).
func TestSaveUsesTwoSpaceIndent(t *testing.T) {
	dir := t.TempDir()
	a := Agent{
		Name:        "a",
		Description: "d",
		Owner:       Owner{Name: "o", Email: "o@example.com"},
		Timezone:    "UTC",
		Defaults:    Defaults{Model: "anthropic/claude-sonnet-5"},
		Variables:   map[string]string{"product_repo": "acme/widgets"},
	}
	if err := a.Save(dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"owner:\n  name: o", "defaults:\n  model: anthropic/claude-sonnet-5", "variables:\n  product_repo: acme/widgets"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("saved yaml should nest with 2 spaces, missing %q:\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), "    ") {
		t.Fatalf("saved yaml contains 4-space indentation:\n%s", raw)
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
