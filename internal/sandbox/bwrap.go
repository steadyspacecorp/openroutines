package sandbox

import (
	"os/exec"
	"slices"
)

// bwrap is the sandbox helper, an OS package (`apt install bubblewrap`)
// rather than a linked library: a subprocess boundary is a much cheaper
// coupling for the supervisor's dependency tree than an in-process one.
const bwrap = "bwrap"

// procMount is how /proc reaches a sandbox, as bwrap arguments.
type procMount []string

// A private procfs shows the attempt only its own pid namespace: a peer
// attempt is not visible at all. `--proc` alone does not ask for that --
// procfs is per-pid-namespace, so a fresh mount is only a fresh *view* when
// a new pid namespace is unshared too, which the argv below always does. The
// kernel refuses that mount inside a container whose runtime masks /proc
// paths -- since Linux 4.2, a non-initial user namespace may not mount a
// procfs less obscured than one it can already see (mount_too_revealing),
// and runc masks /proc/kcore and friends by default. Where that applies the
// container's own /proc is bound in read-only instead, which costs
// process-list privacy and nothing else.
var (
	privateProc = procMount{"--proc", "/proc"}
	sharedProc  = procMount{"--ro-bind", "/proc", "/proc"}
)

// bubblewrap is the preferred rung: a private mount, pid, ipc, uts and user
// namespace per attempt. It exists in two variants that differ only in how
// /proc reaches the sandbox, because on a runtime that masks /proc paths the
// private mount is refused by the kernel and the weaker variant is still
// worth far more than nothing.
type bubblewrap struct{ proc procMount }

func (b bubblewrap) private() bool { return slices.Equal(b.proc, privateProc) }

func (b bubblewrap) Name() string {
	if b.private() {
		return "bubblewrap namespaces, private /proc"
	}
	return "bubblewrap namespaces, shared /proc"
}

// Capabilities: everything a set of namespaces gives by construction. Only
// the process list varies, and only because the kernel may refuse the
// private procfs mount. Even then a peer's environment, memory and
// filesystem stay unreadable -- that is a PTRACE_MODE_READ check, which no
// process satisfies against one whose mm belongs to a different user
// namespace, same uid or not.
func (b bubblewrap) Capabilities() Capabilities {
	return Capabilities{
		UnnameablePaths: true,
		// A peer lives in a pid namespace this attempt is not in, so there is
		// no pid it could name to signal one -- true on both variants, since
		// what the shared /proc costs is seeing that a peer exists, not the
		// ability to reach into its namespace.
		UnsignalablePeers:  true,
		PrivateProcessList: b.private(),
		PrivateIPC:         true,
		PrivateTmp:         true,
		CollapsesTree:      true,
	}
}

// Command wraps argv in the sandbox a describes. The returned command runs
// bwrap, which puts the model process in a fresh pid namespace as pid 2 --
// pid 1 is a trivial bwrap init that reaps orphans, so an abandoned
// subprocess leaves no zombie. Killing this process tears down every
// descendant the attempt created, including any that escaped into its own
// session or process group: the namespace collapses with its init.
func (b bubblewrap) Command(a Attempt, argv ...string) (*exec.Cmd, error) {
	// Validated here, but the argv below uses the names as given: bwrap
	// resolves a bind's source itself, and the destination has to keep the
	// name the run was told to use, which is the unresolved one.
	if err := a.validate(); err != nil {
		return nil, err
	}
	args := []string{
		"--unshare-user", "--unshare-pid", "--unshare-ipc", "--unshare-uts",
		// Skipped rather than fatal: a cgroup namespace hides which cgroup the
		// attempt belongs to and nothing else, and gVisor -- the sandbox under
		// several managed container hosts -- rejects CLONE_NEWCGROUP with
		// EINVAL unless cgroup2 is mounted. Losing that one namespace is not
		// worth refusing to run on a whole class of deploy target.
		"--unshare-cgroup-try",
		// bwrap only auto-creates a user namespace when EUID != 0, and an
		// attempt started by a root supervisor would otherwise keep the
		// container's whole capability set inside it. The read-only binds hold
		// either way -- a user namespace inherits its parent's mounts locked,
		// so neither CAP_SYS_ADMIN nor CAP_MKNOD can undo one -- but nothing in
		// a model process needs a capability, so it gets none.
		"--cap-drop", "ALL",
		// Without a new session (or a seccomp filter on TIOCSTI) a sandboxed
		// process can push characters into the controlling terminal's input
		// queue and run commands outside the sandbox. Required, not optional.
		"--new-session",
		"--die-with-parent",
	}
	for _, p := range slices.Concat(readOnlyOS, osConfig) {
		args = append(args, "--ro-bind-try", p, p)
	}
	args = append(args, b.proc...)
	args = append(args,
		"--dev", "/dev", // a fresh minimal devtmpfs: /dev/shm is this attempt's alone
		"--tmpfs", "/tmp", // the workspace binds over this, so it must come first
		"--ro-bind", a.Workspace, a.Workspace,
	)
	for _, p := range a.Writable {
		args = append(args, "--bind", p, p)
	}
	args = append(args, "--chdir", a.Workspace, "--")
	return exec.Command(bwrap, append(args, argv...)...), nil
}
