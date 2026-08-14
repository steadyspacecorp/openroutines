package runner

import (
	"encoding/json"
	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func genDef(t *testing.T, attempt Attempt, fm ...routine.Frontmatter) string {
	t.Helper()
	ws := t.TempDir()
	agent := &config.Agent{Name: "a", Instructions: "d"}
	front := routine.Frontmatter{Skills: []string{"s1"}}
	if len(fm) > 0 {
		front = fm[0]
	}
	r := &routine.Routine{Name: "x", Frontmatter: front}
	if err := writeAgentDefinition(ws, agent, r, nil, attempt); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(ws, ".opencode", "agents", "routine.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestReadOnlyDefinitionDeniesActingAndMutationTools(t *testing.T) {
	def := genDef(t, Attempt{ReadOnly: true})
	for _, want := range []string{"\"*\": deny", "read: allow", "glob: allow", "grep: allow", "webfetch: deny", "websearch: deny"} {
		if !strings.Contains(def, want) {
			t.Fatalf("read-only definition missing %q:\n%s", want, def)
		}
	}
}

func TestRunDefinitionAllowsActing(t *testing.T) {
	def := genDef(t, Attempt{RunID: "run_t"})
	if strings.Contains(def, "bash: deny") {
		t.Fatalf("run definition wrongly denies acting:\n%s", def)
	}
	if !strings.Contains(def, `"*": deny`) || !strings.Contains(def, `"s1": allow`) {
		t.Fatalf("skill scoping missing:\n%s", def)
	}
}

// Web access is deny-by-default in every generated definition: opencode
// allows webfetch out of the box, and fetched content is model context --
// a prompt-injection vector. The rule must be explicit either way, so a
// harness default change can never silently widen a routine's reach.
func TestWebAccessDeniedByDefault(t *testing.T) {
	def := genDef(t, Attempt{RunID: "run_t"})
	for _, want := range []string{"webfetch: deny", "websearch: deny"} {
		if !strings.Contains(def, want) {
			t.Fatalf("definition missing %q:\n%s", want, def)
		}
	}
}

func TestWebAccessOptIn(t *testing.T) {
	fm := routine.Frontmatter{Webfetch: true, Websearch: true}
	def := genDef(t, Attempt{RunID: "run_t"}, fm)
	for _, want := range []string{"webfetch: allow", "websearch: allow"} {
		if !strings.Contains(def, want) {
			t.Fatalf("opted-in definition missing %q:\n%s", want, def)
		}
	}
}

// The workspace is built by allow-list: exactly openroutines.yml,
// opencode.json, and routines/ travel in. This is the audit's headline test --
// no secret-shaped file (the encrypted store, keys) and no dev-session rules
// file (AGENTS.md/CLAUDE.md, which opencode would load into run context) may
// ever reach a run.
func TestBuildWorkspaceAllowList(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"openroutines.yml":  "name: t\n",
		"opencode.json":     "{}",
		"routines/daily.md": "---\nschedule: \"0 9 * * *\"\n---\nwork",
		".openroutines/plugins/demo/routines/plugin-daily.md": "---\nschedule: \"0 10 * * *\"\n---\nplugin work",
		".openroutines/credentials.yml.enc":                   "ORV1:ciphertext",
		"master.key":                                          "hex",
		"agent_deploy_key":                                    "PRIVATE KEY",
		"AGENTS.md":                                           "dev rules",
		"CLAUDE.md":                                           "dev rules",
		"README.md":                                           "docs",
		"Dockerfile":                                          "FROM x",
		".openroutines/version":                               "v0",
		"skills/s1/SKILL.md":                                  "skill",
		"knowledge/events.md":                                 "events",
		".git/config":                                         "git",
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	workspace := t.TempDir()
	if err := buildWorkspace(dir, workspace, "daily"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"openroutines.yml", "opencode.json", "routines/daily.md", "routines/plugin-daily.md"} {
		if _, err := os.Stat(filepath.Join(workspace, f)); err != nil {
			t.Errorf("%s should travel into the workspace: %v", f, err)
		}
	}
	entries, err := os.ReadDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"openroutines.yml": true, "opencode.json": true, "routines": true}
	for _, e := range entries {
		if !allowed[e.Name()] {
			t.Errorf("%s leaked into the run workspace", e.Name())
		}
	}
}

// One unparseable file is one broken routine, not a broken agent: a healthy
// routine assembles a workspace without it. The broken routine's own run
// still fails, and with the real reason.
func TestBuildWorkspaceIsolatesOtherRoutinesParseErrors(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"openroutines.yaml":                              "name: t\n",
		"routines/daily.md":                              "---\nschedule: \"0 9 * * *\"\n---\nwork",
		"routines/typo.md":                               "---\nschedule: \"0 9 * * *\"\nactve: false\n---\nbroken",
		"routines/twin.md":                               "---\nschedule: \"0 9 * * *\"\n---\nmine",
		".openroutines/plugins/demo/routines/twin.md":    "---\nschedule: \"0 9 * * *\"\n---\ntheirs",
		".openroutines/plugins/demo/routines/plugged.md": "---\nschedule: \"0 10 * * *\"\n---\nplugin work",
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	workspace := t.TempDir()
	if err := buildWorkspace(dir, workspace, "daily"); err != nil {
		t.Fatalf("a sibling's parse error must not fail this routine's run: %v", err)
	}
	for _, f := range []string{"routines/daily.md", "routines/plugged.md", "routines/twin.md"} {
		if _, err := os.Stat(filepath.Join(workspace, f)); err != nil {
			t.Errorf("%s should travel into the workspace: %v", f, err)
		}
	}
	for _, f := range []string{"routines/typo.md"} {
		if _, err := os.Stat(filepath.Join(workspace, f)); err == nil {
			t.Errorf("%s does not load and must not travel into the workspace", f)
		}
	}

	if err := buildWorkspace(dir, t.TempDir(), "typo"); err == nil {
		t.Error("the broken routine's own run must fail")
	} else if !strings.Contains(err.Error(), "frontmatter") {
		t.Errorf("want the parse error, got %v", err)
	}
	if err := buildWorkspace(dir, t.TempDir(), "twin"); err != nil {
		t.Errorf("the agent-owned routine should override its plugin namesake: %v", err)
	}
}

// An ungranted MCP server's entry does not travel into the workspace's
// opencode.json: the run's opencode never contacts it, so an ungranted run
// cannot probe the endpoint or log its needs_auth refusal. Granted entries
// and every other block pass through.
func TestApplyDeclaredMCPFiltersUngrantedServers(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"openroutines.yml":  "name: t\n",
		"opencode.json":     `{"mcp":{"steady":{"type":"remote","url":"https://example.com/mcp"},"other":{"type":"remote","url":"https://example.org/mcp"}},"provider":{"openrouter":{"options":{"baseURL":"https://example.net/v1"}}}}`,
		"routines/daily.md": "---\nschedule: \"0 9 * * *\"\n---\nwork",
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	load := func(workspace string) map[string]any {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(workspace, "opencode.json"))
		if err != nil {
			t.Fatal(err)
		}
		var cfg map[string]any
		if err := json.Unmarshal(raw, &cfg); err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	workspace := t.TempDir()
	if err := buildWorkspace(dir, workspace, "daily"); err != nil {
		t.Fatal(err)
	}
	if err := applyDeclaredMCP(workspace, []string{"steady"}); err != nil {
		t.Fatal(err)
	}
	cfg := load(workspace)
	mcp, _ := cfg["mcp"].(map[string]any)
	if _, ok := mcp["steady"]; !ok {
		t.Error("the granted server must travel into the workspace config")
	}
	if _, ok := mcp["other"]; ok {
		t.Error("an ungranted server must not travel into the workspace config")
	}
	if _, ok := cfg["provider"]; !ok {
		t.Error("blocks other than mcp must pass through")
	}

	bare := t.TempDir()
	if err := buildWorkspace(dir, bare, "daily"); err != nil {
		t.Fatal(err)
	}
	if err := applyDeclaredMCP(bare, nil); err != nil {
		t.Fatal(err)
	}
	cfg = load(bare)
	if _, ok := cfg["mcp"]; ok {
		t.Error("a run with no MCP grants gets no mcp block at all")
	}
	if _, ok := cfg["provider"]; !ok {
		t.Error("blocks other than mcp must pass through")
	}
}

// The standing instruction renders from embedded instruction.md; every
// conditional block must appear exactly when its flag is set, and no
// template syntax may leak into the prompt.
func TestInstructionRendering(t *testing.T) {
	agent := &config.Agent{Name: "test-agent", Instructions: "Tests things"}
	render := func(fm routine.Frontmatter) string {
		t.Helper()
		ws := t.TempDir()
		r := &routine.Routine{Name: "sample", Frontmatter: fm}
		if err := writeAgentDefinition(ws, agent, r, nil, Attempt{RunID: "run_x"}); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(ws, ".opencode", "agents", "routine.md"))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	agent.Variables = map[string]string{"product_repo": "acme/widgets", "docs_url": "https://docs.example.com"}
	full := render(routine.Frontmatter{Reports: true, Teamwork: routine.TeamworkFull})
	for _, want := range []string{
		"You are test-agent",
		"Your standing instructions: Tests things",
		"routine \"sample\" (run run_x)",
		"knowledge/ledgers/sample.md",
		"Every run appends at least one event",
		"Full facts with real links",
		"./changes.md",
		"knowledge/CONSUMED",
		"$DOCS_URL, $PRODUCT_REPO",
		"$TMPDIR",
	} {
		if !strings.Contains(full, want) {
			t.Fatalf("instruction missing %q:\n%s", want, full)
		}
	}
	if strings.Contains(full, "{{") {
		t.Fatalf("template syntax leaked into instruction:\n%s", full)
	}
	for _, want := range []string{"it happened -> append an event", "state, not a log"} {
		if !strings.Contains(full, want) {
			t.Fatalf("instruction missing %q:\n%s", want, full)
		}
	}
	if strings.Contains(full, "does not record events") {
		t.Fatalf("no-events rule rendered for an events-recording routine:\n%s", full)
	}
	plain := render(routine.Frontmatter{Teamwork: routine.TeamworkOff})
	for _, banned := range []string{"Every run appends", "This routine reports", "append an event to knowledge/events.md"} {
		if strings.Contains(plain, banned) {
			t.Fatalf("conditional block %q rendered when its flag was off:\n%s", banned, plain)
		}
	}
	for _, want := range []string{"does not record events", "never write to knowledge/events.md"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("teamwork: off instruction missing %q:\n%s", want, plain)
		}
	}
	// reports: true with no explicit teamwork defaults to off: the reporting
	// block renders, the event-recording one does not.
	reporter := render(routine.Frontmatter{Reports: true})
	if !strings.Contains(reporter, "This routine reports") {
		t.Fatalf("reports: true should render the reporting block:\n%s", reporter)
	}
	if strings.Contains(reporter, "Every run appends") {
		t.Fatalf("reports: true alone should imply teamwork: off, not record events:\n%s", reporter)
	}
	agent.Variables = nil
	if got := render(routine.Frontmatter{}); strings.Contains(got, "configuration variables") {
		t.Fatalf("variables block rendered with no variables configured:\n%s", got)
	}
	agent.Instructions = ""
	if got := render(routine.Frontmatter{}); strings.Contains(got, "standing instructions") || !strings.Contains(got, "You are test-agent, an autonomous agent.\n") {
		t.Fatalf("unset instructions must render a bare identity line:\n%s", got)
	}
}

// A frontmatter skill name is attacker-influencable repo content and becomes
// a filesystem path in the run pipeline: traversal names must be rejected
// before any path construction.
func TestCopyDeclaredSkillsRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "victim")
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "SKILL.md"), []byte("x"), 0o644)
	for _, bad := range []string{"../victim", "../../x", "/abs", "a/b", ".."} {
		if err := copyDeclaredSkills(filepath.Join(dir, "agent"), t.TempDir(), []string{bad}); err == nil {
			t.Errorf("skill name %q should be rejected", bad)
		}
	}
}

func TestCopyDeclaredSkillsPreservesExecutableFiles(t *testing.T) {
	agent := t.TempDir()
	skillDir := filepath.Join(agent, "skills", "demo")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]fs.FileMode{
		"SKILL.md": 0o644,
		"run.sh":   0o755,
	} {
		if err := os.WriteFile(filepath.Join(skillDir, path), []byte("---\nname: demo\ndescription: demo\n---\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	workspace := t.TempDir()
	if err := copyDeclaredSkills(agent, workspace, []string{"demo"}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]fs.FileMode{
		"SKILL.md": 0o644,
		"run.sh":   0o755,
	} {
		info, err := os.Stat(filepath.Join(workspace, ".opencode", "skills", "demo", path))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", path, got, want)
		}
	}
}
