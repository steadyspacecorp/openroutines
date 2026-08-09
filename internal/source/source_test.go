package source

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFetchLocalRepository(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "skills", "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "skills", "demo", "SKILL.md"), []byte("demo"), 0o644); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		args = append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet")
	git("add", "-A")
	git("commit", "--quiet", "-m", "init")
	revision := git("rev-parse", "HEAD")

	root, provenance, cleanup, err := Fetch(filepath.Join(repo, "skills", "demo"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := os.Stat(filepath.Join(root, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if provenance.Path != "skills/demo" || provenance.Revision != revision {
		t.Fatalf("provenance = %#v", provenance)
	}
	if _, _, _, err := Fetch(repo, "", "missing-revision"); err == nil {
		t.Fatal("Fetch accepted a missing revision")
	}
}

func TestResolvePathRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	if _, err := ResolvePath(root, "../outside"); err == nil {
		t.Fatal("ResolvePath accepted parent traversal")
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err == nil {
		if _, err := ResolvePath(root, "linked"); err == nil {
			t.Fatal("ResolvePath accepted a symlink escape")
		}
	}
}
