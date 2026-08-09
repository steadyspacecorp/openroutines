package logging

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/steadyspacecorp/openroutines/internal/scrub"
)

// The output is logfmt: every part of a record is a key=value pair.
// Nothing is interpolated into the message, so a line can be filtered by
// field rather than matched by shape.
func TestOutputIsLogfmt(t *testing.T) {
	var buf bytes.Buffer
	Writer = &buf
	slog.With("routine", "check-in").Error("attempt failed -- will retry", "run_id", "run_abc", "attempts", 3, "error", errors.New("exit status 1"))

	got := strings.TrimSpace(buf.String())
	for _, want := range []string{
		`level=ERROR`,
		`msg="attempt failed -- will retry"`,
		`routine=check-in`,
		`run_id=run_abc`,
		`attempts=3`,
		`error="exit status 1"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}

// Timestamps carry the agent's offset: its schedule is a wall-clock promise
// in that zone, so reading a log line against a cron expression should not
// need arithmetic.
func TestTimestampUsesTheConfiguredZone(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	var buf bytes.Buffer
	Writer = &buf
	Zone = loc
	t.Cleanup(func() { Zone = time.UTC })
	slog.Info("supervising")

	stamp := field(t, buf.String(), "time")
	when, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatalf("time=%q is not RFC3339: %v", stamp, err)
	}
	_, want := time.Now().In(loc).Zone()
	if _, got := when.Zone(); got != want {
		t.Fatalf("offset %d, want %d (time=%q)", got, want, stamp)
	}
}

// Below the configured level nothing is written at all -- the handler is
// the only thing that decides.
func TestLevelGate(t *testing.T) {
	var buf bytes.Buffer
	Writer = &buf
	Level.Set(slog.LevelWarn)
	t.Cleanup(func() { Level.Set(slog.LevelInfo) })

	slog.Info("lifecycle")
	if buf.Len() != 0 {
		t.Fatalf("info should be dropped at warn: %q", buf.String())
	}
	slog.Warn("degraded")
	if !strings.Contains(buf.String(), "degraded") {
		t.Fatalf("warn should be kept at warn: %q", buf.String())
	}
}

// A secret in the scrub registry never reaches the output, whichever call
// site logs it -- redaction is the installed writer's, not the caller's.
func TestRegisteredSecretsAreRedacted(t *testing.T) {
	var buf bytes.Buffer
	Writer = &buf
	scrub.Register(map[string]string{"api_token": "tok-hunter2"})

	slog.Error("push failed", "error", errors.New("remote rejected tok-hunter2"))
	got := buf.String()
	if strings.Contains(got, "tok-hunter2") {
		t.Fatalf("secret survived into the log: %q", got)
	}
	if !strings.Contains(got, "[REDACTED:API_TOKEN]") {
		t.Fatalf("no redaction marker in %q", got)
	}
}

// Trigger polls register bearer material mid-flight while run goroutines
// log concurrently; mutating a plain map under those readers would be a
// fatal runtime error, not just a race.
func TestScrubRegistrationRacesLogging(t *testing.T) {
	Writer = &bytes.Buffer{}
	scrub.Register(map[string]string{"master key": "seed-value"})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 500 {
			scrub.Register(map[string]string{"poll_token": fmt.Sprintf("bearer-%d", i)})
		}
	}()
	for i := range 500 {
		slog.Error("run line carrying seed-value", "n", i)
	}
	<-done
	if got := scrub.Redacted("bearer-499 and seed-value"); strings.Contains(got, "bearer-499") || strings.Contains(got, "seed-value") {
		t.Fatalf("registered secrets not redacted: %q", got)
	}
}

// The level resolves: the environment override, else a default split by
// where the process runs -- info in the container, warn locally. An
// unrecognized override falls back to the default -- IgnoredLevel names
// it; a typo must not change behavior beyond that.
func TestConfigureLevel(t *testing.T) {
	t.Cleanup(func() { Level.Set(slog.LevelInfo) })
	t.Setenv("OPENROUTINES_IN_CONTAINER", "1")
	ConfigureLevel()
	if got := Level.Level(); got != slog.LevelInfo {
		t.Fatalf("no override should mean info in the container, got %v", got)
	}
	t.Setenv("OPENROUTINES_IN_CONTAINER", "")
	ConfigureLevel()
	if got := Level.Level(); got != slog.LevelWarn {
		t.Fatalf("no override should mean warn locally, got %v", got)
	}
	t.Setenv(EnvLevel, "verbose")
	ConfigureLevel()
	if got := Level.Level(); got != slog.LevelWarn {
		t.Fatalf("an unrecognized override should fall back to the default, got %v", got)
	}
	if v, ok := IgnoredLevel(); !ok || v != "verbose" {
		t.Fatalf("the unrecognized override should be reported, got %q ok=%v", v, ok)
	}
	t.Setenv(EnvLevel, "debug")
	ConfigureLevel()
	if got := Level.Level(); got != slog.LevelDebug {
		t.Fatalf("%s should set the level, got %v", EnvLevel, got)
	}
}

func field(t *testing.T, line, key string) string {
	t.Helper()
	for _, pair := range strings.Fields(strings.TrimSpace(line)) {
		if k, v, ok := strings.Cut(pair, "="); ok && k == key {
			return v
		}
	}
	t.Fatalf("no %s= in %q", key, line)
	return ""
}
