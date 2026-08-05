// How the runner executes opencode -- one implementation per deployment
// mode, minted once per attempt. Each mode answers for the whole
// lifecycle: run spawns the model process, kill and reap end it, exec runs
// the supervisor's follow-up subcommands. run and exec share the mode's
// facts but point in opposite trust directions: run builds the attempt's
// confined world, exec is the supervisor's way back in that the attempt
// must not influence.

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

	"github.com/steadyspacecorp/openroutines/internal/sandbox"
)

// opencode is one attempt's grip on its opencode runtime. run hands back
// the model process ready to start; kill ends it the way the mode demands
// and reap sweeps what it left behind; exec runs any one subcommand to
// completion as the supervisor afterward and returns its stdout. The
// runtime doesn't know what its consumers ask -- sessions.go is one,
// fetching through the opencodeExec contract exec happens to satisfy.
type opencode interface {
	run(ocArgs []string) (*exec.Cmd, error)
	kill(cmd *exec.Cmd, done chan error, log *slog.Logger)
	reap(cmd *exec.Cmd)
	exec(args ...string) ([]byte, error)
}

// opencode picks the attempt's deployment mode and checks its
// prerequisites: in the runtime container by default (the container
// boundary is the trust boundary), natively inside the production image or
// when a contributor opts out.
func (sr *StagedRun) opencode() (opencode, error) {
	switch {
	case nativeMode() && os.Getenv("OPENROUTINES_IN_CONTAINER") == "1":
		if _, err := exec.LookPath("opencode"); err != nil {
			return nil, fmt.Errorf("opencode not found in PATH (native mode) -- install it: https://opencode.ai")
		}
		return hostOpencode{
			workspace:    sr.workspace,
			runTmp:       sr.runTmp,
			knowledgeDir: sr.staging.KnowledgeDir,
			env:          sr.env,
		}, nil
	case nativeMode():
		if _, err := exec.LookPath("opencode"); err != nil {
			return nil, fmt.Errorf("opencode not found in PATH (native mode) -- install it: https://opencode.ai")
		}
		return nativeOpencode{workspace: sr.workspace, runTmp: sr.runTmp, env: sr.env}, nil
	default:
		if _, err := exec.LookPath("docker"); err != nil {
			return nil, fmt.Errorf("docker is required to run routines -- the model process executes in a container (see README prerequisites); contributors with opencode installed locally can set OPENROUTINES_NATIVE=1")
		}
		image := runtimeImageTag(sr.dir)
		if err := ensureRuntimeImage(sr.dir, image); err != nil {
			return nil, err
		}
		// Pre-create the attempt home world-writable: the container's agent
		// uid (10001) is not the host user's, and the workspace is a bind
		// mount discarded after the run.
		for _, p := range []string{
			filepath.Join(sr.workspace, attemptHomeName),
			filepath.Join(sr.workspace, attemptHomeName, ".local"),
			filepath.Join(sr.workspace, attemptHomeName, ".local", "share"),
		} {
			if err := os.MkdirAll(p, 0o777); err != nil {
				return nil, err
			}
			_ = os.Chmod(p, 0o777)
		}
		return containerOpencode{
			workspace: sr.workspace,
			image:     image,
			name:      "openroutines-" + sr.meta.RunID,
			env:       sr.env,
		}, nil
	}
}

// nativeMode reports whether to spawn opencode directly instead of in a
// container: inside the production image (which ships opencode), or when a
// contributor explicitly opts out with OPENROUTINES_NATIVE=1.
func nativeMode() bool {
	return os.Getenv("OPENROUTINES_IN_CONTAINER") == "1" || os.Getenv("OPENROUTINES_NATIVE") == "1"
}

// hostOpencode is production: opencode from the image's PATH, each run
// confined in a sandbox of its own -- the strongest rung the host allowed,
// probed at boot (see sandbox.Verify) -- holding the read-only OS, this
// workspace, and nothing else.
type hostOpencode struct {
	processGroup
	workspace    string
	runTmp       string
	knowledgeDir string
	env          []string
}

// home is the disposable per-attempt HOME inside the workspace -- the fact
// both paths share: run lives in it, exec reads the store under it.
func (h hostOpencode) home() string { return filepath.Join(h.workspace, attemptHomeName) }

func (h hostOpencode) dataHome() string { return filepath.Join(h.home(), ".local", "share") }

// run builds the model process inside this attempt's own sandbox. The
// supervisor proved a sandbox can be built here at boot, so failing to
// build one now is a run failure, not a policy decision. Where the
// operator disabled the sandbox, the attempt is unchanged in every other
// respect -- same constructed environment, same disposable HOME, same
// workspace -- because the hatch gives up the confinement, not the rest of
// what a run is. The supervisor said what that costs at boot.
//
// HOME is disposable and the attempt's alone: a shared writable opencode
// home let one routine persist state -- plugins included -- into a later
// routine's session. Provider auth arrives by env var, so opencode needs
// no durable home.
func (h hostOpencode) run(ocArgs []string) (*exec.Cmd, error) {
	cmd := exec.Command("opencode", ocArgs...)
	if !sandbox.Disabled() {
		var err error
		if cmd, err = sandbox.Command(sandbox.Attempt{
			Workspace: h.workspace,
			Writable:  []string{h.knowledgeDir, h.runTmp, h.home()},
		}, append([]string{"opencode"}, ocArgs...)...); err != nil {
			return nil, err
		}
	}
	cmd.Env = slices.Concat(h.env, []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + h.home(),
		"XDG_DATA_HOME=" + h.dataHome(),
		"XDG_CONFIG_HOME=" + filepath.Join(h.home(), ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(h.home(), ".cache"),
		"TMPDIR=" + h.runTmp,
	})
	cmd.Dir = h.workspace
	// A session of its own, not merely a process group: without one a
	// sandboxed process can push characters into the controlling terminal's
	// input queue with TIOCSTI and run commands outside the sandbox
	// (CVE-2017-5226). bwrap asks for this itself with --new-session; a rung
	// that is an ordinary subprocess cannot, so the spawn does it for them. A
	// session leader is a process group leader too, with pgid == pid, so the
	// post-run sweep still has the group it kills.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd, nil
}

// reap sweeps only where nothing else collapses the tree: the bubblewrap
// rung's pid namespace dies with the sandbox and takes every descendant
// with it, while the fallback rung (and the disabled hatch) leaves a
// detached descendant to the runner's group sweep -- a question about the
// confinement in force, not about which deployment mode this is.
func (h hostOpencode) reap(cmd *exec.Cmd) {
	if !sandbox.Provides().CollapsesTree {
		reapGroup(cmd)
	}
}

// exec runs one subcommand with a minted hygiene HOME: this process is not
// sandboxed -- it is an ordinary child of the supervisor -- so it must not
// take its home from the attempt (see captureHome). The attempt's store is
// reached by XDG_DATA_HOME instead: that path carries the attempt's data,
// never its code.
func (h hostOpencode) exec(args ...string) ([]byte, error) {
	home, cleanup, err := captureHome(h.workspace)
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
	return runToFile(cmd)
}

// captureHome mints the HOME one capture exec runs with: an empty,
// supervisor-owned directory the attempt never had write access to.
//
// opencode auto-loads plugins from its config dir at startup, `session
// list` and `export` included (verified against the pinned 1.18.3), and
// that dir resolves under HOME when XDG_CONFIG_HOME is unset. Pointed at
// the attempt's own home, the exec would execute whatever a
// prompt-injected routine left there.
//
// The directory comes from TMPDIR, so it fails closed rather than trust
// it: a TMPDIR inside the workspace would hand the attempt the very home
// this exists to deny it.
//
// What opencode installs under the home it may leave at modes that stop a
// plain walk, so the cleanup removes the tree the way a run workspace is
// removed.
func captureHome(workspace string) (string, func(), error) {
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
		if err := removeTree(home); err != nil {
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

// nativeOpencode is OPENROUTINES_NATIVE=1: the developer's own opencode,
// an explicit, unconfined dev opt-in (local user runs are confined by the
// run container instead). The developer's real HOME stays for both paths:
// their opencode auth lives there -- which also means the session lands in
// their own store, reached afterward by working directory (opencode scopes
// `session list` to the directory a session ran in, and the workspace is
// this attempt's alone). No capture-home hygiene either: the run already
// executed unconfined with this same HOME, so there is nothing left to
// deny it.
type nativeOpencode struct {
	processGroup
	workspace string
	runTmp    string
	env       []string
}

func (n nativeOpencode) run(ocArgs []string) (*exec.Cmd, error) {
	cmd := exec.Command("opencode", ocArgs...)
	cmd.Env = slices.Concat(n.env, []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"TMPDIR=" + n.runTmp,
	})
	cmd.Dir = n.workspace
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}

func (n nativeOpencode) exec(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "opencode", args...)
	cmd.Dir = n.workspace
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	return runToFile(cmd)
}

// containerOpencode is the local default: opencode exists only inside the
// runtime image, so both paths re-enter it with the workspace mounted at
// /work and nothing else from the host visible.
type containerOpencode struct {
	workspace string
	image     string
	name      string
	env       []string
}

// run builds the docker run invocation for the attempt. Env vars are
// passed by NAME only -- docker resolves the values from the client
// process's environment -- so secret values never appear on the command
// line (argv is world-readable via ps for the duration of the run).
func (c containerOpencode) run(ocArgs []string) (*exec.Cmd, error) {
	// HOME is the disposable per-attempt directory inside the mounted
	// workspace -- the same hygiene production applies, and what makes
	// opencode's session storage readable after the container exits
	// instead of vanishing with --rm.
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
	return cmd, nil
}

func (c containerOpencode) kill(cmd *exec.Cmd, done chan error, log *slog.Logger) {
	stopContainer(c.name)
	killClient(cmd, containerExitGrace, done, log)
}

// reap is a no-op: the run's pid namespace dies with `docker run --rm`,
// which reaps every descendant already.
func (c containerOpencode) reap(*exec.Cmd) {}

// captureHomeMount is where the capture exec's empty home lives inside
// the runtime image -- deliberately outside /work, the attempt's
// workspace.
const captureHomeMount = "/capture-home"

// captureOutName is where an in-container capture exec's stdout lands,
// inside the mounted workspace. docker's own stdout is a pipe from the
// container runtime to the client, so the file that defeats opencode's
// lossy exit (see runToFile) has to sit on opencode's side of that
// boundary.
const captureOutName = ".capture-out"

func (c containerOpencode) exec(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()
	// The workspace is model-written, so clear the landing path first:
	// a file the attempt planted there -- a symlink especially -- must
	// not decide where the redirect writes or what the read returns.
	outPath := filepath.Join(c.workspace, captureOutName)
	if err := os.Remove(outPath); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	// Mint the landing file here and hold the descriptor: the accepted
	// local residual lets planted plugin code run inside the capture
	// container, and a path re-resolved after the exec would let that code
	// swap in an absolute symlink and walk the host filesystem with the
	// supervisor's eyes. The container's shell opens the path once, before
	// opencode (and any plugin) starts, so both ends write and read this
	// one inode whatever the name points at later. World-writable for the
	// same reason the attempt home is: the image's agent uid is not the
	// host user's.
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
		// The empty home is a tmpfs rather than a host directory: it is
		// empty by construction, needs no world-writable host dir for the
		// image's agent uid, and dies with the container. exec stays on so
		// capture never breaks on something opencode installs under HOME --
		// bookkeeping must not fail a run.
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

// runToFile runs one capture exec with its stdout connected to a plain
// file and returns what landed there. opencode's CLI calls process.exit()
// as soon as its command handler returns, without draining the final
// stdout write -- and stream writes to a pipe are asynchronous, so a large
// `export` read through a pipe arrives cut at a 64 KiB boundary with exit
// code 0. Writes to a file are synchronous: a file as fd 1 is what makes
// the document arrive whole.
//
// The file comes from the supervisor's own TMPDIR like every supervisor
// temp -- never runTmp, the attempt's space -- but needs none of
// captureHome's discipline: nothing is loaded from it, the child inherits
// only the open descriptor, and the readback goes through that same
// descriptor, so no path the attempt could influence is ever resolved.
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

// tailBuffer keeps the last stderrTailCap bytes written through it. A
// capture exec's stderr exists only to explain a failure, and the
// explanation lands at the end of the stream -- while the stream itself is
// untrusted output that code in the exec can feed for the whole capture
// timeout, so keeping all of it would hand bookkeeping a memory
// exhaustion lever.
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

// execError keeps what a failed capture exec said on stderr: the log line
// built from it is the only trace of why bookkeeping degraded.
func execError(err error, stderr *tailBuffer) error {
	if s := strings.TrimSpace(stderr.String()); s != "" {
		return fmt.Errorf("%w: %s", err, s)
	}
	return err
}
