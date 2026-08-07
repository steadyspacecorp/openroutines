// The three ways an attempt's model process spawns -- one spawnPlan
// constructor per deployment mode, each paired with its sessions_exec.go
// counterpart. The two look alike (same binary, same HOME/XDG vocabulary)
// but point in opposite trust directions: the spawn env is the attempt's
// confined world, the sessions exec the supervisor's way back in that the
// attempt must not influence.

package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"

	"github.com/steadyspacecorp/openroutines/internal/sandbox"
)

// spawnPlan is what one deployment-mode decision mints: the model process
// about to run, and how the attempt's sessions are reached after it exits.
// container carries the docker name when the run lives in a container --
// kills go through the daemon there, not the process group.
type spawnPlan struct {
	cmd       *exec.Cmd
	sessions  opencodeExec
	container string
}

// spawn picks the deployment mode for one attempt: in the runtime container
// by default (the container boundary is the trust boundary), natively inside
// the production image or when a contributor opts out.
func (sr *StagedRun) spawn(ocArgs []string) (spawnPlan, error) {
	switch {
	case nativeMode() && os.Getenv("OPENROUTINES_IN_CONTAINER") == "1":
		return sandboxedSpawn(sr.workspace, sr.runTmp, sr.staging.KnowledgeDir, sr.meta.AttemptUID, sr.env, ocArgs)
	case nativeMode():
		return nativeSpawn(sr.workspace, sr.runTmp, sr.env, ocArgs)
	default:
		return containerSpawn(sr.dir, sr.workspace, sr.meta.RunID, sr.env, ocArgs)
	}
}

// nativeMode reports whether to spawn opencode directly instead of in a
// container: inside the production image (which ships opencode), or when a
// contributor explicitly opts out with OPENROUTINES_NATIVE=1.
func nativeMode() bool {
	return os.Getenv("OPENROUTINES_IN_CONTAINER") == "1" || os.Getenv("OPENROUTINES_NATIVE") == "1"
}

// sandboxedSpawn is production: opencode from the image's PATH, behind the
// Landlock shim -- our own binary applies the rules to itself, then execs
// opencode. See design decision "Runs are sandboxed" for the fail-closed
// policy.
//
// HOME is a disposable per-attempt directory inside the workspace: a shared
// writable opencode home let one routine persist state -- plugins included --
// into a later routine's session. Provider auth arrives by env var, so
// opencode needs no durable home. hostOpencodeExec reads the sessions back
// from that same directory afterward, through a minted home of its own.
func sandboxedSpawn(workspace, runTmp, knowledgeDir string, uid uint32, env, ocArgs []string) (spawnPlan, error) {
	if _, err := exec.LookPath("opencode"); err != nil {
		return spawnPlan{}, fmt.Errorf("opencode not found in PATH (native mode) -- install it: https://opencode.ai")
	}
	attemptHome := filepath.Join(workspace, attemptHomeName)
	ro, rw := sandbox.Paths(workspace, knowledgeDir, runTmp, os.Getenv("HOME"), attemptHome)
	cmd := exec.Command(sandbox.HelperPath, append([]string{"sandbox-exec", "--", "opencode"}, ocArgs...)...)
	cmd.Env = slices.Concat(env, []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + attemptHome,
		"XDG_DATA_HOME=" + filepath.Join(attemptHome, ".local", "share"),
		"XDG_CONFIG_HOME=" + filepath.Join(attemptHome, ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(attemptHome, ".cache"),
		"TMPDIR=" + runTmp,
		sandbox.EnvRO + "=" + sandbox.JoinPaths(ro),
		sandbox.EnvRW + "=" + sandbox.JoinPaths(rw),
		sandbox.EnvAttemptUID + "=" + strconv.FormatUint(uint64(uid), 10),
		sandbox.EnvUnsafeOverride + "=" + os.Getenv(sandbox.EnvUnsafeOverride),
	})
	cmd.Dir = workspace
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:    true,
		Credential: &syscall.Credential{Uid: uid, Gid: uid},
	}
	return spawnPlan{cmd: cmd, sessions: hostOpencodeExec(workspace)}, nil
}

// nativeSpawn is OPENROUTINES_NATIVE=1: an explicit, unconfined dev opt-in
// (local user runs are confined by the run container instead). The
// developer's real HOME stays: their opencode auth lives there -- which also
// means the session lands in their own store, reached after the run by
// working directory (opencode scopes `session list` to the directory a
// session ran in, and the workspace is this attempt's alone --
// nativeOpencodeExec leans on exactly that).
func nativeSpawn(workspace, runTmp string, env, ocArgs []string) (spawnPlan, error) {
	if _, err := exec.LookPath("opencode"); err != nil {
		return spawnPlan{}, fmt.Errorf("opencode not found in PATH (native mode) -- install it: https://opencode.ai")
	}
	cmd := exec.Command("opencode", ocArgs...)
	cmd.Env = slices.Concat(env, []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"TMPDIR=" + runTmp,
	})
	cmd.Dir = workspace
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return spawnPlan{cmd: cmd, sessions: nativeOpencodeExec(workspace)}, nil
}

// containerSpawn is the local default: the runtime image with the workspace
// mounted, nothing else from the host visible (container.go holds the docker
// plumbing). containerOpencodeExec re-enters the same image afterward.
func containerSpawn(dir, workspace, runID string, env, ocArgs []string) (spawnPlan, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return spawnPlan{}, fmt.Errorf("docker is required to run routines -- the model process executes in a container (see README prerequisites); contributors with opencode installed locally can set OPENROUTINES_NATIVE=1")
	}
	image := runtimeImageTag(dir)
	if err := ensureRuntimeImage(dir, image); err != nil {
		return spawnPlan{}, err
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
			return spawnPlan{}, err
		}
		_ = os.Chmod(p, 0o777)
	}
	name := "openroutines-" + runID
	return spawnPlan{
		cmd:       containerCmd(name, workspace, image, env, ocArgs),
		sessions:  containerOpencodeExec(workspace, image),
		container: name,
	}, nil
}
