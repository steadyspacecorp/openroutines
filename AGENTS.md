# AGENTS.md

Guidance for coding agents working on the OpenRoutines framework itself. (If you are working on a *scaffolded agent*, this is the wrong file -- see the AGENTS.md that `openroutines scaffold` placed in that repo.)

## What this is

OpenRoutines generates and runs single-purpose autonomous AI agents: one agent, one job description, one runtime, defined as a git repo and deployed as a Docker container. Routines are markdown files with frontmatter; execution goes through headless opencode; memory lives on a dedicated git branch.

## Read before changing anything

- **README.md and the docs/ pages are the spec.** They were written before the code and the code is built against them. README.md is the big-picture overview; the task-oriented pages in `docs/` carry the detail.
- **docs/design.md is the constitution.** ~20 decisions in Decision/Why format, including a run-lifecycle sequence diagram that doubles as the supervisor's spec. If a change contradicts a documented decision, do not make the change -- argue with the decision first (update docs/design.md in the same PR, or stop and ask). If a change *settles* something new, record it in docs/design.md alongside the code.

Design-first is the workflow here: behavior gets decided in docs/design.md before it gets implemented.

## Repo layout

- `template/` -- the agent skeleton that `openroutines scaffold` stamps out. Compiled into the CLI binary via Go's `embed`; this directory is the source of truth for what every new agent looks like. Keep it consistent with docs/design.md (frontmatter defaults, check-in routine, `openroutines.yml` shape).
- `bin/` -- development scripts: `smoke` (CI's end-to-end test) and `release` (guides you through picking the next version, then tags and pushes; the release workflow runs goreleaser to build and publish the GitHub release, and the pages workflow republishes the install site).
- `www/` -- the public install site (`install.sh`), served at `get.openroutines.dev`: `.github/workflows/pages.yml` deploys it to GitHub Pages together with mirrored release binaries and a `version.txt`, so agent Dockerfiles install the pinned release without registry credentials even while this repository is private.
- `testdata/plugins/` -- three synthetic plugin fixtures (`reporter`, `watcher`, `toolkit`) that `bin/smoke` installs; together they cover the plugin feature matrix (consumer + skill + raw credential + MCP declaration, typed credential + variable + ledger stub, skills-only). Real reference plugins live in the separate openroutines-plugins repository.
- Go code: a single binary, `openroutines`, whose subcommands include the supervisor (`supervise`, the container entrypoint) plus `scaffold`, `configure`, `check`, `status`, `usage`, `sync`, `routines`, `skills`, `plugin`, `credentials`, and `update`.

## Implementation constraints (from docs/design.md -- the short version)

- **Go, minimal dependencies.** Target roughly one dependency (a cron parser). The supervisor is the trusted component; its dependency tree is its attack surface. Stdlib first, always.
- **The supervisor stays dumb.** Tick every minute, re-read frontmatter, run what's due -- serially, one run at a time. Enforcement lives elsewhere: skill, tool, web-access, and MCP scoping in generated opencode agent definitions, filesystem scoping via Landlock, credential scoping via built-from-scratch child environments. Do not add enforcement logic to the supervisor that a lower layer can provide.
- **At-least-once, never silently skipped.** Durable two-phase runs: watermark + pending record + per-attempt reservation (stable `run_id`, per-attempt ids, bounded retries) committed and pushed *before* execution; `flock` for overlap; process-group kills for timeouts.
- **Model processes never touch git.** Routines write to a disposable staged copy of memory; the supervisor validates and imports the diff into its own worktree. Never hand a routine a git worktree or metadata.
- **Secrets discipline.** Child envs are constructed, never inherited -- every child the supervisor spawns, git and the boot probe included, not just routines (`OPENROUTINES_MASTER_KEY` and `OPENROUTINES_DEPLOY_KEY` must never leave the supervisor's own process). Injected secret values are scrubbed from log output.

## Vocabulary

Use these terms exactly; the docs and code should agree:

- **routine** (not job) -- a markdown file in `routines/` (agent-owned) or `plugins/<name>/routines/` (plugin-owned); the filename is its globally unique identity. A **task** is a memory record in `tasks.md`, never a synonym for routine
- **ORA** -- an OpenRoutines agent
- **memory primitives** -- `events.md`, `tasks.md`, `context.md`; per-routine state is a **ledger** (`memory/ledgers/<routine>.md`); per-consumer delivery cursors live under supervisor-owned `state/`
- **supervisor** -- the long-running process in the container; **run** -- one execution of one routine

## Conventions

- The project is **OpenRoutines** in prose and `openroutines` in code, commands, URLs, paths, and other machine identifiers. Never "Open Routines". (docs/design.md "The name".)
- README.md and the `docs/` pages use `--` double hyphens, not em dashes; conversational but technical tone; no marketing superlatives.
- docs/design.md entries are `## Heading` + `**Decision.**` + `**Why.**` -- state the decision, then argue it.
- Documentation paragraphs use one physical line per paragraph; do not add hard line breaks for column wrapping.
- Tests: minimal and behavior-focused. Prefer exercising a real flow (scaffold a temp agent, run a routine against a stub) over unit-testing internals. Don't let testing infrastructure outgrow the thing it tests.
- Commit messages: short imperative subject; body only when the why isn't obvious.

## Verifying changes

Toolchain: Go 1.25+ (any install method; `mise.toml` pins a version for mise users but mise is not required). Routine runs execute the model process in a Docker container by default; set `OPENROUTINES_NATIVE=1` to spawn a locally installed opencode instead (the supervisor tests do this with a fake opencode on PATH). Style is standard Go ([Effective Go](https://go.dev/doc/effective_go) plus the [Go style guide](https://google.github.io/styleguide/go/)), enforced by golangci-lint with the lean config in `.golangci.yml` (`brew install golangci-lint`) -- add a linter only when it catches a real bug class. Before handing work back: `golangci-lint run && go build ./... && go test ./...` plus `bin/smoke`, which builds the CLI, scaffolds a throwaway agent, and exercises the CLI end to end -- `check` on good and broken agents, installing and updating the reference plugins, credentials, `update` -- CI (`.github/workflows/ci.yml`) runs exactly these. For template changes, scaffold a fresh agent and inspect the output -- the embedded template only updates when the binary is rebuilt.
