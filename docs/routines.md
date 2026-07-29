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
| `timeout` | Per-run timeout (default from `openroutines.yml`'s `defaults:`). On expiry the whole process group is killed and the outcome recorded. 15m is a hard ceiling -- the single-instance lease covers one run at a time, so a larger value is capped at 15m and `check` warns; longer work belongs in several runs with memory between them. |
| `active` | `false` parks the routine; the supervisor skips it. `routines activate` / `deactivate` flip it. |
| `skills` | The skills this routine may load -- and only these. See [Extending your agent](extending.md). |
| `credentials` | The credentials injected into this run's environment -- and only these. `steady_token` arrives as `$STEADY_TOKEN`. |
| `webfetch` | `true` grants the webfetch tool. Denied by default: fetched pages become model context, making web access a prompt-injection vector you opt into per routine. |
| `websearch` | `true` grants the websearch tool. Denied by default, same reason. Search runs through Exa -- keyless out of the box; grant an `exa_api_key` credential for keyed use. |
| `mcp` | Names of MCP servers (defined in `opencode.json`) whose tools this routine may call. Denied by default: a server's tool descriptions are third-party text entering model context -- a grant to review like a skill or credential. See [MCP servers](#mcp-servers). |
| `model` | Provider/model override of the agent default, e.g. `anthropic/claude-sonnet-5`. |
| `effort` | Provider-specific reasoning effort. |
| `events` | `false` opts this routine out of recording events -- for reporting routines, where checking in is not work. See [Your agent on the team](teamwork.md). |
| `consumes` | `memory`: this routine consumes the memory change feed -- it receives an inbox of everything since it last reported. See [Your agent on the team](teamwork.md). |

## Scheduling

The supervisor wakes every minute, re-reads routine frontmatter, and dispatches whatever is due -- the files are the schedule; there is no registration step. Missed firings collapse into one catch-up run: an agent down for a week owes one run per routine, not seven. Runs are serial, one at a time; a routine still running is never dispatched again in parallel, and repeated failures trip a per-routine circuit breaker instead of retrying forever. [docs/design.md](design.md) has the full semantics ("Scheduling", "Overlap").

A routine whose file does not load -- a frontmatter typo, a missing closing `---`, a name a plugin's routine also claims -- is skipped, and only it: the other routines run their next fire as usual. The supervisor records an event when a routine stops loading and another when it loads again, so the gap shows up in the agent's own reporting rather than only in the container log; `openroutines check` names the file and the mistake. Fix the file and the next tick picks it up, catch-up included. `routines edit` and `routines remove` work on a routine that does not load, and `routines new` will not create over one.

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

## MCP servers

A routine can call tools from a remote [MCP](https://modelcontextprotocol.io) server. The server is defined once in `opencode.json` -- transport, URL, auth headers, interpreted by opencode alone -- and granted per routine:

```json
"mcp": {
  "steady": {
    "type": "remote",
    "url": "https://app.steady.space/mcp",
    "headers": { "Authorization": "Bearer {env:STEADY_TOKEN}" }
  }
}
```

```yaml
mcp: [steady]
credentials: [steady_token]
```

The grant opens the server's tools to this routine's runs; every other routine keeps them denied. Auth headers reference the run environment, so the server is only reachable when the routine also grants the credential that fills them: the `mcp` grant scopes the tool surface, the credential grant scopes the connection. `check` fails a grant naming a server `opencode.json` doesn't define.

What works: remote servers with static-token or client-credentials auth (a typed `oauth2_client` credential mints the bearer at spawn). OAuth-interactive servers have no headless path, and local stdio servers are out of scope by design -- the runtime image ships no language runtimes.

## Running locally

`routines run` starts opencode in a disposable Docker container, with the same runtime image, opencode version, constructed environment, and assembled workspace as production.

```bash
openroutines routines run doc-drift                 # memory writes are kept
openroutines routines run doc-drift --no-memory     # memory writes are discarded
```

Both forms are real runs. They receive the routine's credentials and tools and may perform external actions; `--no-memory` changes only what happens afterward, discarding staged memory writes and the run record. Use `openroutines check` for non-acting validation. To exercise an acting path, point the routine's configuration at a scratch target and use `--no-memory` when you do not want the result retained in agent memory.

## Recording work

Every run carries a standing instruction that routes what the routine wants to remember into the agent's memory primitives -- events, tasks, context, and the routine's private ledger. You don't design a memory scheme per routine; the rule is injected by the runtime. [Your agent on the team](teamwork.md) covers the primitives and how recorded work becomes reports.
