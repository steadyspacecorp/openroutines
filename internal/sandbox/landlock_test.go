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

// Verifies the production rule set on a real kernel.
// This is the test the security audit asked for first: the run workspace
// must not be writable (it lives inside /tmp, and Landlock rules are
// additive -- a blanket /tmp grant would open it), the supervisor's environ
// must not be readable, and the shared-home leak paths must be closed. It
// also answers whether the /dev grant reaches across the nested /dev/shm
// mount (it does).
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
	knowledgeDir := filepath.Join(workspace, "knowledge")
	for _, d := range []string{runTmp, attemptHome, knowledgeDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	seeded := filepath.Join(workspace, "openroutines.yml")
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

	// The audit's second open question: /dev is granted read-write to every
	// attempt, and /dev/shm is a tmpfs mounted inside it that outlives any one
	// attempt. This file stands in for what an earlier attempt left behind --
	// if the confined helper can read or overwrite it, /dev/shm is a
	// cross-attempt channel and the /dev grant reaches across the nested mount.
	// A host that mounts /dev/shm read-only has nothing to probe, and is not a
	// reason to fail the rest of the confinement assertions.
	planted := fmt.Sprintf("/dev/shm/openroutines-lt-%d", os.Getpid())
	if err := os.WriteFile(planted, []byte("left by a prior attempt"), 0o600); err != nil {
		t.Logf("skipping the /dev/shm probe: %v", err)
		planted = ""
	} else {
		t.Cleanup(func() { os.Remove(planted) })
	}

	ro, rw := Paths(workspace, knowledgeDir, runTmp, home, attemptHome)
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
		"LT_AUTH_STORE="+authStore, // gitleaks:allow -- synthetic fixture
		"LT_SHM_PLANTED="+planted,
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
		"write-knowledge":    "allowed", // the one place writes persist
		"write-runtmp":       "allowed",
		"write-attempt-home": "allowed", // disposable per-attempt HOME
		"read-workspace":     "allowed",
		"read-os":            "allowed",
	}
	if planted != "" {
		// The answer, on a real kernel: yes, the rule reaches across. Landlock
		// walks a path up through mount points, so the /dev grant covers the
		// tmpfs mounted at /dev/shm, and what one attempt writes there the next
		// one reads and rewrites. Recorded as the current behavior, not as
		// desired behavior -- closing the channel flips both to "denied".
		want["read-dev-shm"] = "allowed"
		want["write-dev-shm"] = "allowed"
	}
	for name, exp := range want {
		if got[name] != exp {
			t.Errorf("%s = %q, want %q (full output:\n%s)", name, got[name], exp, out)
		}
	}
}

// Runs confined: it applies the rules exactly as the
// sandbox-exec shim does, probes each boundary, prints "name allowed|denied"
// lines, and exits.
func landlockHelper() {
	ro := filepath.SplitList(os.Getenv(EnvRO))
	rw := filepath.SplitList(os.Getenv(EnvRW))
	if _, _, err := Apply(ro, rw); err != nil {
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

	if planted := os.Getenv("LT_SHM_PLANTED"); planted != "" {
		report("read-dev-shm", tryRead(planted))
		report("write-dev-shm", tryWrite(fmt.Sprintf("/dev/shm/openroutines-lt-w-%d", os.Getpid())))
	}

	ws := os.Getenv("LT_WORKSPACE")
	report("write-workspace", tryWrite(filepath.Join(ws, "probe.txt")))
	report("write-tmp-sibling", tryWrite(filepath.Join(os.TempDir(), fmt.Sprintf("openroutines-lt-%d", os.Getpid()))))
	report("read-deploy-key", tryRead(os.Getenv("LT_DEPLOY_KEY")))
	report("read-auth-store", tryRead(os.Getenv("LT_AUTH_STORE")))
	report("read-supervisor", tryRead(fmt.Sprintf("/proc/%s/environ", os.Getenv("LT_PARENT_PID"))))
	report("write-knowledge", tryWrite(filepath.Join(ws, "knowledge", "probe.md")))
	report("write-runtmp", tryWrite(filepath.Join(os.Getenv("LT_RUNTMP"), "probe")))
	report("write-attempt-home", tryWrite(filepath.Join(os.Getenv("LT_HOME_ATTEMPT"), "probe")))
	report("read-workspace", tryRead(os.Getenv("LT_SEEDED")))
	report("read-os", tryRead("/etc/passwd"))
	os.Exit(0)
}

// A write grant that never got created (the mkdir failed, or nobody made
// it) must not vanish from the ruleset without a trace: Apply reports it
// back as skipped so the shim can name the hole in the confinement.
func TestExistingReportsSkippedPaths(t *testing.T) {
	present := t.TempDir()
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	kept, skipped := existing([]string{present, missing, ""})
	if len(kept) != 1 || kept[0] != present {
		t.Fatalf("kept = %v, want [%s]", kept, present)
	}
	if len(skipped) != 1 || skipped[0] != missing {
		t.Fatalf("skipped = %v, want [%s]", skipped, missing)
	}
}
