package repository

import "testing"

// A repository whose only interesting property is its origin
// URL. `ls-remote --get-url` is git's own answer to "what would you connect
// to", insteadOf rewriting included, and it touches no network.
func originRepo(t *testing.T, origin string) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := git(dir, "init", "-q", "-b", "main", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := git(dir, "remote", "add", "origin", origin); err != nil {
		t.Fatal(err)
	}
	return dir
}

func resolvedOrigin(t *testing.T, dir string) string {
	t.Helper()
	out, err := git(dir, "ls-remote", "--get-url", "origin")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func withDeployKey(t *testing.T) {
	t.Helper()
	prevSSH, prevRewrite := sshCommand, originRewrite
	t.Cleanup(func() { sshCommand, originRewrite = prevSSH, prevRewrite })
	sshCommand = "ssh -i /root/.ssh/openroutines_deploy"
	originRewrite = nil
}

func TestOriginRewriteRoutesHTTPSOriginThroughDeployKey(t *testing.T) {
	withDeployKey(t)
	dir := originRepo(t, "https://github.com/acme/agent.git")

	ConfigureOriginRewrite(dir)

	if got, want := resolvedOrigin(t, dir), "git@github.com:acme/agent.git"; got != want {
		t.Errorf("origin resolves to %q, want %q", got, want)
	}
}

func TestOriginRewriteUsesTheOriginsSSHHostOutsideGitHub(t *testing.T) {
	withDeployKey(t)
	dir := originRepo(t, "https://git.acme.test/acme/agent.git")

	ConfigureOriginRewrite(dir)

	if got, want := resolvedOrigin(t, dir), "git@git.acme.test:acme/agent.git"; got != want {
		t.Errorf("origin resolves to %q, want %q", got, want)
	}
}

func TestOriginRewriteLeavesOriginsTheDeployKeyCannotServe(t *testing.T) {
	for _, origin := range []string{
		"git@github.com:acme/agent.git",
		"ssh://git@github.com/acme/agent.git",
		"https://user:token@github.com/acme/agent.git",
		"https://git.acme.test:8443/acme/agent.git",
		"/srv/git/agent.git",
	} {
		t.Run(origin, func(t *testing.T) {
			withDeployKey(t)
			dir := originRepo(t, origin)

			ConfigureOriginRewrite(dir)

			if got := resolvedOrigin(t, dir); got != origin {
				t.Errorf("origin resolves to %q, want it untouched (%q)", got, origin)
			}
		})
	}
}

func TestOriginRewriteRequiresADeployKey(t *testing.T) {
	withDeployKey(t)
	sshCommand = ""
	dir := originRepo(t, "https://github.com/acme/agent.git")

	ConfigureOriginRewrite(dir)

	if got := resolvedOrigin(t, dir); got != "https://github.com/acme/agent.git" {
		t.Errorf("origin rewritten without a deploy key to authenticate it: %q", got)
	}
}
