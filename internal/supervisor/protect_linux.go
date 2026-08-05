//go:build linux

package supervisor

import "golang.org/x/sys/unix"

// Marks this process non-dumpable: without CAP_SYS_PTRACE nothing else can read
// its /proc/<pid>/environ or mem, or ptrace-attach. The supervisor's
// environment may carry the master and deploy keys, so it does this before any
// child exists -- the belt under the sandbox's brace, and the only cover for
// anything in the container that is not a sandboxed attempt.
func protectSelf() error {
	return unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)
}
