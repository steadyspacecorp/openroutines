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

func NewWriter(dst io.Writer, secrets map[string]string) *Writer {
	return &Writer{dst: dst, secrets: secrets}
}

func (w *Writer) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			// Incomplete line: keep it buffered for the next write.
			w.buf.WriteString(line)
			break
		}
		if _, err := io.WriteString(w.dst, w.redact(line)); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

// Flush writes any buffered partial line, redacted.
func (w *Writer) Flush() {
	if w.buf.Len() > 0 {
		io.WriteString(w.dst, w.redact(w.buf.String()))
		w.buf.Reset()
	}
}

func (w *Writer) redact(s string) string {
	for name, value := range w.secrets {
		if value == "" {
			continue
		}
		s = strings.ReplaceAll(s, value, "[REDACTED:"+strings.ToUpper(name)+"]")
	}
	return s
}
