package runner

import (
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

// The logger tests hand to code that only logs on a failure
// path they aren't asserting on -- there is no log-capture harness in this
// package, so tests that do care about a specific line read it from
// behavior (a returned value, a file on disk), not from log output.
var discardLog = slog.New(slog.DiscardHandler)

// `docker stop` can return without the run's client following the container
// out -- an unresponsive daemon has nothing to stop. The wait on the client
// must end anyway, or a local run parks the caller the way an orphan holding
// the output pipe used to park the supervisor.
func TestKillClientBoundsTheWaitOnAStuckDockerClient(t *testing.T) {
	cmd := exec.Command("sleep", "120")
	cmd.WaitDelay = pipeDrainDeadline
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		killClient(cmd, 100*time.Millisecond, done, discardLog)
	}()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("killClient never returned: the wait on the docker client is unbounded")
	}
}

// The sandboxed spawn path makes the run a session leader rather than setting
// Setpgid, and a session leader leads a fresh process group too. Killing the
// run must reach that whole group: on the rung with no pid namespace a
// same-group descendant would otherwise outlive a timeout and keep writing.
func TestKillingARunReachesTheDescendantsOfASessionLeader(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	cmd := exec.Command("sh", "-c", "sleep 30 & echo $! > "+pidfile+"; wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var pid int
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if raw, err := os.ReadFile(pidfile); err == nil && len(raw) > 0 {
			pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run never reported its descendant's pid")
		}
	}

	killGroup(cmd, time.Second, done, discardLog)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("a descendant in the session leader's group outlived the kill")
		}
	}
}
