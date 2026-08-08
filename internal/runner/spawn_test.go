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

// The precedence is a security claim SECURITY.md makes out loud: a deployed
// agent cannot be talked out of its sandbox by an environment variable,
// however it got set.
func TestTheProductionSandboxOutranksTheContributorOptOut(t *testing.T) {
	for _, tc := range []struct {
		name                string
		inContainer, native string
		want                Isolation
	}{
		{"a developer's machine", "", "", Containerized},
		{"a contributor opting out", "", "1", Unconfined},
		{"the production image", "1", "", Sandboxed},
		{"the production image, opt-out attempted", "1", "1", Sandboxed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OPENROUTINES_IN_CONTAINER", tc.inContainer)
			t.Setenv("OPENROUTINES_NATIVE", tc.native)
			if got := Confinement(); got != tc.want {
				t.Errorf("Confinement() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The sandboxed spawn path makes the run a session leader instead of setting
// Setpgid (hostOpencode.run's TIOCSTI note), and a session leader leads a fresh
// process group too. Killing the run must reach that whole group: on the rung
// with no pid namespace, an ordinary same-group descendant would otherwise
// outlive a timeout and keep writing to staged knowledge.
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

	killGroup(cmd, time.Second, done, slog.New(slog.DiscardHandler))
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
