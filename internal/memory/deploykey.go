package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvDeployKey delivers the SSH private key that lets a deployed agent push
// its memory branch. Scope the key to the one repository. EnvDeployKeyFile
// is the preferred production delivery: a path to the key, so the key value
// itself never appears in /proc/pid/environ.
const (
	EnvDeployKey     = "OPENROUTINES_DEPLOY_KEY"
	EnvDeployKeyFile = "OPENROUTINES_DEPLOY_KEY_FILE"
)

// sshCommand, when set, is exported as GIT_SSH_COMMAND on every git
// invocation so pushes and fetches authenticate with the deploy key.
var sshCommand string

// ConfigureDeployKey materializes OPENROUTINES_DEPLOY_KEY (if present) as a
// supervisor-only key file and routes all git SSH through it. Returns whether
// a key was configured. Host keys are trusted on first use (accept-new);
// pinning is tracked in the hardening backlog.
func ConfigureDeployKey() (bool, error) {
	key := ""
	if path := os.Getenv(EnvDeployKeyFile); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return false, fmt.Errorf("%s: %w", EnvDeployKeyFile, err)
		}
		key = string(raw)
	}
	if key == "" {
		key = os.Getenv(EnvDeployKey)
	}
	if key == "" {
		return false, nil
	}
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
	sshCommand = fmt.Sprintf(
		"ssh -i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=%s",
		keyPath, filepath.Join(sshDir, "known_hosts"),
	)
	return true, nil
}
