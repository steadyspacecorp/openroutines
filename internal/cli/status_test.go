package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const statusAgentYAML = `name: test-agent
description: Tests status
owner:
  name: CI
  email: ci@example.invalid
timezone: UTC
defaults:
  model: fake/model
`

// statusAgent builds an agent directory with the given routines, keyed by
// name -> frontmatter+body.
func statusAgent(t *testing.T, routines map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openroutines.yml"), []byte(statusAgentYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "routines"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range routines {
		if err := os.WriteFile(filepath.Join(dir, "routines", name+".md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// capture runs a command from dir and returns everything it printed.
func capture(t *testing.T, dir string, run func()) string {
	t.Helper()
	t.Chdir(dir)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	run()
	os.Stdout = stdout
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// Web access and MCP servers are authorities like skills and credentials; the
// surfaces that answer "what can this routine reach" have to name them all.
func TestGrantSurfacesNameEveryAuthority(t *testing.T) {
	dir := statusAgent(t, map[string]string{
		"reach": "---\nschedule: \"0 9 * * *\"\nskills: []\ncredentials: [api_token]\nmcp: [feed]\nwebfetch: true\nwebsearch: true\n---\nReach out.\n",
		"quiet": "---\nschedule: \"0 9 * * *\"\n---\nStay in.\n",
	})

	for name, run := range map[string]func(){
		"status": func() { cmdStatus(nil) },
		"list":   func() { routinesList() },
	} {
		out := capture(t, dir, run)
		for _, want := range []string{"creds:1", "mcp:1", "webfetch", "websearch"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s missing grant %q:\n%s", name, want, out)
			}
		}
		for _, line := range strings.Split(out, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "quiet") {
				continue
			}
			for _, grant := range []string{"skills:", "creds:", "mcp:", "webfetch", "websearch"} {
				if strings.Contains(line, grant) {
					t.Fatalf("%s: a routine with no grants should list none: %q", name, line)
				}
			}
		}
	}
}
