package config

import (
	"log/slog"
	"strings"
	"testing"
)

// log_level resolves: env override, then the configured value, then info.
// Unknown values fall back to info -- Problems() flags them; a typo must
// not change behavior beyond the default.
func TestEffectiveLogLevel(t *testing.T) {
	a := Agent{}
	if got := a.EffectiveLogLevel(); got != slog.LevelInfo {
		t.Fatalf("omitted log_level should mean info, got %v", got)
	}
	a.LogLevel = "warn"
	if got := a.EffectiveLogLevel(); got != slog.LevelWarn {
		t.Fatalf("configured warn should resolve, got %v", got)
	}
	a.LogLevel = "verbose"
	if got := a.EffectiveLogLevel(); got != slog.LevelInfo {
		t.Fatalf("unknown value should fall back to info, got %v", got)
	}
	t.Setenv(EnvLogLevel, "debug")
	a.LogLevel = "error"
	if got := a.EffectiveLogLevel(); got != slog.LevelDebug {
		t.Fatalf("%s should override the configured value, got %v", EnvLogLevel, got)
	}
}

// The environment override is the one log_level input Problems() never
// sees, so the process announces a rejected value itself -- this is what
// the announcement keys on.
func TestIgnoredLogLevel(t *testing.T) {
	if v, ok := IgnoredLogLevel(); ok {
		t.Fatalf("unset %s reported as ignored: %q", EnvLogLevel, v)
	}
	t.Setenv(EnvLogLevel, "warn")
	if v, ok := IgnoredLogLevel(); ok {
		t.Fatalf("valid %s reported as ignored: %q", EnvLogLevel, v)
	}
	t.Setenv(EnvLogLevel, "verbose")
	if v, ok := IgnoredLogLevel(); !ok || v != "verbose" {
		t.Fatalf("a typo'd %s must be reported, got %q, %v", EnvLogLevel, v, ok)
	}
}

// An invalid log_level is a configuration problem named with the accepted set.
func TestLogLevelValidation(t *testing.T) {
	a := Agent{
		Name:        "a",
		Description: "d",
		Owner:       Owner{Email: "o@example.com"},
		Timezone:    "UTC",
		Defaults:    Defaults{Model: "anthropic/claude-sonnet-5"},
	}
	a.LogLevel = "info"
	if p := a.Problems(); len(p) != 0 {
		t.Fatalf("valid log_level flagged: %v", p)
	}
	a.LogLevel = "verbose"
	p := a.Problems()
	if len(p) != 1 || !strings.Contains(p[0], "debug, info, warn, error") {
		t.Fatalf("want one problem naming the accepted levels, got %v", p)
	}
}

// An offset past error silences the records the level ladder promises are
// never silenced -- failures and held dispatch -- so it is not a level this
// agent has, however well the stdlib parses it.
func TestLogLevelRejectsOffsets(t *testing.T) {
	for _, s := range []string{"error+1", "warn-1", "info+4"} {
		if lvl, err := ParseLogLevel(s); err == nil {
			t.Errorf("log_level %q must not be accepted, resolved to %v", s, lvl)
		}
	}
	if _, err := ParseLogLevel("ERROR"); err != nil {
		t.Errorf("a named level must stay case-insensitive: %v", err)
	}
}
