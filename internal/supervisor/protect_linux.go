//go:build linux

package supervisor

import "golang.org/x/sys/unix"

// protectSelf marks this process non-dumpable: nothing else can read its
// /proc/<pid>/environ or mem, nor ptrace-attach, without CAP_SYS_PTRACE.
// The supervisor's environment may carry the master and deploy keys, so it
// does this before any child exists. A sandboxed model process is already
// kept from the supervisor by its own confinement; this is the belt under
// that brace, and it also covers anything sharing the container that is not
// a sandboxed attempt.
func protectSelf() error {
	return unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)
}
