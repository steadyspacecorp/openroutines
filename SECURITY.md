# Security

**openroutines** runs autonomous AI agents unattended, with credentials, on your infrastructure. Its primary security boundary separates untrusted model-directed execution from the supervisor's credentials, Git worktree, and host filesystem.

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

openroutines treats model-directed execution as untrusted. This includes model output, commands and tools selected by the model, memory produced by routines, and content fetched from external systems.

openroutines trusts:

- the agent repository's reviewed main branch, including its routines, skills, `agent.yaml`, and `opencode.json`;
- the container host and deployment configuration;
- administrators of the agent's Git origin;
- the selected base image, opencode distribution, model provider, and other runtime dependencies.

The repository is the agent. Someone who can change trusted repository content can change its behavior and authority by design. openroutines validates common mistakes in trusted inputs, but does not attempt to sandbox an agent from its own source code or administrators.

## Security properties

When deployed as documented:

- **Credential scoping:** a model process receives only the routine credentials declared in frontmatter, plus the credential needed for its selected model provider. Its environment is constructed rather than inherited. A typed credential is derived by the supervisor at spawn: the run receives short-lived, revoked-at-attempt-end material (for example a GitHub App installation token) and never the stored root secret.
- **Supervisor-key isolation:** with `OPENROUTINES_MASTER_KEY_FILE` and `OPENROUTINES_DEPLOY_KEY_FILE`, master and deploy key values do not enter the model process environment. Their files are outside the model filesystem sandbox.
- **Filesystem confinement:** local model execution occurs in a disposable container. Production model execution is confined with Landlock to the run workspace and required runtime paths. Writable paths are staged memory, the run temporary directory, a disposable per-attempt home, and `/dev`.
- **Git isolation:** model-directed processes do not receive the supervisor's Git worktree or Git metadata. They write to a disposable staging tree that the supervisor validates before importing.
- **Workspace minimization:** a run workspace is assembled from an allow-list of required repository content. Files such as the encrypted credential store and deploy keys are not copied merely because they exist in the repository.
- **No application ingress:** the shipped agent runtime does not listen on a network port. This does not prevent outbound connections or host-level access configured by an operator. Routine triggers stay outbound: the supervisor polls a frontmatter-declared URL (no redirects, bounded reads, raw credentials only) and treats the response as an opaque comparison value that is never logged raw and never enters model context.
- **Rewrite detection:** memory synchronization rejects a remote history that no longer descends from the last accepted tip, while the accepted reference remains available and trustworthy.

These controls are layered. Production model processes currently share the supervisor's UID; isolation between them relies on Landlock, a constructed environment, file-based key delivery, and a non-dumpable supervisor.

## Deployment requirements

The security properties above assume that operators:

- deliver master and deploy keys through read-only files;
- do not mount the Docker socket or unrelated host directories;
- run only one supervisor instance for an agent;
- keep the host, container runtime, base image, and openroutines release current;
- review repository changes, skills, credentials, and permission grants as code;
- do not disable the production sandbox with `OPENROUTINES_UNSAFE_NO_SANDBOX=1`, and do not run a deployed agent with `OPENROUTINES_NATIVE=1` or with the image's `OPENROUTINES_IN_CONTAINER=1` unset -- either can spawn an unconfined model process, and only the first of those fails closed.

Environment-variable key delivery is supported for compatibility, but has a weaker process-exposure posture than file delivery and is not the recommended production configuration.

## Known limitations

- **Network egress is unrestricted.** A prompt-injected routine can send anything it legitimately holds, including declared credentials and readable memory, to any reachable host. Credential and workspace scoping limit what it holds, not where it can send it.
- **Prompt injection is not solved.** External content may influence model behavior. Treat every skill, credential, and tool grant as authority.
- **Dry runs are not offline sandboxes.** `routines test` withholds declared routine credentials, discards staged memory changes, and denies action-oriented model tools. It still starts the trusted runtime and may contact the selected model provider.
- **Log redaction is best-effort.** Exact secret values are scrubbed from ordinary output, but transformed, encoded, fragmented, or indirectly disclosed values may evade redaction.
- **The supervisor and model process share a Linux user.** In the production container, opencode runs as a child of the supervisor under the same user ID. Unix user permissions therefore do not separate them; isolation relies on the layered controls described above.
- **Trusted code remains powerful.** A malicious routine, skill, `opencode.json`, container image, or host configuration is outside the model sandbox's threat boundary.
- **Release and dependency integrity is incomplete.** Release artifacts are checksummed and macOS binaries are ad-hoc signed; Developer ID signing and notarization are pending. The agent base image and its opencode installation are pinned by tag and version rather than by digest.

## Out of scope

- Attacks requiring control of the trusted agent repository, Git origin administration, container host, or deployment configuration.
- Vulnerabilities in opencode, model providers, Git, or the container runtime, though openroutines may pin or mitigate affected versions.
- Denial of service, scheduling errors, concurrent routine conflicts, or lost agent memory that do not grant access across a security boundary.
- Protection from credentials, files, or capabilities deliberately granted to a routine.
