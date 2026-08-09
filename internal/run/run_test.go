package run

import (
	"regexp"
	"testing"
)

func TestNewID(t *testing.T) {
	for range 100 {
		if id := NewID(); !regexp.MustCompile(`^run_[a-z0-9]{10}$`).MatchString(id) {
			t.Fatalf("NewID() = %q", id)
		}
	}
}

func TestRecordRoundTrip(t *testing.T) {
	record := Record{
		RunID: "run_one", Routine: "digest", Attempt: 2, Outcome: "completed",
		Tokens: &Tokens{Input: 10, Output: 4}, CostReported: 0.25,
	}
	records := ParseRecords([]byte(record.JSON() + "\nnot json\n"))
	if len(records) != 1 {
		t.Fatalf("ParseRecords() returned %d records", len(records))
	}
	if got := records[0]; got.RunID != record.RunID || got.Tokens == nil || got.Tokens.Input != 10 {
		t.Fatalf("ParseRecords() = %#v", got)
	}
}
