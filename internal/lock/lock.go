// Provides the agent's mutual exclusion: named locks that hold
// across processes and die with their holder. What a lock protects, and what
// to say when one is held, belongs to the caller.
package lock

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// Reports that the named lock is held -- by another process, or by
// another goroutine in this one.
var ErrLocked = errors.New("lock held")

// Holds the named lock for one piece of work, or reports ErrLocked; it
// never waits. Call release when the work is done -- or don't: a lock dies
// with the process that holds it, so a caller that crashes strands nothing.
func Take(dir, name string) (release func(), err error) {
	l, err := open(dir, name)
	if err != nil {
		return nil, err
	}
	if err := l.take(); err != nil {
		l.discard()
		return nil, err
	}
	return l.discard, nil
}

// Returns the named lock as a sync.Locker, for the callers that take
// and release it over and over -- and that would rather wait their turn than
// hear that someone else is ahead of them.
func Locker(dir, name string) (sync.Locker, error) {
	return open(dir, name)
}

// Flock(2)-backed mutual exclusion spanning processes; the
// kernel drops the lock when its holder dies. The mutex extends exclusion to
// goroutines sharing one fileLock: flock is per open file description, so a
// second acquisition through the same handle would silently succeed.
type fileLock struct {
	mu sync.Mutex
	f  *os.File
}

// Opens, creating if absent, the file behind a named lock. Read-only
// and 0666 on purpose: flock works regardless of open mode, and a lock file
// created by another identity (root, over `fly ssh console`) with a narrower
// mode locked the supervisor out of that routine until container replacement.
// The chmod after creation beats umask and is best effort.
func open(dir, name string) (*fileLock, error) {
	lockDir := filepath.Join(dir, ".openroutines-tmp", "locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(lockDir, name+".lock"), os.O_CREATE|os.O_RDONLY, 0o666)
	if err != nil {
		return nil, err
	}
	_ = f.Chmod(0o666)
	return &fileLock{f: f}, nil
}

// Acquires without waiting, reporting ErrLocked to a caller that would
// have had to.
func (l *fileLock) take() error {
	if !l.mu.TryLock() {
		return ErrLocked
	}
	if err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		l.mu.Unlock()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return ErrLocked
		}
		return err
	}
	return nil
}

// Blocks until every other holder has released, resuming on EINTR.
// Anything else panics: proceeding unserialized is the thing the lock exists
// to prevent.
func (l *fileLock) Lock() {
	l.mu.Lock()
	for {
		err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_EX)
		if err == nil {
			return
		}
		if !errors.Is(err, syscall.EINTR) {
			slog.Error("lock failed -- refusing to proceed unserialized", "path", l.f.Name(), "error", err)
			panic(fmt.Sprintf("lock %s: %v", l.f.Name(), err))
		}
	}
}

// Hands the lock to the next waiter, keeping it open for another
// acquisition.
func (l *fileLock) Unlock() {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.mu.Unlock()
}

// Closes the handle, and with it any lock held on it. The fileLock is
// spent afterwards, which is why only Take -- whose caller never sees one
// again -- hands it out.
func (l *fileLock) discard() {
	_ = l.f.Close()
}
