package knowledge

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/steadyspacecorp/openroutines/internal/scrub"
)

func flatten(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func (store *Store) scrubbed(line string) string {
	return flatten(scrub.Redacted(line))
}

// Records a supervisor-written event.
func (store *Store) AppendEvent(line string) error {
	p := filepath.Join(store.Worktree(), "events.md")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "- %s\n", store.scrubbed(line))
	return err
}

// Records a supervisor-created task in the Human-owned section.
func (store *Store) AppendHumanTask(taskID, description string) error {
	p := filepath.Join(store.Worktree(), "tasks.md")
	raw, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := string(raw)
	if text == "" {
		text = "# Tasks\n"
	}
	for _, task := range ParseTaskEntries(text) {
		if task.ID == taskID {
			return nil
		}
	}
	entry := fmt.Sprintf("- [ ] `%s` %s", taskID, store.scrubbed(description))
	lines := strings.Split(text, "\n")

	section := -1
	inFence := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if !inFence && trimmed == "## Human-owned" {
			section = i
		}
	}
	if section < 0 {
		text = strings.TrimRight(text, "\n") + "\n\n## Human-owned\n\n" + entry + "\n"
		return os.WriteFile(p, []byte(text), 0o644)
	}

	end := len(lines)
	inFence = false
	for i := section + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if !inFence && strings.HasPrefix(trimmed, "## ") {
			end = i
			break
		}
	}
	for end > section+1 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	out := slices.Insert(lines, end, entry)
	return os.WriteFile(p, []byte(strings.Join(out, "\n")), 0o644)
}

// Completes open tasks whose id starts with idPrefix.
func (store *Store) ResolveHumanTasks(idPrefix, resolution string) (bool, error) {
	p := filepath.Join(store.Worktree(), "tasks.md")
	raw, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	lines := strings.Split(string(raw), "\n")
	changed := false
	for _, task := range ParseTaskEntries(string(raw)) {
		if !task.Open || !strings.HasPrefix(task.ID, idPrefix) {
			continue
		}
		i := task.line
		line := lines[i]
		line = strings.Replace(line, "- [ ]", "- [x]", 1)
		if trimmed := strings.TrimRight(line, " "); strings.HasSuffix(trimmed, ")") {
			at := strings.LastIndex(line, ")")
			line = line[:at] + "; " + resolution + ")"
		} else {
			line += " (" + resolution + ")"
		}
		lines[i] = line
		changed = true
	}
	if !changed {
		return false, nil
	}
	return true, os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o644)
}

// Appends a redacted JSONL run record.
func (store *Store) AppendRunRecord(record string) error {
	p := filepath.Join(store.Worktree(), "runs.jsonl")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, scrub.Redacted(record))
	return err
}
