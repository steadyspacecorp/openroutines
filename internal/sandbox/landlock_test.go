//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLandlockConfinement verifies the production rule set on a real kernel.
// This is the test the security audit asked for first: the run workspace
// must not be writable (it lives inside /tmp, and Landlock rules are
// additive -- a blanket /tmp grant would open it), the supervisor's environ
// must not be readable, and the shared-home leak paths must be closed.
//
// The rules are applied in a helper child process (Landlock binds to the
// caller and everything it spawns -- applying them here would confine the
// test binary itself). The parent plays the supervisor: non-dumpable, with
// a canary in its environment.
func TestLandlockConfinement(t *testing.T) {
	if os.Getenv("LANDLOCK_TEST_HELPER") == "1" {
		landlockHelper()
		return // unreachable; helper always exits
	}

	// The supervisor's boot protection, applied to this process so the
	// helper's environ probe tests the real mechanism.
	if err := ProtectProcess(); err != nil {
		t.Fatalf("ProtectProcess: %v", err)
	}

	// Production layout: workspace inside the real /tmp, exactly as
	// os.MkdirTemp places it in the runner.
	workspace, err := os.MkdirTemp("", "openroutines-run-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(workspace) })
	runTmp := filepath.Join(workspace, ".runtmp")
	attemptHome := filepath.Join(workspace, ".home")
	memoryDir := filepath.Join(workspace, "memory")
	for _, d := range []string{runTmp, attemptHome, memoryDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	seeded := filepath.Join(workspace, "openroutines.yaml")
	if err := os.WriteFile(seeded, []byte("name: t\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The supervisor's home: deploy key and an opencode auth store, neither
	// of which any model process may read.
	home := t.TempDir()
	deployKey := filepath.Join(home, ".ssh", "openroutines_deploy")
	authStore := filepath.Join(home, ".local", "share", "opencode", "auth.json")
	for _, f := range []string{deployKey, authStore} {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(home, ".opencode"), 0o755); err != nil {
		t.Fatal(err)
	}

	ro, rw := Paths(workspace, memoryDir, runTmp, home, attemptHome)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run", "TestLandlockConfinement")
	cmd.Env = append(os.Environ(),
		"LANDLOCK_TEST_HELPER=1",
		"LANDLOCK_TEST_CANARY=squeak",
		EnvRO+"="+JoinPaths(ro),
		EnvRW+"="+JoinPaths(rw),
		"LT_WORKSPACE="+workspace,
		"LT_RUNTMP="+runTmp,
		"LT_HOME_ATTEMPT="+attemptHome,
		"LT_SEEDED="+seeded,
		"LT_DEPLOY_KEY="+deployKey,
		"LT_AUTH_STORE="+authStore,
		fmt.Sprintf("LT_PARENT_PID=%d", os.Getpid()),
	)
	out, err := cmd.CombinedOutput()
	if strings.Contains(string(out), "LANDLOCK_UNAVAILABLE") {
		t.Skipf("landlock unavailable on this kernel:\n%s", out)
	}
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, out)
	}

	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name, result, ok := strings.Cut(line, " "); ok {
			got[name] = result
		}
	}
	want := map[string]string{
		"write-workspace":    "denied",  // the audit's open question: additive /tmp grant would have opened this
		"write-tmp-sibling":  "denied",  // no blanket /tmp
		"read-deploy-key":    "denied",  // supervisor's ~/.ssh
		"read-auth-store":    "denied",  // shared opencode home (cross-routine channel)
		"read-supervisor":    "denied",  // /proc/<supervisor>/environ, closed by non-dumpable
		"write-memory":       "allowed", // the one place writes persist
		"write-runtmp":       "allowed",
		"write-attempt-home": "allowed", // disposable per-attempt HOME
		"read-workspace":     "allowed",
		"read-os":            "allowed",
	}
	for name, exp := range want {
		if got[name] != exp {
			t.Errorf("%s = %q, want %q (full output:\n%s)", name, got[name], exp, out)
		}
	}
}

// landlockHelper runs confined: it applies the rules exactly as the
// sandbox-exec shim does, probes each boundary, prints "name allowed|denied"
// lines, and exits.
func landlockHelper() {
	ro := filepath.SplitList(os.Getenv(EnvRO))
	rw := filepath.SplitList(os.Getenv(EnvRW))
	if _, err := Apply(ro, rw); err != nil {
		fmt.Printf("LANDLOCK_UNAVAILABLE: %v\n", err)
		os.Exit(0)
	}

	report := func(name string, err error) {
		result := "allowed"
		if err != nil {
			result = "denied"
		}
		fmt.Printf("%s %s\n", name, result)
	}
	tryWrite := func(path string) error {
		err := os.WriteFile(path, []byte("x"), 0o644)
		if err == nil {
			os.Remove(path)
		}
		return err
	}
	tryRead := func(path string) error {
		_, err := os.ReadFile(path)
		return err
	}

	ws := os.Getenv("LT_WORKSPACE")
	report("write-workspace", tryWrite(filepath.Join(ws, "probe.txt")))
	report("write-tmp-sibling", tryWrite(filepath.Join(os.TempDir(), fmt.Sprintf("openroutines-lt-%d", os.Getpid()))))
	report("read-deploy-key", tryRead(os.Getenv("LT_DEPLOY_KEY")))
	report("read-auth-store", tryRead(os.Getenv("LT_AUTH_STORE")))
	report("read-supervisor", tryRead(fmt.Sprintf("/proc/%s/environ", os.Getenv("LT_PARENT_PID"))))
	report("write-memory", tryWrite(filepath.Join(ws, "memory", "probe.md")))
	report("write-runtmp", tryWrite(filepath.Join(os.Getenv("LT_RUNTMP"), "probe")))
	report("write-attempt-home", tryWrite(filepath.Join(os.Getenv("LT_HOME_ATTEMPT"), "probe")))
	report("read-workspace", tryRead(os.Getenv("LT_SEEDED")))
	report("read-os", tryRead("/etc/passwd"))
	os.Exit(0)
}
