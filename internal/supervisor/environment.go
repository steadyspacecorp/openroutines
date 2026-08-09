package supervisor

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/steadyspacecorp/openroutines/internal/config"
	"github.com/steadyspacecorp/openroutines/internal/creds"
	"github.com/steadyspacecorp/openroutines/internal/mode"
	"github.com/steadyspacecorp/openroutines/internal/sandbox"
)

// Says once at boot that the master key value is in this
// process's environment -- the weaker delivery, and boot is the only moment
// anyone is told. Fires on a leftover variable too: unset, it still publishes
// the value.
func (s *Supervisor) warnKeyDelivery() {
	if mode.Current() != mode.DeployedContainer || !creds.KeyValueInEnv() {
		return
	}
	slog.Warn("the master key value is in this process's environment -- readable wherever that environment is; mount the key as a file, point the file variable at the path, and unset the value variable",
		"value_env", creds.EnvMasterKey, "file_env", creds.EnvMasterKeyFile)
}

func verifyAttemptGroups(groups []int, slots int) error {
	for slot := range slots {
		gid := attemptUIDBase + slot
		if !slices.Contains(groups, gid) {
			return fmt.Errorf("the agent user is not in attempt group %d for run slot %d -- rebuild the deploy image from the current template Dockerfile", gid, slot+1)
		}
	}
	return nil
}

// Enforces the fail-closed policy at boot, not mid-run.
func (s *Supervisor) verifySandbox() error {
	switch mode.Current() {
	case mode.DeployedContainer:
		// Join the attempt groups first -- whether the image's membership
		// reached this process depends on the init that booted the container
		// -- then verify, refusing at boot rather than failing every attempt
		// at staging.
		if err := sandbox.EnsureAttemptGroups(config.MaxConcurrency + 1); err != nil {
			return err
		}
		groups, err := os.Getgroups()
		if err != nil {
			return fmt.Errorf("attempt group check: %w", err)
		}
		if err := verifyAttemptGroups(groups, cap(s.pool.slots)); err != nil {
			return err
		}
		// Constructed environment, like every other child; TMPDIR is the
		// scratch scope it confines.
		probe := exec.Command(sandbox.HelperPath, "sandbox-probe")
		probe.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
			Uid: attemptUIDBase,
			Gid: attemptUIDBase,
		}}
		probe.Env = []string{
			"TMPDIR=" + os.Getenv("TMPDIR"),
			sandbox.EnvAttemptUID + "=" + strconv.Itoa(attemptUIDBase),
		}
		out, probeErr := probe.Output()
		if probeErr == nil {
			// Prove the other half of reusable identities: an escaped process
			// in its own session can be found and killed by UID.
			hold := exec.Command(sandbox.HelperPath, "sandbox-hold")
			hold.Env = []string{sandbox.EnvAttemptUID + "=" + strconv.Itoa(attemptUIDBase)}
			hold.SysProcAttr = &syscall.SysProcAttr{
				Setsid: true,
				Credential: &syscall.Credential{
					Uid: attemptUIDBase,
					Gid: attemptUIDBase,
				},
			}
			if err := hold.Start(); err != nil {
				return fmt.Errorf("attempt uid cleanup probe start: %w", err)
			}
			if err := s.pool.reap(attemptUIDBase); err != nil {
				_ = hold.Process.Kill()
				_ = hold.Wait()
				return fmt.Errorf("attempt uid cleanup probe: %w", err)
			}
			if err := hold.Wait(); err == nil {
				return fmt.Errorf("attempt uid cleanup probe: escaped process was not killed")
			}
			slog.Info("filesystem sandbox active for model processes", "mode", strings.TrimSpace(string(out)))
			return nil
		}
		// The probe tolerates Landlock absence, so failure here means the
		// identity transition itself is broken -- the gating guarantee no
		// override may waive.
		var exitErr *exec.ExitError
		if errors.As(probeErr, &exitErr) && len(exitErr.Stderr) > 0 {
			return fmt.Errorf("attempt identity probe: %w: %s", probeErr, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return fmt.Errorf("attempt identity probe: %w -- the binary needs cap_setuid and cap_setgid; rebuild the deploy image from the current template Dockerfile", probeErr)
	case mode.LocalNative:
		slog.Warn("OPENROUTINES_NATIVE=1 -- model processes run unconfined (dev mode)")
	case mode.LocalContainer:
		slog.Info("model processes run in the per-run container")
	}
	return nil
}
