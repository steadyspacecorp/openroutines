# AGENTS.md

Guidance for coding agents working on the OpenRoutines framework itself. (If you are working on a *scaffolded agent*, this is the wrong file -- see the AGENTS.md that `openroutines scaffold` placed in that repo.)

## What this is

OpenRoutines generates and runs single-purpose autonomous AI agents: one agent, one job description, one runtime, defined as a git repo and deployed as a Docker container. Routines are markdown files with frontmatter; execution goes through headless opencode; knowledge lives on a dedicated git branch.

## Sources of truth

The README and docs describe the intended user contract and design rationale, not automatic authority over the implementation.
Verify their claims against code and tests, and resolve disagreements by updating whichever side is stale.

Add a Decision/Why entry to docs/design.md only for a durable behavior choice whose rationale a future maintainer will need.

## Ownership

- `template/` is embedded into the CLI and is the source of every scaffolded agent.
- `supervisor` plans and coordinates runs; `runner` prepares, executes, and settles attempts; `knowledge` owns persistence and import; `run` owns run identities and the persisted attempt record schema.

## System invariants

- **Keep supervisor dependencies narrow.** The supervisor is trusted; prefer the standard library.
- **The supervisor stays dumb.** Tick every minute, re-read frontmatter, run what's due -- into a bounded pool of run slots (`concurrency`, serial unless the agent opts in), with every knowledge-worktree operation serialized behind one lock. Enforcement lives elsewhere: skill, tool, web-access, and MCP scoping in generated opencode agent definitions, filesystem and process scoping via the per-run sandbox, credential scoping via built-from-scratch child environments. Do not add enforcement logic to the supervisor that a lower layer can provide.
- **At-least-once, never silently skipped.** Durable two-phase runs: watermark + pending record + per-attempt reservation (stable `run_id`, per-attempt ids, bounded retries) committed and pushed *before* execution; `flock` for overlap; process-group kills for timeouts.
- **Model processes never touch git.** Routines write to a disposable staged copy of knowledge; the supervisor validates and imports the diff into its own worktree. Never hand a routine a git worktree or metadata.
- **Secrets discipline.** Child envs are constructed, never inherited -- every child the supervisor spawns, git and the boot probe included, not just routines (`OPENROUTINES_MASTER_KEY` and `OPENROUTINES_DEPLOY_KEY` must never leave the supervisor's own process). Injected secret values are scrubbed from log output.

## Vocabulary

Use these terms consistently across code, logs, and docs:

- A **routine** is a markdown file in `routines/` or `plugins/<name>/routines/`; a **task** is a knowledge record in `tasks.md`.
- **ORA** -- an OpenRoutines agent
- **knowledge primitives** -- `events.md`, `tasks.md`, `context.md`; per-routine state is a **ledger** (`knowledge/ledgers/<routine>.md`); each reporting routine's delivery cursor lives under supervisor-owned `state/`
- **teamwork primitives** -- events, the schedule (`schedule.md`), and the report (`changes.md`, consumed by a routine declaring `reports: true`)
- The **supervisor** is the long-running process in the container; a **run** is one logical execution of a routine; an **attempt** is one try within that run.

## Conventions

- The project is **OpenRoutines** in prose and `openroutines` in machine identifiers.
- README.md and the `docs/` pages use `--` double hyphens, not em dashes; conversational but technical tone; no marketing superlatives.
- Documentation paragraphs use one sentence per physical line, a blank line between paragraphs -- no column-wrapping, and no giant one-line paragraphs either. A single-sentence edit should produce a single-line diff, not a whole-paragraph one.
- Code comments default to none.
  Write one only to explain something non-obvious -- a hidden invariant, a rejected alternative, a security or kernel gotcha, or the reason behind a magic number.
  Write for the future reader at that location: do not repeat the name or restate what the adjacent code already says.
  Do not treat existing comment density as precedent; every new comment must justify itself independently.

## Verifying changes

Run `make verify` before handoff; CI runs the equivalent gates.
For template changes, rebuild the CLI, scaffold a fresh agent, and inspect the generated output.
