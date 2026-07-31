package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/steadyspacecorp/openroutines/internal/sandbox"
)

// cmdSandboxExec is the internal re-exec shim: apply the Landlock rules from
// the environment to this process, then exec the model process (opencode).
// Rules bind to the caller and its children, which is exactly what exec
// preserves. Not part of the public command surface.
//
// Fail-closed policy: on Linux, no confinement means no run -- unless the
// deliberately ugly OPENROUTINES_UNSAFE_NO_SANDBOX=1 is set. Off Linux
// (native mode is an explicit dev opt-in there), it warns and proceeds.
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
	// grant what it can't resolve, and once confined nothing can create
	// them (found live: opencode's first mkdir of ~/.local was denied).
	for _, p := range rw {
		if p != "" {
			_ = os.MkdirAll(p, 0o755)
		}
	}

	desc, err := sandbox.Apply(ro, rw)
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

// cmdSandboxProbe applies a throwaway ruleset to a child-less scratch scope
// and reports whether confinement is available. Used by supervise at boot
// (fail closed before the first run, not during it).
func cmdSandboxProbe(_ []string) int {
	uid64, err := strconv.ParseUint(os.Getenv(sandbox.EnvAttemptUID), 10, 32)
	if err != nil || uid64 == 0 {
		fmt.Fprintln(os.Stderr, "attempt uid is required")
		return 1
	}
	desc := "uid isolation"
	if landlock, landlockErr := sandbox.Apply([]string{os.TempDir()}, nil); landlockErr == nil {
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
	fmt.Print(string(out))
	return 0
}
