package sandbox

import (
	"os/exec"
	"slices"
)

// An OS package (`apt install bubblewrap`) rather than a linked library: a
// subprocess boundary is a much cheaper coupling for the supervisor's
// dependency tree than an in-process one.
const bwrap = "bwrap"
const unshare = "unshare"

// How /proc reaches a sandbox, as bwrap arguments.
type procMount []string

// A private procfs hides peer attempts, and needs the fresh pid namespace the
// argv below always unshares -- procfs is per-pid-namespace, so `--proc` alone
// is a fresh mount but not a fresh view. The kernel refuses it inside a
// container whose runtime masks /proc paths (mount_too_revealing, Linux 4.2;
// runc masks /proc/kcore and friends by default), where binding the container's
// own /proc read-only costs process-list privacy and nothing else.
var (
	privateProc = procMount{"--proc", "/proc"}
	sharedProc  = procMount{"--ro-bind", "/proc", "/proc"}
)

// The preferred rung: a private mount, pid, ipc, uts and user namespace per
// attempt. The observable variants differ only in how /proc reaches the
// sandbox. Each can also create its user namespace outside bwrap for runtimes
// that reject creating it together with the mount namespace.
type bubblewrap struct {
	proc               procMount
	outerUserNamespace bool
}

func (b bubblewrap) private() bool { return slices.Equal(b.proc, privateProc) }

func (b bubblewrap) Name() string {
	userNamespace := ""
	if b.outerUserNamespace {
		userNamespace = ", outer user namespace"
	}
	if b.private() {
		return "bubblewrap namespaces" + userNamespace + ", private /proc"
	}
	return "bubblewrap namespaces" + userNamespace + ", shared /proc"
}

// Everything a set of namespaces gives by construction. Only the process list
// varies, and even a shared one leaves a peer's environment, memory and
// filesystem unreadable: that is a PTRACE_MODE_READ check, which no process
// satisfies against one whose mm belongs to a different user namespace.
func (b bubblewrap) Capabilities() Capabilities {
	return Capabilities{
		UnnameablePaths: true,
		// True on both variants: a peer lives in a pid namespace this attempt
		// is not in, so there is no pid it could name to signal one.
		UnsignalablePeers:  true,
		PrivateProcessList: b.private(),
		PrivateIPC:         true,
		PrivateTmp:         true,
		CollapsesTree:      true,
	}
}

// Command wraps argv in the sandbox a describes. The model process lands in a
// fresh pid namespace as pid 2 -- pid 1 is a trivial bwrap init that reaps
// orphans -- so killing this process collapses the namespace and takes every
// descendant with it, including any that escaped into its own session.
func (b bubblewrap) Command(workspace string, argv ...string) (*exec.Cmd, error) {
	// Validated here, but the argv below uses the names as given: bwrap
	// resolves a bind's source itself, and the destination has to keep the
	// name the run was told to use, which is the unresolved one.
	if err := validateWorkspace(workspace); err != nil {
		return nil, err
	}
	args := []string{
		"--unshare-pid", "--unshare-ipc", "--unshare-uts",
		// Try, not require: this namespace only hides which cgroup the attempt
		// is in, and gVisor -- the sandbox under several managed container
		// hosts -- rejects CLONE_NEWCGROUP unless cgroup2 is mounted.
		"--unshare-cgroup-try",
		// bwrap auto-creates a user namespace only when EUID != 0, so an
		// attempt under a root supervisor would otherwise keep the container's
		// whole capability set. Nothing in a model process needs one.
		"--cap-drop", "ALL",
		// Without a new session (or a seccomp filter on TIOCSTI) a sandboxed
		// process can push characters into the controlling terminal's input
		// queue and run commands outside the sandbox. Required, not optional.
		"--new-session",
		"--die-with-parent",
	}
	if !b.outerUserNamespace {
		args = append([]string{"--unshare-user"}, args...)
	}
	for _, p := range slices.Concat(readOnlyOS, osConfig) {
		args = append(args, "--ro-bind-try", p, p)
	}
	args = append(args, b.proc...)
	args = append(args,
		"--dev", "/dev", // a fresh minimal devtmpfs: /dev/shm is this attempt's alone
		"--tmpfs", "/tmp", // the workspace binds over this, so it must come first
		"--bind", workspace, workspace,
	)
	args = append(args, "--chdir", workspace, "--")
	if b.outerUserNamespace {
		// gVisor rejects bwrap's combined user-and-mount namespace clone but
		// permits the same namespaces when the mapped user namespace exists first.
		args = append([]string{"--user", "--map-root-user", "--", bwrap}, args...)
		return exec.Command(unshare, append(args, argv...)...), nil
	}
	return exec.Command(bwrap, append(args, argv...)...), nil
}
