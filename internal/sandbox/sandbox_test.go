package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const envRequireSandbox = "OPENROUTINES_REQUIRE_SANDBOX"

// TestMain answers the landlock rung's re-exec the way the production binary
// does. That rung confines an attempt by re-entering whatever binary spawned
// it, which under `go test` is this one -- so without this the rung could
// only be exercised through the CLI, and the code that confines a real run
// would not be the code under test.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == ShimCommand {
		if err := ExecConfined(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

// eachRung runs f against every rung that can really build a sandbox on this
// host, as a subtest named for the rung. The rungs are not interchangeable,
// so a property is asserted against each one that claims it -- and, for the
// ones that do not, the opposite is asserted just as hard. A rung quietly
// getting credit for a property it lacks is the failure this file exists to
// prevent.
//
// Hosts that can build nothing skip, so a contributor on macOS can still run
// the suite. That makes the whole file capable of going quiet, so set
// OPENROUTINES_REQUIRE_SANDBOX=1 where a green run is supposed to mean
// something (CI does) and a host with no rung fails instead of saying
// nothing.
func eachRung(t *testing.T, f func(t *testing.T, b Backend)) {
	t.Helper()
	var available []Backend
	var why []string
	for _, b := range candidates() {
		if err := probe(b); err != nil {
			why = append(why, b.Name()+": "+err.Error())
			continue
		}
		available = append(available, b)
	}
	if len(available) == 0 {
		reason := strings.Join(why, "\n\t")
		if os.Getenv(envRequireSandbox) == "1" {
			t.Fatalf("%s=1 but no rung could build a sandbox:\n\t%s", envRequireSandbox, reason)
		}
		t.Skipf("no sandbox rung is available here:\n\t%s", reason)
	}
	for _, b := range available {
		t.Run(b.Name(), func(t *testing.T) { f(t, b) })
	}
}

// A path the run was not given is unreachable on every rung. What differs is
// how: a rung with UnnameablePaths does not have the path at all, so there is
// nothing to race, guess, or chmod into reach, while a path-denial rung
// leaves the name visible and refuses the open. Both are asserted, because
// the difference is exactly what the operator is told at boot.
func TestTheSandboxKeepsAPeerAttemptsFilesOutOfReach(t *testing.T) {
	eachRung(t, func(t *testing.T, b Backend) {
		peer := filepath.Join(t.TempDir(), "peer-secret")
		if err := os.WriteFile(peer, []byte("another attempt's credential"), 0o600); err != nil {
			t.Fatal(err)
		}

		out, err := run(t, b, Attempt{Workspace: t.TempDir()}, "cat", peer)
		if err == nil {
			t.Fatalf("a peer attempt's file was readable from inside the sandbox: %s", out)
		}
		if b.Capabilities().UnnameablePaths {
			if !strings.Contains(out, "No such file") {
				t.Fatalf("this rung claims unnameable paths, but the peer file failed for another reason: %s", out)
			}
			return
		}
		if strings.Contains(out, "No such file") {
			t.Fatalf("this rung does not claim unnameable paths, yet the peer file was absent rather than denied -- Capabilities understates it: %s", out)
		}
		if !strings.Contains(out, "denied") {
			t.Fatalf("peer file failed for the wrong reason: %s", out)
		}
	})
}

func TestTheWorkspaceIsReadOnlyExceptWhereTheRunMayWrite(t *testing.T) {
	eachRung(t, func(t *testing.T, b Backend) {
		workspace := t.TempDir()
		staged := filepath.Join(workspace, "memory")
		if err := os.Mkdir(staged, 0o755); err != nil {
			t.Fatal(err)
		}
		spec := Attempt{Workspace: workspace, Writable: []string{staged}}

		if out, err := run(t, b, spec, "sh", "-c", "echo tampered > "+filepath.Join(workspace, "routine.md")); err == nil {
			t.Fatalf("the run rewrote the routine it was staged from: %s", out)
		}
		if out, err := run(t, b, spec, "sh", "-c", "echo settled > "+filepath.Join(staged, "events.md")); err != nil {
			t.Fatalf("the run could not write its own staged memory: %v: %s", err, out)
		}
		// The supervisor imports and deletes this afterwards holding no
		// capability of any kind, which only works because the sandboxed
		// process keeps the supervisor's own uid on every rung.
		info, err := os.Stat(filepath.Join(staged, "events.md"))
		if err != nil {
			t.Fatal(err)
		}
		if owner := info.Sys().(*syscall.Stat_t).Uid; owner != uint32(os.Getuid()) {
			t.Fatalf("staged file owner = %d, want the supervisor's own uid %d", owner, os.Getuid())
		}
	})
}

// Whether a peer attempt is visible at all is the property that varies most
// across the ladder, so both answers are pinned. A rung that claims privacy
// must hide the peer; a rung that does not must still keep everything of
// value out of reach, which is what makes it safe to fall back to
// automatically. If that ever flips, the fallback stops being safe and this
// test is what says so.
func TestWhatTheRunCanLearnAboutAPeerProcess(t *testing.T) {
	eachRung(t, func(t *testing.T, b Backend) {
		peerSecret := filepath.Join(t.TempDir(), "peer-only")
		if err := os.WriteFile(peerSecret, []byte("peer's staged memory"), 0o600); err != nil {
			t.Fatal(err)
		}
		// A peer attempt outliving this call, with its own credential in its
		// environment and its own file on disk.
		peer := exec.Command("sleep", "30")
		peer.Env = []string{"PATH=/usr/bin:/bin", "PEER_TOKEN=ghp_pretend_credential"} // gitleaks:allow -- a fake token this test tries and fails to read back
		if err := peer.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = peer.Process.Kill(); _ = peer.Wait() }()
		pid := strconv.Itoa(peer.Process.Pid)

		if b.Capabilities().PrivateProcessList {
			out, err := run(t, b, Attempt{Workspace: t.TempDir()}, "ls", "/proc/"+pid)
			if err == nil {
				t.Fatalf("this rung claims a private process list, but a peer was visible inside it: %s", out)
			}
			return
		}
		// Same uid, same container, peer plainly listed -- and still nothing
		// of value. Reading another process's memory is a PTRACE_MODE_READ
		// check, which this rung fails across whichever boundary it does have.
		for _, probe := range []struct{ what, path string }{
			{"environment", "/proc/" + pid + "/environ"},
			{"memory", "/proc/" + pid + "/mem"},
			{"filesystem", "/proc/" + pid + "/root/"},
		} {
			out, err := run(t, b, Attempt{Workspace: t.TempDir()}, "cat", probe.path)
			if err == nil {
				t.Fatalf("a peer's %s was readable on this rung: %s", probe.what, out)
			}
			if strings.Contains(out, "ghp_pretend_credential") {
				t.Fatalf("a peer's %s leaked its credential: %s", probe.what, out)
			}
			if !strings.Contains(out, "denied") && !strings.Contains(out, "No such") {
				t.Fatalf("peer %s failed for the wrong reason: %s", probe.what, out)
			}
		}
		if out, err := run(t, b, Attempt{Workspace: t.TempDir()}, "cat", peerSecret); err == nil {
			t.Fatalf("a peer's staged memory was readable: %s", out)
		}
	})
}

// Signaling a peer is a denial of service between routines rather than a
// disclosure, which is exactly why it needs pinning: it is the one boundary a
// rung can lack while still being safe to fall back to, so an operator has to
// be told which answer they got instead of it being assumed either way.
func TestWhetherARunCanSignalAProcessOutsideIt(t *testing.T) {
	eachRung(t, func(t *testing.T, b Backend) {
		peer := exec.Command("sleep", "30")
		if err := peer.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = peer.Process.Kill(); _ = peer.Wait() }()

		// Signal 0 asks the kernel the permission question without delivering
		// anything, which is the question this test is about.
		out, err := run(t, b, Attempt{Workspace: t.TempDir()}, "sh", "-c", "kill -0 "+strconv.Itoa(peer.Process.Pid))
		if b.Capabilities().UnsignalablePeers {
			if err == nil {
				t.Fatalf("this rung claims peers cannot be signaled, but a process outside the sandbox was reachable: %s", out)
			}
			return
		}
		if err != nil {
			t.Skipf("a peer was unreachable on a rung that does not claim to isolate signals, so nothing here is proven: %s", out)
		}
	})
}

// A descendant that detaches into a session of its own outlives a process-group
// kill, so something has to catch it. On a rung with a pid namespace the kernel
// does; on one without, nothing here does and the runner must sweep -- which is
// exactly what the runner's reap keys CollapsesTree on, so the claim is pinned
// in both directions rather than assumed.
func TestWhetherKillingTheSandboxKillsAnEscapedDescendant(t *testing.T) {
	eachRung(t, func(t *testing.T, b Backend) {
		workspace := t.TempDir()
		marker := filepath.Join(workspace, "alive")
		if err := os.Mkdir(marker, 0o755); err != nil {
			t.Fatal(err)
		}
		beat := filepath.Join(marker, "beat")
		cmd, err := b.Command(Attempt{Workspace: workspace, Writable: []string{marker}},
			"sh", "-c", "setsid sh -c 'while :; do echo x >> "+beat+"; sleep 0.05; done' & sleep 30")
		if err != nil {
			t.Fatal(err)
		}
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		waitFor(t, beat)

		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()

		before := size(t, beat)
		time.Sleep(300 * time.Millisecond)
		grew := size(t, beat) != before
		if b.Capabilities().CollapsesTree {
			if grew {
				// The pid namespace should have died with its init, taking the
				// escaped descendant with it by the kernel's doing.
				t.Fatalf("this rung claims to collapse the tree, but a descendant outlived the sandbox and kept writing")
			}
			return
		}
		if !grew {
			t.Skip("nothing outlived the sandbox, so this run proves nothing about a rung that does not claim collapse")
		}
		// Documented, not lamented: the runner sweeps the process group for
		// exactly this case. Clean up what the kernel did not.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	})
}

// /etc is where container platforms mount secret files, and a run shares the
// supervisor's uid, so anything reachable there is readable whatever its mode. The policy therefore names
// the /etc entries a run gets, and this asserts the sandbox's own view: what
// a run can reach in /etc is exactly what was asked for, so a new entry on
// the host does not silently become a new entry in every run.
func TestTheSandboxGrantsOnlyTheNamedPartsOfEtc(t *testing.T) {
	eachRung(t, func(t *testing.T, b Backend) {
		if !b.Capabilities().UnnameablePaths {
			// Walking /etc answers the question only where an ungranted entry
			// is absent. Where it is merely denied the listing still shows it,
			// so the reachable-or-not question is asked file by file instead.
			for _, path := range []string{"/etc/shadow", "/etc/secrets", "/etc/ssl/private"} {
				if out, err := run(t, b, Attempt{Workspace: t.TempDir()}, "cat", path); err == nil {
					t.Errorf("%s was readable inside the sandbox: %s", path, out)
				}
			}
			return
		}
		// Two levels deep, so an entry granted for its useful siblings cannot
		// smuggle a directory in with it -- /etc/ssl/private is the case in point.
		out, err := run(t, b, Attempt{Workspace: t.TempDir()}, "find", "/etc", "-mindepth", "1", "-maxdepth", "2")
		if err != nil {
			t.Fatalf("walking /etc failed: %v: %s", err, out)
		}
		for _, path := range strings.Fields(out) {
			named := false
			for _, entry := range osConfig {
				// Either inside something the list names, or a directory that
				// exists only to hold something it names.
				if within(path, entry) || within(entry, path) {
					named = true
					break
				}
			}
			if !named {
				t.Errorf("%s is reachable inside the sandbox but is not on the allow-list", path)
			}
		}
	})
}

func TestASupervisorKeyUnderAGrantedPathIsReportedAsExposed(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/etc/secrets/master.key", false}, // where a platform mounts one
		{"/run/secrets/master.key", false},
		{"/home/agent/.ssh/deploy", false},
		{"/etc/ssl/private/master.key", false}, // /etc/ssl is not granted whole
		{"/etc/ssl/certs/master.key", true},    // the granted entry under it is
		{"/etc/ld.so.conf.d/master.key", true}, // and another
		{"/usr/local/share/master.key", true},  //
		{"/usr/../usr/local/master.key", true}, // cleaned before comparing
		{"/etc/../usr/local/master.key", true}, // and not fooled by the prefix
	} {
		if got := Exposes(tc.path); got != tc.want {
			t.Errorf("Exposes(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// A backend grants what a path resolves to, so that is what exposure is
// about: a key reached through a symlink into a granted tree is readable
// inside the sandbox however ungranted its own name looks, and a relative
// path names whatever it resolves to rather than nothing.
func TestExposureFollowsSymlinksAndRelativePaths(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "into-the-os")
	if err := os.Symlink("/usr/local", link); err != nil {
		t.Fatal(err)
	}
	if !Exposes(filepath.Join(link, "master.key")) {
		t.Errorf("a key reached through a symlink into a granted tree was reported unexposed")
	}

	away := filepath.Join(dir, "elsewhere")
	if err := os.Symlink(t.TempDir(), away); err != nil {
		t.Fatal(err)
	}
	if Exposes(filepath.Join(away, "master.key")) {
		t.Errorf("a symlink to an ungranted directory was reported exposed")
	}

	t.Chdir("/usr/local")
	if !Exposes("share/master.key") {
		t.Errorf("a relative path inside a granted tree was reported unexposed")
	}
}

// A writable grant is the one argument in an Attempt that can hand a run the
// host, so the package that owns the policy refuses it -- on every rung,
// which is why the check lives on Attempt rather than in a backend.
func TestAWritablePathOutsideTheWorkspaceIsRefused(t *testing.T) {
	workspace := t.TempDir()
	for _, b := range candidates() {
		if _, err := b.Command(Attempt{Workspace: workspace, Writable: []string{"/etc"}}, "true"); err == nil {
			t.Errorf("%s: a writable grant outside the workspace was accepted", b.Name())
		}
		if _, err := b.Command(Attempt{Workspace: workspace, Writable: []string{"memory"}}, "true"); err == nil {
			t.Errorf("%s: a relative writable grant was accepted, though the backend would resolve it differently", b.Name())
		}
	}

	// The name is inside the workspace; what it opens is not. A backend grants
	// the target, so a check on the name alone would expose an arbitrary tree.
	escape := filepath.Join(workspace, "memory")
	if err := os.Symlink(t.TempDir(), escape); err != nil {
		t.Fatal(err)
	}
	if _, err := Command(Attempt{Workspace: workspace, Writable: []string{escape}}, "true"); err == nil {
		t.Fatal("a writable symlink out of the workspace was accepted")
	}

	staged := filepath.Join(workspace, "staged")
	if err := os.Mkdir(staged, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Command(Attempt{Workspace: workspace, Writable: []string{staged}}, "true"); err != nil && !strings.Contains(err.Error(), ErrUnavailable.Error()) {
		t.Fatalf("a writable path inside the workspace was refused: %v", err)
	}
}

// The hatch is answered inside this package rather than at each caller, so
// that nothing it reports can disagree with it: a rung that was never applied
// must not be credited with confining anything, and a command built here must
// never be an unconfined one wearing a sandbox's name.
func TestDisablingTheSandboxClaimsNothingAndBuildsNothing(t *testing.T) {
	t.Setenv(EnvDisable, "1")

	if got := Provides(); got != (Capabilities{}) {
		t.Errorf("with the sandbox disabled the ladder still claimed %+v", got)
	}
	if _, err := Verify(); !errors.Is(err, ErrDisabled) {
		t.Errorf("Verify should report the hatch rather than a rung: %v", err)
	}
	if _, err := Command(Attempt{Workspace: t.TempDir()}, "true"); !errors.Is(err, ErrDisabled) {
		t.Errorf("Command should refuse rather than hand back an unconfined command: %v", err)
	}
}

func run(t *testing.T, b Backend, a Attempt, argv ...string) (string, error) {
	t.Helper()
	cmd, err := b.Command(a, argv...)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func waitFor(t *testing.T, path string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return
		}
	}
	t.Fatalf("%s never appeared", path)
}

func size(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}
