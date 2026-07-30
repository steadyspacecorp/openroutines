// Package scrub redacts injected secret values from a byte stream before it
// reaches logs. Defense in depth: exact-value matching only (see design
// decision "Credentials", rule 3) -- the primary protection is that undeclared secrets
// are never in the process at all.
package scrub

import (
	"bytes"
	"io"
	"maps"
	"strings"
	"sync/atomic"
)

// Set is a concurrency-safe secret collection: readers snapshot the current
// map through an atomic pointer while writers swap in a rebuilt copy. The
// supervisor's trigger polls add bearer material mid-flight while run
// goroutines log through writers that hold the set -- mutating a plain map
// under those readers is a fatal runtime error, not just a race.
type Set struct {
	p atomic.Pointer[map[string]string]
}

// NewSet builds a set seeded with initial (copied, never aliased).
func NewSet(initial map[string]string) *Set {
	s := &Set{}
	m := maps.Clone(initial)
	if m == nil {
		m = map[string]string{}
	}
	s.p.Store(&m)
	return s
}

// Snapshot returns the current map. Callers must not mutate it.
func (s *Set) Snapshot() map[string]string { return *s.p.Load() }

// Add copies the current map, lays values over it, and swaps the result in.
func (s *Set) Add(values map[string]string) {
	for {
		old := s.p.Load()
		next := make(map[string]string, len(*old)+len(values))
		maps.Copy(next, *old)
		maps.Copy(next, values)
		if s.p.CompareAndSwap(old, &next) {
			return
		}
	}
}

// Writer replaces known secret values with [REDACTED:name] line by line.
type Writer struct {
	dst      io.Writer
	snapshot func() map[string]string // the secrets as of each write
	buf      bytes.Buffer
}

// NewWriter wraps dst, redacting secret values line by line. The map must
// not be mutated once handed over; a source that grows takes NewSetWriter.
func NewWriter(dst io.Writer, secrets map[string]string) *Writer {
	return &Writer{dst: dst, snapshot: func() map[string]string { return secrets }}
}

// NewSetWriter wraps dst, redacting the set's values as of each write --
// for streams that outlive secret registration.
func NewSetWriter(dst io.Writer, set *Set) *Writer {
	return &Writer{dst: dst, snapshot: set.Snapshot}
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
		if _, err := io.WriteString(w.dst, Redact(line, w.snapshot())); err != nil {
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
		_, _ = io.WriteString(w.dst, Redact(w.buf.String(), w.snapshot()))
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
