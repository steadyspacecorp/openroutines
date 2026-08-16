# Extensibility

## Skills are Agent Skills, enforced by opencode

**Decision.** Skills follow the open [Agent Skills](https://agentskills.io/) standard -- a folder with a `SKILL.md` (name + description frontmatter, instructions in the body, optional scripts and references).
Agent-owned skills live in the agent repo's `skills/` directory; plugin-owned skills remain grouped in `.openroutines/plugins/<plugin>/skills/`.
Skill names are globally unique because routine grants name them without a path.
OpenRoutines copies only a routine's declared skills from either source into opencode's skill-discovery path for its run.
Scoping is enforced by opencode, not the supervisor: for each routine, `openroutines` generates an opencode agent definition whose permission block denies all skills by default and allows exactly the ones the routine's `skills:` frontmatter declares (web access and MCP tools get explicit rules the same way; built-in tools are not denied), and the supervisor spawns `opencode run --agent <routine>`.
Generated definitions are derived from routine frontmatter and gitignored -- frontmatter stays the single reviewable source of truth.
Permission values are only ever `allow` or `deny`, never `ask`: an unattended agent has no one to ask.

**Why.** Adopting the standard means any skill written for Claude Code, Cursor, opencode, or the rest of the ecosystem drops into an ORA unchanged -- skills are the part of an agent worth sharing, and a proprietary format would orphan them.
The flip side: a skill is an executable dependency your agent will follow unattended, so vendoring one into `skills/` is importing code -- review it like code, and pin where it came from.
Enforcement belongs in the layer that executes tools: only opencode can actually stop a run from using bash or loading a skill, and its `deny` semantics *hide* undeclared skills from the model rather than merely refusing them -- no context leak, no supervisor code to write, and the supervisor stays a dumb, auditable scheduler.

Verified by spike against opencode 1.17.12: per-agent permission blocks compose with skill patterns -- a denied skill is absent from the model's system-prompt skill listing, refused on a by-name load, and the wildcard deny also suppresses machine-global skills (`~/.claude/skills/` etc.), making local runs reproducible.
Implementation notes: rule order is significant (emit `"*": deny` before specific allows -- last match wins), and `opencode agent list` statically validates a generated definition, which `openroutines check` uses.

The boundary: skill permissions gate the skill *tool*, not the filesystem -- a routine with read or bash access can still open an undeclared skill's `SKILL.md` directly.
Skill scoping is context and capability hygiene, not a sandbox.
The hard security boundary is credentials: an undeclared secret isn't hidden from a routine, it does not exist in its process environment.

## Plugins: grouped, vendored, and updateable

**Decision.** A plugin is a git repository (or a contained subdirectory of one, `--path`) whose layout mirrors a slice of the agent: a `PLUGIN.md` manifest plus any non-empty subset of `routines/`, `skills/`, and `knowledge/ledgers/` stubs.
The manifest's strictly decoded frontmatter declares `name`, `description`, a `credentials:` block naming the credentials the bundle needs -- names, a per-credential description, and for typed credentials the `openroutines.yml` metadata to add; never values -- a `variables:` block naming required non-secret configuration the same way, and an `mcp:` block declaring each MCP server the bundle's routines grant: description, URL, and the manifest credential its auth header references.
The MCP block is a declaration of need, never configuration -- an MCP entry is an endpoint plus auth headers, exactly what a plugin must not author -- and every `mcp:` grant in a plugin routine must be declared there (strict like credentials, not lenient like skills), so the grant summary tells the whole story.
At install, each declared server is offered one at a time as an interactive consent showing the exact JSON that would land in `opencode.json`; only a live `y` writes it, `--yes` and non-interactive installs only print the snippet as a next step, an already-defined name is never overwritten, and `plugin update` never writes `opencode.json` at all.
The body is the plugin's README, shown at install.
Plugin names are bare domain names (`steady`, `slack`, `github-docs`) -- the context always supplies the noun -- and a repo hosting one standalone plugin is named `openroutines-plugin-<name>`, the Terraform convention.
Reference plugins live in the [openroutines-plugins](https://github.com/steadyspacecorp/openroutines-plugins) repository, not in this one, so the framework repo and the plugin catalog cannot drift.
The smoke test exercises the install contract against synthetic fixtures in `testdata/plugins/` covering the feature matrix -- a consumer routine with a bundled skill, raw credential, and declared MCP server; a routine-only bundle with a typed credential, required variable, and ledger stub; a skills-only bundle -- so framework CI never depends on a real plugin's contents.

`openroutines plugin add <repo>` clones with the same hardening as `skills add` (no ext transport, argument-terminated URL, regular files only, no nested `.git`), validates the whole payload before touching anything, and prints a **grant summary** -- every routine with its schedule, trigger poll URL, model, credentials, skills, MCP servers, and web access.
The summary is an authority overview, not review of executable behavior: routine bodies and every shipped skill file remain supply-chain input that the person must review.
An interactive install copies only after confirmation; a non-interactive install refuses unless the caller passes `--yes`.
The payload is allow-listed, and violation is refusal, not a skipped file: a plugin shipping `opencode.json`, `openroutines.yml`, a Dockerfile, nested git metadata, or executable install hooks of any kind is rejected outright.
Install is rollback-safe and places the complete reviewed payload under `.openroutines/plugins/<name>/`, alongside framework-written `.openroutines-source.yaml` provenance containing the normalized git source, optional subdirectory, and full commit revision.
Routine filenames remain flat logical identities used by schedules, ledgers, locks, and commands: an agent-owned routine shadows the same name from any plugin, while two plugins may not claim one name; skill names need no precedence rule because routine grants refer to them by name.
Ledger stubs are seeds, not managed dependencies: copied into an existing knowledge worktree only at install, otherwise listed as follow-up, and neither add nor update ever replaces live knowledge.

Every installed routine is forced to `active: false`, regardless of the plugin's requested setting, so review, configuration, and explicit `routines activate` happen before the supervisor can execute it.
Internal consistency is required: a skill a plugin routine declares must ship in the plugin or already exist in the agent.
`openroutines plugin list` reports installed provenance.
`openroutines plugin update <name>` fetches the recorded source, validates the new payload before modifying the agent, shows the revision change, file changes, and new grant summary, and requires the same confirmation discipline as add.
Updates are a three-way merge -- recorded upstream revision as base, the locally committed vendored tree as ours, the new upstream revision as theirs -- so local edits and activation choices survive; conflicts are left for ordinary review without advancing the recorded revision, a newly added routine is always forced inactive, and an upstream deletion removes an unchanged file but conflicts with a locally edited one.
The provenance revision advances only after a clean, rollback-safe update.
There is no automatic adoption of files installed by the earlier copy-in implementation: top-level files remain agent-owned, and the few alpha agents using it are migrated manually.

**Why.** The capabilities worth sharing between agents -- a Steady check-in, a Slack reporter -- are exactly one or more routines, their skills, and the names of the credentials to fill in; without a bundle format that trio gets copy-pasted between agent repos and drifts.
Grouping the vendored source makes ownership and provenance evident from browsing the repository, while keeping it ordinary reviewed code with no runtime fetch, special privilege, or nested repository, enforced by the same `check`, frontmatter grants, and sandbox as agent-owned files.
It is still executable supply-chain material: a routine holding a credential can direct its ordinary tools to send that credential somewhere the summary cannot infer, so activation and every update wait for review of the actual diff.
`--yes` makes bypassing the interactive pause an explicit automation decision rather than an ambient property of redirected stdin.
The refusals carry boundaries drawn elsewhere: config files stay out because each belongs to the system that interprets it (a plugin writing `opencode.json` could grant itself permissions); derivation code stays out because credential types are framework code on the trusted side; install hooks stay out because install-time code execution would make plugin management itself the attack surface, and a bundle that needs a hook is hiding work the diff should show.
Recording one source revision inside each grouped subtree supplies enough state for reproducible provenance and a standard three-way merge without turning the agent into a package-manager database.
A plugin without routines remains legitimate (a skills-only plugin whose routines you write yourself); the value over `skills add` is the bundle, credential wiring, provenance, and grant summary, not the routine count.

