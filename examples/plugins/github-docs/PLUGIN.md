---
name: github-docs
description: Watch documentation repositories for changes and keep a running account of what changed and what it affects -- the base of a knowledgebot.
credentials:
  github_app_private_key:
    description: Private key PEM for a GitHub App installed on the watched repos (one-line \n-escaped form recommended)
    type: github_app
variables:
  docs_repos:
    description: Space-separated owner/repo list to watch, e.g. "acme/docs acme/runbooks"
---

# github-docs

The watching half of a knowledgebot: a routine that sweeps configured documentation repositories, records what changed since its last look, and keeps memory current on what those changes affect. What your agent *does* with that awareness -- updating a knowledge base, answering questions, flagging drift against another source -- is the routine you write next; this plugin keeps the raw material fresh.

## What you get

- **doc-watch** -- per repo, diffs the default branch against the last commit it recorded, summarizes meaningful changes into events (renames and typo-fixes collapse; structural and content changes get a line each with links), refreshes context.md with anything future runs should know, and advances its ledger watermark only after recording.

## After installing

1. Create a GitHub App with read-only Contents permission, install it on the repos you want watched, and note its App ID.
2. Add the typed-credential entry to `openroutines.yml` (the App's private key never enters a run -- each run gets a one-hour installation token):

   ```yaml
   credentials:
     github_app_private_key:
       type: github_app
       app_id: "YOUR_APP_ID"
   ```

3. `openroutines credentials set github_app_private_key`
4. Set the `docs_repos` variable in `openroutines.yml`.
5. `openroutines check`, review the diff, commit.

A fine-grained PAT in a raw credential works too if a GitHub App is overkill -- swap the credential name into the routine and skip step 2. The App is the better unattended posture: short-lived tokens, revoked after every run.
