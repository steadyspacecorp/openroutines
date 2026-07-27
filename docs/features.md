# Features

What OpenRoutines does, grouped by the problem each piece solves. The [README](../README.md) is the big-picture overview, the other pages in this directory show how to use these, and [design.md](design.md) records the full reasoning behind each decision, including the alternatives we rejected.

## Build and ship an agent like any other software

Most agent platforms want to be your runtime, your editor, and your vendor. An ORA is a git repository you already know how to build, review, deploy, and roll back.

| What | Why |
|------|-----|
| **Routines are markdown files** -- frontmatter declares the scope (schedule, trigger, skills, credentials, model, effort, timeout), the body is the prompt | Prompts are the program; they belong in files, under version control, in a format humans and models both read natively. Every grant is greppable at the top of the file that defines it. |
| **The repository is the agent** -- config, routines, skills, memory, and encrypted credentials in one git repo that deploys as one Docker container | An agent should be built, reviewed, versioned, and deployed like any other software. No database, no queue, no secrets platform to provision. |
| **Skills follow the open Agent Skills standard** -- anything written for Claude Code, Cursor, or opencode works unchanged; routines get only the skills they declare | Skills are dependencies: instructions, sometimes code, that the agent follows unattended. An open standard keeps them portable; explicit grants keep them reviewable. |
| **Variables** -- non-secret configuration in `openroutines.yaml`, injected into every run as environment variables | The interface is identical to credentials, so the only question a value ever raises is "is this secret?" -- and the answer decides where it lives. |
| **`openroutines check`** -- validates config, frontmatter, schedules, credential wiring, and config drift; made for CI | The repo is the agent, so repo review is agent review. Machine-checkable mistakes should be caught by a machine, on every push, before deploy. |
| **Deploys anywhere a container runs** -- a VPS, Fly, Render, a homelab; the only prerequisite is a git origin to push memory to | One agent, one container, nothing else to provision. If your platform can run Docker, it can run your agent. |
| **One small Go supervisor binary, versioned out of the agent repo** -- the agent pins a release; `openroutines update` bumps the pin and applies template changes as one reviewable commit | Laptop, CI, and production always agree on the framework version, and updating is a diff you review -- never a mystery. What's yours (routines, skills, memory, credentials, `opencode.json`) is never touched. |
| **Plugins** -- `openroutines plugin add` installs a copy-in bundle of routines, skills, and credential names behind a grant summary; installed routines land inactive, and the payload is allow-listed (no config files, no key material, no install hooks) | The capabilities worth sharing are exactly what you'd otherwise copy-paste between agents. Copy-first keeps the trust story one sentence: a plugin has zero special authority -- installing one produces the same files you'd have written by hand, reviewed in the same diff. |

## It remembers, reports, and improves

An agent that can't remember what worked can't get better at its job -- and an agent nobody hears from might as well not be running.

| What | Why |
|------|-----|
| **Structured memory primitives** -- one rule routes every record: events (it happened), tasks (someone must do it), context (informs future decisions), per-routine ledgers (only one routine needs it) | Any autonomous agent beyond the trivial invents these streams eventually; blessing them means nobody reinvents that wheel, and every consumer finds the same shape in every agent. |
| **Memory is a git branch** -- versioned, pushed with every change, trimmed to a retention window while history keeps everything | Memory survives redeploys and rollbacks like a database, but versioned and inspectable. Code rolls back with the image; what the agent learned persists. |
| **The memory branch is the change feed** -- a reporting routine declares `consumes: memory`, receives an inbox of changes since its cursor, and consumes all-or-nothing | Reporting becomes a per-destination view over one stream: pointing a second destination (Steady and Slack, say) at the same agent takes no changes to the routines doing the work, and nothing is delivered twice or lost. |
| **Supervisor-written memory** -- a run that falls over becomes an event or a human-owned task, and the supervisor completes its own blocker tasks when the condition heals | A run that crashes never gets to explain itself, so the framework does it mechanically -- and a three-minute outage should never read as an open blocker days later. |

## You can trust it with credentials

An unattended agent holds secrets and acts on external systems while processing content nobody vetted. The framework's job is to keep what a compromised run *holds* -- and what it can *reach* -- as small as possible.

| What | Why |
|------|-----|
| **Encrypted credentials, Rails-style** -- one master key outside the repo, per-routine grants in frontmatter, log scrubbing for every secret value | The repo stays self-contained without ever containing a usable secret. Most frameworks hand every tool every secret; here the grant is visible in the diff that adds it. |
| **Typed credentials** -- a `credentials:` entry in `openroutines.yaml` derives run-scoped material from a stored root secret (`github_app`: a one-hour installation token minted per attempt, revoked at attempt end) | A root secret exfiltrated once mints tokens until rotated. Deriving supervisor-side means the run holds only what it needs -- short-lived, revocable, and known to the log scrubber. |
| **Filesystem sandbox** -- local runs in a disposable container; production model processes confined with Landlock to an allow-list-built workspace | Model-directed execution is untrusted by definition. The run sees only what its routine needs -- not the credential store, not the deploy keys, not the supervisor's worktree. |
| **Constructed environment and git isolation** -- a run's environment is built from its grants, never inherited, and models write to a staging tree the supervisor validates before importing | An inherited environment would hand every routine the master key. Models write files; the supervisor does git -- so nothing model-directed ever touches trusted history unreviewed. |
| **No application ingress** -- the shipped container listens on no ports; triggers poll outward, logs are the only way in | An agent holding credentials and acting unattended should add zero application-level inbound attack surface. "The container listens on nothing" is a one-sentence trust story. |
| **Dry runs** -- `routines test` withholds routine credentials, denies acting tools, and discards memory writes, in the same containerized runtime as production | You can rehearse a routine against the real model without it authenticating to anything or leaving a trace -- and what you rehearse is what production runs. |

## It runs unattended without falling over

Nobody is watching, so failure has to be survivable by design: nothing silently skipped, nothing duplicated, nothing retried forever.

| What | Why |
|------|-----|
| **Cron scheduling with a durable watermark** -- missed firings collapse into one late run instead of being skipped | A scheduled moment that passes while the container is down runs late instead of never. Missed is recoverable; silently skipped is not. |
| **Persist-before-act run identity** -- every run's intent is committed before the model process spawns, and retries reuse the same run id | The run id is an idempotency key worth having: a container can act, crash, and restart without repeating the action under a fresh identity. |
| **Serial runs, skip-don't-queue, circuit breaker** -- one run at a time, overlapping firings collapse, repeated failures cool down | An unattended agent must degrade predictably: no pileups after downtime, no infinite retry loops burning model spend against a broken dependency. |
| **One instance, enforced** -- the supervisor holds a lease; memory sync rejects history rewrites | The agent is the sole writer to its memory branch. An accidental second instance waits instead of corrupting memory, and a rewritten remote never silently replaces what the agent knows. |

## It responds fast without wasting model runs

Model runs cost real money, and polling on a tight schedule buys responsiveness by burning runs that mostly find nothing.

| What | Why |
|------|-----|
| **Triggers** -- an opt-in outbound change-detection poll wakes a routine when something changed, alongside or instead of its schedule | Schedule-only routines cap responsiveness: a tight poll burns a model run per interval to mostly find nothing. A payload-free wake-up buys latency without ingress -- a spurious fire is harmless, a missed one is covered by the schedule heartbeat. |
| **Bring your own models** -- any provider opencode supports, chosen per routine with optional reasoning effort; custom endpoints (a gateway, a proxy, a self-hosted server) via a `provider` block in `opencode.json` | Model choice is a per-task decision, not a framework commitment -- a cheap model for triage, a strong one for judgment, an open-weight one behind your own gateway. |
