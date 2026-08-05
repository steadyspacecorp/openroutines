# Extending your agent

Giving your agent a new capability usually mixes the same ingredients: a **credential** to authenticate with, sometimes a **skill** for the know-how, sometimes a **variable** for non-secret configuration -- and a routine that puts them to work. A whole capability can also arrive pre-assembled as a **plugin**.

## Skills

Skills follow the open [Agent Skills](https://agentskills.io/) standard, so any skill written for Claude Code, Cursor, opencode, or the rest of the ecosystem works in your agent's `skills/` directory unchanged. A routine only gets the skills its frontmatter declares.

```bash
openroutines skills new my-skill                 # scaffold a blank skill
openroutines skills new owner/repo --path sub/dir  # vendor one from a git repository
openroutines skills list                         # skills and which routines use them
```

Treat a skill like the dependency it is: instructions -- sometimes code -- that your agent will follow unattended. Review what you vendor in.

## Credentials

Secrets are encrypted into the repo, Rails-style: one master key outside the repo, per-routine grants in frontmatter, log scrubbing for every secret value. The repo stays self-contained without ever containing a usable secret.

```bash
openroutines credentials set steady_token    # add or replace one value (prompted, hidden)
openroutines credentials list                # credential names and which routines declare them
```

A routine declares the credentials it needs in frontmatter, and each one arrives in that run's environment -- `steady_token` becomes `$STEADY_TOKEN`. Routines receive only the credentials their frontmatter declares.

### Typed credentials

A credential can also carry a *type*. A `credentials:` entry in `openroutines.yml` tells the runner how to materialize the stored value -- `type: github_app` turns a stored App private key into a short-lived installation token minted fresh for each run, injected as `GITHUB_TOKEN` alongside the App's Git identity, and revoked when the run ends. `type: oauth2_client` does the same for the OAuth2 client-credentials flow most SaaS APIs use for server-to-server access (Help Scout, Stripe, Slack app-level): the runner posts the stored client secret to the entry's `token_url` and injects only the resulting bearer under the entry's `inject_as` name -- same name→env mapping as everywhere else, so `support_desk_token` becomes `$SUPPORT_DESK_TOKEN`. Either way the routine declares the credential exactly as before; it just never sees the root secret. Credentials without an entry inject verbatim, as always.

Multi-line values -- an App private key is the canonical case -- are stored one line: `credentials set` converts real newlines to literal `\n` sequences on the way in (pipe the file: `openroutines credentials set app_key < key.pem`), and typed credentials decode them on use. The one-line form is what keeps the value scrubbable from logs -- redaction matches exact strings in line-based output, which a value spanning lines would never satisfy. A PEM missing its END line is refused rather than stored truncated, and `check` confirms a stored `github_app` value actually parses as a private key, offline. A *raw* multi-line credential reaches the run in the same escaped form, so anything consuming one decodes `\n` itself.

## Variables

Non-secret configuration -- a repo name, a docs URL -- goes in a `variables:` map in `openroutines.yml`, and every run receives each one as an environment variable (`product_repo` becomes `$PRODUCT_REPO`). Secrets go in encrypted credentials instead; inside a routine the interface is the same either way, so the only question a value raises is whether it's secret.

## Plugins

A whole capability -- one or more routines, their skills, the names of the credentials to fill in -- can arrive as a plugin:

```bash
openroutines plugin add steadyspacecorp/openroutines-plugins --path steady
openroutines plugin list
openroutines plugin update steady
```

`plugin add` shows you the bundle's declared authority -- every routine with its schedule, trigger, model, credentials, and skills -- and vendors the complete bundle under `.openroutines/plugins/<name>/` only after you confirm (`--yes` is required when stdin is not interactive). Agent-owned routines and skills stay in the top-level `routines/` and `skills/` directories; plugin-owned files stay grouped with their `PLUGIN.md` and `.openroutines-source.yaml`, so the repository shows exactly where they came from and which upstream commit they track. OpenRoutines discovers both locations through one global routine and skill namespace. The grant summary is not a substitute for reviewing routine bodies and skill files, which are executable supply-chain input. Installed routines are forced inactive so you can configure them, inspect the diff, and activate them explicitly. Secrets are never part of a plugin: you're told which credentials to `credentials set` afterward.

`plugin update <name>` fetches the recorded source, validates and summarizes the new bundle, then merges it against your vendored copy using the recorded revision as the base. Local schedule, prompt, skill, and activation edits survive clean updates; ordinary conflict markers are left for you when both sides changed the same lines, and the recorded revision does not advance until the merge is clean. Newly added routines always land inactive. Updates never touch live memory: ledger stubs are install-time seeds only. A plugin can also be skills-only (you write the routines) -- see [design.md](design.md) for the format and boundaries.

A plugin whose routines use an [MCP server](routines.md#mcp-servers) declares it in the manifest -- description, URL, and the credential its auth header references -- and never ships `opencode.json`, which stays yours. An interactive `plugin add` offers to write each declared server's entry, showing the exact JSON and defaulting to no; `--yes` and non-interactive installs only print the snippet to paste, an entry you already have is never touched, and `plugin update` never writes `opencode.json` -- so a plugin update can propose a new endpoint but only you can make one live:

```yaml
mcp:
  steady:
    description: Steady's MCP server -- check-ins, goals, action items
    url: https://app.steady.space/mcp
    credential: steady_token
```

To author one, copy a reference plugin from the [openroutines-plugins](https://github.com/steadyspacecorp/openroutines-plugins) repository and edit: the `PLUGIN.md` manifest names the bundle and the credentials, variables, and MCP servers it needs; `routines/`, `skills/`, and `memory/ledgers/` stubs are the payload.
