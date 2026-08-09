# Open questions

## Open questions

Hardening backlog -- known gaps between the design above and a fully defensible production posture, roughly in priority order.
(This section holds the reasoning; execution is tracked in the repo's GitHub issues.)

- **Threat model document.** A trust-boundary inventory: what we trust (the `main` branch, the container host, opencode, the model provider) versus what we don't (knowledge content, skill content, model output, fetched web content), and what each enforcement layer is responsible for.
- **Per-routine network egress control.** The largest remaining exfiltration channel. Candidates: Landlock TCP scoping (kernel 6.7+) for port-level rules, or an in-container proxy with per-routine destination allowlists for host-level ones.
- **World-writable roots.** The `1777` roots are the residual from "The required boundary is a per-attempt UID": pointing each attempt's `TMPDIR`/HOME inside its own workspace keeps the runtime out of them by default; Landlock excludes `/tmp` and `/var/tmp` where available, but the required `/dev` grant still reaches `/dev/shm`. Without Landlock, two overlapping attempts can also pass data through `/tmp` or `/var/tmp`. Candidate closures: a private per-attempt `/dev/shm` and tmp mount (needs mount privilege the supervisor does not currently hold), or narrowing what the runtime is allowed to open there. Bounded meanwhile by unguessable per-attempt paths and the passive-channel-not-code-planting distinction.
- **Container hardening contract.** The template Dockerfile and docs should specify: non-root user, read-only root filesystem, dropped capabilities, no-new-privileges, knowledge/CPU/disk limits, and an explicit prohibition on mounting the Docker socket.
- **Secret lifecycle specifics.** Cipher and format for the credentials file (authenticated encryption, versioned header), master-key generation entropy, rotation story for both master key and individual credentials, and deploy-key delivery (file-based secret preferred over env var where the platform allows).
- **Supply-chain posture.** Signed releases and checksums for the binary and install script, pinned opencode version and base-image digest in the template Dockerfile, and provenance guidance for vendored skills.
- **Schema versioning.** A version marker in `openroutines.yml`, frontmatter, the credentials file, and run records, so `openroutines update` can migrate them deliberately.
- **Scheduling edge cases.** DST gaps and repeats (which zone cron evaluates in is settled above; what a 02:30 firing should do on spring-forward day is the cron library's behavior, inherited rather than chosen), changing an agent's `timezone:` mid-life, clock rollback, garbage-collecting deleted routines' scheduling state, and validation rules for names (case-folding, env-var collisions, path traversal).
- **Observability contract.** A stable log schema (run IDs, outcomes, durations, token/cost figures where opencode reports them) so log-scraping monitors have something dependable to scrape.
