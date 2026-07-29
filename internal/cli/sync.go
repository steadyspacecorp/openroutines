package cli

import (
	"fmt"
	"os"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/memory"
)

// cmdSync reconciles the memory worktree with origin from a person's
// checkout. The supervisor already syncs memory every tick on the
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

	// Ensure mints a memory branch when none exists, so make sure this is
	// an agent repository before touching anything.
	if _, err := os.Stat(config.Path(".")); err != nil {
		return fail(fmt.Errorf("run sync from inside an agent repository"))
	}

	// A fresh clone has no memory worktree at all -- the exact checkout most
	// likely to be reaching for sync. Materialize it the way the supervisor
	// does at boot: Ensure adopts the branch from origin and refuses a tip
	// that does not descend from the accepted baseline.
	mem := memory.At(".")
	materialized := !mem.Status().Materialized
	if err := mem.Ensure(); err != nil {
		return fail(err)
	}
	if materialized {
		fmt.Printf("materialized memory/ from the %s branch\n", memory.Branch)
	}

	// Counted before the sync, while it is still true.
	behind := mem.Status().Behind

	rep := mem.Sync()
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
		reportStranded(mem)
		return fail(fmt.Errorf("%s", rep.Detail))
	case rep.Conflict:
		reportStranded(mem)
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

	st := mem.Status()
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
	if err := mem.Push(); err != nil {
		return fail(fmt.Errorf("publishing memory: %w", err))
	}
	fmt.Printf("  published %d commit(s) to origin/%s\n", st.Unpushed, memory.Branch)
	return 0
}

// reportStranded names the memory a supervisor could not put on the branch
// while its sync was blocked -- typically carrying the blocker task that
// explains this same refusal, since the block that stops sync also stops the
// runs that would otherwise report it. Dated, because a snapshot from a
// container that has since been replaced is a record to read, not a repair in
// progress.
func reportStranded(mem *memory.Memory) {
	snap := mem.Blocked()
	if snap.Tip == "" {
		return
	}
	fmt.Printf("  ! the agent stranded memory it could not publish on %s (snapshot taken %s)\n", memory.BlockedRef, snap.When)
	fmt.Printf("    read it: git -C memory show %s:tasks.md -- compare: git -C memory diff %s %s\n",
		memory.BlockedRef, memory.Branch, memory.BlockedRef)
}
