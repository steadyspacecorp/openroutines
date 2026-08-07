package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// Aggregation folds only records that carry tokens (absence is not zero),
// sums per routine, and keeps the most recent model/effort.
func TestAggregateUsage(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "knowledge"), 0o755); err != nil {
		t.Fatal(err)
	}
	lines := `{"routine":"a","tokens":{"input":100,"output":10,"reasoning":2,"cache_read":5,"cache_write":1},"cost_reported":0.01,"model":"m1","effort":"high"}
{"routine":"a","tokens":{"input":50,"output":5,"reasoning":0,"cache_read":0,"cache_write":0},"cost_reported":0.005,"model":"m2"}
{"routine":"b","tokens":{"input":7,"output":3,"reasoning":0,"cache_read":0,"cache_write":0},"cost_reported":0}
{"routine":"old","outcome":"completed"}
not json
`
	if err := os.WriteFile(filepath.Join(dir, "knowledge", "runs.jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	rows, records := aggregateUsage(dir)
	if len(rows) != 2 || rows[0].Routine != "a" || rows[1].Routine != "b" {
		t.Fatalf("rows wrong: %+v", rows)
	}
	// Every parseable record counts, tokens or not: "old" has none, and the
	// unparseable line is not a record at all.
	if records != 4 {
		t.Fatalf("records = %d, want 4", records)
	}
	a := rows[0]
	if a.Runs != 2 || a.Tokens.Input != 150 || a.Tokens.Output != 15 || a.Tokens.Reasoning != 2 ||
		a.Tokens.CacheRead != 5 || a.Tokens.CacheWrite != 1 {
		t.Fatalf("a sums wrong: %+v", a)
	}
	if a.Model != "m2" || a.Effort != "" {
		t.Fatalf("model/effort should be the most recent record's: %+v", a)
	}
	tot := totalUsage(rows)
	if tot.Runs != 3 || tot.Tokens.Input != 157 || tot.Tokens.Output != 18 {
		t.Fatalf("total wrong: %+v", tot)
	}
	if none, records := aggregateUsage(t.TempDir()); none != nil || records != 0 {
		t.Fatalf("no runs.jsonl should aggregate to nil/0, got %+v/%d", none, records)
	}
}

// The case that reads as a silent zero: runs happened, none carried usage.
// Telling someone to wait for records that already exist sends them looking
// in the wrong place -- as a stale knowledge worktree did in practice.
func TestAggregateUsageCountsRecordsWithoutTokens(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "knowledge"), 0o755); err != nil {
		t.Fatal(err)
	}
	lines := `{"routine":"a","outcome":"completed","manual":true}
{"routine":"b","outcome":"completed"}
`
	if err := os.WriteFile(filepath.Join(dir, "knowledge", "runs.jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	rows, records := aggregateUsage(dir)
	if len(rows) != 0 {
		t.Fatalf("records without tokens must not aggregate: %+v", rows)
	}
	if records != 2 {
		t.Fatalf("records = %d, want 2 -- the count is what separates this from a fresh agent", records)
	}
}
