package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/version"
)

func TestSetRepoFromOrigin(t *testing.T) {
	for name, tt := range map[string]struct {
		config string
		origin string
		want   string
	}{
		"GitHub SCP":         {checkAgentYAML + "repo:\n", "git@github.com:acme/agent.git", "https://github.com/acme/agent"},
		"GitHub HTTPS":       {checkAgentYAML, "https://github.com/acme/web.git", "https://github.com/acme/web"},
		"GitHub SSH":         {checkAgentYAML, "ssh://git@ssh.github.com:443/acme/port.git", "https://github.com/acme/port"},
		"dotted GitHub repo": {checkAgentYAML, "git@github.com:acme/agent.name.git", "https://github.com/acme/agent.name"},
		"other SSH":          {checkAgentYAML, "git@gitlab.com:acme/agent.git", "git@gitlab.com:acme/agent.git"},
		"GitHub subdomain":   {checkAgentYAML, "git@enterprise.github.com:acme/agent.git", "git@enterprise.github.com:acme/agent.git"},
		"nested GitHub path": {checkAgentYAML, "https://github.com/acme/agent/subdir", "https://github.com/acme/agent/subdir"},
		"other HTTPS":        {checkAgentYAML, "https://gitlab.com/acme/agent.git", "https://gitlab.com/acme/agent.git"},
		"malformed secret":   {checkAgentYAML, "https://build-user:secret%token@github.com/acme/agent.git", ""},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, config.FileName)
			if err := os.WriteFile(path, []byte(tt.config), 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("git", "init", "--quiet")
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git init: %v: %s", err, out)
			}
			cmd = exec.Command("git", "remote", "add", "origin", tt.origin)
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git remote: %v: %s", err, out)
			}
			agent, err := config.Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := setRepoFromOrigin(dir, agent); err != nil {
				t.Fatal(err)
			}
			updated, err := config.Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			if updated.Repo != tt.want {
				t.Fatalf("repo = %q, want %q", updated.Repo, tt.want)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), "name: test-agent") {
				t.Fatalf("setting repo lost existing config:\n%s", raw)
			}
		})
	}
}

func TestUpdateDoesNotSetRepoForAnAlreadyCurrentAgent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(checkAgentYAML+"repo:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".openroutines"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".openroutines", "version"), []byte(version.Version+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"remote", "add", "origin", "git@github.com:acme/agent.git"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	t.Chdir(dir)
	if code := cmdUpdate(nil); code != 0 {
		t.Fatalf("update exited %d", code)
	}
	agent, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if agent.Repo != "" {
		t.Fatalf("repo was set despite the version guard: %q", agent.Repo)
	}
}
