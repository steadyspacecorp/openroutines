package plugin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitCommitAll initializes a repository at dir if needed and commits
// everything in it, returning the commit hash.
func gitCommitAll(t *testing.T, dir, message string) string {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(err) {
		if out, err := exec.Command("git", "init", "--quiet", dir).CombinedOutput(); err != nil {
			t.Fatalf("git init: %v\n%s", err, out)
		}
	}
	git := func(args ...string) {
		t.Helper()
		args = append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("add", "-A")
	git("commit", "--quiet", "-m", message)
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// Fetch works from a temporary clone even for a local directory, so the
// vendored payload corresponds to the recorded commit, not the working tree.
func TestFetchLocalRepositoryRecordsProvenance(t *testing.T) {
	repo := t.TempDir()
	bundle := filepath.Join(repo, "plugins", "demo")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, FileName), []byte("---\nname: demo\ndescription: d\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rev := gitCommitAll(t, repo, "init")

	root, prov, cleanup, err := Fetch(bundle, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := os.Stat(filepath.Join(root, FileName)); err != nil {
		t.Fatalf("bundle root missing manifest: %v", err)
	}
	resolvedRepo, _ := filepath.EvalSymlinks(repo)
	if prov.Repository != resolvedRepo || prov.Path != "plugins/demo" || prov.Revision != rev {
		t.Fatalf("provenance = %+v, want repo %s path plugins/demo rev %s", prov, resolvedRepo, rev)
	}
	if strings.HasPrefix(root, resolvedRepo) {
		t.Fatalf("root %s should be a clone, not the source repository", root)
	}

	// An uncommitted edit must not reach the fetched payload.
	if err := os.WriteFile(filepath.Join(bundle, FileName), []byte("---\nname: demo\ndescription: dirty\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root2, _, cleanup2, err := Fetch(bundle, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup2()
	raw, err := os.ReadFile(filepath.Join(root2, FileName))
	if err != nil || strings.Contains(string(raw), "dirty") {
		t.Fatalf("fetched payload should come from the commit: %v\n%s", err, raw)
	}
}

func TestSubdirContainment(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "plugins", "demo")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := subdir(root, filepath.Join("plugins", "demo"))
	if err != nil {
		t.Fatal(err)
	}
	if resolved, _ := filepath.EvalSymlinks(inside); got != resolved {
		t.Fatalf("got %q, want %q", got, resolved)
	}

	if _, err := subdir(root, filepath.Join("..", "outside")); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("parent escape should fail, got %v", err)
	}

	outside := t.TempDir()
	link := filepath.Join(root, "linked")
	if err := os.Symlink(outside, link); err == nil {
		if _, err := subdir(root, "linked"); err == nil || !strings.Contains(err.Error(), "escapes") {
			t.Fatalf("symlink escape should fail, got %v", err)
		}
	}
}
