package runner

import (
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/routine"
)

// genDefWithMCP renders a definition for an agent whose opencode.json
// defines two MCP servers.
func genDefWithMCP(t *testing.T, meta Meta, fm routine.Frontmatter) string {
	t.Helper()
	r := &routine.Routine{Name: "x", FM: fm}
	def, err := renderDefinition(&config.Agent{Name: "a", Description: "d"}, r, []string{"slack", "steady"}, meta)
	if err != nil {
		t.Fatal(err)
	}
	return def
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

// An agent with no opencode.json (or no mcp block) renders no MCP rules
// -- the feature is invisible to agents that don't use it.
func TestNoMCPConfigNoRules(t *testing.T) {
	def := genDef(t, Meta{RunID: "run_t"})
	if strings.Contains(def, "_*") {
		t.Fatalf("unexpected MCP rules without mcp config:\n%s", def)
	}
}

// RenderDefinition carries the caller's server list into the rules -- check
// previously rendered from an empty directory, so the MCP rules it claimed
// to validate never existed in its output.
func TestRenderDefinitionCoversMCPRules(t *testing.T) {
	agent := &config.Agent{Name: "a", Description: "d"}
	r := &routine.Routine{Name: "x", FM: routine.Frontmatter{MCP: []string{"steady"}}}
	def, err := RenderDefinition(agent, r, []string{"slack", "steady"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(def, `"steady_*": allow`) || !strings.Contains(def, `"slack_*": deny`) {
		t.Fatalf("rendered definition missing MCP rules:\n%s", def)
	}
}
