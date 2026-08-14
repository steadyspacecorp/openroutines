package repository

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func leaseRepositories(t *testing.T) (a, b string) {
	t.Helper()
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	gitT(t, base, "init", "--quiet", "--bare", origin)
	a = filepath.Join(base, "a")
	b = filepath.Join(base, "b")
	gitT(t, base, "clone", "--quiet", origin, a)
	gitT(t, base, "clone", "--quiet", origin, b)
	if err := os.WriteFile(filepath.Join(a, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, a, "add", "README.md")
	gitT(t, a, "commit", "--quiet", "-m", "fixture")
	gitT(t, a, "push", "--quiet", "origin", "HEAD:main")
	return a, b
}

func TestLeaseCompareAndSwapPreventsRaces(t *testing.T) {
	a, b := leaseRepositories(t)
	repoA, repoB := Open(a), Open(b)
	shaA, err := repoA.WriteLease("instance-a", time.Now(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repoB.WriteLease("instance-b", time.Now(), ""); err == nil {
		t.Fatal("CAS should reject a write against a stale expectation")
	}
	lease, err := repoB.ReadLease()
	if err != nil || lease == nil || lease.Holder != "instance-a" || lease.SHA != shaA {
		t.Fatalf("unexpected lease read: %+v err=%v", lease, err)
	}
	if _, err := repoB.WriteLease("instance-b", time.Now(), lease.SHA); err != nil {
		t.Fatalf("CAS with correct token should succeed: %v", err)
	}
	if lease, _ = repoA.ReadLease(); lease == nil || lease.Holder != "instance-b" {
		t.Fatalf("takeover not visible: %+v", lease)
	}
	repoA.ReleaseLease(shaA)
	current, _ := repoA.ReadLease()
	if current == nil || current.Holder != "instance-b" {
		t.Fatalf("stale release deleted the live lease: %+v", current)
	}
	repoB.ReleaseLease(current.SHA)
	if lease, _ = repoB.ReadLease(); lease != nil {
		t.Fatalf("owned release should remove the lease: %+v", lease)
	}
}

func TestLeasePreservesSubsecondHeartbeats(t *testing.T) {
	a, b := leaseRepositories(t)
	repoA, repoB := Open(a), Open(b)
	now := time.Now().Truncate(time.Second).Add(375 * time.Millisecond)
	if _, err := repoA.WriteLease("instance-a", now, ""); err != nil {
		t.Fatal(err)
	}
	lease, err := repoB.ReadLease()
	if err != nil {
		t.Fatal(err)
	}
	if !lease.At.Equal(now) {
		t.Fatalf("heartbeat read as %s, want %s", lease.At, now)
	}
	legacy, err := parseLeaseTime("1723165200")
	if err != nil || !legacy.Equal(time.Unix(1723165200, 0)) {
		t.Fatalf("legacy heartbeat parsed as %s: %v", legacy, err)
	}
}
