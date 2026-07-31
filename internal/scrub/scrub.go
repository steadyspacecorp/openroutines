// Package scrub redacts secret values from text and byte streams before
// they leave the process. Defense in depth: exact-value matching only (see
// design decision "Credentials", rule 3) -- the primary protection is that
// undeclared secrets are never in the process at all.
//
// There is one process-wide registry. Code that materializes a secret --
// loading a key, decrypting the store, minting a token -- calls Register;
// every consumer (the log writer, the run output stream, memory appends)
// redacts from the same set. Nothing downstream decides whether its text
// needs scrubbing, because a value can only leak from a process that
// materialized it, and materializing is what registers it.
package scrub

import (
	"bytes"
	"io"
	"maps"
	"strings"
	"sync/atomic"
)

// process is the registry: a concurrency-safe map where readers snapshot
// the current state through an atomic pointer while writers swap in a
// rebuilt copy. Trigger polls register bearer material mid-flight while run
// goroutines redact through writers that hold the set -- mutating a plain
// map under those readers is a fatal runtime error, not just a race.
var process atomic.Pointer[map[string]string]

func init() {
	process.Store(&map[string]string{})
}

// Register adds secret values to the process registry, keyed by the name
// that appears in the redaction marker. A re-registered name overwrites
// rather than accumulates, so repeated materialization of the same
// credential keeps the registry bounded.
func Register(values map[string]string) {
	for {
		old := process.Load()
		next := make(map[string]string, len(*old)+len(values))
		maps.Copy(next, *old)
		maps.Copy(next, values)
		if process.CompareAndSwap(old, &next) {
			return
		}
	}
}

// Redacted replaces every registered secret value in s with [REDACTED:name].
func Redacted(s string) string {
	for name, value := range *process.Load() {
		if value == "" {
			continue
		}
		s = strings.ReplaceAll(s, value, "[REDACTED:"+strings.ToUpper(name)+"]")
	}
	return s
}

// Writer redacts registered secret values from a byte stream, line by line,
// reading the registry as of each write -- a stream outlives registration.
type Writer struct {
	dst io.Writer
	buf bytes.Buffer
}

// NewWriter wraps dst in registry-backed redaction.
func NewWriter(dst io.Writer) *Writer {
	return &Writer{dst: dst}
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
		if _, err := io.WriteString(w.dst, Redacted(line)); err != nil {
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
		_, _ = io.WriteString(w.dst, Redacted(w.buf.String()))
		w.buf.Reset()
	}
}
