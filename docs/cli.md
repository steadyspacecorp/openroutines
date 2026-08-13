# CLI reference

Run any command from inside an agent repository (except `new`).

Commands that change the agent -- routines, skills, plugins, credentials -- edit files in the repository, not a running container. Local runs see the change immediately; a deployed agent runs the copy baked into its image and picks the change up on the next rebuild and redeploy (see [Operating in production](operating.md)).

```
openroutines new <path>           create a new agent repository
openroutines configure            fill in openroutines.yml, generate the master key
openroutines check                validate the agent; made for CI
openroutines status               show what the agent has and still needs
openroutines knowledge            inspect and summarize knowledge from origin
openroutines usage                token use and reported cost per routine (--json)
openroutines sync                 pull the agent's latest knowledge from origin (--push)
openroutines routines <command>   new, list, run, edit, activate, deactivate, remove
openroutines skills <command>     new, list, remove
openroutines plugin <command>     add, list, update grouped plugin bundles
openroutines credentials <cmd>    set, list, remove
openroutines supervise            run the scheduler (container entrypoint)
openroutines update               bump the pinned framework version
openroutines version              print the version
```

## new

```
openroutines new <path>
```

Creates a fresh git repository with the agent's skeleton: `openroutines.yml`, a starter check-in routine, an empty `skills/` directory, `AGENTS.md` (with `CLAUDE.md` symlinked to it), a baseline `opencode.json` permission policy, `.gitignore`, Dockerfile, and pinned framework version.

## configure

```
openroutines configure
```

Idempotent -- run it whenever. Fills in `openroutines.yml` ([name](getting-started.md#name-and-instructions), owner, timezone, default model), generates the master key for encrypted credentials, and reports anything the agent still needs.

## check

```
openroutines check
```

Validates config, frontmatter, schedules, credential wiring, and config drift. When an agent-owned routine overrides a same-named plugin routine, check names the shadowed plugin path. Made for CI: run it on every push.

## status

```
openroutines status
```

Shows master key state, models, routines and schedules, skills, knowledge sync state, and token usage.

Each routine also reports what the supervisor's scheduling state says about it: how far its schedule is accounted for (the watermark), a pending run with the attempts it has spent, and an active circuit-breaker cool-down with the time it ends -- a routine sitting one out does not advertise a next firing, because it will not honor it. A routine with nothing under it has no state yet: the supervisor has never seen it, which is what every fresh checkout looks like. The grants in parentheses are the routine's declared authority -- skills, credentials, MCP servers, and web access.

A pending run is followed by what actually becomes of it, which the attempt count alone does not say:

- `next attempt <time>` -- the last attempt failed and the run is backing off.
- `still in flight` -- the attempt is running now. Reserving an attempt and failing one leave the same state on disk, so the tell is the run record, which is written only when an attempt settles. Without a run log to compare against (a checkout that has never run), status falls back to the retry line rather than guess.
- `due now` -- the retry is already owed, or no attempt has touched the run yet.
- `budget spent` -- every attempt is gone; the next tick abandons the run and files a task.
- `held` -- the routine is inactive or declares neither a schedule nor a trigger, so the tick skips it before reading its state. Nothing is coming to advance the run; activating the routine or restoring its schedule releases it.

These lines come out of the `knowledge` branch as your checkout last fetched it, so status says when that reading is behind origin. Run `openroutines sync` for the current picture.

## knowledge

```
openroutines knowledge
openroutines knowledge summarize [--since 24h] [--yes]
openroutines knowledge list [path] [--json]
openroutines knowledge show <path>
openroutines knowledge stats [--json]
```

Fetches and pins one read-only snapshot of `origin/knowledge` without adopting it into local `knowledge/`. In a terminal, the bare command opens a small interactive explorer with Summarize, Browse files, and View stats; when piped, it prints the overview and direct-command help. Every view names a local worktree that is behind, ahead, divergent, or uncommitted while still showing origin.

`summarize` is an ephemeral briefing from the last 24 hours of knowledge changes and the current knowledge and routine schedule, rendered by the agent's default model as Recently, Next, and Waiting on a human. `--since` accepts another Go duration when a different window is useful. It asks before spending tokens (`--yes` confirms non-interactively), prints the captured usage, and has no routine credentials, skills, MCP servers, web grants, knowledge writes, run record, or reporting cursor. `list` and `stats` offer JSON for scripts; `show` prints one file verbatim.

## usage

```
openroutines usage [--json]
```

Token use and reported cost per routine. `--json` emits the machine-readable form for scripts and monitors.

## sync

```
openroutines sync [--push]
```

Reconciles `knowledge/` with origin. A deployed agent writes its knowledge on the `knowledge` branch, and `git pull` in the agent repository moves the remote-tracking ref without touching the knowledge worktree -- so a checkout keeps reading old knowledge until you sync it. `status` and `usage` say when that has happened and name this command.

Syncing is how you adopt knowledge for local curation; `openroutines knowledge` inspects or summarizes origin without changing the local worktree. After a sync, the knowledge primitives are ordinary Markdown files under `knowledge/`, and `knowledge/ledgers/check-in.md` holds the latest check-in the agent delivered -- the teammate-style update as of the last scheduled run. For one composed right now, `openroutines routines run check-in` echoes a fresh report -- manual runs discard their knowledge writes, so the change feed is not consumed.

Fast-forwards when behind, rebases local commits when both sides moved, and refuses rather than resolving anything itself: a conflict is left for you to resolve inside `knowledge/`, and rewritten upstream history is refused outright. `--push` also publishes local knowledge commits.

When a refusal happens, sync also says whether the deployed agent stranded knowledge on `refs/openroutines/blocked` -- a snapshot of what a supervisor whose own sync is blocked could not write to the branch, typically carrying the blocker task that explains this same refusal. Sync fetches the ref for you (git replicates nothing outside `refs/heads` and `refs/tags` on its own), so `git -C knowledge show refs/openroutines/blocked:tasks.md` reads it and `git -C knowledge diff knowledge refs/openroutines/blocked` shows what the agent has that the branch does not. If the container that stranded it is still running, repairing the branch is what puts that state back on it, and the ref is deleted; if that container has since been replaced, the snapshot is a record to read -- apply from it what you want, then drop the ref with `git push origin :refs/openroutines/blocked`.

`status` and `usage` never sync on their own. This command fetches, can rebase, and publishes the accepted-tip baseline that makes rewrite refusal durable -- none of which belongs in a command whose job is to report local state. `knowledge` fetches too, but only to export a pinned read-only origin snapshot; it never reconciles the worktree or publishes a ref.

## routines

```
openroutines routines new <name>         create a routine (inactive until you activate it)
openroutines routines list               names, schedules, grants
openroutines routines run <name> [--write-knowledge] [--rehearse [scenario]]
                                         run once now; optionally settle knowledge or rehearse
openroutines routines edit <name>        open in $EDITOR, validate on close
openroutines routines activate <name>    set active: true
openroutines routines deactivate <name>  set active: false
openroutines routines remove <name>      delete the routine and its scheduling state
```

A routine that does not load still holds its name: `new` refuses rather than overwrite it, `run` reports the parse error instead of "no routine", and `edit` and `remove` operate on the file so you can fix or drop it.

`run` always has the routine's declared credentials and tools and may perform external actions -- it is not a dry run. By default a manual run discards its staged knowledge writes and run record: running from the terminal is the iteration path, and iterating must not teach the agent or consume its change feed by accident. Pass `--write-knowledge` when the run should count -- settle its writes, advance its cursor, leave its record. Use `check` for non-acting validation, and point acting routines at a scratch target when exercising their external path.

Deactivating a routine that is misbehaving in production stops it at the next redeploy, not the next tick -- the deployed supervisor keeps reading the copy in its image until then.

## skills

```
openroutines skills new <name>           scaffold a blank skill
openroutines skills new <git-url | owner/repo> [--path <sub/dir>]
                                         vendor a skill from a git repository
openroutines skills list                 skills and which routines use them
openroutines skills remove <name>        refuses while any routine declares it
```

## plugin

```
openroutines plugin add <git-url | owner/repo | local-dir> [--path sub/dir] [--yes]
openroutines plugin list
openroutines plugin update <name> [--yes]
```

`add` shows the bundle's declared authority and vendors it under `.openroutines/plugins/<name>/` after you confirm; installed routines land inactive. `list` shows installed plugins. `update` fetches the recorded source and three-way merges upstream changes against your vendored copy. `--yes` is required when stdin is not interactive.

## credentials

```
openroutines credentials list           credential names and which routines declare them
openroutines credentials set <name>     add or replace one value (prompted, hidden)
openroutines credentials remove <name>  refuses while any routine declares it
```

`set` writes the encrypted store in the repository. A deployed agent decrypts the copy in its image, so a rotated value reaches production with the next rebuild and redeploy -- until then, runs keep using the old one.

## supervise

```
openroutines supervise
```

Runs the scheduler. This is the container entrypoint; you rarely run it by hand.

## update

```
openroutines update
```

Brings the agent up to the version of the `openroutines` binary you're running: bumps the pin in `.openroutines/version`, rewrites the Dockerfile's base-image tag, and offers other framework-owned file changes interactively with a diff.

## version

```
openroutines version
```

Prints the version. Also `--version` / `-v`.
