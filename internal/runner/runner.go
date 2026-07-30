// Package runner executes one routine attempt: the per-run pipeline shared by
// `openroutines routines run` and the supervisor.
//
// The pipeline (design decision "Appendix: one run, end to end"): assemble a
// disposable run workspace (repo files plus a staged copy of memory, no git
// metadata anywhere), generate the opencode agent definition granting only
// declared skills, construct a clean environment holding only declared
// credentials, spawn headless opencode in its own process group with a
// timeout, then let the caller validate-and-import memory or discard it.
package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/memory"
	"github.com/steadyspacecorp/openroutines/internal/routine"
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
}

// ExecResult is one attempt's outcome. Hint, when set, classifies a common
// failure (currently: provider authentication) so it surfaces as a
// configuration problem instead of an opaque crash.
type ExecResult struct {
	Outcome  Outcome
	ExitCode int
	Duration time.Duration
	Hint     string
	Model    string // the resolved model this attempt ran with
	Effort   string // frontmatter reasoning effort, when set
	Usage    *Usage // token consumption; nil when the surface was unavailable
}

// tailBuffer keeps the last max bytes written -- enough to classify a
// failure from the end of the run's output without holding all of it.
type tailBuffer struct {
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

// authFailurePattern matches provider authentication errors in run output.
// A bad or missing API key otherwise reports as a bare "crashed" -- and in
// production burns five attempts and trips the circuit breaker before a
// human learns the cause was configuration. The `error:` forms are opencode
// passing a provider's status text through verbatim ("Error: Unauthorized"),
// which carries no key-shaped phrase to match on.
var authFailurePattern = regexp.MustCompile(`(?i)invalid x-api-key|api key is invalid|invalid api key|incorrect api key|401 unauthorized|authentication_error|missing.{0,20}api key|error:\s*unauthorized|invalid bearer token`)

// authHint names what the framework knows about an authentication failure
// and the provider's own message does not say: the provider it resolved,
// the endpoint when opencode.json declares one, and the credential the run
// injected -- or that none was. Without these, a bare "Unauthorized" sends
// the investigation outside the framework entirely.
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

// ErrFatal marks a start failure that no retry can fix: the next attempt
// would fail identically, so a caller spending a retry budget should give up
// now and let a person read the error. Classification lives here because the
// runner is what assembles the run and knows why it could not; the supervisor
// only asks whether the failure it was handed is one of these.
var ErrFatal = errors.New("not retryable")

// Staging is the attempt's staged memory, awaiting import or discard.
type Staging struct {
	MemoryDir string
	// BaseDir is the pristine copy of the snapshot the run started from --
	// supervisor-owned, outside the workspace the run can reach. The import
	// diffs staged memory against it, so concurrent runs' settlements
	// compose instead of clobbering each other.
	BaseDir   string
	workspace string

	// ConsumerThrough is the memory commit the delivery inbox was prepared
	// against -- set only for consumer routines, fixed before the run starts.
	ConsumerThrough string
}

// Cleanup discards the whole run workspace, staging and base included.
func (s *Staging) Cleanup() {
	os.RemoveAll(s.workspace)
	if s.BaseDir != "" {
		os.RemoveAll(s.BaseDir)
	}
}

// Consumed reports whether the routine created the consume marker: its
// explicit claim to have covered the whole injected inbox. The canonical
// location is the staged memory directory -- the one workspace path the
// filesystem sandbox leaves writable; the workspace root is still accepted
// for unsandboxed runs.
func (s *Staging) Consumed() bool {
	if _, err := os.Stat(filepath.Join(s.MemoryDir, memory.ConsumeMarker)); err == nil {
		return true
	}
	_, err := os.Stat(filepath.Join(s.workspace, memory.ConsumeMarker))
	return err == nil
}

// Result is a completed manual run (routines run).
type Result struct {
	RunID    string
	Outcome  Outcome
	ExitCode int
	Duration time.Duration
	Commit   string // memory commit hash, when one was made
	Hint     string // classified failure cause, when one was recognized
}

const runIDAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

func newRunID() string {
	buf := make([]byte, 10)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	for i, b := range buf {
		buf[i] = runIDAlphabet[int(b)%len(runIDAlphabet)]
	}
	return "run_" + string(buf)
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

// EffectiveTimeout is what an attempt actually gets: the declared timeout
// under the agent-wide ceiling (max_timeout in openroutines.yml). The cap is
// applied here rather than left to `check`, because the ceiling is the
// agent's guard against a runaway run burning tokens for a day -- a spend
// property cannot rest on a command the operator may never run.
func EffectiveTimeout(agent *config.Agent, r *routine.Routine) time.Duration {
	return min(DeclaredTimeout(agent, r), agent.MaxRunTimeout())
}

// DeclaredTimeout resolves frontmatter over agent defaults over 5m, before the
// ceiling applies. `check` reports on it; execution uses EffectiveTimeout.
func DeclaredTimeout(agent *config.Agent, r *routine.Routine) time.Duration {
	timeout := 5 * time.Minute
	for _, t := range []string{agent.Defaults.Timeout, r.FM.Timeout} {
		if t != "" {
			if d, err := time.ParseDuration(t); err == nil {
				timeout = d
			}
		}
	}
	return timeout
}

// Execute performs one attempt and returns its result plus the staged memory
// for the caller to import or discard. The caller must Cleanup() the staging.
// Cancelling ctx kills the attempt's process group (shutdown semantics).
func Execute(ctx context.Context, dir string, agent *config.Agent, r *routine.Routine, meta Meta) (*ExecResult, *Staging, error) {
	sr, err := Stage(dir, agent, r, meta)
	if err != nil {
		return nil, nil, err
	}
	return sr.Run(ctx)
}

// StagedRun is a fully prepared attempt: credentials resolved, workspace
// built, memory snapshotted, inbox and schedule written -- everything that
// reads the memory worktree or supervisor-owned state is done by the time
// Stage returns. Run spawns the model process and waits, touching neither
// again, which is what lets attempts execute in parallel while staging and
// settlement stay serialized behind the supervisor's memory lock (design
// decision "Overlap").
type StagedRun struct {
	dir       string
	r         *routine.Routine
	meta      Meta
	model     string
	timeout   time.Duration
	level     config.LogLevel
	secrets   *runSecrets
	staging   *Staging
	workspace string
	runTmp    string
	env       []string
	ocArgs    []string
}

// Stage prepares one attempt without spawning anything. On error, everything
// it acquired -- derived credentials, the workspace -- is already released.
func Stage(dir string, agent *config.Agent, r *routine.Routine, meta Meta) (*StagedRun, error) {
	model, err := EffectiveModel(agent, r)
	if err != nil {
		return nil, err
	}
	timeout := EffectiveTimeout(agent, r)
	level := agent.EffectiveLogLevel()
	// The harness config is parsed from the agent repository, not the
	// workspace copy buildWorkspace makes later: MCP permission rules must
	// never depend on pipeline ordering to see the server list. A file
	// opencode could not parse fails the attempt here, before anything runs.
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
	mem := memory.At(dir)
	if err := mem.Ensure(); err != nil {
		return nil, err
	}

	workspace, err := os.MkdirTemp("", "openroutines-run-*")
	if err != nil {
		return nil, err
	}
	staging := &Staging{MemoryDir: filepath.Join(workspace, memory.Dir), workspace: workspace}
	defer func() {
		if !ok {
			staging.Cleanup()
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
	// One read of the worktree becomes both the run's working copy and the
	// import's pristine base: snapshot into the base, clone the base into
	// staging.
	if err := mem.Snapshot(staging.BaseDir); err != nil {
		return nil, err
	}
	if err := memory.CloneTree(staging.BaseDir, staging.MemoryDir); err != nil {
		return nil, err
	}
	if r.FM.IsConsumer() {
		through, err := prepareInbox(dir, workspace, r.Name)
		if err != nil {
			return nil, fmt.Errorf("delivery inbox: %w", err)
		}
		staging.ConsumerThrough = through
	}
	if err := prepareSchedule(dir, workspace, r, agent.Timezone, time.Now()); err != nil {
		return nil, fmt.Errorf("forward schedule: %w", err)
	}
	if err := writeAgentDefinition(workspace, agent, r, oc.MCPServers(), meta); err != nil {
		return nil, err
	}
	runTmp := filepath.Join(workspace, ".runtmp")
	if err := os.MkdirAll(runTmp, 0o755); err != nil {
		return nil, err
	}

	// Clean environment: constructed, never inherited.
	env := []string{
		"TZ=" + agent.Timezone,
		"OPENROUTINES_RUN_ID=" + meta.RunID,
		"OPENROUTINES_ATTEMPT_ID=" + meta.AttemptID,
	}
	if meta.ScheduledFor != "" {
		env = append(env, "OPENROUTINES_SCHEDULED_FOR="+meta.ScheduledFor)
	}
	if r.FM.Websearch {
		// The websearch tool only registers when a search backend is enabled;
		// the permission rule in the generated definition is the actual gate.
		// Exa works keyless; a granted exa_api_key credential lands as
		// EXA_API_KEY, which the backend picks up for keyed use.
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

	// The opencode invocation is identical across spawn paths.
	ocArgs := []string{"run", "--agent", "routine", "-m", model}
	if r.FM.Effort != "" {
		ocArgs = append(ocArgs, "--variant", r.FM.Effort)
	}
	if level == config.LogDebug {
		// The full formatted transcript, plus opencode's own diagnostics.
		ocArgs = append(ocArgs, "--print-logs", "--log-level", "DEBUG")
	} else {
		// Raw JSON events, rendered into the bounded run log below.
		ocArgs = append(ocArgs, "--format", "json")
	}
	ocArgs = append(ocArgs, r.Body)

	ok = true
	return &StagedRun{
		dir:       dir,
		r:         r,
		meta:      meta,
		model:     model,
		timeout:   timeout,
		level:     level,
		secrets:   secrets,
		staging:   staging,
		workspace: workspace,
		runTmp:    runTmp,
		env:       env,
		ocArgs:    ocArgs,
	}, nil
}

// Run spawns the staged attempt's model process and waits it out. Derived
// credential material is revoked when the attempt ends, success or failure;
// a fresh attempt derives fresh material. On error the staging is already
// cleaned, exactly as Execute always behaved.
func (sr *StagedRun) Run(ctx context.Context) (*ExecResult, *Staging, error) {
	r, meta, staging := sr.r, sr.meta, sr.staging
	workspace, runTmp, env, ocArgs := sr.workspace, sr.runTmp, sr.env, sr.ocArgs
	model, timeout, level, secrets := sr.model, sr.timeout, sr.level, sr.secrets
	defer secrets.release()
	ok := false
	defer func() {
		if !ok {
			staging.Cleanup()
		}
	}()

	// Spawn the model process: in the runtime container by default (the
	// container boundary is the trust boundary), natively inside the
	// production image or when a contributor opts out.
	var cmd *exec.Cmd
	containerName := ""
	var ocExec opencodeExec // how capture reaches opencode after the run; nil in native dev mode
	if nativeMode() {
		if _, err := exec.LookPath("opencode"); err != nil {
			return nil, nil, fmt.Errorf("opencode not found in PATH (native mode) -- install it: https://opencode.ai")
		}
		home := os.Getenv("HOME")
		if os.Getenv("OPENROUTINES_IN_CONTAINER") == "1" {
			// Production: the model process runs behind the Landlock shim --
			// our own binary applies the rules to itself, then execs opencode.
			// See design decision "Runs are sandboxed" for the fail-closed policy.
			//
			// HOME is a disposable per-attempt directory inside the workspace:
			// a shared writable opencode home let one routine persist state --
			// plugins included -- into a later routine's session. Provider
			// auth arrives by env var, so opencode needs no durable home.
			self, err := os.Executable()
			if err != nil {
				return nil, nil, err
			}
			attemptHome := filepath.Join(workspace, attemptHomeName)
			ocExec = hostOpencodeExec(workspace)
			ro, rw := sandbox.Paths(workspace, staging.MemoryDir, runTmp, home, attemptHome)
			cmd = exec.Command(self, append([]string{"sandbox-exec", "--", "opencode"}, ocArgs...)...)
			cmd.Env = append(env,
				"PATH="+os.Getenv("PATH"),
				"HOME="+attemptHome,
				"XDG_DATA_HOME="+filepath.Join(attemptHome, ".local", "share"),
				"XDG_CONFIG_HOME="+filepath.Join(attemptHome, ".config"),
				"XDG_CACHE_HOME="+filepath.Join(attemptHome, ".cache"),
				"TMPDIR="+runTmp,
				sandbox.EnvRO+"="+sandbox.JoinPaths(ro),
				sandbox.EnvRW+"="+sandbox.JoinPaths(rw),
				sandbox.EnvUnsafeOverride+"="+os.Getenv(sandbox.EnvUnsafeOverride),
			)
		} else {
			// OPENROUTINES_NATIVE=1: an explicit, unconfined dev opt-in
			// (local user runs are confined by the run container instead).
			// The developer's real HOME stays: their opencode auth lives there.
			cmd = exec.Command("opencode", ocArgs...)
			cmd.Env = append(env,
				"PATH="+os.Getenv("PATH"),
				"HOME="+home,
				"TMPDIR="+runTmp,
			)
		}
		cmd.Dir = workspace
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	} else {
		if _, err := exec.LookPath("docker"); err != nil {
			return nil, nil, fmt.Errorf("docker is required to run routines -- the model process executes in a container (see README prerequisites); contributors with opencode installed locally can set OPENROUTINES_NATIVE=1")
		}
		image := runtimeImageTag(sr.dir)
		if err := ensureRuntimeImage(sr.dir, image); err != nil {
			return nil, nil, err
		}
		// Pre-create the attempt home world-writable: the container's agent
		// uid (10001) is not the host user's, and the workspace is a bind
		// mount discarded after the run.
		for _, p := range []string{
			filepath.Join(workspace, attemptHomeName),
			filepath.Join(workspace, attemptHomeName, ".local"),
			filepath.Join(workspace, attemptHomeName, ".local", "share"),
		} {
			if err := os.MkdirAll(p, 0o777); err != nil {
				return nil, nil, err
			}
			_ = os.Chmod(p, 0o777)
		}
		containerName = "openroutines-" + meta.RunID
		ocExec = containerOpencodeExec(workspace, image)
		cmd = containerCmd(containerName, workspace, image, env, ocArgs)
	}
	// The run log is leveled (design decision "Run output is rendered,
	// bounded, and leveled"): debug streams the formatted transcript as-is,
	// info renders the JSON event stream into bounded lines, warn and error
	// keep the stream out of the log entirely. The tail always captures what
	// the level would have logged -- failure classification and the failure
	// tail below read it.
	tail := &tailBuffer{max: 4096}
	var flush func()
	if level == config.LogDebug {
		scrubber := scrub.NewWriter(io.MultiWriter(os.Stdout, tail), secrets.scrub)
		cmd.Stdout, cmd.Stderr = scrubber, scrubber
		flush = scrubber.Flush
	} else {
		sink := io.Writer(os.Stdout)
		if level > config.LogInfo {
			sink = io.Discard
		}
		dst := io.MultiWriter(sink, tail)
		// Renderers scrub before they truncate -- the ordering that keeps a
		// truncation boundary from splitting a secret past the exact-value
		// matcher. stderr is not part of the event stream; its renderer just
		// passes lines through, bounded.
		rout := newRenderer(dst, secrets.scrub)
		rerr := newRenderer(dst, secrets.scrub)
		cmd.Stdout, cmd.Stderr = rout, rerr
		flush = func() { rout.Flush(); rerr.Flush() }
	}
	cmd.WaitDelay = pipeDrainDeadline

	done := make(chan error, 1)
	kill := func() {
		if containerName != "" {
			stopContainer(containerName)
			killClient(cmd, containerExitGrace, done)
		} else {
			killGroup(cmd, 10*time.Second, done)
		}
	}
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
		if werr != nil && !errors.Is(werr, exec.ErrWaitDelay) {
			res.Outcome = Crashed
			if ee, isExit := werr.(*exec.ExitError); isExit {
				res.ExitCode = ee.ExitCode()
			} else {
				res.ExitCode = -1
			}
		}
		// The model process is gone, but a descendant it detached from its
		// stdio is not: it outlives the attempt in the supervisor's own
		// container and keeps writing to staged memory while the pipeline
		// validates and imports it. The attempt ends with its whole process
		// group whichever way it ended. (In container mode the run's pid
		// namespace dies with `docker run --rm`, which does this already.)
		if containerName == "" {
			reapGroup(cmd)
		}
	case <-time.After(timeout):
		res.Outcome = Timeout
		kill()
	case <-ctx.Done():
		res.Outcome = Canceled
		kill()
	}
	res.Duration = time.Since(started).Round(time.Millisecond)
	flush()
	res.Model = model
	res.Effort = r.FM.Effort
	// Exit code alone is too weak a success signal for an agentic runtime:
	// opencode exits 0 on a session whose agent loop died mid-turn. The
	// session record is what says whether the run actually finished (design
	// decision "A run is completed only when its session ended cleanly").
	session := captureSession(workspace, ocExec)
	res.Usage = session.Usage
	if res.Outcome == Completed && session.Failure != "" {
		res.Outcome = Crashed
		res.Hint = session.Failure
	}
	if res.Outcome == Crashed && authFailurePattern.Match(tail.buf) {
		provider := strings.SplitN(model, "/", 2)[0]
		_, injected := secrets.env[strings.ToUpper(creds.ProviderKeyName(provider))]
		res.Hint = authHint(dir, model, injected)
	}
	if level > config.LogInfo && (res.Outcome == Crashed || res.Outcome == Timeout) && len(tail.buf) > 0 {
		// A failed attempt's last output is the diagnostic payload, not
		// chatter: it escapes the level gate, or a warn/error production
		// agent fails invisibly.
		_, _ = fmt.Fprintf(os.Stdout, "%s %s %s -- last output:\n%s\n", r.Name, meta.RunID, res.Outcome, bytes.TrimSpace(tail.buf))
	}
	ok = true
	return res, staging, nil
}

// Run executes routine `name` manually. noMemory discards staged memory
// writes and the run record after the otherwise ordinary run completes.
func Run(dir, name string, noMemory bool) (*Result, error) {
	agent, err := config.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("not an agent repository: %w", err)
	}
	r, err := routine.Find(dir, name)
	if err != nil {
		return nil, err
	}
	release, err := LockRoutine(dir, name)
	if errors.Is(err, ErrRoutineLocked) {
		return nil, fmt.Errorf("routine %s already has an attempt in flight (the supervisor or another terminal holds its lock) -- skipped", name)
	}
	if err != nil {
		return nil, err
	}
	defer release()
	runID := newRunID()
	exec, staging, err := Execute(context.Background(), dir, agent, r, Meta{RunID: runID, AttemptID: "attempt_01"})
	if err != nil {
		return nil, err
	}
	defer staging.Cleanup()

	res := &Result{RunID: runID, Outcome: exec.Outcome, ExitCode: exec.ExitCode, Duration: exec.Duration, Hint: exec.Hint}
	if noMemory {
		return res, nil
	}

	settlement, err := Settle(dir, r, staging, exec, Meta{RunID: runID, AttemptID: "attempt_01"}, "", nil)
	res.Outcome = settlement.Outcome
	res.Commit = settlement.Commit
	return res, err
}

// Settlement is one attempt's settled, durable outcome.
type Settlement struct {
	Outcome   Outcome // downgraded to Crashed when staged memory was rejected
	Detail    string  // the failure description recorded; "" for clean completions
	Discarded bool    // staged events.md change discarded (events: false)
	Commit    string  // settlement commit hash, "" when nothing changed
}

// Settle makes one attempt's end durable in memory -- the single settlement
// path for manual and scheduled runs. A completed attempt's staged memory is
// imported under routine policy and its consumer cursor advanced; a rejected
// import downgrades the outcome to Crashed. Any failure is recorded as an
// event, every attempt as a run record, and the whole settlement commits as
// one memory commit. stage, when set, runs before that commit so caller
// bookkeeping (scheduling state, abandonment tasks) rides the same commit.
// detail overrides the derived failure description -- for attempts that
// failed before producing a result; raw error text is fine, the append seam
// redacts what it records. A Canceled attempt
// gets only its run record: nothing settled -- the same logical run retries
// -- and no commit of its own (the shutdown commit carries the record).
func Settle(dir string, r *routine.Routine, staging *Staging, res *ExecResult, meta Meta, detail string, stage func(*Settlement)) (*Settlement, error) {
	mem := memory.At(dir)
	s := &Settlement{Outcome: res.Outcome, Detail: detail}
	if res.Outcome == Completed {
		discarded, err := importMemory(dir, r, staging)
		if err != nil {
			s.Outcome = Crashed
			s.Detail = "memory rejected: " + err.Error()
		} else {
			s.Discarded = discarded
			advanceConsumer(dir, r, staging, meta.RunID)
		}
	} else if s.Detail == "" && res.Outcome != Canceled {
		s.Detail = fmt.Sprintf("%s after %s (exit %d)", res.Outcome, res.Duration, res.ExitCode)
		if res.Hint != "" {
			s.Detail += " -- " + res.Hint
		}
	}
	if s.Outcome != Completed && s.Outcome != Canceled {
		_ = mem.AppendEvent(fmt.Sprintf("%s supervisor: routine %s (%s %s) %s", datestamp(), r.Name, meta.RunID, meta.AttemptID, s.Detail))
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

// importMemory applies routine-level memory policy, then imports the staged
// tree. A routine with events: false cannot record events: a staged change
// to events.md is discarded -- the worktree copy wins, the rest of the tree
// imports normally. Reports whether such a change was discarded.
func importMemory(dir string, r *routine.Routine, staging *Staging) (bool, error) {
	mem := memory.At(dir)
	discarded := false
	if !r.FM.RecordsEvents() {
		var err error
		if discarded, err = memory.RestoreFile(staging.MemoryDir, staging.BaseDir, "events.md"); err != nil {
			return false, err
		}
	}
	return discarded, mem.Import(staging.MemoryDir, staging.BaseDir)
}

// prepareInbox fixes the delivery boundary at the memory branch's current
// commit, renders every change since the consumer's cursor into inbox.md in
// the workspace, and returns the fixed `through` commit. A consumer with no
// cursor starts at the current state: nothing to replay, first consume
// initializes the cursor.
func prepareInbox(dir, workspace, consumer string) (string, error) {
	mem := memory.At(dir)
	through, err := mem.Head()
	if err != nil {
		return "", err
	}
	cursor, err := mem.LoadCursor(consumer)
	if err != nil {
		return "", err
	}
	from := ""
	var changes []memory.CommitChange
	if cursor != nil {
		from = cursor.ConsumedThrough
		if changes, err = mem.Changes(from, through); err != nil {
			if errors.Is(err, memory.ErrCursorUnreachable) {
				return "", fmt.Errorf("%w: %w -- repair or delete %s on the memory branch", ErrFatal, err, memory.CursorFile(consumer))
			}
			return "", err
		}
	}
	inbox := memory.RenderInbox(consumer, from, through, changes)
	return through, os.WriteFile(filepath.Join(workspace, memory.InboxFileName), []byte(inbox), 0o644)
}

// advanceConsumer moves a consumer routine's cursor through the inbox it just
// consumed. Runs after a successful import, before the completion commit, so
// consumption and results land in the same commit. No marker, no advance:
// completing a run does not imply consuming its inbox.
func advanceConsumer(dir string, r *routine.Routine, staging *Staging, runID string) {
	if !r.FM.IsConsumer() || staging.ConsumerThrough == "" || !staging.Consumed() {
		return
	}
	_ = memory.At(dir).SaveCursor(r.Name, memory.Cursor{
		ConsumedThrough: staging.ConsumerThrough,
		ByRun:           runID,
		At:              time.Now().UTC(),
	})
}

// recordJSON formats one run record line for runs.jsonl. Usage fields are
// per attempt (spend happens per attempt; retries would double-count a
// run-level figure) and absent means the runtime didn't report, never zero.
func recordJSON(r *routine.Routine, meta Meta, attempt int, res *ExecResult, manual bool) string {
	fields := map[string]any{
		"run_id":          meta.RunID,
		"routine":         r.Name,
		"attempt":         attempt,
		"outcome":         res.Outcome,
		"recorded_at":     timestamp(),
		"duration_ms":     res.Duration.Milliseconds(),
		"exit_code":       res.ExitCode,
		"scheduled_for":   meta.ScheduledFor,
		"covered_through": meta.CoveredThrough,
		"manual":          manual,
	}
	if res.Model != "" {
		fields["model"] = res.Model
	}
	if res.Effort != "" {
		fields["effort"] = res.Effort
	}
	// Why a failed attempt failed belongs in the machine-readable log too,
	// not only in the event: `status` and the check-in routine read this.
	if res.Hint != "" {
		fields["hint"] = res.Hint
	}
	if res.Usage != nil {
		fields["tokens"] = res.Usage
		fields["cost_reported"] = res.Usage.CostReported
	}
	record, _ := json.Marshal(fields)
	return string(record)
}

// runSecrets is a run's resolved secret material: the exact environment to
// inject, the values the log scrubber must redact, and cleanup for derived
// credentials (token revocation at attempt end).
type runSecrets struct {
	env     map[string]string
	scrub   map[string]string
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
// plus the auto-injected provider key for its model. A raw credential
// injects verbatim under its uppercase name; a typed credential (see
// design decision "Credentials have types") is derived by the trusted runner and
// injects its type's surface -- the stored root secret never enters the run.
func resolveCredentials(dir string, agent *config.Agent, r *routine.Routine, model string) (*runSecrets, error) {
	provider := strings.SplitN(model, "/", 2)[0]
	providerKey := creds.ProviderKeyName(provider)
	out := &runSecrets{env: map[string]string{}, scrub: map[string]string{}}

	key, keyErr := creds.LoadKey(dir)
	if keyErr != nil {
		if len(r.FM.Credentials) > 0 {
			return nil, fmt.Errorf("routine declares credentials but %v", keyErr)
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
			out.scrub[name] = v
			continue
		}
		derived, err := creds.Derive(name, spec, v)
		if err != nil {
			out.release()
			return nil, err
		}
		out.cleanup = append(out.cleanup, derived.Cleanup)
		for _, k := range slices.Sorted(maps.Keys(derived.Env)) {
			if err := out.setEnv(k, derived.Env[k]); err != nil {
				out.release()
				return nil, err
			}
		}
		maps.Copy(out.scrub, derived.Scrub)
	}
	if v, present := store[providerKey]; present {
		if err := out.setEnv(strings.ToUpper(providerKey), v); err != nil {
			out.release()
			return nil, err
		}
		out.scrub[providerKey] = v
	}
	return out, nil
}

// buildWorkspace assembles the run workspace by allow-list: exactly what a
// run needs -- the configuration file, opencode.json, and routines/. Everything else a
// run sees is staged deliberately by the pipeline: declared skills, the
// memory snapshot, the delivery inbox, the generated definition. A file not
// on the list -- the encrypted credential store, a stray key, dev rules like
// AGENTS.md -- does not exist in a run. (This replaced a deny-list that
// missed exactly one entry, credentials.yml.enc; allow-lists don't have
// that failure mode.) name is the routine being run -- whose errors are the
// only ones that can fail assembly.
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
	// A file another routine owns is not this run's problem: an unparseable
	// sibling is simply absent from the workspace, the same posture the
	// supervisor's tick takes when it schedules around one. Only an error
	// about this routine -- its own frontmatter, a name it collides on, or an
	// unattributed one that could be hiding it -- fails the attempt. The tick
	// never dispatches the first two (LoadAgent drops those names), so in
	// practice this catches a manual run and the read failures nobody owns.
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
			// Pass the real cause through: a duplicate global name reported
			// as "not found" sends the reader to the wrong directory.
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

// The standing instruction lives in instruction.md -- editable prose,
// compiled into the binary. Dynamic values and the conditional blocks
// (event recording, delivery inbox) render through text/template;
// the permission frontmatter stays code-generated because rule order is
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
	IsConsumer    bool
	Inbox         string
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

// renderDefinition generates the opencode agent for this run: default-deny
// skills with the routine's declared skills allowed, an explicit rule per
// configured MCP server (servers is the agent's opencode.json server list --
// passed in, so rule generation can never silently see an empty config), and
// the standing instruction that frames memory as records, never instructions.
func renderDefinition(agent *config.Agent, r *routine.Routine, servers []string, meta Meta) (string, error) {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "description: Generated for routine %s -- derived from frontmatter, do not edit\n", r.Name)
	b.WriteString("mode: primary\n")
	b.WriteString("permission:\n")
	// Web access is a grant, not a default: opencode allows webfetch out of
	// the box, and fetched content is model context -- a prompt-injection
	// vector. Both web tools get an explicit rule every run, deny unless the
	// routine's frontmatter opts in.
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
	// MCP servers are grants too: a configured server's tools reach a run
	// only when the routine's frontmatter names the server. opencode
	// registers MCP tools as <server>_<tool>, so one glob per configured
	// server closes or opens its whole surface.
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
		IsConsumer:    r.FM.IsConsumer(),
		Inbox:         memory.InboxFileName,
		Marker:        memory.ConsumeMarker,
		Variables:     variablesLine(agent),
	}); err != nil {
		return "", err
	}
	return b.String(), nil
}

// RenderDefinition generates a routine's opencode agent definition exactly
// as a run would, without running anything. check uses it to validate
// routine wiring -- frontmatter through generated definition, MCP rules
// included -- offline, with no provider key and no Docker.
func RenderDefinition(agent *config.Agent, r *routine.Routine, servers []string) (string, error) {
	return renderDefinition(agent, r, servers, Meta{RunID: "run_check", AttemptID: "attempt_00"})
}

// variablesLine renders the agent's variable names for the standing
// instruction ("$PRODUCT_REPO, $DOCS_URL"), so the model knows they exist
// without probing the environment.
func variablesLine(agent *config.Agent) string {
	names := slices.Sorted(maps.Keys(agent.Variables))
	for i, n := range names {
		names[i] = "$" + strings.ToUpper(n)
	}
	return strings.Join(names, ", ")
}

// pipeDrainDeadline bounds how long waiting on the run's output pipes may
// outlast the process itself. A grandchild that left the process group --
// a tool that daemonized into its own session -- keeps the inherited pipe
// open after the run is signalled and killed, and the wait for EOF would
// otherwise never end: the supervisor would hang on a run it had already
// terminated. On expiry the pipes are closed and what can be reaped is
// reaped; the orphan lives on, but it no longer holds the tick loop.
const pipeDrainDeadline = 5 * time.Second

// containerExitGrace is how long `docker run` gets to notice that its
// container is gone before the client itself is killed.
const containerExitGrace = 5 * time.Second

// killClient ends a container run: `docker stop` has already been asked to
// take the container down, so the client should follow it out. When it does
// not -- an unresponsive daemon, a stop that timed out -- the client is
// killed, because the alternative is the tick waiting on a docker CLI that
// may never return. The container can outlive the run; a stuck daemon is the
// operator's problem, but it must not become the supervisor's.
//
// Waiting for Wait to return is not optional: the caller reads the tail
// buffer the output goroutines write to, so returning before Wait would race
// them. The bound comes from making the process exit, not from walking away.
func killClient(cmd *exec.Cmd, grace time.Duration, done chan error) {
	select {
	case <-done:
	case <-time.After(grace):
		_ = cmd.Process.Kill()
		<-done // bounded by WaitDelay now that the process is going away
	}
}

// signalTarget is the run's own process group, or the process alone when the
// spawn path did not make it a group leader. Every spawn path that gets
// signalled sets Setpgid today; the guard is there because signalling -pid
// without it reaches the supervisor's own group and kills the supervisor.
func signalTarget(cmd *exec.Cmd) int {
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid {
		return -cmd.Process.Pid
	}
	return cmd.Process.Pid
}

// killGroup terminates the run's whole process group: SIGTERM, grace, SIGKILL.
// The waits are bounded by the command's WaitDelay, not by the group's
// willingness to exit.
func killGroup(cmd *exec.Cmd, grace time.Duration, done chan error) {
	target := signalTarget(cmd)
	_ = syscall.Kill(target, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(grace):
		_ = syscall.Kill(target, syscall.SIGKILL)
		<-done
	}
}

// reapGroup kills what the model process left running after exiting on its
// own. No grace period: the run is over, nothing left in the group has an
// output anyone reads, and the pipeline is about to treat staging as final.
// Unlike killGroup this runs after the leader has been waited on, so an
// already-empty group's id could in principle have been recycled by then --
// an accepted race: the alternative is leaving the descendant writing to
// memory the supervisor is about to commit. The import re-checks staging at
// open time and does not depend on this having worked (see memory.copyStaged).
func reapGroup(cmd *exec.Cmd) {
	_ = syscall.Kill(signalTarget(cmd), syscall.SIGKILL)
}

func timestamp() string { return time.Now().UTC().Format(time.RFC3339) }

// datestamp is the YYYY-MM-DD prefix event entries carry (see the events.md
// seed); precise times live in runs.jsonl.
func datestamp() string { return time.Now().UTC().Format("2006-01-02") }
