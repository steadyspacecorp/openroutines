package cli

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/knowledge"
	"github.com/steadyspacecorp/openroutines/internal/run"
)

// Mirrors the tokens object run records carry.
type usageTokens = run.Tokens

// One routine's aggregate over the records the retention window keeps. Runs
// counts every recorded run; the token sums and RunsReported cover only runs
// whose runtime reported usage. Model and effort are the most recently
// recorded values, not a per-run breakdown.
type usageRow struct {
	Routine      string      `json:"routine"`
	Runs         int         `json:"runs"`
	RunsReported int         `json:"runs_reported"`
	Tokens       usageTokens `json:"tokens"`
	CostReported float64     `json:"cost_reported"`
	Model        string      `json:"model,omitempty"`
	Effort       string      `json:"effort,omitempty"`
}

// cost_reported is opencode's own cost estimate -- informational, since
// prices drift; tokens with model and effort are the durable record.
const usageUsage = "usage: openroutines usage [--json]"

func cmdUsage(args []string) int {
	positional, flags, help, err := parseFlags(args, map[string]flagSpec{"--json": {}})
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println(usageUsage)
		return 0
	}
	if len(positional) != 0 {
		return fail(fmt.Errorf("%s", usageUsage))
	}
	_, asJSON := flags["--json"]

	rows := aggregateUsage(".")

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
		// A fresh clone of a running agent also reads as "no runs", but the
		// records exist on origin and won't materialize locally on their own.
		if st := knowledge.At(".").Status(); !st.Materialized && st.RemoteKnowledge {
			fmt.Println("knowledge is not materialized in this checkout -- run openroutines sync to adopt the agent's records from origin")
		} else {
			fmt.Println("no runs recorded yet -- records accumulate as routines run")
		}
		printKnowledgeLag(".")
		return 0
	}
	total := totalUsage(rows)
	fmt.Println(bold("token usage (" + retentionLabel(".") + "):"))
	printUsageTable(rows, total)
	if total.RunsReported < total.Runs {
		fmt.Println(dim(fmt.Sprintf("token sums cover the %d of %d run(s) whose runtime reported usage", total.RunsReported, total.Runs)))
	}
	printKnowledgeLag(".")
	return 0
}

// Names the window usage aggregates over, so the header reads "last 30 days"
// instead of the retention term of art.
func retentionLabel(dir string) string {
	retention := knowledge.DefaultRetention
	if agent, err := config.Load(dir); err == nil {
		if d, err := knowledge.ParseRetention(agent.Retention()); err == nil {
			retention = d
		}
	}
	span := retention.String()
	if days := int(retention.Hours() / 24); days >= 1 && retention == time.Duration(days)*24*time.Hour {
		span = fmt.Sprintf("%d days", days)
		if days == 1 {
			span = "1 day"
		}
	}
	return fmt.Sprintf("last %s, since %s", span, time.Now().Add(-retention).Format("January 2, 2006"))
}

// Names the gap between what this checkout has and what origin holds, so a
// stale worktree doesn't read as a quiet zero.
func printKnowledgeLag(dir string) {
	if st := knowledge.At(dir).Status(); st.Behind > 0 {
		fmt.Printf("\n%s these numbers read local knowledge/, %d commit(s) behind origin/%s as of your last fetch -- run openroutines sync to count the latest runs\n", warnMark, st.Behind, knowledge.Branch)
	}
}

// Folds runs.jsonl into per-routine rows, sorted by name. Every parseable
// record counts as a run; the token sums and RunsReported cover only records
// that carry a tokens object -- older releases, native dev runs, and
// unreported usage count as runs without contributing sums.
func aggregateUsage(dir string) []usageRow {
	raw, err := os.ReadFile(filepath.Join(knowledge.At(dir).Worktree(), "runs.jsonl"))
	if err != nil {
		return nil
	}
	byName := map[string]*usageRow{}
	for _, rec := range run.ParseRecords(raw) {
		r := byName[rec.Routine]
		if r == nil {
			r = &usageRow{Routine: rec.Routine}
			byName[rec.Routine] = r
		}
		r.Runs++
		r.CostReported += rec.CostReported
		if rec.Model != "" {
			r.Model, r.Effort = rec.Model, rec.Effort
		}
		if rec.Tokens == nil {
			continue
		}
		r.RunsReported++
		r.Tokens.Input += rec.Tokens.Input
		r.Tokens.Output += rec.Tokens.Output
		r.Tokens.Reasoning += rec.Tokens.Reasoning
		r.Tokens.CacheRead += rec.Tokens.CacheRead
		r.Tokens.CacheWrite += rec.Tokens.CacheWrite
	}
	out := make([]usageRow, 0, len(byName))
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		out = append(out, *byName[name])
	}
	return out
}

func totalUsage(rows []usageRow) usageRow {
	t := usageRow{Routine: "total"}
	for _, r := range rows {
		t.Runs += r.Runs
		t.RunsReported += r.RunsReported
		t.Tokens.Input += r.Tokens.Input
		t.Tokens.Output += r.Tokens.Output
		t.Tokens.Reasoning += r.Tokens.Reasoning
		t.Tokens.CacheRead += r.Tokens.CacheRead
		t.Tokens.CacheWrite += r.Tokens.CacheWrite
		t.CostReported += r.CostReported
	}
	return t
}

// Renders the rows and the total as aligned columns, numbers right-aligned
// under a dim header. A routine whose runs never reported usage keeps blank
// token cells rather than misreading as zero. Columns no row has --
// reasoning, cache traffic, cost, model -- are dropped whole rather than
// printed empty.
func printUsageTable(rows []usageRow, total usageRow) {
	all := append(slices.Clone(rows), total)
	var hasReasoning, hasCache, hasCost, hasModel bool
	for _, r := range all {
		hasReasoning = hasReasoning || r.Tokens.Reasoning > 0
		hasCache = hasCache || r.Tokens.CacheRead > 0 || r.Tokens.CacheWrite > 0
		hasCost = hasCost || r.CostReported > 0
		hasModel = hasModel || r.Model != ""
	}
	blankZero := func(n int64) string {
		if n == 0 {
			return ""
		}
		return formatTokens(n)
	}

	heads := []string{"routine", "runs", "in", "out"}
	leftAligned := []bool{true, false, false, false}
	addCol := func(head string, left bool) {
		heads = append(heads, head)
		leftAligned = append(leftAligned, left)
	}
	if hasReasoning {
		addCol("reasoning", false)
	}
	if hasCache {
		addCol("cache-read", false)
		addCol("cache-write", false)
	}
	if hasCost {
		addCol("reported", false)
	}
	if hasModel {
		addCol("model", true)
	}

	cells := make([][]string, 0, len(all))
	for _, r := range all {
		reported := func(n int64) string {
			if r.RunsReported == 0 {
				return ""
			}
			return formatTokens(n)
		}
		c := []string{r.Routine, strconv.Itoa(r.Runs), reported(r.Tokens.Input), reported(r.Tokens.Output)}
		if hasReasoning {
			c = append(c, blankZero(r.Tokens.Reasoning))
		}
		if hasCache {
			c = append(c, blankZero(r.Tokens.CacheRead), blankZero(r.Tokens.CacheWrite))
		}
		if hasCost {
			cost := ""
			if r.CostReported > 0 {
				cost = fmt.Sprintf("~$%.2f", r.CostReported)
			}
			c = append(c, cost)
		}
		if hasModel {
			m := r.Model
			if r.Effort != "" {
				m += " @" + r.Effort
			}
			c = append(c, m)
		}
		cells = append(cells, c)
	}

	widths := make([]int, len(heads))
	for i, h := range heads {
		widths[i] = len(h)
	}
	for _, c := range cells {
		for i, v := range c {
			widths[i] = max(widths[i], len(v))
		}
	}

	// Pad before styling: escape bytes inside would count toward the width.
	pad := func(i int, v string) string {
		if leftAligned[i] {
			return fmt.Sprintf("%-*s", widths[i], v)
		}
		return fmt.Sprintf("%*s", widths[i], v)
	}
	line := make([]string, len(heads))
	for i, h := range heads {
		line[i] = pad(i, h)
	}
	fmt.Println(dim("  " + strings.TrimRight(strings.Join(line, "  "), " ")))
	for row, c := range cells {
		for i, v := range c {
			line[i] = pad(i, v)
			if i == len(heads)-1 && hasModel && v != "" {
				line[i] = dim(line[i])
			}
		}
		if row == len(cells)-1 {
			line[0] = bold(line[0])
		}
		fmt.Println("  " + strings.TrimRight(strings.Join(line, "  "), " "))
	}
}

// Keeps counts scannable: 812, 13.8k, 2.1M.
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
