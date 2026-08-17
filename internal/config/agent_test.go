package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/creds"
)

func TestInstructionsOptional(t *testing.T) {
	a := Agent{
		Name:     "a",
		Owner:    Owner{Email: "o@example.com"},
		Timezone: "UTC",
		Defaults: Defaults{Model: "anthropic/claude-sonnet-5"},
	}
	if p := a.Problems(); len(p) != 0 {
		t.Fatalf("unset instructions flagged: %v", p)
	}
	a.Instructions = "{{JOB_DESCRIPTION}}"
	if p := a.Problems(); len(p) != 1 || !strings.Contains(p[0], "instructions") {
		t.Fatalf("placeholder instructions: want one problem, got %v", p)
	}
}

func TestRepoConfig(t *testing.T) {
	dir := t.TempDir()
	raw := `name: a
repo: acme/agent
owner:
  email: o@example.com
timezone: UTC
defaults:
  model: anthropic/claude-sonnet-5
`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := a.Repo, "acme/agent"; got != want {
		t.Fatalf("repo = %q, want %q", got, want)
	}
	if problems := a.Problems(); len(problems) != 0 {
		t.Fatalf("valid repository flagged: %v", problems)
	}

	a.Repo = ""
	if problems := a.Problems(); len(problems) != 0 {
		t.Fatalf("omitted repository flagged: %v", problems)
	}
}

func TestVariableNameValidation(t *testing.T) {
	base := Agent{
		Name:         "a",
		Instructions: "d",
		Owner:        Owner{Email: "o@example.com"},
		Timezone:     "UTC",
		Defaults:     Defaults{Model: "anthropic/claude-sonnet-5"},
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

func TestMaxTimeoutCeiling(t *testing.T) {
	a := Agent{
		Name:         "a",
		Instructions: "d",
		Owner:        Owner{Email: "o@example.com"},
		Timezone:     "UTC",
		Defaults:     Defaults{Model: "anthropic/claude-sonnet-5"},
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

func TestConcurrencyConfig(t *testing.T) {
	a := Agent{
		Name:         "a",
		Instructions: "d",
		Owner:        Owner{Email: "o@example.com"},
		Timezone:     "UTC",
		Defaults:     Defaults{Model: "anthropic/claude-sonnet-5"},
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

func TestPathResolvesLegacySpellings(t *testing.T) {
	dir := t.TempDir()
	if got := filepath.Base(Path(dir)); got != FileName {
		t.Fatalf("fresh dir should resolve to %s (the name a write creates), got %s", FileName, got)
	}
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

func TestSaveUsesTwoSpaceIndent(t *testing.T) {
	dir := t.TempDir()
	a := Agent{
		Name:         "a",
		Instructions: "d",
		Repo:         "acme/agent",
		Owner:        Owner{Name: "o", Email: "o@example.com"},
		Timezone:     "UTC",
		Defaults:     Defaults{Model: "anthropic/claude-sonnet-5"},
		Variables:    map[string]string{"product_repo": "acme/widgets"},
	}
	if err := a.Save(dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"repo: acme/agent", "owner:\n  name: o", "defaults:\n  model: anthropic/claude-sonnet-5", "variables:\n  product_repo: acme/widgets"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("saved yaml should nest with 2 spaces, missing %q:\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), "    ") {
		t.Fatalf("saved yaml contains 4-space indentation:\n%s", raw)
	}
}

func TestSavePreservesComments(t *testing.T) {
	dir := t.TempDir()
	raw := "# Required. Agent identity.\nname: old\nowner:\n  # Optional. Owner name.\n  name: old owner\n"
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	a.Name = "new"
	a.Owner.Name = "new owner"
	if err := a.Save(dir); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Required. Agent identity.\nname: new", "# Optional. Owner name.\n  name: new owner"} {
		if !strings.Contains(string(saved), want) {
			t.Fatalf("saved YAML lost comment %q:\n%s", want, saved)
		}
	}
}

func TestProblemsAllowsOwnerToBeUnset(t *testing.T) {
	a := Agent{
		Name:     "agent",
		Timezone: "UTC",
		Defaults: Defaults{Model: "provider/model"},
	}
	if problems := a.Problems(); len(problems) != 0 {
		t.Fatalf("unset optional owner reported as a problem: %v", problems)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, FileName), []byte("name: a\ndescriptoin: typo\n"), 0o644) //nolint:misspell // deliberate: strict decoding must name the typo
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "descriptoin") {     //nolint:misspell // deliberate: asserts the typo is named
		t.Fatalf("expected unknown-field error naming the typo, got %v", err)
	}
}

func TestCredentialEntryValidation(t *testing.T) {
	base := Agent{
		Name:         "a",
		Instructions: "d",
		Owner:        Owner{Email: "o@example.com"},
		Timezone:     "UTC",
		Defaults:     Defaults{Model: "anthropic/claude-sonnet-5"},
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
