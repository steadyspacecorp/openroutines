# Configuration and framework boundaries

## Credentials: encrypted in the repo, scoped per routine

**Decision.** Secrets ship encrypted in the repository, Rails-style: one encrypted file, one master key kept out of the repo (a gitignored file locally, preferably a mounted file in production).
Environment delivery remains available for platforms that cannot mount secrets, with a weaker process-exposure posture -- and the supervisor warns at boot when that is what it got, for the deploy key as much as the master key, since a deployment that chose it once is otherwise never told again.
A routine's process environment receives only the credentials its frontmatter declares (`slack_webhook` → `SLACK_WEBHOOK`), decrypted in memory at spawn time -- never written to disk.
Three rules make the scoping real:

1. **Routine environments are built from scratch, never inherited.** The supervisor holds the master and deploy keys (as file paths, preferably); children get a minimal constructed env (PATH, HOME, TZ, declared credentials, and framework-injected metadata like `OPENROUTINES_RUN_ID`) and nothing else. The `OPENROUTINES_*` prefix is reserved for that framework metadata -- never for secrets, and `openroutines check` rejects credential names that collide.
2. **Model provider keys auto-inject by provider.** They live in the same encrypted file under reserved names; a routine declaring `model: anthropic/...` receives `ANTHROPIC_API_KEY` and no other provider's key, with no frontmatter boilerplate.
3. **Injected secrets are scrubbed from run output.** The supervisor knows every value it injected and redacts them from everything the routine's process emits -- an echoed manual run, opencode's log passed through into the supervisor's (the GitHub Actions trick). Exported sessions in operator storage are deliberately outside this rule: stored history is the operator's, verbatim. Stored values are one line in service of this rule: exact-string matching cannot catch a value that spans lines, so `credentials set` folds multi-line input (an App private key) into an escaped `\n` form on entry -- and refuses a PEM missing its END marker rather than storing a truncated key that decrypts cleanly and fails first in production. Typed consumers decode the escaping on use; a raw multi-line credential is delivered escaped. This is defense in depth, not a guarantee -- exact-value matching can't catch an encoded or split secret -- but it turns the common accident (a routine echoing `$SLACK_WEBHOOK`) into a non-event. The same redaction runs at the knowledge append seam rather than at the call sites that write entries: what the supervisor records in `events.md` and `tasks.md` is committed and pushed, so a git error quoting key material would be a durable, published record, and a seam every entry passes through is the only place that covers the paths nobody remembered. That seam redacts the supervisor's *own* secrets (master key, deploy key). A routine's own credentials are not in that set, and knowledge the model wrote arrives by import rather than through the seam: those are the model's to leak within its declared authority, which is the scoping argument, not a redaction one.

**Why.** The Rails model is battle-tested and requires no secrets platform -- the repo stays self-contained without containing a usable secret on its own.
Per-routine scoping is the substantive security claim: most frameworks hand every tool every secret; here a routine does not receive the deploy key or undeclared routine credentials, and the grant is visible in the diff that adds it.
The clean-env rule is what makes that claim true rather than decorative -- an inherited environment would hand every routine the master key, which unlocks everything.
Log scrubbing is defense in depth: it turns the common accident of echoing an exact injected value into a non-event, but it is not a data-loss-prevention boundary.

## Credentials have types: derived, not just injected

**Decision.** A `credentials:` entry in `openroutines.yml` may declare how a stored credential is materialized into a run.
No entry means `raw` -- injected verbatim under its uppercase name, the default and the entire behavior until now.
A typed entry (`github_app_private_key: { type: github_app, app_id: "..." }`) makes the trusted runner transform the stored root secret at spawn: the routine's frontmatter grant is unchanged, but its environment receives the type's derived surface and never the root secret.
A derived type defines its injected environment, its scrub values, and its cleanup; it is minted fresh per attempt and revoked at attempt end.
Providers are built into the framework -- agent repositories cannot supply derivation code, which would be a privileged plugin boundary on the trusted side.
The first type is `github_app`: sign the App JWT, discover the App's single installation (plurality is refused), mint an unscoped installation token that inherits the installation's repository selection and permissions, resolve the bot identity, and inject `GITHUB_TOKEN`/`GH_TOKEN`, `GITHUB_APP_SLUG`, and Git author/committer identity.
The second is `oauth2_client`: the stored value is an OAuth2 client's secret, exchanged at spawn via the client-credentials grant (form-encoded POST to the entry's `token_url` with its `client_id`) for a bearer injected under the entry's `inject_as` name, uppercased into the run environment.
It has no cleanup: client-credentials bearers are typically non-revocable and simply expire, an asymmetry with `github_app` stated rather than hidden.
Type names name the thing the stored secret belongs to (`github_app`, `oauth2_client`), never the protocol ceremony.
Access is per-agent, not per-routine: the App installation states the agent's whole GitHub reach, exactly as a person's own access does, and an agent whose access feels too broad should be two agents.

**Why.** Injecting an App private key as an ordinary credential hands every declaring routine a long-lived, installation-wide signing key that unrestricted egress cannot keep in the box -- exfiltrated once, it mints tokens until rotated.
Deriving supervisor-side makes the routine's authority what it actually needs: a scoped token that expires in an hour and is revoked when the attempt ends -- and the supervisor finally knows the token, so scrubbing covers it.
Modeling this as a credential *type* rather than a parallel grant system keeps the vocabulary small: frontmatter is untouched, `check` keeps one namespace, and absence-of-metadata *is* the migration story.
Per-routine narrowing is rejected deliberately -- agents are modeled after people, whose access is also not re-scoped per task -- and one installation per App is required because, with no configured installation ID, plurality would make the grant ambiguous.

## Variables: non-secret configuration in openroutines.yml

**Decision.** Non-secret configuration lives in a `variables:` map in `openroutines.yml` -- the GitHub secrets-vs-variables split.
Every run receives every variable in its environment under the same name mapping credentials use (`product_repo` → `PRODUCT_REPO`), and the standing instruction lists the available names so the model uses them instead of hardcoding values.
There is no per-routine scoping and no CLI: `openroutines.yml` is hand-edited, versioned, and reviewed like any other config.
Names follow the credential rules (lowercase snake_case, no reserved `openroutines` prefix) plus one more: they must not shadow the env vars the framework constructs (`TZ`, `PATH`, `HOME`, `TMPDIR`).
On a name collision with a stored credential the credential wins, and `openroutines check` fails the repo until one is renamed.

**Why.** The interface is deliberately identical to credentials -- a prompt or skill says `$STEADY_TOKEN` and `$PRODUCT_REPO` the same way -- so the only question a value ever raises is "is this secret?", and the answer decides where it lives.
Scoping would be ceremony rather than security: `openroutines.yml` travels into every run workspace, so a routine can already read every variable; withholding them from the environment would protect nothing.
And a CLI would be ceremony too -- the whole point of plaintext config in a versioned file is that editing the file *is* the interface.

## No application ingress

**Decision.** The shipped ORA runtime listens on no network port.
There is no admin UI, webhook receiver, or chat gateway.
Routines may reach *out* to networked services through their skills.
Operators still control the agent through its repository, Git origin, container host, and deployment platform.

**Why.** An agent that holds credentials and acts unattended should add no application-level inbound attack surface.
Anything that needs to supply work or context does so through systems the routines call or through trusted repository changes, not an OpenRoutines network service -- and anything the agent needs to hear about, it polls for (see Triggers, including why a webhook receiver was rejected).
Interactive development happens locally, with your own coding agent, against `AGENTS.md`.
Normal host, Git, and deployment access controls remain the operator's responsibility.

## The supervisor is a small Go binary

**Decision.** The supervisor -- scheduling, locking, process control, git sync, and credential handling -- is one released Go binary with a small, auditable dependency tree.

**Why.** The supervisor is trusted with master and deploy keys, so dependency surface matters.
Go's standard library covers the process and filesystem primitives, and a static binary keeps the runtime image small.
## Framework code is versioned out of the agent repo

**Decision.** Framework logic ships in the released `openroutines` binary.
An agent repo carries a version pin and a Dockerfile that installs that pin; local commands warn when the binary and pin disagree.
`openroutines update` applies framework-owned template changes and leaves user-owned files such as `opencode.json` alone.

**Why.** Pinning the binary avoids copying framework code into every agent and makes deployed behavior reproducible.
Updates remain reviewable commits, and rollback is a normal Git revert.
