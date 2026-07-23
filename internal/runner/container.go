package runner

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// The model-directed process runs in a container; the pipeline runs on the
// host. The container boundary matches the trust boundary (DESIGN.md): the
// supervisor side (staging, validation, git) is trusted code, while opencode
// and everything it spawns sees only the mounted, git-free run workspace.

const runtimeImageTag = "openroutines-runtime:local"

// nativeMode reports whether to spawn opencode directly instead of in a
// container: inside the production image (which ships opencode), or when a
// contributor explicitly opts out with OPENROUTINES_NATIVE=1.
func nativeMode() bool {
	return os.Getenv("OPENROUTINES_IN_CONTAINER") == "1" || os.Getenv("OPENROUTINES_NATIVE") == "1"
}

// ensureRuntimeImage builds the Dockerfile's runtime stage (debian + git +
// pinned opencode). Layer caching makes every build after the first fast.
func ensureRuntimeImage(agentDir string) error {
	cmd := exec.Command("docker", "build", "--quiet", "--target", "runtime", "-t", runtimeImageTag, agentDir)
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
func containerCmd(containerName, workspace string, env []string, ocArgs []string) *exec.Cmd {
	args := []string{
		"run", "--rm", "--init",
		"--name", containerName,
		"-v", workspace + ":/work",
		"-w", "/work",
		"-e", "HOME=/home/agent",
		"-e", "TMPDIR=/work/.runtmp",
	}
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		args = append(args, "-e", name)
	}
	args = append(args, runtimeImageTag, "opencode")
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
