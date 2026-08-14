package repository

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func deployedRepository(t *testing.T) {
	t.Helper()
	previousSSH := sshCommand
	t.Cleanup(func() { sshCommand = previousSSH })
	sshCommand = ""
	t.Setenv(EnvDeployKeyFile, "")
	t.Setenv(EnvDeployKey, "synthetic-deploy-key") // gitleaks:allow -- test fixture
	t.Setenv("HOME", t.TempDir())
}

func originRepo(t *testing.T, origin string) string {
	t.Helper()
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main", dir)
	gitT(t, dir, "remote", "add", "origin", origin)
	return dir
}

func TestPrepareFreshInitializesTheDeployedRepository(t *testing.T) {
	deployedRepository(t)
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main", dir)
	gitT(t, dir, "remote", "add", "origin", "ssh://git@ssh.github.com:443/acme/agent.git")
	gitT(t, dir, "remote", "add", "provider", "https://provider.invalid/build.git")
	gitT(t, dir, "config", "--local", "http.https://github.com/.extraheader", "Authorization: bearer build-token")
	if err := os.WriteFile(filepath.Join(dir, ".git", "provider-cruft"), []byte("leftover"), 0o644); err != nil {
		t.Fatal(err)
	}

	repo := Open(dir)
	if err := repo.Prepare("https://github.com/acme/agent.git", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "provider-cruft")); !os.IsNotExist(err) {
		t.Fatalf("provider metadata survived: %v", err)
	}
	if got := rawOriginURL(t, dir); got != "ssh://git@ssh.github.com:443/acme/agent.git" {
		t.Fatalf("origin = %q", got)
	}
	if remotes, err := git(dir, "remote"); err != nil || remotes != "origin" {
		t.Fatalf("remotes = %q, err=%v", remotes, err)
	}
	keys, err := git(dir, "config", "--local", "--name-only", "--list")
	if err != nil {
		t.Fatal(err)
	}
	for _, leftover := range []string{"extraheader", "provider"} {
		if strings.Contains(strings.ToLower(keys), leftover) {
			t.Errorf("provider config %q survived:\n%s", leftover, keys)
		}
	}
}

func TestPreparePreservesTheRuntimeRepositoryOnRestart(t *testing.T) {
	deployedRepository(t)
	dir := t.TempDir()
	repo := Open(dir)
	if err := repo.Prepare("https://github.com/acme/agent.git", true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "local"), []byte("unpublished"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.CommitAll("Unpublished runtime state"); err != nil {
		t.Fatal(err)
	}
	tip := gitT(t, dir, "rev-parse", "HEAD")

	if err := Open(dir).Prepare("https://github.com/acme/agent.git", true); err != nil {
		t.Fatal(err)
	}
	if got := gitT(t, dir, "rev-parse", "HEAD"); got != tip {
		t.Fatalf("runtime tip changed from %s to %s", tip, got)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "local")); err != nil || string(raw) != "unpublished" {
		t.Fatalf("runtime state was not preserved: %q, %v", raw, err)
	}
}

func TestPrepareRefusesToReplaceRuntimeRepositoryForAChangedRepo(t *testing.T) {
	deployedRepository(t)
	dir := t.TempDir()
	repo := Open(dir)
	if err := repo.Prepare("https://github.com/acme/original.git", true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "local"), []byte("unpublished"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.CommitAll("Unpublished runtime state"); err != nil {
		t.Fatal(err)
	}
	tip := gitT(t, dir, "rev-parse", "HEAD")

	err := Open(dir).Prepare("https://github.com/acme/replacement.git", true)
	if err == nil || !strings.Contains(err.Error(), "refusing to replace") {
		t.Fatalf("err=%v", err)
	}
	if got := gitT(t, dir, "rev-parse", "HEAD"); got != tip {
		t.Fatalf("runtime tip changed from %s to %s", tip, got)
	}
	if got := rawOriginURL(t, dir); got != "ssh://git@ssh.github.com:443/acme/original.git" {
		t.Fatalf("origin changed to %q", got)
	}
}

func TestPrepareRequiresRepositoryBeforeRemovingDeployedGit(t *testing.T) {
	deployedRepository(t)
	dir := originRepo(t, "https://github.com/acme/legacy.git")

	err := Open(dir).Prepare("", true)
	if err == nil || !strings.Contains(err.Error(), "repo is required") {
		t.Fatalf("err=%v", err)
	}
	if got := rawOriginURL(t, dir); got != "https://github.com/acme/legacy.git" {
		t.Fatalf("origin changed before configuration failure: %q", got)
	}
}

func TestPrepareRequiresDeployKeyBeforeRemovingDeployedGit(t *testing.T) {
	dir := originRepo(t, "https://github.com/acme/legacy.git")
	t.Setenv(EnvDeployKey, "")
	t.Setenv(EnvDeployKeyFile, "")

	err := Open(dir).Prepare("acme/agent", true)
	if err == nil || !strings.Contains(err.Error(), "deploy key is required") {
		t.Fatalf("err=%v", err)
	}
	if got := rawOriginURL(t, dir); got != "https://github.com/acme/legacy.git" {
		t.Fatalf("origin changed before deploy key failure: %q", got)
	}
}

func TestPrepareValidatesRepoBeforeRemovingDeployedGit(t *testing.T) {
	deployedRepository(t)
	dir := originRepo(t, "https://github.com/acme/legacy.git")

	err := Open(dir).Prepare("https://gitlab.com/acme/agent.git", true)
	if err == nil || !strings.Contains(err.Error(), "SSH Git reference") {
		t.Fatalf("err=%v", err)
	}
	if got := rawOriginURL(t, dir); got != "https://github.com/acme/legacy.git" {
		t.Fatalf("origin changed before authentication failure: %q", got)
	}
}

func TestPreparePreservesTheLocalCheckout(t *testing.T) {
	deployedRepository(t)
	dir := originRepo(t, "https://github.com/acme/local.git")

	if err := Open(dir).Prepare("https://github.com/acme/configured.git", false); err != nil {
		t.Fatal(err)
	}
	if got := rawOriginURL(t, dir); got != "https://github.com/acme/local.git" {
		t.Fatalf("local origin changed to %q", got)
	}
}

func TestOriginNeverReturnsCredentials(t *testing.T) {
	for origin, want := range map[string]string{
		"https://build-user:secret-token@github.com/acme/agent.git": "https://github.com/acme/agent.git",
		"https://build-user:secret%token@github.com/acme/agent.git": "",
	} {
		dir := originRepo(t, origin)
		if got, ok := Open(dir).Origin(); !ok || got != want {
			t.Errorf("Origin() for %q = %q, %v; want %q, true", origin, got, ok, want)
		}
	}
}

func TestRemoteRechecksAfterAnOriginIsAdded(t *testing.T) {
	dir := t.TempDir()
	repo := Open(dir)
	if repo.Remote() {
		t.Fatal("empty directory unexpectedly has a remote")
	}
	gitT(t, dir, "init", "-q", "-b", "main", dir)
	gitT(t, dir, "remote", "add", "origin", "git@example.com:acme/agent.git")
	if !repo.Remote() {
		t.Fatal("repository did not observe the added origin")
	}
}

func rawOriginURL(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
