// Package sandbox confines a model process to the strongest boundary this host
// permits: a ladder of backends, probed strongest-first at boot, settling on
// the first that can really build a sandbox here.
//
// Bubblewrap is preferred -- a private mount, pid, ipc, uts and user namespace
// per attempt -- but needs a host permissive enough to create unprivileged user
// namespaces (docs/operating.md, "Run confinement"). A Landlock domain is the
// fallback: it denies paths rather than building namespaces, asks the host for
// nothing, and keeps the property that makes falling back safe -- every route
// to a peer's secrets runs through the kernel's ptrace check. It gives up the
// rest, so code that depends on a property asks by name (Capabilities).
//
// Neither gives an attempt a network namespace, and a run keeps the
// supervisor's uid: absence from the sandbox, never a file's mode, is what
// protects the supervisor's secrets.
package sandbox

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// ErrUnavailable reports that no rung could confine a run here. Production
// fails closed on it: an unconfined run reaches every peer's credentials and
// the supervisor's own keys.
var ErrUnavailable = errors.New("no run sandbox is available here")

// EnvDisable is the escape hatch for a host that can build no rung at all.
// Deliberately a deployment decision and never a routine's.
const EnvDisable = "OPENROUTINES_DISABLE_SANDBOX"

// ErrDisabled reports that the hatch is open -- a choice already made, which
// callers must tell apart from "no rung worked".
var ErrDisabled = errors.New("the run sandbox is disabled by " + EnvDisable + "=1")

// Disabled reports whether the hatch is open. Read per call rather than settled
// once: it answers about intent rather than about the host.
func Disabled() bool { return os.Getenv(EnvDisable) == "1" }

// Capabilities is what a backend's confinement actually provides. The zero
// value claims nothing, so a caller with no active backend takes the safe
// branch.
type Capabilities struct {
	// UnnameablePaths: an ungranted path is absent rather than denied --
	// nothing to race, guess, or chmod into reach. Landlock cannot give it.
	UnnameablePaths bool
	// PrivateProcessList: a peer attempt is invisible. Without it, peers are
	// listed and their command lines readable.
	PrivateProcessList bool
	// UnsignalablePeers: an attempt cannot signal a process outside its own
	// confinement. Lacking it is a denial of service between routines, never a
	// disclosure -- a peer's secrets stay behind the ptrace check every rung
	// keeps.
	UnsignalablePeers bool
	// PrivateIPC: its own System V IPC and POSIX message queue namespace.
	PrivateIPC bool
	// PrivateTmp: /tmp and /dev/shm are this attempt's alone.
	PrivateTmp bool
	// CollapsesTree: killing the leader kills every descendant, including one
	// that escaped into its own session. Without it the runner sweeps the
	// process group itself.
	CollapsesTree bool
}

// A Backend is one rung of the ladder: a way to build one run's sandbox,
// together with an honest account of what that sandbox is worth.
type Backend interface {
	Name() string
	Capabilities() Capabilities
	Command(workspace string, argv ...string) (*exec.Cmd, error)
}

// The rungs, strongest first. Probing decides between them because the answer
// depends on masked paths, the seccomp profile, AppArmor policy and the kernel
// at once.
func candidates() []Backend {
	return []Backend{
		bubblewrap{proc: privateProc},
		bubblewrap{proc: privateProc, outerUserNamespace: true},
		bubblewrap{proc: sharedProc},
		bubblewrap{proc: sharedProc, outerUserNamespace: true},
		landlockDomain{},
	}
}

// Reports the rung in force, or why there is none. The hatch is answered
// outside the probe so every answer stays consistent with it: with the sandbox
// off, Provides claims nothing and Command refuses rather than handing back a
// rung that was never applied.
func settle() (Backend, error) {
	if Disabled() {
		return nil, ErrDisabled
	}
	return probeLadder()
}

// Picks the strongest rung that can really build a sandbox here, once per
// process. Every failure is kept in the error: "no sandbox" alone gives whoever
// is debugging the host nothing to act on.
var probeLadder = sync.OnceValues(func() (Backend, error) {
	return probeCandidates(candidates(), probe)
})

func probeCandidates(backends []Backend, runProbe func(Backend) error) (Backend, error) {
	rejected := []error{ErrUnavailable}
	results := make([]slog.Attr, 0, len(backends))
	for _, b := range backends {
		err := runProbe(b)
		if err == nil {
			results = append(results, slog.Bool(b.Name(), true))
			logProbeResults(results, b)
			return b, nil
		}
		results = append(results, slog.Bool(b.Name(), false))
		rejected = append(rejected, fmt.Errorf("%s: %w", b.Name(), err))
	}
	logProbeResults(results, nil)
	return nil, errors.Join(rejected...)
}

func logProbeResults(results []slog.Attr, selected Backend) {
	args := []any{slog.GroupAttrs("probes", results...)}
	if selected == nil {
		args = append(args, "selected", "none")
	} else {
		args = append(args, "selected", selected.Name())
	}
	slog.Info("run sandbox probes complete", args...)
}

// Verify reports which rung confines runs here, building a throwaway sandbox
// exactly as attempts will so the fail-closed policy fires at boot rather than
// mid-run.
func Verify() (Backend, error) { return settle() }

// Provides reports what the active rung's confinement is worth, and the zero
// value when there is none -- so keying behavior on a property gets the
// conservative branch rather than an assumption about a sandbox that is absent.
func Provides() Capabilities {
	if b, _ := settle(); b != nil {
		return b.Capabilities()
	}
	return Capabilities{}
}

// Command wraps argv and its workspace in the active sandbox rung.
func Command(workspace string, argv ...string) (*exec.Cmd, error) {
	b, err := settle()
	if err != nil {
		return nil, err
	}
	return b.Command(workspace, argv...)
}

// The operating system an attempt gets: one shared policy each backend
// expresses in its own vocabulary, which is what keeps Exposes correct on every
// rung. A path that does not exist is skipped rather than failing the sandbox.
var readOnlyOS = []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/opt"}

// The part of /etc an attempt gets, named entry by entry rather than granted
// whole: /etc is where container platforms deliver mounted secrets, and a run
// shares the supervisor's uid, so a granted 0600 key file is a readable one.
var osConfig = []string{
	"/etc/ld.so.cache", "/etc/ld.so.conf", "/etc/ld.so.conf.d",
	// Trust roots only, not `/etc/ssl` whole: Debian keeps `/etc/ssl/private`
	// there.
	"/etc/ssl/certs", "/etc/ssl/openssl.cnf", "/etc/ca-certificates.conf",
	"/etc/resolv.conf", "/etc/hosts", "/etc/host.conf", "/etc/nsswitch.conf", "/etc/gai.conf",
	// Not in the shipped image, bound if a base image has them: libc resolves
	// a named service like "https" through /etc/services.
	"/etc/services", "/etc/protocols",
	"/etc/passwd", "/etc/group",
	"/etc/gitconfig",
	"/etc/localtime", "/etc/timezone",
	"/etc/alternatives", "/etc/os-release",
}

// Exposes reports whether a host path would be readable from inside a sandbox
// -- a property of the two lists above rather than of the file's mode, which is
// why the supervisor asks it of its own key files at boot. The answer is the
// same on every rung: a weaker one reads no more of the host, it just protects
// the rest by denial rather than by absence.
func Exposes(path string) bool {
	target := resolve(path)
	for _, root := range slices.Concat(readOnlyOS, osConfig) {
		if within(target, resolve(root)) {
			return true
		}
	}
	return false
}

// Puts a path in the form the kernel will bind: absolute, every symlink
// followed. Both sides need it -- a key reached through a symlink into a bound
// tree is exposed, and a bound entry that is itself a symlink exposes its
// target. EvalSymlinks refuses a leaf that does not exist, the common case here
// (an unmounted key file), so resolve the deepest ancestor that does exist and
// re-attach the rest.
func resolve(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	path = filepath.Clean(path)
	for rest := ""; ; {
		if target, err := filepath.EvalSymlinks(path); err == nil {
			return filepath.Join(target, rest)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return filepath.Join(path, rest)
		}
		rest, path = filepath.Join(filepath.Base(path), rest), parent
	}
}

// Reports whether path is root or lies beneath it, lexically. Callers resolve
// first: comparing unresolved paths answers a question about names rather than
// about what the kernel will open.
func within(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// Absolute is required rather than inferred because this process and the
// sandbox helper resolve a relative workspace against different directories.
func validateWorkspace(workspace string) error {
	if !filepath.IsAbs(workspace) {
		return fmt.Errorf("attempt workspace must be absolute: %s", workspace)
	}
	if _, err := os.Stat(workspace); err != nil {
		return fmt.Errorf("attempt workspace %s: %w", workspace, err)
	}
	return nil
}

// Builds one throwaway sandbox on the given rung. The helpers explain their own
// failures well, so their output is the error.
func probe(b Backend) error {
	dir, err := os.MkdirTemp("", "openroutines-sandbox-probe-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	scratch := filepath.Join(dir, "probe")
	cmd, err := b.Command(dir, "sh", "-c", `echo writable > "$1"`, "sh", scratch)
	if err != nil {
		return err
	}
	cmd.Env = []string{}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if len(out) == 0 {
		return err
	}
	return errors.New(strings.TrimSpace(string(out)))
}
