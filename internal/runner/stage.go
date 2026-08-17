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
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/scrub"
)

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

	echo io.Writer
}

func (p *PreparedAttempt) Discard() error {
	p.secrets.release()
	return p.workspace.Cleanup()
}

const attemptHomeName = ".home"

func Stage(dir string, agent *config.Agent, r *routine.Routine, attempt Attempt, knowledgeLock sync.Locker) (prepared *PreparedAttempt, err error) {
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
	if err := func() error {
		// Under the knowledge lock: one worktree read becomes both the run's
		// working copy and the import's pristine base, never a
		// settlement-in-progress halfway through writing.
		knowledgeLock.Lock()
		defer knowledgeLock.Unlock()
		if attempt.SnapshotDir != "" {
			if err := knowledge.CloneTree(attempt.SnapshotDir, workspace.BaseDir); err != nil {
				return err
			}
		} else {
			if err := store.Ensure(); err != nil {
				return err
			}
			if err := store.Snapshot(workspace.BaseDir); err != nil {
				return err
			}
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
	tempDir := filepath.Join(workspaceRoot, ".runtmp")
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return nil, err
	}
	attemptHome := filepath.Join(workspaceRoot, attemptHomeName)
	if err := os.MkdirAll(attemptHome, 0o755); err != nil {
		return nil, err
	}

	env := attemptEnv(agent, r, attempt, secrets)
	opencodeArgs := attemptArgs(r, model)

	ok = true
	return &PreparedAttempt{
		agentDir:     dir,
		routine:      r,
		attempt:      attempt,
		model:        model,
		timeout:      timeout,
		secrets:      secrets,
		workspace:    workspace,
		tempDir:      tempDir,
		env:          env,
		opencodeArgs: opencodeArgs,
	}, nil
}

func attemptEnv(agent *config.Agent, r *routine.Routine, attempt Attempt, secrets *runSecrets) []string {
	env := []string{
		"TZ=" + agent.Timezone,
		"OPENROUTINES_RUN_ID=" + attempt.RunID,
		"OPENROUTINES_ATTEMPT_ID=" + attempt.ID(),
		"OPENROUTINES_URL=" + r.Frontmatter.EffectiveURL(),
	}
	if !attempt.ScheduledFor.IsZero() {
		env = append(env, "OPENROUTINES_SCHEDULED_FOR="+attempt.ScheduledFor.Format(time.RFC3339))
	}
	if r.Frontmatter.Websearch {
		env = append(env, "OPENCODE_ENABLE_EXA=1")
	}
	if !attempt.CoveredThrough.IsZero() {
		env = append(env, "OPENROUTINES_COVERED_THROUGH="+attempt.CoveredThrough.Format(time.RFC3339))
	}
	for _, k := range slices.Sorted(maps.Keys(secrets.env)) {
		env = append(env, k+"="+secrets.env[k])
	}
	for _, k := range slices.Sorted(maps.Keys(agent.Variables)) {
		if _, taken := secrets.env[strings.ToUpper(k)]; taken {
			continue
		}
		env = append(env, strings.ToUpper(k)+"="+agent.Variables[k])
	}
	return env
}

func attemptArgs(r *routine.Routine, model string) []string {
	opencodeArgs := []string{
		"--print-logs", "--log-level=" + logging.Level.Level().String(),
		"run", "--agent", "routine", "-m", model,
	}
	if r.Frontmatter.Effort != "" {
		opencodeArgs = append(opencodeArgs, "--variant", r.Frontmatter.Effort)
	}
	opencodeArgs = append(opencodeArgs, r.Body)
	return opencodeArgs
}

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

	if p.echo == nil {
		opencodeArgs = append(slices.Clip(opencodeArgs), "--format", "json")
	}

	runtime, err := p.runtime()
	if err != nil {
		return nil, nil, err
	}
	cmd, err := runtime.run(opencodeArgs)
	if err != nil {
		return nil, nil, err
	}
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

	var capture Capture
	sessions, fetchErr := fetchSessions(runtime.exec, attemptLog)
	if fetchErr != nil {
		attemptLog.Warn("session history unavailable -- no usage recorded, the session-outcome check did not run, and sessions were not exported", "error", fetchErr)
	} else {
		capture = captureSessions(sessions, attemptLog)
		exportSessions(sessions, r.Name, attempt.RunID, attemptLog)
	}

	result.Model = model
	result.Effort = r.Frontmatter.Effort
	result.Usage = capture.Usage
	// opencode exits 0 even when its agent loop died mid-turn; the session
	// record decides whether the run actually finished.
	if result.Outcome == Completed && capture.Failure != "" {
		result.Outcome = Crashed
		result.Hint = capture.Failure
	}
	if result.Outcome == Crashed && authFailurePattern.MatchString(capture.Failure) {
		provider := strings.SplitN(model, "/", 2)[0]
		_, injected := secrets.env[strings.ToUpper(creds.ProviderKeyName(provider))]
		result.Hint = authHint(p.agentDir, model, injected)
	} else if result.Outcome == Crashed && isModelNotFound(capture.Failure) {
		provider := strings.SplitN(model, "/", 2)[0]
		_, injected := secrets.env[strings.ToUpper(creds.ProviderKeyName(provider))]
		result.Hint = modelNotFoundHint(model, injected, secrets.credentialErr)
	}
	ok = true
	return result, workspace, nil
}
