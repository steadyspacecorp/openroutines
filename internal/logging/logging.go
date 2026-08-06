// Package logging installs the process-wide logger: log/slog rendered by
// the stdlib TextHandler, so the output is logfmt. Swapping in
// slog.NewJSONHandler is the whole change for a deployment that wants JSON.
//
// This package is boot configuration only. Call sites log through slog's
// default (`slog.Info(...)`, or a child logger such as routine.Log()), and
// the installed writer redacts every secret in the scrub registry, so no
// call site decides whether its line needs scrubbing.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/scrub"
)

// The logger's three knobs. The handler, installed once at package load,
// reads them live -- there is nothing to call after assigning: the CLI
// dispatch sets Level and Zone from the agent's configuration, tests point
// Writer at a buffer, and everything else relies on the defaults.
var (
	// Writer is the log destination -- stderr, the stream diagnostics
	// belong on, leaving stdout to carry run output alone. Everything
	// bound for the log goes through it: the handler writes to it, and so
	// does the opencode passthrough. Tests reassign it to capture the
	// stream; production code never does.
	Writer io.Writer = os.Stderr

	// Level gates the log (design decision "The log is structured"). The
	// zero LevelVar is info; the handler reads it atomically, so Set works
	// mid-process -- assignment is how the env override reaches a live
	// container's level too.
	Level slog.LevelVar

	// Zone renders timestamps in the agent's timezone, the zone its
	// schedule is a wall-clock promise in (design decision "Cron is
	// evaluated in the agent's timezone"). nil means the local zone.
	Zone = time.UTC
)

// The one process-wide handler: scrubbed logfmt to Writer, gated at Level,
// stamped in Zone, installed before anything can log -- so nothing ever
// logs through slog's stock unscrubbed handler, and no path has to
// remember to install it.
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

// EnvLevel sets the level for one process -- the only level knob there
// is, so flipping a live container to debug is an environment change,
// never a redeploy.
const EnvLevel = "OPENROUTINES_LOG_LEVEL"

// parseLevel maps a level name onto the stdlib's ladder. The level gates
// the process's own records at the handler and opencode's passed-through
// diagnostics at their source (each run is asked for this same level) --
// and nothing else; run output is never log lines, and stored history is
// exported sessions in operator storage (design decision "Run history:
// opencode's log passed through, sessions exported"): info (the container
// default) keeps lifecycle lines; warn (the local default) keeps
// degraded-but-running conditions; error keeps failures and held dispatch
// only.
//
// The names are matched here rather than by slog.Level's own parser, which
// also accepts offsets like "error+1" -- a threshold above error silences
// the failures this ladder promises no level silences, and "info+4" would
// say warn while reading like info. Four names, any case; anything else is
// a typo worth reporting.
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

// ConfigureLevel resolves the level this process runs at and sets Level:
// the EnvLevel override wins; otherwise a default split by where the
// process runs -- info in the production container, whose log stream is an
// unattended agent's only interface, and warn locally, where the person at
// the terminal is watching the run's own output and lifecycle lines only
// bury it. An unrecognized override falls through to the default --
// IgnoredLevel announces it; a typo must not change behavior beyond that.
func ConfigureLevel() {
	if v := os.Getenv(EnvLevel); v != "" {
		if lvl, err := parseLevel(v); err == nil {
			Level.Set(lvl)
			return
		}
	}
	if os.Getenv("OPENROUTINES_IN_CONTAINER") == "1" {
		Level.Set(slog.LevelInfo)
	} else {
		Level.Set(slog.LevelWarn)
	}
}

// IgnoredLevel reports an environment override that failed to parse. The
// process announces the typo itself: the variable exists to flip a live
// container to debug, and an operator whose override is silently ignored
// concludes the debug lines don't exist and stops looking.
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
