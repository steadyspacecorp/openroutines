// Package runner executes one routine attempt: the per-run pipeline shared by
// `openroutines routines run` and the supervisor.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/knowledge"
	"github.com/steadyspacecorp/openroutines/internal/logging"
	"github.com/steadyspacecorp/openroutines/internal/mode"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/scrub"
)

// PreparedAttempt holds everything needed to spawn one attempt. Stage does
// every read from the knowledge worktree or supervisor-owned state; Run
// touches neither. Attempts can therefore execute in parallel while
// preparation and settlement serialize behind the knowledge lock.
type PreparedAttempt struct {
	agentDir     string
	routine      *routine.Routine
	attempt      Attempt
	model        string
	timeout      time.Duration
	secrets      *runSecrets
	workspace    *AttemptWorkspace
	tempDir      string
	env          []string
	opencodeArgs []string

	// echo, when set, receives the run's scrubbed stdout live -- the manual
	// `routines run` terminal. The supervisor never sets it.
	echo io.Writer
}

// Discard releases a prepared attempt that will not be spawned (for example,
// because its supervisor lost the lease after preparation).
func (p *PreparedAttempt) Discard() error {
	p.secrets.release()
	return p.workspace.Cleanup()
}

// attemptHomeName is the disposable per-attempt home inside the run
// workspace: sandbox hygiene in production, and what keeps session data
// readable after a local run's container exits.
const attemptHomeName = ".home"

// Stage prepares one attempt without spawning anything. knowledgeLock is the caller's
// knowledge lock, held only around the worktree reads -- credential
// resolution can spend seconds on the network. On error, everything Stage
// acquired is already released.
func Stage(dir string, agent *config.Agent, r *routine.Routine, attempt Attempt, knowledgeLock sync.Locker) (prepared *PreparedAttempt, err error) {
	if mode.Current() == mode.DeployedContainer && attempt.AttemptUID == 0 {
		return nil, fmt.Errorf("%w: production runs require a reserved attempt uid", ErrFatal)
	}
	model, err := EffectiveModel(agent, r)
	if err != nil {
		return nil, err
	}
	declared, badTimeout := declaredTimeout(agent, r)
	if badTimeout != "" {
		r.Log().Warn("unparseable timeout ignored -- falling back", "run_id", attempt.RunID, "value", badTimeout, "using", declared)
	}
	timeout := EffectiveTimeout(agent, r)
	if timeout != declared {
		r.Log().Warn("declared timeout capped by max_timeout", "run_id", attempt.RunID, "declared", declared, "effective", timeout)
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
	store := knowledge.NewStore(dir)

	workspaceRoot, err := os.MkdirTemp("", "openroutines-run-*")
	if err != nil {
		return nil, err
	}
	workspace := &AttemptWorkspace{KnowledgeDir: filepath.Join(workspaceRoot, knowledge.Dir), root: workspaceRoot}
	defer func() {
		if !ok {
			err = errors.Join(err, workspace.Cleanup())
		}
	}()
	if workspace.BaseDir, err = os.MkdirTemp("", "openroutines-base-*"); err != nil {
		return nil, err
	}

	if err := buildWorkspace(dir, workspaceRoot, r.Name); err != nil {
		return nil, err
	}
	if err := copyDeclaredSkills(dir, workspaceRoot, r.Frontmatter.Skills); err != nil {
		return nil, err
	}
	if err := applyDeclaredMCP(workspaceRoot, r.Frontmatter.MCP); err != nil {
		return nil, err
	}
	// Under the knowledge lock: one worktree read becomes both the run's
	// working copy and the import's pristine base, never a
	// settlement-in-progress halfway through writing.
	if err := func() error {
		knowledgeLock.Lock()
		defer knowledgeLock.Unlock()
		if err := store.Ensure(); err != nil {
			return err
		}
		if err := store.Snapshot(workspace.BaseDir); err != nil {
			return err
		}
		if err := knowledge.CloneTree(workspace.BaseDir, workspace.KnowledgeDir); err != nil {
			return err
		}
		if r.Frontmatter.Reports {
			through, firstRun, err := prepareChanges(dir, workspaceRoot, r.Name)
			if err != nil {
				return fmt.Errorf("delivery changes: %w", err)
			}
			workspace.Delivery = DeliveryBoundary{Through: through, FirstRun: firstRun}
		}
		if err := prepareSchedule(dir, workspaceRoot, r, agent.Timezone, time.Now()); err != nil {
			return fmt.Errorf("forward schedule: %w", err)
		}
		if attempt.Rehearsal != "" {
			fixture, err := os.ReadFile(attempt.Rehearsal)
			if err != nil {
				return fmt.Errorf("rehearsal fixture: %w", err)
			}
			if err := os.WriteFile(filepath.Join(workspaceRoot, RehearsalFileName), fixture, 0o444); err != nil {
				return fmt.Errorf("rehearsal fixture: %w", err)
			}
		}
		return nil
	}(); err != nil {
		return nil, err
	}
	if err := writeAgentDefinition(workspaceRoot, agent, r, oc.MCPServers(), attempt); err != nil {
		return nil, err
	}
	runTmp := filepath.Join(workspaceRoot, ".runtmp")
	if err := os.MkdirAll(runTmp, 0o755); err != nil {
		return nil, err
	}
	attemptHome := filepath.Join(workspaceRoot, attemptHomeName)
	if attempt.AttemptUID != 0 && mode.Current() == mode.DeployedContainer {
		// An attempt identity's gid equals its uid (template Dockerfile).
		if err := prepareWorkspaceAccess(attempt.AttemptUID, workspaceRoot); err != nil {
			return nil, fmt.Errorf("preparing read-only attempt workspace: %w", err)
		}
		if err := prepareAttemptTrees(attempt.AttemptUID, workspace.KnowledgeDir, runTmp, attemptHome); err != nil {
			return nil, fmt.Errorf("preparing attempt uid %d trees: %w", attempt.AttemptUID, err)
		}
		workspace.attemptUID = attempt.AttemptUID
	}

	// Clean environment: constructed, never inherited.
	env := frameworkEnv(agent.Timezone, r, attempt)
	if !attempt.ScheduledFor.IsZero() {
		env = append(env, "OPENROUTINES_SCHEDULED_FOR="+attempt.ScheduledFor.Format(time.RFC3339))
	}
	if r.Frontmatter.Websearch {
		// Registers the search backend; the permission rule in the generated
		// definition is the actual gate. Exa works keyless, and a granted
		// exa_api_key lands as EXA_API_KEY for keyed use.
		env = append(env, "OPENCODE_ENABLE_EXA=1")
	}
	if !attempt.CoveredThrough.IsZero() {
		env = append(env, "OPENROUTINES_COVERED_THROUGH="+attempt.CoveredThrough.Format(time.RFC3339))
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
	opencodeArgs := []string{
		"--print-logs", "--log-level=" + logging.Level.Level().String(),
		"run", "--agent", "routine", "-m", model,
	}
	if r.Frontmatter.Effort != "" {
		opencodeArgs = append(opencodeArgs, "--variant", r.Frontmatter.Effort)
	}
	opencodeArgs = append(opencodeArgs, r.Body)

	ok = true
	return &PreparedAttempt{
		agentDir:     dir,
		routine:      r,
		attempt:      attempt,
		model:        model,
		timeout:      timeout,
		secrets:      secrets,
		workspace:    workspace,
		tempDir:      runTmp,
		env:          env,
		opencodeArgs: opencodeArgs,
	}, nil
}

func frameworkEnv(timezone string, r *routine.Routine, attempt Attempt) []string {
	return []string{
		"TZ=" + timezone,
		"OPENROUTINES_RUN_ID=" + attempt.RunID,
		"OPENROUTINES_ATTEMPT_ID=" + attempt.ID(),
		"OPENROUTINES_URL=" + r.Frontmatter.EffectiveURL(),
	}
}

// Run spawns the prepared attempt's model process and waits it out. Derived
// credential material is revoked when the attempt ends, success or failure;
// a fresh attempt derives fresh material. On error the workspace is already
// cleaned, and a cleanup failure is joined to the returned error.
func (p *PreparedAttempt) Run(ctx context.Context) (result *AttemptResult, returnedWorkspace *AttemptWorkspace, err error) {
	r, attempt, workspace := p.routine, p.attempt, p.workspace
	opencodeArgs := p.opencodeArgs
	model, timeout, secrets := p.model, p.timeout, p.secrets
	defer secrets.release()
	ok := false
	defer func() {
		if !ok {
			err = errors.Join(err, workspace.Cleanup())
		}
	}()

	// Unattended runs get JSON events; a manual run keeps opencode's default
	// rendering for the human watching it.
	if p.echo == nil {
		opencodeArgs = append(slices.Clip(opencodeArgs), "--format", "json")
	}

	runtime, err := p.runtime()
	if err != nil {
		return nil, nil, err
	}
	cmd := runtime.run(opencodeArgs)
	// stderr carries opencode's diagnostic log, passed through scrubbed with
	// the attempt's identity appended -- a failed attempt is never
	// invisible. It also carries rendered run progress unless JSON events
	// were asked for; run output must not masquerade as log lines.
	oclog := logging.NewPassthrough(slog.String("routine", r.Name), slog.String("run_id", attempt.RunID))
	errOut := scrub.NewWriter(oclog)
	cmd.Stderr = errOut
	var out *scrub.Writer
	if p.echo != nil {
		out = scrub.NewWriter(p.echo)
		cmd.Stdout = out
	}
	cmd.WaitDelay = pipeDrainDeadline

	attemptLog := r.Log().With("run_id", attempt.RunID)
	done := make(chan error, 1)
	kill := func() { runtime.kill(cmd, done, attemptLog) }
	started := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	result = &AttemptResult{Outcome: Completed}
	go func() { done <- cmd.Wait() }()
	select {
	case werr := <-done:
		// ErrWaitDelay means the process exited fine but something it left
		// behind still held the output pipe: the run's outcome is the
		// process's, not the orphan's -- only the tail of the log is lost.
		if errors.Is(werr, exec.ErrWaitDelay) {
			attemptLog.Warn("run output abandoned after the drain deadline -- a descendant outlived the attempt and the log tail is truncated", "deadline", pipeDrainDeadline)
		} else if werr != nil {
			result.Outcome = Crashed
			var ee *exec.ExitError
			if errors.As(werr, &ee) {
				result.ExitCode = ee.ExitCode()
			} else {
				result.ExitCode = -1
			}
		}
		// A detached descendant could outlive the attempt and keep writing
		// to staged knowledge while the pipeline imports it -- the attempt
		// ends with everything it spawned.
		runtime.reap(cmd)
	case <-time.After(timeout):
		result.Outcome = Timeout
		kill()
	case <-ctx.Done():
		result.Outcome = Canceled
		kill()
	}
	result.Duration = time.Since(started).Round(time.Millisecond)
	if out != nil {
		out.Flush()
	}
	errOut.Flush()
	oclog.Flush()
	result.Model = model
	result.Effort = r.Frontmatter.Effort
	// opencode exits 0 even when its agent loop died mid-turn; the session
	// record decides whether the run actually finished.
	sessions, fetchErr := fetchSessions(runtime.exec, attemptLog)
	capture := captureSessions(sessions, fetchErr, attemptLog)
	result.Usage = capture.Usage
	result.SessionsDir = exportSessions(attempt, sessions, fetchErr, attemptLog)
	if result.Outcome == Completed && capture.Failure != "" {
		result.Outcome = Crashed
		result.Hint = capture.Failure
	}
	if result.Outcome == Crashed && authFailurePattern.MatchString(capture.Failure) {
		provider := strings.SplitN(model, "/", 2)[0]
		_, injected := secrets.env[strings.ToUpper(creds.ProviderKeyName(provider))]
		result.Hint = authHint(p.agentDir, model, injected)
	}
	ok = true
	return result, workspace, nil
}
