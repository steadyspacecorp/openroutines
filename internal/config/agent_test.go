package config

import (
	"strings"
	"testing"
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
