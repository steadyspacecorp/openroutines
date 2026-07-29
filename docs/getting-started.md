# Getting started

From nothing to a running agent: install the CLI, scaffold an agent, configure it, and run your first routine.

## Prerequisites

- **git**
- **Docker** -- needed by `routines run`, which executes in the same container it runs in when deployed; `scaffold`, `configure`, `check`, and `routines new` don't need it
- **An API key for at least one model provider** (Anthropic, OpenAI, ...) -- `configure` encrypts it into the agent's credentials

That's the whole list. You don't install opencode or any language runtime -- everything the agent runs on ships inside the container.

None of it is private, either. The image your routines run in locally builds from public bases, so you can scaffold an agent, write routines, and run them against the real model without any registry access at all. GHCR is a deploy-time dependency only -- see [Operating in production](operating.md).

## Install the CLI

**While this repo is private**, brew and the install script are not live yet -- install from [Releases](https://github.com/steadyspacecorp/openroutines/releases) instead:

```bash
# pick your platform: darwin|linux, arm64|amd64
gh release download --repo steadyspacecorp/openroutines --pattern "openroutines_*_darwin_arm64" --pattern "checksums.txt" -D /tmp/or
(cd /tmp/or && shasum -a 256 -c --ignore-missing checksums.txt)
chmod +x /tmp/or/openroutines_* && mv /tmp/or/openroutines_* ~/bin/openroutines   # any dir on your PATH
openroutines --version
```

(On macOS, always install by moving the file into place as above -- overwriting a running binary in place invalidates the kernel's signature cache. The darwin binaries are ad-hoc signed; Gatekeeper does not quarantine files downloaded via `gh`.)

Once public, installation becomes:

```bash
brew install openroutines
# or
curl -fsSL https://openroutines.dev/install.sh | sh
```

## Scaffold and configure

```bash
openroutines scaffold my-agent
cd my-agent
openroutines configure
```

`scaffold` creates a fresh git repository with the agent's skeleton:

- `openroutines.yml` -- the agent's identity and defaults
- `routines/` -- a starter check-in routine, active by default (twice a day, your agent reports what it did, what it intends to do, and where it's blocked -- to the logs, until you point it somewhere better)
- `skills/` -- empty, ready for you to add to
- `AGENTS.md` -- so you can work on the agent with the coding agent of your choice
- a baseline `opencode.json` permission policy, `.gitignore`, Dockerfile, and pinned framework version

A `memory/` directory appears on first run -- a checkout of the agent's dedicated memory branch, kept out of `main`'s history.

`configure` is idempotent -- run it whenever. It fills in `openroutines.yml` (name, job description, owner, timezone, default model), generates the master key for encrypted credentials, and reports anything the agent still needs.

### Name and job description

The agent's `name` and `description` are standing context, not metadata for people reading the repository. Every run begins by telling the model its name and injecting the description as "your job description"; the routine body then supplies the instructions for that particular run. A routine inherits the job description and does not need to restate it.

Choose both at the altitude where the agent will remain coherent as you add routines. Name the domain, not the first routine. Write the description as a durable mandate: what outcomes the agent owns and where its responsibility ends. A tagline is usually too vague to guide a run, while a checklist of current routines is too specific -- schedules and individual procedures belong in routine files.

For example, an agent that starts by checking documentation but may later synthesize research and report on releases could use:

```yaml
name: Product intelligence
description: Maintain an accurate picture of what customers need and what the product team has shipped, then surface gaps and decisions to the team.
```

`Documentation checker` would corner the same agent around its first routine. Conversely, "Help the product team" would give every routine too little direction. If the work needs a genuinely different mandate, create another agent -- one agent still means one job.

## Pick your models

`configure` sets the agent's default model in `openroutines.yml` -- any provider opencode supports works, and any routine can override the default in its own frontmatter (see [Creating routines](routines.md)).

Custom model endpoints -- an AI gateway, a proxy, a self-hosted server -- are harness configuration: a `provider` block in `opencode.json`, in [opencode's provider schema](https://opencode.ai/docs/providers/), keyed by the prefix your model strings use:

```json
{
  "provider": {
    "my_gateway": {
      "npm": "@ai-sdk/openai-compatible",
      "options": {
        "baseURL": "https://gateway.example.com/v1/compat",
        "apiKey": "{env:MY_GATEWAY_API_KEY}"
      }
    }
  }
}
```

A routine then selects `model: my_gateway/some-model` in its frontmatter, and the credential `my_gateway_api_key` is injected automatically as the provider key. The boundary is simple: each config file belongs to the system that interprets it. Endpoint definitions and permissions are opencode's (`opencode.json`, which `openroutines update` never rewrites); model choice, grants, and schedules are the framework's (`openroutines.yml` and frontmatter). `openroutines check` flags a defined provider that no model string references.

## Your first routine

```bash
openroutines routines new doc-drift
```

Edit the generated file -- schedule and scope in the frontmatter, prompt in the body -- then run it once locally. The local run uses the same runtime image, opencode version, constructed environment, and assembled workspace as production.

`routines run` starts opencode in a disposable Docker container. It is always a real run with the routine's credentials and tools; `--no-memory` discards only its staged memory writes and run record.

```bash
openroutines routines run doc-drift
```

Day to day:

```bash
openroutines status                   # master key, models, routines and schedules, skills, memory sync state, token usage
openroutines routines list            # also: edit, activate, deactivate, remove
openroutines routines run <name> --no-memory
                                      # real external actions; memory discarded
openroutines skills new <name|url>    # scaffold a skill, or vendor one from a git repo; also: list, remove
openroutines credentials set <name>   # add/replace one encrypted secret; also: list, remove
openroutines usage                    # token use and reported cost per routine; --json for scripts
openroutines check                    # validate config, frontmatter, and schedules; made for CI
```

## Where next

- [Creating routines](routines.md) -- the frontmatter reference, schedules, triggers, and local runs
- [Extending your agent](extending.md) -- skills, plugins, credentials, and variables
- [Your agent on the team](teamwork.md) -- how the agent reports its work and improves at its job
- [Operating in production](operating.md) -- deploying, CI/CD, and updating
