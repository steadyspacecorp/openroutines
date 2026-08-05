package runner

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/memory"
	"github.com/steadyspacecorp/openroutines/internal/routine"
)

func TestManualRunInContainerRequiresTheManualIdentity(t *testing.T) {
	// Outside the real image the agent user is not in the manual attempt
	// group, so the reservation must refuse with the image contract named
	// -- the same refusal an operator sees on a stale deploy image. The
	// working path runs in bin/smoke's container stage.
	t.Setenv("OPENROUTINES_IN_CONTAINER", "1")
	_, err := Run(t.TempDir(), "daily", false)
	if !errors.Is(err, ErrFatal) || !strings.Contains(err.Error(), "manual attempt group") {
		t.Fatalf("manual run error = %v, want fatal manual-identity contract error", err)
	}
}

func TestCleanupReportsWorkspaceRemovalFailure(t *testing.T) {
	staging := &Staging{workspace: "\x00"}
	if err := staging.Cleanup(); !errors.Is(err, ErrAttemptCleanup) {
		t.Fatalf("cleanup error = %v, want ErrAttemptCleanup", err)
	}
}

func genDef(t *testing.T, meta Meta, fm ...routine.Frontmatter) string {
	t.Helper()
	ws := t.TempDir()
	agent := &config.Agent{Name: "a", Description: "d"}
	front := routine.Frontmatter{Skills: []string{"s1"}}
	if len(fm) > 0 {
		front = fm[0]
	}
	r := &routine.Routine{Name: "x", FM: front}
	if err := writeAgentDefinition(ws, agent, r, nil, meta); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(ws, ".opencode", "agents", "routine.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The ceiling is the agent's own max_timeout, applied where attempts read
// the timeout -- not left to a `check` the operator may never run. The
// declared value stays readable for `check` to warn about.
func TestTimeoutIsCappedAtTheAgentCeiling(t *testing.T) {
	agent := &config.Agent{Name: "a", Description: "d"}
	agent.Defaults.Timeout = "90m"
	marathon := &routine.Routine{Name: "marathon"}
	if got := DeclaredTimeout(agent, marathon); got != 90*time.Minute {
		t.Fatalf("declared timeout = %s, want 90m", got)
	}
	if got := EffectiveTimeout(agent, marathon); got != 90*time.Minute {
		t.Fatalf("effective timeout = %s, want 90m under the default ceiling", got)
	}

	agent.MaxTimeout = "1h"
	if got := EffectiveTimeout(agent, marathon); got != time.Hour {
		t.Fatalf("effective timeout = %s, want the 1h max_timeout ceiling", got)
	}

	agent.MaxTimeout = ""
	week := &routine.Routine{Name: "week", FM: routine.Frontmatter{Timeout: "168h"}}
	if got := EffectiveTimeout(agent, week); got != config.DefaultMaxTimeout {
		t.Fatalf("effective timeout = %s, want the %s default ceiling", got, config.DefaultMaxTimeout)
	}
}

func TestFrameworkEnvIncludesEffectiveRoutineURL(t *testing.T) {
	meta := Meta{RunID: "run_t", AttemptID: "attempt_01"}
	r := &routine.Routine{FM: routine.Frontmatter{}}
	if got := strings.Join(frameworkEnv("America/New_York", r, meta), "\n"); !strings.Contains(got, "OPENROUTINES_URL=https://openroutines.dev") {
		t.Fatalf("default framework env missing URL:\n%s", got)
	}
	r.FM.URL = "https://example.com/agent"
	if got := strings.Join(frameworkEnv("America/New_York", r, meta), "\n"); !strings.Contains(got, "OPENROUTINES_URL=https://example.com/agent") {
		t.Fatalf("declared framework env missing URL:\n%s", got)
	}
}

func TestRunDefinitionAllowsActing(t *testing.T) {
	def := genDef(t, Meta{RunID: "run_t"})
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
	def := genDef(t, Meta{RunID: "run_t"})
	for _, want := range []string{"webfetch: deny", "websearch: deny"} {
		if !strings.Contains(def, want) {
			t.Fatalf("definition missing %q:\n%s", want, def)
		}
	}
}

// Frontmatter opt-in flips the explicit rule to allow.
func TestWebAccessOptIn(t *testing.T) {
	fm := routine.Frontmatter{Webfetch: true, Websearch: true}
	def := genDef(t, Meta{RunID: "run_t"}, fm)
	for _, want := range []string{"webfetch: allow", "websearch: allow"} {
		if !strings.Contains(def, want) {
			t.Fatalf("opted-in definition missing %q:\n%s", want, def)
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
		"openroutines.yaml":                "name: t\n",
		"routines/daily.md":                "---\nschedule: \"0 9 * * *\"\n---\nwork",
		"routines/typo.md":                 "---\nschedule: \"0 9 * * *\"\nactve: false\n---\nbroken",
		"routines/twin.md":                 "---\nschedule: \"0 9 * * *\"\n---\nmine",
		"plugins/demo/routines/twin.md":    "---\nschedule: \"0 9 * * *\"\n---\ntheirs",
		"plugins/demo/routines/plugged.md": "---\nschedule: \"0 10 * * *\"\n---\nplugin work",
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
	for _, f := range []string{"routines/daily.md", "routines/plugged.md"} {
		if _, err := os.Stat(filepath.Join(workspace, f)); err != nil {
			t.Errorf("%s should travel into the workspace: %v", f, err)
		}
	}
	for _, f := range []string{"routines/typo.md", "routines/twin.md"} {
		if _, err := os.Stat(filepath.Join(workspace, f)); err == nil {
			t.Errorf("%s does not load and must not travel into the workspace", f)
		}
	}

	if err := buildWorkspace(dir, t.TempDir(), "typo"); err == nil {
		t.Error("the broken routine's own run must fail")
	} else if !strings.Contains(err.Error(), "frontmatter") {
		t.Errorf("want the parse error, got %v", err)
	}
	if err := buildWorkspace(dir, t.TempDir(), "twin"); err == nil {
		t.Error("a routine party to a name collision must fail")
	} else if !strings.Contains(err.Error(), "duplicate routine") {
		t.Errorf("want the collision error, got %v", err)
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
	got, err := resolveCredentials(dir, agent, r, "anthropic/claude-sonnet-5")
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

	r.FM.Credentials = []string{"missing_cred"}
	if _, err := resolveCredentials(dir, agent, r, "anthropic/claude-sonnet-5"); err == nil {
		t.Fatal("declaring an absent credential must fail the run, not proceed without it")
	}
}

// The standing instruction renders from embedded instruction.md; every
// conditional block must appear exactly when its flag is set, and no
// template syntax may leak into the prompt.
func TestInstructionRendering(t *testing.T) {
	agent := &config.Agent{Name: "test-agent", Description: "Tests things"}
	render := func(fm routine.Frontmatter) string {
		t.Helper()
		ws := t.TempDir()
		r := &routine.Routine{Name: "sample", FM: fm}
		if err := writeAgentDefinition(ws, agent, r, nil, Meta{RunID: "run_x"}); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(ws, ".opencode", "agents", "routine.md"))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	agent.Variables = map[string]string{"product_repo": "acme/widgets", "docs_url": "https://docs.example.com"}
	full := render(routine.Frontmatter{Consumes: "memory"})
	for _, want := range []string{
		"You are test-agent",
		"routine \"sample\" (run run_x)",
		"memory/ledgers/sample.md",
		"Every run appends at least one event",
		"Full facts with real links",
		"./inbox.md",
		"memory/CONSUMED",
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
	for _, banned := range []string{"Every run appends", "Delivery inbox", "append an event to memory/events.md"} {
		if strings.Contains(plain, banned) {
			t.Fatalf("conditional block %q rendered when its flag was off:\n%s", banned, plain)
		}
	}
	for _, want := range []string{"does not record events", "never write to memory/events.md"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("teamwork: off instruction missing %q:\n%s", want, plain)
		}
	}
	agent.Variables = nil
	if got := render(routine.Frontmatter{}); strings.Contains(got, "configuration variables") {
		t.Fatalf("variables block rendered with no variables configured:\n%s", got)
	}
}

// teamwork: off is enforced at import, not just instructed: a staged change
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
		staging := &Staging{MemoryDir: t.TempDir(), BaseDir: t.TempDir()}
		os.WriteFile(filepath.Join(staging.BaseDir, "events.md"), []byte("base\n"), 0o644)
		os.WriteFile(filepath.Join(staging.BaseDir, "tasks.md"), []byte("none\n"), 0o644)
		os.WriteFile(filepath.Join(staging.MemoryDir, "events.md"), []byte("base\n- sneaky event\n"), 0o644)
		os.WriteFile(filepath.Join(staging.MemoryDir, "tasks.md"), []byte("- [ ] real work\n"), 0o644)
		return dir, staging
	}

	dir, staging := setup(t)
	r := &routine.Routine{Name: "quiet", FM: routine.Frontmatter{Teamwork: routine.TeamworkOff}}
	discarded, _, err := importMemory(dir, r, staging)
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
	discarded, _, err = importMemory(dir, r, staging)
	if err != nil || discarded {
		t.Fatalf("discarded=%v err=%v, want false nil", discarded, err)
	}
	wt = filepath.Join(dir, memory.Dir)
	if got, _ := os.ReadFile(filepath.Join(wt, "events.md")); string(got) != "base\n- sneaky event\n" {
		t.Fatalf("events.md = %q, want staged change imported for a recording routine", got)
	}
}

// settleFixture builds a real agent repo with a materialized memory worktree:
// Settle's commit step needs actual git.
func settleFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "-b", "main", ".")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := memory.At(dir).Ensure(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A new consumer's inbox is empty by construction. Its first successful run
// must still persist the current boundary, or every later run is another
// "first run" that skips forward and receives nothing forever.
func TestSettleBootstrapsAnEmptyConsumerCursor(t *testing.T) {
	dir := settleFixture(t)
	mem := memory.At(dir)
	through, err := mem.Head()
	if err != nil {
		t.Fatal(err)
	}
	stage := func(first bool, boundary string) *Staging {
		s := &Staging{
			MemoryDir:        t.TempDir(),
			BaseDir:          t.TempDir(),
			workspace:        t.TempDir(),
			ConsumerThrough:  boundary,
			ConsumerFirstRun: first,
		}
		if err := mem.Snapshot(s.BaseDir); err != nil {
			t.Fatal(err)
		}
		if err := memory.CloneTree(s.BaseDir, s.MemoryDir); err != nil {
			t.Fatal(err)
		}
		return s
	}
	r := &routine.Routine{Name: "slack-report", FM: routine.Frontmatter{Consumes: "memory"}}
	if _, err := Settle(dir, r, stage(true, through), &ExecResult{Outcome: Completed}, Meta{RunID: "run_first", AttemptID: "attempt_01"}, "", nil); err != nil {
		t.Fatal(err)
	}
	cursor, err := mem.LoadCursor(r.Name)
	if err != nil || cursor == nil || cursor.ConsumedThrough != through || cursor.ByRun != "run_first" {
		t.Fatalf("bootstrap cursor = %+v, err = %v; want %s by run_first", cursor, err, through)
	}

	// Once initialized, successful completion alone must not consume real
	// pending changes: the explicit marker remains the delivery receipt.
	newBoundary, err := mem.Head()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Settle(dir, r, stage(false, newBoundary), &ExecResult{Outcome: Completed}, Meta{RunID: "run_empty", AttemptID: "attempt_01"}, "", nil); err != nil {
		t.Fatal(err)
	}
	cursor, err = mem.LoadCursor(r.Name)
	if err != nil || cursor.ConsumedThrough != through || cursor.ByRun != "run_first" {
		t.Fatalf("cursor advanced without marker: %+v, err = %v", cursor, err)
	}
}

// A completed attempt whose staged memory is rejected settles as crashed --
// in the returned outcome, the failure event, the run record, and the
// settlement commit alike. The run record saying "completed" while the run
// was reported crashed is the divergence Settle exists to prevent.
func TestSettleRecordsRejectedImportAsCrashed(t *testing.T) {
	dir := settleFixture(t)
	staging := &Staging{MemoryDir: t.TempDir()}
	os.WriteFile(filepath.Join(staging.MemoryDir, ".gitignore"), []byte("x"), 0o644)

	r := &routine.Routine{Name: "x", FM: routine.Frontmatter{}}
	settlement, err := Settle(dir, r, staging, &ExecResult{Outcome: Completed}, Meta{RunID: "run_reject", AttemptID: "attempt_01"}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if settlement.Outcome != Crashed || !strings.Contains(settlement.Detail, "memory rejected") {
		t.Fatalf("settlement = %+v, want crashed with memory-rejected detail", settlement)
	}
	if settlement.Commit == "" {
		t.Fatal("settlement should have committed the record and event")
	}
	wt := filepath.Join(dir, memory.Dir)
	records, _ := os.ReadFile(filepath.Join(wt, "runs.jsonl"))
	if !strings.Contains(string(records), `"outcome":"crashed"`) {
		t.Fatalf("run record should carry the settled outcome: %s", records)
	}
	events, _ := os.ReadFile(filepath.Join(wt, "events.md"))
	if !strings.Contains(string(events), "run_reject attempt_01) memory rejected") {
		t.Fatalf("failure event missing: %s", events)
	}
}

// A clean completion imports staged memory, runs the caller's stage hook
// before the settlement commit (so its writes ride along), and commits.
func TestSettleImportsAndCommitsCompletedRun(t *testing.T) {
	dir := settleFixture(t)
	mem := memory.At(dir)
	staging := &Staging{MemoryDir: t.TempDir()}
	if err := mem.Snapshot(staging.MemoryDir); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(staging.MemoryDir, "ledgers", "x.md"), []byte("worked\n"), 0o644)

	staged := false
	r := &routine.Routine{Name: "x", FM: routine.Frontmatter{}}
	settlement, err := Settle(dir, r, staging, &ExecResult{Outcome: Completed}, Meta{RunID: "run_ok", AttemptID: "attempt_01"}, "", func(fin *Settlement) {
		staged = fin.Outcome == Completed
		os.WriteFile(filepath.Join(mem.StateDir(), "x.json"), []byte("{}\n"), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if settlement.Outcome != Completed || settlement.Detail != "" || settlement.Commit == "" {
		t.Fatalf("settlement = %+v, want clean completion with a commit", settlement)
	}
	if !staged {
		t.Fatal("stage hook should see the settled outcome")
	}
	wt := filepath.Join(dir, memory.Dir)
	if got, _ := os.ReadFile(filepath.Join(wt, "ledgers", "x.md")); string(got) != "worked\n" {
		t.Fatalf("staged memory not imported: %q", got)
	}
	// The stage hook's write is inside the settlement commit, not left dirty.
	if changed, _ := exec.Command("git", "-C", wt, "status", "--porcelain").Output(); len(changed) != 0 {
		t.Fatalf("worktree dirty after settlement: %s", changed)
	}
	records, _ := os.ReadFile(filepath.Join(wt, "runs.jsonl"))
	if !strings.Contains(string(records), `"outcome":"completed"`) || !strings.Contains(string(records), `"manual":true`) {
		t.Fatalf("run record wrong: %s", records)
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
	if _, _, err := importMemory(dir, r, staging); err != nil {
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

// Raw credentials inject verbatim under their uppercase names.
func TestResolveCredentialsRaw(t *testing.T) {
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
	s, err := resolveCredentials(dir, agent, r, "openai/gpt")
	if err != nil {
		t.Fatal(err)
	}
	if s.env["STEADY_TOKEN"] != "sekrit" || s.env["OPENAI_API_KEY"] != "provider-key" {
		t.Fatalf("raw injection wrong: %v", s.env)
	}
	if s.scrub["steady_token"] != "sekrit" {
		t.Fatal("raw credential missing from scrub set")
	}

	typed := &routine.Routine{Name: "x", FM: routine.Frontmatter{Credentials: []string{"gh_key"}}}
	// A run with the typed credential fails at derivation (bad key)
	// rather than injecting the stored root secret.
	if _, err = resolveCredentials(dir, agent, typed, "openai/gpt"); err == nil {
		t.Fatal("expected derivation failure for an invalid stored key")
	}
}

// `docker stop` can return without the run's client following the container
// out -- an unresponsive daemon has nothing to stop. The wait on the client
// must end anyway, or a local run parks the caller the way an orphan holding
// the output pipe used to park the supervisor.
func TestKillClientBoundsTheWaitOnAStuckDockerClient(t *testing.T) {
	cmd := exec.Command("sleep", "120")
	cmd.WaitDelay = pipeDrainDeadline
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		killClient(cmd, 100*time.Millisecond, done)
	}()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("killClient never returned: the wait on the docker client is unbounded")
	}
}

// An auth failure's hint names what the framework knows and the provider's
// message does not: the resolved provider, the endpoint opencode.json
// declares, and the credential the run injected -- or that none was (#60).
func TestAuthHintNamesProviderEndpointAndCredential(t *testing.T) {
	dir := t.TempDir()
	cfg := `{"provider":{"my_gateway":{"options":{"baseURL":"https://gateway.example.com/v1/compat"}}}}`
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	hint := authHint(dir, "my_gateway/some-model", true)
	for _, want := range []string{"my_gateway at https://gateway.example.com/v1/compat", "rejected the run's my_gateway_api_key credential", "credentials set my_gateway_api_key"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("hint missing %q:\n%s", want, hint)
		}
	}

	// No provider block: the provider name stands alone. No injected key:
	// the hint says so instead of claiming a credential was rejected.
	hint = authHint(dir, "anthropic/claude-x", false)
	for _, want := range []string{"anthropic rejected the request", "no anthropic_api_key credential is stored"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("hint missing %q:\n%s", want, hint)
		}
	}
}

// opencode passes some providers' status text through verbatim -- "Error:
// Unauthorized: Unauthorized" carries no key-shaped phrase, and unmatched it
// reports as a bare crash (#60).
func TestAuthFailurePatternMatchesPassthroughStatusText(t *testing.T) {
	for _, line := range []string{
		"Error: Unauthorized: Unauthorized",
		"error: unauthorized",
		"Error: invalid bearer token",
		"API key is invalid.",
	} {
		if !authFailurePattern.MatchString(line) {
			t.Fatalf("auth pattern should match %q", line)
		}
	}
	if authFailurePattern.MatchString("the reviewer felt unauthorized to approve") {
		t.Fatal("bare 'unauthorized' outside an error line should not classify as auth failure")
	}
}
