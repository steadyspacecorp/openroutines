// Package sandbox confines a model process to the strongest boundary this
// host actually permits.
//
// Ways to confine a run are not equally strong and not equally available, so
// this is a ladder rather than a single mechanism: it probes backends
// strongest-first at boot and settles on the first that can really build a
// sandbox here, rather than requiring one and refusing every host that
// cannot provide it. Where nothing works, the supervisor does not start.
//
// Bubblewrap is the preferred rung: a private mount, pid, ipc, uts and user
// namespace per attempt, so a peer attempt is not a process this one can see
// and not a path it can name. It needs a container runtime and host
// permissive enough to create unprivileged user namespaces -- see
// docs/operating.md, "Run confinement", for the three separate things that
// can deny that.
//
// A Landlock domain is the fallback, and the two are complementary rather
// than nested: it confines by denying paths instead of by building
// namespaces, and asks the host for nothing whatsoever -- no runtime flag,
// no capability, no sysctl, no privilege -- so it runs on hosts that refuse
// bubblewrap outright and on platforms that take a Dockerfile and nothing
// else. It keeps the property that makes falling back safe: one attempt
// still cannot read another's or the supervisor's secrets, because every
// route to them runs through the kernel's ptrace check, which Landlock
// hooks. It gives up everything else a namespace would have provided --
// paths are denied rather than absent, peers are listed, and nothing
// collapses the process tree. Because the two are not interchangeable, code
// that depends on a property asks for it by name (Capabilities) rather than
// assuming the top rung.
//
// What neither gives an attempt is a network namespace of its own: attempts
// share the container's, so they share its localhost ports and abstract
// socket namespace. The model process also keeps the supervisor's uid, which
// is what lets the supervisor import staged knowledge and delete the workspace
// afterwards with no ownership handover in either direction -- and is why
// absence from the sandbox, never a file's mode, is what protects the
// supervisor's secrets.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// ErrUnavailable reports that no rung of the ladder could confine a run
// here. Production is fail-closed on it, because a run with no sandbox at all
// reaches every other run's credentials and the supervisor's own keys.
var ErrUnavailable = errors.New("no run sandbox is available here")

// EnvDisable is the escape hatch: an operator who cannot get any rung working
// on their host, and would rather run unconfined than not at all, sets it to
// 1. It is deliberately a deployment decision and not a routine's -- see
// docs/design.md, "Confinement is best effort, and the operator owns the
// tradeoff".
const EnvDisable = "OPENROUTINES_DISABLE_SANDBOX"

// ErrDisabled reports that the hatch is open, which every caller has to tell
// apart from "no rung worked": one is a choice already made, the other is a
// refusal to start.
var ErrDisabled = errors.New("the run sandbox is disabled by " + EnvDisable + "=1")

// Disabled reports whether the hatch is open. Read per call rather than
// settled once, because unlike the ladder it is an answer about intent rather
// than about the host.
func Disabled() bool { return os.Getenv(EnvDisable) == "1" }

// Capabilities is what a backend's confinement actually provides. The zero
// value claims nothing, which is the answer that makes callers do the safe
// thing when no backend is active.
type Capabilities struct {
	// UnnameablePaths means an ungranted path is absent from the run's view
	// rather than merely denied -- nothing to race, guess, or chmod into
	// reach. A path-denial mechanism like Landlock does not provide this.
	UnnameablePaths bool
	// PrivateProcessList means a peer attempt is not visible at all. Without
	// it, peers are listed and their command lines readable.
	PrivateProcessList bool
	// UnsignalablePeers means an attempt cannot signal a process outside its
	// own confinement. Without it a routine can kill a peer attempt or the
	// supervisor -- a denial of service between routines, never a disclosure:
	// what protects a peer's secrets is the ptrace check, which holds on
	// every rung.
	UnsignalablePeers bool
	// PrivateIPC means the attempt gets its own System V IPC and POSIX
	// message queue namespace.
	PrivateIPC bool
	// PrivateTmp means /tmp and /dev/shm are this attempt's alone, rather
	// than directories shared with every other run in the container.
	PrivateTmp bool
	// CollapsesTree means killing the leader kills every descendant,
	// including one that escaped into a session of its own -- the property a
	// pid namespace gives for free. Without it the runner must sweep the
	// process group itself.
	CollapsesTree bool
}

// A Backend is one rung of the ladder: a way to build the sandbox an Attempt
// describes, together with an honest account of what that sandbox is worth.
type Backend interface {
	// Name identifies the rung in operator-facing output.
	Name() string
	// Capabilities reports what this rung's confinement actually provides.
	Capabilities() Capabilities
	// Command wraps argv in the sandbox a describes.
	Command(a Attempt, argv ...string) (*exec.Cmd, error)
}

// candidates are the rungs, strongest first. Probing decides between them,
// because the answer depends on the runtime's masked paths, its seccomp
// profile, its AppArmor policy and the kernel at once -- and a sandbox that
// builds is the only claim worth making.
func candidates() []Backend {
	return []Backend{
		bubblewrap{proc: privateProc},
		bubblewrap{proc: sharedProc},
		landlockDomain{},
	}
}

// settle reports the rung in force, or why there is none. The hatch is
// checked outside the probe so that every answer this package gives is
// consistent with it: with the sandbox off, Provides must claim nothing and
// Command must refuse rather than quietly hand back a rung that was never
// applied.
func settle() (Backend, error) {
	if Disabled() {
		return nil, ErrDisabled
	}
	return probeLadder()
}

// probeLadder picks the strongest rung that can really build a sandbox here,
// once per process. Every rung that failed is kept in the error, because an
// operator debugging a refusal needs to know what was tried and why each one
// was rejected -- "no sandbox" alone is not actionable.
var probeLadder = sync.OnceValues(func() (Backend, error) {
	rejected := []error{ErrUnavailable}
	for _, b := range candidates() {
		err := probe(b)
		if err == nil {
			return b, nil
		}
		rejected = append(rejected, fmt.Errorf("%s: %w", b.Name(), err))
	}
	return nil, errors.Join(rejected...)
})

// Verify reports which rung confines runs here, by building a throwaway
// sandbox exactly as attempts will. Called once at boot so the fail-closed
// policy fires before the first run rather than during it.
func Verify() (Backend, error) { return settle() }

// Active is the settled rung, or nil where nothing works. Callers that only
// need to know what the boundary is worth should ask Capabilities instead.
func Active() Backend {
	b, _ := settle()
	return b
}

// Provides reports what the active rung's confinement is worth. With no
// active rung this is the zero value -- claims nothing -- so a caller that
// keys behavior on a property gets the conservative branch rather than an
// assumption about a sandbox that does not exist.
func Provides() Capabilities {
	if b := Active(); b != nil {
		return b.Capabilities()
	}
	return Capabilities{}
}

// Command wraps argv in the sandbox a describes, built by the active rung.
func Command(a Attempt, argv ...string) (*exec.Cmd, error) {
	b, err := settle()
	if err != nil {
		return nil, err
	}
	return b.Command(a, argv...)
}

// readOnlyOS and osConfig are the operating system an attempt gets, and they
// are the one shared policy every backend expresses in its own vocabulary --
// bubblewrap's `--ro-bind-try`, Landlock's read-only rules. Keeping the list
// here rather than per backend is what keeps Exposes correct on every rung
// without duplicating the security-critical part.
//
// Everything outside these and the attempt's own paths is out of reach --
// absent on a rung with UnnameablePaths, denied on one without. Paths that do
// not exist are skipped rather than failing the sandbox, so one image layout
// does not become a hard requirement.
var readOnlyOS = []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/opt"}

// osConfig is the part of /etc an attempt gets: the dynamic linker's search
// path, trust roots, name resolution, uid-to-name lookup, and git's system
// config. /etc is named entry by entry rather than granted whole because it
// is where container platforms deliver mounted secrets, and a run shares the
// supervisor's uid, so a granted 0600 key file is a readable one. This is the
// same allow-list posture as the run workspace, chosen for the same reason: a
// deny-list is one missed entry away from being wrong, and nobody finds the
// missed entry until it matters.
var osConfig = []string{
	"/etc/ld.so.cache", "/etc/ld.so.conf", "/etc/ld.so.conf.d",
	// Trust roots only. Not `/etc/ssl` whole: Debian keeps `/etc/ssl/private`
	// there, and binding a directory whose name says "private" because its
	// siblings are useful is the shape of mistake this list exists to avoid.
	"/etc/ssl/certs", "/etc/ssl/openssl.cnf", "/etc/ca-certificates.conf",
	"/etc/resolv.conf", "/etc/hosts", "/etc/host.conf", "/etc/nsswitch.conf", "/etc/gai.conf",
	// Absent from the shipped image (they come with netbase) but bound if a
	// base image has them: libc resolves a named service like "https" through
	// /etc/services, and nothing else would explain the failure.
	"/etc/services", "/etc/protocols",
	"/etc/passwd", "/etc/group",
	"/etc/gitconfig",
	"/etc/localtime", "/etc/timezone",
	"/etc/alternatives", "/etc/os-release",
}

// Exposes reports whether a host path would be readable from inside a
// sandbox. Absence is what protects the supervisor's secrets now that runs
// share its uid, and absence is a property of the two lists above rather
// than of the file's mode -- so the supervisor asks this of its own key
// files at boot instead of trusting that nobody mounted one somewhere the
// OS binds reach. The answer is the same on every rung, which is the point
// of keeping one list: a weaker rung reads no more of the host than a
// stronger one, it just protects the rest by denial rather than by absence.
func Exposes(path string) bool {
	target := resolve(path)
	for _, root := range slices.Concat(readOnlyOS, osConfig) {
		if within(target, resolve(root)) {
			return true
		}
	}
	return false
}

// resolve puts a path in the form the kernel will bind: absolute, with every
// symlink followed. Both sides need it, and for opposite reasons -- a key
// reached through a symlink into a bound tree is exposed, and a bound entry
// that is itself a symlink exposes its target rather than its own name.
//
// EvalSymlinks refuses a path whose leaf does not exist, which is the common
// case here: a key file the deployment has not mounted, a writable directory
// not created yet. Falling back to the lexical path would then leave one
// side of a comparison resolved and the other not, and two names for the
// same file compare unequal. So resolve the deepest ancestor that does exist
// and re-attach the rest.
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

// within reports whether path is root or lies beneath it, comparing the two
// lexically. Callers pass paths through resolve first; comparing unresolved
// paths answers a question about names rather than about what the kernel
// will open.
func within(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// An Attempt is everything one model process may reach. Every mount into a
// sandbox is potential privilege escalation (bubblewrap's own README), so
// this is security-critical policy and should stay as short as it reads.
type Attempt struct {
	// Workspace is the attempt's staged tree, mounted read-only.
	Workspace string
	// Writable are the paths the attempt may mutate. Each must live inside
	// Workspace -- resolved rejects one that does not, because a writable
	// bind is the one argument here that can hand a run the host.
	Writable []string
}

// validate checks that every writable grant really lands inside the
// workspace. Resolved, not lexical: a backend confines what a path resolves
// to, so a writable entry that is a symlink out of the workspace would
// otherwise pass a check on its name and be granted anyway. Absolute is
// required rather than inferred because the two sides would disagree -- this
// process resolves a relative path against its own directory, and the
// sandbox helper against the workspace it is told to work in.
//
// The resolved form is deliberately not returned: a backend must name paths
// to the run exactly as the runner named them here, because the run was told
// to use those names. Resolving is for the check, not for the argv.
//
// This is shared policy rather than a backend's business: the rungs differ
// in what they enforce, never in what an attempt is allowed to ask for.
func (a Attempt) validate() error {
	workspace := resolve(a.Workspace)
	for _, p := range a.Writable {
		if !filepath.IsAbs(p) || !filepath.IsAbs(a.Workspace) {
			return fmt.Errorf("attempt paths must be absolute: workspace %s, writable %s", a.Workspace, p)
		}
		if !within(resolve(p), workspace) {
			return fmt.Errorf("writable path %s is outside the attempt workspace %s", p, a.Workspace)
		}
	}
	return nil
}

// probe builds one throwaway sandbox on the given rung and reports what went
// wrong, if anything. The helpers explain their own failures well, so their
// output is the error.
func probe(b Backend) error {
	dir, err := os.MkdirTemp("", "openroutines-sandbox-probe-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	scratch := filepath.Join(dir, "rw")
	if err := os.Mkdir(scratch, 0o755); err != nil {
		return err
	}
	cmd, err := b.Command(Attempt{Workspace: dir, Writable: []string{scratch}}, "true")
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
