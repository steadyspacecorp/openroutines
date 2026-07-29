---
name: watcher
description: Synthetic smoke fixture -- a routine-only bundle with a typed credential, a required variable, and a ledger stub.
credentials:
  watcher_app_key:
    description: Fake GitHub App private key for the smoke test; never a real value
    type: github_app
variables:
  watch_repos:
    description: Space-separated owner/repo list the watcher sweeps
---

# watcher

A synthetic fixture for `bin/smoke`, not a real plugin: one routine, a typed `github_app` credential, a required variable, and a ledger stub. Never install it in a real agent.
