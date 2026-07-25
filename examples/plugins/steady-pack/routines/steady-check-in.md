---
schedule: "15 9 * * 1-5"
timeout: 10m
active: true
events: false
consumes: memory
skills: [steady-api, steady-updates]
credentials: [steady_token]
---

File the agent's daily Steady check-in the way a person would: once, at the
start of the workday, covering everything since the last one. Never edit a
check-in after it's submitted -- nobody is notified of edits, so amended
content is guaranteed to be missed.

## 1. Decide whether to file

One gate: `GET /action-items` and look for an open check-in action item due
today. It answers both questions at once: whether Steady expects a check-in
from you today (it tracks schedule changes, holidays, absences) and whether
you've already filed (submitting completes the item). No open item -> stop
without consuming. Never infer either answer from the check-in's content --
check-ins are pre-generated, so empty or non-empty proves nothing.

If the gate passes, always file -- a quiet day still gets a check-in; its
previous is just light.

## 2. Compose

The steady-updates skill is the writing guide.

Voice: casual and human -- a teammate at standup, not a status report
generator. Plain words, contractions welcome, no stiffness.

- **previous** -- the work you did, as recorded in the inbox's new events,
  rewritten from raw facts into a good update: group related work, lead
  with outcomes, link references you can resolve on meaningful phrases
  (never naked URLs). Work only from what the events say -- never embellish
  or invent facts. Real outcomes lead and get the space; NO-OP events
  compress to one short trailing clause. On a quiet day they are the
  update: one line covering what was checked and found clean.
- **intentions** -- required; never leave it blank. All agent work happens
  inside routine runs, so intentions are a projection of the schedule:
  enumerate the active routines in routines/*.md, collect every one (other
  than the steady-* routines themselves) whose next fire time lands before
  the next check-in, and write one line per routine -- its mission, plus
  any open Agent-owned tasks in memory/tasks.md in its domain. "Nothing is
  scheduled" is valid only when that collection is genuinely empty, as
  before a weekend or holiday.
- **blockers** -- every Human-owned task the inbox shows as new or
  transferred, plus any task change that names a dependency it waits on.
  Each ask gets mentioned once; the inbox guarantees that, because a
  transition you consume is never presented again.
- **previous_completed** -- set only when the previous check-in's
  intentions are all covered by the inbox's events; otherwise leave it out.
  Leave mood blank.

## 3. Submit

File through the action item's edit link -- it points at the exact check-in
to fill, and that check-in says which teams it covers. Respect that scope:
never set team_ids yourself.

## 4. Consume

A successful PATCH is this routine's delivery -- consume the inbox. A
failed PATCH or a closed gate means nothing was delivered: leave it
unconsumed.
