# Scheduling and triggers

## Scheduling: a tick loop with catch-up, not cron

**Decision.** The supervisor wakes every minute, re-reads routine frontmatter, and dispatches whatever is due.
Per-routine scheduling state is durable: a watermark records the latest fully accounted-for cron occurrence, and at most one pending run records work that still needs an attempt.

If a pending run exists, the tick retries it under the same `run_id`; otherwise, missed cron occurrences in `(watermark, now]` collapse into one logical run with the earliest `scheduled_for` and latest `covered_through`.
The pending record is committed and pushed before execution.
Each attempt is reserved, committed, and pushed immediately before spawning, so no model process starts without durable attempt accounting; reservations that never spawn are given back.
Success advances the watermark and clears pending; failure preserves pending for a bounded retry budget, after which the run becomes a human-owned abandonment task.
Three consecutive abandonments trip a per-routine cool-down with exponential backoff, and a successful run resets it.
If origin cannot be reached, new logical runs do not start; the condition is recorded durably and catch-up absorbs the pause.

Status shares the scheduler's schedulability predicate and reports held, backing-off, in-flight, due, or budget-spent states instead of inventing a next-fire time.
Every attempt receives the stable `OPENROUTINES_RUN_ID` plus its attempt id and schedule window metadata.

Cron is evaluated in the configured agent timezone.
The schedule parser binds each expression to that location so persisted timestamps cannot freeze the schedule at an old UTC offset across daylight-saving transitions.
The same bound schedule drives dispatch, status, and the forward schedule injected into runs.

**Why.** Re-reading frontmatter keeps files as the schedule and gives local edits immediate effect.
Catch-up and at-least-once execution make downtime recoverable, while the durable run id gives routines an idempotency key for external side effects.

Persist-before-act is a data invariant, not just control flow: if intent or attempt accounting cannot be committed and pushed, dispatch stops.
Per-attempt reservation prevents a lost container from escaping retry limits and prevents queued routines from being charged for attempts they never made.
## Triggers: outbound wake-ups, not ingress and not deliveries

**Decision.** A routine may declare an outbound change-detection poll alongside or instead of a schedule.
The poll has a fixed URL, an optional declared credential, an interval, and either conditional-request validators or a bounded JSON-pointer `select`; without `select`, the response body is reduced to a digest.
A trigger carries no payload: it only wakes the routine, which obtains its real work through its declared skills.
Triggers reduce latency; the schedule remains the correctness backstop.

The first observation establishes a baseline and does not fire.
Later changes mint ordinary logical runs through the existing pending, reservation, retry, abandonment, and circuit-breaker pipeline.
Trigger state stores the last value and validators in supervisor-owned scheduling state; the poll clock is not persisted in the knowledge worktree.
A cron run refreshes the baseline so the same change does not cause a redundant trigger run.

**Why.** Polling avoids both wasteful model runs and a standing inbound service.
A webhook receiver would add public ingress, TLS and signature handling, a durable untrusted payload path, and deployment prerequisites while polling reconciliation would still be required.
The trusted supervisor therefore performs only bounded, non-redirecting GETs, never logs or exposes the response, and derives typed bearer credentials only for the request.
## Overlap: kernel locks, skip-don't-queue

**Decision.** Runs are concurrent across routines, bounded, and opted into: the tick loop marks routines due and launches them, in due order, into a fixed pool of run slots (`concurrency` in `openroutines.yml`; unset and 0 mean strictly serial, the maximum is 32, and the scaffold template opts newly created agents in at 2).
A full pool skips to the next tick rather than queueing -- the pending record *is* the queue.
A routine never overlaps itself: one with an attempt in flight is left out of the tick's planning entirely (its settlement owns that scheduling state), and later firings collapse into `covered_through`.
The dispatcher takes the non-blocking per-routine `flock` before consuming a run slot; a held lock means skip and continue offering unrelated routines during the same tick.
Every knowledge-worktree operation -- the tick's bookkeeping, each attempt's reservation and staging, each settlement -- takes its turn behind one lock: an in-process mutex backed by an `flock` on `.openroutines-tmp/locks/knowledge.lock`, so a manual `routines run` settling beside the supervisor joins the same queue instead of becoming a second writer.
Concurrent runs snapshot the same knowledge and settle at different times, so import compares each staged file with that base and the current worktree: a file the run left untouched imports nothing (a stale copy must never regress what settled since), a file only the run changed copies in whole, and changes on both sides compose only when both retain the complete base as a prefix and append after it.
Any other concurrent edit leaves the canonical file untouched, saves the competing version under `state/conflicts/`, and creates a human-owned task naming both paths; a deletion applies only where the worktree still matches the base.

**Why.** Hand-rolled lock files go stale when a process dies uncleanly, silently deadlocking the routine forever -- the worst failure mode for unattended software.
`flock` locks are released by the kernel when the holder dies, however it dies; staleness is structurally impossible.
Skipping rather than queueing keeps semantics simple: a routine that is still running *is* the current run, and a routine waiting for a slot is just a pending run the next tick offers again.
An earlier version of this decision ran runs serially across routines, deriving single-writer-per-file from one-run-at-a-time; that held while runs were capped at minutes, and stopped holding when the cap moved into user space -- a six-hour run that blocks every scheduled routine behind it is not a scheduler.
What serial execution actually bought was one thing: a single writer to the supervisor's worktree.
Everything else was already parallel-safe by construction -- each run gets its own container, workspace, staged knowledge copy, and constructed environment -- and the worktree keeps its single writer through a mutex held only for bookkeeping.
Single-writer *per file* is provided where it was always really needed, at the import: the three-way merge makes concurrent settlements into the same append stream compose instead of the later import silently reverting the earlier one.
The pool is bounded, small, and configurable because each concurrent run is a container plus live model spend, and runs granted the same credential share its provider rate limits: parallelism buys "a long run doesn't block the agent", not a job farm.

## Timeouts kill the process group

**Decision.** Every run has a timeout (`openroutines.yml` default, frontmatter override).
On expiry the supervisor signals the routine's entire process group -- SIGTERM, a grace period, then SIGKILL -- and records the outcome.
Timeouts have a ceiling, and the ceiling belongs to the operator: `max_timeout` in `openroutines.yml`, 6 hours when unset.
The ceiling is applied where attempts read the timeout, so a larger declared value is silently capped rather than honored; `openroutines check` warns above it so the capping is visible before deploy rather than as a routine mysteriously killed in production.

A run that ends *well* gets the same treatment, minus the grace period: when the model process exits on its own, the supervisor signals the group before the pipeline reads staged knowledge.
(Under `docker run --rm` locally the run's pid namespace already dies with the container; the kill is what production, where the model process is a child of the supervisor in the supervisor's own container, does not otherwise get.)

**Why.** A routine run spawns children (tools, shells); signalling only the direct child leaks grandchildren.
Recording outcomes (`completed` / `timeout` / `crashed`, duration) into knowledge gives run history that survives redeploys and reads as a git log.
The ceiling used to be a correctness bound -- 15 minutes, half the lease TTL, because the lease was only heartbeated between runs -- and that coupling is gone: the lease is renewed *during* runs, so the ceiling's remaining job is protecting the operator's wallet from a run that never ends.
A spend guard belongs in user space, defaulted rather than hardcoded: nobody shopping for an agent runner expects a system that cannot run longer than 15 minutes, but everybody appreciates not discovering a 48-hour run on an invoice.
It is enforced rather than merely advised because a warning is only as good as the operator's habit of running `check`.
Reaping on a clean exit is not tidiness: a descendant that redirected its stdio outlives the process the supervisor waited on, and it can still write to staged knowledge during the window where the supervisor validates and imports it -- the tree being imported would have an author nobody accounted for.
It is one of two defences: a descendant that leaves the process group altogether survives every signal the supervisor can aim, which is why "Every wait is bounded" bounds that case by abandoning the pipe, and why the import re-checks staging at open time.

## Every wait is bounded

**Decision.** No wait the supervisor makes is open-ended, and every deadline ends with the process it is waiting on gone rather than with the supervisor walking away from it.
Git invocations carry a two-minute deadline, and on expiry the whole git process group is signaled the way a run's is -- SIGTERM, a short grace, SIGKILL -- because git does the network through children (ssh, git-remote-https), so killing git alone would leave the stalled transport holding the pipe the supervisor is reading, and because git removes the lock files it holds on SIGTERM and cannot on SIGKILL.
Underneath that backstop sit the transport's own bounds: `http.lowSpeedLimit` / `http.lowSpeedTime` on the HTTP side, `ConnectTimeout` and server keepalives on the SSH side, which abandon a transfer that goes quiet for about a minute and produce a real git error.
A timed-out invocation is an ordinary git failure to everything above it: the tick reports the origin unreachable and carries on.
The run side is bounded the same way -- after a run's process group is signaled and killed, the supervisor stops draining the run's output pipes five seconds later rather than waiting for EOF, and reaps what can be reaped.
Local runs, where the model process is a container rather than a sandboxed child, get the equivalent: the `docker stop` call is itself bounded, and a client that does not follow its container out is killed.

**Why.** A blackholed origin -- a partition that drops packets rather than refusing them -- is the routine cloud failure, and it defeats every bound that depends on bytes moving: the connect never completes, so the no-progress watchdogs never arm, and the wait lasts as long as the TCP stack's retry schedule.
A tick makes several network calls, and one unbounded call parks the entire loop silently: no runs, no failure, nothing in the logs, and a lease going stale underneath a supervisor that is very much alive -- exactly the state that invites a second instance to take over.
The pipe drain is the same failure in a different guise: `Wait` cannot return while *anything* holds the inherited stdout, so a tool that daemonized into its own session survives the process-group kill and hangs the supervisor permanently on a run it has already terminated.
Bounding it costs the tail of that run's log and leaves an orphan the group kill could not reach -- both recoverable.
Two minutes is generous for git against a healthy origin (knowledge branches are small text files) and far below the 30-minute lease TTL; five seconds is long enough for a real flush and short enough to be invisible.
What a deadline may *not* do is abandon a wait while the process is still writing: the run's log tail is read after the wait returns, so the bound has to come from ending the process.
Nor is a bound that fires automatically a clean outcome -- a git invocation whose output was truncated stays a failure, because the callers parse that output and half a SHA is worse than an error.
The deadline is per invocation, not per tick: a tick against a blackholed origin pays it once per network call, so ticks fall behind while the partition lasts.
That is the accepted cost -- the schedule is durable and catches up, staleness stays inside the lease TTL, and a per-tick budget would mean deciding mid-tick to skip work at-least-once has already promised.
