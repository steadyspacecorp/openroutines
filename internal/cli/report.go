package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/steadyspacecorp/openroutines/internal/memory"
	"github.com/steadyspacecorp/openroutines/internal/routine"
)

const reportUsage = "usage: openroutines report"

// checkInRoutine is the reporting routine whose ledger report shows: the
// template's default delivery consumer (design decision "Every agent
// checks in").
const checkInRoutine = "check-in"

// cmdReport syncs memory and shows the latest check-in. The check-in
// routine's delivery step records its composed report in its own ledger --
// the one place on the memory branch a report can live without re-entering
// the change feed -- so showing it is a read, never a run.
func cmdReport(args []string) int {
	positional, _, help, err := parseFlags(args, nil)
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println(reportUsage)
		return 0
	}
	if len(positional) != 0 {
		return fail(errors.New(reportUsage))
	}

	mem := memory.At(".")
	if err := mem.Ensure(); err != nil {
		return fail(err)
	}
	message, err := syncMemoryForRead(mem)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("%s\n\n", message)

	// A stored report only refreshes while a healthy check-in routine keeps
	// delivering; a stale one shown without comment reads as current.
	health := checkInHealth()

	ledger := filepath.Join(mem.Worktree(), "ledgers", checkInRoutine+".md")
	raw, err := os.ReadFile(ledger)
	if errors.Is(err, fs.ErrNotExist) {
		if health != "" {
			fmt.Printf("No check-in recorded, and %s.\n", health)
			return 0
		}
		fmt.Printf("No check-in recorded yet -- the %s routine stores its latest report in memory/ledgers/%s.md when it runs.\nFor a fresh one now: openroutines routines run %s --no-memory\n", checkInRoutine, checkInRoutine, checkInRoutine)
		return 0
	}
	if err != nil {
		return fail(err)
	}
	if health != "" {
		fmt.Printf("note: %s -- the report below will not refresh\n\n", health)
	}
	fmt.Println(strings.TrimSpace(string(raw)))
	return 0
}

// checkInHealth reports why no new report will be recorded, "" when the
// check-in routine is present, loads, and is active.
func checkInHealth() string {
	r, err := routine.Find(".", checkInRoutine)
	switch {
	case errors.Is(err, routine.ErrNotFound):
		return fmt.Sprintf("this agent has no %s routine", checkInRoutine)
	case err != nil:
		return fmt.Sprintf("the %s routine does not load (%v)", checkInRoutine, err)
	case !r.FM.IsActive():
		return fmt.Sprintf("the %s routine is inactive (openroutines routines activate %s)", checkInRoutine, checkInRoutine)
	}
	return ""
}

func syncMemoryForRead(mem *memory.Memory) (string, error) {
	rep := mem.Sync()
	switch {
	case rep.NoOrigin:
		return "Memory is local only (no origin)", nil
	case rep.RemoteMissing:
		return fmt.Sprintf("Memory is local only (origin has no %s branch yet)", memory.Branch), nil
	case rep.Unreachable:
		return "", fmt.Errorf("origin unreachable: %s", rep.Detail)
	case rep.Rewritten:
		reportStranded(mem)
		return "", errors.New(rep.Detail)
	case rep.Conflict:
		reportStranded(mem)
		return "", fmt.Errorf("memory does not reconcile cleanly: %s\n\nresolve inside memory/, commit, then rerun openroutines report", rep.Detail)
	case rep.Detail != "":
		return "", errors.New(rep.Detail)
	default:
		return fmt.Sprintf("Memory is current with origin/%s", memory.Branch), nil
	}
}
