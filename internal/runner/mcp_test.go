package runner

import (
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/routine"
)

func genDefWithMCP(t *testing.T, attempt Attempt, fm routine.Frontmatter) string {
	t.Helper()
	r := &routine.Routine{Name: "x", Frontmatter: fm}
	def, err := renderDefinition(&config.Agent{Name: "a", Instructions: "d"}, r, []string{"slack", "steady"}, attempt)
	if err != nil {
		t.Fatal(err)
	}
	return def
}

func TestMCPServersDenyByDefault(t *testing.T) {
	def := genDefWithMCP(t, Attempt{RunID: "run_t"}, routine.Frontmatter{})
	for _, want := range []string{`"slack_*": deny`, `"steady_*": deny`} {
		if !strings.Contains(def, want) {
			t.Fatalf("definition missing %q:\n%s", want, def)
		}
	}
}

func TestMCPGrantOpensOnlyNamedServer(t *testing.T) {
	def := genDefWithMCP(t, Attempt{RunID: "run_t"}, routine.Frontmatter{MCP: []string{"steady"}})
	if !strings.Contains(def, `"steady_*": allow`) {
		t.Fatalf("granted server not allowed:\n%s", def)
	}
	if !strings.Contains(def, `"slack_*": deny`) {
		t.Fatalf("ungranted server not denied:\n%s", def)
	}
}

func TestNoMCPConfigNoRules(t *testing.T) {
	def := genDef(t, Attempt{RunID: "run_t"})
	if strings.Contains(def, "_*") {
		t.Fatalf("unexpected MCP rules without mcp config:\n%s", def)
	}
}

func TestRenderDefinitionCoversMCPRules(t *testing.T) {
	agent := &config.Agent{Name: "a", Instructions: "d"}
	r := &routine.Routine{Name: "x", Frontmatter: routine.Frontmatter{MCP: []string{"steady"}}}
	def, err := RenderDefinition(agent, r, []string{"slack", "steady"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(def, `"steady_*": allow`) || !strings.Contains(def, `"slack_*": deny`) {
		t.Fatalf("rendered definition missing MCP rules:\n%s", def)
	}
}
