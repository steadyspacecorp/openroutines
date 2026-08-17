package logtest

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/steadyspacecorp/openroutines/internal/logging"
)

func Capture(t *testing.T) *Log {
	t.Helper()
	l := &Log{t: t}
	orig := logging.Writer
	logging.Writer = l
	t.Cleanup(func() { logging.Writer = orig })
	return l
}

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

func (l *Log) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *Log) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Reset()
}

func (l *Log) Expect(substrs ...string) {
	l.t.Helper()
	got := l.String()
	for _, want := range substrs {
		if !strings.Contains(got, want) {
			l.t.Fatalf("expected %q in the log, got %q", want, got)
		}
	}
}

func (l *Log) Reject(substrs ...string) {
	l.t.Helper()
	got := l.String()
	for _, banned := range substrs {
		if strings.Contains(got, banned) {
			l.t.Fatalf("%q must not reach the log: %q", banned, got)
		}
	}
}
