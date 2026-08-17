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

// Derives the local runtime image tag from the agent's version pin, so two
// agent repos with different pins can't retag each other's image between
// build and run (a shared :local tag could).
func runtimeImageTag(agentDir string) string {
	pin := "local"
	if raw, err := os.ReadFile(filepath.Join(agentDir, ".openroutines", "version")); err == nil {
		if v := strings.TrimSpace(string(raw)); v != "" {
			pin = v
		}
	}
	return "openroutines-runtime:" + pin
}

var imageBuildMu sync.Mutex

func ensureRuntimeImage(agentDir, tag string) error {
	imageBuildMu.Lock()
	defer imageBuildMu.Unlock()
	cold := exec.Command("docker", "image", "inspect", tag).Run() != nil
	if cold {
		fmt.Fprintf(os.Stderr, "building the local runtime image (first build downloads a Debian base and opencode -- this can take a few minutes)\n")
		slog.Debug("local runtime image build started", "tag", tag)
	}
	cmd := exec.Command("docker", "build", "--quiet", "--target", "runtime", "-t", tag, agentDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if cold {
			fmt.Fprintln(os.Stderr, "local runtime image build failed")
		}
		return fmt.Errorf("building runtime image failed (is Docker running?): %w\n%s", err, strings.TrimSpace(string(out)))
	}
	if cold {
		fmt.Fprintln(os.Stderr, "local runtime image ready")
	}
	return nil
}

func stopContainer(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), containerStopDeadline)
	defer cancel()
	_ = exec.CommandContext(ctx, "docker", "stop", "-t", "10", name).Run()
}

const containerStopDeadline = 20 * time.Second
