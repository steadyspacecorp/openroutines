package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// stubOpencode answers `session list` with the given ids and `export <id>`
// from the exports map; a missing id is an export failure.
func stubOpencode(t *testing.T, list string, exports map[string]string) opencodeExec {
	t.Helper()
	return func(args ...string) ([]byte, error) {
		switch args[0] {
		case "session":
			return []byte(list), nil
		case "export":
			raw, ok := exports[args[1]]
			if !ok {
				return nil, errors.New("no such session")
			}
			return []byte(raw), nil
		}
		return nil, fmt.Errorf("unexpected opencode call %v", args)
	}
}

var testMeta = Meta{RunID: "run_x", AttemptID: "attempt_01"}

func TestExportDisabledWhenUnset(t *testing.T) {
	t.Setenv(EnvSessionDir, "")
	oc := func(args ...string) ([]byte, error) {
		t.Fatalf("no designated session dir must mean no opencode call, got %v", args)
		return nil, nil
	}
	if dir := exportSessions(testMeta, oc); dir != "" {
		t.Fatalf("export must be a no-op when disabled, got %q", dir)
	}
}

func TestExportSavesEverySession(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvSessionDir, root)
	oc := stubOpencode(t, `[{"id":"ses_a"},{"id":"ses_b"}]`, map[string]string{
		"ses_a": `{"messages":[1]}`,
		"ses_b": `{"messages":[2]}`,
	})

	dir := exportSessions(testMeta, oc)
	if dir != filepath.Join(root, "run_x.attempt_01") {
		t.Fatalf("unexpected session dir %q", dir)
	}
	for id, want := range map[string]string{"ses_a": `{"messages":[1]}`, "ses_b": `{"messages":[2]}`} {
		raw, err := os.ReadFile(filepath.Join(dir, id+".json"))
		if err != nil || string(raw) != want {
			t.Fatalf("%s.json = %q (%v), want %q", id, raw, err, want)
		}
	}
}

// Exported sessions are verbatim, unscrubbed model history, so what lands
// is as sensitive as the credentials the routine could see: owner-only.
func TestExportWritesOwnerOnly(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvSessionDir, root)
	dir := exportSessions(testMeta, stubOpencode(t, `[{"id":"ses_a"}]`, map[string]string{"ses_a": "{}"}))
	for path, want := range map[string]os.FileMode{
		dir:                              0o700,
		filepath.Join(dir, "ses_a.json"): 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s is %o, want %o", path, got, want)
		}
	}
}

func TestExportSkipsWhenAttemptLeftNoSessions(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvSessionDir, root)
	if dir := exportSessions(testMeta, stubOpencode(t, `[]`, nil)); dir != "" {
		t.Fatalf("no sessions must mean no session dir, got %q", dir)
	}
	if _, err := os.Stat(filepath.Join(root, "run_x.attempt_01")); !os.IsNotExist(err) {
		t.Fatal("an attempt without sessions must not leave a directory in operator storage")
	}
}

// The session store is model-writable, so the ids that come back from it
// are not trusted as filenames: one that could climb out of the attempt's
// directory exports nothing.
func TestExportRefusesUnsafeSessionIDs(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvSessionDir, root)
	planted := filepath.Join(root, "planted.json")
	oc := stubOpencode(t, `[{"id":"../planted"}]`, map[string]string{"../planted": "stolen"})

	if dir := exportSessions(testMeta, oc); dir != "" {
		t.Fatalf("an export that landed nothing must report nothing preserved, got %q", dir)
	}
	if _, err := os.Stat(planted); !os.IsNotExist(err) {
		t.Fatal("an unsafe session id must never name a file outside the attempt's directory")
	}
	if _, err := os.Stat(filepath.Join(root, "run_x.attempt_01")); !os.IsNotExist(err) {
		t.Fatal("an export that landed nothing must not leave a directory in operator storage")
	}
}

// An export that fails partway still names its directory: the record points
// at what survived, and the log carries the warning.
func TestExportNamesAPartialExport(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvSessionDir, root)
	oc := stubOpencode(t, `[{"id":"ses_a"},{"id":"ses_gone"}]`, map[string]string{"ses_a": "{}"})

	dir := exportSessions(testMeta, oc)
	if dir != filepath.Join(root, "run_x.attempt_01") {
		t.Fatalf("a partial export must still name its directory, got %q", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "ses_a.json")); err != nil {
		t.Fatalf("what did export must still land: %v", err)
	}
}

// A canceled or lease-lost attempt hands its number back, so the retry
// exports into the directory the first attempt already filled. One
// directory names one attempt's sessions, never two merged.
func TestExportReplacesAPreviousAttemptsSessions(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvSessionDir, root)
	if dir := exportSessions(testMeta, stubOpencode(t, `[{"id":"ses_first"}]`, map[string]string{"ses_first": "{}"})); dir == "" {
		t.Fatal("the first attempt must export cleanly")
	}
	dir := exportSessions(testMeta, stubOpencode(t, `[{"id":"ses_second"}]`, map[string]string{"ses_second": "{}"}))
	if _, err := os.Stat(filepath.Join(dir, "ses_second.json")); err != nil {
		t.Fatalf("the retry's own sessions must land: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ses_first.json")); !os.IsNotExist(err) {
		t.Fatal("a retry must not merge the previous attempt's sessions into its own directory")
	}
}

func TestExportDegradesWhenStorageUnwritable(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(blocked, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvSessionDir, blocked)
	oc := stubOpencode(t, `[{"id":"ses_a"}]`, map[string]string{"ses_a": "{}"})
	if dir := exportSessions(testMeta, oc); dir != "" {
		t.Fatalf("broken operator storage must degrade to nothing preserved, got %q", dir)
	}
}

func TestExportDegradesWhenListingFails(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvSessionDir, root)
	oc := func(...string) ([]byte, error) { return nil, errors.New("no daemon") }
	if dir := exportSessions(testMeta, oc); dir != "" {
		t.Fatalf("an unlistable store must degrade to nothing preserved, got %q", dir)
	}
}
