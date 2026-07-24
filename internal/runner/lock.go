package runner

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// ErrRoutineLocked reports that another process holds the routine's lock --
// an attempt is already in flight.
var ErrRoutineLocked = errors.New("routine attempt already in flight")

// LockRoutine takes the per-routine kernel lock (DESIGN.md "Overlap"): a
// non-blocking flock on .openroutines-tmp/locks/<name>.lock, held for the
// whole attempt lifecycle -- snapshot through import and settlement. Both
// the manual path (routines run|test) and the supervisor cross this seam,
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
