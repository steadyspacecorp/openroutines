package filetree

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyRegularAppliesModeAndSkipPolicies(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "script"), []byte("run"), 0o755)
	os.WriteFile(filepath.Join(src, "notes"), []byte("read"), 0o644)
	os.Mkdir(filepath.Join(src, ".git"), 0o755)
	os.WriteFile(filepath.Join(src, ".git", "config"), []byte("hidden"), 0o644)

	dst := filepath.Join(t.TempDir(), "copy")
	err := CopyRegular(src, dst, Options{
		Mode: PreserveExecutables,
		Skip: func(rel string, _ fs.DirEntry) bool { return rel == ".git" },
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]fs.FileMode{"script": 0o755, "notes": 0o644} {
		info, err := os.Stat(filepath.Join(dst, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", name, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, ".git")); !os.IsNotExist(err) {
		t.Fatalf("skipped directory exists: %v", err)
	}

	dataDst := filepath.Join(t.TempDir(), "data")
	if err := CopyRegular(src, dataDst, Options{Mode: DataFiles}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dataDst, "script"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("DataFiles executable mode = %o, want 644", got)
	}
}
