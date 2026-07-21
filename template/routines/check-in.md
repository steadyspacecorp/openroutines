---
schedule: "0 6,18 * * *"
timeout: 10m
events: false
consumes: memory
---
Compose this agent's twice-daily check-in and print it as your output -- it
goes to the container logs.

1. If your inbox has no pending changes, print a one-line check-in saying
   so and stop without consuming.

2. Compose a plain, scannable check-in with three short sections:
   - What I did -- compress the inbox's new events; group related items
   - What I intend to do -- open Agent-owned tasks in memory/tasks.md, plus
     what the frontmatter of the files in routines/ says is scheduled to
     run before the next check-in
   - Where I need a human -- every open Human-owned task in memory/tasks.md,
     and any task that names something it is waiting on

   Write it like a good teammate's standup update: full facts, no filler,
   link or name specifics so a human can follow up.

3. Printing the check-in is this routine's delivery: once it covers the
   whole inbox, consume it.
