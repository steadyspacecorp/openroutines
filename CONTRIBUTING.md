# Contributing

Contributions are welcome, with one ask: read [docs/design.md](docs/design.md) first. This is an opinionated framework, and the opinions are documented -- each decision comes with its reasoning. Bug fixes and small improvements can go straight to a pull request. For anything that touches a documented decision, open an issue and argue with the reasoning, not just the behavior; if the rationale doesn't hold up, we'll change the design.

Security reports: see [SECURITY.md](SECURITY.md).

## Working on the framework

The CLI, supervisor, and embedded agent template all live in this repo. You'll need Go 1.25+ (install it however you like; if you use [mise](https://mise.jdx.dev), the pinned toolchain in `mise.toml` is picked up automatically). Build and test with the standard commands:

```bash
go build ./...
go vet ./...
go test ./...
```

Before handing work back, also run `golangci-lint run` and `bin/smoke` (builds the CLI, scaffolds a throwaway agent, and asserts `check` passes on a good agent and fails on a broken one) -- CI runs exactly these. [AGENTS.md](AGENTS.md) has the full conventions: vocabulary, style, and the design-first workflow.
