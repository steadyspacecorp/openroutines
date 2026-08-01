package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/sandbox"
)

// ErrRoutineLocked reports that another process holds the routine's lock --
// an attempt is already in flight.
var ErrRoutineLocked = errors.New("routine attempt already in flight")

// LockRoutine takes the per-routine kernel lock (design decision "Overlap"): a
// non-blocking flock on .openroutines-tmp/locks/<name>.lock, held for the
// whole attempt lifecycle -- snapshot through import and settlement. Both
// the manual path (routines run) and the supervisor cross this seam,
// which is exactly what it exists for: a manual run colliding with the
// supervisor's own run of the same routine would double external actions
// and race the import. The kernel drops the lock if the holder dies.
func LockRoutine(dir, name string) (release func(), err error) {
	lockDir := filepath.Join(dir, ".openroutines-tmp", "locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(lockDir, name+".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, ErrRoutineLocked
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// reserveManualIdentity reserves the manual attempt identity: the one uid
// past the supervisor's slot pool, pre-created by the template Dockerfile.
// Production refuses to spawn a model process without a reserved attempt
// uid, and only the supervisor holds the slot pool -- so `routines run`
// inside the container takes this fixed identity instead, serialized by a
// kernel lock so two manual runs cannot share it. Release reaps the
// identity the same way the supervisor reaps its slots.
func reserveManualIdentity(dir string) (uint32, func(), error) {
	const uid = uint32(sandbox.AttemptUIDBase + config.MaxConcurrency)
	lockDir := filepath.Join(dir, ".openroutines-tmp", "locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return 0, nil, err
	}
	f, err := os.OpenFile(filepath.Join(lockDir, "manual-attempt.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return 0, nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return 0, nil, errors.New("another manual run holds the manual attempt identity -- try again when it finishes")
		}
		return 0, nil, err
	}
	return uid, func() {
		if err := sandbox.ReapIdentity(uid); err != nil {
			fmt.Fprintf(os.Stderr, "warning: manual attempt identity cleanup: %v\n", err)
		}
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
