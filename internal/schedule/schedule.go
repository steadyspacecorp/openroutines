package schedule

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/robfig/cron/v3"
)

// A cron expression bound to the agent's timezone. Binding matters
// because a watermark round-tripped through the state file comes back in a
// fabricated fixed-offset zone (time.Time.UnmarshalJSON does that whenever
// the offset isn't time.Local's), and cron evaluates an unbound spec in
// whatever zone its argument carries -- which would freeze the schedule at
// last season's UTC offset and drift it an hour at every DST transition.
type Spec struct {
	sched cron.Schedule
	loc   *time.Location
}

func Parse(expr string, loc *time.Location) (*Spec, error) {
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return nil, err
	}
	return &Spec{sched: sched, loc: loc}, nil
}

func (s *Spec) Next(t time.Time) time.Time {
	return s.sched.Next(t.In(s.loc))
}

type Pending struct {
	RunID          string    `json:"run_id"`
	ScheduledFor   time.Time `json:"scheduled_for"`
	CoveredThrough time.Time `json:"covered_through"`
	CreatedAt      time.Time `json:"created_at"`
	Attempts       int       `json:"attempts"`
	LastAttemptAt  time.Time `json:"last_attempt_at,omitzero"`
}

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

// Counts an abandoned run and, past the threshold, trips
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

func (s *State) RecordSuccess() {
	s.ConsecutiveAbandons = 0
	s.CooldownUntil = time.Time{}
}

func (s *State) CoolingDown(now time.Time) bool {
	return now.Before(s.CooldownUntil)
}

func statePath(stateDir, name string) string {
	return filepath.Join(stateDir, name+".json")
}

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

func Occurrences(spec *Spec, after, until time.Time) (first, last time.Time, n int) {
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

func NextFires(spec *Spec, after, until time.Time, n int) []time.Time {
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

func WindowEnd(spec *Spec, after, until time.Time) time.Time {
	after = after.In(spec.loc)
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

// Implements attempt backoff: 1, 2, 4, 8... minutes after the
// last attempt, capped at 16 minutes. A pending run with no attempts yet is
// runnable immediately.
func NextRetryAt(p *Pending) time.Time {
	if p.Attempts == 0 {
		return p.CreatedAt
	}
	backoff := time.Duration(1<<min(p.Attempts-1, 4)) * time.Minute
	return p.LastAttemptAt.Add(backoff)
}
