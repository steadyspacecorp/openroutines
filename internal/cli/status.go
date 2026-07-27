package cli

import (
	"encoding/json"
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
	routines, parseErrs := routine.LoadDir(filepath.Join(dir, "routines"))
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
	skills, skillErrs := skill.List(filepath.Join(dir, "skills"))
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

// printTokenUsage aggregates per-routine token consumption from the run
// records the retention window keeps. Silent when no record carries usage
// (older releases, native dev runs) -- absence of bookkeeping is not news.
func printTokenUsage(dir string) {
	raw, err := os.ReadFile(filepath.Join(memory.WorktreePath(dir), "runs.jsonl"))
	if err != nil {
		return
	}
	type row struct {
		runs               int
		in, out, reasoning int64
		cost               float64
		model              string
	}
	rows := map[string]*row{}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec struct {
			Routine string  `json:"routine"`
			Model   string  `json:"model"`
			Effort  string  `json:"effort"`
			Cost    float64 `json:"cost_reported"`
			Tokens  *struct {
				Input     int64 `json:"input"`
				Output    int64 `json:"output"`
				Reasoning int64 `json:"reasoning"`
			} `json:"tokens"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil || rec.Tokens == nil {
			continue
		}
		r := rows[rec.Routine]
		if r == nil {
			r = &row{}
			rows[rec.Routine] = r
		}
		r.runs++
		r.in += rec.Tokens.Input
		r.out += rec.Tokens.Output
		r.reasoning += rec.Tokens.Reasoning
		r.cost += rec.Cost
		if rec.Model != "" {
			r.model = rec.Model
			if rec.Effort != "" {
				r.model += " @" + rec.Effort
			}
		}
	}
	if len(rows) == 0 {
		return
	}
	fmt.Printf("\ntoken usage (retention window):\n")
	var tin, tout int64
	var tcost float64
	for _, name := range slices.Sorted(maps.Keys(rows)) {
		r := rows[name]
		fmt.Printf("  %-20s %3d run(s)  in %s  out %s", name, r.runs, formatTokens(r.in), formatTokens(r.out))
		if r.reasoning > 0 {
			fmt.Printf(" (reasoning %s)", formatTokens(r.reasoning))
		}
		if r.cost > 0 {
			fmt.Printf("  ~$%.2f reported", r.cost)
		}
		if r.model != "" {
			fmt.Printf("  %s", r.model)
		}
		fmt.Println()
		tin += r.in
		tout += r.out
		tcost += r.cost
	}
	fmt.Printf("  %-20s          in %s  out %s", "total", formatTokens(tin), formatTokens(tout))
	if tcost > 0 {
		fmt.Printf("  ~$%.2f reported", tcost)
	}
	fmt.Println()
}

// formatTokens keeps counts scannable: 812, 13.8k, 2.1M.
func formatTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
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
