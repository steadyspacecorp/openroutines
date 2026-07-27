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

A credential can also carry a *type*. A `credentials:` entry in `openroutines.yaml` tells the runner how to materialize the stored value -- `type: github_app` turns a stored App private key into a short-lived installation token minted fresh for each run, injected as `GITHUB_TOKEN` alongside the App's Git identity, and revoked when the run ends. `type: oauth2_client` does the same for the OAuth2 client-credentials flow most SaaS APIs use for server-to-server access (Help Scout, Stripe, Slack app-level): the runner posts the stored client secret to the entry's `token_url` and injects only the resulting bearer under the entry's `inject_as` name -- same name→env mapping as everywhere else, so `support_desk_token` becomes `$SUPPORT_DESK_TOKEN`. Either way the routine declares the credential exactly as before; it just never sees the root secret. Credentials without an entry inject verbatim, as always.

## Variables

Non-secret configuration -- a repo name, a docs URL -- goes in a `variables:` map in `openroutines.yaml`, and every run receives each one as an environment variable (`product_repo` becomes `$PRODUCT_REPO`). Secrets go in encrypted credentials instead; inside a routine the interface is the same either way, so the only question a value raises is whether it's secret.

## Plugins

A whole capability -- one or more routines, their skills, the names of the credentials to fill in -- can arrive as a plugin:

```bash
openroutines plugin add steadyspacecorp/openroutines --path examples/plugins/steady
```

`plugin add` shows you the bundle's declared authority -- every routine with its schedule, trigger, model, credentials, and skills -- and copies the files in only after you confirm (`--yes` is required when stdin is not interactive). The summary is not a substitute for reviewing routine bodies and skill files, which are executable supply-chain input. Installed routines are forced inactive so you can configure them, inspect the diff, and activate them explicitly. Install writes nothing outside `routines/` and `skills/`, and secrets are never part of a plugin: you're told which credentials to `credentials set` afterward. Plugins are copy-first: after install the files are yours, indistinguishable from ones you wrote by hand, reviewed and versioned in the same diff. A plugin can also be skills-only (you write the routines) -- see [docs/design.md](design.md) for the format and the boundaries it enforces.

To author one, copy a reference plugin from [`examples/plugins/`](../examples/plugins/) and edit: the `PLUGIN.md` manifest names the bundle and the credentials and variables it needs; `routines/`, `skills/`, and `memory/ledgers/` stubs are the payload.
