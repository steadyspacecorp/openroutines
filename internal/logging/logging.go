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

var (
	Writer io.Writer = os.Stderr

	Level slog.LevelVar

	Zone = time.UTC
)

func init() {
	// Install the scrubbed handler before anything can log; redaction is not
	// left to each caller.
	slog.SetDefault(slog.New(slog.NewTextHandler(scrub.NewWriter(liveWriter{}), &slog.HandlerOptions{
		Level:       &Level,
		ReplaceAttr: inZone,
	})))
}

type liveWriter struct{}

func (liveWriter) Write(p []byte) (int, error) { return Writer.Write(p) }

func inZone(groups []string, a slog.Attr) slog.Attr {
	if Zone != nil && len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
		a.Value = slog.TimeValue(a.Value.Time().In(Zone))
	}
	return a
}

const EnvLevel = "OPENROUTINES_LOG_LEVEL"

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
