package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/routine"
)

func writeMsg(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Usage sums assistant messages only; absence is nil, never zero.
func TestCaptureUsage(t *testing.T) {
	ws := t.TempDir()
	if got := captureSession(ws, nil).Usage; got != nil {
		t.Fatalf("no store should be nil, got %+v", got)
	}
	store := filepath.Join(ws, ".home", ".local", "share", "opencode", "storage", "message", "ses_x")
	writeMsg(t, store, "msg_1.json", `{"role":"assistant","modelID":"m","tokens":{"input":100,"output":20,"reasoning":5,"cache":{"read":7,"write":3}},"cost":0.01}`)
	writeMsg(t, store, "msg_2.json", `{"role":"assistant","tokens":{"input":50,"output":10,"reasoning":0,"cache":{"read":0,"write":0}},"cost":0.005}`)
	writeMsg(t, store, "msg_3.json", `{"role":"user","tokens":{"input":999,"output":999}}`)
	writeMsg(t, store, "msg_4.json", `not json`)
	u := captureSession(ws, nil).Usage
	if u == nil {
		t.Fatal("expected usage")
	}
	if u.Input != 150 || u.Output != 30 || u.Reasoning != 5 || u.CacheRead != 7 || u.CacheWrite != 3 {
		t.Fatalf("sums wrong: %+v", u)
	}
	if u.CostReported < 0.0149 || u.CostReported > 0.0151 {
		t.Fatalf("cost wrong: %v", u.CostReported)
	}
}

// How the session ended is read from the same record usage comes from.
// A claim is only made on positive evidence: an errored message, or a
// session whose runtime reported finish reasons and never finished a turn.
func TestCaptureSessionFailure(t *testing.T) {
	cases := []struct {
		name     string
		messages []string
		want     string
	}{
		{"clean turn", []string{
			`{"role":"assistant","finish":"stop"}`,
		}, ""},
		{"tool loop then a clean turn", []string{
			`{"role":"assistant","finish":"tool-calls"}`,
			`{"role":"assistant","finish":"stop"}`,
		}, ""},
		{"no finish reported", []string{
			`{"role":"assistant"}`,
		}, ""},
		{"no session at all", nil, ""},
		{"ended mid-turn", []string{
			`{"role":"assistant","finish":"tool-calls"}`,
		}, "tool-calls"},
		{"errored message", []string{
			`{"role":"assistant","finish":"stop","error":{"name":"ProviderAuthError","data":{"message":"Unauthorized"}}}`,
		}, "Unauthorized"},
		{"errored message without data", []string{
			`{"role":"assistant","error":{"name":"MessageAbortedError"}}`,
		}, "MessageAbortedError"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			store := filepath.Join(ws, ".home", ".local", "share", "opencode", "storage", "message", "ses_x")
			for i, m := range tc.messages {
				writeMsg(t, store, fmt.Sprintf("msg_%02d.json", i), m)
			}
			got := captureSession(ws, nil).Failure
			if tc.want == "" && got != "" {
				t.Fatalf("expected no claim, got %q", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("failure %q should mention %q", got, tc.want)
			}
		})
	}
}

// The record carries model, effort, and per-attempt tokens when present,
// and omits them -- never zeroes -- when the runtime didn't report.
func TestRecordJSONUsage(t *testing.T) {
	r := &routine.Routine{Name: "x"}
	meta := Meta{RunID: "run_1"}

	bare := recordJSON(r, meta, 1, &ExecResult{Outcome: Completed}, false)
	for _, absent := range []string{"tokens", "model", "effort", "cost_reported"} {
		if strings.Contains(bare, absent) {
			t.Fatalf("unreported %s must be omitted, got %s", absent, bare)
		}
	}

	res := &ExecResult{Outcome: Completed, Model: "fake/model", Effort: "high",
		Usage: &Usage{Input: 100, Output: 20, Reasoning: 5, CacheRead: 7, CacheWrite: 3, CostReported: 0.01}}
	rec := recordJSON(r, meta, 1, res, false)
	for _, want := range []string{`"model":"fake/model"`, `"effort":"high"`, `"input":100`, `"output":20`,
		`"reasoning":5`, `"cache_read":7`, `"cache_write":3`, `"cost_reported":0.01`} {
		if !strings.Contains(rec, want) {
			t.Fatalf("record missing %s: %s", want, rec)
		}
	}
}

// The export path: ask opencode for the session, sum its assistant
// messages; any exec failure falls back to the legacy files.
func TestCaptureViaExport(t *testing.T) {
	calls := [][]string{}
	oc := func(args ...string) ([]byte, error) {
		calls = append(calls, args)
		switch args[0] {
		case "session":
			return []byte(`[{"id":"ses_abc","title":"t"}]`), nil
		case "export":
			if args[1] != "ses_abc" {
				t.Fatalf("export called with %q", args[1])
			}
			return []byte(`{"info":{"id":"ses_abc"},"messages":[
				{"info":{"role":"user"}},
				{"info":{"role":"assistant","tokens":{"input":200,"output":30,"reasoning":8,"cache":{"read":11,"write":2}},"cost":0.02}},
				{"info":{"role":"assistant","tokens":{"input":100,"output":10,"reasoning":0,"cache":{"read":0,"write":0}},"cost":0.01}}
			]}`), nil
		}
		t.Fatalf("unexpected call %v", args)
		return nil, nil
	}
	u := captureSession(t.TempDir(), oc).Usage
	if u == nil || u.Input != 300 || u.Output != 40 || u.Reasoning != 8 || u.CacheRead != 11 || u.CacheWrite != 2 {
		t.Fatalf("export sums wrong: %+v", u)
	}
	if len(calls) != 2 {
		t.Fatalf("expected list then export, got %v", calls)
	}

	// Exec failure: fall back to legacy files (none here -> nil), never error.
	broken := func(...string) ([]byte, error) { return nil, os.ErrPermission }
	if got := captureSession(t.TempDir(), broken).Usage; got != nil {
		t.Fatalf("broken exec should degrade to nil, got %+v", got)
	}

	// Empty session list (no session survived): legacy fallback still works.
	ws := t.TempDir()
	store := filepath.Join(ws, ".home", ".local", "share", "opencode", "storage", "message", "ses_x")
	writeMsg(t, store, "msg_1.json", `{"role":"assistant","tokens":{"input":9,"output":1,"reasoning":0,"cache":{"read":0,"write":0}},"cost":0}`)
	empty := func(...string) ([]byte, error) { return []byte(`[]`), nil }
	if got := captureSession(ws, empty).Usage; got == nil || got.Input != 9 {
		t.Fatalf("legacy fallback after empty list failed: %+v", got)
	}
}

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

	out, err := hostOpencodeExec(ws)("session", "list")
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

	if _, err := hostOpencodeExec(ws)("session", "list"); err == nil {
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

// The local-container variant re-enters the image; its empty home is a
// tmpfs outside /work, so the attempt cannot reach it either.
func TestContainerCaptureRunsWithAnEmptyHome(t *testing.T) {
	ws := t.TempDir()
	fakeBin(t, "docker", "#!/bin/sh\nfor a in \"$@\"; do echo \"$a\"; done\n")

	out, err := containerOpencodeExec(ws, "img")("export", "ses_x")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(out)), "\n")
	joined := strings.Join(args, " ")
	if slices.Contains(args, "HOME=/work/"+attemptHomeName) {
		t.Fatalf("capture must not take its home from the mounted workspace: %s", joined)
	}
	for _, want := range []string{
		"HOME=" + captureHomeMount,
		"XDG_CONFIG_HOME=" + captureHomeMount + "/.config",
		"XDG_DATA_HOME=/work/.home/.local/share",
		"--tmpfs", captureHomeMount + ":mode=0777,exec",
		"-w", "/work", "img", "opencode", "export", "ses_x",
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
