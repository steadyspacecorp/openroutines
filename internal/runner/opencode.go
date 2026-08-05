package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	return home, func() { os.RemoveAll(home) }, nil
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
	dataHome := filepath.Join(workspace, attemptHomeName, ".local", "share")
	return func(args ...string) ([]byte, error) {
		home, cleanup, err := captureHome(workspace)
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
		return cmd.Output()
	}
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
		return cmd.Output()
	}
}

// containerOpencodeExec re-enters the runtime image with the workspace
// mounted -- local runs, where opencode exists only inside the image. No
// network involved; the image is already local.
func containerOpencodeExec(workspace, image string) opencodeExec {
	return func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), captureTimeout)
		defer cancel()
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
			image, "opencode",
		}
		cmd := exec.CommandContext(ctx, "docker", append(dargs, args...)...)
		return cmd.Output()
	}
}
