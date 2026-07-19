---
id: {{CHECKIN_ID}}
schedule: "0 6,18 * * *"
timeout: 10m
worklog: false
---
Compose this agent's twice-daily check-in and print it as your output -- it
goes to the container logs.

1. Read your ledger (memory/ledgers/check-in.md) for the high-water mark of
   the last check-in you reported. If there isn't one, this is the first.

2. Read the memory primitives for everything since that mark:
   - memory/worklog.md -- what the routines accomplished
   - memory/intentions.md -- what is planned or waiting on a human
   - memory/blockers.md -- what failed or needs help

3. Also read the frontmatter of the files in routines/ to see what is
   scheduled to run before the next check-in.

4. Compose a plain, scannable check-in with three short sections:
   - What I did -- compress the worklog entries; group related items
   - What I intend to do -- from intentions and the upcoming schedule
   - Where I'm blocked -- every unresolved blocker, verbatim if unclear

   Write it like a good teammate's standup update: full facts, no filler,
   link or name specifics so a human can follow up.

5. Update your ledger with the new high-water mark (timestamp of the latest
   entry you reported from each primitive), and prune ledger entries older
   than two weeks.
