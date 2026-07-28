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

func genDef(t *testing.T, meta Meta, fm ...routine.Frontmatter) string {
	t.Helper()
	ws := t.TempDir()
	agent := &config.Agent{Name: "a", Description: "d"}
	front := routine.Frontmatter{Skills: []string{"s1"}}
	if len(fm) > 0 {
		front = fm[0]
	}
	r := &routine.Routine{Name: "x", FM: front}
	if err := writeAgentDefinition(ws, agent, r, meta); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(ws, ".opencode", "agents", "routine.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A dry run's permission block is deny-all-first: "*" matches every tool
// name -- built-ins, custom tools, MCP tools -- so nothing outside the
// explicit read/write-memory set can start, not just the three acting tools
// we can name. The wildcard must precede the allows (last match wins).
func TestDryRunDefinitionDeniesAllToolsFirst(t *testing.T) {
	def := genDef(t, Meta{RunID: "run_t", DryRun: true})
	for _, want := range []string{`"*": deny`, "read: allow", "write: allow", "DRY RUN", `"s1": allow`} {
		if !strings.Contains(def, want) {
			t.Fatalf("dry-run definition missing %q:\n%s", want, def)
		}
	}
	if strings.Index(def, `"*": deny`) > strings.Index(def, "read: allow") {
		t.Fatalf("wildcard deny must precede the allows (last match wins):\n%s", def)
	}
	for _, banned := range []string{"bash: allow", "webfetch: allow", "task: allow"} {
		if strings.Contains(def, banned) {
			t.Fatalf("dry-run definition wrongly allows %q:\n%s", banned, def)
		}
	}
}

func TestRealRunDefinitionAllowsActing(t *testing.T) {
	def := genDef(t, Meta{RunID: "run_t"})
	for _, banned := range []string{"bash: deny", "DRY RUN"} {
		if strings.Contains(def, banned) {
			t.Fatalf("real-run definition wrongly contains %q:\n%s", banned, def)
		}
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
	for _, dry := range []bool{false, true} {
		def := genDef(t, Meta{RunID: "run_t", DryRun: dry})
		for _, want := range []string{"webfetch: deny", "websearch: deny"} {
			if !strings.Contains(def, want) {
				t.Fatalf("definition (dry=%v) missing %q:\n%s", dry, want, def)
			}
		}
	}
}

// Frontmatter opt-in flips the explicit rule to allow -- in dry runs too
// (reads are within a rehearsal's scope), where the allow must land after
// the wildcard deny because the last matching rule wins.
func TestWebAccessOptIn(t *testing.T) {
	fm := routine.Frontmatter{Webfetch: true, Websearch: true}
	for _, dry := range []bool{false, true} {
		def := genDef(t, Meta{RunID: "run_t", DryRun: dry}, fm)
		for _, want := range []string{"webfetch: allow", "websearch: allow"} {
			if !strings.Contains(def, want) {
				t.Fatalf("opted-in definition (dry=%v) missing %q:\n%s", dry, want, def)
			}
			if dry && strings.Index(def, `"*": deny`) > strings.Index(def, want) {
				t.Fatalf("wildcard deny must precede %q (last match wins):\n%s", want, def)
			}
		}
	}
}

// The workspace is built by allow-list: exactly openroutines.yml, opencode.json,
// and routines/ travel in. This is the audit's headline test -- no
// secret-shaped file (the encrypted store, keys) and no dev-session rules
// file (AGENTS.md/CLAUDE.md, which opencode would load into run context)
// may ever reach a run.
func TestBuildWorkspaceAllowList(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"openroutines.yml":                      "name: t\n",
		"opencode.json":                         "{}",
		"routines/daily.md":                     "---\nschedule: \"0 9 * * *\"\n---\nwork",
		"plugins/demo/routines/plugin-daily.md": "---\nschedule: \"0 10 * * *\"\n---\nplugin work",
		"credentials.yml.enc":                   "ORV1:ciphertext",
		"master.key":                            "hex",
		"agent_deploy_key":                      "PRIVATE KEY",
		"AGENTS.md":                             "dev rules",
		"CLAUDE.md":                             "dev rules",
		"README.md":                             "docs",
		"Dockerfile":                            "FROM x",
		".openroutines-version":                 "v0",
		"skills/s1/SKILL.md":                    "skill",
		"memory/events.md":                      "events",
		".git/config":                           "git",
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

	agent := &config.Agent{}
	r := &routine.Routine{Name: "x", FM: routine.Frontmatter{Credentials: []string{"slack_webhook"}}}
	got, err := resolveCredentials(dir, agent, r, "anthropic/claude-sonnet-5", false)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"SLACK_WEBHOOK": "hook-value", "ANTHROPIC_API_KEY": "sk-ant-x"}
	if len(got.env) != len(want) {
		t.Fatalf("resolved %v, want exactly %v -- undeclared secrets must not exist in the run", got.env, want)
	}
	for k, v := range want {
		if got.env[k] != v {
			t.Fatalf("resolved %v, want %v", got.env, want)
		}
	}

	dry, err := resolveCredentials(dir, agent, r, "anthropic/claude-sonnet-5", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(dry.env) != 1 || dry.env["ANTHROPIC_API_KEY"] == "" {
		t.Fatalf("dry run resolved %v, want only the provider key", dry.env)
	}

	r.FM.Credentials = []string{"missing_cred"}
	if _, err := resolveCredentials(dir, agent, r, "anthropic/claude-sonnet-5", false); err == nil {
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
		"memory/CONSUMED",
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

func TestConsumeMarkerLivesInStagedMemory(t *testing.T) {
	dir := t.TempDir()
	wt := filepath.Join(dir, memory.Dir)
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	staging := &Staging{MemoryDir: t.TempDir(), workspace: t.TempDir()}
	if staging.Consumed() {
		t.Fatal("Consumed() true with no marker anywhere")
	}
	// The sandbox leaves only staged memory writable: the marker there counts.
	os.WriteFile(filepath.Join(staging.MemoryDir, memory.ConsumeMarker), nil, 0o644)
	if !staging.Consumed() {
		t.Fatal("marker in staged memory not honored")
	}
	// It is a receipt for the runtime, not memory content: import drops it.
	r := &routine.Routine{Name: "report", FM: routine.Frontmatter{Consumes: "memory"}}
	if _, err := ImportMemory(dir, r, staging); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wt, memory.ConsumeMarker)); !os.IsNotExist(err) {
		t.Fatal("consume marker imported into the memory worktree")
	}
	// Unsandboxed runs may still drop the marker at the workspace root.
	legacy := &Staging{MemoryDir: t.TempDir(), workspace: t.TempDir()}
	os.WriteFile(filepath.Join(legacy.workspace, memory.ConsumeMarker), nil, 0o644)
	if !legacy.Consumed() {
		t.Fatal("workspace-root marker no longer honored")
	}
}

// Raw credentials inject verbatim under their uppercase names; a typed
// credential derives nothing in a dry run -- dry runs receive no secrets of
// any kind, so no token is ever minted for one.
func TestResolveCredentialsRawAndDryRun(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, creds.KeyFileName), []byte(creds.GenerateKey()), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := creds.LoadKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := creds.Write(dir, key, map[string]string{
		"steady_token":   "sekrit",
		"openai_api_key": "provider-key",
		"gh_key":         "not a real pem",
	}); err != nil {
		t.Fatal(err)
	}
	agent := &config.Agent{Credentials: map[string]creds.Spec{"gh_key": {Type: "github_app", AppID: "1"}}}

	r := &routine.Routine{Name: "x", FM: routine.Frontmatter{Credentials: []string{"steady_token"}}}
	s, err := resolveCredentials(dir, agent, r, "openai/gpt", false)
	if err != nil {
		t.Fatal(err)
	}
	if s.env["STEADY_TOKEN"] != "sekrit" || s.env["OPENAI_API_KEY"] != "provider-key" {
		t.Fatalf("raw injection wrong: %v", s.env)
	}
	if s.scrub["steady_token"] != "sekrit" {
		t.Fatal("raw credential missing from scrub set")
	}

	// Dry run with a typed credential declared: no store lookup, no
	// derivation (a real derivation of "not a real pem" would error).
	typed := &routine.Routine{Name: "x", FM: routine.Frontmatter{Credentials: []string{"gh_key"}}}
	s, err = resolveCredentials(dir, agent, typed, "openai/gpt", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := s.env["GITHUB_TOKEN"]; present {
		t.Fatal("dry run derived a token")
	}
	if s.env["OPENAI_API_KEY"] != "provider-key" {
		t.Fatal("provider key should still reach dry runs")
	}

	// A live run with the typed credential fails at derivation (bad key)
	// rather than injecting the stored root secret.
	if _, err = resolveCredentials(dir, agent, typed, "openai/gpt", false); err == nil {
		t.Fatal("expected derivation failure for an invalid stored key")
	}
}
