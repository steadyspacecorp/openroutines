package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrUnsupported reports that Landlock does not exist on this platform.
// Local runs are confined by the run container instead; native mode off
// Linux is an explicit development opt-in.
var ErrUnsupported = errors.New("landlock is unavailable on this platform")

// EnvUnsafeOverride deliberately disables the fail-closed sandbox policy.
// The name is ugly on purpose.
const EnvUnsafeOverride = "OPENROUTINES_UNSAFE_NO_SANDBOX"

// Env vars carrying rule paths from the runner to sandbox-exec
// (os.PathListSeparator-joined).
const (
	EnvRO = "OPENROUTINES_SANDBOX_RO"
	EnvRW = "OPENROUTINES_SANDBOX_RW"
)

// Paths computes the rule sets for one attempt: read on the workspace, the
// OS, and the opencode installation; read-write on staged memory, the run
// tmp, and the attempt's disposable HOME -- all inside the workspace -- plus
// /dev.
//
// Landlock rules are additive: a grant on a parent subsumes everything
// beneath it. That is why /tmp is conspicuously absent -- the workspace
// lives inside it, and a blanket /tmp grant would make the entire workspace
// writable. Also absent: the real HOME's dotdirs (.local/.config/.cache are
// per-attempt now -- a shared writable opencode home let one routine plant
// state, plugins included, for a later, more privileged one), the repo, and
// the supervisor's ~/.ssh (the deploy key file).
//
// /proc stays readable: /dev/fd resolves through /proc/self/fd (bash process
// substitution) and node reads its own cgroup files. The secrets that made
// /proc dangerous are protected at the source instead -- the supervisor is
// non-dumpable (ProtectProcess) and keys are file-delivered, so
// /proc/<supervisor>/environ is both unreadable and empty of secrets.
func Paths(workspace, runTmp, home, attemptHome string) (ro, rw []string) {
	ro = []string{
		workspace,
		"/usr", "/bin", "/sbin", "/lib", "/lib64", "/opt", "/etc", "/proc",
		filepath.Join(home, ".opencode"), // the opencode binary itself
	}
	rw = []string{
		filepath.Join(workspace, "memory"),
		runTmp,
		attemptHome,
		"/dev",
	}
	return ro, rw
}

// JoinPaths encodes a rule list for the environment.
func JoinPaths(paths []string) string {
	return strings.Join(paths, string(os.PathListSeparator))
}
