package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/version"
)

// Claude Code reads CLAUDE.md, not AGENTS.md, so the scaffold links one to
// the other -- a symlink, not a copy, so template updates to AGENTS.md can
// never leave a stale CLAUDE.md behind.
func TestNewLinksClaudeMdToAgentsMd(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	if code := cmdNew([]string{dir}); code != 0 {
		t.Fatalf("new exited %d", code)
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

func TestNewWritesAvatar(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	if code := cmdNew([]string{dir}); code != 0 {
		t.Fatalf("new exited %d", code)
	}
	svg, err := os.ReadFile(filepath.Join(dir, "avatar.svg"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(svg), "<svg") {
		t.Fatalf("avatar.svg does not start with an <svg> tag: %.40q", svg)
	}
	png, err := os.ReadFile(filepath.Join(dir, "avatar.png"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(png, []byte("\x89PNG")) {
		t.Fatalf("avatar.png lacks the PNG signature: %.8q", png)
	}
}

func TestNewKeepsFrameworkFilesUnderOpenRoutines(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	if code := cmdNew([]string{dir}); code != 0 {
		t.Fatalf("new exited %d", code)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".openroutines", "version"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != version.Version+"\n" {
		t.Fatalf("version = %q, want %q", got, version.Version+"\n")
	}
	if _, err := os.Stat(filepath.Join(dir, ".openroutines-version")); !os.IsNotExist(err) {
		t.Fatalf("legacy version pin exists: %v", err)
	}
}

func TestNewLeavesRepoForAfterPublishing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	if code := cmdNew([]string{dir}); code != 0 {
		t.Fatalf("new exited %d", code)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "openroutines.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\nrepo:\n") {
		t.Fatalf("new config does not contain an empty repo field:\n%s", raw)
	}
}
