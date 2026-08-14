// How the runner executes opencode -- one implementation per deployment
// mode, minted once per attempt. run and exec share that mode's boundary;
// exec re-enters it without the routine's credentials.

package runner

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/mode"
	"github.com/steadyspacecorp/openroutines/internal/sandbox"
)

// One attempt's grip on its opencode process: run hands back the model process
// ready to start, kill and reap end it, and exec runs one follow-up subcommand
// inside the same authority boundary and returns its stdout.
type attemptRuntime interface {
	run(ocArgs []string) (*exec.Cmd, error)
	kill(cmd *exec.Cmd, done chan error, log *slog.Logger)
	reap(cmd *exec.Cmd)
	exec(args ...string) ([]byte, error)
}

// Picks the attempt's deployment mode and checks its prerequisites.
func (p *PreparedAttempt) runtime() (attemptRuntime, error) {
	currentMode := mode.Current()
	switch currentMode {
	case mode.DeployedContainer:
		if _, err := exec.LookPath("opencode"); err != nil {
			return nil, fmt.Errorf("opencode not found in PATH (native mode) -- install it: https://opencode.ai")
		}
		return sandboxedRuntime{
			workspace: p.workspace.root,
			tempDir:   p.tempDir,
			env:       p.env,
		}, nil
	case mode.LocalNative:
		if _, err := exec.LookPath("opencode"); err != nil {
			return nil, fmt.Errorf("opencode not found in PATH (native mode) -- install it: https://opencode.ai")
		}
		return nativeRuntime{workspace: p.workspace.root, tempDir: p.tempDir, env: p.env}, nil
	case mode.LocalContainer:
		if _, err := exec.LookPath("docker"); err != nil {
			return nil, fmt.Errorf("docker is required to run routines -- the model process executes in a container (see README prerequisites); contributors with opencode installed locally can set OPENROUTINES_NATIVE=1")
		}
		image := runtimeImageTag(p.agentDir)
		if err := ensureRuntimeImage(p.agentDir, image); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Join(p.workspace.root, attemptHomeName, ".local", "share"), 0o755); err != nil {
			return nil, err
		}
		// A bind mount preserves the host's ownership, while the runtime image
		// uses uid 10001. This tree is disposable and contains no stored secrets.
		if err := makeWorldWritable(p.workspace.root); err != nil {
			return nil, err
		}
		return containerRuntime{
			workspace: p.workspace.root,
			image:     image,
			name:      "openroutines-" + p.attempt.RunID,
			env:       p.env,
		}, nil
	}
	return nil, fmt.Errorf("unsupported deployment mode %d", currentMode)
}

func makeWorldWritable(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o777)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			return os.Chmod(path, info.Mode().Perm()|0o666)
		}
		return nil
	})
}

// Production: opencode from the image's PATH, each run confined in a sandbox of
// its own -- the strongest rung the host allowed, probed at boot -- holding the
// read-only OS, this workspace, and nothing else.
type sandboxedRuntime struct {
	processGroup
	workspace string
	tempDir   string
	env       []string
}

func (h sandboxedRuntime) home() string { return filepath.Join(h.workspace, attemptHomeName) }

func (h sandboxedRuntime) dataHome() string { return filepath.Join(h.home(), ".local", "share") }

func (h sandboxedRuntime) command(ocArgs []string) (*exec.Cmd, error) {
	cmd := exec.Command("opencode", ocArgs...)
	if !sandbox.Disabled() {
		var err error
		if cmd, err = sandbox.Command(h.workspace, append([]string{"opencode"}, ocArgs...)...); err != nil {
			return nil, err
		}
	}
	cmd.Dir = h.workspace
	// A session, not merely a process group: without one a sandboxed process can
	// push characters into the controlling terminal's input queue with TIOCSTI
	// and run commands outside the sandbox (CVE-2017-5226). A session leader
	// leads a process group too, so the post-run sweep still has one to kill.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd, nil
}

// Builds the model process inside this attempt's own sandbox. Boot proved a
// sandbox can be built here, so failing to build one now is a run failure and
// not a policy decision. HOME is disposable and the attempt's alone: a shared
// writable home let one routine persist plugins into a later routine's session.
func (h sandboxedRuntime) run(ocArgs []string) (*exec.Cmd, error) {
	cmd, err := h.command(ocArgs)
	if err != nil {
		return nil, err
	}
	cmd.Env = slices.Concat(h.env, []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + h.home(),
		"XDG_DATA_HOME=" + h.dataHome(),
		"XDG_CONFIG_HOME=" + filepath.Join(h.home(), ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(h.home(), ".cache"),
		"TMPDIR=" + h.tempDir,
	})
	return cmd, nil
}

// Sweeps only where nothing else collapses the tree: the bubblewrap rung's pid
// namespace dies with the sandbox and takes every descendant with it, while the
// fallback rung and the open hatch leave a detached descendant behind.
func (h sandboxedRuntime) reap(cmd *exec.Cmd) {
	if !sandbox.Provides().CollapsesTree {
		reapGroup(cmd)
	}
}

// Runs one bookkeeping subcommand inside the attempt's boundary, without the
// routine's credentials.
func (h sandboxedRuntime) exec(args ...string) ([]byte, error) {
	cmd, err := h.command(args)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + h.home(),
		"XDG_CONFIG_HOME=" + filepath.Join(h.home(), ".config"),
		"XDG_DATA_HOME=" + h.dataHome(),
		"XDG_CACHE_HOME=" + filepath.Join(h.home(), ".cache"),
		"TMPDIR=" + h.tempDir,
	}
	return runToFile(ctx, cmd)
}

// OPENROUTINES_NATIVE=1: the developer's own opencode, unconfined by explicit
// opt-in. Their real HOME stays (opencode auth lives there); sessions are
// reached by working directory, which is this attempt's alone. The follow-up
// omits the routine environment but remains intentionally unconfined.
type nativeRuntime struct {
	processGroup
	workspace string
	tempDir   string
	env       []string
}

func (n nativeRuntime) run(ocArgs []string) (*exec.Cmd, error) {
	cmd := exec.Command("opencode", ocArgs...)
	cmd.Env = slices.Concat(n.env, []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"TMPDIR=" + n.tempDir,
	})
	cmd.Dir = n.workspace
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}

func (n nativeRuntime) exec(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()
	cmd := exec.Command("opencode", args...)
	cmd.Dir = n.workspace
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return runToFile(ctx, cmd)
}

// The local default: opencode exists only inside the
// runtime image, so both paths re-enter it with the workspace mounted at
// /work and nothing else from the host visible.
type containerRuntime struct {
	workspace string
	image     string
	name      string
	env       []string
}

// Builds the docker run invocation for the attempt. Env vars are
// passed by NAME only -- docker resolves the values from the client
// process's environment -- so secret values never appear on the command
// line (argv is world-readable via ps for the duration of the run).
func (c containerRuntime) run(ocArgs []string) (*exec.Cmd, error) {
	// HOME is the disposable per-attempt directory inside the mounted
	// workspace, so session storage survives --rm.
	args := []string{
		"run", "--rm", "--init",
		"--name", c.name,
	}
	args = append(args, c.workspaceArgs()...)
	for _, kv := range c.env {
		name, _, _ := strings.Cut(kv, "=")
		args = append(args, "-e", name)
	}
	args = append(args, c.image, "opencode")
	args = append(args, ocArgs...)
	cmd := exec.Command("docker", args...)
	// The docker client needs its own environment (daemon socket, config)
	// plus the values it will forward by name.
	cmd.Env = append(os.Environ(), c.env...)
	return cmd, nil
}

func (c containerRuntime) workspaceArgs() []string {
	return []string{
		"-v", c.workspace + ":/work",
		"-w", "/work",
		"-e", "HOME=/work/" + attemptHomeName,
		"-e", "XDG_DATA_HOME=/work/" + attemptHomeName + "/.local/share",
		"-e", "TMPDIR=/work/.runtmp",
	}
}

func (c containerRuntime) kill(cmd *exec.Cmd, done chan error, log *slog.Logger) {
	stopContainer(c.name)
	killClient(cmd, containerExitGrace, done, log)
}

// A no-op: the run's pid namespace dies with `docker run --rm`,
// which reaps every descendant already.
func (c containerRuntime) reap(*exec.Cmd) {}

// Where an in-container capture exec's stdout lands: the
// file that defeats opencode's lossy exit (see runToFile) has to sit on
// opencode's side of docker's stdout pipe.
const captureOutName = ".capture-out"

func (c containerRuntime) exec(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()
	// Clear the landing path first: a planted symlink must not decide where
	// the redirect writes.
	outPath := filepath.Join(c.workspace, captureOutName)
	if err := os.Remove(outPath); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	// Mint the landing file and hold the descriptor: planted plugin code can
	// run inside the capture container, and a path re-resolved after the
	// exec would let it swap in a symlink and walk the host filesystem with
	// the supervisor's eyes. Both ends use this one inode whatever the name
	// points at later. World-writable because the image's agent uid is not
	// the host user's.
	out, err := os.OpenFile(outPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(outPath) }()
	defer out.Close()
	if err := out.Chmod(0o666); err != nil {
		return nil, err
	}
	dargs := []string{
		"run", "--rm",
	}
	dargs = append(dargs, c.workspaceArgs()...)
	dargs = append(dargs, c.image, "sh", "-c", `exec opencode "$@" > /work/`+captureOutName, "opencode")
	cmd := exec.CommandContext(ctx, "docker", append(dargs, args...)...)
	var stderr tailBuffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, execError(err, &stderr)
	}
	return io.ReadAll(out)
}

// How the direct-spawn modes end a run: the model process
// leads its own process group, created at spawn so a kill reaches every
// descendant.
type processGroup struct{}

func (processGroup) kill(cmd *exec.Cmd, done chan error, log *slog.Logger) {
	killGroup(cmd, 10*time.Second, done, log)
}

func (processGroup) reap(cmd *exec.Cmd) { reapGroup(cmd) }

// Bounds each capture exec: a hung docker or opencode must
// not stall the supervisor's tick.
const captureTimeout = 30 * time.Second

// Runs one capture exec with its stdout on a plain file. opencode
// exits without draining its final stdout write, so a large export through a
// pipe arrives cut at a 64 KiB boundary with exit code 0; file writes are
// synchronous. The child inherits only the open descriptor and the readback
// uses the same one, so no attempt-influenced path is ever resolved.
func runToFile(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	out, err := os.CreateTemp("", "openroutines-capture-out-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(out.Name()) }()
	defer out.Close()
	cmd.Stdout = out
	var stderr tailBuffer
	cmd.Stderr = &stderr
	// With stdout on a file, stderr is the one pipe left that a descendant
	// opencode leaked could hold open past the exec's own death.
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer reapGroup(cmd)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var runErr error
	select {
	case runErr = <-done:
	case <-ctx.Done():
		_ = syscall.Kill(signalTarget(cmd), syscall.SIGKILL)
		<-done
		runErr = ctx.Err()
	}
	if runErr != nil {
		return nil, execError(runErr, &stderr)
	}
	// The child wrote through a dup of this descriptor, so the shared
	// offset now sits at end of file.
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(out)
}

// Keeps the last stderrTailCap bytes: the failure explanation
// lands at the end of the stream, and keeping all of an untrusted stream
// would hand bookkeeping a memory-exhaustion lever.
type tailBuffer struct {
	buf []byte
}

const stderrTailCap = 8 << 10

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if n := len(t.buf) - stderrTailCap; n > 0 {
		t.buf = append(t.buf[:0], t.buf[n:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }

// Keeps what a failed capture exec said on stderr -- the only
// trace of why bookkeeping degraded.
func execError(err error, stderr *tailBuffer) error {
	if s := strings.TrimSpace(stderr.String()); s != "" {
		return fmt.Errorf("%w: %s", err, s)
	}
	return err
}
