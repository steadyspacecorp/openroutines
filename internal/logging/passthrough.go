package logging

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
)

// Passthrough passes an external process's already-formatted (logfmt) log
// lines through one at a time, appending fixed attributes -- no parsing
// needed, since logfmt is just key=value pairs and each line keeps the
// emitting process's own timestamp, level, and fields. Can't go through
// slog itself: a handler renders a Record, and there's no seam for adopting
// a line that's already rendered.
type Passthrough struct {
	dst    io.Writer
	suffix string
	buf    []byte
}

// NewPassthrough decorates every line written to it with attrs and passes
// it to Writer, the process's log destination. The attrs are rendered
// once, by slog's own TextHandler, so their quoting matches the process
// logger's records exactly.
func NewPassthrough(attrs ...slog.Attr) *Passthrough {
	return &Passthrough{dst: liveWriter{}, suffix: renderAttrs(attrs)}
}

func (w *Passthrough) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		w.emit(w.buf[:i])
		w.buf = w.buf[i+1:]
	}
}

// Flush emits a trailing line the stream ended without terminating.
func (w *Passthrough) Flush() {
	if len(w.buf) > 0 {
		w.emit(w.buf)
		w.buf = nil
	}
}

// Writes one decorated line in a single Write call, so concurrent streams
// sharing dst interleave at line granularity -- the same grain a slog
// handler writes at. A write error is swallowed: the log destination
// failing must not fail the process being logged.
func (w *Passthrough) emit(line []byte) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w.dst, "%s%s\n", trimmed, w.suffix)
}

// Formats attrs through a TextHandler with the record built-ins elided (a
// zero time is omitted by the handler on its own), leaving only the
// key=value pairs to append.
func renderAttrs(attrs []slog.Attr) string {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && (a.Key == slog.LevelKey || a.Key == slog.MessageKey) {
				return slog.Attr{}
			}
			return a
		},
	})
	rec := slog.NewRecord(time.Time{}, slog.LevelInfo, "", 0)
	rec.AddAttrs(attrs...)
	_ = h.Handle(context.Background(), rec)
	rendered := strings.TrimSuffix(buf.String(), "\n")
	if rendered == "" {
		return ""
	}
	return " " + rendered
}
