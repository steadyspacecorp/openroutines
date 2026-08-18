//go:build acceptance

package acceptance

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type isolationFixture struct {
	masterKey string
	deployKey string
	image     string
}

var (
	isolationOnce     sync.Once
	isolationBuilt    *isolationFixture
	isolationBuildErr error
)

func TestIsolationBubblewrap(t *testing.T) {
	fixture := isolation(t)
	out := fixture.run(t,
		"-e", "EXPECT_TREE_COLLAPSE=1",
		"-e", "OPENROUTINES_LOG_LEVEL=debug",
		"--security-opt", "seccomp=unconfined",
		"--security-opt", "apparmor=unconfined",
		fixture.image, "sh", "-c", isolationScript,
	)
	assertContains(t, out,
		"visible-commands=",
		"supervisor-environ=Permission denied",
		`selected="bubblewrap namespaces, shared /proc"`,
		"agent-repo=No such file or directory",
		"deploy-key=No such file or directory",
		"workspace-write=x",
	)
}

func TestIsolationLandlock(t *testing.T) {
	fixture := isolation(t)
	out := fixture.run(t,
		"-e", "EXPECT_TREE_COLLAPSE=0",
		"-e", "OPENROUTINES_LOG_LEVEL=debug",
		"--security-opt", "no-new-privileges",
		fixture.image, "sh", "-c", isolationScript,
	)
	assertContains(t, out,
		`selected="landlock domain`,
		"supervisor-environ=Permission denied",
		"agent-repo=Permission denied",
		"deploy-key=Permission denied",
		"workspace-write=x",
	)
}

func TestIsolationDisabled(t *testing.T) {
	fixture := isolation(t)
	out := fixture.run(t,
		"-e", "EXPECT_TREE_COLLAPSE=0",
		"-e", "OPENROUTINES_DISABLE_SANDBOX=1",
		fixture.image, "sh", "-c", isolationScript,
	)
	assertContains(t, out,
		"run sandbox disabled",
		"agent-repo=\n",
		"supervisor-environ=Permission denied",
	)
}

func TestForeignOwnerRoutineLock(t *testing.T) {
	fixture := isolation(t)
	fixture.runWithoutKeys(t,
		"--user", "root",
		"-e", "HOME=/root",
		fixture.image, "sh", "-c", foreignOwnerScript,
	)
}

func isolation(t *testing.T) *isolationFixture {
	t.Helper()
	requireDocker(t)
	isolationOnce.Do(func() {
		isolationBuilt, isolationBuildErr = buildIsolationFixture()
	})
	if isolationBuildErr != nil {
		t.Fatal(isolationBuildErr)
	}
	return isolationBuilt
}

func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err == nil {
		return
	}
	if os.Getenv("OPENROUTINES_REQUIRE_DOCKER") == "1" {
		t.Fatal("docker is required for the acceptance isolation tests")
	}
	t.Skip("docker is not installed")
}

func buildIsolationFixture() (*isolationFixture, error) {
	root := filepath.Join(testRoot, "isolation")
	contextDir := filepath.Join(root, "context")
	agentDir := filepath.Join(contextDir, "agent")
	if err := os.MkdirAll(contextDir, 0o755); err != nil {
		return nil, err
	}

	deployKeyPath := filepath.Join(root, "deploy-key")
	if _, err := runCommand("", nil, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", deployKeyPath); err != nil {
		return nil, err
	}
	if _, err := runCommand("", nil, openroutinesBin, "new", agentDir); err != nil {
		return nil, err
	}
	configureInput := []byte("isolation-agent\nCI\nci@example.invalid\nUTC\nfake/model\nnot-a-real-key\n")
	if _, err := runCommand(agentDir, configureInput, openroutinesBin, "configure"); err != nil {
		return nil, err
	}
	if err := replaceInFile(filepath.Join(agentDir, "openroutines.yml"), `repo: ""`, "repo: git@local:/agent/origin.git"); err != nil {
		return nil, err
	}
	if _, err := runCommand(agentDir, nil, openroutinesBin, "sync"); err != nil {
		return nil, err
	}

	if err := os.WriteFile(filepath.Join(agentDir, "routines", "isolation.md"), []byte(isolationRoutine), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(agentDir, "routines", "manual-probe.md"), []byte(manualProbeRoutine), 0o644); err != nil {
		return nil, err
	}
	stateDir := filepath.Join(agentDir, "knowledge", "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(stateDir, "isolation.json"), []byte(isolationState), 0o644); err != nil {
		return nil, err
	}
	if _, err := runCommand(filepath.Join(agentDir, "knowledge"), nil, "git", "add", "state/isolation.json"); err != nil {
		return nil, err
	}
	if _, err := runCommand(filepath.Join(agentDir, "knowledge"), nil, "git", "-c", "user.name=CI", "-c", "user.email=ci@example.invalid", "commit", "--quiet", "-m", "Seed isolation acceptance run"); err != nil {
		return nil, err
	}
	if _, err := runCommand(agentDir, nil, "git", "worktree", "remove", "knowledge"); err != nil {
		return nil, err
	}
	if _, err := runCommand(agentDir, nil, "git", "clone", "--quiet", "--bare", ".", "origin.git"); err != nil {
		return nil, err
	}

	templateDockerfile, err := os.ReadFile(filepath.Join(repoRoot, "template", "Dockerfile"))
	if err != nil {
		return nil, err
	}
	if !bytes.Contains(templateDockerfile, []byte("bubblewrap")) {
		return nil, fmt.Errorf("template Dockerfile does not install the preferred run sandbox")
	}
	for _, name := range []string{"Dockerfile", "opencode", "ssh"} {
		if err := copyFile(filepath.Join(repoRoot, "acceptance", "testdata", "isolation", name), filepath.Join(contextDir, name)); err != nil {
			return nil, err
		}
	}

	masterKey, err := os.ReadFile(filepath.Join(agentDir, "master.key"))
	if err != nil {
		return nil, err
	}
	if err := os.Rename(filepath.Join(agentDir, "master.key"), filepath.Join(root, "master.key")); err != nil {
		return nil, err
	}
	deployKey, err := os.ReadFile(deployKeyPath)
	if err != nil {
		return nil, err
	}

	archOut, err := runCommand("", nil, "docker", "version", "--format", "{{.Server.Arch}}")
	if err != nil {
		return nil, err
	}
	linuxBin := filepath.Join(contextDir, "openroutines")
	buildEnv := append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+strings.TrimSpace(archOut))
	if _, err := runCommandEnv(repoRoot, buildEnv, "go", "build", "-o", linuxBin, "./cmd/openroutines"); err != nil {
		return nil, err
	}

	isolationImage = fmt.Sprintf("openroutines-acceptance-%d", os.Getpid())
	if _, err := runCommand("", nil, "docker", "build", "--quiet", "-t", isolationImage, contextDir); err != nil {
		return nil, err
	}
	return &isolationFixture{
		masterKey: strings.TrimSpace(string(masterKey)),
		deployKey: strings.TrimSpace(string(deployKey)),
		image:     isolationImage,
	}, nil
}

func (fixture *isolationFixture) run(t *testing.T, args ...string) string {
	t.Helper()
	base := []string{
		"run", "--rm",
		"-e", "OPENROUTINES_MASTER_KEY",
		"-e", "OPENROUTINES_DEPLOY_KEY",
	}
	env := append(os.Environ(),
		"OPENROUTINES_MASTER_KEY="+fixture.masterKey,
		"OPENROUTINES_DEPLOY_KEY="+fixture.deployKey,
	)
	out, err := runCommandEnv("", env, "docker", append(base, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (fixture *isolationFixture) runWithoutKeys(t *testing.T, args ...string) string {
	t.Helper()
	base := []string{"run", "--rm"}
	out, err := runCommandEnv("", os.Environ(), "docker", append(base, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func runCommand(dir string, stdin []byte, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

func runCommandEnv(dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

func replaceInFile(path, old, replacement string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	updated := strings.Replace(string(data), old, replacement, 1)
	if updated == string(data) {
		return fmt.Errorf("%s does not contain %q", path, old)
	}
	return os.WriteFile(path, []byte(updated), 0o644)
}

func copyFile(source, destination string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	return os.WriteFile(destination, data, info.Mode())
}

func assertContains(t *testing.T, output string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(output, fragment) {
			t.Errorf("output does not contain %q:\n%s", fragment, output)
		}
	}
}

const isolationRoutine = `---
schedule: "0 9 * * *"
model: fake/model
---
Record what the sandbox lets this run reach.
`

const manualProbeRoutine = `---
schedule: "0 9 * * *"
active: false
model: fake/model
---
Record what the sandbox lets this manual run reach.
`

const isolationState = `{"routine":"isolation","watermark":"2026-07-31T00:00:00Z","pending":{"run_id":"run_isolation","scheduled_for":"2026-07-31T09:00:00Z","covered_through":"2026-07-31T09:00:00Z","created_at":"2026-07-31T09:00:00Z","attempts":0}}
`

const isolationScript = `
set -e
openroutines supervise > /tmp/supervise.out 2>&1 &
supervisor=$!
completed=false
for _ in $(seq 1 300); do
  if git -C /agent show knowledge:ledgers/isolation.md >/dev/null 2>&1; then
    completed=true
    break
  fi
  if ! kill -0 "$supervisor" 2>/dev/null; then
    cat /tmp/supervise.out
    exit 1
  fi
  sleep 0.1
done
if [ "$completed" != true ]; then
  echo "supervised isolation run did not complete"
  cat /tmp/supervise.out
  exit 1
fi
if ! openroutines routines run manual-probe --write-knowledge > /tmp/manual.out 2>&1; then
  echo "manual run failed beside the supervisor"
  cat /tmp/manual.out
  exit 1
fi
kill -TERM "$supervisor"
wait "$supervisor"
cat /tmp/supervise.out
if [ -z "${OPENROUTINES_DISABLE_SANDBOX:-}" ] && [ -e /agent/capture-escaped ]; then
  echo "capture escaped the run sandbox"
  exit 1
fi
git -C /agent show knowledge:ledgers/isolation.md
git -C /agent show knowledge:ledgers/escapee.md >/dev/null
local_tip=$(git -C /agent rev-parse knowledge)
origin_tip=$(git --git-dir=/agent/origin.git rev-parse knowledge)
if [ "$local_tip" != "$origin_tip" ]; then
  echo "knowledge push did not reach origin: local=$local_tip origin=$origin_tip"
  exit 1
fi
if ls /tmp | grep -q openroutines-; then
  echo "run workspace not cleaned"
  exit 1
fi
if [ "$EXPECT_TREE_COLLAPSE" = 1 ] && pgrep -x sleep >/dev/null 2>&1; then
  echo "escaped descendant outlived the run"
  exit 1
fi
`

const foreignOwnerScript = `
lock=/agent/.openroutines-tmp/locks/manual-probe.lock
openroutines routines run manual-probe > /tmp/stranger.out 2>&1 || true
if [ "$(stat -c %U "$lock" 2>/dev/null)" != root ]; then
  echo "the run made as root left no lock file of its own -- nothing under test"
  cat /tmp/stranger.out
  exit 1
fi
if ! setpriv --reuid agent --regid agent --clear-groups \
  env HOME=/home/agent openroutines routines run manual-probe > /tmp/owner.out 2>&1; then
  echo "the owner could not run a routine whose lock file root created"
  ls -l "$lock"
  cat /tmp/owner.out
  exit 1
fi
`
