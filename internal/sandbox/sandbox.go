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

// Paths computes the rule sets for one attempt: read on the workspace and
// the OS, read-write on staged memory, run tmp, and opencode's own state
// dirs. Conspicuously absent: the repo, the supervisor's HOME/.ssh (the
// deploy key), and everything else.
func Paths(workspace, runTmp, home string) (ro, rw []string) {
	ro = []string{
		workspace,
		"/usr", "/bin", "/sbin", "/lib", "/lib64", "/opt", "/etc", "/proc",
		filepath.Join(home, ".opencode"), // the opencode binary itself
	}
	rw = []string{
		filepath.Join(workspace, "memory"),
		runTmp,
		"/tmp", "/dev",
		filepath.Join(home, ".local"),
		filepath.Join(home, ".config"),
		filepath.Join(home, ".cache"),
	}
	return ro, rw
}

// JoinPaths encodes a rule list for the environment.
func JoinPaths(paths []string) string {
	return strings.Join(paths, string(os.PathListSeparator))
}
