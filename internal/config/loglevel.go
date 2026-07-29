package config

import (
	"fmt"
	"os"
)

// LogLevel orders log verbosity. debug shows the full model-process
// transcript; info (the default) the rendered, bounded run stream plus
// supervisor lifecycle lines; warn drops the run stream and lifecycle,
// keeping degraded-but-running conditions; error keeps failures and held
// dispatch only. Failed attempts print their output tail at every level --
// a failure's last lines are the diagnostic payload, not chatter. See
// design decision "Run output is rendered, bounded, and leveled".
type LogLevel int

// The levels, in increasing severity. The zero value is LogDebug so a
// bare struct suppresses nothing.
const (
	LogDebug LogLevel = iota
	LogInfo
	LogWarn
	LogError
)

// EnvLogLevel overrides the configured level for one process -- config
// edits only reach production on redeploy; flipping a live container to
// debug should not need one.
const EnvLogLevel = "OPENROUTINES_LOG_LEVEL"

var logLevels = map[string]LogLevel{
	"debug": LogDebug,
	"info":  LogInfo,
	"warn":  LogWarn,
	"error": LogError,
}

// ParseLogLevel maps a configured name to its level.
func ParseLogLevel(s string) (LogLevel, error) {
	if lvl, ok := logLevels[s]; ok {
		return lvl, nil
	}
	return LogInfo, fmt.Errorf("log_level %q is not one of debug, info, warn, error", s)
}

// EffectiveLogLevel resolves the level a process runs at: the environment
// override wins, then log_level in the configuration file, then info.
// Unrecognized values fall through -- check and Problems() flag them; a
// typo must not change behavior beyond the default.
func (a *Agent) EffectiveLogLevel() LogLevel {
	for _, v := range []string{os.Getenv(EnvLogLevel), a.LogLevel} {
		if v == "" {
			continue
		}
		if lvl, err := ParseLogLevel(v); err == nil {
			return lvl
		}
	}
	return LogInfo
}
