package runner

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/sandbox"
)

// ErrRoutineLocked reports that another process holds the routine's lock --
// an attempt is already in flight.
var ErrRoutineLocked = errors.New("routine attempt already in flight")

// openLock opens one of the agent's lock files. Read-only, and permissive on
// creation, because whoever created the file must not decide who can lock it
// afterwards: flock locks the open file description regardless of the mode
// the file was opened in, and these files have no contents anyone reads or
// writes. Asking for write access is asking for a permission we never use,
// and one a manual run made as another identity -- root, over `fly ssh
// console` -- takes away for good: the supervisor failed every dispatch of
// that one routine, once a minute, until the container was replaced. The mode
// is set after creation because umask would otherwise decide it, and it is
// best effort: on a file we did not create it is not ours to widen, and
// read-only was enough anyway. World-writable is not an exposure the tree
// does not already have -- the directory these sit in is reachable only by
// identities that can already read the whole repository.
func openLock(dir, name string) (*os.File, error) {
	lockDir := filepath.Join(dir, ".openroutines-tmp", "locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(lockDir, name), os.O_CREATE|os.O_RDONLY, 0o666)
	if err != nil {
		return nil, err
	}
	_ = f.Chmod(0o666)
	return f, nil
}

// LockRoutine takes the per-routine kernel lock (design decision "Overlap"): a
// non-blocking flock on .openroutines-tmp/locks/<name>.lock, held for the
// whole attempt lifecycle -- snapshot through import and settlement. Both
// the manual path (routines run) and the supervisor cross this seam,
// which is exactly what it exists for: a manual run colliding with the
// supervisor's own run of the same routine would double external actions
// and race the import. The kernel drops the lock if the holder dies.
func LockRoutine(dir, name string) (release func(), err error) {
	f, err := openLock(dir, name+".lock")
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

// KnowledgeLock serializes knowledge-worktree critical sections across processes:
// the supervisor's staging snapshots and settlements, and a manual `routines
// run` executing beside it, all stage from and settle into the same worktree
// and index. The supervisor's goroutines already serialize in-process through
// the embedded mutex; the kernel lock underneath extends the same exclusion
// to other processes, and dies with its holder. The mutex is also what keeps
// the flock correct here: every goroutine shares one file description, flock
// on a description that already holds the lock is a no-op, and the first
// unlock would release it for everyone -- the mutex guarantees strict
// acquire/release alternation on that description.
type KnowledgeLock struct {
	mu sync.Mutex
	f  *os.File
}

// OpenKnowledgeLock opens the agent's cross-process knowledge-worktree lock.
func OpenKnowledgeLock(dir string) (*KnowledgeLock, error) {
	f, err := openLock(dir, "knowledge.lock")
	if err != nil {
		return nil, err
	}
	return &KnowledgeLock{f: f}, nil
}

// Lock blocks until this process's goroutines and every other process
// holding the knowledge lock have released it.
func (l *KnowledgeLock) Lock() {
	l.mu.Lock()
	for {
		err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_EX)
		if err == nil {
			return
		}
		if err != syscall.EINTR {
			// Proceeding unserialized is the corruption this lock exists to
			// prevent; there is no recoverable path from here. Logged before
			// the panic so the failure has a scrubbed, structured record in
			// the same stream as everything else.
			slog.Error("knowledge worktree lock failed -- refusing to proceed unserialized", "path", l.f.Name(), "error", err)
			panic(fmt.Sprintf("knowledge worktree lock: %v", err))
		}
	}
}

// Unlock releases the kernel lock, then the in-process mutex.
func (l *KnowledgeLock) Unlock() {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.mu.Unlock()
}

// reserveManualIdentity reserves the manual attempt identity: the one uid
// past the supervisor's slot pool, pre-created by the template Dockerfile.
// Production refuses to spawn a model process without a reserved attempt
// uid, and only the supervisor holds the slot pool -- so `routines run`
// inside the container takes this fixed identity instead, serialized by a
// kernel lock so two manual runs cannot share it.
func reserveManualIdentity(dir string) (uint32, func(), error) {
	const uid = uint32(sandbox.AttemptUIDBase + config.MaxConcurrency)
	// Group membership is how the staged trees are shared with the identity.
	// The container's init may not have delivered the image's membership to
	// this process, so join the pool here; failure names the contract
	// violation instead of surfacing as a bare chgrp error mid-staging.
	if err := sandbox.EnsureAttemptGroups(config.MaxConcurrency + 1); err != nil {
		return 0, nil, fmt.Errorf("%w: %w", ErrFatal, err)
	}
	f, err := openLock(dir, "manual-attempt.lock")
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
	// Prove the identity is empty before handing it out, not only after: a
	// previous manual run that died (the kernel dropped its lock) can leave
	// an escaped descendant that would share -- and be able to inspect --
	// this run's identity. The release-side reap is best effort because the
	// lock dies with the process anyway; this acquire-side proof is what the
	// next run can rely on.
	if err := sandbox.ReapIdentity(uid); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		return 0, nil, fmt.Errorf("manual attempt identity is not clean -- refusing to reuse it: %w", err)
	}
	return uid, func() {
		if err := sandbox.ReapIdentity(uid); err != nil {
			slog.Warn("manual attempt identity not proven empty at release", "uid", uid, "error", err)
		}
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
