# Security

**OpenRoutines** runs autonomous AI agents unattended, with credentials, on your infrastructure. Its primary security boundary separates untrusted model-directed execution from the supervisor's credentials, Git worktree, and host filesystem.

An agent's routines are trusted content, authored and reviewed by one team; this is not a multi-tenant system. What the controls below bound is the damage one routine can do when it is prompt-injected or goes wrong -- reaching another routine's credentials, the supervisor's keys, the repository, or the host. Confinement is best effort by design: the strongest mechanism the host permits, chosen automatically, with the weaker outcomes documented here rather than implied.

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
- **Supervisor-key isolation:** with `OPENROUTINES_MASTER_KEY_FILE` and `OPENROUTINES_DEPLOY_KEY_FILE`, master and deploy key values do not enter the model process environment. Because a run shares the supervisor's user, a key file is protected by not being granted to the run rather than by its mode -- so the sandbox grants `/etc` entry by entry rather than whole (platforms deliver mounted secrets there), and the supervisor refuses to start if either configured key path resolves inside a granted path, naming the variable to fix. Under either delivery mode, the supervisor's children -- the per-minute git invocations included -- receive constructed environments, so an environment-delivered key is not republished in a child's `/proc/<pid>/environ`.
- **Run confinement:** local model execution occurs in a disposable container. Production model execution occurs inside a per-run sandbox built by the strongest mechanism the host permits, selected by probing at boot (see the rung table below; every rung confines every run, and they differ in how much more they provide), unless an operator has disabled it explicitly. The run sees a read-only OS -- system directories whole, `/etc` by named entry -- its own workspace, and nothing else; writable paths are staged knowledge, the run temporary directory, and a disposable per-attempt home. The agent repository, the knowledge worktree, the deploy key, and other runs' workspaces are never granted. Post-run bookkeeping re-executes the runtime as an ordinary supervisor child to read what the attempt consumed; it runs with an empty home and configuration directory, so it does not load plugins or configuration the run itself could write.
- **Git isolation:** model-directed processes do not receive the supervisor's Git worktree or Git metadata. They write to a disposable staging tree that the supervisor validates, then re-checks file by file against what it actually opens as it imports -- a tree a model process could still be writing is not trusted to be what an earlier walk found. A rejected tree is imported in no part.
- **Workspace minimization:** a run workspace is assembled from an allow-list of required repository content. Files such as the encrypted credential store and deploy keys are not copied merely because they exist in the repository.
- **No application ingress:** the shipped agent runtime does not listen on a network port. This does not prevent outbound connections or host-level access configured by an operator. Routine triggers stay outbound: the supervisor polls a frontmatter-declared URL (no redirects, bounded reads), sends either a granted raw bearer or short-lived bearer material derived from a granted typed credential, cleans up derived material immediately after the poll, and treats the response as an opaque comparison value that is never logged raw and never enters model context.
- **Rewrite detection:** knowledge synchronization rejects a remote history that no longer descends from the last accepted tip, while the accepted reference remains available and trustworthy.

These controls are layered. Production model processes run as the supervisor's own unprivileged user, so isolation between concurrent runs -- and between a run and the supervisor -- rests on the run's own confinement rather than on distinct identities. Supervisor secrets are additionally protected by a constructed child environment, file-based key delivery, and a non-dumpable supervisor.

### The confinement rungs

No single confinement mechanism is available on every host, so the supervisor probes at boot and takes the strongest that works, logging which one. The rungs are **not equivalent**.

| | bubblewrap, private `/proc` | bubblewrap, shared `/proc` | Landlock |
|---|---|---|---|
| A run can read a peer run's or the supervisor's credentials, memory, or files | no | no | no |
| A run can ptrace a peer or the supervisor | no | no | no |
| A run can signal a peer or the supervisor | no | no | no on Linux 6.12+; **yes** below it |
| A run can see that a peer exists, and read its command line | no | **yes** | **yes** |
| An ungranted path is *absent* rather than permission-denied | yes | yes | **no** |
| `/tmp` and `/dev/shm` are the run's own | yes | yes | **no** -- withheld instead of shared |
| The run gets its own IPC namespace | yes | yes | **no** |
| Killing the run collapses its whole process tree | yes | yes | **no** -- see below |
| Needs a permissive container runtime | yes | yes | no |
| Needs Landlock in the kernel (Linux 5.13+) | no | no | yes |

On the bubblewrap rungs the boundary is a set of namespaces: a peer is not a process this run can enumerate and not a path it can open. On the Landlock rung it is a domain the run is placed in before it execs, which the kernel checks on every file access and on every `ptrace_may_access` call -- the check standing behind `/proc/<pid>/environ`, `/proc/<pid>/mem`, `process_vm_readv`, and `PTRACE_ATTACH`. So what differs is not whether one run's secrets can reach another but how the reaching fails: absence on the bubblewrap rungs, denial on the Landlock one, plus process-tree teardown, process-list privacy, and signals.

**Signals vary with the kernel rather than with the rung.** `LANDLOCK_SCOPE_SIGNAL` arrived in Linux 6.12; below that a run on the Landlock rung can signal a peer attempt or the supervisor. That is a denial of service between routines inside one agent, never a disclosure -- what protects a peer's secrets is the ptrace check, which Landlock has enforced since its first version. The supervisor reports which answer the kernel gave rather than assuming either.

**Process-tree teardown on the Landlock rung is weaker**, and it is the difference most worth knowing. With no PID namespace to collapse, the runner sweeps the run's process group instead. A descendant that placed itself in a new session (`setsid`) is outside that group and is not swept, so it can outlive the attempt. It remains confined -- a Landlock domain is inherited by children and cannot be dropped -- but it is a lingering process, and on the bubblewrap rungs it would not exist.

## Deployment requirements

The security properties above assume that operators:

- deliver master and deploy keys through read-only files;
- do not mount the Docker socket or unrelated host directories;
- run only one supervisor instance for an agent;
- keep the host, container runtime, base image, and OpenRoutines release current;
- review repository changes, skills, credentials, and permission grants as code;
- grant what at least one confinement rung needs (see [docs/operating.md](docs/operating.md#run-confinement)) -- a supervisor that can build no sandbox at all refuses to start, so this fails closed;
- treat `OPENROUTINES_DISABLE_SANDBOX=1` as an escape hatch of last resort. It exists so a host that can build no sandbox is a warning rather than a dead deployment, and it gives up every property in the rung table below: runs then share a user and a filesystem with each other and with the supervisor, so one routine can read another's credentials and the configured key files;
- do not run a deployed agent with the image's `OPENROUTINES_IN_CONTAINER=1` unset -- that variable is what selects the sandbox, and without it the contributor opt-out `OPENROUTINES_NATIVE=1` spawns model processes with no isolation at all. Inside the image the production setting outranks that opt-out, so a deployed agent cannot be talked out of its sandbox.

Environment-variable key delivery is supported for compatibility, but has a weaker process-exposure posture than file delivery and is not the recommended production configuration. The supervisor warns once at boot when either key value -- master or deploy -- is present in its environment, including when it is a leftover variable alongside file delivery.

## Known limitations

- **Network egress is unrestricted.** A prompt-injected routine can send anything it legitimately holds, including declared credentials and readable knowledge, to any reachable host. Credential and workspace scoping limit what it holds, not where it can send it.
- **Prompt injection is not solved.** External content may influence model behavior. Treat every skill, credential, and tool grant as authority.
- **`--no-knowledge` is not a dry run.** `routines run --no-knowledge` receives declared credentials and tools and may perform external actions. It discards only staged knowledge changes and the run record.
- **Log redaction is best-effort.** Exact secret values are scrubbed from ordinary output, but transformed, encoded, fragmented, or indirectly disclosed values may evade redaction. The same redaction applies to the knowledge entries the supervisor writes, which are committed and pushed; its scope there is the supervisor's own master and deploy keys. Knowledge written by a model process is imported as authored and is not redacted.
- **The supervisor and model process share a Linux user.** In the production container, opencode runs as a child of the supervisor under the same user ID. Unix user permissions therefore do not separate them: the separation is the run's own confinement, plus the layered controls described above. The consequence worth stating on its own is that file modes protect nothing from a run -- only a path being absent from what the sandbox grants does -- so what a run can read is decided entirely by that grant list, and the supervisor refuses to boot if a file-delivered key sits inside it.
- **Concurrent runs share a network namespace.** A run's isolation covers processes and files, not sockets: attempts share the container's network namespace, so they share its loopback interface and its abstract Unix socket namespace. Two concurrent runs can therefore reach each other over localhost, and a run can reach any local service the container runs. Per-run network namespaces are possible but not implemented -- the model provider is across the network, so this needs a per-run egress path rather than just a flag.
- **A run's resource use is not capped.** No memory, process-count, or disk limit is imposed on a model process beyond the routine's timeout. A run can exhaust the container's disk or process table for the duration of its attempt. Its workspace is discarded afterwards and its processes are torn down, so the effect is bounded in time but not in severity -- less tightly on the Landlock rung, where a descendant in a new session survives the sweep (see the rung table above).
- **Trusted code remains powerful.** A malicious routine, skill, `opencode.json`, container image, or host configuration is outside the model sandbox's threat boundary.
- **Release and dependency integrity is incomplete.** Release artifacts are checksummed (and the installer verifies the checksum), and darwin arm64 binaries carry the Go linker's ad-hoc signature; Developer ID signing and notarization are pending. The agent image's Debian base and its opencode installation are pinned by tag and version rather than by digest.

## Out of scope

- Attacks requiring control of the trusted agent repository, Git origin administration, container host, or deployment configuration.
- Vulnerabilities in opencode, model providers, Git, or the container runtime, though OpenRoutines may pin or mitigate affected versions.
- Denial of service, scheduling errors, concurrent routine conflicts, or lost agent knowledge that do not grant access across a security boundary.
- Protection from credentials, files, or capabilities deliberately granted to a routine.
