package runner

import (
	"log/slog"
	"os/exec"
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
