// Package logging installs the process-wide logger: scrubbed logfmt via the
// stdlib TextHandler. Boot configuration only -- call sites log through
// slog's default, and the installed writer redacts, so no call site decides
// whether its line needs scrubbing.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/mode"
	"github.com/steadyspacecorp/openroutines/internal/scrub"
)

// The logger's three knobs; the handler, installed at package load, reads
// them live.
var (
	// Writer is the log destination -- stderr, leaving stdout to run output.
	// Tests reassign it to capture the stream.
	Writer io.Writer = os.Stderr

	// Level gates the log; the handler reads it atomically, so Set works
	// mid-process.
	Level slog.LevelVar

	// Zone renders timestamps in the agent's timezone. nil means local.
	Zone = time.UTC
)

// Installed before anything can log, so nothing ever logs through slog's
// stock unscrubbed handler.
func init() {
	slog.SetDefault(slog.New(slog.NewTextHandler(scrub.NewWriter(liveWriter{}), &slog.HandlerOptions{
		Level:       &Level,
		ReplaceAttr: inZone,
	})))
}

// liveWriter defers to Writer at write time, so reassigning the knob
// redirects the handler already installed.
type liveWriter struct{}

func (liveWriter) Write(p []byte) (int, error) { return Writer.Write(p) }

// inZone restates the record's timestamp in Zone; the handler still
// formats it as RFC3339 with milliseconds.
func inZone(groups []string, a slog.Attr) slog.Attr {
	if Zone != nil && len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
		a.Value = slog.TimeValue(a.Value.Time().In(Zone))
	}
	return a
}

// EnvLevel sets the level for one process, so flipping a live container to
// debug is an environment change, never a redeploy.
const EnvLevel = "OPENROUTINES_LOG_LEVEL"

// parseLevel matches exactly four names, not slog.Level's own parser: that
// parser also accepts offsets like "error+1", a threshold above error that
// would silence the failures this ladder promises no level silences.
func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return slog.LevelInfo, fmt.Errorf("log level %q is not one of debug, info, warn, error", s)
}

// ConfigureLevel sets Level: EnvLevel wins if it parses; otherwise info in
// the container (its log is the only interface) and warn locally (lifecycle
// lines would bury the run output a person is watching). A bad override
// falls through to the default -- IgnoredLevel reports the typo.
func ConfigureLevel() {
	if v := os.Getenv(EnvLevel); v != "" {
		if lvl, err := parseLevel(v); err == nil {
			Level.Set(lvl)
			return
		}
	}
	if mode.Current() == mode.DeployedContainer {
		Level.Set(slog.LevelInfo)
	} else {
		Level.Set(slog.LevelWarn)
	}
}

// IgnoredLevel reports an EnvLevel value that failed to parse, so a typo'd
// override doesn't silently look like debug logging doesn't exist.
func IgnoredLevel() (string, bool) {
	v := os.Getenv(EnvLevel)
	if v == "" {
		return "", false
	}
	if _, err := parseLevel(v); err != nil {
		return v, true
	}
	return "", false
}
