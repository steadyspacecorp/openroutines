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

// ShimCommand re-enters this binary as the sandbox for one attempt. It is an
// internal verb rather than part of the command surface: `openroutines
// sandbox-exec --ro DIR --rw DIR -- argv...` applies the rules to itself and
// execs argv, because a Landlock domain is inherited across execve and there
// is no way to install one in a child from the parent.
const ShimCommand = "sandbox-exec"

// landlockABI is the kernel's Landlock ABI level, or 0 where Landlock is not
// available at all -- not compiled in, or an older kernel than 5.13. Read
// once, because it cannot change under a running process, and used to decide
// what this rung may honestly claim rather than to decide whether to enforce.
var landlockABI = sync.OnceValue(func() int {
	v, err := llsys.LandlockGetABIVersion()
	if err != nil {
		return 0
	}
	return v
})

// landlockDomain is the fallback rung, and the only one that asks the host
// for nothing at all: no runtime flag, no capability, no sysctl, no
// privilege. A process puts itself in a Landlock domain with ordinary
// syscalls a container runtime's default seccomp profile allows.
//
// It is the weaker rung, and the ways it is weaker are worth stating rather
// than discovering: an ungranted path is denied rather than absent, peers are
// listed in /proc, /tmp and /dev/shm are the container's (this rung is
// granted neither), nothing collapses the process tree, and an ungranted
// file's metadata -- mode, timestamps, xattrs -- stays changeable at any ABI,
// because Landlock has no right that covers it and everything here shares one
// user.
//
// What it does keep is the part that decides whether a fallback is safe to
// take automatically. Landlock's domain boundary is checked by the kernel's
// ptrace hook as well as its file hooks, and every way one process reads
// another's secrets goes through that hook: /proc/<pid>/environ,
// /proc/<pid>/mem, process_vm_readv, PTRACE_ATTACH. A sandboxed attempt is in
// a domain that is neither the supervisor's (it has none) nor a sibling
// attempt's, so all of them are refused. Signals are scoped too from ABI v6,
// the one property here that depends on the kernel being new enough and so is
// claimed conditionally.
type landlockDomain struct{}

func (landlockDomain) Name() string {
	return fmt.Sprintf("landlock domain, ABI v%d, no runtime flags", landlockABI())
}

// Capabilities: nothing but the one property that varies with the kernel.
// Everything a namespace would give is absent here, which is what the runner
// keys its process-group sweep on.
func (landlockDomain) Capabilities() Capabilities {
	return Capabilities{
		UnnameablePaths:    false,
		PrivateProcessList: false,
		// LANDLOCK_SCOPE_SIGNAL, ABI v6 and later: an attempt cannot signal a
		// process outside its own domain. Below v6 it can, which is a peer
		// denial-of-service and not a disclosure -- reading a peer's memory
		// goes through the ptrace hook, which has been there since v1.
		UnsignalablePeers: landlockABI() >= 6,
		PrivateIPC:        false,
		PrivateTmp:        false,
		CollapsesTree:     false,
	}
}

// devices are the character devices an attempt gets. They are named one by
// one because Landlock has no way to subtract: granting /dev whole would hand
// every attempt the container's shared /dev/shm, the cross-run channel
// bubblewrap closes with a fresh devtmpfs. This is the same list that
// devtmpfs contains, minus its symlinks into /proc/self/fd.
var devices = []string{
	"/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom",
	"/dev/tty", "/dev/ptmx", "/dev/pts",
}

// Command wraps argv in a Landlock domain granting the same OS bubblewrap
// grants, plus this attempt's own paths, by re-executing this binary as the
// shim. The supervisor's own path is used rather than a lookup, so an attempt
// is confined by the code that spawned it and not by whatever `openroutines`
// happens to be on PATH.
func (landlockDomain) Command(a Attempt, argv ...string) (*exec.Cmd, error) {
	if err := a.validate(); err != nil {
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
	// procfs is not exempt from Landlock -- with no rule for it even
	// /proc/self/status is denied, which makes Node report zero CPUs and
	// throw on process.memoryUsage(). Granting it is what costs this rung its
	// process-list privacy, and nothing else: what a peer's /proc entry holds
	// is protected by the ptrace hook instead.
	args = append(args, "--ro", "/proc")
	for _, p := range devices {
		args = append(args, "--rw", p)
	}
	// Not /tmp: the attempt's temporary directory lives inside its workspace,
	// so the container's shared /tmp is simply never granted.
	args = append(args, "--ro", a.Workspace)
	for _, p := range a.Writable {
		args = append(args, "--rw", p)
	}
	args = append(args, "--")
	cmd := exec.Command(self, append(args, argv...)...)
	cmd.Dir = a.Workspace
	return cmd, nil
}

// ExecConfined is the shim half of this rung: it applies the rules named in
// args to this process and execs the rest, replacing itself. It is exported
// for the CLI verb and used by the sandbox tests through the same entry
// point, so the code that confines a real run is the code under test.
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
	// The boot probe builds its throwaway sandbox with no environment at all.
	// execvp answers an unset PATH with a built-in search path, where Go's
	// LookPath simply fails -- so restore the same fallback, capturing the
	// environment first so it does not reach the process being confined.
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

// confine puts this process in a Landlock domain that grants ro and rw and
// nothing else. Requested at ABI v6 -- the level that scopes signals and
// abstract sockets -- and best-effort from there down, so an older kernel
// still gets the filesystem restriction that is the point of the rung. Best
// effort is safe here only because of the check below: without it, a kernel
// with no Landlock at all would degrade silently to enforcing nothing. What
// each ABI gives up on the way down is recorded in SECURITY.md rather than
// guarded here; below v3 the notable loss is the truncate right, so an
// ungranted file can be emptied though never read or written.
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

// grant turns one path into a Landlock rule, or reports that there is nothing
// to grant. A path that does not exist is skipped rather than fatal, the same
// tolerance bubblewrap's --ro-bind-try gives the shared OS list and for the
// same reason: one image's layout should not become a requirement. The rights
// are chosen by what the path actually is, because Landlock rejects a
// directory right on a regular file -- and the ioctl right, which is what
// lets a run interact with a terminal device, only applies to device files.
func grant(path string, write bool) (landlock.Rule, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, false
	}
	if info.IsDir() {
		if write {
			return landlock.RWDirs(path), true
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
