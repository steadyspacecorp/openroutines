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

var discardLog = slog.New(slog.DiscardHandler)

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
