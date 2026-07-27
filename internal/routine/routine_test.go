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

func TestParseTriggerFrontmatter(t *testing.T) {
	r, err := Parse(writeTemp(t, "---\ntrigger:\n  poll: https://example.com/cursor\n  credential: steady_token\n  select: /cursor\n  interval: 2m\ncredentials: [steady_token]\n---\nCheck the inbox.\n"))
	if err != nil {
		t.Fatal(err)
	}
	tr := r.FM.Trigger
	if tr == nil || tr.Poll != "https://example.com/cursor" || tr.Credential != "steady_token" ||
		tr.Select != "/cursor" || tr.Interval != "2m" {
		t.Fatalf("unexpected trigger parse: %+v", tr)
	}
	if r.FM.Schedule != "" {
		t.Fatalf("schedule should be empty for a trigger-only routine: %q", r.FM.Schedule)
	}

	// No trigger declared: the field stays nil.
	r, err = Parse(writeTemp(t, "---\nschedule: \"* * * * *\"\n---\nBody.\n"))
	if err != nil || r.FM.Trigger != nil {
		t.Fatalf("trigger should be nil when undeclared: %+v err=%v", r.FM.Trigger, err)
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

func TestLoadAgentIncludesPluginsAndDropsDuplicateIdentities(t *testing.T) {
	root := t.TempDir()
	write := func(rel string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("---\nschedule: \"0 9 * * *\"\n---\nwork\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("routines/owned.md")
	write("plugins/demo/routines/plugin-owned.md")
	routines, errs := LoadAgent(root)
	if len(errs) != 0 || len(routines) != 2 {
		t.Fatalf("grouped discovery: routines=%v errs=%v", routines, errs)
	}
	write("plugins/demo/routines/owned.md")
	routines, errs = LoadAgent(root)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "duplicate routine") {
		t.Fatalf("duplicate should be reported: routines=%v errs=%v", routines, errs)
	}
	for _, r := range routines {
		if r.Name == "owned" {
			t.Fatal("ambiguous routine must fail closed, not be returned for execution")
		}
	}
}
