package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/routine"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("..", "..", "testdata", "plugins", name)
}

// The synthetic fixtures cover the plugin feature matrix: each exercises a
// different slice of the format, and this test is what keeps them loadable.
func TestLoadFixturePlugins(t *testing.T) {
	reporter, err := Load(fixture(t, "reporter"), nil)
	if err != nil {
		t.Fatalf("reporter: %v", err)
	}
	if len(reporter.Routines) != 1 || len(reporter.Skills) != 1 || len(reporter.Stubs) != 0 {
		t.Fatalf("reporter shape: %d routines, %d skills, %d stubs", len(reporter.Routines), len(reporter.Skills), len(reporter.Stubs))
	}
	if _, ok := reporter.Manifest.Credentials["reporter_token"]; !ok {
		t.Fatal("reporter manifest missing reporter_token")
	}
	if _, ok := reporter.Manifest.MCP["reporter_feed"]; !ok {
		t.Fatal("reporter manifest missing the reporter_feed mcp declaration")
	}

	watcher, err := Load(fixture(t, "watcher"), nil)
	if err != nil {
		t.Fatalf("watcher: %v", err)
	}
	if len(watcher.Routines) != 1 || len(watcher.Skills) != 0 || len(watcher.Stubs) != 1 {
		t.Fatalf("watcher shape: %d routines, %d skills, %d stubs", len(watcher.Routines), len(watcher.Skills), len(watcher.Stubs))
	}
	if watcher.Manifest.Credentials["watcher_app_key"].Type != "github_app" {
		t.Fatal("watcher credential should be typed github_app")
	}
	if _, ok := watcher.Manifest.Variables["watch_repos"]; !ok {
		t.Fatal("watcher manifest missing the watch_repos variable")
	}

	toolkit, err := Load(fixture(t, "toolkit"), nil)
	if err != nil {
		t.Fatalf("toolkit: %v", err)
	}
	if len(toolkit.Routines) != 0 || len(toolkit.Skills) != 1 {
		t.Fatalf("toolkit shape: %d routines, %d skills", len(toolkit.Routines), len(toolkit.Skills))
	}

	// The grant summary must state the authorities the bundle asks for.
	sum := watcher.Summary()
	for _, want := range []string{"watcher", "watcher_app_key", "typed: github_app", "watch_repos"} {
		if !strings.Contains(sum, want) {
			t.Fatalf("watcher summary missing %q:\n%s", want, sum)
		}
	}
	sum = reporter.Summary()
	for _, want := range []string{"reporter_feed", "https://mcp.example.invalid/feed", "auth via credential reporter_token"} {
		if !strings.Contains(sum, want) {
			t.Fatalf("reporter summary missing %q:\n%s", want, sum)
		}
	}
}

// write builds a minimal plugin dir; extra maps rel path -> content.
func write(t *testing.T, extra map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"PLUGIN.md":        "---\nname: demo\ndescription: A demo plugin.\ncredentials:\n  demo_token:\n    description: A demo token\n---\nBody.\n",
		"routines/demo.md": "---\nschedule: \"0 9 * * *\"\ncredentials: [demo_token]\n---\nDo the demo.\n",
	}
	for k, v := range extra {
		files[k] = v
	}
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadRefusals(t *testing.T) {
	cases := map[string]struct {
		extra map[string]string
		want  string
	}{
		"opencode.json":            {map[string]string{"opencode.json": "{}"}, "harness config"},
		"openroutines.yml":         {map[string]string{"openroutines.yml": "name: x"}, "belongs to the agent"},
		"legacy openroutines.yaml": {map[string]string{"openroutines.yaml": "name: x"}, "belongs to the agent"},
		"legacy agent.yaml":        {map[string]string{"agent.yaml": "name: x"}, "belongs to the agent"},
		"install hook":             {map[string]string{"install.sh": "#!/bin/sh\n"}, "allow-listed"},
		"shared knowledge":         {map[string]string{"knowledge/events.md": "- sneaky\n"}, "never shared knowledge"},
		"master key":               {map[string]string{"master.key": "k"}, "key material"},
		"nested git":               {map[string]string{"skills/demo-skill/.git/config": "bad"}, "nested .git"},
		"dangling skill":           {map[string]string{"routines/other.md": "---\nschedule: \"0 9 * * *\"\nskills: [ghost]\n---\nx\n"}, "neither the plugin nor the agent"},
		"undeclared cred":          {map[string]string{"routines/other.md": "---\nschedule: \"0 9 * * *\"\ncredentials: [ghost_token]\n---\nx\n"}, "missing from the PLUGIN.md credentials block"},
		"unknown manifest field": {map[string]string{
			"PLUGIN.md": "---\nname: demo\ndescription: d\ncredentialz: {}\n---\n",
		}, "field credentialz not found"},
		"credential value": {map[string]string{
			"PLUGIN.md": "---\nname: demo\ndescription: d\ncredentials:\n  demo_token:\n    description: token\n    value: secret\n---\n",
		}, "field value not found"},
		"unknown credential type": {map[string]string{
			"PLUGIN.md": "---\nname: demo\ndescription: d\ncredentials:\n  demo_token:\n    description: token\n    type: plugin_code\n---\n",
		}, "unknown type"},
		"reserved credential env": {map[string]string{
			"PLUGIN.md": "---\nname: demo\ndescription: d\ncredentials:\n  path:\n    description: executable path\n---\n",
		}, "shadow the PATH"},
		"reserved variable env": {map[string]string{
			"PLUGIN.md": "---\nname: demo\ndescription: d\nvariables:\n  home:\n    description: home directory\n---\n",
		}, "shadow the HOME"},
		"credential variable collision": {map[string]string{
			"PLUGIN.md": "---\nname: demo\ndescription: d\ncredentials:\n  demo_token:\n    description: token\nvariables:\n  demo_token:\n    description: non-secret token\n---\n",
		}, "collides with a credential"},
		"undeclared mcp grant": {map[string]string{
			"routines/other.md": "---\nschedule: \"0 9 * * *\"\nmcp: [ghost]\n---\nx\n",
		}, "missing from the PLUGIN.md mcp block"},
		"mcp missing url": {map[string]string{
			"PLUGIN.md": "---\nname: demo\ndescription: d\ncredentials:\n  demo_token:\n    description: token\nmcp:\n  steady:\n    description: Steady's server\n---\n",
		}, "needs a url"},
		"mcp missing description": {map[string]string{
			"PLUGIN.md": "---\nname: demo\ndescription: d\ncredentials:\n  demo_token:\n    description: token\nmcp:\n  steady:\n    url: https://example.test/mcp\n---\n",
		}, "needs a description"},
		"mcp undeclared credential": {map[string]string{
			"PLUGIN.md": "---\nname: demo\ndescription: d\ncredentials:\n  demo_token:\n    description: token\nmcp:\n  steady:\n    description: Steady's server\n    url: https://example.test/mcp\n    credential: ghost_token\n---\n",
		}, "missing from the PLUGIN.md credentials block"},
		"mcp bad server name": {map[string]string{
			"PLUGIN.md": "---\nname: demo\ndescription: d\ncredentials:\n  demo_token:\n    description: token\nmcp:\n  Steady-Prod:\n    description: Steady's server\n    url: https://example.test/mcp\n---\n",
		}, "lowercase snake_case"},
	}
	for name, c := range cases {
		if _, err := Load(write(t, c.extra), nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want error containing %q, got %v", name, c.want, err)
		}
	}

	// A routine's dangling skill is fine when the agent already has it.
	dir := write(t, map[string]string{"routines/other.md": "---\nschedule: \"0 9 * * *\"\nskills: [ghost]\n---\nx\n"})
	if _, err := Load(dir, map[string]bool{"ghost": true}); err != nil {
		t.Fatalf("agent-satisfied skill should load: %v", err)
	}

	// A symlink anywhere is refused.
	dir = write(t, nil)
	if err := os.Symlink("/etc/hosts", filepath.Join(dir, "routines", "link.md")); err == nil {
		if _, err := Load(dir, nil); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("symlink should be refused, got %v", err)
		}
	}

	// Manifest only: nothing to install.
	empty := t.TempDir()
	os.WriteFile(filepath.Join(empty, "PLUGIN.md"), []byte("---\nname: demo\ndescription: d\n---\n"), 0o644)
	if _, err := Load(empty, nil); err == nil || !strings.Contains(err.Error(), "nothing to install") {
		t.Fatalf("empty payload should be refused, got %v", err)
	}

	// Typed credentials work for plugin trigger polls too: installation
	// supplies the metadata, and the supervisor sends only derived material.
	dir = write(t, map[string]string{
		"PLUGIN.md":        "---\nname: demo\ndescription: d\ncredentials:\n  gh_key:\n    description: App key\n    type: github_app\n---\n",
		"routines/demo.md": "---\ntrigger:\n  poll: https://example.com/x\n  credential: gh_key\ncredentials: [gh_key]\n---\nx\n",
	})
	if _, err := Load(dir, nil); err != nil {
		t.Fatalf("typed trigger credential should load: %v", err)
	}
}

// A declared MCP server loads, and the grant summary states both halves of
// the authority: the routine's grant and the endpoint the person must
// define. The summary is the review gate -- an MCP grant it omitted would
// be an authority the person never saw.
func TestMCPDeclarationLoadsAndSummarizes(t *testing.T) {
	dir := write(t, map[string]string{
		"PLUGIN.md":        "---\nname: demo\ndescription: d\ncredentials:\n  demo_token:\n    description: A demo token\nmcp:\n  steady:\n    description: Steady's MCP server\n    url: https://example.test/mcp\n    credential: demo_token\n---\nBody.\n",
		"routines/demo.md": "---\nschedule: \"0 9 * * *\"\ncredentials: [demo_token]\nmcp: [steady]\n---\nDo the demo.\n",
	})
	p, err := Load(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	sum := p.Summary()
	for _, want := range []string{
		"mcp: steady",
		"MCP server to define: steady -- Steady's MCP server (https://example.test/mcp; auth via credential demo_token)",
		"opencode.json mcp entry",
	} {
		if !strings.Contains(sum, want) {
			t.Fatalf("summary missing %q:\n%s", want, sum)
		}
	}
}

// Web access is off by default and a bundle can turn it on, so the summary --
// the only review gate before install -- has to say so.
func TestSummaryStatesWebAccess(t *testing.T) {
	dir := write(t, map[string]string{
		"routines/demo.md": "---\nschedule: \"0 9 * * *\"\ncredentials: [demo_token]\nwebfetch: true\nwebsearch: true\n---\nDo the demo.\n",
	})
	p, err := Load(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum := p.Summary(); !strings.Contains(sum, "web access: webfetch, websearch") {
		t.Fatalf("summary should state web access:\n%s", sum)
	}
}

// testSource is valid provenance: Install validates it as strictly as
// ReadSource does, so the revision has to be a full commit hash.
var testSource = Source{
	Repository: "example.test/demo.git",
	Revision:   "0123456789abcdef0123456789abcdef01234567",
}

func TestPrepareInstallAndApply(t *testing.T) {
	src := write(t, map[string]string{
		"skills/demo-skill/SKILL.md": "---\nname: demo-skill\ndescription: d\n---\nHow.\n",
		"knowledge/ledgers/demo.md":  "# demo ledger\n",
	})
	agent := t.TempDir()
	inst, err := PrepareInstall(agent, src, testSource)
	if err != nil {
		t.Fatal(err)
	}

	// No knowledge worktree yet: stubs go pending, the rest installs.
	installed, pending, err := inst.Apply()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || !strings.Contains(pending[0], "demo.md") {
		t.Fatalf("stub should be pending without a worktree: %v", pending)
	}
	for _, rel := range []string{".openroutines/plugins/demo/routines/demo.md", ".openroutines/plugins/demo/skills/demo-skill/SKILL.md", ".openroutines/plugins/demo/" + SourceFileName} {
		if _, err := os.Stat(filepath.Join(agent, rel)); err != nil {
			t.Fatalf("%s not installed: %v", rel, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(agent, ".openroutines", "plugins", "demo", "routines", "demo.md"))
	if err != nil || !strings.Contains(string(raw), "active: false") {
		t.Fatalf("installed routine must be explicitly inactive: %v\n%s", err, raw)
	}
	if len(installed) != 1 || installed[0] != filepath.Join(".openroutines", "plugins", "demo") {
		t.Fatalf("installed %v, want the grouped plugin directory", installed)
	}

	// Now everything collides, and the refusal happens at prepare -- Apply
	// is unreachable without a clean PrepareInstall.
	_, err = PrepareInstall(agent, src, testSource)
	if err == nil {
		t.Fatal("reinstall over an existing plugin should be refused")
	}
	for _, want := range []string{filepath.Join(".openroutines", "plugins", "demo"), "routine demo", "skill demo-skill"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal should name %q: %v", want, err)
		}
	}

	// With a worktree, the stub lands -- unless the ledger already exists,
	// which is live knowledge and never clobbered.
	install := func(agent string) (installed, pending []string, err error) {
		t.Helper()
		inst, err := PrepareInstall(agent, src, testSource)
		if err != nil {
			t.Fatal(err)
		}
		return inst.Apply()
	}
	agent2 := t.TempDir()
	os.MkdirAll(filepath.Join(agent2, "knowledge", "ledgers"), 0o755)
	installed, pending, err = install(agent2)
	if err != nil || len(pending) != 0 {
		t.Fatalf("stub should install into an existing worktree: pending=%v err=%v", pending, err)
	}
	if len(installed) != 2 {
		t.Fatalf("installed %v, want plugin directory and stub", installed)
	}
	agent3 := t.TempDir()
	os.MkdirAll(filepath.Join(agent3, "knowledge", "ledgers"), 0o755)
	os.WriteFile(filepath.Join(agent3, "knowledge", "ledgers", "demo.md"), []byte("live state\n"), 0o644)
	_, pending, err = install(agent3)
	if err != nil || len(pending) != 1 {
		t.Fatalf("existing ledger must go pending, not be clobbered: pending=%v err=%v", pending, err)
	}
	if raw, _ := os.ReadFile(filepath.Join(agent3, "knowledge", "ledgers", "demo.md")); string(raw) != "live state\n" {
		t.Fatal("live ledger was overwritten")
	}
}

// Provenance that later commands would reject must be refused at prepare
// time, before the review prompt and before anything is copied -- otherwise
// plugin list, plugin update, and check all fail against a plugin the user
// was told installed fine.
func TestPrepareInstallRefusesInvalidProvenance(t *testing.T) {
	src := write(t, nil)
	cases := map[string]Source{
		"missing repository": {Revision: testSource.Revision},
		"missing revision":   {Repository: testSource.Repository},
		"short revision":     {Repository: testSource.Repository, Revision: "abc123"},
		"escaping path":      {Repository: testSource.Repository, Path: "../elsewhere", Revision: testSource.Revision},
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			agent := t.TempDir()
			if _, err := PrepareInstall(agent, src, source); err == nil {
				t.Fatal("want refusal")
			}
			if _, err := os.Stat(filepath.Join(agent, ".openroutines", "plugins")); !os.IsNotExist(err) {
				t.Fatalf("nothing should have been copied: %v", err)
			}
		})
	}
}

// A duplicate plugin name is filtered out of the effective routine list, so
// collision detection must inspect the raw plugin claims and refuse while the
// namespace is invalid.
func TestCollisionsFailClosedOnInvalidNamespace(t *testing.T) {
	src := write(t, nil)
	agent := t.TempDir()
	routine := "---\nschedule: \"0 9 * * *\"\n---\nDo the demo.\n"
	for _, rel := range []string{
		filepath.Join(".openroutines", "plugins", "one", "routines", "demo.md"),
		filepath.Join(".openroutines", "plugins", "two", "routines", "demo.md"),
	} {
		path := filepath.Join(agent, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(routine), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := PrepareInstall(agent, src, testSource)
	if err == nil {
		t.Fatal("want refusal on a duplicated namespace")
	}
	if !strings.Contains(err.Error(), "duplicate routine") {
		t.Fatalf("error should name the duplicate: %v", err)
	}
}

func TestInstallAllowsAgentOwnedRoutineOverride(t *testing.T) {
	src := write(t, nil)
	agent := t.TempDir()
	owned := filepath.Join(agent, "routines", "demo.md")
	if err := os.MkdirAll(filepath.Dir(owned), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(owned, []byte("---\nschedule: \"0 8 * * *\"\n---\nAgent-owned.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inst, err := PrepareInstall(agent, src, testSource)
	if err != nil {
		t.Fatalf("prepare install with override: %v", err)
	}
	if _, _, err := inst.Apply(); err != nil {
		t.Fatalf("apply install with override: %v", err)
	}
	r, err := routine.Find(agent, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if r.Path != owned {
		t.Fatalf("winning routine = %s, want %s", r.Path, owned)
	}
}

func TestInstallRollsBackOnCopyFailure(t *testing.T) {
	src := write(t, map[string]string{
		"skills/demo-skill/SKILL.md": "---\nname: demo-skill\ndescription: d\n---\nHow.\n",
	})
	agent := t.TempDir()
	inst, err := PrepareInstall(agent, src, testSource)
	if err != nil {
		t.Fatal(err)
	}

	// Mutate the local development source after validation. Copy must recheck
	// the boundary, fail, and remove the routine it created earlier.
	if err := os.MkdirAll(filepath.Join(src, "skills", "demo-skill", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "skills", "demo-skill", ".git", "config"), []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := inst.Apply(); err == nil || !strings.Contains(err.Error(), "nested .git") {
		t.Fatalf("want nested .git copy refusal, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(agent, ".openroutines", "plugins", "demo", "routines", "demo.md")); !os.IsNotExist(err) {
		t.Fatalf("partial routine survived rollback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(agent, ".openroutines", "plugins", "demo", "skills", "demo-skill")); !os.IsNotExist(err) {
		t.Fatalf("partial skill survived rollback: %v", err)
	}
}

// The plugin validator consults the framework's shared derived-type list --
// a second hardcoded copy drifted once (oauth2_client was refused here
// while creds accepted it).
func TestManifestAcceptsAllDerivedTypes(t *testing.T) {
	manifest := func(typ string) string {
		return "---\nname: demo\ndescription: d\ncredentials:\n  demo_token:\n    description: t\n  api_secret:\n    description: s\n    type: " + typ + "\n---\n"
	}
	for _, typ := range creds.DerivedTypes {
		dir := write(t, map[string]string{"PLUGIN.md": manifest(typ)})
		if _, err := Load(dir, nil); err != nil {
			t.Fatalf("type %s refused by the plugin validator: %v", typ, err)
		}
	}
	dir := write(t, map[string]string{"PLUGIN.md": manifest("aws_sts")})
	if _, err := Load(dir, nil); err == nil || !strings.Contains(err.Error(), strings.Join(creds.DerivedTypes, ", ")) {
		t.Fatalf("unknown type must be refused naming the known set, got %v", err)
	}
}
