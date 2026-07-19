package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/runner"
)

const newRoutineTemplate = `---
id: %s
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
		return fail(fmt.Errorf("usage: openroutines routines <new|list|run|test|edit|activate|deactivate|remove> [name]"))
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
	case "edit", "activate", "deactivate", "remove":
		return notYet("routines " + sub)
	default:
		return fail(fmt.Errorf("unknown routines command %q", sub))
	}
}

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
	content := fmt.Sprintf(newRoutineTemplate, routine.NewID(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fail(err)
	}
	fmt.Printf("Created %s (inactive -- edit the schedule and prompt, then set active: true)\n", path)
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
	fmt.Printf("%-20s %-12s %-16s %-8s %s\n", "NAME", "ID", "SCHEDULE", "ACTIVE", "GRANTS")
	for _, r := range routines {
		grants := []string{}
		if len(r.FM.Skills) > 0 {
			grants = append(grants, fmt.Sprintf("skills:%d", len(r.FM.Skills)))
		}
		if len(r.FM.Credentials) > 0 {
			grants = append(grants, fmt.Sprintf("creds:%d", len(r.FM.Credentials)))
		}
		fmt.Printf("%-20s %-12s %-16s %-8v %s\n", r.Name, r.FM.ID, r.FM.Schedule, r.FM.IsActive(), strings.Join(grants, " "))
	}
	return 0
}
