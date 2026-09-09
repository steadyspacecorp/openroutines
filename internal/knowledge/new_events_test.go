package knowledge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTakeNewEventsParsesEntriesAndRemovesTheFile(t *testing.T) {
	staging := t.TempDir()
	if err := WriteNewEventsStub(staging); err != nil {
		t.Fatal(err)
	}
	stub, _ := os.ReadFile(filepath.Join(staging, NewEventsFile))
	page := string(stub) + "\n- opened [PR](https://example.test/1)\n  for the rename\n\n- NO-OP: nothing else changed\n"
	os.WriteFile(filepath.Join(staging, NewEventsFile), []byte(page), 0o644)

	entries, err := TakeNewEvents(staging)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"opened [PR](https://example.test/1) for the rename", "NO-OP: nothing else changed"}
	if strings.Join(entries, "|") != strings.Join(want, "|") {
		t.Fatalf("entries = %q, want %q", entries, want)
	}
	if _, err := os.Stat(filepath.Join(staging, NewEventsFile)); !os.IsNotExist(err) {
		t.Fatal("new-events.md should be removed from staging")
	}

	entries, err = TakeNewEvents(staging)
	if err != nil || entries != nil {
		t.Fatalf("absent page: entries=%v err=%v, want nil nil", entries, err)
	}
}

func TestAppendNewEventsStampsEntries(t *testing.T) {
	dir := deliveryFixture(t)
	store := NewStore(dir)
	now := time.Date(2026, 9, 9, 15, 0, 0, 0, time.UTC)
	if err := store.AppendNewEvents("doc-drift", "run_x", now, []string{"plain fact", "second fact"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendNewEvents("doc-drift", "run_y", now, nil); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(store.Worktree(), "events.md"))
	for _, want := range []string{
		"- 2026-09-09 doc-drift (run_x): plain fact\n",
		"- 2026-09-09 doc-drift (run_x): second fact\n",
		"- 2026-09-09 doc-drift (run_y): NO-OP: the run recorded no event\n",
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("events.md missing %q:\n%s", want, raw)
		}
	}
}
