# Your agent on the team

Most of what makes a good teammate is communication: they say what they're planning, what they did, where they're stuck, and what's still on their list. OpenRoutines teamwork primitives give your agent the same habits: declaring intentions, reporting progress, calling out blockers, and tracking tasks.

The primitives are built on the agent's [knowledge](knowledge.md). **Routines that do work** write down plain facts as they go, and **reporting routines** read those facts later to compose updates for the team. That split means you can write working routines without thinking about reporting at all, and point any number of reports and destinations at the facts they leave behind.

## The pieces

Every outcome is carried by a small set of plain files:

| Outcome | Carried by |
|---|---|
| Declare intentions | Every run receives `schedule.md`, the runtime's list of what runs next. A report pairs it with the open Agent-owned tasks in `tasks.md` to say what the agent will do next and what it still owes. |
| Report progress | Working routines record what happened as events in `events.md`. When a reporting routine runs, the runtime hands it `changes.md`: everything added to knowledge since that routine last reported. |
| Call out blockers | Anything that needs a person becomes a Human-owned task in `tasks.md`. Routines file them when they hit a wall, and the supervisor files its own when a run fails or gives up. |
| Track tasks | Any routine that finds work someone must do files a task in `tasks.md`. Each task is one record with a stable id and an owner, updated in place from discovery to resolution. |

To read what your agent has recorded, run `openroutines sync` from an agent checkout and open the files under `knowledge/` -- they are ordinary Markdown, with `knowledge/ledgers/check-in.md` holding the latest check-in the agent delivered. Browse history with `git log knowledge`.

There's one piece you never see in a prompt: the runtime injects standing instructions into every run that teach the model these files and the rules for using them -- what goes where, who owns what, when to consume. Routine prompts never explain the primitives; they only describe the job.

## Declaring intentions

Declaring intentions is a reporting routine's job, and the runtime does the hard part. It parses every routine's `schedule`, works out which ones will run before the next report, and hands the reporting run that list as a read-only `./schedule.md`. The reporting routine pairs those routines with any open Agent-owned tasks in `tasks.md` they can pick up within the reporting window. See [Creating routines](routines.md#scheduling) for the file's details.

## Reporting progress

Progress has two halves: recording and delivery.

**Recording is automatic.** As a working routine does its job, its runs land as events in `events.md`: raw facts with links, including finding nothing ("checked 5 PRs, no doc drift"). Routine authors don't write any of this -- the runtime injects the recording rules into every run. Events stay rough on purpose; polishing them into something readable is the delivery half's job.

**Delivery is reading the record and passing it on.** A routine that declares `reports: true` receives `changes.md`, everything recorded since its last report. It turns that into an update, sends it wherever it's pointed, and marks the batch consumed once the update actually lands. A failed send just means the same changes come back next run -- nothing is lost, and nothing goes out twice. Each reporting routine keeps its own place in the feed, so a second destination (Steady and Slack, say) is just a second routine, and a new one starts from the current state instead of replaying history.

If someone rewrites the knowledge branch's history, a reporting routine's saved place can point at a commit that no longer exists. The routine stops rather than guessing, and `openroutines status` plus a Human-owned task name the cursor file to fix: delete it to start fresh from the current state, or repair the SHA if the unreported changes still need to go out.

## Calling out blockers

A blocker is a Human-owned task in `tasks.md` -- there is no separate blockers file or alert channel to wire up. A handoff a routine cannot complete becomes a Human-owned task; a genuinely blocked task names the dependency it waits on. The supervisor files them too: a run it had to give up on, a tripped circuit breaker, a sync it cannot complete each become a Human-owned task, because a run that falls over never gets to explain itself and a person is the only one who can act. When the condition heals, the supervisor completes its own stale task in place, so a three-minute outage never reads as an open blocker days later.

A report's "where I need a human" section is then just a read: every open Human-owned task, plus any task naming what it waits on. The injected instructions already tell the model what those are and where to find them -- a reporting prompt names the section it wants, nothing more.

Blockers are two-way. Answer one through any channel a routine watches -- a reply to the check-in in Steady or Slack, say -- and the next relevant run reads your answer, files the follow-up as an Agent-owned task, and gets on with the work.

## Tracking tasks

A task is one canonical record from discovery to resolution: a stable id (`task-YYYYMMDD-<n>`), a section naming the owner (`## Agent-owned` / `## Human-owned`), and in-place transitions -- complete, cancel, transfer -- that show up as diffs on one entry, never re-recorded elsewhere. A task tracks where things stand, where events record what happened -- and a transition is itself a change the feed delivers, so completing a task is also how it gets reported. `tasks.md` is exempt from retention trimming -- age doesn't make a task done -- and a synced checkout reads the current list in `knowledge/tasks.md`.

## How loudly a routine participates

One frontmatter key controls it, a three-value ladder:

- `teamwork: full` (the default) -- runs are recorded as events, and upcoming runs appear in the schedule
- `teamwork: events` -- runs are still recorded as events, but upcoming runs are excluded from the schedule, and in turn from reported intentions
- `teamwork: off` -- invisible to the team: for reporting routines, where checking in is not work

Declaring `reports: true` defaults `teamwork` to `off`; set it explicitly for a routine that both reports and does record-worthy work of its own. Whatever the tier, tasks and context stay writable -- even an invisible routine must be able to file a task.

## What this looks like in routines

A working routine needs no teamwork instructions at all -- the runtime injects the recording rules, so the prompt is just the job:

```yaml
---
schedule: "0 9 * * 1-5"
credentials: [github_token]
---
Review yesterday's merged PRs in acme/widgets for documentation drift, and
open a PR fixing whatever you find.
```

Everything this routine contributes comes free: its runs land as events (a no-drift day included), work it can't finish becomes a task with an owner, and its fires appear in every other run's `schedule.md`.

A reporting routine declares the role and decides only three things -- composition, destination, and what counts as delivered:

```yaml
---
schedule: "0 17 * * 1-5"
reports: true
skills: [slack-post]
credentials: [slack_webhook]
---
Post this agent's end-of-day summary to #eng-agents, with three short
sections: what happened (group related items), what's coming before
tomorrow's summary, and where a human is needed.

Posting is delivery.
```

Notice what the prompt never mentions: no file names, no instructions about where events, the schedule, or tasks live -- the injected instructions cover all of that. It decides the sections, the destination, and what counts as delivered. That last line matters: the routine consumes its changes only once the post lands, so a failed post means the same changes return next run.

The check-in routine the template scaffolds is a worked example of the same shape, recording its report in its own ledger instead of posting to a real destination -- after a sync, `knowledge/ledgers/check-in.md` holds the latest one in any checkout. Treat it as a starting point, not a fixture: change its cadence and sections, point it somewhere real by granting a skill and a credential, or replace it with reporting routines of your own.
