# Knowledge

An ORA's knowledge is not the "remembers your preferences" memory of a chat assistant. It's a work record: what happened, what someone must do, what's worth keeping -- versioned in git, reviewed like code. The [teamwork primitives](teamwork.md) report this record; they don't own it.

## What the agent records

One rule routes everything an agent wants to remember, into files any autonomous agent ends up needing -- so you never have to invent them:

- It happened → an event in **events.md** -- raw facts, including NO-OPs ("checked 5 PRs, no doc drift")
- Someone must do it → a task in **tasks.md**, owned by the agent or a human -- one canonical record with a stable id, from discovery to resolution. The supervisor writes here too: a run it had to give up on becomes a human-owned task, so even failures that never got to explain themselves land on someone's list
- It may inform future decisions but requires no action → a line in **context.md**
- Only one routine needs it → that routine's private ledger in `knowledge/ledgers/` -- working state for its next run, not a run log

Records hold facts, never polished prose -- compression and voice are a reader's job. The rule is injected into every run by the runtime, not left to routine authors. Whether a routine's runs are recorded as events at all is a teamwork question, answered by its `teamwork` frontmatter -- see [Your agent on the team](teamwork.md); tasks, context, and ledgers are always writable, because even an invisible routine must be able to file a task.

## Knowledge lives on a git branch

The knowledge an agent builds as it works travels through git, on its own branch -- backed up with every push, kept separate from your routines. Knowledge survives redeploys and rollbacks like a database, but versioned and inspectable: reviewing what your agent has learned is `git log knowledge`, and pruning bad learnings is part of maintaining an agent -- humans can curate the branch, and the agent pulls it before each run. Knowledge is the only branch a running agent syncs with origin: routines and code travel the other way, baked into the image at build time (see [Operating in production](operating.md)).

Run `openroutines sync` from an agent checkout to fetch the latest knowledge, then read the files -- tasks, events, context, and the routine ledgers are ordinary Markdown, and `knowledge/ledgers/check-in.md` holds the latest check-in the agent delivered. Skip the sync when you deliberately want the already-materialized local snapshot.

The working files stay lean, too: entries older than the retention window (`knowledge.retention` in `openroutines.yml`, default 30 days) are trimmed daily, and git history keeps everything forever -- including changes a reporting routine hasn't seen yet. Trimming is housekeeping and is never reported: a routine already past those entries hears nothing about them being pruned.

The full reasoning -- why these primitives, why a branch -- is in [docs/design.md](design.md) ("Knowledge: a dedicated directory on its own branch", "Knowledge records events, tasks, and context").
