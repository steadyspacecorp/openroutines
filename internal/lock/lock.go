// Package lock provides the agent's mutual exclusion: named locks that hold
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

// ErrLocked reports that the named lock is held -- by another process, or by
// another goroutine in this one.
var ErrLocked = errors.New("lock held")

// Take holds the named lock for one piece of work, or reports ErrLocked; it
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

// Locker returns the named lock as a sync.Locker, for the callers that take
// and release it over and over -- and that would rather wait their turn than
// hear that someone else is ahead of them.
func Locker(dir, name string) (sync.Locker, error) {
	return open(dir, name)
}

// fileLock is mutual exclusion backed by flock(2) on a file under
// .openroutines-tmp/locks. Exclusion spans processes -- the supervisor and a
// manual `routines run` are separate processes -- and the kernel drops the
// lock when its holder dies. The mutex extends the same exclusion to
// goroutines sharing one fileLock: flock is per open file description, so a
// second acquisition through the same handle silently succeeds and the first
// release would drop the lock for everyone. Holding the mutex for as long as
// the flock guarantees strict acquire/release alternation on the description.
type fileLock struct {
	mu sync.Mutex
	f  *os.File
}

// open opens, creating if absent, the file behind a named lock. Read-only,
// and permissive on creation, because whoever creates the file must not
// decide who can lock it afterwards: flock locks the open file description
// regardless of the mode the file was opened in, and these files have no
// contents anyone reads or writes. Asking for write access is asking for a
// permission we never use, and one a manual run made as another identity --
// root, over `fly ssh console` -- takes away for good: the supervisor failed
// every dispatch of that one routine, once a minute, until the container was
// replaced. The mode is set after creation because umask would otherwise
// decide it, and it is best effort: on a file we did not create it is not
// ours to widen, and read-only was enough anyway. World-writable is not an
// exposure the tree does not already have -- the directory these sit in is
// reachable only by identities that can already read the whole repository.
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

// take acquires without waiting, reporting ErrLocked to a caller that would
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

// Lock blocks until every other holder has released. EINTR is a signal
// arriving mid-wait rather than an outcome, so the wait resumes. Anything
// else is unrecoverable and panics: a caller that asked to be serialized has
// no second option, and proceeding unserialized is the thing the lock exists
// to prevent. Logged first so the failure has a scrubbed, structured record
// in the same stream as everything else.
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

// Unlock hands the lock to the next waiter, keeping it open for another
// acquisition.
func (l *fileLock) Unlock() {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.mu.Unlock()
}

// discard closes the handle, and with it any lock held on it. The fileLock is
// spent afterwards, which is why only Take -- whose caller never sees one
// again -- hands it out.
func (l *fileLock) discard() {
	_ = l.f.Close()
}
