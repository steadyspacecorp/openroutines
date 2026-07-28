# CLI reference

Run any command from inside an agent repository (except `scaffold`).

```
openroutines scaffold <path>      create a new agent repository
openroutines configure            fill in openroutines.yaml, generate the master key
openroutines check                validate the agent; made for CI
openroutines status               show what the agent has and still needs
openroutines usage                token use and reported cost per routine (--json)
openroutines sync                 pull the agent's latest memory from origin (--push)
openroutines routines <command>   new, list, run, test, edit, activate, deactivate, remove
openroutines skills <command>     new, list, remove
openroutines plugin <command>     add, list, update grouped plugin bundles
openroutines credentials <cmd>    set, list, remove
openroutines supervise            run the scheduler (container entrypoint)
openroutines update               bump the pinned framework version
openroutines version              print the version
```

## scaffold

```
openroutines scaffold <path>
```

Creates a fresh git repository with the agent's skeleton: `openroutines.yaml`, a starter check-in routine, an empty `skills/` directory, `AGENTS.md`, a baseline `opencode.json` permission policy, `.gitignore`, Dockerfile, and pinned framework version.

## configure

```
openroutines configure
```

Idempotent -- run it whenever. Fills in `openroutines.yaml` (name, job description, owner, timezone, default model), generates the master key for encrypted credentials, and reports anything the agent still needs.

## check

```
openroutines check
```

Validates config, frontmatter, schedules, credential wiring, and config drift. Made for CI: run it on every push.

## status

```
openroutines status
```

Shows master key state, models, routines and schedules, skills, memory sync state, and token usage.

## usage

```
openroutines usage [--json]
```

Token use and reported cost per routine. `--json` emits the machine-readable form for scripts and monitors.

## sync

```
openroutines sync [--push]
```

Reconciles `memory/` with origin. A deployed agent writes its memory on the `memory` branch, and `git pull` in the agent repository moves the remote-tracking ref without touching the memory worktree -- so a checkout keeps reading old memory until you sync it. `status` and `usage` say when that has happened and name this command.

Fast-forwards when behind, rebases local commits when both sides moved, and refuses rather than resolving anything itself: a conflict is left for you to resolve inside `memory/`, and rewritten upstream history is refused outright. `--push` also publishes local memory commits.

`status` and `usage` never sync on their own. This command fetches, can rebase, and publishes the accepted-tip baseline that makes rewrite refusal durable -- none of which belongs in a command whose job is to report state.

## routines

```
openroutines routines new <name>         create a routine (inactive until you activate it)
openroutines routines list               names, schedules, grants
openroutines routines run <name>         run once now; memory writes are kept
openroutines routines test <name>        dry run: outbound tools disabled, credentials withheld,
                                         intended actions narrated, memory writes discarded
openroutines routines edit <name>        open in $EDITOR, validate on close
openroutines routines activate <name>    set active: true
openroutines routines deactivate <name>  set active: false
openroutines routines remove <name>      delete the routine and its scheduling state
```

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

`add` shows the bundle's declared authority and vendors it under `plugins/<name>/` after you confirm; installed routines land inactive. `list` shows installed plugins. `update` fetches the recorded source and three-way merges upstream changes against your vendored copy. `--yes` is required when stdin is not interactive.

## credentials

```
openroutines credentials list           credential names and which routines declare them
openroutines credentials set <name>     add or replace one value (prompted, hidden)
openroutines credentials remove <name>  refuses while any routine declares it
```

## supervise

```
openroutines supervise
```

Runs the scheduler. This is the container entrypoint; you rarely run it by hand.

## update

```
openroutines update
```

Brings the agent up to the version of the `openroutines` binary you're running: bumps the pin in `.openroutines-version`, rewrites the Dockerfile's base-image tag, and offers other framework-owned file changes interactively with a diff.

## version

```
openroutines version
```

Prints the version. Also `--version` / `-v`.
