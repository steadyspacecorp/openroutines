package cli

import (
	"fmt"

	"github.com/steadyspacecorp/openroutines/internal/memory"
)

// cmdSync reconciles the memory worktree with origin from a person's
// checkout. The supervisor already runs memory.Sync every tick on the
// deployed side; locally there was no way to reach it, so `git pull` on
// main -- which moves origin/memory without touching the worktree -- left
// status, usage, and the ledgers reading old memory with no way to fix it
// short of raw git against a worktree most people never think about.
//
// Deliberately not run by status or usage: Sync fetches, may rebase, and
// force-pushes the accepted-tip baseline to origin. A command that reports
// state must not do any of that.
func cmdSync(args []string) int {
	push := false
	for _, a := range args {
		if a == "--push" {
			push = true
			continue
		}
		return fail(fmt.Errorf("usage: openroutines sync [--push]"))
	}

	// Counted before the sync, while it is still true.
	behind := memory.Status(".").Behind

	rep := memory.Sync(".")
	switch {
	case rep.NoOrigin:
		fmt.Println("no origin -- memory is local only, nothing to reconcile")
		return 0
	case rep.RemoteMissing:
		fmt.Printf("origin has no %s branch yet -- it is created the first time memory is pushed\n", memory.Branch)
		return 0
	case rep.Unreachable:
		return fail(fmt.Errorf("origin unreachable: %s", rep.Detail))
	case rep.Rewritten:
		// The refusal text already explains the repair; do not paraphrase it.
		return fail(fmt.Errorf("%s", rep.Detail))
	case rep.Conflict:
		return fail(fmt.Errorf("memory does not reconcile cleanly: %s\n\nresolve inside memory/, commit, then rerun openroutines sync", rep.Detail))
	case rep.Detail != "":
		return fail(fmt.Errorf("%s", rep.Detail))
	}

	if rep.Adopted {
		if behind > 0 {
			fmt.Printf("adopted %d commit(s) from origin/%s\n", behind, memory.Branch)
		} else {
			fmt.Printf("reconciled with origin/%s\n", memory.Branch)
		}
	} else {
		fmt.Printf("memory is up to date with origin/%s\n", memory.Branch)
	}

	st := memory.Status(".")
	if st.Uncommitted > 0 {
		fmt.Printf("  ! %d file(s) with uncommitted changes in memory/ -- commit them before they can be published\n", st.Uncommitted)
	}
	if st.Unpushed == 0 {
		return 0
	}
	if !push {
		fmt.Printf("  %d local commit(s) not on origin -- openroutines sync --push to publish\n", st.Unpushed)
		return 0
	}
	if err := memory.Push("."); err != nil {
		return fail(fmt.Errorf("publishing memory: %w", err))
	}
	fmt.Printf("  published %d commit(s) to origin/%s\n", st.Unpushed, memory.Branch)
	return 0
}
