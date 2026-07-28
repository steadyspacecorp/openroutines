# Creating routines

The heart of the agent is its routines: markdown files in `routines/`. Frontmatter scopes the schedule, skills, credentials, model, and optionally the reasoning effort. The body is the prompt.

```markdown
---
schedule: "0 9 * * 1"
timeout: 15m
active: true
skills:
  - steady-updates
credentials:
  - steady_token
  - github_token
model: anthropic/claude-sonnet-5
---
Check that our product documentation hasn't drifted from what we've actually shipped. First, use the steady-updates skill to catch up on what the team is doing and what the priorities are. Then compare the docs against what has shipped since your last run -- your memory has where you left off. Record anything that's now wrong or missing in memory, and print a short drift report; it goes to the container logs.
```

Every grant is greppable at the top of the file that defines it: what a routine can touch is declared in the file, not assembled at runtime.

## Frontmatter reference

| Key | What it declares |
|-----|------------------|
| `schedule` | Cron expression, evaluated in the agent's timezone. A routine needs a `schedule`, a `trigger`, or both. |
| `trigger` | An outbound change-detection poll that wakes the routine when something changed -- see [Triggers](#triggers). |
| `timeout` | Per-run timeout (default from `openroutines.yaml`'s `defaults:`). On expiry the whole process group is killed and the outcome recorded. |
| `active` | `false` parks the routine; the supervisor skips it. `routines activate` / `deactivate` flip it. |
| `skills` | The skills this routine may load -- and only these. See [Extending your agent](extending.md). |
| `credentials` | The credentials injected into this run's environment -- and only these. `steady_token` arrives as `$STEADY_TOKEN`. |
| `webfetch` | `true` grants the webfetch tool. Denied by default: fetched pages become model context, making web access a prompt-injection vector you opt into per routine. |
| `websearch` | `true` grants the websearch tool. Denied by default, same reason. Search runs through Exa -- keyless out of the box; grant an `exa_api_key` credential for keyed use. |
| `mcp` | Names of MCP servers (defined in `opencode.json`'s `mcp` block) whose tools this routine may call. Denied by default: an MCP server's tool descriptions are third-party text entering model context, and its tools act on external systems -- a grant to review like a skill or credential. Auth headers reference run env (`{env:STEADY_TOKEN}`), so a server is only reachable by routines that also grant the matching credential. |
| `model` | Provider/model override of the agent default, e.g. `anthropic/claude-sonnet-5`. |
| `effort` | Provider-specific reasoning effort. |
| `events` | `false` opts this routine out of recording events -- for reporting routines, where checking in is not work. See [Your agent on the team](teamwork.md). |
| `consumes` | `memory`: this routine consumes the memory change feed -- it receives an inbox of everything since it last reported. See [Your agent on the team](teamwork.md). |

## Scheduling

The supervisor wakes every minute, re-reads routine frontmatter, and dispatches whatever is due -- the files are the schedule; there is no registration step. Missed firings collapse into one catch-up run: an agent down for a week owes one run per routine, not seven. Runs are serial, one at a time; a routine still running is never dispatched again in parallel, and repeated failures trip a per-routine circuit breaker instead of retrying forever. [docs/design.md](design.md) has the full semantics ("Scheduling", "Overlap").

Every run also receives the schedule as data: a read-only `./schedule.md` in the workspace listing each active routine's next fires, computed by the scheduler's own parser. When the running routine is itself scheduled, the file fixes its **window** -- now through its next fire-day's first fire -- and splits the other routines in-window (they fire before this routine runs again) and out. Routine prompts should read the file, never re-derive fire times from cron frontmatter: forecasting ("release notes run tonight") becomes transcription, which models get right.

## Triggers

A routine can declare a `trigger` alongside or instead of its `schedule`: a cheap, outbound change-detection poll that makes the routine due.

```yaml
trigger:
  poll: https://api.github.com/notifications
  credential: github_token
  interval: 5m
```

- `poll` -- the URL to check
- `credential` -- optional; sent as a bearer token (must also appear in the routine's `credentials`, and must be a raw credential, not a typed one)
- `select` -- optional; extracts one value from a JSON response by RFC 6901 pointer (e.g. `/messages/0/ts`); without it, the response is reduced to a digest and compared
- `interval` -- poll cadence (default 5m, floor one minute)

A trigger carries no payload: when it fires, the routine runs exactly as it would from a schedule firing and pulls its actual work through its own skills. Triggers are best-effort latency reduction; the schedule remains the correctness backstop (`check` warns on a trigger-only routine with no heartbeat schedule). The reasoning -- and why there is no webhook receiver -- is in [docs/design.md](design.md) ("Triggers").

## Running and testing

Both `routines run` and `routines test` start opencode in a disposable Docker container, with the same runtime image, opencode version, constructed environment, and assembled workspace as production.

```bash
openroutines routines run doc-drift    # the real thing; memory writes are kept
openroutines routines test doc-drift   # dry run
```

`test` is a behavioral rehearsal: the routine's credentials are withheld (only the model provider key is injected), acting tools are denied, the standing instruction says to narrate intended external actions instead of taking them, and memory writes are discarded. You rehearse against the real model without the routine authenticating to anything or leaving a trace -- and what you rehearse is what production runs.

## Recording work

Every run carries a standing instruction that routes what the routine wants to remember into the agent's memory primitives -- events, tasks, context, and the routine's private ledger. You don't design a memory scheme per routine; the rule is injected by the runtime. [Your agent on the team](teamwork.md) covers the primitives and how recorded work becomes reports.
