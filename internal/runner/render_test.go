package runner

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/scrub"
)

func renderEvents(t *testing.T, lines ...string) string {
	t.Helper()
	var out strings.Builder
	r := newRenderer(&out)
	for _, l := range lines {
		if _, err := r.Write([]byte(l + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	r.Flush()
	return out.String()
}

func toolEvent(t *testing.T, tool, title, output string, exit int) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type": "tool_use",
		"part": map[string]any{
			"type": "tool", "tool": tool,
			"state": map[string]any{
				"status": "completed", "title": title, "output": output,
				"metadata": map[string]any{"exit": exit},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The routine's own text prints whole; a failed tool keeps its bounded output
// tail -- where the diagnostic lands -- instead of dumping the whole result.
func TestRendererBoundsFailedToolOutput(t *testing.T) {
	big := strings.Repeat("noise\n", 100_000) + "the part that matters"
	out := renderEvents(t,
		`{"type":"step_start","part":{}}`,
		`{"type":"text","part":{"type":"text","text":"Checked the failed job."}}`,
		toolEvent(t, "bash", "gh run view --log-failed", big, 1),
	)
	if !strings.Contains(out, "Checked the failed job.") {
		t.Fatalf("routine text should print whole:\n%s", out)
	}
	if !strings.Contains(out, "[tool bash] gh run view --log-failed") || !strings.Contains(out, "exit 1") {
		t.Fatalf("tool call should render as a summary line:\n%s", out)
	}
	if !strings.Contains(out, "the part that matters") {
		t.Fatalf("the output tail -- where errors land -- should survive:\n%s", out)
	}
	if len(out) > 4*toolOutputBytes {
		t.Fatalf("600KB of tool output should render bounded, got %d bytes", len(out))
	}
	if strings.Contains(out, "step_start") {
		t.Fatalf("transcript structure should not render:\n%s", out)
	}
}

// A successful tool is an audit summary at info, not a copy of everything it
// read or fetched. The size says detail existed and debug is where to find it.
func TestRendererSuppressesSuccessfulToolOutput(t *testing.T) {
	out := renderEvents(t, toolEvent(t, "read", "work/memory/context.md", "private document contents", 0))
	if !strings.Contains(out, "[tool read] work/memory/context.md") || !strings.Contains(out, "25B output suppressed") {
		t.Fatalf("successful tool should retain a useful summary:\n%s", out)
	}
	if strings.Contains(out, "private document contents") {
		t.Fatalf("successful tool output leaked into the info log:\n%s", out)
	}
}

// Scrubbing happens before truncation: a secret sitting across the
// truncation boundary must still redact, never leak half of itself.
func TestRendererScrubsBeforeTruncating(t *testing.T) {
	secret := "tok-0123456789abcdef0123456789abcdef" // gitleaks:allow -- synthetic fixture
	output := strings.Repeat("x", toolOutputBytes-10) + secret + strings.Repeat("y", toolOutputBytes)
	scrub.Register(map[string]string{"api_token": secret})
	out := renderEvents(t, toolEvent(t, "bash", "echo $API_TOKEN", output, 0))
	if strings.Contains(out, secret) || strings.Contains(out, secret[:12]) {
		t.Fatalf("secret (or a truncated half of it) leaked:\n%s", out)
	}
	if !strings.Contains(out, "output suppressed") {
		t.Fatalf("successful secret-bearing output should reduce to a summary:\n%s", out)
	}
}

// Anything the renderer does not recognize passes through bounded: a
// plain-text line from a fake or future opencode, an event schema the
// framework doesn't know. Degrade, never fail the run.
func TestRendererPassesUnknownLinesThroughBounded(t *testing.T) {
	out := renderEvents(t,
		"plain text from an older opencode",
		`{"type":"future_event","payload":"`+strings.Repeat("z", 3*passthroughBytes)+`"}`,
	)
	if !strings.Contains(out, "plain text from an older opencode") {
		t.Fatalf("plain lines should pass through:\n%s", out)
	}
	if !strings.Contains(out, "truncated]") || len(out) > 2*passthroughBytes {
		t.Fatalf("unknown events should be bounded, got %d bytes:\n%.200s", len(out), out)
	}
}

// Error events keep their payload: the provider-auth hint matches on this
// text, and it is what an operator reads when a run dies.
func TestRendererKeepsErrorEvents(t *testing.T) {
	out := renderEvents(t, `{"type":"error","error":{"name":"APIError","data":{"message":"API key is invalid.","statusCode":401}}}`)
	if !authFailurePattern.MatchString(out) {
		t.Fatalf("rendered error should still classify as an auth failure:\n%s", out)
	}
}

// A single event line beyond the buffer cap is suppressed with a notice
// instead of ballooning memory, and the stream recovers on the next line.
func TestRendererSuppressesOversizedEvents(t *testing.T) {
	var out strings.Builder
	r := newRenderer(&out)
	huge := []byte(toolEvent(t, "bash", "cat warandpeace", strings.Repeat("a", maxEventBytes+(1<<20)), 0) + "\n")
	// A pipe delivers a line this size in chunks, never one Write.
	for len(huge) > 0 {
		n := min(len(huge), 64<<10)
		if _, err := r.Write(huge[:n]); err != nil {
			t.Fatal(err)
		}
		huge = huge[n:]
	}
	if _, err := r.Write([]byte(`{"type":"text","part":{"text":"still here"}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	r.Flush()
	if !strings.Contains(out.String(), "suppressed") || !strings.Contains(out.String(), "still here") {
		t.Fatalf("oversized event should be suppressed and the stream recover:\n%.300s", out.String())
	}
	if len(out.String()) > 1024 {
		t.Fatalf("suppression should be a notice, got %d bytes", len(out.String()))
	}
}

// A failed tool call renders its error text, not silence.
func TestRendererShowsToolErrors(t *testing.T) {
	raw := (`{"type":"tool_use","part":{"tool":"webfetch","state":{"status":"error","title":"fetch docs","error":"connect: connection refused"}}}`)
	out := renderEvents(t, raw)
	if !strings.Contains(out, "connection refused") || !strings.Contains(out, "error") {
		t.Fatalf("tool failure should render its error:\n%s", out)
	}
}

// Concurrent runs share one stdout, so every log line carries its routine's
// name -- and a line split across writes is held and emitted whole, never as
// an unattributed fragment.
func TestPrefixWriterAttributesWholeLines(t *testing.T) {
	var out bytes.Buffer
	w := newPrefixWriter(&out, "check-in")
	if _, err := w.Write([]byte("first li")); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("a partial line escaped before its newline: %q", out.String())
	}
	if _, err := w.Write([]byte("ne\nsecond line\ntail")); err != nil {
		t.Fatal(err)
	}
	w.Flush()
	want := "check-in | first line\ncheck-in | second line\ncheck-in | tail\n"
	if out.String() != want {
		t.Fatalf("prefixed output = %q, want %q", out.String(), want)
	}
	before := out.Len()
	w.Flush() // nothing held: idle
	if out.Len() != before {
		t.Fatal("an empty flush wrote something")
	}
}
