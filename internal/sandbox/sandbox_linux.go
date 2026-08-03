//go:build linux

// Package sandbox applies the Landlock filesystem confinement to the model
// process (design decision "Runs are sandboxed"). Rules bind to the calling
// process and its children, so the runner re-execs through
// `openroutines sandbox-exec`, which applies the rules to itself and then
// execs opencode.
package sandbox

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/landlock-lsm/go-landlock/landlock"
	"golang.org/x/sys/unix"
)

const identityReapTimeout = 5 * time.Second

// DropIdentity moves the shim permanently to the attempt identity. The
// openroutines binary carries only CAP_SETUID/CAP_SETGID; the capless
// opencode exec that follows cannot regain the supervisor identity.
func DropIdentity(uid uint32) error {
	if uid == 0 {
		return fmt.Errorf("attempt uid is required")
	}
	if os.Geteuid() == int(uid) && os.Getegid() == int(uid) {
		return nil // the trusted parent applied Credential before this exec
	}
	if err := unix.Setgroups(nil); err != nil {
		return fmt.Errorf("clear supplementary groups: %w", err)
	}
	if err := unix.Setresgid(int(uid), int(uid), int(uid)); err != nil {
		return fmt.Errorf("set attempt gid: %w", err)
	}
	if err := unix.Setresuid(int(uid), int(uid), int(uid)); err != nil {
		return fmt.Errorf("set attempt uid: %w", err)
	}
	if os.Geteuid() != int(uid) || os.Getegid() != int(uid) {
		return fmt.Errorf("identity transition did not take effect (uid=%d gid=%d)", os.Geteuid(), os.Getegid())
	}
	return nil
}

// ReapIdentity kills every live process still carrying an attempt UID and
// proves the identity is empty before the supervisor returns it to the pool.
// Process-group cleanup handles the ordinary tree; this catches descendants
// that escaped into another session or process group.
func ReapIdentity(uid uint32) error {
	if uid == 0 {
		return fmt.Errorf("attempt uid is required")
	}
	deadline := time.Now().Add(identityReapTimeout)
	for {
		pids, err := identityPIDs(uid)
		if err != nil {
			return err
		}
		if len(pids) == 0 {
			return nil
		}
		for _, pid := range pids {
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
				return fmt.Errorf("kill pid %d for uid %d: %w", pid, uid, err)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("uid %d still owns live processes after cleanup: %v", uid, pids)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func identityPIDs(uid uint32) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("enumerate /proc: %w", err)
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		file, err := os.Open(filepath.Join("/proc", entry.Name(), "status"))
		if err != nil {
			if os.IsNotExist(err) {
				continue // exited between enumeration and open
			}
			return nil, err
		}
		matches, zombie, scanErr := statusIdentity(file, uid)
		_ = file.Close()
		if scanErr != nil {
			return nil, fmt.Errorf("read /proc/%d/status: %w", pid, scanErr)
		}
		if matches && !zombie {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func statusIdentity(file *os.File, uid uint32) (matches, zombie bool, err error) {
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "State:"):
			zombie = strings.Contains(line, "Z (zombie)")
		case strings.HasPrefix(line, "Uid:"):
			for _, field := range strings.Fields(strings.TrimPrefix(line, "Uid:")) {
				value, parseErr := strconv.ParseUint(field, 10, 32)
				if parseErr == nil && uint32(value) == uid {
					matches = true
				}
			}
		}
	}
	return matches, zombie, scanner.Err()
}

// Apply restricts this process (and all its descendants) to read access on
// ro paths and read-write on rw paths. Returns a description of the ABI
// level that took effect, plus any rw paths that don't exist and so were
// dropped from the ruleset -- Landlock errors on nonexistent rule paths, so
// the caller can still confine the process, but must say what got left out.
func Apply(ro, rw []string) (string, []string, error) {
	roKept, _ := existing(ro)
	rwKept, rwSkipped := existing(rw)
	roRule := landlock.RODirs(roKept...)
	rwRule := landlock.RWDirs(rwKept...)
	// V2 adds file re-parenting (renames across directories); fall back to
	// V1 (basic fs) on older kernels. Both are strict: an error means no
	// confinement took effect, and the caller decides fail-closed policy.
	if err := landlock.V2.RestrictPaths(roRule, rwRule); err == nil {
		return "landlock v2", rwSkipped, nil
	}
	if err := landlock.V1.RestrictPaths(roRule, rwRule); err != nil {
		return "", rwSkipped, err
	}
	return "landlock v1", rwSkipped, nil
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

func existing(paths []string) (kept, skipped []string) {
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			kept = append(kept, p)
		} else {
			skipped = append(skipped, p)
		}
	}
	return kept, skipped
}
