package knowledge

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/steadyspacecorp/openroutines/internal/scrub"
)

// EnvDeployKey delivers the SSH private key that lets a deployed agent push
// its knowledge branch. Scope the key to the one repository. EnvDeployKeyFile
// is the preferred production delivery: a path to the key, so the key value
// itself never appears in /proc/pid/environ.
const (
	EnvDeployKey     = "OPENROUTINES_DEPLOY_KEY"
	EnvDeployKeyFile = "OPENROUTINES_DEPLOY_KEY_FILE"
)

// sshCommand, when set, is exported as GIT_SSH_COMMAND on every git
// invocation so pushes and fetches authenticate with the deploy key.
var sshCommand string

// originRewrite, when set, is passed as -c url.<ssh>.insteadOf=<https> on
// every git invocation so an HTTPS origin authenticates with the deploy key.
var originRewrite []string

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
	// Keepalives bound a silently dropped connection: without them a stalled
	// push parks until the kernel gives up, and the single-instance lease
	// goes stale underneath a supervisor that is still running.
	sshCommand = fmt.Sprintf(
		"ssh -i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=%s"+
			" -o ConnectTimeout=10 -o ServerAliveInterval=15 -o ServerAliveCountMax=4",
		keyPath, filepath.Join(sshDir, "known_hosts"),
	)
	return true, nil
}

// registerDeployKey puts the key material in the scrub registry the moment
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

// ConfigureOriginRewrite routes an HTTPS origin through SSH, so the deploy
// key -- an SSH credential -- can authenticate it. `scaffold` leaves the
// origin for the operator to add and `gh` defaults to https, so an HTTPS
// origin is the common case; without this the container has no credential
// for it at all and git exits asking for a username it cannot read. The
// rewrite lives on the supervisor's own invocations: .git/config and the
// built image stay as the operator wrote them.
//
// Left alone: an origin that is already SSH, one carrying credentials of its
// own, and one on a non-default port, whose SSH port the URL does not say.
//
// Reports whether a rewrite was applied, so the supervisor can log the
// protocol switch: a boot that suddenly speaks SSH to a host configured as
// https is otherwise invisible when SSH egress is what's failing.
func ConfigureOriginRewrite(repoDir string) bool {
	if sshCommand == "" {
		return false
	}
	raw, err := git(repoDir, "remote", "get-url", "origin")
	if err != nil {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.Port() != "" || u.User != nil {
		return false
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return false
	}
	originRewrite = []string{
		"-c", fmt.Sprintf("url.git@%s:.insteadOf=%s://%s/", u.Host, u.Scheme, u.Host),
	}
	return true
}
