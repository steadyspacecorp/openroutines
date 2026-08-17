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

	"github.com/landlock-lsm/go-landlock/landlock"
)

const envRequireSandbox = "OPENROUTINES_REQUIRE_SANDBOX"

type probeBackend struct{ name string }

func (b probeBackend) Name() string                               { return b.name }
func (probeBackend) Capabilities() Capabilities                   { return Capabilities{} }
func (probeBackend) Command(string, ...string) (*exec.Cmd, error) { return nil, nil }

type commandBackend struct {
	probeBackend
	command func() *exec.Cmd
}

func (b commandBackend) Command(string, ...string) (*exec.Cmd, error) { return b.command(), nil }

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

func TestSandboxProbeReportsWhenEveryCandidateFails(t *testing.T) {
	var logs bytes.Buffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(original) })

	backends := []Backend{probeBackend{"first"}, probeBackend{"second"}}
	selected, err := probeCandidates(backends, func(b Backend) error {
		return fmt.Errorf("%s failed", b.Name())
	})
	if selected != nil {
		t.Fatalf("selected %q when every candidate failed", selected.Name())
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("probe error = %v, want ErrUnavailable", err)
	}
	got := logs.String()
	for _, want := range []string{
		`probes.first=false`,
		`probes.second=false`,
		`selected=none`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in logs:\n%s", want, got)
		}
	}
}

func TestSandboxProbeTimesOut(t *testing.T) {
	b := commandBackend{
		probeBackend: probeBackend{"hung"},
		command: func() *exec.Cmd {
			return exec.Command("sleep", "10")
		},
	}
	started := time.Now()
	err := probeWithin(b, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("probe error = %v, want timeout", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timed-out probe returned after %s", elapsed)
	}
}

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == ShimCommand {
		if err := ExecConfined(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

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

func TestTheSandboxKeepsAPeerAttemptsFilesOutOfReach(t *testing.T) {
	eachRung(t, func(t *testing.T, b Backend) {
		peer := filepath.Join(t.TempDir(), "peer-secret")
		if err := os.WriteFile(peer, []byte("another attempt's credential"), 0o600); err != nil {
			t.Fatal(err)
		}

		out, err := run(t, b, t.TempDir(), "cat", peer)
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

func TestTheWorkspaceIsWritableAndEverythingOutsideItIsNot(t *testing.T) {
	eachRung(t, func(t *testing.T, b Backend) {
		workspace := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside")

		if out, err := run(t, b, workspace, "sh", "-c", "echo changed > "+filepath.Join(workspace, "routine.md")); err != nil {
			t.Fatalf("the run could not write its disposable workspace: %v: %s", err, out)
		}
		if out, err := run(t, b, workspace, "sh", "-c", "echo escaped > "+outside); err == nil {
			t.Fatalf("the run wrote outside its workspace: %s", out)
		}
		if _, err := os.Stat(outside); !os.IsNotExist(err) {
			t.Fatalf("the run left a file outside its workspace: %v", err)
		}
		info, err := os.Stat(filepath.Join(workspace, "routine.md"))
		if err != nil {
			t.Fatal(err)
		}
		if owner := info.Sys().(*syscall.Stat_t).Uid; owner != uint32(os.Getuid()) {
			t.Fatalf("workspace file owner = %d, want the supervisor's own uid %d", owner, os.Getuid())
		}
	})
}

func TestNoVisibleProcessRootEscapesTheSandbox(t *testing.T) {
	eachRung(t, func(t *testing.T, b Backend) {
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(outside, []byte("outside the sandbox"), 0o600); err != nil {
			t.Fatal(err)
		}

		out, err := run(t, b, t.TempDir(), "sh", "-c", `
for root in /proc/[0-9]*/root; do
  if cat "$root$1" 2>/dev/null; then
    exit 0
  fi
done
exit 1
`, "sh", outside)
		if err == nil {
			t.Fatalf("a visible process root exposed a path outside the sandbox: %s", out)
		}
	})
}

func TestWhatTheRunCanLearnAboutAPeerProcess(t *testing.T) {
	eachRung(t, func(t *testing.T, b Backend) {
		peerSecret := filepath.Join(t.TempDir(), "peer-only")
		if err := os.WriteFile(peerSecret, []byte("peer's staged memory"), 0o600); err != nil {
			t.Fatal(err)
		}
		peer := exec.Command("sleep", "30")
		peer.Env = []string{"PATH=/usr/bin:/bin", "PEER_TOKEN=ghp_pretend_credential"} // gitleaks:allow -- a fake token this test tries and fails to read back
		if err := peer.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = peer.Process.Kill(); _ = peer.Wait() }()
		pid := strconv.Itoa(peer.Process.Pid)

		if b.Capabilities().PrivateProcessList {
			out, err := run(t, b, t.TempDir(), "ls", "/proc/"+pid)
			if err == nil {
				t.Fatalf("this rung claims a private process list, but a peer was visible inside it: %s", out)
			}
			return
		}
		for _, probe := range []struct{ what, path string }{
			{"environment", "/proc/" + pid + "/environ"},
			{"memory", "/proc/" + pid + "/mem"},
			{"filesystem", "/proc/" + pid + "/root/"},
		} {
			out, err := run(t, b, t.TempDir(), "cat", probe.path)
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
		if out, err := run(t, b, t.TempDir(), "cat", peerSecret); err == nil {
			t.Fatalf("a peer's staged memory was readable: %s", out)
		}
	})
}

func TestWhetherARunCanSignalAProcessOutsideIt(t *testing.T) {
	eachRung(t, func(t *testing.T, b Backend) {
		peer := exec.Command("sleep", "30")
		if err := peer.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = peer.Process.Kill(); _ = peer.Wait() }()

		out, err := run(t, b, t.TempDir(), "sh", "-c", "kill -0 "+strconv.Itoa(peer.Process.Pid))
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

func TestWhetherKillingTheSandboxKillsAnEscapedDescendant(t *testing.T) {
	eachRung(t, func(t *testing.T, b Backend) {
		workspace := t.TempDir()
		marker := filepath.Join(workspace, "alive")
		if err := os.Mkdir(marker, 0o755); err != nil {
			t.Fatal(err)
		}
		beat := filepath.Join(marker, "beat")
		pidfile := filepath.Join(marker, "escapee-pid")
		cmd, err := b.Command(workspace,
			"sh", "-c", "setsid sh -c 'echo $$ > "+pidfile+"; while :; do echo x >> "+beat+"; sleep 0.05; done' & sleep 30")
		if err != nil {
			t.Fatal(err)
		}
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
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
	})
}

func TestTheSandboxGrantsOnlyTheNamedPartsOfEtc(t *testing.T) {
	eachRung(t, func(t *testing.T, b Backend) {
		if !b.Capabilities().UnnameablePaths {
			for _, path := range []string{"/etc/shadow", "/etc/secrets", "/etc/ssl/private"} {
				if out, err := run(t, b, t.TempDir(), "cat", path); err == nil {
					t.Errorf("%s was readable inside the sandbox: %s", path, out)
				}
			}
			return
		}
		out, err := run(t, b, t.TempDir(), "find", "/etc", "-mindepth", "1", "-maxdepth", "2")
		if err != nil {
			t.Fatalf("walking /etc failed: %v: %s", err, out)
		}
		for _, path := range strings.Fields(out) {
			named := false
			for _, entry := range osConfig {
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
		{"/etc/secrets/master.key", false},
		{"/run/secrets/master.key", false},
		{"/home/agent/.ssh/deploy", false},
		{"/etc/ssl/private/master.key", false},
		{"/etc/ssl/certs/master.key", true},
		{"/etc/ld.so.conf.d/master.key", true},
		{"/usr/local/share/master.key", true},
		{"/usr/../usr/local/master.key", true},
		{"/etc/../usr/local/master.key", true},
	} {
		if got := Exposes(tc.path); got != tc.want {
			t.Errorf("Exposes(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

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

func TestAnInvalidWorkspaceIsRefused(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	for _, b := range candidates() {
		if _, err := b.Command("workspace", "true"); err == nil {
			t.Errorf("%s: a relative workspace was accepted", b.Name())
		}
		if _, err := b.Command(missing, "true"); err == nil {
			t.Errorf("%s: a missing workspace was accepted", b.Name())
		}
	}
}

func TestLandlockGrantsWorkspaceWritable(t *testing.T) {
	workspace := t.TempDir()
	cmd, err := (landlockDomain{}).Command(workspace, "true")
	if err != nil {
		t.Fatal(err)
	}
	if !containsSequence(cmd.Args, "--rw", workspace) {
		t.Fatalf("workspace is not writable in the Landlock command: %q", cmd.Args)
	}
	if containsSequence(cmd.Args, "--ro", workspace) {
		t.Fatalf("workspace is still read-only in the Landlock command: %q", cmd.Args)
	}
}

func TestPTYDirectoryGrantAllowsDeviceIoctls(t *testing.T) {
	got, ok := grant("/dev/pts", true)
	if !ok {
		t.Skip("/dev/pts is unavailable")
	}
	want := landlock.RWDirs("/dev/pts").WithIoctlDev()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("grant = %v, want %v", got, want)
	}
}

func TestDisablingTheSandboxClaimsNothingAndBuildsNothing(t *testing.T) {
	t.Setenv(EnvDisable, "1")

	if got := Provides(); got != (Capabilities{}) {
		t.Errorf("with the sandbox disabled the ladder still claimed %+v", got)
	}
	if _, err := Verify(); !errors.Is(err, ErrDisabled) {
		t.Errorf("Verify should report the hatch rather than a rung: %v", err)
	}
	if _, err := Command(t.TempDir(), "true"); !errors.Is(err, ErrDisabled) {
		t.Errorf("Command should refuse rather than hand back an unconfined command: %v", err)
	}
}

func run(t *testing.T, b Backend, workspace string, argv ...string) (string, error) {
	t.Helper()
	cmd, err := b.Command(workspace, argv...)
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
