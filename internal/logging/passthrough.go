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

type Passthrough struct {
	dst    io.Writer
	suffix string
	buf    []byte
}

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

func (w *Passthrough) Flush() {
	if len(w.buf) > 0 {
		w.emit(w.buf)
		w.buf = nil
	}
}

func (w *Passthrough) emit(line []byte) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w.dst, "%s%s\n", trimmed, w.suffix)
}

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
