package repository

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/steadyspacecorp/openroutines/internal/scrub"
)

// Delivers the SSH private key that lets a deployed agent push
// its knowledge branch. Scope the key to the one repository.
const (
	DeployKeyFileName = "deploy.key"
	EnvDeployKey      = "OPENROUTINES_DEPLOY_KEY"
	EnvDeployKeyFile  = "OPENROUTINES_DEPLOY_KEY_FILE"
)

// When set, is exported as GIT_SSH_COMMAND on every git
// invocation so pushes and fetches authenticate with the deploy key.
var sshCommand string

// DeployKeyFilePath reports the selected key file, or nothing when a direct
// value wins or the conventional file does not exist.
func DeployKeyFilePath(dir string) string {
	if os.Getenv(EnvDeployKey) != "" {
		return ""
	}
	if path := os.Getenv(EnvDeployKeyFile); path != "" {
		return path
	}
	path := filepath.Join(dir, DeployKeyFileName)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return ""
	}
	return path
}

// Materializes the selected deploy key as a supervisor-only file and routes
// all Git SSH through it. Host keys are trusted on first use (accept-new);
// pinning is tracked in the hardening backlog.
func configureDeployKey(dir string) (bool, error) {
	sshCommand = ""
	key := os.Getenv(EnvDeployKey)
	if key == "" {
		path := DeployKeyFilePath(dir)
		if path == "" {
			return false, nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return false, fmt.Errorf("deploy key file %s: %w", path, err)
		}
		key = string(raw)
	}
	registerDeployKey(key)
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return false, err
	}
	if !strings.HasSuffix(key, "\n") {
		key += "\n" // OpenSSH requires a trailing newline
	}
	keyPath := filepath.Join(sshDir, "openroutines_deploy")
	if err := os.WriteFile(keyPath, []byte(key), 0o600); err != nil {
		return false, err
	}
	if err := validateDeployKey(keyPath); err != nil {
		_ = os.Remove(keyPath)
		return false, err
	}
	// Keepalives bound a silently dropped connection -- a stalled push would
	// park while the single-instance lease goes stale.
	sshCommand = fmt.Sprintf(
		"ssh -i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=%s"+
			" -o ConnectTimeout=10 -o ServerAliveInterval=15 -o ServerAliveCountMax=4",
		keyPath, filepath.Join(sshDir, "known_hosts"),
	)
	return true, nil
}

func validateDeployKey(path string) error {
	cmd := exec.Command("ssh-keygen", "-y", "-P", "", "-f", path)
	cmd.Env = []string{}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(string(out))
	if detail == "" {
		detail = err.Error()
	}
	return fmt.Errorf("deploy key is not a usable unencrypted SSH private key -- provide the complete key, including its BEGIN/END lines: %s", detail)
}

// Puts the key material in the scrub registry the moment
// it enters the process. The key is multi-line and redaction is line by
// line, so each substantial line registers as its own value.
func registerDeployKey(key string) {
	values := map[string]string{}
	for i, line := range strings.Split(key, "\n") {
		line = strings.TrimSpace(line)
		if len(line) >= 16 && !strings.HasPrefix(line, "-----") {
			values[fmt.Sprintf("deploy_key_%d", i)] = line
		}
	}
	scrub.Register(values)
}
