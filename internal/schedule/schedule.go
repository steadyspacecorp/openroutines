// Package schedule implements the durable two-phase scheduling model
// (design decision "Scheduling"): per-routine state keyed by routine id -- a
// watermark (latest cron occurrence fully accounted for) plus at most one
// pending logical run that survives failed attempts under the same run_id.
package schedule

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/robfig/cron/v3"
)

// Pending is a logical run that exists durably before it is allowed to act.
type Pending struct {
	RunID          string    `json:"run_id"`
	ScheduledFor   time.Time `json:"scheduled_for"`
	CoveredThrough time.Time `json:"covered_through"`
	CreatedAt      time.Time `json:"created_at"`
	Attempts       int       `json:"attempts"`
	LastAttemptAt  time.Time `json:"last_attempt_at,omitzero"`
}

// State is one routine's durable scheduling record.
type State struct {
	Routine   string    `json:"routine"`
	Watermark time.Time `json:"watermark"`
	Pending   *Pending  `json:"pending,omitempty"`

	// Circuit breaker: consecutive abandonments trip a cool-down so a
	// persistently failing routine cannot grind attempts (and spend) forever.
	ConsecutiveAbandons int       `json:"consecutive_abandons,omitempty"`
	CooldownUntil       time.Time `json:"cooldown_until,omitzero"`
}

const breakerThreshold = 3

// RecordAbandonment counts an abandoned run and, past the threshold, trips
// the breaker: cool-down of 1h doubling per further abandonment, capped at
// 24h. Returns the cool-down applied (zero when the breaker has not tripped).
func (s *State) RecordAbandonment(now time.Time) time.Duration {
	s.ConsecutiveAbandons++
	if s.ConsecutiveAbandons < breakerThreshold {
		return 0
	}
	cooldown := min(time.Hour<<uint(s.ConsecutiveAbandons-breakerThreshold), 24*time.Hour)
	s.CooldownUntil = now.Add(cooldown)
	return cooldown
}

// RecordSuccess resets the breaker: one good run clears the history.
func (s *State) RecordSuccess() {
	s.ConsecutiveAbandons = 0
	s.CooldownUntil = time.Time{}
}

// CoolingDown reports whether the breaker currently blocks new runs.
func (s *State) CoolingDown(now time.Time) bool {
	return now.Before(s.CooldownUntil)
}

const runIDAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// NewRunID mints a logical run id, stable across every retry attempt.
func NewRunID() string {
	buf := make([]byte, 10)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	for i, b := range buf {
		buf[i] = runIDAlphabet[int(b)%len(runIDAlphabet)]
	}
	return "run_" + string(buf)
}

func statePath(stateDir, name string) string {
	return filepath.Join(stateDir, name+".json")
}

// Load reads a routine's state; nil (no error) when none exists yet.
func Load(stateDir, name string) (*State, error) {
	raw, err := os.ReadFile(statePath(stateDir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("state %s: %w", name, err)
	}
	return &s, nil
}

// Save writes a routine's state.
func (s *State) Save(stateDir string) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(statePath(stateDir, s.Routine), append(raw, '\n'), 0o644)
}

// Occurrences returns the first and last cron firing times in (after, until],
// and how many there were. Multiple missed firings collapse into one run:
// the caller uses first as scheduled_for and last as covered_through.
func Occurrences(spec cron.Schedule, after, until time.Time) (first, last time.Time, n int) {
	t := after
	for i := 0; i < 100000; i++ {
		t = spec.Next(t)
		if t.IsZero() || t.After(until) {
			return
		}
		if n == 0 {
			first = t
		}
		last = t
		n++
	}
	return
}

// NextFires returns up to n firing times strictly after `after` and no
// later than `until`, in `after`'s location.
func NextFires(spec cron.Schedule, after, until time.Time, n int) []time.Time {
	var fires []time.Time
	t := after
	for len(fires) < n {
		t = spec.Next(t)
		if t.IsZero() || t.After(until) {
			break
		}
		fires = append(fires, t)
	}
	return fires
}

// WindowEnd returns the spec's first firing on its next fire-day -- the
// first calendar day after `after`'s (in `after`'s location) with any
// firing. Later same-day firings (retry slots) are skipped: a routine's
// window closes when it next runs fresh, not when it retries. Zero when no
// such firing lands by `until`.
func WindowEnd(spec cron.Schedule, after, until time.Time) time.Time {
	y0, d0 := after.Year(), after.YearDay()
	t := after
	for i := 0; i < 100000; i++ {
		t = spec.Next(t)
		if t.IsZero() || t.After(until) {
			break
		}
		if y, d := t.Year(), t.YearDay(); y != y0 || d != d0 {
			return t
		}
	}
	return time.Time{}
}

// NextRetryAt implements attempt backoff: 1, 2, 4, 8... minutes after the
// last attempt, capped at 16 minutes. A pending run with no attempts yet is
// runnable immediately.
func NextRetryAt(p *Pending) time.Time {
	if p.Attempts == 0 {
		return p.CreatedAt
	}
	backoff := time.Duration(1<<min(p.Attempts-1, 4)) * time.Minute
	return p.LastAttemptAt.Add(backoff)
}
