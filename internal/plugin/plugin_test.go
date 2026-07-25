package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		"opencode.json":   {map[string]string{"opencode.json": "{}"}, "harness config"},
		"agent.yaml":      {map[string]string{"agent.yaml": "name: x"}, "belongs to the agent"},
		"install hook":    {map[string]string{"install.sh": "#!/bin/sh\n"}, "allow-listed"},
		"shared memory":   {map[string]string{"memory/events.md": "- sneaky\n"}, "never shared memory"},
		"master key":      {map[string]string{"master.key": "k"}, "key material"},
		"dangling skill":  {map[string]string{"routines/other.md": "---\nschedule: \"0 9 * * *\"\nskills: [ghost]\n---\nx\n"}, "neither the plugin nor the agent"},
		"undeclared cred": {map[string]string{"routines/other.md": "---\nschedule: \"0 9 * * *\"\ncredentials: [ghost_token]\n---\nx\n"}, "missing from the PLUGIN.md credentials block"},
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
	if got := p.Collisions(agent); len(got) != 0 {
		t.Fatalf("fresh agent should have no collisions: %v", got)
	}

	// No memory worktree yet: stubs go pending, the rest installs.
	installed, pending, err := p.Install(agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || !strings.Contains(pending[0], "demo.md") {
		t.Fatalf("stub should be pending without a worktree: %v", pending)
	}
	for _, rel := range []string{"routines/demo.md", "skills/demo-skill/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(agent, rel)); err != nil {
			t.Fatalf("%s not installed: %v", rel, err)
		}
	}
	if len(installed) != 2 {
		t.Fatalf("installed %v, want the routine and the skill", installed)
	}

	// Now everything collides.
	if got := p.Collisions(agent); len(got) != 2 {
		t.Fatalf("want 2 collisions after install, got %v", got)
	}

	// With a worktree, the stub lands -- unless the ledger already exists,
	// which is live memory and never clobbered.
	agent2 := t.TempDir()
	os.MkdirAll(filepath.Join(agent2, "memory", "ledgers"), 0o755)
	installed, pending, err = p.Install(agent2)
	if err != nil || len(pending) != 0 {
		t.Fatalf("stub should install into an existing worktree: pending=%v err=%v", pending, err)
	}
	if len(installed) != 3 {
		t.Fatalf("installed %v, want routine, skill, and stub", installed)
	}
	agent3 := t.TempDir()
	os.MkdirAll(filepath.Join(agent3, "memory", "ledgers"), 0o755)
	os.WriteFile(filepath.Join(agent3, "memory", "ledgers", "demo.md"), []byte("live state\n"), 0o644)
	_, pending, err = p.Install(agent3)
	if err != nil || len(pending) != 1 {
		t.Fatalf("existing ledger must go pending, not be clobbered: pending=%v err=%v", pending, err)
	}
	if raw, _ := os.ReadFile(filepath.Join(agent3, "memory", "ledgers", "demo.md")); string(raw) != "live state\n" {
		t.Fatal("live ledger was overwritten")
	}
}
