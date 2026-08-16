package repository

import "testing"

func TestGitOriginConvertsGitHubReferencesToSSH(t *testing.T) {
	for value, want := range map[string]string{
		"acme/agent":                        "ssh://git@ssh.github.com:443/acme/agent.git",
		"acme/agent.git":                    "ssh://git@ssh.github.com:443/acme/agent.git",
		"https://github.com/acme/agent":     "ssh://git@ssh.github.com:443/acme/agent.git",
		"https://github.com/acme/agent.git": "ssh://git@ssh.github.com:443/acme/agent.git",
		"https://GITHUB.COM/acme/agent.git": "ssh://git@ssh.github.com:443/acme/agent.git",
		"  https://github.com/acme/agent\n": "ssh://git@ssh.github.com:443/acme/agent.git",
		"https://user:token@github.com/acme/agent.git?source=config#repo": "ssh://git@ssh.github.com:443/acme/agent.git",
	} {
		got, err := GitOrigin(value)
		if err != nil || got != want {
			t.Errorf("GitOrigin(%q) = %q, %v; want %q", value, got, err, want)
		}
	}
}

func TestGitOriginLeavesSSHReferencesUntouched(t *testing.T) {
	for value, want := range map[string]string{
		"git@gitlab.com:acme/agent.git":               "git@gitlab.com:acme/agent.git",
		"ssh://git@git.acme.test:2222/acme/agent.git": "ssh://git@git.acme.test:2222/acme/agent.git",
		"  git@gitlab.com:acme/agent.git\n":           "git@gitlab.com:acme/agent.git",
	} {
		got, err := GitOrigin(value)
		if err != nil || got != want {
			t.Errorf("GitOrigin(%q) = %q, %v; want %q", value, got, err, want)
		}
	}
}

func TestGitOriginRejectsReferencesTheDeployKeyCannotServe(t *testing.T) {
	for _, value := range []string{
		"https://gitlab.com/acme/agent.git",
		"http://github.com/acme/agent.git",
		"git://github.com/acme/agent.git",
		"/srv/git/agent.git",
		"https://github.com/acme/agent.git\nother",
	} {
		if got, err := GitOrigin(value); err == nil {
			t.Errorf("GitOrigin(%q) = %q, want an error", value, got)
		}
	}
}
