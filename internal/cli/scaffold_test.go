package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// Claude Code reads CLAUDE.md, not AGENTS.md, so the scaffold links one to
// the other -- a symlink, not a copy, so template updates to AGENTS.md can
// never leave a stale CLAUDE.md behind.
func TestScaffoldLinksClaudeMdToAgentsMd(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	if code := cmdScaffold([]string{dir}); code != 0 {
		t.Fatalf("scaffold exited %d", code)
	}
	link := filepath.Join(dir, "CLAUDE.md")
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("CLAUDE.md is not a symlink (mode %v)", fi.Mode())
	}
	dest, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if dest != "AGENTS.md" {
		t.Fatalf("CLAUDE.md points at %q, want AGENTS.md", dest)
	}
}
