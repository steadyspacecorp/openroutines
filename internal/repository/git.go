package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Everything a git child inherits from this process -- what git and ssh need
// to work at all. Secrets are added separately only when a specific operation
// requires them.
var gitPassthrough = []string{
	"PATH",
	"HOME",
	"SSH_AUTH_SOCK",
	"TMPDIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
	"GIT_SSL_CAINFO", "GIT_PROXY_SSL_CAINFO",
}

type gitCmd struct {
	*exec.Cmd
	ctx    context.Context
	cancel context.CancelFunc
}

// Credential helpers are the only system/global Git settings re-injected;
// suppressing them would make HTTPS remotes unable to authenticate.
var credentialConfig = sync.OnceValue(readCredentialConfig)

func readCredentialConfig() []string {
	var flags []string
	for _, scope := range []string{"--system", "--global"} {
		cmd := exec.Command("git", "config", scope, "-z", "--get-regexp", `^credential\.`)
		cmd.Env = []string{}
		for _, name := range gitPassthrough {
			if v, ok := os.LookupEnv(name); ok {
				cmd.Env = append(cmd.Env, name+"="+v)
			}
		}
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		for entry := range strings.SplitSeq(string(out), "\x00") {
			if entry == "" {
				continue
			}
			key, value, _ := strings.Cut(entry, "\n")
			flags = append(flags, "-c", key+"="+value)
		}
	}
	return flags
}

func newGitCmd(dir string, args []string) *gitCmd {
	// Construct the child environment explicitly; inheriting it would expose
	// supervisor-only secrets through the child's /proc/<pid>/environ.
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	cmd := exec.CommandContext(ctx, "git", append(slices.Clone(credentialConfig()), args...)...)
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
	// Git delegates network I/O to children, so timeout cancellation must kill
	// the whole process group or a stalled transport keeps the output pipe open.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGitGroup(cmd.Process.Pid) }
	cmd.WaitDelay = gitDrainDeadline
	return &gitCmd{Cmd: cmd, ctx: ctx, cancel: cancel}
}

// SIGTERM gets Git to release lock files before SIGKILL is used.
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

func (c *gitCmd) fail(args []string, err error, out []byte) error {
	switch {
	case c.ctx.Err() != nil:
		return fmt.Errorf("git %s: timed out after %s: %s", strings.Join(args, " "), gitTimeout, strings.TrimSpace(string(out)))
	case errors.Is(err, exec.ErrWaitDelay):
		return fmt.Errorf("git %s: exited cleanly but something it spawned still held the output pipe after %s, so the output is incomplete: %s", strings.Join(args, " "), gitDrainDeadline, strings.TrimSpace(string(out)))
	}
	return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
}

var (
	gitTimeout       = 2 * time.Minute
	gitKillGrace     = 2 * time.Second
	gitDrainDeadline = 5 * time.Second
)

var hermeticConfig = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "protocol.file.allow=user",
	// Delivery uses anchored --grep patterns; fixed mode would silently skip trims.
	"-c", "grep.patternType=basic",
	"-c", "user.name=openroutines",
	"-c", "user.email=agent@openroutines.dev",
	"-c", "gc.auto=0",
	"-c", "maintenance.auto=false",
	"-c", "http.lowSpeedLimit=1000",
	"-c", "http.lowSpeedTime=60",
}

func git(dir string, args ...string) (string, error) {
	cmd := newGitCmd(dir, append(hermeticConfig, args...))
	defer cmd.cancel()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", cmd.fail(args, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func gitEnv(dir string, env []string, args ...string) (string, error) {
	cmd := newGitCmd(dir, append(hermeticConfig, args...))
	defer cmd.cancel()
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", cmd.fail(args, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func gitExitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

func gitStdin(dir, stdin string, args ...string) (string, error) {
	cmd := newGitCmd(dir, append(hermeticConfig, args...))
	defer cmd.cancel()
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", cmd.fail(args, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}
