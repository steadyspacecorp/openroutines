# OpenRoutines

[![ci](https://github.com/steadyspacecorp/openroutines/actions/workflows/ci.yml/badge.svg)](https://github.com/steadyspacecorp/openroutines/actions/workflows/ci.yml)

The framework for running simple, secure, and durable autonomous AI agents.

[Getting started](docs/getting-started.md) · [Creating routines](docs/routines.md) · [Extending your agent](docs/extending.md) · [Operating in production](docs/operating.md) · [CLI reference](docs/cli.md)

Plenty of tools will run a prompt on a schedule. OpenRoutines runs a _job_: a product management agent that gathers research into themes and catches docs drifting from what shipped, an IT ops agent that triages tickets and monitors licenses. One job description, a handful of routines, and a way to report intentions and progress to the team. Scaffold one in minutes, test it locally, deploy it with a push -- then maintain it like any other software you ship.

## 📦 The repo is the agent

Everything your agent is -- routines, skills, memory, encrypted credentials -- lives in one git repository that deploys as one Docker container. Not agents-as-a-service: the repo _as_ the agent, shipped like the rest of your software.

- **Routines are markdown files** -- frontmatter declares the scope, the body is the prompt
- **Deploys anywhere a container runs** -- a VPS, Fly, Render, your homelab
- **No database, no queue, no secrets platform** -- a git origin is the only prerequisite
- **Bring your own models** -- any provider opencode supports, chosen per routine, gateways included

## 🤝 A teammate, not a black box

[Teamwork primitives come built in](docs/teamwork.md): recording work, owning tasks, stating intentions, surfacing blockers. You focus on the routines that do the work -- the teamwork mostly takes care of itself.

- **Structured memory, not chat history** -- one rule routes every record: events (it happened), tasks (someone must do it), context (worth remembering)
- **It lives on a git branch** -- backed up on every push, surviving every redeploy
- **Report where you want, how you want** -- any routine can consume what the agent recorded and report it anywhere: Steady, Slack, a log line
- **Everything is reviewable** -- read it with `git log`, prune it with a commit

## 🔒 Safe to run unattended

Built for the question every team asks first: can we trust this thing?

- **Credentials are encrypted in the repo** -- granted per routine, scrubbed from logs
- **Short-lived tokens when you want them** -- give a credential a type, and a stored root secret (a GitHub App key, an OAuth client) becomes a fresh, expiring token for each run
- **Runs are sandboxed** -- a routine sees only what its frontmatter declares
- **Zero ingress** -- the shipped container listens on no ports

## Quick start

You need git, Docker, and an API key for at least one model provider. Install the CLI (from [Releases](../../releases) while the repo is private; `brew install openroutines` once public), then:

```bash
openroutines scaffold my-agent
cd my-agent
openroutines configure
openroutines routines new doc-drift    # edit the file, then:
openroutines routines run doc-drift    # runs locally in the production container
```

The full walkthrough -- install, configuration, models, your first routine -- is in [Getting started](docs/getting-started.md).

## Why we built this

We built OpenRoutines to run our own operations at [Steady](https://www.runsteady.com). The existing options were all too much or too little: routines locked to one vendor's harness and a single user, personal-assistant frameworks pressed into server duty, whole multi-agent platforms when we wanted one agent with one job. Both extremes carry the same security, durability, and trust problems.

What we wanted was simple: set up and test an agent locally in a few minutes -- skills, routines, credentials, and the model of our choice -- deploy it with a push, and maintain it like any other git-versioned project. Then rinse and repeat for the next agent.

Well, now you can.
