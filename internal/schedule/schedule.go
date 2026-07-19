// Package schedule implements the durable two-phase scheduling model
// (DESIGN.md "Scheduling"): per-routine state keyed by routine id -- a
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

// StateDirName is the supervisor-owned directory inside memory/.
const StateDirName = "state"

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
	RoutineID string    `json:"routine_id"`
	Watermark time.Time `json:"watermark"`
	Pending   *Pending  `json:"pending,omitempty"`
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

func statePath(stateDir, routineID string) string {
	return filepath.Join(stateDir, routineID+".json")
}

// Load reads a routine's state; nil (no error) when none exists yet.
func Load(stateDir, routineID string) (*State, error) {
	raw, err := os.ReadFile(statePath(stateDir, routineID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("state %s: %w", routineID, err)
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
	return os.WriteFile(statePath(stateDir, s.RoutineID), append(raw, '\n'), 0o644)
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
