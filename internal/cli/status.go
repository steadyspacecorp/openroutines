package cli

import (
	"encoding/json"
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
	"github.com/steadyspacecorp/openroutines/internal/schedule"
	"github.com/steadyspacecorp/openroutines/internal/skill"
	"github.com/steadyspacecorp/openroutines/internal/supervisor"
	"github.com/steadyspacecorp/openroutines/internal/version"
)

const statusUsage = "usage: openroutines status"

// cmdStatus shows what the agent has and still needs: identity, key, model,
// routines with their next firing, skills, and knowledge sync state.
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

	fmt.Printf("agent      %s\n", orUnset(agent.Name))
	fmt.Printf("job        %s\n", orUnset(firstLine(agent.Description)))
	fmt.Printf("owner      %s <%s>\n", orUnset(agent.Owner.Name), orUnset(agent.Owner.Email))
	// The supervisor refuses to start on an unloadable timezone; status is a
	// diagnostic, so it says so and carries on in UTC rather than quietly
	// printing fire times the agent would never use.
	loc, err := time.LoadLocation(agent.Timezone)
	tzNote := ""
	if err != nil {
		loc, tzNote = time.UTC, " -- INVALID, times below shown in UTC"
	}
	fmt.Printf("timezone   %s%s\n", orUnset(agent.Timezone), tzNote)
	fmt.Printf("model      %s (default)\n", orUnset(agent.Defaults.Model))
	if len(agent.Variables) > 0 {
		names := slices.Sorted(maps.Keys(agent.Variables))
		fmt.Printf("variables  %s\n", strings.Join(names, ", "))
	}
	if pin, err := os.ReadFile(filepath.Join(dir, ".openroutines", "version")); err == nil {
		fmt.Printf("framework  %s (pinned; this binary is %s)\n", strings.TrimSpace(string(pin)), version.Version)
	}

	// Master key + credentials.
	if key, err := creds.LoadKey(dir); err != nil {
		fmt.Printf("master key MISSING -- run openroutines configure\n")
	} else if store, err := creds.Read(dir, key); err != nil {
		fmt.Printf("master key present, but credentials do not decrypt: %v\n", err)
	} else {
		names := make([]string, 0, len(store))
		for k := range store {
			names = append(names, k)
		}
		if len(names) == 0 {
			fmt.Printf("master key present -- no credentials stored yet\n")
		} else {
			fmt.Printf("master key present -- %d credential(s): %s\n", len(store), strings.Join(names, ", "))
		}
	}

	// Routines.
	now := time.Now().In(loc)
	stateDir := knowledge.At(dir).StateDir()
	settled := settledAttempts(dir)
	routines, parseErrs := routine.LoadAgent(dir)
	fmt.Printf("\nroutines (%d):\n", len(routines))
	reported := false
	for _, r := range routines {
		st, stErr := schedule.Load(stateDir, r.Name)
		state := "inactive"
		next := ""
		if r.FM.IsActive() {
			state = "active"
			// A routine in cool-down does not fire at its next occurrence, so
			// don't print one: a confident time that will not happen is worse
			// than no time at all. The cool-down's end is on the breaker line
			// below, which is the one place it lives.
			if st != nil && st.CoolingDown(now) {
				next = " -- cooling down"
			} else if spec, err := schedule.Parse(r.FM.Schedule, loc); err == nil {
				next = " -- next " + stamp(spec.Next(now), now, loc)
			}
		}
		grants := ""
		if g := grantSummary(r); len(g) > 0 {
			grants = " (" + strings.Join(g, " ") + ")"
		}
		fmt.Printf("  %-20s %-14s %s%s%s\n", r.Name, scheduleSummary(r), state, next, grants)
		if stErr != nil {
			fmt.Printf("      ! %v\n", stErr)
		}
		for _, line := range scheduleStateLines(st, r, now, loc, settled) {
			reported = true
			fmt.Printf("      %s\n", line)
		}
	}
	// The lines above came out of the knowledge worktree, so they are only as
	// current as the last fetch. The knowledge section says so too, but it is
	// screens away by then and these are the most staleness-sensitive numbers
	// the command prints.
	if reported {
		if ms := knowledge.At(dir).Status(); ms.Behind > 0 {
			fmt.Printf("  ! scheduling state above is from knowledge %d commit(s) behind origin/%s -- run openroutines sync\n", ms.Behind, knowledge.Branch)
		}
	}
	for _, e := range parseErrs {
		fmt.Printf("  ! %v\n", e)
	}

	// Skills.
	skills, skillErrs := skill.ListAgent(dir)
	fmt.Printf("\nskills (%d):\n", len(skills))
	for _, s := range skills {
		fmt.Printf("  %-20s %s\n", s.Name, firstLine(s.Description))
	}
	for _, e := range skillErrs {
		fmt.Printf("  ! %v\n", e)
	}

	// Knowledge.
	fmt.Printf("\nknowledge:\n")
	mem := knowledge.At(dir)
	ms := mem.Status()
	if !ms.Materialized {
		if ms.RemoteKnowledge {
			fmt.Printf("  ! not materialized in this checkout -- origin has the agent's knowledge; run openroutines sync to adopt it\n")
		} else {
			fmt.Printf("  not materialized yet -- appears on first run\n")
		}
	} else {
		fmt.Printf("  last commit: %s\n", ms.LastCommit)
		if ms.Uncommitted > 0 {
			fmt.Printf("  ! %d file(s) with uncommitted changes -- commit inside knowledge/ when done curating\n", ms.Uncommitted)
		}
		if ms.Unpushed > 0 {
			fmt.Printf("  %d commit(s) not yet pushed to origin\n", ms.Unpushed)
		}
		if ms.Behind > 0 {
			fmt.Printf("  ! %d commit(s) behind origin/%s -- this checkout is reading old knowledge; run openroutines sync to get the latest from origin\n", ms.Behind, knowledge.Branch)
		}
		if cursors, err := mem.Cursors(); err == nil && len(cursors) > 0 {
			head, _ := mem.Head()
			for name, c := range cursors {
				lag := ""
				if head != "" && !strings.HasPrefix(head, c.ConsumedThrough) && head != c.ConsumedThrough {
					changes, err := mem.Changes(c.ConsumedThrough, head)
					switch {
					// Silence here would read as caught up, which is the one
					// thing a stuck consumer is not: its runs are abandoned on
					// sight until a person repairs the file.
					case errors.Is(err, knowledge.ErrCursorUnreachable):
						lag = fmt.Sprintf(" -- ! not on the knowledge branch, delivery is stuck: repair or delete %s", knowledge.CursorFile(name))
					case err == nil && len(changes) > 0:
						lag = fmt.Sprintf(" -- %d change(s) pending", len(changes))
					}
				}
				fmt.Printf("  consumer %s: through %.12s (run %s)%s\n", name, c.ConsumedThrough, c.ByRun, lag)
			}
		}
	}
	if !mem.HasOrigin() {
		fmt.Printf("  ! no git origin -- knowledge is not durable until one is set\n")
	}

	printTokenUsage(dir)

	// What's still needed.
	if problems := agent.Problems(); len(problems) > 0 {
		fmt.Printf("\nstill needed:\n")
		for _, p := range problems {
			fmt.Printf("  - %s\n", p)
		}
	}
	return 0
}

// scheduleStateLines renders the supervisor's durable scheduling record for
// one routine: what it still owes, and what is holding it. Without them a
// routine four attempts into a retry, or sitting out a 24h circuit-breaker
// cool-down, reads exactly like a healthy one. Nil state means the supervisor
// has never seen the routine, which every local checkout looks like -- silence
// is the honest rendering of that.
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

// pendingDisposition says what becomes of a pending run, which the attempt
// count alone does not tell. The order matters: a routine the tick skips is
// held whatever its budget says, and an attempt that is still running only
// looks like one that failed.
func pendingDisposition(p *schedule.Pending, r *routine.Routine, now time.Time, loc *time.Location, settled map[string]bool) string {
	switch {
	case !supervisor.Schedulable(r):
		return "held -- the supervisor skips this routine, so no attempt is coming"
	case p.Attempts > 0 && settled != nil && !settled[attemptKey(p.RunID, p.Attempts)]:
		// Reserved and not yet settled. The state file cannot tell this from
		// an attempt that failed and is backing off -- reserve writes the
		// count, and a non-final failure leaves it untouched -- so the run
		// record, which only exists once an attempt settles, is the tell.
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

// settledAttempts collects the attempts runs.jsonl has a record for; a record
// is appended at settlement, so a reserved attempt missing from it is still
// running. Nil when there is no log to read at all (never run, or a release
// that predates it): absent evidence must not be read as "in flight".
func settledAttempts(dir string) map[string]bool {
	raw, err := os.ReadFile(filepath.Join(knowledge.At(dir).Worktree(), "runs.jsonl"))
	if err != nil {
		return nil
	}
	settled := map[string]bool{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		var rec struct {
			RunID   string `json:"run_id"`
			Attempt int    `json:"attempt"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil || rec.RunID == "" {
			continue
		}
		settled[attemptKey(rec.RunID, rec.Attempt)] = true
	}
	return settled
}

// stamp formats a scheduling time at the coarsest precision that stays
// unambiguous: weekday and clock inside a week, calendar date beyond it, the
// year once it differs. A held pending run is old by definition -- that is
// why it is on screen -- and "Mon 21:12" nine days stale reads as two.
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

// printTokenUsage shows the one-line total; the numbers live in
// `openroutines usage`. Silent when no record carries usage (older
// releases, native dev runs) -- absence of bookkeeping is not news.
func printTokenUsage(dir string) {
	rows, _ := aggregateUsage(dir)
	if len(rows) == 0 {
		return
	}
	t := totalUsage(rows)
	fmt.Printf("\ntoken usage (retention window): in %s  out %s", formatTokens(t.Tokens.Input), formatTokens(t.Tokens.Output))
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
