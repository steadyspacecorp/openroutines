# Design

The decisions behind Open Routines, and why we made them. The [README](README.md) says what the framework does; this document says why it works the way it does.

## One agent, one job description, one runtime

**Decision.** An Open Routines-generated agent (ORA) is a single agent with a single mandate, defined in `agent.yaml`. There is no fleet management, no orchestration graph, no agent-to-agent protocol.

**Why.** Most of the complexity -- and most of the failure modes -- in agent frameworks comes from coordination between agents. A single agent with a clear job description is easy to reason about, easy to secure, and easy to hold accountable ("what did it do?" is one log and one memory). If you need two jobs done, deploy two agents. This also makes the security model tractable: exactly one runtime means exactly one writer to memory, one credential set, one blast radius.

## The repository is the agent

**Decision.** Everything the agent is -- configuration, routines, skills, structured memory, encrypted credentials -- lives in one git repository. `openroutines scaffold` stamps out a new agent repo from the template embedded in the CLI binary (`template/` in this repo is its source, compiled in via Go's `embed`).

**Why.** Agents-as-platform-accounts hold your agent's identity and memory hostage. Agents-as-repos get the entire mature software lifecycle for free: versioning, review, rollback, CI/CD, forking, diffing. Switching deployment platforms is `git push` and `docker run`. It also means changing your agent's behavior is a reviewable diff, not a click in someone's dashboard.

## Routines are markdown files

**Decision.** Each routine is one markdown file in `routines/`. Frontmatter declares the scope — `schedule`, `timeout`, `active`, `skills`, `credentials`, `model` — and the body is the prompt. Every field but `schedule` is optional: `active` defaults to true, `model` and `timeout` fall back to the `agent.yaml` defaults, and `skills` and `credentials` default to empty — no grants. Deactivating a routine is therefore always an explicit `active: false`, visible in the diff.

**Why.** Prompts are the program; they belong in files, under version control, in a format both humans and models read natively. Frontmatter makes every grant explicit and greppable: what a routine can touch is declared at the top of the file that defines it, not assembled at runtime. Agent-level defaults live in `agent.yaml`; frontmatter overrides them.

## Scheduling: a tick loop with catch-up, not cron

**Decision.** A supervisor process in the container wakes every minute, re-reads routine frontmatter, and fires whatever is due. Due-ness is computed against a persisted per-routine `last_run` (anacron-style catch-up), not just "does the cron expression match this minute."

**Why.** Re-reading frontmatter every tick means schedule changes take effect on the next push with no registration step and no restart — the files are the schedule. Catch-up semantics mean a routine whose moment passes during a redeploy or downtime fires late instead of never; for an unattended agent, "late" is recoverable and "silently skipped" is not. The consequence is **at-least-once** execution: a run killed mid-flight (redeploy, crash) never updated `last_run`, so it re-fires on next boot. Routines should therefore be written to tolerate re-running.

## Overlap: kernel locks, skip-don't-queue

**Decision.** Before spawning a routine, the supervisor takes a non-blocking `flock` on a per-routine lock file. If the lock is held, the run is skipped and logged. There is no queue.

**Why.** Hand-rolled lock files go stale when a process dies uncleanly, silently deadlocking the routine forever — the worst failure mode for unattended software. `flock` locks are released by the kernel when the holder dies, however it dies; staleness is structurally impossible. Skipping (rather than queueing) keeps semantics simple: a routine that is still running *is* the current run.

## Timeouts kill the process group

**Decision.** Every run has a timeout (`agent.yaml` default, frontmatter override). On expiry the supervisor signals the routine's entire process group — SIGTERM, a grace period, then SIGKILL — and records the outcome.

**Why.** A routine run spawns children (tools, shells); signalling only the direct child leaks grandchildren. Recording outcomes (`completed` / `timeout` / `crashed`, duration) into memory gives run history that survives redeploys and reads as a git log.

## Execution: headless opencode, fresh session per run

**Decision.** Routines execute via `opencode run` — headless, `--format json`, model from frontmatter, a fresh session every run. Session continuation is never used. Tool permissions come from a committed `opencode.json` policy, not blanket auto-approval.

**Why.** opencode already does the hard part — the agentic loop, tool use, model wrangling — and ships as a self-contained binary. Fresh sessions keep all continuity in the repo's memory directory: if it's not in the repo, the agent doesn't know it. That's what makes memory portable, inspectable, and git-backed rather than state held hostage in a session store. A committed permission policy means the agent's tool surface is reviewed like code.

## Memory: a dedicated directory on its own branch

**Decision.** Agent memory lives in a dedicated directory, synced to an orphan `memory` branch that the running agent — the branch's sole writer — pushes with a read/write deploy key. `last_run` state and run records live there too. If the branch doesn't exist at boot, the supervisor creates and pushes the empty orphan branch — first boot self-heals; there is no setup step. All of this presumes the repo has a git origin: deploying an ORA requires one (any git host — GitHub, GitLab, Gitea, a bare repo on a VPS), because without an origin, memory has nowhere durable to live. Local development needs no origin; `openroutines check` verifies one exists before you deploy.

**Why.** The analogy is Docker's own: `main` is the image (immutable, what you deploy), the `memory` branch is the volume (mutable, survives redeploys). Keeping memory off `main` means agent commits never trigger CI/CD, never race human pushes, and never pollute the history of human intent. One agent, one runtime → one writer → pushes always fast-forward; the conflict problem is designed out rather than handled. Code rolls back; memory persists — like a database. Reviewing what your agent has learned is `git log memory`. Humans may curate the branch (pruning bad learnings is part of maintaining an agent); the agent pulls before each run. By convention — taught by the template's `AGENTS.md`, not enforced by the framework — each routine keeps a ledger file named after itself (`memory/<routine>.md`) recording what it examined and decided, and prunes that ledger as part of its own prompt. Memory hygiene is part of the job description, not a framework feature.

Mechanically, `memory/` is a **git worktree** of the `memory` branch, ignored by `main` and created lazily by the CLI (locally) or the supervisor (in the container) — one directory, two histories. Routine edits and memory curation are separate commits on separate branches by construction: `git status` on `main` never shows memory churn, and a human curates by committing inside the directory (`git -C memory commit && git -C memory push`). Local runs and production runs touch memory through the identical path, which is what makes local testing faithful. `openroutines status` should surface uncommitted memory-worktree changes, since root `git status` won't.

## Credentials: encrypted in the repo, scoped per routine

**Decision.** Secrets ship encrypted in the repository, Rails-style: one encrypted file, one master key kept out of the repo (a gitignored file locally, an environment variable in production). A routine's process environment receives only the credentials its frontmatter declares (`slack_webhook` → `SLACK_WEBHOOK`), decrypted in memory at spawn time — never written to disk. Three rules make the scoping real:

1. **Routine environments are built from scratch, never inherited.** The supervisor's own env holds `OPENROUTINES_MASTER_KEY` and `OPENROUTINES_DEPLOY_KEY`; children get a minimal constructed env (PATH, HOME, TZ, declared credentials) and nothing else. The `OPENROUTINES_*` prefix is reserved — `openroutines check` rejects credential names that collide.
2. **Model provider keys auto-inject by provider.** They live in the same encrypted file under reserved names; a routine declaring `model: anthropic/...` receives `ANTHROPIC_API_KEY` and no other provider's key, with no frontmatter boilerplate.
3. **Injected secrets are scrubbed from logs.** The supervisor knows every value it injected and redacts them from the routine's log stream (the GitHub Actions trick), so an echoed secret — accidental or prompt-injected — never reaches the log platform.

**Why.** The Rails model is battle-tested and requires no secrets platform — the repo stays self-contained without ever containing a usable secret. Per-routine scoping is the substantive security claim: most frameworks hand every tool every secret; here the daily-digest routine cannot read the deploy token, and the grant is visible in the diff that adds it. The clean-env rule is what makes that claim true rather than decorative — an inherited environment would hand every routine the master key, which unlocks everything. And because logs are the only way into a deployed agent, scrubbing is the difference between "a routine echoed a secret" being a non-event and being an incident.

## No open ports

**Decision.** A deployed ORA listens on nothing. There is no admin UI, no webhook receiver, no chat gateway. Routines may reach *out* to networked services through their skills; logs are the only way in.

**Why.** An agent that holds credentials and acts unattended should have the smallest possible inbound attack surface, and the smallest possible surface is zero. Anything that needs to talk to the agent does so through the repo (routines, memory) or not at all. Interactive development happens locally, with your own coding agent, against `AGENTS.md`.

## The supervisor is a small Go binary

**Decision.** The supervisor — tick loop, locks, timeouts, spawn, git sync — is written in Go, targeting a dependency tree of roughly one (a cron-expression parser).

**Why.** The supervisor is the trusted component: it holds the master key and the deploy key, so its dependency tree is its attack surface. Go's stdlib covers processes, signals, and locking natively, compiles to a static binary, and keeps the image minimal (supervisor + opencode + git, distroless-friendly). It is also the lingua franca of exactly this genre of infrastructure — the audience that runs containers expects the runner to be a small static binary they can read in one sitting. TypeScript would have meant an npm tree inside the trust boundary; Rust buys safety this program doesn't need at a contribution cost it would feel.

## Framework code is versioned out of the agent repo

**Decision.** All framework logic lives in one released Go binary — `openroutines` — whose subcommands include the supervisor (`openroutines supervise`, the container entrypoint) as well as `scaffold`, `configure`, `status`, `routines`, and `skills`. An agent repo carries only a version pin (`.openroutines-version`) and a Dockerfile that installs the pinned binary by checksum. Locally you install `openroutines` once (installer script or Homebrew); the binary reads the repo's pin and warns on mismatch (Bundler-style re-exec of the pinned version is a possible later upgrade). There are no `bin/` shims. `openroutines update` bumps the pin, applies any template-file changes interactively (`rails app:update`-style), and leaves a single reviewable commit.

**Why.** Template repos rot: agent repos are born from `template/` and immediately diverge, so shipping framework *code* into them strands every existing agent on the version it was scaffolded with — and fork-and-merge from upstream conflicts the moment a user touches anything. Shipping a version *pin* instead makes drift structurally impossible for the whole binary surface; only the Dockerfile (a file users rarely edit) ever needs a guided merge. The container is where the pin is enforced — the Dockerfile installs that exact release by checksum, so deployed behavior is reproducible; locally the binary checks the pin and complains about skew. Updating is one commit; rolling back an update is `git revert`. The costs: a one-time local install step (no clone-and-run), and maintaining tagged releases with prebuilt binaries, which GoReleaser makes cheap.

## Skills are Agent Skills, enforced by opencode

**Decision.** Skills follow the open [Agent Skills](https://agentskills.io/) standard — a folder with a `SKILL.md` (name + description frontmatter, instructions in the body, optional scripts and references) — living in the agent repo's `skills/` directory, exposed to opencode through its skill-discovery paths. Scoping is enforced by opencode, not the supervisor: for each routine, `openroutines` generates an opencode agent definition whose permission block denies all skills by default and allows exactly the ones the routine's `skills:` frontmatter declares (same for tool permissions), and the supervisor spawns `opencode run --agent <routine>`. Generated definitions are derived from routine frontmatter and gitignored — frontmatter stays the single reviewable source of truth. Permission values are only ever `allow` or `deny`, never `ask`: an unattended agent has no one to ask.

**Why.** Adopting the standard means any skill written for Claude Code, Cursor, opencode, or the rest of the ecosystem drops into an ORA unchanged — skills are the part of an agent worth sharing, and a proprietary format would orphan them. Enforcement belongs in the layer that executes tools: only opencode can actually stop a run from using bash or loading a skill, and its `deny` semantics *hide* undeclared skills from the model rather than merely refusing them — no context leak, no supervisor code to write. The supervisor stays what it should be: a dumb, auditable scheduler.

Verified by spike against opencode 1.17.12: per-agent permission blocks compose with skill patterns — a denied skill is absent from the model's system-prompt skill listing, refused on a by-name load, and the wildcard deny also suppresses machine-global skills (`~/.claude/skills/` etc.), making local runs reproducible. Implementation notes: rule order is significant (emit `"*": deny` before specific allows — last match wins), and `opencode agent list` statically validates a generated definition, which `openroutines check` uses.

Be honest about the boundary: skill permissions gate the skill *tool*, not the filesystem — a routine with read or bash access can still open an undeclared skill's `SKILL.md` directly. Skill scoping is context and capability hygiene, not a sandbox. The hard security boundary is credentials: an undeclared secret isn't hidden from a routine, it does not exist in its process environment.

## Memory syncs per run, atomically

**Decision.** After every run, the supervisor makes exactly one memory commit — the routine's memory writes plus the supervisor's run record — and pushes it. If a run fails (timeout, crash, kill), the routine's uncommitted memory writes are discarded; only the run record is committed. A failed push never blocks: commits accumulate locally and the next successful push carries them.

**Why.** Run frequency is low, so per-run push costs nothing and keeps origin at most one run behind. Discarding a failed run's writes gives memory run-level atomicity: the `memory` branch only ever contains the output of *completed* runs, so a half-finished run can never corrupt the context future runs read — the re-fired run redoes its work from clean state. One commit per run also makes `git log memory` a legible run journal: one entry, one run.

## Shutdown: terminate fast, lean on at-least-once

**Decision.** On SIGTERM the supervisor stops launching routines, signals in-flight routine process groups (the same SIGTERM → grace → SIGKILL as a timeout), discards their uncommitted memory writes, records the interruptions, makes a final memory commit-and-push, and exits. There is no drain mode. Recommended `docker stop` timeout: 30 seconds.

**Why.** Draining would hold every deploy hostage to the longest in-flight routine for a benefit the design already provides two other ways: an interrupted run never updated `last_run`, so it re-fires on the next boot, and atomic memory sync means it left nothing half-written behind. Fast shutdown keeps redeploys boring — seconds, not routine-timeout minutes — at the cost of occasionally redoing work, which at-least-once semantics already told routine authors to tolerate.

## Runs are sandboxed — and local runs use the production container

**Decision.** Every routine run is filesystem-sandboxed by the supervisor using Landlock (Linux kernel 5.13+): before exec, the routine's process group is restricted to read access on the repo minus undeclared skill directories, and write access to `memory/` and a per-run tmp dir — nothing else, enforced at the syscall level, no root required. The generated opencode agent definitions additionally carry `read` permission patterns denying undeclared skill paths. On hosts whose kernel lacks Landlock, the supervisor warns loudly and runs without it rather than failing.

And local runs go through the same container: `openroutines routines run` builds (or reuses) the agent's image and executes the run inside it with the working copy mounted as a volume, via the same run-once code path the supervisor uses in production. Docker is therefore a prerequisite for `routines run`/`test` — but not for `scaffold`, `configure`, `check`, or `routines new`, which stay native and instant.

**Why.** Skill-permission gating (see above) is context hygiene, not a wall — a routine with bash can `cat` any file it can reach. Landlock turns the declare-or-it-doesn't-exist principle into kernel enforcement, the same way clean-env construction does for credentials: a third enforcement point, built from scratch per run. Running locally through the container is what makes the sandbox — and everything else — identical in both places: same image, same opencode version, same env construction, same kernel-level restrictions (Docker Desktop's VM kernel supports Landlock). "Test locally exactly as it runs in production" becomes mechanically true rather than approximately true, and the works-locally-breaks-in-prod bug class is structurally gone. The volume mount keeps the dev loop fast (no rebuild per routine edit) and writes memory into the local worktree, where it stays until pushed. What sandboxing still doesn't cover: network egress — a routine can exfiltrate what it legitimately holds. Landlock grew TCP scoping in kernel 6.7; per-routine network rules are a plausible later tier, and the credential scoping already caps the blast radius.

## Open questions

None at the moment. The last one — whether opencode's per-agent permission overrides compose with pattern-based skill permissions — was confirmed by spike (see "Skills are Agent Skills, enforced by opencode").
