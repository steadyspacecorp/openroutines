# Execution

## Execution: headless opencode, fresh session per run

**Decision.** Routines execute via `opencode run` -- headless, model from frontmatter, a fresh session every run.
`routines run` is always the real thing: the routine receives its declared credentials and tools and may act on external systems.
A manual run discards its staged knowledge writes and run record by default: the terminal is the iteration path, and iterating must not teach the agent or consume its change feed by accident -- the supervisor is the only writer that settles without being asked.
`--write-knowledge` opts a manual run into settlement, for the catch-up or backfill a person wants the agent to keep.
Neither form is a dry run or a safety boundary; `check` is the non-acting validation path.
There is no separate `routines test`: withholding credentials and acting tools rehearsed precisely the path least likely to fail in production and gave stronger assurance than it earned.
Session continuation is never used.

Permissions are layered: a committed `opencode.json` provides the baseline policy (skills denied outright, the question tool off, the session-title agent disabled), and the generated per-routine agent definitions write explicit rules for everything frontmatter governs -- skills, web access, MCP -- overriding the baseline, with nothing using blanket auto-approval.
Built-in tools, bash included, stay available: the hard security boundary is credentials and the sandbox, not tool availability.
opencode ships with webfetch allowed, so the generated definition writes an explicit rule for both web tools every run -- deny unless the routine opts in with `webfetch: true` / `websearch: true`.
Fetched content is model context, which makes web access a prompt-injection vector -- a grant to review like a skill or credential, never a default.
Opting into websearch also enables opencode's search backend for that run (Exa: keyless by default, keyed when an `exa_api_key` credential is granted).
Disabling the built-in session-title agent is worth its own line: headless runs have no one to read a title, and generating one replays the routine's entire prompt through the model once per run.

Every attempt also **records what it consumed**.
After the model process exits, the runner asks opencode for the attempt's sessions (`session list --format json`, then `export` of each) and sums their assistant messages -- the attempt home normally holds one session, but capture reads every listed one rather than lean on that.
The session-outcome check judges only the first-listed session: the list is ordered most-recently-updated first (verified against the pinned 1.18.3), so that is the session that acted last, and a clean ending in an older one must not vouch for it.
The list names top-level sessions only, so a subagent's child session is neither capturable through the CLI nor able to dilute the outcome check.
Messages persist incrementally, so a timeout still accounts for what was spent, folding `model`, `effort`, and a `tokens` object -- input, output, reasoning, cache read/write -- into the attempt's `runs.jsonl` record alongside opencode's own estimate as an informational `cost_reported`.
Tokens with the model and effort are the durable record; dollars derive at read time, because recorded prices go stale.
Per attempt, not per run: a run-level figure would double-count retries.
Absence means the runtime didn't report -- never zero -- and bookkeeping never fails a run.
`openroutines usage` aggregates the window per routine (`--json` for scripts and monitors; status shows the one-line total); local container runs point the container's HOME at the same disposable attempt home, which matches production hygiene and keeps the capture surface readable after `--rm`.

**The capture step re-enters the run's boundary.**
Reading the finished session requires a second opencode startup, and opencode loads project and home plugins during commands such as `session list` and `export`.
A routine can plant startup code after its own process has loaded; running the follow-up as an ordinary supervisor child would then turn that delayed execution into an authority increase.
Instead, production capture builds a fresh sandbox with the attempt's same boundary, and local capture uses the same disposable container shape, workspace, and attempt home as the run.
The follow-up receives no routine credentials and retains only the writable disposable workspace the routine already had.
Code the run planted therefore gains no new authority from the second startup.
Native mode remains the explicit unconfined opt-in and reuses the developer's home without the routine environment.

Capture remains bounded and best-effort: failure or timeout omits usage and completion evidence but never changes the run's outcome by itself.
Reusing the warmed attempt home also avoids reinstalling declared opencode plugins solely for bookkeeping.

The run workspace excludes the repo's `AGENTS.md` (and `CLAUDE.md`): opencode loads a project-root rules file into any session's context, and that file is written for humans' coding agents in development sessions.
A routine's instructions come only from the generated definition and its own body, so runtime behavior never silently depends on dev-session guidance.
The runtime-critical rules that once lived in both places (full facts with real links, no secrets in knowledge) are injected by the standing instruction.

**Why.** opencode already does the hard part -- the agentic loop, tool use, model wrangling -- and ships as a self-contained binary.
Fresh sessions keep all continuity in the repo's knowledge directory: if it's not in the repo, the agent doesn't know it.
That's what makes knowledge portable, inspectable, and git-backed rather than state held hostage in a session store.
A committed permission policy means the agent's tool surface is reviewed like code.

## Rehearsals: a run mode, not routines

**Decision.** A rehearsal runs a real routine without consequence: `routines run <name> --rehearse [scenario]`.
It has two rungs.
With no fixtures on disk, the rehearsal is **live**: the routine keeps every grant so its reads work against the real world, and a prepended preamble instructs the model to treat every external action as read-only and idempotent, and to print what it would have delivered.
That restraint is instruction, not enforcement -- the enforced part, on both rungs, is that nothing settles: no knowledge writes, no consumption, no run record, `--write-knowledge` refused.
With fixtures, the rehearsal is a **fixture world**: fixtures live in `rehearsals/`, bound to routines by name -- `rehearsals/<name>.md` for the single-fixture case, a `rehearsals/<name>/` directory holding `default.md` and named scenarios once a routine earns more than one -- and the runner strips every grant at the source so the existing pipeline enforces the absence: no credentials resolve, no MCP servers mount, the generated definition denies skills and web access.
The fixture is injected read-only as `./rehearsal.md`; the clock is not faked, a frozen "work as if" moment is fixture prose.
`check` warns on a fixture whose name matches no routine.

**Why.** The predecessor was a shadow routine carrying fixtures and pointing at the real one for its rules ("follow routines/steady-check-in.md exactly") -- it drifts, because the promise to follow is prose, and its "no network calls" discipline was pleading nothing enforced.
Binding fixtures to the routine by name keeps the rules in one place, and the orphan warning is the drift detector the shadow pattern never had.
The live rung exists because demanding fixtures up front prices rehearsal out of its most common use, and it trusts the model with read-only restraint the way every acting run already trusts it with its prompt; the hard guarantee is settlement, which the discard path enforces on both rungs.
Rehearsals ride the manual run path, so a rehearsal can never teach the agent, consume its feed, or advance a cursor.

## A run is completed only when its session ended cleanly

**Decision.** The model process's exit code does not decide the outcome by itself.
After the process exits, the same session read that captures usage also reads how the session ended, and an attempt that exited 0 on a session that did not finish is recorded as `crashed` with the reason in its run record (`hint`) and its failure event.
Two things count as evidence: an assistant message carrying an `error`, and a session whose runtime reported finish reasons but never reported a finished turn -- the shape an agent loop leaves behind when it dies mid-turn on a tool call it never came back from.
The same record classifies provider authentication failures: a crashed attempt whose session error matches the auth shapes gets a hint naming the provider, the endpoint, and the credential the run injected -- classification from the runtime's own account, not from scraping output.
Evidence is required in both directions: a session record that says nothing about how it ended -- an older opencode, a field that moves -- leaves the process's own verdict standing.

**Why.** opencode exits 0 when its agent loop dies on a rejected or failed tool call (#68): a routine denied a scratch write ended its session on the error and reported `{"outcome":"completed","exit_code":0}` with no events, no tasks, and no ledger.
Locally that is a confusing no-op; in production it is the failure the scheduler exists to prevent, because `completed` advances the watermark and clears the pending record -- the work is never retried, and there is a green run record on top of it.
"Silently skipped" is the one outcome at-least-once may not produce, so the success signal has to be stronger than a process's exit status: an agentic runtime's exit code reports that the *harness* ran, not that the *job* did.
The session record is the right surface because it is the runtime's own account of the run, it is already read for usage (one exec, not two), and it is independent of the logging level -- classification that changed with a logging setting would disappear exactly when an operator turned up the logs to investigate.

Failing open on an unrecognized record is deliberate, and is the asymmetry that makes this safe to depend on.
A capture that fails open costs at most one confusing run record, the state before this decision; one that failed closed would mark *every* run crashed the day opencode renames a field -- five attempts each, a task per routine, and a whole agent abandoned on a schema change.
So the framework claims a failure only on evidence and never claims a success it cannot see, which is also why the record carries the reason: a run whose outcome was decided by something other than the exit code has to say what decided it.

## MCP servers are grants, remote-only

**Decision.** An MCP server is defined once in `opencode.json`'s `mcp` block -- transport, URL, auth headers, interpreted by opencode alone -- and granted per routine with `mcp: [name]` frontmatter.
The generated agent definition writes one rule per configured server every run (`"<name>_*"`, opencode's tool-name prefix): deny unless granted.
The workspace copy of `opencode.json` is filtered to match: an ungranted server's entry is removed before the file travels into the run, so the run's opencode does not merely deny the server's tools -- it never contacts the server at all.
`check` fails a grant naming an undefined server and sanctions the `mcp` key.
Auth headers reference run environment (`{env:STEADY_TOKEN}`), so reachability additionally requires the matching credential grant -- the permission rule scopes the tool surface; the credential scopes the connection.
Supported reality: remote servers with static-token or client-credentials auth work; OAuth-interactive servers have no headless path (a refresh-token credential type is the plausible future); stdio/local servers are out of scope by design -- the runtime image ships no language runtimes, and third-party code executing inside the sandbox is the wrong trade.

**Why.** MCP is how a growing share of services publish their tool surface, and a routine author shouldn't need a hand-rolled skill for a service that already ships a server.
But a connected server injects third-party tool descriptions -- the canonical MCP prompt-injection vector -- into every run that can see it, so which routines see which servers must be a reviewable grant, exactly like skills, credentials, and web access.
The choice between MCP and a hand-written skill is call-density and taste, not efficiency, so the framework blesses both and privileges neither.
Filtering the workspace copy is hygiene layered on the grant, not the enforcement: the deny rule and the withheld credential close an ungranted server's surface even when its entry is present.
What removal adds is that the run stops contacting servers it was never going to use -- opencode probes every configured server at session start, so before the filter every ungranted run hit the remote endpoint credential-less and logged a needs_auth refusal that read like a problem on runs behaving exactly as scoped.
Skills got the same treatment for the same reason.
