package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/knowledge"
	"github.com/steadyspacecorp/openroutines/internal/runner"
)

const knowledgeUsage = `Inspect the latest knowledge on origin without changing local knowledge/

Usage:
  openroutines knowledge                         interactive explorer
  openroutines knowledge summarize [--since 24h] [--yes]
                                                 ephemeral default-model briefing
  openroutines knowledge list [path] [--json]    list snapshot files
  openroutines knowledge show <path>             print one snapshot file
  openroutines knowledge stats [--json]          snapshot size and history
`

func cmdKnowledge(args []string) int {
	if wantsHelp(args) {
		fmt.Print(knowledgeUsage)
		return 0
	}
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	if sub != "" && sub != "summarize" && sub != "list" && sub != "show" && sub != "stats" {
		return fail(fmt.Errorf("unknown knowledge command %q\n\n%s", sub, knowledgeUsage))
	}
	mem := knowledge.NewStore(".")
	snap, err := mem.FetchOriginSnapshot()
	if err != nil {
		return fail(err)
	}
	defer func() { _ = snap.Close() }()
	if sub != "" {
		printSnapshotRelation(os.Stderr, snap.Relation(mem))
	}
	switch sub {
	case "summarize":
		return knowledgeSummarize(snap, args)
	case "list":
		return knowledgeList(snap, args)
	case "show":
		return knowledgeShow(snap, args)
	case "stats":
		return knowledgeStats(snap, args)
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		if code := printKnowledgeOverview(mem, snap); code != 0 {
			return code
		}
		fmt.Print("\n" + knowledgeUsage)
		return 0
	}
	return knowledgeInteractive(mem, snap)
}

func printKnowledgeOverview(mem *knowledge.Store, snap *knowledge.OriginSnapshot) int {
	agent, err := config.Load(".")
	if err != nil {
		return fail(err)
	}
	stats, err := snap.Stats()
	if err != nil {
		return fail(err)
	}
	fmt.Println(bold(fmt.Sprintf("%s knowledge at origin/%s", agent.Name, knowledge.Branch)))
	fmt.Println(dim(fmt.Sprintf("updated %s (%s) · %s · %d files · history since %s", relativeTime(stats.LastWrite), formatTime(stats.LastWrite), formatBytes(stats.SizeBytes), stats.Files, stats.FirstWrite.Format("Jan 2, 2006"))))
	printSnapshotRelation(os.Stdout, snap.Relation(mem))
	return 0
}

func printSnapshotRelation(out io.Writer, r knowledge.SnapshotRelation) {
	switch {
	case !r.Materialized:
		_, _ = fmt.Fprintf(out, "%s knowledge/ is not materialized locally; showing origin\n", warnMark)
	case r.Diverged:
		_, _ = fmt.Fprintf(out, "%s local knowledge/ and the origin snapshot have diverged; showing origin\n", warnMark)
	case r.Behind > 0:
		_, _ = fmt.Fprintf(out, "%s local knowledge/ is %d commit(s) behind; showing origin\n", warnMark, r.Behind)
	case r.Ahead > 0:
		_, _ = fmt.Fprintf(out, "%s local knowledge/ has %d commit(s) not on origin; showing origin\n", warnMark, r.Ahead)
	}
	if r.Uncommitted > 0 {
		_, _ = fmt.Fprintf(out, "%s local knowledge/ has %d file(s) with uncommitted changes; showing origin\n", warnMark, r.Uncommitted)
	}
}

func knowledgeInteractive(mem *knowledge.Store, snap *knowledge.OriginSnapshot) int {
	if code := printKnowledgeOverview(mem, snap); code != 0 {
		return code
	}
	in := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("\n" + bold("What would you like to do?") + "\n  1. Summarize\n  2. Browse files\n  3. View stats\n  4. Exit\n> ")
		choice, err := in.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return 0
			}
			return fail(err)
		}
		switch strings.ToLower(strings.TrimSpace(choice)) {
		case "1", "summarize", "s":
			if code := knowledgeSummarizeWithReader(snap, 24*time.Hour, false, in); code != 0 {
				return code
			}
		case "2", "list", "browse", "b":
			if code := knowledgeBrowse(snap, in); code != 0 {
				return code
			}
		case "3", "stats":
			if code := knowledgeStats(snap, nil); code != 0 {
				return code
			}
		case "4", "exit", "quit", "q":
			return 0
		default:
			fmt.Println("choose 1, 2, 3, or 4")
		}
	}
}

func knowledgeList(snap *knowledge.OriginSnapshot, args []string) int {
	positional, flags, help, err := parseFlags(args, map[string]flagSpec{"--json": {}})
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println("usage: openroutines knowledge list [path] [--json]")
		return 0
	}
	if len(positional) > 1 {
		return fail(fmt.Errorf("usage: openroutines knowledge list [path] [--json]"))
	}
	rel := ""
	if len(positional) == 1 {
		rel = positional[0]
	}
	files, err := snap.Files(rel)
	if err != nil {
		return fail(err)
	}
	if _, ok := flags["--json"]; ok {
		raw, _ := json.MarshalIndent(files, "", "  ")
		fmt.Println(string(raw))
		return 0
	}
	for _, f := range files {
		fmt.Printf("%-45s %9s  %s\n", f.Path, formatBytes(f.Size), formatTime(f.LastChanged))
	}
	return 0
}

func knowledgeShow(snap *knowledge.OriginSnapshot, args []string) int {
	positional, _, help, err := parseFlags(args, nil)
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println("usage: openroutines knowledge show <path>")
		return 0
	}
	if len(positional) != 1 {
		return fail(fmt.Errorf("usage: openroutines knowledge show <path>"))
	}
	raw, err := snap.ReadFile(positional[0])
	if err != nil {
		return fail(err)
	}
	if bytes.IndexByte(raw, 0) >= 0 {
		return fail(fmt.Errorf("%s appears to be binary", positional[0]))
	}
	_, _ = os.Stdout.Write(raw)
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		fmt.Println()
	}
	return 0
}

func knowledgeStats(snap *knowledge.OriginSnapshot, args []string) int {
	positional, flags, help, err := parseFlags(args, map[string]flagSpec{"--json": {}})
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println("usage: openroutines knowledge stats [--json]")
		return 0
	}
	if len(positional) != 0 {
		return fail(fmt.Errorf("usage: openroutines knowledge stats [--json]"))
	}
	stats, err := snap.Stats()
	if err != nil {
		return fail(err)
	}
	if _, ok := flags["--json"]; ok {
		raw, _ := json.MarshalIndent(stats, "", "  ")
		fmt.Println(string(raw))
		return 0
	}
	fmt.Printf("snapshot     %s\n", shortCommit(stats.Commit))
	fmt.Printf("first write  %s\n", formatTime(stats.FirstWrite))
	fmt.Printf("last write   %s (%s)\n", formatTime(stats.LastWrite), stats.LastSubject)
	fmt.Printf("history      %d days, %d commits\n", stats.HistoryDays, stats.Commits)
	fmt.Printf("current tree %s across %d files\n", formatBytes(stats.SizeBytes), stats.Files)
	if stats.LargestPath != "" {
		fmt.Printf("largest file %s (%s)\n", stats.LargestPath, formatBytes(stats.LargestBytes))
	}
	return 0
}

func knowledgeBrowse(snap *knowledge.OriginSnapshot, in *bufio.Reader) int {
	files, err := snap.Files("")
	if err != nil {
		return fail(err)
	}
	for {
		fmt.Println("\nFiles:")
		for i, f := range files {
			fmt.Printf("  %2d. %-40s %9s\n", i+1, f.Path, formatBytes(f.Size))
		}
		fmt.Print("Select a file, or b to go back: ")
		line, err := in.ReadString('\n')
		if err != nil {
			return 0
		}
		line = strings.TrimSpace(line)
		if line == "b" || line == "q" || line == "" {
			return 0
		}
		n, err := strconv.Atoi(line)
		if err != nil || n < 1 || n > len(files) {
			fmt.Println("choose a listed file number or b")
			continue
		}
		fmt.Printf("\n--- %s ---\n", bold(files[n-1].Path))
		if code := knowledgeShow(snap, []string{files[n-1].Path}); code != 0 {
			return code
		}
	}
}

func knowledgeSummarize(snap *knowledge.OriginSnapshot, args []string) int {
	positional, flags, help, err := parseFlags(args, map[string]flagSpec{"--yes": {}, "--since": {value: true}})
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println("usage: openroutines knowledge summarize [--since 24h] [--yes]")
		return 0
	}
	if len(positional) != 0 {
		return fail(fmt.Errorf("usage: openroutines knowledge summarize [--since 24h] [--yes]"))
	}
	_, yes := flags["--yes"]
	window := 24 * time.Hour
	if raw, ok := flags["--since"]; ok {
		window, err = time.ParseDuration(raw)
		if err != nil || window <= 0 {
			return fail(fmt.Errorf("--since must be a positive duration (for example 24h)"))
		}
	}
	return knowledgeSummarizeWithReader(snap, window, yes, bufio.NewReader(os.Stdin))
}

func knowledgeSummarizeWithReader(snap *knowledge.OriginSnapshot, window time.Duration, yes bool, in *bufio.Reader) int {
	agent, err := config.Load(".")
	if err != nil {
		return fail(err)
	}
	if !yes {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return fail(fmt.Errorf("summarize calls %s and spends tokens; pass --yes in non-interactive use", agent.Defaults.Model))
		}
		fmt.Printf("Summarize the last %s with %s and spend tokens. Continue? [Y/n] ", window, agent.Defaults.Model)
		answer, _ := in.ReadString('\n')
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer == "n" || answer == "no" {
			return 0
		}
	}
	through := snap.FetchedAt
	since := through.Add(-window)
	recent, err := snap.ChangesSince(since)
	if err != nil {
		return fail(err)
	}
	res, err := runner.SummarizeKnowledge(".", snap.Dir, snap.Commit, since, through, recent, os.Stdout)
	if err != nil {
		return fail(err)
	}
	fmt.Println("\n" + dim(fmt.Sprintf("%s · snapshot %s", runner.FormatUsage(res.Usage), shortCommit(snap.Commit))))
	if res.Outcome != runner.Completed {
		if res.Hint != "" {
			fmt.Println(res.Hint)
		}
		return 1
	}
	return 0
}

func formatBytes(n int64) string {
	if n >= 1024*1024 {
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
	if n >= 1024 {
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	return fmt.Sprintf("%d B", n)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Local().Format("2006-01-02 15:04 MST")
}

func relativeTime(t time.Time) string {
	d := time.Since(t)
	if d < time.Hour {
		return fmt.Sprintf("%d minutes ago", max(0, int(d.Minutes())))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

func shortCommit(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
