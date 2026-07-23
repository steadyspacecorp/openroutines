// Package runner executes one routine attempt: the per-run pipeline shared by
// `openroutines routines run|test` and the supervisor.
//
// The pipeline (DESIGN.md "Appendix: one run, end to end"): assemble a
// disposable run workspace (repo files plus a staged copy of memory, no git
// metadata anywhere), generate the opencode agent definition granting only
// declared skills, construct a clean environment holding only declared
// credentials, spawn headless opencode in its own process group with a
// timeout, then let the caller validate-and-import memory or discard it.
package runner

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
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
)

type Outcome string

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
	DryRun         bool   // routines test: acting tools denied, credentials withheld
}

// ExecResult is one attempt's outcome.
type ExecResult struct {
	Outcome  Outcome
	ExitCode int
	Duration time.Duration
}

// Staging is the attempt's staged memory, awaiting import or discard.
type Staging struct {
	MemoryDir string
	workspace string

	// ConsumerThrough is the memory commit the delivery inbox was prepared
	// against -- set only for consumer routines, fixed before the run starts.
	ConsumerThrough string
}

// Cleanup discards the whole run workspace, staging included.
func (s *Staging) Cleanup() { os.RemoveAll(s.workspace) }

// Consumed reports whether the routine created the consume marker: its
// explicit claim to have covered the whole injected inbox.
func (s *Staging) Consumed() bool {
	_, err := os.Stat(filepath.Join(s.workspace, memory.ConsumeMarker))
	return err == nil
}

// Result is a completed manual run (routines run|test).
type Result struct {
	RunID    string
	Outcome  Outcome
	ExitCode int
	Duration time.Duration
	Commit   string // memory commit hash, when one was made
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
		return "", fmt.Errorf("no model: set model in frontmatter or defaults.model in agent.yaml (openroutines configure)")
	}
	return model, nil
}

// EffectiveTimeout resolves frontmatter over agent defaults over 5m.
func EffectiveTimeout(agent *config.Agent, r *routine.Routine) time.Duration {
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
	model, err := EffectiveModel(agent, r)
	if err != nil {
		return nil, nil, err
	}
	timeout := EffectiveTimeout(agent, r)

	secrets, err := resolveCredentials(dir, r, model, meta.DryRun)
	if err != nil {
		return nil, nil, err
	}
	if err := memory.EnsureWorktree(dir); err != nil {
		return nil, nil, err
	}

	workspace, err := os.MkdirTemp("", "openroutines-run-*")
	if err != nil {
		return nil, nil, err
	}
	staging := &Staging{MemoryDir: filepath.Join(workspace, memory.Dir), workspace: workspace}
	ok := false
	defer func() {
		if !ok {
			staging.Cleanup()
		}
	}()

	if err := buildWorkspace(dir, workspace); err != nil {
		return nil, nil, err
	}
	if err := copyDeclaredSkills(dir, workspace, r.FM.Skills); err != nil {
		return nil, nil, err
	}
	if err := memory.Snapshot(dir, staging.MemoryDir); err != nil {
		return nil, nil, err
	}
	if r.FM.IsConsumer() {
		through, err := prepareInbox(dir, workspace, r.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("delivery inbox: %w", err)
		}
		staging.ConsumerThrough = through
	}
	if err := writeAgentDefinition(workspace, agent, r, meta); err != nil {
		return nil, nil, err
	}
	runTmp := filepath.Join(workspace, ".runtmp")
	if err := os.MkdirAll(runTmp, 0o755); err != nil {
		return nil, nil, err
	}

	// Clean environment: constructed, never inherited.
	env := []string{
		"TZ=" + agent.Timezone,
		"OPENROUTINES_RUN_ID=" + meta.RunID,
		"OPENROUTINES_ATTEMPT_ID=" + meta.AttemptID,
	}
	if meta.DryRun {
		// Skills and prompts can gate on this (e.g. a reporting helper that
		// would otherwise write to an external system).
		env = append(env, "OPENROUTINES_DRY_RUN=1")
	}
	if meta.ScheduledFor != "" {
		env = append(env, "OPENROUTINES_SCHEDULED_FOR="+meta.ScheduledFor)
	}
	if meta.CoveredThrough != "" {
		env = append(env, "OPENROUTINES_COVERED_THROUGH="+meta.CoveredThrough)
	}
	for k, v := range secrets {
		env = append(env, strings.ToUpper(k)+"="+v)
	}
	// Non-secret variables from agent.yaml, injected into every run (dry runs
	// included). On a name collision the credential wins; check flags it.
	for _, k := range slices.Sorted(maps.Keys(agent.Variables)) {
		if _, taken := secrets[k]; taken {
			continue
		}
		env = append(env, strings.ToUpper(k)+"="+agent.Variables[k])
	}

	// The opencode invocation is identical across spawn paths.
	ocArgs := []string{"run", "--agent", "routine", "-m", model}
	if r.FM.Effort != "" {
		ocArgs = append(ocArgs, "--variant", r.FM.Effort)
	}
	ocArgs = append(ocArgs, r.Body)

	// Spawn the model process: in the runtime container by default (the
	// container boundary is the trust boundary), natively inside the
	// production image or when a contributor opts out.
	var cmd *exec.Cmd
	containerName := ""
	if nativeMode() {
		if _, err := exec.LookPath("opencode"); err != nil {
			return nil, nil, fmt.Errorf("opencode not found in PATH (native mode) -- install it: https://opencode.ai")
		}
		home := os.Getenv("HOME")
		baseEnv := append(env,
			"PATH="+os.Getenv("PATH"),
			"HOME="+home, // opencode auth/config lives under HOME
			"TMPDIR="+runTmp,
		)
		if os.Getenv("OPENROUTINES_IN_CONTAINER") == "1" {
			// Production: the model process runs behind the Landlock shim --
			// our own binary applies the rules to itself, then execs opencode.
			// See DESIGN.md "Runs are sandboxed" for the fail-closed policy.
			self, err := os.Executable()
			if err != nil {
				return nil, nil, err
			}
			ro, rw := sandbox.Paths(workspace, runTmp, home)
			cmd = exec.Command(self, append([]string{"sandbox-exec", "--", "opencode"}, ocArgs...)...)
			cmd.Env = append(baseEnv,
				sandbox.EnvRO+"="+sandbox.JoinPaths(ro),
				sandbox.EnvRW+"="+sandbox.JoinPaths(rw),
				sandbox.EnvUnsafeOverride+"="+os.Getenv(sandbox.EnvUnsafeOverride),
			)
		} else {
			// OPENROUTINES_NATIVE=1: an explicit, unconfined dev opt-in
			// (local user runs are confined by the run container instead).
			cmd = exec.Command("opencode", ocArgs...)
			cmd.Env = baseEnv
		}
		cmd.Dir = workspace
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	} else {
		if _, err := exec.LookPath("docker"); err != nil {
			return nil, nil, fmt.Errorf("docker is required to run routines -- the model process executes in a container (see README prerequisites); contributors with opencode installed locally can set OPENROUTINES_NATIVE=1")
		}
		if err := ensureRuntimeImage(dir); err != nil {
			return nil, nil, err
		}
		containerName = "openroutines-" + meta.RunID
		cmd = containerCmd(containerName, workspace, env, ocArgs)
	}
	scrubber := scrub.NewWriter(os.Stdout, secrets)
	cmd.Stdout = scrubber
	cmd.Stderr = scrubber

	done := make(chan error, 1)
	kill := func() {
		if containerName != "" {
			stopContainer(containerName)
			<-done
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
		if werr != nil {
			res.Outcome = Crashed
			if ee, isExit := werr.(*exec.ExitError); isExit {
				res.ExitCode = ee.ExitCode()
			} else {
				res.ExitCode = -1
			}
		}
	case <-time.After(timeout):
		res.Outcome = Timeout
		kill()
	case <-ctx.Done():
		res.Outcome = Canceled
		kill()
	}
	res.Duration = time.Since(started).Round(time.Millisecond)
	scrubber.Flush()

	ok = true
	return res, staging, nil
}

// Run executes routine `name` manually. keep=true imports memory writes and
// records the run (routines run); keep=false discards everything (test).
func Run(dir, name string, keep bool) (*Result, error) {
	agent, err := config.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("not an agent repository: %w", err)
	}
	r, err := routine.Parse(filepath.Join(dir, "routines", name+".md"))
	if err != nil {
		return nil, err
	}
	runID := newRunID()
	exec, staging, err := Execute(context.Background(), dir, agent, r, Meta{RunID: runID, AttemptID: "attempt_01", DryRun: !keep})
	if err != nil {
		return nil, err
	}
	defer staging.Cleanup()

	res := &Result{RunID: runID, Outcome: exec.Outcome, ExitCode: exec.ExitCode, Duration: exec.Duration}
	if !keep {
		return res, nil // test: discard staging, record nothing
	}

	if res.Outcome == Completed {
		if _, err := ImportMemory(dir, r, staging); err != nil {
			res.Outcome = Crashed
			_ = memory.AppendEvent(dir, fmt.Sprintf("%s supervisor: routine %s (%s) memory rejected: %v", datestamp(), r.Name, runID, err))
		} else {
			AdvanceConsumer(dir, r, staging, runID)
		}
	} else {
		_ = memory.AppendEvent(dir, fmt.Sprintf("%s supervisor: routine %s (%s) %s after %s (exit %d)", datestamp(), r.Name, runID, res.Outcome, res.Duration, res.ExitCode))
	}
	if err := memory.AppendRunRecord(dir, RecordJSON(r, Meta{RunID: runID, AttemptID: "attempt_01"}, 1, exec, true)); err != nil {
		return res, err
	}
	commit, err := memory.Commit(dir, fmt.Sprintf("Run %s (%s): %s", r.Name, runID, res.Outcome))
	if err != nil {
		return res, err
	}
	res.Commit = commit
	return res, nil
}

// ImportMemory applies routine-level memory policy, then imports the staged
// tree. A routine with events: false cannot record events: a staged change
// to events.md is discarded -- the worktree copy wins, the rest of the tree
// imports normally. Reports whether such a change was discarded.
func ImportMemory(dir string, r *routine.Routine, staging *Staging) (bool, error) {
	discarded := false
	if !r.FM.RecordsEvents() {
		var err error
		if discarded, err = memory.RestoreFile(dir, staging.MemoryDir, "events.md"); err != nil {
			return false, err
		}
	}
	return discarded, memory.Import(dir, staging.MemoryDir)
}

// prepareInbox fixes the delivery boundary at the memory branch's current
// commit, renders every change since the consumer's cursor into inbox.md in
// the workspace, and returns the fixed `through` commit. A consumer with no
// cursor starts at the current state: nothing to replay, first consume
// initializes the cursor.
func prepareInbox(dir, workspace, consumer string) (string, error) {
	through, err := memory.Head(dir)
	if err != nil {
		return "", err
	}
	cursor, err := memory.LoadCursor(dir, consumer)
	if err != nil {
		return "", err
	}
	from := ""
	var changes []memory.CommitChange
	if cursor != nil {
		from = cursor.ConsumedThrough
		if changes, err = memory.Changes(dir, from, through); err != nil {
			return "", err
		}
	}
	inbox := memory.RenderInbox(consumer, from, through, changes)
	return through, os.WriteFile(filepath.Join(workspace, memory.InboxFileName), []byte(inbox), 0o644)
}

// AdvanceConsumer moves a consumer routine's cursor through the inbox it just
// consumed. Call after a successful import, before the completion commit, so
// consumption and results land in the same commit. No marker, no advance:
// completing a run does not imply consuming its inbox.
func AdvanceConsumer(dir string, r *routine.Routine, staging *Staging, runID string) {
	if !r.FM.IsConsumer() || staging.ConsumerThrough == "" || !staging.Consumed() {
		return
	}
	_ = memory.SaveCursor(dir, r.Name, memory.Cursor{
		ConsumedThrough: staging.ConsumerThrough,
		ByRun:           runID,
		At:              time.Now().UTC(),
	})
}

// RecordJSON formats one run record line for runs.jsonl.
func RecordJSON(r *routine.Routine, meta Meta, attempt int, res *ExecResult, manual bool) string {
	record, _ := json.Marshal(map[string]any{
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
	})
	return string(record)
}

// resolveCredentials builds the routine's secret set: declared credentials
// plus the auto-injected provider key for its model. Names map to env vars.
func resolveCredentials(dir string, r *routine.Routine, model string, dryRun bool) (map[string]string, error) {
	provider := strings.SplitN(model, "/", 2)[0]
	providerKey := creds.ProviderKeyName(provider)

	key, keyErr := creds.LoadKey(dir)
	if keyErr != nil {
		if len(r.FM.Credentials) > 0 && !dryRun {
			return nil, fmt.Errorf("routine declares credentials but %v", keyErr)
		}
		// No store: opencode may still have its own auth for the provider.
		return map[string]string{}, nil
	}
	store, err := creds.Read(dir, key)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if !dryRun {
		// Dry runs never receive the routine's secrets: nothing real can be
		// authenticated against, whatever the model decides to try.
		for _, name := range r.FM.Credentials {
			v, present := store[name]
			if !present {
				return nil, fmt.Errorf("routine declares credential %q, not present in %s", name, creds.FileName)
			}
			out[name] = v
		}
	}
	if v, present := store[providerKey]; present {
		out[providerKey] = v
	}
	return out, nil
}

// buildWorkspace copies the repo working tree into the run workspace,
// excluding git metadata, the real memory worktree, secrets, and generated
// definitions. Routine writes outside memory/ die with the workspace.
func buildWorkspace(dir, workspace string) error {
	skip := map[string]bool{
		".git":              true,
		memory.Dir:          true,
		".opencode":         true,
		"skills":            true, // only declared skills travel in -- see copyDeclaredSkills
		creds.KeyFileName:   true,
		".openroutines-tmp": true,
		// Development-session rules never reach runs: opencode loads a
		// project-root AGENTS.md (or CLAUDE.md fallback) into any session's
		// context, and those files are written for humans' coding agents.
		// Runtime instructions travel only in the generated definition.
		"AGENTS.md": true,
		"CLAUDE.md": true,
	}
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		if skip[strings.Split(rel, string(filepath.Separator))[0]] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		dest := filepath.Join(workspace, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dest, raw, 0o644)
	})
}

// copyDeclaredSkills places exactly the routine's declared skills into the
// workspace at opencode's discovery path (.opencode/skills/). Undeclared
// skills are not merely permission-denied -- they are not present at all.
func copyDeclaredSkills(dir, workspace string, names []string) error {
	for _, name := range names {
		src := filepath.Join(dir, "skills", name)
		if _, err := os.Stat(filepath.Join(src, "SKILL.md")); err != nil {
			return fmt.Errorf("declared skill %q not found in skills/", name)
		}
		dest := filepath.Join(workspace, ".opencode", "skills", name)
		err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
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
// (dry-run, event recording, delivery inbox) render through text/template;
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
	DryRun        bool
	RecordsEvents bool
	IsConsumer    bool
	Inbox         string
	Marker        string
	Variables     string // "$PRODUCT_REPO, $DOCS_URL" -- empty when none configured
}

// writeAgentDefinition generates the opencode agent for this run: default-deny
// skills with the routine's declared skills allowed, and the standing
// instruction that frames memory as records, never instructions.
func writeAgentDefinition(workspace string, agent *config.Agent, r *routine.Routine, meta Meta) error {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "description: Generated for routine %s -- derived from frontmatter, do not edit\n", r.Name)
	b.WriteString("mode: primary\n")
	b.WriteString("permission:\n")
	if meta.DryRun {
		// Dry run: the acting tools are denied at the permission layer --
		// the model cannot reach out even if it tries.
		b.WriteString("  bash: deny\n")
		b.WriteString("  webfetch: deny\n")
		b.WriteString("  websearch: deny\n")
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
		DryRun:        meta.DryRun,
		RecordsEvents: r.FM.RecordsEvents(),
		IsConsumer:    r.FM.IsConsumer(),
		Inbox:         memory.InboxFileName,
		Marker:        memory.ConsumeMarker,
		Variables:     variablesLine(agent),
	}); err != nil {
		return err
	}

	dir := filepath.Join(workspace, ".opencode", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "routine.md"), []byte(b.String()), 0o644)
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

// killGroup terminates the run's whole process group: SIGTERM, grace, SIGKILL.
func killGroup(cmd *exec.Cmd, grace time.Duration, done chan error) {
	pgid := -cmd.Process.Pid
	_ = syscall.Kill(pgid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(grace):
		_ = syscall.Kill(pgid, syscall.SIGKILL)
		<-done
	}
}

func timestamp() string { return time.Now().UTC().Format(time.RFC3339) }

// datestamp is the YYYY-MM-DD prefix event entries carry (see the events.md
// seed); precise times live in runs.jsonl.
func datestamp() string { return time.Now().UTC().Format("2006-01-02") }
