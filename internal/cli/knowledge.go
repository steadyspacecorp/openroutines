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
	"github.com/steadyspacecorp/openroutines/internal/supervisor"
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

// A missing local knowledge/ is not worth a line: the explorer always shows
// origin, and with nothing materialized there is nothing to differ from it.
func printSnapshotRelation(out io.Writer, r knowledge.SnapshotRelation) {
	switch {
	case !r.Materialized:
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
		// The blank line keeps a choice's output off the prompt's back;
		// browse brings its own. Never clear the screen: earlier output --
		// a briefing someone is acting on -- is the point of the session.
		switch strings.ToLower(strings.TrimSpace(choice)) {
		case "1", "summarize", "s":
			fmt.Println()
			if code := knowledgeSummarizeWithReader(snap, 24*time.Hour, false, in); code != 0 {
				return code
			}
		case "2", "list", "browse", "b":
			if code := knowledgeBrowse(snap, in); code != 0 {
				return code
			}
		case "3", "stats":
			fmt.Println()
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
	fmt.Printf("%s %s\n", dim("snapshot    "), shortCommit(stats.Commit))
	fmt.Printf("%s %s\n", dim("first write "), formatTime(stats.FirstWrite))
	fmt.Printf("%s %s %s\n", dim("last write  "), formatTime(stats.LastWrite), dim("("+stats.LastSubject+")"))
	fmt.Printf("%s %d days, %d commits\n", dim("history     "), stats.HistoryDays, stats.Commits)
	fmt.Printf("%s %s across %d files\n", dim("current tree"), formatBytes(stats.SizeBytes), stats.Files)
	if stats.LargestPath != "" {
		fmt.Printf("%s %s (%s)\n", dim("largest file"), stats.LargestPath, formatBytes(stats.LargestBytes))
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
		// The list would bury the file just shown; hold until the reader is
		// done with it.
		fmt.Print("\nb for the file list: ")
		after, err := in.ReadString('\n')
		if err != nil {
			return 0
		}
		if s := strings.TrimSpace(after); s == "q" {
			return 0
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

// briefingWriter renders the streamed briefing for a terminal. The model
// answers in light markdown; on a tty the markers become the same ANSI
// emphasis the rest of the CLI uses -- headings bold, bullet marks dimmed,
// **spans** bold. Unstyled output passes through untouched, so a pipe gets
// the model's raw text.
type briefingWriter struct {
	out io.Writer
	buf []byte
}

func (w *briefingWriter) Write(p []byte) (int, error) {
	if !styled {
		return w.out.Write(p)
	}
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := string(w.buf[:i])
		w.buf = w.buf[i+1:]
		if _, err := io.WriteString(w.out, styleBriefingLine(line)+"\n"); err != nil {
			return len(p), err
		}
	}
}

// Flush styles whatever remains buffered without a trailing newline.
func (w *briefingWriter) Flush() {
	if len(w.buf) == 0 {
		return
	}
	_, _ = io.WriteString(w.out, styleBriefingLine(string(w.buf)))
	w.buf = nil
}

// briefingHeadings are the sections the summary prompt asks for; they are
// recognized whether the model writes them bare, as markdown headings, or
// bold, and rendered the one way the CLI writes headings.
var briefingHeadings = []string{"Recently", "Next", "Waiting on a human"}

func styleBriefingLine(line string) string {
	head := strings.TrimSpace(line)
	head = strings.TrimLeft(head, "# ")
	head = strings.TrimSuffix(head, ":")
	head = strings.TrimPrefix(head, "**")
	head = strings.TrimSuffix(head, "**")
	head = strings.TrimSuffix(head, ":")
	for _, h := range briefingHeadings {
		if strings.EqualFold(head, h) {
			return bold(h)
		}
	}
	rest := strings.TrimLeft(line, " \t")
	indent := line[:len(line)-len(rest)]
	if after, ok := strings.CutPrefix(rest, "- "); ok {
		return indent + dim("-") + " " + styleInlineBold(after)
	}
	if after, ok := strings.CutPrefix(rest, "* "); ok {
		return indent + dim("-") + " " + styleInlineBold(after)
	}
	return styleInlineBold(line)
}

// styleInlineBold turns balanced **spans** into terminal bold; an odd edge
// leaves the line untouched rather than guess.
func styleInlineBold(s string) string {
	parts := strings.Split(s, "**")
	if len(parts) < 3 || len(parts)%2 == 0 {
		return s
	}
	var b strings.Builder
	for i, p := range parts {
		if i%2 == 1 {
			b.WriteString(bold(p))
		} else {
			b.WriteString(p)
		}
	}
	return b.String()
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
		fmt.Printf("Summarize the last %s with %s. Continue? [Y/n] ", window, agent.Defaults.Model)
		answer, _ := in.ReadString('\n')
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer == "n" || answer == "no" {
			return 0
		}
	}
	if err := supervisor.ValidateKeyFileLocations("."); err != nil {
		return fail(err)
	}
	through := snap.FetchedAt
	since := through.Add(-window)
	recent, err := snap.ChangesSince(since)
	if err != nil {
		return fail(err)
	}
	bw := &briefingWriter{out: os.Stdout}
	res, err := runner.SummarizeKnowledge(".", snap.Dir, snap.Commit, since, through, recent, bw)
	if err != nil {
		return fail(err)
	}
	bw.Flush()
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
