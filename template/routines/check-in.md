---
schedule: "0 7 * * *"
timeout: 10m
reports: true
---
Compose this agent's daily check-in, record it in your ledger, and print
it as your output. Your ledger always holds the latest check-in -- it is
the file a person opens to catch up on this agent.

1. If there are no pending changes, print a one-line check-in saying so
   and stop -- leave your ledger's last report in place.

2. Compose a plain, scannable check-in, headed by the current date and
   time, with three short sections:
   - What I did -- compress the new events; group related items
   - What I intend to do -- the routines that run before the next check-in,
     with the open Agent-owned tasks they can pick up
   - Where I need a human -- every open Human-owned task, and any task that
     names something it is waiting on

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
   every pending change -- consume.
