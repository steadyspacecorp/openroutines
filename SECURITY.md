# Security

**OpenRoutines** runs autonomous AI agents unattended, with credentials, on your infrastructure. Its primary security boundary separates untrusted model-directed execution from the supervisor's credentials, Git worktree, and host filesystem.

## Reporting a vulnerability

Email security@steady.space, or -- once this repository is public -- report privately via [GitHub's private vulnerability reporting](../../security/advisories/new). Please do not open a public issue for an exploitable vulnerability. You should receive an acknowledgment within a few days; this is a small project and we will be candid about timelines. We do not accept AI generated security reports

A useful report identifies:

- the attacker's starting access;
- the trust boundary crossed;
- the resulting access or impact;
- a reproduction against the latest release.

Correctness, durability, and availability bugs are welcome as regular GitHub issues unless they cross a security boundary described below.

Before 1.0 there are no backported fixes: a fix ships in the next release, and `openroutines update` brings an agent to the version of the binary you run.

## Security model

OpenRoutines treats model-directed execution as untrusted. This includes model output, commands and tools selected by the model, knowledge produced by routines, and content fetched from external systems.

OpenRoutines trusts:

- the agent repository's reviewed main branch, including its routines, skills, `openroutines.yml`, and `opencode.json`;
- the container host and deployment configuration;
- administrators of the agent's Git origin;
- the selected base image, opencode distribution, model provider, and other runtime dependencies.

The repository is the agent. Someone who can change trusted repository content can change its behavior and authority by design. OpenRoutines validates common mistakes in trusted inputs, but does not attempt to sandbox an agent from its own source code or administrators.

## Security properties

When deployed as documented:

- **Credential scoping:** a model process receives only the routine credentials declared in frontmatter, plus the credential needed for its selected model provider. Its environment is constructed rather than inherited. A typed credential is derived by the supervisor at spawn: the run receives short-lived, revoked-at-attempt-end material (for example a GitHub App installation token) and never the stored root secret.
- **Web-access scoping:** the built-in web tools are denied by default; a run's generated agent definition allows webfetch or websearch only when the routine's frontmatter declares the grant. This scopes the model's general-purpose retrieval surface -- it is not egress control or content isolation: a routine's shell commands can still reach the network, and their responses still enter model context.
- **MCP scoping:** configured MCP servers' tools are denied by default and allowed only for routines whose frontmatter grants the server (`mcp: [name]`). This scopes which routines' context receives a server's third-party tool descriptions -- the standard MCP injection surface -- and which can invoke its tools. Connections still require the matching credential grant for the server's auth header.
- **Supervisor-key isolation:** with `OPENROUTINES_MASTER_KEY_FILE` and `OPENROUTINES_DEPLOY_KEY_FILE`, master and deploy key values do not enter the model process environment. Their files are outside the model filesystem sandbox. Under either delivery mode, the supervisor's children -- the per-minute git invocations included -- receive constructed environments, so an environment-delivered key is not republished in a child's `/proc/<pid>/environ`.
- **Filesystem confinement:** local model execution occurs in a disposable container. In production, every concurrent attempt runs under its own reserved Unix identity; the assembled workspace grants that identity access without admitting other attempts, and Landlock further confines accessible paths when the host supports it. Writable paths are staged knowledge, the run temporary directory, a disposable per-attempt home, and `/dev`. Post-run bookkeeping re-executes the runtime as an ordinary supervisor child to read what the attempt consumed; it runs with an empty home and configuration directory, so it does not load plugins or configuration the run itself could write.
- **Git isolation:** model-directed processes do not receive the supervisor's Git worktree or Git metadata. They write to a disposable staging tree that the supervisor validates, then re-checks file by file against what it actually opens as it imports -- a tree a model process could still be writing is not trusted to be what an earlier walk found. A rejected tree is imported in no part.
- **Workspace minimization:** a run workspace is assembled from an allow-list of required repository content. Files such as the encrypted credential store and deploy keys are not copied merely because they exist in the repository.
- **No application ingress:** the shipped agent runtime does not listen on a network port. This does not prevent outbound connections or host-level access configured by an operator. Routine triggers stay outbound: the supervisor polls a frontmatter-declared URL (no redirects, bounded reads), sends either a granted raw bearer or short-lived bearer material derived from a granted typed credential, cleans up derived material immediately after the poll, and treats the response as an opaque comparison value that is never logged raw and never enters model context.
- **Rewrite detection:** knowledge synchronization rejects a remote history that no longer descends from the last accepted tip, while the accepted reference remains available and trustworthy.

These controls are layered. Production attempts do not share the supervisor's UID or one another's UIDs; boot verifies the identity transition and the supervisor proves an identity empty before reusing it. Landlock adds another filesystem boundary on hosts that support it, while constructed environments, file-based key delivery, and a non-dumpable supervisor protect secrets independently.

## Deployment requirements

The security properties above assume that operators:

- deliver master and deploy keys through read-only files;
- do not mount the Docker socket or unrelated host directories;
- run only one supervisor instance for an agent;
- keep the host, container runtime, base image, and OpenRoutines release current;
- review repository changes, skills, credentials, and permission grants as code;
- preserve the current image's `OPENROUTINES_IN_CONTAINER=1` marker, attempt users and groups, capless sandbox helper, and narrowly scoped supervisor file capabilities -- production boot refuses to dispatch when the required identity transition or cleanup probe fails;
- leave `OPENROUTINES_UNSAFE_NO_SANDBOX` unset unless you deliberately accept losing Landlock's defense-in-depth on a host where it is available; the override does not disable the required per-attempt UID boundary.

Environment-variable key delivery is supported for compatibility, but has a weaker process-exposure posture than file delivery and is not the recommended production configuration. The supervisor warns once at boot when the master key value is present in its environment, including when it is a leftover variable alongside file delivery.

## Known limitations

- **Network egress is unrestricted.** A prompt-injected routine can send anything it legitimately holds, including declared credentials and readable knowledge, to any reachable host. Credential and workspace scoping limit what it holds, not where it can send it.
- **Prompt injection is not solved.** External content may influence model behavior. Treat every skill, credential, and tool grant as authority.
- **A manual run is not a dry run.** `routines run` receives declared credentials and tools and may perform external actions. Its default only discards staged knowledge changes and the run record; `--write-knowledge` settles them.
- **Log redaction is best-effort.** Exact secret values are scrubbed from ordinary output, but transformed, encoded, fragmented, or indirectly disclosed values may evade redaction. The same redaction applies to the knowledge entries the supervisor writes, which are committed and pushed; its scope there is the supervisor's own master and deploy keys. Knowledge written by a model process is imported as authored and is not redacted.
- **`/dev/shm` is shared across runs.** `/dev` is writable in the production sandbox and the tmpfs mounted at `/dev/shm` is reachable through that grant, so runs and retry attempts can pass data through it even though each attempt otherwise gets a disposable workspace and home. Nothing auto-loads from it, so it is a data channel rather than a code-execution one.
- **Trusted code remains powerful.** A malicious routine, skill, `opencode.json`, container image, or host configuration is outside the model sandbox's threat boundary.
- **Release and dependency integrity is incomplete.** Release artifacts are checksummed (and the installer verifies the checksum), and darwin arm64 binaries carry the Go linker's ad-hoc signature; Developer ID signing and notarization are pending. The agent image's Debian base and its opencode installation are pinned by tag and version rather than by digest.

## Out of scope

- Attacks requiring control of the trusted agent repository, Git origin administration, container host, or deployment configuration.
- Vulnerabilities in opencode, model providers, Git, or the container runtime, though OpenRoutines may pin or mitigate affected versions.
- Denial of service, scheduling errors, concurrent routine conflicts, or lost agent knowledge that do not grant access across a security boundary.
- Protection from credentials, files, or capabilities deliberately granted to a routine.
