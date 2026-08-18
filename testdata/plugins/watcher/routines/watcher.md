---
schedule: "0 7-19/3 * * 1-5"
timeout: 10m
active: true
credentials: [watcher_app_key]
---

Acceptance fixture. Watch the repositories in $WATCH_REPOS and record what changed, advancing the ledger watermark. This routine is never meant to run.
