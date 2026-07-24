package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/memory"
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

// The workspace is built by allow-list: exactly agent.yaml, opencode.json,
// and routines/ travel in. This is the audit's headline test -- no
// secret-shaped file (the encrypted store, keys) and no dev-session rules
// file (AGENTS.md/CLAUDE.md, which opencode would load into run context)
// may ever reach a run.
func TestBuildWorkspaceAllowList(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"agent.yaml":            "name: t\n",
		"opencode.json":         "{}",
		"routines/daily.md":     "---\nschedule: \"0 9 * * *\"\n---\nwork",
		"credentials.yml.enc":   "ORV1:ciphertext",
		"master.key":            "hex",
		"agent_deploy_key":      "PRIVATE KEY",
		"AGENTS.md":             "dev rules",
		"CLAUDE.md":             "dev rules",
		"README.md":             "docs",
		"Dockerfile":            "FROM x",
		".openroutines-version": "v0",
		"skills/s1/SKILL.md":    "skill",
		"memory/events.md":      "events",
		".git/config":           "git",
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
	if err := buildWorkspace(dir, workspace); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"agent.yaml", "opencode.json", "routines/daily.md"} {
		if _, err := os.Stat(filepath.Join(workspace, f)); err != nil {
			t.Errorf("%s should travel into the workspace: %v", f, err)
		}
	}
	entries, err := os.ReadDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"agent.yaml": true, "opencode.json": true, "routines": true}
	for _, e := range entries {
		if !allowed[e.Name()] {
			t.Errorf("%s leaked into the run workspace", e.Name())
		}
	}
}

// The constructed environment holds exactly the declared credentials plus
// the model's provider key -- the audit's second headline claim.
func TestResolveCredentialsScope(t *testing.T) {
	dir := t.TempDir()
	key := creds.GenerateKey()
	if err := os.WriteFile(filepath.Join(dir, creds.KeyFileName), []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := creds.LoadKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := map[string]string{
		"slack_webhook":     "hook-value",
		"deploy_token":      "token-value",
		"anthropic_api_key": "sk-ant-x",
		"openai_api_key":    "sk-oai-x",
	}
	if err := creds.Write(dir, loaded, store); err != nil {
		t.Fatal(err)
	}

	r := &routine.Routine{Name: "x", FM: routine.Frontmatter{Credentials: []string{"slack_webhook"}}}
	got, err := resolveCredentials(dir, r, "anthropic/claude-sonnet-5", false)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"slack_webhook": "hook-value", "anthropic_api_key": "sk-ant-x"}
	if len(got) != len(want) {
		t.Fatalf("resolved %v, want exactly %v -- undeclared secrets must not exist in the run", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("resolved %v, want %v", got, want)
		}
	}

	dry, err := resolveCredentials(dir, r, "anthropic/claude-sonnet-5", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(dry) != 1 || dry["anthropic_api_key"] == "" {
		t.Fatalf("dry run resolved %v, want only the provider key", dry)
	}

	r.FM.Credentials = []string{"missing_cred"}
	if _, err := resolveCredentials(dir, r, "anthropic/claude-sonnet-5", false); err == nil {
		t.Fatal("declaring an absent credential must fail the run, not proceed without it")
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
	for _, want := range []string{"it happened -> append an event", "state, not a log"} {
		if !strings.Contains(full, want) {
			t.Fatalf("instruction missing %q:\n%s", want, full)
		}
	}
	if strings.Contains(full, "does not record events") {
		t.Fatalf("no-events rule rendered for an events-recording routine:\n%s", full)
	}
	plain := render(routine.Frontmatter{Events: &off}, false)
	for _, banned := range []string{"DRY RUN", "Every run appends", "Delivery inbox", "append an event to memory/events.md"} {
		if strings.Contains(plain, banned) {
			t.Fatalf("conditional block %q rendered when its flag was off:\n%s", banned, plain)
		}
	}
	for _, want := range []string{"does not record events", "never write to memory/events.md"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("events: false instruction missing %q:\n%s", want, plain)
		}
	}
	agent.Variables = nil
	if got := render(routine.Frontmatter{}, false); strings.Contains(got, "configuration variables") {
		t.Fatalf("variables block rendered with no variables configured:\n%s", got)
	}
}

// events: false is enforced at import, not just instructed: a staged change
// to events.md is discarded (worktree copy wins) while the rest imports.
func TestImportMemoryEnforcesEventsOptOut(t *testing.T) {
	setup := func(t *testing.T) (string, *Staging) {
		t.Helper()
		dir := t.TempDir()
		wt := filepath.Join(dir, memory.Dir)
		if err := os.MkdirAll(wt, 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(wt, "events.md"), []byte("base\n"), 0o644)
		os.WriteFile(filepath.Join(wt, "tasks.md"), []byte("none\n"), 0o644)
		staging := &Staging{MemoryDir: t.TempDir()}
		os.WriteFile(filepath.Join(staging.MemoryDir, "events.md"), []byte("base\n- sneaky event\n"), 0o644)
		os.WriteFile(filepath.Join(staging.MemoryDir, "tasks.md"), []byte("- [ ] real work\n"), 0o644)
		return dir, staging
	}

	off := false
	dir, staging := setup(t)
	r := &routine.Routine{Name: "quiet", FM: routine.Frontmatter{Events: &off}}
	discarded, err := ImportMemory(dir, r, staging)
	if err != nil || !discarded {
		t.Fatalf("discarded=%v err=%v, want true nil", discarded, err)
	}
	wt := filepath.Join(dir, memory.Dir)
	if got, _ := os.ReadFile(filepath.Join(wt, "events.md")); string(got) != "base\n" {
		t.Fatalf("events.md = %q, want staged change discarded", got)
	}
	if got, _ := os.ReadFile(filepath.Join(wt, "tasks.md")); string(got) != "- [ ] real work\n" {
		t.Fatalf("tasks.md = %q, want staged change imported", got)
	}

	dir, staging = setup(t)
	r = &routine.Routine{Name: "loud", FM: routine.Frontmatter{}}
	discarded, err = ImportMemory(dir, r, staging)
	if err != nil || discarded {
		t.Fatalf("discarded=%v err=%v, want false nil", discarded, err)
	}
	wt = filepath.Join(dir, memory.Dir)
	if got, _ := os.ReadFile(filepath.Join(wt, "events.md")); string(got) != "base\n- sneaky event\n" {
		t.Fatalf("events.md = %q, want staged change imported for a recording routine", got)
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
