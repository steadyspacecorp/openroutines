package knowledge

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
)

const NewEventsFile = "new-events.md"

const newEventsStub = "# New events\n\n" +
	"This run's events, appended to the shared events.md when the run settles.\n\n" +
	"Format (one bullet per event, with no date, routine name, or run id):\n\n```markdown\n" +
	"- <what happened, why it matters, links, people>\n" +
	"- NO-OP: <what was checked and found clean>\n```\n"

func WriteNewEventsStub(knowledgeDir string) error {
	root, err := os.OpenRoot(knowledgeDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return root.WriteFile(NewEventsFile, []byte(newEventsStub), 0o644)
}

// Removes the run's new events from staging and returns them; the file never
// travels into the worktree.
func TakeNewEvents(stagingDir string) ([]string, error) {
	root, err := os.OpenRoot(stagingDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	f, err := openStaged(root, NewEventsFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	// Read before Import validates staging, so the per-file cap is enforced
	// here, on the bytes actually read.
	raw, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxFile {
		return nil, fmt.Errorf("staged knowledge file %q exceeds %d bytes -- rejected", NewEventsFile, maxFile)
	}

	var entries []string
	inFence, open := false, false
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64<<10), maxFile)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~"):
			inFence, open = !inFence, false
		case inFence:
		case strings.HasPrefix(line, "- "):
			entries = append(entries, flatten(line[2:]))
			open = true
		case line == "" || strings.HasPrefix(line, "#"):
			open = false
		case open:
			entries[len(entries)-1] += " " + flatten(line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, root.Remove(NewEventsFile)
}

func (store *Store) AppendNewEvents(routineName, runID string, now time.Time, entries []string) error {
	stamp := fmt.Sprintf("%s %s (%s): ", now.UTC().Format("2006-01-02"), routineName, runID)
	if len(entries) == 0 {
		return store.AppendEvent(stamp + "NO-OP: the run recorded no event")
	}
	for _, entry := range entries {
		if err := store.AppendEvent(stamp + entry); err != nil {
			return err
		}
	}
	return nil
}
