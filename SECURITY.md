# Security

openroutines runs autonomous AI agents unattended, with credentials, on your
infrastructure. Security is the reason most of its design decisions exist --
[DESIGN.md](DESIGN.md) records each one with its reasoning.

## Reporting a vulnerability

Report privately via [GitHub's private vulnerability reporting](../../security/advisories/new)
on this repository, or email henry@steady.space. Please don't open a public
issue for anything exploitable. You'll get an acknowledgment within a few
days; this is a small project and we'll be candid about timelines.

## Supported versions

Pre-1.0: only the latest release is supported. `openroutines update` brings
an agent to the version of the binary you run.

## What the design defends

- **Credential scoping**: a routine's process receives only the secrets its
  frontmatter declares, in an environment constructed from scratch. The
  master and deploy keys are delivered to the supervisor by file
  (`OPENROUTINES_*_KEY_FILE`, preferred) or environment variable; the file
  paths sit outside the model sandbox's rule set, and the supervisor marks
  itself non-dumpable at boot, so same-UID model processes can read neither
  its environment nor its memory. Running the model process under its own
  UID is the remaining hardening step (tracked in the backlog).
- **Filesystem confinement**: model processes run in a per-run container
  locally and under a Landlock sandbox in production -- read access to their
  run workspace, the OS, and the opencode installation; write access to
  staged memory, the run tmp, and a disposable per-attempt HOME, nothing
  else. Fails closed at boot, and a Linux-gated integration test asserts
  each boundary on a real kernel.
- **No ingress**: a deployed agent listens on nothing. Logs are the only way
  in.
- **Git isolation**: model processes never touch a git worktree or metadata;
  the supervisor validates staged output (no symlinks, no git control files,
  size limits) before importing it.
- **Memory as untrusted input**: agent memory is framed to the model as
  records, never instructions; sync refuses rewritten remote history.
- **Supply chain**: two runtime dependencies plus Landlock bindings; the
  agent base image pins its opencode version.

## Known limitations (honest list)

- **Network egress is unrestricted.** A prompt-injected routine can send
  anything it legitimately holds (its declared credentials, readable memory)
  to any host. Scoping caps what a routine holds; nothing yet caps where it
  can send. Per-routine egress control is the top item on the hardening
  backlog.
- **Log redaction is best-effort**: exact-value matching only; an encoded
  secret can evade it. The primary protection is scoping, not redaction.
- **Prompt injection is mitigated, not solved.** Skills and web content are
  instructions the model may follow. Treat every skill and credential grant
  as authority.
- Release artifacts are checksummed; darwin binaries are ad-hoc signed. Developer ID signing and notarization are pending (launch checklist). When replacing an installed binary on macOS, download to a temp path and `mv` into place -- overwriting in place invalidates the kernel's signature cache and the binary gets killed.

## Out of scope

- Vulnerabilities in opencode, model providers, or Docker itself (report
  upstream; we'll pin around them where we can).
- Attacks requiring control of the agent's own repository or host -- the
  repo is the agent; whoever controls it controls the agent by design.
- Denial of service against your own agent.
