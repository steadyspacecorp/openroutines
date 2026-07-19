package memory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateAcceptsPlainFiles(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "ledgers"), 0o755)
	os.WriteFile(filepath.Join(dir, "worklog.md"), []byte("fact\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "ledgers", "x.md"), []byte("state\n"), 0o644)
	if err := Validate(dir); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
}

func TestValidateRejectsGitControlFiles(t *testing.T) {
	for _, name := range []string{".gitattributes", ".gitmodules", ".gitignore"} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644)
		if err := Validate(dir); err == nil {
			t.Fatalf("expected rejection for %s", name)
		}
	}
}

func TestValidateRejectsSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.md")
	os.WriteFile(target, []byte("x"), 0o644)
	if err := os.Symlink(target, filepath.Join(dir, "link.md")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if err := Validate(dir); err == nil {
		t.Fatal("expected rejection for symlink")
	}
}

func TestValidateRejectsOversizedFile(t *testing.T) {
	dir := t.TempDir()
	big := make([]byte, maxFile+1)
	os.WriteFile(filepath.Join(dir, "big.md"), big, 0o644)
	if err := Validate(dir); err == nil {
		t.Fatal("expected rejection for oversized file")
	}
}
