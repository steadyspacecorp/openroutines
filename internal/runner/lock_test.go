package runner

import (
	"errors"
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

func TestSingleIdentityLockSerializesAllAttempts(t *testing.T) {
	dir := t.TempDir()
	release, err := LockSingleIdentityAttempt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockSingleIdentityAttempt(dir); !errors.Is(err, ErrSingleIdentityBusy) {
		t.Fatalf("second single-identity lock = %v, want ErrSingleIdentityBusy", err)
	}
	release()
	releaseAgain, err := LockSingleIdentityAttempt(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	releaseAgain()
}

// The memory lock's cross-process exclusion rides the same per-description
// flock semantics: two separate opens of the lock -- a supervisor and a
// manual run -- contend through the kernel, not through the in-process mutex.
func TestMemoryLockExcludesAcrossOpens(t *testing.T) {
	dir := t.TempDir()
	supervisor, err := OpenMemoryLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	manual, err := OpenMemoryLock(dir)
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
		t.Fatal("second open acquired the memory lock while the first held it")
	case <-time.After(50 * time.Millisecond):
	}
	supervisor.Unlock()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("memory lock was not handed over after release")
	}
	manual.Unlock()
}
