# Knowledge sync, lifecycle, and isolation

## Knowledge syncs per run, atomically -- through staging, never the worktree

**Decision.** Routines never touch the real knowledge worktree or Git metadata.
Before an attempt, the supervisor copies knowledge into disposable staging; after success it validates the staged tree, imports the diff into the supervisor-only worktree, records the outcome, commits, and pushes.
Failed staging is discarded as one directory.

Validation and import both reject symlinks, hard links, Git control files, supervisor-owned state, and files outside size/count/depth limits.
Import happens beside the worktree and is promoted only after every file passes, so a rejected run leaves the worktree unchanged.
Git runs with pinned configuration, a constructed environment, and argument-safe invocation.

The worktree must be clean for human-curated knowledge; a blocked sync stops dispatch rather than acting on identities that could disappear with the container.
Each attempt has an intent commit before spawn and a completion commit after settlement.

**Why.** Model-directed processes write only disposable staging, so an interrupted run cannot corrupt future context, plant Git behavior, or overwrite human edits.
The extra validation at import is required because the model can change the staging tree after the initial walk.
## Hermetic git keeps the operator's credential configuration

**Decision.** Every git child runs with a constructed environment and suppressed system and global configuration, with one carve-out: `credential.*` entries from the system and global scopes are read once per process and re-injected as `-c` flags.
Nothing else from those files reaches a git child; repository-local configuration still loads as before.

**Why.** The suppression exists so machine configuration cannot change git's behavior under the supervisor -- hooks, transfer tweaks, URL rewrites.
But credential helpers live in exactly those suppressed files (macOS ships `osxkeychain` in the system gitconfig), so `sync` and `knowledge` could not fetch from an HTTPS origin at all, while plain `git ls-remote` in the same repository succeeded.
The passthrough already trusts the operator's SSH authentication surface -- HOME reaches ssh's keys and config, SSH_AUTH_SOCK reaches the agent -- and credential configuration is the same surface for HTTPS.
Reading configuration executes nothing; a helper runs only when git itself asks for credentials, exactly as it would in the operator's own shell.
In the deployed container no system or global credential configuration exists, so production behavior is unchanged.
## Shutdown: terminate fast, lean on at-least-once

**Decision.** On SIGTERM the supervisor stops launching work, terminates active process groups, discards staging, records interrupted attempts, returns unused reservations, makes a final knowledge commit-and-push, and exits.
There is no drain mode; the recommended container stop timeout is 30 seconds.

**Why.** The pending run survives and retries under the same run ID, so fast redeploys trade occasional repeated work for bounded shutdown time.
## Runs are sandboxed -- and local runs use the production container

**Decision.** Production attempts run under separate Unix identities, with Landlock applied by a capless re-exec shim when available.
The workspace is readable, staged knowledge and per-attempt temporary directories are writable, and a disposable per-attempt HOME prevents shared opencode state from becoming a cross-routine channel.
The rules never grant an ancestor of the workspace, and the required `/dev` access remains an explicit residual channel documented in the open questions.

The supervisor is non-dumpable and gives every child a constructed, secret-free environment.
File-delivered production keys stay outside the sandbox; the identity boundary, not Landlock, is the required boot guarantee.
Startup probes the parent-to-capless-helper UID transition and refuses to supervise if it fails; `OPENROUTINES_UNSAFE_NO_SANDBOX=1` can disable Landlock only.
A Linux integration test checks the real production rules.

Local runs use the production runtime image in a disposable container, while production keeps the model process in the supervisor's container behind the UID boundary.
In both cases the model sees only the assembled workspace and declared skills.
Network egress remains unrestricted beyond the credentials and knowledge the routine legitimately holds.

**Why.** Skill grants are context hygiene, not an access wall.
Landlock adds path denial where available, but the portable security contract is clean environments, per-attempt identities, staged knowledge, and a workspace assembled by allow-list.
## The required boundary is a per-attempt UID; Landlock is defense-in-depth

**Decision.** The production boundary between the supervisor, concurrent attempts, and manual runs is Unix UID separation.
Run slots reserve identities from a bounded pool; manual runs reserve one more identity under a kernel lock and coordinate knowledge settlement with the supervisor's knowledge lock.
An identity remains reserved through settlement, then the supervisor reaps every live process carrying that UID and verifies it is empty before reuse.
A failed reap poisons the identity and stops dispatch so an escaped descendant cannot read a later attempt.

The supervisor alone carries the narrowly scoped capabilities needed to launch and reap identities.
The child transitions to its requested UID/GID before executing a separate capless sandbox shim, so the routine cannot regain supervisor privilege by re-executing the helper.
Boot probes the same transition, and Landlock remains optional defense in depth.

Filesystem access is granted through per-attempt groups, not ownership changes: the supervisor owns staged trees, attempts receive only the group permissions they need, and umask keeps newly created files importable.
Missing supplementary-group setup fails closed.
Production master-key files must be readable only by the agent identity; the deploy key stays in the supervisor's private home.

**Why.** UID ownership expresses the required separation on hosts with or without Landlock and extends between concurrent attempts.
Landlock still denies paths a process owns and closes shared temporary roots where available, but it cannot be the only boot requirement.
The remaining shared-root and network-egress channels are documented as hardening backlog rather than presented as solved.
