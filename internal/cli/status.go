package cli

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/knowledge"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/run"
	"github.com/steadyspacecorp/openroutines/internal/schedule"
	"github.com/steadyspacecorp/openroutines/internal/skill"
	"github.com/steadyspacecorp/openroutines/internal/supervisor"
	"github.com/steadyspacecorp/openroutines/internal/version"
)

const statusUsage = "usage: openroutines status"

func cmdStatus(args []string) int {
	positional, _, help, err := parseFlags(args, nil)
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println(statusUsage)
		return 0
	}
	if len(positional) != 0 {
		return fail(fmt.Errorf("%s", statusUsage))
	}

	dir := "."
	agent, err := config.Load(dir)
	if err != nil {
		return fail(err)
	}
	location := printAgentStatus(dir, agent)
	printCredentialStatus(dir)
	printRoutineStatus(dir, location)
	printSkillStatus(dir)
	printKnowledgeStatus(dir)
	printTokenUsage(dir)
	printConfigurationNeeds(agent)
	return 0
}

func printAgentStatus(dir string, agent *config.Agent) *time.Location {
	fmt.Printf("agent      %s\n", orUnset(agent.Name))
	fmt.Printf("job        %s\n", orUnset(firstLine(agent.Description)))
	fmt.Printf("owner      %s <%s>\n", orUnset(agent.Owner.Name), orUnset(agent.Owner.Email))

	// The supervisor refuses to start on an unloadable timezone; status carries
	// on in UTC but labels the fallback rather than printing impossible fire times.
	location, err := time.LoadLocation(agent.Timezone)
	timezoneNote := ""
	if err != nil {
		location, timezoneNote = time.UTC, " -- INVALID, times below shown in UTC"
	}
	fmt.Printf("timezone   %s%s\n", orUnset(agent.Timezone), timezoneNote)
	fmt.Printf("model      %s (default)\n", orUnset(agent.Defaults.Model))
	if len(agent.Variables) > 0 {
		names := slices.Sorted(maps.Keys(agent.Variables))
		fmt.Printf("variables  %s\n", strings.Join(names, ", "))
	}
	if pin, err := readVersionPin(dir); err == nil {
		fmt.Printf("framework  %s (pinned; this binary is %s)\n", pin, version.Version)
	}
	return location
}

func printCredentialStatus(dir string) {
	key, err := creds.LoadKey(dir)
	if err != nil {
		fmt.Printf("master key MISSING -- run openroutines configure\n")
		return
	}
	store, err := creds.Read(dir, key)
	if err != nil {
		fmt.Printf("master key present, but credentials do not decrypt: %v\n", err)
		return
	}
	names := slices.Sorted(maps.Keys(store))
	if len(names) == 0 {
		fmt.Printf("master key present -- no credentials stored yet\n")
	} else {
		fmt.Printf("master key present -- %d credential(s): %s\n", len(store), strings.Join(names, ", "))
	}
}

func printRoutineStatus(dir string, location *time.Location) {
	now := time.Now().In(location)
	store := knowledge.NewStore(dir)
	settled := settledAttempts(dir)
	routines, parseErrs := routine.LoadAgent(dir)
	fmt.Printf("\n%s\n", bold(fmt.Sprintf("routines (%d):", len(routines))))

	reportedState := false
	for _, routine := range routines {
		state, stateErr := schedule.Load(store.StateDir(), routine.Name)
		activity := "inactive"
		next := ""
		if routine.Frontmatter.IsActive() {
			activity = "active"
			// A routine in cool-down doesn't fire at its next occurrence, so
			// print the cool-down instead of a time that will not happen.
			if state != nil && state.CoolingDown(now) {
				next = " -- cooling down"
			} else if spec, err := schedule.Parse(routine.Frontmatter.Schedule, location); err == nil {
				next = " -- next " + stamp(spec.Next(now), now, location)
			}
		}
		grants := ""
		if summary := grantSummary(routine); len(summary) > 0 {
			grants = " (" + strings.Join(summary, " ") + ")"
		}
		fmt.Printf("  %-20s %-14s %s%s%s\n", routine.Name, scheduleSummary(routine), activity, next, grants)
		if stateErr != nil {
			fmt.Printf("      %s %v\n", warnMark, stateErr)
		}
		for _, line := range scheduleStateLines(state, routine, now, location, settled) {
			reportedState = true
			fmt.Printf("      %s\n", line)
		}
	}
	if reportedState {
		if status := store.Status(); status.Behind > 0 {
			fmt.Printf("  %s scheduling state above is from knowledge %d commit(s) behind origin/%s -- run openroutines sync\n", warnMark, status.Behind, knowledge.Branch)
		}
	}
	for _, err := range parseErrs {
		fmt.Printf("  %s %v\n", warnMark, err)
	}
}

func printSkillStatus(dir string) {
	skills, errs := skill.ListAgent(dir)
	fmt.Printf("\n%s\n", bold(fmt.Sprintf("skills (%d):", len(skills))))
	for _, skill := range skills {
		fmt.Printf("  %-20s %s\n", skill.Name, firstLine(skill.Description))
	}
	for _, err := range errs {
		fmt.Printf("  %s %v\n", warnMark, err)
	}
}

func printKnowledgeStatus(dir string) {
	fmt.Printf("\n%s\n", bold("knowledge:"))
	store := knowledge.NewStore(dir)
	status := store.Status()
	if !status.Materialized {
		if status.RemoteKnowledge {
			fmt.Printf("  %s not materialized in this checkout -- origin has the agent's knowledge; run openroutines sync to adopt it\n", warnMark)
		} else {
			fmt.Printf("  not materialized yet -- appears on first run\n")
		}
	} else {
		fmt.Printf("  last commit: %s\n", status.LastCommit)
		if status.Uncommitted > 0 {
			fmt.Printf("  %s %d file(s) with uncommitted changes -- commit inside knowledge/ when done curating\n", warnMark, status.Uncommitted)
		}
		if status.Unpushed > 0 {
			fmt.Printf("  %d commit(s) not yet pushed to origin\n", status.Unpushed)
		}
		if status.Behind > 0 {
			fmt.Printf("  %s %d commit(s) behind origin/%s -- this checkout is reading old knowledge; run openroutines sync to get the latest from origin\n", warnMark, status.Behind, knowledge.Branch)
		}
		printConsumerStatus(store)
	}
	if !store.HasOrigin() {
		fmt.Printf("  %s no git origin -- knowledge is not durable until one is set\n", warnMark)
	}
}

func printConsumerStatus(store *knowledge.Store) {
	cursors, err := store.Cursors()
	if err != nil || len(cursors) == 0 {
		return
	}
	head, _ := store.Head()
	for name, cursor := range cursors {
		lag := ""
		if head != "" && !strings.HasPrefix(head, cursor.ConsumedThrough) && head != cursor.ConsumedThrough {
			changes, err := store.Changes(cursor.ConsumedThrough, head)
			switch {
			// Silence here would read as caught up; a stuck consumer's runs are
			// abandoned on sight until a person repairs the file.
			case errors.Is(err, knowledge.ErrCursorUnreachable):
				lag = fmt.Sprintf(" -- ! not on the knowledge branch, delivery is stuck: repair or delete %s", knowledge.CursorFile(name))
			case err == nil && len(changes) > 0:
				lag = fmt.Sprintf(" -- %d change(s) pending", len(changes))
			}
		}
		fmt.Printf("  consumer %s: through %.12s (run %s)%s\n", name, cursor.ConsumedThrough, cursor.ByRun, lag)
	}
}

func printConfigurationNeeds(agent *config.Agent) {
	problems := agent.Problems()
	if len(problems) == 0 {
		return
	}
	fmt.Printf("\n%s\n", bold("still needed:"))
	for _, problem := range problems {
		fmt.Printf("  - %s\n", problem)
	}
}

// Renders the supervisor's durable scheduling record for one routine: what
// it still owes, and what is holding it. Without them a routine mid-retry or
// sitting out a circuit-breaker cool-down reads exactly like a healthy one.
// Nil state means the supervisor has never seen the routine -- which every
// local checkout looks like -- so it prints nothing.
func scheduleStateLines(st *schedule.State, r *routine.Routine, now time.Time, loc *time.Location, settled map[string]bool) []string {
	if st == nil {
		return nil
	}
	var lines []string
	if st.CoolingDown(now) {
		lines = append(lines, fmt.Sprintf("! circuit breaker: %d consecutive abandonments -- no new run starts until %s; the next success resets it",
			st.ConsecutiveAbandons, stamp(st.CooldownUntil, now, loc)))
	}
	if p := st.Pending; p != nil {
		lines = append(lines, fmt.Sprintf("pending %s for %s -- %d/%d attempts, %s",
			p.RunID, stamp(p.ScheduledFor, now, loc), p.Attempts, supervisor.MaxAttempts,
			pendingDisposition(p, r, now, loc, settled)))
	}
	return append(lines, "watermark "+stamp(st.Watermark, now, loc))
}

// Says what becomes of a pending run. The case order matters: a routine the
// tick skips is held whatever its budget says, and an attempt still running
// would otherwise look like one that failed.
func pendingDisposition(p *schedule.Pending, r *routine.Routine, now time.Time, loc *time.Location, settled map[string]bool) string {
	switch {
	case !supervisor.Schedulable(r):
		return "held -- the supervisor skips this routine, so no attempt is coming"
	case p.Attempts > 0 && settled != nil && !settled[attemptKey(p.RunID, p.Attempts)]:
		// Reserved but not yet settled. The state file alone can't tell this
		// from a failed attempt backing off -- reserve writes the count and a
		// non-final failure leaves it untouched -- so the run record, written
		// only on settlement, is the tell.
		return fmt.Sprintf("attempt %d started %s, still in flight", p.Attempts, stamp(p.LastAttemptAt, now, loc))
	case p.Attempts >= supervisor.MaxAttempts:
		return "budget spent -- the next tick abandons it"
	case !schedule.NextRetryAt(p).After(now):
		return "due now"
	default:
		return "next attempt " + stamp(schedule.NextRetryAt(p), now, loc)
	}
}

func attemptKey(runID string, attempt int) string {
	return fmt.Sprintf("%s#%d", runID, attempt)
}

// Collects the attempts runs.jsonl has a record for; a record is appended at
// settlement, so a reserved attempt missing from it is still running. Nil
// when there's no log at all -- absence must not read as "in flight".
func settledAttempts(dir string) map[string]bool {
	raw, err := os.ReadFile(filepath.Join(knowledge.NewStore(dir).Worktree(), "runs.jsonl"))
	if err != nil {
		return nil
	}
	settled := map[string]bool{}
	for _, rec := range run.ParseRecords(raw) {
		if rec.RunID == "" {
			continue
		}
		settled[attemptKey(rec.RunID, rec.Attempt)] = true
	}
	return settled
}

// Formats a scheduling time at the coarsest precision that stays
// unambiguous: weekday and clock inside a week, calendar date beyond it, the
// year once it differs -- "Mon 21:12" nine days stale would misread as two.
func stamp(t time.Time, now time.Time, loc *time.Location) string {
	t = t.In(loc)
	switch d := t.Sub(now); {
	case t.Year() != now.Year():
		return t.Format("Jan 2 2006 15:04")
	case d > -6*24*time.Hour && d < 6*24*time.Hour:
		return t.Format("Mon 15:04")
	default:
		return t.Format("Jan 2 15:04")
	}
}

// Shows the one-line total; the breakdown lives in `openroutines usage`.
// Silent when no record carries usage.
func printTokenUsage(dir string) {
	t := totalUsage(aggregateUsage(dir))
	if t.RunsReported == 0 {
		return
	}
	fmt.Printf("\ntoken usage (%s): in %s  out %s", retentionLabel(dir), formatTokens(t.Tokens.Input), formatTokens(t.Tokens.Output))
	if t.CostReported > 0 {
		fmt.Printf("  ~$%.2f reported", t.CostReported)
	}
	fmt.Printf(" -- openroutines usage for the breakdown\n")
}

func orUnset(s string) string {
	if s == "" || strings.Contains(s, "{{") {
		return "(not set)"
	}
	return s
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
