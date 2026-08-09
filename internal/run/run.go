// Package run owns run identities and the persisted attempt record schema.
package run

import (
	"crypto/rand"
	"encoding/json"
	"strings"
)

const idAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// NewID returns a random logical run identifier.
func NewID() string {
	buf := make([]byte, 10)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	for i, b := range buf {
		buf[i] = idAlphabet[int(b)%len(idAlphabet)]
	}
	return "run_" + string(buf)
}

// Tokens is one attempt's reported token consumption.
type Tokens struct {
	Input        int64   `json:"input"`
	Output       int64   `json:"output"`
	Reasoning    int64   `json:"reasoning"`
	CacheRead    int64   `json:"cache_read"`
	CacheWrite   int64   `json:"cache_write"`
	CostReported float64 `json:"-"`
}

// Record is one persisted routine attempt.
type Record struct {
	RunID          string  `json:"run_id"`
	Routine        string  `json:"routine"`
	Attempt        int     `json:"attempt"`
	Outcome        string  `json:"outcome"`
	RecordedAt     string  `json:"recorded_at"`
	DurationMS     int64   `json:"duration_ms"`
	ExitCode       int     `json:"exit_code"`
	ScheduledFor   string  `json:"scheduled_for"`
	CoveredThrough string  `json:"covered_through"`
	Manual         bool    `json:"manual"`
	Model          string  `json:"model,omitempty"`
	Effort         string  `json:"effort,omitempty"`
	Hint           string  `json:"hint,omitempty"`
	Tokens         *Tokens `json:"tokens,omitempty"`
	CostReported   float64 `json:"cost_reported,omitempty"`
}

// JSON encodes the record as one runs.jsonl line.
func (r Record) JSON() string {
	raw, _ := json.Marshal(r)
	return string(raw)
}

// ParseRecords decodes valid records and skips malformed lines.
func ParseRecords(raw []byte) []Record {
	var records []Record
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record Record
		if json.Unmarshal([]byte(line), &record) == nil {
			records = append(records, record)
		}
	}
	return records
}
