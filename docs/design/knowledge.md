# Knowledge

## Knowledge: a dedicated directory on its own branch

**Decision.** Agent knowledge lives in a dedicated directory, synced to an orphan `knowledge` branch that the running agent -- the branch's sole writer -- pushes with a read/write deploy key.
Scheduling state and run records live there too.
If the branch doesn't exist at boot, the supervisor creates and pushes it -- first boot self-heals; there is no setup step.
All of this presumes a git origin: deploying an ORA requires one (any git host -- GitHub, GitLab, Gitea, a bare repo on a VPS), since that's the only durable home for knowledge.
Local development needs no origin; `openroutines check` verifies one exists before you deploy.
Which protocol that origin speaks is not the operator's problem: the deploy key is an SSH credential, so a supervisor holding one rewrites an HTTPS origin to SSH on its own git invocations, leaving `.git/config` and the built image as written.
Without that, the common case -- `new` adds no origin and `gh` defaults to https -- deploys an agent with no usable credential for its own repository, and the container restart-loops before it supervises anything.

**Why.** The analogy is Docker's own: `main` is the image (immutable, what you deploy), the `knowledge` branch is the volume (mutable, survives redeploys).
Keeping knowledge off `main` means agent commits never trigger CI/CD, never race human pushes, and never pollute the history of human intent.
One agent, one runtime → one writer → pushes fast-forward in the normal case; the rare exceptions (human curation racing a run) are handled by the defensive sync rules below, never silently.
Code rolls back; knowledge persists -- like a database, and reviewing what your agent has learned is `git log knowledge`.
Humans may curate the branch (pruning bad learnings is part of maintaining an agent); the agent pulls before each run.
By convention -- stated in the standing instruction injected into every run -- each routine keeps a ledger file named after itself (`knowledge/ledgers/<routine>.md`) recording what it examined and decided, and prunes that ledger as part of its own prompt: knowledge hygiene is part of the job description, not a framework feature.

Mechanically, `knowledge/` is a **git worktree** of the `knowledge` branch, ignored by `main` and created lazily by the CLI (locally) or the supervisor (in the container) -- one directory, two histories.
Routine edits and knowledge curation are separate commits on separate branches by construction: `git status` on `main` never shows knowledge churn, and a human curates by committing inside the directory (`git -C knowledge commit && git -C knowledge push`).
Local runs and production runs touch knowledge through the identical path, which is what makes local testing faithful.
`openroutines status` surfaces uncommitted knowledge-worktree changes, since root `git status` won't.
At run time the worktree is supervisor-only: routines work against a disposable staged copy, so a run can never clobber uncommitted human curation -- `routines run --write-knowledge` imports results into the worktree and asks only that it be clean at import time, while the default manual run discards the staged copy entirely.

Because humans can push to the branch and the agent reads it as model context, **knowledge is an untrusted input channel** and is handled that way.
The injected standing instruction frames the primitives as *records to consult*, never instructions to obey.
Sync is defensive: the supervisor pulls fast-forward-only and refuses to adopt rewritten history when the last accepted tip remains available and trustworthy (a force-pushed `knowledge` branch stops sync -- and dispatch -- rather than silently feeding the agent altered context); pushes are fast-forward-only, and a rejected push triggers fetch-and-rebase of the local commits -- append-only files rebase cleanly -- with conflicts stopping sync, never resolving silently.
The last accepted tip is recorded as a ref on origin (`refs/openroutines/accepted`), so refusal survives repeated sync cycles and container replacement while that ref remains intact.
This is defensive rewrite detection, not an integrity guarantee against an administrator or deploy-key holder able to alter both refs; accepting a deliberate rewrite is an explicit human act -- move the accepted ref to the new tip.
And "one writer" is enforced for deployed supervisors, not assumed: at boot the supervisor takes a lease (a heartbeat ref pushed to origin) and renews it before every run it dispatches, and an instance that sees a live foreign lease -- a rolling deploy's overlap, an accidental second replica -- waits or exits instead of running routines.
Release is ownership-checked (compare-and-swap, like renewal), so a stale instance shutting down cannot delete the new holder's live lease.

## The lease is renewed per run, not per tick

**Decision.** The single-instance lease is heartbeated before every run the supervisor dispatches, not once per tick -- and *during* every run, on a quarter-TTL cadence, from a goroutine that lives only while the supervisor is blocked waiting on the model process and is joined before settlement touches the worktree.
The TTL -- 30 minutes -- bounds takeover latency, not run length: a run may last hours (up to the `max_timeout` ceiling) without the lease going stale underneath it.
A heartbeat younger than a quarter of the TTL is left alone, so a tick of quick routines does not push once per routine; worst-case staleness stays half a TTL no matter what is running.
The tick heartbeats before it acts on the sync verdict, because a sync-blocked instance is alive and holding the lease even though it is not dispatching.
A renewal that fails, or that finds a live foreign lease, stops dispatch for the rest of the tick and the next tick re-evaluates from scratch; the pause is logged on the transition, not once per tick, and unlike a sync blocker it records nothing in knowledge -- an instance that cannot prove it is the writer must not write.
Mid-run the same principle cancels the run itself: a renewal failure inside the TTL of the last accepted heartbeat is tolerated (an origin blip passes, and until the TTL expires this instance is still provably the only writer), but past the TTL, or the moment a live foreign lease appears, the in-flight attempt is canceled -- it settles as interrupted, the attempt is handed back, and whoever holds the lease retries it.
The heartbeat carries wall-clock time, because staleness is judged against a real clock, not the tick's.

**Why.** A serial tick's wall time was unbounded, so a heartbeat that only fired at the top of the tick was stale for as long as the work took.
Under the original 5-minute TTL, one ordinary 10-minute run left the lease expired for twice its TTL -- so an instance booting in that window (the rolling-deploy overlap the lease exists for) read an expired lease, judged itself eligible, took it, and started dispatching while the first was still executing.
The per-routine `flock` cannot cover that: the lock file lives inside each container's own filesystem, and two instances share nothing but origin.
Renewing between runs bounds staleness by one run; renewing during the run removes the run from the equation, which frees the timeout ceiling to be a spend guard instead of a correctness bound.
An earlier version rejected the in-run heartbeat as concurrent git inside the trusted component and coupled the TTL to a 15-minute run ceiling instead -- which made the ceiling load-bearing and forecloses the most obvious use of an agent runner.
The concurrency is narrower than that rejection assumed: lease operations touch only the lease ref and the object store, never the worktree or the index, so a heartbeat may run beside anything else under git's own per-ref locking, with a transient failure retried on the next cadence as the worst case.
That is why heartbeats run outside the knowledge lock: that lock serializes worktree writers, and a heartbeat is not one.
The TTL is decoupled from run length because it is also recovery latency -- a SIGKILLed instance's lease has to expire before a replacement may run, and a TTL that scaled with a 6-hour ceiling would leave a crashed agent dead for half a day.
Cancelling the run when the lease is provably gone is the mid-run form of the standing rule: an instance that cannot prove it is the only writer does not act.

Bounding the run is only half of it: the interval between heartbeats also contains git, and a stalled transfer has no natural end.
So every git invocation carries stall limits (SSH keepalives, HTTP low-speed thresholds) -- without them a push into a silently dropped connection parks until the kernel gives up, unbounded time the lease spends expiring underneath a supervisor that is still perfectly alive.
A failed push is a condition the design already handles; a hung one is not.

## Knowledge records events, tasks, and context

**Decision.** One semantic rule routes everything a run wants to remember.
It happened → an **event** (`knowledge/events.md`, append-only history, including explicit NO-OPs).
Someone must do it → a **task** (`knowledge/tasks.md`, owned by the agent or a human).
It may inform future decisions but requires no action → **context** (`knowledge/context.md`).
Only one routine needs it → that routine's private **ledger** (`knowledge/ledgers/<routine>.md`) -- working state, not a run log: a run that changes nothing writes nothing there.
Records hold full facts, never polished prose -- "reviewed PR #482, no doc update needed" -- compression, selection, and voice are a reader's job at read time.
The standing instruction carrying the rule is injected by the runtime into every run's generated agent definition, not left to routine authors; a routine opts out of recording events entirely with `teamwork: off` frontmatter.
The opt-out is enforced at import: a staged change to `events.md` is discarded -- the worktree copy wins, the rest imports, and the discard is logged rather than failing a run whose real work already happened.
Supervisor-written events about the routine (failures, abandonment) are unaffected.
The boundary with the teamwork primitives: **teamwork reports knowledge; it doesn't own it** -- the `teamwork` key gates exactly event recording, no teamwork setting governs tasks, context, or ledgers (even an invisible routine must be able to file a task), and ledgers never enter the delivery feed.

A task is **one canonical record from discovery to resolution**: a stable id (`` `task-YYYYMMDD-<n>` ``), a section that names the owner (`## Agent-owned` / `## Human-owned`), and in-place transitions -- complete, cancel, transfer -- that show up as diffs on one entry.
Tasks are state; events are history; a task transition is itself an observable change, so nothing gets double-recorded to make it reportable.
There is no blockers file: a human handoff is a human-owned task, an undelivered notification is an unconsumed change, and a genuinely blocked task names the dependency it waits on.
The supervisor writes knowledge too, mechanically and always as single-line entries (raw multi-line tool output is flattened): a failure it observed is an event, and giving up -- an abandoned run, a tripped breaker, a sync or a commit it cannot complete -- is a human-owned task, because a run that falls over never gets to explain itself.
A blocker is the task alone -- its creation is already an observable change, so a companion event would record the same fact twice.
Recovery is symmetric: when the condition heals, the supervisor completes its own stale task in place, so a three-minute outage never reads as an open blocker days later.
Ids are convention, not schema: `openroutines check` warns on task entries without one, and the supervisor never rewrites model-authored knowledge.

**Why.** Any autonomous agent beyond the trivial invents these streams eventually; blessing them means nobody reinvents that wheel, and every consumer finds the same shape in every ORA.
The previous model (worklog/intentions/blockers) worked but let physical layout leak into meaning: a human ask started life as a blocker, got reported, then was rewritten into intentions -- the same obligation with two representations and no stable identity, and "blocker" conflating a handoff, an undelivered notification, and an actual impediment.
Separating what a record *means* (event, task, context) from whether anyone has *seen* it (delivery, below) dissolves all three conflations at once.
Raw-facts-only keeps the division of labor clean (the same activity-stream model Steady uses): recording stays cheap and mechanical, judgment happens at read time.

The working set has a **retention window** (`knowledge.retention` in `openroutines.yml`, default 30 days): once a day the supervisor trims entries older than the window from `events.md`, `context.md`, and the run records.
Nothing is lost: git history is the unlimited archive; the working files are the recent window a routine actually loads into context.
Entry age comes from `git blame`, not timestamp parsing, so routines aren't forced into a date format; headers, blank lines, and uncommitted lines always survive.
`tasks.md` is exempt -- it's a living list, and age doesn't make a task done -- as are the per-routine ledgers, which their routines already prune.
A still-relevant long-lived fact gets refreshed when a run reaffirms it; an untouched one ages out of the working view without leaving history.

## Knowledge is inspectable from origin

**Decision.** `openroutines knowledge` fetches and pins one read-only snapshot of `origin/knowledge`, warns when the local worktree differs, and presents an interactive terminal picker for a model-backed summary, file browsing, and statistics; the same operations are direct subcommands for scripts.
It never adopts or changes the local knowledge worktree.
`summarize` uses the agent's default model to render the last 24 hours of committed knowledge changes (`--since` selects another duration), current knowledge, and the current routine schedule as `Recently`, `Next`, and `Waiting on a human`; it confirms the token-spending call, prints its usage, receives only model-provider authentication, and has no routine grants or settlement path.
`sync` remains the explicit command that reconciles the local worktree with origin.

**Why.** `openroutines knowledge` is the human and scripting inspection surface for the branch that otherwise stays deliberately separate from the checkout.
It fetches and pins one read-only snapshot of `origin/knowledge` without adopting, rebasing, committing, or pushing it, then offers an interactive terminal picker (and equivalent direct subcommands) to summarize the snapshot, browse its files, or inspect statistics.
The summary is an ephemeral default-model call over that snapshot and the current routine schedule: it asks before spending tokens, receives no routine credentials, skills, MCP servers, or web grants, writes no knowledge, advances no reporting cursor, and prints its usage with the result.
It is therefore a requested view, not a reporting routine or a second delivery mechanism.
`sync` remains the command that adopts origin into the local knowledge worktree; `status` and `usage` continue to report local operational state and warn when it is stale.
The earlier command with this name merely fetched and printed the raw primitives, which made it a redundant pager; the remote-first explorer earns the surface by making opaque branch state directly inspectable and by giving a person the compressed briefing they were actually asking for.

## Alerting degrades to durability when the datastore breaks

**Decision.** The knowledge branch is the agent's alerting channel -- a blocker is a human-owned task, and everything that carries one outward (a check-in routine, a digest, a person reading `git log knowledge`) reads that branch.
So the failures that break the branch are the ones where alerting has to be *defined* rather than assumed, and what it degrades to there is **durability, not delivery**: the record leaves the container and waits on origin.
Two failure modes, two mechanics.

An **unreachable origin** stops the tick at the lease heartbeat, upstream of every other place the supervisor writes anything, so the condition is recorded where it is detected, in the sync step: a human-owned task committed to the local branch for as long as the outage lasts, resolved in place and pushed with everything else the moment origin answers again.
Nothing reaches a person mid-outage -- there is no channel to reach them through -- and until origin answers the record is only in the container, because origin *is* the durable store and it is the thing that is down.
What that buys is an outage record surviving the supervisor process and arriving intact when the datastore does; a container replaced mid-outage still loses it, and nothing short of a second datastore would change that.

A **blocked sync** (rewritten history, a conflict) is the narrower case: origin is reachable, and the knowledge branch is precisely what the supervisor is refusing to write.
Knowledge goes to a supervisor-owned ref instead, `refs/openroutines/blocked`, force-updated -- the branch on origin stays exactly as the human left it, so publishing there resolves nothing and hides nothing.
What lands on the ref is a **parentless snapshot of the knowledge tree, never the supervisor's own tip**: the tip drags its lineage with it, and when a rewrite is what blocked sync, that lineage is the history a human just rewrote away -- so pushing it would restore, under a ref nobody thinks to look at, whatever the rewrite was performed to remove.
`openroutines sync` names the ref and dates the snapshot when it reports the same refusal, and the one thing the ref is *not* is a merge queue: the supervisor that stranded it puts that state on the branch through the ordinary push once a human repairs the history, and then deletes the ref.
A snapshot this instance did not strand is never deleted (it is the only copy of some earlier container's blocker) and never migrated either -- if the container that wrote it is gone, it is a record to read, and a person applies from it what they want.

**Why.** Every unattended agent has to answer what happens when the alerting channel is itself the failure, and the two tempting answers are both wrong.
Pushing the blocker onto the branch anyway would resolve the thing this design refuses to resolve silently: while sync is blocked, local history is by construction not a fast-forward of origin's tip, so the push is either rejected -- accomplishing nothing -- or, in the one case where it succeeds, origin was force-pushed *backward* and the agent's own history would overwrite a human's rollback.
The only case where "just push it" does something is the case where what it does is damage.
Adding a second channel -- SMTP, a webhook -- would put an outbound integration, its credentials, and its own failure modes inside the trusted component, exercised only when least is working.
The ref is neither: same origin, same deploy key, same push, aimed at a name nothing else writes.
As a snapshot rather than a tip, its force-update is a property instead of a hazard -- a later blocked container supersedes an earlier snapshot, where pushing tips would have orphaned commits only that earlier container had.
It buys the property that was missing -- the blocker outlives the container -- and is honest about the rest: a blocked sync halts dispatch, so nobody is told anything until they look.
Notification while the datastore is healthy is a routine's job (see Delivery); while it is not, "the record is on origin and says what happened" is the contract in its place.

## Delivery: the knowledge branch is the change feed

**Decision.** Reporting is a per-destination view over changes to knowledge, and the knowledge branch's own commits are the change stream -- no report queues, no draining.
A routine that reports declares `reports: true` in frontmatter; declaring it defaults the routine's `teamwork` to `off` -- reporting is definitionally not work -- and an explicit `teamwork: full` or `events` overrides that default for a routine that both reports and does record-worthy work of its own.
The retired `consumes` key is parsed only to be rejected with the mapping (`consumes: knowledge` is now `reports: true`), like `events` before it.
Before a reporting run, the runtime fixes a boundary at the knowledge branch's current commit, walks every commit between the consumer's **cursor** and that boundary, and injects the additions and transitions as a generated `changes.md` snapshot in the disposable run workspace.
When the routine's work has covered the whole change set, it creates a `CONSUMED` marker file inside its staged knowledge directory -- writable to the run wherever the run executes.
The marker is a receipt for the runtime, never knowledge content: import strips it, and after a successful import the runtime advances the cursor through the fixed boundary, in the same completion commit that carries the run's results.
No marker, no advance: completing a run never implies consuming its change set, so a check-in that finds nothing due exits normally and the same changes return next time.
The one exception is a successful first run, whose change set is empty by construction because history is not replayed: the runtime establishes its starting cursor without a marker, or every empty first run would discard its temporary starting point and remain a first run forever.
Consumption is all-or-nothing -- selective delivery would need item-level receipts, deliberately out of scope.

Cursors are per-consumer and framework-managed: JSON under the supervisor-owned `state/` directory on the knowledge branch -- inspectable and repairable like the rest of scheduling state, never staged into a run (a consumer cannot edit its own bookkeeping, and never reacts to cursor commits).
A new consumer starts at the current commit: replaying an agent's entire past into a fresh adapter would be a surprise, not a feature.
A cursor that stops naming a commit on the branch is a person's to fix: the range cannot be walked, so the run is abandoned on its first attempt with a task naming the cursor file, and `openroutines status` says the same thing beside the consumer, because a stuck cursor and a caught-up one are otherwise indistinguishable there.
The usual cause is the framework's own doing rather than a bad hand edit: sync rebases local commits when origin has diverged -- human curation racing a run -- and that rewrites any commit a cursor was written against but not yet pushed.
Repairing it automatically is wrong: the only recovery a machine can compute is the merge-base, and resetting a cursor there re-delivers everything since -- an adapter re-posting weeks of history is a worse failure than a stopped one, and only a person knows what was already posted.

The feed walks **commit by commit, never a net endpoint diff**: an event added and later pruned by retention still reaches a consumer that hasn't seen it, which lets retention keep the working files small without a second grooming policy.
The trim commit itself is **not delivered** -- the supervisor marks it with a trailer (`Openroutines-Retention-Trim: true`) and the feed skips every commit carrying one.
Pruning is bookkeeping, not a knowledge change: a consumer already past those entries would otherwise be handed a block of removals for history it consumed weeks ago, once a day, forever, while the removals it does need -- a task completed or canceled in place -- are ordinary commits and still arrive.
Because that commit is skipped whole, it is scoped to the files retention rewrites: a maintenance commit that swept up work merely dirty when it fired would delete that work from the feed with no trace anywhere.
Framework bookkeeping (`state/`, run records) and routine-private ledgers are excluded from the feed.
The marker is a file, not a CLI command, by necessity as much as taste: the model process runs in the runtime container, which carries no `openroutines` binary, and a file in the discarded workspace fits the staging invariant (models write files; the supervisor does git).

**Why.** The old check-in drained the shared files destructively, which quietly gave the queues exactly one consumer: Steady could be *replaced* by Slack, but the two could not independently report the same facts.
Cursor-per-consumer fixes that with machinery the design already had -- every run already leaves an intent commit and a completion commit, so the transaction boundaries exist; delivery just reads them.
The failure mode is honest: cursor advancement and an external post are not atomic, so a crash between posting and consuming re-presents the change set -- at-least-once, same as run execution, and the same answer applies (destinations with idempotency keys or searchable artifacts dedupe; others tolerate a rare duplicate).
What OpenRoutines promises is only that an unconsumed change remains available; duplicate prevention belongs to the adapter that knows its destination.
(Model and problem statement: Adam's work-primitives brief, product-pal repo.)

