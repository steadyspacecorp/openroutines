package schedule

import (
	"testing"
	"time"
)

func spec(t *testing.T, expr string) *Spec {
	t.Helper()
	s, err := Parse(expr, time.UTC)
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
	first, _, n := Occurrences(daily, nine, nine.Add(30*time.Hour))
	if n != 1 || first.Day() != 2 {
		t.Fatalf("interval must be (after, until]: n=%d first=%v", n, first)
	}
	_, last, n := Occurrences(daily, nine.Add(-time.Hour), nine)
	if n != 1 || !last.Equal(nine) {
		t.Fatalf("until must be inclusive: n=%d last=%v", n, last)
	}
}

func TestOccurrencesAcrossSpringForwardGap(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database")
	}
	s, err := Parse("30 2 * * *", ny)
	if err != nil {
		t.Fatal(err)
	}
	after := time.Date(2026, 3, 6, 12, 0, 0, 0, ny)
	until := time.Date(2026, 3, 10, 12, 0, 0, 0, ny)
	first, last, n := Occurrences(s, after, until)
	if n < 3 || n > 4 {
		t.Fatalf("expected 3-4 firings across the gap window, got %d", n)
	}
	if !first.Before(last) {
		t.Fatalf("firings must increase: first=%v last=%v", first, last)
	}
	if last.Day() != 10 && last.Day() != 9 {
		t.Fatalf("post-gap firings lost: last=%v", last)
	}
}

func TestOccurrencesAcrossFallBack(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database")
	}
	s, err := Parse("30 1 * * *", ny)
	if err != nil {
		t.Fatal(err)
	}
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

func TestOccurrencesHoldWallClockAfterStateRoundTrip(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database")
	}
	defer func(l *time.Location) { time.Local = l }(time.Local)
	time.Local = time.UTC

	dir := t.TempDir()
	st := &State{Routine: "daily", Watermark: time.Date(2026, 10, 30, 6, 0, 0, 0, ny)}
	if err := st.Save(dir); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dir, "daily")
	if err != nil {
		t.Fatal(err)
	}

	s, err := Parse("0 6 * * *", ny)
	if err != nil {
		t.Fatal(err)
	}
	until := time.Date(2026, 11, 2, 12, 0, 0, 0, ny)
	first, last, n := Occurrences(s, loaded.Watermark, until)
	if n != 3 {
		t.Fatalf("expected 3 firings, got %d", n)
	}
	for _, tc := range []struct {
		label string
		got   time.Time
		day   int
	}{{"first", first, 31}, {"last", last, 2}} {
		wall := tc.got.In(ny)
		if wall.Day() != tc.day || wall.Hour() != 6 || wall.Minute() != 0 {
			t.Fatalf("%s firing = %v, want 06:00 New York on day %d", tc.label, wall, tc.day)
		}
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
	now := time.Date(2026, 7, 28, 7, 0, 0, 0, time.UTC)
	end := WindowEnd(checkIn, now, now.AddDate(0, 0, 14))
	want := time.Date(2026, 7, 29, 7, 0, 0, 0, time.UTC)
	if !end.Equal(want) {
		t.Fatalf("window end = %v, want %v", end, want)
	}
}

func TestWindowEndCrossesWeekend(t *testing.T) {
	checkIn := spec(t, "0 7,8 * * 1-5")
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
