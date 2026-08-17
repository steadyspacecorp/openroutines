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

var ErrLocked = errors.New("lock held")

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

func Locker(dir, name string) (sync.Locker, error) {
	return open(dir, name)
}

type fileLock struct {
	mu sync.Mutex
	f  *os.File
}

// flock is per open file description, so the process-local mutex is required
// to make a second acquisition through the same handle contend as expected.

func open(dir, name string) (*fileLock, error) {
	lockDir := filepath.Join(dir, ".openroutines-tmp", "locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return nil, err
	}
	// 0666 keeps locks created by another OS user usable; flock itself does not
	// require write access, and a narrower mode would strand the routine.
	f, err := os.OpenFile(filepath.Join(lockDir, name+".lock"), os.O_CREATE|os.O_RDONLY, 0o666)
	if err != nil {
		return nil, err
	}
	_ = f.Chmod(0o666)
	return &fileLock{f: f}, nil
}

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

func (l *fileLock) Unlock() {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.mu.Unlock()
}

func (l *fileLock) discard() {
	_ = l.f.Close()
}
