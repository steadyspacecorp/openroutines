package logging

import (
	"bytes"
	"log/slog"
	"testing"
)

func TestPassthroughDecoratesEachLine(t *testing.T) {
	var out bytes.Buffer
	Writer = &out
	w := NewPassthrough(slog.String("routine", "check-in"), slog.String("run_id", "run_x"))
	line := `timestamp=2026-08-03T19:42:06.412Z level=INFO run=c613738c message="creating instance" directory=/work`
	if _, err := w.Write([]byte(line + "\n")); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), line+" routine=check-in run_id=run_x\n"; got != want {
		t.Fatalf("passthrough = %q, want %q", got, want)
	}
}

func TestPassthroughQuotesLikeTheHandler(t *testing.T) {
	var out bytes.Buffer
	Writer = &out
	w := NewPassthrough(slog.String("routine", "my routine"))
	if _, err := w.Write([]byte("level=INFO message=hi\n")); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "level=INFO message=hi routine=\"my routine\"\n"; got != want {
		t.Fatalf("passthrough = %q, want %q", got, want)
	}
}

func TestPassthroughBuffersPartialLines(t *testing.T) {
	var out bytes.Buffer
	Writer = &out
	w := NewPassthrough(slog.String("run_id", "run_x"))
	for _, chunk := range []string{"level=INFO mess", "age=first\n\nlevel=WARN message=last"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := out.String(), "level=INFO message=first run_id=run_x\n"; got != want {
		t.Fatalf("only the completed line may pass through before Flush, got %q, want %q", got, want)
	}
	w.Flush()
	if got, want := out.String(), "level=INFO message=first run_id=run_x\nlevel=WARN message=last run_id=run_x\n"; got != want {
		t.Fatalf("Flush must emit the trailing line, got %q, want %q", got, want)
	}
}
