# Your agent on the team

An ORA's memory is not the "remembers your preferences" memory of a chat assistant. It's a work record -- and the work record is what lets an autonomous agent participate in the rituals of a team: state intentions, report progress, surface blockers, and fold what worked back into how it does the job. Out of the box, every agent checks in twice a day like a teammate would.

The mechanism is a small set of versioned memory primitives, and a change feed built on top of them.

## What the agent records

One rule routes everything an agent wants to remember, into files any autonomous agent ends up needing -- so you never have to invent them:

- It happened → an event in **events.md** -- raw facts, including NO-OPs ("checked 5 PRs, no doc drift")
- Someone must do it → a task in **tasks.md**, owned by the agent or a human -- one canonical record with a stable id, from discovery to resolution. The supervisor writes here too: a run it had to give up on becomes a human-owned task, so even failures that never got to explain themselves land on someone's list
- It may inform future decisions but requires no action → a line in **context.md**
- Only one routine needs it → that routine's private ledger in `memory/ledgers/` -- working state for its next run, not a run log

Records hold facts, never polished prose -- compression and voice are a reader's job. The rule is injected into every run by the runtime, not left to routine authors; a reporting routine opts out of recording events with `events: false` (checking in is not work).

## Memory lives on a git branch

The memory an agent builds as it works travels through git, on its own branch -- backed up with every push, kept separate from your routines. Memory survives redeploys and rollbacks like a database, but versioned and inspectable: reviewing what your agent has learned is `git log memory`, and pruning bad learnings is part of maintaining an agent -- humans can curate the branch, and the agent pulls before each run.

The working files stay lean, too: entries older than the retention window (`memory.retention` in `openroutines.yml`, default 30 days) are trimmed daily, and git history keeps everything forever -- including changes a consumer hasn't seen yet.

## Reporting: the memory branch is the change feed

Because memory is a git branch, its commits are a change feed: a reporting routine declares `consumes: memory`, receives an inbox of everything since it last reported, and marks the batch consumed when its report covers it. Each consumer keeps its own cursor, so pointing a second destination at the same agent -- Steady and Slack, say -- takes no changes to the routines doing the work. Nothing is delivered twice or lost: an unconsumed change remains available and returns next time.

## The check-in routine

The starter check-in routine is the first consumer: twice a day it turns the feed into a teammate-style update in your logs -- what I did, what I intend to do, where I need a human. It ships active by default, declares no skills and no credentials, and makes every agent observable from day one. Pointing it at Steady, Slack, or anywhere else is a frontmatter diff that adds a skill and a credential.

The full reasoning -- why these primitives, why a branch, why cursors per consumer -- is in [docs/design.md](design.md) ("Memory", "Memory records events, tasks, and context", "Delivery", "Every agent checks in").
