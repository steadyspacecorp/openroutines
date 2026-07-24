package routine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sample.md")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParse(t *testing.T) {
	r, err := Parse(writeTemp(t, "---\nschedule: \"0 9 * * 1\"\nskills: [a]\n---\nDo the thing.\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r.FM.Schedule != "0 9 * * 1" || r.Body != "Do the thing." {
		t.Fatalf("unexpected parse: %+v body=%q", r.FM, r.Body)
	}
	if !r.FM.IsActive() || !r.FM.RecordsEvents() {
		t.Fatal("defaults should be active=true events=true")
	}
}

func TestParseRejectsMissingFrontmatter(t *testing.T) {
	if _, err := Parse(writeTemp(t, "just a body\n")); err == nil {
		t.Fatal("expected error for missing frontmatter")
	}
	if _, err := Parse(writeTemp(t, "---\nid: x\nno closing fence")); err == nil {
		t.Fatal("expected error for unterminated frontmatter")
	}
}

func TestSetActiveTogglesInPlace(t *testing.T) {
	p := writeTemp(t, "---\nschedule: \"0 9 * * 1\"\n# keep me\n---\nBody stays.\n")
	if err := SetActive(p, false); err != nil {
		t.Fatal(err)
	}
	r, err := Parse(p)
	if err != nil {
		t.Fatal(err)
	}
	if r.FM.IsActive() {
		t.Fatal("expected inactive")
	}
	raw, _ := os.ReadFile(p)
	if !strings.Contains(string(raw), "# keep me") || !strings.Contains(string(raw), "Body stays.") {
		t.Fatalf("file surgery damaged content: %q", raw)
	}
	if err := SetActive(p, true); err != nil {
		t.Fatal(err)
	}
	if r, _ = Parse(p); !r.FM.IsActive() {
		t.Fatal("expected active again")
	}
}

// Names become filesystem paths; the grammar must be closed under path
// construction.
func TestNamePattern(t *testing.T) {
	for _, ok := range []string{"daily", "steady-check-in", "a11y_sweep", "r2"} {
		if !NamePattern.MatchString(ok) {
			t.Errorf("%q should be a valid routine name", ok)
		}
	}
	for _, bad := range []string{"..", ".", "../x", "a/b", "a\\b", "/abs", "Daily", "a b", "-lead", "trail-", "a..b", ""} {
		if NamePattern.MatchString(bad) {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

// A typo'd frontmatter key must be an error, not a silent fall-through to
// the field's default (actve: false -> active-by-default was the audit's
// example).
func TestParseRejectsUnknownFrontmatterKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.md")
	os.WriteFile(path, []byte("---\nschedule: \"* * * * *\"\nactve: false\n---\nbody\n"), 0o644)
	if _, err := Parse(path); err == nil || !strings.Contains(err.Error(), "actve") {
		t.Fatalf("expected unknown-field error naming the typo, got %v", err)
	}
}
