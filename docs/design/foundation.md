# Foundation

## One agent, one job description, one runtime

**Decision.** An OpenRoutines agent (ORA) is a single agent with a single mandate, defined in `openroutines.yml`.
Its name and description are standing context, not repository metadata: the runtime injects both into every run before the routine-specific instructions.
The description states the durable mandate; routine bodies define the individual recurring ways the agent carries it out.
There is no fleet management, no orchestration graph, no agent-to-agent protocol.

**Why.** Most of the complexity -- and most of the failure modes -- in agent frameworks comes from coordination between agents.
A single agent with a clear job description is easy to reason about, easy to secure, and easy to hold accountable: "what did it do?" is one log and one knowledge.
If you need two jobs done, deploy two agents.
It also makes the security model tractable: exactly one runtime means exactly one writer to knowledge, one credential set, one blast radius.

## The repository is the agent

**Decision.** Everything the agent is -- configuration, routines, skills, structured knowledge, encrypted credentials -- lives in one git repository.
`openroutines scaffold` stamps out a new agent repo from the template embedded in the CLI binary (`template/` in this repo is its source, compiled in via Go's `embed`).
The reviewed repository is trusted source code: whoever can change its routines, skills, configuration, or image can change the agent's behavior and authority.
The security boundary isolates model-directed execution and external content from supervisor authority; it does not isolate an agent from its own source code or administrators.
[SECURITY.md](../../SECURITY.md) defines that threat model precisely.

The framework's configuration file is `openroutines.yml`, named for the system that reads it -- the same rule that keeps `opencode.json` the harness's file -- and spelled `.yml` like `.openroutines/credentials.yml.enc` (#50).
The earlier spellings `openroutines.yaml` and the original `agent.yaml` are still read, so a pinned agent migrates when its operator chooses; `check` nudges the rename, and `Save` writes back to whichever name the repository actually has.

Framework-managed and vendored repository content lives under `.openroutines/`: the release pin `.openroutines/version`, the encrypted credential store `.openroutines/credentials.yml.enc`, and installed plugins under `.openroutines/plugins/<plugin>/`.
The primary manifest stays at the root because it defines the agent, while the hidden directory keeps framework bookkeeping and third-party bundles from crowding agent-authored routines, skills, and knowledge.
Plugin routines and skills remain reviewable authority despite their location: CLI output and documentation always name their full paths, and their names share the same global namespaces as agent-owned files.

**Why.** Agents-as-platform-accounts hold your agent's identity and knowledge hostage.
Agents-as-repos get the entire mature software lifecycle for free: versioning, review, rollback, CI/CD, forking, diffing.
Switching deployment platforms is `git push` and `docker run`; changing your agent's behavior is a reviewable diff, not a click in someone's dashboard.

## Routines are markdown files

**Decision.** Each routine is one markdown file: agent-owned routines live in `routines/`, plugin-owned ones in `.openroutines/plugins/<plugin>/routines/`.
Frontmatter declares the scope -- `schedule`, `trigger`, `timeout`, `url`, `active`, `skills`, `credentials`, `webfetch`, `websearch`, `mcp`, `model`, `effort` (provider-specific reasoning effort, passed to opencode as `--variant`), `teamwork`, `reports` -- and the body is the prompt.
Every field is optional except that at least one of `schedule` and `trigger` must be present.
Defaults: `active` is true; `teamwork` is `full`, except that `reports: true` defaults it to `off`; `url` is `https://openroutines.dev` and arrives in the run as `$OPENROUTINES_URL`; `model` and `timeout` fall back to the `openroutines.yml` defaults; `skills`, `credentials`, and web access grant nothing.
Deactivating a routine is therefore always an explicit `active: false`, visible in the diff.
**The filename is the routine's globally unique identity**, with one explicit precedence rule: an agent-owned `routines/<name>.md` shadows a plugin-owned routine with the same filename.
That lets an agent replace a vendored implementation without editing the plugin and losing the override on update, and `openroutines check` names every shadowed plugin path so the effective source is explicit.
Two plugins claiming the same filename remain an error; a plugin name is provenance, not a namespace.
Scheduling state, ledgers, and run records all key on the filename, so the winning routine continues the same identity.
Renaming a routine is retiring one routine and starting another (fresh watermark, orphaned state for the old name).
We tried a generated frontmatter `id` for rename-safety and removed it: one more field in every file wasn't worth insuring an occasional, understandable event.

**Why.** Prompts are the program; they belong in files, under version control, in a format both humans and models read natively.
Frontmatter makes every grant explicit and greppable: what a routine can touch is declared at the top of the file that defines it, not assembled at runtime.
Agent-level defaults live in `openroutines.yml`; frontmatter overrides them.
A canonical URL is ordinary non-secret run metadata: making it explicit prevents routines that create external activity from inventing placeholder links, while the OpenRoutines site is an honest fallback for portable plugin routines that cannot know the agent repository's URL.

## A broken routine is one broken routine

**Decision.** A routine that does not load -- unknown frontmatter key, unterminated frontmatter, a filename two plugin files claim -- is absent, and nothing more.
The tick schedules the rest; a healthy routine's run assembles its workspace without it.
Load errors are attributed to the routine they concern (`routine.Error`, carrying the name and the file), and three rules follow from attribution.
A name any load error is about is **dropped from the loaded set**, so what the scheduler sees is only what a run could actually assemble a workspace around -- the filename is the identity, so a file that will not parse still claims its name, and a name two plugins claim is ambiguous even when one of them is the broken one.
The agent-owned precedence rule applies before this validation: when `routines/<name>.md` exists, plugin files with that name are ignored, while an invalid agent-owned file still holds the winning name and reports its own error.
The routine actually being run is the one place attribution is used to *fail*: its own unparseable file, or a name it is party to a plugin collision on, fails the attempt with the real error, and lookups of that name report the parse failure rather than "no routine" (`routine.ErrNotFound` is reserved for a name nothing claims, so `routines new` refuses to write over a file it could not read).
And absence is **recorded, not just logged**: the supervisor writes an event when a routine stops loading and another when it loads again.

**Why.** The blast radius of a typo should be the file it is in.
Failing every run on any parse error is not fail-closed, it is fail-everywhere: the broken routine is already excluded from scheduling, so all the strictness buys is that the *healthy* routines mint pending runs, push intent commits, and fail at workspace assembly -- five attempts each, abandonment tasks across the board, circuit breakers tripped.
Dropping the name is the same cascade from the other side: a routine the tick will mint, push, and dispatch, only for the runner to refuse the workspace, is worse than one that never ran -- so the two must agree, and the loaded set is where they agree.
The transitions are events rather than human-owned tasks because a broken file heals by being edited and the next tick notices; there is nothing for a person to close.
The one error class that concerns everyone -- a directory that would not read, which could be hiding any routine -- is unattributed: it still fails every attempt, which is loud on its own.
The exception is `plugin add`/`update`, which refuses to install against any load error: it is checking a name against the whole namespace, and a namespace with a hole in it is not a namespace.
