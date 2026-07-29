package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/schedule"
)

// ScheduleFileName is the read-only forward schedule injected into every
// run workspace: what fires when, computed by the same parser that
// schedules runs, so the model never derives fire times from cron by hand.
const ScheduleFileName = "schedule.md"

const (
	// scheduleHorizonDays bounds the forward scan; a weekly cron always
	// lands inside it, and anything rarer honestly reports no fire.
	scheduleHorizonDays = 14
	// scheduleFireCount is how many upcoming fires each routine shows --
	// two, so a same-day retry slot never hides the next fresh fire.
	scheduleFireCount = 2
)

const timeLayout = "Mon 2006-01-02 15:04"

// scheduleRow is one routine's computed slice of the forward schedule.
type scheduleRow struct {
	name     string
	spec     string
	fires    []time.Time
	eventful bool // records events: work the schedule forecasts
}

// prepareSchedule renders the agent's forward schedule into the workspace.
// Unparseable routines are skipped, not fatal -- `check` and the supervisor
// already surface them.
func prepareSchedule(dir, workspace string, r *routine.Routine, tz string, now time.Time) error {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	all, _ := routine.LoadAgent(dir)
	rendered := renderSchedule(all, r, now, loc)
	return os.WriteFile(filepath.Join(workspace, ScheduleFileName), []byte(rendered), 0o644)
}

// renderSchedule formats the forward schedule a run receives: every active
// routine's next fires, split eventless (reporting -- fire times are facts,
// not forecast work) from eventful, and -- when the running routine is
// scheduled -- the window through its next fire-day with the eventful
// routines partitioned in-window/out. The running routine's window computes
// even when it is inactive: a manually-run routine still has a schedule to
// stand in. Everything is computed and displayed in loc, the agent's
// timezone, by the same bound parser that dispatches runs -- so the forecast
// a routine reads and the times it actually fires at cannot disagree.
func renderSchedule(all []*routine.Routine, self *routine.Routine, now time.Time, loc *time.Location) string {
	now = now.In(loc)
	until := now.AddDate(0, 0, scheduleHorizonDays)

	var windowEnd time.Time
	if self != nil && self.FM.Schedule != "" {
		if spec, err := schedule.Parse(self.FM.Schedule, loc); err == nil {
			windowEnd = schedule.WindowEnd(spec, now, until)
		}
	}

	var rows []scheduleRow
	for _, r := range all {
		if r.FM.Schedule == "" || !r.FM.IsActive() {
			continue
		}
		spec, err := schedule.Parse(r.FM.Schedule, loc)
		if err != nil {
			continue
		}
		rows = append(rows, scheduleRow{
			name:     r.Name,
			spec:     r.FM.Schedule,
			fires:    schedule.NextFires(spec, now, until, scheduleFireCount),
			eventful: r.FM.RecordsEvents(),
		})
	}
	sort.SliceStable(rows, func(i, j int) bool { return firstFire(rows[i]).Before(firstFire(rows[j])) })

	var b strings.Builder
	fmt.Fprintf(&b, "now: %s (%s)\n", now.Format(timeLayout), now.Location())
	if !windowEnd.IsZero() {
		fmt.Fprintf(&b, "window: now → %s (%s's next fire on its next fire-day)\n",
			windowEnd.Format(timeLayout), self.Name)
	}
	var eventful []scheduleRow
	for _, row := range rows {
		if row.eventful {
			eventful = append(eventful, row)
			continue
		}
		fmt.Fprintf(&b, "eventless: %s next %s\n", row.name, formatFires(row.fires))
	}
	b.WriteString("\n")

	if windowEnd.IsZero() {
		writeTable(&b, "routine", eventful)
		return b.String()
	}
	var in, out []scheduleRow
	for _, row := range eventful {
		if len(row.fires) > 0 && row.fires[0].Before(windowEnd) {
			in = append(in, row)
		} else {
			out = append(out, row)
		}
	}
	if len(in) == 0 {
		fmt.Fprintf(&b, "in-window: none before %s\n", windowEnd.Format(timeLayout))
	} else {
		writeTable(&b, "in-window", in)
	}
	if len(out) > 0 {
		b.WriteString("\n")
		writeTable(&b, "out (after window)", out)
	}
	return b.String()
}

// firstFire orders rows soonest-first; a routine with no fire in the
// horizon sorts last.
func firstFire(row scheduleRow) time.Time {
	if len(row.fires) == 0 {
		return time.Unix(1<<62, 0)
	}
	return row.fires[0]
}

func formatFires(fires []time.Time) string {
	if len(fires) == 0 {
		return fmt.Sprintf("none in the next %d days", scheduleHorizonDays)
	}
	parts := make([]string, len(fires))
	for i, t := range fires {
		parts[i] = t.Format(timeLayout)
	}
	return strings.Join(parts, ", ")
}

func writeTable(b *strings.Builder, title string, rows []scheduleRow) {
	if len(rows) == 0 {
		return
	}
	nameW, specW := len(title), len("schedule")
	for _, row := range rows {
		nameW = max(nameW, len(row.name))
		specW = max(specW, len(row.spec))
	}
	fmt.Fprintf(b, "%-*s  %-*s  %s\n", nameW, title, specW, "schedule", "next fires")
	for _, row := range rows {
		fmt.Fprintf(b, "%-*s  %-*s  %s\n", nameW, row.name, specW, row.spec, formatFires(row.fires))
	}
}
