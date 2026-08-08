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
## Confinement is best effort, and the operator owns the tradeoff

**Decision.** An ORA's routines come from one person or team and are reviewed like the rest of the repository, so this is not a multi-tenant system and does not try to be one.
What it defends against is a routine that goes wrong -- prompt-injected by something it fetched, or simply mistaken -- and what it promises is to bound the damage: no reaching another routine's credentials, the master or deploy key, the agent repository, or knowledge the run was not staged with.
Confinement serving that promise is therefore best effort: the strongest mechanism the host permits, chosen by probing and simply used.
The rung is logged, not argued about, because an operator whose host reaches only a lower one has no lever the ladder did not already try.
`OPENROUTINES_DISABLE_SANDBOX=1` is for a host that can build nothing and an operator who would rather run unconfined than not at all -- a deployment decision, never a routine's, noted once at boot.

**Why.** Nobody adopts OpenRoutines for its sandbox, and an isolation mechanism that turns an unusual host into a dead deployment has failed at the job people came for.
Blast radius is also the honest claim, because the sandbox is the third layer rather than the first: opencode scopes what a routine can ask for at all, and the constructed child environment scopes which credentials it holds.
The sandbox is what stays true when those two are subverted -- which is why it is worth having, and why it does not have to be perfect to be worth having.
Fail-closed stays the default for the one case where the loss is total: with no mechanism at all, concurrent runs share a uid with nothing between them, so the promise above is gone rather than weakened.
## Runs are sandboxed -- and local runs use the production container

**Decision.** In production every model process runs inside its own sandbox; the next decision covers which mechanism builds it.
Whichever is in force, the sandbox holds a read-only OS (`/usr`, `/bin`, `/sbin`, `/lib`, `/lib64`, `/opt`, and `/etc` entry by named entry rather than whole), `/proc`, a `/dev` assembled without the container's shared `/dev/shm`, the run workspace read-only, and read-write access to exactly three trees inside it: staged knowledge, the run tmp, and a disposable per-attempt HOME.
Out of reach: the agent repository, the knowledge worktree, the supervisor's `~/.ssh`, and every concurrent attempt's workspace.
The per-attempt HOME exists because a shared writable opencode home is a cross-routine channel -- opencode persists session state and auto-loads global plugins from its config dir -- and `/dev/shm` is withheld for the weaker form of the same reason.
Every run gets the network, because the model it calls is across it; per-routine egress is a proxy problem rather than a sandbox one.
Every sandboxed model process starts in a session of its own, or it could push characters into the controlling terminal with `TIOCSTI` and run commands outside the sandbox (CVE-2017-5226).

The sandbox fails closed at boot rather than mid-run: `supervise` builds a throwaway sandbox at startup and refuses to start when it cannot, unless the operator took the hatch above.
The supervisor is non-dumpable and gives every child a constructed, secret-free environment, because those protect it from anything sharing its container that is not a sandboxed run.
File-delivered keys must point outside every granted path, which the supervisor checks at boot rather than assumes.
An integration test builds real sandboxes and asserts each boundary from inside one; `bin/smoke` boots a real `supervise` in a container and asserts a dispatched run completes.

Local runs use the production runtime image in a disposable container, and `OPENROUTINES_NATIVE=1` is an explicit unconfined opt-in for contributors.
In production the topology differs and is worth stating precisely: the model process is a child of the supervisor in the same container, and the barrier between them is the run's own sandbox rather than a container boundary.
In both cases the model sees only the assembled workspace and declared skills.

**Why.** Skill grants are context hygiene, not an access wall -- a routine with bash can `cat` any file it can reach.
The sandbox turns declare-or-it-doesn't-exist into kernel enforcement: a path never granted cannot be opened, raced, guessed, or chmodded into reach, so there is no rule to get wrong, only a list to keep short.
Because a run holds the supervisor's own uid, a `0600` file inside a granted path is a readable file: modes protect nothing from a run, only absence from the grant does.
`/etc` was the concrete trap -- bound whole it looks like inert OS config, but it is where hosting platforms mount secret files, so a deployment that put its master key where its platform documents would have handed that key to every routine.
The boot check exists because the allow-list is necessary and not sufficient: it fixes the trap that is known, and the check catches the next one.
Network egress and resource use remain uncovered, and are documented as hardening backlog rather than presented as solved.
## The run boundary is a per-run sandbox, the strongest the host allows

**Decision.** The boundary between the supervisor and the model process, and between concurrent model processes, is a per-run sandbox built by the strongest mechanism the host actually permits, chosen by probing at boot rather than by configuration.
This supersedes "The required boundary is a per-attempt UID; Landlock is defense-in-depth"; the reserved identity pool and its lifecycle are retired with it.

There are two mechanisms, tried strongest-first, and the supervisor takes the first that can really build a throwaway sandbox here.
Bubblewrap is preferred: private mount, pid, ipc, uts and user namespaces per attempt, plus a cgroup namespace where the host allows one.
It has two variants differing only in how `/proc` reaches the sandbox -- a private `/proc` hides peer attempts, and where the runtime masks `/proc` paths the kernel refuses that mount, so the container's own `/proc` is bound read-only instead, costing process-list privacy and nothing else.
A plain Landlock domain is the fallback: no namespaces, and nothing asked of the host at all -- no runtime flag, no capability, no sysctl, just syscalls the default container seccomp profile already allows.
Where neither works the supervisor refuses to start, unless the operator disabled the sandbox deliberately.

Both express the same grant list in each backend's own vocabulary, which is what keeps the "can a run reach this key file?" boot check correct on every rung without duplicating the security-critical part.
Neither gives an attempt a network namespace of its own, so attempts share the container's -- and therefore its localhost ports and abstract socket namespace.

The two are complementary rather than nested, because they ask the host for very different things.
Bubblewrap needs unprivileged user namespaces, which three separate things can deny: a runtime's default seccomp profile refuses the namespace-creating syscalls, its default AppArmor profile contains a bare `deny mount,`, and a host can set `kernel.apparmor_restrict_unprivileged_userns=1`.
The third is not addressable from `docker run` at all and is the default on Ubuntu 24.04 and its derivatives.
The Landlock rung is immune to all three because it creates no namespace to be restricted; its only requirement is Landlock compiled into a Linux 5.13 or newer kernel, which is what makes the design deployable on a platform whose entire interface is a repository with a Dockerfile.
The exchange runs the other way too: a microVM host has no runtime profile to lift and no such sysctl, so bubblewrap reaches the top rung unconfigured on a kernel that may carry no Landlock at all.

What a rung is worth is declared rather than assumed: each backend states whether it provides unnameable paths, a private process list, peer signal isolation, private IPC, a private `/tmp` and `/dev/shm`, and process-tree collapse, and code that depends on a property asks for it by name.
That is load-bearing in exactly one place -- the runner sweeps the run's process group only where nothing else collapses the tree.
On the Landlock rung the sweep is incomplete: a descendant that called `setsid` is outside the process group and lingers, still confined by an inherited domain it cannot drop.
Two capabilities vary with the kernel rather than the rung, and both are reported rather than assumed: `LANDLOCK_SCOPE_SIGNAL` arrived in Linux 6.12, so below it a run can signal a peer -- a denial of service between routines, never a disclosure, because a peer's secrets are protected by the `ptrace_may_access` check Landlock has hooked since its first version -- and `LANDLOCK_ACCESS_FS_TRUNCATE` arrived in 6.2, so below it a run can empty an ungranted file it can neither read nor write.
File metadata is outside Landlock at any ABI.
Those limits are recorded in SECURITY.md rather than guarded in code.

The dependency posture is the one place this decision spends something.
`bwrap` is an OS package invoked as a subprocess, which keeps it out of the supervisor's Go dependency tree and brings two properties from outside the argument list: every bind is mounted `nodev`, and `PR_SET_NO_NEW_PRIVS` is set unconditionally.
Landlock cannot be a subprocess -- a domain has to be installed by the process being confined -- so the supervisor re-executes its own binary as an internal `sandbox-exec` shim that restricts itself and then execs the model process.
Enforcement there is `github.com/landlock-lsm/go-landlock`, a deliberate exception to the near-zero-dependency rule: the alternative is owning ruleset construction, ABI downgrade, and the all-threads restriction a Go runtime needs, forever, as security-critical code.
A setuid `bwrap` is not the lighter version of the same idea -- it would be a permanent privilege-escalation primitive inside the container, and `bwrap` refuses to run carrying file capabilities anyway.

An integration test builds real sandboxes on every rung available and asserts each boundary from inside one, and asserts just as hard that a rung gets no credit for a property it lacks.
`bin/smoke` boots a real `supervise` in a container three times: with the flags that select bubblewrap, with no runtime flags at all, and with the sandbox disabled.
CI sets `OPENROUTINES_REQUIRE_SANDBOX=1` so "no sandbox here" fails rather than skips, and lifts the userns sysctl its runners set.

**Decision (secrets).** A file-delivered master key in production must be mounted for the `agent` identity without group or other read bits, and key loading rejects a broader mode.
The deploy key stays a file because `GIT_SSH_COMMAND` points `ssh -i` at a path, but its `0600` copy lives in the supervisor's `0700` home.
What keeps either from a run is that no sandbox grants it -- absence, not ownership, because a run holds the supervisor's own uid and satisfies every mode check the supervisor does.
Two things follow from absence being a property of the grant list rather than of the file: the sandbox grants `/etc` entry by named entry, and the supervisor resolves both configured key paths at boot and refuses to start if either lands inside a granted path, naming the variable to fix.
One boot step checks both keys against one rule -- can the thing we isolate reach it?
A key file inside a granted path is fatal; a key value in the environment is a warning, exposed to the host but not to a run, whose environment is constructed rather than inherited.

**Why.** Every property a UID boundary enforces is a statement about which UID may read which path, and expressing it that way costs a whole identity lifecycle -- allocating, granting, reaping, proving, poisoning, and reclaiming -- plus file capabilities on the supervisor binary and a reserved identity pool baked into the image.
A sandbox expresses the same properties by construction and needs none of that machinery.
It also closes channels ownership could only narrow: a `1777` root like `/dev/shm` is a shared directory no mode can lock, and a sandbox simply does not contain one.
The honest cost is that the boundary now depends on a kernel feature and the host's security policy rather than on Unix UIDs, which exist everywhere -- which is exactly why it is chosen by probing.
Kernel version alone was never a sufficient test: build config and host policy are independent gates, and a sandbox that actually builds is the only claim worth making.

Requiring one mechanism is the mistake this decision exists to avoid.
Bubblewrap alone would refuse to boot on a stock Ubuntu 24.04 host, through a restriction no runtime flag can lift, and on every platform whose entire interface is a repository with a Dockerfile.
A boundary that refuses to run on the commonest deploy targets is not a stronger boundary, it is a boundary nobody gets.
With something underneath, the demotion is graceful: a masked `/proc` costs process-list privacy, an unavailable namespace costs unnameability and process-tree collapse, and only where nothing works does the supervisor refuse.

The runtime flags the top rung asks for are a real weakening of the container's own hardening, so on that rung the trade is not "add isolation" but "move isolation inward one level".
That is defensible when the threat is prompt injection steering one trusted routine against another inside a single-tenant container, and much less so if the container boundary is doing real work.
The Landlock rung asks for nothing, so there it is not a trade at all -- an operator who cares more about the outer boundary can pass no flags and get the fallback, deliberately.
