package runner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// attemptHomeName is the disposable per-attempt home inside the run
// workspace. Production created it for sandbox hygiene (alpha.22); local
// container runs point HOME here too, which is what keeps the attempt's
// session data readable after the container exits.
const attemptHomeName = ".home"

// opencodeExec runs one opencode subcommand in the attempt's context after
// the model process exits -- session capture and export reach opencode
// through it. The spawn path decides where opencode exists (on PATH in the
// production container and in native dev mode, via the runtime image for
// local runs) and returns its stdout.
type opencodeExec func(args ...string) ([]byte, error)

// captureTimeout bounds each capture exec: a hung docker or opencode must
// not stall the supervisor's tick.
const captureTimeout = 30 * time.Second

// captureHomeMount is where the capture's empty home lives inside the
// runtime image -- deliberately outside /work, the attempt's workspace.
const captureHomeMount = "/capture-home"

// captureHome mints the HOME one capture exec runs with: an empty,
// supervisor-owned directory the attempt never had write access to.
//
// The capture step is not sandboxed -- it is an ordinary child of the
// supervisor -- so it must not take its home from the attempt. opencode
// auto-loads plugins from its config dir at startup, `session list` and
// `export` included (verified against the pinned 1.18.3), and that dir
// resolves under HOME when XDG_CONFIG_HOME is unset. Pointed at the
// attempt's own home, capture would execute whatever a prompt-injected
// routine left there. The session store is reached by XDG_DATA_HOME
// instead: that path carries the attempt's data, never its code.
//
// The directory comes from TMPDIR, so it fails closed rather than trust
// it: a TMPDIR inside the workspace would hand the attempt the very home
// this exists to deny it.
//
// gid names the attempt identity the exec will run as (0 when there is
// none): what opencode installs under the home then arrives attempt-owned
// with modes the group cannot cover, so the cleanup removes the tree
// through that identity's own help.
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

// hostOpencodeExec runs opencode from PATH against the attempt's session
// store -- the production container, where the binary sits next to the
// supervisor. The working directory stays the workspace: opencode scopes
// sessions to the directory they ran in.
func hostOpencodeExec(workspace string) opencodeExec {
	attemptHome := filepath.Join(workspace, attemptHomeName)
	dataHome := filepath.Join(attemptHome, ".local", "share")
	return func(args ...string) ([]byte, error) {
		gid := attemptGroup(attemptHome)
		home, cleanup, err := captureHome(workspace, gid)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "opencode", args...)
		cmd.Dir = workspace
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + home,
			"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
			"XDG_DATA_HOME=" + dataHome,
		}
		// The store is attempt-owned and opencode writes there even to
		// answer a read (its startup log, sqlite's WAL side files), which
		// the supervisor's group access cannot cover: files opencode
		// creates 0644 never carried a group-write bit for the shim's
		// umask to keep. So the exec runs as the attempt's own identity,
		// with the minted home handed to it on the group axis.
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
}

// attemptGroup reads back the identity the supervisor granted the attempt
// home to at staging (gid equals uid by design), 0 when the home is grouped
// to the supervisor itself: no identity scheme -- native mode, tests -- and
// nothing to transition to.
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

// nativeOpencodeExec runs the developer's own opencode against their own
// session store -- OPENROUTINES_NATIVE=1, where the run itself used the
// real HOME. The store holds the developer's whole history, but opencode
// scopes `session list` to the working directory a session ran in, and the
// workspace is this attempt's alone, so only this attempt's sessions are
// reachable. No capture-home hygiene either: the run already executed
// unconfined with this same HOME, so there is nothing left to deny it.
func nativeOpencodeExec(workspace string) opencodeExec {
	return func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "opencode", args...)
		cmd.Dir = workspace
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
		return runToFile(cmd)
	}
}

// captureOutName is where an in-container capture exec's stdout lands,
// inside the mounted workspace. docker's own stdout is a pipe from the
// container runtime to the client, so the file that defeats opencode's
// lossy exit (see runToFile) has to sit on opencode's side of that
// boundary.
const captureOutName = ".capture-out"

// containerOpencodeExec re-enters the runtime image with the workspace
// mounted -- local runs, where opencode exists only inside the image. No
// network involved; the image is already local.
func containerOpencodeExec(workspace, image string) opencodeExec {
	return func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
		defer cancel()
		// The workspace is model-written, so clear the landing path first:
		// a file the attempt planted there -- a symlink especially -- must
		// not decide where the redirect writes or what the read returns.
		outPath := filepath.Join(workspace, captureOutName)
		if err := os.Remove(outPath); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		defer func() { _ = os.Remove(outPath) }()
		dargs := []string{
			"run", "--rm",
			"-v", workspace + ":/work",
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
			image, "sh", "-c", `exec opencode "$@" > /work/` + captureOutName, "opencode",
		}
		cmd := exec.CommandContext(ctx, "docker", append(dargs, args...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return nil, execError(err, &stderr)
		}
		return os.ReadFile(outPath)
	}
}

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
	var stderr bytes.Buffer
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

// execError keeps what a failed capture exec said on stderr: the log line
// built from it is the only trace of why bookkeeping degraded.
func execError(err error, stderr *bytes.Buffer) error {
	if s := strings.TrimSpace(stderr.String()); s != "" {
		return fmt.Errorf("%w: %s", err, s)
	}
	return err
}
