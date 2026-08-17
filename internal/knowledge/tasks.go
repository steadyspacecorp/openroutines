package knowledge

import "strings"

type TaskEntry struct {
	Text string
	ID   string
	Open bool
	line int
}

func ParseTaskEntries(text string) []TaskEntry {
	var entries []TaskEntry
	inFence := false
	for i, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		entry, ok := parseTaskEntry(trimmed)
		if !ok {
			continue
		}
		entry.line = i
		entries = append(entries, entry)
	}
	return entries
}

func parseTaskEntry(line string) (TaskEntry, bool) {
	entry := TaskEntry{Text: line}
	var rest string
	switch {
	case strings.HasPrefix(line, "- [ ]"):
		entry.Open = true
		rest = strings.TrimSpace(strings.TrimPrefix(line, "- [ ]"))
	case strings.HasPrefix(line, "- [x]"):
		rest = strings.TrimSpace(strings.TrimPrefix(line, "- [x]"))
	default:
		return TaskEntry{}, false
	}
	if strings.HasPrefix(rest, "`") {
		if end := strings.Index(rest[1:], "`"); end >= 0 {
			entry.ID = rest[1 : end+1]
		}
	}
	return entry, true
}
