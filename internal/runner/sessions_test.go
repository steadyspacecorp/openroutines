package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

func captureVia(t *testing.T, oc opencodeExec) Capture {
	t.Helper()
	exports, err := fetchSessions(oc, discardLog)
	if err != nil {
		t.Fatal(err)
	}
	return captureSessions(exports, discardLog)
}

func exportVia(t *testing.T, oc opencodeExec) {
	t.Helper()
	exports, err := fetchSessions(oc, discardLog)
	if err != nil {
		t.Fatal(err)
	}
	exportSessions(exports, testRoutine, testRunID, discardLog)
}

func exportedPath(t *testing.T, dir, sessionID string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*_"+testRoutine+"_"+testRunID+"_"+sessionID+".json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected one exported %s session, got %v (%v)", sessionID, matches, err)
	}
	return matches[0]
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
	switch {
	case u == nil:
		t.Fatal("expected usage")
	case u.Input != 150 || u.Output != 30 || u.Reasoning != 5 || u.CacheRead != 7 || u.CacheWrite != 3:
		t.Fatalf("sums wrong: %+v", u)
	case u.CostReported < 0.0149 || u.CostReported > 0.0151:
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
	oc := stubOpencode(t, `[{"id":"ses_x"}]`, map[string]string{"ses_x": `{"messages":"unreadable"}`})
	s := captureVia(t, oc)

	if s.Usage != nil {
		t.Fatalf("an unreadable session should degrade to nil usage, got %+v", s.Usage)
	}
	if s.Failure != "" {
		t.Fatalf("an unreadable session must claim nothing about the outcome, got %q", s.Failure)
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

// Wraps a stub so its first `count` answers to `args`
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

// Answers `session list` with the given ids and `export <id>`
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

const testRoutine = "check-in"
const testRunID = "run_x"

func TestExportDisabledWhenUnset(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv(EnvSessionDir, "")
	exports := []sessionExport{{id: "ses_a", raw: []byte("{}")}}
	exportSessions(exports, testRoutine, testRunID, discardLog)
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("disabled export must not write files, got %v (%v)", entries, err)
	}
}

func TestExportSavesEverySession(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvSessionDir, root)
	oc := stubOpencode(t, `[{"id":"ses_a"},{"id":"ses_b"}]`, map[string]string{
		"ses_a": `{"messages":[1]}`,
		"ses_b": `{"messages":[2]}`,
	})

	exportVia(t, oc)
	filenamePattern := regexp.MustCompile(`^\d{8}T\d{6}Z_check-in_run_x_ses_[ab]\.json$`)
	for id, want := range map[string]string{"ses_a": `{"messages":[1]}`, "ses_b": `{"messages":[2]}`} {
		path := exportedPath(t, root, id)
		filename := filepath.Base(path)
		if !filenamePattern.MatchString(filename) {
			t.Fatalf("unexpected session filename %q", filename)
		}
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != want {
			t.Fatalf("%s.json = %q (%v), want %q", id, raw, err, want)
		}
	}
}

// Exported sessions are verbatim, unscrubbed model history, so what lands
// is as sensitive as the credentials the routine could see: owner-only.
func TestExportWritesOwnerOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	t.Setenv(EnvSessionDir, root)
	exportVia(t, stubOpencode(t, `[{"id":"ses_a"}]`, map[string]string{"ses_a": "{}"}))
	path := exportedPath(t, root, "ses_a")
	for name, want := range map[string]os.FileMode{root: 0o700, path: 0o600} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s is %o, want %o", name, got, want)
		}
	}
}

func TestExportSkipsWhenAttemptLeftNoSessions(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvSessionDir, root)
	exportVia(t, stubOpencode(t, `[]`, nil))
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("an attempt without sessions must not leave files in operator storage, got %v (%v)", entries, err)
	}
}

func TestExportUsesSessionIDBase(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvSessionDir, root)
	planted := filepath.Join(filepath.Dir(root), "planted.json")
	oc := stubOpencode(t, `[{"id":"../../planted"}]`, map[string]string{"../../planted": `{"planted":true}`})

	exportVia(t, oc)
	if _, err := os.Stat(planted); !os.IsNotExist(err) {
		t.Fatal("an unsafe session id must never name a file outside operator storage")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), "_planted.json") {
		t.Fatalf("session id path must flatten into one exported file, got %v (%v)", entries, err)
	}
}

// Retries share a run id but leave distinct sessions. Flat operator storage
// retains both histories rather than erasing the first attempt's evidence.
func TestExportPreservesPreviousAttemptsSessions(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvSessionDir, root)
	exportVia(t, stubOpencode(t, `[{"id":"ses_first"}]`, map[string]string{"ses_first": "{}"}))
	exportVia(t, stubOpencode(t, `[{"id":"ses_second"}]`, map[string]string{"ses_second": "{}"}))
	if _, err := os.Stat(exportedPath(t, root, "ses_second")); err != nil {
		t.Fatalf("the retry's own sessions must land: %v", err)
	}
	if _, err := os.Stat(exportedPath(t, root, "ses_first")); err != nil {
		t.Fatalf("the previous attempt's session must remain: %v", err)
	}
}

func TestExportDoesNotFailWhenStorageIsUnwritable(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(blocked, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvSessionDir, blocked)
	oc := stubOpencode(t, `[{"id":"ses_a"}]`, map[string]string{"ses_a": "{}"})
	exportVia(t, oc)
}
