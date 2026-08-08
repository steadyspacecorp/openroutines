package cli

import (
	"fmt"

	"github.com/steadyspacecorp/openroutines/internal/knowledge"
)

// Reconciles the knowledge worktree with origin from a person's checkout.
// `git pull` on main moves origin/knowledge without touching the worktree,
// leaving status, usage, and the ledgers reading old knowledge.
//
// Deliberately not run by status or usage: this fetches, may rebase, and
// force-pushes the accepted-tip baseline to origin. A command that reports
// state must not do any of that.
const syncUsage = "usage: openroutines sync [--push]"

func cmdSync(args []string) int {
	positional, flags, help, err := parseFlags(args, map[string]flagSpec{"--push": {}})
	if err != nil {
		return fail(err)
	}
	if help {
		fmt.Println(syncUsage)
		return 0
	}
	if len(positional) != 0 {
		return fail(fmt.Errorf("%s", syncUsage))
	}
	_, push := flags["--push"]

	// A fresh clone has no knowledge worktree at all -- the exact checkout most
	// likely to be reaching for sync. Ensure materializes it the way the
	// supervisor does at boot, refusing a tip that doesn't descend from the
	// accepted baseline.
	mem := knowledge.At(".")
	materialized := !mem.Status().Materialized
	if err := mem.Ensure(); err != nil {
		return fail(err)
	}
	if materialized {
		fmt.Printf("materialized knowledge/ from the %s branch\n", knowledge.Branch)
	}

	behind := mem.Status().Behind // counted before the sync, while still true

	rep := mem.Sync()
	switch {
	case rep.NoOrigin:
		fmt.Println("no origin -- knowledge is local only, nothing to reconcile")
		return 0
	case rep.RemoteMissing:
		fmt.Printf("origin has no %s branch yet -- it is created the first time knowledge is pushed\n", knowledge.Branch)
		return 0
	case rep.Unreachable:
		return fail(fmt.Errorf("origin unreachable: %s", rep.Detail))
	case rep.Rewritten: // the refusal text already explains the repair
		reportStranded(mem)
		return fail(fmt.Errorf("%s", rep.Detail))
	case rep.Conflict:
		reportStranded(mem)
		return fail(fmt.Errorf("knowledge does not reconcile cleanly: %s\n\nresolve inside knowledge/, commit, then rerun openroutines sync", rep.Detail))
	case rep.Detail != "":
		return fail(fmt.Errorf("%s", rep.Detail))
	}

	if rep.Adopted {
		if behind > 0 {
			fmt.Printf("adopted %d commit(s) from origin/%s\n", behind, knowledge.Branch)
		} else {
			fmt.Printf("reconciled with origin/%s\n", knowledge.Branch)
		}
	} else {
		fmt.Printf("knowledge is up to date with origin/%s\n", knowledge.Branch)
	}

	st := mem.Status()
	if st.Uncommitted > 0 {
		fmt.Printf("  ! %d file(s) with uncommitted changes in knowledge/ -- commit them before they can be published\n", st.Uncommitted)
	}
	if st.Unpushed == 0 {
		return 0
	}
	if !push {
		fmt.Printf("  %d local commit(s) not on origin -- openroutines sync --push to publish\n", st.Unpushed)
		return 0
	}
	if err := mem.Push(); err != nil {
		return fail(fmt.Errorf("publishing knowledge: %w", err))
	}
	fmt.Printf("  published %d commit(s) to origin/%s\n", st.Unpushed, knowledge.Branch)
	return 0
}

// Names the knowledge a supervisor could not put on the branch while its
// sync was blocked -- typically carrying the blocker task that explains the
// refusal, since the block that stops sync also stops the runs that would
// otherwise report it.
func reportStranded(mem *knowledge.Knowledge) {
	snap := mem.Blocked()
	if snap.Tip == "" {
		return
	}
	fmt.Printf("  ! the agent stranded knowledge it could not publish on %s (snapshot taken %s)\n", knowledge.BlockedRef, snap.When)
	fmt.Printf("    read it: git -C knowledge show %s:tasks.md -- compare: git -C knowledge diff %s %s\n",
		knowledge.BlockedRef, knowledge.Branch, knowledge.BlockedRef)
}
