package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	if raw, err := os.ReadFile(filepath.Join(agentDir, ".openroutines-version")); err == nil {
		if v := strings.TrimSpace(string(raw)); v != "" {
			pin = v
		}
	}
	return "openroutines-runtime:" + pin
}

// nativeMode reports whether to spawn opencode directly instead of in a
// container: inside the production image (which ships opencode), or when a
// contributor explicitly opts out with OPENROUTINES_NATIVE=1.
func nativeMode() bool {
	return os.Getenv("OPENROUTINES_IN_CONTAINER") == "1" || os.Getenv("OPENROUTINES_NATIVE") == "1"
}

// ensureRuntimeImage builds the Dockerfile's runtime stage (debian + git +
// pinned opencode). Layer caching makes every build after the first fast --
// but the first build downloads a Debian base and opencode, so say so: a
// silent multi-minute wait reads as a hang and gets Ctrl-C'd.
func ensureRuntimeImage(agentDir, tag string) error {
	if err := exec.Command("docker", "image", "inspect", tag).Run(); err != nil {
		fmt.Printf("building the local runtime image (first build downloads the base image and opencode -- this can take a few minutes)...\n")
	}
	cmd := exec.Command("docker", "build", "--quiet", "--target", "runtime", "-t", tag, agentDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("building runtime image failed (is Docker running?): %v\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// containerCmd builds the docker run invocation for one attempt: the run
// workspace mounted at /work, the constructed env passed through, nothing
// else from the host visible. Env vars are passed by NAME only -- docker
// resolves the values from the client process's environment -- so secret
// values never appear on the command line (argv is world-readable via ps
// for the duration of the run).
func containerCmd(containerName, workspace, image string, env []string, ocArgs []string) *exec.Cmd {
	// HOME is the disposable per-attempt directory inside the mounted
	// workspace -- the same hygiene production applies, and what makes
	// opencode's session storage (token usage) readable after the
	// container exits instead of vanishing with --rm.
	args := []string{
		"run", "--rm", "--init",
		"--name", containerName,
		"-v", workspace + ":/work",
		"-w", "/work",
		"-e", "HOME=/work/" + attemptHomeName,
		"-e", "XDG_DATA_HOME=/work/" + attemptHomeName + "/.local/share",
		"-e", "TMPDIR=/work/.runtmp",
	}
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		args = append(args, "-e", name)
	}
	args = append(args, image, "opencode")
	args = append(args, ocArgs...)
	cmd := exec.Command("docker", args...)
	// The docker client needs its own environment (daemon socket, config)
	// plus the values it will forward by name.
	cmd.Env = append(os.Environ(), env...)
	return cmd
}

// stopContainer terminates a run's container with the same semantics as a
// process-group kill: SIGTERM (forwarded by --init), 10s grace, then SIGKILL.
func stopContainer(name string) {
	_ = exec.Command("docker", "stop", "-t", "10", name).Run()
}
