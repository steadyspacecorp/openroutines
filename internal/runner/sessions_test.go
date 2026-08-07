package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/scrub"
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

// sessionOf answers the capture surface with one session holding the given
// message records.
func sessionOf(t *testing.T, msgs ...string) opencodeExec {
	t.Helper()
	infos := make([]string, len(msgs))
	for i, m := range msgs {
		infos[i] = `{"info":` + m + `}`
	}
	return stubOpencode(t, `[{"id":"ses_x"}]`, map[string]string{
		"ses_x": `{"messages":[` + strings.Join(infos, ",") + `]}`,
	})
}

// captureVia fetches and captures in one step, the way the runner composes
// them.
func captureVia(t *testing.T, oc opencodeExec) Capture {
	t.Helper()
	exports, err := fetchSessions(oc, discardLog)
	return captureSessions(exports, err, discardLog)
}

// exportVia fetches and exports in one step, the way the runner composes
// them.
func exportVia(t *testing.T, oc opencodeExec) string {
	t.Helper()
	exports, err := fetchSessions(oc, discardLog)
	return exportSessions(testMeta, exports, err, discardLog)
}

// Usage sums assistant messages only; absence is nil, never zero.
func TestCaptureUsage(t *testing.T) {
	if got := captureVia(t, stubOpencode(t, `[]`, nil)).Usage; got != nil {
		t.Fatalf("no sessions should be nil, got %+v", got)
	}
	oc := sessionOf(t,
		`{"role":"assistant","modelID":"m","tokens":{"input":100,"output":20,"reasoning":5,"cache":{"read":7,"write":3}},"cost":0.01}`,
		`{"role":"assistant","tokens":{"input":50,"output":10,"reasoning":0,"cache":{"read":0,"write":0}},"cost":0.005}`,
		`{"role":"user","tokens":{"input":999,"output":999}}`,
	)
	u := captureVia(t, oc).Usage
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
			got := captureVia(t, sessionOf(t, tc.messages...)).Failure
			if tc.want == "" && got != "" {
				t.Fatalf("expected no claim, got %q", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("failure %q should mention %q", got, tc.want)
			}
		})
	}
}

// The failure claim quotes a model-writable record, and it outlives the
// mint registration that would otherwise redact it downstream (events, the
// run record, the manual echo) -- so it must leave the lift already
// redacted, while the registration is still live.
func TestCaptureSessionFailureIsRedacted(t *testing.T) {
	release := scrub.RegisterEphemeral("github_app bearer (gh)", "tok-sensitive-123")
	defer release()
	oc := sessionOf(t, `{"role":"assistant","error":{"name":"ProviderAuthError","data":{"message":"401 for token tok-sensitive-123"}}}`)
	got := captureVia(t, oc).Failure
	if strings.Contains(got, "tok-sensitive-123") || !strings.Contains(got, "[REDACTED:") {
		t.Fatalf("session-derived failure text must be lifted redacted, got %q", got)
	}
}

// A broken capture surface degrades to an empty record -- no usage, no
// outcome claim -- never an error: bookkeeping must not fail a run.
func TestCaptureFailsOpen(t *testing.T) {
	broken := func(...string) ([]byte, error) { return nil, os.ErrPermission }

	s := captureVia(t, broken)

	if s.Usage != nil {
		t.Fatalf("broken exec should degrade to nil usage, got %+v", s.Usage)
	}
	if s.Failure != "" {
		t.Fatalf("broken exec must claim nothing about the outcome, got %q", s.Failure)
	}
}

// The capture must sum every session the attempt left, not only the most
// recent: the fresh home normally holds exactly one, but nothing enforces
// that, and a partial sum is a silently wrong usage record.
func TestCaptureSumsEverySession(t *testing.T) {
	oc := stubOpencode(t, `[{"id":"ses_a"},{"id":"ses_b"}]`, map[string]string{
		"ses_a": `{"messages":[{"info":{"role":"assistant","tokens":{"input":100,"output":20,"reasoning":6,"cache":{"read":9,"write":4}},"cost":0.01}}]}`,
		"ses_b": `{"messages":[{"info":{"role":"assistant","tokens":{"input":50,"output":10,"reasoning":0,"cache":{"read":0,"write":0}},"cost":0.005}}]}`,
	})

	s := captureVia(t, oc)

	if s.Failure != "" {
		t.Fatalf("unexpected failure: %q", s.Failure)
	}
	u := s.Usage
	if u == nil || u.Input != 150 || u.Output != 30 || u.Reasoning != 6 || u.CacheRead != 9 || u.CacheWrite != 4 {
		t.Fatalf("capture must sum every session, got %+v", u)
	}
}

// The outcome is judged from the most significant session alone: the first
// listed, which `session list` orders most-recently-updated first, is the
// session that acted last. A clean ending in some older session must not
// vouch for the one that actually held the run -- while usage still sums
// them all.
func TestCaptureJudgesTheMostRecentSession(t *testing.T) {
	oc := stubOpencode(t, `[{"id":"ses_live"},{"id":"ses_side"}]`, map[string]string{
		"ses_live": `{"messages":[{"info":{"role":"assistant","tokens":{"input":100,"output":20,"reasoning":0,"cache":{"read":0,"write":0}},"cost":0.01,"finish":"tool-calls"}}]}`,
		"ses_side": `{"messages":[{"info":{"role":"assistant","tokens":{"input":50,"output":10,"reasoning":0,"cache":{"read":0,"write":0}},"cost":0.005,"finish":"stop"}}]}`,
	})

	s := captureVia(t, oc)

	if !strings.Contains(s.Failure, "tool-calls") {
		t.Fatalf("an older session's clean end must not mask the latest session's unfinished turn, got %q", s.Failure)
	}
	if u := s.Usage; u == nil || u.Input != 150 || u.Output != 30 {
		t.Fatalf("usage must still sum every session, got %+v", u)
	}
}

// truncatingOpencode wraps a stub so its first `count` answers to `args`
// come back cut in half with a clean exit -- the shape of opencode's lossy
// stdout (see completeJSON).
func truncatingOpencode(oc opencodeExec, count int, args ...string) opencodeExec {
	return func(got ...string) ([]byte, error) {
		raw, err := oc(got...)
		if err == nil && count > 0 && strings.Join(got, " ") == strings.Join(args, " ") {
			count--
			return raw[:len(raw)/2], nil
		}
		return raw, err
	}
}

// A truncated document with a clean exit is opencode's lossy stdout, not
// an answer: one fresh invocation is expected to return the whole thing.
func TestFetchRetriesTruncatedJSON(t *testing.T) {
	for name, args := range map[string][]string{
		"list":   {"session", "list", "--format", "json"},
		"export": {"export", "ses_x"},
	} {
		t.Run(name, func(t *testing.T) {
			oc := truncatingOpencode(sessionOf(t, `{"role":"assistant","finish":"stop"}`), 1, args...)
			exports, err := fetchSessions(oc, discardLog)
			if err != nil {
				t.Fatal(err)
			}
			if len(exports) != 1 || !strings.Contains(string(exports[0].raw), "stop") {
				t.Fatalf("the retry must return the whole document, got %+v", exports)
			}
		})
	}
}

func TestFetchGivesUpOnPersistentTruncation(t *testing.T) {
	oc := truncatingOpencode(sessionOf(t, `{"role":"assistant"}`), 2, "export", "ses_x")
	if _, err := fetchSessions(oc, discardLog); err == nil {
		t.Fatal("a document truncated twice must surface as an error, not as data")
	}
}

// A session that could not be fetched empties the whole capture -- a
// partial sum is a silently wrong usage record -- while export still
// preserves the sessions that did arrive.
func TestPartialFetchEmptiesCaptureButStillExports(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvSessionDir, root)
	oc := stubOpencode(t, `[{"id":"ses_a"},{"id":"ses_gone"}]`, map[string]string{
		"ses_a": `{"messages":[{"info":{"role":"assistant","tokens":{"input":9,"output":9,"reasoning":0,"cache":{"read":0,"write":0}}}}]}`,
	})

	exports, err := fetchSessions(oc, discardLog)
	if err == nil {
		t.Fatal("a fetch that lost a session must say so")
	}
	if s := captureSessions(exports, err, discardLog); s.Usage != nil || s.Failure != "" {
		t.Fatalf("a partial fetch must empty the capture, got %+v", s)
	}
	dir := exportSessions(testMeta, exports, err, discardLog)
	if _, statErr := os.Stat(filepath.Join(dir, "ses_a.json")); statErr != nil {
		t.Fatalf("the sessions that arrived must still be preserved: %v", statErr)
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
	exports := []sessionExport{{id: "ses_a", raw: []byte("{}")}}
	if dir := exportSessions(testMeta, exports, nil, discardLog); dir != "" {
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

	dir := exportVia(t, oc)
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
	dir := exportVia(t, stubOpencode(t, `[{"id":"ses_a"}]`, map[string]string{"ses_a": "{}"}))
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
	if dir := exportVia(t, stubOpencode(t, `[]`, nil)); dir != "" {
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
	oc := stubOpencode(t, `[{"id":"../planted"}]`, map[string]string{"../planted": `{"planted":true}`})

	if dir := exportVia(t, oc); dir != "" {
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

	dir := exportVia(t, oc)
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
	if dir := exportVia(t, stubOpencode(t, `[{"id":"ses_first"}]`, map[string]string{"ses_first": "{}"})); dir == "" {
		t.Fatal("the first attempt must export cleanly")
	}
	dir := exportVia(t, stubOpencode(t, `[{"id":"ses_second"}]`, map[string]string{"ses_second": "{}"}))
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
	if dir := exportVia(t, oc); dir != "" {
		t.Fatalf("broken operator storage must degrade to nothing preserved, got %q", dir)
	}
}

func TestExportDegradesWhenListingFails(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvSessionDir, root)
	oc := func(...string) ([]byte, error) { return nil, errors.New("no daemon") }
	if dir := exportVia(t, oc); dir != "" {
		t.Fatalf("an unlistable store must degrade to nothing preserved, got %q", dir)
	}
}
