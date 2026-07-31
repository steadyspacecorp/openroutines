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
	"io"
	"log/slog"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/scrub"
)

// Setup installs the process-wide logger, writing scrubbed logfmt to w.
// There is one per process -- a supervisor, or a single manual run.
//
// loc renders timestamps in the agent's timezone, the zone its schedule is
// a wall-clock promise in (design decision "Cron is evaluated in the
// agent's timezone"). nil means the local zone.
func Setup(w io.Writer, level slog.Level, loc *time.Location) {
	slog.SetDefault(slog.New(slog.NewTextHandler(scrub.NewWriter(w), &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: inZone(loc),
	})))
}

// inZone restates the record's timestamp in loc; the handler still formats
// it as RFC3339 with milliseconds.
func inZone(loc *time.Location) func([]string, slog.Attr) slog.Attr {
	if loc == nil {
		return nil
	}
	return func(groups []string, a slog.Attr) slog.Attr {
		if len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
			a.Value = slog.TimeValue(a.Value.Time().In(loc))
		}
		return a
	}
}
