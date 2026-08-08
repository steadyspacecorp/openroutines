package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// Every parseable record counts as a run; token sums and RunsReported
// cover only records that carry tokens (absence is not zero). Model and
// effort are the most recent record's.
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
	rows := aggregateUsage(dir)
	// "old" ran without reporting tokens and still counts as a run; the
	// unparseable line is not a record at all.
	if len(rows) != 3 || rows[0].Routine != "a" || rows[1].Routine != "b" || rows[2].Routine != "old" {
		t.Fatalf("rows wrong: %+v", rows)
	}
	a := rows[0]
	if a.Runs != 2 || a.RunsReported != 2 || a.Tokens.Input != 150 || a.Tokens.Output != 15 ||
		a.Tokens.Reasoning != 2 || a.Tokens.CacheRead != 5 || a.Tokens.CacheWrite != 1 {
		t.Fatalf("a sums wrong: %+v", a)
	}
	if a.Model != "m2" || a.Effort != "" {
		t.Fatalf("model/effort should be the most recent record's: %+v", a)
	}
	if old := rows[2]; old.Runs != 1 || old.RunsReported != 0 || old.Tokens.Input != 0 {
		t.Fatalf("old should count its run and report nothing: %+v", old)
	}
	tot := totalUsage(rows)
	if tot.Runs != 4 || tot.RunsReported != 3 || tot.Tokens.Input != 157 || tot.Tokens.Output != 18 {
		t.Fatalf("total wrong: %+v", tot)
	}
	if none := aggregateUsage(t.TempDir()); none != nil {
		t.Fatalf("no runs.jsonl should aggregate to nil, got %+v", none)
	}
}

// Runs whose runtime reported no usage still count as runs -- the table
// shows them with blank token cells rather than pretending they did not
// happen, which in practice sent someone hunting for missing runs.
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
	rows := aggregateUsage(dir)
	if len(rows) != 2 {
		t.Fatalf("every run should have a row: %+v", rows)
	}
	for _, r := range rows {
		if r.Runs != 1 || r.RunsReported != 0 {
			t.Fatalf("run without tokens should count and report nothing: %+v", r)
		}
	}
}
