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

// Dev-session rules files must never reach the run workspace: opencode would
// load a project-root AGENTS.md into run context, and that file is written
// for humans' coding agents, not routines.
func TestWorkspaceExcludesDevRulesFiles(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"AGENTS.md", "CLAUDE.md", "agent.yaml"} {
		os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644)
	}
	workspace := t.TempDir()
	if err := buildWorkspace(dir, workspace); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"AGENTS.md", "CLAUDE.md"} {
		if _, err := os.Stat(filepath.Join(workspace, f)); !os.IsNotExist(err) {
			t.Fatalf("%s leaked into the run workspace", f)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, "agent.yaml")); err != nil {
		t.Fatal("agent.yaml should travel into the workspace")
	}
}

// The standing instruction renders from embedded instruction.md; every
// conditional block must appear exactly when its flag is set, and no
// template syntax may leak into the prompt.
func TestInstructionRendering(t *testing.T) {
	agent := &config.Agent{Name: "test-agent", Description: "Tests things"}
	render := func(fm routine.Frontmatter, dry bool) string {
		t.Helper()
		ws := t.TempDir()
		r := &routine.Routine{Name: "sample", FM: fm}
		if err := writeAgentDefinition(ws, agent, r, Meta{RunID: "run_x", DryRun: dry}); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(ws, ".opencode", "agents", "routine.md"))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	agent.Variables = map[string]string{"product_repo": "acme/widgets", "docs_url": "https://docs.example.com"}
	off := false
	full := render(routine.Frontmatter{Consumes: "memory"}, true)
	for _, want := range []string{
		"You are test-agent",
		"routine \"sample\" (run run_x)",
		"DRY RUN",
		"memory/ledgers/sample.md",
		"Every run appends at least one event",
		"Full facts with real links",
		"./inbox.md",
		"./CONSUMED",
		"$DOCS_URL, $PRODUCT_REPO",
	} {
		if !strings.Contains(full, want) {
			t.Fatalf("instruction missing %q:\n%s", want, full)
		}
	}
	if strings.Contains(full, "{{") {
		t.Fatalf("template syntax leaked into instruction:\n%s", full)
	}
	plain := render(routine.Frontmatter{Events: &off}, false)
	for _, banned := range []string{"DRY RUN", "Every run appends", "Delivery inbox"} {
		if strings.Contains(plain, banned) {
			t.Fatalf("conditional block %q rendered when its flag was off:\n%s", banned, plain)
		}
	}
	agent.Variables = nil
	if got := render(routine.Frontmatter{}, false); strings.Contains(got, "configuration variables") {
		t.Fatalf("variables block rendered with no variables configured:\n%s", got)
	}
}
