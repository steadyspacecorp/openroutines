---
schedule: "0 6,18 * * *"
timeout: 10m
reports: true
---
Compose this agent's twice-daily check-in and print it as your output -- it
goes to the container logs.

1. If there are no pending changes, print a one-line check-in saying so
   and stop.

2. Compose a plain, scannable check-in with three short sections:
   - What I did -- compress the new events; group related items
   - What I intend to do -- the routines that run before the next check-in,
     with the open Agent-owned tasks they can pick up
   - Where I need a human -- every open Human-owned task, and any task that
     names something it is waiting on

   Write it like a good teammate's standup update: full facts, no filler,
   link or name specifics so a human can follow up.

3. Printing the check-in is this routine's delivery.
