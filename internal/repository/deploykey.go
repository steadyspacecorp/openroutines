package repository

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steadyspacecorp/openroutines/internal/scrub"
)

// Delivers the SSH private key that lets a deployed agent push
// its knowledge branch. Scope the key to the one repository. EnvDeployKeyFile
// is the preferred production delivery: a path to the key, so the key value
// itself never appears in /proc/pid/environ.
const (
	EnvDeployKey     = "OPENROUTINES_DEPLOY_KEY"
	EnvDeployKeyFile = "OPENROUTINES_DEPLOY_KEY_FILE"
)

// When set, is exported as GIT_SSH_COMMAND on every git
// invocation so pushes and fetches authenticate with the deploy key.
var sshCommand string

// Materializes OPENROUTINES_DEPLOY_KEY (if present) as a
// supervisor-only key file and routes all git SSH through it. Returns whether
// a key was configured. Host keys are trusted on first use (accept-new);
// pinning is tracked in the hardening backlog.
func configureDeployKey() (bool, error) {
	sshCommand = ""
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
	// Keepalives bound a silently dropped connection -- a stalled push would
	// park while the single-instance lease goes stale.
	sshCommand = fmt.Sprintf(
		"ssh -i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=%s"+
			" -o ConnectTimeout=10 -o ServerAliveInterval=15 -o ServerAliveCountMax=4",
		keyPath, filepath.Join(sshDir, "known_hosts"),
	)
	return true, nil
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
