package runner

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The model-directed process runs in a container; the pipeline runs on the
// host. The container boundary matches the trust boundary: the
// supervisor side (staging, validation, git) is trusted code, while opencode
// and everything it spawns sees only the mounted, git-free run workspace.

// runtimeImageTag derives the local runtime image tag from the agent's
// version pin, so two agent repos with different pins can't retag each
// other's image between build and run (a shared :local tag could).
func runtimeImageTag(agentDir string) string {
	pin := "local"
	if raw, err := os.ReadFile(filepath.Join(agentDir, ".openroutines", "version")); err == nil {
		if v := strings.TrimSpace(string(raw)); v != "" {
			pin = v
		}
	}
	return "openroutines-runtime:" + pin
}

// ensureRuntimeImage builds the Dockerfile's runtime stage (debian + git +
// pinned opencode). Layer caching makes every build after the first fast --
// but the first build downloads a Debian base and opencode, so say so: a
// silent multi-minute wait reads as a hang and gets Ctrl-C'd.
// imageBuildMu keeps concurrent attempts from racing `docker build` on the
// same tag: the first does the real build, the rest hit its cache. The
// build always runs -- it is how a Dockerfile edit takes effect -- but two
// first-boot builds at once would download everything twice and print the
// notice twice.
var imageBuildMu sync.Mutex

func ensureRuntimeImage(agentDir, tag string) error {
	imageBuildMu.Lock()
	defer imageBuildMu.Unlock()
	if err := exec.Command("docker", "image", "inspect", tag).Run(); err != nil {
		slog.Info("building the local runtime image (first build downloads a Debian base and opencode -- this can take a few minutes)", "tag", tag)
	}
	cmd := exec.Command("docker", "build", "--quiet", "--target", "runtime", "-t", tag, agentDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("building runtime image failed (is Docker running?): %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// stopContainer terminates a run's container with the same semantics as a
// process-group kill: SIGTERM (forwarded by --init), 10s grace, then SIGKILL.
// The CLI call itself is bounded too -- `docker stop` talks to a daemon that
// can be as unresponsive as the container it is being asked to stop, and the
// caller escalates when this returns without the run's client exiting.
func stopContainer(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), containerStopDeadline)
	defer cancel()
	_ = exec.CommandContext(ctx, "docker", "stop", "-t", "10", name).Run()
}

// containerStopDeadline covers the 10s stop grace plus the daemon's own
// round trips; past that the daemon is not answering.
const containerStopDeadline = 20 * time.Second
