package knowledge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"
)

// gitPassthrough is everything a git child inherits from this process --
// what git and ssh need to work at all.
var gitPassthrough = []string{
	"PATH",          // git's own subcommands, ssh, credential helpers
	"HOME",          // ~/.ssh: known_hosts and the operator's keys and config
	"SSH_AUTH_SOCK", // the operator's ssh-agent
	"TMPDIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
	// A TLS-inspecting proxy's CA bundle; the global config that could carry
	// http.sslCAInfo is sent to /dev/null.
	"GIT_SSL_CAINFO", "GIT_PROXY_SSL_CAINFO",
}

// gitCmd is a git invocation together with the deadline that bounds it.
type gitCmd struct {
	*exec.Cmd
	ctx    context.Context
	cancel context.CancelFunc
}

// newGitCmd builds a git invocation with a constructed environment and no
// system or global config. Inheriting the environment would publish
// OPENROUTINES_MASTER_KEY in the child's /proc/<pid>/environ -- non-dumpable
// does not survive execve.
func newGitCmd(dir string, args []string) *gitCmd {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	cmd := exec.CommandContext(ctx, "git", append(slices.Clone(originRewrite), args...)...)
	cmd.Dir = dir
	cmd.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	for _, name := range gitPassthrough {
		if v, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+v)
		}
	}
	if sshCommand != "" {
		cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND="+sshCommand)
	}
	// git does the network through children (ssh, git-remote-https); the
	// deadline has to reach the whole group or a stalled transport keeps
	// holding the output pipe.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGitGroup(cmd.Process.Pid) }
	cmd.WaitDelay = gitDrainDeadline
	return &gitCmd{Cmd: cmd, ctx: ctx, cancel: cancel}
}

// killGitGroup ends a timed-out invocation's process group: SIGTERM, grace,
// SIGKILL. The grace matters: git releases its lock files on SIGTERM but not
// SIGKILL, and a stranded lock fails every later invocation.
func killGitGroup(pid int) error {
	pgid := -pid
	if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	time.Sleep(gitKillGrace)
	if err := syscall.Kill(pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// fail wraps a failed invocation, naming the deadline when it was the cause
// ("signal: killed" on a fetch would read as a supervisor bug, not an origin
// gone dark). An abandoned drain is a failure even though git exited cleanly:
// the output is truncated and callers parse it. Only the last case keeps the
// error in the chain -- gitExitCode must never read an exit status out of a
// deadline we imposed.
func (c *gitCmd) fail(args []string, err error, out []byte) error {
	switch {
	case c.ctx.Err() != nil:
		return fmt.Errorf("git %s: timed out after %s: %s", strings.Join(args, " "), gitTimeout, strings.TrimSpace(string(out)))
	case errors.Is(err, exec.ErrWaitDelay):
		return fmt.Errorf("git %s: exited cleanly but something it spawned still held the output pipe after %s, so the output is incomplete: %s", strings.Join(args, " "), gitDrainDeadline, strings.TrimSpace(string(out)))
	}
	return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
}

// The bounds on a git invocation; the drain has to outlast the grace.
// gitTimeout sits above the transport's low-speed bounds because those only
// fire once bytes move -- a blackholed connect parks for many minutes.
// Variables so tests can drive them.
var (
	gitTimeout       = 2 * time.Minute
	gitKillGrace     = 2 * time.Second
	gitDrainDeadline = 5 * time.Second
)

// hermeticConfig is the -c configuration every git invocation carries: no
// hooks, a fixed commit identity, no auto-gc (a detached gc keeps writing
// .git/objects after the command returns), and low-speed bounds so a quiet
// transfer is abandoned rather than parked forever.
var hermeticConfig = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "protocol.file.allow=user",
	// A repo-config grep.patternType=fixed would break the delivery feed's
	// anchored --grep and quietly put every retention trim back in it.
	"-c", "grep.patternType=basic",
	"-c", "user.name=openroutines",
	"-c", "user.email=agent@openroutines.dev",
	"-c", "gc.auto=0",
	"-c", "maintenance.auto=false",
	"-c", "http.lowSpeedLimit=1000",
	"-c", "http.lowSpeedTime=60",
}

// git runs a git command against the repo with hermetic configuration:
// no system/global config leaks in.
func git(dir string, args ...string) (string, error) {
	cmd := newGitCmd(dir, append(hermeticConfig, args...))
	defer cmd.cancel()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", cmd.fail(args, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitExitCode reports the status git exited with, or -1 when it never got to
// exit -- "git answered no" versus "git could not answer".
func gitExitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

// Worktree returns the knowledge worktree location inside the agent repo.
