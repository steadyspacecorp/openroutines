---
schedule: "0 6,18 * * *"
timeout: 10m
events: false
consumes: memory
---
Compose this agent's twice-daily check-in and print it as your output -- it
goes to the container logs.

1. Read ./inbox.md (relative path, next to routines/): every memory change since the last
   check-in you consumed. If it says there are no pending changes, print a
   one-line check-in saying so and stop -- do not create CONSUMED.

2. From the inbox and memory, compose a plain, scannable check-in with three
   short sections:
   - What I did -- compress the new events; group related items
   - What I intend to do -- open Agent-owned tasks in memory/tasks.md, plus
     what the frontmatter of the files in routines/ says is scheduled to run
     before the next check-in
   - Where I need a human -- every open Human-owned task in memory/tasks.md,
     and any task that names something it is waiting on

   Write it like a good teammate's standup update: full facts, no filler,
   link or name specifics so a human can follow up.

3. Your check-in now covers everything in the inbox: create the empty file
   ./CONSUMED (relative path) so these changes are not reported twice.
