package runner

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeBin puts an executable named `name` on PATH for the test.
func fakeBin(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// reportEnv is a stand-in opencode that prints the environment it was
// handed instead of doing any work.
const reportEnv = `#!/bin/sh
echo "HOME=$HOME"
echo "XDG_CONFIG_HOME=$XDG_CONFIG_HOME"
echo "XDG_DATA_HOME=$XDG_DATA_HOME"
echo "HOME_ENTRIES=$(ls -A "$HOME" 2>/dev/null | tr '\n' ',')"
[ -f cwd-marker ] && echo "CWD=workspace"
`

// The capture step runs unsandboxed as a child of the supervisor, so its
// HOME must be a supervisor-owned empty directory -- never the attempt's
// own home, whose config dir opencode auto-loads plugins from.
func TestHostCaptureRunsWithAnEmptyHome(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "cwd-marker"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// What a prompt-injected routine would leave behind for capture to load.
	writeMsg(t, filepath.Join(ws, ".home", ".config", "opencode", "plugin"), "evil.js", "export const Evil = async () => ({})")
	fakeBin(t, "opencode", reportEnv)

	out, err := hostOpencode{workspace: ws}.sessions("session", "list")
	if err != nil {
		t.Fatal(err)
	}
	env := parseEnv(t, string(out))

	if env["HOME"] == "" || strings.HasPrefix(env["HOME"], ws) {
		t.Fatalf("HOME must be outside the attempt's workspace, got %q", env["HOME"])
	}
	if env["HOME_ENTRIES"] != "" {
		t.Fatalf("capture home must be empty, holds %q", env["HOME_ENTRIES"])
	}
	if want := filepath.Join(env["HOME"], ".config"); env["XDG_CONFIG_HOME"] != want {
		t.Fatalf("XDG_CONFIG_HOME = %q, want %q", env["XDG_CONFIG_HOME"], want)
	}
	if want := filepath.Join(ws, ".home", ".local", "share"); env["XDG_DATA_HOME"] != want {
		t.Fatalf("XDG_DATA_HOME = %q, want %q", env["XDG_DATA_HOME"], want)
	}
	if env["CWD"] != "workspace" {
		t.Fatal("capture must run in the workspace -- opencode scopes sessions to it")
	}
	if _, err := os.Stat(env["HOME"]); !os.IsNotExist(err) {
		t.Fatalf("capture home must be removed after the exec: %v", err)
	}
}

// The capture home comes from TMPDIR, so a TMPDIR inside the workspace
// would hand the attempt the home this exists to deny it. Fail closed:
// no exec at all, rather than one with an attempt-writable home.
func TestHostCaptureRefusesAHomeInsideTheWorkspace(t *testing.T) {
	ws := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	fakeBin(t, "opencode", "#!/bin/sh\ntouch "+marker+"\n")
	t.Setenv("TMPDIR", ws)

	if _, err := (hostOpencode{workspace: ws}).sessions("session", "list"); err == nil {
		t.Fatal("capture must refuse a home inside the workspace")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("capture must not exec opencode once the home check fails")
	}
	entries, err := os.ReadDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the rejected home must be cleaned up, workspace holds %v", entries)
	}
}

// truncateOnPipe stands in for opencode's real defect: its CLI calls
// process.exit() before an async stdout write drains, and stream writes to
// a pipe are asynchronous, so a large payload through a pipe arrives cut
// short with exit code 0 -- while writes to a file are synchronous and
// arrive whole. The stand-in cuts deterministically where the real race
// cuts sometimes: an exec that hands opencode a pipe fails these tests.
const truncateOnPipe = `#!/bin/sh
if [ -p /dev/stdout ]; then printf '{"messages":[{"in'; else printf '{"messages":[]}'; fi
`

func TestHostCaptureSurvivesOpencodesLossyPipeWrites(t *testing.T) {
	fakeBin(t, "opencode", truncateOnPipe)
	out, err := hostOpencode{workspace: t.TempDir()}.sessions("export", "ses_x")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"messages":[]}` {
		t.Fatalf("the exec must hand opencode a file, not a pipe -- got %q", out)
	}
}

func TestNativeCaptureSurvivesOpencodesLossyPipeWrites(t *testing.T) {
	fakeBin(t, "opencode", truncateOnPipe)
	out, err := nativeOpencode{workspace: t.TempDir()}.sessions("export", "ses_x")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"messages":[]}` {
		t.Fatalf("the exec must hand opencode a file, not a pipe -- got %q", out)
	}
}

// fakeDocker stands in for the docker client: it records its arguments in
// the workspace file the exec reads back -- the container's own stdout
// route is a pipe, so nothing the exec returns may come from there -- and
// says something on stdout to prove that stream is ignored.
func fakeDocker(t *testing.T, ws string) {
	t.Helper()
	fakeBin(t, "docker", "#!/bin/sh\necho pipe-noise\nfor a in \"$@\"; do echo \"$a\"; done > "+ws+"/"+captureOutName+"\n")
}

// The local-container variant re-enters the image; its empty home is a
// tmpfs outside /work, so the attempt cannot reach it either. opencode's
// stdout is redirected to a file inside the container, because a file is
// what defeats its lossy exit and docker's own stdout is a pipe end to end.
func TestContainerCaptureRunsWithAnEmptyHome(t *testing.T) {
	ws := t.TempDir()
	fakeDocker(t, ws)

	out, err := containerOpencode{workspace: ws, image: "img"}.sessions("export", "ses_x")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(out)), "\n")
	joined := strings.Join(args, " ")
	if slices.Contains(args, "pipe-noise") {
		t.Fatalf("the exec must read the workspace file, never docker's stdout: %s", joined)
	}
	if slices.Contains(args, "HOME=/work/"+attemptHomeName) {
		t.Fatalf("capture must not take its home from the mounted workspace: %s", joined)
	}
	for _, want := range []string{
		"HOME=" + captureHomeMount,
		"XDG_CONFIG_HOME=" + captureHomeMount + "/.config",
		"XDG_DATA_HOME=/work/.home/.local/share",
		"--tmpfs", captureHomeMount + ":mode=0777,exec",
		"-w", "/work", "img",
		"sh", "-c", `exec opencode "$@" > /work/` + captureOutName,
		"opencode", "export", "ses_x",
	} {
		if !slices.Contains(args, want) {
			t.Fatalf("missing %q in docker args: %s", want, joined)
		}
	}
	for i, a := range args {
		if a == "-v" && strings.HasSuffix(args[i+1], ":"+captureHomeMount) {
			t.Fatalf("the home must be a tmpfs, not a bind mount: %s", joined)
		}
	}
	if _, err := os.Stat(filepath.Join(ws, captureOutName)); !os.IsNotExist(err) {
		t.Fatalf("the landing file must be removed after the exec: %v", err)
	}
}

// The workspace is model-written, so a symlink the attempt planted at the
// landing path must not decide where the redirect writes or what the read
// returns.
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

	if _, err := (containerOpencode{workspace: ws, image: "img"}).sessions("export", "ses_x"); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(victim); err != nil || string(raw) != "untouched" {
		t.Fatalf("a planted symlink must not route the exec's output elsewhere: %q (%v)", raw, err)
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
