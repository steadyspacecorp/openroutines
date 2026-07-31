package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// EnvLogLevel overrides the configured level for one process -- config
// edits only reach production on redeploy; flipping a live container to
// debug should not need one.
const EnvLogLevel = "OPENROUTINES_LOG_LEVEL"

// ParseLogLevel maps the agent-wide log_level onto the stdlib's ladder:
// debug shows the full model-process transcript; info (the default) the
// rendered, bounded run stream plus supervisor lifecycle lines; warn drops
// the run stream and lifecycle, keeping degraded-but-running conditions;
// error keeps failures and held dispatch only. Failed attempts print their
// output tail at every level -- a failure's last lines are the diagnostic
// payload, not chatter. See design decision "Run output is rendered,
// bounded, and leveled".
//
// slog.Level is the type throughout rather than a framework-specific enum:
// it already orders these four levels and is what every log handler
// expects. The names are matched here rather than by slog.Level's own
// parser, which also accepts offsets like "error+1" -- a threshold above
// error silences the failures this ladder promises no level silences, and
// "info+4" would say warn while reading like info. Four names, any case;
// anything else is a typo worth reporting in the agent's own vocabulary.
func ParseLogLevel(s string) (slog.Level, error) {
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
	return slog.LevelInfo, fmt.Errorf("log_level %q is not one of debug, info, warn, error", s)
}

// EffectiveLogLevel resolves the level a process runs at: the environment
// override wins, then log_level in the configuration file, then info.
// Unrecognized values fall through -- check and Problems() flag them; a
// typo must not change behavior beyond the default.
func (a *Agent) EffectiveLogLevel() slog.Level {
	for _, v := range []string{os.Getenv(EnvLogLevel), a.LogLevel} {
		if v == "" {
			continue
		}
		if lvl, err := ParseLogLevel(v); err == nil {
			return lvl
		}
	}
	return slog.LevelInfo
}
