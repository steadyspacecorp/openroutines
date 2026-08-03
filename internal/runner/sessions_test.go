package runner

import (
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
