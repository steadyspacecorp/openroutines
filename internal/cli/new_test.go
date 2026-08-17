package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/version"
)

func TestNewInitializesCredentialsWithAFreshConventionalKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	t.Setenv(creds.EnvMasterKey, creds.GenerateKey())
	t.Setenv(creds.EnvMasterKeyFile, "/tmp/should-not-be-used")
	var code int
	out := capture(t, ".", func() { code = cmdNew([]string{dir}) })
	if code != 0 {
		t.Fatalf("new exited %d", code)
	}
	if strings.Contains(out, "master.key") || strings.Contains(out, "credential") {
		t.Fatalf("new exposed credential implementation details:\n%s", out)
	}

	t.Setenv(creds.EnvMasterKey, "")
	t.Setenv(creds.EnvMasterKeyFile, "")
	keyPath := filepath.Join(dir, creds.KeyFileName)
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("master key mode = %04o, want 0600", info.Mode().Perm())
	}
	key, err := creds.LoadKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := creds.Read(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(store) != 0 {
		t.Fatalf("new credential store = %v, want empty", store)
	}
}

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

func TestNewInitializesRepoWithoutCommittingScaffold(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	if code := cmdNew([]string{dir}); code != 0 {
		t.Fatalf("new exited %d", code)
	}
	cmd := exec.Command("git", "rev-parse", "--verify", "HEAD")
	cmd.Dir = dir
	if err := cmd.Run(); err == nil {
		t.Fatal("new created an initial commit")
	}
	cmd = exec.Command("git", "status", "--porcelain", "--untracked-files=all")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "?? openroutines.yml\n") {
		t.Fatalf("scaffold is not present as uncommitted work:\n%s", out)
	}
	if !strings.Contains(string(out), "?? "+creds.FileName+"\n") {
		t.Fatalf("encrypted credential store is not present as uncommitted work:\n%s", out)
	}
	if strings.Contains(string(out), creds.KeyFileName) {
		t.Fatalf("master key is not ignored:\n%s", out)
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
