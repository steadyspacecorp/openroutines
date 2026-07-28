package schedule

import (
	"testing"
	"time"

	"github.com/robfig/cron/v3"
)

func spec(t *testing.T, expr string) cron.Schedule {
	t.Helper()
	s, err := cron.ParseStandard(expr)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOccurrencesCollapseWindow(t *testing.T) {
	daily := spec(t, "0 9 * * *")
	after := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	first, last, n := Occurrences(daily, after, until)
	if n != 7 {
		t.Fatalf("expected 7 firings in a week, got %d", n)
	}
	if first.Day() != 1 || last.Day() != 7 {
		t.Fatalf("wrong window: first=%v last=%v", first, last)
	}
}

func TestOccurrencesIntervalIsHalfOpen(t *testing.T) {
	daily := spec(t, "0 9 * * *")
	nine := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	// after == a firing time: that firing is excluded (already accounted for).
	first, _, n := Occurrences(daily, nine, nine.Add(30*time.Hour))
	if n != 1 || first.Day() != 2 {
		t.Fatalf("interval must be (after, until]: n=%d first=%v", n, first)
	}
	// until == a firing time: that firing is included.
	_, last, n := Occurrences(daily, nine.Add(-time.Hour), nine)
	if n != 1 || !last.Equal(nine) {
		t.Fatalf("until must be inclusive: n=%d last=%v", n, last)
	}
}

func TestOccurrencesAcrossSpringForwardGap(t *testing.T) {
	// US DST 2026: clocks jump 02:00 -> 03:00 on March 8 in New York.
	// A 02:30 schedule has no valid firing that day; whatever the cron
	// library does with it, the invariants that matter to the supervisor
	// are: no panic, strictly increasing firings, and no lost days around
	// the gap.
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database")
	}
	s := spec(t, "30 2 * * *")
	after := time.Date(2026, 3, 6, 12, 0, 0, 0, ny)
	until := time.Date(2026, 3, 10, 12, 0, 0, 0, ny)
	first, last, n := Occurrences(s, after, until)
	if n < 3 || n > 4 {
		t.Fatalf("expected 3-4 firings across the gap window, got %d", n)
	}
	if !first.Before(last) {
		t.Fatalf("firings must increase: first=%v last=%v", first, last)
	}
	// The day after the gap must fire normally.
	if last.Day() != 10 && last.Day() != 9 {
		t.Fatalf("post-gap firings lost: last=%v", last)
	}
}

func TestOccurrencesAcrossFallBack(t *testing.T) {
	// US DST end 2026: clocks repeat 01:00-02:00 on November 1 in New York.
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database")
	}
	s := spec(t, "30 1 * * *")
	after := time.Date(2026, 10, 30, 12, 0, 0, 0, ny)
	until := time.Date(2026, 11, 3, 12, 0, 0, 0, ny)
	prev := after
	cur := after
	count := 0
	for {
		cur = s.Next(cur)
		if cur.After(until) {
			break
		}
		if !cur.After(prev) {
			t.Fatalf("firings must be strictly increasing: %v then %v", prev, cur)
		}
		prev = cur
		count++
	}
	if count < 3 || count > 5 {
		t.Fatalf("expected 3-5 firings across fall-back, got %d", count)
	}
}

func TestNextRetryAtBackoffProgression(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	p := &Pending{CreatedAt: t0, LastAttemptAt: t0}
	if got := NextRetryAt(&Pending{CreatedAt: t0}); !got.Equal(t0) {
		t.Fatalf("no attempts yet: runnable immediately, got %v", got)
	}
	for _, tc := range []struct {
		attempts int
		want     time.Duration
	}{{1, time.Minute}, {2, 2 * time.Minute}, {3, 4 * time.Minute}, {4, 8 * time.Minute}, {5, 16 * time.Minute}, {9, 16 * time.Minute}} {
		p.Attempts = tc.attempts
		if got := NextRetryAt(p).Sub(t0); got != tc.want {
			t.Fatalf("attempts=%d: want backoff %v, got %v", tc.attempts, tc.want, got)
		}
	}
}

func TestBreakerTripAndReset(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	s := &State{Routine: "x"}
	if cd := s.RecordAbandonment(now); cd != 0 {
		t.Fatalf("abandonment 1 must not trip: %v", cd)
	}
	if cd := s.RecordAbandonment(now); cd != 0 {
		t.Fatalf("abandonment 2 must not trip: %v", cd)
	}
	if cd := s.RecordAbandonment(now); cd != time.Hour {
		t.Fatalf("abandonment 3 trips at 1h, got %v", cd)
	}
	if cd := s.RecordAbandonment(now); cd != 2*time.Hour {
		t.Fatalf("abandonment 4 doubles to 2h, got %v", cd)
	}
	s.ConsecutiveAbandons = 10
	if cd := s.RecordAbandonment(now); cd != 24*time.Hour {
		t.Fatalf("cool-down caps at 24h, got %v", cd)
	}
	if !s.CoolingDown(now.Add(time.Minute)) {
		t.Fatal("should be cooling down")
	}
	s.RecordSuccess()
	if s.ConsecutiveAbandons != 0 || s.CoolingDown(now.Add(time.Minute)) {
		t.Fatalf("success must fully reset the breaker: %+v", s)
	}
}

func TestNextFiresBoundedByCountAndHorizon(t *testing.T) {
	weekdays := spec(t, "0 9 * * 1-5")
	// Tue 2026-07-28 07:00.
	now := time.Date(2026, 7, 28, 7, 0, 0, 0, time.UTC)
	fires := NextFires(weekdays, now, now.AddDate(0, 0, 14), 3)
	if len(fires) != 3 {
		t.Fatalf("expected 3 fires, got %d", len(fires))
	}
	if fires[0].Day() != 28 || fires[1].Day() != 29 || fires[2].Day() != 30 {
		t.Fatalf("wrong fires: %v", fires)
	}
	monthly := spec(t, "0 9 1 * *")
	if got := NextFires(monthly, now, now.AddDate(0, 0, 2), 2); len(got) != 0 {
		t.Fatalf("horizon must bound the scan, got %v", got)
	}
}

func TestWindowEndSkipsSameDayRetrySlot(t *testing.T) {
	checkIn := spec(t, "0 7,8 * * 1-5")
	// Running at Tue 07:00: the 08:00 slot is today's retry, not the close.
	now := time.Date(2026, 7, 28, 7, 0, 0, 0, time.UTC)
	end := WindowEnd(checkIn, now, now.AddDate(0, 0, 14))
	want := time.Date(2026, 7, 29, 7, 0, 0, 0, time.UTC)
	if !end.Equal(want) {
		t.Fatalf("window end = %v, want %v", end, want)
	}
}

func TestWindowEndCrossesWeekend(t *testing.T) {
	checkIn := spec(t, "0 7,8 * * 1-5")
	// Friday morning: the next fire-day is Monday.
	now := time.Date(2026, 7, 31, 8, 30, 0, 0, time.UTC)
	end := WindowEnd(checkIn, now, now.AddDate(0, 0, 14))
	want := time.Date(2026, 8, 3, 7, 0, 0, 0, time.UTC)
	if !end.Equal(want) {
		t.Fatalf("window end = %v, want %v", end, want)
	}
}

func TestWindowEndZeroBeyondHorizon(t *testing.T) {
	monthly := spec(t, "0 9 1 * *")
	now := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	if end := WindowEnd(monthly, now, now.AddDate(0, 0, 14)); !end.IsZero() {
		t.Fatalf("expected zero beyond horizon, got %v", end)
	}
}
