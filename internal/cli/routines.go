package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/steadyspacecorp/openroutines/internal/memory"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/runner"
)

const newRoutineTemplate = `---
schedule: "0 9 * * *"
active: false
skills: []
credentials: []
---
Describe the job here. The body of this file is the prompt: say what to do,
what memory to consult (your ledger is memory/ledgers/%s.md), and what to
record when done. Set active: true when it's ready to run on schedule.
`

func cmdRoutines(args []string) int {
	if len(args) == 0 {
		fmt.Print(routinesUsage)
		return 2
	}
	if wantsHelp(args[:1]) {
		fmt.Print(routinesUsage)
		return 0
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "new":
		return routinesNew(rest)
	case "list":
		positional, _, help, err := parseFlags(rest, nil)
		if err != nil {
			return fail(err)
		}
		if help {
			fmt.Println("usage: openroutines routines list")
			return 0
		}
		if len(positional) != 0 {
			return fail(fmt.Errorf("usage: openroutines routines list"))
		}
		return routinesList()
	case "run":
		return routinesRun(rest)
	case "edit":
		return routinesEdit(rest)
	case "activate":
		return routinesSetActive(rest, true)
	case "deactivate":
		return routinesSetActive(rest, false)
	case "remove":
		return routinesRemove(rest)
	default:
		fmt.Fprintf(os.Stderr, "openroutines: unknown routines command %q\n\n", sub)
		fmt.Print(routinesUsage)
		return 2
	}
}

const routinesUsage = `Manage this agent's routines (markdown files in routines/)

Usage:
  openroutines routines new <name>         create a routine (inactive until you activate it)
  openroutines routines list               names, schedules, grants
  openroutines routines run <name> [--no-memory]
                                           run once now; --no-memory discards memory writes
  openroutines routines edit <name>        open in $EDITOR, validate on close
  openroutines routines activate <name>    set active: true
  openroutines routines deactivate <name>  set active: false
  openroutines routines remove <name>      delete the routine and its scheduling state
`

func routinesRun(args []string) int {
	nameArg, noMemory, help, err := parseRoutineRunArgs(args)
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println(routinesRunUsage)
		return 0
	}
	name, err := routineName(nameArg)
	if err != nil {
		return fail(err)
	}
	res, err := runner.Run(".", name, noMemory)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("\n%s: %s in %s (run %s)\n", name, res.Outcome, res.Duration, res.RunID)
	if res.SessionsDir != "" {
		fmt.Printf("sessions: %s\n", res.SessionsDir)
	}
	if res.Hint != "" {
		fmt.Println(res.Hint)
	}
	for _, f := range res.Conflicted {
		fmt.Printf("note: a concurrent run edited the same lines in %s -- both versions kept; curate the file if they disagree\n", f)
	}
	if noMemory {
		fmt.Println("memory discarded: external actions were still performed and credentials were available (first runs may still have initialized the memory worktree)")
	} else if res.Commit != "" {
		fmt.Printf("memory updated: commit %s on the %s branch\n", res.Commit, "memory")
	}
	if res.Outcome != runner.Completed {
		return 1
	}
	return 0
}

const routinesRunUsage = "usage: openroutines routines run <name> [--no-memory]"

func parseRoutineRunArgs(args []string) (string, bool, bool, error) {
	positional, flags, help, err := parseFlags(args, map[string]flagSpec{"--no-memory": {}})
	if err != nil {
		return "", false, false, err
	}
	if help {
		return "", false, true, nil
	}
	if len(positional) != 1 {
		return "", false, false, fmt.Errorf("%s", routinesRunUsage)
	}
	_, noMemory := flags["--no-memory"]
	return positional[0], noMemory, false, nil
}

func routinesNew(args []string) int {
	positional, _, help, err := parseFlags(args, nil)
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println("usage: openroutines routines new <name>")
		return 0
	}
	if len(positional) != 1 {
		return fail(fmt.Errorf("usage: openroutines routines new <name>"))
	}
	name, err := routineName(positional[0])
	if err != nil {
		return fail(err)
	}
	path := routinePath(name)
	existing, err := routine.Find(".", name)
	if err == nil {
		return fail(fmt.Errorf("routine %q already exists at %s", name, existing.Path))
	}
	// "Not found" is the only error that clears the way: a file that does not
	// load still claims its name, and creating over it would replace someone's
	// prompt with the template.
	if !errors.Is(err, routine.ErrNotFound) {
		return fail(fmt.Errorf("cannot create %q: %w", name, err))
	}
	if err := os.MkdirAll("routines", 0o755); err != nil {
		return fail(err)
	}
	content := fmt.Sprintf(newRoutineTemplate, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fail(err)
	}
	fmt.Printf("Created %s (inactive -- edit the schedule and prompt, then set active: true)\n", path)
	return 0
}

// routineFile resolves the file a named routine lives in, for the two
// commands that act on the file rather than on what it declares. A routine
// that does not load is exactly the one you need to edit or remove, so take
// the file the load error names -- unless the name is claimed by two files,
// where picking one would be a guess.
func routineFile(name string) (string, error) {
	r, err := routine.Find(".", name)
	if err == nil {
		return r.Path, nil
	}
	var re *routine.Error
	if errors.As(err, &re) && re.Path != "" {
		return re.Path, nil
	}
	return "", err
}

// routineName validates a CLI routine argument against the name grammar.
// Names become paths (routines/<name>.md, lock files, ledgers), so this
// runs before any path construction.
func routineName(arg string) (string, error) {
	name := strings.TrimSuffix(arg, ".md")
	if !routine.NamePattern.MatchString(name) {
		return "", fmt.Errorf("routine name %q must be lowercase alphanumerics with hyphens/underscores", name)
	}
	return name, nil
}

func routinePath(name string) string {
	return filepath.Join("routines", name+".md")
}

func routinesEdit(args []string) int {
	positional, _, help, err := parseFlags(args, nil)
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println("usage: openroutines routines edit <name>")
		return 0
	}
	if len(positional) != 1 {
		return fail(fmt.Errorf("usage: openroutines routines edit <name>"))
	}
	name, err := routineName(positional[0])
	if err != nil {
		return fail(err)
	}
	path, err := routineFile(name)
	if err != nil {
		return fail(err)
	}
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		return fail(fmt.Errorf("set $EDITOR (or $VISUAL) to use routines edit -- or just open %s", path))
	}
	cmd := exec.Command(editor, path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fail(err)
	}
	// A routine you just edited should still be valid.
	if _, err := routine.Parse(path); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s: %v\n", path, err)
		return 1
	}
	return 0
}

func routinesSetActive(args []string, active bool) int {
	verb := "activate"
	if !active {
		verb = "deactivate"
	}
	positional, _, help, err := parseFlags(args, nil)
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Printf("usage: openroutines routines %s <name>\n", verb)
		return 0
	}
	if len(positional) != 1 {
		return fail(fmt.Errorf("usage: openroutines routines %s <name>", verb))
	}
	name, err := routineName(positional[0])
	if err != nil {
		return fail(err)
	}
	r, err := routine.Find(".", name)
	if err != nil {
		return fail(err)
	}
	path := r.Path
	if err := routine.SetActive(path, active); err != nil {
		return fail(err)
	}
	fmt.Printf("%sd %s (commit the diff to make it stick in production)\n", verb, path)
	return 0
}

func routinesRemove(args []string) int {
	positional, _, help, err := parseFlags(args, nil)
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println("usage: openroutines routines remove <name>")
		return 0
	}
	if len(positional) != 1 {
		return fail(fmt.Errorf("usage: openroutines routines remove <name>"))
	}
	name, err := routineName(positional[0])
	if err != nil {
		return fail(err)
	}
	path, err := routineFile(name)
	if err != nil {
		return fail(err)
	}
	if err := os.Remove(path); err != nil {
		return fail(err)
	}
	fmt.Printf("Removed %s\n", path)
	// Best effort: clean up the routine's state on the memory branch --
	// scheduling state, trigger baseline, consumer cursor -- so the branch
	// doesn't accumulate orphans that misfire a later routine with the same
	// name. Its ledger stays -- that's memory.
	removed, _ := memory.At(".").RemoveRoutineState(name)
	for _, p := range removed {
		fmt.Printf("Removed %s (commit inside memory/ to record it)\n", p)
	}
	return 0
}

func routinesList() int {
	routines, errs := routine.LoadAgent(".")
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}
	if len(routines) == 0 {
		fmt.Println("No routines. Create one: openroutines routines new <name>")
		return 0
	}
	fmt.Printf("%-20s %-16s %-8s %s\n", "NAME", "SCHEDULE", "ACTIVE", "GRANTS")
	for _, r := range routines {
		fmt.Printf("%-20s %-16s %-8v %s\n", r.Name, scheduleSummary(r), r.FM.IsActive(), strings.Join(grantSummary(r), " "))
	}
	return 0
}

// grantSummary names every authority a routine declares. Web access and MCP
// servers are grants like skills and credentials -- a surface that answers
// "what can this routine reach" and omits them answers it wrong.
func grantSummary(r *routine.Routine) []string {
	var grants []string
	if n := len(r.FM.Skills); n > 0 {
		grants = append(grants, fmt.Sprintf("skills:%d", n))
	}
	if n := len(r.FM.Credentials); n > 0 {
		grants = append(grants, fmt.Sprintf("creds:%d", n))
	}
	if n := len(r.FM.MCP); n > 0 {
		grants = append(grants, fmt.Sprintf("mcp:%d", n))
	}
	if r.FM.Webfetch {
		grants = append(grants, "webfetch")
	}
	if r.FM.Websearch {
		grants = append(grants, "websearch")
	}
	return grants
}

// scheduleSummary describes when a routine runs: its cron schedule, its
// trigger, or both.
func scheduleSummary(r *routine.Routine) string {
	switch {
	case r.FM.Schedule != "" && r.FM.Trigger != nil:
		return r.FM.Schedule + " +trigger"
	case r.FM.Trigger != nil:
		return "trigger"
	default:
		return r.FM.Schedule
	}
}
