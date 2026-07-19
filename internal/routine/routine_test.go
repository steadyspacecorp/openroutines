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
	r, err := Parse(writeTemp(t, "---\nid: r_abc12345\nschedule: \"0 9 * * 1\"\nskills: [a]\n---\nDo the thing.\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r.FM.ID != "r_abc12345" || r.FM.Schedule != "0 9 * * 1" || r.Body != "Do the thing." {
		t.Fatalf("unexpected parse: %+v body=%q", r.FM, r.Body)
	}
	if !r.FM.IsActive() || !r.FM.LogsWork() {
		t.Fatal("defaults should be active=true worklog=true")
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

func TestNewIDMatchesPattern(t *testing.T) {
	for range 50 {
		if id := NewID(); !IDPattern.MatchString(id) {
			t.Fatalf("generated id %q does not match pattern", id)
		}
	}
}

func TestSetActiveTogglesInPlace(t *testing.T) {
	p := writeTemp(t, "---\nid: r_abc12345\nschedule: \"0 9 * * 1\"\n# keep me\n---\nBody stays.\n")
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
