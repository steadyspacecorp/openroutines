package runner

import (
	"strings"
	"testing"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/routine"
)

func boolPtr(v bool) *bool { return &v }

func scheduleFixture() []*routine.Routine {
	return []*routine.Routine{
		{Name: "steady-check-in", Frontmatter: routine.Frontmatter{Schedule: "0 7,8 * * 1-5", Reports: true}},
		{Name: "steady-inbox", Frontmatter: routine.Frontmatter{Schedule: "45 8-17/3 * * 1-5", Teamwork: routine.TeamworkOff}},
		{Name: "announcements", Frontmatter: routine.Frontmatter{Schedule: "30 8 * * 2", Teamwork: routine.TeamworkEvents}},
		{Name: "doc-drift", Frontmatter: routine.Frontmatter{Schedule: "0 9 * * 1-5"}},
		{Name: "roadmap-groomer", Frontmatter: routine.Frontmatter{Schedule: "0 17 * * 2"}},
		{Name: "a11y-sweep", Frontmatter: routine.Frontmatter{Schedule: "30 9 * * 3"}},
		{Name: "release-notes", Frontmatter: routine.Frontmatter{Schedule: "0 21 * * 1"}},
		{Name: "retired", Frontmatter: routine.Frontmatter{Schedule: "0 9 * * *", Active: boolPtr(false)}},
	}
}

func TestRenderScheduleWindowSplit(t *testing.T) {
	all := scheduleFixture()
	now := time.Date(2026, 7, 28, 7, 0, 0, 0, time.UTC)
	got := renderSchedule(all, all[0], now, time.UTC)

	for _, want := range []string{
		"now: Tue 2026-07-28 07:00 (UTC)",
		"window: now → Wed 2026-07-29 07:00 (steady-check-in's next fire on its next fire-day)",
		"fact: announcements next Tue 2026-07-28 08:30",
		"fact: steady-inbox next Tue 2026-07-28 08:45",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if !all[2].Frontmatter.RecordsEvents() {
		t.Fatal("teamwork: events must not disable event recording")
	}

	in := section(got, "in-window", "out (after window)")
	out := section(got, "out (after window)", "")
	if strings.Contains(in, "announcements") || strings.Contains(out, "announcements") {
		t.Fatalf("teamwork: events routine must stay out of the work tables:\n%s", got)
	}
	for name, wantIn := range map[string]bool{
		"doc-drift":       true,
		"roadmap-groomer": true,
		"a11y-sweep":      false,
		"release-notes":   false,
	} {
		if strings.Contains(in, name) != wantIn {
			t.Fatalf("%s in-window=%v, want %v:\n%s", name, !wantIn, wantIn, got)
		}
		if strings.Contains(out, name) == wantIn {
			t.Fatalf("%s misplaced in out table:\n%s", name, got)
		}
	}
	if strings.Contains(got, "retired") {
		t.Fatalf("inactive routine must not render:\n%s", got)
	}
}

func TestRenderScheduleDegradesWithoutSelfSchedule(t *testing.T) {
	all := scheduleFixture()
	unscheduled := &routine.Routine{Name: "on-demand", Frontmatter: routine.Frontmatter{}}
	now := time.Date(2026, 7, 28, 7, 0, 0, 0, time.UTC)
	got := renderSchedule(all, unscheduled, now, time.UTC)
	if strings.Contains(got, "window:") || strings.Contains(got, "in-window") {
		t.Fatalf("no window without a self schedule:\n%s", got)
	}
	if !strings.Contains(got, "routine") || !strings.Contains(got, "doc-drift") {
		t.Fatalf("facts table must still render:\n%s", got)
	}
}

func TestRenderScheduleInactiveSelfKeepsWindow(t *testing.T) {
	probe := &routine.Routine{Name: "check-in-probe", Frontmatter: routine.Frontmatter{
		Schedule: "0 7 * * 1-5", Active: boolPtr(false), Teamwork: routine.TeamworkOff,
	}}
	all := append(scheduleFixture(), probe)
	now := time.Date(2026, 7, 28, 7, 0, 0, 0, time.UTC)
	got := renderSchedule(all, probe, now, time.UTC)
	if !strings.Contains(got, "window: now → Wed 2026-07-29 07:00") {
		t.Fatalf("inactive self must still compute its window:\n%s", got)
	}
}

func TestRenderScheduleUsesAgentTimezoneNotArgumentZone(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database")
	}
	all := scheduleFixture()
	now := time.Date(2026, 7, 28, 11, 0, 0, 0, time.UTC)
	got := renderSchedule(all, all[0], now, ny)

	for _, want := range []string{
		"now: Tue 2026-07-28 07:00 (America/New_York)",
		"window: now → Wed 2026-07-29 07:00 (steady-check-in's next fire on its next fire-day)",
		"fact: announcements next Tue 2026-07-28 08:30",
		"fact: steady-inbox next Tue 2026-07-28 08:45",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

func section(got, from, to string) string {
	start := strings.Index(got, from)
	if start < 0 {
		return ""
	}
	rest := got[start:]
	if to == "" {
		return rest
	}
	if end := strings.Index(rest, to); end >= 0 {
		return rest[:end]
	}
	return rest
}
