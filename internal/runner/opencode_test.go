package runner

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fakeBin(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestLocalContainerCanWriteTheWholeWorkspace(t *testing.T) {
	workspace := t.TempDir()
	paths := []string{
		workspace,
		filepath.Join(workspace, "opencode.json"),
		filepath.Join(workspace, ".opencode"),
		filepath.Join(workspace, ".opencode", "agents", "routine.md"),
	}
	if err := os.MkdirAll(filepath.Dir(paths[3]), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths[1], paths[3]} {
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := makeWorldWritable(workspace); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o222 != 0o222 {
			t.Errorf("%s mode = %o, want writable by the container uid", path, info.Mode().Perm())
		}
	}
}

const reportEnv = `#!/bin/sh
echo "HOME=$HOME"
echo "XDG_CONFIG_HOME=$XDG_CONFIG_HOME"
echo "XDG_DATA_HOME=$XDG_DATA_HOME"
echo "HOME_ENTRIES=$(ls -A "$HOME" 2>/dev/null | tr '\n' ',')"
echo "ROUTINE_SECRET=${ROUTINE_SECRET-unset}"
[ -f cwd-marker ] && echo "CWD=workspace"
`

func TestHostCaptureUsesTheAttemptHomeWithoutCredentials(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("OPENROUTINES_DISABLE_SANDBOX", "1")
	if err := os.WriteFile(filepath.Join(ws, "cwd-marker"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	writeMsg(t, filepath.Join(ws, ".home", ".config", "opencode", "plugin"), "evil.js", "export const Evil = async () => ({})")
	fakeBin(t, "opencode", reportEnv)

	out, err := sandboxedRuntime{workspace: ws, tempDir: filepath.Join(ws, ".runtmp"), env: []string{"ROUTINE_SECRET=secret"}}.exec("session", "list")
	if err != nil {
		t.Fatal(err)
	}
	env := parseEnv(t, string(out))

	if want := filepath.Join(ws, attemptHomeName); env["HOME"] != want {
		t.Fatalf("HOME = %q, want the attempt home %q", env["HOME"], want)
	}
	if !strings.Contains(env["HOME_ENTRIES"], ".config") {
		t.Fatalf("capture did not re-enter the attempt home: %q", env["HOME_ENTRIES"])
	}
	if want := filepath.Join(env["HOME"], ".config"); env["XDG_CONFIG_HOME"] != want {
		t.Fatalf("XDG_CONFIG_HOME = %q, want %q", env["XDG_CONFIG_HOME"], want)
	}
	if want := filepath.Join(ws, ".home", ".local", "share"); env["XDG_DATA_HOME"] != want {
		t.Fatalf("XDG_DATA_HOME = %q, want %q", env["XDG_DATA_HOME"], want)
	}
	if env["CWD"] != "workspace" {
		t.Fatal("capture must run in the attempt workspace")
	}
	if env["ROUTINE_SECRET"] != "unset" {
		t.Fatal("capture must not receive the routine's credentials")
	}
}

const truncateOnPipe = `#!/bin/sh
if [ -p /dev/stdout ]; then printf '{"messages":[{"in'; else printf '{"messages":[]}'; fi
`

func TestHostCaptureSurvivesOpencodesLossyPipeWrites(t *testing.T) {
	t.Setenv("OPENROUTINES_DISABLE_SANDBOX", "1")
	fakeBin(t, "opencode", truncateOnPipe)
	out, err := sandboxedRuntime{workspace: t.TempDir()}.exec("export", "ses_x")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"messages":[]}` {
		t.Fatalf("the exec must hand opencode a file, not a pipe -- got %q", out)
	}
}

func TestNativeCaptureSurvivesOpencodesLossyPipeWrites(t *testing.T) {
	fakeBin(t, "opencode", truncateOnPipe)
	out, err := nativeRuntime{workspace: t.TempDir()}.exec("export", "ses_x")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"messages":[]}` {
		t.Fatalf("the exec must hand opencode a file, not a pipe -- got %q", out)
	}
}

func fakeDocker(t *testing.T, ws string) {
	t.Helper()
	fakeBin(t, "docker", "#!/bin/sh\necho pipe-noise\nfor a in \"$@\"; do echo \"$a\"; done > "+ws+"/"+captureOutName+"\n")
}

func TestContainerCaptureUsesTheAttemptHomeWithoutCredentials(t *testing.T) {
	ws := t.TempDir()
	fakeDocker(t, ws)

	out, err := containerRuntime{workspace: ws, image: "img", env: []string{"ROUTINE_SECRET=secret"}}.exec("export", "ses_x")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(out)), "\n")
	joined := strings.Join(args, " ")
	if slices.Contains(args, "pipe-noise") {
		t.Fatalf("the exec must read the workspace file, never docker's stdout: %s", joined)
	}
	for _, want := range []string{
		"HOME=/work/" + attemptHomeName,
		"XDG_DATA_HOME=/work/.home/.local/share",
		"TMPDIR=/work/.runtmp",
		"-w", "/work", "img",
		"sh", "-c", `exec opencode "$@" > /work/` + captureOutName,
		"opencode", "export", "ses_x",
	} {
		if !slices.Contains(args, want) {
			t.Fatalf("missing %q in docker args: %s", want, joined)
		}
	}
	if slices.Contains(args, "ROUTINE_SECRET") {
		t.Fatalf("capture must not receive the routine's credentials: %s", joined)
	}
	if _, err := os.Stat(filepath.Join(ws, captureOutName)); !os.IsNotExist(err) {
		t.Fatalf("the landing file must be removed after the exec: %v", err)
	}
}

func TestContainerCaptureClearsAPlantedLandingFile(t *testing.T) {
	ws := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(ws, captureOutName)); err != nil {
		t.Fatal(err)
	}
	fakeDocker(t, ws)

	if _, err := (containerRuntime{workspace: ws, image: "img"}).exec("export", "ses_x"); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(victim); err != nil || string(raw) != "untouched" {
		t.Fatalf("a planted symlink must not route the exec's output elsewhere: %q (%v)", raw, err)
	}
}

func TestContainerCaptureIgnoresALandingFileSwappedMidExec(t *testing.T) {
	ws := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte(`{"host":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	landing := ws + "/" + captureOutName
	fakeBin(t, "docker", "#!/bin/sh\nprintf legit > "+landing+"\nrm "+landing+"\nln -s "+secret+" "+landing+"\n")

	out, err := (containerRuntime{workspace: ws, image: "img"}).exec("export", "ses_x")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "legit" {
		t.Fatalf("the exec must return what the container wrote, not what the swapped path points at: %q", out)
	}
}

func TestCaptureKeepsOnlyTheTailOfStderr(t *testing.T) {
	fakeBin(t, "opencode", `#!/bin/sh
i=0
while [ $i -lt 5000 ]; do echo "noise line $i" >&2; i=$((i+1)); done
echo "the real failure" >&2
exit 1
`)
	_, err := nativeRuntime{workspace: t.TempDir()}.exec("session", "list")
	if err == nil {
		t.Fatal("the exec must surface the failure")
	}
	if !strings.Contains(err.Error(), "the real failure") {
		t.Fatalf("the tail of stderr must survive, got %q", err.Error()[:min(len(err.Error()), 200)])
	}
	if len(err.Error()) > 16<<10 {
		t.Fatalf("kept stderr must be bounded, got %d bytes", len(err.Error()))
	}
}

func TestSuccessfulCaptureReapsItsDescendants(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	fakeBin(t, "opencode", "#!/bin/sh\nsh -c 'trap \"\" HUP; while :; do sleep 30; done' >/dev/null 2>&1 &\necho $! > "+pidfile+"\n")
	if _, err := (nativeRuntime{workspace: t.TempDir()}).exec("session", "list"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	for deadline := time.Now().Add(time.Second); ; time.Sleep(10 * time.Millisecond) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a descendant outlived a successful capture")
		}
	}
}

func parseEnv(t *testing.T, out string) map[string]string {
	t.Helper()
	env := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("unparsable line %q", line)
		}
		env[k] = v
	}
	return env
}
