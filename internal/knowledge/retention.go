package knowledge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var trimmedStreams = []string{"events.md", "context.md"}

const runRecordsFile = "runs.jsonl"

const trimTrailer = "Openroutines-Retention-Trim: true"

func (store *Store) CommitTrim(keep time.Duration) (string, error) {
	message := fmt.Sprintf("Trim knowledge to retention window (%s)\n\n%s\n", keep, trimTrailer)
	return store.commitPaths(message, append([]string{runRecordsFile}, trimmedStreams...)...)
}

func (store *Store) Trim(keep time.Duration, now time.Time) (bool, error) {
	wt := store.Worktree()
	cutoff := now.Add(-keep)
	changed := false

	for _, name := range trimmedStreams {
		path := filepath.Join(wt, name)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		ages, err := store.blameLineTimes(name)
		if err != nil {
			return changed, fmt.Errorf("trim %s: %w", name, err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return changed, err
		}
		lines := strings.Split(string(raw), "\n")
		kept := lines[:0:0]
		dropped := 0
		inFence := false
		for i, line := range lines {
			t := strings.TrimSpace(line)
			fence := strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")
			keepLine := true
			if !inFence && !fence && strings.HasPrefix(t, "- ") {
				if at, known := ages[i+1]; known && at.Before(cutoff) {
					keepLine = false
				}
			}
			if fence {
				inFence = !inFence
			}
			if keepLine {
				kept = append(kept, line)
			} else {
				dropped++
			}
		}
		if dropped > 0 {
			if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
				return changed, err
			}
			changed = true
		}
	}

	if trimmed, err := trimRunRecords(filepath.Join(wt, runRecordsFile), cutoff); err != nil {
		return changed, err
	} else if trimmed {
		changed = true
	}
	return changed, nil
}

func (store *Store) blameLineTimes(file string) (map[int]time.Time, error) {
	out, err := store.worktree.Run("blame", "--line-porcelain", "--", file)
	if err != nil {
		return nil, err
	}
	ages := map[int]time.Time{}
	var line int
	var uncommitted bool
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		t := sc.Text()
		switch {
		case strings.HasPrefix(t, "\t"):

			line = 0
		case line == 0 && len(t) > 40 && t[40] == ' ':

			fields := strings.Fields(t)
			if len(fields) >= 3 {
				if n, err := strconv.Atoi(fields[2]); err == nil {
					line = n
					uncommitted = strings.HasPrefix(t, strings.Repeat("0", 40))
				}
			}
		case strings.HasPrefix(t, "committer-time "):
			if unix, err := strconv.ParseInt(strings.TrimPrefix(t, "committer-time "), 10, 64); err == nil && line > 0 && !uncommitted {
				ages[line] = time.Unix(unix, 0)
			}
		}
	}
	return ages, nil
}

func trimRunRecords(path string, cutoff time.Time) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	var kept []string
	dropped := 0
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec struct {
			RecordedAt time.Time `json:"recorded_at"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err == nil && !rec.RecordedAt.IsZero() && rec.RecordedAt.Before(cutoff) {
			dropped++
			continue
		}
		kept = append(kept, line)
	}
	if dropped == 0 {
		return false, nil
	}
	content := strings.Join(kept, "\n")
	if content != "" {
		content += "\n"
	}
	return true, os.WriteFile(path, []byte(content), 0o644)
}
