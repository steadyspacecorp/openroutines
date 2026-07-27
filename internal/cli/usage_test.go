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
	if err := os.MkdirAll(filepath.Join(dir, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	lines := `{"routine":"a","tokens":{"input":100,"output":10,"reasoning":2,"cache_read":5,"cache_write":1},"cost_reported":0.01,"model":"m1","effort":"high"}
{"routine":"a","tokens":{"input":50,"output":5,"reasoning":0,"cache_read":0,"cache_write":0},"cost_reported":0.005,"model":"m2"}
{"routine":"b","tokens":{"input":7,"output":3,"reasoning":0,"cache_read":0,"cache_write":0},"cost_reported":0}
{"routine":"old","outcome":"completed"}
not json
`
	if err := os.WriteFile(filepath.Join(dir, "memory", "runs.jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	rows := aggregateUsage(dir)
	if len(rows) != 2 || rows[0].Routine != "a" || rows[1].Routine != "b" {
		t.Fatalf("rows wrong: %+v", rows)
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
	if none := aggregateUsage(t.TempDir()); none != nil {
		t.Fatalf("no runs.jsonl should aggregate to nil, got %+v", none)
	}
}
