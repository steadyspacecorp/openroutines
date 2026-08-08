package cli

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/sandbox"
)

// The internal re-exec shim: apply Landlock rules from the environment to
// this process, then exec the model process, so the rules bind to it and
// its children. Not part of the public command surface.
//
// Fail-closed on Linux: no confinement means no run, unless
// OPENROUTINES_UNSAFE_NO_SANDBOX=1 is set. Off Linux it warns and proceeds.
func cmdSandboxExec(args []string) int {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return fail(fmt.Errorf("sandbox-exec: nothing to exec"))
	}
	ro := filepath.SplitList(os.Getenv(sandbox.EnvRO))
	rw := filepath.SplitList(os.Getenv(sandbox.EnvRW))
	// Write-granted paths must exist before rules apply: Landlock can't
	// grant what it can't resolve, and once confined nothing can create them.
	for _, p := range rw {
		if p == "" {
			continue
		}
		if err := os.MkdirAll(p, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "[sandbox: write grant %s could not be created (%v) -- the run will not be able to write there]\n", p, err)
		}
	}

	desc, skippedRW, err := sandbox.Apply(ro, rw)
	for _, p := range skippedRW {
		fmt.Fprintf(os.Stderr, "[sandbox: write grant %s does not exist -- dropped from the ruleset; the run will not be able to write there]\n", p)
	}
	switch {
	case err == nil:
		fmt.Fprintf(os.Stderr, "[sandbox: %s]\n", desc)
	case os.Getenv(sandbox.EnvUnsafeOverride) == "1":
		fmt.Fprintf(os.Stderr, "[sandbox: landlock disabled by %s; uid isolation remains active]\n", sandbox.EnvUnsafeOverride)
	case runtime.GOOS != "linux":
		fmt.Fprintf(os.Stderr, "[sandbox: unavailable on %s -- native mode is a dev opt-in, proceeding unconfined]\n", runtime.GOOS)
	default:
		fmt.Fprintf(os.Stderr, "[sandbox: landlock unavailable (%v); uid isolation remains active]\n", err)
	}

	uid64, parseErr := strconv.ParseUint(os.Getenv(sandbox.EnvAttemptUID), 10, 32)
	if parseErr != nil || uid64 == 0 {
		return fail(fmt.Errorf("sandbox-exec: invalid attempt uid %q", os.Getenv(sandbox.EnvAttemptUID)))
	}
	if err := sandbox.DropIdentity(uint32(uid64)); err != nil {
		return fail(fmt.Errorf("sandbox-exec: uid isolation failed: %w", err))
	}

	// Everything the model process creates must stay reachable by the
	// supervisor via the attempt's group. Keep group bits, drop world.
	syscall.Umask(0o007)

	bin, err := exec.LookPath(args[0])
	if err != nil {
		return fail(err)
	}
	// Exec replaces this process; the applied rules travel with it.
	if err := syscall.Exec(bin, args, os.Environ()); err != nil {
		return fail(fmt.Errorf("exec %s: %w", args[0], err))
	}
	return 0 // unreachable
}

// Restores group access on files a model process chmod'd to exclude the
// group (e.g. 0600), which the supervisor -- holding no CAP_DAC_OVERRIDE --
// can't otherwise delete. The runner spawns this as the attempt identity
// when cleanup fails; running unprivileged, it can only touch what that
// identity already owned.
func cmdSandboxReclaim(args []string) int {
	if len(args) != 1 {
		return fail(fmt.Errorf("sandbox-reclaim: exactly one root expected"))
	}
	_ = filepath.WalkDir(args[0], func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // unreadable subtree: nothing more to reclaim there
		}
		if entry.IsDir() {
			_ = os.Chmod(path, 0o770)
			return nil
		}
		if entry.Type().IsRegular() {
			_ = os.Chmod(path, 0o660)
		}
		return nil
	})
	return 0
}

// Applies a throwaway ruleset to a child-less scratch scope and reports
// whether confinement is available. Used by supervise at boot
// (fail closed before the first run, not during it).
func cmdSandboxProbe(_ []string) int {
	uid64, err := strconv.ParseUint(os.Getenv(sandbox.EnvAttemptUID), 10, 32)
	if err != nil || uid64 == 0 {
		fmt.Fprintln(os.Stderr, "attempt uid is required")
		return 1
	}
	desc := "uid isolation"
	if landlock, _, landlockErr := sandbox.Apply([]string{os.TempDir()}, nil); landlockErr == nil {
		desc += " + " + landlock
	}
	if err := sandbox.DropIdentity(uint32(uid64)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(desc)
	return 0
}

func cmdSandboxSpawnProbe(_ []string) int {
	uid64, err := strconv.ParseUint(os.Getenv(sandbox.EnvAttemptUID), 10, 32)
	if err != nil || uid64 == 0 {
		return fail(fmt.Errorf("sandbox-spawn-probe: attempt uid is required"))
	}
	cmd := exec.Command(sandbox.HelperPath, "sandbox-probe")
	cmd.Env = []string{
		"TMPDIR=" + os.Getenv("TMPDIR"),
		sandbox.EnvAttemptUID + "=" + strconv.FormatUint(uid64, 10),
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
		Uid: uint32(uid64),
		Gid: uint32(uid64),
	}}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fail(fmt.Errorf("sandbox-spawn-probe: %w: %s", err, strings.TrimSpace(string(out))))
	}
	hold := exec.Command(sandbox.HelperPath, "sandbox-hold")
	hold.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
		Credential: &syscall.Credential{
			Uid: uint32(uid64),
			Gid: uint32(uid64),
		},
	}
	if err := hold.Start(); err != nil {
		return fail(fmt.Errorf("sandbox-spawn-probe: start cleanup target: %w", err))
	}
	if err := sandbox.ReapIdentity(uint32(uid64)); err != nil {
		_ = hold.Process.Kill()
		_ = hold.Wait()
		return fail(fmt.Errorf("sandbox-spawn-probe: cleanup: %w", err))
	}
	if err := hold.Wait(); err == nil {
		return fail(fmt.Errorf("sandbox-spawn-probe: cleanup target survived"))
	}
	fmt.Print(string(out))
	return 0
}

func cmdSandboxHold(_ []string) int {
	time.Sleep(time.Minute)
	return 0
}
