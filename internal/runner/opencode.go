// How the runner executes opencode -- one implementation per deployment
// mode, minted once per attempt. run and exec point in opposite trust
// directions: run builds the attempt's confined world, exec is the
// supervisor's way back in that the attempt must not influence.

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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/mode"
	"github.com/steadyspacecorp/openroutines/internal/sandbox"
)

// attemptRuntime is one attempt's grip on its opencode process: run hands
// back the model process ready to start, kill and reap end it, and exec runs
// one follow-up subcommand as the supervisor and returns its stdout.
type attemptRuntime interface {
	run(ocArgs []string) *exec.Cmd
	kill(cmd *exec.Cmd, done chan error, log *slog.Logger)
	reap(cmd *exec.Cmd)
	exec(args ...string) ([]byte, error)
}

// runtime picks the attempt's deployment mode and checks its prerequisites.
func (p *PreparedAttempt) runtime() (attemptRuntime, error) {
	currentMode := mode.Current()
	switch currentMode {
	case mode.DeployedContainer:
		if _, err := exec.LookPath("opencode"); err != nil {
			return nil, fmt.Errorf("opencode not found in PATH (native mode) -- install it: https://opencode.ai")
		}
		return sandboxedRuntime{
			workspace:    p.workspace.root,
			tempDir:      p.tempDir,
			knowledgeDir: p.workspace.KnowledgeDir,
			uid:          p.attempt.AttemptUID,
			env:          p.env,
		}, nil
	case mode.LocalNative:
		if _, err := exec.LookPath("opencode"); err != nil {
			return nil, fmt.Errorf("opencode not found in PATH (native mode) -- install it: https://opencode.ai")
		}
		return nativeRuntime{workspace: p.workspace.root, tempDir: p.tempDir, env: p.env}, nil
	default:
		if _, err := exec.LookPath("docker"); err != nil {
			return nil, fmt.Errorf("docker is required to run routines -- the model process executes in a container (see README prerequisites); contributors with opencode installed locally can set OPENROUTINES_NATIVE=1")
		}
		image := runtimeImageTag(p.agentDir)
		if err := ensureRuntimeImage(p.agentDir, image); err != nil {
			return nil, err
		}
		// Pre-create the attempt home world-writable: the container's agent
		// uid (10001) is not the host user's, and the workspace is a bind
		// mount discarded after the run.
		for _, path := range []string{
			filepath.Join(p.workspace.root, attemptHomeName),
			filepath.Join(p.workspace.root, attemptHomeName, ".local"),
			filepath.Join(p.workspace.root, attemptHomeName, ".local", "share"),
		} {
			if err := os.MkdirAll(path, 0o777); err != nil {
				return nil, err
			}
			_ = os.Chmod(path, 0o777)
		}
		return containerRuntime{
			workspace: p.workspace.root,
			image:     image,
			name:      "openroutines-" + p.attempt.RunID,
			env:       p.env,
		}, nil
	}
}

// sandboxedRuntime is production: opencode from the image's PATH, the run
// confined behind the Landlock shim as the attempt's own identity.
type sandboxedRuntime struct {
	processGroup
	workspace    string
	tempDir      string
	knowledgeDir string
	uid          uint32
	env          []string
}

// home is the disposable per-attempt HOME inside the workspace.
func (h sandboxedRuntime) home() string { return filepath.Join(h.workspace, attemptHomeName) }

func (h sandboxedRuntime) dataHome() string { return filepath.Join(h.home(), ".local", "share") }

// run builds the model process behind the Landlock shim -- our own binary
// applies the rules to itself, then execs opencode. HOME is disposable and
// the attempt's alone: a shared writable home let one routine persist state,
// plugins included, into a later routine's session.
func (h sandboxedRuntime) run(ocArgs []string) *exec.Cmd {
	ro, rw := sandbox.Paths(h.workspace, h.knowledgeDir, h.tempDir, os.Getenv("HOME"), h.home())
	cmd := exec.Command(sandbox.HelperPath, append([]string{"sandbox-exec", "--", "opencode"}, ocArgs...)...)
	cmd.Env = slices.Concat(h.env, []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + h.home(),
		"XDG_DATA_HOME=" + h.dataHome(),
		"XDG_CONFIG_HOME=" + filepath.Join(h.home(), ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(h.home(), ".cache"),
		"TMPDIR=" + h.tempDir,
		sandbox.EnvRO + "=" + sandbox.JoinPaths(ro),
		sandbox.EnvRW + "=" + sandbox.JoinPaths(rw),
		sandbox.EnvAttemptUID + "=" + strconv.FormatUint(uint64(h.uid), 10),
		sandbox.EnvUnsafeOverride + "=" + os.Getenv(sandbox.EnvUnsafeOverride),
	})
	cmd.Dir = h.workspace
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:    true,
		Credential: &syscall.Credential{Uid: h.uid, Gid: h.uid},
	}
	return cmd
}

// exec runs one subcommand with a minted hygiene HOME (see captureHome); the
// attempt's store is reached by XDG_DATA_HOME -- data, never code.
func (h sandboxedRuntime) exec(args ...string) ([]byte, error) {
	gid := attemptGroup(h.home())
	home, cleanup, err := captureHome(h.workspace, gid)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "opencode", args...)
	cmd.Dir = h.workspace
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_DATA_HOME=" + h.dataHome(),
	}
	// opencode writes to the attempt-owned store even to answer a read, with
	// modes the supervisor's group access cannot cover -- so the exec runs
	// as the attempt's own identity.
	if gid != 0 {
		if err := os.Chown(home, -1, int(gid)); err != nil {
			return nil, fmt.Errorf("granting the capture home to the attempt group: %w", err)
		}
		if err := os.Chmod(home, 0o770); err != nil {
			return nil, fmt.Errorf("granting the capture home to the attempt group: %w", err)
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: gid, Gid: gid}}
	}
	return runToFile(cmd)
}

// captureHome mints the HOME one capture exec runs with: empty and never
// attempt-writable. opencode auto-loads plugins from its config dir at
// startup, `session list` and `export` included (verified against 1.18.3) --
// pointed at the attempt's own home, the exec would execute whatever a
// prompt-injected routine left there. The directory comes from TMPDIR, so a
// TMPDIR inside the workspace is refused. gid names the identity the exec
// runs as; cleanup removes the tree through that identity's own help.
func captureHome(workspace string, gid uint32) (string, func(), error) {
	home, err := os.MkdirTemp("", "openroutines-capture-*")
	if err != nil {
		return "", nil, err
	}
	inside, err := underDir(workspace, home)
	if err != nil || inside {
		os.RemoveAll(home)
		if err != nil {
			return "", nil, err
		}
		return "", nil, fmt.Errorf("capture home %s is inside the run workspace -- TMPDIR must point outside it", home)
	}
	cleanup := func() {
		if err := removeAttemptTree(gid, home); err != nil {
			slog.Warn("could not remove the capture home", "path", home, "error", err)
		}
	}
	return home, cleanup, nil
}

// underDir reports whether path sits at or below dir, both resolved: the
// containment check has to survive /var -> /private/var style symlinks.
func underDir(dir, path string) (bool, error) {
	d, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false, err
	}
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(d, p)
	if err != nil {
		return false, err
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)), nil
}

// attemptGroup reads back the identity the attempt home was granted to at
// staging; 0 means no identity scheme (native mode, tests).
func attemptGroup(attemptHome string) uint32 {
	info, err := os.Stat(attemptHome)
	if err != nil {
		return 0
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Gid == uint32(os.Getgid()) {
		return 0
	}
	return st.Gid
}

// nativeRuntime is OPENROUTINES_NATIVE=1: the developer's own opencode,
// unconfined by explicit opt-in. Their real HOME stays (opencode auth lives
// there); sessions are reached by working directory, which is this attempt's
// alone. No capture-home hygiene -- the run already executed unconfined with
// this same HOME.
type nativeRuntime struct {
	processGroup
	workspace string
	tempDir   string
	env       []string
}

func (n nativeRuntime) run(ocArgs []string) *exec.Cmd {
	cmd := exec.Command("opencode", ocArgs...)
	cmd.Env = slices.Concat(n.env, []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"TMPDIR=" + n.tempDir,
	})
	cmd.Dir = n.workspace
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

func (n nativeRuntime) exec(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "opencode", args...)
	cmd.Dir = n.workspace
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	return runToFile(cmd)
}

// containerRuntime is the local default: opencode exists only inside the
// runtime image, so both paths re-enter it with the workspace mounted at
// /work and nothing else from the host visible.
type containerRuntime struct {
	workspace string
	image     string
	name      string
	env       []string
}

// run builds the docker run invocation for the attempt. Env vars are
// passed by NAME only -- docker resolves the values from the client
// process's environment -- so secret values never appear on the command
// line (argv is world-readable via ps for the duration of the run).
func (c containerRuntime) run(ocArgs []string) *exec.Cmd {
	// HOME is the disposable per-attempt directory inside the mounted
	// workspace, so session storage survives --rm.
	args := []string{
		"run", "--rm", "--init",
		"--name", c.name,
		"-v", c.workspace + ":/work",
		"-w", "/work",
		"-e", "HOME=/work/" + attemptHomeName,
		"-e", "XDG_DATA_HOME=/work/" + attemptHomeName + "/.local/share",
		"-e", "TMPDIR=/work/.runtmp",
	}
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
	return cmd
}

func (c containerRuntime) kill(cmd *exec.Cmd, done chan error, log *slog.Logger) {
	stopContainer(c.name)
	killClient(cmd, containerExitGrace, done, log)
}

// reap is a no-op: the run's pid namespace dies with `docker run --rm`,
// which reaps every descendant already.
func (c containerRuntime) reap(*exec.Cmd) {}

// captureHomeMount is where the capture exec's empty home lives inside
// the runtime image -- deliberately outside /work, the attempt's
// workspace.
const captureHomeMount = "/capture-home"

// captureOutName is where an in-container capture exec's stdout lands: the
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
		"-v", c.workspace + ":/work",
		// A tmpfs: empty by construction, dies with the container. exec
		// stays on so capture never breaks on something opencode installs
		// under HOME.
		"--tmpfs", captureHomeMount + ":mode=0777,exec",
		"-w", "/work",
		"-e", "HOME=" + captureHomeMount,
		"-e", "XDG_CONFIG_HOME=" + captureHomeMount + "/.config",
		"-e", "XDG_DATA_HOME=/work/" + attemptHomeName + "/.local/share",
		c.image, "sh", "-c", `exec opencode "$@" > /work/` + captureOutName, "opencode",
	}
	cmd := exec.CommandContext(ctx, "docker", append(dargs, args...)...)
	var stderr tailBuffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, execError(err, &stderr)
	}
	return io.ReadAll(out)
}

// processGroup is how the direct-spawn modes end a run: the model process
// leads its own process group, created at spawn so a kill reaches every
// descendant.
type processGroup struct{}

func (processGroup) kill(cmd *exec.Cmd, done chan error, log *slog.Logger) {
	killGroup(cmd, 10*time.Second, done, log)
}

func (processGroup) reap(cmd *exec.Cmd) { reapGroup(cmd) }

// captureTimeout bounds each capture exec: a hung docker or opencode must
// not stall the supervisor's tick.
const captureTimeout = 30 * time.Second

// runToFile runs one capture exec with its stdout on a plain file. opencode
// exits without draining its final stdout write, so a large export through a
// pipe arrives cut at a 64 KiB boundary with exit code 0; file writes are
// synchronous. The child inherits only the open descriptor and the readback
// uses the same one, so no attempt-influenced path is ever resolved.
func runToFile(cmd *exec.Cmd) ([]byte, error) {
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
	if err := cmd.Run(); err != nil {
		return nil, execError(err, &stderr)
	}
	// The child wrote through a dup of this descriptor, so the shared
	// offset now sits at end of file.
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(out)
}

// tailBuffer keeps the last stderrTailCap bytes: the failure explanation
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

// execError keeps what a failed capture exec said on stderr -- the only
// trace of why bookkeeping degraded.
func execError(err error, stderr *tailBuffer) error {
	if s := strings.TrimSpace(stderr.String()); s != "" {
		return fmt.Errorf("%w: %s", err, s)
	}
	return err
}
