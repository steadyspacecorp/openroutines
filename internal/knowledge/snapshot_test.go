package knowledge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFetchOriginSnapshotDoesNotAdoptLocalKnowledge(t *testing.T) {
	a, b := twoClones(t)
	localBefore := gitT(t, filepath.Join(b, "knowledge"), "rev-parse", "HEAD")
	writeKnowledge(t, a, "events.md", "remote fact\n")
	writeKnowledge(t, a, "large.md", strings.Repeat("x", 128))
	if _, err := At(a).Commit("remote fact"); err != nil {
		t.Fatal(err)
	}
	if err := At(a).Push(); err != nil {
		t.Fatal(err)
	}

	snap, err := At(b).FetchOriginSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	raw, err := snap.ReadFile("events.md")
	if err != nil || string(raw) != "remote fact\n" {
		t.Fatalf("snapshot events = %q, %v", raw, err)
	}
	if localAfter := gitT(t, filepath.Join(b, "knowledge"), "rev-parse", "HEAD"); localAfter != localBefore {
		t.Fatalf("snapshot adopted local knowledge: %s -> %s", localBefore, localAfter)
	}
	relation := snap.Relation(At(b))
	if relation.Behind != 1 || relation.Ahead != 0 || relation.Diverged {
		t.Fatalf("relation = %+v, want one commit behind", relation)
	}
	stats, err := snap.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files == 0 || stats.SizeBytes < 128 || stats.LargestPath == "" {
		t.Fatalf("stats = %+v", stats)
	}
	changes, err := snap.ChangesSince(time.Now().Add(-time.Hour))
	if err != nil || !strings.Contains(changes, "remote fact") {
		t.Fatalf("recent changes = %q, %v", changes, err)
	}
}

func TestSnapshotRejectsPathsOutsideTree(t *testing.T) {
	dir := t.TempDir()
	snap := &OriginSnapshot{Dir: dir}
	if _, err := snap.ReadFile("../secret"); err == nil {
		t.Fatal("expected traversal to fail")
	}
	if err := os.WriteFile(filepath.Join(dir, "plain.md"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if raw, err := snap.ReadFile("plain.md"); err != nil || string(raw) != "ok" {
		t.Fatalf("ordinary read = %q, %v", raw, err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("no"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err == nil {
		if _, err := snap.ReadFile(filepath.Join("link", "secret")); err == nil {
			t.Fatal("expected an intermediate symlink to fail")
		}
	}
}
