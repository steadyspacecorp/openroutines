---
schedule: "45 8-17 * * 1-5"
# For faster replies without more model runs, add a trigger and slow the
# schedule to a heartbeat -- point it at a cheap endpoint that changes when
# anything new lands for the agent, e.g.:
# trigger:
#   poll: https://service.steady.space/api/v2/digest
#   credential: steady_token
#   interval: 5m
timeout: 10m
active: true
events: false
skills: [steady-api]
credentials: [steady_token]
---

Watch Steady and keep memory current. Replying to comments is your only
Steady write -- everything else the agent reports goes through
steady-check-in. Most runs find nothing new: end quickly.

## 1. Collect

- `GET /me` -> your id.
- Your check-ins from the last 7 days
  (`/check-ins?since=...&people_ids[]=<your id>`), then the comments on
  each (`/check-ins/{id}/comments`).
- `GET /digest?since=<day before your last completed check-in>` -- catches
  comments elsewhere: on your goal updates, and comments or @mentions of
  you on other people's resources. No check-in to anchor on -> look back 3
  days. Keep no cursor; re-reading is safe.

## 2. Answer comments

Your ledger (memory/ledgers/steady-inbox.md) records the ids of comments
you already replied to -- that record is what stops double replies. A
comment is handled when its id is in the ledger, or a comment of yours on
the same resource has a later created_at (compare timestamps, never list
position). **Every unhandled comment gets a reply, no exceptions**;
skipping one means every future run re-examines it. After a reply posts
successfully, add the replied-to comment's id to the ledger; a failed post
gets no entry, so the next run retries it. Prune entries once their comment
ages out of the collection window. For each in-scope, unhandled comment by
someone else:

- **Action request** -> add an Agent-owned task to memory/tasks.md (stable
  id; source: requester + the commented resource); reply with a brief "on
  it" that names when -- the next run of the routine whose domain covers
  the item (schedules are in routines/*.md frontmatter).
- **Question or feedback** -> answer it in-thread.
- **Answer to a Human-owned task in memory/tasks.md** -> resolve the task
  in place: transfer it to Agent-owned if the ask became agent work, or
  cancel it if declined; reply with a brief acknowledgement.
- **Anything else** (FYI, status update, someone claiming work) -> reply
  with a brief acknowledgement.

Reply on the same resource the comment is on --
`POST /check-ins/{id}/comments` or `POST /goal-updates/{id}/comments` with
`{"body": "<markdown>"}`. Casual teammate voice: short and warm, not
corporate. One reply may answer several pending comments on a resource.

## 3. Groom

- Mark open Agent-owned tasks in memory/tasks.md done when memory/events.md
  or the agent's check-ins show they happened; merge duplicates; delete
  done tasks after about a week.
- A Human-owned task leaves memory/tasks.md only two ways: a human
  explicitly settled the ask, or it has gone unanswered for about three
  weeks and is quietly cancelled. Never remove one for any other reason;
  when in doubt, leave it.
- Keep every memory file small and factual.
