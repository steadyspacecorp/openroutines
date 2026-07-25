// Package scrub redacts injected secret values from a byte stream before it
// reaches logs. Defense in depth: exact-value matching only (see DESIGN.md
// "Credentials" rule 3) -- the primary protection is that undeclared secrets
// are never in the process at all.
package scrub

import (
	"bytes"
	"io"
	"strings"
)

// Writer replaces known secret values with [REDACTED:name] line by line.
type Writer struct {
	dst     io.Writer
	secrets map[string]string // name -> value
	buf     bytes.Buffer
}

// NewWriter wraps dst, redacting secret values line by line.
func NewWriter(dst io.Writer, secrets map[string]string) *Writer {
	return &Writer{dst: dst, secrets: secrets}
}

// maxBuffered caps the partial-line buffer: output with no newlines flushes
// through redaction in chunks instead of growing without bound. A secret
// split across the chunk boundary can evade redaction -- accepted; this is
// defense in depth, and an unbounded buffer is a memory hole.
const maxBuffered = 1 << 20

func (w *Writer) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			// Incomplete line: keep it buffered for the next write.
			w.buf.WriteString(line)
			break
		}
		if _, err := io.WriteString(w.dst, Redact(line, w.secrets)); err != nil {
			return len(p), err
		}
	}
	if w.buf.Len() > maxBuffered {
		w.Flush()
	}
	return len(p), nil
}

// Flush writes any buffered partial line, redacted.
func (w *Writer) Flush() {
	if w.buf.Len() > 0 {
		_, _ = io.WriteString(w.dst, Redact(w.buf.String(), w.secrets))
		w.buf.Reset()
	}
}

// Redact replaces every known secret value in s with [REDACTED:name].
func Redact(s string, secrets map[string]string) string {
	for name, value := range secrets {
		if value == "" {
			continue
		}
		s = strings.ReplaceAll(s, value, "[REDACTED:"+strings.ToUpper(name)+"]")
	}
	return s
}
