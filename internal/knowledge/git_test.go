package knowledge

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func TestGitChildEnvExcludesSupervisorSecrets(t *testing.T) {
	const masterKey = "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899" // gitleaks:allow -- synthetic fixture
	t.Setenv("OPENROUTINES_MASTER_KEY", masterKey)
	t.Setenv("OPENROUTINES_DEPLOY_KEY", "-----BEGIN OPENSSH PRIVATE KEY-----")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_SSL_CAINFO", "/etc/ssl/certs/corporate-proxy.pem")

	cmd := newGitCmd(t.TempDir(), []string{"status"})
	defer cmd.cancel()
	env := cmd.Env

	for _, kv := range env {
		if strings.HasPrefix(kv, "OPENROUTINES_") {
			t.Errorf("git child inherits framework variable: %q", strings.SplitN(kv, "=", 2)[0])
		}
		if strings.Contains(kv, masterKey) {
			t.Errorf("git child carries the master key: %q", strings.SplitN(kv, "=", 2)[0])
		}
	}
	for _, want := range []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"GIT_SSL_CAINFO=" + os.Getenv("GIT_SSL_CAINFO"),
	} {
		if !slices.Contains(env, want) {
			t.Errorf("git child env missing %q: %v", want, env)
		}
	}
}

// HTTPS remotes authenticate through credential helpers declared in the
// system and global gitconfig the hermetic environment suppresses; those
// entries -- and only those -- must survive as -c flags.
func TestCredentialConfigSurvivesHermeticSuppression(t *testing.T) {
	home := t.TempDir()
	config := "[credential]\n\thelper = store\n[core]\n\thooksPath = /tmp/evil\n"
	if err := os.WriteFile(home+"/.gitconfig", []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	flags := strings.Join(readCredentialConfig(), " ")
	if !strings.Contains(flags, "credential.helper=store") {
		t.Errorf("credential config missing from flags: %q", flags)
	}
	if strings.Contains(flags, "hooksPath") {
		t.Errorf("non-credential config leaked into flags: %q", flags)
	}
}

func TestGitChildEnvCarriesDeployKeySSHCommand(t *testing.T) {
	prev := sshCommand
	t.Cleanup(func() { sshCommand = prev })
	sshCommand = "ssh -i /root/.ssh/openroutines_deploy"

	cmd := newGitCmd(t.TempDir(), []string{"push"})
	defer cmd.cancel()
	if !slices.Contains(cmd.Env, "GIT_SSH_COMMAND="+sshCommand) {
		t.Error("GIT_SSH_COMMAND missing from git child env")
	}
}
