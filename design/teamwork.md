# Teamwork

## Teamwork participation is one ladder key

**Decision.** A routine declares its teamwork participation with a single `teamwork` key: `full` (the default -- runs are recorded as events, and scheduled fires fill `schedule.md`'s tables), `events` (runs are recorded, but fires appear as `fact:` lines only -- the conditional custodian: the work still happens on schedule, it just isn't advertised in the tables the other runs read), and `off` (neither -- reporting routines, where checking in is not work).
The retired `events` key is parsed only to be rejected with the mapping (`events: false` is now `teamwork: off`): a routine still declaring it is one broken routine, named by `check`, never silently reinterpreted.

**Why.** The two participation axes -- is a run recorded, how does a fire appear -- are not independent: a fire filling the tables while its runs leave no record would have reporting announce activity the record can never confirm, so that state was never legal.
Two boolean keys encode four states and need an implication rule plus a `check` error to police the illegal one; a three-value ladder encodes exactly the three legal states, and the contradiction becomes unrepresentable.
The values are words the system already taught -- `events` names precisely what the tier gates, and `off` is plain English -- so the key adds no vocabulary.
The ladder is closed by construction: the excluded state can never become legal, so the enum cannot sprawl.

## The teamwork lexicon

**Decision.** Teamwork terms name artifacts users can open: `events.md`, `tasks.md`, `context.md`, ledgers, `changes.md`, `schedule.md`, the check-in, and the `teamwork` and `reports` frontmatter keys.
Knowledge is the substrate; teamwork primitives are the events, schedule, and report built on it.
Retired keys (`events`, `consumes`) hard-error in `check` with their replacement rather than being silently reinterpreted.

**Why.** A closed vocabulary keeps the user-facing model aligned with the files and keys the runtime actually reads, instead of accumulating metaphors for the same artifacts.
## The forward schedule is injected, never derived

**Decision.** Every run workspace receives a generated `schedule.md`, rendered by the runtime with the same cron parser the supervisor schedules with: each active routine's next fires (routines below full participation as `fact:` lines -- fires to know about, not to report on), and, when the running routine is scheduled, its **window** -- now through its first fire on its next fire-day, same-day retry slots skipped -- with the other full-participation routines split in-window/out.
Trigger fires never appear (a wake-up cannot be computed ahead), and the running routine's own window remains operational self-knowledge regardless of its teamwork tier.
The standing instruction names the file and forbids deriving fire times from `routines/` frontmatter.
A routine without a schedule gets the facts without a window.

**Why.** Reporting routines must forecast -- "what happens before I report again" is half of any check-in -- and models cannot be trusted to compute it: in a 13-run, four-model probe matrix (product-pal's routine-schedule skill, this feature's proving ground), cron arithmetic and window membership were the dominant instruction-following failures, and became a 12/12 pass the moment the computation was handed over as text.
`changes.md` answers "what happened since I last reported"; `schedule.md` is its forward twin.
Baking it in also ends the double bookkeeping: the scheduler was already the sole authority on fire times, and now nothing else ever computes them.

## Every agent checks in

**Decision.** The template ships a check-in routine, active by default, scheduled daily -- 7am agent time, so the report is waiting when a person sits down.
It is the first reporting routine (`reports: true` -- the template's one teamwork declaration, since reporting defaults `teamwork` to `off`): it reads its injected `changes.md` plus its injected `schedule.md`, composes a plain check-in -- what I did, what I intend to do, where I need a human -- records it in its own ledger, prints it, and consumes the change set.
The ledger copy is the delivery that lasts, and the ledger is the one place on the knowledge branch that can hold a report safely: it travels with knowledge to every checkout, it is excluded from the change feed (a stored report can never re-enter `changes.md` and echo into the next report), and it is exempt from retention.
After an `openroutines sync`, `knowledge/ledgers/check-in.md` holds the latest check-in in any checkout, while printed output reaches a person only on the interactive path (a manual `routines run`) and in the exported session.
It declares no skills and no credentials; pointing it at Steady, Slack, or anywhere else is a frontmatter diff that adds a skill and a credential.

**Why.** An unattended agent needs a heartbeat a human can read -- on a human cadence when it has a real reporting destination, on demand from the ledger when it does not.
The schedule earns its keep even when nobody hears the scheduled copy: it is the feed's metronome, because the change feed accumulates from the cursor forward and reading the stored report consumes nothing, so without a scheduled consumption the pending set grows without bound and the eventual check-in covers months, not a day.
Once daily is the floor that keeps the loop live and the window human-sized while spending one model invocation a day on an agent nobody has pointed anywhere yet; a destination that wants a tighter cadence changes one cron line.
Shipping it default-on makes every ORA observable from day one with zero configuration and zero external dependencies, and makes the upgrade to a real reporting destination a two-line diff instead of a design exercise.
