package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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
	sub, rest := args[0], args[1:]
	switch sub {
	case "new":
		return routinesNew(rest)
	case "list":
		return routinesList()
	case "run":
		return routinesRun(rest, true)
	case "test":
		return routinesRun(rest, false)
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
  openroutines routines list               names, ids, schedules, grants
  openroutines routines run <name>         run once now; memory writes are kept
  openroutines routines test <name>        run once now; memory writes are discarded
  openroutines routines edit <name>        open in $EDITOR, validate on close
  openroutines routines activate <name>    set active: true
  openroutines routines deactivate <name>  set active: false
  openroutines routines remove <name>      delete the routine and its scheduling state
`

func routinesRun(args []string, keep bool) int {
	if len(args) != 1 {
		verb := "run"
		if !keep {
			verb = "test"
		}
		return fail(fmt.Errorf("usage: openroutines routines %s <name>", verb))
	}
	name := strings.TrimSuffix(args[0], ".md")
	res, err := runner.Run(".", name, keep)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("\n%s: %s in %s (run %s)\n", name, res.Outcome, res.Duration, res.RunID)
	if !keep {
		fmt.Println("test mode: memory writes discarded, nothing recorded")
	} else if res.Commit != "" {
		fmt.Printf("memory updated: commit %s on the %s branch\n", res.Commit, "memory")
	}
	if res.Outcome != runner.Completed {
		return 1
	}
	return 0
}

func routinesNew(args []string) int {
	if len(args) != 1 {
		return fail(fmt.Errorf("usage: openroutines routines new <name>"))
	}
	name := strings.TrimSuffix(args[0], ".md")
	path := filepath.Join("routines", name+".md")
	if _, err := os.Stat(path); err == nil {
		return fail(fmt.Errorf("%s already exists", path))
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

func routinePath(arg string) string {
	return filepath.Join("routines", strings.TrimSuffix(arg, ".md")+".md")
}

func routinesEdit(args []string) int {
	if len(args) != 1 {
		return fail(fmt.Errorf("usage: openroutines routines edit <name>"))
	}
	path := routinePath(args[0])
	if _, err := os.Stat(path); err != nil {
		return fail(fmt.Errorf("no routine %q", args[0]))
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
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		return 1
	}
	return 0
}

func routinesSetActive(args []string, active bool) int {
	verb := "activate"
	if !active {
		verb = "deactivate"
	}
	if len(args) != 1 {
		return fail(fmt.Errorf("usage: openroutines routines %s <name>", verb))
	}
	path := routinePath(args[0])
	if _, err := os.Stat(path); err != nil {
		return fail(fmt.Errorf("no routine %q", args[0]))
	}
	if err := routine.SetActive(path, active); err != nil {
		return fail(err)
	}
	fmt.Printf("%sd %s (commit the diff to make it stick in production)\n", verb, path)
	return 0
}

func routinesRemove(args []string) int {
	if len(args) != 1 {
		return fail(fmt.Errorf("usage: openroutines routines remove <name>"))
	}
	path := routinePath(args[0])
	r, err := routine.Parse(path)
	if err != nil {
		return fail(fmt.Errorf("no routine %q: %v", args[0], err))
	}
	if err := os.Remove(path); err != nil {
		return fail(err)
	}
	fmt.Printf("Removed %s\n", path)
	// Best effort: clean up the routine's scheduling state so the memory
	// branch doesn't accumulate orphans. Its ledger stays -- that's memory.
	statePath := filepath.Join("memory", "state", r.Name+".json")
	if err := os.Remove(statePath); err == nil {
		fmt.Printf("Removed scheduling state %s (commit inside memory/ to record it)\n", statePath)
	}
	return 0
}

func routinesList() int {
	routines, errs := routine.LoadDir("routines")
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}
	if len(routines) == 0 {
		fmt.Println("No routines. Create one: openroutines routines new <name>")
		return 0
	}
	fmt.Printf("%-20s %-16s %-8s %s\n", "NAME", "SCHEDULE", "ACTIVE", "GRANTS")
	for _, r := range routines {
		grants := []string{}
		if len(r.FM.Skills) > 0 {
			grants = append(grants, fmt.Sprintf("skills:%d", len(r.FM.Skills)))
		}
		if len(r.FM.Credentials) > 0 {
			grants = append(grants, fmt.Sprintf("creds:%d", len(r.FM.Credentials)))
		}
		fmt.Printf("%-20s %-16s %-8v %s\n", r.Name, r.FM.Schedule, r.FM.IsActive(), strings.Join(grants, " "))
	}
	return 0
}
