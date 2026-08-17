package runner

import (
	"log/slog"
	"os/exec"
	"syscall"
	"time"
)

const pipeDrainDeadline = 5 * time.Second

const containerExitGrace = 5 * time.Second

func killClient(cmd *exec.Cmd, grace time.Duration, done chan error, log *slog.Logger) {
	select {
	case <-done:
	case <-time.After(grace):
		log.Warn("docker client did not exit after the container stopped -- killed", "grace", grace)
		_ = cmd.Process.Kill()
		<-done
	}
}

func signalTarget(cmd *exec.Cmd) int {
	// Negative PIDs signal the run's process group; without Setsid/Setpgid, that would hit the supervisor.
	if attr := cmd.SysProcAttr; attr != nil && (attr.Setpgid || attr.Setsid) {
		return -cmd.Process.Pid
	}
	return cmd.Process.Pid
}

func killGroup(cmd *exec.Cmd, grace time.Duration, done chan error, log *slog.Logger) {
	target := signalTarget(cmd)
	_ = syscall.Kill(target, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(grace):
		log.Warn("run did not exit on SIGTERM -- killed", "grace", grace)
		_ = syscall.Kill(target, syscall.SIGKILL)
		<-done
	}
}

func reapGroup(cmd *exec.Cmd) {
	// The leader has already been waited on, so the group id could in principle
	// have been recycled -- an accepted race. Import re-checks staging at open
	// time and does not depend on this having worked.
	_ = syscall.Kill(signalTarget(cmd), syscall.SIGKILL)
}
