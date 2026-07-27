//go:build linux

// Package sandbox applies the Landlock filesystem confinement to the model
// process (design decision "Runs are sandboxed"). Rules bind to the calling
// process and its children, so the runner re-execs through
// `openroutines sandbox-exec`, which applies the rules to itself and then
// execs opencode.
package sandbox

import (
	"os"

	"github.com/landlock-lsm/go-landlock/landlock"
	"golang.org/x/sys/unix"
)

// Apply restricts this process (and all its descendants) to read access on
// ro paths and read-write on rw paths. Returns a description of the ABI
// level that took effect. Paths that don't exist are skipped -- Landlock
// errors on nonexistent rule paths.
func Apply(ro, rw []string) (string, error) {
	roRule := landlock.RODirs(existing(ro)...)
	rwRule := landlock.RWDirs(existing(rw)...)
	// V2 adds file re-parenting (renames across directories); fall back to
	// V1 (basic fs) on older kernels. Both are strict: an error means no
	// confinement took effect, and the caller decides fail-closed policy.
	if err := landlock.V2.RestrictPaths(roRule, rwRule); err == nil {
		return "landlock v2", nil
	}
	if err := landlock.V1.RestrictPaths(roRule, rwRule); err != nil {
		return "", err
	}
	return "landlock v1", nil
}

// ProtectProcess marks this process non-dumpable: a same-UID process can no
// longer read its /proc/<pid>/environ or mem, nor ptrace-attach, without
// CAP_SYS_PTRACE. The supervisor calls this at boot, before any model
// process exists -- its environment may carry the master and deploy keys,
// and Landlock cannot help (it is a filesystem sandbox; procfs access
// checks are ptrace-mode checks, and dumpable is what fails them).
func ProtectProcess() error {
	return unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)
}

func existing(paths []string) []string {
	out := paths[:0:0]
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}
