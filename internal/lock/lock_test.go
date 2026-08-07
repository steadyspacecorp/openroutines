package lock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Locks are held per open file description, so a second Take in the same
// process contends exactly like one from another process.
func TestTakeExcludesOtherHolders(t *testing.T) {
	dir := t.TempDir()
	release, err := Take(dir, "daily")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Take(dir, "daily"); !errors.Is(err, ErrLocked) {
		t.Fatalf("second acquisition should report the held lock, got %v", err)
	}
	// A different name is a different lock.
	other, err := Take(dir, "weekly")
	if err != nil {
		t.Fatalf("independent lock should acquire: %v", err)
	}
	other()
	release()
	// Released: the work it guarded can happen again.
	again, err := Take(dir, "daily")
	if err != nil {
		t.Fatalf("released lock should re-acquire: %v", err)
	}
	again()
}

// A lock file left behind by a differently privileged invocation -- a manual
// run someone made as root inside the container -- must still lock. Nothing
// ever reads or writes these files, so wanting write access to one is asking
// for a permission that would wedge a routine forever when it is missing: the
// supervisor failed every dispatch of one routine, once a minute, until the
// container was replaced. Ownership cannot be faked without privilege, but
// the mode is the half that bites, so this stands in for it.
func TestTakesALockItCannotWrite(t *testing.T) {
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
	release, err := Take(dir, "daily")
	if err != nil {
		t.Fatalf("a read-only lock file should still lock: %v", err)
	}
	release()
}

// A Locker waits for the holder instead of reporting contention, across
// processes (two of them) as much as within one.
func TestLockerWaitsForTheHolder(t *testing.T) {
	dir := t.TempDir()
	first, err := Locker(dir, "knowledge")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Locker(dir, "knowledge")
	if err != nil {
		t.Fatal(err)
	}
	first.Lock()
	acquired := make(chan struct{})
	go func() {
		second.Lock()
		close(acquired)
	}()
	select {
	case <-acquired:
		t.Fatal("second locker acquired the lock while the first held it")
	case <-time.After(50 * time.Millisecond):
	}
	first.Unlock()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("lock was not handed over after release")
	}
	second.Unlock()
}
