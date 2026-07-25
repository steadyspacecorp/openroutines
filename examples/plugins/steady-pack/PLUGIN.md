---
name: steady-pack
description: File daily Steady check-ins from agent memory and keep up with comments -- the two routines every Steady-connected agent otherwise reinvents.
credentials:
  steady_token:
    description: A Steady personal access token (steady_pat_...) for the agent's own Steady account
---

# Steady pack

Connects an agent to [Steady](https://runsteady.com) the way a teammate is connected: it files a daily check-in composed from the agent's memory, and it reads and answers comments addressed to the agent.

## What you get

- **steady-check-in** -- weekday mornings, consumes the memory change feed and files one check-in covering everything since the last one: previous work from events, intentions projected from the routine schedules, blockers from human-owned tasks.
- **steady-inbox** -- hourly during the workday, replies to comments on the agent's check-ins and goal updates, turns action requests into memory tasks, and grooms the task list. Most runs find nothing and end quickly.
- **steady-api** and **steady-updates** skills -- the API reference and the writing guide the routines work from.

## After installing

1. `openroutines credentials set steady_token` -- a personal access token for the agent's own Steady account (create one at Settings → API).
2. Adjust the schedules to your workday and timezone.
3. `openroutines check`, review the diff, commit.

The check-in routine is a memory-feed consumer: it needs nothing beyond what your other routines already record. If your agent has no other routines yet, the check-ins will be quiet -- that's correct, not broken.
