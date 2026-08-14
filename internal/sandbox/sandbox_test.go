package sandbox

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
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

type probeBackend struct{ name string }

func (b probeBackend) Name() string                                { return b.name }
func (probeBackend) Capabilities() Capabilities                    { return Capabilities{} }
func (probeBackend) Command(Attempt, ...string) (*exec.Cmd, error) { return nil, nil }

func TestSandboxProbeResultsAreLoggedTogether(t *testing.T) {
	var logs bytes.Buffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(original) })

	backends := []Backend{probeBackend{"first"}, probeBackend{"second"}}
	selected, err := probeCandidates(backends, func(b Backend) error {
		if b.Name() == "first" {
			return errors.New("not supported here")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if selected.Name() != "second" {
		t.Fatalf("selected %q, want second", selected.Name())
	}
	got := logs.String()
	for _, want := range []string{
		`msg="run sandbox probes complete"`,
		`probes.first=false`,
		`probes.second=true`,
		`selected=second`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in logs:\n%s", want, got)
		}
	}
	if strings.Count(got, "\n") != 1 {
		t.Errorf("expected one probe summary, got:\n%s", got)
	}
}

// TestMain answers the landlock rung's re-exec the way the production binary
// does: that rung confines an attempt by re-entering whatever binary spawned
// it, which under `go test` is this one.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == ShimCommand {
		if err := ExecConfined(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

// Runs f against every rung that can really build a sandbox here, as a subtest
// named for the rung. A property is asserted against each rung that claims it
// and, just as hard, denied against the ones that do not -- a rung quietly
// getting credit for a property it lacks is the failure this file exists to
// prevent. Hosts that can build nothing skip, so CI sets
// OPENROUTINES_REQUIRE_SANDBOX=1 to make a green run mean something.
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

// A path the run was not given is unreachable on every rung; what differs is
// how. Both answers are asserted, because the difference between an absent path
// and a denied one is exactly what the operator is told at boot.
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
		// The supervisor imports and deletes this holding no capability at all,
		// which works only because a run keeps the supervisor's own uid.
		info, err := os.Stat(filepath.Join(staged, "events.md"))
		if err != nil {
			t.Fatal(err)
		}
		if owner := info.Sys().(*syscall.Stat_t).Uid; owner != uint32(os.Getuid()) {
			t.Fatalf("staged file owner = %d, want the supervisor's own uid %d", owner, os.Getuid())
		}
	})
}

// Peer visibility varies most across the ladder, so both answers are pinned. A
// rung that does not hide a peer must still keep everything of value out of
// reach -- if that flips, falling back stops being safe and this test says so.
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
		// Same uid, same container, peer plainly listed -- and still nothing of
		// value: reading another process is a PTRACE_MODE_READ check, which this
		// rung fails across whichever boundary it does have.
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

// Signaling a peer is a denial of service rather than a disclosure, which is
// why it needs pinning: it is the one boundary a rung can lack and still be
// safe to fall back to, so the answer must be told rather than assumed.
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

// A descendant that detaches into its own session outlives a process-group
// kill, so something has to catch it: the kernel on a rung with a pid
// namespace, the runner's own sweep otherwise. That sweep keys on CollapsesTree,
// so the claim is pinned in both directions.
func TestWhetherKillingTheSandboxKillsAnEscapedDescendant(t *testing.T) {
	eachRung(t, func(t *testing.T, b Backend) {
		workspace := t.TempDir()
		marker := filepath.Join(workspace, "alive")
		if err := os.Mkdir(marker, 0o755); err != nil {
			t.Fatal(err)
		}
		beat := filepath.Join(marker, "beat")
		pidfile := filepath.Join(marker, "escapee-pid")
		cmd, err := b.Command(Attempt{Workspace: workspace, Writable: []string{marker}},
			"sh", "-c", "setsid sh -c 'echo $$ > "+pidfile+"; while :; do echo x >> "+beat+"; sleep 0.05; done' & sleep 30")
		if err != nil {
			t.Fatal(err)
		}
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		// The group kill below cannot reach the escapee -- that is the point --
		// so the cleanup reaps it by the pid it recorded. Not on a collapsing
		// rung, where that pid is namespace-local and the kernel's reaping is
		// the thing under test.
		t.Cleanup(func() {
			if b.Capabilities().CollapsesTree {
				return
			}
			raw, err := os.ReadFile(pidfile)
			if err != nil {
				return
			}
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
		})
		waitFor(t, beat)

		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()

		before := size(t, beat)
		time.Sleep(300 * time.Millisecond)
		grew := size(t, beat) != before
		if b.Capabilities().CollapsesTree {
			if grew {
				t.Fatalf("this rung claims to collapse the tree, but a descendant outlived the sandbox and kept writing")
			}
			return
		}
		if !grew {
			t.Skip("nothing outlived the sandbox, so this run proves nothing about a rung that does not claim collapse")
		}
		// An escapee here is expected: the runner sweeps the process group for
		// exactly this case, and the cleanup above reaps this test's own.
	})
}

// /etc is where container platforms mount secret files, and a run shares the
// supervisor's uid, so anything reachable there is readable whatever its mode.
// Asserted from the sandbox's own view, so a new entry on the host does not
// silently become a new entry in every run.
func TestTheSandboxGrantsOnlyTheNamedPartsOfEtc(t *testing.T) {
	eachRung(t, func(t *testing.T, b Backend) {
		if !b.Capabilities().UnnameablePaths {
			// Walking /etc only answers where an ungranted entry is absent; where
			// it is merely denied the listing still shows it, so ask file by file.
			for _, path := range []string{"/etc/shadow", "/etc/secrets", "/etc/ssl/private"} {
				if out, err := run(t, b, Attempt{Workspace: t.TempDir()}, "cat", path); err == nil {
					t.Errorf("%s was readable inside the sandbox: %s", path, out)
				}
			}
			return
		}
		// Two levels deep, so an entry granted for its useful siblings cannot
		// smuggle a directory in with it -- /etc/ssl/private being the case.
		out, err := run(t, b, Attempt{Workspace: t.TempDir()}, "find", "/etc", "-mindepth", "1", "-maxdepth", "2")
		if err != nil {
			t.Fatalf("walking /etc failed: %v: %s", err, out)
		}
		for _, path := range strings.Fields(out) {
			named := false
			for _, entry := range osConfig {
				// Inside something the list names, or a directory that exists
				// only to hold something it names.
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

// A backend grants what a path resolves to, so that is what exposure is about:
// a key reached through a symlink into a granted tree is readable inside the
// sandbox however ungranted its own name looks.
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
// host, so it is refused on every rung -- which is why the check lives on
// Attempt rather than in a backend.
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
// nothing it reports can disagree with it: a rung that was never applied must
// not be credited, and Command must not hand back an unconfined command.
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
