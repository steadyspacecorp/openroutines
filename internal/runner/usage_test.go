package runner

import (
	"os"
	"path/filepath"
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
	if got := captureUsage(ws, nil); got != nil {
		t.Fatalf("no store should be nil, got %+v", got)
	}
	store := filepath.Join(ws, ".home", ".local", "share", "opencode", "storage", "message", "ses_x")
	writeMsg(t, store, "msg_1.json", `{"role":"assistant","modelID":"m","tokens":{"input":100,"output":20,"reasoning":5,"cache":{"read":7,"write":3}},"cost":0.01}`)
	writeMsg(t, store, "msg_2.json", `{"role":"assistant","tokens":{"input":50,"output":10,"reasoning":0,"cache":{"read":0,"write":0}},"cost":0.005}`)
	writeMsg(t, store, "msg_3.json", `{"role":"user","tokens":{"input":999,"output":999}}`)
	writeMsg(t, store, "msg_4.json", `not json`)
	u := captureUsage(ws, nil)
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
	u := captureUsage(t.TempDir(), oc)
	if u == nil || u.Input != 300 || u.Output != 40 || u.Reasoning != 8 || u.CacheRead != 11 || u.CacheWrite != 2 {
		t.Fatalf("export sums wrong: %+v", u)
	}
	if len(calls) != 2 {
		t.Fatalf("expected list then export, got %v", calls)
	}

	// Exec failure: fall back to legacy files (none here -> nil), never error.
	broken := func(...string) ([]byte, error) { return nil, os.ErrPermission }
	if got := captureUsage(t.TempDir(), broken); got != nil {
		t.Fatalf("broken exec should degrade to nil, got %+v", got)
	}

	// Empty session list (no session survived): legacy fallback still works.
	ws := t.TempDir()
	store := filepath.Join(ws, ".home", ".local", "share", "opencode", "storage", "message", "ses_x")
	writeMsg(t, store, "msg_1.json", `{"role":"assistant","tokens":{"input":9,"output":1,"reasoning":0,"cache":{"read":0,"write":0}},"cost":0}`)
	empty := func(...string) ([]byte, error) { return []byte(`[]`), nil }
	if got := captureUsage(ws, empty); got == nil || got.Input != 9 {
		t.Fatalf("legacy fallback after empty list failed: %+v", got)
	}
}
