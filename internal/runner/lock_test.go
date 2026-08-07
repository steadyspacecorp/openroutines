package runner

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// flock semantics are per open-file-description, so a second acquisition in
// the same process contends exactly like one from another process.
func TestLockRoutineExcludesConcurrentAttempts(t *testing.T) {
	dir := t.TempDir()
	release, err := LockRoutine(dir, "daily")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockRoutine(dir, "daily"); !errors.Is(err, ErrRoutineLocked) {
		t.Fatalf("second acquisition should report the held lock, got %v", err)
	}
	// A different routine is a different lock.
	release2, err := LockRoutine(dir, "weekly")
	if err != nil {
		t.Fatalf("independent routine lock should acquire: %v", err)
	}
	release2()
	release()
	// Released: the routine can run again.
	release3, err := LockRoutine(dir, "daily")
	if err != nil {
		t.Fatalf("released lock should re-acquire: %v", err)
	}
	release3()
}

// A lock file left behind by a differently privileged invocation -- a manual
// run someone made as root inside the container -- must still lock. Nothing
// ever reads or writes these files, so wanting write access to one is asking
// for a permission that would wedge the routine forever when it is missing:
// the supervisor failed every dispatch of one routine, once a minute, until
// the container was replaced. Ownership cannot be faked without privilege,
// but the mode is the half that bites, so this stands in for it.
func TestLockRoutineTakesALockFileItCannotWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the mode bits this asserts on")
	}
	dir := t.TempDir()
	locks := filepath.Join(dir, ".openroutines-tmp", "locks")
	if err := os.MkdirAll(locks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locks, "daily.lock"), nil, 0o444); err != nil {
		t.Fatal(err)
	}
	release, err := LockRoutine(dir, "daily")
	if err != nil {
		t.Fatalf("a read-only lock file should still lock: %v", err)
	}
	release()
}

// The knowledge lock's cross-process exclusion rides the same per-description
// flock semantics: two separate opens of the lock -- a supervisor and a
// manual run -- contend through the kernel, not through the in-process mutex.
func TestKnowledgeLockExcludesAcrossOpens(t *testing.T) {
	dir := t.TempDir()
	supervisor, err := OpenKnowledgeLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	manual, err := OpenKnowledgeLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	supervisor.Lock()
	acquired := make(chan struct{})
	go func() {
		manual.Lock()
		close(acquired)
	}()
	select {
	case <-acquired:
		t.Fatal("second open acquired the knowledge lock while the first held it")
	case <-time.After(50 * time.Millisecond):
	}
	supervisor.Unlock()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("knowledge lock was not handed over after release")
	}
	manual.Unlock()
}
