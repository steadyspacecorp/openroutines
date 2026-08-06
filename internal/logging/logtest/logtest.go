// Package logtest captures the process log for tests. Capture redirects
// the log stream -- slog records and the opencode passthrough alike --
// into a test-scoped buffer, and the Log it returns makes "the expected
// thing was logged" a single assertion. Tests never touch the logging
// package's knobs themselves.
package logtest

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/logging"
)

// Capture redirects the process log into a fresh Log until the test ends,
// then restores the real destination.
func Capture(t *testing.T) *Log {
	t.Helper()
	l := &Log{t: t}
	orig := logging.Writer
	logging.Writer = l
	t.Cleanup(func() { logging.Writer = orig })
	return l
}

// Log is the captured stream. The lock is what makes assertions safe
// while a run's goroutines are still logging.
type Log struct {
	t   *testing.T
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *Log) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// String returns everything logged so far, for the assertions Expect and
// Reject don't cover -- counting occurrences, walking lines.
func (l *Log) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// Reset discards what was captured so far, so a test asserting in phases
// reads each phase alone.
func (l *Log) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Reset()
}

// Expect fails the test unless every substring was logged.
func (l *Log) Expect(substrs ...string) {
	l.t.Helper()
	got := l.String()
	for _, want := range substrs {
		if !strings.Contains(got, want) {
			l.t.Fatalf("expected %q in the log, got %q", want, got)
		}
	}
}

// Reject fails the test if any substring was logged.
func (l *Log) Reject(substrs ...string) {
	l.t.Helper()
	got := l.String()
	for _, banned := range substrs {
		if strings.Contains(got, banned) {
			l.t.Fatalf("%q must not reach the log: %q", banned, got)
		}
	}
}
