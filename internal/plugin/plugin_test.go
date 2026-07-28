package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/creds"
)

func examples(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("..", "..", "examples", "plugins", name)
}

// The reference plugins are the fixtures: each exercises a different slice
// of the format, and this test is what keeps them loadable.
func TestLoadReferencePlugins(t *testing.T) {
	steady, err := Load(examples(t, "steady"), nil)
	if err != nil {
		t.Fatalf("steady: %v", err)
	}
	if len(steady.Routines) != 2 || len(steady.Skills) != 2 || len(steady.Stubs) != 1 {
		t.Fatalf("steady shape: %d routines, %d skills, %d stubs", len(steady.Routines), len(steady.Skills), len(steady.Stubs))
	}
	if _, ok := steady.Manifest.Credentials["steady_token"]; !ok {
		t.Fatal("steady manifest missing steady_token")
	}

	slack, err := Load(examples(t, "slack"), nil)
	if err != nil {
		t.Fatalf("slack: %v", err)
	}
	if len(slack.Routines) != 1 || len(slack.Skills) != 1 {
		t.Fatalf("slack shape: %d routines, %d skills", len(slack.Routines), len(slack.Skills))
	}

	docs, err := Load(examples(t, "github-docs"), nil)
	if err != nil {
		t.Fatalf("github-docs: %v", err)
	}
	if len(docs.Routines) != 1 || len(docs.Skills) != 0 {
		t.Fatalf("github-docs shape: %d routines, %d skills", len(docs.Routines), len(docs.Skills))
	}
	if docs.Manifest.Credentials["github_app_private_key"].Type != "github_app" {
		t.Fatal("github-docs credential should be typed github_app")
	}
	if _, ok := docs.Manifest.Variables["docs_repos"]; !ok {
		t.Fatal("github-docs manifest missing the docs_repos variable")
	}

	// The grant summary must state the authorities the bundle asks for.
	sum := docs.Summary()
	for _, want := range []string{"doc-watch", "github_app_private_key", "typed: github_app", "docs_repos"} {
		if !strings.Contains(sum, want) {
			t.Fatalf("github-docs summary missing %q:\n%s", want, sum)
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
		"shared memory":            {map[string]string{"memory/events.md": "- sneaky\n"}, "never shared memory"},
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

	// A typed trigger credential is refused at load, before any copy.
	dir = write(t, map[string]string{
		"PLUGIN.md":        "---\nname: demo\ndescription: d\ncredentials:\n  gh_key:\n    description: App key\n    type: github_app\n---\n",
		"routines/demo.md": "---\ntrigger:\n  poll: https://example.com/x\n  credential: gh_key\ncredentials: [gh_key]\n---\nx\n",
	})
	if _, err := Load(dir, nil); err == nil || !strings.Contains(err.Error(), "typed") {
		t.Fatalf("typed trigger credential should be refused, got %v", err)
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

// testSource is valid provenance: Install validates it as strictly as
// ReadSource does, so the revision has to be a full commit hash.
var testSource = Source{
	Repository: "example.test/demo.git",
	Revision:   "0123456789abcdef0123456789abcdef01234567",
}

func TestCollisionsAndInstall(t *testing.T) {
	src := write(t, map[string]string{
		"skills/demo-skill/SKILL.md": "---\nname: demo-skill\ndescription: d\n---\nHow.\n",
		"memory/ledgers/demo.md":     "# demo ledger\n",
	})
	p, err := Load(src, nil)
	if err != nil {
		t.Fatal(err)
	}

	agent := t.TempDir()
	got, err := p.Collisions(agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("fresh agent should have no collisions: %v", got)
	}

	// No memory worktree yet: stubs go pending, the rest installs.
	installed, pending, err := p.Install(agent, testSource)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || !strings.Contains(pending[0], "demo.md") {
		t.Fatalf("stub should be pending without a worktree: %v", pending)
	}
	for _, rel := range []string{"plugins/demo/routines/demo.md", "plugins/demo/skills/demo-skill/SKILL.md", "plugins/demo/" + SourceFileName} {
		if _, err := os.Stat(filepath.Join(agent, rel)); err != nil {
			t.Fatalf("%s not installed: %v", rel, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(agent, "plugins", "demo", "routines", "demo.md"))
	if err != nil || !strings.Contains(string(raw), "active: false") {
		t.Fatalf("installed routine must be explicitly inactive: %v\n%s", err, raw)
	}
	if len(installed) != 1 || installed[0] != filepath.Join("plugins", "demo") {
		t.Fatalf("installed %v, want the grouped plugin directory", installed)
	}

	// Now everything collides.
	got, err = p.Collisions(agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want plugin, routine, and skill collisions after install, got %v", got)
	}

	// With a worktree, the stub lands -- unless the ledger already exists,
	// which is live memory and never clobbered.
	agent2 := t.TempDir()
	os.MkdirAll(filepath.Join(agent2, "memory", "ledgers"), 0o755)
	installed, pending, err = p.Install(agent2, testSource)
	if err != nil || len(pending) != 0 {
		t.Fatalf("stub should install into an existing worktree: pending=%v err=%v", pending, err)
	}
	if len(installed) != 2 {
		t.Fatalf("installed %v, want plugin directory and stub", installed)
	}
	agent3 := t.TempDir()
	os.MkdirAll(filepath.Join(agent3, "memory", "ledgers"), 0o755)
	os.WriteFile(filepath.Join(agent3, "memory", "ledgers", "demo.md"), []byte("live state\n"), 0o644)
	_, pending, err = p.Install(agent3, testSource)
	if err != nil || len(pending) != 1 {
		t.Fatalf("existing ledger must go pending, not be clobbered: pending=%v err=%v", pending, err)
	}
	if raw, _ := os.ReadFile(filepath.Join(agent3, "memory", "ledgers", "demo.md")); string(raw) != "live state\n" {
		t.Fatal("live ledger was overwritten")
	}
}

// Provenance that later commands would reject must be refused at install
// time, before anything is copied -- otherwise plugin list, plugin update,
// and check all fail against a plugin the user was told installed fine.
func TestInstallRefusesInvalidProvenance(t *testing.T) {
	p, err := Load(write(t, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]Source{
		"missing repository": {Revision: testSource.Revision},
		"missing revision":   {Repository: testSource.Repository},
		"short revision":     {Repository: testSource.Repository, Revision: "abc123"},
		"escaping path":      {Repository: testSource.Repository, Path: "../elsewhere", Revision: testSource.Revision},
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			agent := t.TempDir()
			if _, _, err := p.Install(agent, source); err == nil {
				t.Fatal("want refusal")
			}
			if _, err := os.Stat(filepath.Join(agent, "plugins")); !os.IsNotExist(err) {
				t.Fatalf("nothing should have been copied: %v", err)
			}
		})
	}
}

// A duplicate global name is filtered out of the routine and skill lists, so
// collision detection against an already-ambiguous agent would miss exactly
// the name in conflict. Refuse while the namespace is invalid.
func TestCollisionsFailClosedOnInvalidNamespace(t *testing.T) {
	p, err := Load(write(t, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	agent := t.TempDir()
	routine := "---\nschedule: \"0 9 * * *\"\n---\nDo the demo.\n"
	for _, rel := range []string{
		filepath.Join("routines", "demo.md"),
		filepath.Join("plugins", "other", "routines", "demo.md"),
	} {
		path := filepath.Join(agent, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(routine), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := p.Collisions(agent)
	if err == nil {
		t.Fatalf("want refusal on a duplicated namespace, got collisions %v", got)
	}
	if !strings.Contains(err.Error(), "duplicate routine") {
		t.Fatalf("error should name the duplicate: %v", err)
	}
}

func TestInstallRollsBackOnCopyFailure(t *testing.T) {
	src := write(t, map[string]string{
		"skills/demo-skill/SKILL.md": "---\nname: demo-skill\ndescription: d\n---\nHow.\n",
	})
	p, err := Load(src, nil)
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
	agent := t.TempDir()
	if _, _, err := p.Install(agent, testSource); err == nil || !strings.Contains(err.Error(), "nested .git") {
		t.Fatalf("want nested .git copy refusal, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(agent, "plugins", "demo", "routines", "demo.md")); !os.IsNotExist(err) {
		t.Fatalf("partial routine survived rollback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(agent, "plugins", "demo", "skills", "demo-skill")); !os.IsNotExist(err) {
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

func TestMergeTreesPreservesLocalEditsAndReportsConflicts(t *testing.T) {
	base := t.TempDir()
	ours := t.TempDir()
	theirs := t.TempDir()
	dest := t.TempDir()
	writeFile := func(root, rel, content string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(base, "routines/demo.md", "schedule: old\nunchanged: one\nunchanged: two\nprompt: old\n")
	writeFile(ours, "routines/demo.md", "schedule: local\nunchanged: one\nunchanged: two\nprompt: old\n")
	writeFile(theirs, "routines/demo.md", "schedule: old\nunchanged: one\nunchanged: two\nprompt: upstream\n")
	writeFile(theirs, "skills/new/SKILL.md", "new\n")

	conflicts, err := MergeTrees(base, ours, theirs, dest)
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("clean merge: conflicts=%v err=%v", conflicts, err)
	}
	raw, _ := os.ReadFile(filepath.Join(dest, "routines", "demo.md"))
	if !strings.Contains(string(raw), "schedule: local") || !strings.Contains(string(raw), "prompt: upstream") {
		t.Fatalf("merge did not preserve both sides:\n%s", raw)
	}
	if _, err := os.Stat(filepath.Join(dest, "skills", "new", "SKILL.md")); err != nil {
		t.Fatalf("upstream addition missing: %v", err)
	}

	conflictDest := t.TempDir()
	writeFile(ours, "routines/demo.md", "schedule: ours\nunchanged: one\nunchanged: two\nprompt: old\n")
	writeFile(theirs, "routines/demo.md", "schedule: theirs\nunchanged: one\nunchanged: two\nprompt: old\n")
	conflicts, err = MergeTrees(base, ours, theirs, conflictDest)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || conflicts[0] != filepath.Join("routines", "demo.md") {
		t.Fatalf("want routine conflict, got %v", conflicts)
	}
	raw, _ = os.ReadFile(filepath.Join(conflictDest, "routines", "demo.md"))
	if !strings.Contains(string(raw), "<<<<<<< local") {
		t.Fatalf("conflict markers missing:\n%s", raw)
	}
}
