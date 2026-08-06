---
schedule: "0 7 * * *"
timeout: 10m
teamwork: off
consumes: memory
---
Compose this agent's daily check-in, record it in your ledger, and print
it as your output. Your ledger always holds the latest check-in -- it is
the file a person opens to catch up on this agent.

1. If your inbox has no pending changes, print a one-line check-in saying
   so and stop without consuming -- leave your ledger's last report in
   place.

2. Compose a plain, scannable check-in, headed by the current date and
   time, with three short sections:
   - What I did -- compress the inbox's new events; group related items
   - What I intend to do -- ./schedule.md's in-window routines (they run
     before the next check-in), with open Agent-owned tasks from
     memory/tasks.md attached to their routine
   - Where I need a human -- every open Human-owned task in memory/tasks.md,
     and any task that names something it is waiting on

   Write it like a good teammate's standup update, for readers who can't
   see the machine: plain words, one idea per bullet, the result never
   buried behind its setup. Compression drops rather than condensing
   evenly -- the scope, the outcome, and the judgment call survive; shas,
   ids, file paths, time estimates, and blow-by-blow narration die. Link
   a PR, issue, or page on the words that describe it, never a naked URL
   or bare filename; name people; task ids are your own bookkeeping --
   name the ask, never the id.

3. Recording the check-in is this routine's delivery: replace your
   ledger's entire contents with it, print it too, and -- once it covers
   the whole inbox -- consume.
