---
schedule: "0 7-19/3 * * 1-5"
# For near-real-time watching of a public repo, a trigger can wake this
# routine when the default branch moves (conditional GET makes the poll
# nearly free; unauthenticated is fine at this interval for public repos --
# a trigger credential must be raw, so the typed github_app credential
# cannot authenticate the poll):
# trigger:
#   poll: https://api.github.com/repos/OWNER/REPO/commits?per_page=1
#   select: /0/sha
#   interval: 5m
timeout: 15m
active: true
credentials: [github_app_private_key]
---

Watch the documentation repositories in $DOCS_REPOS for changes and keep
memory current on what changed and what it affects. `gh` is authenticated
via $GITHUB_TOKEN. Most runs find nothing: record the NO-OP and end.

## Per repository

Your ledger (memory/ledgers/doc-watch.md) holds a watermark per repo: the
last default-branch commit you accounted for.

1. **Find the movement.** Compare the default branch head against the
   watermark (`gh api repos/{repo}/compare/{watermark}...{head}`). No
   watermark yet -> record the current head as the baseline, note the
   adoption in events, and move on -- never try to account for a repo's
   whole history.
2. **Account for it.** Read the compare's commits and changed files, and
   record what a colleague tracking the docs would want to know:
   - One event per meaningful change: what changed, in which doc, why it
     matters, with links to the commit or PR. Group a batch of related
     commits (one PR, one topic) into one event.
   - Collapse noise: typo fixes, formatting, link rot repairs -> at most
     one summary line for the batch.
   - Refresh memory/context.md when a change alters standing facts a
     future run should know -- a doc moved, a section was deprecated, a
     new area appeared. Keep it small; drop entries the repos no longer
     support.
3. **Advance the watermark** to the head you accounted for -- only after
   the events are recorded, so a crash re-reads instead of skipping.

A repo that fails (renamed, permissions, rate limit) gets one event naming
the error, and its watermark stays put; if it stays broken across runs,
raise it as a Human-owned task in memory/tasks.md instead of repeating the
event.
