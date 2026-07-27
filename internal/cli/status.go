package cli

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/memory"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/skill"
	"github.com/steadyspacecorp/openroutines/internal/version"
)

// cmdStatus shows what the agent has and still needs: identity, key, model,
// routines with their next firing, skills, and memory sync state.
func cmdStatus(_ []string) int {
	dir := "."
	agent, err := config.Load(dir)
	if err != nil {
		return fail(fmt.Errorf("not an agent repository (no %s)", config.FileName))
	}

	fmt.Printf("agent      %s\n", orUnset(agent.Name))
	fmt.Printf("job        %s\n", orUnset(firstLine(agent.Description)))
	fmt.Printf("owner      %s <%s>\n", orUnset(agent.Owner.Name), orUnset(agent.Owner.Email))
	fmt.Printf("timezone   %s\n", orUnset(agent.Timezone))
	fmt.Printf("model      %s (default)\n", orUnset(agent.Defaults.Model))
	if len(agent.Variables) > 0 {
		names := slices.Sorted(maps.Keys(agent.Variables))
		fmt.Printf("variables  %s\n", strings.Join(names, ", "))
	}
	if pin, err := os.ReadFile(filepath.Join(dir, ".openroutines-version")); err == nil {
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
	now := time.Now()
	if loc, err := time.LoadLocation(agent.Timezone); err == nil {
		now = now.In(loc)
	}
	routines, parseErrs := routine.LoadAgent(dir)
	fmt.Printf("\nroutines (%d):\n", len(routines))
	for _, r := range routines {
		state := "inactive"
		next := ""
		if r.FM.IsActive() {
			state = "active"
			if spec, err := cron.ParseStandard(r.FM.Schedule); err == nil {
				next = " -- next " + spec.Next(now).Format("Mon 15:04")
			}
		}
		grants := ""
		if n := len(r.FM.Skills) + len(r.FM.Credentials); n > 0 {
			grants = fmt.Sprintf(" (%d grant(s))", n)
		}
		fmt.Printf("  %-20s %-14s %s%s%s\n", r.Name, scheduleSummary(r), state, next, grants)
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

	// Memory.
	fmt.Printf("\nmemory:\n")
	ms := memory.Status(dir)
	if !ms.Materialized {
		fmt.Printf("  not materialized yet -- appears on first run\n")
	} else {
		fmt.Printf("  last commit: %s\n", ms.LastCommit)
		if ms.Uncommitted > 0 {
			fmt.Printf("  ! %d file(s) with uncommitted changes -- commit inside memory/ when done curating\n", ms.Uncommitted)
		}
		if ms.Unpushed > 0 {
			fmt.Printf("  %d commit(s) not yet pushed to origin\n", ms.Unpushed)
		}
		if cursors, err := memory.Cursors(dir); err == nil && len(cursors) > 0 {
			head, _ := memory.Head(dir)
			for name, c := range cursors {
				lag := ""
				if head != "" && !strings.HasPrefix(head, c.ConsumedThrough) && head != c.ConsumedThrough {
					if changes, err := memory.Changes(dir, c.ConsumedThrough, head); err == nil && len(changes) > 0 {
						lag = fmt.Sprintf(" -- %d change(s) pending", len(changes))
					}
				}
				fmt.Printf("  consumer %s: through %.12s (run %s)%s\n", name, c.ConsumedThrough, c.ByRun, lag)
			}
		}
	}
	if !memory.HasOrigin(dir) {
		fmt.Printf("  ! no git origin -- memory is not durable until one is set\n")
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

// printTokenUsage shows the one-line total; the numbers live in
// `openroutines usage`. Silent when no record carries usage (older
// releases, native dev runs) -- absence of bookkeeping is not news.
func printTokenUsage(dir string) {
	rows := aggregateUsage(dir)
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
