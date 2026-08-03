// Package sandbox confines model processes to declared filesystem paths
// with Landlock on Linux; other platforms report ErrUnsupported.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// ErrUnsupported reports that Landlock does not exist on this platform.
// Local runs are confined by the run container instead; native mode off
// Linux is an explicit development opt-in.
var ErrUnsupported = errors.New("landlock is unavailable on this platform")

// AttemptUIDBase is the first identity in the reserved production attempt
// pool. The supervisor's run slots use AttemptUIDBase through
// AttemptUIDBase+concurrency-1; the identity one past the concurrency
// ceiling is reserved for manual runs (`routines run` inside the container),
// so a manual attempt can never collide with a supervisor slot. The template
// Dockerfile pre-creates all of them and puts the agent user in each
// identity's group.
const AttemptUIDBase = 20000

// EnsureAttemptGroups makes this process a member of every attempt group,
// joining the missing ones with the binary's cap_setgid. The image grants
// the membership via useradd -G, but whether it reaches the process depends
// on the init that booted the container: some call initgroups, others set
// only uid and gid and clear the supplementary groups. So membership is
// asserted here, not assumed from /etc/group.
func EnsureAttemptGroups(identities int) error {
	current, err := os.Getgroups()
	if err != nil {
		return fmt.Errorf("attempt group check: %w", err)
	}
	var missing []int
	for i := range identities {
		if gid := AttemptUIDBase + i; !slices.Contains(current, gid) {
			missing = append(missing, gid)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if err := syscall.Setgroups(append(current, missing...)); err != nil {
		return fmt.Errorf("joining the attempt groups: %w -- the binary needs cap_setgid; rebuild the deploy image from the current template Dockerfile", err)
	}
	return nil
}

// HelperPath is the capless production re-exec target. The supervisor binary
// is executable only by the agent identity because it carries UID-switching
// capabilities; attempts may execute this copy, but it carries no capability
// with which to change identity.
const HelperPath = "/usr/local/lib/openroutines/sandbox-exec"

// EnvUnsafeOverride deliberately disables the fail-closed sandbox policy.
// The name is ugly on purpose.
const EnvUnsafeOverride = "OPENROUTINES_UNSAFE_NO_SANDBOX"

// Env vars carrying rule paths from the runner to sandbox-exec
// (os.PathListSeparator-joined).
const (
	EnvRO         = "OPENROUTINES_SANDBOX_RO"
	EnvRW         = "OPENROUTINES_SANDBOX_RW"
	EnvAttemptUID = "OPENROUTINES_ATTEMPT_UID"
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
		filepath.Join(home, ".opencode"), // opencode's per-user install (native mode; absent in the container)
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
