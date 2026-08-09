// Package runner executes one routine attempt: the per-run pipeline shared by
// `openroutines routines run` and the supervisor.
package runner

import (
	"context"
	"errors"
	"fmt"
	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/knowledge"
	"github.com/steadyspacecorp/openroutines/internal/logging"
	"github.com/steadyspacecorp/openroutines/internal/mode"
	"github.com/steadyspacecorp/openroutines/internal/routine"
	"github.com/steadyspacecorp/openroutines/internal/scrub"
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
)

// StagedRun is a fully prepared attempt: everything that reads the knowledge
// worktree or supervisor-owned state happens in Stage, and Run touches
// neither -- which is what lets attempts execute in parallel while staging
// and settlement serialize behind the knowledge lock.
type StagedRun struct {
	dir       string
	r         *routine.Routine
	meta      Attempt
	model     string
	timeout   time.Duration
	secrets   *runSecrets
	staging   *AttemptWorkspace
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
func Stage(dir string, agent *config.Agent, r *routine.Routine, meta Attempt, mu sync.Locker) (stagedRun *StagedRun, err error) {
	if mode.Current().Container && meta.AttemptUID == 0 {
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
	store := knowledge.NewStore(dir)

	workspace, err := os.MkdirTemp("", "openroutines-run-*")
	if err != nil {
		return nil, err
	}
	staging := &AttemptWorkspace{KnowledgeDir: filepath.Join(workspace, knowledge.Dir), workspace: workspace}
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
	if err := copyDeclaredSkills(dir, workspace, r.Frontmatter.Skills); err != nil {
		return nil, err
	}
	if err := applyDeclaredMCP(workspace, r.Frontmatter.MCP); err != nil {
		return nil, err
	}
	// Under the knowledge lock: one worktree read becomes both the run's
	// working copy and the import's pristine base, never a
	// settlement-in-progress halfway through writing.
	if err := func() error {
		mu.Lock()
		defer mu.Unlock()
		if err := store.Ensure(); err != nil {
			return err
		}
		if err := store.Snapshot(staging.BaseDir); err != nil {
			return err
		}
		if err := knowledge.CloneTree(staging.BaseDir, staging.KnowledgeDir); err != nil {
			return err
		}
		if r.Frontmatter.Reports {
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
	if meta.AttemptUID != 0 && mode.Current().Container {
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
	if !meta.ScheduledFor.IsZero() {
		env = append(env, "OPENROUTINES_SCHEDULED_FOR="+meta.ScheduledFor.Format(time.RFC3339))
	}
	if r.Frontmatter.Websearch {
		// Registers the search backend; the permission rule in the generated
		// definition is the actual gate. Exa works keyless, and a granted
		// exa_api_key lands as EXA_API_KEY for keyed use.
		env = append(env, "OPENCODE_ENABLE_EXA=1")
	}
	if !meta.CoveredThrough.IsZero() {
		env = append(env, "OPENROUTINES_COVERED_THROUGH="+meta.CoveredThrough.Format(time.RFC3339))
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
	if r.Frontmatter.Effort != "" {
		ocArgs = append(ocArgs, "--variant", r.Frontmatter.Effort)
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

func frameworkEnv(timezone string, r *routine.Routine, meta Attempt) []string {
	return []string{
		"TZ=" + timezone,
		"OPENROUTINES_RUN_ID=" + meta.RunID,
		"OPENROUTINES_ATTEMPT_ID=" + meta.ID(),
		"OPENROUTINES_URL=" + r.Frontmatter.EffectiveURL(),
	}
}

// Run spawns the staged attempt's model process and waits it out. Derived
// credential material is revoked when the attempt ends, success or failure;
// a fresh attempt derives fresh material. On error the staging is already
// cleaned, and a cleanup failure is joined to the returned error.
func (sr *StagedRun) Run(ctx context.Context) (result *AttemptResult, returnedStaging *AttemptWorkspace, err error) {
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
	res := &AttemptResult{Outcome: Completed}
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
	res.Effort = r.Frontmatter.Effort
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
