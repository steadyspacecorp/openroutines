# AGENTS.md

Guidance for coding agents working on the openroutines framework itself. (If you are working on a *scaffolded agent*, this is the wrong file -- see the AGENTS.md that `openroutines scaffold` placed in that repo.)

## What this is

openroutines generates and runs single-purpose autonomous AI agents: one agent, one job description, one runtime, defined as a git repo and deployed as a Docker container. Routines are markdown files with frontmatter; execution goes through headless opencode; memory lives on a dedicated git branch.

## Read before changing anything

- **README.md is the spec.** It was written before the code and the code is built against it.
- **DESIGN.md is the constitution.** ~20 decisions in Decision/Why format, including a run-lifecycle sequence diagram that doubles as the supervisor's spec. If a change contradicts a documented decision, do not make the change -- argue with the decision first (update DESIGN.md in the same PR, or stop and ask). If a change *settles* something new, record it in DESIGN.md alongside the code.

Design-first is the workflow here: behavior gets decided in DESIGN.md before it gets implemented.

## Repo layout

- `template/` -- the agent skeleton that `openroutines scaffold` stamps out. Compiled into the CLI binary via Go's `embed`; this directory is the source of truth for what every new agent looks like. Keep it consistent with DESIGN.md (frontmatter defaults, check-in routine, `agent.yaml` shape).
- `bin/` -- development scripts for working on the framework (not shipped).
- Go code (in progress): a single binary, `openroutines`, whose subcommands include the supervisor (`supervise`, the container entrypoint) plus `scaffold`, `configure`, `check`, `status`, `routines`, `skills`, `update`.

## Implementation constraints (from DESIGN.md -- the short version)

- **Go, minimal dependencies.** Target roughly one dependency (a cron parser). The supervisor is the trusted component; its dependency tree is its attack surface. Stdlib first, always.
- **The supervisor stays dumb.** Tick every minute, re-read frontmatter, run what's due -- serially, one run at a time. Enforcement lives elsewhere: skill/tool scoping in generated opencode agent definitions, filesystem scoping via Landlock, credential scoping via built-from-scratch child environments. Do not add enforcement logic to the supervisor that a lower layer can provide.
- **At-least-once, never silently skipped.** Durable two-phase runs: watermark + pending record (stable `run_id`, per-attempt ids, bounded retries) committed and pushed *before* execution; `flock` for overlap; process-group kills for timeouts.
- **Model processes never touch git.** Routines write to a disposable staged copy of memory; the supervisor validates and imports the diff into its own worktree. Never hand a routine a git worktree or metadata.
- **Secrets discipline.** Child envs are constructed, never inherited (`OPENROUTINES_MASTER_KEY` and `OPENROUTINES_DEPLOY_KEY` must never reach a routine). Injected secret values are scrubbed from log output.

## Vocabulary

Use these terms exactly; the docs and code should agree:

- **routine** (not task, not job) -- a markdown file in `routines/`
- **ORA** -- an openroutines-generated agent
- **memory primitives** -- `worklog.md`, `intentions.md`, `blockers.md`; per-routine state is a **ledger** (`memory/ledgers/<routine>.md`)
- **supervisor** -- the long-running process in the container; **run** -- one execution of one routine

## Conventions

- The project name is **openroutines** -- one word, all lowercase, everywhere, even starting a sentence. Never "Open Routines" or "OpenRoutines".
- README.md uses `--` double hyphens, not em dashes; conversational but technical tone; no marketing superlatives.
- DESIGN.md entries are `## Heading` + `**Decision.**` + `**Why.**` -- state the decision, then argue it.
- Tests: minimal and behavior-focused. Prefer exercising a real flow (scaffold a temp agent, run a routine against a stub) over unit-testing internals. Don't let testing infrastructure outgrow the thing it tests.
- Commit messages: short imperative subject; body only when the why isn't obvious.

## Verifying changes

Toolchain: Go 1.25+ (any install method; `mise.toml` pins a version for mise users but mise is not required). Routine runs execute the model process in a Docker container by default; set `OPENROUTINES_NATIVE=1` to spawn a locally installed opencode instead (the supervisor tests do this with a fake opencode on PATH). Before handing work back: `go build ./... && go vet ./... && go test ./...` plus `bin/smoke`, which builds the CLI, scaffolds a throwaway agent, and asserts `check` passes on a good agent and fails on a broken one -- CI (`.github/workflows/ci.yml`) runs exactly these. For template changes, scaffold a fresh agent and inspect the output -- the embedded template only updates when the binary is rebuilt.
