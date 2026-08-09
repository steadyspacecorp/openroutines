package runner

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/lock"
	"github.com/steadyspacecorp/openroutines/internal/mode"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/run"
)

// The fixture document injected into a rehearsal run's
// workspace.
const RehearsalFileName = "rehearsal.md"

const fixturePreamble = `REHEARSAL RUN, fixture world. The fixtures in ./rehearsal.md replace
every outside read for this run -- including ./changes.md and
./schedule.md wherever the fixtures provide stand-ins. You have no
credentials, no MCP servers, no skills, and no web access; do not
attempt external calls, the fixtures are the world. Nothing you produce
leaves the run: knowledge writes are discarded. Follow the routine
below exactly, against the fixtures.

`

// Governs a rehearsal with no fixtures: grants stay so reads
// work, the read-only restraint is asked of the model rather than enforced --
// the enforced part is that nothing settles.
const livePreamble = `REHEARSAL RUN, live world. Read anything this routine normally reads --
your credentials and tools are present -- but treat every external
action as read-only and idempotent: write nothing, post nothing, change
no state in any outside system. Anything the routine would deliver to a
destination, print here instead; printed output is this rehearsal's
delivery. Knowledge writes are discarded and nothing is consumed.
Follow the routine below exactly, under these restraints.

`

// Executes routine name manually. Rehearsals always discard knowledge.
// In the production container a manual run reserves the manual attempt
// identity, so it can never share a uid with a supervisor slot.
func RunManual(dir, name string, options ManualOptions) (result *ManualResult, err error) {
	attempt := Attempt{RunID: run.NewID(), Number: 1, Rehearsal: options.Fixture}
	if mode.Current() == mode.DeployedContainer {
		uid, releaseIdentity, err := reserveManualIdentity(dir)
		if err != nil {
			return nil, err
		}
		defer releaseIdentity()
		attempt.AttemptUID = uid
	}
	agent, err := config.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("not an agent repository: %w", err)
	}
	r, err := routine.Find(dir, name)
	if err != nil {
		return nil, err
	}
	if options.Rehearse {
		rr := *r
		if options.Fixture != "" {
			// Grants are stripped at the source so the existing pipeline
			// enforces the absence.
			rr.Frontmatter.Credentials = nil
			rr.Frontmatter.MCP = nil
			rr.Frontmatter.Skills = nil
			rr.Frontmatter.Webfetch = false
			rr.Frontmatter.Websearch = false
			rr.Body = fixturePreamble + r.Body
		} else {
			rr.Body = livePreamble + r.Body
		}
		r = &rr
		options.DiscardKnowledge = true
	}
	// One attempt per routine at a time, held snapshot through settlement --
	// the same lock the supervisor takes, so a manual run cannot double the
	// supervisor's own run of this routine.
	release, err := lock.Take(dir, name)
	if errors.Is(err, lock.ErrLocked) {
		return nil, fmt.Errorf("routine %s already has an attempt in flight (the supervisor or another terminal holds its lock) -- skipped", name)
	}
	if err != nil {
		return nil, err
	}
	defer release()
	// A supervisor may be settling runs into the same worktree beside this
	// process; snapshot and settlement take turns behind its lock.
	knowledgeLock, err := lock.Locker(dir, "knowledge")
	if err != nil {
		return nil, err
	}
	prepared, err := Stage(dir, agent, r, attempt, knowledgeLock)
	if err != nil {
		return nil, err
	}
	// Echo the run's scrubbed output to the terminal as it streams.
	prepared.echo = os.Stdout
	attemptResult, workspace, err := prepared.Run(context.Background())
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, workspace.Cleanup()) }()

	result = &ManualResult{RunID: attempt.RunID, Outcome: attemptResult.Outcome, ExitCode: attemptResult.ExitCode, Duration: attemptResult.Duration, Hint: attemptResult.Hint, SessionsDir: attemptResult.SessionsDir}
	if options.DiscardKnowledge {
		return result, nil
	}

	knowledgeLock.Lock()
	defer knowledgeLock.Unlock()
	settlement, err := Settle(dir, r, workspace, attemptResult, attempt, "", nil)
	result.Outcome = settlement.Outcome
	result.Commit = settlement.Commit
	result.Conflicts = settlement.Conflicts
	return result, err
}
