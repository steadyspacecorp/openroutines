# Contributing

Contributions are welcome, with one ask: read [docs/design.md](docs/design.md) first. This is an opinionated framework, and the opinions are documented -- each decision comes with its reasoning. Bug fixes and small improvements can go straight to a pull request. For anything that touches a documented decision, open an issue and argue with the reasoning, not just the behavior; if the rationale doesn't hold up, we'll change the design.

Security reports: see [SECURITY.md](SECURITY.md).

## Working on the framework

The CLI, supervisor, and embedded agent template all live in this repo. You'll need Go 1.26.5 and [golangci-lint](https://golangci-lint.run) (install them however you like; `.tool-versions` records the versions CI uses). The Makefile holds every task entry point:

```bash
make build      # the openroutines binary, into bin/
make test       # go test ./...
make verify     # everything CI runs
```

Before handing work back, run `make verify`: lint, `govulncheck`, a go.mod tidiness check, a gitleaks secret scan, the race-enabled test suite, and `bin/smoke` -- which builds the CLI, scaffolds a throwaway agent, and asserts `check` passes on a good agent and fails on a broken one. CI runs the same set. If you add a test fixture that reads like a credential, mark it with a trailing `// gitleaks:allow` comment saying why it isn't one. [AGENTS.md](AGENTS.md) has the full conventions: vocabulary, style, and the design-first workflow.
