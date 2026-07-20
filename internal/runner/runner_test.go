package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/routine"
)

func genDef(t *testing.T, meta Meta) string {
	t.Helper()
	ws := t.TempDir()
	agent := &config.Agent{Name: "a", Description: "d"}
	r := &routine.Routine{Name: "x", FM: routine.Frontmatter{Skills: []string{"s1"}}}
	if err := writeAgentDefinition(ws, agent, r, meta); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(ws, ".opencode", "agents", "routine.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestDryRunDefinitionDeniesActingTools(t *testing.T) {
	def := genDef(t, Meta{RunID: "run_t", DryRun: true})
	for _, want := range []string{"bash: deny", "webfetch: deny", "websearch: deny", "DRY RUN", `"s1": allow`} {
		if !strings.Contains(def, want) {
			t.Fatalf("dry-run definition missing %q:\n%s", want, def)
		}
	}
}

func TestRealRunDefinitionAllowsActing(t *testing.T) {
	def := genDef(t, Meta{RunID: "run_t"})
	for _, banned := range []string{"bash: deny", "webfetch: deny", "DRY RUN"} {
		if strings.Contains(def, banned) {
			t.Fatalf("real-run definition wrongly contains %q:\n%s", banned, def)
		}
	}
	if !strings.Contains(def, `"*": deny`) || !strings.Contains(def, `"s1": allow`) {
		t.Fatalf("skill scoping missing:\n%s", def)
	}
}
