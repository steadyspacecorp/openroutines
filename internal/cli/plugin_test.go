package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPluginSubdirContainment(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "plugins", "demo")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := pluginSubdir(root, "plugins/demo")
	if err != nil {
		t.Fatal(err)
	}
	if resolved, _ := filepath.EvalSymlinks(inside); got != resolved {
		t.Fatalf("got %q, want %q", got, resolved)
	}

	if _, err := pluginSubdir(root, "../outside"); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("parent escape should fail, got %v", err)
	}

	outside := t.TempDir()
	link := filepath.Join(root, "linked")
	if err := os.Symlink(outside, link); err == nil {
		if _, err := pluginSubdir(root, "linked"); err == nil || !strings.Contains(err.Error(), "escapes") {
			t.Fatalf("symlink escape should fail, got %v", err)
		}
	}
}
