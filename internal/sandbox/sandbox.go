// Package sandbox confines model processes to declared filesystem paths
// with Landlock on Linux; other platforms report ErrUnsupported.
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
// OS, and the opencode installation; read-write on the staged memory the
// runner names, the run tmp, and the attempt's disposable HOME -- all inside
// the workspace -- plus /dev.
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
// non-dumpable (ProtectProcess), keys are file-delivered, and every child the
// supervisor spawns (git included) gets a constructed, secret-free
// environment. That last one matters because non-dumpable does not survive
// execve: a child inheriting the supervisor's environment would publish the
// master key in its own /proc/<pid>/environ.
func Paths(workspace, memoryDir, runTmp, home, attemptHome string) (ro, rw []string) {
	ro = []string{
		workspace,
		"/usr", "/bin", "/sbin", "/lib", "/lib64", "/opt", "/etc", "/proc",
		filepath.Join(home, ".opencode"), // the opencode binary itself
	}
	rw = []string{
		memoryDir,
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
