// Package supervisor is the long-running scheduler: the container entrypoint.
//
// Every tick it re-reads routine frontmatter, reconciles memory with origin,
// and dispatches due routines serially through the shared run pipeline --
// implementing the durable two-phase run model: a logical run
// exists durably (committed, pushed) before it is allowed to act; failed
// attempts retry under the same run id with backoff; abandonment after a
// bounded number of attempts records a human-owned task and advances the watermark.
package supervisor

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/memory"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/runner"
	"github.com/steadyspacecorp/openroutines/internal/sandbox"
	"github.com/steadyspacecorp/openroutines/internal/schedule"
	"github.com/steadyspacecorp/openroutines/internal/scrub"
)

// Scheduling constants: the tick cadence and the per-run attempt cap.
const (
	TickInterval = time.Minute
	MaxAttempts  = 5
)

// Supervisor is the tick loop: it re-reads routines, mints and dispatches
// runs, and syncs memory with origin.
type Supervisor struct {
	Dir        string
	InstanceID string
	Log        *log.Logger

	mem           *memory.Memory
	noOrigin      bool
	loc           *time.Location
	retention     time.Duration
	lastTrim      time.Time
	leaseSHA      string        // CAS token: the lease blob we last wrote
	leaseTTL      time.Duration // how long a lease survives without a heartbeat
	leaseRenewed  time.Time     // wall clock of the last accepted heartbeat
	leaseWarned   bool          // dispatch pause already announced for the current lease problem
	syncBlocked   bool          // rewritten-history or conflict: stop adopting/pushing
	syncWarned    bool          // blocker already raised for the current sync problem
	unreachWarned bool
	commitWarned  bool              // intent commit failing: dispatch is halted, someone must look
	loadFailed    map[string]string // routine name -> the load failure already recorded
	secrets       map[string]string // the supervisor's own secrets, for redacting its output

	// Trigger bookkeeping that is deliberately not durable: last-poll times
	// (persisting them would dirty the memory worktree every tick) and
	// poll-error dedup (log on transition, not per tick).
	lastPolled map[string]time.Time
	pollFailed map[string]bool
}

// New builds a supervisor for the agent repository at dir.
func New(dir string) (*Supervisor, error) {
	agent, err := config.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("not an agent repository: %w", err)
	}
	if problems := agent.Problems(); len(problems) > 0 {
		return nil, fmt.Errorf("agent not configured (%s) -- run openroutines configure", problems[0])
	}
	loc, err := time.LoadLocation(agent.Timezone)
	if err != nil {
		return nil, err
	}
	retention, err := memory.ParseRetention(agent.Retention())
	if err != nil {
		return nil, err
	}
	secrets := supervisorSecrets(dir)
	mem := memory.At(dir)
	return &Supervisor{
		Dir:        dir,
		InstanceID: memory.InstanceID(),
		Log:        log.New(scrub.NewWriter(os.Stdout, secrets), "", log.LstdFlags|log.LUTC),
		mem:        mem,
		leaseTTL:   memory.LeaseTTL,
		loc:        loc,
		retention:  retention,
		noOrigin:   !mem.HasOrigin(),
		secrets:    secrets,
		lastPolled: map[string]time.Time{},
		pollFailed: map[string]bool{},
	}, nil
}

// supervisorSecrets collects the secret values the supervisor itself holds
// -- the master key and the deploy key -- so its own log lines and memory
// events can be redacted. (The model's output stream has its own scrubber,
// seeded with the run's credentials.) A git error quoting an origin URL or
// key material would otherwise be logged verbatim and committed to memory.
// The deploy key is multi-line, and logs are scrubbed line by line, so each
// substantial line registers as its own value.
func supervisorSecrets(dir string) map[string]string {
	out := map[string]string{}
	if key, err := creds.LoadKey(dir); err == nil {
		out["master_key"] = hex.EncodeToString(key)
	}
	deployKey := os.Getenv(memory.EnvDeployKey)
	if path := os.Getenv(memory.EnvDeployKeyFile); deployKey == "" && path != "" {
		if raw, err := os.ReadFile(path); err == nil {
			deployKey = string(raw)
		}
	}
	for i, line := range strings.Split(deployKey, "\n") {
		line = strings.TrimSpace(line)
		if len(line) >= 16 && !strings.HasPrefix(line, "-----") {
			out[fmt.Sprintf("deploy_key_%d", i)] = line
		}
	}
	return out
}

// registerScrub adds a trigger poll's bearer material to the supervisor's
// scrub set, under prefixed names so an agent credential name can never
// collide with the master or deploy key entries. Entries are overwritten by
// the next poll rather than removed: a bearer that outlives its poll (an
// oauth2_client token expires on the provider's clock) stays redactable.
func (s *Supervisor) registerScrub(values map[string]string) {
	for name, v := range values {
		s.secrets["trigger "+name] = v
	}
}

func (s *Supervisor) stateDir() string { return s.mem.StateDir() }

// Run is the supervise loop: startup, then one Tick per minute until ctx is
// cancelled, then shutdown (final commit and push, lease release).
func (s *Supervisor) Run(ctx context.Context) error {
	// The supervisor's environment may carry the master and deploy keys.
	// Non-dumpable closes the /proc/<pid>/environ and ptrace paths from
	// same-UID model processes -- set before any child ever exists.
	if err := sandbox.ProtectProcess(); err != nil {
		s.Log.Printf("WARNING: could not mark supervisor non-dumpable: %v", err)
	}
	if configured, err := memory.ConfigureDeployKey(); err != nil {
		return fmt.Errorf("deploy key: %w", err)
	} else if configured {
		s.Log.Printf("deploy key configured for memory sync")
	}
	if err := s.mem.Ensure(); err != nil {
		return err
	}
	if s.noOrigin {
		s.Log.Printf("WARNING: no git origin -- memory is not durable and the single-instance lease is disabled (local mode)")
	} else {
		if err := s.acquireLease(ctx); err != nil {
			return err
		}
		defer func() { s.mem.ReleaseLease(s.leaseSHA) }()
	}
	s.Log.Printf("supervising %s (instance %s, tick %s)", s.Dir, s.InstanceID, TickInterval)
	if err := s.verifySandbox(); err != nil {
		return err
	}

	ticker := time.NewTicker(TickInterval)
	defer ticker.Stop()
	for {
		s.Tick(ctx, time.Now())
		select {
		case <-ctx.Done():
			s.shutdown()
			return nil
		case <-ticker.C:
		}
	}
}

// Tick performs one scheduling pass at the given time. Exported so tests can
// drive the supervisor with synthetic clocks.
func (s *Supervisor) Tick(ctx context.Context, now time.Time) {
	now = now.In(s.loc)

	// Reconcile memory with origin, defensively; renew the single-instance
	// lease and pause dispatch entirely if we no longer hold it.
	if !s.noOrigin {
		s.syncOnce()
		// Heartbeat before the sync verdict, not after: an instance that is
		// blocked is still alive, and a lease that lapses while its holder
		// runs would invite a replacement to start writing beside it.
		if !s.renewLease() {
			return
		}
		if s.syncBlocked {
			// Rewritten history or a conflict needs a human. Dispatching
			// anyway would take external actions under identities that exist
			// only in this container -- lost on replacement, then re-run as
			// duplicates. Same rule as an unreachable origin: hold.
			return
		}
	}

	// Once a day, trim the record streams to the retention window. Git
	// history keeps everything; the working files stay lean.
	if now.Sub(s.lastTrim) >= 24*time.Hour {
		s.lastTrim = now
		if changed, err := s.mem.Trim(s.retention, now); err != nil {
			s.Log.Printf("retention trim: %v", err)
		} else if changed {
			if _, err := s.mem.Commit(fmt.Sprintf("Trim memory to retention window (%s)", s.retention)); err != nil {
				s.Log.Printf("retention trim commit: %v", err)
			}
			s.pushBestEffort()
			s.Log.Printf("memory: trimmed record streams to the %s retention window", s.retention)
		}
	}

	routines, parseErrs := routine.LoadAgent(s.Dir)
	s.reportLoadFailures(parseErrs, now)

	// Phase 1: reconcile scheduling state; collect runnable pending runs.
	type dispatch struct {
		r  *routine.Routine
		st *schedule.State
	}
	var due []dispatch
	for _, r := range routines {
		if !r.FM.IsActive() || (r.FM.Schedule == "" && r.FM.Trigger == nil) {
			continue
		}
		var spec *schedule.Spec
		if r.FM.Schedule != "" {
			var err error
			spec, err = schedule.Parse(r.FM.Schedule, s.loc)
			if err != nil {
				s.Log.Printf("%s: bad schedule %q: %v", r.Name, r.FM.Schedule, err)
				continue
			}
		}
		if r.FM.Trigger != nil {
			if err := r.FM.Trigger.Validate(); err != nil {
				s.Log.Printf("%s: %v", r.Name, err)
				continue
			}
		}
		st, err := schedule.Load(s.stateDir(), r.Name)
		if err != nil {
			s.Log.Printf("%s: %v", r.Name, err)
			continue
		}
		if st == nil {
			// First sight of this routine: it owes nothing from before it existed.
			st = &schedule.State{Routine: r.Name, Watermark: now}
			if err := st.Save(s.stateDir()); err != nil {
				s.Log.Printf("%s: %v", r.Name, err)
				continue
			}
			s.Log.Printf("%s: registered (watermark %s)", r.Name, now.Format(time.RFC3339))
			continue
		}
		if st.Pending == nil {
			if st.CoolingDown(now) {
				continue // circuit breaker: no new runs until the cool-down ends
			}
			minted := false
			if spec != nil {
				first, last, n := schedule.Occurrences(spec, st.Watermark, now)
				if n > 0 {
					st.Pending = &schedule.Pending{
						RunID:          schedule.NewRunID(),
						ScheduledFor:   first,
						CoveredThrough: last,
						CreatedAt:      now,
					}
					minted = true
					if n > 1 {
						s.Log.Printf("%s: %d missed firings collapse into run %s", r.Name, n, st.Pending.RunID)
					}
					// The scheduled run will pull whatever the trigger would
					// have announced; refresh the baseline so the same news
					// doesn't double-fire right after it.
					if r.FM.Trigger != nil {
						s.refreshTriggerBaseline(r, now)
					}
				}
			}
			if !minted && r.FM.Trigger != nil {
				if s.evaluateTrigger(r, now) {
					st.Pending = &schedule.Pending{
						RunID:          schedule.NewRunID(),
						ScheduledFor:   now,
						CoveredThrough: now,
						CreatedAt:      now,
					}
					minted = true
					s.Log.Printf("%s: trigger fired -- run %s", r.Name, st.Pending.RunID)
				}
			}
			if !minted {
				continue
			}
			if err := st.Save(s.stateDir()); err != nil {
				s.Log.Printf("%s: %v", r.Name, err)
				continue
			}
		}
		if st.Pending.Attempts >= MaxAttempts {
			// Every attempt in the budget was started and none of them
			// settled: the supervisor did not survive them. Settlement is
			// where a run is normally abandoned, but a run that kills its
			// container never gets there.
			s.abandon(r.Name, st, fmt.Sprintf("%d attempts started, none settled -- the supervisor did not survive them", st.Pending.Attempts), now)
			if err := st.Save(s.stateDir()); err != nil {
				s.Log.Printf("%s: %v", r.Name, err)
			}
			continue
		}
		if now.Before(schedule.NextRetryAt(st.Pending)) {
			continue // backing off after a failed attempt
		}
		due = append(due, dispatch{r, st})
	}

	// This tick's own bookkeeping -- minted pending records, refreshed trigger
	// baselines, abandonments -- has to be durable before anything acts on it.
	if !s.commitIntent("Record scheduling state") {
		return
	}

	// Phase 2: execute serially, in due order. Each attempt reserves itself
	// just before it starts (see execute), so a lost container costs a retry
	// only for the run that was actually running.
	sort.Slice(due, func(i, j int) bool {
		return due[i].st.Pending.ScheduledFor.Before(due[j].st.Pending.ScheduledFor)
	})
	for _, d := range due {
		if ctx.Err() != nil {
			return // shutting down: stop launching, nothing is reserved yet
		}
		// Heartbeat before every run, not once per tick: a tick runs every due
		// routine to completion, so its wall time is unbounded and a per-tick
		// heartbeat would leave the lease stale for as long as the work takes.
		// Renewing here bounds staleness by one run -- which is why the TTL
		// only has to outlast the longest supported timeout. Losing the lease
		// means another instance is live: stop dispatching, like a blocked
		// sync, rather than act as a second writer.
		if !s.noOrigin && !s.leaseFresh() && !s.renewLease() {
			return
		}
		s.execute(ctx, d.r, d.st, now)
	}
}

// commitIntent makes the memory worktree durable before anything acts on it,
// and reports whether it got there. Persist-before-act rests on the data, not
// on control flow: a tick that wrote state and then failed to commit it leaves
// the record on disk and nowhere else, and no later tick would mint anything
// to notice. So whatever the worktree carries is the intent -- Commit no-ops
// on a clean tree, and the normal path costs nothing.
func (s *Supervisor) commitIntent(message string) bool {
	sha, err := s.mem.Commit(message)
	if err != nil {
		// Dispatch halts until this clears, and only a person can clear it:
		// a supervisor that cannot record what it is about to do must not do it.
		s.blockOnce("commit", "intent commit failed -- runs held: "+err.Error(), &s.commitWarned)
		return false
	}
	s.recover("commit", "intent commit recovered -- runs resumed", &s.commitWarned)
	if s.noOrigin || s.syncBlocked || sha == "" {
		return true
	}
	if err := s.mem.Push(); err != nil {
		// An identity that isn't durable is how duplicates happen: without
		// a pushed intent, no new logical run starts.
		s.blockOnce("push", "intent push failed -- runs held until origin is reachable: "+err.Error(), &s.unreachWarned)
		return false
	}
	s.recover("push", "push to origin recovered -- runs resumed", &s.unreachWarned)
	return true
}

// reserve claims the attempt a routine is about to run. Returns the give-back
// for an attempt that never becomes a run: a shutdown, a failed intent commit.
func reserve(p *schedule.Pending, now time.Time) (giveBack func()) {
	prior := p.LastAttemptAt
	p.Attempts++
	p.LastAttemptAt = now
	return func() {
		p.Attempts--
		p.LastAttemptAt = prior
	}
}

// abandon gives up on a pending run: the work becomes a human-owned task (a
// run that falls over never gets to explain itself, and only a person can
// act), the watermark advances so the schedule moves on, and the breaker
// counts the abandonment. The caller saves the state and commits it.
func (s *Supervisor) abandon(name string, st *schedule.State, detail string, now time.Time) {
	p := st.Pending
	date := now.UTC().Format("2006-01-02")
	_ = s.mem.AppendHumanTask("task-"+p.RunID,
		fmt.Sprintf("Investigate routine %s: run %s abandoned after %d attempts (last failure: %s) -- watermark advanced, this work will not retry on its own (source: supervisor; added %s)", name, p.RunID, p.Attempts, detail, date))
	st.Watermark = p.CoveredThrough
	st.Pending = nil
	if cooldown := st.RecordAbandonment(now); cooldown > 0 {
		_ = s.mem.AppendEvent(fmt.Sprintf("%s supervisor: routine %s circuit breaker tripped after %d consecutive abandonments -- cooling down for %s, next success resets", date, name, st.ConsecutiveAbandons, cooldown))
		s.Log.Printf("%s: circuit breaker tripped -- cooling down for %s", name, cooldown)
	}
	s.Log.Printf("%s: %s abandoned after %d attempts", name, p.RunID, p.Attempts)
}

// execute runs one attempt of a pending logical run and settles the outcome.
func (s *Supervisor) execute(ctx context.Context, r *routine.Routine, st *schedule.State, now time.Time) {
	// The per-routine kernel lock covers the whole attempt, snapshot through
	// settlement. Held means a manual `routines run` is mid-flight in this
	// checkout: skip and log, per design decision "Overlap" -- the pending run
	// retries on a later tick.
	release, lockErr := runner.LockRoutine(s.Dir, r.Name)
	if lockErr != nil {
		if errors.Is(lockErr, runner.ErrRoutineLocked) {
			s.Log.Printf("%s: attempt already in flight elsewhere (lock held) -- skipping this tick", r.Name)
		} else {
			s.Log.Printf("%s: routine lock: %v", r.Name, lockErr)
		}
		return
	}
	defer release()

	agent, err := config.Load(s.Dir)
	if err != nil {
		s.Log.Printf("%s: %v", r.Name, err)
		return
	}

	// Reserve the attempt before spawning anything, and make the reservation
	// durable in its own right: no model process starts unless the attempt
	// that spawned it is committed and pushed. A container lost mid-attempt is
	// replaced by one that reads this record, so the budget drains as it
	// should instead of retrying forever at attempts: 0.
	p := st.Pending
	giveBack := reserve(p, now)
	if err := st.Save(s.stateDir()); err != nil {
		s.Log.Printf("%s: %v", r.Name, err)
		return
	}
	if !s.commitIntent(fmt.Sprintf("Reserve %s attempt %d (%s)", r.Name, p.Attempts, p.RunID)) {
		giveBack()
		if err := st.Save(s.stateDir()); err != nil {
			s.Log.Printf("%s: %v", r.Name, err)
		}
		return
	}

	meta := runner.Meta{
		RunID:          p.RunID,
		AttemptID:      fmt.Sprintf("attempt_%02d", p.Attempts),
		ScheduledFor:   p.ScheduledFor.Format(time.RFC3339),
		CoveredThrough: p.CoveredThrough.Format(time.RFC3339),
	}
	s.Log.Printf("%s: %s %s starting (scheduled %s)", r.Name, p.RunID, meta.AttemptID, meta.ScheduledFor)

	res, staging, err := runner.Execute(ctx, s.Dir, agent, r, meta)
	detail := ""
	if err != nil {
		s.Log.Printf("%s: %s failed to start: %v", r.Name, p.RunID, err)
		res = &runner.ExecResult{Outcome: runner.Crashed, ExitCode: -1}
		// Start-failure text quotes raw errors (git, provider); memory events
		// are committed and pushed, so redact before recording.
		detail = scrub.Redact(err.Error(), s.secrets)
	} else {
		defer staging.Cleanup()
	}

	// The settlement commit carries this attempt's scheduling consequences:
	// success clears pending and advances the watermark; the final failed
	// attempt abandons the run -- a human-owned task (someone must act)
	// alongside the advanced watermark; shutdown returns the reserved attempt
	// so an interrupted attempt doesn't count toward abandonment and the same
	// logical run retries on next boot.
	abandoned := false
	settlement, serr := runner.Settle(s.Dir, r, staging, res, meta, detail, func(fin *runner.Settlement) {
		switch {
		case fin.Outcome == runner.Canceled:
			giveBack()
		case fin.Outcome == runner.Completed:
			st.Watermark = p.CoveredThrough
			st.Pending = nil
			st.RecordSuccess()
		case p.Attempts >= MaxAttempts:
			abandoned = true
			s.abandon(r.Name, st, fin.Detail, now)
		}
		if err := st.Save(s.stateDir()); err != nil {
			s.Log.Printf("%s: %v", r.Name, err)
		}
	})
	if serr != nil {
		s.Log.Printf("%s: %s settle: %v", r.Name, p.RunID, serr)
	}
	if settlement.Discarded {
		s.Log.Printf("%s: discarded staged events.md change (events: false)", r.Name)
	}
	switch {
	case settlement.Outcome == runner.Canceled:
		s.Log.Printf("%s: %s interrupted by shutdown -- will retry on next boot", r.Name, p.RunID)
		return // no push: shutdown's final commit and push carry the record
	case settlement.Outcome == runner.Completed:
		s.Log.Printf("%s: %s completed in %s", r.Name, p.RunID, res.Duration)
	case abandoned:
		// abandon() already said so.
	default:
		s.Log.Printf("%s: %s attempt failed (%s) -- will retry", r.Name, p.RunID, settlement.Detail)
	}
	s.pushBestEffort()
}

func (s *Supervisor) syncOnce() {
	rep := s.mem.Sync()
	switch {
	case rep.Rewritten:
		s.syncBlocked = true
		s.blockOnce("sync", "memory branch history rewritten on origin -- sync stopped, running on local state: "+rep.Detail, &s.syncWarned)
	case rep.Conflict:
		s.syncBlocked = true
		s.blockOnce("sync", "memory sync conflict -- sync stopped, running on local state: "+rep.Detail, &s.syncWarned)
	case rep.Unreachable:
		s.Log.Printf("origin unreachable: %s", rep.Detail)
	default:
		s.syncBlocked = false
		s.recover("sync", "memory sync with origin recovered", &s.syncWarned)
		if rep.Adopted {
			s.Log.Printf("memory: adopted remote commits")
		}
	}
}

// reportLoadFailures records in memory that a routine has stopped loading --
// and, later, that it loads again. The tick schedules around a broken file
// (design decision "A broken routine is one broken routine"), so without this
// the only trace of a routine that quietly stopped running is a log line
// nobody is tailing. The transitions are events rather than human-owned tasks:
// a broken file heals by being edited and the next tick notices, so there is
// nothing for a person to close. Unattributed failures -- a directory that
// would not read -- are left out: they fail every attempt, and abandonment
// already files a task for each.
func (s *Supervisor) reportLoadFailures(errs []error, now time.Time) {
	failing := map[string]string{}
	for _, e := range errs {
		s.Log.Printf("routine load error: %v", e)
		var re *routine.Error
		if !errors.As(e, &re) {
			continue
		}
		// The path is absolute in the container; the event is read in the
		// repository, where the file is routines/<name>.md. And load errors
		// quote file contents, which memory events push.
		text := strings.TrimPrefix(e.Error(), s.Dir+string(filepath.Separator))
		failing[re.Name] = scrub.Redact(text, s.secrets)
	}

	var news []string
	for _, name := range slices.Sorted(maps.Keys(failing)) {
		if s.loadFailed[name] != failing[name] {
			news = append(news, fmt.Sprintf("routine %s does not load (%s) -- it will not run until the file is fixed", name, failing[name]))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(s.loadFailed)) {
		if _, still := failing[name]; !still {
			news = append(news, fmt.Sprintf("routine %s loads again", name))
		}
	}
	s.loadFailed = failing
	if len(news) == 0 {
		return
	}

	date := now.UTC().Format("2006-01-02")
	for _, line := range news {
		if err := s.mem.AppendEvent(fmt.Sprintf("%s supervisor: %s", date, line)); err != nil {
			s.Log.Printf("recording routine load status: %v", err)
			return
		}
		s.Log.Print(line)
	}
	if _, err := s.mem.Commit("Record routine load status"); err != nil {
		s.Log.Printf("routine load status commit failed: %v", err)
		return
	}
	s.pushBestEffort()
}

// blockOnce records a blocking condition when it first appears -- as a
// human-owned task only, because only a person can clear it and the task's
// creation is itself an observable change: a companion event would
// double-record the same fact. The task id is date-scoped so a supervisor
// restart doesn't re-record it -- AppendHumanTask skips ids already present.
func (s *Supervisor) blockOnce(kind, msg string, warned *bool) {
	// Sync/push failure text quotes raw git errors, which can carry a
	// tokened origin URL -- and the message below is committed to memory.
	msg = scrub.Redact(msg, s.secrets)
	s.Log.Printf("BLOCKED: %s", msg)
	if !*warned {
		*warned = true
		date := time.Now().UTC().Format("2006-01-02")
		_ = s.mem.AppendHumanTask("task-"+kind+"-"+time.Now().UTC().Format("20060102"),
			fmt.Sprintf("%s (source: supervisor; added %s)", msg, date))
		_, _ = s.mem.Commit("Record supervisor blocker")
		s.pushBestEffort()
	}
}

// recover clears a blocker whose condition has healed: any open task-<kind>-*
// the supervisor previously recorded is completed in place -- the transition
// is the record, no companion event. It runs on every healthy tick and
// matches by id prefix, so it also heals blockers raised before a restart --
// a blocker that outlives its outage is noise a person has to chase.
func (s *Supervisor) recover(kind, msg string, warned *bool) {
	*warned = false
	changed, err := s.mem.ResolveHumanTasks("task-"+kind+"-",
		"done "+time.Now().UTC().Format("2006-01-02")+" -- "+msg)
	if err != nil || !changed {
		return
	}
	s.Log.Printf("RECOVERED: %s", msg)
	_, _ = s.mem.Commit("Resolve supervisor blocker")
	s.pushBestEffort()
}

func (s *Supervisor) pushBestEffort() {
	if s.noOrigin || s.syncBlocked {
		return
	}
	if err := s.mem.Push(); err != nil {
		s.Log.Printf("memory push failed (will retry): %v", err)
	}
}

// verifySandbox enforces the fail-closed policy at boot, not mid-run. Only
// production (inside the agent image) spawns model processes natively behind
// the Landlock shim; everywhere else they run in the per-run container, or
// the operator has explicitly opted into unconfined dev mode.
func (s *Supervisor) verifySandbox() error {
	switch {
	case os.Getenv("OPENROUTINES_IN_CONTAINER") == "1":
		self, err := os.Executable()
		if err != nil {
			return err
		}
		// Constructed, like every other child: an inherited environment
		// would republish the supervisor's keys in the probe's own
		// /proc/<pid>/environ. TMPDIR is the scratch scope it confines.
		probe := exec.Command(self, "sandbox-probe")
		probe.Env = []string{"TMPDIR=" + os.Getenv("TMPDIR")}
		out, probeErr := probe.Output()
		if probeErr == nil {
			s.Log.Printf("filesystem sandbox: %s active for model processes", strings.TrimSpace(string(out)))
			return nil
		}
		if os.Getenv(sandbox.EnvUnsafeOverride) == "1" {
			s.Log.Printf("WARNING: filesystem sandbox DISABLED by %s -- model processes run unconfined", sandbox.EnvUnsafeOverride)
			return nil
		}
		return fmt.Errorf("filesystem sandbox required but unavailable (host kernel without Landlock?) -- refusing to supervise; set %s=1 to run unconfined deliberately", sandbox.EnvUnsafeOverride)
	case os.Getenv("OPENROUTINES_NATIVE") == "1":
		s.Log.Printf("WARNING: OPENROUTINES_NATIVE=1 -- model processes run unconfined (dev mode)")
	default:
		s.Log.Printf("model processes run in the per-run container")
	}
	return nil
}

func (s *Supervisor) shutdown() {
	s.Log.Printf("shutting down: final memory sync")
	if _, err := s.mem.Commit("Shutdown"); err != nil {
		s.Log.Printf("shutdown commit: %v", err)
	}
	s.pushBestEffort()
}

// acquireLease enforces "exactly one instance": a fresh foreign lease means
// another supervisor is alive (rolling deploy overlap, accidental replica),
// so this one waits rather than corrupting memory. The write is atomic
// (compare-and-swap on the lease ref): two instances racing cannot both win.
func (s *Supervisor) acquireLease(ctx context.Context) error {
	for {
		lease, err := s.mem.ReadLease()
		if err != nil {
			return fmt.Errorf("lease: %w", err)
		}
		expected := ""
		eligible := lease == nil
		if lease != nil {
			expected = lease.SHA
			eligible = lease.Holder == s.InstanceID || time.Since(lease.At) > s.leaseTTL
		}
		if eligible {
			now := time.Now()
			sha, werr := s.mem.WriteLease(s.InstanceID, now, expected)
			if werr == nil {
				s.holdLease(sha, now)
				return nil
			}
			// CAS lost: someone else moved first. Loop and re-evaluate.
			s.Log.Printf("lease race lost -- re-evaluating")
			continue
		}
		s.Log.Printf("another instance holds the lease (%s, heartbeat %s) -- waiting", lease.Holder, lease.At.Format(time.RFC3339))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
		}
	}
}

// renewLease heartbeats atomically against the lease we last wrote. Returns
// false -- pause all dispatch -- when another live instance holds it. The
// heartbeat carries wall-clock time, not the tick's time: liveness is about
// this process still breathing, and staleness is judged against a real clock.
func (s *Supervisor) renewLease() bool {
	now := time.Now()
	if sha, err := s.mem.WriteLease(s.InstanceID, now, s.leaseSHA); err == nil {
		s.holdLease(sha, now)
		return true
	}
	lease, err := s.mem.ReadLease()
	if err == nil && lease != nil && lease.Holder != s.InstanceID && time.Since(lease.At) <= s.leaseTTL {
		return s.leaseLost(fmt.Sprintf("lease held by %s (last heartbeat %s ago, expires in %s) -- pausing dispatch",
			lease.Holder, time.Since(lease.At).Round(time.Second), (s.leaseTTL - time.Since(lease.At)).Round(time.Second)))
	}
	expected := ""
	if lease != nil {
		expected = lease.SHA
	}
	if sha, werr := s.mem.WriteLease(s.InstanceID, now, expected); werr == nil {
		s.holdLease(sha, now)
		return true
	}
	return s.leaseLost("lease renewal failed -- pausing dispatch until origin accepts a heartbeat")
}

// leaseFresh reports whether the last heartbeat is recent enough that another
// one before the next run would be redundant. What this permits stays inside
// the TTL by construction: a run may begin with a lease up to a quarter of the
// TTL old and last at most memory.MaxRunTimeout (half the TTL), leaving a
// quarter to spare.
func (s *Supervisor) leaseFresh() bool {
	return time.Since(s.leaseRenewed) < s.leaseTTL/4
}

// holdLease records a successful heartbeat, announcing the recovery when the
// previous one failed.
func (s *Supervisor) holdLease(sha string, at time.Time) {
	if s.leaseWarned {
		s.leaseWarned = false
		s.Log.Printf("lease heartbeat recovered -- dispatch resumed")
	}
	s.leaseSHA = sha
	s.leaseRenewed = at
}

// leaseLost pauses dispatch and says why the first time. A rolling deploy's
// overlap persists for many ticks; one line each would bury the transition
// that matters. Unlike blockOnce this records nothing in memory -- an
// instance that cannot prove it is the writer must not write.
func (s *Supervisor) leaseLost(msg string) bool {
	if !s.leaseWarned {
		s.leaseWarned = true
		s.Log.Printf("BLOCKED: %s", msg)
	}
	return false
}
