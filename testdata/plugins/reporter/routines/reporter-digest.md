---
schedule: "0 9 * * 1-5"
timeout: 5m
active: true
events: false
consumes: memory
skills: [reporter-format]
credentials: [reporter_token]
mcp: [reporter_feed]
---

Smoke fixture. Consume the memory change feed and report it to the fake destination. This routine is never meant to run.
