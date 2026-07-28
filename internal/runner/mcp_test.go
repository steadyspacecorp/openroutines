package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/routine"
)

// genDefWithMCP renders a definition in a workspace whose opencode.json
// defines two MCP servers -- the copied-in harness config the runner reads
// server names from.
func genDefWithMCP(t *testing.T, meta Meta, fm routine.Frontmatter) string {
	t.Helper()
	ws := t.TempDir()
	cfg := `{"mcp": {"steady": {"type": "remote", "url": "https://example.test/mcp"}, "slack": {"type": "remote", "url": "https://example.test/slack"}}}`
	if err := os.WriteFile(filepath.Join(ws, "opencode.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &routine.Routine{Name: "x", FM: fm}
	if err := writeAgentDefinition(ws, &config.Agent{Name: "a", Description: "d"}, r, meta); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(ws, ".opencode", "agents", "routine.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Every configured server gets an explicit rule, deny unless granted --
// the tools (and their third-party descriptions) never reach an ungranted
// routine's model.
func TestMCPServersDenyByDefault(t *testing.T) {
	def := genDefWithMCP(t, Meta{RunID: "run_t"}, routine.Frontmatter{})
	for _, want := range []string{`"slack_*": deny`, `"steady_*": deny`} {
		if !strings.Contains(def, want) {
			t.Fatalf("definition missing %q:\n%s", want, def)
		}
	}
}

// A grant opens exactly the named server; the others stay closed.
func TestMCPGrantOpensOnlyNamedServer(t *testing.T) {
	def := genDefWithMCP(t, Meta{RunID: "run_t"}, routine.Frontmatter{MCP: []string{"steady"}})
	if !strings.Contains(def, `"steady_*": allow`) {
		t.Fatalf("granted server not allowed:\n%s", def)
	}
	if !strings.Contains(def, `"slack_*": deny`) {
		t.Fatalf("ungranted server not denied:\n%s", def)
	}
}

// MCP tools act on external systems, which a rehearsal must not do: the
// grant does not apply to dry runs, structurally rather than by relying on
// withheld credentials to fail the connection.
func TestMCPDeniedInDryRunDespiteGrant(t *testing.T) {
	def := genDefWithMCP(t, Meta{RunID: "run_t", DryRun: true}, routine.Frontmatter{MCP: []string{"steady"}})
	if !strings.Contains(def, `"steady_*": deny`) || strings.Contains(def, `"steady_*": allow`) {
		t.Fatalf("dry run must deny granted MCP server:\n%s", def)
	}
}

// A workspace with no opencode.json (or no mcp block) renders no MCP rules
// -- the feature is invisible to agents that don't use it.
func TestNoMCPConfigNoRules(t *testing.T) {
	def := genDef(t, Meta{RunID: "run_t"})
	if strings.Contains(def, "_*") {
		t.Fatalf("unexpected MCP rules without mcp config:\n%s", def)
	}
}
