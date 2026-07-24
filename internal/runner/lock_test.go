package runner

import (
	"errors"
	"testing"
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
