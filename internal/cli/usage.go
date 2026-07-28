package cli

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/steadyspacecorp/openroutines/internal/memory"
)

// usageTokens mirrors the tokens object run records carry.
type usageTokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	Reasoning  int64 `json:"reasoning"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// usageRow is one routine's aggregate over the records the retention
// window keeps. Model and effort are the most recently recorded values --
// a routine's identity for cost purposes, not a per-run breakdown.
type usageRow struct {
	Routine      string      `json:"routine"`
	Runs         int         `json:"runs"`
	Tokens       usageTokens `json:"tokens"`
	CostReported float64     `json:"cost_reported"`
	Model        string      `json:"model,omitempty"`
	Effort       string      `json:"effort,omitempty"`
}

// cmdUsage reports token use and reported cost per routine, aggregated
// from runs.jsonl. --json emits the machine-readable form for scripts and
// monitors; tokens with model and effort are the durable record, and
// cost_reported is opencode's own estimate (informational -- prices drift).
func cmdUsage(args []string) int {
	asJSON := false
	for _, a := range args {
		if a == "--json" {
			asJSON = true
			continue
		}
		return fail(fmt.Errorf("usage: openroutines usage [--json]"))
	}
	rows, records := aggregateUsage(".")

	if asJSON {
		out := struct {
			Window   string     `json:"window"`
			Routines []usageRow `json:"routines"`
			Total    usageRow   `json:"total"`
		}{Window: "retention", Routines: rows, Total: totalUsage(rows)}
		raw, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return fail(err)
		}
		fmt.Println(string(raw))
		return 0
	}

	if len(rows) == 0 {
		// Three different situations reached this line as one silent "no
		// usage yet". Records that carry no tokens are the confusing case:
		// runs did happen, so telling someone to wait for them is wrong.
		if records == 0 {
			// So is a fresh clone of a running agent: the records exist on
			// origin, and no amount of waiting materializes them locally.
			if st := memory.At(".").Status(); !st.Materialized && st.RemoteMemory {
				fmt.Println("memory is not materialized in this checkout -- run openroutines sync to adopt the agent's records from origin")
			} else {
				fmt.Println("no runs recorded yet -- records accumulate as routines run")
			}
		} else {
			fmt.Printf("%d run(s) recorded, none reported token usage -- absent usage means the runtime did not report it, never zero\n", records)
		}
		printMemoryLag(".")
		return 0
	}
	fmt.Println("token usage (retention window):")
	for _, r := range rows {
		printUsageLine(r.Routine, r, true)
	}
	printUsageLine("total", totalUsage(rows), false)
	printMemoryLag(".")
	return 0
}

// printMemoryLag names the gap between what this checkout has and what
// origin holds. usage reads only the worktree -- that is the contract, and
// this is what keeps a stale worktree from reading as a quiet zero.
func printMemoryLag(dir string) {
	if st := memory.At(dir).Status(); st.Behind > 0 {
		fmt.Printf("\nmemory/ is %d commit(s) behind origin/%s as of your last fetch -- run openroutines sync to get the latest memory from origin\n", st.Behind, memory.Branch)
	}
}

// aggregateUsage folds runs.jsonl into per-routine rows, sorted by name,
// and returns how many records it read. Records without a tokens object
// (older releases, native dev runs, a runtime that did not report) are
// skipped -- absence is not zero -- so the count is what distinguishes an
// agent that has never run from one whose runs carry no usage.
func aggregateUsage(dir string) ([]usageRow, int) {
	raw, err := os.ReadFile(filepath.Join(memory.At(dir).Worktree(), "runs.jsonl"))
	if err != nil {
		return nil, 0
	}
	records := 0
	byName := map[string]*usageRow{}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec struct {
			Routine string       `json:"routine"`
			Model   string       `json:"model"`
			Effort  string       `json:"effort"`
			Cost    float64      `json:"cost_reported"`
			Tokens  *usageTokens `json:"tokens"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		records++
		if rec.Tokens == nil {
			continue
		}
		r := byName[rec.Routine]
		if r == nil {
			r = &usageRow{Routine: rec.Routine}
			byName[rec.Routine] = r
		}
		r.Runs++
		r.Tokens.Input += rec.Tokens.Input
		r.Tokens.Output += rec.Tokens.Output
		r.Tokens.Reasoning += rec.Tokens.Reasoning
		r.Tokens.CacheRead += rec.Tokens.CacheRead
		r.Tokens.CacheWrite += rec.Tokens.CacheWrite
		r.CostReported += rec.Cost
		if rec.Model != "" {
			r.Model, r.Effort = rec.Model, rec.Effort
		}
	}
	out := make([]usageRow, 0, len(byName))
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		out = append(out, *byName[name])
	}
	return out, records
}

func totalUsage(rows []usageRow) usageRow {
	t := usageRow{Routine: "total"}
	for _, r := range rows {
		t.Runs += r.Runs
		t.Tokens.Input += r.Tokens.Input
		t.Tokens.Output += r.Tokens.Output
		t.Tokens.Reasoning += r.Tokens.Reasoning
		t.Tokens.CacheRead += r.Tokens.CacheRead
		t.Tokens.CacheWrite += r.Tokens.CacheWrite
		t.CostReported += r.CostReported
	}
	return t
}

func printUsageLine(label string, r usageRow, withRuns bool) {
	runs := "        "
	if withRuns {
		runs = fmt.Sprintf("%3d run(s)", r.Runs)
	}
	fmt.Printf("  %-20s %s  in %s  out %s", label, runs, formatTokens(r.Tokens.Input), formatTokens(r.Tokens.Output))
	if r.Tokens.Reasoning > 0 {
		fmt.Printf(" (reasoning %s)", formatTokens(r.Tokens.Reasoning))
	}
	// Cache traffic usually dwarfs fresh input and is priced differently --
	// without it a human cannot derive the spend from the counts.
	if r.Tokens.CacheRead > 0 || r.Tokens.CacheWrite > 0 {
		fmt.Printf("  cache-read %s  cache-write %s", formatTokens(r.Tokens.CacheRead), formatTokens(r.Tokens.CacheWrite))
	}
	if r.CostReported > 0 {
		fmt.Printf("  ~$%.2f reported", r.CostReported)
	}
	if r.Model != "" {
		fmt.Printf("  %s", r.Model)
		if r.Effort != "" {
			fmt.Printf(" @%s", r.Effort)
		}
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
