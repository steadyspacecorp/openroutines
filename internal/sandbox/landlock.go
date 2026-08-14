package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sync"
	"syscall"

	"github.com/landlock-lsm/go-landlock/landlock"
	llsys "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// ShimCommand re-enters this binary as the sandbox for one attempt:
// `sandbox-exec --ro DIR --rw DIR -- argv...` applies the rules to itself and
// execs argv, because a Landlock domain is inherited across execve and cannot
// be installed in a child from the parent. Internal, not a command anyone types.
const ShimCommand = "sandbox-exec"

// The kernel's Landlock ABI level, 0 where Landlock is unavailable (not
// compiled in, or a kernel older than 5.13). Read once -- it cannot change
// under a running process -- and used to decide what this rung may claim.
var landlockABI = sync.OnceValue(func() int {
	v, err := llsys.LandlockGetABIVersion()
	if err != nil {
		return 0
	}
	return v
})

// The fallback rung, and the only one that asks the host for nothing at all: no
// runtime flag, no capability, no sysctl, no privilege. Weaker in ways worth
// stating -- an ungranted path is denied rather than absent, peers are listed
// in /proc, /tmp and /dev/shm are withheld, nothing collapses the
// process tree, and an ungranted file's metadata stays changeable at any ABI
// because Landlock has no right that covers it.
//
// What makes falling back safe automatically is the ptrace hook: it checks the
// domain boundary, and every way one process reads another's secrets goes
// through it (/proc/<pid>/environ, /proc/<pid>/mem, process_vm_readv,
// PTRACE_ATTACH).
type landlockDomain struct{}

func (landlockDomain) Name() string {
	return fmt.Sprintf("landlock domain, ABI v%d, no runtime flags", landlockABI())
}

// Nothing but the one property that varies with the kernel. Everything a
// namespace would give is absent here, which is what the runner keys its
// process-group sweep on.
func (landlockDomain) Capabilities() Capabilities {
	return Capabilities{
		UnnameablePaths:    false,
		PrivateProcessList: false,
		// LANDLOCK_SCOPE_SIGNAL, ABI v6 and later. Below it an attempt can
		// signal a peer -- a denial of service, not a disclosure: memory reads
		// go through the ptrace hook, which has been there since v1.
		UnsignalablePeers: landlockABI() >= 6,
		PrivateIPC:        false,
		PrivateTmp:        false,
		CollapsesTree:     false,
	}
}

// The character devices an attempt gets, named one by one because Landlock has
// no way to subtract: granting /dev whole would hand every attempt the
// container's shared /dev/shm. Same list devtmpfs holds, minus its symlinks
// into /proc/self/fd.
var devices = []string{
	"/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom",
	"/dev/tty", "/dev/ptmx", "/dev/pts",
}

// Command wraps argv in a Landlock domain granting the same OS bubblewrap
// grants plus this attempt's own paths, by re-executing this binary as the
// shim. This process's own path, not a lookup: an attempt is confined by the
// code that spawned it, never by whatever `openroutines` is on PATH.
func (landlockDomain) Command(workspace string, argv ...string) (*exec.Cmd, error) {
	if err := validateWorkspace(workspace); err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	args := []string{ShimCommand}
	for _, p := range slices.Concat(readOnlyOS, osConfig) {
		args = append(args, "--ro", p)
	}
	// procfs is not exempt from Landlock -- with no rule even /proc/self/status
	// is denied, which makes Node report zero CPUs and throw on
	// process.memoryUsage(). Granting it costs this rung its process-list
	// privacy and nothing else; the ptrace hook still guards what a peer holds.
	args = append(args, "--ro", "/proc")
	for _, p := range devices {
		args = append(args, "--rw", p)
	}
	// Not /tmp: the attempt's temporary directory lives inside its workspace,
	// so the container's shared /tmp is simply never granted.
	args = append(args, "--rw", workspace)
	args = append(args, "--")
	cmd := exec.Command(self, append(args, argv...)...)
	cmd.Dir = workspace
	return cmd, nil
}

// ExecConfined applies the rules named in args to this process and execs the
// rest, replacing itself. Exported for the CLI verb, and reached by the sandbox
// tests through the same entry point so the code that confines a real run is
// the code under test.
func ExecConfined(args []string) error {
	var ro, rw []string
	for len(args) > 0 {
		flag := args[0]
		if flag == "--" {
			args = args[1:]
			break
		}
		if len(args) < 2 {
			return fmt.Errorf("%s: %s needs a path", ShimCommand, flag)
		}
		switch flag {
		case "--ro":
			ro = append(ro, args[1])
		case "--rw":
			rw = append(rw, args[1])
		default:
			return fmt.Errorf("%s: unknown argument %q", ShimCommand, flag)
		}
		args = args[2:]
	}
	if len(args) == 0 {
		return fmt.Errorf("%s: nothing to exec", ShimCommand)
	}
	if err := confine(ro, rw); err != nil {
		return fmt.Errorf("%s: %w", ShimCommand, err)
	}
	// The boot probe builds its sandbox with no environment at all. execvp
	// answers an unset PATH with a built-in search path where Go's LookPath
	// simply fails, so restore the fallback -- capturing the environment first
	// so it does not reach the process being confined.
	env := os.Environ()
	if os.Getenv("PATH") == "" {
		_ = os.Setenv("PATH", "/usr/bin:/bin")
	}
	bin, err := exec.LookPath(args[0])
	if err != nil {
		return err
	}
	// Exec rather than spawn: the domain travels across execve, and the run
	// gets the pid the supervisor is already waiting on.
	return syscall.Exec(bin, args, env)
}

// Puts this process in a Landlock domain granting ro and rw and nothing else.
// Requested at ABI v6 -- the level that scopes signals and abstract sockets --
// and best effort from there down, which is safe only because of the check
// below: a kernel with no Landlock would otherwise degrade silently to
// enforcing nothing. What each ABI gives up is recorded in SECURITY.md; below
// v3 the notable loss is the truncate right, so an ungranted file can be
// emptied though never read or written.
func confine(ro, rw []string) error {
	if landlockABI() == 0 {
		return errors.New("this kernel has no Landlock support")
	}
	rules := make([]landlock.Rule, 0, len(ro)+len(rw))
	for _, p := range ro {
		if r, ok := grant(p, false); ok {
			rules = append(rules, r)
		}
	}
	for _, p := range rw {
		if r, ok := grant(p, true); ok {
			rules = append(rules, r)
		}
	}
	cfg := landlock.V6.BestEffort()
	if err := cfg.RestrictPaths(rules...); err != nil {
		return err
	}
	return cfg.RestrictScoped()
}

// Turns one path into a Landlock rule, or reports there is nothing to grant. A
// missing path is skipped rather than fatal, the same tolerance bubblewrap's
// --ro-bind-try gives: one image's layout should not become a requirement.
// Rights follow what the path is -- Landlock rejects a directory right on a
// regular file, and the ioctl right a terminal needs applies only to devices.
func grant(path string, write bool) (landlock.Rule, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, false
	}
	if info.IsDir() {
		if write {
			rule := landlock.RWDirs(path)
			// The pty slave is created beneath this directory, so its ioctl
			// permission must come from the directory rule rather than a file rule.
			if path == "/dev/pts" {
				rule = rule.WithIoctlDev()
			}
			return rule, true
		}
		return landlock.RODirs(path), true
	}
	if !write {
		return landlock.ROFiles(path), true
	}
	rule := landlock.RWFiles(path)
	if info.Mode()&os.ModeDevice != 0 {
		rule = rule.WithIoctlDev()
	}
	return rule, true
}
