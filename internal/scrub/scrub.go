// Package scrub redacts secret values from text and byte streams before they
// leave the process. Defense in depth: exact-value matching only (design
// decision "Credentials", rule 3) -- the primary protection is that
// undeclared secrets never enter the process. One process-wide registry:
// code that materializes a secret calls Register, and every consumer (log
// writer, run output stream, knowledge appends) redacts from the same set.
package scrub

import (
	"bytes"
	"io"
	"maps"
	"strconv"
	"strings"
	"sync/atomic"
)

// One registered secret: the marker name is not the map key, so several
// live entries may share a name (see RegisterEphemeral).
type entry struct {
	name  string
	value string
}

// The registry: a concurrency-safe map where readers snapshot the current
// state through an atomic pointer while writers swap in a rebuilt copy.
// Trigger polls register bearer material mid-flight while run goroutines
// redact through writers that hold the set -- mutating a plain map under
// those readers is a fatal runtime error, not just a race.
var process atomic.Pointer[map[string]entry]

func init() {
	process.Store(&map[string]entry{})
}

func swap(mutate func(map[string]entry)) {
	for {
		old := process.Load()
		next := make(map[string]entry, len(*old)+1)
		maps.Copy(next, *old)
		mutate(next)
		if process.CompareAndSwap(old, &next) {
			return
		}
	}
}

// Register adds process-lifetime secret values, keyed by the name that
// appears in the redaction marker. A re-registered name overwrites rather
// than accumulates, so repeated materialization of the same credential
// keeps the registry bounded.
func Register(values map[string]string) {
	swap(func(next map[string]entry) {
		for name, value := range values {
			next["named:"+name] = entry{name, value}
		}
	})
}

var ephemeralSeq atomic.Uint64

// RegisterEphemeral adds one short-lived secret value under its own entry:
// concurrent runs minting the same credential each hold distinct material,
// and one registration must never displace another still in use. The
// returned release removes exactly this entry, which is what keeps the
// registry bounded -- by live material, not by history.
func RegisterEphemeral(name, value string) (release func()) {
	id := "ephemeral:" + strconv.FormatUint(ephemeralSeq.Add(1), 10)
	swap(func(next map[string]entry) { next[id] = entry{name, value} })
	return func() {
		swap(func(next map[string]entry) { delete(next, id) })
	}
}

// Redacted replaces every registered secret value in s with [REDACTED:name].
func Redacted(s string) string {
	for _, e := range *process.Load() {
		if e.value == "" {
			continue
		}
		s = strings.ReplaceAll(s, e.value, "[REDACTED:"+strings.ToUpper(e.name)+"]")
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
// defense in depth, and an unbounded buffer is a knowledge hole.
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
