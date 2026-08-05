package routine

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/logging"
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
	r, err := Parse(writeTemp(t, "---\nschedule: \"0 9 * * 1\"\nurl: https://example.com/agent\nskills: [a]\n---\nDo the thing.\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r.FM.Schedule != "0 9 * * 1" || r.Body != "Do the thing." {
		t.Fatalf("unexpected parse: %+v body=%q", r.FM, r.Body)
	}
	if !r.FM.IsActive() || !r.FM.RecordsEvents() || !r.FM.FullTeamwork() {
		t.Fatal("defaults should be active=true teamwork=full")
	}
	if got := r.FM.EffectiveURL(); got != "https://example.com/agent" {
		t.Fatalf("effective URL = %q, want declared URL", got)
	}
}

func TestFrontmatterTeamworkLadder(t *testing.T) {
	for _, tc := range []struct {
		teamwork string
		records  bool
		full     bool
	}{
		{teamwork: "", records: true, full: true},
		{teamwork: TeamworkFull, records: true, full: true},
		{teamwork: TeamworkEvents, records: true, full: false},
		{teamwork: TeamworkOff, records: false, full: false},
	} {
		fm := Frontmatter{Teamwork: tc.teamwork}
		if got := fm.RecordsEvents(); got != tc.records {
			t.Errorf("teamwork %q: RecordsEvents() = %v, want %v", tc.teamwork, got, tc.records)
		}
		if got := fm.FullTeamwork(); got != tc.full {
			t.Errorf("teamwork %q: FullTeamwork() = %v, want %v", tc.teamwork, got, tc.full)
		}
	}
}

func TestParseRejectsUnknownTeamworkValue(t *testing.T) {
	_, err := Parse(writeTemp(t, "---\nschedule: \"0 9 * * *\"\nteamwork: quiet\n---\nBody.\n"))
	if err == nil || !strings.Contains(err.Error(), "must be full, events, or off") {
		t.Fatalf("expected teamwork value error, got %v", err)
	}
}

func TestParseRejectsRetiredEventsKey(t *testing.T) {
	_, err := Parse(writeTemp(t, "---\nschedule: \"0 9 * * *\"\nevents: false\n---\nBody.\n"))
	if err == nil || !strings.Contains(err.Error(), `"teamwork: off"`) {
		t.Fatalf("expected retired-key error carrying the mapping, got %v", err)
	}
}

func TestFrontmatterURLDefaultsAndRejectsInvalidValues(t *testing.T) {
	r, err := Parse(writeTemp(t, "---\nschedule: \"0 9 * * *\"\n---\nBody.\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := r.FM.EffectiveURL(); got != DefaultURL {
		t.Fatalf("effective URL = %q, want %q", got, DefaultURL)
	}

	for _, bad := range []string{"example.com/agent", "ftp://example.com/agent", "https://user:pass@example.com/agent"} {
		_, err := Parse(writeTemp(t, "---\nschedule: \"0 9 * * *\"\nurl: "+bad+"\n---\nBody.\n"))
		if err == nil || !strings.Contains(err.Error(), "absolute http(s) URL") {
			t.Errorf("url %q: expected validation error, got %v", bad, err)
		}
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

// Load errors name the routine they are about, so one broken file can be
// worked around by everyone else -- and so the broken routine's own lookup
// reports the reason instead of "no routine".
func TestLoadErrorsAreAttributedToTheirRoutine(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "routines"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "routines", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("healthy.md", "---\nschedule: \"0 9 * * *\"\n---\nwork\n")
	write("typo.md", "---\nschedule: \"0 9 * * *\"\nactve: false\n---\nbroken\n")

	_, errs := LoadAgent(root)
	if len(errs) != 1 {
		t.Fatalf("expected one error, got %v", errs)
	}
	if Concerns(errs[0], "healthy") {
		t.Errorf("a typo in typo.md is not healthy's problem: %v", errs[0])
	}
	if !Concerns(errs[0], "typo") {
		t.Errorf("the error is about typo: %v", errs[0])
	}

	if _, err := Find(root, "healthy"); err != nil {
		t.Errorf("healthy is findable alongside a broken sibling: %v", err)
	}
	_, err := Find(root, "typo")
	if err == nil || !strings.Contains(err.Error(), "actve") {
		t.Errorf("want the parse error for the broken routine, got %v", err)
	}
	if !strings.Contains(err.Error(), filepath.Join("routines", "typo.md")) {
		t.Errorf("the error should name the file it is about, got %v", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("a routine that does not load is not a routine that is absent: %v", err)
	}
	if _, err := Find(root, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a name nothing claims is ErrNotFound, got %v", err)
	}
}

// A file that does not parse still claims its name, so a name two files claim
// is a collision even when one of them is the broken file -- and both are
// dropped. Otherwise the tick would schedule the healthy one, mint and push
// its run, and only then have the runner refuse to assemble the workspace.
func TestBrokenFileClaimsItsNameAgainstAHealthyOne(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{filepath.Join(root, "routines"), filepath.Join(root, "plugins", "demo", "routines")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "routines", "daily.md"),
		[]byte("---\nschedule: \"0 9 * * *\"\n---\nwork\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plugins", "demo", "routines", "daily.md"),
		[]byte("---\nschedule: \"0 9 * * *\"\nactve: false\n---\nbroken\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	routines, errs := LoadAgent(root)
	if len(routines) != 0 {
		t.Errorf("daily is ambiguous while one of its two files does not load: %v", routines)
	}
	if len(errs) != 1 || !Concerns(errs[0], "daily") {
		t.Fatalf("want one error about daily, got %v", errs)
	}
	if !strings.Contains(errs[0].Error(), filepath.Join("plugins", "demo", "routines", "daily.md")) {
		t.Errorf("the error must name the broken file, not just the routine: %v", errs[0])
	}
}

// Log binds the routine's identity to the process logger: concurrent runs
// share one stdout, so every line carries routine= (and whatever the caller
// adds, like run_id) without the call site repeating it.
func TestLogBindsRoutineIdentity(t *testing.T) {
	var buf bytes.Buffer
	logging.Setup(&buf, slog.LevelInfo, time.UTC)
	(&Routine{Name: "check-in"}).Log().With("run_id", "run_abc").Error("attempt failed -- will retry", "detail", "exit status 1")

	got := strings.TrimSpace(buf.String())
	for _, want := range []string{`level=ERROR`, `msg="attempt failed -- will retry"`, "routine=check-in", "run_id=run_abc", `detail="exit status 1"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}
