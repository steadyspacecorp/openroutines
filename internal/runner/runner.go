// Package runner executes one routine attempt: the per-run pipeline shared by
// `openroutines routines run` and the supervisor. Stage assembles a disposable
// workspace and a clean environment, Run spawns headless opencode, Settle
// imports or discards staged knowledge (design decision "Appendix: one run,
// end to end").
package runner

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/knowledge"
	"github.com/steadyspacecorp/openroutines/internal/lock"
	"github.com/steadyspacecorp/openroutines/internal/logging"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/run"
	"github.com/steadyspacecorp/openroutines/internal/sandbox"
	"github.com/steadyspacecorp/openroutines/internal/scrub"
	"github.com/steadyspacecorp/openroutines/internal/skill"
)

// Outcome classifies how an attempt ended.
type Outcome string

// The terminal outcomes an attempt reports.
const (
	Completed Outcome = "completed"
	Timeout   Outcome = "timeout"
	Crashed   Outcome = "crashed"
	Canceled  Outcome = "canceled" // shutdown interrupted the attempt
)

// Meta identifies the attempt: run id (stable across retries), attempt id,
// and the schedule window for supervisor-dispatched runs.
type Meta struct {
	RunID          string
	AttemptID      string
	ScheduledFor   string // RFC3339, empty for manual runs
	CoveredThrough string // RFC3339, empty for manual runs
	AttemptUID     uint32 // production-only identity, from the supervisor's pool or the manual-run reservation
	Rehearsal      string // fixture path; set only for manual rehearsal runs
}

// ExecResult is one attempt's outcome. Hint, when set, classifies a common
// failure (currently: provider authentication) so it surfaces as a
// configuration problem instead of an opaque crash.
type ExecResult struct {
	Outcome     Outcome
	ExitCode    int
	Duration    time.Duration
	Hint        string
	Model       string // the resolved model this attempt ran with
	Effort      string // frontmatter reasoning effort, when set
	Usage       *Usage // token consumption; nil when the surface was unavailable
	SessionsDir string // the attempt's exported sessions, "" when no session dir is designated
}

// authFailurePattern matches provider authentication errors in the session
// record's failure text, so a bad key reads as configuration instead of an
// opaque crash. The `error:` forms cover bare status text passed through
// verbatim ("... ended on an error: Unauthorized").
var authFailurePattern = regexp.MustCompile(`(?i)invalid x-api-key|api key is invalid|invalid api key|incorrect api key|401 unauthorized|authentication_error|missing.{0,20}api key|error:\s*unauthorized|invalid bearer token`)

// authHint adds what the provider's own message does not say: the resolved
// provider, the declared endpoint, and whether a credential was injected.
func authHint(dir, model string, injected bool) string {
	provider := strings.SplitN(model, "/", 2)[0]
	keyName := creds.ProviderKeyName(provider)
	endpoint := provider
	if oc, err := config.LoadOpenCode(dir); err == nil {
		if u := oc.ProviderBaseURL(provider); u != "" {
			endpoint = provider + " at " + u
		}
	}
	if injected {
		return fmt.Sprintf("provider authentication failed -- %s rejected the run's %s credential (openroutines credentials set %s)", endpoint, keyName, keyName)
	}
	return fmt.Sprintf("provider authentication failed -- %s rejected the request and no %s credential is stored (openroutines credentials set %s)", endpoint, keyName, keyName)
}

// ErrFatal marks a start failure no retry can fix; a caller spending a retry
// budget should give up now. The runner classifies because it assembled the
// run; the supervisor only asks.
var ErrFatal = errors.New("not retryable")

// ErrAttemptCleanup marks a workspace that was not proven discarded. The
// supervisor must poison the attempt identity instead of returning it to the
// pool, or its next assignee could read the leftover tree.
var ErrAttemptCleanup = errors.New("attempt workspace cleanup failed")

// Staging is the attempt's staged knowledge, awaiting import or discard.
type Staging struct {
	KnowledgeDir string
	// BaseDir is the pristine snapshot the run started from, outside the
	// run's reach; the import diffs staged knowledge against it so
	// concurrent settlements compose.
	BaseDir   string
	workspace string
	// attemptUID is set when the workspace was prepared for an attempt
	// identity: Cleanup may then need that identity's help to reclaim
	// paths the model process chmodded away from the group.
	attemptUID uint32

	// ConsumerThrough is the knowledge commit the delivery change set was
	// prepared against -- set only for reporting routines, fixed before the
	// run starts.
	ConsumerThrough string
	// ConsumerFirstRun is true when no durable cursor existed at preparation.
	// A successful empty bootstrap establishes that cursor without asking the
	// routine to claim it delivered anything.
	ConsumerFirstRun bool
}

// Cleanup discards the whole run workspace, staging and base included. A
// model process can chmod its own files away from the group, so removal may
// need the attempt identity's own help (see removeAttemptTree).
func (s *Staging) Cleanup() error {
	if s.attemptUID != 0 {
		// Kill anything still carrying the identity first: an escaped
		// descendant could otherwise race the removal.
		if err := sandbox.ReapIdentity(s.attemptUID); err != nil {
			return fmt.Errorf("%w: reap uid %d before removal: %w", ErrAttemptCleanup, s.attemptUID, err)
		}
	}
	if err := removeAttemptTree(s.attemptUID, s.workspace); err != nil {
		return fmt.Errorf("%w: remove %s: %w", ErrAttemptCleanup, s.workspace, err)
	}
	if s.BaseDir != "" {
		if err := os.RemoveAll(s.BaseDir); err != nil {
			slog.Warn("could not remove the attempt's knowledge base snapshot", "path", s.BaseDir, "error", err)
		}
	}
	return nil
}

// Consumed reports whether the routine created the consume marker. The staged
// knowledge directory is canonical (the one sandbox-writable workspace path);
// the workspace root is still accepted for unsandboxed runs.
func (s *Staging) Consumed() bool {
	if _, err := os.Stat(filepath.Join(s.KnowledgeDir, knowledge.ConsumeMarker)); err == nil {
		return true
	}
	_, err := os.Stat(filepath.Join(s.workspace, knowledge.ConsumeMarker))
	return err == nil
}

// Result is a completed manual run (routines run).
type Result struct {
	RunID       string
	Outcome     Outcome
	ExitCode    int
	Duration    time.Duration
	Commit      string               // knowledge commit hash, when one was made
	Hint        string               // classified failure cause, when one was recognized
	SessionsDir string               // the run's exported sessions, "" when no session dir is designated
	Conflicted  []knowledge.Conflict // semantic edits preserved outside the canonical file
}

// EffectiveModel resolves frontmatter over agent defaults.
func EffectiveModel(agent *config.Agent, r *routine.Routine) (string, error) {
	model := r.FM.Model
	if model == "" {
		model = agent.Defaults.Model
	}
	if model == "" || strings.Contains(model, "{{") {
		return "", fmt.Errorf("no model: set model in frontmatter or defaults.model in openroutines.yml (openroutines configure)")
	}
	return model, nil
}

// EffectiveTimeout is the declared timeout capped by the agent's max_timeout
// ceiling -- applied here, not in `check`: a spend guard cannot rest on a
// command the operator may never run.
func EffectiveTimeout(agent *config.Agent, r *routine.Routine) time.Duration {
	return min(DeclaredTimeout(agent, r), agent.MaxRunTimeout())
}

// DeclaredTimeout resolves frontmatter over agent defaults over 5m, before the
// ceiling applies. `check` reports on it; execution uses EffectiveTimeout.
func DeclaredTimeout(agent *config.Agent, r *routine.Routine) time.Duration {
	timeout, _ := declaredTimeout(agent, r)
	return timeout
}

// declaredTimeout also reports the raw value that failed to parse, "" when
// every declared value parsed clean, so Stage can warn about what it dropped.
func declaredTimeout(agent *config.Agent, r *routine.Routine) (timeout time.Duration, badValue string) {
	timeout = 5 * time.Minute
	for _, t := range []string{agent.Defaults.Timeout, r.FM.Timeout} {
		if t == "" {
			continue
		}
		if d, err := time.ParseDuration(t); err == nil {
			timeout = d
		} else {
			badValue = t
		}
	}
	return timeout, badValue
}

// StagedRun is a fully prepared attempt: everything that reads the knowledge
// worktree or supervisor-owned state happens in Stage, and Run touches
// neither -- which is what lets attempts execute in parallel while staging
// and settlement serialize behind the knowledge lock.
type StagedRun struct {
	dir       string
	r         *routine.Routine
	meta      Meta
	model     string
	timeout   time.Duration
	secrets   *runSecrets
	staging   *Staging
	workspace string
	runTmp    string
	env       []string
	ocArgs    []string

	// echo, when set, receives the run's scrubbed stdout live -- the manual
	// `routines run` terminal. The supervisor never sets it.
	echo io.Writer
}

// Discard releases a staged attempt that will not be spawned (for example,
// because its supervisor lost the lease after staging).
func (sr *StagedRun) Discard() error {
	sr.secrets.release()
	return sr.staging.Cleanup()
}

// attemptHomeName is the disposable per-attempt home inside the run
// workspace: sandbox hygiene in production, and what keeps session data
// readable after a local run's container exits.
const attemptHomeName = ".home"

// Stage prepares one attempt without spawning anything. mu is the caller's
// knowledge lock, held only around the worktree reads -- credential
// resolution can spend seconds on the network. On error, everything Stage
// acquired is already released.
func Stage(dir string, agent *config.Agent, r *routine.Routine, meta Meta, mu sync.Locker) (stagedRun *StagedRun, err error) {
	if os.Getenv("OPENROUTINES_IN_CONTAINER") == "1" && meta.AttemptUID == 0 {
		return nil, fmt.Errorf("%w: production runs require a reserved attempt uid", ErrFatal)
	}
	model, err := EffectiveModel(agent, r)
	if err != nil {
		return nil, err
	}
	declared, badTimeout := declaredTimeout(agent, r)
	if badTimeout != "" {
		r.Log().Warn("unparseable timeout ignored -- falling back", "run_id", meta.RunID, "value", badTimeout, "using", declared)
	}
	timeout := EffectiveTimeout(agent, r)
	if timeout != declared {
		r.Log().Warn("declared timeout capped by max_timeout", "run_id", meta.RunID, "declared", declared, "effective", timeout)
	}
	// Parsed from the agent repository, not the workspace copy made later:
	// MCP permission rules must not depend on pipeline ordering.
	oc, err := config.LoadOpenCode(dir)
	if err != nil {
		return nil, err
	}

	secrets, err := resolveCredentials(dir, agent, r, model)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			secrets.release()
		}
	}()
	mem := knowledge.At(dir)

	workspace, err := os.MkdirTemp("", "openroutines-run-*")
	if err != nil {
		return nil, err
	}
	staging := &Staging{KnowledgeDir: filepath.Join(workspace, knowledge.Dir), workspace: workspace}
	defer func() {
		if !ok {
			err = errors.Join(err, staging.Cleanup())
		}
	}()
	if staging.BaseDir, err = os.MkdirTemp("", "openroutines-base-*"); err != nil {
		return nil, err
	}

	if err := buildWorkspace(dir, workspace, r.Name); err != nil {
		return nil, err
	}
	if err := copyDeclaredSkills(dir, workspace, r.FM.Skills); err != nil {
		return nil, err
	}
	if err := applyDeclaredMCP(workspace, r.FM.MCP); err != nil {
		return nil, err
	}
	// Under the knowledge lock: one worktree read becomes both the run's
	// working copy and the import's pristine base, never a
	// settlement-in-progress halfway through writing.
	if err := func() error {
		mu.Lock()
		defer mu.Unlock()
		if err := mem.Ensure(); err != nil {
			return err
		}
		if err := mem.Snapshot(staging.BaseDir); err != nil {
			return err
		}
		if err := knowledge.CloneTree(staging.BaseDir, staging.KnowledgeDir); err != nil {
			return err
		}
		if r.FM.Reports {
			through, firstRun, err := prepareChanges(dir, workspace, r.Name)
			if err != nil {
				return fmt.Errorf("delivery changes: %w", err)
			}
			staging.ConsumerThrough = through
			staging.ConsumerFirstRun = firstRun
		}
		if err := prepareSchedule(dir, workspace, r, agent.Timezone, time.Now()); err != nil {
			return fmt.Errorf("forward schedule: %w", err)
		}
		if meta.Rehearsal != "" {
			fixture, err := os.ReadFile(meta.Rehearsal)
			if err != nil {
				return fmt.Errorf("rehearsal fixture: %w", err)
			}
			if err := os.WriteFile(filepath.Join(workspace, RehearsalFileName), fixture, 0o444); err != nil {
				return fmt.Errorf("rehearsal fixture: %w", err)
			}
		}
		return nil
	}(); err != nil {
		return nil, err
	}
	if err := writeAgentDefinition(workspace, agent, r, oc.MCPServers(), meta); err != nil {
		return nil, err
	}
	runTmp := filepath.Join(workspace, ".runtmp")
	if err := os.MkdirAll(runTmp, 0o755); err != nil {
		return nil, err
	}
	attemptHome := filepath.Join(workspace, attemptHomeName)
	if meta.AttemptUID != 0 && os.Getenv("OPENROUTINES_IN_CONTAINER") == "1" {
		// An attempt identity's gid equals its uid (template Dockerfile).
		if err := prepareWorkspaceAccess(meta.AttemptUID, workspace); err != nil {
			return nil, fmt.Errorf("preparing read-only attempt workspace: %w", err)
		}
		if err := prepareAttemptTrees(meta.AttemptUID, staging.KnowledgeDir, runTmp, attemptHome); err != nil {
			return nil, fmt.Errorf("preparing attempt uid %d trees: %w", meta.AttemptUID, err)
		}
		staging.attemptUID = meta.AttemptUID
	}

	// Clean environment: constructed, never inherited.
	env := frameworkEnv(agent.Timezone, r, meta)
	if meta.ScheduledFor != "" {
		env = append(env, "OPENROUTINES_SCHEDULED_FOR="+meta.ScheduledFor)
	}
	if r.FM.Websearch {
		// Registers the search backend; the permission rule in the generated
		// definition is the actual gate. Exa works keyless, and a granted
		// exa_api_key lands as EXA_API_KEY for keyed use.
		env = append(env, "OPENCODE_ENABLE_EXA=1")
	}
	if meta.CoveredThrough != "" {
		env = append(env, "OPENROUTINES_COVERED_THROUGH="+meta.CoveredThrough)
	}
	for _, k := range slices.Sorted(maps.Keys(secrets.env)) {
		env = append(env, k+"="+secrets.env[k])
	}
	// Non-secret variables from openroutines.yml are injected into every run.
	// On a name collision the credential wins; check flags it.
	for _, k := range slices.Sorted(maps.Keys(agent.Variables)) {
		if _, taken := secrets.env[strings.ToUpper(k)]; taken {
			continue
		}
		env = append(env, strings.ToUpper(k)+"="+agent.Variables[k])
	}

	// Identical across spawn paths. opencode's --log-level takes the same
	// four names slog renders.
	ocArgs := []string{
		"--print-logs", "--log-level=" + logging.Level.Level().String(),
		"run", "--agent", "routine", "-m", model,
	}
	if r.FM.Effort != "" {
		ocArgs = append(ocArgs, "--variant", r.FM.Effort)
	}
	ocArgs = append(ocArgs, r.Body)

	ok = true
	return &StagedRun{
		dir:       dir,
		r:         r,
		meta:      meta,
		model:     model,
		timeout:   timeout,
		secrets:   secrets,
		staging:   staging,
		workspace: workspace,
		runTmp:    runTmp,
		env:       env,
		ocArgs:    ocArgs,
	}, nil
}

func frameworkEnv(timezone string, r *routine.Routine, meta Meta) []string {
	return []string{
		"TZ=" + timezone,
		"OPENROUTINES_RUN_ID=" + meta.RunID,
		"OPENROUTINES_ATTEMPT_ID=" + meta.AttemptID,
		"OPENROUTINES_URL=" + r.FM.EffectiveURL(),
	}
}

// Run spawns the staged attempt's model process and waits it out. Derived
// credential material is revoked when the attempt ends, success or failure;
// a fresh attempt derives fresh material. On error the staging is already
// cleaned, and a cleanup failure is joined to the returned error.
func (sr *StagedRun) Run(ctx context.Context) (result *ExecResult, returnedStaging *Staging, err error) {
	r, meta, staging := sr.r, sr.meta, sr.staging
	dir := sr.dir
	ocArgs := sr.ocArgs
	model, timeout, secrets := sr.model, sr.timeout, sr.secrets
	defer secrets.release()
	ok := false
	defer func() {
		if !ok {
			err = errors.Join(err, staging.Cleanup())
		}
	}()

	// Unattended runs get JSON events; a manual run keeps opencode's default
	// rendering for the human watching it.
	if sr.echo == nil {
		ocArgs = append(slices.Clip(ocArgs), "--format", "json")
	}

	oc, err := sr.opencode()
	if err != nil {
		return nil, nil, err
	}
	cmd := oc.run(ocArgs)
	// stderr carries opencode's diagnostic log, passed through scrubbed with
	// the attempt's identity appended -- a failed attempt is never
	// invisible. It also carries rendered run progress unless JSON events
	// were asked for; run output must not masquerade as log lines.
	oclog := logging.NewPassthrough(slog.String("routine", r.Name), slog.String("run_id", meta.RunID))
	errOut := scrub.NewWriter(oclog)
	cmd.Stderr = errOut
	var out *scrub.Writer
	if sr.echo != nil {
		out = scrub.NewWriter(sr.echo)
		cmd.Stdout = out
	}
	cmd.WaitDelay = pipeDrainDeadline

	attemptLog := r.Log().With("run_id", meta.RunID)
	done := make(chan error, 1)
	kill := func() { oc.kill(cmd, done, attemptLog) }
	started := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	res := &ExecResult{Outcome: Completed}
	go func() { done <- cmd.Wait() }()
	select {
	case werr := <-done:
		// ErrWaitDelay means the process exited fine but something it left
		// behind still held the output pipe: the run's outcome is the
		// process's, not the orphan's -- only the tail of the log is lost.
		if errors.Is(werr, exec.ErrWaitDelay) {
			attemptLog.Warn("run output abandoned after the drain deadline -- a descendant outlived the attempt and the log tail is truncated", "deadline", pipeDrainDeadline)
		} else if werr != nil {
			res.Outcome = Crashed
			var ee *exec.ExitError
			if errors.As(werr, &ee) {
				res.ExitCode = ee.ExitCode()
			} else {
				res.ExitCode = -1
			}
		}
		// A detached descendant could outlive the attempt and keep writing
		// to staged knowledge while the pipeline imports it -- the attempt
		// ends with everything it spawned.
		oc.reap(cmd)
	case <-time.After(timeout):
		res.Outcome = Timeout
		kill()
	case <-ctx.Done():
		res.Outcome = Canceled
		kill()
	}
	res.Duration = time.Since(started).Round(time.Millisecond)
	if out != nil {
		out.Flush()
	}
	errOut.Flush()
	oclog.Flush()
	res.Model = model
	res.Effort = r.FM.Effort
	// opencode exits 0 even when its agent loop died mid-turn; the session
	// record decides whether the run actually finished.
	sessions, fetchErr := fetchSessions(oc.exec, attemptLog)
	capture := captureSessions(sessions, fetchErr, attemptLog)
	res.Usage = capture.Usage
	res.SessionsDir = exportSessions(meta, sessions, fetchErr, attemptLog)
	if res.Outcome == Completed && capture.Failure != "" {
		res.Outcome = Crashed
		res.Hint = capture.Failure
	}
	if res.Outcome == Crashed && authFailurePattern.MatchString(capture.Failure) {
		provider := strings.SplitN(model, "/", 2)[0]
		_, injected := secrets.env[strings.ToUpper(creds.ProviderKeyName(provider))]
		res.Hint = authHint(dir, model, injected)
	}
	ok = true
	return res, staging, nil
}

// RehearsalFileName is the fixture document injected into a rehearsal run's
// workspace.
const RehearsalFileName = "rehearsal.md"

const fixturePreamble = `REHEARSAL RUN, fixture world. The fixtures in ./rehearsal.md replace
every outside read for this run -- including ./changes.md and
./schedule.md wherever the fixtures provide stand-ins. You have no
credentials, no MCP servers, no skills, and no web access; do not
attempt external calls, the fixtures are the world. Nothing you produce
leaves the run: knowledge writes are discarded. Follow the routine
below exactly, against the fixtures.

`

// livePreamble governs a rehearsal with no fixtures: grants stay so reads
// work, the read-only restraint is asked of the model rather than enforced --
// the enforced part is that nothing settles.
const livePreamble = `REHEARSAL RUN, live world. Read anything this routine normally reads --
your credentials and tools are present -- but treat every external
action as read-only and idempotent: write nothing, post nothing, change
no state in any outside system. Anything the routine would deliver to a
destination, print here instead; printed output is this rehearsal's
delivery. Knowledge writes are discarded and nothing is consumed.
Follow the routine below exactly, under these restraints.

`

// Run executes routine `name` manually. skipKnowledge discards staged writes
// and the run record; rehearse runs against the fixture (grants stripped) or
// the live world (read-only by instruction), always discarding knowledge.
// In the production container a manual run reserves the manual attempt
// identity, so it can never share a uid with a supervisor slot.
func Run(dir, name string, skipKnowledge, rehearse bool, fixture string) (result *Result, err error) {
	meta := Meta{RunID: run.NewID(), AttemptID: "attempt_01", Rehearsal: fixture}
	if os.Getenv("OPENROUTINES_IN_CONTAINER") == "1" {
		uid, releaseIdentity, err := reserveManualIdentity(dir)
		if err != nil {
			return nil, err
		}
		defer releaseIdentity()
		meta.AttemptUID = uid
	}
	agent, err := config.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("not an agent repository: %w", err)
	}
	r, err := routine.Find(dir, name)
	if err != nil {
		return nil, err
	}
	if rehearse {
		rr := *r
		if fixture != "" {
			// Grants are stripped at the source so the existing pipeline
			// enforces the absence.
			rr.FM.Credentials = nil
			rr.FM.MCP = nil
			rr.FM.Skills = nil
			rr.FM.Webfetch = false
			rr.FM.Websearch = false
			rr.Body = fixturePreamble + r.Body
		} else {
			rr.Body = livePreamble + r.Body
		}
		r = &rr
		skipKnowledge = true
	}
	// One attempt per routine at a time, held snapshot through settlement --
	// the same lock the supervisor takes, so a manual run cannot double the
	// supervisor's own run of this routine.
	release, err := lock.Take(dir, name)
	if errors.Is(err, lock.ErrLocked) {
		return nil, fmt.Errorf("routine %s already has an attempt in flight (the supervisor or another terminal holds its lock) -- skipped", name)
	}
	if err != nil {
		return nil, err
	}
	defer release()
	// A supervisor may be settling runs into the same worktree beside this
	// process; snapshot and settlement take turns behind its lock.
	memLock, err := lock.Locker(dir, "knowledge")
	if err != nil {
		return nil, err
	}
	sr, err := Stage(dir, agent, r, meta, memLock)
	if err != nil {
		return nil, err
	}
	// Echo the run's scrubbed output to the terminal as it streams.
	sr.echo = os.Stdout
	exec, staging, err := sr.Run(context.Background())
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, staging.Cleanup()) }()

	res := &Result{RunID: meta.RunID, Outcome: exec.Outcome, ExitCode: exec.ExitCode, Duration: exec.Duration, Hint: exec.Hint, SessionsDir: exec.SessionsDir}
	if skipKnowledge {
		return res, nil
	}

	memLock.Lock()
	defer memLock.Unlock()
	settlement, err := Settle(dir, r, staging, exec, meta, "", nil)
	res.Outcome = settlement.Outcome
	res.Commit = settlement.Commit
	res.Conflicted = settlement.Conflicted
	return res, err
}

// Settlement is one attempt's settled, durable outcome.
type Settlement struct {
	Outcome   Outcome // downgraded to Crashed when staged knowledge was rejected
	Detail    string  // the failure description recorded; "" for clean completions
	Discarded bool    // staged events.md change discarded (teamwork: off)
	Commit    string  // settlement commit hash, "" when nothing changed
	// Conflicted names files a concurrently settled run also edited; the
	// staged competitor was quarantined for a person to resolve.
	Conflicted []knowledge.Conflict
}

// Settle makes one attempt's end durable in knowledge -- the single
// settlement path for manual and scheduled runs. A rejected import downgrades
// the outcome to Crashed. stage, when set, runs before the settlement commit
// so caller bookkeeping rides the same commit. detail overrides the derived
// failure description. A Canceled attempt gets only its run record and no
// commit of its own -- the same logical run retries.
func Settle(dir string, r *routine.Routine, staging *Staging, res *ExecResult, meta Meta, detail string, stage func(*Settlement)) (*Settlement, error) {
	mem := knowledge.At(dir)
	s := &Settlement{Outcome: res.Outcome, Detail: detail}
	if res.Outcome == Completed {
		discarded, conflicted, err := importKnowledge(dir, r, staging)
		if err != nil {
			s.Outcome = Crashed
			s.Detail = "knowledge rejected: " + err.Error()
		} else {
			s.Discarded = discarded
			s.Conflicted = conflicted
			advanceConsumer(dir, r, staging, meta.RunID)
		}
	} else if s.Detail == "" && res.Outcome != Canceled {
		s.Detail = fmt.Sprintf("%s after %s (exit %d)", res.Outcome, res.Duration, res.ExitCode)
		if res.Hint != "" {
			s.Detail += " -- " + res.Hint
		}
	}
	if s.Outcome != Completed && s.Outcome != Canceled {
		if err := mem.AppendEvent(fmt.Sprintf("%s supervisor: routine %s (%s %s) %s", datestamp(), r.Name, meta.RunID, meta.AttemptID, s.Detail)); err != nil {
			r.Log().Warn("could not record the failure event -- this log line is the only copy", "run_id", meta.RunID, "error", err)
		}
	}
	if stage != nil {
		stage(s)
	}
	rec := *res
	rec.Outcome = s.Outcome
	if err := mem.AppendRunRecord(recordJSON(r, meta, parseAttempt(meta.AttemptID), &rec, meta.ScheduledFor == "")); err != nil {
		return s, err
	}
	if s.Outcome == Canceled {
		return s, nil
	}
	commit, err := mem.Commit(fmt.Sprintf("Run %s (%s): %s", r.Name, meta.RunID, s.Outcome))
	if err != nil {
		return s, err
	}
	s.Commit = commit
	return s, nil
}

func parseAttempt(attemptID string) int {
	var n int
	_, _ = fmt.Sscanf(attemptID, "attempt_%d", &n)
	return n
}

// importKnowledge applies routine-level policy, then imports the staged tree:
// teamwork: off discards a staged events.md change, the rest imports
// normally. Reports whether such a change was discarded.
func importKnowledge(dir string, r *routine.Routine, staging *Staging) (discarded bool, conflicted []knowledge.Conflict, err error) {
	mem := knowledge.At(dir)
	if !r.FM.RecordsEvents() {
		if discarded, err = knowledge.RestoreFile(staging.KnowledgeDir, staging.BaseDir, "events.md"); err != nil {
			return false, nil, err
		}
	}
	conflicted, err = mem.Import(staging.KnowledgeDir, staging.BaseDir)
	return discarded, conflicted, err
}

// prepareChanges fixes the delivery boundary at the knowledge branch's
// current commit and renders the change set since the routine's cursor into
// the workspace. No cursor means first run: nothing to replay.
func prepareChanges(dir, workspace, consumer string) (string, bool, error) {
	mem := knowledge.At(dir)
	through, err := mem.Head()
	if err != nil {
		return "", false, err
	}
	cursor, err := mem.LoadCursor(consumer)
	if err != nil {
		return "", false, err
	}
	firstRun := cursor == nil
	from := ""
	var changes []knowledge.CommitChange
	if cursor != nil {
		from = cursor.ConsumedThrough
		if changes, err = mem.Changes(from, through); err != nil {
			if errors.Is(err, knowledge.ErrCursorUnreachable) {
				return "", false, fmt.Errorf("%w: %w -- repair or delete %s on the knowledge branch", ErrFatal, err, knowledge.CursorFile(consumer))
			}
			return "", false, err
		}
	}
	rendered := knowledge.RenderChanges(consumer, from, through, changes)
	return through, firstRun, os.WriteFile(filepath.Join(workspace, knowledge.ChangesFileName), []byte(rendered), 0o644)
}

// advanceConsumer moves a reporting routine's cursor after a successful
// import, before the completion commit, so consumption and results land
// together. Exception to the marker rule: a successful first run's change set
// is empty by construction, so completion establishes the starting cursor.
func advanceConsumer(dir string, r *routine.Routine, staging *Staging, runID string) {
	if !r.FM.Reports || staging.ConsumerThrough == "" || (!staging.ConsumerFirstRun && !staging.Consumed()) {
		return
	}
	if err := knowledge.At(dir).SaveCursor(r.Name, knowledge.Cursor{
		ConsumedThrough: staging.ConsumerThrough,
		ByRun:           runID,
		At:              time.Now().UTC(),
	}); err != nil {
		r.Log().Error("cursor not advanced -- this change set will be delivered again", "run_id", runID, "through", staging.ConsumerThrough, "error", err)
	}
}

// recordJSON formats one run record line for runs.jsonl. Usage fields are
// per attempt (spend happens per attempt; retries would double-count a
// run-level figure) and absent means the runtime didn't report, never zero.
func recordJSON(r *routine.Routine, meta Meta, attempt int, res *ExecResult, manual bool) string {
	record := run.Record{
		RunID: meta.RunID, Routine: r.Name, Attempt: attempt, Outcome: string(res.Outcome),
		RecordedAt: timestamp(), DurationMS: res.Duration.Milliseconds(), ExitCode: res.ExitCode,
		ScheduledFor: meta.ScheduledFor, CoveredThrough: meta.CoveredThrough, Manual: manual,
		Model: res.Model, Effort: res.Effort, Hint: res.Hint, Tokens: res.Usage,
	}
	if res.Usage != nil {
		record.CostReported = res.Usage.CostReported
	}
	return record.JSON()
}

// runSecrets is a run's resolved secret material: the environment to inject,
// and cleanup for derived credentials. Redaction registers where values
// materialize, not here.
type runSecrets struct {
	env     map[string]string
	cleanup []func()
}

func (s *runSecrets) setEnv(name, value string) error {
	if _, taken := s.env[name]; taken {
		return fmt.Errorf("credential grants set the %s environment variable twice", name)
	}
	s.env[name] = value
	return nil
}

// release disposes of derived material -- best-effort, once, at attempt end.
func (s *runSecrets) release() {
	for _, f := range s.cleanup {
		f()
	}
	s.cleanup = nil
}

// resolveCredentials builds the routine's secret set: declared credentials
// plus the provider key for its model. Raw credentials inject verbatim under
// their uppercase name; typed ones inject their derived surface -- the stored
// root secret never enters the run. A failed resolve releases whatever it
// already derived.
func resolveCredentials(dir string, agent *config.Agent, r *routine.Routine, model string) (_ *runSecrets, err error) {
	provider := strings.SplitN(model, "/", 2)[0]
	providerKey := creds.ProviderKeyName(provider)
	out := &runSecrets{env: map[string]string{}}
	defer func() {
		if err != nil {
			out.release()
		}
	}()

	key, keyErr := creds.LoadKey(dir)
	if keyErr != nil {
		if len(r.FM.Credentials) > 0 {
			return nil, fmt.Errorf("routine declares credentials but %w", keyErr)
		}
		// No store: opencode may still have its own auth for the provider.
		return out, nil
	}
	store, err := creds.Read(dir, key)
	if err != nil {
		return nil, err
	}
	for _, name := range r.FM.Credentials {
		v, present := store[name]
		if !present {
			return nil, fmt.Errorf("routine declares credential %q, not present in %s", name, creds.FileName)
		}
		spec, typed := agent.Credentials[name]
		if !typed {
			if err := out.setEnv(strings.ToUpper(name), v); err != nil {
				return nil, err
			}
			continue
		}
		derived, err := creds.Derive(name, spec, v)
		if err != nil {
			return nil, err
		}
		out.cleanup = append(out.cleanup, derived.Cleanup)
		for _, k := range slices.Sorted(maps.Keys(derived.Env)) {
			if err := out.setEnv(k, derived.Env[k]); err != nil {
				return nil, err
			}
		}
	}
	if v, present := store[providerKey]; present {
		if err := out.setEnv(strings.ToUpper(providerKey), v); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// buildWorkspace assembles the run workspace by allow-list: the configuration
// file, opencode.json, and routines/ -- everything else a run sees is staged
// deliberately by the pipeline. (A deny-list once missed exactly one entry,
// the encrypted credential store.) name is the routine being run, whose
// errors are the only ones that can fail assembly.
func buildWorkspace(dir, workspace, name string) error {
	for _, file := range []string{filepath.Base(config.Path(dir)), config.OpenCodeFileName} {
		raw, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := os.WriteFile(filepath.Join(workspace, file), raw, 0o644); err != nil {
			return err
		}
	}
	// An unparseable sibling is simply absent from the workspace; only an
	// error concerning this routine fails the attempt.
	routines, errs := routine.LoadAgent(dir)
	for _, err := range errs {
		if routine.Concerns(err, name) {
			return err
		}
	}
	for _, r := range routines {
		raw, err := os.ReadFile(r.Path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(workspace, "routines"), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(workspace, "routines", r.Name+".md"), raw, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// copyDeclaredSkills places exactly the routine's declared skills into the
// workspace at opencode's discovery path (.opencode/skills/). Undeclared
// skills are not merely permission-denied -- they are not present at all.
func copyDeclaredSkills(dir, workspace string, names []string) error {
	for _, name := range names {
		// Grammar before path use: a frontmatter name like "../../x" would
		// otherwise read outside skills/ and write outside the workspace.
		if !skill.NamePattern.MatchString(name) {
			return fmt.Errorf("declared skill %q is not a valid Agent Skills name", name)
		}
		found, err := skill.Find(dir, name)
		if err != nil {
			// Pass the real cause through -- a duplicate name is not "not
			// found".
			return fmt.Errorf("declared skill unavailable: %w", err)
		}
		src := found.Dir
		dest := filepath.Join(workspace, ".opencode", "skills", name)
		err = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(src, path)
			target := filepath.Join(dest, rel)
			if d.IsDir() {
				return os.MkdirAll(target, 0o755)
			}
			if !d.Type().IsRegular() {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			return os.WriteFile(target, raw, 0o755)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// applyDeclaredMCP rewrites the workspace's opencode.json so its mcp block
// holds only the declared servers. The deny rules already close ungranted
// surfaces; removing the entry keeps the run's opencode from contacting the
// server at all. Raw JSON values keep unrelated configuration byte-exact.
func applyDeclaredMCP(workspace string, granted []string) error {
	path := filepath.Join(workspace, config.OpenCodeFileName)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(raw, &cfg); err != nil {
		// Stage's LoadOpenCode already failed the attempt for this; on any
		// other path the file just travels as written.
		return nil
	}
	mcpRaw, ok := cfg["mcp"]
	if !ok {
		return nil
	}
	var mcp map[string]json.RawMessage
	if err := json.Unmarshal(mcpRaw, &mcp); err != nil || len(mcp) == 0 {
		return nil
	}
	filtered := map[string]json.RawMessage{}
	for _, name := range granted {
		if entry, ok := mcp[name]; ok {
			filtered[name] = entry
		}
	}
	if len(filtered) == len(mcp) {
		return nil
	}
	if len(filtered) == 0 {
		delete(cfg, "mcp")
	} else {
		filteredRaw, err := json.Marshal(filtered)
		if err != nil {
			return err
		}
		cfg["mcp"] = filteredRaw
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

// The standing instruction: editable prose compiled into the binary. The
// permission frontmatter stays code-generated because rule order is
// load-bearing.
//
//go:embed instruction.md
var instructionSrc string

var instructionTmpl = template.Must(template.New("instruction").Parse(instructionSrc))

type instructionData struct {
	AgentName     string
	Description   string
	RoutineName   string
	RunID         string
	RecordsEvents bool
	Reports       bool
	Changes       string
	Marker        string
	Variables     string // "$PRODUCT_REPO, $DOCS_URL" -- empty when none configured
}

// writeAgentDefinition places the generated opencode agent for this run at
// the harness's discovery path in the workspace.
func writeAgentDefinition(workspace string, agent *config.Agent, r *routine.Routine, servers []string, meta Meta) error {
	def, err := renderDefinition(agent, r, servers, meta)
	if err != nil {
		return err
	}
	dir := filepath.Join(workspace, ".opencode", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "routine.md"), []byte(def), 0o644)
}

// renderDefinition generates the run's opencode agent: default-deny skills,
// an explicit rule per configured MCP server (servers is passed in so rule
// generation can never silently see an empty config), and the standing
// instruction.
func renderDefinition(agent *config.Agent, r *routine.Routine, servers []string, meta Meta) (string, error) {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "description: Generated for routine %s -- derived from frontmatter, do not edit\n", r.Name)
	b.WriteString("mode: primary\n")
	b.WriteString("permission:\n")
	// Web access is a grant, not a default: opencode allows webfetch out of
	// the box, and fetched content is a prompt-injection vector.
	for _, w := range []struct {
		tool    string
		granted bool
	}{{"webfetch", r.FM.Webfetch}, {"websearch", r.FM.Websearch}} {
		action := "deny"
		if w.granted {
			action = "allow"
		}
		fmt.Fprintf(&b, "  %s: %s\n", w.tool, action)
	}
	// opencode registers MCP tools as <server>_<tool>, so one glob per
	// configured server closes or opens its whole surface.
	for _, server := range servers {
		action := "deny"
		if slices.Contains(r.FM.MCP, server) {
			action = "allow"
		}
		fmt.Fprintf(&b, "  %q: %s\n", server+"_*", action)
	}
	b.WriteString("  skill:\n")
	b.WriteString("    \"*\": deny\n") // order matters: last matching rule wins
	for _, s := range r.FM.Skills {
		fmt.Fprintf(&b, "    %q: allow\n", s)
	}
	b.WriteString("---\n\n")

	if err := instructionTmpl.Execute(&b, instructionData{
		AgentName:     agent.Name,
		Description:   strings.TrimSpace(agent.Description),
		RoutineName:   r.Name,
		RunID:         meta.RunID,
		RecordsEvents: r.FM.RecordsEvents(),
		Reports:       r.FM.Reports,
		Changes:       knowledge.ChangesFileName,
		Marker:        knowledge.ConsumeMarker,
		Variables:     variablesLine(agent),
	}); err != nil {
		return "", err
	}
	return b.String(), nil
}

// RenderDefinition generates a routine's agent definition exactly as a run
// would, without running anything -- check validates wiring with it offline.
func RenderDefinition(agent *config.Agent, r *routine.Routine, servers []string) (string, error) {
	return renderDefinition(agent, r, servers, Meta{RunID: "run_check", AttemptID: "attempt_00"})
}

// variablesLine renders the agent's variable names ("$PRODUCT_REPO,
// $DOCS_URL") for the standing instruction.
func variablesLine(agent *config.Agent) string {
	names := slices.Sorted(maps.Keys(agent.Variables))
	for i, n := range names {
		names[i] = "$" + strings.ToUpper(n)
	}
	return strings.Join(names, ", ")
}

// pipeDrainDeadline bounds how long waiting on the run's output pipes may
// outlast the process: a daemonized grandchild keeps the inherited pipe open
// forever, and the wait for EOF must not hold the tick loop.
const pipeDrainDeadline = 5 * time.Second

// containerExitGrace is how long `docker run` gets to notice that its
// container is gone before the client itself is killed.
const containerExitGrace = 5 * time.Second

// killClient ends a container run after `docker stop` was asked to take the
// container down: a client that does not follow it out is killed rather than
// waited on forever. Waiting for Wait to return is not optional -- the caller
// flushes the stream writers, and returning before Wait would race them.
func killClient(cmd *exec.Cmd, grace time.Duration, done chan error, log *slog.Logger) {
	select {
	case <-done:
	case <-time.After(grace):
		log.Warn("docker client did not exit after the container stopped -- killed", "grace", grace)
		_ = cmd.Process.Kill()
		<-done // bounded by WaitDelay now that the process is going away
	}
}

// signalTarget is the run's process group when it leads one. The guard
// matters: signaling -pid without Setpgid would reach the supervisor's own
// group.
func signalTarget(cmd *exec.Cmd) int {
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid {
		return -cmd.Process.Pid
	}
	return cmd.Process.Pid
}

// killGroup terminates the run's whole process group: SIGTERM, grace, SIGKILL.
// The waits are bounded by the command's WaitDelay, not by the group's
// willingness to exit.
func killGroup(cmd *exec.Cmd, grace time.Duration, done chan error, log *slog.Logger) {
	target := signalTarget(cmd)
	_ = syscall.Kill(target, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(grace):
		log.Warn("run did not exit on SIGTERM -- killed", "grace", grace)
		_ = syscall.Kill(target, syscall.SIGKILL)
		<-done
	}
}

// reapGroup kills what the model process left running after exiting. It runs
// after the leader was waited on, so the group id could in principle have
// been recycled -- an accepted race; the import re-checks staging at open
// time and does not depend on this having worked.
func reapGroup(cmd *exec.Cmd) {
	_ = syscall.Kill(signalTarget(cmd), syscall.SIGKILL)
}

func timestamp() string { return time.Now().UTC().Format(time.RFC3339) }

// datestamp is the YYYY-MM-DD prefix event entries carry.
func datestamp() string { return time.Now().UTC().Format("2006-01-02") }
