---
schedule: "0 9 * * 1-5"
timeout: 5m
active: true
reports: true
skills: [reporter-format]
credentials: [reporter_token]
mcp: [reporter_feed]
---

Acceptance fixture. Consume the knowledge change feed and report it to the fake destination. This routine is never meant to run.
