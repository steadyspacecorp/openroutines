package runner

import (
	"log/slog"
	"os/exec"
	"syscall"
	"time"
)

// pipeDrainDeadline bounds how long waiting on the run's output pipes may
// outlast the process: a daemonized grandchild keeps the inherited pipe open
// forever, and the wait for EOF must not hold the tick loop.
const pipeDrainDeadline = 5 * time.Second

// containerExitGrace is how long `docker run` gets to notice that its
// container is gone before the client itself is killed.
const containerExitGrace = 5 * time.Second

// killClient ends a container run after `docker stop` was asked to take the
// container down: a client that does not follow it out is killed rather than
// waited on forever. Waiting for Wait to return is not optional -- the caller
// flushes the stream writers, and returning before Wait would race them.
func killClient(cmd *exec.Cmd, grace time.Duration, done chan error, log *slog.Logger) {
	select {
	case <-done:
	case <-time.After(grace):
		log.Warn("docker client did not exit after the container stopped -- killed", "grace", grace)
		_ = cmd.Process.Kill()
		<-done // bounded by WaitDelay now that the process is going away
	}
}

// signalTarget is the run's process group when it leads one. The guard
// matters: signaling -pid without Setpgid would reach the supervisor's own
// group.
func signalTarget(cmd *exec.Cmd) int {
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid {
		return -cmd.Process.Pid
	}
	return cmd.Process.Pid
}

// killGroup terminates the run's whole process group: SIGTERM, grace, SIGKILL.
// The waits are bounded by the command's WaitDelay, not by the group's
// willingness to exit.
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

// reapGroup kills what the model process left running after exiting. It runs
// after the leader was waited on, so the group id could in principle have
// been recycled -- an accepted race; the import re-checks staging at open
// time and does not depend on this having worked.
func reapGroup(cmd *exec.Cmd) {
	_ = syscall.Kill(signalTarget(cmd), syscall.SIGKILL)
}
