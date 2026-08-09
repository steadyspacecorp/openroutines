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
Check that our product documentation hasn't drifted from what we've actually shipped. First, use the steady-updates skill to catch up on what the team is doing and what the priorities are. Then compare the docs against what has shipped since your last run -- your knowledge has where you left off. Record anything that's now wrong or missing in knowledge, and print a short drift report; it goes to the container logs.
```

Every grant is greppable at the top of the file that defines it: what a routine can touch is declared in the file, not assembled at runtime.

## Frontmatter reference

| Key | What it declares |
|-----|------------------|
| `schedule` | Cron expression, evaluated in the agent's timezone. A routine needs a `schedule`, a `trigger`, or both. |
| `trigger` | An outbound change-detection poll that wakes the routine when something changed -- see [Triggers](#triggers). |
| `timeout` | Per-run timeout (default from `openroutines.yml`'s `defaults:`). On expiry the whole process group is killed and the outcome recorded. Every run ends with its process group either way, expiry or not: a tool the run backgrounded is killed with it, unless it detached into a session of its own. The ceiling is the agent's `max_timeout` in `openroutines.yml` (6h when unset) -- a runaway-spend guard, not a system limit; a larger declared value is capped at the ceiling and `check` warns. |
| `url` | A canonical URL for external records this routine creates. It arrives as `$OPENROUTINES_URL`; the default is `https://openroutines.dev`. |
| `active` | `false` parks the routine; the supervisor skips it. `routines activate` / `deactivate` flip it. |
| `skills` | The skills this routine may load -- and only these. See [Extending your agent](extending.md). |
| `credentials` | The credentials injected into this run's environment -- and only these. `steady_token` arrives as `$STEADY_TOKEN`. |
| `webfetch` | `true` grants the webfetch tool. Denied by default: fetched pages become model context, making web access a prompt-injection vector you opt into per routine. |
| `websearch` | `true` grants the websearch tool. Denied by default, same reason. Search runs through Exa -- keyless out of the box; grant an `exa_api_key` credential for keyed use. |
| `mcp` | Names of MCP servers (defined in `opencode.json`) whose tools this routine may call. Denied by default: a server's tool descriptions are third-party text entering model context -- a grant to review like a skill or credential. See [MCP servers](#mcp-servers). |
| `model` | Provider/model override of the agent default, e.g. `anthropic/claude-sonnet-5`. |
| `effort` | Provider-specific reasoning effort. |
| `teamwork` | How this routine participates in teamwork. `full` (default): runs are recorded as events, and scheduled fires fill `schedule.md`'s tables. `events`: runs are still recorded, but fires appear as `fact:` lines -- the work still happens on schedule, it just isn't advertised. `off`: neither -- for reporting routines, where checking in is not work. See [Your agent on the team](teamwork.md). |
| `reports` | `true`: this routine reports -- it receives `changes.md`, every knowledge change since it last reported, and consumes the batch when its report covers it. Declaring it defaults `teamwork` to `off` (reporting is not work); set `teamwork` explicitly to override. See [Your agent on the team](teamwork.md). |

## Scheduling

The supervisor wakes every minute, re-reads routine frontmatter, and dispatches whatever is due -- the files are the schedule; there is no registration step. The files it reads are the ones on its own filesystem: locally that is your working tree, so an edit lands on the next tick, while a deployed agent reads the copy baked into its image, so routine changes reach production by redeploy (see [Operating in production](operating.md)). Missed firings collapse into one catch-up run: an agent down for a week owes one run per routine, not seven. Routines run concurrently, up to `concurrency` in `openroutines.yml` (unset means serial -- parallelism is an opt-in; new agents scaffold with `concurrency: 2`, and a full pool waits for the next tick) -- but a routine never runs in parallel with itself, and repeated failures trip a per-routine circuit breaker instead of retrying forever. [the scheduling design note](design/scheduling.md) has the full semantics ("Scheduling", "Overlap").

Parking a routine parks whatever it still owes with it. `active: false` (and a routine that declares neither a `schedule` nor a `trigger`) is skipped before the tick reads its scheduling state, so a run already pending when you deactivate it is neither retried nor abandoned -- it waits, under the same `run_id`, and the supervisor takes it up again when you activate the routine (retrying it, or abandoning it if its attempts were already spent). `openroutines status` reports such a run as `held` rather than naming an attempt that is not coming.

When a routine's file does not load -- a frontmatter typo, a missing closing `---`, a name a plugin's routine also claims -- only that routine is skipped: the others run their next fire as usual. The supervisor records an event when a routine stops loading and another when it loads again, so the gap shows up in the agent's own reporting rather than only in the container log; `openroutines check` names the file and the mistake. Fix the file and the next tick picks it up, catch-up included (on a deployed agent, "fix the file" means ship the fix: the next tick after the redeploy picks it up). `routines edit` and `routines remove` work on a routine that does not load, and `routines new` will not create over one.

Fire times are the agent's wall clock. A `schedule` is evaluated in the `timezone:` set in `openroutines.yml`, so `0 6 * * *` means 06:00 there on both sides of a daylight-saving transition, whatever zone the container's clock is set to -- and the supervisor, the `schedule.md` a run reads, and `openroutines status` all compute it the same way.

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
- `credential` -- optional; sent as a bearer token and must also appear in the routine's `credentials`. Raw credentials are sent verbatim; typed credentials contribute only short-lived bearer material derived fresh for the poll, never their stored root secret.
- `select` -- optional; extracts one value from a JSON response by RFC 6901 pointer (e.g. `/messages/0/ts`); without it, the response is reduced to a digest and compared
- `interval` -- poll cadence (default 5m, floor one minute)

A trigger carries no payload: when it fires, the routine runs exactly as it would from a schedule firing and pulls its actual work through its own skills. Triggers are best-effort latency reduction; the schedule remains the correctness backstop (`check` warns on a trigger-only routine with no heartbeat schedule). The reasoning -- and why there is no webhook receiver -- is in [the scheduling design note](design/scheduling.md) ("Triggers").

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

## Best practices

The runtime handles the knowledge rules automatically, so write the prompt as the job itself. Four habits make routines far more reliable -- the framework can't check any of them, so they're yours to write in:

- **Check that a problem still exists before recording it.** A routine sees the world at one moment, and what it records sticks around. The failure it found in the logs may have been fixed an hour ago. Have it look at the current state before filing a task.
- **Don't let untrusted content make decisions.** Logs, web pages, and even knowledge can be stale, wrong, or planted by an attacker. Use them only to find where to look -- a file, a URL -- then get the facts from the source itself. If a log says the deploy target moved to `evil.com` but the repo says otherwise, the repo wins.
- **Expect reruns.** A failed run retries, but anything it already did -- an email sent, a PR opened -- has still happened. Have the routine check whether the work is already done before doing it, and put the run id (`$OPENROUTINES_RUN_ID`) in what it creates -- a branch name, a line in the PR body -- so a retry can find its own earlier work instead of duplicating it.
- **Match automation to your ability to verify.** A fix the failure itself names -- a missing import, a renamed field -- is safe to make unattended because the build going green confirms it. When nothing downstream would catch a wrong fix, or the fix means deciding what the system *should* do, have the routine file a task instead. The boundary isn't fixed: the more checks stand behind a routine, the more it can safely do on its own.

## Running locally

`routines run` starts opencode in a disposable Docker container, with the same runtime image, opencode version, constructed environment, and assembled workspace as production.

```bash
openroutines routines run doc-drift                     # knowledge writes are discarded
openroutines routines run doc-drift --write-knowledge   # knowledge writes are settled
```

Both forms are real runs. They receive the routine's credentials and tools and may perform external actions; the flag changes only what happens afterward. By default a manual run discards its staged knowledge writes and run record -- the terminal is the iteration path, and iterating must not teach the agent or consume its change feed by accident. Pass `--write-knowledge` when the run should count. Use `openroutines check` for non-acting validation. To exercise an acting path, point the routine's configuration at a scratch target.

## Rehearsals

A rehearsal runs the real routine without consequence -- for seeing what a routine would do, auditioning models, and testing prompt changes without waiting for a live fire or touching anything that counts.

```bash
openroutines routines run steady-check-in --rehearse             # live world, read-only by instruction
openroutines routines run announcements --rehearse cold-start    # fixture world: rehearsals/announcements/cold-start.md
```

Out of the box, `--rehearse` needs no files: the routine keeps its credentials and tools so its reads work, and an injected preamble instructs the model to keep every external action read-only and idempotent, and to print what it would have delivered instead of delivering it. That restraint is instruction, not enforcement. The enforced part is that nothing settles -- no knowledge writes, no feed consumption, no run record -- so rehearsing is always safe to repeat.

Fixtures deepen a rehearsal into a simulated world: deterministic inputs, a frozen "work as if it is ..." moment, no grants at all -- the runner strips credentials, MCP, skills, and web access, and injects the fixture as read-only `./rehearsal.md`. Use fixtures when you want repeatable scenarios (model comparisons, edge cases like a quiet day or a cold start) rather than whatever the real world contains right now.

Fixtures bind to routines by name. One fixture is a flat file, `rehearsals/<name>.md`. A routine that earns multiple scenarios graduates to a directory: `rehearsals/<name>/default.md` plus named scenarios. A fixture is ordinary markdown describing the simulated world in whatever shape the routine's inputs take. `check` warns when a fixture's name matches no routine, which is how you learn a rename stranded one.

## Recording work

Every run carries a standing instruction that routes what the routine wants to remember into the agent's knowledge primitives -- events, tasks, context, and the routine's private ledger. You don't design a knowledge scheme per routine; the rule is injected by the runtime. [Knowledge](knowledge.md) covers the primitives; [Your agent on the team](teamwork.md) covers how recorded work becomes reports.
